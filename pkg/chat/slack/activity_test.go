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
	"unicode/utf8"

	"github.com/go-steer/switchboard/pkg/chat"
)

func streamActivity() *chat.Activity {
	return &chat.Activity{
		Head: "✖ Ran 2 tools (1 failed)",
		Steps: []chat.Step{
			{Title: "bash", Status: chat.StepDone, Code: "kubectl get pods -A", Note: "⏱ 2.3s"},
			{Title: "bash (exit 2)", Status: chat.StepFailed, Code: "kubectl get ns --context nope"},
			{Title: "read_file", Status: chat.StepRunning},
		},
	}
}

// The shape below is the one measured against Slack from the demo app on
// 2026-10-21 — a context line, then task cards whose details are rich text —
// so a change to it is a change to something known to post.
func TestActivityRendersAHeadAndOneTaskCardPerStep(t *testing.T) {
	blocks := activityBlocks(streamActivity(), toMrkdwn)
	got, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	want := `[` +
		`{"elements":[{"text":"✖ Ran 2 tools (1 failed)","type":"mrkdwn"}],"type":"context"},` +
		`{"details":{"elements":[` +
		`{"elements":[{"text":"kubectl get pods -A","type":"text"}],"type":"rich_text_preformatted"},` +
		`{"elements":[{"text":"⏱ 2.3s","type":"text"}],"type":"rich_text_section"}],"type":"rich_text"},` +
		`"status":"complete","task_id":"step-1","title":"bash","type":"task_card"},` +
		`{"details":{"elements":[` +
		`{"elements":[{"text":"kubectl get ns --context nope","type":"text"}],"type":"rich_text_preformatted"}],"type":"rich_text"},` +
		`"status":"error","task_id":"step-2","title":"bash (exit 2)","type":"task_card"},` +
		`{"status":"in_progress","task_id":"step-3","title":"read_file","type":"task_card"}` +
		`]`
	if string(got) != want {
		t.Errorf("blocks =\n%s\nwant\n%s", got, want)
	}
}

func TestActivityWithNoHeadIsCardsAlone(t *testing.T) {
	blocks := activityBlocks(&chat.Activity{Steps: []chat.Step{{Title: "bash", Status: chat.StepRunning}}}, toMrkdwn)
	if len(blocks) != 1 || blocks[0]["type"] != "task_card" {
		t.Errorf("blocks = %v, want one task card", blocks)
	}
}

// Slack refuses "pending", so nothing may reach it as a status but the three it
// takes — including a StepStatus this renderer has never heard of.
func TestEveryStatusIsOneSlackTakes(t *testing.T) {
	a := &chat.Activity{Steps: []chat.Step{
		{Title: "a", Status: chat.StepRunning},
		{Title: "b", Status: chat.StepDone},
		{Title: "c", Status: chat.StepFailed},
		{Title: "d", Status: "pending"},
		{Title: "e"},
	}}
	ok := map[any]bool{taskInProgress: true, taskComplete: true, taskError: true}
	for _, b := range activityBlocks(a, toMrkdwn) {
		if !ok[b["status"]] {
			t.Errorf("%v: status %v is not one Slack accepts", b["title"], b["status"])
		}
	}
}

func TestNoActivityRendersNoCards(t *testing.T) {
	for name, a := range map[string]*chat.Activity{
		"nil":      nil,
		"no steps": {Head: "⏳ Working… 3s"},
	} {
		if blocks := activityBlocks(a, toMrkdwn); blocks != nil {
			t.Errorf("%s: blocks = %v, want nil", name, blocks)
		}
	}
}

// More steps than a message can hold go as text, whole, rather than as cards
// with some quietly missing.
func TestTooManyStepsGoAsText(t *testing.T) {
	a := &chat.Activity{Steps: make([]chat.Step, maxTaskCards+1)}
	if blocks := activityBlocks(a, toMrkdwn); blocks != nil {
		t.Errorf("%d steps rendered %d blocks, want nil", len(a.Steps), len(blocks))
	}
}

// Cut on a character, never inside one: Slack counts characters, and half a
// multi-byte one is not valid UTF-8.
func TestALongTitleIsCutOnACharacter(t *testing.T) {
	a := &chat.Activity{Steps: []chat.Step{{Title: strings.Repeat("é", maxTaskTitle+10), Status: chat.StepDone}}}
	title, _ := activityBlocks(a, toMrkdwn)[0]["title"].(string)
	if n := utf8.RuneCountInString(title); n != maxTaskTitle {
		t.Errorf("title is %d characters, want %d", n, maxTaskTitle)
	}
	if !utf8.ValidString(title) {
		t.Error("title is not valid UTF-8")
	}
}

// An argument is the agent's, and goes as a rich-text text element, which Slack
// shows literally: a mention in it stays characters.
func TestAnArgumentIsLiteralText(t *testing.T) {
	a := &chat.Activity{Steps: []chat.Step{{Title: "bash", Status: chat.StepDone, Code: "echo <!channel> *hi*"}}}
	details := activityBlocks(a, toMrkdwn)[0]["details"].(map[string]any)["elements"].([]any)
	pre := details[0].(map[string]any)
	el := pre["elements"].([]any)[0].(map[string]any)
	if pre["type"] != "rich_text_preformatted" || el["type"] != "text" || el["text"] != "echo <!channel> *hi*" {
		t.Errorf("argument not carried verbatim as a text element: %v", pre)
	}
}

// activityServer fakes chat.postMessage and chat.update, refusing blocks when
// told to, and records every request's form.
type activityServer struct {
	mu     sync.Mutex
	refuse bool
	forms  []map[string][]string
}

func (s *activityServer) handler(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	s.mu.Lock()
	s.forms = append(s.forms, r.Form)
	refuse := s.refuse && strings.Contains(r.FormValue("blocks"), "task_card")
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if refuse {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_blocks"}`))
		return
	}
	_, _ = w.Write([]byte(`{"ok":true,"channel":"C0","ts":"111.111"}`))
}

func newActivityServer(t *testing.T, refuse bool) (*activityServer, *Adapter) {
	t.Helper()
	s := &activityServer{refuse: refuse}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", s.handler)
	mux.HandleFunc("/chat.update", s.handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, newTestAdapter(srv.URL)
}

// Cards whatever RichBlocks says, with the text beside them for notifications.
func TestAnActivityIsPostedAsTaskCardsWithoutRichBlocks(t *testing.T) {
	s, a := newActivityServer(t, false)
	if a.richBlocks {
		t.Fatal("this test is about the path where rich rendering is off")
	}
	_, err := a.Send(context.Background(), chat.Reply{
		Conversation: "C0:100.5", Text: "▸ Running `bash`", Kind: chat.KindActivity,
		Activity: &chat.Activity{Steps: []chat.Step{{Title: "bash", Status: chat.StepRunning}}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(s.forms) != 1 {
		t.Fatalf("%d posts, want 1", len(s.forms))
	}
	if blocks := s.forms[0]["blocks"]; len(blocks) != 1 || !strings.Contains(blocks[0], `"task_card"`) {
		t.Errorf("blocks = %v, want task cards", blocks)
	}
	if text := s.forms[0]["text"]; len(text) != 1 || !strings.Contains(text[0], "Running") {
		t.Errorf("text = %v, want the fallback beside the cards", text)
	}
}

// A Slack that refuses the cards still gets the notice, as text.
func TestRefusedCardsPostAsText(t *testing.T) {
	s, a := newActivityServer(t, true)
	ref, err := a.Send(context.Background(), chat.Reply{
		Conversation: "C0:100.5", Text: "▸ Running `bash`", Kind: chat.KindActivity,
		Activity: &chat.Activity{Steps: []chat.Step{{Title: "bash", Status: chat.StepRunning}}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ref.ID != "111.111" {
		t.Errorf("ref = %+v, want the text post's", ref)
	}
	if len(s.forms) != 2 {
		t.Fatalf("%d posts, want the refused cards and then the text", len(s.forms))
	}
	if blocks := s.forms[1]["blocks"]; len(blocks) != 0 {
		t.Errorf("the retry carried blocks %v, want text alone", blocks)
	}
}

func TestAnEditRedrawsTheCards(t *testing.T) {
	s, a := newActivityServer(t, false)
	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	err := a.Update(context.Background(), ref, chat.Reply{
		Conversation: "C0:100.5", Text: "⏳ Working… 30s · running `bash` (step 1)", Kind: chat.KindProgress,
		Activity: &chat.Activity{Head: "⏳ Working… 30s · step 1", Steps: []chat.Step{{Title: "bash", Status: chat.StepDone}}},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if blocks := s.forms[0]["blocks"]; len(blocks) != 1 || !strings.Contains(blocks[0], `"status":"complete"`) {
		t.Errorf("blocks = %v, want the redrawn cards", blocks)
	}
}

// The edit that retires a placeholder carries no Activity; it must take the
// cards down, as the text path's explicit empty block set does.
func TestAnEditWithoutActivityTakesTheCardsDown(t *testing.T) {
	s, a := newActivityServer(t, false)
	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	if err := a.Update(context.Background(), ref, chat.Reply{Conversation: "C0:100.5", Text: "✔ Done · 40s", Kind: chat.KindActivity}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if blocks := s.forms[0]["blocks"]; len(blocks) != 1 || blocks[0] != "[]" {
		t.Errorf("blocks = %v, want [] so the cards come down", blocks)
	}
}

func TestRefusedCardsEditAsText(t *testing.T) {
	s, a := newActivityServer(t, true)
	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	err := a.Update(context.Background(), ref, chat.Reply{
		Conversation: "C0:100.5", Text: "⏳ Working… 30s", Kind: chat.KindProgress,
		Activity: &chat.Activity{Steps: []chat.Step{{Title: "bash", Status: chat.StepRunning}}},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(s.forms) != 2 || s.forms[1]["blocks"][0] != "[]" {
		t.Errorf("forms = %v, want the refused cards then a text edit", s.forms)
	}
}

// One refusal is the workspace's answer, not the message's: after it, no reply
// tries cards again — each would cost a refused call before the text lands —
// and the adapter stops claiming to draw them, so the router stops making the
// edits only cards would show.
func TestOneRefusalStopsTheCards(t *testing.T) {
	s, a := newActivityServer(t, true)
	if !a.DrawsActivity() {
		t.Fatal("a fresh adapter does not draw activity")
	}
	reply := chat.Reply{
		Conversation: "C0:100.5", Text: "▸ Running `bash`", Kind: chat.KindActivity,
		Activity: &chat.Activity{Steps: []chat.Step{{Title: "bash", Status: chat.StepRunning}}},
	}
	if _, err := a.Send(context.Background(), reply); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if a.DrawsActivity() {
		t.Error("still claims to draw activity after Slack refused the cards")
	}
	if _, err := a.Send(context.Background(), reply); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	if err := a.Update(context.Background(), ref, reply); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// The refused post and its text retry, then one text post, then one text edit.
	if len(s.forms) != 4 {
		t.Fatalf("%d calls, want 4 — cards tried once only", len(s.forms))
	}
	for i, f := range s.forms[2:] {
		if b := f["blocks"]; len(b) > 0 && strings.Contains(b[0], "task_card") {
			t.Errorf("call %d after the refusal still sent cards: %v", i+2, b)
		}
	}
}
