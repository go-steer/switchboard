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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub"
	chatv1 "google.golang.org/api/chat/v1"
)

// The replay corpus lives in testdata/events: one raw Pub/Sub payload per file,
// exactly as Chat delivered it. TestReplay pushes each one through the real
// dispatch path and records what the gateway did in testdata/replay.
//
// The point is fixture provenance. Everything else in this package tests the
// adapter against payloads written by hand from the documentation, which can
// only prove the code matches somebody's reading of the docs. Capture real
// traffic with --googlechat-log-events (see docs/googlechat-setup.md), drop the
// payloads in here, and rerun with -update: the diff shows exactly how Chat's
// real events differ from what was assumed, and from then on they are
// regression tests.
//
// Both dialects belong in the corpus. A payload the decoder ignores is worth
// keeping too — "ignored" is a result, and a future change that starts
// answering a bot's own message shows up as a diff.
//
// Fixtures named *-live-* are captured traffic rather than hand-written, kept
// under their own prefix so the provenance stays visible. Scrub before
// committing: a real payload carries the sender's name, address, avatar URL,
// domain, space IDs, and a configCompleteRedirectUri bearing a token. Replace
// them, keeping each value's shape, and leave everything else exactly as Chat
// sent it — the shape is the whole point of the fixture.

// replayResult is the observable outcome of one event: what reached the router
// and what the gateway sent back.
type replayResult struct {
	Kind     string        `json:"kind"`
	Turns    []replayTurn  `json:"turns,omitempty"`
	Commands []replayCmd   `json:"commands,omitempty"`
	Posts    []replayWrite `json:"posts,omitempty"`
	Patches  []replayWrite `json:"patches,omitempty"`
}

type replayTurn struct {
	Conversation string `json:"conversation"`
	Channel      string `json:"channel"`
	Caller       string `json:"caller"`
	Text         string `json:"text"`
}

type replayCmd struct {
	Name    string   `json:"name"`
	Args    []string `json:"args,omitempty"`
	Channel string   `json:"channel"`
	Caller  string   `json:"caller"`
}

type replayWrite struct {
	Target   string `json:"target"`
	Text     string `json:"text,omitempty"`
	Fallback string `json:"fallback,omitempty"`
	Card     string `json:"card,omitempty"`
}

func TestReplay(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "events", "*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no replay corpus in testdata/events")
	}
	outDir := filepath.Join("testdata", "replay")
	if *updateGolden {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", outDir, err)
		}
	}

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		t.Run(name, func(t *testing.T) {
			payload, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			got := replay(t, payload)

			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			if err := enc.Encode(got); err != nil {
				t.Fatalf("marshal: %v", err)
			}

			path := filepath.Join(outDir, name+".json")
			if *updateGolden {
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v (run: go test ./pkg/chat/googlechat -run Replay -update)", path, err)
			}
			if buf.String() != string(want) {
				t.Errorf("%s changed — rerun with -update and check the diff is intended.\n got: %s\nwant: %s",
					path, buf.String(), want)
			}
		})
	}
}

// replay runs one payload through the real dispatch path against fakes, with
// one mapped command and gateway cards on. status rather than the rich default
// on purpose: these goldens are about decoding and routing an inbound event,
// and everything they post is a gateway message, so rich would pin the same
// output while making every golden churn on an answer-card change. The rich
// answer path is covered end to end by TestSendLongAnswerInRichMode.
func replay(t *testing.T, payload []byte) replayResult {
	t.Helper()
	m := &fakeMessenger{}
	h := &choiceHandler{}
	h.ack = "Progress mode for this channel is *status*."
	h.choices = []string{"off", "indicator", "status", "stream"}

	a := &Adapter{
		msg:   m,
		cards: CardsStatus,
		cmds:  map[int64]string{1: "progress"},
		logf:  func(string, ...any) {},
	}
	a.dispatch(context.Background(), h, &pubsub.Message{Data: payload})

	out := replayResult{Kind: "ignored"}
	for _, msg := range h.msgs {
		out.Kind = "message"
		out.Turns = append(out.Turns, replayTurn{
			Conversation: msg.Conversation,
			Channel:      msg.Channel,
			Caller:       msg.Caller,
			Text:         msg.Text,
		})
	}
	for _, c := range h.cmds {
		out.Kind = "command"
		out.Commands = append(out.Commands, replayCmd{
			Name: c.Name, Args: c.Args, Channel: c.Channel, Caller: c.Caller,
		})
	}
	for _, c := range m.creates {
		if out.Kind == "ignored" {
			out.Kind = "posted"
		}
		out.Posts = append(out.Posts, replayWrite{
			Target: c.parent + "|" + c.thread, Text: c.text, Fallback: c.fallback, Card: describeCard(c.card),
		})
	}
	for _, p := range m.patches {
		out.Patches = append(out.Patches, replayWrite{
			Target: p.name, Text: p.text, Card: describeCard(p.card),
		})
	}
	return out
}

// describeCard summarizes a card in one line. The golden is for spotting
// routing and decode changes; the exact card JSON is pinned by TestCardsGolden.
func describeCard(card *chatv1.GoogleAppsCardV1Card) string {
	if card == nil {
		return ""
	}
	var b strings.Builder
	if card.Header != nil {
		b.WriteString("header=" + card.Header.Title + " ")
	}
	for _, s := range card.Sections {
		for _, w := range s.Widgets {
			switch {
			case w.DecoratedText != nil:
				icon := ""
				if w.DecoratedText.StartIcon != nil && w.DecoratedText.StartIcon.MaterialIcon != nil {
					icon = w.DecoratedText.StartIcon.MaterialIcon.Name + ":"
				}
				b.WriteString("[" + icon + w.DecoratedText.Text + "]")
			case w.TextParagraph != nil:
				b.WriteString("[" + w.TextParagraph.Text + "]")
			case w.ButtonList != nil:
				var labels []string
				for _, btn := range w.ButtonList.Buttons {
					labels = append(labels, btn.Text)
				}
				b.WriteString("[buttons:" + strings.Join(labels, ",") + "]")
			case w.Divider != nil:
				b.WriteString("[---]")
			}
		}
	}
	// Anything clickable is called out by path, so a click affordance that the
	// switch above has no case for still shows up as a golden diff rather than
	// summarising to nothing. A click never reaches this app (#28).
	if found := interactivePaths(card); len(found) > 0 {
		b.WriteString("[clickable:" + strings.Join(found, ",") + "]")
	}
	return b.String()
}
