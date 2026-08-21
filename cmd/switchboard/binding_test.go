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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// boundSession is the session the fake daemon below already has running, as
// the ingress would name it on the wire.
const boundSession = "core-agent/incident-7"

// agentEventAt renders one agent frame carrying model text.
func agentEventAt(seq int64, text string) string {
	return fmt.Sprintf(`{"seq":%d,"event":{"Content":{"parts":[{"text":%q}],"role":"model"},"Partial":false}}`, seq, text)
}

// boundFrame is one agent event in the fake session's stream, kept with its
// seq so a subscriber can be served from where it says it got to.
type boundFrame struct {
	seq  int64
	data string
}

func agentFrame(seq int64, text string) boundFrame {
	return boundFrame{seq: seq, data: agentEventAt(seq, text)}
}

// boundDaemon is a daemon holding one long-running session that switchboard did
// not create: the unattended case (#38).
//
// It does not know a head probe from a relay, because a daemon does not: every
// connection is served the frames past its `since` and then held open, and a
// probe is only a connection that hangs up once the frames stop coming. Tests
// that need to know which connection was which read `resumed`, which records
// them in order.
type boundDaemon struct {
	creates, injects atomic.Int64
	// injectStatus, when set, is what inject answers with instead of success:
	// the daemon having forgotten the session it was bound to.
	injectStatus int
	// resumed records the `since` of every connection to the event stream.
	resumed chan string
	// gone, once closed, is the session disappearing out from under whoever is
	// watching it: open streams end and new ones are refused.
	gone chan struct{}

	mu      sync.Mutex
	frames  []boundFrame
	streams []chan boundFrame
}

// publish adds a frame to the session and hands it to whoever is listening. A
// frame published while nobody is connected is still there for the next
// subscriber, which is what a replay window is.
func (d *boundDaemon) publish(frames ...boundFrame) {
	for _, f := range frames {
		d.mu.Lock()
		d.frames = append(d.frames, f)
		subs := append([]chan boundFrame(nil), d.streams...)
		d.mu.Unlock()
		for _, s := range subs {
			select {
			case s <- f:
			default: // a reader this far behind is not what any test here is about
			}
		}
	}
}

func (d *boundDaemon) mux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		d.creates.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"fresh"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) {
		d.injects.Add(1)
		if d.injectStatus != 0 {
			http.Error(w, "unknown session", d.injectStatus)
			return
		}
		fmt.Fprint(w, `{"injected":"ok"}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		if d.resumed != nil {
			d.resumed <- r.URL.Query().Get("since")
		}
		select {
		case <-d.gone:
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()

		ch := make(chan boundFrame, 64)
		d.mu.Lock()
		var backlog []boundFrame
		for _, f := range d.frames {
			if f.seq > since {
				backlog = append(backlog, f)
			}
		}
		d.streams = append(d.streams, ch)
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			d.streams = slices.DeleteFunc(d.streams, func(c chan boundFrame) bool { return c == ch })
			d.mu.Unlock()
		}()

		write := func(f boundFrame) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", daemon.EventAgent, f.data)
			w.(http.Flusher).Flush()
		}
		for _, f := range backlog {
			write(f)
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-d.gone:
				return
			case f := <-ch:
				write(f)
			}
		}
	})
	return mux
}

// boundRouter wires a router against d, with a sender the test can read.
func boundRouter(t *testing.T, d *boundDaemon) (*Router, *fakeSender) {
	t.Helper()
	srv := httptest.NewServer(d.mux(t))
	t.Cleanup(srv.Close)
	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	return NewRouter(dc, fake, ProgressOff, nil, nil), fake
}

func mustSession(t *testing.T, ref string) daemon.Session {
	t.Helper()
	s, err := parseSessionRef(ref)
	if err != nil {
		t.Fatalf("parseSessionRef(%q): %v", ref, err)
	}
	return s
}

// TestRouterAdoptsABoundSession is the whole point of #38: a thread an
// unattended agent opened reaches that agent's session, not a new one — and it
// picks up at the head, so the hour of incident work behind it stays out of
// the chat thread.
func TestRouterAdoptsABoundSession(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 8)}
	d.publish(agentFrame(1, "pods are restarting"), agentFrame(5, "it is the readiness probe"))
	router, fake := boundRouter(t, d)
	sess := mustSession(t, boundSession)

	head, err := router.PrepareBind(context.Background(), "C0", sess)
	if err != nil {
		t.Fatalf("PrepareBind: %v", err)
	}
	if head != 5 {
		t.Fatalf("head = %d, want 5 (the end of the backlog)", head)
	}
	router.CommitBind("C0:100.1", sess, head)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msg := chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "what happened?"}
	if err := router.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	d.publish(agentFrame(6, "here is what I found"))

	// Three connections to the event stream, in order: the bind's head probe,
	// the probe adoption takes when the thread starts listening, and the relay
	// itself — which is the only one that resumes anywhere but the beginning.
	for i, want := range []string{"0", "0", "5"} {
		select {
		case since := <-d.resumed:
			if since != want {
				t.Errorf("event stream %d asked for since=%s, want %s: the backlog belongs to the agent, not the thread",
					i+1, since, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d connections to the event stream; the relay never subscribed", i)
		}
	}
	select {
	case rep := <-fake.replies:
		if rep.Text != "here is what I found" {
			t.Errorf("relayed %q; the only thing the thread should see is what came after the bind", rep.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the adopted session's answer")
	}
	if got := d.creates.Load(); got != 0 {
		t.Errorf("creates = %d, want 0: the conversation was bound to a session that already existed", got)
	}
	if got := d.injects.Load(); got != 1 {
		t.Errorf("injects = %d, want 1", got)
	}
}

// TestAdoptedRelayAssertsNoCaller: switchboard did not open this session and
// has no standing to claim it belongs to whoever happened to reply in the
// thread. The turn it injects is still attributed to them.
func TestAdoptedRelayAssertsNoCaller(t *testing.T) {
	callers := make(chan string, 2)
	injectCallers := make(chan string, 2)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, r *http.Request) {
		injectCallers <- r.Header.Get("X-Asserted-Caller")
		fmt.Fprint(w, `{"injected":"ok"}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		callers <- r.Header.Get("X-Asserted-Caller")
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	router := NewRouter(dc, &fakeSender{replies: make(chan chat.Reply, 4)}, ProgressOff, nil, nil)
	router.CommitBind("C0:1", mustSession(t, boundSession), 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "alice@example.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Both connections: the probe adoption takes, then the relay. Reading only
	// the first would be reading the probe, which asserts nothing by
	// construction and so would agree with a relay that asserted the world.
	for i := range 2 {
		select {
		case got := <-callers:
			if got != "" {
				t.Errorf("event stream %d asserted caller %q on an adopted session, want none", i+1, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d connections to the event stream; the relay never subscribed", i)
		}
	}
	select {
	case got := <-injectCallers:
		if got != "alice@example.com" {
			t.Errorf("inject asserted caller %q, want the human who typed the turn", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was injected")
	}
}

// TestBindRefusesAConversationThatAlreadyHasASession: the binding would be
// recorded, never consulted, and the caller would believe replies were
// reaching the incident session. Refusing before the post is what makes that
// visible.
func TestBindRefusesAConversationThatAlreadyHasASession(t *testing.T) {
	d := &boundDaemon{}
	router, _ := boundRouter(t, d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A human speaks first: the conversation creates a session of its own.
	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "alice@example.com", Text: "hello"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	_, err := router.PrepareBind(ctx, "C0:1", mustSession(t, boundSession))
	if !errors.Is(err, errConversationBound) {
		t.Fatalf("PrepareBind err = %v, want errConversationBound", err)
	}
	if got := d.injects.Load(); got != 1 {
		t.Errorf("injects = %d; the refused bind should have touched nothing", got)
	}
}

// TestBindRefusesASessionAlreadyBoundElsewhere: two threads relaying one
// session would each post every answer it produced, to two sets of readers who
// asked different questions.
func TestBindRefusesASessionAlreadyBoundElsewhere(t *testing.T) {
	d := &boundDaemon{}
	d.publish(agentFrame(3, "working"))
	router, _ := boundRouter(t, d)
	sess := mustSession(t, boundSession)

	head, err := router.PrepareBind(context.Background(), "C0", sess)
	if err != nil {
		t.Fatalf("PrepareBind: %v", err)
	}
	router.CommitBind("C0:100.1", sess, head)

	if _, err := router.PrepareBind(context.Background(), "C0:200.2", sess); !errors.Is(err, errSessionBound) {
		t.Fatalf("PrepareBind err = %v, want errSessionBound", err)
	}
	// The same thread is not "elsewhere": a caller posting a second update into
	// the thread it already bound is not asking for anything new.
	if _, err := router.PrepareBind(context.Background(), "C0:100.1", sess); err != nil {
		t.Errorf("re-binding the same session to the same conversation: %v", err)
	}
}

// TestBindRefusesASessionTheDaemonDoesNotHave. The head probe is also the
// existence check, and this is the failure it exists to catch: a thread bound
// to nothing takes a human's reply and drops it.
func TestBindRefusesASessionTheDaemonDoesNotHave(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such session", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	router := NewRouter(dc, &fakeSender{replies: make(chan chat.Reply, 4)}, ProgressOff, nil, nil)

	_, err = router.PrepareBind(context.Background(), "C0", mustSession(t, boundSession))
	if err == nil {
		t.Fatal("PrepareBind accepted a session the daemon has never heard of")
	}
	if !isMissingSession(err) {
		t.Errorf("err = %v, want a 404 the ingress can report as a missing session", err)
	}
}

// TestALostBindingIsAnnouncedAndDropped covers GAP-7's other half: the daemon
// forgot the session a thread was bound to. The reply goes nowhere, and the
// one thing that must not happen is for switchboard to quietly open a fresh
// session and let the thread carry on as if the agent still remembered it.
func TestALostBindingIsAnnouncedAndDropped(t *testing.T) {
	d := &boundDaemon{injectStatus: http.StatusNotFound}
	router, fake := boundRouter(t, d)
	router.CommitBind("C0:1", mustSession(t, boundSession), 5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msg := chat.Message{Conversation: "C0:1", Caller: "alice@example.com", Text: "what happened?"}
	if err := router.Handle(ctx, msg); err == nil {
		t.Fatal("Handle succeeded against a session the daemon does not have")
	}

	select {
	case rep := <-fake.replies:
		if !strings.Contains(rep.Text, boundSession) {
			t.Errorf("notice = %q, want the session named so an operator can go and look", rep.Text)
		}
		if !strings.Contains(rep.Text, "not delivered") {
			t.Errorf("notice = %q, want it to say the message went nowhere", rep.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the thread was told nothing")
	}

	router.mu.Lock()
	_, stillBound := router.bindings["C0:1"]
	_, stillSessioned := router.sessions["C0:1"]
	router.mu.Unlock()
	if stillBound {
		t.Error("the binding survived; every later turn would fail the same way")
	}
	if stillSessioned {
		t.Error("the dead entry survived; the next turn would reuse it")
	}

	// And the thread is usable again: the next message opens a session of its
	// own, which is the honest outcome now that it has been announced.
	d.injectStatus = 0
	if err := router.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle after the lost binding: %v", err)
	}
	if got := d.creates.Load(); got != 1 {
		t.Errorf("creates = %d, want 1", got)
	}
}

// TestRebindingTheSameSessionMovesTheResumePoint: a caller posting a second
// update an hour into an incident has moved on, and so should the thread's
// resume point — otherwise the first reply drags in everything the agent did
// between the two posts.
func TestRebindingTheSameSessionMovesTheResumePoint(t *testing.T) {
	router, _ := boundRouter(t, &boundDaemon{})
	sess := mustSession(t, boundSession)

	router.CommitBind("C0:1", sess, 5)
	router.CommitBind("C0:1", sess, 42)

	router.mu.Lock()
	got := router.bindings["C0:1"]
	router.mu.Unlock()
	if got.since != 42 {
		t.Errorf("since = %d, want 42", got.since)
	}
}

// TestCommitBindYieldsToAConversationThatGotASessionFirst. Commit runs after
// the post, so it cannot refuse — but it can decline to overwrite a session
// that is already answering someone.
func TestCommitBindYieldsToAConversationThatGotASessionFirst(t *testing.T) {
	d := &boundDaemon{}
	router, _ := boundRouter(t, d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "alice@example.com", Text: "hello"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	router.CommitBind("C0:1", mustSession(t, boundSession), 5)

	router.mu.Lock()
	_, bound := router.bindings["C0:1"]
	router.mu.Unlock()
	if bound {
		t.Error("a binding was recorded over a live session, where nothing would ever consult it")
	}
}

// TestBindingsAreBoundedAndSayWhatTheyDropped: an evicted thread still looks
// bound to the caller that bound it, so the eviction is a log line rather than
// a silent deletion.
func TestBindingsAreBoundedAndSayWhatTheyDropped(t *testing.T) {
	var logged strings.Builder
	router := NewRouter(nil, &fakeSender{replies: make(chan chat.Reply, 1)}, ProgressOff, nil,
		func(f string, a ...any) { fmt.Fprintf(&logged, f+"\n", a...) })

	for i := range maxBindings + 1 {
		router.CommitBind(fmt.Sprintf("C%d", i), daemon.Session{App: "core-agent", ID: fmt.Sprintf("s%d", i)}, int64(i))
	}

	router.mu.Lock()
	held := len(router.bindings)
	_, oldest := router.bindings["C0"]
	inverse := len(router.boundTo)
	router.mu.Unlock()
	if held != maxBindings {
		t.Errorf("bindings held = %d, want %d", held, maxBindings)
	}
	if oldest {
		t.Error("the oldest binding survived; eviction is oldest-first")
	}
	if inverse != maxBindings {
		t.Errorf("boundTo = %d entries, want %d: the inverse index leaks otherwise", inverse, maxBindings)
	}
	if !strings.Contains(logged.String(), "C0: evicted") {
		t.Error("eviction was not logged")
	}
}

// TestParseSessionRef. Both halves land in a URL path the daemon client builds
// by concatenation, so this is a validation test before it is a parsing one.
func TestParseSessionRef(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want daemon.Session
	}{
		{"core-agent/s1", daemon.Session{App: "core-agent", ID: "s1"}},
		{"core-agent/01H_9-x.2", daemon.Session{App: "core-agent", ID: "01H_9-x.2"}},
	} {
		got, err := parseSessionRef(tc.in)
		if err != nil {
			t.Errorf("parseSessionRef(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSessionRef(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{
		"", "s1", "core-agent/", "/s1", "core-agent//s1", "core-agent/a/b",
		"../../sessions/x", "core-agent/..", "core-agent/./x",
		"core-agent/s1?x=1", "core-agent/s1#f", "core-agent/s 1", "core-agent/s\n1",
		"core-agent/s%2f1",
	} {
		if got, err := parseSessionRef(bad); err == nil {
			t.Errorf("parseSessionRef(%q) = %+v, want an error", bad, got)
		}
	}
}

// probeOnlyRouter is a router whose daemon answers every event-stream
// connection with the same short backlog and then closes it. Ending the stream
// is what lets a head probe return without waiting out its quiet window, which
// keeps the tests that only care about binding out of the business of timing;
// boundDaemon holds its streams open, the way a daemon with a live session
// does.
func probeOnlyRouter(t *testing.T) *Router {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", daemon.EventAgent, agentEventAt(5, "mid-incident"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return NewRouter(dc, &fakeSender{replies: make(chan chat.Reply, 8)}, ProgressOff, nil, nil)
}

// TestOnlyOneOfTwoRacingBindsTakesTheSession. Checking that a session is free
// and recording that it is taken are separated by a platform call, so the check
// alone proves nothing: two posts naming the same session would both pass it,
// both post, and both bind — two relays, every answer twice, and an inverse
// index that can only name one of them.
func TestOnlyOneOfTwoRacingBindsTakesTheSession(t *testing.T) {
	router := probeOnlyRouter(t)
	sess := mustSession(t, boundSession)

	type outcome struct {
		conv string
		err  error
	}
	out := make(chan outcome, 2)
	start := make(chan struct{})
	for _, conv := range []string{"C0", "C1"} {
		go func() {
			<-start
			_, err := router.PrepareBind(context.Background(), conv, sess)
			out <- outcome{conv: conv, err: err}
		}()
	}
	close(start)

	var won []string
	for range 2 {
		o := <-out
		switch {
		case o.err == nil:
			won = append(won, o.conv)
		case errors.Is(o.err, errBindInFlight):
			// The refusal this test is about. It is always this one and not
			// errSessionBound: the reservation is taken before the platform
			// call, so the loser is turned away while the winner is still
			// mid-bind. A loser arriving after the winner's commit gets
			// errSessionBound instead, which is
			// TestARefusedBindNamesTheThreadThatHasTheSession.
		default:
			t.Errorf("PrepareBind(%s) = %v, want nil or a bind conflict", o.conv, o.err)
		}
	}
	if len(won) != 1 {
		t.Fatalf("%d of 2 racing binds were accepted, want 1", len(won))
	}
	router.CommitBind(won[0]+":thread", sess, 5)

	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.bindings) != 1 || len(router.boundTo) != 1 || len(router.bindOrder) != 1 {
		t.Errorf("bindings=%d boundTo=%d bindOrder=%d, want 1 of each",
			len(router.bindings), len(router.boundTo), len(router.bindOrder))
	}
	if len(router.reserving) != 0 {
		t.Errorf("reserving = %v, want the reservation released by the commit", router.reserving)
	}
}

// TestARefusedBindNamesTheThreadThatHasTheSession. The caller that hits this is
// the one posting each update to the bare space it started in: the binding is
// recorded under the *thread* the first post created, so every later post is a
// conflict. It is refused, because two threads relaying one session would
// double-post every answer — but the refusal has to say where to post instead,
// or the caller has no move.
func TestARefusedBindNamesTheThreadThatHasTheSession(t *testing.T) {
	router := probeOnlyRouter(t)
	sess := mustSession(t, boundSession)

	if _, err := router.PrepareBind(context.Background(), "spaces/AAA", sess); err != nil {
		t.Fatalf("first PrepareBind: %v", err)
	}
	router.CommitBind("spaces/AAA:spaces/AAA/threads/BBB", sess, 5)

	_, err := router.PrepareBind(context.Background(), "spaces/AAA", sess)
	if !errors.Is(err, errSessionBound) {
		t.Fatalf("second PrepareBind = %v, want errSessionBound", err)
	}
	var bc *bindConflict
	if !errors.As(err, &bc) {
		t.Fatalf("err = %v, want a *bindConflict naming the thread", err)
	}
	if bc.conv != "spaces/AAA:spaces/AAA/threads/BBB" {
		t.Errorf("conflict names %q, want the thread the session is bound to", bc.conv)
	}
	// And the refusal did not leave a reservation behind, or the session would
	// be unbindable even after the thread let it go.
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.reserving) != 0 {
		t.Errorf("reserving = %v, want empty after a refusal", router.reserving)
	}
}

// TestAbortBindGivesTheSessionBack: a post that fails after its bind was
// prepared must not leave the session reserved for the life of the process.
func TestAbortBindGivesTheSessionBack(t *testing.T) {
	router := probeOnlyRouter(t)
	sess := mustSession(t, boundSession)

	if _, err := router.PrepareBind(context.Background(), "C0", sess); err != nil {
		t.Fatalf("PrepareBind: %v", err)
	}
	if _, err := router.PrepareBind(context.Background(), "C1", sess); !errors.Is(err, errBindInFlight) {
		t.Fatalf("second PrepareBind = %v, want errBindInFlight while the first is open", err)
	}
	router.AbortBind(sess)

	if _, err := router.PrepareBind(context.Background(), "C1", sess); err != nil {
		t.Errorf("PrepareBind after an abort: %v, want the session free again", err)
	}
}

// TestPrepareBindReleasesTheSessionWhenTheDaemonRefuses: the same, for the
// failure that happens inside PrepareBind itself.
func TestPrepareBindReleasesTheSessionWhenTheDaemonRefuses(t *testing.T) {
	mux := http.NewServeMux()
	var seen atomic.Int64
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		if seen.Add(1) == 1 {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	router := NewRouter(dc, &fakeSender{replies: make(chan chat.Reply, 8)}, ProgressOff, nil, nil)
	sess := mustSession(t, boundSession)

	if _, err := router.PrepareBind(context.Background(), "C0", sess); err == nil {
		t.Fatal("PrepareBind on a session the daemon 404s = nil, want an error")
	}
	if _, err := router.PrepareBind(context.Background(), "C0", sess); err != nil {
		t.Errorf("PrepareBind once the session is back: %v, want it bindable", err)
	}
}

// TestConcurrentTurnsInADeadBoundThreadAnnounceOnce covers the two things that
// go wrong when two messages arrive in a bound thread at once and the daemon
// has lost the session.
//
// The thread hears about it once, not once per message. And the entry's stop —
// written by the goroutine that created it, read by whichever goroutine
// discards it — is published by the close of ready like the session itself is;
// a stop read as nil leaves the relay reconnecting forever against a session
// that will never exist again. The slow logf widens that window: with the
// write moved back after close(ready), this test reports a data race under
// -race rather than failing an assertion.
func TestConcurrentTurnsInADeadBoundThreadAnnounceOnce(t *testing.T) {
	d := &boundDaemon{injectStatus: http.StatusNotFound}
	srv := httptest.NewServer(d.mux(t))
	t.Cleanup(srv.Close)
	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	slow := func(string, ...any) { time.Sleep(2 * time.Millisecond) }
	router := NewRouter(dc, fake, ProgressOff, nil, slow)
	router.CommitBind("C0:1", mustSession(t, boundSession), 5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msg := chat.Message{Conversation: "C0:1", Caller: "alice@example.com", Text: "what happened?"}
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			defer func() { done <- struct{}{} }()
			if err := router.Handle(ctx, msg); err == nil {
				t.Error("Handle succeeded against a session the daemon does not have")
			}
		}()
	}
	for range 2 {
		<-done
	}

	select {
	case rep := <-fake.replies:
		if !strings.Contains(rep.Text, boundSession) {
			t.Errorf("notice = %q, want the session named", rep.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the thread was told nothing")
	}
	select {
	case rep := <-fake.replies:
		t.Errorf("the thread was told twice; the second notice was %q", rep.Text)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestCommitBindWillNotTakeAThreadFromAnotherSession. PrepareBind checked the
// conversation the caller *addressed*; the binding is recorded under the one
// the message landed in, and those differ whenever a post creates its thread.
// If the landed thread already belongs to another session, taking it would
// strand that session's caller — its thread would start answering to someone
// else — so the bind is dropped and said out loud instead.
func TestCommitBindWillNotTakeAThreadFromAnotherSession(t *testing.T) {
	var logged strings.Builder
	router, _ := boundRouter(t, &boundDaemon{})
	router.logf = func(format string, args ...any) {
		fmt.Fprintf(&logged, format+"\n", args...)
	}
	first, second := mustSession(t, boundSession), mustSession(t, "core-agent/incident-9")

	router.CommitBind("C0:1", first, 5)
	router.CommitBind("C0:1", second, 9)

	router.mu.Lock()
	got := router.bindings["C0:1"]
	other := router.boundTo[sessionRef(second)]
	router.mu.Unlock()
	if got.sess != first {
		t.Errorf("binding = %+v, want the session that had the thread first", got)
	}
	if other != "" {
		t.Errorf("boundTo[%s] = %q, want the refused bind recorded nowhere", sessionRef(second), other)
	}
	if !strings.Contains(logged.String(), "already belongs to") {
		t.Errorf("logs did not say why the bind was dropped:\n%s", logged.String())
	}
}

// TestAdoptionResumesFromTheHeadNowNotTheBind. The bind measures a head, and
// then nothing relays it until a human replies — which for an incident feed is
// hours later, with the agent working the whole time. Resuming from the bind
// would replay every one of those turns into the thread at once: the same wall
// of transcript the bind measured a head to avoid, moved from before the bind
// to between the bind and the reply.
func TestAdoptionResumesFromTheHeadNowNotTheBind(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 8)}
	d.publish(agentFrame(5, "it is the readiness probe"))
	router, fake := boundRouter(t, d)
	sess := mustSession(t, boundSession)

	head, err := router.PrepareBind(context.Background(), "C0", sess)
	if err != nil {
		t.Fatalf("PrepareBind: %v", err)
	}
	router.CommitBind("C0:100.1", sess, head)

	// The hours between the alert and someone reading it.
	d.publish(
		agentFrame(6, "draining node 3"),
		agentFrame(7, "rollout paused"),
		agentFrame(8, "waiting on the operator"),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Handle(ctx, chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "status?"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	d.publish(agentFrame(9, "the operator is you"))

	// Connections in order: the bind's probe, adoption's probe, the relay.
	var since []string
	for range 3 {
		select {
		case s := <-d.resumed:
			since = append(since, s)
		case <-time.After(2 * time.Second):
			t.Fatalf("connections so far: %v; the relay never subscribed", since)
		}
	}
	if got := since[2]; got != "8" {
		t.Errorf("relay resumed from since=%s, want 8: the work done between the bind and the reply "+
			"is the agent's, not the thread's", got)
	}

	select {
	case rep := <-fake.replies:
		if rep.Text != "the operator is you" {
			t.Errorf("first thing in the thread was %q, want only what happened after the reply", rep.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the thread heard nothing")
	}
}

// TestAnEvictedBindingCanBeBoundAgainWhileItsThreadIsLive. Eviction drops the
// record, not the thread: the entry goes on relaying the session it adopted.
// Refusing the caller who could put the record back — because the map it was
// checked against is the one eviction emptied — would make the thread
// permanently unrecoverable, which is the opposite of what a bounded map is
// allowed to cost.
func TestAnEvictedBindingCanBeBoundAgainWhileItsThreadIsLive(t *testing.T) {
	d := &boundDaemon{}
	router, _ := boundRouter(t, d)
	sess := mustSession(t, boundSession)
	router.CommitBind("C0:1", sess, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "alice@example.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// What eviction does to the maps, without the thousand bindings it takes to
	// get there.
	router.mu.Lock()
	router.unbind("C0:1")
	router.mu.Unlock()

	if _, err := router.PrepareBind(ctx, "C0:1", sess); err != nil {
		t.Fatalf("re-binding an evicted thread that is still relaying that very session: %v", err)
	}
	router.CommitBind("C0:1", sess, 3)
	router.mu.Lock()
	defer router.mu.Unlock()
	if got, ok := router.bindings["C0:1"]; !ok || got.sess != sess {
		t.Errorf("bindings[C0:1] = %+v, %v; want the binding back", got, ok)
	}
}

// TestARelayThatLosesItsSessionSaysSoAndStops. The other way this failure
// arrives: not a message that cannot be delivered, but a stream that ends in a
// 404 while nobody is typing. Reconnecting would poll forever for something
// that is never coming back, and a thread quietly waiting on an agent that no
// longer exists reads exactly like one waiting on an agent that is thinking.
func TestARelayThatLosesItsSessionSaysSoAndStops(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 8), gone: make(chan struct{})}
	router, fake := boundRouter(t, d)
	router.minBackoff = time.Millisecond
	sess := mustSession(t, boundSession)
	router.CommitBind("C0:1", sess, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "alice@example.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Wait for the relay itself, so the session goes missing under a stream
	// that is open rather than before one ever was.
	for range 2 {
		select {
		case <-d.resumed:
		case <-time.After(2 * time.Second):
			t.Fatal("the relay never subscribed")
		}
	}
	close(d.gone)

	select {
	case rep := <-fake.replies:
		if !strings.Contains(rep.Text, boundSession) {
			t.Errorf("notice = %q, want the session named", rep.Text)
		}
		if !strings.Contains(rep.Text, "Nothing further") {
			t.Errorf("notice = %q; nothing was undelivered here — what is lost is what the thread was waiting for", rep.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the thread was never told its session was gone")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		router.mu.Lock()
		bindings, sessions := len(router.bindings), len(router.sessions)
		router.mu.Unlock()
		if bindings == 0 && sessions == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bindings=%d sessions=%d, want both dropped so the next message plainly starts fresh",
				bindings, sessions)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
