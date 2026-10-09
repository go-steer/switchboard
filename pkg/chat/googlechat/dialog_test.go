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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/switchboard/pkg/chat"
)

// dirHandler is a handler that is also an agent directory.
type dirHandler struct {
	fakeHandler
	agents []chat.AgentChoice
}

func (h *dirHandler) AgentChoices(string) []chat.AgentChoice { return h.agents }

func twoChoices() []chat.AgentChoice {
	return []chat.AgentChoice{
		{Name: "platform", Label: "Platform agent", Default: true},
		{Name: "general", Label: "General agent", Description: "Quick questions, no cluster access"},
	}
}

// slashDialog is a dialog-flagged /agent (command 100) with the given text.
func slashDialog(args string) string {
	return `{"chat": {"user": {"name": "users/5", "email": "ada@example.com"}, "space": {"name": "spaces/AAA"},
		"appCommandPayload": {"appCommandMetadata": {"appCommandId": 100, "appCommandType": "SLASH_COMMAND"},
		"dialogEventType": "REQUEST_DIALOG", "isDialogEvent": true,
		"space": {"name": "spaces/AAA"},
		"message": {"name": "spaces/AAA/messages/C1.C1", "argumentText": "` + args + `",
		"sender": {"name": "users/5"}, "thread": {"name": "spaces/AAA/threads/C1"}}}}}`
}

func serveDialog(t *testing.T, h chat.Handler, body string) map[string]any {
	t.Helper()
	a := newIngressAdapter(t, &fakeMessenger{})
	a.cmds = map[int64]string{100: "agent"}
	var turns turnGroup
	r := postEvent(body)
	r.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	a.eventHandler(context.Background(), &turns, h).ServeHTTP(rec, r)
	turns.Wait()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response %q: %v", rec.Body.String(), err)
	}
	return out
}

// A bare, dialog-flagged /agent opens the picker: the space's agents in a
// dropdown with the default selected, a prompt, and a Start that submits back.
func TestABareAgentCommandOpensThePicker(t *testing.T) {
	h := &dirHandler{agents: twoChoices()}
	out := serveDialog(t, h, slashDialog(""))
	raw, _ := json.Marshal(out)
	s := string(raw)
	for _, want := range []string{`"pushCard"`, `"type":"DROPDOWN"`, `"value":"platform"`, `"selected":true`,
		`"text":"Platform agent (default)"`, `"text":"General agent — Quick questions, no cluster access"`,
		`"name":"prompt"`, `"key":"switchboard_dialog"`, `"function":"` + testAudience + `"`} {
		if !strings.Contains(s, want) {
			t.Errorf("dialog response lacks %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"value":"general","selected":true`) {
		t.Error("a non-default agent is preselected")
	}
	if len(h.cmds) != 0 {
		t.Errorf("opening the picker ran a command: %+v", h.cmds)
	}
}

// Submitting runs the agent command with the chosen agent and the prompt as
// typed, from no thread (the agent's thread is a starter's), and closes with
// the command's acknowledgment.
func TestSubmittingThePickerStartsTheConversation(t *testing.T) {
	h := &dirHandler{agents: twoChoices()}
	h.ack = "Starting a thread with General agent…"
	body := `{"commonEventObject": {"parameters": {"switchboard_dialog": "agent"},
		"formInputs": {"agent": {"stringInputs": {"value": ["general"]}},
		               "prompt": {"stringInputs": {"value": ["what is a pod?\nbriefly"]}}}},
		"chat": {"user": {"name": "users/5", "email": "ada@example.com"}, "space": {"name": "spaces/AAA"},
		"buttonClickedPayload": {"isDialogEvent": true, "dialogEventType": "SUBMIT_DIALOG", "space": {"name": "spaces/AAA"}}}}`
	out := serveDialog(t, h, body)
	if len(h.cmds) != 1 {
		t.Fatalf("commands = %+v, want one", h.cmds)
	}
	c := h.cmds[0]
	if c.Name != "agent" || c.Args[0] != "general" || c.Text != "general what is a pod?\nbriefly" || c.Conversation != "" || c.Channel != "spaces/AAA" {
		t.Errorf("command = %+v", c)
	}
	raw, _ := json.Marshal(out)
	if s := string(raw); !strings.Contains(s, `"endNavigation":{"action":"CLOSE_DIALOG"}`) || !strings.Contains(s, "Starting a thread with General agent") {
		t.Errorf("response = %s, want the dialog closed with the acknowledgment", s)
	}
}

// Typed with arguments while flagged, the command runs as text — from its own
// thread, so a thread that has an agent keeps it — and the dialog response
// only closes.
func TestAFlaggedCommandWithArgumentsRunsAsText(t *testing.T) {
	h := &dirHandler{agents: twoChoices()}
	out := serveDialog(t, h, slashDialog("general hello"))
	if len(h.cmds) != 1 || h.cmds[0].Text != "general hello" || h.cmds[0].Conversation != "spaces/AAA:spaces/AAA/threads/C1" {
		t.Errorf("commands = %+v", h.cmds)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"CLOSE_DIALOG"`) || strings.Contains(string(raw), "pushCard") {
		t.Errorf("response = %s, want a close and no picker", raw)
	}
}

// With nothing to pick from (one agent, or a handler with no directory), the
// command runs as text and the dialog closes with what it said.
func TestNoPickerWithoutChoices(t *testing.T) {
	h := &fakeHandler{ack: "This gateway has one agent: just mention the app to talk to it."}
	out := serveDialog(t, h, slashDialog(""))
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "one agent") || strings.Contains(string(raw), "pushCard") {
		t.Errorf("response = %s", raw)
	}
}

// A dialog step is recognised only for the agent picker: another button's
// submit, or an unflagged command, keeps its own path.
func TestIsAgentDialog(t *testing.T) {
	for name, tc := range map[string]struct {
		in   inbound
		want bool
	}{
		"flagged command":    {inbound{kind: kindCommand, dialog: "REQUEST_DIALOG"}, true},
		"plain command":      {inbound{kind: kindCommand}, false},
		"picker submit":      {inbound{kind: kindButton, dialog: "SUBMIT_DIALOG", params: map[string]string{paramDialog: dialogAgent}}, true},
		"other dialog press": {inbound{kind: kindButton, dialog: "SUBMIT_DIALOG", params: map[string]string{"x": "y"}}, false},
		"plain press":        {inbound{kind: kindButton, params: map[string]string{paramDialog: dialogAgent}}, false},
	} {
		if got := isAgentDialog(tc.in); got != tc.want {
			t.Errorf("%s: isAgentDialog = %v, want %v", name, got, tc.want)
		}
	}
}
