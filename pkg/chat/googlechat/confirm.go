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
	"context"
	"encoding/json"
	"strings"

	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

// A Broad answer — one that outlives the request being made — takes two
// presses on Chat, as it does on Slack (#92). chat.DecisionOption.Broad is a
// request for friction, and Slack spends its native confirm dialog on it;
// Chat has none, so the first press swaps the question's row of answers for a
// line naming what is about to be granted, with Yes, allow (the real press)
// and Back (the row restored).
//
// All of it happens here, with no router involvement, and from the click
// alone: the click carries its hosting card whole (inbound.hosting), so the
// confirmation is built from the question it replaces, and Back carries the
// answers it restores in its own parameters. Nothing is remembered between
// the two presses, so a restart in between costs nothing. Approval checks
// stay where they are: only Yes reaches the router, which refuses it exactly
// as it would the one-press answer.

// confirmSuffix ends the line a confirmation adds to the question. It is also
// how Back recognizes that line, to take it out again.
const confirmSuffix = " — this outlasts the request being made."

// wireOption is a decision option as Back carries it.
type wireOption struct {
	V string `json:"v"`
	L string `json:"l"`
	B bool   `json:"b,omitempty"`
}

// runStage answers a click that does not decide yet: the first press on a
// Broad answer, or Back.
func (a *Adapter) runStage(ctx context.Context, in inbound, conv, decisionID string) {
	stage := in.params[paramStage]
	// A card that no longer offers this decision has been settled — the
	// router's record replaced the buttons — and a stage click on it is stale:
	// a client still showing the old card. Rewriting would put live buttons
	// back over the record of who decided, and pressing them could only find
	// nothing pending. Left alone.
	if in.hosting != nil && !offersDecision(in.hosting, decisionID) {
		a.logf.Infof("googlechat: %s on %s: the question is no longer open", stage, conv)
		return
	}
	var card *chatv1.GoogleAppsCardV1Card
	var text string
	switch stage {
	case stageConfirm:
		if in.hosting != nil && hasBack(in.hosting, decisionID) {
			// Already a confirmation — a double tap, or two people at once.
			// Built again from itself it would read its own Yes as the only
			// answer and lose the rest; it is already right as it is.
			return
		}
		card, text = confirmCard(in.hosting, decisionID, in.params[paramOption], a.actionURL())
	case stageBack:
		card, text = restoredCard(in.hosting, decisionID, in.params[paramOptions], a.actionURL())
	default:
		return // not one of ours
	}
	if card == nil {
		a.logf.Warnf("googlechat: %s on %s: nothing to render", stage, conv)
		return
	}
	// Without the hosting card the question cannot be rebuilt, so the
	// confirmation goes beside it as a reply rather than over it: rewriting
	// would leave only the Broad grant on the question.
	if in.messageName == "" || in.hosting == nil {
		if _, err := a.post(ctx, conv, card, text); err != nil {
			a.logf.Errorf("googlechat: %s %s: %v", in.params[paramStage], conv, err)
		}
		return
	}
	if err := a.rewrite(ctx, in.messageName, card, text); err != nil {
		a.logf.Errorf("googlechat: %s %s: %v", in.params[paramStage], in.messageName, err)
	}
}

// confirmCard is the question with its row of answers replaced by a
// confirmation of the one pressed. Without the hosting card — a dialect or
// path that did not carry it — the question cannot be restored, so it offers
// Yes alone: still a second, deliberate press naming what it grants.
func confirmCard(hosting *chatv1.GoogleAppsCardV1Card, id, option, actionURL string) (*chatv1.GoogleAppsCardV1Card, string) {
	opts := hostedOptions(hosting, id)
	label := option
	for _, o := range opts {
		if o.Value == option && strings.TrimSpace(o.Label) != "" {
			label = o.Label
		}
	}
	// Escaped HTML, not markdown: a label can carry agent-supplied text (the
	// verb in "Allow every <verb> this session"), and on the line confirming a
	// grant it must read as written, not render.
	line := &chatv1.GoogleAppsCardV1Widget{TextParagraph: &chatv1.GoogleAppsCardV1TextParagraph{
		Text: "<b>" + htmlEscaper.Replace(clampRunes(label, maxButtonText*4)) + "</b>" + htmlEscaper.Replace(confirmSuffix),
	}}
	yes := actionButton("Yes, allow", actionURL,
		&chatv1.GoogleAppsCardV1ActionParameter{Key: paramDecision, Value: id},
		&chatv1.GoogleAppsCardV1ActionParameter{Key: paramOption, Value: option},
	)
	var back *chatv1.GoogleAppsCardV1Button
	if len(opts) > 0 {
		wire := make([]wireOption, 0, len(opts))
		for _, o := range opts {
			wire = append(wire, wireOption{V: o.Value, L: o.Label, B: o.Broad})
		}
		if b, err := json.Marshal(wire); err == nil {
			back = actionButton("Back", actionURL,
				&chatv1.GoogleAppsCardV1ActionParameter{Key: paramDecision, Value: id},
				&chatv1.GoogleAppsCardV1ActionParameter{Key: paramStage, Value: stageBack},
				&chatv1.GoogleAppsCardV1ActionParameter{Key: paramOptions, Value: string(b)},
			)
		}
	}
	widgets := append(hostedBody(hosting), line, buttonRow(1, yes, back))
	return widgetCard(widgets...), label + confirmSuffix
}

// restoredCard is Back: the question as it was before the confirmation, its
// answers rebuilt from what Back carried.
func restoredCard(hosting *chatv1.GoogleAppsCardV1Card, id, encoded, actionURL string) (*chatv1.GoogleAppsCardV1Card, string) {
	var wire []wireOption
	if err := json.Unmarshal([]byte(encoded), &wire); err != nil || len(wire) == 0 {
		return nil, ""
	}
	opts := make([]chat.DecisionOption, 0, len(wire))
	for _, w := range wire {
		opts = append(opts, chat.DecisionOption{Value: w.V, Label: w.L, Broad: w.B})
	}
	widgets := append(hostedBody(hosting), decisionRow(id, opts, actionURL))
	return widgetCard(widgets...), chat.DecisionText(&chat.Decision{ID: id, Options: opts})
}

// buttonsFor visits every button on card that acts on this decision, with its
// parameters.
func buttonsFor(card *chatv1.GoogleAppsCardV1Card, id string, visit func(b *chatv1.GoogleAppsCardV1Button, p map[string]string)) {
	if card == nil {
		return
	}
	for _, s := range card.Sections {
		for _, w := range s.Widgets {
			if w == nil || w.ButtonList == nil {
				continue
			}
			for _, b := range w.ButtonList.Buttons {
				if b == nil || b.OnClick == nil || b.OnClick.Action == nil {
					continue
				}
				p := map[string]string{}
				for _, kv := range b.OnClick.Action.Parameters {
					if kv != nil {
						p[kv.Key] = kv.Value
					}
				}
				if p[paramDecision] == id {
					visit(b, p)
				}
			}
		}
	}
}

// offersDecision reports whether card still has a button acting on id.
func offersDecision(card *chatv1.GoogleAppsCardV1Card, id string) bool {
	found := false
	buttonsFor(card, id, func(*chatv1.GoogleAppsCardV1Button, map[string]string) { found = true })
	return found
}

// hasBack reports whether card is already this decision's confirmation.
func hasBack(card *chatv1.GoogleAppsCardV1Card, id string) bool {
	found := false
	buttonsFor(card, id, func(_ *chatv1.GoogleAppsCardV1Button, p map[string]string) {
		found = found || p[paramStage] == stageBack
	})
	return found
}

// hostedBody is the hosting card's question: every widget but its buttons and
// any confirmation line a previous press added.
func hostedBody(card *chatv1.GoogleAppsCardV1Card) []*chatv1.GoogleAppsCardV1Widget {
	if card == nil {
		return nil
	}
	var out []*chatv1.GoogleAppsCardV1Widget
	for _, s := range card.Sections {
		for _, w := range s.Widgets {
			if w == nil || w.ButtonList != nil {
				continue
			}
			if w.TextParagraph != nil && strings.HasSuffix(strings.TrimSpace(w.TextParagraph.Text), strings.TrimSpace(confirmSuffix)) {
				continue
			}
			out = append(out, w)
		}
	}
	return out
}

// hostedOptions reads a question's answers back off its own buttons: each
// button answering this decision, its label, and whether it asked for
// confirmation — which is what Broad renders as.
func hostedOptions(card *chatv1.GoogleAppsCardV1Card, id string) []chat.DecisionOption {
	var out []chat.DecisionOption
	buttonsFor(card, id, func(b *chatv1.GoogleAppsCardV1Button, p map[string]string) {
		if p[paramOption] == "" {
			return
		}
		out = append(out, chat.DecisionOption{
			Value: p[paramOption],
			Label: b.Text,
			Broad: p[paramStage] == stageConfirm,
		})
	})
	return out
}
