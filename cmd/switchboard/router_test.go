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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// fakeSender captures replies the router relays back.
type fakeSender struct {
	replies chan chat.Reply
}

func (f *fakeSender) Send(_ context.Context, r chat.Reply) error {
	f.replies <- r
	return nil
}

// TestRouterRoundTrip drives the full inbound path against a fake daemon:
// a first turn creates a session, injects, wakes, and relays the agent's
// SSE output back through the sender; a second turn on the same
// conversation reuses the session (no second create).
func TestRouterRoundTrip(t *testing.T) {
	var creates, injects, wakes atomic.Int64
	injected := make(chan string, 4)
	const agentEvent = `{"seq":1,"event":{"Content":{"parts":[{"text":"the answer"}],"role":"model"},"Partial":false}}`

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Asserted-Caller"); got != "alice@example.com" {
			t.Errorf("create X-Asserted-Caller = %q", got)
		}
		creates.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) {
		injects.Add(1)
		b, _ := io.ReadAll(r.Body)
		injected <- string(b)
		fmt.Fprint(w, `{"injected":"ok","session":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/wake", func(w http.ResponseWriter, r *http.Request) {
		wakes.Add(1)
		fmt.Fprint(w, `{"woken":"s1"}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PathValue("app"); got != "core-agent" {
			t.Errorf("events app = %q, want core-agent", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", daemon.EventAgent, agentEvent)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // hold the stream open like a live daemon
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	fake := &fakeSender{replies: make(chan chat.Reply, 4)}
	router := NewRouter(dc, fake, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msg := chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "first"}
	if err := router.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle turn 1: %v", err)
	}

	// The agent's turn is relayed back into the same conversation.
	select {
	case rep := <-fake.replies:
		if rep.Conversation != "C0:100.1" || rep.Text != "the answer" {
			t.Fatalf("relayed reply = %+v", rep)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for relayed reply")
	}

	// Second turn on the same conversation reuses the session.
	msg2 := msg
	msg2.Text = "second"
	if err := router.Handle(ctx, msg2); err != nil {
		t.Fatalf("Handle turn 2: %v", err)
	}

	if got := creates.Load(); got != 1 {
		t.Errorf("creates = %d, want 1 (session should be reused)", got)
	}
	if got := injects.Load(); got != 2 {
		t.Errorf("injects = %d, want 2", got)
	}
	if got := wakes.Load(); got != 2 {
		t.Errorf("wakes = %d, want 2", got)
	}
	assertInjected(t, injected, "first")
	assertInjected(t, injected, "second")
}

func assertInjected(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case body := <-ch:
		if !strings.Contains(body, `"message":"`+want+`"`) {
			t.Errorf("inject body = %s, want message %q", body, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("no inject carrying %q", want)
	}
}

// TestRouterConcurrentFirstTurnsCreateOnce verifies the create-once guard:
// simultaneous first turns on one conversation must yield a single session.
func TestRouterConcurrentFirstTurnsCreateOnce(t *testing.T) {
	var creates atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		creates.Add(1)
		time.Sleep(20 * time.Millisecond) // widen the race window
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/wake", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	router := NewRouter(dc, &fakeSender{replies: make(chan chat.Reply, 8)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			errs <- router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"})
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("creates = %d, want 1", got)
	}
}
