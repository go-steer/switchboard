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

	"github.com/go-steer/switchboard/pkg/chat"
)

// TestClassifyPlatformErrors checks Slack's error codes reach callers as the
// provider-neutral sentinels in pkg/chat, so a caller can tell a permanent
// failure from one worth retrying without learning Slack's vocabulary. The
// codes are asserted through a real Send/Update rather than against classify
// directly, so the wrapping in the egress paths is covered too.
func TestClassifyPlatformErrors(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{
		{"channel_not_found", chat.ErrNotFound},
		{"message_not_found", chat.ErrNotFound},
		{"not_in_channel", chat.ErrDenied},
		{"is_archived", chat.ErrDenied},
		{"cant_update_message", chat.ErrDenied},
		{"ratelimited", nil},
		{"internal_error", nil},
	} {
		t.Run(tc.code, func(t *testing.T) {
			mux := http.NewServeMux()
			reply := func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"ok":false,"error":%q}`, tc.code)
			}
			mux.HandleFunc("/chat.postMessage", reply)
			mux.HandleFunc("/chat.update", reply)
			srv := httptest.NewServer(mux)
			defer srv.Close()
			a := newTestAdapter(srv.URL)

			_, sendErr := a.Send(context.Background(), chat.Reply{Conversation: "C0:100.5", Text: "hi"})
			updErr := a.Update(context.Background(),
				chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}, chat.Reply{Text: "hi"})

			for name, err := range map[string]error{"Send": sendErr, "Update": updErr} {
				if err == nil {
					t.Fatalf("%s returned no error for %q", name, tc.code)
				}
				if tc.want == nil {
					if errors.Is(err, chat.ErrNotFound) || errors.Is(err, chat.ErrDenied) {
						t.Errorf("%s classified %q as permanent: %v", name, tc.code, err)
					}
				} else if !errors.Is(err, tc.want) {
					t.Errorf("%s error for %q = %v, want errors.Is %v", name, tc.code, err, tc.want)
				}
				// Classification must not clutter the message a human reads,
				// or split it across lines the log cannot hold.
				if !strings.Contains(err.Error(), tc.code) {
					t.Errorf("%s error lost Slack's own wording: %v", name, err)
				}
				if strings.Contains(err.Error(), "\n") {
					t.Errorf("%s error spans lines, which would break the log: %q", name, err)
				}
			}
		})
	}
}

// TestFitsOneMessage checks the limit is measured on the rendered text, not
// the raw text — mrkdwn escaping grows a body, and the outbound ingress
// decides when to roll a growing message over on this answer.
func TestFitsOneMessage(t *testing.T) {
	a := newTestAdapter("http://example.invalid")
	if !a.FitsOneMessage("short") {
		t.Error("a short text was reported as not fitting")
	}
	if a.FitsOneMessage(strings.Repeat("x", slackTextLimit+1)) {
		t.Error("a text past the limit was reported as fitting")
	}
	// "&" renders as "&amp;", so a text that fits raw need not fit rendered.
	ampersands := strings.Repeat("&", slackTextLimit-1)
	if a.FitsOneMessage(ampersands) {
		t.Error("escaping growth was ignored; Slack would truncate the message")
	}
}
