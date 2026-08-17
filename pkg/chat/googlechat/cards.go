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
//     acknowledgment with its choice buttons. These say something *about* the
//     gateway rather than carrying model output, and a card is what makes them
//     legible at a glance: an icon distinguishes "still working" from "that
//     failed", and buttons turn "type `progress stream`" into one click.
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
// Buttons carry their meaning in action parameters, not in the action function
// name: an add-on that extends Chat never populates
// commonEventObject.invokedFunction, so a parameter is the only place a click's
// identity can survive the round trip. The same parameters arrive in the legacy
// dialect's common.parameters, so one encoding serves both.
package googlechat

import (
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
	maxWidgetText = 3500
	// maxCardHeader is the budget for a section header, which is one line in
	// every client — a paragraph's worth of text there just gets clipped.
	maxCardHeader = 200
	// cardID names the single card on a message. Chat requires an id per card;
	// it is only used to address the card within the message.
	cardID = "switchboard"
)

// Action parameter keys. A click reaches HandleCommand as though the invoker
// had typed the command, so the parameters are exactly a command's verb and its
// single argument.
const (
	paramCommand = "switchboard_command"
	paramArg     = "switchboard_arg"
)

// Material icons for the gateway cards. Named rather than inlined so the set
// stays visible in one place.
const (
	iconProgress = "hourglass_top"
	iconActivity = "settings"
	iconNotice   = "error"
	iconAck      = "tune"
	iconWelcome  = "waving_hand"
)

// CardMode selects how much of the adapter's output is rendered as cards. It is
// a mode rather than a bool because the two card families carry very different
// risk: gateway cards are small, fixed shapes the gateway itself authors, while
// an answer card is a render of arbitrary model output. An operator who wants
// the first without the second (the default) can have it.
type CardMode string

const (
	// CardsOff sends everything as plain Chat text.
	CardsOff CardMode = "off"
	// CardsStatus renders the gateway's own messages — progress, activity,
	// notices, command acknowledgments, the welcome — as cards, and leaves
	// model answers as text. The default.
	CardsStatus CardMode = "status"
	// CardsRich additionally renders model answers as structural cards.
	CardsRich CardMode = "rich"
)

// ParseCardMode validates a --googlechat-cards value.
func ParseCardMode(s string) (CardMode, bool) {
	switch CardMode(strings.ToLower(strings.TrimSpace(s))) {
	case CardsOff:
		return CardsOff, true
	case CardsStatus, "":
		return CardsStatus, true
	case CardsRich:
		return CardsRich, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Widget helpers
// ---------------------------------------------------------------------------

// markdownWidget is a paragraph rendered with Chat's markdown text syntax, so
// a model turn's own markup — lists, emphasis, links, fenced code — is passed
// through for Chat to render rather than translated first. Chat-only, and used
// only by the opt-in answer card. Returns nil when the text reduces to nothing
// (Chat rejects an empty widget).
func markdownWidget(md string) *chatv1.GoogleAppsCardV1Widget {
	md = strings.TrimSpace(md)
	if md == "" {
		return nil
	}
	return &chatv1.GoogleAppsCardV1Widget{
		TextParagraph: &chatv1.GoogleAppsCardV1TextParagraph{
			Text:       clamp(md, maxWidgetText),
			TextSyntax: "MARKDOWN",
		},
	}
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

// commandButton builds a button that re-invokes a gateway command with one
// argument. Chat delivers the click asynchronously over Pub/Sub, so the action
// asks for no synchronous response — the adapter answers by patching the card.
func commandButton(label, command, arg string) *chatv1.GoogleAppsCardV1Button {
	return &chatv1.GoogleAppsCardV1Button{
		Text: label,
		OnClick: &chatv1.GoogleAppsCardV1OnClick{
			Action: &chatv1.GoogleAppsCardV1Action{
				// Chat requires a function name even though an add-on never
				// reports it back; the parameters carry the real identity.
				Function: "switchboard",
				Parameters: []*chatv1.GoogleAppsCardV1ActionParameter{
					{Key: paramCommand, Value: command},
					{Key: paramArg, Value: arg},
				},
			},
		},
	}
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
		return widgetCard(iconTextWidget(iconActivity, text))
	case chat.KindNotice:
		return widgetCard(iconTextWidget(iconNotice, text))
	case chat.KindAck:
		return widgetCard(iconTextWidget(iconAck, text))
	}
	return nil
}

// ackCard renders a command acknowledgment with a button per accepted value, so
// the invoker can change a setting by clicking rather than by retyping the
// command. choices may be empty, in which case this is just the ack card.
func ackCard(text, command string, choices []string) *chatv1.GoogleAppsCardV1Card {
	card := widgetCard(iconTextWidget(iconAck, text))
	if card == nil || command == "" || len(choices) == 0 {
		return card
	}
	buttons := make([]*chatv1.GoogleAppsCardV1Button, 0, len(choices))
	for _, c := range choices {
		if c == "" {
			continue
		}
		buttons = append(buttons, commandButton(c, command, c))
	}
	if len(buttons) == 0 {
		return card
	}
	sec := card.Sections[0]
	sec.Widgets = append(sec.Widgets, &chatv1.GoogleAppsCardV1Widget{
		ButtonList: &chatv1.GoogleAppsCardV1ButtonList{Buttons: buttons},
	})
	return card
}

// welcomeCard greets a space the app was just added to. It is the one card with
// a header: this is the only message that has to introduce the app itself.
func welcomeCard(choices []string) *chatv1.GoogleAppsCardV1Card {
	card := widgetCard(
		htmlWidget("Mention me in a thread and I'll relay the turn to the agent. "+
			"Every reply lands back in the same thread, and a thread is one conversation."),
		iconTextWidget(iconWelcome, "Long turns report progress while they run — pick how much you want to see."),
	)
	if card == nil {
		return nil
	}
	card.Header = &chatv1.GoogleAppsCardV1CardHeader{
		Title:    "switchboard",
		Subtitle: "chat gateway for core-agent",
	}
	if len(choices) > 0 {
		buttons := make([]*chatv1.GoogleAppsCardV1Button, 0, len(choices))
		for _, c := range choices {
			if c != "" {
				buttons = append(buttons, commandButton(c, "progress", c))
			}
		}
		if len(buttons) > 0 {
			sec := card.Sections[0]
			sec.Widgets = append(sec.Widgets, &chatv1.GoogleAppsCardV1Widget{
				ButtonList: &chatv1.GoogleAppsCardV1ButtonList{Buttons: buttons},
			})
		}
	}
	return card
}

// welcomeText is the plain-text form of the welcome, used when cards are off
// and as the card's fallback.
const welcomeText = "*switchboard* is connected. Mention me in a thread and I'll relay the turn " +
	"to the agent; every reply lands back in the same thread, and a thread is one conversation. " +
	"Set how much progress you see with `progress <off|indicator|status|stream>`."

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

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		text := strings.Join(para, "\n")
		para = para[:0]
		if w := markdownWidget(text); w != nil {
			cur.Widgets = append(cur.Widgets, w)
			widgets++
		}
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
			if w := markdownWidget("**" + title + "**"); w != nil {
				cur.Widgets = append(cur.Widgets, w)
				widgets++
			}
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
		last := sections[len(sections)-1]
		if w := markdownWidget("**" + strings.Join(pending, " — ") + "**"); w != nil {
			last.Widgets = append(last.Widgets, w)
			widgets++
		}
	}

	if len(sections) == 0 || widgets == 0 || widgets > maxCardWidgets {
		return nil
	}
	// A single unheaded paragraph is a card that looks exactly like the text it
	// replaced, minus the ability to be chunked. Not worth it.
	if !structure {
		return nil
	}
	return &chatv1.GoogleAppsCardV1Card{Sections: sections}
}
