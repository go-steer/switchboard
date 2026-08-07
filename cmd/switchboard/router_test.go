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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// fakeSender captures replies the router relays back and records the
// progress-message lifecycle (each Send hands back a unique ref; Update and
// Delete are recorded so tests can assert placeholder handling).
type fakeSender struct {
	replies chan chat.Reply

	mu      sync.Mutex
	nextID  int
	deleted []chat.MessageRef
}

func (f *fakeSender) Send(_ context.Context, r chat.Reply) (chat.MessageRef, error) {
	f.mu.Lock()
	f.nextID++
	ref := chat.MessageRef{Conversation: r.Conversation, ID: fmt.Sprintf("ts%d", f.nextID)}
	f.mu.Unlock()
	f.replies <- r
	return ref, nil
}

// Update satisfies the sender interface; the router does not use it until the
// status/stream progress modes land, so the fake simply accepts the call.
func (f *fakeSender) Update(_ context.Context, _ chat.MessageRef, _ chat.Reply) error {
	return nil
}

func (f *fakeSender) Delete(_ context.Context, ref chat.MessageRef) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, ref)
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) deletedRefs() []chat.MessageRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chat.MessageRef(nil), f.deleted...)
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
	router := NewRouter(dc, fake, ProgressOff, nil)

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

// TestRouterProgressIndicator verifies the indicator lifecycle: on wake the
// router posts a placeholder, and when the agent's real reply arrives the
// relay deletes that placeholder before posting the answer. The fake daemon
// withholds the agent event until the test releases it, so the placeholder is
// guaranteed to be observed (and its ref recorded) before the answer.
func TestRouterProgressIndicator(t *testing.T) {
	release := make(chan struct{})
	const agentEvent = `{"seq":1,"event":{"Content":{"parts":[{"text":"the answer"}],"role":"model"},"Partial":false}}`

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	mux.HandleFunc("POST /sessions/{app}/{sid}/wake", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		<-release // hold the answer back until the placeholder is in flight
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", daemon.EventAgent, agentEvent)
		f.Flush()
		<-r.Context().Done()
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	fake := &fakeSender{replies: make(chan chat.Reply, 4)}
	router := NewRouter(dc, fake, ProgressIndicator, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Handle posts the placeholder synchronously; it must arrive first.
	ind := recvReply(t, fake.replies)
	if ind.Text != indicatorText {
		t.Fatalf("first reply = %q, want indicator %q", ind.Text, indicatorText)
	}

	// Release the real answer; the relay should clear the placeholder, then post.
	close(release)
	ans := recvReply(t, fake.replies)
	if ans.Text != "the answer" {
		t.Fatalf("second reply = %q, want the answer", ans.Text)
	}

	// The placeholder (ts1, the first Send) must have been deleted.
	deadline := time.After(2 * time.Second)
	for {
		if containsRefID(fake.deletedRefs(), "ts1") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("placeholder was not deleted; deletes = %+v", fake.deletedRefs())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func containsRefID(refs []chat.MessageRef, id string) bool {
	for _, r := range refs {
		if r.ID == id {
			return true
		}
	}
	return false
}

func recvReply(t *testing.T, ch <-chan chat.Reply) chat.Reply {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for relayed reply")
		return chat.Reply{}
	}
}

// TestRouterRelayReconnectsAndDedupes drives a stream that drops after the
// first turn: the relay must reconnect (resuming from the last seq seen) and
// deliver later turns, while a turn replayed across the reconnect boundary is
// posted only once.
func TestRouterRelayReconnectsAndDedupes(t *testing.T) {
	var conns atomic.Int64
	since2 := make(chan string, 1)
	const ev1 = `{"seq":1,"event":{"Content":{"parts":[{"text":"answer 1"}],"role":"model"},"Partial":false}}`
	const ev2 = `{"seq":2,"event":{"Content":{"parts":[{"text":"answer 2"}],"role":"model"},"Partial":false}}`

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	mux.HandleFunc("POST /sessions/{app}/{sid}/wake", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		if conns.Add(1) == 1 {
			// First connection: deliver turn 1, then drop the stream.
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", daemon.EventAgent, ev1)
			return
		}
		// Reconnect: it must resume past turn 1. Replay turn 1 (a boundary
		// duplicate) then deliver turn 2; dedup must drop the replay.
		since2 <- r.URL.Query().Get("since")
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", daemon.EventAgent, ev1)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", daemon.EventAgent, ev2)
		f.Flush()
		<-r.Context().Done()
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	router := NewRouter(dc, fake, ProgressOff, nil)
	router.minBackoff, router.maxBackoff = 5*time.Millisecond, 20*time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := recvReply(t, fake.replies); got.Text != "answer 1" {
		t.Fatalf("first reply = %q, want answer 1", got.Text)
	}
	select {
	case s := <-since2:
		if s != "1" {
			t.Errorf("reconnect since = %q, want 1 (resume past turn 1)", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not reconnect after the stream dropped")
	}
	if got := recvReply(t, fake.replies); got.Text != "answer 2" {
		t.Fatalf("second reply = %q, want answer 2 (turn 1 replay should be deduped)", got.Text)
	}
	select {
	case dup := <-fake.replies:
		t.Fatalf("unexpected extra reply (dedup failed): %q", dup.Text)
	case <-time.After(100 * time.Millisecond):
	}
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
	router := NewRouter(dc, &fakeSender{replies: make(chan chat.Reply, 8)}, ProgressOff, nil)
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
