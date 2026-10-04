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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// core-agent 2.10 sends a turn's answer before its turn-complete (measured
// live), where the daemon this was written against sent the boundary first.
// These pin the gateway against the newer order.

const (
	orderUsageBaseline = `{"tokens_in_total":0,"tokens_out_total":0,"cost_usd_total":0,"turns_total":0}`
	orderUsageAfter    = `{"tokens_in_total":5792,"tokens_out_total":48,"cost_usd_total":0.0013,"turns_total":1}`
	orderComplete      = `{"prompt_id":"p-1","model":"gemini-3.7-flash","tokens_in":5792,"tokens_out":48,"latency_ms":2731}`
)

// fitsAllSender is a fakeSender that reports every text fits one message, as
// both shipped adapters do for a short answer.
type fitsAllSender struct{ *fakeSender }

func (fitsAllSender) FitsOneMessage(string) bool { return true }

func answerFirstDaemon(t *testing.T) *daemon.Client {
	t.Helper()
	var subscribes atomic.Int64
	return scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		send(daemon.EventUsage, orderUsageBaseline)
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		send(daemon.EventUsage, orderUsageAfter)
		send(daemon.EventAgent, narratedAnswerEvent)  // the answer…
		send(daemon.EventTurnComplete, orderComplete) // …then its boundary
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		return true
	})
}

// The footer belongs on the answer even when the turn-complete carrying the
// usage arrives after it. It used to stay banked and ride the next text — the
// next prompt's answer, a turn late (seen live).
func TestTheFooterLandsOnTheAnswerWhenTheBoundaryFollowsIt(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, answerFirstDaemon(t), fitsAllSender{fake}, time.Hour)
	r.setShowUsage(true)
	r.footDelay = 50 * time.Millisecond

	handleOne(t, r, ctx, fake)
	answer := recvReply(t, fake.replies)
	if answer.Text != "the answer" {
		t.Fatalf("second post = %q, want the answer", answer.Text)
	}
	waitFor(t, func() bool {
		for _, u := range fake.updatedCalls() {
			if u.text == "the answer" && u.reply.Usage != nil {
				return true
			}
		}
		return false
	}, "the answer to be edited to carry its footer")
	for _, u := range fake.updatedCalls() {
		if u.text == "the answer" && u.reply.Usage != nil && u.reply.Usage.TokensOut != 48 {
			t.Errorf("footer usage = %+v, want this turn's (48 tokens out)", u.reply.Usage)
		}
	}
	e := entryFor(t, r)
	if usage, _ := e.takeUsage(); usage != nil {
		t.Errorf("usage still banked after it was footed: %+v — it would ride the next answer", usage)
	}
}

// An answer followed at once by its boundary must not post a placeholder and
// delete it a moment later. Re-anchoring waits, the boundary lands first, and
// the only progress message the turn ever posts is the first.
func TestAnAnswerAheadOfItsBoundaryDoesNotFlickerThePlaceholder(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, answerFirstDaemon(t), fake, time.Hour)
	r.reanchorDelay = 200 * time.Millisecond

	handleOne(t, r, ctx, fake)
	if got := recvReply(t, fake.replies); got.Text != "the answer" {
		t.Fatalf("second post = %q, want the answer", got.Text)
	}
	waitFor(t, func() bool { return len(fake.deletedCalls()) == 1 }, "the placeholder to be taken down")
	time.Sleep(3 * r.reanchorDelay) // past when a re-anchor would have fired
	select {
	case extra := <-fake.replies:
		t.Errorf("posted %q after the answer: a placeholder re-anchored only to be deleted", extra.Text)
	default:
	}
	if n := len(fake.deletedCalls()); n != 1 {
		t.Errorf("deleted %d messages, want only the turn's one placeholder", n)
	}
}

// The older, boundary-first order still works: in a tool turn the narration
// goes out first, then the turn-complete, then the answer. The answer — not
// the narration — carries the footer, and is delivered as the turn's end.
// Footing at the boundary straight away put the footer on the narration and
// read the answer as more narration (caught in review).
func TestTheFooterStaysOnTheAnswerWhenTheBoundaryComesFirst(t *testing.T) {
	var subscribes atomic.Int64
	dc := scriptedDaemon(t, &subscribes, func(_ int64, send func(name, data string)) bool {
		send(daemon.EventCapabilities, capsWithBoundary)
		send(daemon.EventUsage, orderUsageBaseline)
		send(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
		send(daemon.EventAgent, narrationEvent)
		send(daemon.EventUsage, orderUsageAfter)
		send(daemon.EventTurnComplete, orderComplete) // boundary first…
		send(daemon.EventAgent, narratedAnswerEvent)  // …then the answer
		send(daemon.EventStatusUpdate, `{"turn_state":"idle"}`)
		return true
	})
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, dc, fitsAllSender{fake}, time.Hour)
	r.setShowUsage(true)
	r.footDelay = 200 * time.Millisecond

	handleOne(t, r, ctx, fake)
	var answer chat.Reply
	waitFor(t, func() bool {
		for {
			select {
			case got := <-fake.replies:
				if got.Text == "the answer" {
					answer = got
					return true
				}
			default:
				return false
			}
		}
	}, "the answer to be posted")
	if answer.Usage == nil || answer.Usage.TokensOut != 48 {
		t.Errorf("answer usage = %+v, want this turn's footer on the answer itself", answer.Usage)
	}
	time.Sleep(3 * r.footDelay) // past when a misplaced footing would have fired
	for _, u := range fake.updatedCalls() {
		if u.reply.Usage != nil {
			t.Errorf("edited %q to carry a footer; the answer already had it", u.text)
		}
	}
	if e := entryFor(t, r); e.turnInFlight() {
		t.Errorf("the answer was read as narration: the turn is still in flight")
	}
}

// With show-usage off, an answer-first turn must still settle at its boundary:
// a settled turn left banked makes the next turn's first narration look like
// its end, and keeps the reaper away (caught in review).
func TestAnAnswerFirstTurnSettlesWithUsageOff(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	r, ctx := narrationRouter(t, answerFirstDaemon(t), fitsAllSender{fake}, time.Hour)
	r.footDelay = 50 * time.Millisecond

	handleOne(t, r, ctx, fake)
	if got := recvReply(t, fake.replies); got.Text != "the answer" {
		t.Fatalf("second post = %q, want the answer", got.Text)
	}
	e := entryFor(t, r)
	waitFor(t, func() bool { return !e.awaitingAnswer() }, "the turn to settle with usage off")
	for _, u := range fake.updatedCalls() {
		if u.reply.Usage != nil {
			t.Errorf("a footer was added with show-usage off: %q", u.text)
		}
	}
}
