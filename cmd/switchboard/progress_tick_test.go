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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

func TestFormatElapsed(t *testing.T) {
	for _, tt := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{400 * time.Millisecond, "0s"},
		{1500 * time.Millisecond, "2s"},
		{45 * time.Second, "45s"},
		{59500 * time.Millisecond, "1m00s"}, // rounds up across the minute
		{90 * time.Second, "1m30s"},
		{9 * time.Minute, "9m00s"},
		{59*time.Minute + 59*time.Second, "59m59s"},
		{time.Hour, "1h00m"},
		{67 * time.Minute, "1h07m"},
		{25 * time.Hour, "25h00m"},
		{-time.Second, "0s"}, // a clock skew must not print a negative age
	} {
		if got := formatElapsed(tt.in); got != tt.want {
			t.Errorf("formatElapsed(%s) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTickText(t *testing.T) {
	for _, tt := range []struct {
		name    string
		elapsed time.Duration
		tools   []string
		step    int
		want    string
	}{
		{"clock only", 45 * time.Second, nil, 0, "⏳ Working… 45s"},
		{"with tool", 150 * time.Second, []string{"bash"}, 7, "⏳ Working… 2m30s · running `bash` (step 7)"},
		{"parallel tools", 5 * time.Second, []string{"read", "grep"}, 1,
			"⏳ Working… 5s · running `read`, `grep` (step 1)"},
	} {
		if got := tickText(tt.elapsed, tt.tools, tt.step); got != tt.want {
			t.Errorf("%s: tickText = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestTickRenderScopesDetailToMode checks the clock is shown in both modes but
// the tool detail only in status: indicator is documented as the lightweight
// placeholder, and naming tools there would quietly turn it into status mode.
func TestTickRenderScopesDetailToMode(t *testing.T) {
	e := &sessionEntry{}
	e.beginTurn(chat.MessageRef{ID: "ts1"}, time.Now().Add(-90*time.Second))
	if _, _, ok := e.noteActivity([]string{"bash"}); !ok {
		t.Fatal("noteActivity on a live turn reported no message to edit")
	}

	_, status, ok := e.tickRender(ProgressStatus)
	if !ok {
		t.Fatal("tickRender(status) on a live turn = !ok")
	}
	if !strings.Contains(status, "1m30s") || !strings.Contains(status, "running `bash` (step 1)") {
		t.Errorf("status tick = %q, want the clock and the tool", status)
	}
	_, indicator, ok := e.tickRender(ProgressIndicator)
	if !ok {
		t.Fatal("tickRender(indicator) on a live turn = !ok")
	}
	if want := "⏳ Working… 1m30s"; indicator != want {
		t.Errorf("indicator tick = %q, want %q", indicator, want)
	}
}

// TestTickRenderAfterTurnEnds checks a tick that fires just as the reply lands
// renders nothing: the progress message has been deleted by then, and editing
// it would either fail or resurrect a placeholder below the answer.
func TestTickRenderAfterTurnEnds(t *testing.T) {
	e := &sessionEntry{}
	e.beginTurn(chat.MessageRef{ID: "ts1"}, time.Now())
	if ref := e.takeProgress(); ref.ID != "ts1" {
		t.Fatalf("takeProgress = %q, want ts1", ref.ID)
	}
	if _, _, ok := e.tickRender(ProgressStatus); ok {
		t.Error("tickRender after the turn ended = ok, want the tick dropped")
	}
	if _, _, ok := e.noteActivity([]string{"bash"}); ok {
		t.Error("noteActivity after the turn ended = ok, want a fallback notice instead")
	}
	// Retiring twice — clearProgress on a turn whose ticker already stopped —
	// must not double-close the stop channel.
	e.takeProgress()
	e.stopTicker()
}

// TestBeginTurnRetiresThePreviousTicker checks a second turn starting before
// the first has replied leaves only one ticker running: two would race to
// rewrite the same message with two different clocks.
func TestBeginTurnRetiresThePreviousTicker(t *testing.T) {
	e := &sessionEntry{}
	_, first := e.beginTurn(chat.MessageRef{ID: "ts1"}, time.Now())
	stale, second := e.beginTurn(chat.MessageRef{ID: "ts2"}, time.Now())
	if stale.ID != "ts1" {
		t.Errorf("stale = %q, want the first turn's message ts1", stale.ID)
	}
	select {
	case <-first:
	default:
		t.Error("the first turn's ticker was not stopped")
	}
	select {
	case <-second:
		t.Error("the second turn's ticker was stopped as well")
	default:
	}
}

// progressID reads the entry's live progress message under its lock.
func progressID(e *sessionEntry) string {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	return e.progressMsg.ID
}

// tickRouter wires a router with a fast ticker straight to a fake sender, with
// no daemon in the way: the clock is driven by startProgress and the turn
// boundary is applied by hand, so these tests do not have to race an SSE
// stream to observe it.
func tickRouter(t *testing.T, mode ProgressMode, interval time.Duration, out sender) (*Router, *sessionEntry, context.Context) {
	t.Helper()
	r := NewRouter(nil, out, mode, nil, nil)
	r.tickInterval = interval
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return r, &sessionEntry{}, ctx
}

// TestTickerAdvancesTheClock is the point of #37: a turn that makes no tool
// calls still has to visibly move, or a wedged turn and a thinking one look
// exactly alike.
func TestTickerAdvancesTheClock(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 4)}
	r, e, ctx := tickRouter(t, ProgressIndicator, 5*time.Millisecond, fake)

	r.startProgress(ctx, e, "C0:1")
	if got := recvReply(t, fake.replies); got.Text != workingText {
		t.Fatalf("placeholder = %q, want %q", got.Text, workingText)
	}
	waitFor(t, func() bool {
		n := 0
		for _, u := range fake.updatedCalls() {
			if u.ref.ID == "ts1" && strings.HasPrefix(u.text, workingText+" ") {
				n++
			}
		}
		return n >= 2
	}, "the progress message was never re-rendered with a clock")

	// Re-rendering is not enough on its own: at a 5ms interval every tick
	// rounds to "0s", so the assertion above also passes against a clock that
	// never moves. Rewind the turn's start and require the number to follow.
	e.pmu.Lock()
	e.turnStart = e.turnStart.Add(-61 * time.Second)
	e.pmu.Unlock()
	waitFor(t, func() bool {
		for _, u := range fake.updatedCalls() {
			if u.ref.ID == "ts1" && strings.HasPrefix(u.text, workingText+" 1m") {
				return true
			}
		}
		return false
	}, "the clock never caught up with the elapsed minute")
}

// TestTickerStopsWhenTheTurnCompletes checks the daemon's turn boundary halts
// the clock. Without it a turn that ends with no answer to deliver — an error,
// an interrupt (#34) — would tick forever, and a dead turn would look like the
// busiest thing in the channel.
func TestTickerStopsWhenTheTurnCompletes(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 4)}
	r, e, ctx := tickRouter(t, ProgressIndicator, 5*time.Millisecond, fake)

	r.startProgress(ctx, e, "C0:1")
	recvReply(t, fake.replies)
	waitFor(t, func() bool { return len(fake.updatedCalls()) >= 2 }, "the ticker never started")

	e.stopTicker()
	time.Sleep(50 * time.Millisecond) // let any tick already in flight land
	settled := len(fake.updatedCalls())
	time.Sleep(50 * time.Millisecond) // ten more intervals' worth
	if got := len(fake.updatedCalls()); got != settled {
		t.Errorf("edits went %d → %d after the turn completed; the clock did not stop", settled, got)
	}
	// The message itself survives, for the reply to retire.
	if progressID(e) != "ts1" {
		t.Error("stopTicker retired the progress message; only the reply should")
	}
}

// TestTickerOffByInterval pins the zero value: a Router built without going
// through NewRouter has no interval, and must simply not tick rather than spin
// on a zero-length timer.
func TestTickerOffByInterval(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 4)}
	r, e, ctx := tickRouter(t, ProgressIndicator, 0, fake)

	r.startProgress(ctx, e, "C0:1")
	recvReply(t, fake.replies)
	time.Sleep(50 * time.Millisecond)
	if got := fake.updatedCalls(); len(got) != 0 {
		t.Errorf("ticker disabled but the message was edited %d times: %+v", len(got), got)
	}
}

// TestTickerGivesUpOnATurnThatNeverEnds checks the backstop: turn-complete is
// the real boundary, but if the daemon loses a turn entirely the clock has to
// stop by itself rather than edit the channel until the process restarts.
func TestTickerGivesUpOnATurnThatNeverEnds(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 4)}
	r, e, ctx := tickRouter(t, ProgressIndicator, 5*time.Millisecond, fake)
	r.tickMaxAge = 40 * time.Millisecond

	r.startProgress(ctx, e, "C0:1")
	recvReply(t, fake.replies)
	waitFor(t, func() bool { return len(fake.updatedCalls()) >= 2 }, "the ticker never started")

	time.Sleep(80 * time.Millisecond) // past the backstop
	settled := len(fake.updatedCalls())
	time.Sleep(50 * time.Millisecond)
	if got := len(fake.updatedCalls()); got != settled {
		t.Errorf("edits went %d → %d past the backstop; the clock never gave up", settled, got)
	}
}

// TestTickerKeepsShowingTheLastTool checks the clock does not erase what the
// agent is doing: status mode has one message, and a tick that dropped back to
// a bare "Working…" would be a regression on the tool detail already shipped.
func TestTickerKeepsShowingTheLastTool(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 4)}
	r, e, ctx := tickRouter(t, ProgressStatus, 5*time.Millisecond, fake)

	r.startProgress(ctx, e, "C0:1")
	recvReply(t, fake.replies)
	r.postActivity(ctx, e, "C0:1", ProgressStatus, []daemon.ToolCall{{Name: "lookup"}})

	waitFor(t, func() bool {
		n := 0
		for _, u := range fake.updatedCalls() {
			if strings.Contains(u.text, "running `lookup` (step 1)") {
				n++
			}
		}
		return n >= 2 // the activity edit, plus at least one tick that kept it
	}, "the ticker dropped the tool activity from the status message")
}

// failingSender fails every in-place edit, standing in for a rate-limited or
// otherwise unhappy platform.
type failingSender struct {
	fakeSender
	updates atomic.Int64
}

func (f *failingSender) Update(context.Context, chat.MessageRef, chat.Reply) error {
	f.updates.Add(1)
	return errors.New("rate limited")
}

// TestTickerBacksOffOnFailure checks a failing edit neither takes the turn down
// nor turns into a hot retry loop: the interval doubles, so a rate-limited
// conversation is not answered with edits at the rate that provoked it.
func TestTickerBacksOffOnFailure(t *testing.T) {
	fake := &failingSender{fakeSender: fakeSender{replies: make(chan chat.Reply, 4)}}
	const interval = 5 * time.Millisecond
	r, e, ctx := tickRouter(t, ProgressIndicator, interval, fake)

	r.startProgress(ctx, e, "C0:1")
	recvReply(t, fake.replies)
	const window = 250 * time.Millisecond
	time.Sleep(window)

	got := fake.updates.Load()
	if got == 0 {
		t.Fatal("the ticker never tried an edit")
	}
	// Doubling from 5ms fits ~6 attempts into 250ms; a hot loop would fit 50.
	if flat := int64(window / interval); got >= flat/2 {
		t.Errorf("%d failed edits in %s — no backoff (a flat retry would be ~%d)", got, window, flat)
	}
	// The turn is unharmed: the message is still there for the reply to retire.
	if progressID(e) != "ts1" {
		t.Error("a failed tick lost the progress message")
	}
}
