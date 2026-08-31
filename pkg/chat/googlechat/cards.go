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

// This file builds the Google Chat card (cardsV2) payloads the adapter can send
// instead of flat text. Two families live here:
//
//   - Gateway cards — progress, activity, notice, welcome, and the command
//     acknowledgment. These say something *about* the gateway rather than
//     carrying model output, and a card is what makes them legible at a glance:
//     an icon distinguishes "still working" from "that failed".
//   - The answer card — an opt-in structural render of a model turn, giving
//     real section headers and dividers that flat text can only approximate.
//
// Two rules, borrowed from the Slack Block Kit renderer they mirror:
//
//   - A builder returns nil on empty, over-limit, or unexpected input, and the
//     caller then sends plain text. A rich render is a nice-to-have; it must
//     never lose a message.
//   - Every card the adapter sends is paired with a text fallback (Chat shows
//     it in notifications and where cards cannot render), and the card is
//     clamped to Chat's limits before the API call — never Text *and* CardsV2
//     on one message, which would render the same content twice.
//
// No card here carries a button, or any other widget a user can operate. A
// live click on one reached nothing: the app's connection settings route four
// triggers (message, app command, added-to-space, removed-from-space) and a
// click is not among them, so Chat answered it with "Switchboard is unable to
// process your request" and no event arrived (#28). That is a limit of the
// add-on dialect, the one this gateway ships — the legacy Chat-API dialect does
// deliver clicks over the same Pub/Sub transport — and the gateway does not
// render buttons for legacy either, because a control whose existence depends
// on which console checkbox an operator ticked is worse than no control. See
// DESIGN §3.3 for why the add-on dialect is the one worth designing against.
// Where the welcome's row used to sit, the accepted values are named in the
// text instead. Decoding a click is still implemented, for the HTTP ingress in
// #29 — see event.go.
package googlechat

import (
	"encoding/json"
	"regexp"
	"strings"

	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

// Chat card limits and identifiers.
const (
	// maxCardWidgets caps the widgets in a rendered answer card. Chat's own
	// ceiling is 100; stopping short leaves room for the header and keeps a
	// pathological turn from producing an unreadable wall of widgets.
	maxCardWidgets = 80
	// maxWidgetText is the per-widget text budget. Chat accepts more in a
	// paragraph, but a widget this long is already collapsed behind "show
	// more" in every client.
	//
	// An answer's paragraph spills over this into consecutive widgets rather
	// than being cut at it: that is a presentation limit, and presentation is
	// no reason to discard what the model wrote. The gateway's own widgets do
	// still clamp, because their text is authored here and is nowhere near the
	// budget — a clamp that can never fire is the cheapest backstop there is.
	// It would not be a safe *split* if it did fire: those widgets carry HTML,
	// and clamp is a byte cut that can land inside a tag or an entity.
	maxWidgetText = 3500
	// maxCardHeader is the budget for a section header, which is one line in
	// every client — a paragraph's worth of text there just gets clipped.
	maxCardHeader = 200
	// maxCardBytes is the budget for the whole rendered card, measured as the
	// JSON Chat is actually handed. Chat caps a message — text, cardsV2 and
	// accessory widgets together — at 32,000 bytes, and tells an app over it to
	// send several messages instead, which is what the text path already does.
	//
	// This is the binding constraint, not maxCardWidgets: spilling produces
	// widgets of up to maxWidgetText, so a card reaches 32 KB at around a dozen
	// of them and would need over 250 KB of answer to reach eighty. Without
	// this bail an over-long answer would cost a rejected write before the text
	// fallback ran — and Chat's per-space write quota is one per second.
	//
	// Held under the real ceiling by enough for the rest of the message: the
	// fallback text (clamped to chatTextLimit), the usage footer appended after
	// this check, and the thread and card envelope.
	maxCardBytes = 26000
	// cardID names the single card on a message. Chat requires an id per card;
	// it is only used to address the card within the message.
	cardID = "switchboard"
)

// Material icons for the gateway cards. Named rather than inlined so the set
// stays visible in one place.
const (
	iconProgress = "hourglass_top"
	iconActivity = "settings"
	iconToolOK   = "check_circle"
	iconToolFail = "cancel"
	iconNotice   = "error"
	iconAck      = "tune"
	iconWelcome  = "waving_hand"
	iconUsage    = "receipt_long"
)

// CardMode selects how much of the adapter's output is rendered as cards. It is
// a mode rather than a bool because the two card families carry very different
// risk: gateway cards are small, fixed shapes the gateway itself authors, while
// an answer card is a render of arbitrary model output. An operator who wants
// the first without the second can have it.
type CardMode string

const (
	// CardsOff sends everything as plain Chat text.
	CardsOff CardMode = "off"
	// CardsStatus renders the gateway's own messages — progress, activity,
	// notices, command acknowledgments, the welcome — as cards, and leaves
	// model answers as text.
	CardsStatus CardMode = "status"
	// CardsRich additionally renders model answers as structural cards. The
	// default: a card is not chunked, so it is the only mode in which a long
	// fenced answer cannot straddle a message boundary at all, and the render
	// is already conditional — answerCard returns nil unless the answer has a
	// header or a rule that draws, so a conversational reply behaves exactly
	// as it does in status mode. A render bug cannot cost a reply either: a
	// panic recovers into nil and a card Chat rejects falls back to text.
	CardsRich CardMode = "rich"
)

// ParseCardMode validates a --googlechat-cards value.
func ParseCardMode(s string) (CardMode, bool) {
	switch CardMode(strings.ToLower(strings.TrimSpace(s))) {
	case CardsOff:
		return CardsOff, true
	case CardsStatus:
		return CardsStatus, true
	case CardsRich, "":
		return CardsRich, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Widget helpers
// ---------------------------------------------------------------------------

// markdownWidgets renders a run of a model turn with Chat's markdown text
// syntax, so its own markup — lists, emphasis, links, fenced code — is passed
// through for Chat to render rather than translated first. Chat-only, and used
// only by the opt-in answer card. Returns nil when the text reduces to nothing
// (Chat rejects an empty widget).
//
// Text over the per-widget budget becomes several consecutive widgets rather
// than one cut short. A section body is whatever sits between two headers, so
// one long fenced block is all it takes to pass the budget, and the text path
// posts that same answer complete across several messages — a card must not be
// the mode that loses the tail of it (#32). Splitting is the shared fence-aware
// one, because a widget boundary inside a ``` renders the backticks literally
// exactly as a message boundary does.
//
// Links and fenced blocks were confirmed to render this way in the Card
// Builder (see docs/googlechat-setup.md §A); that is the evidence this path
// rests on, since nothing offline can check it.
func markdownWidgets(md string) []*chatv1.GoogleAppsCardV1Widget {
	md = strings.TrimSpace(md)
	if md == "" {
		return nil
	}
	parts := chat.ChunkText(md, maxWidgetText)
	out := make([]*chatv1.GoogleAppsCardV1Widget, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, &chatv1.GoogleAppsCardV1Widget{
			TextParagraph: &chatv1.GoogleAppsCardV1TextParagraph{
				Text:       p,
				TextSyntax: "MARKDOWN",
			},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// htmlWidget is a paragraph of gateway-authored text. It takes Chat markup and
// emits the HTML subset a text paragraph renders by default — the conservative
// path, used wherever the gateway speaks for itself.
func htmlWidget(text string) *chatv1.GoogleAppsCardV1Widget {
	body := toCardHTML(strings.TrimSpace(text))
	if body == "" {
		return nil
	}
	return &chatv1.GoogleAppsCardV1Widget{
		TextParagraph: &chatv1.GoogleAppsCardV1TextParagraph{Text: clamp(body, maxWidgetText)},
	}
}

// iconTextWidget is a line of text preceded by a Material icon — the shape
// every gateway card leads with, because the icon is what makes the message's
// nature readable before the text is. text arrives as Chat markup and is
// converted to the HTML subset DecoratedText accepts, which is also what
// escapes any angle brackets in it.
func iconTextWidget(icon, text string) *chatv1.GoogleAppsCardV1Widget {
	body := toCardHTML(stripLeadEmoji(text))
	if body == "" {
		return nil
	}
	return &chatv1.GoogleAppsCardV1Widget{
		DecoratedText: &chatv1.GoogleAppsCardV1DecoratedText{
			StartIcon: &chatv1.GoogleAppsCardV1Icon{
				MaterialIcon: &chatv1.GoogleAppsCardV1MaterialIcon{Name: icon},
			},
			Text:     clamp(body, maxWidgetText),
			WrapText: true,
		},
	}
}

func dividerWidget() *chatv1.GoogleAppsCardV1Widget {
	return &chatv1.GoogleAppsCardV1Widget{Divider: &chatv1.GoogleAppsCardV1Divider{}}
}

// singleCard wraps one card for a message's cardsV2 list, or nil when the card
// has no sections — an empty card is rejected by Chat and would lose the reply.
func singleCard(card *chatv1.GoogleAppsCardV1Card) []*chatv1.CardWithId {
	if card == nil || len(card.Sections) == 0 {
		return nil
	}
	return []*chatv1.CardWithId{{CardId: cardID, Card: card}}
}

// widgetCard assembles one unheaded section from widgets, dropping the nils the
// builders return for empty input. Returns nil when nothing is left.
func widgetCard(widgets ...*chatv1.GoogleAppsCardV1Widget) *chatv1.GoogleAppsCardV1Card {
	kept := make([]*chatv1.GoogleAppsCardV1Widget, 0, len(widgets))
	for _, w := range widgets {
		if w != nil {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return &chatv1.GoogleAppsCardV1Card{
		Sections: []*chatv1.GoogleAppsCardV1Section{{Widgets: kept}},
	}
}

// ---------------------------------------------------------------------------
// Gateway cards
// ---------------------------------------------------------------------------

// gatewayCard renders one of the gateway's own replies. The Kind is what the
// router said the message *means*; this is where that meaning becomes an icon.
// Returns nil for a kind that has no card (an agent answer) or empty text.
func gatewayCard(kind chat.ReplyKind, text string) *chatv1.GoogleAppsCardV1Card {
	switch kind {
	case chat.KindProgress:
		return widgetCard(iconTextWidget(iconProgress, text))
	case chat.KindActivity:
		return widgetCard(iconTextWidget(activityIcon(text), text))
	case chat.KindNotice:
		return widgetCard(iconTextWidget(iconNotice, text))
	case chat.KindAck:
		return widgetCard(iconTextWidget(iconAck, text))
	}
	return nil
}

// activityIcon picks the material icon for a tool-activity notice from the
// emoji the router led it with.
//
// iconTextWidget strips that emoji, on the reasoning that the widget's icon
// already says what it says. That holds while every notice of a kind means the
// same thing, and stopped holding when tool notices gained verdicts (#36): the
// difference between ✅ and ❌ was deleted and a static gear put in its place,
// so a card reader could not see that a tool had failed. Translate it instead.
func activityIcon(text string) string {
	switch {
	case strings.HasPrefix(text, "✅"):
		return iconToolOK
	case strings.HasPrefix(text, "❌"):
		return iconToolFail
	}
	return iconActivity
}

// welcomeCard greets a space the app was just added to. It is the one card with
// a header: this is the only message that has to introduce the app itself.
//
// choices names the progress modes in the text. It was a row of buttons until
// #28 established that a click never reaches this app, which made the row a
// control that could only ever fail. The values still come from the handler
// rather than a literal here, so this package learns no router vocabulary
// either way, and #29 is where the row can come back.
func welcomeCard(choices []string) *chatv1.GoogleAppsCardV1Card {
	card := widgetCard(
		htmlWidget("Mention me in a thread and I'll relay the turn to the agent. "+
			"Every reply lands back in the same thread, and a thread is one conversation."),
		iconTextWidget(iconWelcome, progressHint(choices)),
	)
	if card == nil {
		return nil
	}
	card.Header = &chatv1.GoogleAppsCardV1CardHeader{
		Title:    "switchboard",
		Subtitle: "chat gateway for core-agent",
	}
	return card
}

// welcomeTextFor is the plain-text form of the welcome, used when cards are off
// and as the card's fallback. It takes the same choices the card does: the two
// are one message, and a literal list here would be a second copy of the
// router's vocabulary free to drift from the card's.
func welcomeTextFor(choices []string) string {
	return "*switchboard* is connected. Mention me in a thread and I'll relay the turn " +
		"to the agent; every reply lands back in the same thread, and a thread is one " +
		"conversation. " + progressHint(choices)
}

// progressHint is the welcome's sentence about long-turn feedback, naming the
// command and the values the handler accepts. A handler that reports no values
// still gets the command named — the argument list is dropped, not the
// sentence, because an empty one ("progress <>") reads as a bug while a welcome
// that never says the word "progress" leaves the setting undiscoverable.
func progressHint(choices []string) string {
	list := strings.Join(nonBlank(choices), "|")
	if list == "" {
		return "Long turns report progress while they run. Set how much you see with `progress`."
	}
	return "Long turns report progress while they run. Set how much you see with " +
		"`progress <" + list + ">`."
}

// nonBlank drops the blanks a handler's choice list may carry and trims what it
// keeps, so neither an empty value nor a padded one can widen the rendered list
// to "off||stream" or "off| stream".
func nonBlank(vals []string) []string {
	kept := make([]string, 0, len(vals))
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			kept = append(kept, v)
		}
	}
	return kept
}

// ---------------------------------------------------------------------------
// Answer card
// ---------------------------------------------------------------------------

// Line classification for the answer renderer. Deliberately coarser than the
// Slack Block Kit port: Chat's MARKDOWN text syntax already renders lists,
// emphasis, links, and code inside a paragraph, so the only structure worth
// lifting out of the text is what a paragraph *cannot* express — a section
// header and a divider.
var (
	cardHeaderLineRE  = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$`)
	cardHRLineRE      = regexp.MustCompile(`^\s{0,3}([-*_])(?:\s*[-*_]){2,}\s*$`)
	cardFenceLineRE   = regexp.MustCompile("^\\s*(`{3,}|~{3,})")
	cardHeaderCleanRE = regexp.MustCompile("[*_~`]")
)

// answerCard renders a model turn as a structural card: an ATX header starts a
// new titled section, a horizontal rule becomes a divider, and everything
// between is a markdown paragraph that Chat renders itself. Returns nil when
// the turn is empty, has no structure worth a card, or exceeds the widget
// budget — the caller then sends plain text, which loses nothing.
func answerCard(markdown string) (card *chatv1.GoogleAppsCardV1Card) {
	if strings.TrimSpace(markdown) == "" {
		return nil
	}
	defer func() {
		if recover() != nil {
			card = nil // a rendering bug must never drop a reply
		}
	}()

	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	var (
		sections  []*chatv1.GoogleAppsCardV1Section
		cur       = &chatv1.GoogleAppsCardV1Section{}
		para      []string
		widgets   int
		structure bool // saw a header or a rule: the card earns its keep
	)

	// add appends a run's widgets — several when the run is over the per-widget
	// budget — and keeps the count they are checked against in step with them.
	add := func(sec *chatv1.GoogleAppsCardV1Section, md string) {
		w := markdownWidgets(md)
		sec.Widgets = append(sec.Widgets, w...)
		widgets += len(w)
	}
	flushPara := func() {
		if len(para) == 0 {
			return
		}
		text := strings.Join(para, "\n")
		para = para[:0]
		add(cur, text)
	}
	// A header whose section turns out to have no body of its own — "# Results"
	// immediately followed by "## Passing" — must not vanish with the empty
	// section. Chat has no nested sections, so it is carried onto the next
	// header instead of being dropped.
	var pending []string
	flushSection := func() {
		flushPara()
		switch {
		case len(cur.Widgets) > 0:
			sections = append(sections, cur)
			pending = nil
		case cur.Header != "":
			pending = append(pending, cur.Header)
		}
		cur = &chatv1.GoogleAppsCardV1Section{}
	}
	setHeader := func(title string) {
		cur.Header = clamp(strings.Join(append(pending, title), " — "), maxCardHeader)
		pending = nil
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		// A fenced block is copied through verbatim: its contents must not be
		// read as headers or rules, and Chat's markdown renders the fence.
		if cardFenceLineRE.MatchString(line) {
			marker := strings.TrimSpace(line)
			para = append(para, line)
			for i++; i < len(lines); i++ {
				para = append(para, lines[i])
				if strings.HasPrefix(strings.TrimSpace(lines[i]), marker[:3]) {
					break
				}
			}
			continue
		}
		if m := cardHeaderLineRE.FindStringSubmatch(line); m != nil {
			title := strings.TrimSpace(cardHeaderCleanRE.ReplaceAllString(m[2], ""))
			if title == "" {
				continue
			}
			structure = true
			// A top-level header opens a new section; a deeper one is a bold
			// lead-in inside the section it belongs to, which keeps a deeply
			// nested answer from fragmenting into a dozen title bars.
			if len(m[1]) <= 2 {
				flushSection()
				setHeader(title)
				continue
			}
			flushPara()
			add(cur, "**"+title+"**")
			continue
		}
		if cardHRLineRE.MatchString(line) {
			flushPara()
			// Only a rule that actually draws counts as structure; otherwise a
			// leading "---" would make a bare paragraph look card-worthy.
			if len(cur.Widgets) > 0 {
				cur.Widgets = append(cur.Widgets, dividerWidget())
				widgets++
				structure = true
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			flushPara()
			continue
		}
		para = append(para, line)
	}
	flushSection()
	// A header at the very end has no following section to be carried onto, so
	// it lands as a bold lead-in on the last one rather than being lost.
	if len(pending) > 0 && len(sections) > 0 {
		add(sections[len(sections)-1], "**"+strings.Join(pending, " — ")+"**")
	}

	if len(sections) == 0 || widgets == 0 || widgets > maxCardWidgets {
		return nil
	}
	// A single unheaded paragraph is a card that looks exactly like the text it
	// replaced, minus the ability to be chunked. Not worth it.
	if !structure {
		return nil
	}
	card = &chatv1.GoogleAppsCardV1Card{Sections: sections}
	// An answer too big for one Chat message goes out as text, which splits
	// into as many messages as it needs. Checked here rather than left to Chat
	// so an oversize answer costs no rejected write on the way to the fallback.
	if cardBytes(card) > maxCardBytes {
		return nil
	}
	return card
}

// cardBytes is the card's size on the wire — the JSON of the cardsV2 list, the
// same encoding the API call sends. Measured rather than estimated because
// escaping and the per-widget envelope are a large enough share of a card built
// from model output to make an estimate wrong in the direction that matters.
func cardBytes(card *chatv1.GoogleAppsCardV1Card) int {
	b, err := json.Marshal(singleCard(card))
	if err != nil {
		// Unmarshalable means the API call would fail too: treat it as
		// over-budget and let the caller send text.
		return maxCardBytes + 1
	}
	return len(b)
}

// withUsageFooter appends the turn's accounting to an answer card as a final
// iconed line, separated by a divider. It runs after answerCard's widget
// budget so the footer can never be what pushes a card over — and it appends
// to the card's last section rather than inserting anywhere else, because the
// footer has to stay last no matter what the overflow rules become (#32).
//
// card may be nil, meaning the answer is going out as plain text; the footer
// is deliberately dropped there rather than appended as a line, which would
// have to survive the 4096-char chunker.
func withUsageFooter(card *chatv1.GoogleAppsCardV1Card, u *chat.Usage) *chatv1.GoogleAppsCardV1Card {
	if card == nil || u == nil || len(card.Sections) == 0 {
		return card
	}
	w := iconTextWidget(iconUsage, u.Line())
	if w == nil {
		return card
	}
	last := card.Sections[len(card.Sections)-1]
	last.Widgets = append(last.Widgets, dividerWidget(), w)
	return card
}
