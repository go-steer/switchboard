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
	updated []fakeUpdate
	deleted []chat.MessageRef
}

// fakeUpdate records one in-place edit (used to assert status-mode behavior).
type fakeUpdate struct {
	ref  chat.MessageRef
	text string
}

func (f *fakeSender) Send(_ context.Context, r chat.Reply) (chat.MessageRef, error) {
	f.mu.Lock()
	f.nextID++
	ref := chat.MessageRef{Conversation: r.Conversation, ID: fmt.Sprintf("ts%d", f.nextID)}
	f.mu.Unlock()
	f.replies <- r
	return ref, nil
}

func (f *fakeSender) Update(_ context.Context, ref chat.MessageRef, r chat.Reply) error {
	f.mu.Lock()
	f.updated = append(f.updated, fakeUpdate{ref: ref, text: r.Text})
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) updatedCalls() []fakeUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeUpdate(nil), f.updated...)
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
// a first turn creates a session, injects, and relays the agent's SSE output
// back through the sender; a second turn on the same conversation reuses the
// session (no second create). The wake route is registered and asserted
// untouched: inject already wakes the session, so a wake here would run the
// turn a second time and duplicate the reply in the thread.
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
	router := NewRouter(dc, fake, ProgressOff, nil, nil)

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
	if got := wakes.Load(); got != 0 {
		t.Errorf("wakes = %d, want 0 (inject already wakes; a second signal runs a duplicate turn)", got)
	}
	assertInjected(t, injected, "first")
	assertInjected(t, injected, "second")
}

// TestRouterHandleSurfacesErrors verifies that a daemon failure mid-turn
// (a failed inject, after the session already exists) is reported into the
// thread rather than only logged, and that the notice text distinguishes a
// transient (5xx) failure — worth retrying — from a terminal (4xx) one.
func TestRouterHandleSurfacesErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		injectCode int
		wantText   string
	}{
		{"transient", http.StatusServiceUnavailable, errNoticeTransient},
		{"terminal", http.StatusBadRequest, errNoticeTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
			})
			mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", tc.injectCode)
			})

			srv := httptest.NewServer(mux)
			defer srv.Close()

			dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
			if err != nil {
				t.Fatalf("daemon.New: %v", err)
			}
			fake := &fakeSender{replies: make(chan chat.Reply, 4)}
			router := NewRouter(dc, fake, ProgressOff, nil, nil)

			msg := chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "hi"}
			if err := router.Handle(context.Background(), msg); err == nil {
				t.Fatal("Handle: want error, got nil")
			}

			select {
			case rep := <-fake.replies:
				if rep.Conversation != "C0:100.1" || rep.Text != tc.wantText {
					t.Fatalf("error notice = %+v, want text %q", rep, tc.wantText)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for error notice")
			}
		})
	}
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
	router := NewRouter(dc, fake, ProgressIndicator, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Handle posts the placeholder synchronously; it must arrive first.
	ind := recvReply(t, fake.replies)
	if ind.Text != workingText {
		t.Fatalf("first reply = %q, want indicator %q", ind.Text, workingText)
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

// newEventRouter builds a router (in the given progress mode) wired to a fake
// daemon that streams the supplied agent-event payloads in order once release
// is closed — pass a nil release to stream immediately — then holds the stream
// open like a live daemon.
func newEventRouter(t *testing.T, mode ProgressMode, release <-chan struct{}, events ...string) (*Router, *fakeSender) {
	t.Helper()
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
		if release != nil {
			<-release
		}
		for _, ev := range events {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", daemon.EventAgent, ev)
		}
		f.Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	return NewRouter(dc, fake, mode, nil, nil), fake
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal(msg)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

const (
	toolCallEvent = `{"seq":1,"event":{"Content":{"parts":[{"functionCall":{"name":"lookup"}}],"role":"model"}}}`
	answerEvent   = `{"seq":2,"event":{"Content":{"parts":[{"text":"the answer"}],"role":"model"},"Partial":false}}`
)

// TestRouterProgressStream verifies stream mode posts a standalone notice for a
// tool call and then relays the completed turn — with no in-place edits and no
// managed placeholder.
func TestRouterProgressStream(t *testing.T) {
	router, fake := newEventRouter(t, ProgressStream, nil, toolCallEvent, answerEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := recvReply(t, fake.replies); got.Text != activityText([]string{"lookup"}) {
		t.Fatalf("first reply = %q, want tool notice %q", got.Text, activityText([]string{"lookup"}))
	}
	if got := recvReply(t, fake.replies); got.Text != "the answer" {
		t.Fatalf("second reply = %q, want the answer", got.Text)
	}
	if n := len(fake.updatedCalls()); n != 0 {
		t.Errorf("stream mode edited %d message(s); want 0", n)
	}
}

// TestRouterProgressStatus verifies status mode keeps one message per turn: the
// placeholder posted on wake is edited in place to name the running tool, then
// deleted when the answer is posted. The fake withholds events until the
// placeholder is in flight so the edit targets it deterministically.
func TestRouterProgressStatus(t *testing.T) {
	release := make(chan struct{})
	router, fake := newEventRouter(t, ProgressStatus, release, toolCallEvent, answerEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Handle posts the status placeholder synchronously; it must arrive first.
	if got := recvReply(t, fake.replies); got.Text != workingText {
		t.Fatalf("first reply = %q, want placeholder %q", got.Text, workingText)
	}

	// Release the tool + answer events.
	close(release)
	if got := recvReply(t, fake.replies); got.Text != "the answer" {
		t.Fatalf("reply = %q, want the answer", got.Text)
	}

	// The status message (ts1) was edited in place to name the tool. The edit
	// carries the ticker's line — clock, tool, step — because both writers
	// render into this one message and must agree on its shape (#37).
	waitFor(t, func() bool {
		for _, u := range fake.updatedCalls() {
			if u.ref.ID == "ts1" && strings.Contains(u.text, "running `lookup` (step 1)") {
				return true
			}
		}
		return false
	}, "status message was not edited with the tool notice")
	// ...and then retired when the answer was posted.
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts1") },
		"status message was not deleted when the answer arrived")
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
	router := NewRouter(dc, fake, ProgressOff, nil, nil)
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
	router := NewRouter(dc, &fakeSender{replies: make(chan chat.Reply, 8)}, ProgressOff, nil, nil)
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

// newCommandRouter builds a router with no live daemon — enough to exercise
// HandleCommand, which never touches the client or the sender.
func newCommandRouter(t *testing.T, mode ProgressMode) *Router {
	t.Helper()
	dc, err := daemon.New(daemon.Config{BaseURL: "http://127.0.0.1:1", BearerToken: "tok"})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return NewRouter(dc, &fakeSender{replies: make(chan chat.Reply, 1)}, mode, nil, nil)
}

// TestRouterHandleCommand covers the progress command surface: querying,
// setting (case-insensitively), per-channel isolation, invalid values, the
// missing-channel guard, and help/unknown fallbacks.
func TestRouterHandleCommand(t *testing.T) {
	r := newCommandRouter(t, ProgressIndicator)
	ctx := context.Background()

	// Query with no override reports the process default.
	if ack, _ := r.HandleCommand(ctx, chat.Command{Name: "progress", Channel: "C1"}); !strings.Contains(ack, "indicator") {
		t.Errorf("query ack = %q, want it to name the default mode", ack)
	}

	// Set a valid mode; the override sticks for that channel only.
	ack, err := r.HandleCommand(ctx, chat.Command{Name: "progress", Channel: "C1", Args: []string{"status"}})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.Contains(ack, "status") {
		t.Errorf("set ack = %q, want it to confirm status", ack)
	}
	if got := r.progressFor("C1"); got != ProgressStatus {
		t.Errorf("progressFor(C1) = %q, want status", got)
	}
	if got := r.progressFor("C2"); got != ProgressIndicator {
		t.Errorf("progressFor(C2) = %q, want the untouched default", got)
	}

	// The value is case-insensitive.
	if _, err := r.HandleCommand(ctx, chat.Command{Name: "progress", Channel: "C1", Args: []string{"OFF"}}); err != nil {
		t.Fatalf("set OFF: %v", err)
	}
	if got := r.progressFor("C1"); got != ProgressOff {
		t.Errorf("progressFor(C1) after OFF = %q, want off", got)
	}

	// An invalid value is a helpful ack, not a change.
	if ack, _ := r.HandleCommand(ctx, chat.Command{Name: "progress", Channel: "C1", Args: []string{"bogus"}}); !strings.Contains(ack, "Unknown progress mode") {
		t.Errorf("invalid ack = %q", ack)
	}
	if got := r.progressFor("C1"); got != ProgressOff {
		t.Errorf("invalid value changed the mode to %q", got)
	}

	// Setting requires a channel.
	if ack, _ := r.HandleCommand(ctx, chat.Command{Name: "progress", Args: []string{"status"}}); !strings.Contains(ack, "channel") {
		t.Errorf("no-channel ack = %q, want the channel guard", ack)
	}

	// Empty and unknown commands both fall back to the usage line.
	if ack, _ := r.HandleCommand(ctx, chat.Command{Name: "", Channel: "C1"}); !strings.Contains(ack, "progress") {
		t.Errorf("help ack = %q", ack)
	}
	if ack, _ := r.HandleCommand(ctx, chat.Command{Name: "wat", Channel: "C1"}); !strings.Contains(ack, "progress") {
		t.Errorf("unknown ack = %q", ack)
	}
}

// TestRouterCommandOverridesTurnMode proves a per-channel override set via a
// command actually changes turn handling: with the process default off, a
// command flips channel C0 to stream, so a tool call now surfaces a standalone
// activity notice ahead of the answer.
func TestRouterCommandOverridesTurnMode(t *testing.T) {
	router, fake := newEventRouter(t, ProgressOff, nil, toolCallEvent, answerEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if ack, err := router.HandleCommand(ctx, chat.Command{Name: "progress", Channel: "C0", Args: []string{"stream"}}); err != nil {
		t.Fatalf("command: %v", err)
	} else if !strings.Contains(ack, "stream") {
		t.Fatalf("command ack = %q, want it to confirm stream", ack)
	}

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Channel: "C0", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := recvReply(t, fake.replies); got.Text != activityText([]string{"lookup"}) {
		t.Fatalf("first reply = %q, want tool notice under the stream override", got.Text)
	}
	if got := recvReply(t, fake.replies); got.Text != "the answer" {
		t.Fatalf("second reply = %q, want the answer", got.Text)
	}
}
