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
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// A stream notice carries its commands in code blocks, which an icon line's
// HTML cannot render; it goes out as a MARKDOWN paragraph instead. A notice
// with no block (status mode's terse line) keeps its icon line.
func TestAStreamNoticeWithACodeBlockRendersAsMarkdown(t *testing.T) {
	a := &Adapter{cards: CardsStatus}
	block := a.cardFor(chat.Reply{Kind: chat.KindActivity, Text: "✅ Ran **bash**\n```\nkubectl get pods -A\n```\n⏱ 12s"})
	if block == nil || block.Sections[0].Widgets[0].TextParagraph == nil ||
		block.Sections[0].Widgets[0].TextParagraph.TextSyntax != "MARKDOWN" {
		t.Fatalf("notice with a code block = %+v, want a MARKDOWN paragraph", block)
	}
	if got := block.Sections[0].Widgets[0].TextParagraph.Text; !strings.Contains(got, "```\nkubectl get pods -A\n```\n⏱ 12s") {
		t.Errorf("paragraph = %q, want the block and the duration line intact", got)
	}
	terse := a.cardFor(chat.Reply{Kind: chat.KindActivity, Text: "🔧 Running `bash`"})
	if terse == nil || terse.Sections[0].Widgets[0].DecoratedText == nil {
		t.Errorf("terse notice = %+v, want its icon line", terse)
	}
}

// A frame of several calls renders one widget per block — the header, then
// each call — so Chat stacks them. In one MARKDOWN paragraph Chat dropped the
// blank lines between calls and a four-call notice ran together on one line
// (reported from the GKE deployment).
func TestAMultiCallNoticeStacksOneWidgetPerCall(t *testing.T) {
	a := &Adapter{cards: CardsRich}
	text := "✔\uFE0E Ran 3 tools\n\n" +
		"✔\uFE0E **retrieve_raw**\n```\ncall_1\n```\n⏱ 0.6s\n\n" +
		"✔\uFE0E **retrieve_raw**\n```\ncall_2\n```\n⏱ 0.6s\n\n" +
		"✔\uFE0E **gke_get_k8s_resource** ×2\n```\nTABLE\n```"
	card := a.cardFor(chat.Reply{Kind: chat.KindActivity, Text: text})
	if card == nil {
		t.Fatal("no card")
	}
	w := card.Sections[0].Widgets
	if len(w) != 4 {
		t.Fatalf("%d widgets, want 4 (header + one per call)", len(w))
	}
	if got := w[0].TextParagraph.Text; got != "✔\uFE0E Ran 3 tools" {
		t.Errorf("header widget = %q", got)
	}
	if got := w[2].TextParagraph.Text; got != "✔\uFE0E **retrieve_raw**\n```\ncall_2\n```\n⏱ 0.6s" {
		t.Errorf("second call widget = %q, want the call's whole block, fence intact", got)
	}
	for i, x := range w {
		if x.TextParagraph == nil || x.TextParagraph.TextSyntax != "MARKDOWN" {
			t.Errorf("widget %d is not a MARKDOWN paragraph: %+v", i, x)
		}
	}
}

// The router's heavy marks (✔/✖ with the text-style selector, ▸) still pick
// the verdict icon, and are stripped from the icon line's text so the mark is
// not shown twice. ✅/❌ from an older router still map too.
func TestTheNoticeMarksPickTheVerdictIcon(t *testing.T) {
	for text, want := range map[string]string{
		"✔\uFE0E Ran `bash`":    iconToolOK,
		"✖\uFE0E Ran `bash`":    iconToolFail,
		"▸ Running `bash`":      iconActivity,
		"✅ Ran `bash`":          iconToolOK,
		"❌ Ran `bash` (exit 2)": iconToolFail,
	} {
		if got := activityIcon(text); got != want {
			t.Errorf("activityIcon(%q) = %q, want %q", text, got, want)
		}
	}
	for _, text := range []string{"✔\uFE0E Ran `bash`", "✖\uFE0E Ran `bash`", "▸ Running `bash`"} {
		if got := stripLeadEmoji(text); strings.ContainsAny(got, "✔✖▸\uFE0E") {
			t.Errorf("stripLeadEmoji(%q) = %q, still carries the mark", text, got)
		}
	}
}

// A frame of more distinct calls than a card holds widgets falls back to one
// chunked paragraph rather than a card Chat would reject — on an edit, a
// rejection leaves the notice stuck at ▸ (caught in review).
func TestAHugeFrameStaysUnderTheWidgetBudget(t *testing.T) {
	a := &Adapter{cards: CardsRich}
	var b strings.Builder
	b.WriteString("✔\uFE0E Ran 120 tools")
	for i := 0; i < 120; i++ {
		b.WriteString("\n\n✔\uFE0E **bash**\n```\necho " + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + "\n```")
	}
	card := a.cardFor(chat.Reply{Kind: chat.KindActivity, Text: b.String()})
	if card == nil {
		t.Fatal("no card")
	}
	if n := len(card.Sections[0].Widgets); n > maxCardWidgets {
		t.Errorf("%d widgets, over the budget of %d", n, maxCardWidgets)
	}
}
