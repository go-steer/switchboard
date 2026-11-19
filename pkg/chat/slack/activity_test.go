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

// turnActivity is a turn part-way through: a call that has finished, a
// delegation still running with two calls under it, and a call that failed.
func turnActivity() *chat.Activity {
	return &chat.Activity{
		Title:  "Working… 40s · 5 calls · _fallback › grep",
		Status: chat.StepRunning,
		Steps: []chat.Step{
			{Title: "bash", Status: chat.StepDone, Code: "kubectl get pods -A", Note: "2.3s"},
			{Title: "_fallback", Status: chat.StepRunning, Code: "check billing", Calls: 2, Steps: []chat.Step{
				{Title: "read_file", Status: chat.StepDone, Code: "deploy/billing.yaml", Note: "0.4s"},
				{Title: "grep", Status: chat.StepRunning, Code: "memory"},
			}},
			{Title: "bash (exit 2)", Status: chat.StepFailed},
		},
	}
}

// line is the JSON of one line of a card's details, built the way the
// renderer orders it: the mark, the bold title, the plain run, the code.
func line(mark, title, plain, code string) string {
	q := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	els := `{"text":` + q(mark+" ") + `,"type":"text"},{"style":{"bold":true},"text":` + q(title) + `,"type":"text"}`
	if plain != "" {
		els += `,{"text":` + q(plain) + `,"type":"text"}`
	}
	if code != "" {
		els += `,{"style":{"code":true},"text":` + q(code) + `,"type":"text"}`
	}
	return `{"elements":[` + els + `],"type":"rich_text_section"}`
}

func list(indent string, lines ...string) string {
	return `{"elements":[` + strings.Join(lines, ",") + `],"indent":` + indent + `,"style":"bullet","type":"rich_text_list"}`
}

// The shape below is the one measured against Slack on 2026-11-19 — one task
// card whose details are bullet lists, a delegation's calls nested at indent 1
// — so a change to it is a change to something known to post.
func TestAnActivityIsOneTaskCard(t *testing.T) {
	blocks := activityBlocks(turnActivity())
	got, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"block_id":"sb-activity","details":{"elements":[` +
		list("0",
			line("✔\uFE0E", "bash", "  2.3s  ", "kubectl get pods -A"),
			line("▸", "_fallback", " · 2 calls  ", "check billing")) + `,` +
		list("1",
			line("✔\uFE0E", "read_file", "  0.4s  ", "deploy/billing.yaml"),
			line("▸", "grep", "  ", "memory")) + `,` +
		list("0",
			line("✖\uFE0E", "bash (exit 2)", "", "")) +
		`],"type":"rich_text"},"status":"in_progress","task_id":"turn","title":"Working… 40s · 5 calls · _fallback › grep","type":"task_card"}]`
	if string(got) != want {
		t.Errorf("blocks =\n%s\nwant\n%s", got, want)
	}
}

// The card is the whole message: no context line above it, one block.
func TestAnActivityIsASingleBlock(t *testing.T) {
	blocks := activityBlocks(turnActivity())
	if len(blocks) != 1 || blocks[0]["type"] != "task_card" {
		t.Fatalf("blocks = %v, want one task card and nothing else", blocks)
	}
}

// cardLines digs the lines out of a card's details, list by list.
func cardLines(t *testing.T, card map[string]any) [][]map[string]any {
	t.Helper()
	details, ok := card["details"].(map[string]any)
	if !ok {
		t.Fatalf("card has no details: %v", card)
	}
	var out [][]map[string]any
	for _, l := range details["elements"].([]any) {
		var lines []map[string]any
		for _, sec := range l.(map[string]any)["elements"].([]any) {
			lines = append(lines, sec.(map[string]any))
		}
		out = append(out, lines)
	}
	return out
}

// els is a line's text elements, in order.
func els(line map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range line["elements"].([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

// One line per step, in one order: the mark and a space; the title in bold;
// then how many calls were made under it, the note, and the argument as code —
// each only when there is one, the plain parts two spaces apart.
func TestALineIsInOrder(t *testing.T) {
	a := &chat.Activity{Title: "t", Steps: []chat.Step{
		{Title: "_fallback", Status: chat.StepDone, Calls: 23, Note: "1m02s", Code: "check billing"},
		{Title: "one", Status: chat.StepDone, Calls: 1},
		{Title: "bare", Status: chat.StepDone},
		{Title: "noted", Status: chat.StepDone, Note: "2.3s"},
		{Title: "coded", Status: chat.StepDone, Code: "ls"},
	}}
	lines := cardLines(t, activityBlocks(a)[0])[0]
	text := func(i int) (out []string) {
		for _, e := range els(lines[i]) {
			out = append(out, e["text"].(string))
		}
		return out
	}
	for i, want := range [][]string{
		{"✔\uFE0E ", "_fallback", " · 23 calls  1m02s  ", "check billing"},
		{"✔\uFE0E ", "one", " · 1 call"},
		{"✔\uFE0E ", "bare"},
		{"✔\uFE0E ", "noted", "  2.3s"},
		{"✔\uFE0E ", "coded", "  ", "ls"},
	} {
		if got := text(i); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("line %d = %q, want %q", i, got, want)
		}
	}
	full := els(lines[0])
	if style, _ := full[1]["style"].(map[string]any); style["bold"] != true {
		t.Errorf("title style = %v, want bold", full[1]["style"])
	}
	if style, _ := full[3]["style"].(map[string]any); style["code"] != true {
		t.Errorf("argument style = %v, want code", full[3]["style"])
	}
	// The count of calls under a step is beside its name, not part of it.
	if _, styled := full[2]["style"]; styled {
		t.Errorf("the plain run is styled: %v", full[2])
	}
}

// The check and the cross carry the text-style selector: without it Slack
// printed the bare character as ":heavy_check_mark:" (measured).
func TestMarksCarryTheTextSelector(t *testing.T) {
	a := &chat.Activity{Title: "t", Steps: []chat.Step{
		{Title: "a", Status: chat.StepRunning},
		{Title: "b", Status: chat.StepDone},
		{Title: "c", Status: chat.StepFailed},
		{Title: "d", Status: "pending"},
	}}
	lines := cardLines(t, activityBlocks(a)[0])[0]
	for i, want := range []string{"▸ ", "\u2714\uFE0E ", "\u2716\uFE0E ", "▸ "} {
		if got := els(lines[i])[0]["text"]; got != want {
			t.Errorf("mark %d = %q, want %q", i, got, want)
		}
	}
}

// A step no result was seen for is drawn with a mark of its own — an en dash,
// which is not the check, the cross or the triangle of something running —
// as literal text like the others, in the card and in the lines under it.
func TestAnUnfinishedStepHasANeutralMark(t *testing.T) {
	a := &chat.Activity{Title: "32 calls · 2m15s · 1 unfinished", Status: chat.StepDone, Steps: []chat.Step{
		{Title: "_fallback", Status: chat.StepUnfinished, Calls: 31, Code: "check billing", Steps: []chat.Step{
			{Title: "get_k8s_logs ×31 (exit 1)", Status: chat.StepFailed},
			{Title: "grep", Status: chat.StepUnfinished},
		}},
	}}
	a.Recent = []chat.Step{{Title: "grep", Status: chat.StepUnfinished}}
	blocks := activityBlocks(a)
	lists := cardLines(t, blocks[0])
	head, nested := els(lists[0][0]), els(lists[1][1])
	for name, line := range map[string][]map[string]any{"top-level": head, "nested": nested} {
		mark := line[0]
		if mark["text"] != "\u2013 " || mark["type"] != "text" {
			t.Errorf("%s mark = %v, want a literal en dash", name, mark)
		}
		if _, styled := mark["style"]; styled {
			t.Errorf("%s mark is styled: %v", name, mark)
		}
	}
	for _, other := range []string{"▸", "\u2714", "\u2716"} {
		if strings.Contains(head[0]["text"].(string), other) {
			t.Errorf("the unfinished mark %q contains %q", head[0]["text"], other)
		}
	}
	// The rest of the line is as any other.
	if head[1]["text"] != "_fallback" || head[2]["text"] != " · 31 calls  " || head[3]["text"] != "check billing" {
		t.Errorf("line = %v", head)
	}
	// And in the lines drawn under the card.
	recent := blocks[1]["elements"].([]any)[0].(map[string]any)["elements"].([]any)[0].(map[string]any)
	if got := els(recent)[0]["text"]; got != "\u2013 " {
		t.Errorf("recent mark = %q, want the en dash", got)
	}
	// The card it sits in reads done.
	if blocks[0]["status"] != taskComplete {
		t.Errorf("card status = %v", blocks[0]["status"])
	}
}

// A step's own steps are a list of their own at indent 1, directly after it,
// and the steps that follow go back to indent 0.
func TestNestedStepsAreIndentedUnderTheirStep(t *testing.T) {
	a := &chat.Activity{Title: "t", Steps: []chat.Step{
		{Title: "a", Status: chat.StepDone},
		{Title: "parent", Status: chat.StepDone, Steps: []chat.Step{{Title: "kid1", Status: chat.StepDone}, {Title: "kid2", Status: chat.StepDone}}},
		{Title: "b", Status: chat.StepDone},
		{Title: "c", Status: chat.StepDone},
	}}
	details := activityBlocks(a)[0]["details"].(map[string]any)["elements"].([]any)
	var shape []string
	for _, l := range details {
		m := l.(map[string]any)
		if m["type"] != "rich_text_list" || m["style"] != "bullet" {
			t.Fatalf("details element = %v, want a bullet list", m)
		}
		var titles []string
		for _, sec := range m["elements"].([]any) {
			titles = append(titles, els(sec.(map[string]any))[1]["text"].(string))
		}
		b, _ := json.Marshal(m["indent"])
		shape = append(shape, string(b)+":"+strings.Join(titles, ","))
	}
	if got, want := strings.Join(shape, " "), "0:a,parent 1:kid1,kid2 0:b,c"; got != want {
		t.Errorf("lists = %s, want %s", got, want)
	}
}

// Slack refuses "pending", so nothing may reach it as a status but the three it
// takes — including a StepStatus this renderer has never heard of.
func TestEveryStatusIsOneSlackTakes(t *testing.T) {
	for status, want := range map[chat.StepStatus]string{
		chat.StepRunning: taskInProgress,
		chat.StepDone:    taskComplete,
		chat.StepFailed:  taskError,
		// Not a state the router gives a card, and never one that spins.
		chat.StepUnfinished: taskComplete,
		"pending":           taskInProgress,
		"":                  taskInProgress,
	} {
		if got := activityBlocks(&chat.Activity{Title: "t", Status: status})[0]["status"]; got != want {
			t.Errorf("status %q drew as %v, want %v", status, got, want)
		}
	}
}

// An activity with no steps yet is a card that is a title alone, which Slack
// takes (measured): no details at all, rather than empty ones.
func TestAnActivityWithNoStepsIsATitleOnlyCard(t *testing.T) {
	blocks := activityBlocks(&chat.Activity{Title: "Working… 3s · 0 calls", Status: chat.StepRunning})
	if len(blocks) != 1 || blocks[0]["type"] != "task_card" || blocks[0]["title"] != "Working… 3s · 0 calls" {
		t.Fatalf("blocks = %v, want one titled card", blocks)
	}
	if _, has := blocks[0]["details"]; has {
		t.Errorf("a card with no steps carries details: %v", blocks[0]["details"])
	}
}

func TestNoActivityRendersNoCard(t *testing.T) {
	if blocks := activityBlocks(nil); blocks != nil {
		t.Errorf("blocks = %v, want nil", blocks)
	}
}

// The belt: more lines than a card was measured to hold go as text, whole,
// rather than as a card with some quietly missing. Nested lines count.
func TestTooManyLinesGoAsText(t *testing.T) {
	flat := &chat.Activity{Title: "t", Steps: make([]chat.Step, maxTaskLines)}
	if blocks := activityBlocks(flat); blocks == nil {
		t.Errorf("%d lines rendered nothing, want a card", maxTaskLines)
	}
	flat.Steps = append(flat.Steps, chat.Step{Title: "one more"})
	if blocks := activityBlocks(flat); blocks != nil {
		t.Errorf("%d lines rendered %d blocks, want nil", len(flat.Steps), len(blocks))
	}
	nested := &chat.Activity{Title: "t", Steps: []chat.Step{{Title: "parent", Steps: make([]chat.Step, maxTaskLines)}}}
	if blocks := activityBlocks(nested); blocks != nil {
		t.Errorf("%d nested lines under one step rendered a card, want nil", maxTaskLines)
	}
}

// Cut on a character, never inside one: Slack counts characters, and half a
// multi-byte one is not valid UTF-8. The card's title and a line's alike.
func TestLongTitlesAreCutOnACharacter(t *testing.T) {
	long := strings.Repeat("é", maxTaskTitle+10)
	a := &chat.Activity{Title: long, Steps: []chat.Step{{Title: long, Status: chat.StepDone, Code: strings.Repeat("ü", maxTaskCode+10)}}}
	card := activityBlocks(a)[0]
	line := els(cardLines(t, card)[0][0])
	for name, got := range map[string]string{
		"card title": card["title"].(string),
		"line title": line[1]["text"].(string),
	} {
		if n := utf8.RuneCountInString(got); n != maxTaskTitle {
			t.Errorf("%s is %d characters, want %d", name, n, maxTaskTitle)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s is not valid UTF-8", name)
		}
	}
	code := line[3]["text"].(string)
	if n := utf8.RuneCountInString(code); n != maxTaskCode || !utf8.ValidString(code) {
		t.Errorf("argument is %d characters (valid: %t), want %d valid ones", n, utf8.ValidString(code), maxTaskCode)
	}
}

// An argument is the agent's, and goes as a rich-text text element, which Slack
// shows literally: a mention in it stays characters. So does a tool name.
func TestArgumentsAndNamesAreLiteralText(t *testing.T) {
	a := &chat.Activity{Title: "t", Steps: []chat.Step{{Title: "<!here>", Status: chat.StepDone, Code: "echo <!channel> *hi* <https://x|y>"}}}
	line := els(cardLines(t, activityBlocks(a)[0])[0][0])
	for i, e := range line {
		if e["type"] != "text" {
			t.Errorf("element %d is a %v, want a literal text element", i, e["type"])
		}
	}
	if got := line[1]["text"]; got != "<!here>" {
		t.Errorf("name = %q, want it verbatim", got)
	}
	if got := line[3]["text"]; got != "echo <!channel> *hi* <https://x|y>" {
		t.Errorf("argument = %q, want it verbatim", got)
	}
}

// The card's block_id is fixed. A card a reader has opened snaps shut on every
// edit unless the block keeps the id it had (measured 2026-11-19), so the id
// must be there and must not depend on what the card says.
func TestTheCardKeepsOneBlockID(t *testing.T) {
	first := activityBlocks(turnActivity())
	second := activityBlocks(&chat.Activity{Title: "Done · 2m · 9 calls", Status: chat.StepDone, Steps: []chat.Step{{Title: "bash ×9", Status: chat.StepDone}}})
	a, _ := first[0]["block_id"].(string)
	b, _ := second[0]["block_id"].(string)
	if a == "" || a != b {
		t.Errorf("block ids = %q and %q, want one non-empty id on both", a, b)
	}
}

// The lines an activity wants seen without a click are a second block under
// the card: one flat bullet list, with a fixed id of its own.
func TestRecentLinesAreASecondBlock(t *testing.T) {
	a := turnActivity()
	a.Recent = []chat.Step{
		{Title: "read_file", Status: chat.StepDone, Code: "deploy/billing.yaml", Note: "0.4s"},
		{Title: "grep", Status: chat.StepRunning, Code: "memory"},
	}
	blocks := activityBlocks(a)
	if len(blocks) != 2 || blocks[0]["type"] != "task_card" {
		t.Fatalf("blocks = %v, want the card and the recent lines", blocks)
	}
	got, err := json.Marshal(blocks[1])
	if err != nil {
		t.Fatal(err)
	}
	want := `{"block_id":"sb-activity-recent","elements":[` +
		list("0",
			line("✔\uFE0E", "read_file", "  0.4s  ", "deploy/billing.yaml"),
			line("▸", "grep", "  ", "memory")) +
		`],"type":"rich_text"}`
	if string(got) != want {
		t.Errorf("recent block =\n%s\nwant\n%s", got, want)
	}
	if blocks[1]["block_id"] == blocks[0]["block_id"] {
		t.Error("the two blocks share an id; they must differ within a message")
	}

	// And it is the same id whatever the lines say.
	a.Recent = []chat.Step{{Title: "bash", Status: chat.StepDone}}
	if again := activityBlocks(a); again[1]["block_id"] != blocks[1]["block_id"] {
		t.Errorf("recent block id changed between renders: %v then %v", blocks[1]["block_id"], again[1]["block_id"])
	}
}

// With nothing recent the message is the card alone: a finished card folds to
// its one title line.
func TestNoRecentLinesIsTheCardAlone(t *testing.T) {
	a := turnActivity()
	for name, recent := range map[string][]chat.Step{"nil": nil, "empty": {}} {
		a.Recent = recent
		if blocks := activityBlocks(a); len(blocks) != 1 {
			t.Errorf("%s recent: %d blocks, want the card alone", name, len(blocks))
		}
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

// A card whatever RichBlocks says, with the text beside it for notifications.
func TestAnActivityIsPostedAsATaskCardWithoutRichBlocks(t *testing.T) {
	s, a := newActivityServer(t, false)
	if a.richBlocks {
		t.Fatal("this test is about the path where rich rendering is off")
	}
	_, err := a.Send(context.Background(), chat.Reply{
		Conversation: "C0:100.5", Text: "⏳ Working… 5s · running `bash` (step 1)", Kind: chat.KindProgress,
		Activity: &chat.Activity{Title: "Working… 5s · 1 call · bash", Status: chat.StepRunning, Steps: []chat.Step{{Title: "bash", Status: chat.StepRunning}}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(s.forms) != 1 {
		t.Fatalf("%d posts, want 1", len(s.forms))
	}
	if blocks := s.forms[0]["blocks"]; len(blocks) != 1 || strings.Count(blocks[0], `"task_card"`) != 1 {
		t.Errorf("blocks = %v, want one task card", blocks)
	}
	if text := s.forms[0]["text"]; len(text) != 1 || !strings.Contains(text[0], "running") {
		t.Errorf("text = %v, want the fallback beside the card", text)
	}
}

// A Slack that refuses the card still gets the message, as text.
func TestARefusedCardPostsAsText(t *testing.T) {
	s, a := newActivityServer(t, true)
	ref, err := a.Send(context.Background(), chat.Reply{
		Conversation: "C0:100.5", Text: "⏳ Working… 5s · running `bash` (step 1)", Kind: chat.KindProgress,
		Activity: &chat.Activity{Title: "Working… 5s · 1 call", Status: chat.StepRunning, Steps: []chat.Step{{Title: "bash", Status: chat.StepRunning}}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ref.ID != "111.111" {
		t.Errorf("ref = %+v, want the text post's", ref)
	}
	if len(s.forms) != 2 {
		t.Fatalf("%d posts, want the refused card and then the text", len(s.forms))
	}
	if blocks := s.forms[1]["blocks"]; len(blocks) != 0 {
		t.Errorf("the retry carried blocks %v, want text alone", blocks)
	}
	if text := s.forms[1]["text"]; len(text) != 1 || !strings.Contains(text[0], "running") {
		t.Errorf("the retry's text = %v, want the reply's", text)
	}
}

func TestAnEditRedrawsTheCard(t *testing.T) {
	s, a := newActivityServer(t, false)
	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	err := a.Update(context.Background(), ref, chat.Reply{
		Conversation: "C0:100.5", Text: "⏳ Working… 30s · running `bash` (step 1)", Kind: chat.KindProgress,
		Activity: &chat.Activity{Title: "Working… 30s · 1 call", Status: chat.StepRunning, Steps: []chat.Step{{Title: "bash", Status: chat.StepDone}}},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if blocks := s.forms[0]["blocks"]; len(blocks) != 1 || !strings.Contains(blocks[0], `"status":"in_progress"`) || !strings.Contains(blocks[0], `"block_id":"sb-activity"`) {
		t.Errorf("blocks = %v, want the redrawn card under its fixed id", blocks)
	}
}

// The edit that retires a plain placeholder carries no Activity; it must take
// a card down, as the text path's explicit empty block set does.
func TestAnEditWithoutActivityTakesTheCardDown(t *testing.T) {
	s, a := newActivityServer(t, false)
	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	if err := a.Update(context.Background(), ref, chat.Reply{Conversation: "C0:100.5", Text: "✔ Done · 40s", Kind: chat.KindActivity}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if blocks := s.forms[0]["blocks"]; len(blocks) != 1 || blocks[0] != "[]" {
		t.Errorf("blocks = %v, want [] so the card comes down", blocks)
	}
}

func TestARefusedCardEditsAsText(t *testing.T) {
	s, a := newActivityServer(t, true)
	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	err := a.Update(context.Background(), ref, chat.Reply{
		Conversation: "C0:100.5", Text: "⏳ Working… 30s", Kind: chat.KindProgress,
		Activity: &chat.Activity{Title: "Working… 30s · 1 call", Steps: []chat.Step{{Title: "bash", Status: chat.StepRunning}}},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(s.forms) != 2 || s.forms[1]["blocks"][0] != "[]" {
		t.Errorf("forms = %v, want the refused card then a text edit", s.forms)
	}
}

// One refusal is the workspace's answer, not the message's: after it, no reply
// tries a card again — each would cost a refused call before the text lands —
// and the adapter stops claiming to draw them, so the router goes back to what
// it does for an adapter that never did.
func TestOneRefusalStopsTheCards(t *testing.T) {
	s, a := newActivityServer(t, true)
	if !a.DrawsActivity() {
		t.Fatal("a fresh adapter does not draw activity")
	}
	reply := chat.Reply{
		Conversation: "C0:100.5", Text: "⏳ Working… 5s · running `bash` (step 1)", Kind: chat.KindProgress,
		Activity: &chat.Activity{Title: "Working… 5s · 1 call", Steps: []chat.Step{{Title: "bash", Status: chat.StepRunning}}},
	}
	if _, err := a.Send(context.Background(), reply); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if a.DrawsActivity() {
		t.Error("still claims to draw activity after Slack refused the card")
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
		t.Fatalf("%d calls, want 4 — a card tried once only", len(s.forms))
	}
	for i, f := range s.forms[1:] {
		if b := f["blocks"]; len(b) > 0 && strings.Contains(b[0], "task_card") {
			t.Errorf("call %d after the refusal still sent a card: %v", i+1, b)
		}
		if text := f["text"]; len(text) != 1 || !strings.Contains(text[0], "running") {
			t.Errorf("call %d after the refusal: text = %v, want the reply's", i+1, text)
		}
	}
}

// The text beside a card is cut on a character. A kept card's text lists every
// call, so it is routinely past the cut, and its marks are multi-byte.
func TestTheTextBesideACardIsCutOnACharacter(t *testing.T) {
	s, a := newActivityServer(t, false)
	long := strings.Repeat("✔ `bash`\n", 400) // 3 bytes of mark at every offset mod 10
	for cut := 0; cut < 3; cut++ {
		text := strings.Repeat("x", cut) + long
		_, err := a.Send(context.Background(), chat.Reply{
			Conversation: "C0:100.5", Text: text, Kind: chat.KindActivity,
			Activity: &chat.Activity{Title: "Done · 5s · 400 calls", Status: chat.StepDone, Steps: []chat.Step{{Title: "bash ×400", Status: chat.StepDone}}},
		})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		got := s.forms[len(s.forms)-1]["text"][0]
		if !utf8.ValidString(got) {
			t.Errorf("offset %d: the text beside the card is not valid UTF-8 (%d bytes)", cut, len(got))
		}
		if len(got) > maxSectionText || len(got) < maxSectionText-3 {
			t.Errorf("offset %d: the text beside the card is %d bytes, want it cut just under %d", cut, len(got), maxSectionText)
		}
	}
	ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
	if err := a.Update(context.Background(), ref, chat.Reply{
		Conversation: "C0:100.5", Text: "xx" + long, Kind: chat.KindActivity,
		Activity: &chat.Activity{Title: "Done", Status: chat.StepDone},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := s.forms[len(s.forms)-1]["text"][0]; !utf8.ValidString(got) || len(got) > maxSectionText {
		t.Errorf("an edit's text beside the card: %d bytes, valid UTF-8 %t", len(got), utf8.ValidString(got))
	}
}

// A refused card goes as its text, in an edit that cannot be split across
// messages, so a long record is cut — and cut on a character, on the plain
// text path and on the RichBlocks one alike.
func TestARefusedCardsTextIsCutOnACharacter(t *testing.T) {
	long := strings.Repeat("✔ `bash`\n", 800) // 8,000 bytes, a 3-byte mark every ten
	for name, rich := range map[string]bool{"text": false, "rich blocks": true} {
		t.Run(name, func(t *testing.T) {
			limit := slackTextLimit
			if rich {
				limit = maxSectionText
			}
			for offset := 0; offset < 3; offset++ {
				s, a := newActivityServer(t, true)
				a.richBlocks = rich
				ref := chat.MessageRef{Conversation: "C0:100.5", ID: "111.111"}
				err := a.Update(context.Background(), ref, chat.Reply{
					Conversation: "C0:100.5", Text: strings.Repeat("x", offset) + long, Kind: chat.KindActivity,
					Activity: &chat.Activity{Title: "Done · 5s · 800 calls", Status: chat.StepDone, Steps: []chat.Step{{Title: "bash ×800", Status: chat.StepDone}}},
				})
				if err != nil {
					t.Fatalf("Update: %v", err)
				}
				if len(s.forms) != 2 {
					t.Fatalf("%d calls, want the refused card and then its text", len(s.forms))
				}
				got := s.forms[1]["text"][0]
				if !utf8.ValidString(got) {
					t.Errorf("offset %d: the fallback text is not valid UTF-8 (%d bytes)", offset, len(got))
				}
				if len(got) > limit || len(got) < limit-3 {
					t.Errorf("offset %d: the fallback text is %d bytes, want it cut just under %d", offset, len(got), limit)
				}
			}
		})
	}
}

// clamp never returns more than it was allowed, never cuts inside a character,
// and leaves alone what already fits.
func TestClampCutsOnACharacter(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"héllo", 2, "h"},  // é is two bytes: the cut would split it
		{"héllo", 3, "hé"}, // and here it does not
		{"✔✔", 4, "✔"},
		{"✔✔", 2, ""},
	} {
		if got := clamp(tc.in, tc.max); got != tc.want {
			t.Errorf("clamp(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

// An activity past the belt goes as its text, and that is not a refusal: the
// next one that fits is still drawn.
func TestAnActivityPastTheBeltPostsAsTextAndRefusesNothing(t *testing.T) {
	s, a := newActivityServer(t, false)
	reply := chat.Reply{
		Conversation: "C0:100.5", Text: "the whole thing, in text", Kind: chat.KindActivity,
		Activity: &chat.Activity{Title: "Done", Status: chat.StepDone, Steps: make([]chat.Step, maxTaskLines+1)},
	}
	if _, err := a.Send(context.Background(), reply); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(s.forms) != 1 {
		t.Fatalf("%d posts, want one", len(s.forms))
	}
	if b := s.forms[0]["blocks"]; len(b) > 0 && strings.Contains(b[0], "task_card") {
		t.Errorf("an activity past the belt was sent as a card: %d bytes of blocks", len(b[0]))
	}
	if text := s.forms[0]["text"]; len(text) != 1 || text[0] != "the whole thing, in text" {
		t.Errorf("text = %v, want the reply's", text)
	}
	if !a.DrawsActivity() {
		t.Error("an oversized activity stopped the cards; only a refusal should")
	}
}
