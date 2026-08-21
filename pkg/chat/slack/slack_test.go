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

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/go-steer/switchboard/pkg/chat"
)

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
	for _, bad := range []string{"", ":no-channel"} {
		if _, _, ok := splitConversation(bad); ok {
			t.Errorf("splitConversation(%q) accepted, want rejected", bad)
		}
	}
}

// TestSplitConversationThreadless covers the outbound-ingress shape: a caller
// with no thread to reply in names a bare channel, and egress reads that as
// "post at the top level".
func TestSplitConversationThreadless(t *testing.T) {
	for _, key := range []string{"C0123", "C0123:"} {
		channel, thread, ok := splitConversation(key)
		if !ok || channel != "C0123" || thread != "" {
			t.Errorf("split(%q) = %q, %q, %v; want C0123, \"\", true", key, channel, thread, ok)
		}
	}
}

func TestNewValidation(t *testing.T) {
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

// TestNewWithoutAnAppTokenIsEgressOnly checks the Socket Mode credential is
// required only to receive (#23). An outbound-only deployment posts with the
// bot token and has no use for an app-level token, a WebSocket, or the event
// subscriptions that come with one — and until now could not start without
// them, so its uptime was bound to an inbound path it never read.
func TestNewWithoutAnAppTokenIsEgressOnly(t *testing.T) {
	a, err := New(Config{BotToken: "xoxb-x"})
	if err != nil {
		t.Fatalf("New without an app token: %v", err)
	}
	if a.sm != nil {
		t.Error("built a Socket Mode client with no app token to authenticate it")
	}

	// Run refuses rather than blocking on a source that does not exist, and
	// says so in a way a caller can branch on.
	err = a.Run(context.Background(), nil)
	if !errors.Is(err, chat.ErrNoInbound) {
		t.Errorf("Run = %v, want chat.ErrNoInbound", err)
	}

	// Egress is unaffected: it authenticates with the bot token throughout.
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C0","ts":"111.111"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a.api = slack.New("xoxb-x", slack.OptionAPIURL(srv.URL+"/"))

	ref, err := a.Send(context.Background(), chat.Reply{Conversation: "C0", Text: "digest"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ref.ID != "111.111" {
		t.Errorf("Send returned %+v, want the posted message's ts", ref)
	}
}

// TestRunWithAnAppTokenDoesNotRefuse checks the guard above keys off the
// missing socket and nothing else — an adapter that does have one gets past it
// and stops at the auth check instead, which is as far as this can go without
// a workspace.
func TestRunWithAnAppTokenDoesNotRefuse(t *testing.T) {
	a, err := New(Config{AppToken: "xapp-x", BotToken: "xoxb-x"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Pointed at a local server, so the test is hermetic by construction rather
	// than by the cancelled context below happening to short-circuit the
	// transport before it dials slack.com.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	defer srv.Close()
	a.api = slack.New("xoxb-x", slack.OptionAppLevelToken("xapp-x"), slack.OptionAPIURL(srv.URL+"/"))

	// Cancelled up front so nothing is dialed for long.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx, nil); errors.Is(err, chat.ErrNoInbound) {
		t.Errorf("Run = %v, want anything but ErrNoInbound on a configured adapter", err)
	}
}

func TestParseMentionCommand(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantName string
		wantArgs string // space-joined
	}{
		{"progress", true, "progress", ""},              // bare verb: query
		{"progress status", true, "progress", "status"}, // verb + one arg: set
		{"Progress STATUS", true, "progress", "STATUS"}, // verb case-folded; arg preserved
		{"progress on the ticket?", false, "", ""},      // 4 tokens: an agent turn
		{"progress status extra", false, "", ""},        // 3 tokens: an agent turn
		{"deploy the thing", false, "", ""},             // unknown verb
		{"", false, "", ""},
	}
	for _, tc := range cases {
		cmd, ok := parseMentionCommand(tc.in)
		if ok != tc.wantOK {
			t.Errorf("parseMentionCommand(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if cmd.Name != tc.wantName || strings.Join(cmd.Args, " ") != tc.wantArgs {
			t.Errorf("parseMentionCommand(%q) = {name %q args %v}, want {name %q args %q}",
				tc.in, cmd.Name, cmd.Args, tc.wantName, tc.wantArgs)
		}
	}
}

func TestParseSlashCommand(t *testing.T) {
	cases := []struct {
		text     string
		wantName string
		wantArgs string // space-joined
	}{
		{"progress status", "progress", "status"},
		{"PROGRESS Status Extra", "progress", "Status Extra"}, // explicit surface parses freely
		{"  progress   status  ", "progress", "status"},       // extra whitespace collapses
		{"", "", ""}, // empty text: help
	}
	for _, tc := range cases {
		cmd := parseSlashCommand(slack.SlashCommand{ChannelID: "C1", Text: tc.text})
		if cmd.Channel != "C1" {
			t.Errorf("parseSlashCommand(%q) channel = %q, want C1", tc.text, cmd.Channel)
		}
		if cmd.Name != tc.wantName || strings.Join(cmd.Args, " ") != tc.wantArgs {
			t.Errorf("parseSlashCommand(%q) = {name %q args %v}, want {name %q args %q}",
				tc.text, cmd.Name, cmd.Args, tc.wantName, tc.wantArgs)
		}
	}
}
