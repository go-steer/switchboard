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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// A stream notice carries its tools as steps beside its text, and a result's
// edit redraws them: the same groups, the same arguments, the same verdicts the
// text says, so an adapter drawing the steps shows nothing the text does not.
func TestAStreamNoticeCarriesItsSteps(t *testing.T) {
	release := make(chan struct{})
	router, fake := newEventRouter(t, ProgressStream, release, twoCallEvent, okResultEvent, failResultEvent, lateAnswerEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := recvReply(t, fake.replies); got.Activity != nil {
		t.Errorf("the clock placeholder carries steps %+v, want none before a tool", got.Activity)
	}
	close(release)
	notice := recvReply(t, fake.replies)
	want := &chat.Activity{Head: "▸ Running 2 tools", Steps: []chat.Step{
		{Title: "bash", Status: chat.StepRunning, Code: "kubectl get pods -A"},
		{Title: "bash", Status: chat.StepRunning, Code: "kubectl get ns --context nope"},
	}}
	if !reflect.DeepEqual(notice.Activity, want) {
		t.Errorf("notice steps = %+v\nwant %+v", notice.Activity, want)
	}
	recvReply(t, fake.replies) // the answer

	waitFor(t, func() bool { return len(fake.updatedCalls()) == 2 }, "the results did not edit the notice twice")
	edits := fake.updatedCalls()
	got := edits[len(edits)-1].reply.Activity
	if got == nil || len(got.Steps) != 2 {
		t.Fatalf("final steps = %+v, want two", got)
	}
	if got.Head != markFailed+" Ran 2 tools (1 failed)" {
		t.Errorf("final head = %q", got.Head)
	}
	if s := got.Steps[0]; s.Status != chat.StepDone || s.Title != "bash" {
		t.Errorf("first step = %+v, want bash done", s)
	}
	if s := got.Steps[1]; s.Status != chat.StepFailed || s.Title != "bash (exit 2)" {
		t.Errorf("second step = %+v, want bash failed with its exit", s)
	}
}

// statusEdits returns the edits made to the status message ts1, in order.
func statusEdits(fake *fakeSender) []fakeUpdate {
	var out []fakeUpdate
	for _, u := range fake.updatedCalls() {
		if u.ref.ID == "ts1" {
			out = append(out, u)
		}
	}
	return out
}

// Status mode's one message draws the turn's tools as steps, by name only, and
// ticks each off as its result lands — for an adapter that draws steps — and
// is kept as the turn's record when the answer arrives, not deleted.
func TestStatusModeTicksResultsOffItsSteps(t *testing.T) {
	release := make(chan struct{})
	router, fake := newEventRouter(t, ProgressStatus, release, twoCallEvent, okResultEvent, failResultEvent, lateAnswerEvent)
	fake.draws = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	recvReply(t, fake.replies) // the placeholder, ts1
	close(release)
	if got := recvReply(t, fake.replies); got.Text != "the answer" {
		t.Fatalf("reply = %q, want the answer", got.Text)
	}
	waitFor(t, func() bool { return len(statusEdits(fake)) == 4 }, "the status message was not finalised")
	if containsRefID(fake.deletedRefs(), "ts1") {
		t.Error("the status message was deleted, want it kept as the turn's record")
	}

	edits := statusEdits(fake)
	if len(edits) != 4 {
		t.Fatalf("%d edits to the status message, want the call, one per result and the final: %+v", len(edits), edits)
	}
	final := edits[3].reply
	if final.Kind != chat.KindActivity || !strings.HasPrefix(final.Text, markOK+" Done · ") {
		t.Errorf("final = %q (%s), want the done line", final.Text, final.Kind)
	}
	if final.Activity == nil || !strings.HasSuffix(final.Activity.Head, " · 1 step") ||
		!reflect.DeepEqual(final.Activity.Steps, []chat.Step{{Title: "bash", Status: chat.StepDone}, {Title: "bash (exit 2)", Status: chat.StepFailed}}) {
		t.Errorf("final steps = %+v, want both verdicts under the done line", final.Activity)
	}
	if want := markOK + " bash\n" + markFailed + " bash (exit 2)"; !strings.HasSuffix(final.Text, want) {
		t.Errorf("final text = %q, want it to list the steps", final.Text)
	}
	edits = edits[:3]
	steps := func(i int) []chat.Step {
		if edits[i].reply.Activity == nil {
			t.Fatalf("edit %d carries no steps", i)
		}
		return edits[i].reply.Activity.Steps
	}
	// The call: two identical names, collapsed, and no argument — status mode
	// never shows one.
	if got, want := steps(0), []chat.Step{{Title: "bash ×2", Status: chat.StepRunning}}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the call: %+v, want %+v", got, want)
	}
	if got, want := steps(1), []chat.Step{{Title: "bash", Status: chat.StepDone}, {Title: "bash", Status: chat.StepRunning}}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the first result: %+v, want %+v", got, want)
	}
	if got, want := steps(2), []chat.Step{{Title: "bash", Status: chat.StepDone}, {Title: "bash (exit 2)", Status: chat.StepFailed}}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the second result: %+v, want %+v", got, want)
	}
	// The text is the line it always was — a result changes only the steps.
	// Compared past the clock, which may tick between edits.
	afterClock := func(s string) string { _, rest, _ := strings.Cut(s, " · "); return rest }
	for i := 1; i < len(edits); i++ {
		if afterClock(edits[i].text) != afterClock(edits[0].text) {
			t.Errorf("edit %d changed the line: %q, was %q", i, edits[i].text, edits[0].text)
		}
	}
}

// For an adapter that draws no steps a result would be an edit that changes
// nothing, so it is not sent — status mode on Google Chat edits exactly as
// often as it did before steps existed.
func TestStatusModeSendsNoResultEditsToAnAdapterThatCannotDrawThem(t *testing.T) {
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

	if edits := statusEdits(fake); len(edits) != 1 {
		t.Errorf("%d edits to the status message, want only the call's: %+v", len(edits), edits)
	}
}

// A turn that ran no tool has only a clock to keep, which says nothing once
// the answer is in: it is retired as before, even where steps are drawn.
func TestAStatusTurnWithoutToolsKeepsNothing(t *testing.T) {
	release := make(chan struct{})
	router, fake := newEventRouter(t, ProgressStatus, release, answerEvent)
	fake.draws = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	recvReply(t, fake.replies)
	close(release)
	recvReply(t, fake.replies)
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts1") }, "the clock was not retired")
}

// A kept record never shows a step still running: one whose result never came
// finished with the turn, or failed with it when the turn was stopped.
func TestAKeptTrailLeavesNothingRunning(t *testing.T) {
	var tr statusTrail
	tr.add([]daemon.ToolCall{{ID: "a", Name: "bash"}, {ID: "b", Name: "read"}})
	tr.file([]daemon.ToolResult{{ID: "a", Name: "bash"}})

	done := tr.final("✔ Done · 5s", 2, true)
	if got := done.Steps[1].Status; got != chat.StepDone {
		t.Errorf("unanswered call in a finished turn = %s, want done", got)
	}
	if done.Head != "✔ Done · 5s · 2 steps" {
		t.Errorf("head = %q", done.Head)
	}
	stopped := tr.final("✖ Stopped · 5s", 2, false)
	if got := stopped.Steps[1].Status; got != chat.StepFailed {
		t.Errorf("unanswered call in a stopped turn = %s, want failed", got)
	}
	if got := stopped.Steps[0].Status; got != chat.StepDone {
		t.Errorf("answered call = %s, want its own verdict", got)
	}
	var empty statusTrail
	if empty.final("✔ Done", 0, true) != nil {
		t.Error("an empty trail kept something")
	}
}

// A kept record holds more than a running message, but still only the latest.
func TestAKeptTrailIsBounded(t *testing.T) {
	var tr statusTrail
	for i := 0; i < statusStepsKept+5; i++ {
		name := "bash"
		if i%2 == 1 {
			name = "read"
		}
		tr.add([]daemon.ToolCall{{Name: name}})
	}
	if got := len(tr.final("✔ Done", 1, true).Steps); got != statusStepsKept {
		t.Errorf("kept %d steps, want %d", got, statusStepsKept)
	}
}

func TestStatusTrailFilesByIDNewestFirst(t *testing.T) {
	var tr statusTrail
	tr.add([]daemon.ToolCall{{ID: "1", Name: "bash"}})
	tr.add([]daemon.ToolCall{{ID: "1", Name: "bash"}}) // a daemon numbering per frame
	if !tr.file([]daemon.ToolResult{{ID: "1", Name: "bash"}}) {
		t.Fatal("a matching id filed nothing")
	}
	if tr.res[0] != nil || tr.res[1] == nil {
		t.Errorf("filed against %v, want the newest call holding the id", tr.res)
	}
}

func TestStatusTrailFallsBackToTheOldestUnansweredName(t *testing.T) {
	var tr statusTrail
	tr.add([]daemon.ToolCall{{Name: "bash"}, {Name: "read"}, {Name: "bash"}})
	tr.file([]daemon.ToolResult{{Name: "bash", Failed: true}})
	if tr.res[0] == nil || tr.res[2] != nil {
		t.Errorf("filed against %v, want the oldest bash", tr.res)
	}
	if tr.file([]daemon.ToolResult{{Name: "write"}}) {
		t.Error("a result for a tool never called filed something")
	}
}

// Ids first across the whole frame: an id-less result guessing by name must
// not take the line an id in the same frame owns outright.
func TestStatusTrailFilesIDsBeforeGuessingByName(t *testing.T) {
	var tr statusTrail
	tr.add([]daemon.ToolCall{{ID: "a", Name: "bash"}, {Name: "bash"}})
	tr.file([]daemon.ToolResult{{Name: "bash", Failed: true}, {ID: "a", Name: "bash"}})
	if tr.res[0] == nil || tr.res[0].Failed {
		t.Errorf("call a = %+v, want its own success", tr.res[0])
	}
	if tr.res[1] == nil || !tr.res[1].Failed {
		t.Errorf("the id-less call = %+v, want the failure", tr.res[1])
	}
}

// The trail never stores an argument: status mode does not show them, and a
// summary never kept is one no later edit can leak.
func TestStatusTrailKeepsNoArguments(t *testing.T) {
	var tr statusTrail
	tr.add([]daemon.ToolCall{{ID: "a", Name: "bash", Arg: "cat /etc/secret"}})
	if tr.calls[0].Arg != "" {
		t.Errorf("stored argument %q", tr.calls[0].Arg)
	}
}

func TestStatusTrailIsBounded(t *testing.T) {
	var tr statusTrail
	for i := 0; i < statusTrailMemory+10; i++ {
		tr.add([]daemon.ToolCall{{Name: "bash"}})
	}
	if len(tr.calls) != statusTrailMemory || len(tr.res) != statusTrailMemory {
		t.Errorf("trail holds %d calls, %d results; want %d", len(tr.calls), len(tr.res), statusTrailMemory)
	}
}

// Only the latest steps are drawn, under a head with the clock and step count.
func TestStatusTrailDrawsTheLatestSteps(t *testing.T) {
	var tr statusTrail
	if tr.activity(time.Second, 0) != nil {
		t.Error("an empty trail drew steps")
	}
	for i := 0; i < statusStepsShown+3; i++ {
		// Alternating names, so no two neighbours collapse into one step.
		name := "bash"
		if i%2 == 1 {
			name = "read"
		}
		tr.add([]daemon.ToolCall{{Name: name}})
	}
	a := tr.activity(95*time.Second, 11)
	if len(a.Steps) != statusStepsShown {
		t.Errorf("drew %d steps, want %d", len(a.Steps), statusStepsShown)
	}
	if a.Head != "⏳ Working… 1m35s · step 11" {
		t.Errorf("head = %q", a.Head)
	}
	if last := a.Steps[len(a.Steps)-1]; last.Title != "bash" {
		t.Errorf("last step = %+v, want the latest call", last)
	}
}

// A terse notice — status mode's, with no message of its own to edit — is
// never registered, so no result can tick it off. Steps drawn from it would
// read "in progress" for good; it goes as its text alone.
func TestATerseNoticeDrawsNoSteps(t *testing.T) {
	calls := []daemon.ToolCall{{ID: "a", Name: "bash", Arg: "kubectl get pods"}}
	if a := noticeActivity(calls, nil, nil, false); a != nil {
		t.Errorf("terse notice steps = %+v, want none", a)
	}
	if a := noticeActivity(calls, nil, nil, true); a == nil {
		t.Error("a stream notice drew no steps")
	}
}

// The kept record's text says what its steps do, so a client that cannot draw
// the cards — or a Slack that refuses them — still shows the record.
func TestAKeptRecordSaysItsStepsInText(t *testing.T) {
	a := &chat.Activity{Head: "✔ Done · 5s · 2 steps", Steps: []chat.Step{
		{Title: "bash", Status: chat.StepDone},
		{Title: "bash (exit 2)", Status: chat.StepFailed},
	}}
	want := "✔ Done · 5s · 2 steps\n" + markOK + " bash\n" + markFailed + " bash (exit 2)"
	if got := stepsText(a); got != want {
		t.Errorf("text =\n%s\nwant\n%s", got, want)
	}
}

// A record that cannot be written is not left behind as a frozen clock: the
// message is retired as it would have been before records were kept.
func TestAKeptRecordThatCannotBeWrittenIsRetired(t *testing.T) {
	release := make(chan struct{})
	router, fake := newEventRouter(t, ProgressStatus, release, twoCallEvent, okResultEvent, failResultEvent, lateAnswerEvent)
	fake.draws = true
	fake.failUpdates(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := router.Handle(ctx, chat.Message{Conversation: "C0:1", Caller: "a@b.com", Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	recvReply(t, fake.replies)
	close(release)
	recvReply(t, fake.replies)
	waitFor(t, func() bool { return containsRefID(fake.deletedRefs(), "ts1") }, "the unkeepable status message was not retired")
}
