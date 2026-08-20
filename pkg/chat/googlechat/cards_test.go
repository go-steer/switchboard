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
	"encoding/json"
	"fmt"
	"slices"
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
	// A tool notice carries its verdict in the emoji it leads with, and
	// iconTextWidget strips that emoji on the reasoning that the widget's icon
	// already says the same thing. Once tool notices gained verdicts (#36) it
	// no longer did: ✅ and ❌ were both deleted and replaced by the same gear,
	// so a card reader could not see that a tool had failed. Translate, don't
	// discard.
	verdicts := []struct {
		text string
		icon string
	}{
		{"✅ Ran `bash` — make test", iconToolOK},
		{"❌ Ran 3 tools (1 failed)", iconToolFail},
		{"🔧 Running `bash`", iconActivity},
		{"Ran `bash`", iconActivity},
	}
	for _, tt := range verdicts {
		// Through toChatText, as cardFor calls it: activityIcon reads the lead
		// emoji off that output, not off the router's raw text.
		card := gatewayCard(chat.KindActivity, toChatText(tt.text))
		if card == nil {
			t.Fatalf("%q: no card", tt.text)
		}
		if got := card.Sections[0].Widgets[0].DecoratedText.StartIcon.MaterialIcon.Name; got != tt.icon {
			t.Fatalf("%q: icon = %q, want %q", tt.text, got, tt.icon)
		}
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

// TestGatewayCardsCarryNothingClickable: a click never reaches this app (#28),
// so no card the gateway sends may carry a control. The list of cards is
// hand-maintained — a new one has to be added here, and a spilled answer and
// the usage footer are listed because those are the shapes most easily
// forgotten — but what counts as a control is not: interactivePaths reads the
// card's JSON, so a widget kind added to chatv1 is covered without anyone
// remembering it.
func TestGatewayCardsCarryNothingClickable(t *testing.T) {
	spilled := strings.Repeat("A sentence that goes on. ", 400)
	cards := map[string]*chatv1.GoogleAppsCardV1Card{
		"progress":                gatewayCard(chat.KindProgress, "Working…"),
		"activity":                gatewayCard(chat.KindActivity, "Running `bash`"),
		"notice":                  gatewayCard(chat.KindNotice, "That turn didn't go through."),
		"ack":                     gatewayCard(chat.KindAck, "Progress mode is *off*."),
		"welcome":                 welcomeCard([]string{"off", "indicator", "status", "stream"}),
		"welcome-without-choices": welcomeCard(nil),
		"answer":                  answerCard("# Findings\n\nThe first thing.\n"),
		"answer-spilled":          answerCard("# Findings\n\n" + spilled),
		"answer-with-usage": withUsageFooter(answerCard("# Findings\n\nThe first thing.\n"),
			&chat.Usage{Model: "echo", TokensIn: 10, TokensOut: 20}),
	}
	for name, card := range cards {
		if card == nil {
			t.Fatalf("%s: no card", name)
		}
		assertNothingClickable(t, name, card)
	}
}

// assertNothingClickable fails when a card carries a control. It covers the
// card only; Chat's message-level accessoryWidgets would be the other half,
// and nothing here sets one.
func assertNothingClickable(t *testing.T, name string, card *chatv1.GoogleAppsCardV1Card) {
	t.Helper()
	if found := interactivePaths(card); len(found) > 0 {
		t.Errorf("%s card carries a control at %s. No card this gateway sends has one: "+
			"an action click never reaches the app (#28), and an openLink button, which "+
			"sends no event at all, is untested here (#29)", name, strings.Join(found, ", "))
	}
}

// clickableFields are the words Chat spells its interactive fields with. Every
// affordance that sends the app an event carries one of them in its JSON name —
// buttonList, chipList, onClick, switchControl, textInput, selectionInput,
// dateTimePicker, overflowMenu, cardActions, eventActions — so matching the name
// catches the widget kinds chatv1 has not grown yet.
//
// Two deliberate edges. It is a ban on controls, not only on event-senders: an
// openLink button sends the app nothing and would match anyway, which is right
// while none has been tried here (#29). And it is not a ban on everything a
// user can operate: a collapsible section and a maxLines paragraph both render
// an expander, and neither has "control" in its name — they are inert, so they
// are out of scope rather than missed.
//
// None of the fields the gateway's own cards emit contains one of these words:
// header, title, subtitle, sections, widgets, textParagraph, text, textSyntax,
// maxLines, decoratedText, startIcon, materialIcon, wrapText, divider.
var clickableFields = []string{
	"button", "chip", "onclick", "action", "control", "input", "picker", "selection", "menu",
}

// interactivePaths returns the dotted JSON paths at which a card carries a
// control, sorted so a failure reads the same every run. It walks the marshalled
// card rather than the Go struct because that is the shape Chat is actually
// handed, and because a field nobody has heard of still has a name. Only keys
// are examined, so text the model wrote cannot trip it.
//
// The walk stops descending at a match: the subtree under a control is more of
// the same control, and the outermost path is the one worth naming.
func interactivePaths(card *chatv1.GoogleAppsCardV1Card) []string {
	b, err := json.Marshal(card)
	if err != nil {
		return []string{fmt.Sprintf("<unmarshalable: %v>", err)}
	}
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		return []string{fmt.Sprintf("<undecodable: %v>", err)}
	}
	var found []string
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch node := v.(type) {
		case map[string]any:
			for k, sub := range node {
				at := path + "." + k
				if isClickableField(k) {
					found = append(found, strings.TrimPrefix(at, "."))
					continue
				}
				walk(at, sub)
			}
		case []any:
			for _, sub := range node {
				walk(path+"[]", sub)
			}
		}
	}
	walk("", tree)
	slices.Sort(found)
	return found
}

func isClickableField(name string) bool {
	lower := strings.ToLower(name)
	for _, word := range clickableFields {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// TestInteractivePathsFindsTheAffordancesItClaimsTo: the invariant above is only
// as good as this, so the shapes a card could regrow a control in are checked
// against it directly — including the two that a ButtonList-only check missed.
func TestInteractivePathsFindsTheAffordancesItClaimsTo(t *testing.T) {
	clean := welcomeCard([]string{"off", "stream"})
	if got := interactivePaths(clean); len(got) != 0 {
		t.Fatalf("the shipped welcome should be clean, got %v", got)
	}
	button := &chatv1.GoogleAppsCardV1Button{Text: "off"}
	grafts := map[string]func(*chatv1.GoogleAppsCardV1Card){
		"sections[].widgets[].buttonList": func(c *chatv1.GoogleAppsCardV1Card) {
			c.Sections[0].Widgets = append(c.Sections[0].Widgets, &chatv1.GoogleAppsCardV1Widget{
				ButtonList: &chatv1.GoogleAppsCardV1ButtonList{Buttons: []*chatv1.GoogleAppsCardV1Button{button}},
			})
		},
		"sections[].widgets[].decoratedText.button": func(c *chatv1.GoogleAppsCardV1Card) {
			c.Sections[0].Widgets[1].DecoratedText.Button = button
		},
		"fixedFooter.primaryButton": func(c *chatv1.GoogleAppsCardV1Card) {
			c.FixedFooter = &chatv1.GoogleAppsCardV1CardFixedFooter{PrimaryButton: button}
		},
		"sections[].collapseControl": func(c *chatv1.GoogleAppsCardV1Card) {
			c.Sections[0].CollapseControl = &chatv1.GoogleAppsCardV1CollapseControl{HorizontalAlignment: "START"}
		},
	}
	for want, graft := range grafts {
		card := welcomeCard([]string{"off", "stream"})
		graft(card)
		if got := interactivePaths(card); len(got) != 1 || got[0] != want {
			t.Errorf("grafting %s: interactivePaths = %v, want exactly [%s]", want, got, want)
		}
	}
}

func TestAckCardIsJustTheAck(t *testing.T) {
	card := gatewayCard(chat.KindAck, "Nothing to configure.")
	if card == nil {
		t.Fatalf("an ack should be a card")
	}
	if n := len(card.Sections[0].Widgets); n != 1 {
		t.Fatalf("want one widget, got %d", n)
	}
	if gatewayCard(chat.KindAck, "   ") != nil {
		t.Fatalf("an empty ack must not produce a card")
	}
}

func TestWelcomeCard(t *testing.T) {
	card := welcomeCard([]string{"off", "", " stream "})
	if card == nil || card.Header == nil || card.Header.Title != "switchboard" {
		t.Fatalf("welcome card should introduce the app: %+v", card)
	}
	text := cardText(card)
	// The values come from the handler and are named in the text, since a
	// button row cannot be clicked (#28). Neither a blank choice nor a padded
	// one may widen the list to "off||stream" or "off| stream ".
	if !strings.Contains(text, "progress &lt;off|stream&gt;") {
		t.Fatalf("welcome should name the accepted progress values, got %q", text)
	}
	// A handler with no choices to report still gets a welcome, and must not
	// be given an empty argument list to read.
	bare := welcomeCard(nil)
	if bare == nil || bare.Header == nil {
		t.Fatalf("a welcome with no choices is still a card")
	}
	bareText := cardText(bare)
	if strings.Contains(bareText, "progress &lt;") {
		t.Fatalf("no choices should mean no argument list, got %q", bareText)
	}
	// The list goes, the command does not: a welcome that never says
	// "progress" leaves the setting undiscoverable.
	if !strings.Contains(bareText, "progress") {
		t.Fatalf("a welcome should name the command even with no values, got %q", bareText)
	}
	if empty := welcomeCard([]string{"", "  "}); empty == nil ||
		cardText(empty) != bareText {
		t.Fatalf("choices that are all blank should read exactly the same as none")
	}
}

// TestWelcomeTextMatchesTheCard: the card and its text fallback are one
// message, so both name the handler's values and neither hard-codes them. They
// used to be a derived list and a literal, free to drift apart unseen — the
// fallback is only rendered where the card is not.
func TestWelcomeTextMatchesTheCard(t *testing.T) {
	choices := []string{"off", "stream"}
	if text := welcomeTextFor(choices); !strings.Contains(text, "progress <off|stream>") {
		t.Fatalf("the fallback should name the handler's values, got %q", text)
	}
	bare := welcomeTextFor(nil)
	if strings.Contains(bare, "progress <") {
		t.Fatalf("no choices should mean no argument list, got %q", bare)
	}
	if !strings.Contains(bare, "`progress`") {
		t.Fatalf("the fallback should still name the command, got %q", bare)
	}
	// Same sentence in both, modulo the card's HTML escaping of the brackets.
	hint := progressHint(choices)
	if !strings.Contains(welcomeTextFor(choices), hint) {
		t.Fatalf("fallback dropped the progress hint %q", hint)
	}
	if card := welcomeCard(choices); card == nil ||
		!strings.Contains(cardText(card), toCardHTML(hint)) {
		t.Fatalf("card dropped the progress hint %q", hint)
	}
}

// cardText joins every widget's rendered text, so a test can assert on
// what the card says without depending on which widget kind says it.
func cardText(card *chatv1.GoogleAppsCardV1Card) string {
	var b strings.Builder
	for _, sec := range card.Sections {
		for _, w := range sec.Widgets {
			if w.TextParagraph != nil {
				b.WriteString(w.TextParagraph.Text + "\n")
			}
			if w.DecoratedText != nil {
				b.WriteString(w.DecoratedText.Text + "\n")
			}
		}
	}
	return b.String()
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

// TestAnswerCardBailsBeforeChatsMessageCeiling is the other half of spilling:
// an answer big enough that the card would not fit in one Chat message has to
// go out as text, and has to do it without spending a rejected write first.
// Chat caps a message at 32,000 bytes; maxCardWidgets never gets near that,
// because a spilled widget is up to maxWidgetText and eighty of them is a
// quarter of a megabyte.
func TestAnswerCardBailsBeforeChatsMessageCeiling(t *testing.T) {
	body := func(lines int) string {
		var b strings.Builder
		b.WriteString("# Deploy\n\nRun it:\n\n```hcl\n")
		for i := 0; i < lines; i++ {
			fmt.Fprintf(&b, "resource \"google_project_service\" \"svc_%04d\" { service = \"x\" }\n", i)
		}
		b.WriteString("```\n")
		return b.String()
	}

	// Comfortably inside the ceiling: still a card, and still spilled.
	card := answerCard(body(200))
	if card == nil {
		t.Fatalf("an answer that fits should still render as a card")
	}
	if n := cardBytes(card); n > maxCardBytes {
		t.Fatalf("card is %d bytes, over the %d budget", n, maxCardBytes)
	}

	// Past it: text, which splits across as many messages as it needs.
	for _, lines := range []int{600, 2000} {
		if card := answerCard(body(lines)); card != nil {
			t.Errorf("a %d-byte answer produced a %d-byte card; want the text path",
				len(body(lines)), cardBytes(card))
		}
	}

	// The bail has to leave room for everything else on the message: the
	// fallback text and the usage footer both ride alongside the card.
	biggest := answerCard(body(430))
	for lines := 430; lines > 0 && biggest == nil; lines -= 10 {
		biggest = answerCard(body(lines))
	}
	if biggest == nil {
		t.Fatalf("no card at any size")
	}
	withFooter := withUsageFooter(biggest, &chat.Usage{
		Model: "gemini-3.7-flash", TokensIn: 5000, TokensOut: 1, CostUSD: 0.0037537,
	})
	if n := cardBytes(withFooter) + chatTextLimit; n > 32000 {
		t.Errorf("card plus fallback text is %d bytes, over Chat's 32000 ceiling", n)
	}
}

// TestGatewayWidgetsStillClamp pins the other half of the decision: the
// gateway's own widgets keep their budget, as a backstop that cannot fire —
// their text is authored here, fixed, and nowhere near it. Splitting them would
// be no safer: they carry HTML, and a clamp is a byte cut that can land inside
// a tag or an entity just as a split can.
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
