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
	"strings"
	"testing"
)

func TestSummariseArgDelegationSkipsIncludeInput(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			// core-agent's delegation tools are called with a switch beside
			// the request, and the switch sorts first: the notice read
			// "_fallback … true" (seen live). The request is the argument.
			name: "a delegation shows its request, not the switch beside it",
			args: map[string]any{"include_input": true, "request": "check why the billing pods are restarting"},
			want: "check why the billing pods are restarting",
		},
		{
			name: "the switch alone is no summary, in either spelling",
			args: map[string]any{"include_input": true, "includeInput": false},
			want: "",
		},
		{
			// It goes through the same pass as any other argument.
			name: "a delegation's request is clamped and redacted like any other",
			args: map[string]any{"include_input": true, "request": "use --token=s3cr3t " + strings.Repeat("x", 300)},
			want: "use --token=<redacted> " + strings.Repeat("x", argSummaryCap-1-len("use --token=<redacted> ")) + "…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summariseArg(tc.args); got != tc.want {
				t.Errorf("summariseArg(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
