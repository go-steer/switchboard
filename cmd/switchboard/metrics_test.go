// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetricsRecordNilSafe verifies the record helpers are no-ops on a nil
// *metrics, so the router runs uninstrumented without panicking.
func TestMetricsRecordNilSafe(t *testing.T) {
	var m *metrics // nil
	m.recordMessage(nil)
	m.recordMessage(fmt.Errorf("boom"))
	m.recordCommand()
	m.recordDaemon("inject", time.Millisecond, nil)
	m.recordReply(nil)
	m.recordTurnRelayed()
	m.recordReconnect()
	m.sessionOpened()
	// Reaching here without a panic is the assertion.
}

// TestServeMetricsDisabled verifies an empty addr binds nothing and returns
// only once ctx is cancelled.
func TestServeMetricsDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveMetrics(ctx, "", newMetrics()) }()
	select {
	case err := <-done:
		t.Fatalf("serveMetrics returned before cancel: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveMetrics(disabled) = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveMetrics did not return after cancel")
	}
}

// TestServeMetricsEndpoints starts the server on an ephemeral port and asserts
// /healthz and /metrics both answer, then that ctx cancellation shuts it down.
func TestServeMetricsEndpoints(t *testing.T) {
	addr := freeAddr(t)
	m := newMetrics()
	m.recordMessage(nil) // ensure at least one series is exported

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveMetrics(ctx, addr, m) }()

	base := "http://" + addr
	waitReady(t, base+"/healthz")

	if body, code := httpGet(t, base+"/healthz"); code != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Fatalf("/healthz = %d %q, want 200 \"ok\"", code, body)
	}
	body, code := httpGet(t, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", code)
	}
	if !strings.Contains(body, "switchboard_messages_total") {
		t.Fatalf("/metrics missing switchboard_messages_total:\n%s", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveMetrics = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveMetrics did not shut down after cancel")
	}
}

// TestServeMetricsBindError verifies a port already in use surfaces as an
// error rather than being swallowed — this is what lets serve fail fast.
func TestServeMetricsBindError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := serveMetrics(ctx, ln.Addr().String(), newMetrics()); err == nil {
		t.Fatal("serveMetrics on a busy port returned nil, want error")
	}
}

// TestRouterRecordsMetrics drives one inbound turn end to end against a fake
// daemon and asserts the router incremented the create/inject/wake, message,
// reply, turn-relayed, and active-session collectors.
func TestRouterRecordsMetrics(t *testing.T) {
	const agentEvent = `{"seq":1,"event":{"Content":{"parts":[{"text":"the answer"}],"role":"model"},"Partial":false}}`
	var creates atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) {
		creates.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"injected":"ok","session":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/wake", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"woken":"s1"}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", daemon.EventAgent, agentEvent)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	m := newMetrics()
	fake := &fakeSender{replies: make(chan chat.Reply, 4)}
	router := NewRouter(dc, fake, ProgressOff, m, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "alice@example.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Wait for the relayed reply so the SSE path (turnsRelayed + reply) ran.
	select {
	case <-fake.replies:
	case <-time.After(2 * time.Second):
		t.Fatal("no relayed reply")
	}

	if got := testutil.ToFloat64(m.messages.WithLabelValues("ok")); got != 1 {
		t.Errorf("messages{ok} = %v, want 1", got)
	}
	for _, op := range []string{"create", "inject", "wake"} {
		if got := testutil.ToFloat64(m.daemonRequests.WithLabelValues(op, "ok")); got != 1 {
			t.Errorf("daemon_requests{%s,ok} = %v, want 1", op, got)
		}
	}
	if got := testutil.ToFloat64(m.turnsRelayed); got != 1 {
		t.Errorf("turns_relayed = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.repliesSent.WithLabelValues("ok")); got != 1 {
		t.Errorf("replies_sent{ok} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.activeSessions); got != 1 {
		t.Errorf("active_sessions = %v, want 1", got)
	}
}

// freeAddr reserves an ephemeral port and returns its address. The listener is
// closed before returning so serveMetrics can bind it; the reuse window is
// small enough for tests.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// waitReady polls url until it answers or the deadline passes.
func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:gosec // test-local loopback URL
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready", url)
}

func httpGet(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // test-local loopback URL
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
