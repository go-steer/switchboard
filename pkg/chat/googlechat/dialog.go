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
	"net/http"
	"strings"

	chatv1 "google.golang.org/api/chat/v1"

	"github.com/go-steer/switchboard/pkg/chat"
)

// The agent picker dialog (#140, phase 3b).
//
// A slash command mapped to `agent` and marked "Opens a dialog" in the Chat
// API console arrives with dialogEventType REQUEST_DIALOG, and must be
// answered with a dialog (RenderActions), not a message — every invocation,
// arguments or not (measured in multi-agent-chat). Bare, it opens the picker:
// the agents the space offers in a dropdown, the default selected, an optional
// prompt, and Start. Submitting arrives as a dialog button
// (SUBMIT_DIALOG), runs the `agent` command with what was chosen, and closes
// the dialog with the command's acknowledgment as a notification. Typed with
// arguments, the command runs as typed and the dialog response only closes.
//
// The command flag is not the only way in, and on an add-on Chat app not a
// reliable one: with "Opens a dialog" ticked, the slash command still arrived
// as a plain command, no dialogEventType on it (measured on the GKE
// deployment). So a bare `agent`'s listing card carries a "Start a
// conversation…" button whose action has interaction OPEN_DIALOG — the
// documented way for a message button to open a dialog — and its click
// (params switchboard_dialog=open) is answered with the picker.
//
// HTTP ingress only: a dialog is the synchronous response to the request
// that asked for it, which Pub/Sub delivery has no way to give. Without the
// console flag none of this is reached, and `agent` works as text. Any command
// marked "Opens a dialog" takes this path — Chat expects a dialog response for
// it — and one that is not `agent` simply runs and closes with its
// acknowledgment.

const (
	// paramDialog marks a dialog's Start button, so its submit is recognised.
	paramDialog = "switchboard_dialog"
	dialogAgent = "agent"
	dialogOpen  = "open" // the listing card's button that opens the picker

	// The dialog's input names, read back from formInputs on submit.
	inputAgent  = "agent"
	inputPrompt = "prompt"
)

// isAgentDialog reports whether in is a step of the agent dialog: a command
// asking for one, or the picker's own submit.
func isAgentDialog(in inbound) bool {
	switch {
	case in.kind == kindCommand && in.dialog == "REQUEST_DIALOG":
		return true
	case in.kind == kindButton && in.dialog == "SUBMIT_DIALOG" && in.params[paramDialog] == dialogAgent:
		return true
	case in.kind == kindButton && in.params[paramDialog] == dialogOpen:
		// The opener's click. Not gated on dialogEventType: its answer must be
		// a dialog whatever the payload says, as the button asked for one.
		return true
	}
	return false
}

// isBareAgent reports whether cmd lists the agents rather than starting one.
func isBareAgent(cmd chat.Command) bool {
	return (cmd.Name == "agent" || cmd.Name == "agents") && len(cmd.Args) == 0
}

// agentListingCard is a bare `agent`'s reply: the listing as a MARKDOWN
// paragraph — the ack card's icon line renders only an HTML subset, and the
// listing's bold came out as literal asterisks (reported from the GKE
// deployment) — and, when there is a choice to make and an endpoint to open
// it from, the button that opens the picker.
func (a *Adapter) agentListingCard(ack string, h chat.Handler, space string) *chatv1.GoogleAppsCardV1Card {
	widgets := markdownWidgets(ack)
	if dir, ok := h.(chat.AgentDirectory); ok && len(dir.AgentChoices(space)) > 0 {
		if open := actionButton("Start a conversation…", a.actionURL(),
			&chatv1.GoogleAppsCardV1ActionParameter{Key: paramDialog, Value: dialogOpen}); open != nil {
			open.OnClick.Action.Interaction = "OPEN_DIALOG"
			widgets = append(widgets, buttonRow(1, open))
		}
	}
	return widgetCard(widgets...)
}

// answerDialog answers a step of the agent dialog with the RenderActions it
// needs: the picker, or the close.
func (a *Adapter) answerDialog(w http.ResponseWriter, runCtx context.Context, h chat.Handler, in inbound) {
	in.caller = a.callerOf(in)
	var resp any
	switch {
	case in.kind == kindCommand:
		resp = a.dialogForCommand(runCtx, h, in)
	case in.params[paramDialog] == dialogOpen:
		resp = a.openPicker(h, in.space)
	default:
		resp = a.submitDialog(runCtx, h, in)
	}
	b, err := json.Marshal(resp)
	if err != nil {
		a.logf.Errorf("googlechat: ingress: encode dialog response: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(b); err != nil {
		a.logf.Warnf("googlechat: ingress: write dialog response: %v", err)
	}
}

// openPicker answers the listing card's button with the picker, or closes
// with a reason when there is nothing to pick.
func (a *Adapter) openPicker(h chat.Handler, space string) any {
	if dir, ok := h.(chat.AgentDirectory); ok {
		if choices := dir.AgentChoices(space); len(choices) > 0 {
			if card := agentDialogCard(choices, a.actionURL()); card != nil {
				return openDialog(card)
			}
		}
	}
	return closeDialog("No agents are available here.")
}

// dialogForCommand opens the picker for a bare command, or runs a command
// typed with arguments and closes.
func (a *Adapter) dialogForCommand(ctx context.Context, h chat.Handler, in inbound) any {
	cmd := a.commandOf(in)
	if isBareAgent(cmd) {
		if dir, ok := h.(chat.AgentDirectory); ok {
			if choices := dir.AgentChoices(in.space); len(choices) > 0 {
				if card := agentDialogCard(choices, a.actionURL()); card != nil {
					return openDialog(card)
				}
			}
		}
	}
	// Arguments, or nothing to pick from: the command as text, with its
	// acknowledgment as the closing notification.
	cmd.Conversation = conversationKey(in.space, in.thread)
	return closeDialog(a.runForAck(ctx, h, cmd))
}

// submitDialog runs the agent the picker chose, with its prompt, and closes.
func (a *Adapter) submitDialog(ctx context.Context, h chat.Handler, in inbound) any {
	name := strings.TrimSpace(in.form[inputAgent])
	if name == "" {
		return closeDialog("No agent was chosen.")
	}
	prompt := strings.TrimSpace(in.form[inputPrompt])
	text := name
	if prompt != "" {
		text += " " + prompt
	}
	// No Conversation: a dialog belongs to no thread, so the agent's thread
	// is a starter message's, as for every new conversation.
	cmd := chat.Command{Name: "agent", Channel: in.space, Caller: in.caller,
		Args: strings.Fields(text), Text: text}
	return closeDialog(a.runForAck(ctx, h, cmd))
}

// runForAck runs a command and returns its acknowledgment, or a stand-in.
func (a *Adapter) runForAck(ctx context.Context, h chat.Handler, cmd chat.Command) string {
	ack, err := h.HandleCommand(ctx, cmd)
	if err != nil {
		a.logf.Errorf("googlechat: dialog command %q: %v", cmd.Name, err)
		return "That didn't work. Check the logs or contact an admin."
	}
	if ack == "" {
		ack = "Done."
	}
	return ack
}

// agentDialogCard is the picker: a dropdown of agents (the default selected,
// each described if it has a description), a prompt, and Start.
func agentDialogCard(choices []chat.AgentChoice, actionURL string) *chatv1.GoogleAppsCardV1Card {
	start := actionButton("Start", actionURL,
		&chatv1.GoogleAppsCardV1ActionParameter{Key: paramDialog, Value: dialogAgent})
	if start == nil {
		return nil // no endpoint URL to submit to
	}
	items := make([]*chatv1.GoogleAppsCardV1SelectionItem, 0, len(choices))
	for _, c := range choices {
		label := c.Label
		if c.Default {
			label += " (default)"
		}
		// The description goes in the label: SelectionItem.bottomText is
		// for multiselect menus, and a dropdown would drop it.
		if c.Description != "" {
			label += " — " + c.Description
		}
		items = append(items, &chatv1.GoogleAppsCardV1SelectionItem{
			Text:     clampRunes(label, 120),
			Value:    c.Name,
			Selected: c.Default,
			// Selected false is meaningful only beside one that is true;
			// sent either way so the default is the one marked.
			ForceSendFields: []string{"Selected"},
		})
	}
	return &chatv1.GoogleAppsCardV1Card{
		Header: &chatv1.GoogleAppsCardV1CardHeader{Title: "Talk to an agent"},
		Sections: []*chatv1.GoogleAppsCardV1Section{{Widgets: []*chatv1.GoogleAppsCardV1Widget{
			{SelectionInput: &chatv1.GoogleAppsCardV1SelectionInput{
				Name: inputAgent, Label: "Agent", Type: "DROPDOWN", Items: items,
			}},
			{TextInput: &chatv1.GoogleAppsCardV1TextInput{
				Name: inputPrompt, Label: "Prompt (optional)", Type: "MULTIPLE_LINE",
				HintText: "Starts a new thread with the agent; reply there to continue.",
			}},
			{ButtonList: &chatv1.GoogleAppsCardV1ButtonList{Buttons: []*chatv1.GoogleAppsCardV1Button{start}}},
		}}},
	}
}

// The add-on RenderActions shapes a dialog is opened and closed with.
type renderActions struct {
	Action renderAction `json:"action"`
}

type renderAction struct {
	Navigations  []navigation  `json:"navigations"`
	Notification *notification `json:"notification,omitempty"`
}

type navigation struct {
	PushCard      *chatv1.GoogleAppsCardV1Card `json:"pushCard,omitempty"`
	EndNavigation *endNavigation               `json:"endNavigation,omitempty"`
}

type endNavigation struct {
	Action string `json:"action"`
}

type notification struct {
	Text string `json:"text"`
}

func openDialog(card *chatv1.GoogleAppsCardV1Card) renderActions {
	return renderActions{Action: renderAction{Navigations: []navigation{{PushCard: card}}}}
}

func closeDialog(text string) renderActions {
	ra := renderActions{Action: renderAction{Navigations: []navigation{{EndNavigation: &endNavigation{Action: "CLOSE_DIALOG"}}}}}
	if text = strings.TrimSpace(text); text != "" {
		ra.Action.Notification = &notification{Text: clampRunes(text, 500)}
	}
	return ra
}
