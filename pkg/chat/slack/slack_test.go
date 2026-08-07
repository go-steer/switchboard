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

package slack

import "testing"

func TestStripMentions(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"<@U123> hello there", "hello there"},
		{"<@U123|switchboard> deploy the thing", "deploy the thing"},
		{"hey <@U123> can you <@U456> help", "hey can you help"},
		{"   <@U123>   spaced   out   ", "spaced   out"},
		{"no mention here", "no mention here"},
		{"<@U123>", ""},
		// Multi-line turns keep their newlines and indentation so the
		// markdown renderer still sees block structure (regression: a
		// prior strip collapsed all whitespace and flattened the body).
		{"<@U123>\n# Release Notes\n\n- alpha\n- beta", "# Release Notes\n\n- alpha\n- beta"},
		{"<@U123> do this:\n- top\n    - nested", "do this:\n- top\n    - nested"},
	}
	for _, tc := range cases {
		if got := stripMentions(tc.in); got != tc.want {
			t.Errorf("stripMentions(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestThreadRoot(t *testing.T) {
	// A top-level mention (no thread ts) roots the thread on its own ts.
	if got := threadRoot("", "111.0001"); got != "111.0001" {
		t.Errorf("threadRoot top-level = %q, want 111.0001", got)
	}
	// A reply carries the existing thread ts, which must win.
	if got := threadRoot("100.0001", "111.0002"); got != "100.0001" {
		t.Errorf("threadRoot reply = %q, want 100.0001", got)
	}
}

func TestConversationKeyRoundTrip(t *testing.T) {
	key := conversationKey("C0123", "1699.0001")
	if key != "C0123:1699.0001" {
		t.Fatalf("key = %q", key)
	}
	channel, thread, ok := splitConversation(key)
	if !ok || channel != "C0123" || thread != "1699.0001" {
		t.Fatalf("split(%q) = %q, %q, %v", key, channel, thread, ok)
	}
}

func TestSplitConversationRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "no-colon", ":no-channel", "no-thread:"} {
		if _, _, ok := splitConversation(bad); ok {
			t.Errorf("splitConversation(%q) accepted, want rejected", bad)
		}
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{BotToken: "xoxb-x"}); err == nil {
		t.Error("expected error without AppToken")
	}
	if _, err := New(Config{AppToken: "xapp-x"}); err == nil {
		t.Error("expected error without BotToken")
	}
	a, err := New(Config{AppToken: "xapp-x", BotToken: "xoxb-x"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.mode != CallerEmail {
		t.Errorf("default mode = %q, want %q", a.mode, CallerEmail)
	}
	if a.Name() != "slack" {
		t.Errorf("Name = %q", a.Name())
	}
}
