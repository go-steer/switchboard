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

package daemon

import (
	"testing"
	"time"
)

// liveUsageEvent is a usage-update payload captured verbatim from a running
// daemon during the walkthrough. Parsing the real shape (running totals and
// by_model alongside last_turn) is the point: the fields switchboard wants are
// the minority of what arrives.
const liveUsageEvent = `{"tokens_in_total":5000,"tokens_out_total":1,"cost_usd_total":0.0037537,` +
	`"turns_total":1,"by_model":{"gemini-3.7-flash":{"tokens_in":5000,"tokens_out":1,` +
	`"cost_usd":0.0037537,"turns":1}},"last_turn":{"tokens_in":5000,"tokens_out":1,` +
	`"cost_usd":0.0037537,"model":"gemini-3.7-flash"}}`

// liveTurnCompleteEvent is the turn-complete payload from the same capture.
const liveTurnCompleteEvent = `{"prompt_id":"p-1","model":"gemini-3.7-flash",` +
	`"tokens_in":5000,"tokens_out":1,"latency_ms":3142}`

func TestSessionUsage(t *testing.T) {
	got, ok := SessionUsage(liveUsageEvent)
	if !ok {
		t.Fatal("SessionUsage(live) not ok")
	}
	want := UsageTotals{Model: "gemini-3.7-flash", TokensIn: 5000, TokensOut: 1, CostUSD: 0.0037537, Calls: 1}
	if got != want {
		t.Errorf("SessionUsage = %+v, want %+v", got, want)
	}
}

// TestSessionUsageTakesTotalsNotLastTurn pins the distinction the daemon's
// field names invite getting wrong: on a turn that made tool calls, last_turn
// describes only the final model call, and reading it would undercount the
// turn several times over. Payload captured live on 2026-08-19 from the sixth
// usage-update of a five-tool turn.
func TestSessionUsageTakesTotalsNotLastTurn(t *testing.T) {
	const midTurn = `{"tokens_in_total":38362,"tokens_out_total":114,"cost_usd_total":0.008949,` +
		`"turns_total":7,"by_model":{"gemini-3.7-flash":{"tokens_in":38362,"tokens_out":114,` +
		`"cost_usd":0.008949,"turns":7}},"last_turn":{"tokens_in":5660,"tokens_in_cached":5000,` +
		`"tokens_out":21,"cost_usd":0.0009487499999999999,"model":"gemini-3.7-flash"}}`

	got, ok := SessionUsage(midTurn)
	if !ok {
		t.Fatal("SessionUsage(mid-turn) not ok")
	}
	if got.TokensIn != 38362 || got.TokensOut != 114 {
		t.Errorf("tokens = %d in / %d out, want the totals 38362 / 114 (not last_turn's 5660 / 21)",
			got.TokensIn, got.TokensOut)
	}
	if got.Calls != 7 {
		t.Errorf("Calls = %d, want 7 — the daemon counts model calls, not conversational turns", got.Calls)
	}
}

func TestTurnCompleted(t *testing.T) {
	u, ok := TurnCompleted(liveTurnCompleteEvent)
	if !ok {
		t.Fatal("TurnCompleted(live) not ok")
	}
	// Only the model and the whole-turn latency: this event's tokens describe
	// the last model call, so taking them would undercount a tool-using turn.
	want := TurnUsage{Model: "gemini-3.7-flash", Latency: 3142 * time.Millisecond}
	if u != want {
		t.Errorf("TurnCompleted = %+v, want %+v", u, want)
	}
}

// TestSessionUsagePriming covers the two priming events a stream opens with,
// both captured live. Each is the baseline every later total is differenced
// against, so both must parse: rejecting the fresh-session one as "empty"
// would cost that session's very first turn its numbers.
func TestSessionUsagePriming(t *testing.T) {
	fresh, ok := SessionUsage(`{"tokens_in_total":0,"tokens_out_total":0,"cost_usd_total":0,"turns_total":0}`)
	if !ok {
		t.Error("all-zero priming event rejected; it is a valid baseline of zero")
	}
	if (fresh != UsageTotals{}) {
		t.Errorf("fresh priming = %+v, want a zero baseline", fresh)
	}
	// Resubscribing to a session that has already run turns primes with the
	// totals so far — which is what keeps a reconnect from attributing the
	// whole session to the next reply.
	resumed, ok := SessionUsage(`{"tokens_in_total":5022,"tokens_out_total":18,"cost_usd_total":0.003834,"turns_total":1}`)
	if !ok {
		t.Fatal("resumed priming event rejected")
	}
	if resumed.TokensIn != 5022 || resumed.Calls != 1 {
		t.Errorf("resumed priming = %+v, want the session's totals so far", resumed)
	}
}

// TestUsageParsersReject covers the payloads that must not yield accounting:
// malformed JSON, and an object carrying no totals at all. Each has to report
// !ok rather than a zero value a caller might adopt as a baseline — doing so
// would make the next turn look like it had consumed the whole session.
func TestUsageParsersReject(t *testing.T) {
	for _, tt := range []struct {
		name string
		data string
	}{
		{"empty object", `{}`},
		{"unrelated fields", `{"model":"gemini-3.7-flash"}`},
		{"not json", `{`},
		{"null", `null`},
	} {
		if u, ok := SessionUsage(tt.data); ok {
			t.Errorf("SessionUsage(%s) = %+v, want !ok", tt.name, u)
		}
	}
	for _, tt := range []struct {
		name string
		data string
	}{
		{"empty object", `{}`},
		{"prompt id only", `{"prompt_id":"p-1"}`},
		{"tokens but no latency", `{"tokens_in":5000,"tokens_out":1}`},
		{"not json", `{`},
	} {
		if u, ok := TurnCompleted(tt.data); ok {
			t.Errorf("TurnCompleted(%s) = %+v, want !ok", tt.name, u)
		}
	}
}

func TestTurnUsageEmpty(t *testing.T) {
	if !(TurnUsage{}).Empty() {
		t.Error("zero TurnUsage is not Empty")
	}
	// A free turn still reports a model, so an all-zero-cost turn is not empty.
	if (TurnUsage{Model: "m"}).Empty() {
		t.Error("TurnUsage{Model} reported Empty")
	}
	if (TurnUsage{TokensOut: 1}).Empty() {
		t.Error("TurnUsage{TokensOut} reported Empty")
	}
}
