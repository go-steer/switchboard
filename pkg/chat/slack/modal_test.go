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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/go-steer/switchboard/pkg/chat"
)

// agentRouter is a Handler with an agent directory that records commands.
type agentRouter struct {
	mu       sync.Mutex
	choices  []chat.AgentChoice
	commands []chat.Command
	ack      string
}

func (h *agentRouter) Handle(context.Context, chat.Message) error    { return nil }
func (h *agentRouter) HandlePress(context.Context, chat.Press) error { return nil }
func (h *agentRouter) AgentChoices(string) []chat.AgentChoice        { return h.choices }
func (h *agentRouter) HandleCommand(_ context.Context, c chat.Command) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commands = append(h.commands, c)
	return h.ack, nil
}

func (h *agentRouter) got() []chat.Command {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]chat.Command(nil), h.commands...)
}

var twoAgents = []chat.AgentChoice{
	{Name: "platform", Label: "Platform agent", Description: "Runs the cluster", Default: true},
	{Name: "general", Label: "General", Description: "Anything else"},
}

// slackAPI fakes the Web API calls the picker makes, recording each body.
type slackAPI struct {
	mu       sync.Mutex
	views    []string
	ephemera []map[string]string
	openFail bool
	lookups  int
}

func (s *slackAPI) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/views.open", func(w http.ResponseWriter, r *http.Request) {
		var body struct { // views.open posts JSON, not a form
			View json.RawMessage `json:"view"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.views = append(s.views, string(body.View))
		fail := s.openFail
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail {
			_, _ = w.Write([]byte(`{"ok":false,"error":"expired_trigger_id"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"view":{"id":"V1"}}`))
	})
	mux.HandleFunc("/chat.postEphemeral", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.ephemera = append(s.ephemera, map[string]string{
			"channel": r.FormValue("channel"), "user": r.FormValue("user"), "text": r.FormValue("text"),
		})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"message_ts":"1.2"}`))
	})
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.lookups++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"user_not_found"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A bare `/switchboard agent` opens the picker instead of listing: the
// channel's agents, the default preselected, the channel carried along.
func TestABareAgentCommandOpensThePicker(t *testing.T) {
	api := &slackAPI{}
	a := newTestAdapter(api.server(t).URL)
	h := &agentRouter{choices: twoAgents}

	a.handleSlashCommand(context.Background(), h, nil, slack.SlashCommand{
		Command: "/switchboard", Text: "agent", ChannelID: "C1", UserID: "U9", TriggerID: "T1", ResponseURL: "https://hooks.example/r1",
	})

	if len(h.got()) != 0 {
		t.Errorf("the command ran as text too: %+v", h.got())
	}
	if len(api.views) != 1 {
		t.Fatalf("views.open calls = %d, want 1", len(api.views))
	}
	var v slack.ModalViewRequest
	if err := json.Unmarshal([]byte(api.views[0]), &v); err != nil {
		t.Fatalf("view: %v", err)
	}
	if v.CallbackID != agentModalID || v.PrivateMetadata != `{"c":"C1","r":"https://hooks.example/r1"}` {
		t.Errorf("callback %q, metadata %q; want %q and the channel", v.CallbackID, v.PrivateMetadata, agentModalID)
	}
	for _, want := range []string{`"value":"platform"`, `"value":"general"`, "Anything else", `"initial_option"`} {
		if !strings.Contains(api.views[0], want) {
			t.Errorf("view lacks %s: %s", want, api.views[0])
		}
	}
	if !strings.Contains(api.views[0], `"initial_option":{"text":{"type":"plain_text","text":"Platform agent (default)"`) {
		t.Errorf("the default is not preselected: %s", api.views[0])
	}
}

// When Slack refuses the picker, or there is nothing to pick, the command
// still answers — as the typed listing.
func TestThePickerFallsBackToTheListing(t *testing.T) {
	for name, tc := range map[string]struct {
		choices  []chat.AgentChoice
		trigger  string
		openFail bool
	}{
		"slack refuses":  {choices: twoAgents, trigger: "T1", openFail: true},
		"no trigger":     {choices: twoAgents},
		"nothing chosen": {trigger: "T1"},
	} {
		t.Run(name, func(t *testing.T) {
			api := &slackAPI{openFail: tc.openFail}
			a := newTestAdapter(api.server(t).URL)
			h := &agentRouter{choices: tc.choices, ack: "**Agents here**"}

			a.handleSlashCommand(context.Background(), h, nil, slack.SlashCommand{
				Command: "/switchboard", Text: "agent", ChannelID: "C1", UserID: "U9", TriggerID: tc.trigger,
			})

			if got := h.got(); len(got) != 1 || got[0].Name != "agent" {
				t.Errorf("commands = %+v, want the listing to run", got)
			}
		})
	}
}

// `agent general …` keeps the typed form: no picker.
func TestANamedAgentCommandOpensNoPicker(t *testing.T) {
	api := &slackAPI{}
	a := newTestAdapter(api.server(t).URL)
	h := &agentRouter{choices: twoAgents}

	a.handleSlashCommand(context.Background(), h, nil, slack.SlashCommand{
		Command: "/switchboard", Text: "agent general hi", ChannelID: "C1", UserID: "U9", TriggerID: "T1", ResponseURL: "https://hooks.example/r1",
	})

	if len(api.views) != 0 {
		t.Errorf("views.open called for a named agent")
	}
	if got := h.got(); len(got) != 1 || got[0].Text != "general hi" {
		t.Errorf("commands = %+v", got)
	}
}

func submission(agent, prompt string) slack.InteractionCallback {
	var cb slack.InteractionCallback
	cb.Type = slack.InteractionTypeViewSubmission
	cb.User.ID = "U9"
	cb.View.CallbackID = agentModalID
	cb.View.PrivateMetadata = `{"c":"C1"}`
	cb.View.State = &slack.ViewState{Values: map[string]map[string]slack.BlockAction{
		modalAgentBlock:  {modalAgentAction: {SelectedOption: slack.OptionBlockObject{Value: agent}}},
		modalPromptBlock: {modalPromptInput: {Value: prompt}}},
	}
	return cb
}

func waitForCommand(t *testing.T, h *agentRouter) chat.Command {
	t.Helper()
	for range 500 {
		if got := h.got(); len(got) > 0 {
			return got[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no command reached the handler")
	return chat.Command{}
}

// Start runs as the typed `agent <name> <prompt>`, in the channel the picker
// was opened in, as the person who submitted it; the ack reaches them alone.
func TestThePickersSubmitRunsTheAgentCommand(t *testing.T) {
	api := &slackAPI{}
	a := newTestAdapter(api.server(t).URL)
	a.callerByID["U9"] = "asker@example.com"
	h := &agentRouter{ack: "Starting a thread with General…"}

	a.handleInteractive(context.Background(), h, nil, submission("general", "tell me\nabout Kubernetes"))

	got := waitForCommand(t, h)
	if got.Name != "agent" || got.Channel != "C1" || got.Caller != "asker@example.com" || got.CallerMention != "<@U9>" {
		t.Errorf("command = %+v", got)
	}
	if got.Text != "general tell me\nabout Kubernetes" || len(got.Args) == 0 || got.Args[0] != "general" {
		t.Errorf("text %q, args %q; want the agent then the prompt, its lines kept", got.Text, got.Args)
	}
	for range 500 {
		api.mu.Lock()
		n := len(api.ephemera)
		api.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.ephemera) != 1 || api.ephemera[0]["user"] != "U9" || api.ephemera[0]["channel"] != "C1" {
		t.Errorf("ephemeral acks = %+v, want one to the submitter in C1", api.ephemera)
	}
}

func TestASubmitWithNoPromptJustNamesTheAgent(t *testing.T) {
	cmd, _, ok := agentCommandFromSubmission(submission("general", "  "))
	if !ok || cmd.Text != "general" || len(cmd.Args) != 1 {
		t.Errorf("cmd = %+v, ok %v", cmd, ok)
	}
}

// Another view's submission, or a picker with no agent, is not the command.
func TestOtherSubmissionsAreNotTheAgentCommand(t *testing.T) {
	other := submission("general", "")
	other.View.CallbackID = "someone_else"
	empty := submission("", "hi")
	block := submission("general", "")
	block.Type = slack.InteractionTypeBlockActions
	for name, cb := range map[string]slack.InteractionCallback{"other view": other, "no agent": empty, "block action": block} {
		if _, _, ok := agentCommandFromSubmission(cb); ok {
			t.Errorf("%s: read as the agent command", name)
		}
	}
}

// With the slash command's response_url, the acknowledgment goes back through
// it — which works where the app is not in the channel — not as a post.
func TestThePickersAckGoesThroughTheResponseURL(t *testing.T) {
	api := &slackAPI{}
	a := newTestAdapter(api.server(t).URL)
	h := &agentRouter{ack: "Starting a thread with General…"}
	got := make(chan map[string]any, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body
	}))
	t.Cleanup(hook.Close)

	cb := submission("general", "hi")
	cb.View.PrivateMetadata = `{"c":"C1","r":"` + hook.URL + `"}`
	a.handleInteractive(context.Background(), h, nil, cb)

	select {
	case body := <-got:
		if body["response_type"] != "ephemeral" || !strings.HasPrefix(body["text"].(string), "Starting") {
			t.Errorf("response_url body = %v", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing posted to the response_url")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.ephemera) != 0 {
		t.Errorf("also posted ephemerally: %+v", api.ephemera)
	}
}

// A long default label is clamped before the marker, not through it.
func TestALongDefaultLabelKeepsItsMarker(t *testing.T) {
	v := agentModal([]chat.AgentChoice{{Name: "x", Label: strings.Repeat("x", 90), Default: true}}, modalMeta{Channel: "C1"})
	sel := v.Blocks.BlockSet[0].(*slack.InputBlock).Element.(*slack.SelectBlockElement)
	text := sel.Options[0].Text.Text
	if !strings.HasSuffix(text, defaultMarker) || len([]rune(text)) > 75 {
		t.Errorf("label = %q (%d runes)", text, len([]rune(text)))
	}
}

// The listing fallback skips resolving the caller: it names none, and a slow
// picker open may have used the ack window up.
func TestTheListingFallbackResolvesNoCaller(t *testing.T) {
	api := &slackAPI{openFail: true}
	a := newTestAdapter(api.server(t).URL)
	h := &agentRouter{choices: twoAgents, ack: "**Agents here**"}

	a.handleSlashCommand(context.Background(), h, nil, slack.SlashCommand{
		Command: "/switchboard", Text: "agent", ChannelID: "C1", UserID: "U9", TriggerID: "T1",
	})

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.lookups != 0 {
		t.Errorf("users.info called %d times for the listing", api.lookups)
	}
	if got := h.got(); len(got) != 1 {
		t.Errorf("commands = %+v, want the listing", got)
	}
}
