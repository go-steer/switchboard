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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// The frames a backlog test plays. Two turns' worth: the first answers while a
// second message is already queued behind it, which is #42's second case.
const (
	backlogQueued    = `{"state":"queued","prompt_id":"p-2","queued_at":"2026-09-04T10:00:00Z"}`
	backlogDequeued  = `{"state":"dequeued","prompt_id":"p-2"}`
	backlogComplete  = `{"prompt_id":"t-1","model":"gemini-3.7-flash","latency_ms":900}`
	backlogAnswer1   = `{"seq":1,"event":{"Content":{"parts":[{"text":"first answer"}],"role":"model"},"Partial":false}}`
	backlogAnswer2   = `{"seq":2,"event":{"Content":{"parts":[{"text":"second answer"}],"role":"model"},"Partial":false}}`
	backlogNarration = `{"seq":1,"event":{"Content":{"parts":[{"text":"let me check"}],"role":"model"},"Partial":false}}`
	backlogTurnError = `{"kind":"model_error","code":"UNAVAILABLE","message":"upstream 503","retryable":true}`
	statusIdle       = `{"turn_state":"idle"}`
	statusStreaming  = `{"turn_state":"streaming"}`
)

// frame is one server-sent event a fed daemon will write.
type frame struct{ name, data string }

// fedDaemon is scriptedDaemon driven a frame at a time, so a test can establish
// a backlog and then look at the thread at the exact moment an answer lands
// rather than after the whole script has run. Only the first subscribe is fed;
// a reconnect gets the capabilities frame and then nothing.
func fedDaemon(t *testing.T, feed <-chan frame) *daemon.Client {
	t.Helper()
	var subscribes atomic.Int64
	return scriptedDaemon(t, &subscribes, func(n int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		if n > 1 {
			return true
		}
		for {
			select {
			case f, ok := <-feed:
				if !ok {
					return true
				}
				send(f.name, f.data)
			case <-time.After(gateBound):
				return true
			}
		}
	})
}

// TestBacklogKeepsThePlaceholderForTheNextTurn is #42's second case. A thread
// with a second message already queued behind the turn that is answering must
// not have its placeholder retired by that answer: the queued message gets a
// turn of its own, and the single progress slot is the only one it has. The
// answer should re-anchor the placeholder below itself instead, exactly as
// mid-turn narration does.
func TestBacklogKeepsThePlaceholderForTheNextTurn(t *testing.T) {
	feed := make(chan frame)
	dc := fedDaemon(t, feed)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 5*time.Millisecond)

	if got := handleOne(t, r, ctx, fake).Text; got != workingText {
		t.Fatalf("first post = %q, want the placeholder %q", got, workingText)
	}
	e := entryFor(t, r)

	// A second message lands on the daemon's inbox while the first turn runs.
	feed <- frame{daemon.EventInbox, backlogQueued}
	waitFor(t, e.backlogged, "the queued inbox event did not reach the backlog")

	// Age the turn so the re-anchored clock has to prove it kept the original
	// start rather than restarting at 0s.
	rewindTurn(e, 61*time.Second)

	// The first turn finishes and answers.
	feed <- frame{daemon.EventTurnComplete, backlogComplete}
	feed <- frame{daemon.EventAgent, backlogAnswer1}
	if got := recvReply(t, fake.replies).Text; got != "first answer" {
		t.Fatalf("second post = %q, want the first answer", got)
	}

	// The placeholder moved rather than retiring, and kept its clock.
	moved := recvReply(t, fake.replies)
	if !strings.HasPrefix(moved.Text, workingText+" 1m") {
		t.Fatalf("third post = %q, want a re-anchored placeholder still counting from the turn's start", moved.Text)
	}
	if moved.Kind != chat.KindProgress {
		t.Errorf("re-anchored placeholder Kind = %v, want KindProgress", moved.Kind)
	}
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts1") },
		"the answer did not retire the original placeholder on its way to re-anchoring")
	if got := progressID(e); got != "ts3" {
		t.Errorf("entry tracks progress message %q, want the re-anchored ts3", got)
	}

	// The queued message is taken up. The placeholder it inherits was left
	// frozen by the previous turn's boundary, and picking the message up is
	// what starts its clock again.
	feed <- frame{daemon.EventInbox, backlogDequeued}
	waitFor(t, func() bool { return !e.backlogged() },
		"the dequeued inbox event did not empty the backlog")
	waitFor(t, e.turnInFlight, "picking up the queued message did not put a turn back in flight")
	waitFor(t, func() bool {
		edits := fake.updatedCalls()
		return len(edits) > 0 && edits[len(edits)-1].ref.ID == "ts3"
	}, "the inherited placeholder has no running clock")
	if got := progressID(e); got != "ts3" {
		t.Errorf("the resumed turn tracks progress message %q, want the inherited ts3", got)
	}

	// Answered, with nothing behind it: this one is the thread's last word.
	feed <- frame{daemon.EventTurnComplete, backlogComplete}
	feed <- frame{daemon.EventAgent, backlogAnswer2}
	if got := recvReply(t, fake.replies).Text; got != "second answer" {
		t.Fatalf("fourth post = %q, want the second answer", got)
	}
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts3") },
		"the last answer did not retire the re-anchored placeholder")
	waitFor(t, func() bool { return !e.turnInFlight() },
		"the last answer left the turn in flight")
}

// TestIdleResynchronisesAStaleBacklog covers the one thing replay cannot
// repair. Inbox events carry no seq, so a dequeued emitted while the stream was
// down is gone for good — and the id it would have retired would otherwise sit
// in the backlog forever, re-anchoring a placeholder after every answer the
// session ever gives. An idle daemon has drained its inbox, which is the fact
// that puts the set back in step.
func TestIdleResynchronisesAStaleBacklog(t *testing.T) {
	feed := make(chan frame)
	dc := fedDaemon(t, feed)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 0)

	if got := handleOne(t, r, ctx, fake).Text; got != workingText {
		t.Fatalf("first post = %q, want the placeholder", got)
	}
	e := entryFor(t, r)

	// A queue event whose matching dequeue never arrives.
	feed <- frame{daemon.EventInbox, backlogQueued}
	waitFor(t, e.backlogged, "the queued inbox event did not reach the backlog")

	// The daemon reports a turn running and then going idle, which is its own
	// statement that nothing is left waiting.
	feed <- frame{daemon.EventStatusUpdate, statusStreaming}
	feed <- frame{daemon.EventStatusUpdate, statusIdle}
	waitFor(t, func() bool { return !e.backlogged() },
		"an idle daemon left a stale id in the backlog")

	// So the next answer is treated as the thread's last word again.
	feed <- frame{daemon.EventTurnComplete, backlogComplete}
	feed <- frame{daemon.EventAgent, backlogAnswer1}
	if got := recvReply(t, fake.replies).Text; got != "first answer" {
		t.Fatalf("second post = %q, want the answer", got)
	}
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts1") },
		"the answer did not retire the placeholder after the backlog resynchronised")
}

// TestBacklogIgnoresUnreadableInboxEvents pins the direction an unparseable
// frame fails in. A backlog that grows on a frame it did not understand strands
// a placeholder for the life of the session; one that ignores it behaves as the
// build did before #42.
func TestBacklogIgnoresUnreadableInboxEvents(t *testing.T) {
	feed := make(chan frame)
	dc := fedDaemon(t, feed)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 0)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)

	for _, bad := range []string{`{`, `{"state":"queued"}`, `{"prompt_id":"p-9"}`, `{"state":"pondered","prompt_id":"p-9"}`} {
		feed <- frame{daemon.EventInbox, bad}
	}
	// Nothing to wait for — the assertion is an absence — so drive one readable
	// frame through behind them and use its effect as the barrier.
	feed <- frame{daemon.EventInbox, backlogQueued}
	waitFor(t, e.backlogged, "the readable inbox event did not reach the backlog")
	feed <- frame{daemon.EventInbox, backlogDequeued}
	waitFor(t, func() bool { return !e.backlogged() },
		"an unreadable or unknown-state inbox event was counted into the backlog")
}

// TestBacklogSetToleratesReplay is the property that lets the backlog survive a
// reconnect at all. Inbox events cannot be deduplicated — they carry no seq —
// so the same queued and dequeued pair is simply seen again, and a set has to
// come out where a counter would drift.
func TestBacklogSetToleratesReplay(t *testing.T) {
	e := &sessionEntry{}
	q := daemon.InboxChange{State: daemon.InboxQueued, PromptID: "p-1"}
	d := daemon.InboxChange{State: daemon.InboxDequeued, PromptID: "p-1"}

	e.noteInbox(q)
	e.noteInbox(q)
	if !e.backlogged() {
		t.Fatal("a queued message is not in the backlog")
	}
	e.noteInbox(d)
	if e.backlogged() {
		t.Error("one dequeue did not clear a message queued twice; the set is counting")
	}
	e.noteInbox(d)
	if e.backlogged() {
		t.Error("a replayed dequeue put something back in the backlog")
	}
	// A dequeue seen before its queue — the order a resume can deliver them in
	// when the queue fell before the last seq and the dequeue after it.
	e.noteInbox(daemon.InboxChange{State: daemon.InboxDequeued, PromptID: "p-2"})
	if e.backlogged() {
		t.Error("dequeuing an unknown id created a backlog entry")
	}
}

// TestFollowUpKeepsTheRunningTurnsAccounting is the footer half of #42's second
// case. A second message in a thread whose first is still working used to run
// Handle's unconditional resetUsage, discarding the accounting banked for the
// turn that was about to answer: that answer posted bare, and the follow-up's
// footer covered both turns' spend.
//
// It has to go through Handle to be a test of anything. The bug was never in
// the bank — noteTotals and takeUsage were always right — it was in who emptied
// it and when, so a test that drives sessionEntry directly passes on the
// pre-fix code.
func TestFollowUpKeepsTheRunningTurnsAccounting(t *testing.T) {
	feed := make(chan frame)
	dc := fedDaemon(t, feed)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 0)
	r.setShowUsage(true)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)

	// The running turn banks its spend. The first frame is the baseline the
	// session arrived at; the growth over it is what this turn cost.
	feed <- frame{daemon.EventUsage, usageTotals(1000, 10, 0.001, 1)}
	feed <- frame{daemon.EventUsage, usageTotals(6000, 20, 0.003, 2)}

	// A second message lands behind it. The inbox event is the daemon's own
	// account of the queueing, and doubles as the barrier proving the two usage
	// frames were read before the follow-up runs: one stream, read in order.
	feed <- frame{daemon.EventInbox, backlogQueued}
	waitFor(t, e.backlogged, "the queued inbox event did not reach the backlog")
	if !e.turnInFlight() {
		t.Fatal("the first turn is not in flight, so this is not the case under test")
	}
	followUp := chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "and another thing"}
	if err := r.Handle(ctx, followUp); err != nil {
		t.Fatalf("follow-up Handle: %v", err)
	}

	// The first turn answers, and the footer it carries is its own.
	feed <- frame{daemon.EventTurnComplete, backlogComplete}
	feed <- frame{daemon.EventAgent, backlogAnswer1}
	answer := recvMatching(t, fake.replies, "first answer")
	if answer.Usage == nil {
		t.Fatal("the answer posted bare: the follow-up discarded the running turn's accounting")
	}
	if answer.Usage.TokensIn != 5000 || answer.Usage.TokensOut != 10 {
		t.Errorf("footer = %d in / %d out, want the first turn's own 5000 / 10",
			answer.Usage.TokensIn, answer.Usage.TokensOut)
	}
}

// TestIdleAfterAnOrdinaryTurnResynchronisesAStaleBacklog is the resync on the
// path it actually has to work on. The turn that just ended took its boundary
// from turn-complete, which is the normal case and the one that leaves the
// connection's "the daemon said working" flag clear — so a resync gated on that
// flag never runs where it matters, and the stale id sits there re-anchoring a
// placeholder after every answer the session gives from then on.
func TestIdleAfterAnOrdinaryTurnResynchronisesAStaleBacklog(t *testing.T) {
	feed := make(chan frame)
	dc := fedDaemon(t, feed)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 0)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)

	// A queued message whose matching dequeue is lost to a stream outage.
	feed <- frame{daemon.EventInbox, backlogQueued}
	waitFor(t, e.backlogged, "the queued inbox event did not reach the backlog")

	// The turn ends the way turns ordinarily end: turn-complete, the answer,
	// then the status-update trailing them. No frame ever said "working".
	feed <- frame{daemon.EventTurnComplete, backlogComplete}
	feed <- frame{daemon.EventAgent, backlogAnswer1}
	recvMatching(t, fake.replies, "first answer")

	feed <- frame{daemon.EventStatusUpdate, statusIdle}
	waitFor(t, func() bool { return !e.backlogged() },
		"an idle daemon left a stale id in the backlog after an ordinary turn")
}

// TestBoundaryAfterNarrationKeepsTheQueuedTurnsPlaceholder is #42's second case
// reached through the placeholder cleanup rather than through the answer. A
// turn that narrates has spoken, and a turn that has spoken gets its
// placeholder deleted at its boundary rather than frozen — which is right when
// the thread is going quiet and wrong when a message is queued behind it,
// because that message's turn is left with no clock at all.
func TestBoundaryAfterNarrationKeepsTheQueuedTurnsPlaceholder(t *testing.T) {
	feed := make(chan frame)
	dc := fedDaemon(t, feed)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 0)

	handleOne(t, r, ctx, fake) // ts1
	e := entryFor(t, r)

	feed <- frame{daemon.EventInbox, backlogQueued}
	waitFor(t, e.backlogged, "the queued inbox event did not reach the backlog")

	// The turn says something on its way: the placeholder moves below it (ts3)
	// and the turn is marked as having spoken.
	feed <- frame{daemon.EventAgent, backlogNarration}
	recvMatching(t, fake.replies, "let me check")
	waitFor(t, func() bool { return progressID(e) == "ts3" },
		"the narration did not re-anchor the placeholder")

	// Then the boundary lands, and the dequeued behind it hands the queued
	// message its turn. Same stream, so the dequeued doubles as the barrier
	// proving the boundary was handled first.
	feed <- frame{daemon.EventTurnComplete, backlogComplete}
	feed <- frame{daemon.EventInbox, backlogDequeued}
	waitFor(t, e.turnInFlight, "picking up the queued message did not put a turn back in flight")

	if got := progressID(e); got != "ts3" {
		t.Errorf("the queued turn tracks progress message %q, want the inherited ts3", got)
	}
	if containsRefID(fake.deletedRefs(), "ts3") {
		t.Error("the boundary deleted the placeholder the queued turn had to inherit")
	}
}

// TestTurnErrorKeepsTheQueuedTurnsPlaceholder is the same case on the failure
// path. A turn that dies is still a turn the next one is queued behind, and
// retiring the single progress slot on the way out leaves that next turn
// running with nothing on screen. The notice goes in below the placeholder and
// the placeholder is moved back under it, frozen until the queued turn starts.
func TestTurnErrorKeepsTheQueuedTurnsPlaceholder(t *testing.T) {
	feed := make(chan frame)
	dc := fedDaemon(t, feed)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 0)

	handleOne(t, r, ctx, fake) // ts1
	e := entryFor(t, r)

	feed <- frame{daemon.EventInbox, backlogQueued}
	waitFor(t, e.backlogged, "the queued inbox event did not reach the backlog")

	feed <- frame{daemon.EventTurnError, backlogTurnError}
	if notice := recvReply(t, fake.replies); notice.Kind != chat.KindNotice { // ts2
		t.Fatalf("second post Kind = %v, want the failure notice", notice.Kind)
	}
	moved := recvReply(t, fake.replies) // ts3
	if moved.Kind != chat.KindProgress || !strings.HasPrefix(moved.Text, workingText) {
		t.Fatalf("third post = %q (%v), want the placeholder re-anchored below the notice", moved.Text, moved.Kind)
	}
	waitFor(t, func() bool { return progressID(e) == "ts3" },
		"the failed turn did not keep a placeholder for the message queued behind it")

	feed <- frame{daemon.EventInbox, backlogDequeued}
	waitFor(t, e.turnInFlight, "picking up the queued message did not put a turn back in flight")
	if got := progressID(e); got != "ts3" {
		t.Errorf("the queued turn tracks progress message %q, want the inherited ts3", got)
	}
	if containsRefID(fake.deletedRefs(), "ts3") {
		t.Error("the failure deleted the placeholder the queued turn had to inherit")
	}
}

// TestFollowUpBetweenTheBoundaryAndTheAnswerKeepsTheAccounting closes the rest
// of the window the footer guard has to cover. turn-complete ends the turn
// before the frame carrying that turn's text, so for the moment between the two
// the entry reads as idle while holding the figures the answer is about to
// print — and a follow-up handled in there discarded them.
//
// The symptom is worse than a missing footer: with the bank emptied, the
// turn-complete is gone too, so the answer behind it reads as narration and the
// thread gets a clock re-anchored underneath a delivered reply.
func TestFollowUpBetweenTheBoundaryAndTheAnswerKeepsTheAccounting(t *testing.T) {
	feed := make(chan frame)
	dc := fedDaemon(t, feed)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 0)
	r.setShowUsage(true)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)

	feed <- frame{daemon.EventUsage, usageTotals(1000, 10, 0.001, 1)}
	feed <- frame{daemon.EventUsage, usageTotals(6000, 20, 0.003, 2)}
	feed <- frame{daemon.EventTurnComplete, backlogComplete}
	waitFor(t, e.awaitingAnswer, "turn-complete did not bank the turn's boundary")
	if e.turnInFlight() {
		t.Fatal("the turn still reads as in flight, so this is not the window under test")
	}

	followUp := chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "and another thing"}
	if err := r.Handle(ctx, followUp); err != nil {
		t.Fatalf("follow-up Handle: %v", err)
	}

	feed <- frame{daemon.EventAgent, backlogAnswer1}
	answer := recvMatching(t, fake.replies, "first answer")
	if answer.Usage == nil {
		t.Fatal("the answer posted bare: the follow-up discarded the finished turn's accounting")
	}
	if answer.Usage.TokensIn != 5000 || answer.Usage.TokensOut != 10 {
		t.Errorf("footer = %d in / %d out, want the first turn's own 5000 / 10",
			answer.Usage.TokensIn, answer.Usage.TokensOut)
	}
}

// TestIdleDropsTheDeadTurnsAccounting is the other half of that guard: what the
// bank must lose. A turn whose only boundary is the daemon going idle produced
// no answer, so nothing will ever carry what it spent — turn-error drops those
// figures already, and the idle boundary has to agree, or the next turn's
// footer bills the two together.
//
// Handle is not what proves it. A message arriving through Handle clears the
// bank on its own; the turn here is picked up off the daemon's own inbox, which
// is the path that has nothing else to fall back on.
func TestIdleDropsTheDeadTurnsAccounting(t *testing.T) {
	feed := make(chan frame)
	dc := fedDaemon(t, feed)
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fake, 0)
	r.setShowUsage(true)

	handleOne(t, r, ctx, fake)
	e := entryFor(t, r)

	// A turn that spends 5000 and then dies with neither a turn-complete nor a
	// turn-error: the status-update going idle is the only boundary it gets.
	feed <- frame{daemon.EventUsage, usageTotals(1000, 10, 0.001, 1)}
	feed <- frame{daemon.EventUsage, usageTotals(6000, 20, 0.003, 2)}
	feed <- frame{daemon.EventStatusUpdate, statusStreaming}
	feed <- frame{daemon.EventStatusUpdate, statusIdle}
	waitFor(t, func() bool { return !e.turnInFlight() }, "the idle status-update did not end the turn")

	// The next turn comes off the daemon's inbox and answers, spending 500.
	feed <- frame{daemon.EventInbox, backlogDequeued}
	feed <- frame{daemon.EventUsage, usageTotals(6500, 25, 0.004, 3)}
	feed <- frame{daemon.EventTurnComplete, backlogComplete}
	feed <- frame{daemon.EventAgent, backlogAnswer1}
	answer := recvMatching(t, fake.replies, "first answer")
	if answer.Usage == nil {
		t.Fatal("the answer posted bare")
	}
	if answer.Usage.TokensIn != 500 || answer.Usage.TokensOut != 5 {
		t.Errorf("footer = %d in / %d out, want this turn's own 500 / 5; the dead turn's spend was carried forward",
			answer.Usage.TokensIn, answer.Usage.TokensOut)
	}
}

// recvMatching drains replies until one carries the given text, so a test can
// assert on an answer without also counting the placeholders posted around it.
func recvMatching(t *testing.T, ch <-chan chat.Reply, text string) chat.Reply {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var seen []string
	for {
		select {
		case rep := <-ch:
			if rep.Text == text {
				return rep
			}
			seen = append(seen, rep.Text)
		case <-deadline:
			t.Fatalf("no reply with text %q; saw %q", text, seen)
		}
	}
}
