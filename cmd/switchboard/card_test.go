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
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
	"github.com/go-steer/switchboard/pkg/subagent"
)

// These pin the record a card is drawn from (turnCard) on its own, with no
// router around it: what it stores, how results find their calls, and what
// each rendering says. The paths that feed and edit it are in
// card_router_test.go.

var cardT0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// titles lists steps by title, a step's own steps in brackets after it.
func titles(steps []chat.Step) string {
	var out []string
	for _, s := range steps {
		t := s.Title
		if len(s.Steps) > 0 {
			t += "[" + titles(s.Steps) + "]"
		}
		out = append(out, t)
	}
	return strings.Join(out, " ")
}

// statuses lists steps' states in order, a step's own steps included.
func statuses(steps []chat.Step) []chat.StepStatus {
	var out []chat.StepStatus
	for _, s := range steps {
		out = append(out, s.Status)
		out = append(out, statuses(s.Steps)...)
	}
	return out
}

func anyRunning(steps []chat.Step) bool {
	for _, s := range statuses(steps) {
		if s == chat.StepRunning {
			return true
		}
	}
	return false
}

func TestCardFilesByIDNewestFirst(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "1", Name: "bash"}}, false, cardT0)
	c.add([]daemon.ToolCall{{ID: "1", Name: "bash"}}, false, cardT0) // a daemon numbering per frame
	if changed, rest := c.file([]daemon.ToolResult{{ID: "1", Name: "bash"}}, cardT0); !changed || len(rest) != 0 {
		t.Fatalf("a matching id: changed=%t rest=%v, want it filed", changed, rest)
	}
	if c.calls[0].res != nil || c.calls[1].res == nil {
		t.Errorf("filed against the older call; want the newest holding the id")
	}
}

func TestCardFallsBackToTheOldestUnansweredName(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{Name: "bash"}, {Name: "read"}, {Name: "bash"}}, false, cardT0)
	c.file([]daemon.ToolResult{{Name: "bash", Failed: true}}, cardT0)
	if c.calls[0].res == nil || c.calls[2].res != nil {
		t.Errorf("want the oldest bash answered and the newer one still open")
	}
	changed, rest := c.file([]daemon.ToolResult{{Name: "write"}}, cardT0)
	if changed || len(rest) != 1 {
		t.Errorf("a result for a tool never called: changed=%t rest=%v, want it handed back", changed, rest)
	}
}

// Ids first across the whole frame: an id-less result guessing by name must
// not take the line an id in the same frame owns outright.
func TestCardFilesIDsBeforeGuessingByName(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash"}, {Name: "bash"}}, false, cardT0)
	c.file([]daemon.ToolResult{{Name: "bash", Failed: true}, {ID: "a", Name: "bash"}}, cardT0)
	if r := c.calls[0].res; r == nil || r.Failed {
		t.Errorf("call a = %+v, want its own success", r)
	}
	if r := c.calls[1].res; r == nil || !r.Failed {
		t.Errorf("the id-less call = %+v, want the failure", r)
	}
}

// An id nobody holds does not fall back on its name: it answers a call some
// other message announced, or none.
func TestCardDoesNotGuessForAnUnknownID(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash"}}, false, cardT0)
	changed, rest := c.file([]daemon.ToolResult{{ID: "zzz", Name: "bash"}}, cardT0)
	if changed || len(rest) != 1 || c.calls[0].res != nil {
		t.Errorf("an unknown id: changed=%t rest=%v res=%v, want nothing filed", changed, rest, c.calls[0].res)
	}
}

// Status mode's record never stores an argument or draws a duration: it does
// not show them, and a summary never kept is one no later edit, snapshot or
// fallback text can leak.
func TestAStatusCardKeepsNoArguments(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash", Arg: "cat /etc/secret"}}, false, cardT0)
	c.addSub("bash", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "i", Name: "read_file", Arg: "/etc/shadow"}}}}, false, cardT0)
	c.addSub("bash", []subagent.Frame{{Results: []daemon.ToolResult{{ID: "i", Name: "read_file"}}}}, false, cardT0)
	c.file([]daemon.ToolResult{{ID: "a", Name: "bash"}}, cardT0.Add(3*time.Second))
	if c.calls[0].arg != "" || c.calls[0].children[0].arg != "" {
		t.Errorf("stored arguments %q and %q", c.calls[0].arg, c.calls[0].children[0].arg)
	}
	raw, err := json.Marshal([]any{c.live(time.Second, false), c.final(time.Second, true), c.persisted(), cardText(c.final(time.Second, true))})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"secret", "shadow"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("a rendering of a status card carries %q: %s", leak, raw)
		}
	}
	for _, s := range c.final(time.Second, true).Steps {
		if s.Code != "" || s.Note != "" || s.Steps[0].Code != "" || s.Steps[0].Note != "" {
			t.Errorf("a status step carries detail: %+v", s)
		}
	}
}

// Stream mode's record carries each call's summarised argument and how long it
// took; the subagent's durations are its own, from its event timestamps.
func TestAStreamCardCarriesArgumentsAndDurations(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "p", Name: "_fallback", Arg: "check billing"}}, true, cardT0)
	at := time.Date(2026, 11, 18, 19, 0, 0, 0, time.UTC)
	c.addSub("_fallback", []subagent.Frame{{At: at, Calls: []daemon.ToolCall{{ID: "f1", Name: "get_k8s_pods", Arg: "billing"}}}}, true, cardT0)
	c.addSub("_fallback", []subagent.Frame{{At: at.Add(2400 * time.Millisecond), Results: []daemon.ToolResult{{ID: "f1", Name: "get_k8s_pods"}}}}, true, cardT0.Add(time.Hour))
	c.file([]daemon.ToolResult{{ID: "p", Name: "_fallback"}}, cardT0.Add(5*time.Second))

	want := []chat.Step{{
		Title: "_fallback", Status: chat.StepDone, Code: "check billing", Note: "5.0s", Calls: 1,
		Steps: []chat.Step{{Title: "get_k8s_pods", Status: chat.StepDone, Code: "billing", Note: "2.4s"}},
	}}
	if got := c.final(time.Minute, true).Steps; !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %+v\nwant    %+v", got, want)
	}
}

// A subagent's results tick its calls off by id first, then by name, and only
// within that subagent.
func TestASubagentsResultsStayWithinIt(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "p1", Name: "alpha"}, {ID: "p2", Name: "beta"}}, true, cardT0)
	c.addSub("alpha", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "x", Name: "grep"}, {Name: "grep"}}}}, true, cardT0)
	c.addSub("beta", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "x", Name: "grep"}}}}, true, cardT0)

	// beta's result, by an id alpha also uses: it must land on beta's call.
	c.addSub("beta", []subagent.Frame{{Results: []daemon.ToolResult{{ID: "x", Name: "grep", Failed: true}}}}, true, cardT0)
	alpha, beta := c.calls[0], c.calls[1]
	if alpha.children[0].res != nil || beta.children[0].res == nil {
		t.Fatalf("beta's result landed on alpha's call")
	}
	// alpha: a name-only result in the same frame as an id must take the
	// id-less call, not the one the id owns.
	c.addSub("alpha", []subagent.Frame{{Results: []daemon.ToolResult{{Name: "grep", Failed: true}, {ID: "x", Name: "grep"}}}}, true, cardT0)
	if r := alpha.children[0].res; r == nil || r.Failed {
		t.Errorf("alpha's id call = %+v, want its own success", r)
	}
	if r := alpha.children[1].res; r == nil || !r.Failed {
		t.Errorf("alpha's id-less call = %+v, want the failure", r)
	}
	if c.total != 5 || c.failed != 2 {
		t.Errorf("total %d failed %d, want 5 calls of which 2 failed", c.total, c.failed)
	}
}

// Frames with no delegation to go under are dropped rather than drawn as the
// agent's own calls: they are a poller's that outlived its turn.
func TestSubagentFramesWithNoDelegationAreDropped(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{Name: "bash"}}, true, cardT0)
	if c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{Name: "grep"}}}}, true, cardT0) {
		t.Error("frames for a subagent nothing delegated to changed the card")
	}
	if c.total != 1 || len(c.calls) != 1 || len(c.calls[0].children) != 0 {
		t.Errorf("card = %d calls, %d total; want the one bash", len(c.calls), c.total)
	}
}

// A delegation's result closes whatever its subagent still had open: nothing
// under a call that has returned is left drawn as running.
func TestADelegationReturningClosesItsOpenCalls(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "p", Name: "_fallback"}}, true, cardT0)
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "f1", Name: "grep", Arg: "x"}}}}, true, cardT0)
	c.file([]daemon.ToolResult{{ID: "p", Name: "_fallback"}}, cardT0.Add(time.Second))
	a := c.live(time.Second, false)
	if anyRunning(a.Steps) {
		t.Errorf("steps = %+v, want nothing running under a delegation that returned", a.Steps)
	}
	if note := a.Steps[0].Steps[0].Note; note != "" {
		t.Errorf("a call closed with its delegation shows a duration %q; it was never measured", note)
	}
}

// Adjacent identical calls collapse to one line with a count, at each level;
// calls that differ in name, argument or verdict stay apart.
func TestCardGroupsAdjacentIdenticalCalls(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{Name: "bash"}, {Name: "bash"}, {Name: "read"}, {Name: "bash"}, {ID: "p", Name: "_fallback"}}, false, cardT0)
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{Name: "grep"}, {Name: "grep"}, {Name: "grep"}, {Name: "cat"}}}}, false, cardT0)
	if got, want := titles(c.live(0, false).Steps), "bash ×2 read bash _fallback[grep ×3 cat]"; got != want {
		t.Errorf("steps = %s, want %s", got, want)
	}

	// With arguments shown, the argument is part of what makes two calls the
	// same, and a result landing on one of a run splits it.
	var d turnCard
	d.add([]daemon.ToolCall{{ID: "a", Name: "bash", Arg: "ls"}, {ID: "b", Name: "bash", Arg: "ls"}, {ID: "c", Name: "bash", Arg: "pwd"}}, true, cardT0)
	if got, want := titles(d.live(0, false).Steps), "bash ×2 bash"; got != want {
		t.Errorf("steps = %s, want %s", got, want)
	}
	d.file([]daemon.ToolResult{{ID: "a", Name: "bash", Failed: true, Detail: "exit 2"}}, cardT0)
	if got, want := titles(d.live(0, false).Steps), "bash (exit 2) bash bash"; got != want {
		t.Errorf("after one of the run failed: %s, want %s", got, want)
	}
	// A run shows no duration: no one number is the run's.
	d.file([]daemon.ToolResult{{ID: "b", Name: "bash"}, {ID: "c", Name: "bash"}}, cardT0.Add(time.Second))
	var e turnCard
	e.add([]daemon.ToolCall{{ID: "a", Name: "bash", Arg: "ls"}, {ID: "b", Name: "bash", Arg: "ls"}}, true, cardT0)
	e.file([]daemon.ToolResult{{ID: "a", Name: "bash"}, {ID: "b", Name: "bash"}}, cardT0.Add(time.Second))
	if s := e.final(0, true).Steps; len(s) != 1 || s[0].Title != "bash ×2" || s[0].Note != "" {
		t.Errorf("a finished run = %+v, want one line with a count and no duration", s)
	}
}

func TestCardTitles(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash"}}, false, cardT0)
	if got, want := c.live(190*time.Second, false).Title, "Working… 3m10s · 1 call · bash"; got != want {
		t.Errorf("running title = %q, want %q", got, want)
	}
	c.add([]daemon.ToolCall{{ID: "b", Name: "bash"}, {ID: "c", Name: "read"}}, false, cardT0)
	if got, want := c.live(190*time.Second, false).Title, "Working… 3m10s · 3 calls · bash ×2, read"; got != want {
		t.Errorf("running title = %q, want %q", got, want)
	}
	c.file([]daemon.ToolResult{{ID: "a"}, {ID: "b", Failed: true}, {ID: "c", Failed: true}}, cardT0)
	if got, want := c.live(190*time.Second, false).Title, "Working… 3m10s · 3 calls"; got != want {
		t.Errorf("title with nothing running = %q, want %q", got, want)
	}
	done := c.final(302*time.Second, true)
	if done.Title != "Done · 5m02s · 3 calls (2 failed)" || done.Status != chat.StepDone {
		t.Errorf("finished = %q (%s)", done.Title, done.Status)
	}
	stopped := c.final(72*time.Second, false)
	if stopped.Title != "Stopped · 1m12s · 3 calls" || stopped.Status != chat.StepFailed {
		t.Errorf("stopped = %q (%s)", stopped.Title, stopped.Status)
	}
	var clean turnCard
	clean.add([]daemon.ToolCall{{Name: "bash"}}, false, cardT0)
	if got, want := clean.final(5*time.Second, true).Title, "Done · 5s · 1 call"; got != want {
		t.Errorf("finished with no failure = %q, want %q", got, want)
	}
	// Plain text throughout: no mark, no emoji, no markup.
	for _, title := range []string{done.Title, stopped.Title, c.live(time.Second, false).Title} {
		if strings.ContainsAny(title, "⏳✔✖▸*`_") {
			t.Errorf("title %q carries a mark or markup", title)
		}
	}
}

// The title names what a subagent is running under the call that delegated to
// it, and stops when that call returns. The count is every call the turn has
// made, the subagent's included and the delegating call once.
func TestTheTitleNamesTheSubagentsTool(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "p", Name: "_fallback"}}, false, cardT0)
	if got, want := c.live(5*time.Second, false).Title, "Working… 5s · 1 call · _fallback"; got != want {
		t.Errorf("before the subagent's first call: %q, want %q", got, want)
	}
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "f1", Name: "get_k8s_logs"}}}}, false, cardT0)
	if got, want := c.live(5*time.Second, false).Title, "Working… 5s · 2 calls · _fallback › get_k8s_logs"; got != want {
		t.Errorf("with the subagent's tool running: %q, want %q", got, want)
	}
	c.addSub("_fallback", []subagent.Frame{{Results: []daemon.ToolResult{{ID: "f1", Name: "get_k8s_logs"}}}}, false, cardT0)
	c.file([]daemon.ToolResult{{ID: "p", Name: "_fallback"}}, cardT0)
	if got, want := c.live(5*time.Second, false).Title, "Working… 5s · 2 calls"; got != want {
		t.Errorf("after the delegation returned: %q, want %q", got, want)
	}

	// spawn_agent names the agent it spawned, as the status line always has.
	var s turnCard
	s.add([]daemon.ToolCall{{ID: "sp", Name: "spawn_agent", Subagent: "cluster"}}, false, cardT0)
	s.addSub("cluster", []subagent.Frame{{Calls: []daemon.ToolCall{{Name: "get_k8s_events"}}}}, false, cardT0)
	if got, want := s.live(time.Second, false).Title, "Working… 1s · 2 calls · cluster › get_k8s_events"; got != want {
		t.Errorf("spawned agent: %q, want %q", got, want)
	}
}

// A name keeps its underscores wherever they fall — boldName drops one at
// either end, which made "_fallback" read "fallback" — and loses anything that
// is not part of a tool name.
func TestPlainNameKeepsUnderscores(t *testing.T) {
	for in, want := range map[string]string{
		"_fallback":             "_fallback",
		"__init__":              "__init__",
		"mcp__github__list_prs": "mcp__github__list_prs",
		"bash":                  "bash",
		"propose-change":        "propose-change",
		"a.b:c/d":               "a.b:c/d",
		"<!channel>":            "channel",
		"[x](http://e.vil)":     "xhttp://e.vil",
		"*bold* `code`":         "boldcode",
		"":                      "?",
		"***":                   "?",
	} {
		if got := plainName(in); got != want {
			t.Errorf("plainName(%q) = %q, want %q", in, got, want)
		}
	}
	// boldName is as it was: the notices outside card mode still use it.
	if got := boldName("_fallback"); got != "fallback" {
		t.Errorf("boldName(_fallback) = %q; the text notices are not meant to change", got)
	}

	var c turnCard
	c.add([]daemon.ToolCall{{ID: "p", Name: "_fallback"}}, false, cardT0)
	a := c.live(time.Second, false)
	if !strings.HasSuffix(a.Title, " · _fallback") || a.Steps[0].Title != "_fallback" {
		t.Errorf("title %q, step %q: want _fallback with its underscore in both", a.Title, a.Steps[0].Title)
	}
}

// A finished card never shows a line still running: one whose result never
// came finished with the turn, or failed with it when the turn was stopped.
func TestAFinishedCardLeavesNothingRunning(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash"}, {ID: "p", Name: "_fallback"}}, false, cardT0)
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k", Name: "grep"}}}}, false, cardT0)
	c.file([]daemon.ToolResult{{ID: "a", Name: "bash"}}, cardT0)

	if got, want := statuses(c.live(0, false).Steps), []chat.StepStatus{chat.StepDone, chat.StepRunning, chat.StepRunning}; !reflect.DeepEqual(got, want) {
		t.Errorf("live = %v, want %v", got, want)
	}
	if got, want := statuses(c.final(0, true).Steps), []chat.StepStatus{chat.StepDone, chat.StepDone, chat.StepDone}; !reflect.DeepEqual(got, want) {
		t.Errorf("finished = %v, want %v", got, want)
	}
	if got, want := statuses(c.final(0, false).Steps), []chat.StepStatus{chat.StepDone, chat.StepFailed, chat.StepFailed}; !reflect.DeepEqual(got, want) {
		t.Errorf("stopped = %v, want %v: an answered call keeps its own verdict", got, want)
	}
	// A stretch left behind for a newer message guesses at nothing: what has
	// no result is unfinished, which is neither running nor a verdict.
	if got, want := statuses(c.segment(0).Steps), []chat.StepStatus{chat.StepDone, chat.StepUnfinished, chat.StepUnfinished}; !reflect.DeepEqual(got, want) {
		t.Errorf("segment = %v, want %v", got, want)
	}
}

// A card left behind because a newer message took the slot draws a call with
// no result as unfinished, at both levels, and its title says how many. Seen
// live: a delegation that never returned, over thirty-one calls that all
// failed, was kept as "✔ _fallback" under a check — a healthy finished turn,
// which it was not.
func TestASupersededCardDrawsCallsWithNoResultAsUnfinished(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "p", Name: "_fallback", Arg: "check billing"}}, true, cardT0)
	var calls []daemon.ToolCall
	var results []daemon.ToolResult
	for i := 0; i < 31; i++ {
		id := fmt.Sprintf("k%d", i)
		calls = append(calls, daemon.ToolCall{ID: id, Name: "get_k8s_logs", Arg: "billing-0"})
		results = append(results, daemon.ToolResult{ID: id, Name: "get_k8s_logs", Failed: true, Detail: "exit 1"})
	}
	c.addSub("_fallback", []subagent.Frame{{Calls: calls}, {Results: results}}, true, cardT0)

	a := c.segment(135 * time.Second)
	if a.Title != "32 calls · 2m15s · 1 unfinished" {
		t.Errorf("title = %q, want the count of calls no result was seen for", a.Title)
	}
	// The card still reads done: the usual cause is a follow-up question in
	// the middle of a healthy turn.
	if a.Status != chat.StepDone {
		t.Errorf("card status = %s, want done", a.Status)
	}
	if len(a.Steps) != 1 || a.Steps[0].Title != "_fallback" || a.Steps[0].Status != chat.StepUnfinished {
		t.Fatalf("steps = %+v, want the delegation that never returned drawn unfinished", a.Steps)
	}
	if kids := a.Steps[0].Steps; len(kids) != 1 || kids[0].Title != "get_k8s_logs ×31 (exit 1)" || kids[0].Status != chat.StepFailed {
		t.Errorf("under it = %+v, want the calls that failed keeping their own verdict", kids)
	}
	if text := cardText(a); !strings.Contains(text, "\n– `_fallback` · 31 calls  `check billing`\n") || strings.Contains(text, markOK+" `_fallback`") {
		t.Errorf("text does not draw the delegation with the neutral mark:\n%s", text)
	}
	if a.Recent != nil {
		t.Errorf("a card left behind carries recent lines")
	}

	// Under a delegation too: a subagent's call still out is unfinished, and
	// counted. Adjacent ones still collapse into one line.
	var d turnCard
	d.add([]daemon.ToolCall{{ID: "a", Name: "bash"}, {ID: "p", Name: "_fallback"}}, false, cardT0)
	d.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k1", Name: "grep"}, {ID: "k2", Name: "cat"}, {ID: "k3", Name: "cat"}}}}, false, cardT0)
	d.addSub("_fallback", []subagent.Frame{{Results: []daemon.ToolResult{{ID: "k1", Name: "grep"}}}}, false, cardT0)
	d.file([]daemon.ToolResult{{ID: "a", Name: "bash"}}, cardT0)
	b := d.segment(5 * time.Second)
	if b.Title != "5 calls · 5s · 3 unfinished" {
		t.Errorf("title = %q, want the delegation and its two calls still out counted", b.Title)
	}
	if got, want := titles(b.Steps), "bash _fallback[grep cat ×2]"; got != want {
		t.Errorf("steps = %s, want %s", got, want)
	}
	if got, want := statuses(b.Steps), []chat.StepStatus{chat.StepDone, chat.StepUnfinished, chat.StepDone, chat.StepUnfinished}; !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}

	// Nothing still out: no suffix, and nothing unfinished.
	d.addSub("_fallback", []subagent.Frame{{Results: []daemon.ToolResult{{ID: "k2", Name: "cat"}, {ID: "k3", Name: "cat"}}}}, false, cardT0)
	d.file([]daemon.ToolResult{{ID: "p", Name: "_fallback"}}, cardT0)
	if e := d.segment(5 * time.Second); e.Title != "5 calls · 5s" {
		t.Errorf("title with every call answered = %q, want no unfinished suffix", e.Title)
	}

	// The other ways a turn ends still say what they said: a finished turn's
	// unanswered call finished with it, a stopped turn's failed with it.
	if got := statuses(c.final(time.Second, true).Steps)[0]; got != chat.StepDone {
		t.Errorf("finished turn: the unanswered delegation = %s, want done", got)
	}
	if got := statuses(c.final(time.Second, false).Steps)[0]; got != chat.StepFailed {
		t.Errorf("stopped turn: the unanswered delegation = %s, want failed", got)
	}
}

// The unfinished state survives the state file as itself, and a restart leaves
// it as it is: no result was seen then, and none will be now.
func TestUnfinishedSurvivesTheStateFile(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash"}, {ID: "p", Name: "_fallback"}}, false, cardT0)
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k", Name: "grep"}}}}, false, cardT0)
	c.file([]daemon.ToolResult{{ID: "a", Name: "bash"}}, cardT0)

	raw, err := json.Marshal(&cardRecord{Calls: 3, Steps: stepRecords(c.segment(time.Second).Steps)})
	if err != nil {
		t.Fatal(err)
	}
	var back cardRecord
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Steps[1].Status; got != string(chat.StepUnfinished) {
		t.Errorf("round-tripped status = %q, want %q", got, chat.StepUnfinished)
	}
	if got := back.Steps[1].Steps[0].Status; got != string(chat.StepUnfinished) {
		t.Errorf("round-tripped nested status = %q, want %q", got, chat.StepUnfinished)
	}
	want := []chat.StepStatus{chat.StepDone, chat.StepUnfinished, chat.StepUnfinished}
	if got := statuses(back.interrupted().Steps); !reflect.DeepEqual(got, want) {
		t.Errorf("after a restart = %v, want %v: unfinished stays unfinished", got, want)
	}
}

// Stream mode shows the latest five lines under the card while the turn runs,
// flattened and oldest first, with the card's own disclosure; status shows
// none; and no finished card does.
func TestRecentIsTheLatestFiveLinesOfARunningStreamCard(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash", Arg: "one"}, {ID: "p", Name: "_fallback", Arg: "check"}}, true, cardT0)
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{
		{ID: "k1", Name: "grep", Arg: "memory"}, {ID: "k2", Name: "grep", Arg: "memory"}, {ID: "k3", Name: "cat", Arg: "f"},
	}}}, true, cardT0)
	c.add([]daemon.ToolCall{{ID: "b", Name: "read", Arg: "two"}, {ID: "c", Name: "write", Arg: "three"}}, true, cardT0)
	c.file([]daemon.ToolResult{{ID: "b", Name: "read"}}, cardT0.Add(2300*time.Millisecond))

	a := c.live(time.Second, true)
	// Six lines after grouping: bash, _fallback, grep ×2, cat, read, write.
	want := []chat.Step{
		{Title: "_fallback", Status: chat.StepRunning, Code: "check", Calls: 3},
		{Title: "grep ×2", Status: chat.StepRunning, Code: "memory"},
		{Title: "cat", Status: chat.StepRunning, Code: "f"},
		{Title: "read", Status: chat.StepDone, Code: "two", Note: "2.3s"},
		{Title: "write", Status: chat.StepRunning, Code: "three"},
	}
	if !reflect.DeepEqual(a.Recent, want) {
		t.Errorf("recent = %+v\nwant     %+v", a.Recent, want)
	}
	if len(a.Recent) != cardRecentLines {
		t.Errorf("%d recent lines, want %d", len(a.Recent), cardRecentLines)
	}
	// The card itself still holds every line, nested.
	if got := stepLines(a.Steps); got != 6 {
		t.Errorf("the card draws %d lines, want all 6", got)
	}

	if got := c.live(time.Second, false).Recent; got != nil {
		t.Errorf("status recent = %+v, want none", got)
	}
	for name, fin := range map[string]*chat.Activity{
		"done":    c.final(time.Second, true),
		"stopped": c.final(time.Second, false),
		"segment": c.segment(time.Second),
		"restart": c.persisted().interrupted(),
	} {
		if fin.Recent != nil {
			t.Errorf("%s card carries recent lines %+v, want none", name, fin.Recent)
		}
	}
	if f, ok := c.planFreeze(time.Second, 100); !ok || f.act.Recent != nil {
		t.Errorf("a frozen card carries recent lines")
	}

	// Fewer than five lines: all of them.
	var few turnCard
	few.add([]daemon.ToolCall{{Name: "bash", Arg: "ls"}}, true, cardT0)
	if got := few.live(0, true).Recent; len(got) != 1 || got[0].Title != "bash" {
		t.Errorf("recent of a one-line card = %+v", got)
	}
}

// Freezing leaves behind every call that has returned and carries on every
// one that has not: the frozen message shows nothing running, nothing a
// result may still arrive for is forgotten, and a delegation still in flight
// heads both.
func TestFreezingSplitsFinishedFromRunning(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash", Arg: "one"}, {ID: "p", Name: "_fallback", Arg: "check"}, {ID: "b", Name: "read", Arg: "two"}}, true, cardT0)
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k1", Name: "grep"}, {ID: "k2", Name: "cat"}, {ID: "k3", Name: "ls"}}}}, true, cardT0)
	c.addSub("_fallback", []subagent.Frame{{Results: []daemon.ToolResult{{ID: "k1", Name: "grep"}, {ID: "k2", Name: "cat", Failed: true}}}}, true, cardT0)
	c.file([]daemon.ToolResult{{ID: "a", Name: "bash"}}, cardT0)

	f, ok := c.planFreeze(72*time.Second, 100)
	if !ok {
		t.Fatal("a card with finished calls had nothing to freeze")
	}
	// Planning changes nothing: the platform may yet refuse the split.
	if len(c.calls) != 3 || len(c.calls[1].children) != 3 {
		t.Fatalf("planning a freeze changed the record: %d calls", len(c.calls))
	}
	if got, want := titles(f.act.Steps), "bash _fallback[grep cat]"; got != want {
		t.Errorf("frozen = %s, want %s", got, want)
	}
	if anyRunning(f.act.Steps) {
		t.Errorf("the frozen message shows a line running: %+v", f.act.Steps)
	}
	// The delegation is still out, so its line here — only there to head the
	// calls under it that returned — is unfinished, not done. Those calls
	// keep their own verdicts.
	if got, want := statuses(f.act.Steps), []chat.StepStatus{chat.StepDone, chat.StepUnfinished, chat.StepDone, chat.StepFailed}; !reflect.DeepEqual(got, want) {
		t.Errorf("frozen statuses = %v, want %v", got, want)
	}
	// It carries on below, so the title does not count it as unfinished.
	if f.act.Title != "3 calls · 1m12s" || f.act.Status != chat.StepDone {
		t.Errorf("frozen title = %q (%s), want this message's calls and the clock", f.act.Title, f.act.Status)
	}
	// The continuation, rendered before anything is committed.
	rest := c.rest(f)
	if got, want := titles(rest.live(0, false).Steps), "_fallback[ls] read"; got != want {
		t.Errorf("continuation = %s, want %s", got, want)
	}

	c.commitFreeze(f)
	if got, want := titles(c.live(0, false).Steps), "_fallback[ls] read"; got != want {
		t.Errorf("after the freeze the card holds %s, want %s", got, want)
	}
	// Whole-turn counts survive what left memory.
	if c.total != 6 || c.failed != 1 {
		t.Errorf("total %d failed %d, want the whole turn's 6 and 1", c.total, c.failed)
	}
	// The delegation's line still says how many calls its subagent has made.
	if got := c.live(0, false).Steps[0].Calls; got != 3 {
		t.Errorf("the carried delegation counts %d calls, want all 3", got)
	}
	// Everything carried is still addressable, by id and by name.
	c.addSub("_fallback", []subagent.Frame{{Results: []daemon.ToolResult{{ID: "k3", Name: "ls"}}}}, true, cardT0)
	if changed, rest := c.file([]daemon.ToolResult{{ID: "p", Name: "_fallback"}, {Name: "read"}}, cardT0); !changed || len(rest) != 0 {
		t.Errorf("results for carried calls: changed=%t rest=%v, want all filed", changed, rest)
	}
	if got := c.final(90*time.Second, true); got.Title != "Done · 1m30s · 6 calls (1 failed)" || titles(got.Steps) != "_fallback[ls] read" {
		t.Errorf("final = %q %s", got.Title, titles(got.Steps))
	}
}

// One freeze leaves behind no more than the line limit, so a burst between two
// edits cannot make a frozen message longer than a card holds. A delegation
// with more calls than fit is frozen a message's worth at a time, its line
// heading each, and every call lands in exactly one message.
func TestAFreezeIsBoundedByTheLineLimit(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "p", Name: "_fallback", Arg: "check"}}, true, cardT0)
	var calls []daemon.ToolCall
	var results []daemon.ToolResult
	for i := 0; i < 10; i++ {
		calls = append(calls, daemon.ToolCall{ID: fmt.Sprintf("k%d", i), Name: fmt.Sprintf("tool_%d", i)})
		results = append(results, daemon.ToolResult{ID: fmt.Sprintf("k%d", i), Name: fmt.Sprintf("tool_%d", i)})
	}
	c.addSub("_fallback", []subagent.Frame{{Calls: calls}, {Results: results}}, true, cardT0)
	c.file([]daemon.ToolResult{{ID: "p", Name: "_fallback"}}, cardT0)
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash"}, {ID: "b", Name: "read"}}, true, cardT0)
	c.file([]daemon.ToolResult{{ID: "a", Name: "bash"}}, cardT0)

	const limit = 4
	seen := make(map[string]int)
	counted, messages := 0, 0
	for {
		f, ok := c.planFreeze(time.Second, limit)
		if !ok {
			break
		}
		if messages++; messages > 10 {
			t.Fatal("freezing never finished")
		}
		if got := stepLines(f.act.Steps); got > limit {
			t.Errorf("frozen message %d draws %d lines, want at most %d", messages, got, limit)
		}
		if anyRunning(f.act.Steps) {
			t.Errorf("frozen message %d shows a line running: %s", messages, titles(f.act.Steps))
		}
		for _, s := range f.act.Steps {
			if s.Title == "_fallback" {
				if s.Calls != 10 || s.Status != chat.StepDone {
					t.Errorf("message %d: the delegation's line = %+v, want it done and counting all 10", messages, s)
				}
			} else {
				seen[s.Title]++
			}
			for _, sub := range s.Steps {
				seen[sub.Title]++
			}
		}
		var n int
		if _, err := fmt.Sscanf(f.act.Title, "%d call", &n); err != nil {
			t.Fatalf("frozen title %q: %v", f.act.Title, err)
		}
		counted += n
		c.commitFreeze(f)
	}
	if messages != 4 {
		t.Errorf("froze into %d messages, want 4: three calls under the delegation's line each time, then the rest", messages)
	}
	for i := 0; i < 10; i++ {
		if name := fmt.Sprintf("tool_%d", i); seen[name] != 1 {
			t.Errorf("%s is frozen %d times, want exactly once", name, seen[name])
		}
	}
	if seen["bash"] != 1 || seen["read"] != 0 {
		t.Errorf("bash frozen %d times and read %d; want the finished one once and the running one never", seen["bash"], seen["read"])
	}
	// Each call is counted in one message's title: the 10, the delegation, bash.
	if counted != 12 {
		t.Errorf("the frozen titles count %d calls between them, want 12", counted)
	}
	if got := titles(c.live(0, false).Steps); got != "read" {
		t.Errorf("what is left is %q, want the one call still running", got)
	}
	if c.total != 13 {
		t.Errorf("total = %d, want the turn's 13", c.total)
	}
}

// A freeze never takes a call that has no result — the heading copy of a
// delegation's line aside, which is drawn unfinished. That holds under a
// delegation that has returned too: a call added under it afterwards is not
// assumed finished because its delegation is, and stays in the record, where a
// result can still find it.
func TestAFreezeNeverTakesACallWithNoResult(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "p", Name: "_fallback"}, {ID: "q", Name: "other"}}, true, cardT0)
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k1", Name: "grep"}}}, {Results: []daemon.ToolResult{{ID: "k1", Name: "grep"}}}}, true, cardT0)
	c.file([]daemon.ToolResult{{ID: "p", Name: "_fallback", Failed: true, Detail: "exit 2"}, {ID: "q", Name: "other"}}, cardT0)
	// Frames that arrive behind the delegation's result.
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k2", Name: "late"}}}}, true, cardT0)
	c.addSub("other", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k3", Name: "later"}}}}, true, cardT0)

	f, ok := c.planFreeze(time.Second, 100)
	if !ok {
		t.Fatal("nothing to freeze")
	}
	// _fallback's returned call is frozen under its line, which says what
	// the delegation's result was; "other" has nothing under it that has
	// returned, so nothing of it is frozen.
	if got, want := titles(f.act.Steps), "_fallback (exit 2)[grep]"; got != want {
		t.Errorf("frozen = %s, want %s", got, want)
	}
	if got, want := statuses(f.act.Steps), []chat.StepStatus{chat.StepFailed, chat.StepDone}; !reflect.DeepEqual(got, want) {
		t.Errorf("frozen statuses = %v, want %v", got, want)
	}
	c.commitFreeze(f)
	if got, want := titles(c.live(0, false).Steps), "_fallback (exit 2)[late] other[later]"; got != want {
		t.Errorf("after the freeze the card holds %s, want %s", got, want)
	}
	// Still addressable.
	if !c.addSub("_fallback", []subagent.Frame{{Results: []daemon.ToolResult{{ID: "k2", Name: "late", Failed: true}}}}, true, cardT0) {
		t.Error("the late call's result found nothing to answer")
	}
	if got := c.live(0, false).Steps[0].Steps[0].Status; got != chat.StepFailed {
		t.Errorf("the late call = %s, want its own verdict", got)
	}
}

// With nothing finished there is nothing to leave behind.
func TestACardWithNothingFinishedIsNotFrozen(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash"}, {ID: "p", Name: "_fallback"}}, true, cardT0)
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k", Name: "grep"}}}}, true, cardT0)
	if _, ok := c.planFreeze(time.Second, 100); ok {
		t.Error("a card whose every call is still running was frozen")
	}
	var empty turnCard
	if _, ok := empty.planFreeze(time.Second, 100); ok {
		t.Error("an empty card was frozen")
	}
}

// A result recorded between planning a freeze and committing it is not lost:
// the call it answered was not frozen, so it stays, answered.
func TestAResultDuringAFreezeIsKept(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash"}, {ID: "b", Name: "read"}}, true, cardT0)
	c.file([]daemon.ToolResult{{ID: "a", Name: "bash"}}, cardT0)
	f, _ := c.planFreeze(time.Second, 100)
	c.file([]daemon.ToolResult{{ID: "b", Name: "read", Failed: true}}, cardT0)
	c.add([]daemon.ToolCall{{ID: "c", Name: "write"}}, true, cardT0)
	c.commitFreeze(f)
	a := c.live(0, false)
	if got, want := titles(a.Steps), "read write"; got != want {
		t.Fatalf("after the commit the card holds %s, want %s", got, want)
	}
	if a.Steps[0].Status != chat.StepFailed {
		t.Errorf("read = %s, want the verdict that landed mid-freeze", a.Steps[0].Status)
	}
}

// The kept record's text says what its card does, so a client that cannot
// draw the card — or a Slack that refuses it — still shows the record: a final
// line, a line per call, a subagent's calls indented under the call that made
// them.
func TestAKeptCardSaysItsCallsInText(t *testing.T) {
	a := &chat.Activity{Title: "Done · 40s · 4 calls (1 failed)", Status: chat.StepDone, Steps: []chat.Step{
		{Title: "bash", Status: chat.StepDone, Code: "kubectl get pods -A", Note: "2.3s"},
		{Title: "_fallback", Status: chat.StepDone, Calls: 2, Code: "check `billing`", Steps: []chat.Step{
			{Title: "read_file", Status: chat.StepDone, Note: "<0.1s"},
			{Title: "grep ×2 (exit 1)", Status: chat.StepFailed},
		}},
	}}
	want := markOK + " Done · 40s · 4 calls (1 failed)\n" +
		markOK + " `bash`  2.3s  `kubectl get pods -A`\n" +
		markOK + " `_fallback` · 2 calls  `check " + inlineCode("`billing`") + "`\n" +
		"  ↳ " + markOK + " `read_file`  <0.1s\n" +
		"  ↳ " + markFailed + " `grep ×2 (exit 1)`"
	if got := cardText(a); got != want {
		t.Errorf("text =\n%s\nwant\n%s", got, want)
	}
	stopped := &chat.Activity{Title: "Stopped · 5s · 1 call", Status: chat.StepFailed, Steps: []chat.Step{{Title: "bash", Status: chat.StepFailed}}}
	if got, want := cardText(stopped), markFailed+" Stopped · 5s · 1 call\n"+markFailed+" `bash`"; got != want {
		t.Errorf("stopped text = %q, want %q", got, want)
	}
}

// A card in flight survives the state file and comes back as interrupted: the
// title says what happened to the card, and every line that was running reads
// unfinished.
func TestAPersistedCardIsFinalisedAsInterrupted(t *testing.T) {
	var c turnCard
	if c.persisted() != nil {
		t.Error("a record with no calls persisted something")
	}
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash", Arg: "ls"}, {ID: "p", Name: "_fallback", Arg: "check"}}, true, cardT0)
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k1", Name: "grep", Arg: "x"}, {ID: "k2", Name: "cat", Arg: "y"}}}}, true, cardT0)
	c.addSub("_fallback", []subagent.Frame{{Results: []daemon.ToolResult{{ID: "k1", Name: "grep", Failed: true, Detail: "exit 1"}}}}, true, cardT0)
	c.file([]daemon.ToolResult{{ID: "a", Name: "bash"}}, cardT0.Add(2*time.Second))

	raw, err := json.Marshal(c.persisted())
	if err != nil {
		t.Fatal(err)
	}
	var back cardRecord
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	got := back.interrupted()
	// Not "stopped": a restart of the gateway is not the turn failing. The
	// card reads failed — the icon that says look here — and its title says
	// what happened to it.
	if got.Title != "Interrupted by a restart · 4 calls" || got.Status != chat.StepFailed {
		t.Errorf("restored = %q (%s)", got.Title, got.Status)
	}
	if strings.Contains(got.Title, "Stopped") {
		t.Errorf("title %q says the turn stopped; a restart does not mean that", got.Title)
	}
	if want := "bash _fallback[grep (exit 1) cat]"; titles(got.Steps) != want {
		t.Errorf("restored steps = %s, want %s", titles(got.Steps), want)
	}
	// What was running reads unfinished, which is not a verdict; a verdict
	// that was seen — a call that finished, a call that failed — is kept.
	wantStatus := []chat.StepStatus{chat.StepDone, chat.StepUnfinished, chat.StepFailed, chat.StepUnfinished}
	if !reflect.DeepEqual(statuses(got.Steps), wantStatus) {
		t.Errorf("restored statuses = %v, want %v", statuses(got.Steps), wantStatus)
	}
	if got.Steps[0].Code != "ls" || got.Steps[0].Note != "2.0s" || got.Steps[1].Calls != 2 {
		t.Errorf("restored detail lost: %+v", got.Steps)
	}
	if got.Recent != nil {
		t.Errorf("an interrupted card carries recent lines")
	}
	// The text says the same.
	text := cardText(got)
	if !strings.HasPrefix(text, markFailed+" Interrupted by a restart · 4 calls\n") ||
		!strings.Contains(text, "\n– `_fallback` · 2 calls  `check`\n") || !strings.Contains(text, "  ↳ – `cat`  `y`") ||
		!strings.Contains(text, "  ↳ "+markFailed+" `grep (exit 1)`  ") {
		t.Errorf("text =\n%s", text)
	}
	// A status no build knows is not a verdict either, and is not running.
	odd := (&cardRecord{Calls: 1, Steps: []stepRecord{{Title: "bash", Status: "paused"}}}).interrupted()
	if odd.Steps[0].Status != chat.StepUnfinished {
		t.Errorf("an unknown persisted status drew as %s", odd.Steps[0].Status)
	}

	// The same from the record itself, for a card the process is about to
	// exit under.
	live := c.interrupted()
	if live.Title != got.Title || live.Status != chat.StepFailed || !reflect.DeepEqual(statuses(live.Steps), wantStatus) || live.Recent != nil {
		t.Errorf("at exit = %q (%s) %v, want what the restart draws", live.Title, live.Status, statuses(live.Steps))
	}
}

// Committing a freeze takes out of the record what the plan froze and nothing
// else. A subagent call recorded between the plan and the commit, under a
// delegation that had already returned and was frozen whole, used to leave
// with it: drawn in neither message, while the turn's count still held it.
func TestACallRecordedDuringAFreezeUnderAReturnedDelegationIsKept(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "p", Name: "_fallback", Arg: "check"}}, true, cardT0)
	c.addSub("_fallback", []subagent.Frame{
		{Calls: []daemon.ToolCall{{ID: "k1", Name: "tool_1"}}},
		{Results: []daemon.ToolResult{{ID: "k1", Name: "tool_1"}}},
	}, true, cardT0)
	c.file([]daemon.ToolResult{{ID: "p", Name: "_fallback", Failed: true, Detail: "exit 2"}}, cardT0)

	f, ok := c.planFreeze(time.Second, 100)
	if !ok {
		t.Fatal("nothing to freeze")
	}
	if got, want := titles(f.act.Steps), "_fallback (exit 2)[tool_1]"; got != want {
		t.Fatalf("frozen = %s, want %s", got, want)
	}
	if rest := c.rest(f); rest.holds() {
		t.Fatalf("the continuation planned holds %s, want nothing", titles(rest.live(0, false).Steps))
	}
	// The platform calls are in the air; the subagent reports another call.
	c.addSub("_fallback", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k2", Name: "tool_2"}}}}, true, cardT0)
	c.commitFreeze(f)

	if c.total != 3 {
		t.Fatalf("total = %d, want 3", c.total)
	}
	// k2 is in the record, under its delegation's line — which says what the
	// delegation's result was, and still counts both its calls.
	live := c.live(0, false)
	if got, want := titles(live.Steps), "_fallback (exit 2)[tool_2]"; got != want {
		t.Fatalf("after the commit the record holds %q, want %q: the call recorded during the freeze is in neither message", got, want)
	}
	if head := live.Steps[0]; head.Status != chat.StepFailed || head.Calls != 2 || head.Steps[0].Status != chat.StepRunning {
		t.Errorf("heading = %+v", head)
	}
	if !strings.HasSuffix(live.Title, " · 3 calls · _fallback › tool_2") {
		t.Errorf("title = %q, want the turn's count and the call still running", live.Title)
	}
	// Still addressable, and when it is frozen in its turn the delegation's
	// line is not counted a second time.
	if !c.addSub("_fallback", []subagent.Frame{{Results: []daemon.ToolResult{{ID: "k2", Name: "tool_2"}}}}, true, cardT0) {
		t.Fatal("k2's result found nothing to answer")
	}
	g, ok := c.planFreeze(time.Second, 100)
	if !ok || g.act.Title != "1 call · 1s" || titles(g.act.Steps) != "_fallback (exit 2)[tool_2]" {
		t.Fatalf("the next freeze = %+v, want the one call under its heading", g)
	}
	c.commitFreeze(g)
	if c.holds() {
		t.Errorf("the record still holds %s", titles(c.live(0, false).Steps))
	}
	// Nothing left to draw in this message, and still a turn with a card: it
	// has made calls, and whatever it makes next is recorded with them.
	if !c.started() {
		t.Error("a turn whose calls are all frozen no longer counts as having a card")
	}
	var empty turnCard
	if empty.started() {
		t.Error("a turn with no calls counts as having a card")
	}
}

// A delegation that returned and was frozen whole has left the record. If its
// subagent then reports more calls they get a fresh copy of its line to go
// under, in the message being edited now, and are counted — they used to find
// no delegation and vanish from the card and its count (or, in stream, fall
// through to notices of their own).
func TestFramesAfterADelegationWasFrozenGetAFreshHeading(t *testing.T) {
	for name, detail := range map[string]bool{"status": false, "stream": true} {
		t.Run(name, func(t *testing.T) {
			var c turnCard
			c.add([]daemon.ToolCall{{ID: "sp", Name: "spawn_agent", Subagent: "cluster", Arg: "check events"}, {ID: "a", Name: "bash", Arg: "ls"}}, detail, cardT0)
			c.addSub("cluster", []subagent.Frame{
				{Calls: []daemon.ToolCall{{ID: "k1", Name: "tool_1", Arg: "x"}}},
				{Results: []daemon.ToolResult{{ID: "k1", Name: "tool_1"}}},
			}, detail, cardT0)
			c.file([]daemon.ToolResult{{ID: "sp", Name: "spawn_agent"}}, cardT0)
			f, ok := c.planFreeze(time.Second, 100)
			if !ok {
				t.Fatal("nothing to freeze")
			}
			c.commitFreeze(f)
			if got := titles(c.live(0, false).Steps); got != "bash" {
				t.Fatalf("after the freeze the record holds %q, want the call still running alone", got)
			}
			if !c.follows("cluster") {
				t.Fatal("the record no longer follows a subagent whose delegation was frozen")
			}
			if c.follows("stranger") {
				t.Error("the record follows a subagent nothing delegated to")
			}

			// The subagent reports again.
			if !c.addSub("cluster", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k2", Name: "tool_2", Arg: "y"}, {ID: "k3", Name: "tool_3", Arg: "z"}}}}, detail, cardT0) {
				t.Fatal("frames after the delegation was frozen changed nothing")
			}
			if c.total != 5 {
				t.Errorf("total = %d, want the two new calls counted", c.total)
			}
			live := c.live(0, false)
			if got, want := titles(live.Steps), "bash spawn_agent[tool_2 tool_3]"; got != want {
				t.Fatalf("the record holds %q, want %q", got, want)
			}
			head := live.Steps[1]
			if head.Status != chat.StepDone || head.Calls != 3 {
				t.Errorf("fresh heading = %+v, want the delegation's own verdict and all three of its calls counted", head)
			}
			if !strings.HasSuffix(live.Title, " · 5 calls · cluster › tool_2, tool_3") {
				t.Errorf("title = %q, want what the subagent is running", live.Title)
			}
			// One heading, however many frames follow.
			c.addSub("cluster", []subagent.Frame{{Calls: []daemon.ToolCall{{ID: "k4", Name: "tool_4"}}}}, detail, cardT0)
			if got, want := titles(c.live(0, false).Steps), "bash spawn_agent[tool_2 tool_3 tool_4]"; got != want {
				t.Errorf("after another frame: %q, want %q", got, want)
			}
			// Results find them, and a message's own count leaves the
			// repeated heading out.
			c.addSub("cluster", []subagent.Frame{{Results: []daemon.ToolResult{{ID: "k2", Name: "tool_2"}, {ID: "k3", Name: "tool_3"}, {ID: "k4", Name: "tool_4"}}}}, detail, cardT0)
			g, ok := c.planFreeze(time.Second, 100)
			if !ok || g.act.Title != "3 calls · 1s" {
				t.Fatalf("the next freeze = %+v, want the three calls and not their heading counted", g.act)
			}
			if seg := c.segment(time.Second); seg.Title != "4 calls · 1s · 1 unfinished" {
				t.Errorf("superseded title = %q, want bash and the three calls, the heading left out", seg.Title)
			}
			if (live.Steps[1].Code != "") != detail {
				t.Errorf("heading argument %q in %s", live.Steps[1].Code, name)
			}
		})
	}
}

// In a card a call too quick to time usefully still says so. A notice prints
// nothing for it, as it always has; in a column of calls that made the quick
// one look like the one whose time had gone missing.
func TestACardSaysAFastCallWasFast(t *testing.T) {
	var c turnCard
	c.add([]daemon.ToolCall{{ID: "a", Name: "bash", Arg: "true"}, {ID: "b", Name: "read", Arg: "f"}}, true, cardT0)
	c.file([]daemon.ToolResult{{ID: "a", Name: "bash"}}, cardT0.Add(20*time.Millisecond))
	c.file([]daemon.ToolResult{{ID: "b", Name: "read"}}, cardT0.Add(2300*time.Millisecond))
	steps := c.final(time.Second, true).Steps
	if steps[0].Note != "<0.1s" || steps[1].Note != "2.3s" {
		t.Errorf("durations = %q and %q, want <0.1s and 2.3s", steps[0].Note, steps[1].Note)
	}
	// The notices are as they were: nothing under the threshold.
	fast := toolLine(toolGroup{call: daemon.ToolCall{Name: "bash", Arg: "true"}, res: &daemon.ToolResult{Name: "bash"}, n: 1, took: 20 * time.Millisecond}, true, true)
	if strings.Contains(fast, "⏱") {
		t.Errorf("a notice printed a duration for an instant call:\n%s", fast)
	}
}
