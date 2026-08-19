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
	"unicode/utf8"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// TestTurnErrorNotice covers what a reader is told, which differs by what they
// can do about it: retry, don't bother, or go reset a guardrail.
func TestTurnErrorNotice(t *testing.T) {
	for _, tt := range []struct {
		name     string
		te       daemon.TurnError
		wantLead string
		contains []string
		absent   []string
		maxLen   int // 0 means unchecked
	}{
		{
			name: "retryable says try again",
			te: daemon.TurnError{
				Kind: daemon.TurnErrorRateLimited, Code: "429",
				Message: "Quota exceeded for aiplatform.googleapis.com", Retryable: true,
			},
			wantLead: errNoticeTurnTransient,
			contains: []string{"rate_limited (429)", "Quota exceeded for aiplatform.googleapis.com"},
		},
		{
			name: "terminal says retrying will not help",
			te: daemon.TurnError{
				Kind: daemon.TurnErrorAuth, Code: "PERMISSION_DENIED",
				Message: "caller lacks aiplatform.endpoints.predict",
				Hint:    "Verify the runtime service account has roles/aiplatform.user.",
			},
			wantLead: errNoticeTurnTerminal,
			contains: []string{"auth_error (PERMISSION_DENIED)", "roles/aiplatform.user"},
		},
		{
			name: "cost ceiling is a guardrail, not a retry",
			te: daemon.TurnError{
				Kind:    daemon.TurnErrorCostCeiling,
				Message: "per-turn cost ceiling of $5.00 exceeded",
			},
			wantLead: errNoticeGuardrail,
			contains: []string{"cost_ceiling", "operator resets it"},
			// The worst possible advice here: the agent refuses new turns.
			absent: []string{"try again"},
		},
		{
			name:     "watchdog is a guardrail too",
			te:       daemon.TurnError{Kind: daemon.TurnErrorWatchdog, Message: "5 identical tool calls"},
			wantLead: errNoticeGuardrail,
			contains: []string{"watchdog"},
			absent:   []string{"try again"},
		},
		{
			name:     "an unknown kind still renders",
			te:       daemon.TurnError{Kind: "something_new", Message: "the model exploded"},
			wantLead: errNoticeTurnTerminal,
			contains: []string{"something_new", "the model exploded"},
		},
		{
			name:     "no code and no hint leaves no empty brackets or trailing blank line",
			te:       daemon.TurnError{Kind: daemon.TurnErrorUnknown, Message: "nope"},
			wantLead: errNoticeTurnTerminal,
			contains: []string{"unknown: nope"},
			absent:   []string{"()", "\n\n"},
		},
		{
			// The guardrail trips build their message without the daemon's
			// length cap, and the watchdog's interpolates a trigger reason of
			// no fixed size — so the notice bounds it here.
			name: "an unbounded message is cut before it reaches the thread",
			te: daemon.TurnError{
				Kind:    daemon.TurnErrorWatchdog,
				Message: strings.Repeat("looping on the same tool call. ", 40),
			},
			wantLead: errNoticeGuardrail,
			contains: []string{"..."},
			maxLen:   len(errNoticeGuardrail) + len(daemon.TurnErrorWatchdog) + noticeDetailCap + 8,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := turnErrorNotice(tt.te)
			if tt.maxLen > 0 && len(got) > tt.maxLen {
				t.Errorf("notice is %d bytes, want at most %d: %q", len(got), tt.maxLen, got)
			}
			if !strings.HasPrefix(got, tt.wantLead) {
				t.Errorf("notice = %q, want it to lead with %q", got, tt.wantLead)
			}
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("notice = %q, want it to contain %q", got, want)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(got, bad) {
					t.Errorf("notice = %q, want it not to contain %q", got, bad)
				}
			}
			if strings.HasSuffix(got, "\n") {
				t.Errorf("notice = %q, want no trailing newline", got)
			}
		})
	}
}

// streamPlan describes what a fake daemon's event stream does. The three
// shapes the tests need are "a live stream", "a daemon that is gone", and "a
// daemon that keeps dropping the connection but is otherwise fine" — the last
// being the one that must not be mistaken for the second.
type streamPlan struct {
	// events is replayed at the top of a subscribe.
	events [][2]string
	// once replays events on the first subscribe only. Later reconnects get an
	// empty stream, which is what an outage following a live turn looks like.
	once bool
	// ends returns from each subscribe once it has replayed, rather than
	// holding the connection open.
	ends bool
}

// planDaemon serves a session whose event stream follows the plan.
func planDaemon(t *testing.T, p streamPlan) (*daemon.Client, *atomic.Int64) {
	t.Helper()
	var subscribes atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"injected":"ok","session":"s1"}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		n := subscribes.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if n == 1 || !p.once {
			for _, ev := range p.events {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev[0], ev[1])
				w.(http.Flusher).Flush()
			}
		}
		if p.ends {
			return
		}
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return dc, &subscribes
}

// errorDaemon serves a session whose stream replays the given events and then
// holds open. streamEnds makes every subscribe return immediately instead,
// which is what the relay sees when the daemon has gone away.
func errorDaemon(t *testing.T, streamEnds bool, events ...[2]string) (*daemon.Client, *atomic.Int64) {
	t.Helper()
	return planDaemon(t, streamPlan{events: events, ends: streamEnds})
}

// errorRouter wires a router with the reconnect and grace timings compressed,
// so an outage can be exercised without a real ninety-second wait.
func errorRouter(t *testing.T, dc *daemon.Client, out sender, grace time.Duration) (*Router, context.Context) {
	t.Helper()
	r := NewRouter(dc, out, ProgressIndicator, nil, func(string, ...any) {})
	r.minBackoff, r.maxBackoff = 5*time.Millisecond, 10*time.Millisecond
	r.streamGrace = grace
	r.tickInterval = 0 // the clock is #37's business, not this test's
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return r, ctx
}

// handleOne drives one inbound turn and returns the progress placeholder the
// router posted for it.
func handleOne(t *testing.T, r *Router, ctx context.Context, fake *fakeSender) chat.Reply {
	t.Helper()
	msg := chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "ping"}
	if err := r.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	placeholder := recvReply(t, fake.replies)
	if placeholder.Text != workingText {
		t.Fatalf("first post = %q, want the progress placeholder", placeholder.Text)
	}
	return placeholder
}

// TestRelaySurfacesTurnError is #34: a turn that dies inside the daemon has to
// say so. Before this the frame was dropped on the floor and the thread went
// quiet with the placeholder still up.
func TestRelaySurfacesTurnError(t *testing.T) {
	const turnError = `{"kind":"auth_error","code":"PERMISSION_DENIED",` +
		`"message":"caller lacks aiplatform.endpoints.predict","retryable":false,` +
		`"hint":"Verify the runtime service account has roles/aiplatform.user."}`

	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false, [2]string{daemon.EventTurnError, turnError})
	r, ctx := errorRouter(t, dc, fake, time.Hour)

	handleOne(t, r, ctx, fake)

	notice := recvReply(t, fake.replies)
	if notice.Kind != chat.KindNotice {
		t.Errorf("notice Kind = %q, want %q", notice.Kind, chat.KindNotice)
	}
	for _, want := range []string{"auth_error", "aiplatform.endpoints.predict", "roles/aiplatform.user"} {
		if !strings.Contains(notice.Text, want) {
			t.Errorf("notice = %q, want it to contain %q", notice.Text, want)
		}
	}
	// The stranded placeholder is half the bug: a clock left running on a turn
	// that is already dead.
	waitFor(t, func() bool { return len(fake.deletedRefs()) == 1 }, "the progress message was not retired")
}

// TestRelayTurnErrorCarriesNoUsage checks a dead turn's accounting is dropped
// rather than banked for whoever replies next. The notice itself never carries
// a footer — it is not an answer — so the risk is the next turn inheriting it.
func TestRelayTurnErrorCarriesNoUsage(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false,
		[2]string{daemon.EventUsage, usageTotals(1000, 10, 0.001, 1)},
		[2]string{daemon.EventUsage, usageTotals(9000, 90, 0.05, 2)},
		[2]string{daemon.EventTurnComplete, `{"model":"gemini-3.7-flash","latency_ms":4000}`},
		[2]string{daemon.EventTurnError, `{"kind":"transient_network","message":"model call timed out","retryable":true}`},
	)
	r, ctx := errorRouter(t, dc, fake, time.Hour)
	r.setShowUsage(true)

	handleOne(t, r, ctx, fake)
	notice := recvReply(t, fake.replies)
	if notice.Usage != nil {
		t.Errorf("the failure notice carries Usage %+v, want none", *notice.Usage)
	}

	// The entry banked 8000 tokens against a turn that never answered. It must
	// not follow the next reply out.
	r.mu.Lock()
	e := r.sessions["C0:100.1"]
	r.mu.Unlock()
	if e == nil {
		t.Fatal("no session entry")
	}
	if got := e.takeUsage(); got != nil {
		t.Errorf("the dead turn left %+v banked, want nothing", *got)
	}
}

// TestRelayIgnoresUnreadableTurnError checks a frame that says nothing produces
// nothing. "Something went wrong" with no detail is not an improvement on
// silence, and posting one per malformed frame would be worse than either.
func TestRelayIgnoresUnreadableTurnError(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false,
		[2]string{daemon.EventTurnError, `{"retryable":true}`},
		[2]string{daemon.EventTurnError, `not json at all`},
		[2]string{daemon.EventAgent, usageAnswerEvent},
	)
	r, ctx := errorRouter(t, dc, fake, time.Hour)

	handleOne(t, r, ctx, fake)
	// The next thing posted is the answer, not a notice about either frame.
	got := recvReply(t, fake.replies)
	if got.Text != "pong" {
		t.Errorf("next post = %q (kind %q), want the answer", got.Text, got.Kind)
	}
}

// TestStreamLostTellsTheThread is the failure mode confirmed live on
// 2026-08-18: the daemon was stopped mid-turn and the thread simply went
// quiet. No turn-error can arrive for this one — the daemon is the thing that
// went away — so the relay has to notice the silence itself.
func TestStreamLostTellsTheThread(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, subscribes := errorDaemon(t, true) // every subscribe returns at once
	r, ctx := errorRouter(t, dc, fake, 20*time.Millisecond)

	handleOne(t, r, ctx, fake)

	notice := recvReply(t, fake.replies)
	if notice.Kind != chat.KindNotice || !strings.Contains(notice.Text, "Lost contact") {
		t.Fatalf("posted %q (kind %q), want the lost-contact notice", notice.Text, notice.Kind)
	}
	waitFor(t, func() bool { return len(fake.deletedRefs()) == 1 }, "the progress message was not retired")

	// Said once, however long the outage lasts. The relay keeps reconnecting
	// behind it — a notice per retry would be its own outage.
	before := subscribes.Load()
	waitFor(t, func() bool { return subscribes.Load() > before+2 }, "the relay stopped reconnecting")
	select {
	case extra := <-fake.replies:
		t.Errorf("a second notice was posted: %q", extra.Text)
	default:
	}
}

// TestStreamLostStaysQuietWhenNobodyIsWaiting checks an idle session's dropped
// stream is not an event in the conversation. Reconnects happen all the time;
// only one with a turn waiting on it is worth interrupting anyone for.
func TestStreamLostStaysQuietWhenNobodyIsWaiting(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, subscribes := errorDaemon(t, true)
	r, ctx := errorRouter(t, dc, fake, 10*time.Millisecond)

	// A session with no turn in flight: subscribe directly rather than through
	// Handle, which is what injects one.
	if _, err := r.session(ctx, "C0:100.1", "C0", "alice@example.com"); err != nil {
		t.Fatalf("session: %v", err)
	}
	waitFor(t, func() bool { return subscribes.Load() > 3 }, "the relay never retried")
	select {
	case got := <-fake.replies:
		t.Errorf("posted %q for an idle session's dropped stream, want silence", got.Text)
	default:
	}
}

// TestAnswerBeatsTheLostStreamNotice runs a turn to its answer and then takes
// the daemon away for good. Nobody is waiting any more, so the outage is not an
// event in the conversation — without this the thread would be told it lost
// contact mid-turn, about the very turn that had just answered.
func TestAnswerBeatsTheLostStreamNotice(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, subscribes := planDaemon(t, streamPlan{
		events: [][2]string{{daemon.EventAgent, usageAnswerEvent}},
		once:   true, // the answer lands, then the daemon is gone
		ends:   true,
	})
	r, ctx := errorRouter(t, dc, fake, 20*time.Millisecond)

	handleOne(t, r, ctx, fake)
	if got := recvReply(t, fake.replies); got.Text != "pong" {
		t.Fatalf("relayed %q, want the answer", got.Text)
	}

	// Well past the grace, and still reconnecting into nothing.
	before := subscribes.Load()
	waitFor(t, func() bool { return subscribes.Load() > before+8 }, "the relay stopped reconnecting")
	select {
	case extra := <-fake.replies:
		t.Errorf("posted %q after the turn had answered, want silence", extra.Text)
	default:
	}
}

// TestTurnErrorAfterAnAnswerStillSpeaks is the case that makes the failure
// notice a separate claim from the turn itself. A cost ceiling is enforced at
// the turn boundary, so its turn-error arrives *after* the text the turn
// produced: gating the notice on "was a turn still in flight" swallowed exactly
// the failure the reader most needs to hear about, since the agent will now
// refuse everything they send until someone resets it.
func TestTurnErrorAfterAnAnswerStillSpeaks(t *testing.T) {
	const tripped = `{"kind":"cost_ceiling","code":"cost_ceiling",` +
		`"message":"per-turn cost ceiling exceeded: this turn cost $5.2000, ceiling is $5.0000. ` +
		`Agent will refuse new turns until the operator resets it (/guardrail reset).","retryable":false}`

	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false,
		[2]string{daemon.EventAgent, usageAnswerEvent},
		[2]string{daemon.EventTurnError, tripped},
	)
	r, ctx := errorRouter(t, dc, fake, time.Hour)

	handleOne(t, r, ctx, fake)
	if got := recvReply(t, fake.replies); got.Text != "pong" {
		t.Fatalf("relayed %q, want the answer", got.Text)
	}
	notice := recvReply(t, fake.replies)
	if notice.Kind != chat.KindNotice {
		t.Errorf("notice Kind = %q, want %q", notice.Kind, chat.KindNotice)
	}
	if !strings.HasPrefix(notice.Text, errNoticeGuardrail) {
		t.Errorf("notice = %q, want the guardrail lead", notice.Text)
	}
}

// TestRepeatGuardrailRefusalStillReadsAsAGuardrail covers every message sent
// after the trip. Only the trip itself is classified; the refusals that follow
// come back as kind "unknown", and telling the reader "retrying won't help" —
// without saying an operator has to reset something — leaves them stuck.
func TestRepeatGuardrailRefusalStillReadsAsAGuardrail(t *testing.T) {
	refusal := daemon.TurnError{
		Kind: daemon.TurnErrorUnknown,
		Message: "per-turn cost ceiling exceeded: this turn cost $5.2000, ceiling is $5.0000. " +
			"Agent will refuse new turns until the operator resets it (/guardrail reset).",
	}
	if !refusal.Guardrail() {
		t.Fatal("a preflight guardrail refusal was not recognized as one")
	}
	got := turnErrorNotice(refusal)
	if !strings.HasPrefix(got, errNoticeGuardrail) {
		t.Errorf("notice = %q, want the guardrail lead", got)
	}
	if strings.Contains(got, "try again") {
		t.Errorf("notice = %q, want no suggestion to retry", got)
	}
}

// TestTurnCompleteEndsATurnWithNothingToSay covers the turn that produced no
// relayable text — interrupted, empty, or an answer suppressed as a reconnect
// replay. turn-complete is the only signal such a turn ever gives, so if it
// does not end the turn the entry stays marked in flight forever and the next
// outage announces a turn that finished long ago.
func TestTurnCompleteEndsATurnWithNothingToSay(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, subscribes := planDaemon(t, streamPlan{
		events: [][2]string{{daemon.EventTurnComplete, `{"model":"gemini-3.7-flash","latency_ms":900}`}},
		once:   true,
		ends:   true,
	})
	r, ctx := errorRouter(t, dc, fake, 20*time.Millisecond)

	handleOne(t, r, ctx, fake)
	waitFor(t, func() bool { return subscribes.Load() > 10 }, "the relay stopped reconnecting")
	select {
	case got := <-fake.replies:
		t.Errorf("posted %q for a turn the daemon had already completed, want silence", got.Text)
	default:
	}
}

// TestBlippingStreamIsNotALostOne separates "the daemon is gone" from "the
// connection keeps being cut". A proxy or an idle timeout can end an SSE stream
// every few seconds while the daemon is perfectly healthy, and a long turn
// spent thinking produces no events of its own to prove otherwise — so the
// grace has to be measured from the last sign of life on the wire, not from the
// last answer relayed.
func TestBlippingStreamIsNotALostOne(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, subscribes := planDaemon(t, streamPlan{
		// What a real reconnect carries: the daemon opens every stream with
		// its capabilities frame, and nothing else while the turn thinks.
		events: [][2]string{{"capabilities", `{"protocol_version":"1.4.0","server":"core-agent/2.9.0-dev"}`}},
		ends:   true,
	})
	r, ctx := errorRouter(t, dc, fake, 50*time.Millisecond)

	handleOne(t, r, ctx, fake)
	// Many reconnects, spanning several multiples of the grace, with the turn
	// still in flight throughout.
	waitFor(t, func() bool { return subscribes.Load() > 25 }, "the relay stopped reconnecting")
	select {
	case got := <-fake.replies:
		t.Errorf("posted %q while the stream was alive, want silence", got.Text)
	default:
	}
}

// TestUsageFromAFailedTurnDoesNotFollowTheNextOne pins what resetUsage is for.
// The dead turn banked 8000 tokens against an answer that never came; the next
// turn's footer must report its own 500, not 8500.
func TestUsageFromAFailedTurnDoesNotFollowTheNextOne(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false,
		[2]string{daemon.EventUsage, usageTotals(1000, 10, 0.001, 1)},
		[2]string{daemon.EventUsage, usageTotals(9000, 90, 0.05, 2)},
		[2]string{daemon.EventTurnComplete, `{"model":"gemini-3.7-flash","latency_ms":4000}`},
		[2]string{daemon.EventTurnError, `{"kind":"transient_network","message":"model call timed out","retryable":true}`},
		// The turn the reader sends next, on the same session.
		[2]string{daemon.EventUsage, usageTotals(9500, 95, 0.052, 3)},
		[2]string{daemon.EventTurnComplete, `{"model":"gemini-3.7-flash","latency_ms":700}`},
		[2]string{daemon.EventAgent, usageAnswerEvent},
	)
	r, ctx := errorRouter(t, dc, fake, time.Hour)
	r.setShowUsage(true)

	handleOne(t, r, ctx, fake)
	if notice := recvReply(t, fake.replies); notice.Kind != chat.KindNotice {
		t.Fatalf("first post = %q (kind %q), want the failure notice", notice.Text, notice.Kind)
	}
	answer := recvReply(t, fake.replies)
	if answer.Text != "pong" {
		t.Fatalf("next post = %q, want the answer", answer.Text)
	}
	if answer.Usage == nil {
		t.Fatal("the answer carries no usage footer")
	}
	if answer.Usage.TokensIn != 500 || answer.Usage.TokensOut != 5 {
		t.Errorf("footer = %d in / %d out, want 500/5 — the dead turn's tokens followed it",
			answer.Usage.TokensIn, answer.Usage.TokensOut)
	}
}

// TestClampNotice bounds daemon-supplied text on the way into a chat message.
// The guardrail trips build their message without the length cap the rest of
// the daemon's failures go through, and the watchdog's interpolates a trigger
// reason of no fixed size.
func TestClampNotice(t *testing.T) {
	if got := clampNotice("  a\n  short  one  "); got != "a short one" {
		t.Errorf("clampNotice collapsed to %q, want %q", got, "a short one")
	}
	long := strings.Repeat("x", 400)
	got := clampNotice(long)
	if len(got) != noticeDetailCap {
		t.Errorf("clampNotice returned %d bytes, want %d", len(got), noticeDetailCap)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("clampNotice = %q, want it to show it was cut", got)
	}
	// A cut inside a rune renders as a replacement character, which reads as
	// corruption rather than truncation.
	runes := strings.Repeat("é", 400)
	if got := clampNotice(runes); !utf8.ValidString(got) {
		t.Errorf("clampNotice split a rune: %q", got)
	}
}

// TestDeliveredAnswerEndsTheTurn checks the answer path actually clears the
// flag — otherwise a stream drop long after a quiet session had answered would
// tell the thread it lost contact mid-turn.
func TestDeliveredAnswerEndsTheTurn(t *testing.T) {
	fake := &fakeSender{replies: make(chan chat.Reply, 8)}
	dc, _ := errorDaemon(t, false, [2]string{daemon.EventAgent, usageAnswerEvent})
	r, ctx := errorRouter(t, dc, fake, time.Hour)

	handleOne(t, r, ctx, fake)
	if got := recvReply(t, fake.replies); got.Text != "pong" {
		t.Fatalf("relayed %q, want the answer", got.Text)
	}
	r.mu.Lock()
	e := r.sessions["C0:100.1"]
	r.mu.Unlock()
	if e.turnInFlight() {
		t.Error("the turn is still marked in flight after its answer was delivered")
	}
}
