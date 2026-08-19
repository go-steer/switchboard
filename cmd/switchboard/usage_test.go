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
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// The payloads a turn produces, in the order a live daemon emits them: the
// subscribe-time priming total, then a running total per model call, then
// turn-complete — all of it *before* the agent event carrying the answer, so
// the router has to bank it and attach it on delivery.
const (
	primingUsageEvent = `{"tokens_in_total":0,"tokens_out_total":0,"cost_usd_total":0,"turns_total":0}`
	usageEvent        = `{"tokens_in_total":5000,"tokens_out_total":1,"cost_usd_total":0.0037537,"turns_total":1,` +
		`"last_turn":{"tokens_in":5000,"tokens_out":1,"cost_usd":0.0037537,"model":"gemini-3.7-flash"}}`
	turnCompleteEvent = `{"prompt_id":"p-1","model":"gemini-3.7-flash","tokens_in":5000,"tokens_out":1,"latency_ms":3142}`
	usageAnswerEvent  = `{"seq":1,"event":{"Content":{"parts":[{"text":"pong"}],"role":"model"},"Partial":false}}`
)

// usageTotals renders a running-total payload, for building the sequence a
// multi-call turn produces.
func usageTotals(in, out int64, cost float64, calls int) string {
	return fmt.Sprintf(`{"tokens_in_total":%d,"tokens_out_total":%d,"cost_usd_total":%v,"turns_total":%d,`+
		`"last_turn":{"tokens_in":1,"tokens_out":1,"cost_usd":0.1,"model":"gemini-3.7-flash"}}`,
		in, out, cost, calls)
}

// usageDaemon serves a session whose stream replays one turn's worth of
// events in the captured order, then holds open like a live daemon.
func usageDaemon(t *testing.T, events ...[2]string) *daemon.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":"core-agent","sessionID":"s1"}`)
	})
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"injected":"ok","session":"s1"}`)
	})
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for _, ev := range events {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev[0], ev[1])
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dc, err := daemon.New(daemon.Config{BaseURL: srv.URL, BearerToken: "tok", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return dc
}

// relayOneTurn runs a turn through the router and returns the reply it posted.
func relayOneTurn(t *testing.T, dc *daemon.Client, showUsage bool) chat.Reply {
	t.Helper()
	fake := &fakeSender{replies: make(chan chat.Reply, 4)}
	router := NewRouter(dc, fake, ProgressOff, nil, nil)
	router.setShowUsage(showUsage)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msg := chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "ping"}
	if err := router.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	select {
	case rep := <-fake.replies:
		return rep
	case <-time.After(2 * time.Second):
		t.Fatal("no reply relayed")
		return chat.Reply{}
	}
}

// relayReplies is relayOneTurn for a turn that posts more than once, with the
// footer enabled throughout.
func relayReplies(t *testing.T, dc *daemon.Client, n int) []chat.Reply {
	t.Helper()
	fake := &fakeSender{replies: make(chan chat.Reply, n+4)}
	router := NewRouter(dc, fake, ProgressOff, nil, nil)
	router.setShowUsage(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msg := chat.Message{Conversation: "C0:100.1", Caller: "alice@example.com", Text: "ping"}
	if err := router.Handle(ctx, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	out := make([]chat.Reply, 0, n)
	for len(out) < n {
		select {
		case rep := <-fake.replies:
			out = append(out, rep)
		case <-time.After(2 * time.Second):
			t.Fatalf("relayed %d replies, want %d", len(out), n)
		}
	}
	return out
}

// TestRelayAttachesUsage checks the router merges the two lifecycle events —
// cost comes only from usage-update, latency only from turn-complete — and
// hands the result to the adapter on the answer that follows them.
func TestRelayAttachesUsage(t *testing.T) {
	dc := usageDaemon(t,
		[2]string{daemon.EventUsage, primingUsageEvent},
		[2]string{daemon.EventUsage, usageEvent},
		[2]string{daemon.EventTurnComplete, turnCompleteEvent},
		[2]string{daemon.EventAgent, usageAnswerEvent},
	)
	rep := relayOneTurn(t, dc, true)
	if rep.Text != "pong" {
		t.Fatalf("relayed text = %q, want pong", rep.Text)
	}
	if rep.Usage == nil {
		t.Fatal("reply carries no Usage")
	}
	want := chat.Usage{
		Model: "gemini-3.7-flash", TokensIn: 5000, TokensOut: 1,
		CostUSD: 0.0037537, Latency: 3142 * time.Millisecond,
	}
	if *rep.Usage != want {
		t.Errorf("Usage = %+v, want %+v", *rep.Usage, want)
	}
}

// TestRelayAccumulatesToolCallUsage is the case the daemon's field names make
// easy to get wrong. A turn that runs tools produces one usage-update per
// model call, each last_turn describing only that call; the turn's real cost
// is the growth in the running totals. Sequence taken from a live five-tool
// turn on 2026-08-19, where reading last_turn would have reported 5,660
// tokens for a turn that consumed 33,340.
func TestRelayAccumulatesToolCallUsage(t *testing.T) {
	dc := usageDaemon(t,
		[2]string{daemon.EventUsage, usageTotals(5022, 18, 0.003834, 1)}, // priming: the prior turn
		[2]string{daemon.EventUsage, usageTotals(10372, 33, 0.0045277, 2)},
		[2]string{daemon.EventUsage, usageTotals(15908, 48, 0.005361, 3)},
		[2]string{daemon.EventUsage, usageTotals(21475, 63, 0.0062175, 4)},
		[2]string{daemon.EventUsage, usageTotals(27073, 78, 0.00709725, 5)},
		[2]string{daemon.EventUsage, usageTotals(32702, 93, 0.00800025, 6)},
		[2]string{daemon.EventUsage, usageTotals(38362, 114, 0.008949, 7)},
		[2]string{daemon.EventTurnComplete, `{"model":"gemini-3.7-flash","latency_ms":26215}`},
		[2]string{daemon.EventAgent, usageAnswerEvent},
	)
	rep := relayOneTurn(t, dc, true)
	if rep.Usage == nil {
		t.Fatal("reply carries no Usage")
	}
	// 38362-5022 in, 114-18 out, 0.008949-0.003834 spent.
	if rep.Usage.TokensIn != 33340 || rep.Usage.TokensOut != 96 {
		t.Errorf("tokens = %d in / %d out, want 33340 / 96", rep.Usage.TokensIn, rep.Usage.TokensOut)
	}
	if got := rep.Usage.CostUSD; got < 0.005114 || got > 0.005116 {
		t.Errorf("CostUSD = %v, want ~0.005115", got)
	}
	if rep.Usage.Latency != 26215*time.Millisecond {
		t.Errorf("Latency = %v, want 26.215s", rep.Usage.Latency)
	}
}

// TestRelayUsageOptIn checks the default: the events are still consumed, but
// nothing about the turn's spend reaches the conversation.
func TestRelayUsageOptIn(t *testing.T) {
	dc := usageDaemon(t,
		[2]string{daemon.EventUsage, primingUsageEvent},
		[2]string{daemon.EventUsage, usageEvent},
		[2]string{daemon.EventTurnComplete, turnCompleteEvent},
		[2]string{daemon.EventAgent, usageAnswerEvent},
	)
	if rep := relayOneTurn(t, dc, false); rep.Usage != nil {
		t.Errorf("Usage = %+v with --show-usage off, want nil", *rep.Usage)
	}
}

// TestRelayWithoutUsageEvents checks a daemon that reports no usage at all — an
// older build, or a turn that errored before accounting — relays the answer
// with no footer rather than an all-zero one.
func TestRelayWithoutUsageEvents(t *testing.T) {
	dc := usageDaemon(t, [2]string{daemon.EventAgent, usageAnswerEvent})
	rep := relayOneTurn(t, dc, true)
	if rep.Text != "pong" {
		t.Fatalf("relayed text = %q, want pong", rep.Text)
	}
	if rep.Usage != nil {
		t.Errorf("Usage = %+v with no usage events, want nil", *rep.Usage)
	}
}

// TestUsageClearedBetweenTurns checks a turn's numbers cannot leak onto the
// next one: takeUsage empties the bank while keeping the totals baseline, so
// a second turn is differenced from where the first left off rather than from
// zero.
func TestUsageClearedBetweenTurns(t *testing.T) {
	e := &sessionEntry{}
	e.noteTotals(daemon.UsageTotals{Model: "m", TokensIn: 100, TokensOut: 10, CostUSD: 1})
	e.noteTotals(daemon.UsageTotals{Model: "m", TokensIn: 150, TokensOut: 15, CostUSD: 1.5})
	e.noteTurnComplete(daemon.TurnUsage{Latency: time.Second})

	got := e.takeUsage()
	if got == nil {
		t.Fatal("takeUsage after noteTotals = nil")
	}
	want := chat.Usage{Model: "m", TokensIn: 50, TokensOut: 5, CostUSD: 0.5, Latency: time.Second}
	if *got != want {
		t.Errorf("takeUsage = %+v, want %+v", *got, want)
	}
	if again := e.takeUsage(); again != nil {
		t.Errorf("second takeUsage = %+v, want nil", *again)
	}

	// The next turn differences from 150, not from zero.
	e.noteTotals(daemon.UsageTotals{Model: "m", TokensIn: 170, TokensOut: 17, CostUSD: 1.7})
	e.noteTurnComplete(daemon.TurnUsage{Latency: time.Second})
	second := e.takeUsage()
	if second == nil {
		t.Fatal("second turn banked nothing")
	}
	if second.TokensIn != 20 || second.TokensOut != 2 {
		t.Errorf("second turn = %d in / %d out, want 20 / 2", second.TokensIn, second.TokensOut)
	}
}

// TestUsageFirstReportOnlyBaselines checks a stream's first totals report is
// taken as a baseline and not as a turn's cost — otherwise resuming a
// long-running session would bill its whole history to the next reply. Not
// even the model name is taken from it: a footer reporting a model and nothing
// else is a report about a turn this reply did not run.
func TestUsageFirstReportOnlyBaselines(t *testing.T) {
	e := &sessionEntry{}
	e.noteTotals(daemon.UsageTotals{Model: "m", TokensIn: 500_000, TokensOut: 9_000, CostUSD: 42})
	e.noteTurnComplete(daemon.TurnUsage{}) // release the bank, so nil means empty and not withheld
	if got := e.takeUsage(); got != nil {
		t.Errorf("first report banked %+v, want nothing at all", *got)
	}
}

// TestUsageIgnoresBackwardsTotals checks a total that goes backwards cannot
// produce a negative count — it is nothing this can make sense of, so the
// delta is dropped and the baseline follows the daemon.
func TestUsageIgnoresBackwardsTotals(t *testing.T) {
	e := &sessionEntry{}
	e.noteTotals(daemon.UsageTotals{TokensIn: 100, TokensOut: 10, CostUSD: 1})
	e.noteTotals(daemon.UsageTotals{TokensIn: 50, TokensOut: 5, CostUSD: 0.5})
	e.noteTurnComplete(daemon.TurnUsage{Latency: time.Second})
	got := e.takeUsage()
	if got == nil {
		t.Fatal("backwards totals banked nothing at all; want the latency with no counts")
	}
	if got.TokensIn != 0 || got.TokensOut != 0 || got.CostUSD != 0 {
		t.Errorf("backwards totals produced counts %+v, want all zero", *got)
	}
	// The baseline still followed the daemon down, so the next report is
	// differenced from 50 rather than from the stale 100.
	e.noteTotals(daemon.UsageTotals{TokensIn: 60, TokensOut: 6, CostUSD: 0.6})
	e.noteTurnComplete(daemon.TurnUsage{Latency: time.Second})
	next := e.takeUsage()
	if next == nil || next.TokensIn != 10 {
		t.Errorf("after a backwards total the baseline did not follow the daemon: %+v", next)
	}
}

// TestUsageWithheldUntilTurnComplete checks the bank is not released early.
// Not every agent event carrying text is the answer — a model turn that
// narrates before calling a tool arrives as text too — and releasing on the
// first of those would put a fraction of the turn's cost on an interim message
// and leave the answer reporting only the last model call.
func TestUsageWithheldUntilTurnComplete(t *testing.T) {
	e := &sessionEntry{}
	e.noteTotals(daemon.UsageTotals{Model: "m", TokensIn: 100, TokensOut: 10, CostUSD: 1})
	e.noteTotals(daemon.UsageTotals{Model: "m", TokensIn: 150, TokensOut: 15, CostUSD: 1.5})
	if got := e.takeUsage(); got != nil {
		t.Errorf("mid-turn takeUsage = %+v, want nil until turn-complete", *got)
	}
	e.noteTotals(daemon.UsageTotals{Model: "m", TokensIn: 200, TokensOut: 20, CostUSD: 2})
	e.noteTurnComplete(daemon.TurnUsage{Latency: 2 * time.Second})

	got := e.takeUsage()
	if got == nil {
		t.Fatal("takeUsage after turn-complete = nil")
	}
	// Both deltas, not just the one after the withheld read.
	want := chat.Usage{Model: "m", TokensIn: 100, TokensOut: 10, CostUSD: 1, Latency: 2 * time.Second}
	if *got != want {
		t.Errorf("takeUsage = %+v, want the whole turn %+v", *got, want)
	}
}

// TestUsageFromADeadTurnIsDropped checks a turn that ended without an answer
// cannot bill the next one. turn-complete fires, nothing is ever delivered, and
// the numbers sit banked; the next inbound turn has to clear them, and to keep
// the baseline so its own delta is measured from where the dead turn stopped.
func TestUsageFromADeadTurnIsDropped(t *testing.T) {
	e := &sessionEntry{}
	e.noteTotals(daemon.UsageTotals{Model: "m", TokensIn: 100, TokensOut: 10, CostUSD: 1})
	e.noteTotals(daemon.UsageTotals{Model: "m", TokensIn: 5_000, TokensOut: 500, CostUSD: 9})
	e.noteTurnComplete(daemon.TurnUsage{Latency: time.Minute})
	// No deliverText: the turn errored, or was interrupted (#34).

	e.resetUsage() // what Handle does for the next inbound turn
	e.noteTotals(daemon.UsageTotals{Model: "m", TokensIn: 5_100, TokensOut: 510, CostUSD: 9.5})
	e.noteTurnComplete(daemon.TurnUsage{Latency: time.Second})

	got := e.takeUsage()
	if got == nil {
		t.Fatal("the second turn banked nothing")
	}
	want := chat.Usage{Model: "m", TokensIn: 100, TokensOut: 10, CostUSD: 0.5, Latency: time.Second}
	if *got != want {
		t.Errorf("takeUsage = %+v, want only the second turn %+v", *got, want)
	}
}

// TestRelayWithholdsUsageFromInterimText is the end-to-end form of the same
// thing: an agent event that narrates alongside a tool call is relayed as its
// own message, and must not carry a footer. Only the answer that follows
// turn-complete does, and it reports the whole turn.
func TestRelayWithholdsUsageFromInterimText(t *testing.T) {
	const narration = `{"seq":1,"event":{"Content":{"parts":[{"text":"let me check"},` +
		`{"functionCall":{"name":"lookup"}}],"role":"model"},"Partial":false}}`
	const answer = `{"seq":2,"event":{"Content":{"parts":[{"text":"pong"}],"role":"model"},"Partial":false}}`

	dc := usageDaemon(t,
		[2]string{daemon.EventUsage, usageTotals(1000, 10, 0.001, 1)}, // priming
		[2]string{daemon.EventUsage, usageTotals(6000, 20, 0.002, 2)},
		[2]string{daemon.EventAgent, narration},
		[2]string{daemon.EventUsage, usageTotals(12000, 40, 0.004, 3)},
		[2]string{daemon.EventTurnComplete, `{"model":"gemini-3.7-flash","latency_ms":9000}`},
		[2]string{daemon.EventAgent, answer},
	)
	replies := relayReplies(t, dc, 2)
	if replies[0].Text != "let me check" {
		t.Fatalf("first relayed text = %q, want the narration", replies[0].Text)
	}
	if replies[0].Usage != nil {
		t.Errorf("interim message carries Usage %+v, want none", *replies[0].Usage)
	}
	if replies[1].Text != "pong" {
		t.Fatalf("second relayed text = %q, want the answer", replies[1].Text)
	}
	if replies[1].Usage == nil {
		t.Fatal("the answer carries no Usage")
	}
	// 12000-1000 in and 40-10 out: every model call in the turn, including the
	// one whose text was relayed early.
	if replies[1].Usage.TokensIn != 11000 || replies[1].Usage.TokensOut != 30 {
		t.Errorf("answer usage = %d in / %d out, want the whole turn 11000 / 30",
			replies[1].Usage.TokensIn, replies[1].Usage.TokensOut)
	}
}
