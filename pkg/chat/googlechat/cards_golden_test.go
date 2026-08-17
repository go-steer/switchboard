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

package googlechat

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

// updateGolden regenerates testdata/cards rather than asserting against it:
//
//	go test ./pkg/chat/googlechat -run Golden -update
var updateGolden = flag.Bool("update", false, "rewrite the golden card JSON in testdata/cards")

// TestCardsGolden pins the exact JSON each card builder emits.
//
// Unit tests can only check that a card has the shape this package intended.
// Whether Chat's renderer accepts that shape, and what it looks like when it
// does, is not something any assertion here can answer — so these files are
// also the input to a human check: paste one into Google's Card Builder and
// look at it. A diff in review is then a visible change to what users see,
// which is the property a struct-field assertion cannot give.
func TestCardsGolden(t *testing.T) {
	// Text as the router actually writes it, emoji and all — the card path has
	// to cope with the real thing, not a tidied-up sample.
	cases := []struct {
		name string
		card *chatv1.GoogleAppsCardV1Card
	}{
		{"progress", gatewayCard(chat.KindProgress, toChatText("⏳ Working…"))},
		{"activity", gatewayCard(chat.KindActivity, toChatText("🔧 Running `bash`"))},
		{"notice", gatewayCard(chat.KindNotice, toChatText(
			"⚠️ That turn didn't go through — the daemon returned 503. Try again."))},
		{"ack-with-choices", ackCard(
			toChatText("Progress mode for this channel is *indicator*. "+
				"Change it with `progress <off|indicator|status|stream>`."),
			"progress", []string{"off", "indicator", "status", "stream"})},
		{"ack-plain", ackCard(toChatText("Progress mode set to *stream*."), "progress", nil)},
		{"welcome", welcomeCard([]string{"off", "indicator", "status", "stream"})},
		{"answer", answerCard("# Deploy check\n\n" +
			"Three things stood out.\n\n" +
			"## Findings\n\n" +
			"The **readiness probe** hits `/healthz` on the wrong port. " +
			"See [the manifest](https://example.com/deploy.yaml).\n\n" +
			"```yaml\nreadinessProbe:\n  httpGet:\n    port: 8080\n```\n\n" +
			"---\n\n" +
			"#### Next\n\nRe-run after patching.\n")},
	}

	dir := filepath.Join("testdata", "cards")
	if *updateGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.card == nil {
				t.Fatalf("builder returned no card — the golden would be meaningless")
			}
			// An encoder rather than json.Marshal: the default escapes < and &
			// to < / &, which would hide the very HTML these cards
			// are built to emit behind unreadable escapes.
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			if err := enc.Encode(tc.card); err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := buf.Bytes()

			path := filepath.Join(dir, tc.name+".json")
			if *updateGolden {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v (run: go test ./pkg/chat/googlechat -run Golden -update)", path, err)
			}
			if string(got) != string(want) {
				t.Errorf("%s is stale — rerun with -update and eyeball the diff.\n got: %s\nwant: %s",
					path, got, want)
			}
		})
	}
}
