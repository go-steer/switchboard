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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/switchboard/internal/logging"
	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// Card mode end to end: a router, a scripted daemon, and an adapter that draws
// Activity. What the record stores and how it renders is card_test.go's; these
// are the paths that feed it, edit it, and decide what becomes of it.

// cardFake is a sender that draws Activity, as the Slack adapter does, and
// keeps what each message was last told to say — a thread, as a reader of it
// would see it.
type cardFake struct {
	draws atomic.Bool

	mu      sync.Mutex
	n       int
	order   []string              // message ids, in the order they were posted
	posted  map[string]chat.Reply // what each message was posted as
	edits   map[string][]chat.Reply
	deleted map[string]bool
	// attempts counts the edits tried on each message, failed ones included.
	attempts map[string]int
	// failEdit, when set, decides whether an edit fails; failDelete, whether
	// a delete does.
	failEdit   func(chat.MessageRef, chat.Reply) error
	failDelete func(chat.MessageRef) error
	// editDelay is how long each edit takes to reach the platform.
	editDelay time.Duration
	// onSend, when set, runs as a post is about to be made, outside the
	// fake's lock: what happens in the thread while a post is in the air.
	onSend func(chat.Reply)
	// refuseCards makes the fake behave as Slack does when it refuses a
	// card: the reply goes as its text, and it stops drawing activity.
	refuseCards bool
}

func newCardFake() *cardFake {
	f := &cardFake{posted: make(map[string]chat.Reply), edits: make(map[string][]chat.Reply), deleted: make(map[string]bool), attempts: make(map[string]int)}
	f.draws.Store(true)
	return f
}

func (f *cardFake) DrawsActivity() bool { return f.draws.Load() }

// refuse is the adapter's latch: a card it is asked to draw while refusing
// goes as text, and it draws no more.
func (f *cardFake) refuse(r chat.Reply) chat.Reply {
	if r.Activity != nil && f.refuseCards {
		f.draws.Store(false)
	}
	if !f.draws.Load() {
		r.Activity = nil
	}
	return r
}

func (f *cardFake) Send(ctx context.Context, r chat.Reply) (chat.MessageRef, error) {
	if err := ctx.Err(); err != nil {
		return chat.MessageRef{}, err // as a real platform call would
	}
	f.mu.Lock()
	hook := f.onSend
	f.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("m%d", f.n)
	f.order = append(f.order, id)
	f.posted[id] = f.refuse(r)
	return chat.MessageRef{Conversation: r.Conversation, ID: id}, nil
}

func (f *cardFake) Update(ctx context.Context, ref chat.MessageRef, r chat.Reply) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	delay := f.editDelay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[ref.ID]++
	if f.failEdit != nil {
		if err := f.failEdit(ref, r); err != nil {
			return err
		}
	}
	f.edits[ref.ID] = append(f.edits[ref.ID], f.refuse(r))
	return nil
}

func (f *cardFake) Delete(_ context.Context, ref chat.MessageRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete != nil {
		if err := f.failDelete(ref); err != nil {
			return err
		}
	}
	f.deleted[ref.ID] = true
	return nil
}

// tried is how many edits were attempted on message id, failed ones included.
func (f *cardFake) tried(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[id]
}

// setHooks installs the fake's failure hooks.
func (f *cardFake) setHooks(failEdit func(chat.MessageRef, chat.Reply) error, failDelete func(chat.MessageRef) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failEdit, f.failDelete = failEdit, failDelete
}

// postedReply is what message id was posted as.
func (f *cardFake) postedReply(id string) chat.Reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posted[id]
}

// spinning lists the messages still standing that draw something as running:
// a card whose status is running, or a line in one.
func (f *cardFake) spinning() []string {
	var out []string
	for _, id := range f.ids() {
		if r, ok := f.now(id); ok && r.Activity != nil && (r.Activity.Status == chat.StepRunning || anyRunning(r.Activity.Steps)) {
			out = append(out, id)
		}
	}
	return out
}

// now is what message id says at this moment: its latest edit, or what it was
// posted as. ok is false for a message that was never posted or was deleted.
func (f *cardFake) now(id string) (chat.Reply, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.posted[id]; !ok || f.deleted[id] {
		return chat.Reply{}, false
	}
	if e := f.edits[id]; len(e) > 0 {
		return e[len(e)-1], true
	}
	return f.posted[id], true
}

// thread is every message still standing, in the order posted.
func (f *cardFake) thread() []chat.Reply {
	f.mu.Lock()
	ids := append([]string(nil), f.order...)
	f.mu.Unlock()
	var out []chat.Reply
	for _, id := range ids {
		if r, ok := f.now(id); ok {
			out = append(out, r)
		}
	}
	return out
}

// ids are the ids of every message posted, deleted or not, in order.
func (f *cardFake) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func (f *cardFake) editsTo(id string) []chat.Reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chat.Reply(nil), f.edits[id]...)
}

func (f *cardFake) editCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.edits {
		n += len(e)
	}
	return n
}

func (f *cardFake) wasDeleted(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleted[id]
}

func (f *cardFake) anyDeleted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deleted) > 0
}

// postedAs finds the first message posted with exactly this text.
func (f *cardFake) postedAs(text string) (id string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		if f.posted[id].Text == text {
			return id, true
		}
	}
	return "", false
}

const cardConv = "C0:100.1"

// cardRig is a router in card mode — the adapter draws Activity — wired to the
// subagent rig's scripted daemon, for the tests that need a subagent's turn
// log. Redraws are coalesced over a millisecond, so a test sees an edit as
// soon as there is something to draw.
//
// That rig's daemon does not say when a turn ends, so the first text is the
// answer and ends the turn itself. The tests about boundaries and narration
// use cardFed.
func cardRig(t *testing.T, mode ProgressMode) (*Router, *cardFake, *subagentRig, context.Context) {
	t.Helper()
	router, _, rig := newSubagentRig(t, mode)
	fake := newCardFake()
	router.out = fake
	router.cardRedraw = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return router, fake, rig, ctx
}

// cardFeed is a daemon's event stream, written by the test a frame at a time.
type cardFeed chan frame

func (f cardFeed) sendAgent(data string)          { f <- frame{daemon.EventAgent, data} }
func (f cardFeed) sendLifecycle(typ, data string) { f <- frame{typ, data} }

// cardFed is a router in card mode on a daemon that signals the end of a turn
// (turn-complete), so a text that arrives mid-turn is narration until the
// boundary says otherwise.
//
// The re-anchor that follows narration is put off for an hour, which is to
// say never: a test that sends an answer and then its boundary gets the
// boundary first, as the real delay arranges, and the tests about narration
// itself set the delay to zero.
func cardFed(t *testing.T, mode ProgressMode) (*Router, *cardFake, cardFeed, context.Context) {
	t.Helper()
	feed := make(cardFeed)
	fake := newCardFake()
	router := NewRouter(fedDaemon(t, feed), fake, mode, nil, nil)
	router.minBackoff, router.maxBackoff = 5*time.Millisecond, 10*time.Millisecond
	router.streamGrace = time.Hour
	router.cardRedraw = time.Millisecond
	router.reanchorDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	// Before the daemon's own cleanup, which waits for its stream handler:
	// closing the feed is what lets that handler go.
	t.Cleanup(func() {
		cancel()
		close(feed)
	})
	return router, fake, feed, ctx
}

// ask sends a message and waits for its placeholder, returning its id.
func ask(t *testing.T, r *Router, ctx context.Context, fake *cardFake) string {
	t.Helper()
	before := len(fake.ids())
	if err := r.Handle(ctx, chat.Message{Conversation: cardConv, Channel: "C0", Caller: "alice@example.com", Text: "ping"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ids := fake.ids()
	if len(ids) != before+1 {
		t.Fatalf("Handle posted %d messages, want the placeholder alone", len(ids)-before)
	}
	id := ids[before]
	if got, _ := fake.now(id); got.Text != workingText || got.Kind != chat.KindProgress || got.Activity != nil {
		t.Fatalf("placeholder = %+v, want the plain clock with no Activity", got)
	}
	return id
}

func callEvent(seq int, id, name, arg string) string {
	return fmt.Sprintf(`{"seq":%d,"event":{"Content":{"parts":[{"functionCall":{"id":%q,"name":%q,"args":{"command":%q}}}],"role":"model"},"Partial":false}}`, seq, id, name, arg)
}

func resultEvent(seq int, id, name string) string {
	return fmt.Sprintf(`{"seq":%d,"event":{"Content":{"parts":[{"functionResponse":{"id":%q,"name":%q,"response":{"exit_code":0}}}],"role":"user"}}}`, seq, id, name)
}

func failedEvent(seq int, id, name string) string {
	return fmt.Sprintf(`{"seq":%d,"event":{"Content":{"parts":[{"functionResponse":{"id":%q,"name":%q,"response":{"exit_code":2}}}],"role":"user"}}}`, seq, id, name)
}

func textEvent(seq int, text string) string {
	return fmt.Sprintf(`{"seq":%d,"event":{"Content":{"parts":[{"text":%q}],"role":"model"},"Partial":false}}`, seq, text)
}

const turnDone = `{"model":"gemini","latency_ms":1200}`

// waitCard waits until message id says something cond accepts, and returns it.
func waitCard(t *testing.T, fake *cardFake, id string, what string, cond func(chat.Reply) bool) chat.Reply {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, ok := fake.now(id)
		if ok && cond(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("message %s never showed %s; it says %q %+v (still standing: %t)", id, what, got.Text, got.Activity, ok)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func hasTitle(re string) func(chat.Reply) bool {
	rx := regexp.MustCompile(re)
	return func(r chat.Reply) bool { return r.Activity != nil && rx.MatchString(r.Activity.Title) }
}

// waitPosted waits for a message posted with exactly this text.
func waitPosted(t *testing.T, fake *cardFake, text string) string {
	t.Helper()
	var id string
	waitFor(t, func() bool {
		var ok bool
		id, ok = fake.postedAs(text)
		return ok
	}, fmt.Sprintf("%q was never posted", text))
	return id
}

// The predicate, in one place: status and stream, and only for an adapter
// that draws activity.
func TestCardModeIsStatusAndStreamWhereTheAdapterDraws(t *testing.T) {
	fake := newCardFake()
	r := NewRouter(nil, fake, ProgressStatus, nil, nil)
	for mode, want := range map[ProgressMode]bool{
		ProgressStatus: true, ProgressStream: true, ProgressIndicator: false, ProgressOff: false,
	} {
		if got := r.cardMode(mode); got != want {
			t.Errorf("cardMode(%s) with an adapter that draws = %t, want %t", mode, got, want)
		}
	}
	fake.draws.Store(false) // the refusal latch
	for _, mode := range progressModes {
		if r.cardMode(mode) {
			t.Errorf("cardMode(%s) once the adapter stopped drawing = true", mode)
		}
	}
	plain := NewRouter(nil, &fakeSender{replies: make(chan chat.Reply, 1)}, ProgressStatus, nil, nil)
	for _, mode := range progressModes {
		if plain.cardMode(mode) {
			t.Errorf("cardMode(%s) for an adapter that draws nothing = true", mode)
		}
	}
}

// subagentRun drives a turn that delegates once to _fallback, whose subagent
// makes n calls, each to a tool of its own name with an argument of its own,
// and then answers.
func subagentRun(t *testing.T, mode ProgressMode, n int) (card string, fake *cardFake) {
	t.Helper()
	router, fake, rig, ctx := cardRig(t, mode)
	card = ask(t, router, ctx, fake)

	seq := int64(2)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("f%02d", i)
		rig.addSubEvent("_fallback", seq, fmt.Sprintf(
			`{"seq":%d,"event":{"Content":{"parts":[{"functionCall":{"id":%q,"name":"tool_%02d","args":{"command":"arg-%02d"}}}],"role":"model"},"Partial":false}}`, seq, id, i, i))
		seq++
		rig.addSubEvent("_fallback", seq, fmt.Sprintf(
			`{"seq":%d,"event":{"Content":{"parts":[{"functionResponse":{"id":%q,"name":"tool_%02d","response":{"ok":true}}}],"role":"user"}}}`, seq, id, i))
		seq++
	}
	// And the loop-control call a subagent ends on, which is the driver's and
	// not something the agent ran: it is not a line and not counted.
	rig.addSubEvent("_fallback", seq, fmt.Sprintf(
		`{"seq":%d,"event":{"Content":{"parts":[{"functionCall":{"id":"syn","name":"return_result","args":{"result":"done"}}}],"role":"model"},"Partial":false}}`, seq))
	seq++
	rig.sendAgent(callEvent(1, "p1", "_fallback", "check billing"))
	waitCard(t, fake, card, "the subagent's calls", func(r chat.Reply) bool {
		return r.Activity != nil && len(r.Activity.Steps) == 1 && len(r.Activity.Steps[0].Steps) == n
	})
	rig.sendAgent(resultEvent(int(seq), "p1", "_fallback"))
	rig.sendAgent(textEvent(int(seq)+1, "the answer"))
	rig.sendLifecycle(daemon.EventTurnComplete, turnDone)
	waitPosted(t, fake, "the answer")
	waitCard(t, fake, card, "its final state", hasTitle(`^Done · `))
	return card, fake
}

// A 60-call subagent run in stream ends with all 60 calls in the kept card,
// each with its argument, under the call that delegated to them — and nothing
// else in the thread: no notice per frame, no notice per subagent call.
func TestAStreamCardKeepsEveryCallOfALongSubagentRun(t *testing.T) {
	card, fake := subagentRun(t, ProgressStream, 60)

	kept, _ := fake.now(card)
	if kept.Kind != chat.KindActivity || kept.Activity.Status != chat.StepDone {
		t.Errorf("kept card is %s / %s, want a finished activity", kept.Kind, kept.Activity.Status)
	}
	if !regexp.MustCompile(`^Done · \d+s · 61 calls$`).MatchString(kept.Activity.Title) {
		t.Errorf("title = %q, want the turn's 61 calls: the 60 and the delegation once", kept.Activity.Title)
	}
	if kept.Activity.Recent != nil {
		t.Errorf("a finished card carries recent lines: %+v", kept.Activity.Recent)
	}
	if len(kept.Activity.Steps) != 1 {
		t.Fatalf("steps = %s, want the delegation alone at the top", titles(kept.Activity.Steps))
	}
	parent := kept.Activity.Steps[0]
	if parent.Title != "_fallback" || parent.Status != chat.StepDone || parent.Calls != 60 || parent.Code != "check billing" || parent.Note == "" {
		t.Errorf("delegation = %+v, want _fallback done, counting 60, with its argument and how long it took", parent)
	}
	if len(parent.Steps) != 60 {
		t.Fatalf("%d calls under the delegation, want all 60", len(parent.Steps))
	}
	for i, s := range parent.Steps {
		if want := fmt.Sprintf("tool_%02d", i); s.Title != want || s.Status != chat.StepDone || s.Code != fmt.Sprintf("arg-%02d", i) || s.Note == "" {
			t.Errorf("call %d = %+v, want %s done with its argument and a duration", i, s, want)
		}
	}
	// The text beside the card says the whole thing too.
	if !strings.HasPrefix(kept.Text, markOK+" Done · ") || strings.Count(kept.Text, "\n") != 61 ||
		!strings.Contains(kept.Text, "  ↳ "+markOK+" `tool_59`") || !strings.Contains(kept.Text, "`arg-59`") {
		t.Errorf("kept text does not list the calls:\n%s", kept.Text)
	}

	for _, e := range append(fake.editsTo(card), kept) {
		if strings.Contains(e.Text, "return_result") || (e.Activity != nil && strings.Contains(titles(e.Activity.Steps)+e.Activity.Title, "return_result")) {
			t.Fatalf("the subagent's loop-control call was drawn: %q %+v", e.Text, e.Activity)
		}
	}
	if fake.wasDeleted(card) {
		t.Error("the card was deleted")
	}
	thread := fake.thread()
	if len(thread) != 2 || thread[1].Text != "the answer" {
		var texts []string
		for _, m := range thread {
			texts = append(texts, m.Text)
		}
		t.Errorf("thread = %q, want the card and the answer and nothing else", texts)
	}
	// While it ran, stream showed the latest lines under the card.
	sawRecent := false
	for _, e := range fake.editsTo(card) {
		if e.Activity != nil && e.Activity.Status == chat.StepRunning && len(e.Activity.Recent) > 0 {
			sawRecent = true
			if len(e.Activity.Recent) > cardRecentLines {
				t.Errorf("%d recent lines, want at most %d", len(e.Activity.Recent), cardRecentLines)
			}
		}
	}
	if !sawRecent {
		t.Error("no running edit of a stream card carried recent lines")
	}
}

// The same run in status: all 60 names and the right count, and no argument or
// duration anywhere — not in the card, not in the text beside it.
func TestAStatusCardKeepsEveryNameAndNoArguments(t *testing.T) {
	card, fake := subagentRun(t, ProgressStatus, 60)

	kept, _ := fake.now(card)
	if !regexp.MustCompile(`^Done · \d+s · 61 calls$`).MatchString(kept.Activity.Title) {
		t.Errorf("title = %q, want 61 calls", kept.Activity.Title)
	}
	parent := kept.Activity.Steps[0]
	if parent.Title != "_fallback" || parent.Calls != 60 || len(parent.Steps) != 60 {
		t.Fatalf("delegation = %q counting %d with %d calls under it, want _fallback and all 60", parent.Title, parent.Calls, len(parent.Steps))
	}
	for i, s := range parent.Steps {
		if s.Title != fmt.Sprintf("tool_%02d", i) || s.Status != chat.StepDone {
			t.Errorf("call %d = %+v", i, s)
		}
	}
	// Every edit the card ever had, not only the last.
	for _, e := range append(fake.editsTo(card), kept) {
		if strings.Contains(e.Text, "arg-") || strings.Contains(e.Text, "check billing") {
			t.Fatalf("status text carries an argument: %q", e.Text)
		}
		if e.Activity == nil {
			continue
		}
		if e.Activity.Recent != nil {
			t.Fatalf("a status card carries recent lines: %+v", e.Activity.Recent)
		}
		for _, s := range append([]chat.Step{e.Activity.Steps[0]}, e.Activity.Steps[0].Steps...) {
			if s.Code != "" || s.Note != "" {
				t.Fatalf("a status step carries detail: %+v", s)
			}
		}
	}
	if fake.wasDeleted(card) {
		t.Error("the card was deleted")
	}
}

// The title names the tool a subagent is running, under the call that
// delegated to it, and stops naming it when the delegation returns. _fallback
// keeps its underscore in the title and on its line.
func TestTheCardTitleFollowsTheSubagent(t *testing.T) {
	for _, mode := range []ProgressMode{ProgressStatus, ProgressStream} {
		t.Run(string(mode), func(t *testing.T) {
			router, fake, rig, ctx := cardRig(t, mode)
			card := ask(t, router, ctx, fake)

			rig.addSubEvent("_fallback", 2, `{"seq":2,"event":{"Content":{"parts":[{"functionCall":{"id":"f1","name":"get_k8s_pods","args":{"namespace":"billing"}}}],"role":"model"},"Partial":false}}`)
			rig.sendAgent(callEvent(1, "p1", "_fallback", "check billing"))
			running := waitCard(t, fake, card, "the subagent's tool in its title", hasTitle(`^Working… \d+s · 2 calls · _fallback › get_k8s_pods$`))
			if got := running.Activity.Steps[0].Title; got != "_fallback" {
				t.Errorf("the delegation's line reads %q, want _fallback with its underscore", got)
			}
			if running.Activity.Status != chat.StepRunning {
				t.Errorf("a running card's status = %s", running.Activity.Status)
			}
			// The text beside the card is the line status mode has always written.
			if !strings.Contains(running.Text, "running `_fallback` › `get_k8s_pods` (call 1)") {
				t.Errorf("text beside the card = %q, want the status line", running.Text)
			}

			rig.addSubEvent("_fallback", 3, `{"seq":3,"event":{"Content":{"parts":[{"functionResponse":{"id":"f1","name":"get_k8s_pods","response":{"pods":3}}}],"role":"user"}}}`)
			rig.sendAgent(resultEvent(4, "p1", "_fallback"))
			after := waitCard(t, fake, card, "a title with no subagent tool", hasTitle(`^Working… \d+s · 2 calls$`))
			if anyRunning(after.Activity.Steps) {
				t.Errorf("steps after the delegation returned = %+v, want none running", after.Activity.Steps)
			}
			if strings.Contains(after.Text, "get_k8s_pods") {
				t.Errorf("text after the delegation returned still names the subagent's tool: %q", after.Text)
			}
		})
	}
}

// A card past its line limit is frozen where it is and the turn carries on in
// a new message: every call stays in the thread exactly once, a frozen message
// shows nothing running, and the last card counts the whole turn.
func TestALongCardRollsOver(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStream)
	router.cardMaxLines = 4
	first := ask(t, router, ctx, fake)

	const n = 13
	seq := 1
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("tool_%02d", i)
		rig.sendAgent(callEvent(seq, name, name, fmt.Sprintf("arg-%02d", i)))
		seq++
		if i == 5 {
			continue // one call left unanswered across the rollovers
		}
		rig.sendAgent(resultEvent(seq, name, name))
		seq++
		// Let the card draw now and then, so there is something to freeze
		// more than once whatever the scheduler does.
		if i%4 == 3 {
			want := name
			waitFor(t, func() bool {
				for _, m := range fake.thread() {
					if m.Activity != nil && strings.Contains(titles(m.Activity.Steps), want) {
						return true
					}
				}
				return false
			}, "call "+want+" was never drawn")
		}
	}
	rig.sendAgent(resultEvent(seq, "tool_05", "tool_05"))
	rig.sendAgent(textEvent(seq+1, "the answer"))
	rig.sendLifecycle(daemon.EventTurnComplete, turnDone)
	waitPosted(t, fake, "the answer")
	waitFor(t, func() bool {
		for _, m := range fake.thread() {
			if m.Activity != nil && strings.HasPrefix(m.Activity.Title, "Done · ") {
				return true
			}
		}
		return false
	}, "no card was finalised")

	if fake.anyDeleted() {
		t.Errorf("a message was deleted during a rollover")
	}
	var cards []chat.Reply
	for _, m := range fake.thread() {
		if m.Activity != nil {
			cards = append(cards, m)
		}
	}
	if len(cards) < 2 {
		t.Fatalf("%d cards in the thread, want the turn rolled over into several", len(cards))
	}
	if got, _ := fake.now(first); got.Activity == nil || got.Activity.Status != chat.StepDone || !regexp.MustCompile(`^\d+ calls? · \d+s$`).MatchString(got.Activity.Title) {
		t.Errorf("the first card = %+v, want it frozen as a finished stretch", got.Activity)
	}
	seen := make(map[string]int)
	for i, c := range cards {
		a := c.Activity
		if anyRunning(a.Steps) || a.Status == chat.StepRunning {
			t.Errorf("card %d (%q) shows something running: %s", i, a.Title, titles(a.Steps))
		}
		if a.Recent != nil {
			t.Errorf("card %d (%q) still carries recent lines", i, a.Title)
		}
		if c.Kind != chat.KindActivity {
			t.Errorf("card %d is a %s message, want a finished activity", i, c.Kind)
		}
		for _, s := range a.Steps {
			seen[s.Title]++
			if !strings.Contains(c.Text, "`"+s.Title+"`") {
				t.Errorf("card %d's text does not list %s", i, s.Title)
			}
			// Each was ticked off by its own result — the one that stayed
			// unanswered across the rollovers too, which is what staying
			// addressable means: a call closed for want of one has no
			// duration to show.
			if s.Status != chat.StepDone || s.Note == "" {
				t.Errorf("card %d: %s = %s with duration %q, want it done by its own result", i, s.Title, s.Status, s.Note)
			}
		}
		last := i == len(cards)-1
		if last && !regexp.MustCompile(fmt.Sprintf(`^Done · \d+s · %d calls$`, n)).MatchString(a.Title) {
			t.Errorf("the last card's title = %q, want the whole turn's %d calls", a.Title, n)
		}
		if !last && stepLines(a.Steps) > router.cardMaxLines {
			t.Errorf("frozen card %d holds %d lines, want at most the limit of %d", i, stepLines(a.Steps), router.cardMaxLines)
		}
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("tool_%02d", i)
		if seen[name] != 1 {
			t.Errorf("%s is in the thread %d times, want exactly once", name, seen[name])
		}
	}
	if len(seen) != n {
		t.Errorf("the thread holds %d distinct calls, want %d", len(seen), n)
	}
}

// A delegation still in flight when its card rolls over heads both messages:
// its finished calls stay behind under its line, and it carries on in front
// with the ones still to finish, still counting all its calls.
func TestARolloverSplitsADelegationInFlight(t *testing.T) {
	router, fake, rig, ctx := cardRig(t, ProgressStream)
	router.cardMaxLines = 4
	first := ask(t, router, ctx, fake)

	add := func(seq int64, part string) {
		rig.addSubEvent("_fallback", seq, fmt.Sprintf(`{"seq":%d,"event":{"Content":{"parts":[%s],"role":"model"},"Partial":false}}`, seq, part))
	}
	sub := func(i int) string {
		return fmt.Sprintf(`{"functionCall":{"id":"k%d","name":"tool_%d","args":{"command":"a%d"}}}`, i, i, i)
	}
	subDone := func(i int) string {
		return fmt.Sprintf(`{"functionResponse":{"id":"k%d","name":"tool_%d","response":{"ok":true}}}`, i, i)
	}
	rig.sendAgent(callEvent(1, "p1", "_fallback", "check billing"))
	for i := 0; i < 5; i++ {
		add(int64(2+2*i), sub(i))
		add(int64(3+2*i), subDone(i))
	}
	add(20, sub(9)) // and one still running
	waitFor(t, func() bool { return len(fake.ids()) >= 2 }, "the card never rolled over")

	frozen := waitCard(t, fake, first, "its frozen state", func(r chat.Reply) bool {
		return r.Activity != nil && r.Activity.Status == chat.StepDone
	})
	if len(frozen.Activity.Steps) != 1 || frozen.Activity.Steps[0].Title != "_fallback" || anyRunning(frozen.Activity.Steps) {
		t.Fatalf("frozen = %s, want the delegation's line over its finished calls, nothing running", titles(frozen.Activity.Steps))
	}
	// The delegation has not returned, and this message will not be edited
	// when it does: its line here is unfinished, not a check. It carries on
	// below, so the title does not count it.
	if got := frozen.Activity.Steps[0].Status; got != chat.StepUnfinished {
		t.Errorf("the frozen heading line = %s, want unfinished", got)
	}
	for _, s := range frozen.Activity.Steps[0].Steps {
		if s.Status != chat.StepDone {
			t.Errorf("frozen call %s = %s, want its own verdict", s.Title, s.Status)
		}
	}
	if strings.Contains(frozen.Activity.Title, "unfinished") || !strings.Contains(frozen.Text, "\n– `_fallback`") {
		t.Errorf("frozen title %q, text %q: want no unfinished count and the neutral mark", frozen.Activity.Title, frozen.Text)
	}
	next := fake.ids()[1]
	cont := waitCard(t, fake, next, "the delegation carried on", func(r chat.Reply) bool {
		return r.Activity != nil && len(r.Activity.Steps) == 1 && strings.HasSuffix(titles(r.Activity.Steps), "tool_9]")
	})
	head := cont.Activity.Steps[0]
	if head.Title != "_fallback" || head.Status != chat.StepRunning || head.Calls != 6 {
		t.Errorf("the carried delegation = %+v, want _fallback still running and counting all 6 of its calls", head)
	}
	if !strings.HasSuffix(cont.Activity.Title, " · 7 calls · _fallback › tool_9") {
		t.Errorf("the continuation's title = %q, want the whole turn's count and what is running", cont.Activity.Title)
	}
	// Between them the two messages hold each of the subagent's calls once.
	seen := make(map[string]int)
	for _, a := range []*chat.Activity{frozen.Activity, cont.Activity} {
		for _, s := range a.Steps[0].Steps {
			seen[s.Title]++
		}
	}
	for _, i := range []int{0, 1, 2, 3, 4, 9} {
		if name := fmt.Sprintf("tool_%d", i); seen[name] != 1 {
			t.Errorf("%s is drawn %d times across the two messages, want once", name, seen[name])
		}
	}

	// The carried calls are still addressable: their results land on the new
	// card — and the delegation's real verdict, a failure, lands on its line
	// there.
	add(21, subDone(9))
	rig.sendAgent(failedEvent(30, "p1", "_fallback"))
	rig.sendAgent(textEvent(31, "the answer"))
	rig.sendLifecycle(daemon.EventTurnComplete, turnDone)
	done := waitCard(t, fake, next, "its final state", hasTitle(`^Done · \d+s · 7 calls \(1 failed\)$`))
	if anyRunning(done.Activity.Steps) || done.Activity.Steps[0].Steps[len(done.Activity.Steps[0].Steps)-1].Note == "" {
		t.Errorf("final = %+v, want the carried call ticked off by its own result", done.Activity.Steps)
	}
	if line := done.Activity.Steps[0]; line.Title != "_fallback (exit 2)" || line.Status != chat.StepFailed {
		t.Errorf("the continuation's delegation line = %+v, want the delegation's own failure", line)
	}
	if still, _ := fake.now(first); still.Activity.Steps[0].Status != chat.StepUnfinished {
		t.Errorf("the frozen heading line became %s once the delegation returned; that message is not edited again", still.Activity.Steps[0].Status)
	}
	if fake.anyDeleted() {
		t.Error("a message was deleted")
	}
}

// The same when narration splits the card under a delegation still out: its
// heading line in the frozen card is unfinished, and the line in the card the
// turn continues in follows the delegation to its real verdict.
func TestANarrationSplitLeavesADelegationsLineUnfinished(t *testing.T) {
	router, fake, rig, ctx := cardRig(t, ProgressStream)
	first := ask(t, router, ctx, fake)
	e := entryFor(t, router)
	// This rig's daemon does not say it signals the end of a turn; say so
	// for it, so that text arriving mid-turn is narration and not the answer.
	e.signalsEnd.Store(true)

	sub := func(seq int64, part string) {
		rig.addSubEvent("_fallback", seq, fmt.Sprintf(`{"seq":%d,"event":{"Content":{"parts":[%s],"role":"model"},"Partial":false}}`, seq, part))
	}
	sub(2, `{"functionCall":{"id":"k1","name":"tool_1","args":{"command":"a"}}}`)
	sub(3, `{"functionResponse":{"id":"k1","name":"tool_1","response":{"ok":true}}}`)
	sub(4, `{"functionCall":{"id":"k2","name":"tool_2","args":{"command":"b"}}}`)
	rig.sendAgent(callEvent(1, "p1", "_fallback", "check billing"))
	waitCard(t, fake, first, "the subagent's calls", func(r chat.Reply) bool {
		return r.Activity != nil && titles(r.Activity.Steps) == "_fallback[tool_1 tool_2]" && r.Activity.Steps[0].Steps[0].Status == chat.StepDone
	})

	rig.sendAgent(textEvent(50, "still looking"))
	waitPosted(t, fake, "still looking")
	frozen := waitCard(t, fake, first, "its frozen state", hasTitle(`^1 call · \d+s$`))
	if got, want := titles(frozen.Activity.Steps), "_fallback[tool_1]"; got != want {
		t.Fatalf("frozen = %s, want %s", got, want)
	}
	if got := statuses(frozen.Activity.Steps); got[0] != chat.StepUnfinished || got[1] != chat.StepDone {
		t.Errorf("frozen statuses = %v, want the delegation's heading line unfinished over its finished call", got)
	}
	var next string
	waitFor(t, func() bool {
		next = progressID(e)
		return next != "" && next != first
	}, "the turn did not continue in a fresh message")
	cont := waitCard(t, fake, next, "the delegation carried on", func(r chat.Reply) bool {
		return r.Activity != nil && titles(r.Activity.Steps) == "_fallback[tool_2]"
	})
	if got := statuses(cont.Activity.Steps); got[0] != chat.StepRunning || got[1] != chat.StepRunning {
		t.Errorf("continuation statuses = %v, want the delegation and its call still running", got)
	}

	sub(51, `{"functionResponse":{"id":"k2","name":"tool_2","response":{"ok":true}}}`)
	rig.sendAgent(resultEvent(60, "p1", "_fallback"))
	rig.sendLifecycle(daemon.EventTurnComplete, turnDone)
	done := waitCard(t, fake, next, "its final state", hasTitle(`^Done · \d+s · 3 calls$`))
	if line := done.Activity.Steps[0]; line.Title != "_fallback" || line.Status != chat.StepDone || line.Note == "" {
		t.Errorf("the continuation's delegation line = %+v, want it done by the delegation's own result", line)
	}
	if still, _ := fake.now(first); still.Activity.Steps[0].Status != chat.StepUnfinished {
		t.Errorf("the frozen heading line became %s; that message is not edited again", still.Activity.Steps[0].Status)
	}
	if spin := fake.spinning(); len(spin) != 0 {
		t.Errorf("messages %v still draw something running", spin)
	}
}

// warnings is a log sink's WARN lines containing sub.
func warnings(sink *logSink, sub string) []string {
	var out []string
	for _, l := range sink.at(sub) {
		if l.level == logging.LevelWarn {
			out = append(out, l.text)
		}
	}
	return out
}

// A card moved below narration is posted again and the old one deleted. If
// the delete fails, the old one was last drawn running and nothing would edit
// it again: it is finished in place instead. If that fails too, the log names
// the message — a spinner that never stops is the one thing a card must not
// become, and the operator is the only one left who can remove it.
func TestAMovedCardThatCannotBeDeletedIsFinishedInPlace(t *testing.T) {
	for name, alsoFailEdit := range map[string]bool{"the edit lands": false, "the edit fails too": true} {
		t.Run(name, func(t *testing.T) {
			router, fake, feed, ctx := cardFed(t, ProgressStream)
			router.reanchorDelay = 0
			sink := &logSink{}
			router.logf = sink.logf
			first := ask(t, router, ctx, fake)
			e := entryFor(t, router)

			// One call, still running: nothing to freeze, so narration
			// moves the card whole.
			feed.sendAgent(callEvent(1, "a", "bash", "one"))
			waitCard(t, fake, first, "its call", hasTitle(` · 1 call · bash$`))
			fake.mu.Lock()
			fake.failDelete = func(chat.MessageRef) error { return errors.New("delete refused") }
			if alsoFailEdit {
				fake.failEdit = func(ref chat.MessageRef, _ chat.Reply) error {
					if ref.ID == first {
						return errors.New("edit refused")
					}
					return nil
				}
			}
			fake.mu.Unlock()

			feed.sendAgent(textEvent(2, "still going"))
			waitPosted(t, fake, "still going")
			var next string
			waitFor(t, func() bool {
				next = progressID(e)
				return next != "" && next != first
			}, "the card was not moved")
			if moved, _ := fake.now(next); moved.Activity == nil || titles(moved.Activity.Steps) != "bash" {
				t.Fatalf("moved card = %+v, want the running call drawn again", moved.Activity)
			}

			if alsoFailEdit {
				waitFor(t, func() bool { return len(warnings(sink, "it is left drawn as running")) == 1 },
					"nothing told the operator a card was left spinning")
				warn := warnings(sink, "it is left drawn as running")[0]
				if !strings.Contains(warn, cardConv) || !strings.Contains(warn, "message "+first+" ") {
					t.Errorf("warning = %q, want it to name the conversation and the message", warn)
				}
				return
			}
			left := waitCard(t, fake, first, "a finished state", func(r chat.Reply) bool {
				return r.Activity != nil && r.Activity.Status != chat.StepRunning
			})
			if left.Activity.Status != chat.StepDone || anyRunning(left.Activity.Steps) || left.Kind != chat.KindActivity {
				t.Errorf("the card left behind = %+v (%s), want it finished with nothing running", left.Activity, left.Kind)
			}
			if got := statuses(left.Activity.Steps); len(got) != 1 || got[0] != chat.StepUnfinished {
				t.Errorf("its call = %v, want unfinished: it carries on in the moved card", got)
			}
			if !regexp.MustCompile(`^1 call · \d+s · 1 unfinished$`).MatchString(left.Activity.Title) {
				t.Errorf("its title = %q", left.Activity.Title)
			}
			if len(warnings(sink, "could not be deleted; finished it in place")) != 1 {
				t.Errorf("the fallback was not logged: %v", sink.matching("progress"))
			}
			if len(warnings(sink, "it is left drawn as running")) != 0 {
				t.Errorf("a card that was finished was reported as spinning")
			}

			// The turn carries on in the moved card, and when it ends
			// nothing anywhere is left running.
			feed.sendAgent(resultEvent(3, "a", "bash"))
			feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
			waitCard(t, fake, next, "its final state", hasTitle(`^Done · \d+s · 1 call$`))
			if spin := fake.spinning(); len(spin) != 0 {
				t.Errorf("messages %v still draw something running", spin)
			}
		})
	}
}

// A split posts the message the turn continues in before it freezes the old
// one. If the freeze fails the new message is deleted and the turn carries on
// where it was; if that delete fails as well, the new message — posted
// running — is finished in place, and failing that, named in the log.
func TestASplitThatFailsDoesNotLeaveTheCardItPostedSpinning(t *testing.T) {
	for name, alsoFailEdit := range map[string]bool{"the edit lands": false, "the edit fails too": true} {
		t.Run(name, func(t *testing.T) {
			router, fake, feed, ctx := cardFed(t, ProgressStream)
			router.cardMaxLines = 2
			sink := &logSink{}
			router.logf = sink.logf
			first := ask(t, router, ctx, fake)
			e := entryFor(t, router)

			feed.sendAgent(callEvent(1, "a", "bash", "one"))
			feed.sendAgent(resultEvent(2, "a", "bash"))
			feed.sendAgent(callEvent(3, "b", "read", "two"))
			feed.sendAgent(resultEvent(4, "b", "read"))
			waitCard(t, fake, first, "two finished calls", func(r chat.Reply) bool {
				return r.Activity != nil && len(r.Activity.Steps) == 2 && !anyRunning(r.Activity.Steps)
			})
			// The freeze — the first edit that would make the old card a
			// finished one — fails, once; and no delete works.
			fake.mu.Lock()
			froze := false
			fake.failDelete = func(chat.MessageRef) error { return errors.New("delete refused") }
			fake.failEdit = func(ref chat.MessageRef, r chat.Reply) error {
				if ref.ID == first && !froze && r.Activity != nil && r.Activity.Status == chat.StepDone {
					froze = true
					return errors.New("edit refused")
				}
				if alsoFailEdit && ref.ID != first {
					return errors.New("edit refused")
				}
				return nil
			}
			fake.mu.Unlock()

			// A third line takes the card over its limit.
			feed.sendAgent(callEvent(5, "c", "write", "three"))
			var posted string
			waitFor(t, func() bool {
				ids := fake.ids()
				if len(ids) < 2 {
					return false
				}
				posted = ids[1]
				return true
			}, "the split never posted the message the turn would continue in")
			if as := fake.postedReply(posted); as.Activity == nil || as.Activity.Status != chat.StepRunning {
				t.Fatalf("the split posted %+v, want a running card", as.Activity)
			}

			if alsoFailEdit {
				waitFor(t, func() bool { return len(warnings(sink, "it is left drawn as running")) == 1 },
					"nothing told the operator a card was left spinning")
				warn := warnings(sink, "it is left drawn as running")[0]
				if !strings.Contains(warn, cardConv) || !strings.Contains(warn, "message "+posted+" ") {
					t.Errorf("warning = %q, want it to name the conversation and the message", warn)
				}
				return
			}
			left := waitCard(t, fake, posted, "a finished state", func(r chat.Reply) bool {
				return r.Activity != nil && r.Activity.Status != chat.StepRunning
			})
			if left.Activity.Status != chat.StepDone || anyRunning(left.Activity.Steps) {
				t.Errorf("the unused card = %+v, want it finished with nothing running", left.Activity)
			}
			if got, want := titles(left.Activity.Steps), "write"; got != want || left.Activity.Steps[0].Status != chat.StepUnfinished {
				t.Errorf("the unused card holds %s (%s), want the call it was posted with, unfinished", got, left.Activity.Steps[0].Status)
			}
			if len(warnings(sink, "could not be deleted; finished it in place")) != 1 {
				t.Errorf("the fallback was not logged: %v", sink.matching("progress"))
			}
			// Nothing changed for the turn: the old card is still the one
			// being edited, with every call in it.
			waitCard(t, fake, first, "all three calls", func(r chat.Reply) bool {
				return r.Activity != nil && titles(r.Activity.Steps) == "bash read write"
			})
			if got := progressID(e); got != first {
				t.Errorf("the entry tracks %q, want the old card still", got)
			}
			if fake.wasDeleted(first) || fake.wasDeleted(posted) {
				t.Error("a message was deleted")
			}
		})
	}
}

// A burst far past the limit — a subagent's whole run arriving in one poll —
// rolls over into as many messages as it takes, none of them longer than the
// limit, rather than into one frozen message too long to be a card.
func TestABurstRollsOverIntoBoundedMessages(t *testing.T) {
	router, fake, rig, ctx := cardRig(t, ProgressStream)
	router.cardMaxLines = 5
	ask(t, router, ctx, fake)

	const n = 23
	seq := int64(2)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("k%02d", i)
		rig.addSubEvent("_fallback", seq, fmt.Sprintf(
			`{"seq":%d,"event":{"Content":{"parts":[{"functionCall":{"id":%q,"name":"tool_%02d","args":{"command":"a"}}}],"role":"model"},"Partial":false}}`, seq, id, i))
		seq++
		rig.addSubEvent("_fallback", seq, fmt.Sprintf(
			`{"seq":%d,"event":{"Content":{"parts":[{"functionResponse":{"id":%q,"name":"tool_%02d","response":{"ok":true}}}],"role":"user"}}}`, seq, id, i))
		seq++
	}
	rig.sendAgent(callEvent(1, "p1", "_fallback", "check billing"))
	last := fmt.Sprintf("tool_%02d", n-1)
	waitFor(t, func() bool {
		for _, m := range fake.thread() {
			if m.Activity != nil && strings.Contains(titles(m.Activity.Steps), last) {
				return true
			}
		}
		return false
	}, "the subagent's last call was never drawn")
	rig.sendAgent(resultEvent(int(seq), "p1", "_fallback"))
	rig.sendAgent(textEvent(int(seq)+1, "the answer"))
	waitPosted(t, fake, "the answer")
	waitFor(t, func() bool {
		for _, m := range fake.thread() {
			if m.Activity != nil && strings.HasPrefix(m.Activity.Title, "Done · ") {
				return true
			}
		}
		return false
	}, "no card was finalised")

	seen := make(map[string]int)
	cards := 0
	for _, m := range fake.thread() {
		if m.Activity == nil {
			continue
		}
		cards++
		if got := stepLines(m.Activity.Steps); got > router.cardMaxLines {
			t.Errorf("card %q draws %d lines, want at most %d", m.Activity.Title, got, router.cardMaxLines)
		}
		if anyRunning(m.Activity.Steps) {
			t.Errorf("card %q shows a line running", m.Activity.Title)
		}
		for _, s := range m.Activity.Steps {
			if s.Title != "_fallback" || s.Calls != n {
				t.Errorf("card %q: top line = %+v, want the delegation counting all %d", m.Activity.Title, s, n)
			}
			for _, sub := range s.Steps {
				seen[sub.Title]++
			}
		}
	}
	if cards < 5 {
		t.Errorf("%d cards, want the run spread over several", cards)
	}
	for i := 0; i < n; i++ {
		if name := fmt.Sprintf("tool_%02d", i); seen[name] != 1 {
			t.Errorf("%s is in the thread %d times, want exactly once", name, seen[name])
		}
	}
	if fake.anyDeleted() {
		t.Error("a message was deleted")
	}
}

// Narration splits the card: the card is frozen where it is, the narration
// posts under it, and the turn carries on in a fresh card below with the same
// clock. Later calls go to the new card, whose final title counts the turn.
func TestNarrationSplitsTheCard(t *testing.T) {
	for _, mode := range []ProgressMode{ProgressStatus, ProgressStream} {
		t.Run(string(mode), func(t *testing.T) {
			router, fake, rig, ctx := cardFed(t, mode)
			router.reanchorDelay = 0
			first := ask(t, router, ctx, fake)
			e := entryFor(t, router)
			rewindTurn(e, 90*time.Second)

			rig.sendAgent(callEvent(1, "a", "bash", "one"))
			rig.sendAgent(resultEvent(2, "a", "bash"))
			waitCard(t, fake, first, "its call ticked off", func(r chat.Reply) bool {
				return r.Activity != nil && !anyRunning(r.Activity.Steps)
			})
			rig.sendAgent(textEvent(3, "let me look further"))
			narration := waitPosted(t, fake, "let me look further")

			frozen := waitCard(t, fake, first, "its frozen state", hasTitle(`^1 call · 1m3\ds$`))
			if frozen.Activity.Status != chat.StepDone || frozen.Kind != chat.KindActivity || titles(frozen.Activity.Steps) != "bash" || frozen.Activity.Recent != nil {
				t.Errorf("frozen card = %+v (%s)", frozen.Activity, frozen.Kind)
			}
			var next string
			waitFor(t, func() bool {
				next = progressID(e)
				return next != "" && next != first
			}, "the turn did not continue in a fresh message")
			ids := fake.ids()
			if strings.Join(ids, " ") != strings.Join([]string{first, narration, next}, " ") {
				t.Fatalf("thread order = %v, want the card, the narration, the fresh card", ids)
			}
			// Nothing carried over: the fresh message is the clock, still the
			// turn's own.
			if fresh, _ := fake.now(next); fresh.Activity != nil || !strings.HasPrefix(fresh.Text, workingText+" 1m3") || fresh.Kind != chat.KindProgress {
				t.Errorf("fresh message = %+v, want the plain clock still counting from the turn's start", fresh)
			}

			rig.sendAgent(callEvent(4, "b", "read", "two"))
			later := waitCard(t, fake, next, "the later call", hasTitle(`^Working… 1m3\ds · 2 calls · read$`))
			if titles(later.Activity.Steps) != "read" {
				t.Errorf("the fresh card holds %s, want the later call alone", titles(later.Activity.Steps))
			}
			rig.sendAgent(resultEvent(5, "b", "read"))
			rig.sendLifecycle(daemon.EventTurnComplete, turnDone)
			rig.sendAgent(textEvent(6, "the answer"))
			waitPosted(t, fake, "the answer")
			waitCard(t, fake, next, "the whole turn's count", hasTitle(`^Done · 1m3\ds · 2 calls$`))

			if still, _ := fake.now(first); still.Activity == nil || titles(still.Activity.Steps) != "bash" {
				t.Errorf("the frozen card changed after the split: %+v", still.Activity)
			}
			if fake.anyDeleted() {
				t.Error("a message was deleted")
			}
		})
	}
}

// A card with no call yet is a clock, and narration re-anchors it as it always
// has: a fresh placeholder below, the old one deleted.
func TestNarrationReanchorsACardWithNoCalls(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStream)
	router.reanchorDelay = 0
	first := ask(t, router, ctx, fake)
	e := entryFor(t, router)

	rig.sendAgent(textEvent(1, "let me think"))
	waitPosted(t, fake, "let me think")
	waitFor(t, func() bool { return fake.wasDeleted(first) }, "the clock was not moved below the narration")
	if next := progressID(e); next == first || next == "" {
		t.Errorf("progress message = %q, want a fresh one", next)
	}
}

// A card whose calls are all still running has nothing to leave behind, so it
// moves whole: its calls are drawn in the fresh message and stay addressable.
func TestNarrationMovesACardWithNothingFinished(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStream)
	router.reanchorDelay = 0
	first := ask(t, router, ctx, fake)
	e := entryFor(t, router)

	rig.sendAgent(callEvent(1, "a", "bash", "one"))
	waitCard(t, fake, first, "its call", hasTitle(` · 1 call · bash$`))
	rig.sendAgent(textEvent(2, "still going"))
	waitPosted(t, fake, "still going")
	waitFor(t, func() bool { return fake.wasDeleted(first) }, "the card was not moved")
	next := progressID(e)
	if moved, _ := fake.now(next); moved.Activity == nil || titles(moved.Activity.Steps) != "bash" || !anyRunning(moved.Activity.Steps) {
		t.Fatalf("moved card = %+v, want the running call drawn again", moved.Activity)
	}
	rig.sendAgent(resultEvent(3, "a", "bash"))
	waitCard(t, fake, next, "the call ticked off", func(r chat.Reply) bool { return r.Activity != nil && !anyRunning(r.Activity.Steps) })
}

// noBoundary makes the entry's daemon one that does not say when a turn ends,
// so the first text is the answer and ends the turn itself — the path every
// build before turn-complete took, and the one where the answer, not a
// boundary, is what retires the progress message.
func noBoundary(t *testing.T, e *sessionEntry) {
	t.Helper()
	waitFor(t, e.signalsEnd.Load, "the capabilities frame never arrived")
	e.signalsEnd.Store(false)
}

// An answer finalises the card in place and leaves it: done, every line ticked.
func TestAnAnswerKeepsTheCard(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStream)
	card := ask(t, router, ctx, fake)
	noBoundary(t, entryFor(t, router))

	rig.sendAgent(callEvent(1, "a", "bash", "one"))
	rig.sendAgent(callEvent(2, "b", "read", "two"))
	rig.sendAgent(failedEvent(3, "a", "bash"))
	// read never answers: the turn's end closes it.
	rig.sendAgent(textEvent(4, "the answer"))
	answer := waitPosted(t, fake, "the answer")

	kept := waitCard(t, fake, card, "its final state", hasTitle(`^Done · \d+s · 2 calls \(1 failed\)$`))
	if kept.Activity.Status != chat.StepDone || kept.Kind != chat.KindActivity {
		t.Errorf("kept = %s / %s", kept.Activity.Status, kept.Kind)
	}
	want := []chat.Step{
		{Title: "bash (exit 2)", Status: chat.StepFailed, Code: "one"},
		{Title: "read", Status: chat.StepDone, Code: "two"},
	}
	for i, s := range kept.Activity.Steps {
		if s.Title != want[i].Title || s.Status != want[i].Status || s.Code != want[i].Code {
			t.Errorf("step %d = %+v, want %+v", i, s, want[i])
		}
	}
	if fake.wasDeleted(card) {
		t.Error("the card was deleted")
	}
	if ids := fake.ids(); len(ids) != 2 || ids[0] != card || ids[1] != answer {
		t.Errorf("thread = %v, want the card above the answer", ids)
	}
	if e := entryFor(t, router); progressID(e) != "" {
		t.Errorf("the entry still tracks a progress message after the turn ended")
	}
}

// A turn that ends without ever answering does not leave its card spinning:
// the boundary finalises it.
func TestATurnBoundaryWithNoAnswerFinalisesTheCard(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStatus)
	card := ask(t, router, ctx, fake)

	rig.sendAgent(callEvent(1, "a", "bash", "one"))
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	rig.sendLifecycle(daemon.EventTurnComplete, turnDone)
	kept := waitCard(t, fake, card, "its final state", hasTitle(`^Done · \d+s · 1 call$`))
	if kept.Activity.Status != chat.StepDone || anyRunning(kept.Activity.Steps) {
		t.Errorf("kept = %+v, want nothing running", kept.Activity)
	}
	if fake.anyDeleted() {
		t.Error("a message was deleted")
	}
}

// A turn the daemon reports idle on without a turn-complete — interrupted, or
// failed in a way nothing classified — does not leave its card spinning
// either, and from a daemon that sends turn-complete it reads stopped, not
// done: the boundary that says a turn succeeded never came.
func TestATurnThatGoesIdleWithNoBoundaryStopsTheCard(t *testing.T) {
	router, fake, feed, ctx := cardFed(t, ProgressStream)
	card := ask(t, router, ctx, fake)

	feed.sendLifecycle(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
	feed.sendAgent(callEvent(1, "a", "bash", "one"))
	feed.sendAgent(resultEvent(2, "a", "bash"))
	feed.sendAgent(callEvent(3, "b", "read", "two"))
	waitCard(t, fake, card, "both calls", hasTitle(` · 2 calls · read$`))
	feed.sendLifecycle(daemon.EventStatusUpdate, statusIdle)

	kept := waitCard(t, fake, card, "its stopped state", hasTitle(`^Stopped · \d+s · 2 calls$`))
	if got := statuses(kept.Activity.Steps); len(got) != 2 || got[0] != chat.StepDone || got[1] != chat.StepFailed {
		t.Errorf("statuses = %v, want the answered call done and the running one failed", got)
	}
	if fake.anyDeleted() {
		t.Error("a message was deleted")
	}

	// A daemon that sends no turn-complete ends every turn this way: done.
	router, fake, feed, ctx = cardFed(t, ProgressStream)
	card = ask(t, router, ctx, fake)
	noBoundary(t, entryFor(t, router))
	feed.sendLifecycle(daemon.EventStatusUpdate, `{"turn_state":"streaming"}`)
	feed.sendAgent(callEvent(1, "a", "bash", "one"))
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	feed.sendLifecycle(daemon.EventStatusUpdate, statusIdle)
	waitCard(t, fake, card, "its final state", hasTitle(`^Done · \d+s · 1 call$`))
}

// A failed turn keeps its card too, as stopped: what was still running reads
// failed, what had answered keeps its verdict.
func TestAFailedTurnKeepsTheCardAsStopped(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStream)
	card := ask(t, router, ctx, fake)

	rig.sendAgent(callEvent(1, "a", "bash", "one"))
	rig.sendAgent(resultEvent(2, "a", "bash"))
	rig.sendAgent(callEvent(3, "b", "read", "two"))
	waitCard(t, fake, card, "both calls", hasTitle(` · 2 calls · read$`))
	rig.sendLifecycle(daemon.EventTurnError, `{"kind":"transient_network","message":"model call timed out","retryable":true}`)

	kept := waitCard(t, fake, card, "its stopped state", hasTitle(`^Stopped · \d+s · 2 calls$`))
	if kept.Activity.Status != chat.StepFailed || kept.Kind != chat.KindActivity {
		t.Errorf("kept = %s / %s, want a stopped activity", kept.Activity.Status, kept.Kind)
	}
	if got := statuses(kept.Activity.Steps); len(got) != 2 || got[0] != chat.StepDone || got[1] != chat.StepFailed {
		t.Errorf("statuses = %v, want the answered call done and the running one failed", got)
	}
	if !strings.HasPrefix(kept.Text, markFailed+" Stopped · ") {
		t.Errorf("kept text = %q", kept.Text)
	}
	if fake.wasDeleted(card) {
		t.Error("the card was deleted")
	}
	// And the thread is told why, under it.
	waitFor(t, func() bool {
		th := fake.thread()
		return len(th) == 2 && th[1].Kind == chat.KindNotice
	}, "the failure notice was not posted under the card")
}

// A new message while a turn's card is live takes the progress slot. The card
// is kept — as a finished stretch, since its turn is still running — and not
// deleted as a clock would be.
func TestANewTurnKeepsTheCardItTakesOverFrom(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStream)
	card := ask(t, router, ctx, fake)

	rig.sendAgent(callEvent(1, "a", "bash", "one"))
	rig.sendAgent(resultEvent(2, "a", "bash"))
	rig.sendAgent(callEvent(3, "b", "read", "two"))
	waitCard(t, fake, card, "both calls", hasTitle(` · 2 calls · read$`))

	second := ask(t, router, ctx, fake)
	kept := waitCard(t, fake, card, "its kept state", hasTitle(`^2 calls · \d+s · 1 unfinished$`))
	if kept.Activity.Status != chat.StepDone || anyRunning(kept.Activity.Steps) || kept.Kind != chat.KindActivity {
		t.Errorf("kept = %+v (%s), want a finished stretch with nothing running", kept.Activity, kept.Kind)
	}
	// The call still out is not drawn as done: nothing will tick it off now,
	// and whether it succeeds is not something this message will ever know.
	if got := statuses(kept.Activity.Steps); len(got) != 2 || got[0] != chat.StepDone || got[1] != chat.StepUnfinished {
		t.Errorf("statuses = %v, want the answered call done and the one still out unfinished", got)
	}
	if !strings.Contains(kept.Text, "\n– `read`  `two`") || !strings.Contains(kept.Text, markOK+" `bash`") {
		t.Errorf("kept text = %q, want the neutral mark on the call still out", kept.Text)
	}
	if fake.wasDeleted(card) {
		t.Error("the card was deleted")
	}
	// What the first turn does next is drawn into the slot's new message.
	rig.sendAgent(callEvent(4, "c", "write", "three"))
	waitCard(t, fake, second, "the next call", hasTitle(` · 1 call · write$`))
}

// A stale card whose clock had already stopped is finalised as stopped.
func TestAStaleCardWithAStoppedClockIsKeptAsStopped(t *testing.T) {
	fake := newCardFake()
	r := NewRouter(nil, fake, ProgressStatus, nil, nil)
	ctx := context.Background()
	ref, _ := fake.Send(ctx, chat.Reply{Conversation: cardConv, Text: workingText})
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash"}}, false, time.Now())
	r.retireStale(ctx, cardConv, progressTurn{ref: ref, start: time.Now().Add(-72 * time.Second), card: c, live: false})
	if got, ok := fake.now(ref.ID); !ok || got.Activity == nil || got.Activity.Title != "Stopped · 1m12s · 1 call" || got.Activity.Steps[0].Status != chat.StepFailed {
		t.Errorf("stale card = %+v (kept: %t), want it kept as stopped", got.Activity, ok)
	}
	// A stale clock is deleted, as it always was.
	clock, _ := fake.Send(ctx, chat.Reply{Conversation: cardConv, Text: workingText})
	r.retireStale(ctx, cardConv, progressTurn{ref: clock, start: time.Now()})
	if !fake.wasDeleted(clock.ID) {
		t.Error("a stale clock was not deleted")
	}
}

// An answer with a message queued behind it: this turn's card is finalised
// and kept, and the queued turn is given a fresh placeholder below the answer,
// which picking the message up starts.
func TestAQueuedTurnGetsAFreshPlaceholderAndTheCardIsKept(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStream)
	card := ask(t, router, ctx, fake)
	e := entryFor(t, router)
	noBoundary(t, e) // the answer itself ends the turn, with the card still live

	rig.sendAgent(callEvent(1, "a", "bash", "one"))
	rig.sendAgent(resultEvent(2, "a", "bash"))
	waitCard(t, fake, card, "its call ticked off", func(r chat.Reply) bool { return r.Activity != nil && !anyRunning(r.Activity.Steps) })
	rig.sendLifecycle(daemon.EventInbox, backlogQueued)
	waitFor(t, e.backlogged, "the queued inbox event did not reach the backlog")

	rig.sendAgent(textEvent(3, "first answer"))
	answer := waitPosted(t, fake, "first answer")
	kept := waitCard(t, fake, card, "its final state", hasTitle(`^Done · \d+s · 1 call$`))
	if kept.Activity.Status != chat.StepDone {
		t.Errorf("kept status = %s", kept.Activity.Status)
	}
	var fresh string
	waitFor(t, func() bool {
		fresh = progressID(e)
		return fresh != "" && fresh != card
	}, "the queued turn was given no placeholder")
	if ids := fake.ids(); strings.Join(ids, " ") != strings.Join([]string{card, answer, fresh}, " ") {
		t.Errorf("thread order = %v, want the card, the answer, the fresh placeholder", ids)
	}
	if got, _ := fake.now(fresh); got.Text != workingText || got.Activity != nil || got.Kind != chat.KindProgress {
		t.Errorf("fresh placeholder = %+v, want a plain clock", got)
	}

	// The queued message is taken up: its turn runs on the fresh placeholder
	// and its calls draw a card of their own.
	rig.sendLifecycle(daemon.EventInbox, backlogDequeued)
	waitFor(t, e.turnInFlight, "picking up the queued message did not put a turn in flight")
	rig.sendAgent(callEvent(5, "b", "read", "two"))
	second := waitCard(t, fake, fresh, "the queued turn's call", hasTitle(`^Working… \d+s · 1 call · read$`))
	if titles(second.Activity.Steps) != "read" {
		t.Errorf("the queued turn's card holds %s, want its own call alone", titles(second.Activity.Steps))
	}
	if still, _ := fake.now(card); still.Activity == nil || !strings.HasPrefix(still.Activity.Title, "Done · ") || titles(still.Activity.Steps) != "bash" {
		t.Errorf("the first turn's card changed: %+v", still.Activity)
	}
	if fake.anyDeleted() {
		t.Error("a message was deleted")
	}
}

// The same with the boundary ahead of the answer, the order a daemon that
// signals the end of a turn sends them in: the boundary finalises the card and
// posts the queued turn's placeholder, and the answer moves that placeholder
// below itself as it always moved the clock.
func TestABoundaryWithAQueuedTurnKeepsTheCard(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStatus)
	card := ask(t, router, ctx, fake)
	e := entryFor(t, router)

	rig.sendAgent(callEvent(1, "a", "bash", "one"))
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	rig.sendLifecycle(daemon.EventInbox, backlogQueued)
	waitFor(t, e.backlogged, "the queued inbox event did not reach the backlog")

	rig.sendLifecycle(daemon.EventTurnComplete, turnDone)
	waitCard(t, fake, card, "its final state", hasTitle(`^Done · \ds · 1 call$`))
	rig.sendAgent(textEvent(2, "first answer"))
	waitPosted(t, fake, "first answer")
	waitFor(t, func() bool {
		th := fake.thread()
		return len(th) == 3 && th[0].Activity != nil && th[1].Text == "first answer" && th[2].Kind == chat.KindProgress && th[2].Activity == nil
	}, "the thread did not settle as the card, the answer, the queued turn's placeholder")
	if fake.wasDeleted(card) {
		t.Error("the card was deleted")
	}
	if got := progressID(e); got == card || got == "" {
		t.Errorf("the entry tracks %q, want the queued turn's placeholder", got)
	}
}

// A failed turn with a message queued behind it: the same, with the card
// stopped.
func TestAFailedTurnWithAQueuedMessageKeepsTheCard(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStatus)
	card := ask(t, router, ctx, fake)
	e := entryFor(t, router)

	rig.sendAgent(callEvent(1, "a", "bash", "one"))
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	rig.sendLifecycle(daemon.EventInbox, backlogQueued)
	waitFor(t, e.backlogged, "the queued inbox event did not reach the backlog")
	rig.sendLifecycle(daemon.EventTurnError, `{"kind":"transient_network","message":"model call timed out","retryable":true}`)

	waitCard(t, fake, card, "its stopped state", hasTitle(`^Stopped · \d+s · 1 call$`))
	var fresh string
	waitFor(t, func() bool {
		fresh = progressID(e)
		return fresh != "" && fresh != card
	}, "the queued turn was given no placeholder")
	th := fake.thread()
	if len(th) != 3 || th[1].Kind != chat.KindNotice || th[2].Text != workingText {
		t.Errorf("thread = %d messages, want the card, the notice, the fresh placeholder", len(th))
	}
	if fake.anyDeleted() {
		t.Error("a message was deleted")
	}
}

// A turn that made no calls has only a clock, which is deleted exactly as it
// is for an adapter that draws nothing.
func TestATurnWithNoCallsKeepsNothing(t *testing.T) {
	for _, mode := range []ProgressMode{ProgressStatus, ProgressStream} {
		t.Run(string(mode), func(t *testing.T) {
			router, fake, rig, ctx := cardFed(t, mode)
			clock := ask(t, router, ctx, fake)
			rig.sendLifecycle(daemon.EventTurnComplete, turnDone)
			rig.sendAgent(textEvent(1, "the answer"))
			waitPosted(t, fake, "the answer")
			waitFor(t, func() bool { return fake.wasDeleted(clock) }, "the clock was not deleted")
			if th := fake.thread(); len(th) != 1 || th[0].Text != "the answer" {
				t.Errorf("thread = %d messages, want the answer alone", len(th))
			}
			if n := len(fake.editsTo(clock)); n != 0 {
				t.Errorf("the clock was edited %d times, want none", n)
			}
		})
	}
}

// A turn that made no calls and never answers leaves its clock where it
// stopped, as it always has: the boundary finalises a card, and a clock is not
// one. The frozen "Working… 2m30s" is the only trace the question was heard.
func TestABoundaryLeavesAClockWithNoCallsAlone(t *testing.T) {
	for _, mode := range []ProgressMode{ProgressStatus, ProgressStream} {
		t.Run(string(mode), func(t *testing.T) {
			router, fake, feed, ctx := cardFed(t, mode)
			clock := ask(t, router, ctx, fake)
			e := entryFor(t, router)
			feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
			waitFor(t, func() bool { return !e.turnInFlight() }, "the boundary did not end the turn")
			// The boundary's handling is synchronous on the relay; a frame
			// behind it proves it has finished.
			feed.sendLifecycle(daemon.EventInbox, backlogQueued)
			waitFor(t, e.backlogged, "the relay did not get past the boundary")

			if fake.wasDeleted(clock) {
				t.Error("the clock was deleted at the boundary; it is the answer, or the next turn, that retires it")
			}
			if n := len(fake.editsTo(clock)); n != 0 {
				t.Errorf("the clock was edited %d times at the boundary, want it left as it stopped", n)
			}
			if got := progressID(e); got != clock {
				t.Errorf("the entry tracks %q, want the clock still in place for the answer to retire", got)
			}
			if ids := fake.ids(); len(ids) != 1 {
				t.Errorf("%d messages posted, want the clock alone", len(ids))
			}
		})
	}
}

// rateLimited is an edit refused with a delay to wait out, as an adapter
// reports one (chat.RetryAfter).
type rateLimited struct{ after time.Duration }

func (e rateLimited) Error() string             { return "rate limited" }
func (e rateLimited) RetryAfter() time.Duration { return e.after }

// The edit that finalises a card is tried again when it fails: one that lands
// on a later attempt leaves the card finalised, exactly as if the first had.
func TestAFailedFinalisingEditIsRetried(t *testing.T) {
	router, fake, feed, ctx := cardFed(t, ProgressStatus)
	router.cardRetry = time.Millisecond
	sink := &logSink{}
	router.logf = sink.logf
	card := ask(t, router, ctx, fake)
	feed.sendAgent(callEvent(1, "a", "bash", "one"))
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	before := fake.tried(card)
	failures := 2
	fake.setHooks(func(chat.MessageRef, chat.Reply) error {
		if failures > 0 {
			failures--
			return errors.New("edit refused")
		}
		return nil
	}, nil)
	feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
	kept := waitCard(t, fake, card, "its final state", hasTitle(`^Done · \ds · 1 call$`))
	if kept.Activity.Status != chat.StepDone || anyRunning(kept.Activity.Steps) {
		t.Errorf("kept = %+v", kept.Activity)
	}
	if got := fake.tried(card) - before; got != 3 {
		t.Errorf("%d attempts at the finalising edit, want the two that failed and the one that landed", got)
	}
	if fake.wasDeleted(card) {
		t.Error("the card was deleted")
	}
	if len(warnings(sink, "could not be finalised")) != 0 {
		t.Errorf("a card that was finalised was reported as not: %v", sink.matching("finalise"))
	}
}

// A card whose finalising edit never lands is not deleted — it used to be,
// which threw away the turn's record because an edit failed. It is left as it
// was last drawn, after a bounded number of attempts, and the log names the
// conversation and the message.
func TestACardThatCannotBeFinalisedIsLeftAndNamed(t *testing.T) {
	router, fake, feed, ctx := cardFed(t, ProgressStatus)
	router.cardRetry = time.Millisecond
	sink := &logSink{}
	router.logf = sink.logf
	card := ask(t, router, ctx, fake)
	feed.sendAgent(callEvent(1, "a", "bash", "one"))
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	before := fake.tried(card)
	fake.setHooks(func(chat.MessageRef, chat.Reply) error { return errors.New("edit refused") }, nil)
	feed.sendLifecycle(daemon.EventTurnComplete, turnDone)

	waitFor(t, func() bool { return len(warnings(sink, "could not be finalised")) == 1 }, "nothing said the card could not be finalised")
	warn := warnings(sink, "could not be finalised")[0]
	if !strings.Contains(warn, cardConv) || !strings.Contains(warn, "message "+card+" ") {
		t.Errorf("warning = %q, want it to name the conversation and the message", warn)
	}
	if got := fake.tried(card) - before; got != cardFinishAttempts {
		t.Errorf("%d attempts at the finalising edit, want %d and no more", got, cardFinishAttempts)
	}
	if fake.wasDeleted(card) || fake.anyDeleted() {
		t.Error("a card that holds the turn's calls was deleted because its last edit failed")
	}
	if still, ok := fake.now(card); !ok || still.Activity == nil || titles(still.Activity.Steps) != "bash" {
		t.Errorf("the card is no longer standing with its call: %+v", still.Activity)
	}
	// It is out of service all the same: the answer does not try again.
	feed.sendAgent(textEvent(2, "the answer"))
	waitPosted(t, fake, "the answer")
	if got := progressID(entryFor(t, router)); got != "" {
		t.Errorf("the entry still tracks %q", got)
	}
}

// A failure the platform calls permanent is not retried: the message is gone,
// or not ours to edit, and asking again will not change that.
func TestAPermanentlyRefusedFinalisingEditIsNotRetried(t *testing.T) {
	for name, refusal := range map[string]error{"not found": chat.ErrNotFound, "denied": chat.ErrDenied} {
		t.Run(name, func(t *testing.T) {
			router, fake, feed, ctx := cardFed(t, ProgressStatus)
			router.cardRetry = time.Millisecond
			sink := &logSink{}
			router.logf = sink.logf
			card := ask(t, router, ctx, fake)
			feed.sendAgent(callEvent(1, "a", "bash", "one"))
			waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
			before := fake.tried(card)
			fake.setHooks(func(chat.MessageRef, chat.Reply) error { return fmt.Errorf("platform says no: %w", refusal) }, nil)
			feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
			waitFor(t, func() bool { return len(warnings(sink, "could not be finalised")) == 1 }, "nothing said the card could not be finalised")
			if got := fake.tried(card) - before; got != 1 {
				t.Errorf("%d attempts, want one: a permanent refusal is not retried", got)
			}
			if fake.anyDeleted() {
				t.Error("the card was deleted")
			}
		})
	}
}

// A rate limit that says how long to wait is waited out, not answered with
// another edit at once.
func TestAFinalisingEditHonoursARateLimit(t *testing.T) {
	router, fake, feed, ctx := cardFed(t, ProgressStatus)
	router.cardRetry = time.Millisecond
	card := ask(t, router, ctx, fake)
	feed.sendAgent(callEvent(1, "a", "bash", "one"))
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	const asked = 150 * time.Millisecond
	var refusedAt, landedAt time.Time
	fake.setHooks(func(_ chat.MessageRef, r chat.Reply) error {
		if refusedAt.IsZero() {
			refusedAt = time.Now()
			return fmt.Errorf("slack: update: %w", rateLimited{after: asked})
		}
		landedAt = time.Now()
		return nil
	}, nil)
	feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
	waitCard(t, fake, card, "its final state", hasTitle(`^Done · `))
	fake.mu.Lock()
	gap := landedAt.Sub(refusedAt)
	fake.mu.Unlock()
	if gap < asked {
		t.Errorf("the edit was tried again after %s, want the %s the platform asked for waited out", gap, asked)
	}
}

// Redraws are coalesced: a burst of frames costs far fewer edits than frames,
// and the last state is always drawn — by the trailing redraw, with no turn
// boundary to force it.
func TestCardRedrawsAreCoalesced(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStream)
	const interval = 150 * time.Millisecond
	router.cardRedraw = interval
	card := ask(t, router, ctx, fake)

	const n = 20
	start := time.Now()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("tool_%02d", i)
		rig.sendAgent(callEvent(2*i+1, name, name, "x"))
		rig.sendAgent(resultEvent(2*i+2, name, name))
	}
	// The first frame is drawn at once.
	waitCard(t, fake, card, "its first call", func(r chat.Reply) bool { return r.Activity != nil })
	// And the last state arrives on its own: every call, every one answered.
	last := waitCard(t, fake, card, "every call ticked off", func(r chat.Reply) bool {
		return r.Activity != nil && len(r.Activity.Steps) == n && !anyRunning(r.Activity.Steps)
	})
	elapsed := time.Since(start)
	if !strings.HasSuffix(last.Activity.Title, fmt.Sprintf(" · %d calls", n)) {
		t.Errorf("last title = %q", last.Activity.Title)
	}
	// One edit at once and then at most one per interval; the 2n frames were
	// all in well inside a few of those.
	edits := len(fake.editsTo(card))
	if allowed := 2 + int(elapsed/interval); edits > allowed {
		t.Errorf("%d edits in %s for %d frames, want at most %d: one per %s", edits, elapsed.Round(time.Millisecond), 2*n, allowed, interval)
	}
	if edits >= n {
		t.Errorf("%d edits for %d frames, want far fewer", edits, 2*n)
	}
	// Nothing further is owed: the card is not edited again while idle.
	time.Sleep(2 * interval)
	if again := len(fake.editsTo(card)); again != edits {
		t.Errorf("the card was edited %d more times with nothing to draw", again-edits)
	}
}

// The trailing redraw is the turn's own: one still owed when the turn ends is
// dropped with it, and the final state is drawn at once rather than waiting
// out the interval.
func TestEndingTheTurnDrawsTheFinalStateAtOnce(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStream)
	router.cardRedraw = time.Hour // nothing coalesced is ever drawn in this test
	card := ask(t, router, ctx, fake)

	rig.sendAgent(callEvent(1, "a", "bash", "one"))
	waitCard(t, fake, card, "its first call", hasTitle(` · 1 call · bash$`))
	rig.sendAgent(resultEvent(2, "a", "bash"))
	rig.sendAgent(callEvent(3, "b", "read", "two"))
	rig.sendAgent(resultEvent(4, "b", "read"))
	rig.sendLifecycle(daemon.EventTurnComplete, turnDone)
	kept := waitCard(t, fake, card, "its final state", hasTitle(`^Done · \d+s · 2 calls$`))
	if titles(kept.Activity.Steps) != "bash read" || anyRunning(kept.Activity.Steps) {
		t.Errorf("final = %s, want both calls, drawn without waiting for the interval", titles(kept.Activity.Steps))
	}
	if n := len(fake.editsTo(card)); n != 2 {
		t.Errorf("%d edits, want the first call's and the final one", n)
	}
	e := entryFor(t, router)
	e.pmu.Lock()
	owed := e.redrawTimer != nil
	e.pmu.Unlock()
	if owed {
		t.Error("a trailing redraw is still owed after the turn ended")
	}
}

// A turn that has a card keeps it when the adapter stops drawing cards half
// way (Slack refused one): the record goes on being fed — calls, results —
// for the rest of the turn, nothing is posted beside it, and what is kept at
// the end says every call the turn made, as text. It used to stop being fed
// and still be kept, so a three-call turn ended as "Done · 1 call". The next
// turn starts outside card mode.
func TestACardTurnKeepsItsRecordWhenCardsAreRefused(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		router, fake, feed, ctx := cardFed(t, ProgressStatus)
		card := ask(t, router, ctx, fake)

		feed.sendAgent(callEvent(1, "a", "bash", "one"))
		waitCard(t, fake, card, "its first call, drawn", hasTitle(` · 1 call · bash$`))
		fake.mu.Lock()
		fake.refuseCards = true // from here Slack refuses the card
		fake.mu.Unlock()
		feed.sendAgent(resultEvent(2, "a", "bash"))
		feed.sendAgent(callEvent(3, "b", "read", "two"))
		feed.sendAgent(resultEvent(4, "b", "read"))
		feed.sendAgent(callEvent(5, "c", "write", "three"))
		feed.sendAgent(failedEvent(6, "c", "write"))
		feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
		feed.sendAgent(textEvent(7, "the answer"))
		waitPosted(t, fake, "the answer")

		kept := waitCard(t, fake, card, "its kept text", func(r chat.Reply) bool { return strings.HasPrefix(r.Text, markOK+" Done · ") })
		if kept.Activity != nil {
			t.Errorf("the adapter drew a card after refusing them: %+v", kept.Activity)
		}
		first, _, _ := strings.Cut(kept.Text, "\n")
		if !regexp.MustCompile(`^.{1,4} Done · \d+s · 3 calls \(1 failed\)$`).MatchString(first) {
			t.Errorf("kept text starts %q, want all three calls counted", first)
		}
		for _, line := range []string{markOK + " `bash`", markOK + " `read`", markFailed + " `write (exit 2)`"} {
			if !strings.Contains(kept.Text, "\n"+line) {
				t.Errorf("kept text does not list %q:\n%s", line, kept.Text)
			}
		}
		if strings.Contains(kept.Text, "`one`") || strings.Contains(kept.Text, "`two`") || strings.Contains(kept.Text, "`three`") {
			t.Errorf("a status record carries arguments:\n%s", kept.Text)
		}
		if router.drawsActivity() {
			t.Error("the router still believes cards are drawn")
		}
		// Nothing was posted beside the record, and it was not deleted.
		if th := fake.thread(); len(th) != 2 || th[1].Text != "the answer" {
			t.Errorf("thread has %d messages, want the record and the answer", len(th))
		}
		if fake.anyDeleted() {
			t.Error("a message was deleted")
		}
	})

	t.Run("stream", func(t *testing.T) {
		router, fake, feed, ctx := cardFed(t, ProgressStream)
		fake.refuseCards = true // refused from the very first card
		card := ask(t, router, ctx, fake)
		e := entryFor(t, router)

		feed.sendAgent(callEvent(1, "a", "bash", "one"))
		waitFor(t, func() bool { return len(fake.editsTo(card)) > 0 }, "the first call was never drawn")
		if got, _ := fake.now(card); got.Activity != nil || !strings.Contains(got.Text, "running `bash`") {
			t.Errorf("after the refusal the message says %+v, want the text alone", got)
		}
		if router.cardMode(ProgressStream) {
			t.Fatal("the router still believes cards are drawn")
		}
		if !router.carded(e, ProgressStream) {
			t.Fatal("the turn dropped its card when the adapter stopped drawing")
		}
		feed.sendAgent(callEvent(2, "b", "read", "two"))
		feed.sendAgent(resultEvent(3, "b", "read"))
		feed.sendAgent(resultEvent(4, "a", "bash"))
		feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
		feed.sendAgent(textEvent(5, "the answer"))
		waitPosted(t, fake, "the answer")

		kept := waitCard(t, fake, card, "its kept text", func(r chat.Reply) bool { return strings.HasPrefix(r.Text, markOK+" Done · ") })
		first, _, _ := strings.Cut(kept.Text, "\n")
		if !strings.HasSuffix(first, " · 2 calls") {
			t.Errorf("kept text starts %q, want both calls counted", first)
		}
		// Stream's record keeps each call's argument, and each was ticked
		// off by its own result: it has a duration.
		for _, line := range []string{markOK + " `bash`  <0.1s  `one`", markOK + " `read`  <0.1s  `two`"} {
			if !strings.Contains(kept.Text, "\n"+line) {
				t.Errorf("kept text does not list %q:\n%s", line, kept.Text)
			}
		}
		// No notice per frame: the turn's calls are in its record.
		for _, m := range fake.thread() {
			if m.Kind == chat.KindActivity && strings.Contains(m.Text, "Running **") {
				t.Errorf("a notice was posted beside the card turn's record: %q", m.Text)
			}
		}
		if th := fake.thread(); len(th) != 2 {
			t.Errorf("thread has %d messages, want the record and the answer", len(th))
		}

		// The next turn starts outside card mode: stream is a notice per
		// frame again, and its clock is deleted when the answer lands.
		clock := ask(t, router, ctx, fake)
		if router.carded(e, ProgressStream) {
			t.Fatal("the next turn started with a card, with an adapter that draws none")
		}
		feed.sendAgent(callEvent(6, "c", "write", "three"))
		var notice string
		waitFor(t, func() bool {
			for _, id := range fake.ids() {
				if r, ok := fake.now(id); ok && r.Kind == chat.KindActivity && strings.Contains(r.Text, "Running **write**") {
					notice = id
					return true
				}
			}
			return false
		}, "the next turn did not post a notice for its call")
		feed.sendAgent(resultEvent(7, "c", "write"))
		waitCard(t, fake, notice, "its result", func(r chat.Reply) bool { return strings.Contains(r.Text, "Ran **write**") })
		feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
		feed.sendAgent(textEvent(8, "second answer"))
		waitPosted(t, fake, "second answer")
		waitFor(t, func() bool { return fake.wasDeleted(clock) }, "the next turn's clock was not deleted")
	})
}

// The same rule when it is the channel's mode that changes under a turn with a
// card: the card goes on being fed to the end of the turn.
func TestACardTurnKeepsItsRecordWhenTheModeChanges(t *testing.T) {
	router, fake, feed, ctx := cardFed(t, ProgressStatus)
	card := ask(t, router, ctx, fake)
	e := entryFor(t, router)

	feed.sendAgent(callEvent(1, "a", "bash", "one"))
	waitCard(t, fake, card, "its first call", hasTitle(` · 1 call · bash$`))
	router.setProgress(e.channel, ProgressIndicator)
	feed.sendAgent(resultEvent(2, "a", "bash"))
	feed.sendAgent(callEvent(3, "b", "read", "two"))
	feed.sendAgent(resultEvent(4, "b", "read"))
	feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
	kept := waitCard(t, fake, card, "its final state", hasTitle(`^Done · \d+s · 2 calls$`))
	if titles(kept.Activity.Steps) != "bash read" || anyRunning(kept.Activity.Steps) {
		t.Errorf("kept = %s, want both calls, ticked off", titles(kept.Activity.Steps))
	}
}

// An adapter that draws no activity sees what it saw before cards, in both
// modes: no Activity on anything, status's one edited line deleted when the
// answer lands, stream's notice per frame. (The rest of the suite runs on such
// an adapter too; this is the two modes side by side on the card tests' own
// script.)
func TestAnAdapterThatDrawsNothingSeesNoCards(t *testing.T) {
	for _, mode := range []ProgressMode{ProgressStatus, ProgressStream} {
		t.Run(string(mode), func(t *testing.T) {
			router, fake, rig, ctx := cardRig(t, mode)
			fake.draws.Store(false)
			clock := ask(t, router, ctx, fake)

			rig.addSubEvent("_fallback", 2, `{"seq":2,"event":{"Content":{"parts":[{"functionCall":{"id":"f1","name":"get_k8s_pods","args":{"namespace":"billing"}}}],"role":"model"},"Partial":false}}`)
			rig.addSubEvent("_fallback", 3, `{"seq":3,"event":{"Content":{"parts":[{"functionResponse":{"id":"f1","name":"get_k8s_pods","response":{"pods":3}}}],"role":"user"}}}`)
			rig.sendAgent(callEvent(1, "p1", "_fallback", "check billing"))
			if mode == ProgressStatus {
				waitFor(t, func() bool {
					for _, e := range fake.editsTo(clock) {
						if strings.Contains(e.Text, "running `_fallback` › `get_k8s_pods` (call 1)") {
							return true
						}
					}
					return false
				}, "status did not name the subagent's tool on its line")
			} else {
				waitFor(t, func() bool {
					for _, m := range fake.thread() {
						if m.Kind == chat.KindActivity && strings.Contains(m.Text, "**fallback** › **get_k8s_pods**") {
							return true
						}
					}
					return false
				}, "stream did not post a notice for the subagent's call")
			}
			rig.sendAgent(resultEvent(4, "p1", "_fallback"))
			rig.sendLifecycle(daemon.EventTurnComplete, turnDone)
			rig.sendAgent(textEvent(5, "the answer"))
			waitPosted(t, fake, "the answer")
			waitFor(t, func() bool { return fake.wasDeleted(clock) }, "the placeholder was not deleted")

			fake.mu.Lock()
			defer fake.mu.Unlock()
			for id, r := range fake.posted {
				if r.Activity != nil {
					t.Errorf("message %s was posted with an Activity", id)
				}
			}
			for id, edits := range fake.edits {
				for _, r := range edits {
					if r.Activity != nil {
						t.Errorf("message %s was edited with an Activity", id)
					}
					if strings.HasPrefix(r.Text, markOK+" Done") {
						t.Errorf("message %s was finalised rather than deleted: %q", id, r.Text)
					}
				}
			}
			notices := 0
			for _, r := range fake.posted {
				if r.Kind == chat.KindActivity {
					notices++
				}
			}
			if mode == ProgressStream && notices != 2 {
				t.Errorf("stream posted %d notices, want the delegation's and the subagent call's", notices)
			}
			if mode == ProgressStatus && notices != 0 {
				t.Errorf("status posted %d notices, want none", notices)
			}
		})
	}
}

// A restart finalises the card a turn had up, from the state file, rather than
// deleting the turn's record or leaving it spinning — as interrupted, not as
// stopped: the gateway restarting is not the turn failing.
func TestARestartFinalisesAPersistedCardAsInterrupted(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 16)}
	r, _ := boundRouter(t, d)
	fake := newCardFake()
	r.out = fake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// What the dead process had in the thread.
	fake.mu.Lock()
	fake.order = append(fake.order, "ts-card")
	fake.posted["ts-card"] = chat.Reply{Conversation: "C0:1", Text: workingText}
	fake.mu.Unlock()

	r.restore(ctx, routerState{Version: stateVersion, Sessions: map[string]sessionRecord{"C0:1": {
		Session: "core-agent/s1", Owner: "alice@example.com", Touched: time.Now(), Placeholder: "ts-card",
		Card: &cardRecord{Calls: 3, Steps: []stepRecord{
			{Title: "bash", Status: "done", Code: "ls", Note: "2.0s"},
			{Title: "_fallback", Status: "running", Calls: 1, Steps: []stepRecord{{Title: "grep", Status: "running", Code: "x"}}},
		}},
	}}}, time.Now())

	kept := waitCard(t, fake, "ts-card", "its interrupted state", hasTitle(`^Interrupted by a restart · 3 calls$`))
	if kept.Activity.Status != chat.StepFailed || kept.Kind != chat.KindActivity || kept.Activity.Recent != nil {
		t.Errorf("kept = %+v (%s)", kept.Activity, kept.Kind)
	}
	want := []chat.StepStatus{chat.StepDone, chat.StepUnfinished, chat.StepUnfinished}
	if got := statuses(kept.Activity.Steps); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("statuses = %v, want %v: what was running reads unfinished, not failed", got, want)
	}
	if !strings.HasPrefix(kept.Text, markFailed+" Interrupted by a restart · 3 calls\n") || !strings.Contains(kept.Text, "  ↳ – `grep`  `x`") {
		t.Errorf("kept text =\n%s", kept.Text)
	}
	if fake.wasDeleted("ts-card") {
		t.Error("the card was deleted")
	}
	// Once it has landed the card is owed nothing more, and the snapshot
	// stops naming it.
	waitFor(t, func() bool {
		st := r.snapshot()
		rec := st.Sessions["C0:1"]
		return rec.Placeholder == "" && rec.Card == nil && len(st.Owed) == 0
	}, "the snapshot still names a card that has been finalised")
	if n := fake.tried("ts-card"); n != 1 {
		t.Errorf("the card was edited %d times, want once: restore finalises it, and the revive leaves it alone", n)
	}
}

// A placeholder that was a plain clock is still taken down on restart, as it
// always was.
func TestARestartStillClearsAClock(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 16)}
	r, _ := boundRouter(t, d)
	fake := newCardFake()
	r.out = fake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.restore(ctx, routerState{Version: stateVersion, Sessions: map[string]sessionRecord{"C0:1": {
		Session: "core-agent/s1", Owner: "alice@example.com", Touched: time.Now(), Placeholder: "ts-working",
	}}}, time.Now())
	waitFor(t, func() bool { return fake.wasDeleted("ts-working") }, "the placeholder was not taken down")
}

// A persisted card that cannot be finalised at boot is not deleted, is named
// in the log, and stays in the snapshot for the next process to try.
func TestARestartLeavesACardItCannotFinaliseForTheNext(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 16)}
	r, _ := boundRouter(t, d)
	r.cardRetry = time.Millisecond
	sink := &logSink{}
	r.logf = sink.logf
	fake := newCardFake()
	fake.failEdit = func(chat.MessageRef, chat.Reply) error { return errors.New("edit refused") }
	r.out = fake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	card := &cardRecord{Calls: 1, Steps: []stepRecord{{Title: "bash", Status: "running"}}}
	r.restore(ctx, routerState{Version: stateVersion, Sessions: map[string]sessionRecord{"C0:1": {
		Session: "core-agent/s1", Owner: "alice@example.com", Touched: time.Now(), Placeholder: "ts-working", Card: card,
	}}}, time.Now())

	waitFor(t, func() bool { return len(warnings(sink, "could not be finalised")) == 1 }, "nothing said the card could not be finalised")
	if warn := warnings(sink, "could not be finalised")[0]; !strings.Contains(warn, "C0:1") || !strings.Contains(warn, "message ts-working ") {
		t.Errorf("warning = %q, want it to name the conversation and the message", warn)
	}
	if fake.tried("ts-working") != cardFinishAttempts {
		t.Errorf("%d attempts, want %d", fake.tried("ts-working"), cardFinishAttempts)
	}
	if fake.anyDeleted() {
		t.Error("a card was deleted because it could not be finalised")
	}
	// Still owed, and on the snapshot's own list of such: not on the
	// conversation's record, where it would read as a turn in flight.
	st := r.snapshot()
	if len(st.Owed) != 1 || st.Owed[0].Conversation != "C0:1" || st.Owed[0].Message != "ts-working" ||
		st.Owed[0].Title != "Interrupted by a restart · 1 call" || st.Owed[0].Status != "failed" || len(st.Owed[0].Steps) != 1 {
		t.Errorf("the snapshot owes %+v; want the one card, as interrupted, for the next process", st.Owed)
	}
	if rec := st.Sessions["C0:1"]; rec.Placeholder != "" || rec.Card != nil || rec.InFlight {
		t.Errorf("the conversation's record names placeholder %q, card %+v, in flight %t; an owed card is not a turn in flight", rec.Placeholder, rec.Card, rec.InFlight)
	}
}

// The snapshot keeps the card of a turn in flight beside its placeholder, and
// drops it when the turn ends.
func TestTheSnapshotKeepsTheCardOfATurnInFlight(t *testing.T) {
	router, fake, rig, ctx := cardFed(t, ProgressStream)
	card := ask(t, router, ctx, fake)
	e := entryFor(t, router)

	if rec := e.record(); rec.Placeholder != card || rec.Card != nil {
		t.Errorf("before any call: placeholder %q, card %+v; want the clock and no card", rec.Placeholder, rec.Card)
	}
	rig.sendAgent(callEvent(1, "a", "bash", "one"))
	rig.sendAgent(resultEvent(2, "a", "bash"))
	rig.sendAgent(callEvent(3, "b", "read", "two"))
	waitCard(t, fake, card, "both calls", hasTitle(` · 2 calls · read$`))

	rec := e.record()
	if rec.Placeholder != card || rec.Card == nil {
		t.Fatalf("in flight: placeholder %q, card %+v; want both", rec.Placeholder, rec.Card)
	}
	if rec.Card.Calls != 2 || len(rec.Card.Steps) != 2 || rec.Card.Steps[0].Status != "done" || rec.Card.Steps[1].Status != "running" || rec.Card.Steps[1].Code != "two" {
		t.Errorf("recorded card = %+v", rec.Card)
	}
	// It is in what a restart would read.
	if st := router.snapshot(); st.Sessions[cardConv].Card == nil {
		t.Error("the snapshot carries no card for the turn in flight")
	}

	rig.sendLifecycle(daemon.EventTurnComplete, turnDone)
	waitCard(t, fake, card, "its final state", hasTitle(`^Done · `))
	if rec := e.record(); rec.Placeholder != "" || rec.Card != nil {
		t.Errorf("after the turn ended: placeholder %q, card %+v; want neither", rec.Placeholder, rec.Card)
	}
}

// For an adapter that draws no card a result would be an edit that changes
// nothing, so it is not sent: status mode there edits exactly as often as it
// did before cards — once for the call, never for a result — and the message
// is deleted when the answer lands.
func TestStatusModeSendsNoResultEditsToAnAdapterThatDrawsNothing(t *testing.T) {
	release := make(chan struct{})
	router, fake := newEventRouter(t, ProgressStatus, release, twoCallEvent, okResultEvent, failResultEvent, lateAnswerEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	recvReply(t, fake.replies)
	close(release)
	recvReply(t, fake.replies)
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts1") }, "the status message was not retired")

	var edits []fakeUpdate
	for _, u := range fake.updatedCalls() {
		if u.ref.ID == "ts1" {
			edits = append(edits, u)
		}
	}
	if len(edits) != 1 {
		t.Fatalf("%d edits to the status message, want only the call's: %+v", len(edits), edits)
	}
	if edits[0].reply.Activity != nil || !strings.Contains(edits[0].text, "running `bash`, `bash` (step 1)") {
		t.Errorf("the call's edit = %q with activity %+v, want the status line and nothing drawn", edits[0].text, edits[0].reply.Activity)
	}
}

// cardUnit is a router in card mode with one conversation's entry and no
// daemon behind it: the tests below drive the router's own steps by hand —
// startProgress, postActivity, a boundary — where a scripted stream would only
// add a race to wait out. No ticker unless a test sets tickInterval.
func cardUnit(t *testing.T, mode ProgressMode) (*Router, *cardFake, *sessionEntry, context.Context) {
	t.Helper()
	fake := newCardFake()
	r := NewRouter(nil, fake, mode, nil, nil)
	r.tickInterval = 0
	r.cardRedraw = time.Millisecond
	r.cardRetry = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return r, fake, unitEntry(r, cardConv), ctx
}

// unitEntry adds a ready entry for conv to r.
func unitEntry(r *Router, conv string) *sessionEntry {
	e := &sessionEntry{ready: make(chan struct{}), sess: testSession}
	close(e.ready)
	r.mu.Lock()
	r.sessions[conv] = e
	r.mu.Unlock()
	return e
}

// begin starts a turn on e and returns its placeholder's id.
func begin(t *testing.T, r *Router, ctx context.Context, fake *cardFake, e *sessionEntry, conv string) string {
	t.Helper()
	r.startProgress(ctx, e, conv)
	e.beginTurnInFlight()
	id := progressID(e)
	if id == "" {
		t.Fatal("startProgress posted no placeholder")
	}
	return id
}

func toolCall(id, name, arg string) daemon.ToolCall {
	return daemon.ToolCall{ID: id, Name: name, Arg: arg}
}

// Shutdown finalises every card still running, as interrupted, on a context
// that outlives the cancelled one and whether or not there is a state file;
// a plain clock is left as a shutdown always left it; and what was finalised
// is no longer named by the snapshot.
func TestShutdownFinalisesEveryLiveCard(t *testing.T) {
	r, fake, e1, ctx := cardUnit(t, ProgressStream)
	if r.store != nil {
		t.Fatal("this test is about a run with no state file")
	}
	convs := []string{cardConv, "C0:200.1", "C0:300.1", "C0:400.1", "C0:500.1"}
	entries := map[string]*sessionEntry{cardConv: e1}
	cards := map[string]string{}
	for _, conv := range convs[1:] {
		entries[conv] = unitEntry(r, conv)
	}
	for _, conv := range convs {
		e := entries[conv]
		cards[conv] = begin(t, r, ctx, fake, e, conv)
		r.postActivity(ctx, e, conv, ProgressStream, []daemon.ToolCall{toolCall("a", "bash", "one")})
		r.cardToolResults(ctx, e, conv, ProgressStream, []daemon.ToolResult{{ID: "a", Name: "bash"}})
		r.postActivity(ctx, e, conv, ProgressStream, []daemon.ToolCall{toolCall("b", "read", "two")})
		waitCard(t, fake, cards[conv], "both calls", hasTitle(` · 2 calls · read$`))
	}
	// And a turn with no calls: a clock.
	clockEntry := unitEntry(r, "C0:900.1")
	clock := begin(t, r, ctx, fake, clockEntry, "C0:900.1")

	// The run is over: its context is cancelled, and each edit takes a while.
	done, cancel := context.WithCancel(ctx)
	cancel()
	fake.mu.Lock()
	fake.editDelay = 60 * time.Millisecond
	fake.mu.Unlock()
	start := time.Now()
	r.finishCardsAtExit(done)
	took := time.Since(start)

	for _, conv := range convs {
		got, ok := fake.now(cards[conv])
		if !ok || got.Activity == nil || got.Activity.Title != "Interrupted by a restart · 2 calls" {
			t.Fatalf("%s: card = %+v (standing: %t), want it finalised as interrupted before finishCardsAtExit returns", conv, got.Activity, ok)
		}
		if got.Activity.Status != chat.StepFailed || got.Kind != chat.KindActivity || got.Activity.Recent != nil {
			t.Errorf("%s: card = %+v (%s)", conv, got.Activity, got.Kind)
		}
		if st := statuses(got.Activity.Steps); len(st) != 2 || st[0] != chat.StepDone || st[1] != chat.StepUnfinished {
			t.Errorf("%s: statuses = %v, want the answered call done and the running one unfinished", conv, st)
		}
		if !strings.HasPrefix(got.Text, markFailed+" Interrupted by a restart · 2 calls\n") || !strings.Contains(got.Text, "\n– `read`  `two`") {
			t.Errorf("%s: text = %q", conv, got.Text)
		}
		// Out of service, so the last snapshot does not name it.
		if rec := entries[conv].record(); rec.Placeholder != "" || rec.Card != nil {
			t.Errorf("%s: the record still names placeholder %q, card %+v", conv, rec.Placeholder, rec.Card)
		}
	}
	// All at once, not one after another: five edits of 60ms each.
	if took > 200*time.Millisecond {
		t.Errorf("finalising %d cards took %s; they are meant to go out together", len(convs), took)
	}
	if spin := fake.spinning(); len(spin) != 0 {
		t.Errorf("messages %v are still drawn as running after shutdown", spin)
	}
	// The clock is left exactly as it was.
	if fake.wasDeleted(clock) || fake.tried(clock) != 0 {
		t.Errorf("the clock was touched at shutdown (deleted: %t, edits: %d)", fake.wasDeleted(clock), fake.tried(clock))
	}
	if rec := clockEntry.record(); rec.Placeholder != clock {
		t.Errorf("the clock's record names %q, want it still named for the next process to clear", rec.Placeholder)
	}
	if fake.anyDeleted() {
		t.Error("a message was deleted at shutdown")
	}
}

// A card that cannot be finalised at shutdown stays named in the snapshot,
// with its calls, for the next process to finalise; and the log says so.
func TestShutdownLeavesACardItCannotFinaliseForTheNextProcess(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStatus)
	sink := &logSink{}
	r.logf = sink.logf
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	fake.setHooks(func(chat.MessageRef, chat.Reply) error { return errors.New("edit refused") }, nil)

	r.finishCardsAtExit(ctx)

	if len(warnings(sink, "could not be finalised")) != 1 {
		t.Errorf("warnings = %v, want one naming the card", sink.matching("progress"))
	}
	rec := e.record()
	if rec.Placeholder != card || rec.Card == nil || rec.Card.Calls != 1 {
		t.Errorf("record names placeholder %q and card %+v; want both, for the next process", rec.Placeholder, rec.Card)
	}
	if fake.anyDeleted() {
		t.Error("the card was deleted")
	}
}

// A finalising edit still being retried when the process is told to stop is
// waited for: shutdown does not return while a card it could still finalise
// is on its way.
func TestShutdownWaitsForAFinalisingEditInFlight(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStatus)
	r.cardRetry = 120 * time.Millisecond
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	// The platform refuses edits for a while, and takes them again before
	// the retry is due. A shutdown that did not wait for that retry would
	// make its own one attempt now, inside the refusal, and go.
	down := time.Now().Add(60 * time.Millisecond)
	fake.setHooks(func(chat.MessageRef, chat.Reply) error {
		if time.Now().Before(down) {
			return errors.New("edit refused")
		}
		return nil
	}, nil)
	r.retireProgress(ctx, e, cardConv, true) // the answer landed; the edit failed
	if got, _ := fake.now(card); got.Activity == nil || got.Activity.Status != chat.StepRunning {
		t.Fatalf("the card = %+v, want it still as last drawn while the retry waits", got.Activity)
	}
	r.finishCardsAtExit(ctx)
	if got, _ := fake.now(card); got.Activity == nil || !strings.HasPrefix(got.Activity.Title, "Done · ") {
		t.Errorf("after shutdown the card = %+v, want the retry to have landed first", got.Activity)
	}
	if n := len(r.snapshot().Owed); n != 0 {
		t.Errorf("%d cards owed after the retry landed", n)
	}
}

// A restart finalises every persisted card, whatever becomes of its record: a
// record too old to re-attach, one past the age at which it is forgotten, one
// that cannot be read, one whose task was deleted. Finalising used to wait for
// the session to be revived, and each of these never was.
func TestRestoreFinalisesEveryPersistedCard(t *testing.T) {
	d := &boundDaemon{resumed: make(chan string, 16)}
	r, _ := boundRouter(t, d)
	fake := newCardFake()
	r.out = fake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := time.Now()
	card := func() *cardRecord {
		return &cardRecord{Calls: 2, Steps: []stepRecord{{Title: "bash", Status: "done"}, {Title: "read", Status: "running"}}}
	}
	records := map[string]sessionRecord{
		"C0:recent":     {Session: "core-agent/s1", Owner: "alice@example.com", Touched: now},
		"C0:dormant":    {Session: "core-agent/s2", Owner: "alice@example.com", Touched: now.Add(-2 * reviveWindow)},
		"C0:forgotten":  {Session: "core-agent/s3", Owner: "alice@example.com", Touched: now.Add(-2 * dormantMaxAge)},
		"C0:unreadable": {Session: "not a session ref", Owner: "alice@example.com", Touched: now},
		"C0:deleted":    {Session: "core-agent/s5", Owner: "alice@example.com", Touched: now, Task: "task-5", TaskDeleted: true, TaskDeletedReason: "quiet"},
	}
	ids := map[string]string{}
	fake.mu.Lock()
	for conv, rec := range records {
		id := "ts-" + conv
		ids[conv] = id
		rec.Placeholder, rec.Card = id, card()
		records[conv] = rec
		fake.order = append(fake.order, id)
		fake.posted[id] = chat.Reply{Conversation: conv, Text: workingText}
	}
	fake.mu.Unlock()

	r.restore(ctx, routerState{Version: stateVersion, Sessions: records}, now)

	for conv, id := range ids {
		got := waitCard(t, fake, id, "its interrupted state ("+conv+")", hasTitle(`^Interrupted by a restart · 2 calls$`))
		if st := statuses(got.Activity.Steps); len(st) != 2 || st[0] != chat.StepDone || st[1] != chat.StepUnfinished {
			t.Errorf("%s: statuses = %v", conv, st)
		}
	}
	if fake.anyDeleted() {
		t.Error("a card was deleted")
	}
	// Each exactly once: the revive of the recent one does not touch it again.
	waitFor(t, func() bool { return len(r.owedCards()) == 0 }, "cards are still owed after every one was finalised")
	for conv, id := range ids {
		if n := fake.tried(id); n != 1 {
			t.Errorf("%s: edited %d times, want once", conv, n)
		}
	}
	// And no record names one any more.
	r.mu.Lock()
	for conv, rec := range r.dormant {
		if rec.Placeholder != "" || rec.Card != nil {
			t.Errorf("dormant record %s still names placeholder %q, card %+v", conv, rec.Placeholder, rec.Card)
		}
	}
	_, forgot := r.dormant["C0:forgotten"]
	_, unread := r.dormant["C0:unreadable"]
	r.mu.Unlock()
	if forgot || unread {
		t.Error("a record that should have been dropped was kept")
	}
	for conv, rec := range r.snapshot().Sessions {
		if rec.Placeholder != "" || rec.Card != nil {
			t.Errorf("the snapshot's record for %s still names placeholder %q, card %+v", conv, rec.Placeholder, rec.Card)
		}
	}
}

// A conversation in a task is re-attached at boot only if a turn was in
// flight. Taking the card off its record to finalise it must not make it look
// as if none was.
func TestAnInFlightTaskConversationStillReattachesAtBoot(t *testing.T) {
	for name, tc := range map[string]struct {
		rec         sessionRecord
		wantRevived int
	}{
		"a card in flight":  {sessionRecord{Placeholder: "ts-1", Card: &cardRecord{Calls: 1, Steps: []stepRecord{{Title: "bash", Status: "running"}}}}, 1},
		"a clock in flight": {sessionRecord{Placeholder: "ts-1"}, 1},
		// A shutdown finalised the turn's card and took it out of service:
		// no placeholder, and the turn is still in flight.
		"a card finalised at exit": {sessionRecord{InFlight: true}, 1},
		"nothing in flight":        {sessionRecord{}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			d := &boundDaemon{resumed: make(chan string, 16)}
			r, _ := boundRouter(t, d)
			r.out = newCardFake()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := tc.rec
			rec.Session, rec.Owner, rec.Touched, rec.Task = "core-agent/s1", "alice@example.com", time.Now(), "task-1"
			revived, _ := r.restore(ctx, routerState{Version: stateVersion, Sessions: map[string]sessionRecord{"C0:1": rec}}, time.Now())
			if revived != tc.wantRevived {
				t.Errorf("re-attached %d conversation(s) at boot, want %d", revived, tc.wantRevived)
			}
		})
	}
}

// After a failed edit the card's redraws stand off, longer each time, however
// fast the frames keep coming — a conversation that is being rate limited is
// not answered with more edits at the same rate — and the first edit that
// lands ends the stand-off.
func TestCardRedrawsBackOffAfterAFailedEdit(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStream)
	const interval = 10 * time.Millisecond
	r.cardRedraw = interval
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall("c0", "tool_0", "x")})
	waitCard(t, fake, card, "its first call", hasTitle(` · 1 call · tool_0$`))

	fake.setHooks(func(chat.MessageRef, chat.Reply) error { return errors.New("rate limited") }, nil)
	before := fake.tried(card)
	const window = 400 * time.Millisecond
	deadline := time.Now().Add(window)
	n := 1
	for time.Now().Before(deadline) {
		r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall(fmt.Sprintf("c%d", n), fmt.Sprintf("tool_%d", n), "x")})
		n++
		time.Sleep(2 * time.Millisecond)
	}
	attempts := fake.tried(card) - before
	// At the plain interval this window holds forty. Doubling from it —
	// 20ms, 40, 80, 160, 320 — it holds five or six.
	if attempts > 9 {
		t.Errorf("%d edits attempted in %s of failures, want them backing off (%d frames arrived)", attempts, window, n)
	}
	if attempts < 2 {
		t.Errorf("%d edits attempted in %s; a failed edit is meant to be tried again", attempts, window)
	}

	// The edit lands again: the state that was owed is drawn without another
	// frame to ask for it, and the stand-off is over.
	fake.setHooks(nil, nil)
	waitCard(t, fake, card, "every call, once edits land again", func(rep chat.Reply) bool {
		return rep.Activity != nil && len(rep.Activity.Steps) == n
	})
	e.pmu.Lock()
	standoff := e.redrawBackoff
	e.pmu.Unlock()
	if standoff != 0 {
		t.Errorf("stand-off after a successful edit = %s, want none", standoff)
	}
	last := fmt.Sprintf("tool_%d", n)
	time.Sleep(2 * interval)
	asked := time.Now()
	r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall("last", last, "x")})
	waitCard(t, fake, card, "the next call", func(rep chat.Reply) bool {
		return rep.Activity != nil && strings.HasSuffix(titles(rep.Activity.Steps), last)
	})
	if d := time.Since(asked); d > 100*time.Millisecond {
		t.Errorf("the next call took %s to draw; the stand-off did not end", d)
	}
}

// Moving the progress message — a split, a re-anchor — and finalising a card
// each change what the snapshot names, so each schedules a write. Unsaid, a
// crash before the next thing that dirtied the state had the next process
// finalise the wrong message.
func TestChangingTheProgressMessageMarksTheStateDirty(t *testing.T) {
	dirtied := func(r *Router) bool {
		select {
		case <-r.dirty:
			return true
		default:
			return false
		}
	}
	setup := func(t *testing.T, mode ProgressMode) (*Router, *cardFake, *sessionEntry, context.Context, string) {
		r, fake, e, ctx := cardUnit(t, mode)
		r.dirty = make(chan struct{}, 1)
		card := begin(t, r, ctx, fake, e, cardConv)
		return r, fake, e, ctx, card
	}

	t.Run("a split", func(t *testing.T) {
		r, fake, e, ctx, card := setup(t, ProgressStream)
		r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall("a", "bash", "one")})
		r.cardToolResults(ctx, e, cardConv, ProgressStream, []daemon.ToolResult{{ID: "a", Name: "bash"}})
		waitCard(t, fake, card, "its call ticked off", func(rep chat.Reply) bool { return rep.Activity != nil && !anyRunning(rep.Activity.Steps) })
		dirtied(r)
		e.emu.Lock()
		split, _ := r.splitCard(ctx, e, cardConv, ProgressStream)
		e.emu.Unlock()
		if !split || progressID(e) == card {
			t.Fatalf("the card was not split (split=%t, progress message %q)", split, progressID(e))
		}
		if !dirtied(r) {
			t.Error("a split moved the progress message and scheduled no write")
		}
	})
	t.Run("a card moved whole", func(t *testing.T) {
		r, fake, e, ctx, card := setup(t, ProgressStream)
		r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall("a", "bash", "one")})
		waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
		dirtied(r)
		r.reanchorProgress(ctx, e, cardConv)
		if progressID(e) == card {
			t.Fatal("the card was not moved")
		}
		if !dirtied(r) {
			t.Error("a re-anchor moved the progress message and scheduled no write")
		}
	})
	t.Run("a clock re-anchored", func(t *testing.T) {
		r, _, e, ctx, clock := setup(t, ProgressIndicator)
		dirtied(r)
		r.reanchorProgress(ctx, e, cardConv)
		if progressID(e) == clock {
			t.Fatal("the clock was not moved")
		}
		if !dirtied(r) {
			t.Error("a re-anchor moved the progress message and scheduled no write")
		}
	})
	t.Run("a card finalised", func(t *testing.T) {
		r, fake, e, ctx, card := setup(t, ProgressStatus)
		r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
		waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
		dirtied(r)
		r.settleCard(ctx, e, cardConv, true)
		if !dirtied(r) {
			t.Error("finalising a card scheduled no write: a restart would finalise it again, as interrupted")
		}
	})
}

// A trailing redraw belongs to the turn that owed it. One whose turn has been
// reset since — its timer already fired, and was waiting — must neither draw
// into the next turn's message nor take the redraw that turn is owed.
func TestAStaleTrailingRedrawIsDropped(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStream)
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	// Let any redraw of the setup's own finish.
	time.Sleep(5 * time.Millisecond)

	owed := time.NewTimer(time.Hour)
	defer owed.Stop()
	e.pmu.Lock()
	if e.redrawTimer != nil {
		e.redrawTimer.Stop()
	}
	e.redrawGen = 7
	e.redrawTimer = owed
	e.pmu.Unlock()
	before := fake.tried(card)

	r.trailingRedraw(ctx, e, cardConv, 6) // an earlier turn's timer, firing late
	e.pmu.Lock()
	still := e.redrawTimer == owed
	e.pmu.Unlock()
	if fake.tried(card) != before {
		t.Error("a stale trailing redraw edited the message")
	}
	if !still {
		t.Error("a stale trailing redraw took the redraw the current turn is owed")
	}

	r.trailingRedraw(ctx, e, cardConv, 7) // the current one
	e.pmu.Lock()
	cleared := e.redrawTimer == nil
	e.pmu.Unlock()
	if fake.tried(card) != before+1 || !cleared {
		t.Errorf("the current trailing redraw: %d edits, timer cleared %t; want one edit and the redraw no longer owed", fake.tried(card)-before, cleared)
	}
}

// A queued turn taking up a stranded placeholder re-dates it and starts its
// clock. Not one that is a card holding calls: that would redraw it as a bare
// clock and take its calls out of the thread.
func TestResumingDoesNotTakeOverACardThatHoldsCalls(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStatus)
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	e.stopTicker() // stranded: a message with no clock running

	if _, ok := e.resumeTurnClock(time.Now()); ok {
		t.Error("a card that holds calls was taken over as the next turn's clock")
	}
	e.pmu.Lock()
	holds, total := e.card.holds(), e.card.total
	e.pmu.Unlock()
	if !holds || total != 1 {
		t.Errorf("the card's record was reset (holds %t, %d calls)", holds, total)
	}

	// A stranded clock is still taken over, as it always was.
	_, _, e2, _ := cardUnit(t, ProgressStatus)
	e2.beginTurn(chat.MessageRef{ID: "clock"}, time.Now())
	e2.stopTicker()
	if _, ok := e2.resumeTurnClock(time.Now()); !ok {
		t.Error("a stranded clock was not taken over")
	}
}

// A split posts and freezes before it swaps the new message in, and a later
// turn can take the slot in between. The swap is a compare-and-swap: the later
// turn's message stays the progress message, its record is not touched, and
// the message the split posted is taken back out.
func TestASplitDoesNotOverwriteALaterTurnsMessage(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStream)
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall("a", "bash", "one")})
	r.cardToolResults(ctx, e, cardConv, ProgressStream, []daemon.ToolResult{{ID: "a", Name: "bash"}})
	r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall("b", "read", "two")})
	waitCard(t, fake, card, "both calls", hasTitle(` · 2 calls · read$`))

	// While the split's freezing edit is in the air, a later turn takes the
	// slot, and records a call of its own.
	later := chat.MessageRef{Conversation: cardConv, ID: "later"}
	took := false
	fake.setHooks(func(ref chat.MessageRef, rep chat.Reply) error {
		if ref.ID == card && !took && rep.Activity != nil && rep.Activity.Status == chat.StepDone {
			took = true
			e.supersede(later, time.Now())
			e.noteActivity([]daemon.ToolCall{toolCall("z", "write", "three")}, true, true)
		}
		return nil
	}, nil)
	e.emu.Lock()
	r.splitCard(ctx, e, cardConv, ProgressStream)
	e.emu.Unlock()

	if !took {
		t.Fatal("the split never made its freezing edit")
	}
	if got := progressID(e); got != later.ID {
		t.Errorf("the progress message is %q, want the later turn's", got)
	}
	e.pmu.Lock()
	rec := titles(cardSteps(e.card.calls, cardLive))
	e.pmu.Unlock()
	if rec != "write" {
		t.Errorf("the later turn's record holds %q, want its own call and nothing taken from it", rec)
	}
	ids := fake.ids()
	if len(ids) != 2 || !fake.wasDeleted(ids[1]) {
		t.Errorf("messages %v (second deleted: %t), want the one the split posted taken back out", ids, len(ids) > 1 && fake.wasDeleted(ids[1]))
	}

	// And directly: a swap against a message that is not the current one
	// changes nothing.
	plan := &cardFreeze{}
	if e.commitSplit(chat.MessageRef{ID: card}, chat.MessageRef{ID: "fresh"}, plan) {
		t.Error("commitSplit swapped against a message that is no longer current")
	}
	if e.commitSplit(chat.MessageRef{}, chat.MessageRef{ID: "fresh"}, plan) {
		t.Error("commitSplit swapped against no message")
	}
	if got := progressID(e); got != later.ID {
		t.Errorf("the progress message is %q after a refused swap", got)
	}
}

// A ticker that reaches its deadline with no turn boundary stops the clock. A
// clock is left where it stopped. A card would be left spinning, so it is
// finalised as stopped.
func TestTheTickDeadlineStopsACard(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStatus)
	r.tickInterval = time.Hour // a ticker, which never ticks before its deadline
	r.tickMaxAge = 50 * time.Millisecond
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))

	kept := waitCard(t, fake, card, "its stopped state", hasTitle(`^Stopped · \d+s · 1 call$`))
	if kept.Activity.Status != chat.StepFailed || anyRunning(kept.Activity.Steps) {
		t.Errorf("kept = %+v", kept.Activity)
	}
	if got := progressID(e); got != "" {
		t.Errorf("the entry still tracks %q", got)
	}
	if fake.anyDeleted() {
		t.Error("a message was deleted")
	}
}

// The backstop that gives a turn up after it narrated and then never ended
// stops the clock too, and so must finalise a card.
func TestTheTurnBackstopStopsACard(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStatus)
	r.tickMaxAge = 50 * time.Millisecond // and no ticker: this is the backstop's own timer
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))

	r.armTurnBackstop(ctx, e, cardConv)
	kept := waitCard(t, fake, card, "its stopped state", hasTitle(`^Stopped · \d+s · 1 call$`))
	if kept.Activity.Status != chat.StepFailed || anyRunning(kept.Activity.Steps) {
		t.Errorf("kept = %+v", kept.Activity)
	}
	if e.turnInFlight() {
		t.Error("the turn is still in flight after its backstop fired")
	}
}

// Past its deadline a turn has no ticker. If it then makes its first call, the
// card that draws has nothing to finalise it should the turn's boundary never
// come — the deadline that would have was spent on a clock. So the card gets a
// ticker of its own, and that one's deadline stops it.
func TestACardStartedAfterTheTickerGaveUpGetsATickerOfItsOwn(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStatus)
	r.tickInterval = 15 * time.Millisecond
	r.tickMaxAge = 60 * time.Millisecond
	clock := begin(t, r, ctx, fake, e, cardConv)
	waitFor(t, func() bool {
		e.pmu.Lock()
		defer e.pmu.Unlock()
		return e.tickDead
	}, "the ticker never reached its deadline")
	if fake.wasDeleted(clock) || progressID(e) != clock {
		t.Fatal("the clock was not left where it stopped")
	}
	ticked := fake.tried(clock)

	r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, clock, "its first call", hasTitle(` · 1 call · bash$`))
	// It ticks again...
	waitFor(t, func() bool { return fake.tried(clock) >= ticked+3 }, "the card is not being ticked")
	// ...and the new ticker's deadline finalises it.
	kept := waitCard(t, fake, clock, "its stopped state", hasTitle(`^Stopped · \d+s · 1 call$`))
	if anyRunning(kept.Activity.Steps) || kept.Activity.Status != chat.StepFailed {
		t.Errorf("kept = %+v", kept.Activity)
	}
}

// The daemon's boundary stops the clock and then finalises the card, and a new
// message can take the slot between the two. The card it takes over from then
// belongs to a turn that completed, and says so; one whose clock was stopped
// by a deadline giving up on it still says stopped.
func TestACardTakenOverAfterItsBoundaryKeepsHowItsTurnEnded(t *testing.T) {
	for name, tc := range map[string]struct {
		stop func(*sessionEntry)
		want string
	}{
		"turn-complete":          {func(e *sessionEntry) { e.stopTickerAt(true) }, `^Done · \d+s · 1 call$`},
		"idle, no turn-complete": {func(e *sessionEntry) { e.stopTickerAt(false) }, `^Stopped · \d+s · 1 call$`},
		"given up on":            {func(e *sessionEntry) { e.stopTicker() }, `^Stopped · \d+s · 1 call$`},
	} {
		t.Run(name, func(t *testing.T) {
			r, fake, e, ctx := cardUnit(t, ProgressStatus)
			card := begin(t, r, ctx, fake, e, cardConv)
			r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
			r.cardToolResults(ctx, e, cardConv, ProgressStatus, []daemon.ToolResult{{ID: "a", Name: "bash"}})
			waitCard(t, fake, card, "its call ticked off", func(rep chat.Reply) bool { return rep.Activity != nil && !anyRunning(rep.Activity.Steps) })

			tc.stop(e)                           // the boundary stops the clock...
			r.startProgress(ctx, e, cardConv)    // ...a new message takes the slot...
			r.settleCard(ctx, e, cardConv, true) // ...and the boundary's own settle finds nothing left
			waitCard(t, fake, card, "how its turn ended", hasTitle(tc.want))
			if fake.wasDeleted(card) {
				t.Error("the card was deleted")
			}
		})
	}
}

// A redraw that fails is tried again on its own, once the stand-off it earned
// is over: the change that asked for it is still undrawn, and there may be no
// other frame, and no tick, to bring it out.
func TestAFailedRedrawIsTriedAgainWithoutAnotherFrame(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStream) // no ticker
	r.cardRedraw = 10 * time.Millisecond
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, card, "its first call", hasTitle(` · 1 call · bash$`))
	time.Sleep(3 * r.cardRedraw) // so the next request draws at once

	failures := 1
	fake.setHooks(func(chat.MessageRef, chat.Reply) error {
		if failures > 0 {
			failures--
			return errors.New("edit refused")
		}
		return nil
	}, nil)
	before := fake.tried(card)
	r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall("b", "read", "two")})
	if got := fake.tried(card) - before; got != 1 {
		t.Fatalf("%d edits attempted for the second call, want the one that failed", got)
	}
	// Nothing else arrives. The second call is drawn all the same.
	got := waitCard(t, fake, card, "the call whose redraw failed", hasTitle(` · 2 calls · bash, read$`))
	if titles(got.Activity.Steps) != "bash read" {
		t.Errorf("card holds %s", titles(got.Activity.Steps))
	}
}

// The waits between attempts at a finalising edit grow: each is twice the
// last.
func TestFinalisingRetriesBackOff(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStatus)
	const first = 25 * time.Millisecond
	r.cardRetry = first
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	var at []time.Time
	fake.setHooks(func(chat.MessageRef, chat.Reply) error {
		at = append(at, time.Now())
		return errors.New("edit refused")
	}, nil)
	r.retireProgress(ctx, e, cardConv, true)
	waitFor(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(at) == cardFinishAttempts
	}, "the attempts were never all made")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for i, want := range []time.Duration{first, 2 * first, 4 * first} {
		if gap := at[i+1].Sub(at[i]); gap < want {
			t.Errorf("wait before attempt %d was %s, want at least %s", i+2, gap, want)
		}
	}
}

// A shutdown that finalises a turn's card takes it out of service, and with it
// the placeholder that told the next process a turn was in flight. The record
// says so itself, so a conversation in a task still re-attaches at boot and
// its answer does not wait for somebody to speak — and only then: a turn that
// has ended, a clock, a turn with no placeholder at all record what they did.
func TestACardFinalisedAtExitStillRecordsItsTurnInFlight(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStatus)
	e.task = "task-1"
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	if rec := e.record(); rec.Placeholder != card || rec.InFlight {
		t.Fatalf("while running: placeholder %q, in flight %t; want the placeholder to say it", rec.Placeholder, rec.InFlight)
	}

	r.finishCardsAtExit(ctx)
	waitCard(t, fake, card, "its interrupted state", hasTitle(`^Interrupted by a restart · 1 call$`))
	rec := e.record()
	if rec.Placeholder != "" || rec.Card != nil {
		t.Errorf("after exit the record names placeholder %q, card %+v; want neither: it is finalised", rec.Placeholder, rec.Card)
	}
	if !rec.InFlight {
		t.Fatal("after exit the record no longer says a turn is in flight")
	}

	// The next process re-attaches it at boot, through the state file.
	raw, err := json.Marshal(routerState{Version: stateVersion, Sessions: map[string]sessionRecord{"C0:1": rec}})
	if err != nil {
		t.Fatal(err)
	}
	var st routerState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	rec = st.Sessions["C0:1"]
	rec.Session, rec.Owner, rec.Touched = "core-agent/s1", "alice@example.com", time.Now()
	if rec.Task != "task-1" || !rec.InFlight {
		t.Fatalf("round-tripped record: task %q, in flight %t", rec.Task, rec.InFlight)
	}
	d := &boundDaemon{resumed: make(chan string, 16)}
	next, _ := boundRouter(t, d)
	next.out = newCardFake()
	nctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if revived, _ := next.restore(nctx, routerState{Version: stateVersion, Sessions: map[string]sessionRecord{"C0:1": rec}}, time.Now()); revived != 1 {
		t.Errorf("re-attached %d conversation(s) at boot, want the one whose card was finalised at exit", revived)
	}

	// What does not say so.
	t.Run("a turn that ended", func(t *testing.T) {
		r, fake, e, ctx := cardUnit(t, ProgressStatus)
		card := begin(t, r, ctx, fake, e, cardConv)
		r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
		waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
		e.endTurn()
		r.settleCard(ctx, e, cardConv, true)
		r.finishCardsAtExit(ctx)
		if rec := e.record(); rec.InFlight || rec.Placeholder != "" {
			t.Errorf("a turn that ended records in flight %t, placeholder %q", rec.InFlight, rec.Placeholder)
		}
	})
	t.Run("a clock", func(t *testing.T) {
		r, fake, e, ctx := cardUnit(t, ProgressStatus)
		clock := begin(t, r, ctx, fake, e, cardConv)
		r.finishCardsAtExit(ctx)
		if rec := e.record(); rec.InFlight || rec.Placeholder != clock {
			t.Errorf("a clock records in flight %t, placeholder %q; want its placeholder to go on saying it, as before", rec.InFlight, rec.Placeholder)
		}
	})
	t.Run("a turn with no placeholder", func(t *testing.T) {
		r, _, e, ctx := cardUnit(t, ProgressOff)
		e.beginTurnInFlight()
		r.finishCardsAtExit(ctx)
		if rec := e.record(); rec.InFlight || rec.Placeholder != "" {
			t.Errorf("a turn with no placeholder records in flight %t; outside card mode the record is what it always was", rec.InFlight)
		}
	})
	t.Run("the next turn", func(t *testing.T) {
		// The flag is the turn's: a turn that begins afterwards starts clear.
		e.beginTurnInFlight()
		if e.record().InFlight {
			t.Error("a later turn inherited the flag")
		}
	})
}

// exhaust ends a turn whose one call is drawn, with every edit failing as
// transient errors do, and waits for the attempts to run out.
func exhaust(t *testing.T, r *Router, fake *cardFake, e *sessionEntry, ctx context.Context) (card string) {
	t.Helper()
	card = begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))
	before := fake.tried(card)
	fake.setHooks(func(chat.MessageRef, chat.Reply) error { return errors.New("slack is down") }, nil)
	e.endTurn()
	r.retireProgress(ctx, e, cardConv, true)
	waitFor(t, func() bool {
		r.fmu.Lock()
		defer r.fmu.Unlock()
		return fake.tried(card)-before == cardFinishAttempts && r.finishing == 0
	}, "the attempts at the finalising edit never ran out")
	return card
}

// A finalising edit that runs out of attempts is not forgotten: the card is
// owed it, on a list of its own in the snapshot, and the next process makes
// it — as what the turn's end was, not as a restart. Owing it does not touch
// the conversation's record: a later turn's placeholder is its own, and a turn
// that has ended does not read as one in flight.
func TestACardWhoseLastEditRanOutOfAttemptsIsOwedAcrossARestart(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStream)
	e.task = "task-1"
	card := exhaust(t, r, fake, e, ctx)

	if got, _ := fake.now(card); got.Activity == nil || got.Activity.Status != chat.StepRunning {
		t.Fatalf("the card = %+v; this test is about one left as it was last drawn", got.Activity)
	}
	st := r.snapshot()
	if len(st.Owed) != 1 {
		t.Fatalf("the snapshot owes %d cards, want the one whose last edit never landed", len(st.Owed))
	}
	o := st.Owed[0]
	if o.Conversation != cardConv || o.Message != card || o.Status != "done" || o.Since.IsZero() ||
		!regexp.MustCompile(`^Done · \d+s · 1 call$`).MatchString(o.Title) ||
		len(o.Steps) != 1 || o.Steps[0].Title != "bash" || o.Steps[0].Status != "done" || o.Steps[0].Code != "one" {
		t.Errorf("owed = %+v, want the finished card the turn ended as", o)
	}
	// The conversation's record is not touched: the turn has ended.
	if rec := st.Sessions[cardConv]; rec.Placeholder != "" || rec.Card != nil || rec.InFlight {
		t.Errorf("the record names placeholder %q, card %+v, in flight %t; an owed card is none of those", rec.Placeholder, rec.Card, rec.InFlight)
	}

	// A later turn on the same conversation has the slot to itself.
	fake.setHooks(nil, nil)
	second := begin(t, r, ctx, fake, e, cardConv)
	st = r.snapshot()
	if rec := st.Sessions[cardConv]; rec.Placeholder != second || rec.Card != nil {
		t.Errorf("the record names placeholder %q, card %+v; want the later turn's clock", rec.Placeholder, rec.Card)
	}
	if len(st.Owed) != 1 || st.Owed[0].Message != card {
		t.Errorf("the snapshot owes %+v; want the first turn's card still", st.Owed)
	}

	// Through the state file, into the next process.
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back routerState
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	// The ended turn's own record, as it was before the later turn began:
	// in a task, and nothing in flight. It must not be woken at boot.
	ended := sessionRecord{Session: "core-agent/s1", Owner: "alice@example.com", Touched: time.Now(), Task: "task-1"}
	d := &boundDaemon{resumed: make(chan string, 16)}
	next, _ := boundRouter(t, d)
	nfake := newCardFake()
	nfake.order = append(nfake.order, card)
	nfake.posted[card] = chat.Reply{Conversation: cardConv, Text: workingText}
	next.out = nfake
	nctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	revived, _ := next.restore(nctx, routerState{Version: stateVersion, Sessions: map[string]sessionRecord{cardConv: ended}, Owed: back.Owed}, time.Now())
	if revived != 0 {
		t.Errorf("an owed card woke %d task conversation(s) whose turn had ended", revived)
	}
	kept := waitCard(t, nfake, card, "the edit it was owed", hasTitle(`^Done · \d+s · 1 call$`))
	if kept.Activity.Status != chat.StepDone || kept.Kind != chat.KindActivity || titles(kept.Activity.Steps) != "bash" ||
		kept.Activity.Steps[0].Status != chat.StepDone || kept.Activity.Steps[0].Code != "one" {
		t.Errorf("kept = %+v (%s)", kept.Activity, kept.Kind)
	}
	if !strings.HasPrefix(kept.Text, markOK+" Done · ") || strings.Contains(kept.Text, "restart") {
		t.Errorf("kept text = %q, want the turn's own ending and nothing about a restart", kept.Text)
	}
	waitFor(t, func() bool { return len(next.snapshot().Owed) == 0 }, "the card is still owed after its edit landed")
	if nfake.anyDeleted() {
		t.Error("a message was deleted")
	}
}

// A card still owed its last edit is tried once more as the process exits: an
// outage long over does not leave it spinning until the next boot.
func TestShutdownRetriesACardStillOwedItsLastEdit(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStream)
	sink := &logSink{}
	r.logf = sink.logf
	card := exhaust(t, r, fake, e, ctx)

	// Still down: it stays owed, and the log says which.
	r.finishCardsAtExit(ctx)
	if len(r.snapshot().Owed) != 1 {
		t.Fatalf("after a shutdown that could not reach the platform, %d cards owed, want the one", len(r.snapshot().Owed))
	}
	if warn := warnings(sink, "still owed its last edit"); len(warn) != 1 || !strings.Contains(warn[0], cardConv) || !strings.Contains(warn[0], "message "+card+" ") {
		t.Errorf("warnings = %v, want one naming the conversation and the message", warn)
	}
	// Back up.
	fake.setHooks(nil, nil)
	r.finishCardsAtExit(ctx)
	if got, _ := fake.now(card); got.Activity == nil || !strings.HasPrefix(got.Activity.Title, "Done · ") || got.Activity.Status != chat.StepDone {
		t.Errorf("after shutdown the card = %+v, want the edit it was owed", got.Activity)
	}
	if n := len(r.snapshot().Owed); n != 0 {
		t.Errorf("%d cards still owed after the edit landed", n)
	}
}

// An owed card the platform refuses for good is written off: the message is
// gone, or not ours to edit, and it must not be asked for on every boot for
// ever. A card that is merely old is given up on the same way.
func TestAnOwedCardThePlatformRefusesForGoodIsWrittenOff(t *testing.T) {
	for name, refusal := range map[string]error{"not found": chat.ErrNotFound, "denied": chat.ErrDenied, "unsupported": chat.ErrUnsupported} {
		t.Run(name, func(t *testing.T) {
			d := &boundDaemon{resumed: make(chan string, 16)}
			r, _ := boundRouter(t, d)
			r.cardRetry = time.Millisecond
			fake := newCardFake()
			fake.failEdit = func(chat.MessageRef, chat.Reply) error { return fmt.Errorf("platform says no: %w", refusal) }
			r.out = fake
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.restore(ctx, routerState{
				Version: stateVersion,
				// One a turn's end left owed, and one the last process left in flight.
				Owed: []owedRecord{{Conversation: "C0:9", Message: "ts-owed", Title: "Done · 5s · 1 call", Status: "done",
					Steps: []stepRecord{{Title: "bash", Status: "done"}}, Since: time.Now().Add(-time.Hour)}},
				Sessions: map[string]sessionRecord{"C0:1": {
					Session: "core-agent/s1", Owner: "alice@example.com", Touched: time.Now().Add(-2 * reviveWindow), Placeholder: "ts-flight",
					Card: &cardRecord{Calls: 1, Steps: []stepRecord{{Title: "bash", Status: "running"}}},
				}},
			}, time.Now())

			waitFor(t, func() bool { return len(r.owedCards()) == 0 }, "a card the platform refused for good is still owed")
			for _, id := range []string{"ts-owed", "ts-flight"} {
				if n := fake.tried(id); n != 1 {
					t.Errorf("%s: %d attempts, want one: a permanent refusal is not retried", id, n)
				}
			}
			st := r.snapshot()
			if len(st.Owed) != 0 {
				t.Errorf("the snapshot still owes %+v", st.Owed)
			}
			if rec := st.Sessions["C0:1"]; rec.Placeholder != "" || rec.Card != nil || rec.InFlight {
				t.Errorf("the record names placeholder %q, card %+v, in flight %t", rec.Placeholder, rec.Card, rec.InFlight)
			}
		})
	}

	t.Run("too old", func(t *testing.T) {
		d := &boundDaemon{resumed: make(chan string, 16)}
		r, _ := boundRouter(t, d)
		sink := &logSink{}
		r.logf = sink.logf
		fake := newCardFake()
		r.out = fake
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r.restore(ctx, routerState{Version: stateVersion, Owed: []owedRecord{
			{Conversation: "C0:9", Message: "ts-old", Title: "Done · 5s · 1 call", Status: "done", Since: time.Now().Add(-2 * dormantMaxAge)},
			{Conversation: "C0:9", Message: "ts-recent", Title: "Done · 5s · 1 call", Status: "done", Since: time.Now().Add(-time.Hour)},
			{Conversation: "", Message: "ts-nowhere", Title: "Done"},
		}}, time.Now())
		waitFor(t, func() bool { return fake.tried("ts-recent") == 1 && len(r.owedCards()) == 0 }, "the recent card was not finalised")
		if fake.tried("ts-old") != 0 || fake.tried("ts-nowhere") != 0 {
			t.Errorf("a card past its age, or with nowhere to go, was edited")
		}
		if warn := warnings(sink, "giving up on message ts-old"); len(warn) != 1 || !strings.Contains(warn[0], "C0:9") {
			t.Errorf("warnings = %v, want one naming the card given up on", sink.matching("state:"))
		}
	})
}

// What a restored card is edited into never draws as running, whatever the
// file says: it is the last edit the message gets.
func TestAnOwedRecordNeverRestoresAsRunning(t *testing.T) {
	for status, want := range map[string]chat.StepStatus{"done": chat.StepDone, "failed": chat.StepFailed, "running": chat.StepDone, "": chat.StepDone, "odd": chat.StepDone} {
		act := owedRecord{Title: "t", Status: status, Steps: []stepRecord{
			{Title: "a", Status: "done"}, {Title: "b", Status: "failed"}, {Title: "c", Status: "unfinished"},
			{Title: "d", Status: "running", Steps: []stepRecord{{Title: "e", Status: "running"}}}, {Title: "f", Status: "odd"},
		}}.activity()
		if act.Status != want {
			t.Errorf("status %q restored as %s, want %s", status, act.Status, want)
		}
		got := statuses(act.Steps)
		wantSteps := []chat.StepStatus{chat.StepDone, chat.StepFailed, chat.StepUnfinished, chat.StepUnfinished, chat.StepUnfinished, chat.StepUnfinished}
		if fmt.Sprint(got) != fmt.Sprint(wantSteps) {
			t.Errorf("steps restored as %v, want %v", got, wantSteps)
		}
	}
	// And it round-trips what the router owed, the ref's own key included.
	o := &owedCard{conv: "C0:1", ref: chat.MessageRef{Conversation: "C0:1:landed", ID: "m1"}, since: time.Now(),
		act: &chat.Activity{Title: "Stopped · 5s · 2 calls", Status: chat.StepFailed, Steps: []chat.Step{
			{Title: "_fallback", Status: chat.StepFailed, Calls: 1, Code: "x", Note: "2.0s", Steps: []chat.Step{{Title: "grep", Status: chat.StepUnfinished}}}}}}
	rec := o.record()
	if rec.In != "C0:1:landed" || rec.Message != "m1" || rec.Conversation != "C0:1" {
		t.Errorf("record = %+v", rec)
	}
	back := rec.activity()
	if back.Title != o.act.Title || back.Status != chat.StepFailed || titles(back.Steps) != "_fallback[grep]" ||
		back.Steps[0].Calls != 1 || back.Steps[0].Code != "x" || back.Steps[0].Note != "2.0s" || back.Steps[0].Steps[0].Status != chat.StepUnfinished {
		t.Errorf("round trip = %+v", back)
	}
	same := (&owedCard{conv: "C0:1", ref: chat.MessageRef{Conversation: "C0:1", ID: "m1"}, act: o.act}).record()
	if same.In != "" {
		t.Errorf("a ref keyed as its conversation recorded In = %q", same.In)
	}
}

// The cards owed their last edit are bounded: past the bound the oldest is
// given up, and named.
func TestOwedCardsAreBounded(t *testing.T) {
	r, _, _, _ := cardUnit(t, ProgressStatus)
	sink := &logSink{}
	r.logf = sink.logf
	t0 := time.Now().Add(-time.Hour)
	act := &chat.Activity{Title: "Done", Status: chat.StepDone}
	for i := 0; i <= maxOwedCards; i++ {
		r.owe("C0:1", chat.MessageRef{Conversation: "C0:1", ID: fmt.Sprintf("m%03d", i)}, act, t0.Add(time.Duration(i)*time.Second))
	}
	owed := r.owedCards()
	if len(owed) != maxOwedCards {
		t.Fatalf("%d cards owed, want the bound of %d", len(owed), maxOwedCards)
	}
	if owed[0].ref.ID != "m001" || owed[len(owed)-1].ref.ID != fmt.Sprintf("m%03d", maxOwedCards) {
		t.Errorf("owed runs %s..%s, want the oldest given up and the rest oldest first", owed[0].ref.ID, owed[len(owed)-1].ref.ID)
	}
	if warn := warnings(sink, "giving up on message m000"); len(warn) != 1 {
		t.Errorf("warnings = %v, want one naming the card given up", sink.matching("progress"))
	}
	// Owing the same message again replaces it, and gives nothing up.
	r.owe("C0:1", chat.MessageRef{Conversation: "C0:1", ID: "m001"}, act, t0)
	if len(r.owedCards()) != maxOwedCards || len(warnings(sink, "giving up")) != 1 {
		t.Errorf("owing a message twice changed the count or gave another up")
	}
}

// A clock being moved below narration can become a card while the message it
// moves to is being posted: the turn's first call lands in between, and draws
// the old message running. It was rendered as a clock, so a failed delete used
// to leave it at that — spinning. What it is is asked at the swap, and a card
// that cannot be deleted is finished in place.
func TestAClockThatBecomesACardMidMoveIsNotLeftSpinning(t *testing.T) {
	for name, alsoFailEdit := range map[string]bool{"the edit lands": false, "the edit fails too": true} {
		t.Run(name, func(t *testing.T) {
			r, fake, e, ctx := cardUnit(t, ProgressStream)
			sink := &logSink{}
			r.logf = sink.logf
			old := begin(t, r, ctx, fake, e, cardConv)

			// While the move's post is in the air, the first call lands and
			// is drawn — into the old message, which is still the slot.
			once := false
			fake.mu.Lock()
			fake.onSend = func(chat.Reply) {
				if once {
					return
				}
				once = true
				r.postActivity(ctx, e, cardConv, ProgressStream, []daemon.ToolCall{toolCall("a", "bash", "one")})
			}
			fake.failDelete = func(chat.MessageRef) error { return errors.New("delete refused") }
			fake.mu.Unlock()
			finishes := 0
			if alsoFailEdit {
				fake.setHooks(func(ref chat.MessageRef, rep chat.Reply) error {
					if ref.ID == old && rep.Activity != nil && rep.Activity.Status != chat.StepRunning {
						finishes++
						return errors.New("edit refused")
					}
					return nil
				}, func(chat.MessageRef) error { return errors.New("delete refused") })
			}

			r.reanchorProgress(ctx, e, cardConv)

			fresh := progressID(e)
			if !once || fresh == old || fresh == "" {
				t.Fatalf("the move did not happen as staged (call landed: %t, progress message %q)", once, fresh)
			}
			if posted := fake.postedReply(fresh); posted.Activity != nil {
				t.Fatalf("the new message was posted as %+v; this test is about a clock rendered before the call landed", posted.Activity)
			}
			if alsoFailEdit {
				if finishes != 1 {
					t.Fatalf("%d attempts to finish the old card, want one", finishes)
				}
				warn := warnings(sink, "it is left drawn as running")
				if len(warn) != 1 || !strings.Contains(warn[0], cardConv) || !strings.Contains(warn[0], "message "+old+" ") {
					t.Errorf("warnings = %v, want one naming the conversation and the message", warn)
				}
				// And it is owed the edit, for the way out and the next boot.
				if owed := r.snapshot().Owed; len(owed) != 1 || owed[0].Message != old || owed[0].Status != "done" {
					t.Errorf("owed = %+v, want the old card", owed)
				}
				return
			}
			left, ok := fake.now(old)
			if !ok || left.Activity == nil {
				t.Fatalf("the old message = %+v (standing: %t)", left, ok)
			}
			if left.Activity.Status != chat.StepDone || anyRunning(left.Activity.Steps) || left.Kind != chat.KindActivity {
				t.Errorf("the old message = %+v (%s); it was drawn as a running card and could not be deleted, so it must be finished", left.Activity, left.Kind)
			}
			if got := statuses(left.Activity.Steps); len(got) != 1 || got[0] != chat.StepUnfinished {
				t.Errorf("its call = %v, want unfinished: it carries on in the new message", got)
			}
			if spin := fake.spinning(); len(spin) != 0 {
				// The new message has not been redrawn yet: it is a clock.
				t.Errorf("messages %v are drawn as running", spin)
			}
			// The call carries on in the new message.
			r.cardToolResults(ctx, e, cardConv, ProgressStream, []daemon.ToolResult{{ID: "a", Name: "bash"}})
			waitCard(t, fake, fresh, "the call, ticked off", func(rep chat.Reply) bool {
				return rep.Activity != nil && titles(rep.Activity.Steps) == "bash" && !anyRunning(rep.Activity.Steps)
			})
		})
	}
}

// A shutdown edits a card and then takes it out of service. A later turn can
// take the slot while that edit is in the air, and what is in the slot then is
// that turn's message, not the card that was edited: it is left where it is.
func TestShutdownDoesNotClearAMessageItDidNotFinalise(t *testing.T) {
	r, fake, e, ctx := cardUnit(t, ProgressStatus)
	card := begin(t, r, ctx, fake, e, cardConv)
	r.postActivity(ctx, e, cardConv, ProgressStatus, []daemon.ToolCall{toolCall("a", "bash", "one")})
	waitCard(t, fake, card, "its call", hasTitle(` · 1 call · bash$`))

	later := chat.MessageRef{Conversation: cardConv, ID: "later"}
	took := false
	fake.setHooks(func(ref chat.MessageRef, rep chat.Reply) error {
		if ref.ID == card && !took && rep.Activity != nil && strings.HasPrefix(rep.Activity.Title, "Interrupted") {
			took = true
			e.supersede(later, time.Now()) // a new turn's placeholder takes the slot
		}
		return nil
	}, nil)
	r.finishCardsAtExit(ctx)

	if !took {
		t.Fatal("the shutdown never edited the card")
	}
	if got := progressID(e); got != later.ID {
		t.Errorf("the progress message is %q, want the later turn's left in place", got)
	}
	if e.leftAtExit.Load() {
		t.Error("the entry says its card was finalised at exit; the slot was not this card's by then")
	}
	if rec := e.record(); rec.Placeholder != later.ID {
		t.Errorf("the record names %q, want the later turn's placeholder", rec.Placeholder)
	}
}

// traceCardFake models Google Chat's capability set: it draws Activity, leaves
// a visible tombstone if Delete is ever called, and caps activity cards at
// maxLines via chat.ActivityLineLimit.
type traceCardFake struct {
	*cardFake
	maxLines int
	deletes  atomic.Int32
}

func (f *traceCardFake) DeleteLeavesTrace() bool { return true }
func (f *traceCardFake) MaxActivityLines() int   { return f.maxLines }
func (f *traceCardFake) Delete(ctx context.Context, ref chat.MessageRef) error {
	f.deletes.Add(1)
	return f.cardFake.Delete(ctx, ref)
}

func traceCardFed(t *testing.T, mode ProgressMode, maxLines int) (*Router, *traceCardFake, cardFeed, context.Context) {
	t.Helper()
	r, base, feed, ctx := cardFed(t, mode)
	tf := &traceCardFake{cardFake: base, maxLines: maxLines}
	r.out = tf
	return r, tf, feed, ctx
}

func traceCardUnit(t *testing.T, mode ProgressMode, maxLines int) (*Router, *traceCardFake, *sessionEntry, context.Context) {
	t.Helper()
	r, base, e, ctx := cardUnit(t, mode)
	tf := &traceCardFake{cardFake: base, maxLines: maxLines}
	r.out = tf
	return r, tf, e, ctx
}

// An adapter implementing chat.ActivityLineLimit sets its own rollover cap
// without touching router.cardMaxLines: 10 distinct calls on a 4-line adapter
// roll over at 4 lines rather than waiting for defaultCardMaxLines (400).
func TestAdapterActivityLineLimitCapsRollover(t *testing.T) {
	r, tf, feed, ctx := traceCardFed(t, ProgressStream, 4)
	if r.cardMaxLines != 0 {
		t.Fatalf("router.cardMaxLines = %d, want 0 so the adapter capability drives rollover", r.cardMaxLines)
	}
	if got := r.cardLineLimit(); got != 4 {
		t.Fatalf("cardLineLimit() = %d, want 4 from MaxActivityLines()", got)
	}

	// Precedence check: explicit router override still wins; zero falls back to default.
	rZero := NewRouter(nil, &traceCardFake{cardFake: newCardFake(), maxLines: 0}, ProgressStream, nil, nil)
	if got := rZero.cardLineLimit(); got != defaultCardMaxLines {
		t.Fatalf("zero MaxActivityLines: cardLineLimit() = %d, want %d", got, defaultCardMaxLines)
	}
	rOverride := NewRouter(nil, &traceCardFake{cardFake: newCardFake(), maxLines: 100}, ProgressStream, nil, nil)
	rOverride.cardMaxLines = 2
	if got := rOverride.cardLineLimit(); got != 2 {
		t.Fatalf("explicit router.cardMaxLines: cardLineLimit() = %d, want 2", got)
	}

	ask(t, r, ctx, tf.cardFake)
	const n = 10
	seq := 1
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("tool_%02d", i)
		feed.sendAgent(callEvent(seq, name, name, fmt.Sprintf("arg-%02d", i)))
		seq++
		feed.sendAgent(resultEvent(seq, name, name))
		seq++
		if i%4 == 3 {
			want := name
			waitFor(t, func() bool {
				for _, m := range tf.thread() {
					if m.Activity != nil && strings.Contains(titles(m.Activity.Steps), want) {
						return true
					}
				}
				return false
			}, "call "+want+" was never drawn")
		}
	}
	feed.sendAgent(textEvent(seq, "done"))
	feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
	waitPosted(t, tf.cardFake, "done")
	waitFor(t, func() bool {
		for _, m := range tf.thread() {
			if m.Activity != nil && strings.HasPrefix(m.Activity.Title, "Done · ") {
				return true
			}
		}
		return false
	}, "final card never settled")

	if got := tf.deletes.Load(); got != 0 {
		t.Errorf("Delete called %d times during rollover, want 0", got)
	}
	var cards []chat.Reply
	for _, m := range tf.thread() {
		if m.Activity != nil {
			cards = append(cards, m)
		}
	}
	if len(cards) < 3 {
		t.Fatalf("10 calls with MaxActivityLines=4 produced %d cards, want >= 3", len(cards))
	}
	for i, c := range cards {
		if lines := stepLines(c.Activity.Steps); lines > 4 {
			t.Errorf("card %d holds %d lines, want <= 4", i, lines)
		}
	}
	if spin := tf.spinning(); len(spin) != 0 {
		t.Errorf("spinning messages left behind: %v", spin)
	}
}

// Where DeleteLeavesTrace() is true (Google Chat, where Delete succeeds and
// leaves a permanent "Message deleted by its author" tombstone), Delete must
// never be called on any card-mode path: no-tool turns, tool turns, narration
// splits, rollovers, failed freezes in splitCard, pre-empted splitCard swaps,
// or raced queuePlaceholder posts.
func TestDeleteLeavesTraceNeverCallsDeleteInCardMode(t *testing.T) {
	t.Run("no-tool turn finalises placeholder in place", func(t *testing.T) {
		r, tf, feed, ctx := traceCardFed(t, ProgressStream, 4)
		clock := ask(t, r, ctx, tf.cardFake)
		feed.sendAgent(textEvent(1, "plain answer"))
		feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
		waitPosted(t, tf.cardFake, "plain answer")
		done := waitCard(t, tf.cardFake, clock, "finalised Done line", func(rep chat.Reply) bool {
			return rep.Kind == chat.KindActivity && strings.HasPrefix(rep.Text, markOK+" Done")
		})
		if done.Activity != nil {
			t.Errorf("no-tool final placeholder got Activity %+v, want plain final line", done.Activity)
		}
		if got := tf.deletes.Load(); got != 0 {
			t.Errorf("Delete called %d times on no-tool turn, want 0", got)
		}
	})

	t.Run("tool turn keeps settled card", func(t *testing.T) {
		r, tf, feed, ctx := traceCardFed(t, ProgressStream, 4)
		card := ask(t, r, ctx, tf.cardFake)
		feed.sendAgent(callEvent(1, "a", "bash", "ls"))
		feed.sendAgent(resultEvent(2, "a", "bash"))
		feed.sendAgent(textEvent(3, "tool answer"))
		feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
		waitPosted(t, tf.cardFake, "tool answer")
		kept := waitCard(t, tf.cardFake, card, "settled card", hasTitle(`^Done · \d+s · 1 call$`))
		if kept.Activity.Status != chat.StepDone {
			t.Errorf("kept card status = %s, want done", kept.Activity.Status)
		}
		if got := tf.deletes.Load(); got != 0 {
			t.Errorf("Delete called %d times on tool turn, want 0", got)
		}
	})

	t.Run("narration both unsplit and split never deletes", func(t *testing.T) {
		r, tf, feed, ctx := traceCardFed(t, ProgressStream, 4)
		r.reanchorDelay = 0
		first := ask(t, r, ctx, tf.cardFake)

		// Unfinished call only: nothing to freeze, and !deletesCleanly() leaves
		// the card where it is instead of post+delete moving it.
		feed.sendAgent(callEvent(1, "a", "bash", "one"))
		waitCard(t, tf.cardFake, first, "first running call", hasTitle(` · 1 call · bash$`))
		feed.sendAgent(textEvent(2, "checking first"))
		waitPosted(t, tf.cardFake, "checking first")

		// Now finish call a, start call b, and narrate again -> splitCard runs!
		feed.sendAgent(resultEvent(3, "a", "bash"))
		feed.sendAgent(callEvent(4, "b", "read", "two"))
		waitCard(t, tf.cardFake, first, "finished a and running b", func(rep chat.Reply) bool {
			return rep.Activity != nil && len(rep.Activity.Steps) == 2 && rep.Activity.Steps[0].Status == chat.StepDone
		})
		feed.sendAgent(textEvent(5, "checking second"))
		waitPosted(t, tf.cardFake, "checking second")
		waitCard(t, tf.cardFake, first, "frozen first card", hasTitle(`^1 call · \d+s$`))

		feed.sendAgent(resultEvent(6, "b", "read"))
		feed.sendAgent(textEvent(7, "all clear"))
		feed.sendLifecycle(daemon.EventTurnComplete, turnDone)
		waitPosted(t, tf.cardFake, "all clear")
		waitFor(t, func() bool { return len(tf.spinning()) == 0 }, "a card remained spinning")
		if got := tf.deletes.Load(); got != 0 {
			t.Errorf("Delete called %d times across narration splits, want 0", got)
		}
	})

	t.Run("failed freeze in splitCard edits unused card or clock in place without Delete", func(t *testing.T) {
		// Case A: unused != nil (running call carried into fresh), both when
		// the finishing edit on fresh lands and when it fails (owing the edit).
		for _, failUnusedEdit := range []bool{false, true} {
			r, tf, e, ctx := traceCardUnit(t, ProgressStream, 2)
			first := begin(t, r, ctx, tf.cardFake, e, cardConv)
			e.noteActivity([]daemon.ToolCall{toolCall("a", "bash", "one"), toolCall("b", "read", "two")}, true, true)
			e.noteCardResults([]daemon.ToolResult{{ID: "a", Name: "bash"}, {ID: "b", Name: "read"}})
			e.noteActivity([]daemon.ToolCall{toolCall("c", "write", "three")}, true, true)

			tf.setHooks(func(ref chat.MessageRef, rep chat.Reply) error {
				if ref.ID == first && rep.Activity != nil && rep.Activity.Status == chat.StepDone {
					return errors.New("freeze failed")
				}
				if failUnusedEdit && ref.ID != first {
					return errors.New("unused edit failed")
				}
				return nil
			}, nil)

			e.emu.Lock()
			split, _ := r.splitCard(ctx, e, cardConv, ProgressStream)
			e.emu.Unlock()
			if split {
				t.Fatal("splitCard reported split=true when freeze failed")
			}
			if got := tf.deletes.Load(); got != 0 {
				t.Fatalf("failUnusedEdit=%t: Delete called %d times, want 0", failUnusedEdit, got)
			}
			ids := tf.ids()
			if len(ids) != 2 {
				t.Fatalf("posted messages = %v, want [first, fresh]", ids)
			}
			fresh := ids[1]
			if failUnusedEdit {
				if owed := r.snapshot().Owed; len(owed) != 1 || owed[0].Message != fresh {
					t.Errorf("owed = %+v, want fresh (%s) owed its finishing edit", owed, fresh)
				}
			} else {
				got, ok := tf.now(fresh)
				if !ok || got.Activity == nil || got.Activity.Status != chat.StepDone || got.Activity.Steps[0].Status != chat.StepUnfinished {
					t.Errorf("unused card = %+v, want finished segment with unfinished call", got)
				}
			}
		}

		// Case B: unused == nil (no running call carried into fresh; fresh was
		// posted as a plain clock). Failed freeze retires fresh in place to
		// Stopped instead of calling Delete.
		r, tf, e, ctx := traceCardUnit(t, ProgressStream, 4)
		first := begin(t, r, ctx, tf.cardFake, e, cardConv)
		e.noteActivity([]daemon.ToolCall{toolCall("a", "bash", "one")}, true, true)
		e.noteCardResults([]daemon.ToolResult{{ID: "a", Name: "bash"}})
		tf.setHooks(func(ref chat.MessageRef, _ chat.Reply) error {
			if ref.ID == first {
				return errors.New("freeze failed")
			}
			return nil
		}, nil)

		e.emu.Lock()
		split, _ := r.splitCard(ctx, e, cardConv, ProgressStream)
		e.emu.Unlock()
		if split {
			t.Fatal("splitCard reported split=true when freeze failed")
		}
		if got := tf.deletes.Load(); got != 0 {
			t.Fatalf("unused==nil: Delete called %d times, want 0", got)
		}
		fresh := tf.ids()[1]
		if got, ok := tf.now(fresh); !ok || got.Kind != chat.KindActivity || !strings.HasPrefix(got.Text, markFailed+" Stopped") {
			t.Errorf("unused clock = %+v (standing=%t), want finalised Stopped line", got, ok)
		}
	})

	t.Run("pre-empted splitCard and raced queuePlaceholder never call Delete", func(t *testing.T) {
		for _, carryRunning := range []bool{true, false} {
			r, tf, e, ctx := traceCardUnit(t, ProgressStream, 2)
			card := begin(t, r, ctx, tf.cardFake, e, cardConv)
			e.noteActivity([]daemon.ToolCall{toolCall("a", "bash", "one")}, true, true)
			e.noteCardResults([]daemon.ToolResult{{ID: "a", Name: "bash"}})
			if carryRunning {
				e.noteActivity([]daemon.ToolCall{toolCall("b", "read", "two")}, true, true)
			}

			later := chat.MessageRef{Conversation: cardConv, ID: "later"}
			took := false
			tf.setHooks(func(ref chat.MessageRef, rep chat.Reply) error {
				if ref.ID == card && !took && rep.Activity != nil && rep.Activity.Status == chat.StepDone {
					took = true
					e.supersede(later, time.Now())
				}
				return nil
			}, nil)

			e.emu.Lock()
			r.splitCard(ctx, e, cardConv, ProgressStream)
			e.emu.Unlock()

			if !took {
				t.Fatalf("carryRunning=%t: split never made freezing edit", carryRunning)
			}
			if got := tf.deletes.Load(); got != 0 {
				t.Fatalf("carryRunning=%t: Delete called %d times on pre-empted split, want 0", carryRunning, got)
			}
			fresh := tf.ids()[1]
			got, ok := tf.now(fresh)
			if !ok {
				t.Fatalf("carryRunning=%t: orphaned message missing", carryRunning)
			}
			if carryRunning {
				if got.Activity == nil || got.Activity.Status != chat.StepDone || anyRunning(got.Activity.Steps) {
					t.Errorf("orphaned card = %+v, want finished segment with nothing running", got.Activity)
				}
			} else if got.Kind != chat.KindActivity || !strings.HasPrefix(got.Text, markFailed+" Stopped") {
				t.Errorf("orphaned clock = %+v, want Stopped final line", got)
			}
		}

		// Raced queuePlaceholder: slot already filled before strandProgress runs.
		r, tf, e, ctx := traceCardUnit(t, ProgressStream, 4)
		_ = begin(t, r, ctx, tf.cardFake, e, cardConv) // fills progressMsg slot
		r.queuePlaceholder(ctx, e, cardConv)
		if got := tf.deletes.Load(); got != 0 {
			t.Fatalf("raced queuePlaceholder called Delete %d times, want 0", got)
		}
		raced := tf.ids()[1]
		if got, ok := tf.now(raced); !ok || got.Kind != chat.KindActivity || !strings.HasPrefix(got.Text, markFailed+" Stopped") {
			t.Errorf("raced placeholder = %+v, want Stopped final line", got)
		}
	})

	t.Run("correlated 429 on splitCard freeze and dropCard retries until settled", func(t *testing.T) {
		for _, carryRunning := range []bool{true, false} {
			r, tf, e, ctx := traceCardUnit(t, ProgressStream, 2)
			r.cardRetry = 10 * time.Millisecond
			first := begin(t, r, ctx, tf.cardFake, e, cardConv)
			e.noteActivity([]daemon.ToolCall{toolCall("a", "bash", "one")}, true, true)
			e.noteCardResults([]daemon.ToolResult{{ID: "a", Name: "bash"}})
			if carryRunning {
				e.noteActivity([]daemon.ToolCall{toolCall("b", "read", "two")}, true, true)
			}

			var rateLimited atomic.Bool
			rateLimited.Store(true)
			tf.setHooks(func(chat.MessageRef, chat.Reply) error {
				if rateLimited.Load() {
					return errors.New("429 per-space write quota")
				}
				return nil
			}, nil)

			e.emu.Lock()
			split, _ := r.splitCard(ctx, e, cardConv, ProgressStream)
			e.emu.Unlock()
			if split {
				t.Fatalf("carryRunning=%t: splitCard succeeded under 429", carryRunning)
			}
			fresh := tf.ids()[1]
			if owed := r.snapshot().Owed; len(owed) != 1 || owed[0].Message != fresh {
				t.Fatalf("carryRunning=%t: owed immediately after 429 = %+v, want [%s]", carryRunning, owed, fresh)
			}

			// Once the 1-second 429 window clears, background retryFinish settles
			// fresh without waiting for process exit and removes it from Owed.
			rateLimited.Store(false)
			waitFor(t, func() bool {
				got, ok := tf.now(fresh)
				if !ok {
					return false
				}
				if carryRunning {
					return got.Activity != nil && got.Activity.Status == chat.StepDone && !anyRunning(got.Activity.Steps)
				}
				return got.Kind == chat.KindActivity && strings.HasPrefix(got.Text, markFailed+" Stopped")
			}, fmt.Sprintf("carryRunning=%t: fresh %s never settled after 429 cleared", carryRunning, fresh))
			waitFor(t, func() bool { return len(r.snapshot().Owed) == 0 },
				fmt.Sprintf("carryRunning=%t: Owed not cleared after background retry landed", carryRunning))
			if got := tf.deletes.Load(); got != 0 {
				t.Errorf("carryRunning=%t: Delete called %d times, want 0", carryRunning, got)
			}
			_ = first
		}
	})
}
