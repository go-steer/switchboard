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
	"strings"

	"github.com/slack-go/slack"

	"github.com/go-steer/switchboard/pkg/chat"
)

// Slack's limits on the pieces of an actions block. Exceeding any of them
// fails the whole post with invalid_blocks, taking the question with it — so
// every one of these is enforced on the way out rather than discovered.
const (
	maxActionElements = 25
	maxButtonText     = 75
	maxActionID       = 255
	maxActionValue    = 2000
	maxConfirmTitle   = 100
	maxConfirmText    = 300
	maxConfirmButton  = 30
)

// actionPrefix marks a block action as switchboard's answer to a Decision,
// and namespaces it against any other interactive element this app grows
// later. The chosen option's value follows it, because Slack guarantees to
// return action_id on the press while the element's value is easier to lose
// in a payload shape change.
const actionPrefix = "sbdecide:"

// decisionBlocks renders a Decision as a question with buttons under it:
// a section carrying the body, then one actions block of answers.
//
// Returns nil when there is nothing to decide, which sends the reply down the
// ordinary text path — a question whose answers did not survive rendering is
// still a question worth posting, and posting it as prose is how somebody
// finds out the agent is stuck.
func decisionBlocks(text string, d *chat.Decision, mrkdwnFn func(string) string) []map[string]any {
	if !d.Deciding() {
		return nil
	}
	elements := buttonElements(d)
	if len(elements) < 2 {
		// One button is not a choice. Rather than offer a press that cannot
		// be weighed against anything, fall back to prose that lists what the
		// answers were.
		return nil
	}
	blocks := []map[string]any{
		{"type": "actions", "elements": elements},
	}
	if body := strings.TrimSpace(mrkdwnFn(text)); body != "" {
		blocks = append([]map[string]any{sectionBlock(body)}, blocks...)
	}
	return blocks
}

// buttonElements builds one button per option, dropping any that cannot be
// represented rather than failing the post. An option whose value does not fit
// an action_id is unanswerable — the press would come back naming something
// else — so it is left out instead of rendered as a button that lies.
func buttonElements(d *chat.Decision) []any {
	var elements []any
	for _, o := range d.Options {
		if len(elements) == maxActionElements {
			break
		}
		id := actionPrefix + o.Value
		if o.Value == "" || len(id) > maxActionID || len(d.ID) > maxActionValue {
			continue
		}
		label := strings.TrimSpace(o.Label)
		if label == "" {
			label = o.Value
		}
		b := map[string]any{
			"type":      "button",
			"action_id": id,
			"value":     d.ID,
			// plain_text, not mrkdwn: Slack renders no markup on a button, and
			// asking it to would show the asterisks.
			"text": map[string]any{"type": "plain_text", "text": clampRunes(label, maxButtonText)},
		}
		if o.Broad {
			// Slack's one affordance for "are you sure" — spent on the answers
			// that outlive the question, which is what Broad is for. It is also
			// the only place the full label can be read without a button's
			// width budget, so the dialog repeats it rather than paraphrasing.
			b["confirm"] = confirmDialog(label)
			b["style"] = "danger"
		}
		elements = append(elements, b)
	}
	return elements
}

// confirmDialog is the second look a Broad answer gets. Deliberately not
// alarming about it: the answer is one somebody meant to give, and a dialog
// that reads as a warning teaches people to dismiss dialogs.
func confirmDialog(label string) map[string]any {
	return map[string]any{
		"title":   map[string]any{"type": "plain_text", "text": clampRunes("Confirm", maxConfirmTitle)},
		"text":    map[string]any{"type": "mrkdwn", "text": clampRunes(label+" — this outlasts the request being made.", maxConfirmText)},
		"confirm": map[string]any{"type": "plain_text", "text": clampRunes("Yes, allow", maxConfirmButton)},
		"deny":    map[string]any{"type": "plain_text", "text": clampRunes("Back", maxConfirmButton)},
	}
}

// clampRunes bounds a string to n runes, never splitting one. Slack counts
// characters, and a label cut mid-rune is rejected as invalid UTF-8 rather
// than merely looking wrong.
func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// pressFrom reads a Slack block-actions callback as a Press, reporting whether
// it is one of switchboard's decisions at all. Everything about the answer
// comes off the callback itself — including who pressed it, which is the one
// field that must never be inherited from the message being answered.
func pressFrom(cb slack.InteractionCallback) (chat.Press, bool) {
	if cb.Type != slack.InteractionTypeBlockActions {
		return chat.Press{}, false
	}
	var action *slack.BlockAction
	for _, a := range cb.ActionCallback.BlockActions {
		if a != nil && strings.HasPrefix(a.ActionID, actionPrefix) {
			action = a
			break
		}
	}
	if action == nil {
		return chat.Press{}, false
	}
	channel := cb.Channel.ID
	if channel == "" {
		channel = cb.Container.ChannelID
	}
	if channel == "" {
		return chat.Press{}, false
	}
	// The message the buttons are on is the root of the conversation the
	// question was asked in, exactly as an inbound mention would key it.
	msgTS := cb.Container.MessageTs
	conv := conversationKey(channel, threadRoot(cb.Container.ThreadTs, msgTS))
	return chat.Press{
		Conversation: conv,
		Channel:      channel,
		DecisionID:   action.Value,
		Option:       strings.TrimPrefix(action.ActionID, actionPrefix),
		Message:      chat.MessageRef{Conversation: conv, ID: msgTS},
	}, true
}
