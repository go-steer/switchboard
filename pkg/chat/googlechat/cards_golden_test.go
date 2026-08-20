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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

// updateGolden regenerates testdata/cards rather than asserting against it:
//
//	go test ./pkg/chat/googlechat -run Golden -update
var updateGolden = flag.Bool("update", false, "rewrite the golden card JSON in testdata/cards")

// goldenAnswer is the model turn the answer-card goldens render — headers,
// emphasis, a link, a fence and a rule, so one card exercises every structure
// the renderer lifts out of the text.
const goldenAnswer = "# Deploy check\n\n" +
	"Three things stood out.\n\n" +
	"## Findings\n\n" +
	"The **readiness probe** hits `/healthz` on the wrong port. " +
	"See [the manifest](https://example.com/deploy.yaml).\n\n" +
	"```yaml\nreadinessProbe:\n  httpGet:\n    port: 8080\n```\n\n" +
	"---\n\n" +
	"#### Next\n\nRe-run after patching.\n"

// goldenSpilledAnswer holds one fenced block too long for a single widget, so
// there is a golden showing what a spilled run looks like on the wire:
// consecutive paragraphs, each fence closed in the widget that opened it, and
// no ellipsis anywhere. Sized just over the budget rather than far over it, so
// the file stays something a reviewer can read and paste.
//
// It also records what spilling does not preserve: the continuation widget
// reopens a bare ``` rather than ```sh. Neither Chat nor Slack highlights by
// language, so the tag is inert on both, and the alternative — reopening with
// the opener's info string — is a change to the shared splitter that the text
// path would have to be re-pinned for.
var goldenSpilledAnswer = func() string {
	var b strings.Builder
	b.WriteString("# Enable the APIs\n\nRun this once per project:\n\n```sh\n")
	for i := 0; i < 90; i++ {
		fmt.Fprintf(&b, "gcloud services enable service_%03d.googleapis.com\n", i)
	}
	b.WriteString("```\n")
	return b.String()
}()

func countWidgets(card *chatv1.GoogleAppsCardV1Card) int {
	n := 0
	for _, s := range card.Sections {
		n += len(s.Widgets)
	}
	return n
}

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
		// minWidgets guards a fixture whose point is how many widgets it
		// produces: without it, -update would happily record a card that had
		// stopped spilling, and the golden would pin the bug.
		minWidgets int
	}{
		{"progress", gatewayCard(chat.KindProgress, toChatText("⏳ Working…")), 0},
		{"activity", gatewayCard(chat.KindActivity, toChatText("🔧 Running `bash`")), 0},
		{"notice", gatewayCard(chat.KindNotice, toChatText(
			"⚠️ That turn didn't go through — the daemon returned 503. Try again.")), 0},
		// The ack that names the accepted values: the angle brackets around
		// them have to arrive escaped, since DecoratedText accepts only
		// <b> <i> <s> <a> <br> and renders anything else as broken markup.
		{"ack-with-values", gatewayCard(chat.KindAck,
			toChatText("Progress mode for this channel is *indicator*. "+
				"Change it with `progress <off|indicator|status|stream>`.")), 0},
		// The other ack: the one confirming a change, which names no values.
		// That is the surface the button row was removed from without anything
		// taking its place (#28), so it is pinned to keep the difference from
		// the ack above visible rather than remembered.
		{"ack-plain", gatewayCard(chat.KindAck,
			toChatText("Progress mode for this channel set to *stream*.")), 0},
		{"welcome", welcomeCard([]string{"off", "indicator", "status", "stream"}), 0},
		{"answer", answerCard(goldenAnswer), 0},
		// The whole fence has to survive, across as many widgets as it takes.
		{"answer-spilled", answerCard(goldenSpilledAnswer), 2},
		// The --show-usage footer, pinned separately: it is a widget shape
		// nothing else on the card uses, and the Card Builder is the only place
		// its size and colour can actually be judged.
		{"answer-with-usage", withUsageFooter(answerCard(goldenAnswer), &chat.Usage{
			Model: "gemini-3.7-flash", TokensIn: 5000, TokensOut: 1,
			CostUSD: 0.0037537, Latency: 3142 * time.Millisecond,
		}), 0},
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
			if n := countWidgets(tc.card); n < tc.minWidgets {
				t.Fatalf("card has %d widgets, want at least %d — the fixture no longer "+
					"exercises what it was written for", n, tc.minWidgets)
			}
			// An encoder rather than json.MarshalIndent, for the trailing
			// newline the goldens carry — the one a text editor and git both
			// expect at the end of a file.
			//
			// No SetEscapeHTML(false): it would not reach the angle brackets
			// and ampersands in the widget text anyway, because the generated
			// chatv1 types marshal themselves and hand back bytes already
			// \u-escaped. That is what a golden here looks like, and Card
			// Builder reads it either way, which is what the fixtures are for.
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
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
