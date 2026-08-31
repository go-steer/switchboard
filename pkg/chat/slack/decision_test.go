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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/go-steer/switchboard/pkg/chat"
)

// twoOptions is the smallest thing that is actually a decision.
func twoOptions() *chat.Decision {
	return &chat.Decision{ID: "prompt-1", Options: []chat.DecisionOption{
		{Value: "deny", Label: "Deny"},
		{Value: "allow-once", Label: "Allow once"},
	}}
}

// actionsOf returns the elements of the first actions block, failing if there
// is none — every assertion below is about what the buttons say.
func actionsOf(t *testing.T, blocks []map[string]any) []map[string]any {
	t.Helper()
	for _, b := range blocks {
		if b["type"] != "actions" {
			continue
		}
		els, _ := b["elements"].([]any)
		out := make([]map[string]any, 0, len(els))
		for _, e := range els {
			m, ok := e.(map[string]any)
			if !ok {
				t.Fatalf("actions element is %T, not an object", e)
			}
			out = append(out, m)
		}
		return out
	}
	t.Fatalf("no actions block in %v", blocks)
	return nil
}

// buttonText digs the plain_text label out of a button element.
func buttonText(t *testing.T, el map[string]any) string {
	t.Helper()
	obj, ok := el["text"].(map[string]any)
	if !ok {
		t.Fatalf("button has no text object: %v", el)
	}
	s, _ := obj["text"].(string)
	return s
}

// The answer has to survive the round trip: what Slack sends back on a press
// is the action_id and the value, and nothing else identifies which question
// was answered or how.
func TestEveryAnswerCarriesEnoughToBeAnswered(t *testing.T) {
	d := twoOptions()
	els := actionsOf(t, decisionBlocks("run this?", d, toMrkdwn))
	if len(els) != 2 {
		t.Fatalf("got %d buttons, want 2", len(els))
	}
	for i, el := range els {
		want := d.Options[i]
		if got := el["action_id"]; got != actionPrefix+want.Value {
			t.Errorf("button %d action_id = %v, want %q", i, got, actionPrefix+want.Value)
		}
		if got := el["value"]; got != d.ID {
			t.Errorf("button %d value = %v, want the decision id %q", i, got, d.ID)
		}
		if got := buttonText(t, el); got != want.Label {
			t.Errorf("button %d reads %q, want %q", i, got, want.Label)
		}
	}
}

// Buttons render no markup, so a label sent as mrkdwn shows its asterisks.
func TestButtonLabelsAreNotMarkup(t *testing.T) {
	for _, el := range actionsOf(t, decisionBlocks("body", twoOptions(), toMrkdwn)) {
		obj := el["text"].(map[string]any)
		if obj["type"] != "plain_text" {
			t.Errorf("button label type = %v, want plain_text", obj["type"])
		}
	}
}

// Broad is a request for friction. Slack's one affordance for it is the
// confirmation dialog, and an answer that outlives the request is the reason
// the flag exists — so it has to reach the payload, not merely be readable.
func TestAnAnswerThatOutlivesTheRequestAsksTwice(t *testing.T) {
	d := twoOptions()
	d.Options = append(d.Options, chat.DecisionOption{
		Value: "allow-always", Label: "Always allow this directory", Broad: true,
	})
	els := actionsOf(t, decisionBlocks("body", d, toMrkdwn))
	if len(els) != 3 {
		t.Fatalf("got %d buttons, want 3", len(els))
	}
	for i, el := range els[:2] {
		if _, ok := el["confirm"]; ok {
			t.Errorf("button %d asks twice for an answer that ends with the request", i)
		}
	}
	confirm, ok := els[2]["confirm"].(map[string]any)
	if !ok {
		t.Fatal("the standing grant does not ask twice")
	}
	// The dialog is the one place the label can be read without a button's
	// width budget, so it must carry the label rather than paraphrase it.
	text := confirm["text"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Always allow this directory") {
		t.Errorf("confirmation does not repeat what is being allowed: %q", text)
	}
}

// A label past Slack's button limit fails the whole post with invalid_blocks,
// taking the question with it. Clamping is what keeps one long tool name from
// costing the thread its only way to unblock the agent.
func TestALabelTooLongForSlackIsCutRatherThanRejected(t *testing.T) {
	d := twoOptions()
	d.Options[1].Label = strings.Repeat("é", 500) // multi-byte, so a naive cut splits a rune
	els := actionsOf(t, decisionBlocks("body", d, toMrkdwn))
	label := buttonText(t, els[1])
	if n := len([]rune(label)); n > maxButtonText {
		t.Errorf("label is %d runes, want <= %d", n, maxButtonText)
	}
	if !strings.HasSuffix(label, "…") {
		t.Errorf("a clipped label does not say it was clipped: %q", label)
	}
	// A cut mid-rune is rejected by Slack as invalid UTF-8, which fails the
	// post rather than merely looking wrong.
	if !utf8.ValidString(label) {
		t.Errorf("clipping split a rune: %q", label)
	}
}

// An option whose value will not fit an action_id comes back naming something
// else, so it is dropped rather than rendered as a button that lies about what
// it does.
func TestAnUnanswerableOptionIsNotOffered(t *testing.T) {
	d := twoOptions()
	d.Options = append(d.Options,
		chat.DecisionOption{Value: strings.Repeat("x", maxActionID), Label: "Too long to identify"},
		chat.DecisionOption{Value: "", Label: "Nameless"},
	)
	els := actionsOf(t, decisionBlocks("body", d, toMrkdwn))
	if len(els) != 2 {
		t.Fatalf("got %d buttons, want only the 2 that can be answered", len(els))
	}
	for _, el := range els {
		if buttonText(t, el) == "Too long to identify" || buttonText(t, el) == "Nameless" {
			t.Errorf("offered an answer that cannot come back: %v", el)
		}
	}
}

// Slack caps an actions block at 25 elements and rejects the whole payload
// past it.
func TestNoMoreButtonsThanSlackWillTake(t *testing.T) {
	d := &chat.Decision{ID: "p"}
	for i := range maxActionElements + 10 {
		d.Options = append(d.Options, chat.DecisionOption{
			Value: string(rune('a'+i%26)) + strings.Repeat("z", i), Label: "opt",
		})
	}
	if n := len(actionsOf(t, decisionBlocks("body", d, toMrkdwn))); n != maxActionElements {
		t.Errorf("got %d buttons, want %d", n, maxActionElements)
	}
}

// Nothing to decide means no interactive render: the reply falls through to
// the ordinary text path, which still posts the question as prose. A question
// nobody can click is worse than one nobody can read, but only just.
func TestNothingToDecideRendersNoButtons(t *testing.T) {
	one := &chat.Decision{ID: "p", Options: []chat.DecisionOption{{Value: "deny", Label: "Deny"}}}
	for name, d := range map[string]*chat.Decision{
		"nil":   nil,
		"empty": {ID: "p"},
		"one":   one,
	} {
		if got := decisionBlocks("body", d, toMrkdwn); got != nil {
			t.Errorf("%s decision rendered %d blocks, want none", name, len(got))
		}
	}
	// Two options that both fail to render is the same situation arrived at
	// later, and must reach the same answer.
	unanswerable := &chat.Decision{ID: "p", Options: []chat.DecisionOption{
		{Value: "", Label: "a"}, {Value: "", Label: "b"},
	}}
	if got := decisionBlocks("body", unanswerable, toMrkdwn); got != nil {
		t.Errorf("two unanswerable options rendered %d blocks, want none", len(got))
	}
}

// The body goes above the buttons, so the question is read before it is
// answered rather than after.
func TestTheQuestionComesBeforeTheAnswers(t *testing.T) {
	blocks := decisionBlocks("**Permission needed**", twoOptions(), toMrkdwn)
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want a section then an actions block: %v", len(blocks), blocks)
	}
	if blocks[0]["type"] != "section" || blocks[1]["type"] != "actions" {
		t.Errorf("block order is %v then %v", blocks[0]["type"], blocks[1]["type"])
	}
	body := blocks[0]["text"].(map[string]any)["text"].(string)
	if !strings.Contains(body, "Permission needed") {
		t.Errorf("section does not carry the question: %q", body)
	}
}

// A question with no body is still a question. Slack rejects a section whose
// text is empty, so the buttons have to go out alone rather than not at all.
func TestAQuestionWithNoBodyStillOffersItsAnswers(t *testing.T) {
	blocks := decisionBlocks("   ", twoOptions(), toMrkdwn)
	if len(blocks) != 1 || blocks[0]["type"] != "actions" {
		t.Fatalf("got %v, want a lone actions block", blocks)
	}
}

// sanitizeBlocks runs over everything on the way out; a decision payload must
// survive it, since a dropped actions block is a question with no answers.
func TestASanitizedDecisionKeepsItsButtons(t *testing.T) {
	blocks := sanitizeBlocks(decisionBlocks("body", twoOptions(), toMrkdwn))
	if len(actionsOf(t, blocks)) != 2 {
		t.Errorf("sanitizing dropped the answers: %v", blocks)
	}
}

// ---------------------------------------------------------------- the press

// callback builds a block_actions payload of the shape Slack sends.
func callback(actionID, value string) slack.InteractionCallback {
	cb := slack.InteractionCallback{Type: slack.InteractionTypeBlockActions}
	cb.Channel.ID = "C1"
	cb.Container.MessageTs = "200.2"
	cb.Container.ThreadTs = "100.1"
	cb.User.ID = "U9"
	cb.ActionCallback.BlockActions = []*slack.BlockAction{{ActionID: actionID, Value: value}}
	return cb
}

// A press has to name the same conversation an inbound mention in that thread
// would, or the router looks up a session that does not exist.
func TestAPressNamesTheThreadItWasMadeIn(t *testing.T) {
	p, ok := pressFrom(callback(actionPrefix+"allow-once", "prompt-1"))
	if !ok {
		t.Fatal("a block-actions press on our own button was not recognised")
	}
	wantConv := conversationKey("C1", "100.1")
	if p.Conversation != wantConv {
		t.Errorf("conversation = %q, want %q", p.Conversation, wantConv)
	}
	if p.Channel != "C1" {
		t.Errorf("channel = %q, want C1", p.Channel)
	}
	if p.DecisionID != "prompt-1" || p.Option != "allow-once" {
		t.Errorf("press = {%q %q}, want {prompt-1 allow-once}", p.DecisionID, p.Option)
	}
	// The ref points at the message the buttons are on, which is what a later
	// in-place update has to address.
	if p.Message.ID != "200.2" || p.Message.Conversation != wantConv {
		t.Errorf("message ref = %+v, want the posted question", p.Message)
	}
}

// A press on a message that roots its own thread keys on that message, exactly
// as a top-level mention does.
func TestAPressOnAThreadlessMessageRootsTheThread(t *testing.T) {
	cb := callback(actionPrefix+"deny", "p")
	cb.Container.ThreadTs = ""
	p, ok := pressFrom(cb)
	if !ok {
		t.Fatal("press not recognised")
	}
	if got, want := p.Conversation, conversationKey("C1", "200.2"); got != want {
		t.Errorf("conversation = %q, want %q", got, want)
	}
}

// The adapter shares its socket with slash commands and shortcuts. Anything
// that is not one of switchboard's decision buttons must fall straight
// through, not arrive as a press with empty fields.
func TestSomethingThatIsNotOneOfOurButtonsIsNotAPress(t *testing.T) {
	if _, ok := pressFrom(callback("some_other_app_action", "v")); ok {
		t.Error("an unrelated block action was read as a decision")
	}
	shortcut := callback(actionPrefix+"deny", "p")
	shortcut.Type = slack.InteractionTypeShortcut
	if _, ok := pressFrom(shortcut); ok {
		t.Error("a shortcut was read as a decision")
	}
	noChannel := callback(actionPrefix+"deny", "p")
	noChannel.Channel.ID = ""
	if _, ok := pressFrom(noChannel); ok {
		t.Error("a press naming no channel was accepted; it addresses nothing")
	}
}

// The press carries no caller: resolving it is the dispatcher's job, from the
// callback's own user. Filling it in here from anything else would attribute
// an approval to whoever the message happened to be about.
func TestPressFromLeavesTheCallerToBeResolved(t *testing.T) {
	p, _ := pressFrom(callback(actionPrefix+"deny", "p"))
	if p.Caller != "" {
		t.Errorf("caller = %q, want it left for the dispatcher to resolve", p.Caller)
	}
}

// pressRecorder is a Handler that only cares about presses.
type pressRecorder struct {
	mu      sync.Mutex
	presses []chat.Press
	err     error
}

func (h *pressRecorder) Handle(context.Context, chat.Message) error { return nil }

func (h *pressRecorder) HandleCommand(context.Context, chat.Command) (string, error) {
	return "", nil
}

func (h *pressRecorder) HandlePress(_ context.Context, p chat.Press) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.presses = append(h.presses, p)
	return h.err
}

func (h *pressRecorder) got() []chat.Press {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]chat.Press(nil), h.presses...)
}

// waitForPress polls for the handoff, which happens off the socket goroutine so
// a daemon round trip cannot eat the 3-second ack window.
func waitForPress(t *testing.T, h *pressRecorder) []chat.Press {
	t.Helper()
	for range 200 {
		if got := h.got(); len(got) > 0 {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no press reached the handler")
	return nil
}

// The presser is the person who clicked, resolved from the callback's own
// user. Attributing an approval to anyone else — the message's author, the
// agent that asked — would put a name on a decision they did not make.
func TestThePressIsAttributedToWhoeverPressedIt(t *testing.T) {
	a := newTestAdapter("http://127.0.0.1:0")
	a.callerByID["U9"] = "presser@example.com"
	h := &pressRecorder{}

	a.handleInteractive(context.Background(), h, nil, callback(actionPrefix+"allow-once", "prompt-1"))

	got := waitForPress(t, h)[0]
	if got.Caller != "presser@example.com" {
		t.Errorf("caller = %q, want the person who pressed", got.Caller)
	}
	if got.Option != "allow-once" || got.DecisionID != "prompt-1" {
		t.Errorf("press = %+v, want the answer that was clicked", got)
	}
}

// The socket loop has to route the interactive event at all. Everything above
// calls handleInteractive directly, so without this the arm that reaches it
// could go away with every other test still passing.
func TestTheSocketLoopRoutesAnInteraction(t *testing.T) {
	a := newTestAdapter("http://127.0.0.1:0")
	a.callerByID["U9"] = "presser@example.com"
	h := &pressRecorder{}

	a.dispatch(context.Background(), h, socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: callback(actionPrefix+"deny", "pr1"),
	})

	if got := waitForPress(t, h)[0]; got.Option != "deny" {
		t.Errorf("press = %+v, want the answer that was clicked", got)
	}
}

// A payload of the wrong shape under the right event type is a protocol
// mismatch, not a press. It must not panic the socket loop.
func TestAnInteractionOfTheWrongShapeIsDropped(t *testing.T) {
	a := newTestAdapter("http://127.0.0.1:0")
	h := &pressRecorder{}

	a.dispatch(context.Background(), h, socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: "not a callback",
	})

	time.Sleep(20 * time.Millisecond)
	if got := h.got(); len(got) != 0 {
		t.Errorf("handler saw %+v, want nothing", got)
	}
}

// An interaction that is not one of our buttons must not reach the handler at
// all: the socket carries every interactive payload the app is subscribed to.
func TestAnUnrelatedInteractionNeverReachesTheHandler(t *testing.T) {
	a := newTestAdapter("http://127.0.0.1:0")
	h := &pressRecorder{}

	a.handleInteractive(context.Background(), h, nil, callback("other_app:go", "v"))

	time.Sleep(20 * time.Millisecond)
	if got := h.got(); len(got) != 0 {
		t.Errorf("handler saw %+v, want nothing", got)
	}
}

// A handler that fails is logged, not retried and not propagated: the ack has
// already gone out, and there is nobody left to tell.
func TestAFailedPressIsLoggedNotPropagated(t *testing.T) {
	var logged []string
	var mu sync.Mutex
	a := newTestAdapter("http://127.0.0.1:0")
	// Primed, so the only thing that can log here is the failure itself —
	// resolving a caller against an unreachable API logs too, and a test that
	// accepts any line at all would pass on that.
	a.callerByID["U9"] = "presser@example.com"
	a.logf = func(f string, v ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(f, v...))
	}
	h := &pressRecorder{err: errors.New("daemon down")}

	a.handleInteractive(context.Background(), h, nil, callback(actionPrefix+"deny", "p"))
	waitForPress(t, h)

	for range 500 {
		mu.Lock()
		lines := strings.Join(logged, "\n")
		mu.Unlock()
		// The line has to name the failure and which answer it was, or it says
		// only that something happened somewhere.
		if strings.Contains(lines, "daemon down") && strings.Contains(lines, "deny") {
			return
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Errorf("a press that failed left no usable trace: %v", logged)
}

// Send posts a decision as buttons whether or not rich rendering is on: that
// flag is about how prose looks, this is about whether the question can be
// answered at all.
func TestADecisionIsPostedWithButtonsWithoutRichBlocks(t *testing.T) {
	var gotBlocks string
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotBlocks = r.FormValue("blocks")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C0","ts":"111.111"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	if a.richBlocks {
		t.Fatal("this test is about the path where rich rendering is off")
	}
	_, err := a.Send(context.Background(), chat.Reply{
		Conversation: "C0:100.5",
		Text:         "Permission needed",
		Kind:         chat.KindDecision,
		Decision:     twoOptions(),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(gotBlocks, `"actions"`) {
		t.Errorf("posted no actions block: %s", gotBlocks)
	}
	if !strings.Contains(gotBlocks, actionPrefix+"allow-once") {
		t.Errorf("posted no answerable button: %s", gotBlocks)
	}
}

// An ordinary reply must not grow buttons because this code exists.
func TestAnOrdinaryReplyStillPostsAsText(t *testing.T) {
	var gotBlocks, gotText string
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotBlocks, gotText = r.FormValue("blocks"), r.FormValue("text")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C0","ts":"1.1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	if _, err := a.Send(context.Background(), chat.Reply{Conversation: "C0:100.5", Text: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotBlocks != "" {
		t.Errorf("a plain reply grew blocks: %s", gotBlocks)
	}
	if gotText != "hello" {
		t.Errorf("text = %q, want hello", gotText)
	}
}

// A question that has been answered must stop being answerable. chat.update
// leaves a message's existing blocks alone when the field is absent, so an edit
// that only sets text repaints the words above a set of buttons that still
// work — and a second press on a settled question is exactly the confusion the
// edit exists to prevent.
func TestAnEditTakesTheButtonsDown(t *testing.T) {
	var gotBlocks, gotText string
	var sawBlocksField bool
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_, sawBlocksField = r.Form["blocks"]
		gotBlocks, gotText = r.FormValue("blocks"), r.FormValue("text")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C0","ts":"111.111"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	err := a.Update(context.Background(), ref, chat.Reply{
		Conversation: "C0:100.5",
		Text:         "Allowed, this once — ana@example.com",
		Kind:         chat.KindDecision,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !sawBlocksField {
		t.Fatal("the edit sent no blocks field at all, so Slack keeps the buttons the question was posted with")
	}
	if gotBlocks != "[]" {
		t.Errorf("blocks = %q, want [] — anything else leaves something rendered", gotBlocks)
	}
	if !strings.Contains(gotText, "ana@example.com") {
		t.Errorf("text = %q, want the record of who answered", gotText)
	}
}

// Update has two paths and only one of them is above. With --slack-rich-blocks
// the edit sends its own block set, which replaces the posted one wholesale —
// so the buttons come down for a different reason, and it is worth pinning that
// they do rather than reasoning that they must.
func TestAnEditTakesTheButtonsDownWithRichBlocksToo(t *testing.T) {
	var gotBlocks string
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotBlocks = r.FormValue("blocks")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C0","ts":"111.111"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	a.richBlocks = true
	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	err := a.Update(context.Background(), ref, chat.Reply{
		Conversation: "C0:100.5",
		Text:         "**Permission needed** — `bash`\n\n✅ **Allowed**, this once — ana@example.com",
		Kind:         chat.KindDecision,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if strings.Contains(gotBlocks, "actions") || strings.Contains(gotBlocks, "button") {
		t.Errorf("the edit still renders something pressable: %s", gotBlocks)
	}
	if !strings.Contains(gotBlocks, "ana@example.com") {
		t.Errorf("blocks = %s, want the record of who answered", gotBlocks)
	}
}
