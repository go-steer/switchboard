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
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
	"github.com/go-steer/switchboard/pkg/subagent"
)

// Card mode.
//
// Where the adapter draws chat.Activity (Slack, as a task card), a turn's tool
// activity in status and stream mode is one message: the progress placeholder,
// edited in place for the whole turn and kept when the turn ends. The calls
// the turn made are the card's lines, a subagent's calls nested under the call
// that delegated to it; the card's title says where the turn has got to.
//
// turnCard is the record that message is drawn from. It is state, not
// rendering: every edit renders it afresh (redrawProgress), so an edit shows
// everything recorded before it and nothing is patched in place.
//
// Anywhere else — an adapter that draws no Activity, indicator and off — none
// of this runs, and the router does what it did before cards existed.

const (
	// defaultCardRedraw is the least time between two edits of a card caused
	// by calls, results and subagent frames. A busy turn produces several of
	// those a second and each would be an API edit; one edit per interval,
	// with a trailing one so the last state is always drawn, shows the same
	// thing. Slack took an edit every three seconds a hundred times running
	// (measured 2026-11-19).
	defaultCardRedraw = 3 * time.Second

	// defaultCardMaxLines is how many lines a card holds before it is frozen
	// where it is and the turn continues in a new message below. Half of what
	// a card was measured to hold, so a burst between two edits cannot carry
	// it past that.
	defaultCardMaxLines = 400

	// cardRecentLines is how many of a running turn's latest lines stream
	// mode shows under the card (chat.Activity.Recent). A card draws folded
	// to its title; these are what a reader sees of the turn's detail without
	// opening it.
	cardRecentLines = 5
)

// turnCard is one turn's calls, for the card they are drawn as. The zero value
// is an empty record, which draws nothing: the placeholder stays the plain
// clock until the first call.
type turnCard struct {
	// calls are the calls in the message being edited now, oldest first. A
	// long turn's earlier calls are frozen in earlier messages and are not
	// here; one whose result has not arrived always is.
	calls []*cardCall
	// total is every call the turn has made, a subagent's included and the
	// call that delegated to it counted once; failed is how many of them
	// failed. Both count the whole turn, whatever has been frozen.
	total, failed int
	// gone are the delegations that have returned and left the record with a
	// frozen message, by the subagent each drove: what its line said. If the
	// subagent reports more calls afterwards they go under a fresh copy of
	// that line in the message being edited now (addSub), rather than nowhere.
	gone map[string]*cardCall
}

// cardCall is one call. Names and verdicts always; the argument and the
// duration only where the mode discloses them (detail), since a summary never
// stored is one no edit can leak.
type cardCall struct {
	id, name string
	arg      string
	detail   bool
	// sub is the subagent branch this call would drive, if it drives one:
	// what its poller is keyed by, and so how a subagent's frames find the
	// call they belong under.
	sub string
	// res is the call's result; nil while it runs.
	res *daemon.ToolResult
	// took is how long the call ran, and timed whether that was measured: a
	// call closed with its delegation rather than by its own result has no
	// duration to show.
	took  time.Duration
	timed bool
	// seen is when the call reached the gateway, and at the subagent's own
	// timestamp for it, where the daemon recorded one.
	seen, at time.Time
	// children are the calls a subagent made under this one that are in this
	// message; subCalls is every call it has made, frozen ones included.
	children []*cardCall
	subCalls int
	// heading says this call's own line has already been drawn, and counted,
	// in a message frozen earlier: it is here again only to head calls its
	// subagent reported after that. A message's own count of calls leaves it
	// out.
	heading bool
}

// holds reports whether the record has a call to draw. It is what makes a
// progress message a card rather than a clock, and so what is kept when the
// turn ends.
func (t *turnCard) holds() bool { return len(t.calls) > 0 }

// started reports whether the turn has recorded a call at all, in this message
// or one frozen earlier. A turn that has is a turn with a card, and stays one
// to its end: its calls, results and subagent frames keep being recorded here
// whatever becomes of the adapter's ability to draw them.
func (t *turnCard) started() bool { return t.total > 0 }

// follows reports whether the record has somewhere to put a subagent's
// frames: a call that delegated to it, in this message or one frozen earlier.
func (t *turnCard) follows(sub string) bool {
	return t.delegation(sub) != nil || t.gone[sub] != nil
}

// add records a frame of the agent's own calls. detail is the mode's
// disclosure: stream keeps each call's summarised argument, status keeps the
// name alone.
func (t *turnCard) add(calls []daemon.ToolCall, detail bool, now time.Time) {
	for _, c := range calls {
		cc := &cardCall{id: c.ID, name: c.Name, detail: detail, seen: now}
		if detail {
			cc.arg = c.Arg
		}
		if sub, _, ok := subagentTarget(c); ok {
			cc.sub = sub
		}
		t.calls = append(t.calls, cc)
		t.total++
	}
}

// file records a frame of the agent's own results, reporting whether any
// answered a call and handing back the ones that did not — a stream notice
// posted before the card existed may be waiting for those.
//
// The matching is resolveTool's: by call id where there is one (newest first,
// the safe reading of a repeated id), and otherwise the oldest unanswered call
// of the same name — with every id in the frame filed before any name is
// guessed at, as applyToolResults does.
func (t *turnCard) file(results []daemon.ToolResult, now time.Time) (changed bool, rest []daemon.ToolResult) {
	set := func(c *cardCall, r daemon.ToolResult) {
		t.settle(c, r, now.Sub(c.seen))
		// The delegation has returned, so whatever its subagent was running
		// is over: a child whose own result never reached the gateway is
		// closed with it rather than left drawn as running.
		for _, ch := range c.children {
			if ch.res == nil {
				ch.res = &daemon.ToolResult{Name: ch.name}
			}
		}
		changed = true
	}
	for _, r := range results {
		if r.ID == "" {
			continue
		}
		found := false
		for i := len(t.calls) - 1; i >= 0; i-- {
			if c := t.calls[i]; c.id == r.ID && c.res == nil {
				set(c, r)
				found = true
				break
			}
		}
		if !found {
			rest = append(rest, r)
		}
	}
	for _, r := range results {
		if r.ID != "" {
			continue
		}
		found := false
		for _, c := range t.calls {
			if c.name == r.Name && c.res == nil {
				set(c, r)
				found = true
				break
			}
		}
		if !found {
			rest = append(rest, r)
		}
	}
	return changed, rest
}

// settle files r against c.
func (t *turnCard) settle(c *cardCall, r daemon.ToolResult, took time.Duration) {
	c.res = &r
	c.took, c.timed = took, true
	if r.Failed {
		t.failed++
	}
}

// addSub records a batch of a subagent's frames: its calls become children of
// the call that delegated to it, and its results tick those children off — by
// id first, then by name, within that subagent, as applySubagentFrames matches
// its notices. A call's duration is the subagent's own, from its event
// timestamps (subagentTook). It reports whether anything changed.
//
// Frames with no delegation to go under are dropped. That is a poller that
// has outlived its turn: the record was reset for the next one, and these
// calls belong to neither.
func (t *turnCard) addSub(sub string, frames []subagent.Frame, detail bool, now time.Time) (changed bool) {
	for _, f := range frames {
		if len(f.Calls) > 0 {
			p := t.delegation(sub)
			if p == nil {
				p = t.rehead(sub)
			}
			if p != nil {
				for _, c := range f.Calls {
					ch := &cardCall{id: c.ID, name: c.Name, detail: detail, seen: now, at: f.At}
					if detail {
						ch.arg = c.Arg
					}
					p.children = append(p.children, ch)
					p.subCalls++
					t.total++
				}
				changed = true
			}
		}
		for _, r := range f.Results {
			if r.ID == "" {
				continue
			}
			if ch := t.subCall(sub, func(c *cardCall) bool { return c.id == r.ID }, true); ch != nil {
				t.settle(ch, r, subagentTook(ch.at, f.At, ch.seen))
				changed = true
			}
		}
		for _, r := range f.Results {
			if r.ID != "" {
				continue
			}
			if ch := t.subCall(sub, func(c *cardCall) bool { return c.name == r.Name }, false); ch != nil {
				t.settle(ch, r, subagentTook(ch.at, f.At, ch.seen))
				changed = true
			}
		}
	}
	return changed
}

// rehead gives a subagent's calls somewhere to go when the delegation that
// drove it has returned and left the record with a frozen message: a fresh
// copy of the delegation's line, saying what the original said, in the message
// being edited now. Without it those calls were in no message, and missing
// from the count. nil when no delegation to this subagent has left.
func (t *turnCard) rehead(sub string) *cardCall {
	was := t.gone[sub]
	if was == nil {
		return nil
	}
	delete(t.gone, sub)
	head := *was
	head.children, head.heading = nil, true
	t.calls = append(t.calls, &head)
	return &head
}

// delegation is the call a subagent's frames go under: the oldest call
// driving that subagent that has not returned, or — for frames that arrive
// behind its result — the newest that has.
func (t *turnCard) delegation(sub string) *cardCall {
	var last *cardCall
	for _, c := range t.calls {
		if c.sub != sub {
			continue
		}
		if c.res == nil {
			return c
		}
		last = c
	}
	return last
}

// subCall finds an unanswered call a subagent made: the newest matching one
// when newest is set (an id), the oldest otherwise (a name) — the two
// directions resolveTool searches in, for the reasons it gives.
func (t *turnCard) subCall(sub string, match func(*cardCall) bool, newest bool) *cardCall {
	var found *cardCall
	for _, p := range t.calls {
		if p.sub != sub {
			continue
		}
		for _, ch := range p.children {
			if ch.res != nil || !match(ch) {
				continue
			}
			if !newest {
				return ch
			}
			found = ch
		}
	}
	return found
}

// cardView is how a call whose result has not arrived is drawn.
type cardView int

const (
	// cardLive draws it running: the turn is.
	cardLive cardView = iota
	// cardDone draws it done: the message is finished, and so is whatever it
	// shows.
	cardDone
	// cardStopped draws it failed: the turn was stopped under it.
	cardStopped
	// cardLeft draws it unfinished: the message is being left behind while
	// the call is still out, and will not be drawn again. Whatever becomes of
	// the call, this message never learns it, so it says neither done nor
	// failed (chat.StepUnfinished).
	cardLeft
)

// status is c's state as view draws it.
func (c *cardCall) status(view cardView) chat.StepStatus {
	switch {
	case c.res == nil && view == cardLive:
		return chat.StepRunning
	case c.res == nil && view == cardStopped:
		return chat.StepFailed
	case c.res == nil && view == cardLeft:
		return chat.StepUnfinished
	case c.res != nil && c.res.Failed:
		return chat.StepFailed
	}
	return chat.StepDone
}

// cardSteps renders calls as steps, adjacent identical ones collapsed into one
// line with a count — groupCalls' rule, at each level: same tool, same
// argument, same verdict. A call with calls under it is never collapsed.
func cardSteps(calls []*cardCall, view cardView) []chat.Step {
	var steps []chat.Step
	var prev *cardCall
	n := 0
	flush := func() {
		if prev == nil {
			return
		}
		s := chat.Step{Title: plainName(prev.name), Status: prev.status(view), Calls: prev.subCalls}
		if n > 1 {
			s.Title += " ×" + strconv.Itoa(n)
		}
		if prev.res != nil && prev.res.Failed && prev.res.Detail != "" {
			s.Title += " (" + prev.res.Detail + ")"
		}
		if prev.detail {
			s.Code = prev.arg
			// A run shows no duration: its calls each took their own time
			// and no one number is the run's.
			if n == 1 && prev.timed {
				s.Note = cardTook(prev.took)
			}
		}
		if len(prev.children) > 0 {
			s.Steps = cardSteps(prev.children, view)
		}
		steps = append(steps, s)
	}
	for _, c := range calls {
		if prev != nil && len(prev.children) == 0 && len(c.children) == 0 && prev.subCalls == 0 && c.subCalls == 0 &&
			prev.name == c.name && prev.arg == c.arg && prev.detail == c.detail && sameVerdict(prev.res, c.res) {
			n++
			continue
		}
		flush()
		prev, n = c, 1
	}
	flush()
	return steps
}

// cardTook is how long a call took, as a card's line says it. A notice prints
// no time for a call under minShownTook, where the number is noise; in a
// card's column of calls that left the one quick call looking like the one
// whose time had gone missing, so here it reads "<0.1s".
func cardTook(d time.Duration) string {
	if d < minShownTook {
		return "<0.1s"
	}
	return formatTook(d)
}

// stepLines is how many lines steps draw: one each, and one for each step
// under one.
func stepLines(steps []chat.Step) int {
	n := len(steps)
	for _, s := range steps {
		n += len(s.Steps)
	}
	return n
}

// live renders the record for a turn in flight:
//
//	Working… 3m10s · 26 calls · bash
//	Working… 3m10s · 26 calls · _fallback › get_k8s_logs
//
// recent is whether the latest lines are also shown under the card (stream
// mode); a status card is its title alone until it is opened. A card that has
// finished never shows them — see final, segment and interrupted — so the
// message folds to its one title line.
func (t *turnCard) live(elapsed time.Duration, recent bool) *chat.Activity {
	title := "Working… " + formatElapsed(elapsed) + " · " + callCount(t.total)
	if what := t.runningNow(); what != "" {
		title += " · " + what
	}
	a := &chat.Activity{Title: title, Status: chat.StepRunning, Steps: cardSteps(t.calls, cardLive)}
	if recent {
		a.Recent = recentSteps(a.Steps, cardRecentLines)
	}
	return a
}

// recentSteps is the latest n lines of steps as one flat list, oldest first:
// each step's line and then the lines under it, nesting dropped. A line keeps
// what the card's draws beside it — argument, duration, the count of calls
// under it — so the mode's disclosure is the same in both places.
func recentSteps(steps []chat.Step, n int) []chat.Step {
	var flat []chat.Step
	for _, s := range steps {
		line := s
		line.Steps = nil
		flat = append(flat, line)
		for _, sub := range s.Steps {
			sub.Steps = nil
			flat = append(flat, sub)
		}
	}
	if len(flat) > n {
		flat = flat[len(flat)-n:]
	}
	return flat
}

// final renders the record for a turn that has ended. No line is left reading
// as running: a call whose result never arrived finished with the turn, or
// failed with it when the turn was stopped.
//
//	Done · 5m02s · 61 calls (2 failed)
//	Stopped · 1m12s · 9 calls
func (t *turnCard) final(elapsed time.Duration, done bool) *chat.Activity {
	if !done {
		return &chat.Activity{
			Title:  "Stopped · " + formatElapsed(elapsed) + " · " + callCount(t.total),
			Status: chat.StepFailed,
			Steps:  cardSteps(t.calls, cardStopped),
		}
	}
	title := "Done · " + formatElapsed(elapsed) + " · " + callCount(t.total)
	if t.failed > 0 {
		title += " (" + strconv.Itoa(t.failed) + " failed)"
	}
	return &chat.Activity{Title: title, Status: chat.StepDone, Steps: cardSteps(t.calls, cardDone)}
}

// segment renders the record as a finished stretch of a turn that carries on
// elsewhere — a turn a newer message has taken the progress slot from. The
// title counts this message's calls, not the turn's:
//
//	9 calls · 1m12s
//	32 calls · 2m15s · 1 unfinished
//
// The whole record is left behind here, calls still out included, and nothing
// will file their results: the record the new message draws from starts
// empty. Such a call is drawn unfinished, not done — a delegation that never
// returned read "✔ _fallback" over thirty-one failed calls (seen live) — and
// the title says how many there are. The card itself still reads done: the
// usual cause is a follow-up question in the middle of a healthy turn.
func (t *turnCard) segment(elapsed time.Duration) *chat.Activity {
	n, out := 0, 0
	count := func(c *cardCall) {
		if !c.heading {
			n++
		}
		if c.res == nil {
			out++
		}
	}
	for _, c := range t.calls {
		count(c)
		for _, ch := range c.children {
			count(ch)
		}
	}
	a := segmentActivity(t.calls, n, elapsed)
	if out > 0 {
		a.Title += " · " + strconv.Itoa(out) + " unfinished"
	}
	return a
}

// segmentActivity is a stretch of a turn in a message that will not be drawn
// again, n calls of it finishing there. Nothing in it reads as running. A call
// with no result reads unfinished: in a stretch frozen by a rollover or by
// narration that is only ever the line of a delegation still out, there to
// head the calls under it that have returned, and the delegation's own verdict
// lands on its line in the message the turn continues in.
func segmentActivity(calls []*cardCall, n int, elapsed time.Duration) *chat.Activity {
	return &chat.Activity{
		Title:  callCount(n) + " · " + formatElapsed(elapsed),
		Status: chat.StepDone,
		Steps:  cardSteps(calls, cardLeft),
	}
}

// runningNow names what the turn is running, for the title: a subagent's tool
// under the call that delegated to it, or else the calls still unanswered.
func (t *turnCard) runningNow() string {
	for i := len(t.calls) - 1; i >= 0; i-- {
		c := t.calls[i]
		if c.sub == "" {
			continue
		}
		// Whether or not the delegation itself has returned: a subagent
		// that reports calls after it has is still running them.
		if inner := runningNames(c.children); inner != "" {
			return plainName(c.sub) + " › " + inner
		}
	}
	return runningNames(t.calls)
}

// runningNames lists the unanswered calls by name, comma-joined, a run of the
// same name collapsed to "name ×N".
func runningNames(calls []*cardCall) string {
	var parts []string
	prev, n := "", 0
	flush := func() {
		switch {
		case n == 1:
			parts = append(parts, plainName(prev))
		case n > 1:
			parts = append(parts, plainName(prev)+" ×"+strconv.Itoa(n))
		}
	}
	for _, c := range calls {
		if c.res != nil {
			continue
		}
		if n > 0 && c.name == prev {
			n++
			continue
		}
		flush()
		prev, n = c.name, 1
	}
	flush()
	return strings.Join(parts, ", ")
}

// cardFreeze is a card about to be frozen where it is: what the frozen message
// will say, and which calls leave the record with it.
type cardFreeze struct {
	act *chat.Activity
	// kids are, for each call with a line in the frozen message, the calls
	// under it that are frozen with it. They are exactly what leaves the
	// record: a call recorded under it after the plan was made is not in the
	// frozen message, and stays.
	kids map[*cardCall]map[*cardCall]bool
	// whole are the calls whose own line is frozen for good: they have
	// returned, and everything under them when the plan was made is frozen
	// too. Such a call leaves the record, unless something has been recorded
	// under it since. Any other call in kids stays, with what is left under
	// it, and its line in the frozen message is a copy to head its calls.
	whole map[*cardCall]bool
}

// left is what stays under c once f is committed: everything f did not freeze.
func (f *cardFreeze) left(c *cardCall) []*cardCall {
	gone := f.kids[c]
	if len(gone) == 0 {
		return c.children
	}
	var left []*cardCall
	for _, ch := range c.children {
		if !gone[ch] {
			left = append(left, ch)
		}
	}
	return left
}

// planFreeze works out how to freeze the card: calls that have returned stay
// behind, oldest first and up to limit lines, and every call that has not goes
// on to the next message — so the frozen one shows nothing as running, and no
// call a result may still arrive for stops being addressable.
//
// A delegation heads both messages when it has to: one still in flight leaves
// the calls its subagent has finished behind under a copy of its line, and one
// with more calls under it than the frozen message has room for leaves as many
// as fit. Either way the delegation's own line stays in the record with the
// rest, and what is left over is frozen by the next rollover. The copy of a
// line whose delegation is still out is the one thing frozen without a result,
// and it is drawn unfinished (segmentActivity): the frozen message is not
// edited when the delegation returns, so it must not say how it went.
//
// The limit is what keeps a frozen message a card: frozen whole, a burst that
// arrived between two edits could be more lines than a card is known to hold.
//
// ok is false when nothing has returned yet. Then there is nothing to leave
// behind, and the card is better moved whole.
//
// Nothing is changed here: the caller posts and edits first, and commits only
// what the platform took (commitFreeze).
func (t *turnCard) planFreeze(elapsed time.Duration, limit int) (f *cardFreeze, ok bool) {
	f = &cardFreeze{whole: make(map[*cardCall]bool), kids: make(map[*cardCall]map[*cardCall]bool)}
	var frozen []*cardCall
	// n counts the calls that finish in the frozen message. A call whose
	// line is only there to head some of its calls is not one of them: it is
	// counted where it leaves the record.
	n := 0
	// lines counts calls, which is at least the lines they draw: a run of
	// identical ones draws as one.
	lines := 0
	for _, c := range t.calls {
		if lines >= limit {
			break
		}
		// What of c can stay behind: the calls under it that have returned,
		// and c itself once it has and nothing under it is still out. A
		// delegation's result closes whatever was open under it, so one that
		// has returned normally goes whole; a call added under it afterwards
		// is not assumed finished because its delegation is.
		var under []*cardCall
		for _, ch := range c.children {
			if ch.res != nil {
				under = append(under, ch)
			}
		}
		if len(under) == 0 && (c.res == nil || len(c.children) > 0) {
			continue // nothing of it has returned, or nothing under it has
		}
		if room := limit - lines - 1; len(under) > room {
			if room <= 0 {
				break
			}
			under = under[:room]
		}
		lines += 1 + len(under)
		set := make(map[*cardCall]bool, len(under))
		for _, ch := range under {
			set[ch] = true
		}
		f.kids[c] = set
		// The frozen message gets a copy of the line, over exactly the calls
		// frozen with it: the call itself may yet gain more.
		head := *c
		head.children = under
		frozen = append(frozen, &head)
		n += len(under)
		if c.res != nil && len(under) == len(c.children) {
			f.whole[c] = true
			if !c.heading {
				n++
			}
		}
	}
	if len(frozen) == 0 {
		return nil, false
	}
	f.act = segmentActivity(frozen, n, elapsed)
	return f, true
}

// rest is the record as it will be once f is committed, for rendering the
// message the turn continues in before anything has been changed.
func (t *turnCard) rest(f *cardFreeze) turnCard {
	next := turnCard{total: t.total, failed: t.failed}
	for _, c := range t.calls {
		left := f.left(c)
		if f.whole[c] && len(left) == 0 {
			continue
		}
		if len(left) != len(c.children) {
			head := *c
			head.children = left
			head.heading = head.heading || f.whole[c]
			next.calls = append(next.calls, &head)
			continue
		}
		next.calls = append(next.calls, c)
	}
	return next
}

// commitFreeze takes out of the record what f froze, and nothing else. Calls
// and results recorded since the plan was made are untouched: a frozen call
// had already returned, so nothing since can have changed it, and a call
// recorded since is in no frozen message and must stay in this one — under a
// delegation f froze whole too, which then stays to head it (it was "drawn in
// neither message", caught in review).
func (t *turnCard) commitFreeze(f *cardFreeze) {
	kept := t.calls[:0:0]
	for _, c := range t.calls {
		left := f.left(c)
		if f.whole[c] && len(left) == 0 {
			// Gone for good. If it drove a subagent, remember its line: the
			// subagent may still report (rehead).
			if c.sub != "" {
				if t.gone == nil {
					t.gone = make(map[string]*cardCall)
				}
				was := *c
				was.children = nil
				t.gone[c.sub] = &was
			}
			continue
		}
		c.children = left
		// Its line was frozen as finished and counted there; here it is a
		// heading from now on.
		c.heading = c.heading || f.whole[c]
		kept = append(kept, c)
	}
	t.calls = kept
}

// cardText is a finished Activity as text, in the router's markup: its title
// under the mark for how it ended, then a line per step, a step's own steps
// indented under it. It is what goes beside the card as Reply.Text, so a
// client that cannot draw the card — or a Slack that refuses it — still shows
// the whole record.
//
//	✔ Done · 40s · 3 calls
//	✔ `bash`  2.3s  `kubectl get pods -A`
//	✔ `_fallback` · 1 call
//	  ↳ ✔ `read_file`  <0.1s  `deploy/billing.yaml`
//
// Titles and arguments go in code spans: a tool name may begin with an
// underscore, and an argument is the agent's.
func cardText(a *chat.Activity) string {
	lines := []string{stepMark(a.Status) + " " + a.Title}
	for _, s := range a.Steps {
		lines = append(lines, stepText(s))
		for _, sub := range s.Steps {
			lines = append(lines, "  ↳ "+stepText(sub))
		}
	}
	return strings.Join(lines, "\n")
}

// stepText is one step as a line of text, in the order the card draws it.
func stepText(s chat.Step) string {
	line := stepMark(s.Status) + " `" + inlineCode(s.Title) + "`"
	if s.Calls > 0 {
		line += " · " + callCount(s.Calls)
	}
	if s.Note != "" {
		line += "  " + s.Note
	}
	if s.Code != "" {
		line += "  `" + inlineCode(s.Code) + "`"
	}
	return line
}

// markUnfinished leads a line no result was seen for (chat.StepUnfinished): an
// en dash, which is none of the three marks a verdict or a running call gets.
const markUnfinished = "–"

// stepMark is the mark the notices use for a state.
func stepMark(s chat.StepStatus) string {
	switch s {
	case chat.StepDone:
		return markOK
	case chat.StepFailed:
		return markFailed
	case chat.StepUnfinished:
		return markUnfinished
	}
	return markRunning
}

// callCount is "1 call" or "n calls".
func callCount(n int) string {
	if n == 1 {
		return "1 call"
	}
	return strconv.Itoa(n) + " calls"
}

// plainName makes an agent-supplied tool name fit to draw as literal text: it
// keeps what tool names are made of — letters, digits and ._:/- — and drops
// the rest; a name with nothing left reads "?".
//
// boldName's whitelist, but for the underscore. There it is kept only between
// two name characters, since at either end it reads as emphasis inside **…**;
// a card's title and lines are drawn literally, so it is kept wherever it
// falls and "_fallback" reads "_fallback".
func plainName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._:/-", r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "?"
	}
	return b.String()
}

// cardRecord is a card in flight as the state file keeps it: the steps it
// drew and how many calls its turn had made. It is what lets the next process
// finalise the message rather than delete it or leave it spinning.
type cardRecord struct {
	Calls int          `json:"calls"`
	Steps []stepRecord `json:"steps,omitempty"`
}

// stepRecord is a chat.Step on disk.
type stepRecord struct {
	Title  string       `json:"title"`
	Status string       `json:"status,omitempty"`
	Code   string       `json:"code,omitempty"`
	Note   string       `json:"note,omitempty"`
	Calls  int          `json:"calls,omitempty"`
	Steps  []stepRecord `json:"steps,omitempty"`
}

// persisted is the record as the state file keeps it, or nil when it holds no
// call.
func (t *turnCard) persisted() *cardRecord {
	if !t.holds() {
		return nil
	}
	return &cardRecord{Calls: t.total, Steps: stepRecords(cardSteps(t.calls, cardLive))}
}

func stepRecords(steps []chat.Step) []stepRecord {
	if len(steps) == 0 {
		return nil
	}
	out := make([]stepRecord, len(steps))
	for i, s := range steps {
		out[i] = stepRecord{Title: s.Title, Status: string(s.Status), Code: s.Code, Note: s.Note, Calls: s.Calls, Steps: stepRecords(s.Steps)}
	}
	return out
}

// interrupted renders a card the gateway restarted under, from what the state
// file kept of it.
//
//	Interrupted by a restart · 9 calls
//
// Not "stopped": a restart of the gateway is not the turn failing. The daemon
// carries on, and with a state file the answer still arrives. What is true is
// that this card stopped being followed, so the card reads failed — the icon
// that says look here — while a line that was still running reads unfinished,
// which is not a verdict: its result, if it came, went to a process that is
// gone.
func (c *cardRecord) interrupted() *chat.Activity {
	return &chat.Activity{
		Title:  interruptedTitle(c.Calls),
		Status: chat.StepFailed,
		Steps:  interruptedSteps(c.Steps),
	}
}

// interrupted is the same for a card the process is about to exit under, from
// the record itself.
func (t *turnCard) interrupted() *chat.Activity {
	return &chat.Activity{
		Title:  interruptedTitle(t.total),
		Status: chat.StepFailed,
		Steps:  cardSteps(t.calls, cardLeft),
	}
}

func interruptedTitle(calls int) string {
	return "Interrupted by a restart · " + callCount(calls)
}

func interruptedSteps(recs []stepRecord) []chat.Step {
	if len(recs) == 0 {
		return nil
	}
	out := make([]chat.Step, len(recs))
	for i, s := range recs {
		status := chat.StepStatus(s.Status)
		if status != chat.StepDone && status != chat.StepFailed {
			// Running, or anything this build does not know: no result was
			// seen, and none will be. A verdict that was seen is kept.
			status = chat.StepUnfinished
		}
		out[i] = chat.Step{Title: s.Title, Status: status, Code: s.Code, Note: s.Note, Calls: s.Calls, Steps: interruptedSteps(s.Steps)}
	}
	return out
}
