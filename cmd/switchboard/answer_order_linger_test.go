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

// Regression tests from review of the answer-order fix: paths that left a
// turn settled after it ended, where the next turn's first narration would
// then read as its end (the turn cut short, its placeholder deleted).

package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

const (
	evB       = `{"seq":3,"event":{"Content":{"parts":[{"text":"auto-continue text B"}],"role":"model"},"Partial":false}}`
	usage2    = `{"tokens_in_total":9000,"tokens_out_total":100,"cost_usd_total":0.002,"turns_total":2}`
	complete2 = `{"prompt_id":"p-2","model":"gemini-3.7-flash","tokens_in":3208,"tokens_out":52,"latency_ms":900}`
)

func drain(fake *fakeSender, d time.Duration) []chat.Reply {
	var out []chat.Reply
	deadline := time.After(d)
	for {
		select {
		case r := <-fake.replies:
			out = append(out, r)
		case <-deadline:
			return out
		}
	}
}

// Empty turn-complete payload on answer-first daemon: markSettled branch never schedules a foot.
func TestAnUnreadableTurnCompleteStillSettlesTheTurn(t *testing.T) {
	var subscribes atomic.Int64
	dc := scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		send(daemon.EventUsage, orderUsageBaseline)
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		send(daemon.EventUsage, orderUsageAfter)
		send(daemon.EventAgent, narratedAnswerEvent)
		send(daemon.EventTurnComplete, `{"prompt_id":"p-1"}`)
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 16)}
	r, ctx := narrationRouter(t, dc, fitsAllSender{fake}, time.Hour)
	r.footDelay = 50 * time.Millisecond
	handleOne(t, r, ctx, fake)
	drain(fake, 400*time.Millisecond)
	if e := entryFor(t, r); e.awaitingAnswer() {
		t.Errorf("empty turn-complete: settled lingers (awaitingAnswer true) after answer-first turn")
	}
}

// Answer-first, then an auto_continue turn whose text lands within footDelay.
func TestANextTurnInsideTheFootDelayLeavesNothingSettled(t *testing.T) {
	var subscribes atomic.Int64
	dc := scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		send(daemon.EventUsage, orderUsageBaseline)
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		send(daemon.EventUsage, orderUsageAfter)
		send(daemon.EventAgent, narratedAnswerEvent)
		send(daemon.EventTurnComplete, orderComplete)
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		// auto_continue turn
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		time.Sleep(50 * time.Millisecond)
		send(daemon.EventUsage, usage2)
		send(daemon.EventAgent, evB)
		send(daemon.EventTurnComplete, complete2)
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 16)}
	r, ctx := narrationRouter(t, dc, fitsAllSender{fake}, time.Hour)
	r.setShowUsage(true)
	r.footDelay = 300 * time.Millisecond
	handleOne(t, r, ctx, fake)
	for _, rp := range drain(fake, 1200*time.Millisecond) {
		t.Logf("post %q usage=%+v", rp.Text, rp.Usage)
	}
	for _, u := range fake.updatedCalls() {
		t.Logf("update %q usage=%+v", u.text, u.reply.Usage)
	}
	e := entryFor(t, r)
	t.Logf("awaiting=%v inflight=%v", e.awaitingAnswer(), e.turnInFlight())
	if e.awaitingAnswer() {
		t.Errorf("after auto-continue: settled lingers for the next prompt")
	}
}

// Lingering settled (empty turn-complete) makes the next prompt's narration end its turn.
func TestALeftoverSettledTurnCannotEndTheNextTurnOnNarration(t *testing.T) {
	var subscribes atomic.Int64
	dc := scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		send(daemon.EventUsage, orderUsageBaseline)
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		send(daemon.EventUsage, orderUsageAfter)
		send(daemon.EventAgent, narratedAnswerEvent)
		send(daemon.EventTurnComplete, `{"prompt_id":"p-1"}`)
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		time.Sleep(700 * time.Millisecond)
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		send(daemon.EventAgent, evB) // turn 2 narration (no boundary yet)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 16)}
	r, ctx := narrationRouter(t, dc, fitsAllSender{fake}, time.Hour)
	r.footDelay = 50 * time.Millisecond
	handleOne(t, r, ctx, fake)
	drain(fake, 400*time.Millisecond)
	handleOne(t, r, ctx, fake)
	for _, rp := range drain(fake, 600*time.Millisecond) {
		t.Logf("post %q", rp.Text)
	}
	e := entryFor(t, r)
	if !e.turnInFlight() {
		t.Errorf("turn 2 narration ended turn 2 (inFlight=false); deleted=%d", len(fake.deletedCalls()))
	}
}

// slowDeleteSender makes every Delete take d, the way a slow or rate-limited
// platform call holds up the relay goroutine that issued it.
type slowDeleteSender struct {
	fitsAllSender
	d time.Duration
}

func (s slowDeleteSender) Delete(ctx context.Context, ref chat.MessageRef) error {
	time.Sleep(s.d)
	return s.fitsAllSender.Delete(ctx, ref)
}

// A boundary-first answer the stream holds back longer than footDelay is still
// the turn's end, not narration: it must not re-anchor a stopped clock below
// itself and leave it there. Its footer is lost — the quiet boundary was
// settled without it — which is the documented edge.
func TestAnAnswerSlowerThanTheFootDelayStillEndsTheTurn(t *testing.T) {
	var subscribes atomic.Int64
	dc := scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		send(daemon.EventUsage, orderUsageBaseline)
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		send(daemon.EventUsage, orderUsageAfter)
		send(daemon.EventTurnComplete, orderComplete)
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		time.Sleep(300 * time.Millisecond)
		send(daemon.EventAgent, narratedAnswerEvent)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 16)}
	r, ctx := narrationRouter(t, dc, fitsAllSender{fake}, time.Hour)
	r.setShowUsage(true)
	r.footDelay = 100 * time.Millisecond

	handleOne(t, r, ctx, fake)
	posts := drain(fake, 800*time.Millisecond)
	for _, p := range posts {
		if p.Kind == chat.KindProgress {
			t.Errorf("posted progress %q after the answer: a stopped clock re-anchored and stranded", p.Text)
		}
	}
	if len(fake.deletedCalls()) != 1 {
		t.Errorf("deleted %d messages, want the turn's placeholder taken down once", len(fake.deletedCalls()))
	}
}

// The boundary's own platform calls are slow — a Delete taking longer than
// footDelay — but the daemon sends the answer straight behind the boundary.
// The quiet timer counts from after that handling, and the answer's arrival
// decides first, so the answer (not the narration) carries the footer.
func TestASlowRelayDoesNotHandTheFooterToNarration(t *testing.T) {
	var subscribes atomic.Int64
	dc := scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		send(daemon.EventUsage, orderUsageBaseline)
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		send(daemon.EventAgent, narrationEvent)
		send(daemon.EventUsage, orderUsageAfter)
		send(daemon.EventTurnComplete, orderComplete)
		send(daemon.EventAgent, narratedAnswerEvent)
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 16)}
	r, ctx := narrationRouter(t, dc, slowDeleteSender{fitsAllSender{fake}, 300 * time.Millisecond}, time.Hour)
	r.setShowUsage(true)
	r.footDelay = 100 * time.Millisecond

	handleOne(t, r, ctx, fake)
	var answer *chat.Reply
	for _, p := range drain(fake, 1500*time.Millisecond) {
		if p.Text == "the answer" {
			p := p
			answer = &p
		}
	}
	if answer == nil || answer.Usage == nil || answer.Usage.TokensOut != 48 {
		t.Errorf("answer = %+v, want it posted with this turn's footer", answer)
	}
	for _, u := range fake.updatedCalls() {
		if u.reply.Usage != nil {
			t.Errorf("edited %q to carry the footer; it belonged to the answer", u.text)
		}
	}
}

// Answer-first, then auto_continue's empty user-role prompt and its text, all
// faster than footDelay. The user-role event settles the first turn on its
// answer at once; the continuation's text does not take that turn's usage.
func TestAUserRoleEventSettlesTheAnswerFirstTurn(t *testing.T) {
	var subscribes atomic.Int64
	dc := scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		send(daemon.EventUsage, orderUsageBaseline)
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		send(daemon.EventUsage, orderUsageAfter)
		send(daemon.EventAgent, narratedAnswerEvent)
		send(daemon.EventTurnComplete, orderComplete)
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		send(daemon.EventAgent, `{"seq":3,"event":{"Content":{"parts":[],"role":"user"}}}`)
		send(daemon.EventUsage, usage2)
		send(daemon.EventAgent, `{"seq":4,"event":{"Content":{"parts":[{"text":"auto-continue text B"}],"role":"model"},"Partial":false}}`)
		send(daemon.EventTurnComplete, complete2)
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 16)}
	r, ctx := narrationRouter(t, dc, fitsAllSender{fake}, time.Hour)
	r.setShowUsage(true)
	r.footDelay = time.Second // longer than the whole script

	handleOne(t, r, ctx, fake)
	posts := drain(fake, 600*time.Millisecond)
	waitFor(t, func() bool {
		for _, u := range fake.updatedCalls() {
			if u.text == "the answer" && u.reply.Usage != nil {
				return true
			}
		}
		return false
	}, "the first answer to be footed when the user-role event arrived")
	for _, p := range posts {
		if p.Text == "auto-continue text B" && p.Usage != nil && p.Usage.TokensOut == 48 {
			t.Errorf("the continuation took the first turn's footer: %+v", p.Usage)
		}
	}
}
