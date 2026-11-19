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
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
	"github.com/go-steer/switchboard/pkg/subagent"
)

// Subagent inner-tool progress (#193).
//
// A parent session's SSE /events stream carries only the parent's own frames:
// when the parent delegates to a synchronous specialist (_fallback,
// propose-change) or synchronously awaits a spawned subagent (spawn_agent with
// wait: true), the SSE stream emits one outer functionCall at the start and one
// functionResponse at the end, nothing between. For a multi-step delegation
// that takes thirty to ninety seconds, stream mode sits silent and status mode
// appears frozen on the outer tool name.
//
// While a parent tool call that can own a subagent branch is in flight, one
// background poller per subagent name tails:
//
//	GET /sessions/<app>/<sid>/agents/<name>/events?since=<cursor>
//
// at ~1s until the parent call's ToolResult arrives or the turn ends. Ordinary
// tools that succeed without creating a subagent branch answer 404 on their
// completion drain and are remembered on the session so later calls to them
// never poll (while live 404 probes back off exponentially so slow tools or
// subagents parked on an approval gate do not poll at 1 Hz or poison the
// cache); daemons without the route (501) or without an event log (412) turn
// subagent polling off for the session on the first probe.

const (
	// defaultSubagentPollInterval is how often an active subagent branch is
	// polled for new inner tool calls and results.
	defaultSubagentPollInterval = time.Second

	// maxSubagentPagesPerTick bounds cursor pagination within one live poll so
	// a chatty subagent cannot hold the poller indefinitely on a single tick.
	maxSubagentPagesPerTick = 10

	// maxSubagentPagesOnDrain allows a wider catch-up window on the single
	// synchronous completion drain when the parent ToolResult arrives.
	maxSubagentPagesOnDrain = 50
)

// subWatch tracks one in-flight parent tool call that may be driving a
// subagent branch.
type subWatch struct {
	id         string // parent FunctionCall.ID, when present
	parentTool string // parent FunctionCall.Name (e.g. "propose-change" or "spawn_agent")
	fromSpawn  bool   // true when parentTool is spawn_agent
}

// subPoller coordinates the 1s background ticker for one subagent name with
// the synchronous final drain that runs when the parent call's ToolResult
// arrives. pollMu is never acquired under any sessionEntry lock.
type subPoller struct {
	cancel context.CancelFunc
	// base is the context the poller's own was derived from: the relay's.
	// Work the poller starts that must outlive it — a card redraw owed after
	// its last poll — runs on this one.
	base       context.Context
	turn       int64
	nonSpawn   bool // guarded by sessionEntry.smu
	sawSuccess bool // guarded by sessionEntry.smu
	pollMu     sync.Mutex
	misses     atomic.Int32
	done       atomic.Bool
}

func (r *Router) subagentPollInterval() time.Duration {
	if r.subPollInterval > 0 {
		return r.subPollInterval
	}
	return defaultSubagentPollInterval
}

// subagentTarget maps a parent tool call to the subagent branch name to poll,
// reporting fromSpawn for spawn_agent calls and ok=false when the call cannot
// address a subagent branch.
func subagentTarget(c daemon.ToolCall) (name string, fromSpawn, ok bool) {
	switch c.Name {
	case "spawn_agent":
		return c.Subagent, true, subagent.ValidName(c.Subagent)
	case "stop_agent", "spawn_remote_agent":
		return "", false, false
	default:
		return c.Name, false, subagent.ValidName(c.Name)
	}
}

// setSubFloor raises the minimum event seq from which newly discovered
// subagents on this entry begin polling, so an adopted or revived session does
// not replay subagent turns that ran before switchboard attached.
func (e *sessionEntry) setSubFloor(seq int64) {
	if seq <= 0 {
		return
	}
	e.smu.Lock()
	if seq > e.subFloor {
		e.subFloor = seq
	}
	e.smu.Unlock()
}

// hasSubagentWatches reports whether any parent call on this entry is currently
// watching a subagent branch.
func (e *sessionEntry) hasSubagentWatches() bool {
	e.smu.Lock()
	defer e.smu.Unlock()
	return len(e.subWatches) > 0
}

func (e *sessionEntry) markNoSubagents() {
	e.smu.Lock()
	e.noSubagents = true
	pollers := e.subPollers
	e.subWatches = nil
	e.subPollers = nil
	e.smu.Unlock()
	for _, p := range pollers {
		p.done.Store(true)
		p.cancel()
	}
}

func (e *sessionEntry) markNotSubagent(name string) {
	e.smu.Lock()
	if e.notSubagent == nil {
		e.notSubagent = make(map[string]bool)
	}
	e.notSubagent[name] = true
	e.smu.Unlock()
}

func (e *sessionEntry) retireSubagentPoller(name string, p *subPoller) {
	e.smu.Lock()
	if e.subPollers[name] == p {
		delete(e.subPollers, name)
		delete(e.subWatches, name)
	}
	e.smu.Unlock()
	p.done.Store(true)
	p.cancel()
}

// stopAllSubagents cancels every active subagent poller on the entry and drops
// in-flight watch claims, while keeping subSeq, subFloor, noSubagents, and
// notSubagent intact for subsequent turns on the same session.
func (e *sessionEntry) stopAllSubagents() {
	e.smu.Lock()
	if len(e.subPollers) == 0 && len(e.subWatches) == 0 {
		e.smu.Unlock()
		return
	}
	pollers := e.subPollers
	e.subWatches = nil
	e.subPollers = nil
	e.smu.Unlock()
	for _, p := range pollers {
		p.done.Store(true)
		p.cancel()
	}
}

// startSubagentWatches begins polling the subagent turn-log route for each
// candidate parent tool call in calls (status and stream modes only). callSeq
// is the parent event's seq: because core-agent assigns seq monotonically
// across the root session tree and persists the parent FunctionCall before
// invoking the subagent, every inner event produced by this call has
// seq > callSeq. Fast tools that return before the first poll tick pay only
// the single synchronous drain on ToolResult (and zero polls thereafter once
// cached in notSubagent).
func (r *Router) startSubagentWatches(ctx context.Context, conv string, e *sessionEntry, mode ProgressMode, callSeq int64, calls []daemon.ToolCall) {
	if mode != ProgressStatus && mode != ProgressStream {
		return
	}
	sc := r.subagentsOf(e)
	if sc == nil {
		return
	}
	turn := e.turnSeq.Load()

	type launch struct {
		name string
		p    *subPoller
		ctx  context.Context
	}
	var toLaunch []launch

	e.smu.Lock()
	if e.noSubagents {
		e.smu.Unlock()
		return
	}
	for _, c := range calls {
		subName, fromSpawn, ok := subagentTarget(c)
		if !ok || (!fromSpawn && e.notSubagent[subName]) {
			continue
		}
		if e.subWatches == nil {
			e.subWatches = make(map[string][]subWatch)
		}
		if e.subPollers == nil {
			e.subPollers = make(map[string]*subPoller)
		}
		if e.subSeq == nil {
			e.subSeq = make(map[string]int64)
		}
		if e.subCallTotal == nil {
			e.subCallTotal = make(map[string]int)
		}
		if len(e.subWatches[subName]) == 0 {
			e.subCallTotal[subName] = 0
			e.subSeq[subName] = max(e.subSeq[subName], callSeq, e.subFloor)
		}
		e.subWatches[subName] = append(e.subWatches[subName], subWatch{
			id:         c.ID,
			parentTool: c.Name,
			fromSpawn:  fromSpawn,
		})
		p := e.subPollers[subName]
		if p == nil {
			pctx, cancel := context.WithCancel(ctx)
			p = &subPoller{cancel: cancel, base: ctx, turn: turn}
			e.subPollers[subName] = p
			toLaunch = append(toLaunch, launch{name: subName, p: p, ctx: pctx})
		}
		if !fromSpawn {
			p.nonSpawn = true
		}
	}
	e.smu.Unlock()

	for _, l := range toLaunch {
		go r.runSubagentPoller(l.ctx, conv, e, sc, l.name, l.p)
	}
}

func (r *Router) runSubagentPoller(ctx context.Context, conv string, e *sessionEntry, sc *subagent.Client, subName string, p *subPoller) {
	base := r.subagentPollInterval()
	timer := time.NewTimer(base)
	defer timer.Stop()

	maxAge := r.tickMaxAge
	if maxAge <= 0 {
		maxAge = progressTickMaxAge
	}
	deadline := time.NewTimer(maxAge)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			e.retireSubagentPoller(subName, p)
			return
		case <-timer.C:
		}
		if !r.pollSubagent(ctx, conv, e, sc, subName, p, false, false) {
			p.cancel()
			return
		}
		shift := min(int(p.misses.Load()), 4)
		timer.Reset(base << shift)
	}
}

// finishSubagentsOnResults matches arriving parent ToolResults against active
// subagent watches. When the last watch for a subagent finishes, its
// background ticker is cancelled and one synchronous final drain runs before
// returning so inner subagent tool notices settle ahead of the parent tool's
// own completion update.
func (r *Router) finishSubagentsOnResults(ctx context.Context, conv string, e *sessionEntry, results []daemon.ToolResult) {
	type donePoller struct {
		name        string
		p           *subPoller
		canCache404 bool
	}
	var finished []donePoller

	e.smu.Lock()
	if len(e.subWatches) == 0 {
		e.smu.Unlock()
		return
	}
	noteMatch := func(subName string, w subWatch, failed bool) {
		if p := e.subPollers[subName]; p != nil && !w.fromSpawn && !failed {
			p.sawSuccess = true
		}
		if len(e.subWatches[subName]) > 0 {
			return
		}
		delete(e.subWatches, subName)
		if p := e.subPollers[subName]; p != nil {
			delete(e.subPollers, subName)
			finished = append(finished, donePoller{
				name:        subName,
				p:           p,
				canCache404: p.nonSpawn && p.sawSuccess,
			})
		}
	}
	// Exact call IDs first across the frame, then oldest matching parent tool
	// name for results that carry no ID — mirroring applyToolResults.
	for _, res := range results {
		if res.ID == "" {
			continue
		}
		for subName, list := range e.subWatches {
			if idx := slices.IndexFunc(list, func(w subWatch) bool { return w.id == res.ID }); idx >= 0 {
				w := list[idx]
				e.subWatches[subName] = slices.Delete(list, idx, idx+1)
				noteMatch(subName, w, res.Failed)
				break
			}
		}
	}
	for _, res := range results {
		if res.ID != "" {
			continue
		}
		for subName, list := range e.subWatches {
			if idx := slices.IndexFunc(list, func(w subWatch) bool { return w.parentTool == res.Name }); idx >= 0 {
				w := list[idx]
				e.subWatches[subName] = slices.Delete(list, idx, idx+1)
				noteMatch(subName, w, res.Failed)
				break
			}
		}
	}
	e.smu.Unlock()

	if len(finished) == 0 {
		return
	}
	sc := r.subagentsOf(e)
	for _, f := range finished {
		f.p.cancel()
		if sc != nil && ctx.Err() == nil {
			r.pollSubagent(ctx, conv, e, sc, f.name, f.p, true, f.canCache404)
		}
		f.p.done.Store(true)
		e.clearSubagentActivity(f.name)
	}
}

// pollSubagent runs one cursor-paginated poll for subName under p.pollMu and
// applies any new inner tool calls and results. It returns whether the
// background ticker should keep polling.
func (r *Router) pollSubagent(ctx context.Context, conv string, e *sessionEntry, sc *subagent.Client, subName string, p *subPoller, finalDrain, canCache404 bool) bool {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()

	if p.done.Load() || ctx.Err() != nil || e.turnSeq.Load() != p.turn {
		return false
	}

	e.smu.Lock()
	if e.noSubagents || (p.nonSpawn && e.notSubagent[subName]) {
		e.smu.Unlock()
		return false
	}
	if !finalDrain && e.subPollers[subName] != p {
		e.smu.Unlock()
		return false
	}
	since := max(e.subSeq[subName], e.subFloor)
	e.smu.Unlock()

	maxPages := maxSubagentPagesPerTick
	if finalDrain {
		maxPages = maxSubagentPagesOnDrain
	}

	var frames []subagent.Frame
	nextSince := since
	for range maxPages {
		page, err := sc.Events(ctx, e.sess, e.owner, subName, nextSince)
		if err != nil {
			if ctx.Err() != nil || p.done.Load() {
				return false
			}
			if len(frames) > 0 || nextSince > since {
				r.logf.Warnf("subagent %s (%s): poll: %v", conv, subName, err)
				break
			}
			switch {
			case errors.Is(err, subagent.ErrNotSupported):
				e.markNoSubagents()
				return false
			case errors.Is(err, subagent.ErrNotSubagent):
				// Under --no-background-agents a sync subagent (or agentic_*
				// subtask) has no declared catalog entry, so it answers 404
				// until its approval gate clears and its runner commits the
				// child's opening prompt row — and writes none at all if the
				// call is denied or refused pre-launch. Back off live 404
				// polls without poisoning the cache, and cache 404 only on the
				// completion drain of a non-spawn_agent parent tool that ran
				// to a non-failed result without creating a branch.
				if !finalDrain {
					p.misses.Add(1)
					return true
				}
				if canCache404 {
					e.markNotSubagent(subName)
				}
				return false
			case !daemon.IsTransient(err):
				r.logf.Warnf("subagent %s (%s): giving up on turn log: %v", conv, subName, err)
				if !finalDrain {
					e.retireSubagentPoller(subName, p)
				}
				return false
			default:
				r.logf.Warnf("subagent %s (%s): poll: %v", conv, subName, err)
				return !finalDrain
			}
		}
		p.misses.Store(0)
		frames = append(frames, page.Events...)
		advanced := page.NextSince > nextSince
		if advanced {
			nextSince = page.NextSince
		}
		if !page.Truncated || !advanced {
			break
		}
	}

	if p.done.Load() || ctx.Err() != nil || e.turnSeq.Load() != p.turn {
		return false
	}

	var totalCalls int
	var lastCalls []daemon.ToolCall
	e.smu.Lock()
	if e.subSeq == nil {
		e.subSeq = make(map[string]int64)
	}
	if e.subCallTotal == nil {
		e.subCallTotal = make(map[string]int)
	}
	if nextSince > e.subSeq[subName] {
		e.subSeq[subName] = nextSince
	}
	totalCalls = e.subCallTotal[subName]
	for _, f := range frames {
		if len(f.Calls) > 0 {
			totalCalls += len(f.Calls)
			lastCalls = f.Calls
		}
	}
	e.subCallTotal[subName] = totalCalls
	e.smu.Unlock()

	if len(frames) == 0 {
		return !finalDrain
	}
	e.touch()

	mode := r.progressFor(e.channel)
	if r.carded(e, mode) {
		// Card mode, either mode: the subagent's calls go on the turn's card,
		// under the call that delegated to it, and nothing is posted for
		// them. Without a card to put them on — a turn with no placeholder —
		// the mode does below what it does for an adapter that draws none.
		if taken, changed := e.noteCardSubagent(subName, frames, mode == ProgressStream); taken {
			if !finalDrain && len(lastCalls) > 0 {
				e.noteSubagentActivity(subName, toolNames(lastCalls), totalCalls) // for the text beside the card
			}
			if changed {
				r.markDirty() // the snapshot keeps the card of a turn in flight
				r.requestRedraw(p.base, e, conv)
			}
			return !finalDrain
		}
	}
	switch mode {
	case ProgressStatus:
		if !finalDrain && len(lastCalls) > 0 {
			if e.noteSubagentActivity(subName, toolNames(lastCalls), totalCalls) {
				if _, err := r.redrawProgress(ctx, e, conv, ProgressStatus); err != nil {
					r.logf.Warnf("subagent %s (%s): status activity: %v", conv, subName, err)
				}
			}
		}
	case ProgressStream:
		r.applySubagentFrames(ctx, conv, e, subName, p.turn, frames)
	}
	return !finalDrain
}

// applySubagentFrames posts and updates stream-mode notices for one batch of
// subagent frames. Calls and results that arrive in the same poll batch are
// coalesced before posting, so a sub-second inner tool (read_file, edit_file)
// posts once in its settled "✔ Ran" state rather than spending a Send plus an
// immediate Update.
func (r *Router) applySubagentFrames(ctx context.Context, conv string, e *sessionEntry, subName string, turn int64, frames []subagent.Frame) {
	now := time.Now()
	var staged []*activityNote
	var edits []toolEdit

	e.amu.Lock()
	if e.turnSeq.Load() != turn {
		e.amu.Unlock()
		return
	}
	touchEdit := func(n *activityNote) {
		if n.ref.ID == "" {
			return // staged: will be sent after the lock is released
		}
		if slices.ContainsFunc(edits, func(ed toolEdit) bool { return ed.note == n }) {
			return
		}
		edits = append(edits, toolEdit{note: n, ref: n.ref})
	}
	for _, f := range frames {
		if len(f.Calls) > 0 {
			staged = append(staged, &activityNote{
				subagent: subName,
				calls:    slices.Clone(f.Calls),
				res:      make([]*daemon.ToolResult, len(f.Calls)),
				detail:   true,
				seen:     now,
				eventAt:  f.At,
				took:     make([]time.Duration, len(f.Calls)),
			})
		}
		if len(f.Results) > 0 {
			for _, res := range f.Results {
				if res.ID != "" {
					if n, ok := resolveSubagentToolByID(e.notices, staged, subName, res, f.At); ok {
						touchEdit(n)
					}
				}
			}
			for _, res := range f.Results {
				if res.ID == "" {
					if n, ok := resolveSubagentToolByName(e.notices, staged, subName, res, f.At); ok {
						touchEdit(n)
					}
				}
			}
		}
	}
	for i := range edits {
		edits[i].text = edits[i].note.render()
	}
	stagedTexts := make([]string, len(staged))
	for i, n := range staged {
		stagedTexts[i] = n.render()
	}
	e.amu.Unlock()

	for _, ed := range edits {
		if err := r.out.Update(ctx, ed.ref, chat.Reply{Conversation: conv, Text: ed.text, Kind: chat.KindActivity}); err != nil {
			r.logf.Warnf("subagent %s (%s): tool result: %v", conv, subName, err)
		}
	}
	for i, n := range staged {
		ref, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: stagedTexts[i], Kind: chat.KindActivity})
		r.metrics.recordReply(err)
		if err != nil {
			r.logf.Warnf("subagent %s (%s): activity: %v", conv, subName, err)
			continue
		}
		e.amu.Lock()
		stillPending := !n.allDone() && e.turnSeq.Load() == turn
		if stillPending {
			n.ref = ref
			e.notices = capNotices(append(e.notices, n))
		}
		e.amu.Unlock()
	}
}

func resolveSubagentToolByID(existing, staged []*activityNote, subName string, r daemon.ToolResult, resAt time.Time) (*activityNote, bool) {
	for i := len(staged) - 1; i >= 0; i-- {
		if matchNoteByID(staged[i], subName, r, resAt) {
			return staged[i], true
		}
	}
	for i := len(existing) - 1; i >= 0; i-- {
		if matchNoteByID(existing[i], subName, r, resAt) {
			return existing[i], true
		}
	}
	return nil, false
}

func matchNoteByID(n *activityNote, subName string, r daemon.ToolResult, resAt time.Time) bool {
	if n.subagent != subName {
		return false
	}
	for j, c := range n.calls {
		if c.ID == r.ID && n.res[j] == nil {
			res := r
			n.res[j] = &res
			n.took[j] = subagentTook(n.eventAt, resAt, n.seen)
			return true
		}
	}
	return false
}

func resolveSubagentToolByName(existing, staged []*activityNote, subName string, r daemon.ToolResult, resAt time.Time) (*activityNote, bool) {
	for _, n := range existing {
		if matchNoteByName(n, subName, r, resAt) {
			return n, true
		}
	}
	for _, n := range staged {
		if matchNoteByName(n, subName, r, resAt) {
			return n, true
		}
	}
	return nil, false
}

func matchNoteByName(n *activityNote, subName string, r daemon.ToolResult, resAt time.Time) bool {
	if n.subagent != subName {
		return false
	}
	at := n.pending(r.Name)
	if at == -1 {
		return false
	}
	res := r
	n.res[at] = &res
	n.took[at] = subagentTook(n.eventAt, resAt, n.seen)
	return true
}

func subagentTook(callAt, resAt, seen time.Time) time.Duration {
	if !callAt.IsZero() && !resAt.IsZero() {
		if d := resAt.Sub(callAt); d >= 0 {
			return d
		}
	}
	return time.Since(seen)
}

// allDone reports whether every call on the notice has received a result.
func (n *activityNote) allDone() bool {
	for _, r := range n.res {
		if r == nil {
			return false
		}
	}
	return true
}

// capNotices bounds addressable notices to noticeMemory, evicting already-settled
// notices first so a chatty subagent's finished inner calls never push the
// parent's still-running delegation notice out of reach.
func capNotices(notices []*activityNote) []*activityNote {
	if len(notices) <= noticeMemory {
		return notices
	}
	excess := len(notices) - noticeMemory
	kept := make([]*activityNote, 0, len(notices))
	for _, n := range notices {
		if excess > 0 && n.allDone() {
			excess--
			continue
		}
		kept = append(kept, n)
	}
	if extra := len(kept) - noticeMemory; extra > 0 {
		kept = append([]*activityNote(nil), kept[extra:]...)
	}
	return kept
}

// noteSubagentActivity records the latest inner tool(s) a subagent started for
// status mode, reporting whether there is a progress message to show them in.
// The caller redraws (redrawProgress), as noteActivity's does: recording and
// rendering are apart so every render reads the state as it is when its edit
// goes out.
func (e *sessionEntry) noteSubagentActivity(subagent string, tools []string, call int) bool {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.progressMsg.ID == "" || e.turnStart.IsZero() {
		return false
	}
	e.subAgent, e.subTools, e.subCall = subagent, tools, call
	return true
}

// noteCardSubagent records a batch of a subagent's frames on the turn's card.
// taken is false when the card has nowhere to put them — no progress message,
// or no call on it that delegated to this subagent — and the caller does what
// it does without a card. changed is whether the card now draws differently.
func (e *sessionEntry) noteCardSubagent(sub string, frames []subagent.Frame, detail bool) (taken, changed bool) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.progressMsg.ID == "" || e.turnStart.IsZero() || !e.card.follows(sub) {
		return false, false
	}
	return true, e.card.addSub(sub, frames, detail, time.Now())
}

// clearSubagentActivity drops the subagent inner-tool trail from the status
// placeholder once that subagent's outer call has returned.
func (e *sessionEntry) clearSubagentActivity(subagent string) {
	e.pmu.Lock()
	if e.subAgent == subagent {
		e.subAgent, e.subTools, e.subCall = "", nil, 0
	}
	e.pmu.Unlock()
}

// subTickText renders the progress placeholder line when a subagent's inner
// tool activity is active, falling back to tickText when none is:
//
//	⏳ Working… 25s · running `propose-change` › `vcs_clone` (call 2)
func subTickText(elapsed time.Duration, tools []string, step int, subAgent string, subTools []string, subCall int) string {
	if subAgent != "" && len(subTools) > 0 {
		text := workingText + " " + formatElapsed(elapsed) +
			" · running `" + inlineCode(subAgent) + "` › " + quotedTools(subTools)
		if subCall > 0 {
			text += fmt.Sprintf(" (call %d)", subCall)
		}
		return text
	}
	return tickText(elapsed, tools, step)
}

// subagentActivityText renders a standalone stream-mode activity notice
// attributed to subagent:
//
//	▸ Running **propose-change** › **vcs_clone**
//	```
//	https://github.com/org/repo
//	```
//
//	✔ Ran 2 tools (**propose-change**)
//
//	✔ **read_file**
//	```
//	deploy/billing.yaml
//	```
//
//	✔ **grep**
//	```
//	memory
//	```
func subagentActivityText(subagent string, calls []daemon.ToolCall, res []*daemon.ToolResult, took []time.Duration) string {
	groups := groupCalls(calls, res, took)
	if len(groups) == 1 {
		return subToolLine(subagent, groups[0], true)
	}
	blocks := make([]string, 0, len(groups)+1)
	head := activityHeader(groups)
	if subagent != "" {
		head += " (**" + boldName(subagent) + "**)"
	}
	blocks = append(blocks, head)
	for _, g := range groups {
		blocks = append(blocks, toolLine(g, true, false))
	}
	return strings.Join(blocks, "\n\n")
}

func subToolLine(subagent string, g toolGroup, verb bool) string {
	line := toolIcon(g.res)
	if verb {
		if g.res == nil {
			line += " Running"
		} else {
			line += " Ran"
		}
	}
	if subagent != "" {
		line += " **" + boldName(subagent) + "** ›"
	}
	line += " **" + boldName(g.call.Name) + "**"
	if g.n > 1 {
		line += " ×" + strconv.Itoa(g.n)
	}
	if g.res != nil && g.res.Failed && g.res.Detail != "" {
		line += " (" + g.res.Detail + ")"
	}
	if g.call.Arg != "" {
		line += "\n```\n" + fencedCode(g.call.Arg) + "\n```"
	}
	if g.res != nil && g.n == 1 && g.took >= minShownTook {
		line += "\n⏱ " + formatTook(g.took)
	}
	return line
}
