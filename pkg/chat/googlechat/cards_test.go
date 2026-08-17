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

	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

func TestParseCardMode(t *testing.T) {
	tests := []struct {
		in   string
		want CardMode
		ok   bool
	}{
		{"", CardsStatus, true}, // the zero value is the default
		{"off", CardsOff, true},
		{"status", CardsStatus, true},
		{"rich", CardsRich, true},
		{" RICH ", CardsRich, true},
		{"blocks", "", false},
	}
	for _, tt := range tests {
		got, ok := ParseCardMode(tt.in)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Fatalf("ParseCardMode(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestGatewayCardIcons(t *testing.T) {
	tests := []struct {
		kind chat.ReplyKind
		icon string
	}{
		{chat.KindProgress, iconProgress},
		{chat.KindActivity, iconActivity},
		{chat.KindNotice, iconNotice},
		{chat.KindAck, iconAck},
	}
	for _, tt := range tests {
		card := gatewayCard(tt.kind, "something happened")
		if card == nil {
			t.Fatalf("kind %q: no card", tt.kind)
		}
		w := card.Sections[0].Widgets[0]
		if w.DecoratedText == nil || w.DecoratedText.StartIcon == nil {
			t.Fatalf("kind %q: widget carries no icon: %+v", tt.kind, w)
		}
		if got := w.DecoratedText.StartIcon.MaterialIcon.Name; got != tt.icon {
			t.Fatalf("kind %q: icon = %q, want %q", tt.kind, got, tt.icon)
		}
	}
	if gatewayCard(chat.KindAnswer, "an answer") != nil {
		t.Fatalf("an agent answer is not a gateway card")
	}
	if gatewayCard(chat.KindNotice, "   ") != nil {
		t.Fatalf("empty text must not produce a card")
	}
}

// TestGatewayCardStripsTheEmojiTheIconReplaces keeps the card from saying the
// same thing twice — the router's ⏳ and the widget's hourglass.
func TestGatewayCardStripsTheEmojiTheIconReplaces(t *testing.T) {
	card := gatewayCard(chat.KindProgress, "⏳ Working…")
	text := card.Sections[0].Widgets[0].DecoratedText.Text
	if strings.Contains(text, "⏳") {
		t.Fatalf("card text should drop the emoji the icon replaces, got %q", text)
	}
	if text != "Working…" {
		t.Fatalf("card text = %q, want %q", text, "Working…")
	}
}

func TestAckCardButtons(t *testing.T) {
	card := ackCard("Progress mode is *off*.", "progress", []string{"off", "indicator", "", "stream"})
	if card == nil {
		t.Fatalf("no card")
	}
	widgets := card.Sections[0].Widgets
	last := widgets[len(widgets)-1]
	if last.ButtonList == nil {
		t.Fatalf("last widget should be the button list, got %+v", last)
	}
	buttons := last.ButtonList.Buttons
	if len(buttons) != 3 {
		t.Fatalf("want 3 buttons (the empty choice dropped), got %d", len(buttons))
	}
	for i, want := range []string{"off", "indicator", "stream"} {
		b := buttons[i]
		if b.Text != want {
			t.Fatalf("button %d label = %q, want %q", i, b.Text, want)
		}
		params := map[string]string{}
		for _, p := range b.OnClick.Action.Parameters {
			params[p.Key] = p.Value
		}
		// The identity has to ride in the parameters: an add-on that extends
		// Chat never reports back the invoked function name.
		if params[paramCommand] != "progress" || params[paramArg] != want {
			t.Fatalf("button %d parameters = %v", i, params)
		}
	}
}

func TestAckCardWithoutChoices(t *testing.T) {
	card := ackCard("Nothing to configure.", "progress", nil)
	if card == nil {
		t.Fatalf("an ack with no choices is still a card")
	}
	for _, w := range card.Sections[0].Widgets {
		if w.ButtonList != nil {
			t.Fatalf("no choices should mean no buttons")
		}
	}
	if ackCard("   ", "progress", []string{"off"}) != nil {
		t.Fatalf("an empty ack must not produce a card")
	}
}

func TestWelcomeCard(t *testing.T) {
	card := welcomeCard([]string{"off", "stream"})
	if card == nil || card.Header == nil || card.Header.Title != "switchboard" {
		t.Fatalf("welcome card should introduce the app: %+v", card)
	}
	var buttons int
	for _, w := range card.Sections[0].Widgets {
		if w.ButtonList != nil {
			buttons = len(w.ButtonList.Buttons)
		}
	}
	if buttons != 2 {
		t.Fatalf("want 2 progress buttons, got %d", buttons)
	}
	if bare := welcomeCard(nil); bare == nil || bare.Header == nil {
		t.Fatalf("a welcome with no choices is still a card")
	}
}

func TestAnswerCardStructure(t *testing.T) {
	md := "# Findings\n\nThe first thing.\n\n## Detail\n\nMore words.\n\n---\n\nA closing note.\n"
	card := answerCard(md)
	if card == nil {
		t.Fatalf("a structured answer should render as a card")
	}
	if len(card.Sections) != 2 {
		t.Fatalf("want 2 sections (one per top-level header), got %d", len(card.Sections))
	}
	if card.Sections[0].Header != "Findings" || card.Sections[1].Header != "Detail" {
		t.Fatalf("unexpected section headers: %q, %q", card.Sections[0].Header, card.Sections[1].Header)
	}
	var dividers int
	for _, w := range card.Sections[1].Widgets {
		if w.Divider != nil {
			dividers++
		}
		if w.TextParagraph != nil && w.TextParagraph.TextSyntax != "MARKDOWN" {
			t.Fatalf("answer paragraphs should be rendered as markdown, got %q", w.TextParagraph.TextSyntax)
		}
	}
	if dividers != 1 {
		t.Fatalf("want 1 divider for the rule, got %d", dividers)
	}
}

// TestAnswerCardDeepHeadersStayInTheirSection keeps a deeply nested answer from
// fragmenting into a title bar per subheading.
func TestAnswerCardDeepHeaders(t *testing.T) {
	card := answerCard("# Top\n\nbody\n\n#### Deep\n\nmore\n")
	if card == nil {
		t.Fatalf("no card")
	}
	if len(card.Sections) != 1 {
		t.Fatalf("a level-4 header should not open a section, got %d sections", len(card.Sections))
	}
	var sawBold bool
	for _, w := range card.Sections[0].Widgets {
		if w.TextParagraph != nil && w.TextParagraph.Text == "**Deep**" {
			sawBold = true
		}
	}
	if !sawBold {
		t.Fatalf("a deep header should survive as a bold lead-in: %+v", card.Sections[0].Widgets)
	}
}

func TestAnswerCardDeclines(t *testing.T) {
	tests := []struct {
		name string
		md   string
	}{
		{"empty", "   "},
		{"no structure worth a card", "just a paragraph of prose, nothing to lay out"},
		{"only a fence", "```\ncode\n```"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if card := answerCard(tt.md); card != nil {
				t.Fatalf("expected the text path, got a card: %+v", card)
			}
		})
	}
	// Too many widgets to lay out safely: fall back to text.
	var b strings.Builder
	for i := 0; i < maxCardWidgets+5; i++ {
		b.WriteString("# H\n\nbody\n\n")
	}
	if card := answerCard(b.String()); card != nil {
		t.Fatalf("an over-budget answer should fall back to text")
	}
}

// TestAnswerCardFenceIsOpaque checks a header-looking line inside a code block
// is not lifted out as a section.
func TestAnswerCardFenceIsOpaque(t *testing.T) {
	card := answerCard("# Real\n\n```sh\n# not a header\n---\n```\n")
	if card == nil {
		t.Fatalf("no card")
	}
	if len(card.Sections) != 1 {
		t.Fatalf("fence contents must not open sections, got %d", len(card.Sections))
	}
	var body string
	for _, w := range card.Sections[0].Widgets {
		if w.TextParagraph != nil {
			body += w.TextParagraph.Text
		}
		if w.Divider != nil {
			t.Fatalf("a rule inside a fence must not become a divider")
		}
	}
	if !strings.Contains(body, "# not a header") || !strings.Contains(body, "```") {
		t.Fatalf("fence body was mangled: %q", body)
	}
}

func TestSingleCardRejectsEmpty(t *testing.T) {
	if singleCard(nil) != nil {
		t.Fatalf("nil card should produce no cardsV2 list")
	}
	if singleCard(&chatv1.GoogleAppsCardV1Card{}) != nil {
		t.Fatalf("a card with no sections would be rejected by Chat and must not be sent")
	}
	cards := singleCard(widgetCard(htmlWidget("hi")))
	if len(cards) != 1 || cards[0].CardId != cardID {
		t.Fatalf("unexpected cardsV2 list: %+v", cards)
	}
}
