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
	"fmt"
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
		{"", CardsRich, true}, // the zero value is the default
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
		// A rule with nothing before it draws no divider, so it is not the
		// structure that makes a card worth building.
		{"leading rule over a bare paragraph", "---\n\njust a paragraph of prose"},
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

// A header whose section has no body of its own used to be dropped along with
// the empty section, silently losing content from a very ordinary shape. Chat
// has no nested sections, so it is carried onto the next header instead.
func TestAnswerCardKeepsHeadersWithNoBody(t *testing.T) {
	card := answerCard("# Results\n\n## Passing\n\nall good\n")
	if card == nil {
		t.Fatalf("no card")
	}
	if len(card.Sections) != 1 {
		t.Fatalf("sections = %d, want 1: %+v", len(card.Sections), card.Sections)
	}
	if got := card.Sections[0].Header; got != "Results — Passing" {
		t.Fatalf("header = %q, want the parent carried onto the child", got)
	}

	// A header at the very end has no next section to be carried onto; it
	// lands as a bold lead-in on the last one rather than disappearing.
	card = answerCard("# Results\n\nall good\n\n## Trailing\n")
	if card == nil {
		t.Fatalf("no card for a trailing header")
	}
	last := card.Sections[len(card.Sections)-1]
	body := last.Widgets[len(last.Widgets)-1].TextParagraph
	if body == nil || !strings.Contains(body.Text, "Trailing") {
		t.Fatalf("trailing header lost: %+v", last.Widgets)
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

// TestAnswerCardSpillsALongSectionInsteadOfTruncating is #32's regression test:
// one long fenced block in a section used to arrive cut at maxWidgetText with an
// ellipsis, where the same answer in status or off mode arrives complete across
// two posts. Truncation is the one failure a fallback cannot excuse — nothing is
// logged and the reader's only signal is the "…".
func TestAnswerCardSpillsALongSectionInsteadOfTruncating(t *testing.T) {
	var body strings.Builder
	body.WriteString("# Deploy\n\nRun it:\n\n```\n")
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&body, "gcloud services enable service_%03d.googleapis.com\n", i)
	}
	body.WriteString("```\n")
	md := body.String()
	if len(md) < 3*maxWidgetText {
		t.Fatalf("fixture is only %d bytes; it has to need several widgets", len(md))
	}

	card := answerCard(md)
	if card == nil {
		t.Fatalf("a headed answer should render as a card")
	}

	var texts []string
	for _, s := range card.Sections {
		for _, w := range s.Widgets {
			if w.TextParagraph == nil {
				continue
			}
			text := w.TextParagraph.Text
			if len(text) > maxWidgetText {
				t.Errorf("widget is %d bytes, over the %d budget", len(text), maxWidgetText)
			}
			if strings.Contains(text, "…") {
				t.Errorf("widget was truncated rather than spilled: %q", lastBytes(text, 80))
			}
			if strings.Count(text, "```")%2 != 0 {
				t.Errorf("widget leaves a fence open, so Chat renders the backticks literally")
			}
			texts = append(texts, text)
		}
	}
	if len(texts) < 2 {
		t.Fatalf("want the section spilled across several widgets, got %d", len(texts))
	}

	// Every line the model wrote is still somewhere in the card, including the
	// last one — the whole point of spilling rather than cutting.
	joined := strings.Join(texts, "\n")
	for _, want := range []string{"service_000", "service_125", "service_249"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q was lost from the card", want)
		}
	}
}

// TestGatewayWidgetsStillClamp pins the other half of the decision: the
// gateway's own widgets keep their budget. Their text is authored here and
// nowhere near it, and they carry HTML, where a split could land inside a tag
// or a character entity.
func TestGatewayWidgetsStillClamp(t *testing.T) {
	long := strings.Repeat("a", maxWidgetText*2)
	w := htmlWidget(long)
	if w == nil || w.TextParagraph == nil {
		t.Fatalf("no widget")
	}
	if len(w.TextParagraph.Text) > maxWidgetText {
		t.Fatalf("html widget is %d bytes, over the %d budget", len(w.TextParagraph.Text), maxWidgetText)
	}
	w = iconTextWidget(iconNotice, long)
	if w == nil || w.DecoratedText == nil {
		t.Fatalf("no widget")
	}
	if len(w.DecoratedText.Text) > maxWidgetText {
		t.Fatalf("icon widget is %d bytes, over the %d budget", len(w.DecoratedText.Text), maxWidgetText)
	}
}

// lastBytes trims a value to its tail for an error message, since what a
// truncation test wants to show is the end of the string.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
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
