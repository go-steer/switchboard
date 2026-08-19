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

import "testing"

func TestTurnFailed(t *testing.T) {
	for _, tt := range []struct {
		name   string
		data   string
		want   TurnError
		wantOK bool
	}{
		{
			name: "a classified failure, as the daemon emits it",
			data: `{"kind":"auth_error","code":"PERMISSION_DENIED",` +
				`"message":"caller lacks aiplatform.endpoints.predict","retryable":false,` +
				`"hint":"Verify the runtime service account has roles/aiplatform.user."}`,
			want: TurnError{
				Kind: TurnErrorAuth, Code: "PERMISSION_DENIED",
				Message:   "caller lacks aiplatform.endpoints.predict",
				Retryable: false,
				Hint:      "Verify the runtime service account has roles/aiplatform.user.",
			},
			wantOK: true,
		},
		{
			name:   "retryable is carried through, not inferred from the kind",
			data:   `{"kind":"rate_limited","code":"429","message":"quota exceeded","retryable":true}`,
			want:   TurnError{Kind: TurnErrorRateLimited, Code: "429", Message: "quota exceeded", Retryable: true},
			wantOK: true,
		},
		{
			name:   "unknown fields from a newer daemon are ignored, not fatal",
			data:   `{"kind":"watchdog","message":"5 identical tool calls","signal":"repeated_tool_call","turn":7}`,
			want:   TurnError{Kind: TurnErrorWatchdog, Message: "5 identical tool calls"},
			wantOK: true,
		},
		{
			name:   "an unrecognized kind is preserved, not normalized away",
			data:   `{"kind":"quota_frozen","message":"billing account disabled"}`,
			want:   TurnError{Kind: "quota_frozen", Message: "billing account disabled"},
			wantOK: true,
		},
		{
			name:   "a message with no kind is still worth surfacing",
			data:   `{"message":"the model exploded"}`,
			want:   TurnError{Kind: TurnErrorUnknown, Message: "the model exploded"},
			wantOK: true,
		},
		{
			name: "a frame with nothing to say is dropped",
			data: `{"retryable":true}`,
		},
		{
			name: "not an object",
			data: `"turn-error"`,
		},
		{
			name: "not JSON",
			data: `kind=auth_error`,
		},
		{
			name: "empty",
			data: ``,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := TurnFailed(tt.data)
			if ok != tt.wantOK {
				t.Fatalf("TurnFailed ok = %v, want %v (got %+v)", ok, tt.wantOK, got)
			}
			if got != tt.want {
				t.Errorf("TurnFailed = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestTurnErrorGuardrail pins the two kinds that mean "an operator has to go
// and do something", because the advice they need is the opposite of every
// other kind's.
func TestTurnErrorGuardrail(t *testing.T) {
	for kind, want := range map[string]bool{
		TurnErrorCostCeiling:   true,
		TurnErrorWatchdog:      true,
		TurnErrorAuth:          false,
		TurnErrorRateLimited:   false,
		TurnErrorTransientNet:  false,
		TurnErrorConfig:        false,
		TurnErrorModelNotFound: false,
		TurnErrorUnknown:       false,
		"":                     false,
		"something_new":        false,
	} {
		if got := (TurnError{Kind: kind}).Guardrail(); got != want {
			t.Errorf("TurnError{Kind: %q}.Guardrail() = %v, want %v", kind, got, want)
		}
	}
}

// TestGuardrailRefusalFallsBackToTheMessage covers every turn after the trip.
// The daemon classifies only the trip itself; the refusals that follow are
// preflight errors routed through its generic classifier, which has no
// guardrail category and reports them as "unknown". Those refusals are the
// common case — one trip, then everything the reader sends until an operator
// resets it — so recognizing them matters more than recognizing the trip.
func TestGuardrailRefusalFallsBackToTheMessage(t *testing.T) {
	for _, tt := range []struct {
		name string
		msg  string
		want bool
	}{
		{
			name: "per-turn ceiling refusal",
			msg: "per-turn cost ceiling exceeded: this turn cost $5.2000, ceiling is $5.0000. " +
				"Agent will refuse new turns until the operator resets it (/guardrail reset, or " +
				"POST /sessions/{id}/guardrails/reset).",
			want: true,
		},
		{
			name: "watchdog refusal",
			msg: "watchdog halted the agent (repeated_tool_call): 5 identical calls. " +
				"Agent will refuse new turns until the operator resets it (/guardrail reset).",
			want: true,
		},
		{
			name: "an ordinary failure is not a guardrail",
			msg:  "caller lacks aiplatform.endpoints.predict",
			want: false,
		},
		{
			name: "no message, no guess",
			msg:  "",
			want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			te := TurnError{Kind: TurnErrorUnknown, Message: tt.msg}
			if got := te.Guardrail(); got != tt.want {
				t.Errorf("Guardrail() = %v, want %v for %q", got, tt.want, tt.msg)
			}
		})
	}
}
