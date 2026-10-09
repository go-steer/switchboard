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
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/go-steer/switchboard/pkg/chat"
)

// The agent picker modal (#140, phase 3c), Slack's counterpart of the Google
// Chat dialog. A bare `/switchboard agent` opens it with the slash command's
// trigger id: the channel's agents in a select (the default preselected, each
// described), an optional prompt, Start. The submit arrives as a
// view_submission; acknowledging it (handleInteractive does, empty) closes the
// modal, and the choice runs as the `agent` command — the same starter
// message and thread as the typed form. The channel rides in the view's
// private metadata, since a view belongs to no channel, and so does the slash
// command's response_url: the acknowledgment goes back through it, which —
// unlike a post — works where the app is not a member (caught in review).

const (
	agentModalID = "switchboard_agent"

	modalAgentBlock  = "agent"
	modalAgentAction = "agent_select"
	modalPromptBlock = "prompt"
	modalPromptInput = "prompt_input"

	defaultMarker = " (default)"

	openModalTimeout = 2 * time.Second
	maxSelectOptions = 100 // Slack's cap on a static_select; past it, the listing
)

// isBareAgent reports whether cmd lists the agents rather than starting one.
func isBareAgent(cmd chat.Command) bool {
	return (cmd.Name == "agent" || cmd.Name == "agents") && len(cmd.Args) == 0
}

// openAgentModal opens the picker for a bare `agent`, and reports whether it
// did. False — nothing to pick, no trigger, or Slack refused — leaves the
// command to answer as text.
func (a *Adapter) openAgentModal(ctx context.Context, h chat.Handler, sc slack.SlashCommand) bool {
	dir, ok := h.(chat.AgentDirectory)
	if !ok || sc.TriggerID == "" {
		return false
	}
	choices := dir.AgentChoices(sc.ChannelID)
	if len(choices) == 0 || len(choices) > maxSelectOptions {
		return false
	}
	// Bounded: this runs inside the slash command's 3s ack window, and a slow
	// open must leave time to answer as text instead.
	octx, cancel := context.WithTimeout(ctx, openModalTimeout)
	defer cancel()
	meta := modalMeta{Channel: sc.ChannelID, ResponseURL: sc.ResponseURL}
	if _, err := a.api.OpenViewContext(octx, sc.TriggerID, agentModal(choices, meta)); err != nil {
		// A timeout can still have opened it Slack-side, and then the person
		// sees both the picker and the listing: harmless, and rare.
		a.logf.Warnf("slack: open the agent picker: %v; answering as text", err)
		return false
	}
	return true
}

// modalMeta is what the picker carries in its private metadata.
type modalMeta struct {
	Channel     string `json:"c"`
	ResponseURL string `json:"r,omitempty"`
}

// agentModal is the picker view.
func agentModal(choices []chat.AgentChoice, meta modalMeta) slack.ModalViewRequest {
	raw, _ := json.Marshal(meta) // two strings: cannot fail
	opts := make([]*slack.OptionBlockObject, 0, len(choices))
	var initial *slack.OptionBlockObject
	for _, c := range choices {
		label := clampRunes(c.Label, 75)
		if c.Default {
			// Clamped first, so a long label cannot cut the marker off.
			label = clampRunes(c.Label, 75-len(defaultMarker)) + defaultMarker
		}
		o := slack.NewOptionBlockObject(c.Name, slack.NewTextBlockObject(slack.PlainTextType, label, false, false), nil)
		if c.Description != "" {
			o.Description = slack.NewTextBlockObject(slack.PlainTextType, clampRunes(c.Description, 75), false, false)
		}
		opts = append(opts, o)
		if c.Default {
			initial = o
		}
	}
	sel := slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, nil, modalAgentAction, opts...)
	if initial != nil {
		sel.InitialOption = initial
	}
	agentInput := slack.NewInputBlock(modalAgentBlock,
		slack.NewTextBlockObject(slack.PlainTextType, "Agent", false, false), nil, sel)

	prompt := slack.NewPlainTextInputBlockElement(
		slack.NewTextBlockObject(slack.PlainTextType, "What do you want to ask?", false, false), modalPromptInput)
	prompt.Multiline = true
	promptInput := slack.NewInputBlock(modalPromptBlock,
		slack.NewTextBlockObject(slack.PlainTextType, "Prompt", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Starts a new thread with the agent; reply there to continue.", false, false),
		prompt)
	promptInput.Optional = true

	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      agentModalID,
		PrivateMetadata: string(raw),
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Talk to an agent", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Start", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Blocks:          slack.Blocks{BlockSet: []slack.Block{agentInput, promptInput}},
	}
}

// agentCommandFromSubmission turns the picker's submit into the `agent`
// command it stands for, and the response_url to acknowledge it through. ok is
// false for any other view's submission.
func agentCommandFromSubmission(cb slack.InteractionCallback) (chat.Command, string, bool) {
	if cb.Type != slack.InteractionTypeViewSubmission || cb.View.CallbackID != agentModalID {
		return chat.Command{}, "", false
	}
	var meta modalMeta
	if err := json.Unmarshal([]byte(cb.View.PrivateMetadata), &meta); err != nil {
		return chat.Command{}, "", false
	}
	var name, prompt string
	if cb.View.State != nil {
		if v, ok := cb.View.State.Values[modalAgentBlock][modalAgentAction]; ok {
			name = strings.TrimSpace(v.SelectedOption.Value)
		}
		if v, ok := cb.View.State.Values[modalPromptBlock][modalPromptInput]; ok {
			prompt = strings.TrimSpace(v.Value)
		}
	}
	if name == "" || meta.Channel == "" {
		return chat.Command{}, "", false
	}
	text := name
	if prompt != "" {
		text += " " + prompt
	}
	cmd := chat.Command{Name: "agent", Channel: meta.Channel, Args: strings.Fields(text), Text: text}
	if cb.User.ID != "" {
		cmd.CallerMention = "<@" + cb.User.ID + ">"
	}
	return cmd, meta.ResponseURL, true
}

// runModalCommand runs the picker's command and shows its acknowledgment to
// the submitter alone, as the slash command's ephemeral ack would have:
// through the command's response_url, or an ephemeral post without one.
func (a *Adapter) runModalCommand(ctx context.Context, h chat.Handler, cmd chat.Command, userID, responseURL string) {
	cmd.Caller = a.resolveCaller(ctx, userID)
	ack, err := h.HandleCommand(ctx, cmd)
	if err != nil {
		a.logf.Errorf("slack: agent picker: %v", err)
		ack = "Sorry, that command failed."
	}
	if ack == "" {
		return
	}
	if responseURL != "" {
		msg := &slack.WebhookMessage{ResponseType: slack.ResponseTypeEphemeral, Text: toMrkdwn(ack)}
		if err := slack.PostWebhookContext(ctx, responseURL, msg); err != nil {
			a.logf.Warnf("slack: agent picker ack in %s: %v", cmd.Channel, err)
		}
		return
	}
	if userID == "" {
		return
	}
	if _, err := a.api.PostEphemeralContext(ctx, cmd.Channel, userID, slack.MsgOptionText(toMrkdwn(ack), false)); err != nil {
		a.logf.Warnf("slack: agent picker ack in %s: %v", cmd.Channel, err)
	}
}
