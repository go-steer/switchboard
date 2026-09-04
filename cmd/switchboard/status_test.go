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

// entryFor returns the router's session entry for the conversation handleOne
// drives, once the relay has created it.
func entryFor(t *testing.T, r *Router) *sessionEntry {
	t.Helper()
	var e *sessionEntry
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		e = r.sessions["C0:100.1"]
		return e != nil
	}, "the session entry was never created")
	return e
}

// logSink collects a router's log lines for assertions about what an operator
// would see.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// matching returns the collected lines containing sub.
func (l *logSink) matching(sub string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			out = append(out, line)
		}
	}
	return out
}

// holdsProgress reports whether the entry still owns a progress message — the
// difference between stopping the clock on it and orphaning it.
func holdsProgress(e *sessionEntry) bool {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	return e.progressMsg.ID != ""
}

// TestStatusUpdateEndsATurnTheDaemonCouldNotClassify is why status-update is
// worth reading at all. The daemon ends a turn with turn-complete when it
// succeeded and turn-error when it failed in a way it could classify — but it
// emits status-update idle from the turn's cleanup on *every* exit path. A
// turn-error frame this build cannot read is logged and walked away from, which
// before this left the entry marked in flight and its clock running until the
// hour-long backstop, and made the next unrelated outage announce a failure for
// a turn that had ended long before.
func TestStatusUpdateEndsATurnTheDaemonCouldNotClassify(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false,
		[2]string{daemon.EventStatusUpdate, `{"turn_state":"streaming"}`},
		[2]string{daemon.EventTurnError, `{"retryable":true}`}, // says nothing: dropped
		[2]string{daemon.EventStatusUpdate, `{"turn_state":"idle"}`},
	)
	r, ctx := errorRouter(t, dc, fake, time.Hour)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)

	waitFor(t, func() bool { return !e.turnInFlight() }, "the turn was left in flight")

	// Ending the turn is not the same as answering it: nothing is posted, and
	// the placeholder stays up for a reply that may yet arrive.
	select {
	case extra := <-fake.replies:
		t.Errorf("status-update posted %q; it is a lifecycle event, not a message", extra.Text)
	case <-time.After(50 * time.Millisecond):
	}
	// Still up means still *owned*: a boundary that clears the entry's
	// reference instead of just stopping its clock leaves a "Working…" message
	// in the thread that nothing can ever edit or retire.
	if !holdsProgress(e) {
		t.Error("the boundary orphaned the placeholder; nothing can retire it now")
	}
	if got := fake.deletedRefs(); len(got) != 0 {
		t.Errorf("the placeholder was deleted by a lifecycle event: %v", got)
	}
}

// TestTurnErrorDisarmsTheTrailingIdle is the regression for a race this file's
// own feature introduced. The daemon emits turn-error and then, from the same
// cleanup, status-update idle. failTurn posts its notice synchronously on the
// relay goroutine, so the idle is processed a chat-API round trip later — and a
// follow-up posted inside that window would be ended by a boundary belonging to
// the turn before it: placeholder frozen, entry cleared, and the stream-lost
// backstop disarmed for a turn still owed.
func TestTurnErrorDisarmsTheTrailingIdle(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	gate := &gatedSender{fakeSender: fake, sending: make(chan struct{}), release: make(chan struct{})}
	dc, _ := errorDaemon(t, false,
		[2]string{daemon.EventStatusUpdate, `{"turn_state":"streaming"}`},
		[2]string{daemon.EventTurnError, `{"kind":"model_error","message":"upstream 503"}`},
		[2]string{daemon.EventStatusUpdate, `{"turn_state":"idle"}`},
	)
	r, ctx := errorRouter(t, dc, gate, time.Hour)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)

	// Hold the relay inside failTurn's Send, then start the next turn.
	<-gate.sending
	if err := r.Handle(ctx, chat.Message{
		Conversation: "C0:100.1", Caller: "alice@example.com", Text: "and again",
	}); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	if !e.turnInFlight() {
		t.Fatal("the second turn was never marked in flight")
	}
	close(gate.release)

	// The trailing idle now belongs to a turn that is already accounted for.
	time.Sleep(100 * time.Millisecond)
	if !e.turnInFlight() {
		t.Error("the previous turn's trailing idle ended the turn that replaced it")
	}
}

// gatedSender blocks the relay inside the one Send that failTurn makes, so a
// test can act in the window that Send holds open.
type gatedSender struct {
	*fakeSender
	once    sync.Once
	sending chan struct{}
	release chan struct{}
}

func (g *gatedSender) Send(ctx context.Context, r chat.Reply) (chat.MessageRef, error) {
	if r.Kind == chat.KindNotice {
		g.once.Do(func() {
			close(g.sending)
			<-g.release
		})
	}
	return g.fakeSender.Send(ctx, r)
}

// TestStatusUpdateBlockedIsNotATurnBoundary: awaiting_permission means the
// daemon has stopped on something only a human at its own console can answer.
// The turn is still owed, so reading "not streaming" as "over" would retire the
// thread's progress and mark the session idle while the agent waits.
func TestStatusUpdateBlockedIsNotATurnBoundary(t *testing.T) {
	sink := &logSink{}
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false,
		[2]string{daemon.EventStatusUpdate, `{"turn_state":"streaming"}`},
		[2]string{daemon.EventStatusUpdate, `{"turn_state":"awaiting_permission"}`},
	)
	r, ctx := errorRouter(t, dc, fake, time.Hour)
	r.logf = sink.logf

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)

	// Wait for the frame to have been seen, then check nothing was concluded.
	waitFor(t, func() bool { return len(sink.matching("waiting on a human")) == 1 },
		"the parked turn was never logged")
	if !e.turnInFlight() {
		t.Error("awaiting_permission ended the turn; it is a pause, not an ending")
	}
	if got := fake.deletedRefs(); len(got) != 0 {
		t.Errorf("the progress message was retired while the daemon waits: %v", got)
	}
}

// TestStatusUpdateIdleAtStreamOpenSparesALiveTurn: the daemon opens every
// stream with a status snapshot, and on a session between turns that snapshot
// says idle. A turn posted a moment earlier has not reached the daemon yet, so
// acting on that idle would retire its placeholder before it ever ran.
//
// The daemon here holds the snapshot back until the inject has landed, which is
// the ordering that makes the guard load-bearing rather than incidental.
func TestStatusUpdateIdleAtStreamOpenSparesALiveTurn(t *testing.T) {
	injected := make(chan struct{})
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"injected":"ok","session":"s1"}`)
		once.Do(func() { close(injected) })
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-injected:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "event: status-update\ndata: {\"turn_state\":\"idle\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := errorRouter(t, dc, fake, time.Hour)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)
	<-injected

	// The snapshot lands after the turn is in flight. Nothing about it says
	// this turn ended — the relay has not seen the daemon report it running.
	time.Sleep(100 * time.Millisecond)
	if !e.turnInFlight() {
		t.Error("an idle snapshot ended a turn the daemon had not started yet")
	}
	if got := fake.deletedRefs(); len(got) != 0 {
		t.Errorf("the placeholder was retired by a stream-open snapshot: %v", got)
	}
}

// TestCapabilitiesReportsWhatTheDaemonWillNotSend: switchboard's error notices,
// usage footers and progress boundaries each depend on an event an older daemon
// may not send, and the symptom of a missing one is absence — a quiet thread, a
// reply with no footer. Absence looks nothing like a version mismatch from the
// outside, so the mismatch is stated when the stream opens.
func TestCapabilitiesReportsWhatTheDaemonWillNotSend(t *testing.T) {
	sink := &logSink{}
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false, [2]string{daemon.EventCapabilities,
		`{"protocol_version":"1.1.0","server":"core-agent/0.4.0",` +
			`"event_types":["usage-update","turn-complete","stream-chunk","tool-call"]}`})
	r, ctx := errorRouter(t, dc, fake, time.Hour)
	r.logf = sink.logf

	handleOne(t, r, ctx, fake)

	waitFor(t, func() bool { return len(sink.matching("does not advertise")) == 1 },
		"the daemon's missing events were never reported")

	ident := sink.matching("connected to")
	if len(ident) != 1 {
		t.Fatalf("want one identity line, got %v", ident)
	}
	for _, want := range []string{"core-agent/0.4.0", "1.1.0"} {
		if !strings.Contains(ident[0], want) {
			t.Errorf("identity line = %q, want it to name %q", ident[0], want)
		}
	}

	warn := sink.matching("does not advertise")[0]
	for _, want := range []string{daemon.EventStatusUpdate, daemon.EventTurnError} {
		if !strings.Contains(warn, want) {
			t.Errorf("warning = %q, want it to name %q", warn, want)
		}
	}
	// Named only for what is actually absent, or the warning is noise.
	for _, bad := range []string{daemon.EventUsage, daemon.EventTurnComplete} {
		if strings.Contains(warn, bad) {
			t.Errorf("warning = %q, but the daemon does advertise %q", warn, bad)
		}
	}
	// The legacy agent event is never advertised by any conformant daemon, so
	// requiring it would fire this warning against every one of them.
	if strings.Contains(warn, `"`+daemon.EventAgent+`"`) || strings.Contains(warn, " agent") {
		t.Errorf("warning = %q, but the agent event is deliberately not advertised", warn)
	}
}

// TestStatusUpdateUnknownStateIsNotAStartOrAnEnd: the spec reserves the right
// to add turn states, so "not idle" must not be shorthand for "running". A
// build that reads a state it has never heard of as the start of a turn arms
// the boundary, and the next idle retires a turn it knows nothing about.
func TestStatusUpdateUnknownStateIsNotAStartOrAnEnd(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false,
		[2]string{daemon.EventStatusUpdate, `{"turn_state":"compacting"}`},
		[2]string{daemon.EventStatusUpdate, `{"turn_state":"idle"}`},
	)
	r, ctx := errorRouter(t, dc, fake, time.Hour)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)

	time.Sleep(100 * time.Millisecond)
	if !e.turnInFlight() {
		t.Error("a state this build does not know was read as a turn starting, and the idle after it ended the turn")
	}
}

// TestStatusUpdateIdleOnAReconnectSparesTheNextTurn: the flag that gates the
// idle boundary is per connection, so a reconnect's opening snapshot cannot
// retire a turn on the strength of a "streaming" seen on a stream that is gone.
// The cost is a turn that both started and ended inside the outage staying in
// flight — the same thing a turn-complete lost in that gap already does, and
// the safe direction: the other way retires a live turn's placeholder and
// disarms the stream-lost notice that covers the outage.
func TestStatusUpdateIdleOnAReconnectSparesTheNextTurn(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	var subscribes atomic.Int64
	dc := scriptedDaemon(t, &subscribes, func(n int64, send func(name, data string)) bool {
		if n == 1 {
			send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
			return false // drop the stream mid-turn
		}
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		return true
	})
	r, ctx := errorRouter(t, dc, fake, time.Hour)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)

	waitFor(t, func() bool { return subscribes.Load() >= 2 }, "the relay never reconnected")
	time.Sleep(100 * time.Millisecond)
	if !e.turnInFlight() {
		t.Error("a reconnect's opening snapshot ended a turn using a streaming seen on the previous stream")
	}
}

// TestStatusUpdateAfterABoundaryIsInert: the daemon sends status-update for
// reasons other than a turn changing state — a model swap, a permission-mode
// change — and those carry turn_state idle when the session is between turns.
// Once a boundary has been acted on the flag has to go back down, or the next
// such frame ends whatever turn has started since.
func TestStatusUpdateAfterABoundaryIsInert(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	secondTurn := make(chan struct{})
	var injects atomic.Int64
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"injected":"ok","session":"s1"}`)
		if injects.Add(1) == 2 {
			once.Do(func() { close(secondTurn) })
		}
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func(name, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
			w.(http.Flusher).Flush()
		}
		w.(http.Flusher).Flush()
		flush(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		flush(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		select {
		case <-secondTurn:
		case <-req.Context().Done():
			return
		}
		// Not a turn ending: the session is idle and something else about it
		// changed. The turn now in flight has not been reported running.
		flush(daemon.EventStatusUpdate, `{"turn_state":"idle","perm_mode":"yolo"}`)
		<-req.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	r, ctx := errorRouter(t, dc, fake, time.Hour)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)
	waitFor(t, func() bool { return !e.turnInFlight() }, "the first turn was left in flight")

	if err := r.Handle(ctx, chat.Message{
		Conversation: "C0:100.1", Caller: "alice@example.com", Text: "and again",
	}); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	<-secondTurn

	time.Sleep(100 * time.Millisecond)
	if !e.turnInFlight() {
		t.Error("a status-update sent while idle ended the turn that had started since the last boundary")
	}
}

// scriptedDaemon serves a session whose event stream is written by script, once
// per subscribe. script returns whether to hold the connection open.
func scriptedDaemon(t *testing.T, subscribes *atomic.Int64, script func(n int64, send func(name, data string)) bool) *daemon.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"injected":"ok","session":"s1"}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, req *http.Request) {
		n := subscribes.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		hold := script(n, func(name, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
			w.(http.Flusher).Flush()
		})
		if hold {
			<-req.Context().Done()
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return dc
}

// TestCapabilitiesWarningNamesOnlyWhatIsAbsent walks the rest of the frame: the
// warning has to name every event switchboard actually reads and nothing else,
// stay silent for a daemon that advertises them all, and still say something
// useful about a daemon that will not say what it is.
func TestCapabilitiesWarningNamesOnlyWhatIsAbsent(t *testing.T) {
	const everything = `["stream-chunk","tool-call","tool-result",` +
		`"status-update","usage-update","turn-complete","turn-error","inbox","pause"]`
	cases := []struct {
		name    string
		frame   string
		ident   []string
		missing []string
		silent  bool
	}{{
		name:   "a conformant daemon is not warned about",
		frame:  `{"protocol_version":"1.5.0","server":"core-agent/0.9.2","event_types":` + everything + `}`,
		ident:  []string{"core-agent/0.9.2", "1.5.0"},
		silent: true,
	}, {
		// Losing these is what #34, the usage footer and #42's backlog are
		// built on. The inbox pair is the quietest of them: without it a
		// thread whose second message arrives before the first has answered
		// silently goes back to losing a placeholder, which is a symptom
		// nobody would connect to a capabilities frame unaided.
		name: "a daemon with no accounting and no failure report",
		frame: `{"protocol_version":"1.5.0","server":"core-agent/0.9.2",` +
			`"event_types":["stream-chunk","tool-call","status-update"]}`,
		missing: []string{daemon.EventUsage, daemon.EventTurnComplete, daemon.EventTurnError, daemon.EventInbox},
	}, {
		// The loudest absence there is: no stream-chunk means no answers.
		name: "a daemon that will not stream the answer",
		frame: `{"protocol_version":"1.5.0","server":"core-agent/0.9.2",` +
			`"event_types":["status-update","usage-update","turn-complete","turn-error"]}`,
		missing: []string{daemon.EventStreamChunk, daemon.EventToolCall},
	}, {
		name:    "a daemon that says only what it sends",
		frame:   `{"event_types":["status-update"]}`,
		ident:   []string{"unidentified daemon", "an unstated protocol version"},
		missing: []string{daemon.EventStreamChunk, daemon.EventUsage},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &logSink{}
			fake := &fakeSender{replies: make(chan chat.Reply, 8)}
			// The literal event name, not the constant: this is the string on
			// the wire, and a test that reads it from the same constant the
			// router does would agree with any value at all.
			dc, _ := errorDaemon(t, false, [2]string{"capabilities", tc.frame})
			r, ctx := errorRouter(t, dc, fake, time.Hour)
			r.logf = sink.logf

			handleOne(t, r, ctx, fake)
			waitFor(t, func() bool { return len(sink.matching("connected to")) == 1 },
				"the daemon's identity was never logged")
			for _, want := range tc.ident {
				if !strings.Contains(sink.matching("connected to")[0], want) {
					t.Errorf("identity line = %q, want it to name %q",
						sink.matching("connected to")[0], want)
				}
			}

			if tc.silent {
				time.Sleep(50 * time.Millisecond)
				if got := sink.matching("does not advertise"); len(got) != 0 {
					t.Errorf("warned about a daemon that advertises everything: %v", got)
				}
				return
			}
			waitFor(t, func() bool { return len(sink.matching("does not advertise")) == 1 },
				"the missing events were never reported")
			warn := sink.matching("does not advertise")[0]
			for _, want := range tc.missing {
				if !strings.Contains(warn, want) {
					t.Errorf("warning = %q, want it to name the absent %q", warn, want)
				}
			}
		})
	}
}

// TestCapabilitiesLoggedOncePerSession: the frame arrives on every stream open,
// and a session that reconnects for a week must not repeat the same two lines
// each time.
func TestCapabilitiesLoggedOncePerSession(t *testing.T) {
	sink := &logSink{}
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	// ends: every subscribe replays the frame and then drops, so the relay
	// reconnects in a tight loop.
	dc, subscribes := planDaemon(t, streamPlan{
		events: [][2]string{{daemon.EventCapabilities,
			`{"protocol_version":"1.5.0","server":"core-agent/0.9.2","event_types":["usage-update"]}`}},
		ends: true,
	})
	r, ctx := errorRouter(t, dc, fake, time.Hour)
	r.logf = sink.logf

	handleOne(t, r, ctx, fake)

	waitFor(t, func() bool { return subscribes.Load() >= 4 }, "the relay stopped reconnecting")
	if got := sink.matching("connected to"); len(got) != 1 {
		t.Errorf("logged the daemon's identity %d times across %d streams: %v",
			len(got), subscribes.Load(), got)
	}
	if got := sink.matching("does not advertise"); len(got) != 1 {
		t.Errorf("repeated the capability warning %d times: %v", len(got), got)
	}
}

// TestRelayIgnoresUnreadableLifecycleFrames: a frame that says nothing produces
// nothing — no log spam beyond one line, and above all no turn boundary
// invented from a payload that could not be read.
func TestRelayIgnoresUnreadableLifecycleFrames(t *testing.T) {
	sink := &logSink{}
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false,
		[2]string{daemon.EventStatusUpdate, `{"turn_state":"streaming"}`},
		[2]string{daemon.EventStatusUpdate, `not json at all`},
		[2]string{daemon.EventStatusUpdate, `{"model":"gemini-3.7-flash"}`},
		[2]string{daemon.EventCapabilities, `{}`},
		[2]string{daemon.EventAgent, usageAnswerEvent},
	)
	r, ctx := errorRouter(t, dc, fake, time.Hour)
	r.logf = sink.logf

	handleOne(t, r, ctx, fake)
	if got := recvReply(t, fake.replies); got.Text != "pong" {
		t.Errorf("next post = %q (kind %q), want the answer", got.Text, got.Kind)
	}
	if got := len(sink.matching("unreadable status-update")); got != 2 {
		t.Errorf("logged %d unreadable status frames, want 2", got)
	}
	if got := len(sink.matching("unreadable capabilities")); got != 1 {
		t.Errorf("logged %d unreadable capability frames, want 1", got)
	}
	if got := sink.matching("connected to"); len(got) != 0 {
		t.Errorf("an empty capabilities frame was reported as an identity: %v", got)
	}
}

// TestStatusUpdateStopsTheClock pairs with #37: the turn boundary status-update
// reports has to retire the ticker, or a turn that ends without an answer keeps
// claiming to be working.
func TestStatusUpdateStopsTheClock(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false,
		[2]string{daemon.EventStatusUpdate, `{"turn_state":"streaming"}`},
		[2]string{daemon.EventStatusUpdate, `{"turn_state":"idle"}`},
	)
	r, ctx := errorRouter(t, dc, fake, time.Hour)
	r.tickInterval = 10 * time.Millisecond

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)
	waitFor(t, func() bool { return !e.turnInFlight() }, "the turn was left in flight")

	// Drain whatever ticked before the boundary, then check the clock is out.
	time.Sleep(30 * time.Millisecond)
	before := len(fake.updatedCalls())
	time.Sleep(60 * time.Millisecond)
	if after := len(fake.updatedCalls()); after != before {
		t.Errorf("the clock ticked %d more times after the daemon went idle", after-before)
	}
}
