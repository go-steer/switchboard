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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
)

var testUsage = &chat.Usage{
	Model: "gemini-3.7-flash", TokensIn: 5000, TokensOut: 1,
	CostUSD: 0.0037537, Latency: 3142 * time.Millisecond,
}

const testUsageLine = "gemini-3.7-flash · 5,000 in / 1 out · $0.0038 · 3.1s"

func TestWithUsageFooter(t *testing.T) {
	base := []map[string]any{headerBlock("Results"), sectionBlock("body")}

	got := withUsageFooter(base, testUsage)
	if len(got) != 3 {
		t.Fatalf("blocks = %d, want 3", len(got))
	}
	last := got[2]
	if last["type"] != "context" {
		t.Errorf("footer type = %v, want context", last["type"])
	}
	els, _ := last["elements"].([]any)
	if len(els) != 1 {
		t.Fatalf("footer elements = %d, want 1", len(els))
	}
	el, _ := els[0].(map[string]any)
	if el["type"] != "mrkdwn" || el["text"] != testUsageLine {
		t.Errorf("footer element = %+v, want mrkdwn %q", el, testUsageLine)
	}
}

// TestWithUsageFooterNoRender covers the two cases that must leave the payload
// untouched: no usage to show, and no rich render to attach it to (a nil block
// list means Send is falling back to plain mrkdwn, where the footer is
// deliberately suppressed).
func TestWithUsageFooterNoRender(t *testing.T) {
	base := []map[string]any{sectionBlock("body")}
	if got := withUsageFooter(base, nil); len(got) != 1 {
		t.Errorf("blocks with nil usage = %d, want 1", len(got))
	}
	if got := withUsageFooter(nil, testUsage); got != nil {
		t.Errorf("withUsageFooter(nil, usage) = %v, want nil", got)
	}
	if got := withUsageFooter(base, &chat.Usage{}); len(got) != 1 {
		t.Errorf("blocks with empty usage = %d, want 1", len(got))
	}
}

// TestWithUsageFooterAtBlockCeiling checks the footer survives a payload
// already at Slack's 50-block limit: sanitizeBlocks would otherwise truncate
// the tail, and the footer is the one block that has to stay last.
func TestWithUsageFooterAtBlockCeiling(t *testing.T) {
	base := make([]map[string]any, maxBlocks)
	for i := range base {
		base[i] = sectionBlock("body")
	}
	got := sanitizeBlocks(withUsageFooter(base, testUsage))
	if len(got) != maxBlocks {
		t.Fatalf("blocks = %d, want %d", len(got), maxBlocks)
	}
	if got[len(got)-1]["type"] != "context" {
		t.Errorf("last block = %v, want context", got[len(got)-1]["type"])
	}
}

// TestSendAttachesUsageFooter drives the whole egress: with rich blocks on, the
// posted payload ends in the context block; with them off, the plain mrkdwn
// path posts the answer alone.
func TestSendAttachesUsageFooter(t *testing.T) {
	for _, tt := range []struct {
		name       string
		rich       bool
		wantFooter bool
	}{
		{"rich blocks", true, true},
		{"plain mrkdwn", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var body string
			mux := http.NewServeMux()
			mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				body = r.FormValue("blocks") + "\x00" + r.FormValue("text")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"channel":"C0","ts":"111.111"}`))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			a := newTestAdapter(srv.URL)
			a.richBlocks = tt.rich
			reply := chat.Reply{Conversation: "C0:100.5", Text: "# Results\n\nall good\n", Usage: testUsage}
			if _, err := a.Send(context.Background(), reply); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if got := strings.Contains(body, testUsageLine); got != tt.wantFooter {
				t.Errorf("payload contains usage = %v, want %v (payload: %s)", got, tt.wantFooter, body)
			}
			if !tt.wantFooter {
				return
			}
			// The footer must be the final block, after the answer's own.
			var blocks []map[string]any
			if err := json.Unmarshal([]byte(strings.SplitN(body, "\x00", 2)[0]), &blocks); err != nil {
				t.Fatalf("unmarshal blocks: %v", err)
			}
			if len(blocks) < 2 || blocks[len(blocks)-1]["type"] != "context" {
				t.Errorf("blocks do not end in a context footer: %+v", blocks)
			}
		})
	}
}
