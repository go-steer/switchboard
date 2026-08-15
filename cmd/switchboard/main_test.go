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
	"slices"
	"strings"
	"testing"
)

// TestSplitList checks the allowlist flag parser tolerates the shapes a
// Kubernetes manifest or a shell actually produces.
func TestSplitList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,", nil},
		{"C0123", []string{"C0123"}},
		{"C0123,C4567", []string{"C0123", "C4567"}},
		{" C0123 , C4567 ", []string{"C0123", "C4567"}},
		{"C0123,,C4567,", []string{"C0123", "C4567"}},
		{"C0123:1723742400.0001", []string{"C0123:1723742400.0001"}},
	} {
		if got := splitList(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("splitList(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRunServeIngressValidation checks a misconfigured ingress is refused at
// startup, before anything dials a chat platform — a caller's escalation path
// silently not existing is exactly the failure this surface must not have.
func TestRunServeIngressValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		token string // value for $SWITCHBOARD_INGRESS_TOKEN
		want  string
	}{
		{
			name: "googlechat is refused",
			args: []string{"--platform", "googlechat", "--ingress-addr", "127.0.0.1:0"},
			want: "Slack-only",
		},
		{
			name: "no token",
			args: []string{"--platform", "slack", "--ingress-addr", "127.0.0.1:0"},
			want: "no ingress token",
		},
		{
			name:  "token from a named env var",
			args:  []string{"--platform", "slack", "--ingress-addr", "127.0.0.1:0", "--ingress-token-env", "NOPE_UNSET"},
			token: "set-but-under-the-other-name",
			want:  "$NOPE_UNSET",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SWITCHBOARD_DAEMON_TOKEN", "daemon-token")
			t.Setenv("SWITCHBOARD_INGRESS_TOKEN", tc.token)
			err := runServe(tc.args)
			if err == nil {
				t.Fatal("runServe accepted a misconfigured ingress")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestParseCallerMode and TestParseProgressMode pin the flag vocabulary: a
// typo must fail at startup rather than silently selecting a default.
func TestParseCallerMode(t *testing.T) {
	for _, ok := range []string{"email", "id"} {
		if _, err := parseCallerMode(ok); err != nil {
			t.Errorf("parseCallerMode(%q) = %v, want no error", ok, err)
		}
	}
	if _, err := parseCallerMode("username"); err == nil {
		t.Error("parseCallerMode accepted an unknown mode")
	}
}

func TestParseProgressMode(t *testing.T) {
	for _, ok := range []string{"off", "indicator", "status", "stream"} {
		if _, err := parseProgressMode(ok); err != nil {
			t.Errorf("parseProgressMode(%q) = %v, want no error", ok, err)
		}
	}
	if _, err := parseProgressMode("verbose"); err == nil {
		t.Error("parseProgressMode accepted an unknown mode")
	}
}
