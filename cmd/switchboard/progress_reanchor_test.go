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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// gateBound bounds every park below. A test that fails an assertion never
// reaches the close that opens its gate, and httptest.Server.Close waits on
// handlers rather than cancelling them — so an unbounded park turns a one-line
// failure into one that runs out the package timeout ten minutes later.
const gateBound = 5 * time.Second

// A turn that speaks before it is finished. The narration is a completed,
// non-partial agent event — the wire tells it apart from the answer by one
// thing only, that turn-complete has not landed yet.
const (
	narrationEvent      = `{"seq":1,"event":{"Content":{"parts":[{"text":"let me check the logs…"}],"role":"model"},"Partial":false}}`
	narratedAnswerEvent = `{"seq":2,"event":{"Content":{"parts":[{"text":"the answer"}],"role":"model"},"Partial":false}}`
	narratedComplete    = `{"prompt_id":"p-1","model":"gemini-3.7-flash","latency_ms":1200}`

	capsWithBoundary = `{"protocol_version":"1.5.0","server":"core-agent/0.9.2",` +
		`"event_types":["stream-chunk","tool-call","status-update","usage-update","turn-complete","turn-error"]}`
	capsWithoutBoundary = `{"protocol_version":"1.4.0","server":"core-agent/0.8.0",` +
		`"event_types":["stream-chunk","tool-call","status-update","usage-update","turn-error"]}`
)

// narrationDaemon serves one turn in three beats so a test can look at the
// thread between them: the capabilities frame at stream open, the narration
// once narrate closes, and the turn's end once answer closes.
func narrationDaemon(t *testing.T, caps string, narrate, answer <-chan struct{}) *daemon.Client {
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
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func(name, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
			w.(http.Flusher).Flush()
		}
		w.(http.Flusher).Flush()
		flush(daemon.EventCapabilities, caps)
		select {
		case <-narrate:
		case <-req.Context().Done():
			return
		case <-time.After(gateBound):
			return
		}
		flush(daemon.EventAgent, narrationEvent)
		select {
		case <-answer:
		case <-req.Context().Done():
			return
		case <-time.After(gateBound):
			return
		}
		flush(daemon.EventTurnComplete, narratedComplete)
		flush(daemon.EventAgent, narratedAnswerEvent)
		<-req.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return dc
}

// TestNarrationReanchorsThePlaceholder is #42's first half. A model turn that
// talks on its way to an answer used to be delivered as the answer: the
// placeholder was deleted, the clock stopped and the turn marked done, so the
// rest of the turn — often the long part — ran with nothing in the thread
// saying so. It should instead move the placeholder below the narration and
// carry on counting.
func TestNarrationReanchorsThePlaceholder(t *testing.T) {
	narrate, answer := make(chan struct{}), make(chan struct{})
	dc := narrationDaemon(t, capsWithBoundary, narrate, answer)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 5*time.Millisecond)

	if got := handleOne(t, r, ctx, fake).Text; got != workingText {
		t.Fatalf("first post = %q, want the placeholder %q", got, workingText)
	}
	e := entryFor(t, r)

	// Age the turn past a minute so the re-anchored clock has to prove it
	// carried the original start time across rather than restarting at 0s.
	rewindTurn(e, 61*time.Second)

	close(narrate)
	if got := recvReply(t, fake.replies).Text; got != "let me check the logs…" {
		t.Fatalf("second post = %q, want the narration", got)
	}

	// The placeholder moves rather than retiring: a fresh one below the
	// narration, the original deleted, and the same clock still running.
	moved := recvReply(t, fake.replies)
	if !strings.HasPrefix(moved.Text, workingText+" 1m") {
		t.Fatalf("third post = %q, want a re-anchored placeholder still counting from the turn's start", moved.Text)
	}
	if moved.Kind != chat.KindProgress {
		t.Errorf("re-anchored placeholder Kind = %v, want KindProgress", moved.Kind)
	}
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts1") },
		"the narration did not retire the original placeholder")
	if !e.turnInFlight() {
		t.Error("narration ended the turn; the answer is still owed")
	}
	if got := progressID(e); got != "ts3" {
		t.Errorf("entry tracks progress message %q, want the re-anchored ts3", got)
	}
	// The ticker was never restarted, so if it is still editing it is the
	// original one — now writing into the message that replaced its target.
	waitFor(t, func() bool {
		edits := fake.updatedCalls()
		return len(edits) > 0 && edits[len(edits)-1].ref.ID == "ts3"
	}, "the clock stopped at the re-anchor, or kept editing the deleted placeholder")

	close(answer)
	if got := recvReply(t, fake.replies).Text; got != "the answer" {
		t.Fatalf("fourth post = %q, want the answer", got)
	}
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts3") },
		"the answer did not retire the re-anchored placeholder")
	waitFor(t, func() bool { return !e.turnInFlight() }, "the answer left the turn in flight")
}

// TestUnreadableTurnCompleteStillEndsTheTurn covers the difference between a
// boundary arriving and a boundary being legible. daemon.TurnCompleted reports
// !ok for a frame naming neither a model nor a latency as much as for one that
// will not parse; deriving "the turn is over" from that parse would read the
// answer behind such a frame as narration and re-anchor a placeholder
// underneath it, with the ticker already stopped and nothing left to retire it.
func TestUnreadableTurnCompleteStillEndsTheTurn(t *testing.T) {
	var subscribes atomic.Int64
	turn := make(chan struct{})
	dc := scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		if !opened(turn) { // let the placeholder land first, so it is there to be mishandled
			return false
		}
		send(daemon.EventTurnComplete, `{}`)
		send(daemon.EventAgent, narratedAnswerEvent)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 0)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)
	close(turn)

	if got := recvReply(t, fake.replies).Text; got != "the answer" {
		t.Fatalf("second post = %q, want the answer", got)
	}
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts1") },
		"an empty turn-complete left the answer looking like narration")
	waitFor(t, func() bool { return !e.turnInFlight() }, "the turn was left in flight")
	if got := progressID(e); got != "" {
		t.Errorf("entry still tracks a progress message %q; a placeholder was re-anchored under the answer", got)
	}
	noFurtherPost(t, fake)
}

// TestABoundaryLostToAReconnectStillEndsTheTurn is the other way the absence of
// a turn-complete lies. Lifecycle frames carry no seq, so one emitted during a
// stream outage is gone for good — while the answer behind it is replayed from
// seq on resume. Reading that absence as "still running" would park a live
// clock under a delivered answer and leave the entry in flight, which is what
// arms a stream-lost notice for a turn that was answered an hour ago.
func TestABoundaryLostToAReconnectStillEndsTheTurn(t *testing.T) {
	var subscribes atomic.Int64
	turn := make(chan struct{})
	dc := scriptedDaemon(t, &subscribes, func(n int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		if n == 1 {
			// Drop only once the turn is under way, so it is the turn that
			// outlives the connection. Its boundary is emitted into the gap and
			// never reaches switchboard; the answer behind it is replayed.
			opened(turn)
			return false
		}
		send(daemon.EventAgent, narratedAnswerEvent)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 0)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)
	close(turn)

	if got := recvReply(t, fake.replies).Text; got != "the answer" {
		t.Fatalf("second post = %q, want the answer", got)
	}
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts1") },
		"a turn that outlived its connection still trusted the missing boundary")
	waitFor(t, func() bool { return !e.turnInFlight() }, "the turn was left in flight")
	if got := progressID(e); got != "" {
		t.Errorf("entry still tracks a progress message %q; a placeholder was re-anchored under the answer", got)
	}
	noFurtherPost(t, fake)
}

// TestCapabilitiesAreRereadOnEveryStream pins where the capabilities frame is
// recorded. The logging around it is deliberately once per session — every
// reconnect repeats the frame, and a session that reconnects for a week should
// not repeat the line — but what the daemon can do is read on every turn, and a
// reconnect can land on a different build. Here the first stream does not
// advertise turn-complete and the second does; the turn that runs on the second
// must get the re-anchor.
func TestCapabilitiesAreRereadOnEveryStream(t *testing.T) {
	var subscribes atomic.Int64
	narrate := make(chan struct{})
	dc := scriptedDaemon(t, &subscribes, func(n int64, send func(name, data string)) bool {
		if n == 1 {
			send(daemon.EventCapabilities, capsWithoutBoundary)
			return false // drop straight away; no turn has been asked for yet
		}
		send(daemon.EventCapabilities, capsWithBoundary)
		if !opened(narrate) {
			return false
		}
		send(daemon.EventAgent, narrationEvent)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 0)

	// Reach the session without running a turn on the first stream, and wait
	// for the reconnect so the turn belongs wholly to the second one.
	e, err := r.session(ctx, "C0:100.1", "C0", "alice@example.com")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	waitFor(t, func() bool { return subscribes.Load() >= 2 }, "the relay never reconnected")
	waitFor(t, e.signalsEnd.Load, "the second stream's capabilities frame was never read")

	handleOne(t, r, ctx, fake)
	close(narrate)
	if got := recvReply(t, fake.replies).Text; got != "let me check the logs…" {
		t.Fatalf("second post = %q, want the narration", got)
	}
	if got := recvReply(t, fake.replies).Text; !strings.HasPrefix(got, workingText) {
		t.Fatalf("third post = %q, want a re-anchored placeholder", got)
	}
}

// TestNarrationEndsTheTurnWithoutATurnBoundary pins the fallback. A daemon that
// does not advertise turn-complete gives switchboard no way to tell narration
// from an answer, and guessing "not the answer" would re-anchor a placeholder
// after the last thing the turn ever says — leaving a clock running forever.
// Against that daemon the pre-#42 behaviour is the safe one.
func TestNarrationEndsTheTurnWithoutATurnBoundary(t *testing.T) {
	narrate, answer := make(chan struct{}), make(chan struct{})
	dc := narrationDaemon(t, capsWithoutBoundary, narrate, answer)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := errorRouter(t, dc, fake, time.Hour)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)

	close(narrate)
	if got := recvReply(t, fake.replies).Text; got != "let me check the logs…" {
		t.Fatalf("second post = %q, want the narration", got)
	}
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts1") },
		"the placeholder outlived the text that was treated as the answer")

	close(answer)
	if got := recvReply(t, fake.replies).Text; got != "the answer" {
		t.Fatalf("third post = %q, want the answer", got)
	}
	// Nothing was re-anchored, so nothing but the original was ever posted as
	// progress: three sends in total, one delete.
	noFurtherPost(t, fake)
	if refs := fake.deletedRefs(); len(refs) != 1 {
		t.Errorf("deleted %d messages, want only the original placeholder: %+v", len(refs), refs)
	}
	if got := progressID(e); got != "" {
		t.Errorf("entry still tracks a progress message %q", got)
	}
}

// TestABoundaryThatIsNotTurnCompleteRetiresTheReanchor is the gap between what
// the capabilities frame promises and what a given turn does. signalsEnd says
// "this daemon build sends turn-complete", which is a statement about the
// build, not a guarantee about every turn: the idle status-update is the
// boundary for anything that ends outside turn-complete and turn-error, and the
// relay has a branch for exactly that. A turn that ends there has its answer
// read as narration — unavoidable, the answer really is indistinguishable — so
// what must not also happen is the placeholder being re-anchored *under* that
// answer and then frozen there by the boundary.
func TestABoundaryThatIsNotTurnCompleteRetiresTheReanchor(t *testing.T) {
	var subscribes atomic.Int64
	turn := make(chan struct{})
	dc := scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		if !opened(turn) {
			return false
		}
		// No turn-complete anywhere: this turn ends the other way.
		send(daemon.EventAgent, narratedAnswerEvent)
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 5*time.Millisecond)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)
	close(turn)

	if got := recvReply(t, fake.replies).Text; got != "the answer" {
		t.Fatalf("second post = %q, want the answer", got)
	}
	waitFor(t, func() bool { return !e.turnInFlight() }, "the idle boundary did not end the turn")
	waitFor(t, func() bool { return progressID(e) == "" },
		"a clock was left under the answer by a turn that never sent turn-complete")
	waitFor(t, func() bool { return len(fake.deletedRefs()) == 2 },
		"the re-anchored placeholder was never deleted")
}

// TestNarrationWithNoAnswerBehindItRetiresItsPlaceholder is the same hazard from
// the other side: a turn that speaks and then ends with nothing more to say.
// The turn boundaries deliberately freeze the placeholder rather than delete
// it, because for a silent turn it is the only record the question was heard —
// but this turn has spoken, so the record exists and the freeze would leave a
// stopped clock sitting under the text.
func TestNarrationWithNoAnswerBehindItRetiresItsPlaceholder(t *testing.T) {
	var subscribes atomic.Int64
	turn := make(chan struct{})
	dc := scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		if !opened(turn) {
			return false
		}
		send(daemon.EventAgent, narrationEvent)
		send(daemon.EventTurnComplete, narratedComplete)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 5*time.Millisecond)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)
	close(turn)

	if got := recvReply(t, fake.replies).Text; got != "let me check the logs…" {
		t.Fatalf("second post = %q, want the narration", got)
	}
	if got := recvReply(t, fake.replies).Text; !strings.HasPrefix(got, workingText) {
		t.Fatalf("third post = %q, want a re-anchored placeholder", got)
	}
	waitFor(t, func() bool { return !e.turnInFlight() }, "turn-complete did not end the turn")
	waitFor(t, func() bool { return progressID(e) == "" },
		"the turn ended with its re-anchored clock still under the narration")
	waitFor(t, func() bool { return len(fake.deletedRefs()) == 2 },
		"the re-anchored placeholder was never deleted")
}

// TestATurnLeftOpenWithNoTickerIsStillGivenUp covers the two progress modes
// that have no placeholder and so no ticker. Narration leaves the turn in
// flight on purpose — that is what keeps the stream-lost notice armed for a
// turn that spoke and then lost its stream — but off and stream have nothing
// running that would ever take it back out, and an entry stuck in flight
// announces a lost turn at the next unrelated outage, hours later.
func TestATurnLeftOpenWithNoTickerIsStillGivenUp(t *testing.T) {
	var subscribes atomic.Int64
	dc := scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		send(daemon.EventAgent, narrationEvent)
		return true // and then the turn simply never ends
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r := NewRouter(dc, fake, ProgressStream, nil, func(string, ...any) {})
	r.minBackoff, r.maxBackoff = 5*time.Millisecond, 10*time.Millisecond
	r.streamGrace = time.Hour
	r.tickInterval = 0
	r.tickMaxAge = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	msg := chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "ping"}
	if err := r.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := recvReply(t, fake.replies).Text; got != "let me check the logs…" {
		t.Fatalf("first post = %q, want the narration; stream mode posts no placeholder", got)
	}
	e := entryFor(t, r)
	if !e.turnInFlight() {
		t.Fatal("narration ended the turn; the rest of it is still owed")
	}
	waitFor(t, func() bool { return !e.turnInFlight() },
		"the turn was left in flight with nothing running that could ever end it")
}

// TestTheBackstopEndsOnlyTheTurnItWasArmedFor pins the identity check on the
// backstop's timer. It is armed for up to an hour, which is long enough for the
// thread to have moved on to another turn — and ending *that* turn would take
// its stream-lost notice down with it, for a failure it had nothing to do with.
func TestTheBackstopEndsOnlyTheTurnItWasArmedFor(t *testing.T) {
	e := &sessionEntry{}
	e.beginTurnInFlight()
	armed := e.turnSeq.Load()

	e.endTurn() // the turn ends on its own, the way nearly all of them do
	e.beginTurnInFlight()
	if e.endTurnIf(armed) {
		t.Fatal("a backstop armed for the previous turn ended the one now running")
	}
	if !e.turnInFlight() {
		t.Fatal("the turn now running was ended by the previous turn's backstop")
	}
	if !e.endTurnIf(e.turnSeq.Load()) {
		t.Fatal("the current turn's own backstop was refused")
	}
}

// TestReplaceProgressRefusesAStaleSwap covers the window the re-anchor cannot
// close: it has to post the replacement before it can offer one, and a second
// turn can begin while that post is in the air. Swapping blindly would leak the
// new turn's placeholder — the ticker would keep writing to a message no reply
// is ever going to retire.
func TestReplaceProgressRefusesAStaleSwap(t *testing.T) {
	e := &sessionEntry{}
	old := chat.MessageRef{Conversation: "C0:1", ID: "ts1"}
	e.beginTurn(old, time.Now())

	// A later turn takes the slot, exactly as it would while the re-anchor's
	// Send was in flight.
	next := chat.MessageRef{Conversation: "C0:1", ID: "ts9"}
	e.beginTurn(next, time.Now())

	if e.replaceProgress(old, chat.MessageRef{Conversation: "C0:1", ID: "ts5"}) {
		t.Fatal("a stale re-anchor was allowed to overwrite the current turn's placeholder")
	}
	if got := progressRef(e); got != next {
		t.Fatalf("progress message = %+v, wanted the newer turn's %+v", got, next)
	}
	if !e.replaceProgress(next, chat.MessageRef{Conversation: "C0:1", ID: "ts10"}) {
		t.Fatal("a current re-anchor was refused")
	}
	if got := progressID(e); got != "ts10" {
		t.Fatalf("progress message = %q, want the re-anchored ts10", got)
	}

	// An entry with nothing outstanding has nothing to swap: takeProgress is
	// what a delivered answer does, and a re-anchor racing it must not put a
	// placeholder back. The swap it offers is a real one — the ref it read
	// before the answer landed — not a zero.
	current := progressRef(e)
	e.takeProgress()
	if e.replaceProgress(current, chat.MessageRef{Conversation: "C0:1", ID: "ts11"}) {
		t.Fatal("a re-anchor resurrected the placeholder of a turn that had ended")
	}
	if got := progressID(e); got != "" {
		t.Fatalf("progress message = %q, want none: the turn had ended", got)
	}
	// And a caller with nothing to swap *from* is refused too, which is what
	// keeps a re-anchor that never read a placeholder from inventing one.
	if e.replaceProgress(chat.MessageRef{}, chat.MessageRef{Conversation: "C0:1", ID: "ts12"}) {
		t.Fatal("a zero placeholder was accepted as the thing being replaced")
	}
}

// noFurtherPost fails if anything else reaches the thread. Several tests here
// turn on a message *not* being posted — a placeholder re-anchored under the
// answer is the whole failure mode #42's guards exist to prevent — and that is
// not something an assertion on what did arrive can catch.
func noFurtherPost(t *testing.T, fake *fakeSender) {
	t.Helper()
	select {
	case extra := <-fake.replies:
		t.Fatalf("a further post %q reached the thread; the turn was over", extra.Text)
	case <-time.After(100 * time.Millisecond):
	}
}

// opened reports whether the test opened the gate within gateBound.
func opened(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(gateBound):
		return false
	}
}

// progressRef is progressID for a test that needs the whole ref.
func progressRef(e *sessionEntry) chat.MessageRef {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	return e.progressMsg
}

// rewindTurn ages the turn in flight, so a test can assert on a clock without
// waiting out the time it reads.
func rewindTurn(e *sessionEntry, d time.Duration) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	e.turnStart = e.turnStart.Add(-d)
}

// narrationRouter wires a router in indicator mode to a fake daemon, with a
// tick interval of the test's choosing (0 for no ticker) and a stream grace
// long enough that no test here trips the lost-stream notice.
func narrationRouter(t *testing.T, dc *daemon.Client, out sender, tick time.Duration) (*Router, context.Context) {
	t.Helper()
	r := NewRouter(dc, out, ProgressIndicator, nil, func(string, ...any) {})
	r.minBackoff, r.maxBackoff = 5*time.Millisecond, 10*time.Millisecond
	r.streamGrace = time.Hour
	r.tickInterval = tick
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return r, ctx
}
