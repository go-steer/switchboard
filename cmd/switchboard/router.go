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
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-steer/switchboard/internal/logging"
	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// SSE relay reconnect backoff bounds. A dropped stream is reconnected after
// minBackoff, doubling up to maxBackoff while it keeps failing and resetting to
// minBackoff once a connection makes progress again.
const (
	reconnectMinBackoff = time.Second
	reconnectMaxBackoff = 30 * time.Second
)

// streamLostGrace is how long the relay will fail to reconnect, with a turn
// still waiting on the daemon, before it says so in the thread. Reconnection
// itself never gives up; this only bounds how long the conversation is left
// guessing.
//
// Long enough that a rolling daemon restart, or a blip that outlasts a couple
// of backoff steps, resolves without a notice nobody needed — and short enough
// that a reader is not still watching a placeholder minutes after the agent
// stopped existing.
const streamLostGrace = 90 * time.Second

// sender is the egress half of a chat.Adapter the router needs to relay
// replies and manage long-turn progress messages. Narrowed to the methods
// the router uses so it stays testable with a fake.
type sender interface {
	Send(context.Context, chat.Reply) (chat.MessageRef, error)
	Update(context.Context, chat.MessageRef, chat.Reply) error
	Delete(context.Context, chat.MessageRef) error
}

// ProgressMode selects how the router signals liveness while an agent turn
// runs:
//   - off: nothing until the turn completes (the reply lands as before).
//   - indicator: post a lightweight placeholder on wake, deleted when the
//     turn's reply arrives.
//   - status: post one message on wake and edit it in place to name the tool
//     the agent is currently running; the reply replaces it when ready.
//   - stream: relay each completed model turn and post a standalone notice for
//     every tool the agent runs — the most transparent, and noisiest.
type ProgressMode string

const (
	ProgressOff       ProgressMode = "off"
	ProgressIndicator ProgressMode = "indicator"
	ProgressStatus    ProgressMode = "status"
	ProgressStream    ProgressMode = "stream"
)

// progressModes is every accepted mode, in the order they are offered. One
// list, read by everything that has to agree on the set: parseProgressMode,
// the `progress` command's own text, and the values reported to an adapter
// through chat.CommandChoices. They used to be four independent literals, so
// a fifth mode could reach the flag parser while going missing from the help
// text and from what a card offers.
var progressModes = []ProgressMode{ProgressOff, ProgressIndicator, ProgressStatus, ProgressStream}

// clocked reports whether a mode keeps a "⏳ Working…" placeholder with a
// running clock for the turn. Every mode but off: stream used not to, which
// left the one mode showing what the agent runs as the one that never said
// how long it had been at it (reported from the live Chat rig). In stream the
// clock is the indicator's alone; the tools are on their own notices.
func (m ProgressMode) clocked() bool { return m != ProgressOff }

// progressModeNames is progressModes as plain strings — the form both the
// capability and the message text want.
func progressModeNames() []string {
	names := make([]string, len(progressModes))
	for i, m := range progressModes {
		names[i] = string(m)
	}
	return names
}

// workingText is the initial progress message posted on wake in
// every mode but off while a turn is in flight. It is always a
// transient message — retired when the reply is delivered — never the answer.
const workingText = "⏳ Working…"

const (
	// progressTickInterval is how often a turn in flight re-renders its
	// progress message with the elapsed clock. Coarse deliberately: every tick
	// is an edit against the platform's API, and a turn running for an hour
	// would spend thousands of them at one-second resolution to convey nothing
	// a fifteen-second clock does not.
	progressTickInterval = 15 * time.Second

	// progressTickMaxBackoff bounds the retry interval after a failed edit, so
	// a rate-limited conversation stops answering a rate limit with more edits
	// but still recovers without waiting out the whole turn.
	progressTickMaxBackoff = 4 * time.Minute

	// progressTickMaxAge gives up on a turn that has neither answered nor
	// reported itself complete: the clock stops, the message freezes where it
	// got to, and the turn stops counting as in flight. turn-complete is the
	// real boundary; this is the backstop for when it never arrives, so a
	// daemon that loses a turn leaves a frozen message rather than a goroutine
	// editing a channel until the process is restarted. Generous, because
	// switchboard cannot tell a genuinely long turn from a lost one and giving
	// up on a live one is the worse mistake.
	progressTickMaxAge = time.Hour
)

// quotedTools renders a tool list as backticked names: "`bash`, `read`".
func quotedTools(tools []string) string {
	quoted := make([]string, len(tools))
	for i, t := range tools {
		quoted[i] = "`" + inlineCode(t) + "`"
	}
	return strings.Join(quoted, ", ")
}

// toolNames pulls the names out of a call list, for the renderers that show
// nothing else.
func toolNames(calls []daemon.ToolCall) []string {
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.Name
	}
	return names
}

// toolGroup is a run of adjacent calls that render as one line: same tool, same
// argument summary, same verdict so far. Fifteen `bash` calls in a turn is the
// normal case (#36), and three of them arriving in one frame used to render as
// "`bash`, `bash`, `bash`", which reads as a bug rather than as information.
type toolGroup struct {
	call daemon.ToolCall
	res  *daemon.ToolResult
	n    int
	// took is how long the call ran, for a group of one that has finished;
	// zero when unknown. A collapsed group shows none, since its calls each
	// took their own time and no one number is the group's.
	took time.Duration
}

// groupCalls collapses adjacent identical calls, where identical means the
// reader could not tell them apart anyway: same name, same summary, same
// verdict. Calls that differ in any of those stay separate lines — the point of
// the argument summary is that three concurrent shells become three legible
// lines rather than one count.
func groupCalls(calls []daemon.ToolCall, res []*daemon.ToolResult, took []time.Duration) []toolGroup {
	var groups []toolGroup
	for i, c := range calls {
		var r *daemon.ToolResult
		if i < len(res) {
			r = res[i]
		}
		var d time.Duration
		if i < len(took) {
			d = took[i]
		}
		if n := len(groups); n > 0 {
			if prev := groups[n-1]; prev.call.Name == c.Name && prev.call.Arg == c.Arg && sameVerdict(prev.res, r) {
				groups[n-1].n++
				groups[n-1].took = 0
				continue
			}
		}
		groups = append(groups, toolGroup{call: c, res: r, n: 1, took: d})
	}
	return groups
}

// minShownTook is the shortest call duration worth printing. Below it the
// number is noise to a reader — the call was instant, as far as anyone
// watching is concerned — and it is mostly the gateway's own latency anyway.
const minShownTook = 100 * time.Millisecond

// formatTook renders a call's duration: tenths of a second under ten seconds,
// where they still mean something, and the turn clock's own form above.
func formatTook(d time.Duration) string {
	// Rounded first, so 9.97s reads "10s" rather than a "10.0s" that the
	// next tenth of a second would turn into "10s" anyway.
	d = d.Round(100 * time.Millisecond)
	if d < 10*time.Second {
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	}
	return formatElapsed(d)
}

// sameVerdict reports whether two results would render the same, treating "no
// result yet" as its own state.
func sameVerdict(a, b *daemon.ToolResult) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return a.Failed == b.Failed && a.Detail == b.Detail
}

// The marks a tool notice leads with: running, succeeded, failed. Heavy
// monochrome glyphs rather than emoji (chosen on the live rig: ✅/❌ read as
// clutter in a busy thread). ✔ and ✖ are emoji that default to text style,
// and a client may still draw them in colour, so each carries the text-style
// variation selector (U+FE0E) to ask for the glyph. The Chat adapter reads
// them back to pick an icon (activityIcon), so it shares these spellings.
const (
	markRunning = "▸"
	markOK      = "✔\uFE0E"
	markFailed  = "✖\uFE0E"
)

// toolIcon is the state of one group at a glance: still running, done, failed.
func toolIcon(r *daemon.ToolResult) string {
	switch {
	case r == nil:
		return markRunning
	case r.Failed:
		return markFailed
	}
	return markOK
}

// toolLine renders one group: icon, tool, how many of it, why it failed, and —
// in stream mode only — what it was called with. verb puts "Running"/"Ran"
// after the icon, for a notice that is one line and has no header to carry it.
//
// In stream mode a call is a small block rather than a line: a header naming
// the tool and its verdict, the argument in a fenced code block, and how long
// the call took on a line of its own (asked for on the live Chat rig — a
// command reads as code in a block, and a duration tacked onto the end of it
// read as part of it). The tool name is bold rather than a code span: a code
// span renders monospace nowhere in a Chat card's markdown (measured), and a
// fence is what the argument needs.
//
// The argument goes through fencedCode, as an approval prompt's command does:
// it is agent-supplied, and a ``` in it would end the block early.
func toolLine(g toolGroup, detail, verb bool) string {
	line := toolIcon(g.res)
	if verb {
		if g.res == nil {
			line += " Running"
		} else {
			line += " Ran"
		}
	}
	line += " **" + boldName(g.call.Name) + "**"
	if g.n > 1 {
		line += " ×" + strconv.Itoa(g.n)
	}
	if g.res != nil && g.res.Failed && g.res.Detail != "" {
		line += " (" + g.res.Detail + ")"
	}
	if detail && g.call.Arg != "" {
		line += "\n```\n" + fencedCode(g.call.Arg) + "\n```"
	}
	if g.res != nil && g.n == 1 && g.took >= minShownTook {
		line += "\n⏱ " + formatTook(g.took)
	}
	return line
}

// boldName makes an agent-supplied tool name safe inside **…**. Unlike a code
// span, bold shields nothing: a name shaped like [text](url) or <url|text>
// would render as a link in the gateway's own notice (caught in review). So
// it keeps only what tool names are made of — letters, digits and ._:/- — and
// an underscore only between two of those, since one at either end reads as
// emphasis. Anything else is dropped; a name with nothing left reads "?".
func boldName(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(".:/-", r):
			b.WriteRune(r)
		case r == '_' && i > 0 && i < len(rs)-1 && nameRune(rs[i-1]) && nameRune(rs[i+1]):
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "?"
	}
	return b.String()
}

// nameRune reports whether r may flank an underscore in a bold tool name. An
// underscore counts, so mcp__github__list_prs keeps its double underscores.
func nameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// activityText renders a standalone tool-activity notice — stream mode, and
// status mode with no message left to edit. res is parallel to calls and holds
// each call's result once it has landed, so the same function renders the
// notice when it is posted and again as each result ticks a line off:
//
//	✔ Ran **bash**                                    (stream, one call: a block)
//	```
//	kubectl get pods -A
//	```
//	⏱ 2.3s
//
//	✖ Ran 2 tools (1 failed)                          (stream, a parallel frame:
//	                                                  header, then one block per
//	✔ **bash**                                        call, blank-line separated)
//	```
//	kubectl get pods -A
//	```
//
//	✖ **bash** (exit 2)
//	```
//	kubectl get ns --context nope
//	```
//
//	▸ Running `bash` ×3                               (status: names, no arguments)
//
// detail is the mode gate. Only stream carries argument summaries: status edits
// one message in place and wants a short line, and a reader who chose indicator
// or status did not ask to see what the agent is running things with.
func activityText(calls []daemon.ToolCall, res []*daemon.ToolResult, detail bool) string {
	return timedActivityText(calls, res, nil, detail)
}

// timedActivityText is activityText with each finished call's duration, which
// stream mode prints on its own line under the call ("⏱ 2.3s"). took is parallel to calls.
func timedActivityText(calls []daemon.ToolCall, res []*daemon.ToolResult, took []time.Duration, detail bool) string {
	if !detail {
		// The terse shape, unchanged but for the count: names, comma-joined,
		// with runs of the same name collapsed rather than repeated.
		var parts []string
		for _, g := range groupCalls(stripArgs(calls), nil, nil) {
			part := "`" + inlineCode(g.call.Name) + "`"
			if g.n > 1 {
				part += " ×" + strconv.Itoa(g.n)
			}
			parts = append(parts, part)
		}
		return markRunning + " Running " + strings.Join(parts, ", ")
	}
	groups := groupCalls(calls, res, took)
	if len(groups) == 1 {
		return toolLine(groups[0], true, true)
	}
	blocks := make([]string, 0, len(groups)+1)
	blocks = append(blocks, activityHeader(groups))
	for _, g := range groups {
		blocks = append(blocks, toolLine(g, true, false))
	}
	return strings.Join(blocks, "\n\n")
}

// activityHeader summarises a multi-call notice in its first line, so a reader
// scrolling past sees the state of the frame without reading the blocks.
func activityHeader(groups []toolGroup) string {
	total, done, failed := 0, 0, 0
	for _, g := range groups {
		total += g.n
		if g.res == nil {
			continue
		}
		done += g.n
		if g.res.Failed {
			failed += g.n
		}
	}
	head := markRunning + " Running " + strconv.Itoa(total) + " tools"
	if done == total {
		head = markOK + " Ran " + strconv.Itoa(total) + " tools"
		if failed > 0 {
			head = markFailed + " Ran " + strconv.Itoa(total) + " tools (" + strconv.Itoa(failed) + " failed)"
		}
	}
	return head
}

// stripArgs drops the argument summaries, for the renderer that must not show
// them. Its live effect is grouping — groupCalls compares Arg, so without this
// two `bash` calls with different commands would not collapse to `bash` ×2 in
// a mode that never shows the difference. Not showing them is structural: the
// terse branch reads only Name. This is the belt to that brace, so a future
// edit that does read Arg there is still safe.
func stripArgs(calls []daemon.ToolCall) []daemon.ToolCall {
	bare := make([]daemon.ToolCall, len(calls))
	for i, c := range calls {
		bare[i] = daemon.ToolCall{ID: c.ID, Name: c.Name}
	}
	return bare
}

// tickText renders the progress message of a turn in flight: the working
// marker, how long it has been running, and — status mode only — the tool it
// is on and how many steps it has taken.
//
//	⏳ Working… 45s
//	⏳ Working… 2m30s · running `bash` (step 7)
//
// The clock is the whole point. A turn that makes no tool calls posts
// "⏳ Working…" once and then looks identical whether it is thinking hard or
// has died, which is the only signal a reader has to go on.
func tickText(elapsed time.Duration, tools []string, step int) string {
	text := workingText + " " + formatElapsed(elapsed)
	if len(tools) > 0 {
		text += " · running " + quotedTools(tools)
		if step > 0 {
			text += fmt.Sprintf(" (step %d)", step)
		}
	}
	return text
}

// formatElapsed renders a turn's age at second resolution: "45s", "2m30s",
// "1h07m". Compact because it sits inline in a chat line, and never fractional
// because the clock is read for "is this moving", not for measurement.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Round(time.Second).Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
	}
}

// Router maps chat conversations onto core-agent sessions and shuttles
// turns across the daemon contract. It is the chat.Handler an adapter
// dispatches inbound messages to. One session per conversation key; each
// session gets one long-lived SSE subscription whose agent output is
// relayed back through the adapter — subscribing once (rather than
// per-turn) is what keeps the daemon from replaying prior turns on every
// message.
type Router struct {
	// reg is the routing snapshot — the agents every conversation's daemon is
	// found in (#140), and the agent settings the admin API gave channels
	// (#156) — swapped whole on each change so a turn never sees half of one.
	// Read through agents() and settingsFor.
	reg atomic.Pointer[registry]
	// regAdmin changes reg at runtime; nil when nothing may (a test, an
	// outbound-only run).
	regAdmin *registryAdmin
	// gatewayID goes into ax task names, so gateways sharing a cluster never
	// share a conversation's task (setGateway).
	gatewayID string
	// owners is who each personal agent's thread belongs to (#172), under
	// mu and persisted; ownerNoticed spaces the notice telling others so.
	owners       map[string]threadOwner
	ownerNoticed map[string]time.Time
	// axDeleting gates each ax task being deleted (#173), under mu.
	axDeleting map[string]chan struct{}
	out        sender
	metrics    *metrics
	logf       logging.Logf

	// parkWait overrides parkRespondTimeout; zero means the constant. A test
	// seam.
	parkWait time.Duration

	// audit is the ingress record (#89): one line per turn and per press.
	// Nil records nothing, which is the default.
	audit *auditLog

	// defaults are the channel-scopable settings as they apply to a channel
	// with nothing said about it, and byChannel is what a config file said
	// about the ones it named (#71). Both are written once at startup — by
	// NewRouter and the setters below — before the adapter is dispatching, and
	// read through settingsFor from then on. Neither is guarded: nothing
	// mutates them after startup, and the one setting that does change at
	// runtime (the progress mode, via a chat command) lives in overrides,
	// which is.
	defaults  channelSettings
	byChannel map[string]channelSettings

	// Reconnect backoff bounds for the SSE relay; defaulted in NewRouter and
	// overridable in tests so a reconnect can be exercised without real waits.
	minBackoff, maxBackoff time.Duration

	// streamGrace is how long the relay may be disconnected, with a turn in
	// flight, before the thread is told contact was lost. Defaulted in
	// NewRouter and shortened in tests.
	streamGrace time.Duration

	// tickInterval is how often a turn in flight re-renders its progress
	// message with the elapsed clock. Defaulted in NewRouter and overridable in
	// tests so a tick can be observed without a real wait; zero or negative
	// disables the ticker and leaves the placeholder static. tickMaxAge is the
	// backstop that stops a clock the daemon never called time on; both are
	// defaulted in NewRouter and shortened in tests.
	tickInterval, tickMaxAge time.Duration

	// subPollInterval is how often an active subagent branch is polled for
	// inner tool calls (#193). Zero or less means defaultSubagentPollInterval.
	subPollInterval time.Duration

	// cardRedraw is the least time between two edits of a card caused by
	// calls, results and subagent frames (requestRedraw), and cardMaxLines how
	// many lines a card holds before it is frozen and continued in a new
	// message. Zero or less means defaultCardRedraw and defaultCardMaxLines;
	// tests shorten both.
	cardRedraw   time.Duration
	cardMaxLines int

	// cardRetry is the first wait before a card's finalising edit is tried
	// again (retryFinish); zero or less means defaultCardRetry, and tests
	// shorten it.
	cardRetry time.Duration

	// fmu guards owed and finishing, and is a leaf: nothing is called with it
	// held.
	//
	// owed are the cards still owed the edit that finalises them, by message
	// (owedCard, in state.go): one the last process left in
	// flight, or one whose finalising edit has failed so far. Each is in the
	// snapshot, on a list of its own rather than on its conversation's
	// record, until that edit lands or the platform refuses it for good — so
	// a card is owed its last edit whatever becomes of the session, and
	// owing one says nothing about whether a turn is running there now.
	//
	// finishing counts the finalising edits being retried in the background:
	// what a shutdown waits for before it lets the process go.
	fmu       sync.Mutex
	owed      map[chat.MessageRef]*owedCard
	finishing int

	// footDelay is how long a turn-complete waits for an answer behind it; see
	// scheduleFoot. Zero or less means defaultFootDelay.
	footDelay time.Duration

	// reanchorDelay is how long narration waits before moving the progress
	// placeholder below itself. A daemon that sends the answer ahead of its
	// turn-complete (core-agent 2.10) makes every answer look like narration
	// for a moment; re-anchoring at once posted a fresh placeholder and had
	// the boundary delete it a second later — a message that appears and
	// vanishes on every turn (seen live). Waiting lets the boundary land
	// first, after which the re-anchor finds nothing to move. Zero re-anchors
	// at once.
	reanchorDelay time.Duration

	mu       sync.Mutex
	sessions map[string]*sessionEntry

	// dormant holds the conversations that have a session but no live entry:
	// restored from the state file and not yet re-attached (#86). A dormant
	// record is the conversation's session as surely as a live entry is, so
	// session() revives it before anything would open a new one, and a bind
	// treats it as taken. Guarded by mu.
	dormant map[string]sessionRecord

	// store persists the routing table, and dirty schedules a write; both nil
	// when --state-dir is unset, which keeps everything in memory as before.
	store  stateStore
	dirty  chan struct{}
	saveMu sync.Mutex // one snapshot-and-write at a time; see trySaveState

	// idleTTL releases a conversation's streams after this long without
	// traffic (#87); zero never does. Set once at startup.
	idleTTL time.Duration

	// The conversations whose session switchboard did not create, recorded by
	// the outbound ingress and consulted by session() before it creates one
	// (#38). Guarded by mu, like sessions, because every rule about a binding
	// is a rule about the session the conversation already has: see binding.go.
	//
	// bindings is the map itself; boundTo is its inverse, which is what enforces
	// one conversation per session; bindOrder is insertion order, for eviction;
	// reserving holds the sessions a bind is in flight for, which is what makes
	// that inverse true across the platform call in the middle of one.
	bindings  map[string]binding
	boundTo   map[string]string
	bindOrder []string
	reserving map[string]string

	// outbound holds the unbound outbound-ingress threads whose first inbound
	// reply has not yet reached an agent session (see outbound.go). Guarded by
	// mu, like sessions and bindings, so session() and NoteOutbound agree on
	// whether a thread already has a session.
	outbound      map[string]*outboundThread
	outboundOrder []string

	// omu guards overrides, the per-channel progress-mode overrides set at
	// runtime via chat commands (HandleCommand). This is the narrowest of the
	// three layers settingsFor resolves and the only one reachable at runtime;
	// a channel absent from the map falls back to its byChannel block, or to
	// defaults.
	omu       sync.Mutex
	overrides map[string]ProgressMode

	// The approval client that relays a daemon's permission prompts and sends
	// back what someone decided is per agent (agent.approvals, #140): it is a
	// connection to one daemon. None anywhere leaves the feature off for every
	// channel, whatever a config file says — see setApprovals. Whether a given
	// channel uses it is channelSettings.approvals: the decision to put prompts
	// in front of a particular room can be per-channel, a connection cannot.
}

// channelSettings is every gateway setting that may differ between channels,
// resolved for one of them. It exists so that adding the next such setting is
// a field here rather than a third bespoke lookup: before #71 the progress mode
// had progressFor and everything else was read straight off Router, which is
// exactly the asymmetry that made "the SRE channel approves prod, a scratch
// channel approves nothing" impossible to express.
type channelSettings struct {
	// progress is the long-turn feedback mode.
	progress ProgressMode

	// defaultAgent is the agent a new conversation here goes to; empty means
	// the registry's global default. agents is the allow list; empty allows
	// every agent (#140).
	defaultAgent string
	agents       []string

	// approvals is whether permission prompts are put into this channel at
	// all. False leaves them where they were, waiting on a console.
	approvals bool

	// showUsage turns on the per-turn tokens/cost footer. Off unless the
	// operator asked for it: what a turn cost is spend data, and a shared
	// channel is the wrong place to disclose it by default.
	showUsage bool

	// approvers is who may answer one of this channel's prompts. The zero
	// value lets anyone who can post in the conversation answer, which is the
	// shipped default — see approverPolicy for why that is a posture rather
	// than an omission.
	approvers approverPolicy

	// standing narrows approvers further for the one answer whose reach is
	// not bounded by the room it was pressed in: a grant that outlives the
	// session (approval.Decision.Standing). A standing press has to satisfy
	// both policies, so this can only narrow — a standing list naming someone
	// the approver list does not still refuses them. The zero value adds no
	// restriction, which keeps a deployment that never sets it exactly where
	// it was (#85).
	standing approverPolicy
}

// sessionEntry is a conversation's session plus the state to create it
// exactly once under concurrent inbound turns. ready is closed when
// creation finishes (successfully or not); waiters block on it.
type sessionEntry struct {
	ready chan struct{}
	sess  daemon.Session
	err   error

	// emu orders edits to the progress message. A tick reads the message
	// under pmu and edits it after letting go, so without this a tick in
	// flight could land after the edit that finalises the placeholder and
	// put the clock back (caught in review): every editor holds emu across
	// its edit and re-checks that the message is still the current one.
	emu sync.Mutex

	// agent names the registry entry whose daemon holds sess (#140): resolved
	// when the conversation began and fixed for its life. Empty only for an
	// entry revived from a record written before the registry existed, which
	// belongs to the default agent. Set before ready closes.
	agent string
	// task is the ax task the conversation lives in, for an agent of kind
	// "ax" (#170): set before anything is sent, never changed after.
	task string
	// agentBusy is the daemon's own word that a turn is running or parked on
	// a human: set by a working or blocked status-update, cleared only by an
	// idle one (the snapshot every stream opens with included). Releasing a
	// conversation consults it as well as inFlight, which can read idle while
	// the daemon still works — after a reconnect, a long turn's narration is
	// taken as its answer (caught in review, #171).
	agentBusy atomic.Bool
	// pins counts turns being set up on this entry — from finding it to
	// marking the turn in flight, platform posts in between — which a
	// release must not take the entry out from under (caught in review).
	pins atomic.Int32

	// adopted marks a session switchboard did not create but was told about by
	// the outbound ingress (#38). It changes two things: the entry starts at the
	// session's head rather than at zero, and a daemon that has never heard of
	// the session is a broken binding to announce rather than a turn to retry.
	// Set once, at construction, under Router.mu.
	adopted bool

	// owner is the identity the relay subscribes as: the caller who created
	// the session, or empty for an adopted one. It is what a missing-session
	// answer is checked against before the entry is given up on (sessionGone).
	// Set before ready is closed, like sess.
	owner string

	// touched is the last traffic on the conversation, as UnixNano: what the
	// state file records and the revive window is measured against (#86).
	touched atomic.Int64

	// stop ends this entry's relay goroutine. The relay otherwise runs for the
	// life of the process, which is right for a session that keeps answering;
	// an entry that is discarded (a binding the daemon has lost) has to take its
	// subscription with it, or it reconnects forever against a session that will
	// never exist again — and a second relay starts alongside it when the
	// conversation opens its next session.
	//
	// Written before close(ready), like sess, and read only by goroutines that
	// waited on it. That ordering is the whole synchronisation: a concurrent
	// turn released by ready has to see a stop it can call.
	stop context.CancelFunc

	channel string       // platform channel, for resolving the channel's progress mode
	seq     atomic.Int64 // highest agent-event seq seen, fed back as `since` on resume
	relayed atomic.Int64 // highest seq whose answer was posted, for exactly-once delivery across reconnects
	// delivered is relayed once the post has actually been made — relayed
	// moves first, to keep a reconnect from posting twice. delivered is what
	// the state file records, so a post cut off by shutdown is retried by the
	// next process rather than recorded as done (#86).
	delivered atomic.Int64
	// attempted is the highest seq whose answer a post was started for, and
	// is written to the state file before the post goes out (noteAttempt).
	// After a restart, an answer in (delivered, attempted] may or may not have
	// reached the thread, and verifyThrough is that bound: those posts ask the
	// adapter to check first. Everything past it is certainly new.
	attempted     atomic.Int64
	verifyThrough atomic.Int64
	// noticed is the same watermark for tool activity, kept separate from
	// relayed on purpose. They dedupe different things — one guards the answer,
	// the other guards a progress notice — and sharing a counter means a tool
	// result can raise the bar the answer has to clear. Answers and tool frames
	// share a seq space that only ever goes up, so today that costs nothing; the
	// point of relayed is to survive a stream that does something unexpected,
	// and the worst thing this gateway can do is swallow an answer.
	noticed atomic.Int64

	// inFlight is true from the moment a turn is handed to the daemon until
	// something concludes it: the daemon's turn-complete or turn-error, an
	// answer delivered, or the relay giving up on a stream that stayed down.
	// It is what tells a dropped stream apart from a dropped stream *with
	// someone waiting on it*, and it is deliberately independent of the
	// progress message, which off mode never posts.
	//
	// failed is the separate claim on *telling the thread the turn failed*.
	// The two cannot be the same flag: an answer ends the turn, but a turn can
	// answer and still fail afterwards — a cost ceiling is enforced at the turn
	// boundary, so its turn-error lands after the text it paid for — and that
	// failure still has to be reported. Reset per turn.
	inFlight atomic.Bool
	failed   atomic.Bool

	// watchingPerms is the claim on this entry's permission subscription. The
	// capabilities frame that starts it arrives on every reconnect, so without
	// it a session that reconnects for a week accumulates a watcher per
	// connection — each one seeded with the same pending prompts, each one
	// posting the same question again.
	watchingPerms atomic.Bool

	// asked records which permission prompts have already been put in the
	// thread, and what each one said, guarded by qmu.
	//
	// One watcher is not enough to make a question appear once. The prompt
	// stream has no keep-alive, so an intermediary cuts an idle one routinely,
	// and every resubscription is seeded with every prompt still pending — by
	// design, since that is what makes reconnecting cursorless. A permission
	// prompt is exactly the thing that stays pending for minutes while somebody
	// is found, so without this the thread collects one more copy of the same
	// question per reconnect, each with its own live buttons, and only the
	// first press does anything.
	qmu   sync.Mutex
	asked map[string]*askRecord

	// tmu serializes recording how a question ended, the edit included, so that
	// claiming the right to write a record and writing it are one step. Two
	// presses that both claim before either writes would land their edits in
	// whichever order the platform answered in, and the thread would settle on
	// whichever that was rather than on the one that won.
	//
	// Held across the edit, which is a network call. It is a leaf — nothing
	// reached from the adapter comes back here — and it blocks only other
	// presses on the same conversation, which are the things that have to be
	// ordered anyway.
	tmu sync.Mutex

	// described is the claim on logging what the daemon said it is and can do.
	// The capabilities frame arrives on every stream open, so a session that
	// reconnects for a week would otherwise repeat the same one or two lines
	// each time; the interesting moment is the first one.
	described atomic.Bool

	// signalsEnd says the daemon explicitly advertised turn-complete, which is
	// what makes "this text is not the answer yet" a thing switchboard can know
	// rather than guess. Re-read from every capabilities frame, so a reconnect
	// onto a different daemon build is followed rather than remembered.
	//
	// Strictly advertised, not Missing()'s "advertised nothing, so assume the
	// best": being wrong here means treating the final answer as narration, and
	// a placeholder re-anchored after the last message of a turn is one that
	// nothing will ever retire.
	signalsEnd atomic.Bool

	// streamGen counts the session's SSE connections; turnGen is the one the
	// turn in flight started on. A turn that outlives its connection cannot
	// trust the absence of a turn-complete to mean the turn is still running:
	// lifecycle frames carry no seq, so a boundary emitted during the outage is
	// simply gone, while the answer behind it is replayed from seq. Same
	// generation means the whole turn was watched on one connection and an
	// absent boundary is real; a mismatch falls back to ending the turn on the
	// first text, which is what every build before #42 did.
	streamGen atomic.Int64
	turnGen   atomic.Int64

	// turnSeq counts the turns handed to the daemon, so anything holding a
	// timer against a turn can tell "the turn I was armed for" from "whichever
	// turn happens to be running when I fire". Without it the hour-long
	// backstop below would end a *later* turn — disarming that turn's
	// stream-lost notice for a failure it had nothing to do with.
	turnSeq atomic.Int64

	// spoke says the turn in flight has already put text in the thread and was
	// left open for more, which is the state #42's re-anchor creates. It
	// decides what a turn boundary does with the placeholder: normally the
	// message is frozen rather than deleted, because it is the only trace the
	// turn happened, but once the turn has spoken that is no longer true and a
	// stopped "⏳ Working…" sitting under the text is just litter. Reset per
	// turn, and consumed by whichever boundary ends it.
	//
	// backstopped is the claim on arming that turn's backstop, so a turn that
	// narrates five times gets one timer rather than five.
	spoke       atomic.Bool
	backstopped atomic.Bool

	// pmu guards progressMsg, the session's current transient progress message
	// (the indicator placeholder, or the status message being edited with tool
	// steps). Handle posts it before waking; relay edits it on tool activity
	// (status mode) and retires it when the reply is delivered. It never holds
	// the answer. Zero value means none is outstanding.
	pmu         sync.Mutex
	progressMsg chat.MessageRef
	// reanchorGen identifies the delayed re-anchor pending for this entry, or
	// is zero when none is: the timer and flushReanchor each claim it with a
	// compare-and-swap, so it runs once whichever comes first.
	reanchorGen atomic.Int64

	// The ticker's state for the turn in flight, under pmu because it renders
	// into that same progressMsg and must not become a second source of truth
	// for it. turnStart is when the turn was handed to the daemon — what the
	// elapsed clock counts from, not the moment the placeholder landed; tools
	// and step are the last tool activity seen, replayed by each tick so the
	// clock does not erase it; card is every call the turn has made, for an
	// adapter that draws them (card mode, see card.go); tickStop closes the
	// ticker goroutine down.
	turnStart time.Time
	tools     []string
	step      int
	subAgent  string
	subTools  []string
	subCall   int
	card      turnCard
	tickStop  chan struct{}

	// A card's redraws, coalesced (requestRedraw), under pmu with the state
	// they render: redrawAt is when the progress message was last rendered
	// for an edit, and redrawTimer the trailing redraw owed for what has been
	// recorded since, nil when none is. redrawGen tells a timer the turn's
	// state was reset under from the one armed after it.
	redrawAt    time.Time
	redrawTimer *time.Timer
	redrawGen   uint64
	// leftAtExit says the turn in flight had its card finalised by a shutdown
	// (interruptCard): the turn is still running on the daemon, and nothing in
	// the record says so any more. See sessionRecord.InFlight.
	leftAtExit atomic.Bool

	// redrawBackoff is how long the card's coalesced redraws stand off after
	// a failed edit, zero when the last one landed. clockDone says the
	// daemon's own boundary stopped this turn's clock, as against anything
	// else that did; tickDead that the ticker gave up at its deadline while
	// the turn was still nominally running. All three are the turn's, under
	// pmu, and reset with it.
	redrawBackoff time.Duration
	clockDone     bool
	tickDead      bool

	// bmu guards backlog, the messages the daemon has told us are sitting on
	// this session's inbox and have not yet been taken up by a turn. Keyed by
	// the daemon's prompt id, filled from `inbox`/queued and emptied by
	// `inbox`/dequeued — a set rather than a count, so a reconnect replaying
	// either event changes nothing (daemon.InboxChange).
	//
	// It answers one question, in deliverText: when a turn's answer arrives, is
	// there another turn still owed in this thread? That is the second half of
	// #42. The progress placeholder is a single slot, so an answer that retires
	// it while a later turn is still running leaves that turn with no
	// placeholder and no clock; knowing the backlog is non-empty is what turns
	// that retire into a re-anchor.
	//
	// Deliberately not keyed to the messages *switchboard* injected. Anything on
	// this session's inbox produces a turn in this thread — an agent-initiated
	// message (#38), a second gateway, an operator at the daemon's own console —
	// and every one of them is a turn whose placeholder must survive.
	bmu     sync.Mutex
	backlog map[string]struct{}

	// amu guards notices, the tool-activity notices this session has posted and
	// not yet seen every result for. A result names the call it answers, so the
	// notice that announced that call can be edited in place to tick it off —
	// which is what keeps a fifteen-tool turn at fifteen messages instead of
	// thirty (#36). Only the modes that post standalone notices use it; status
	// mode renders its tools into the one message it already edits.
	amu     sync.Mutex
	notices []*activityNote

	// umu guards the usage accounting for the turn currently in flight: usage
	// is what has accrued so far, totals the last running total seen (have
	// says whether there is one to difference against). The daemon reports
	// per-model-call, not per conversational turn, so a turn's real cost is
	// the growth in its totals — see daemon.UsageTotals. Both feeding events
	// arrive before the agent event carrying the answer, so relay accumulates
	// here and deliverText takes the result as the reply's footer.
	// settled says turn-complete has arrived, which is the only signal that
	// what is banked is a whole conversational turn rather than a partial one.
	//
	// unfooted is the turn's latest text posted before its turn-complete, the
	// message a footer belongs on if the boundary turns out to follow it. "Both
	// feeding events arrive before the answer" held for the daemon this was
	// written against; core-agent 2.10 sends the answer first and the
	// turn-complete after (measured live), which left the footer banked for
	// whatever text came next — the next prompt's answer, a turn late.
	umu      sync.Mutex
	usage    daemon.TurnUsage
	totals   daemon.UsageTotals
	have     bool
	settled  bool
	unfooted unfootedText
	// footPending says a turn-complete is waiting to learn which text it
	// belonged to. The next agent event decides (see resolveFoot); footTimer
	// decides instead if the stream goes quiet first. It measures quiet, not
	// time since the boundary: every event the relay processes resets it, so
	// a relay slowed by its own platform calls cannot make it fire early.
	footPending bool
	footTimer   *time.Timer
	footDelay   time.Duration

	// seedMu serializes a new session's first Inject across concurrent turns
	// released by ready, so a second turn cannot overtake the one carrying
	// remembered outbound thread context (or run bare while the first fails
	// and leaves a stale seed behind). seeded turns true once the session
	// accepts a turn or emits turn activity, after which turns run without
	// taking seedMu.
	seedMu sync.Mutex
	seeded atomic.Bool

	// smu guards subagent turn-log watchers and cursors (#193); see subagents.go.
	smu          sync.Mutex
	subFloor     int64
	noSubagents  bool
	notSubagent  map[string]bool
	subSeq       map[string]int64
	subCallTotal map[string]int
	subWatches   map[string][]subWatch
	subPollers   map[string]*subPoller
}

// unfootedText is a posted text that may yet turn out to be its turn's answer.
type unfootedText struct {
	ref  chat.MessageRef
	text string
}

// noteUnfooted records text posted before its turn's boundary, replacing any
// earlier one: only the last text before the boundary is the answer.
func (e *sessionEntry) noteUnfooted(ref chat.MessageRef, text string) {
	e.umu.Lock()
	defer e.umu.Unlock()
	e.unfooted = unfootedText{ref: ref, text: text}
}

// takeUnfootedAnswer hands back the turn's last unfooted text together with
// its now-settled usage, consuming both — or ok false when there is no such
// text or no settled usage, leaving the usage banked for a text still to come
// (the older order, boundary first).
func (e *sessionEntry) takeUnfootedAnswer() (t unfootedText, u daemon.TurnUsage, ok bool) {
	e.umu.Lock()
	defer e.umu.Unlock()
	if !e.footPending || e.unfooted.ref.ID == "" || !e.settled {
		return unfootedText{}, daemon.TurnUsage{}, false
	}
	t, u = e.unfooted, e.usage
	e.unfooted, e.usage, e.settled = unfootedText{}, daemon.TurnUsage{}, false
	e.endFootLocked()
	return t, u, true
}

// dropUnfooted forgets the pending text and any footing armed for it: an
// answer was delivered with the usage, or a new turn began.
func (e *sessionEntry) dropUnfooted() {
	e.umu.Lock()
	defer e.umu.Unlock()
	e.dropUnfootedLocked()
}

func (e *sessionEntry) dropUnfootedLocked() {
	e.unfooted = unfootedText{}
	e.endFootLocked()
}

func (e *sessionEntry) endFootLocked() {
	e.footPending = false
	if e.footTimer != nil {
		e.footTimer.Stop()
		e.footTimer = nil
	}
}

// armFoot marks a boundary pending and starts the quiet timer. A second
// turn-complete replaces the first's.
func (e *sessionEntry) armFoot(d time.Duration, fire func()) {
	e.umu.Lock()
	defer e.umu.Unlock()
	e.endFootLocked()
	e.footPending, e.footDelay = true, d
	e.footTimer = time.AfterFunc(d, fire)
}

// quietReset restarts the quiet timer while a boundary is pending: the stream
// is still talking.
func (e *sessionEntry) quietReset() {
	e.umu.Lock()
	defer e.umu.Unlock()
	if e.footPending && e.footTimer != nil {
		e.footTimer.Reset(e.footDelay)
	}
}

// footIsPending reports whether a turn-complete is waiting on its answer.
func (e *sessionEntry) footIsPending() bool {
	e.umu.Lock()
	defer e.umu.Unlock()
	return e.footPending
}

// claimFootByText settles a pending boundary in favour of a text arriving
// after it: the older, boundary-first order, where this text is the answer
// and takes the usage the ordinary way. The pending text before it, if any,
// was narration.
func (e *sessionEntry) claimFootByText() {
	e.umu.Lock()
	defer e.umu.Unlock()
	if e.footPending {
		e.unfooted = unfootedText{}
		e.endFootLocked()
	}
}

// clearStaleSettled drops a settled turn's usage that no text has claimed by
// the time the footing fires: the turn ended with no answer, or a text from a
// later turn took the usage before this boundary's own (see footLastAnswer).
// Left set, it would read the next turn's first narration as that turn's end.
func (e *sessionEntry) clearStaleSettled() {
	e.umu.Lock()
	defer e.umu.Unlock()
	if e.footPending && e.unfooted.ref.ID == "" && e.settled {
		e.usage, e.settled = daemon.TurnUsage{}, false
	}
	e.endFootLocked()
}

// noteTotals folds one usage-update into the turn in flight by differencing it
// against the last totals seen. The very first report on the entry only
// establishes the baseline — it is the daemon's subscribe-time priming event,
// which on a resumed session already carries every turn the session has ever
// run, and counting it in full would bill the whole history to one reply.
//
// The baseline then persists across reconnects, so a stream that drops and
// resumes differences from where it left off rather than re-baselining. The
// cost of that is a stream outage spanning more than one turn: the reconnect
// yields a single delta covering all of them, which lands on whichever reply
// is relayed first while the rest get no footer. Over-attributing one reply is
// preferable to the alternatives — re-baselining would lose those turns'
// numbers entirely, and treating each stream as fresh would bill a resumed
// session's whole history to its next answer.
func (e *sessionEntry) noteTotals(t daemon.UsageTotals) {
	e.umu.Lock()
	defer e.umu.Unlock()
	prev, seeded := e.totals, e.have
	e.totals, e.have = t, true
	if !seeded {
		return // a baseline only: it describes turns this reply did not run
	}
	if t.Model != "" {
		e.usage.Model = t.Model
	}
	// Guard the deltas: a total that went backwards is not something this can
	// make sense of, and must not turn into a negative token count.
	if d := t.TokensIn - prev.TokensIn; d > 0 {
		e.usage.TokensIn += d
	}
	if d := t.TokensOut - prev.TokensOut; d > 0 {
		e.usage.TokensOut += d
	}
	if d := t.CostUSD - prev.CostUSD; d > 0 {
		e.usage.CostUSD += d
	}
}

// noteTurnComplete records what only the turn-complete event knows — the
// turn's wall-clock duration — and marks the bank complete, which is what
// releases it to the next reply.
func (e *sessionEntry) noteTurnComplete(u daemon.TurnUsage) {
	e.umu.Lock()
	defer e.umu.Unlock()
	if u.Model != "" {
		e.usage.Model = u.Model
	}
	if u.Latency != 0 {
		e.usage.Latency = u.Latency
	}
	e.settled = true
}

// markSettled records the turn's boundary without any figures to go with it,
// for a turn-complete whose payload could not be read.
//
// The event *arriving* is the boundary. daemon.TurnCompleted reports !ok for a
// frame naming neither a model nor a latency as much as for one that will not
// parse, and deriving the boundary from that parse made an empty turn-complete
// mean "this turn is still running" — which then reads the answer as narration
// and re-anchors a placeholder underneath it that nothing ever retires.
func (e *sessionEntry) markSettled() {
	e.umu.Lock()
	defer e.umu.Unlock()
	e.settled = true
}

// resetUsage discards whatever is still banked as a new turn is handed to the
// daemon. A turn that ended without an answer — an error, an interrupt (#34) —
// leaves its numbers with nothing to attach them to, and they must not surface
// on the next reply as if that reply had spent them. The totals baseline is
// deliberately kept: it is what the new turn is differenced against, so the
// dead turn's spend is dropped from the footer rather than double-counted.
func (e *sessionEntry) resetUsage() {
	e.umu.Lock()
	defer e.umu.Unlock()
	e.usage, e.settled = daemon.TurnUsage{}, false
	e.dropUnfootedLocked()
}

// takeUsage reads and clears the turn's accounting, returning nil unless a
// whole conversational turn is banked. settled reports whether turn-complete
// had arrived — which is to say whether the text this is being taken for is the
// turn's answer or something it said on the way there. It is true even when the
// figures themselves come to nothing, so a caller can tell "the turn is over
// and cost nothing worth printing" from "the turn is still running".
//
// The turn-complete gate is what keeps a fraction of a turn from being
// reported as the whole of it. Not every agent event carrying text is the
// answer: a model turn that narrates before calling a tool ("let me check…")
// arrives as text too, and relaying it drains the bank. Since turn-complete
// lands before the answer and after every usage-update, requiring it means an
// interim message gets no footer and the answer gets the complete figure.
//
// The totals baseline survives: it is the running session figure the *next*
// turn differences against.
func (e *sessionEntry) takeUsage() (u *chat.Usage, settled bool) {
	e.umu.Lock()
	defer e.umu.Unlock()
	if !e.settled {
		return nil, false
	}
	banked := e.usage
	e.usage, e.settled = daemon.TurnUsage{}, false
	if banked.Empty() {
		return nil, true
	}
	return &chat.Usage{
		Model:     banked.Model,
		TokensIn:  banked.TokensIn,
		TokensOut: banked.TokensOut,
		CostUSD:   banked.CostUSD,
		Latency:   banked.Latency,
	}, true
}

// awaitingAnswer reports whether a turn's boundary has been banked and the text
// it belongs to has not arrived yet. takeUsage is what clears it, so this is
// exactly the window between the daemon's turn-complete and the agent frame
// carrying that turn's answer.
//
// It is short — the two frames arrive back to back — but it is a window in
// which the entry looks idle to everything that asks turnInFlight, because
// turn-complete has already called endTurn. Anything that would discard the
// turn's accounting or resynchronise on "the session is quiet" has to know the
// difference between quiet and one frame from done.
func (e *sessionEntry) awaitingAnswer() bool {
	e.umu.Lock()
	defer e.umu.Unlock()
	return e.settled
}

// beginTurnInFlight marks a turn as running and clears the previous turn's
// claim on the failure notice. Called *before* the inject request, not after
// it: the relay goroutine is already dispatching, and a turn can fail and emit
// its turn-error before the inject response has even been read. Marking it
// afterwards lost that notice, and could land the store after the relay had
// already ended the turn — leaving inFlight stuck true, so the next unrelated
// outage announced a failure for a turn that answered long ago. Handle undoes
// it if the inject never lands.
func (e *sessionEntry) beginTurnInFlight() {
	e.leftAtExit.Store(false)
	e.failed.Store(false)
	e.spoke.Store(false)
	e.backstopped.Store(false)
	// The bump and the clear are one step, under the lock noteToolCalls takes.
	// A notice is registered only if turnSeq still matches what postActivity
	// read before its send, and the previous turn's notices are dropped here —
	// they are still in the thread, since stream mode's trail is a record
	// rather than a transient, but nothing arriving now belongs to them.
	// Taking the two halves separately leaves a window between them that
	// neither guard covers, and the order does not close it: a late notice
	// landing in the gap either passes a stamp that has not been bumped yet or
	// appends behind a clear that has already run. Under one lock there is no
	// gap, so a late notice lands wholly before the transition, where the clear
	// drops it, or wholly after, where the stamp does.
	e.amu.Lock()
	e.turnSeq.Add(1)
	e.notices = nil
	e.amu.Unlock()
	e.stopAllSubagents()
	e.turnGen.Store(e.streamGen.Load())
	e.inFlight.Store(true)
}

// noteSpoke records that the turn has put text in the thread and been left
// running, and takeSpoke consumes that at the boundary that ends it.
func (e *sessionEntry) noteSpoke()      { e.spoke.Store(true) }
func (e *sessionEntry) takeSpoke() bool { return e.spoke.Swap(false) }

// claimBackstop reports whether this caller is the one that gets to arm the
// turn's backstop. One per turn, however often the turn speaks.
func (e *sessionEntry) claimBackstop() bool { return e.backstopped.CompareAndSwap(false, true) }

// endTurnIf concludes the turn only if it is still the turn the caller was
// armed against, reporting whether this call is the one that did it.
func (e *sessionEntry) endTurnIf(turn int64) bool {
	if e.turnSeq.Load() != turn {
		return false
	}
	return e.endTurn()
}

// watchedWhole reports whether the turn in flight has been on one connection
// for its whole life, and so whether "no turn-complete yet" can be believed.
func (e *sessionEntry) watchedWhole() bool { return e.turnGen.Load() == e.streamGen.Load() }

// noteInbox folds one inbox transition into the backlog: a queued message joins
// it, a dequeued one leaves. Both are idempotent, which is what lets a replayed
// event after a reconnect be applied rather than filtered — see
// daemon.InboxChange. A state this build does not know moves nothing: the spec
// reserves room for more, and guessing which way an unknown one points is how a
// set like this drifts.
func (e *sessionEntry) noteInbox(c daemon.InboxChange) {
	e.bmu.Lock()
	defer e.bmu.Unlock()
	switch {
	case c.Queued():
		if e.backlog == nil {
			e.backlog = make(map[string]struct{})
		}
		e.backlog[c.PromptID] = struct{}{}
	case c.Dequeued():
		delete(e.backlog, c.PromptID)
	}
}

// clearBacklog forgets everything the backlog holds, for a moment the daemon
// has said outright that nothing is waiting.
//
// This is the set's only way back from a lost event. A dequeued emitted during
// a stream outage never arrives — inbox events carry no seq, so the resume does
// not replay them — and the id it would have retired would otherwise sit in the
// backlog for the life of the session, making every answer after it look like
// one with another turn behind it. An idle session has by definition drained
// its inbox and is running nothing, so that is the fact to resynchronise on.
func (e *sessionEntry) clearBacklog() {
	e.bmu.Lock()
	defer e.bmu.Unlock()
	clear(e.backlog)
}

// backlogged reports whether the daemon has work on this session's inbox that
// no turn has taken up yet.
func (e *sessionEntry) backlogged() bool {
	e.bmu.Lock()
	defer e.bmu.Unlock()
	return len(e.backlog) > 0
}

// endTurn concludes the turn in flight, reporting whether this call is the one
// that did it. Nobody is waiting on the daemon once this returns, so a stream
// that drops afterwards is an idle session rather than an abandoned turn.
func (e *sessionEntry) endTurn() bool {
	ended := e.inFlight.CompareAndSwap(true, false)
	e.stopAllSubagents()
	return ended
}

// turnInFlight reports whether a turn is waiting on the daemon right now.
func (e *sessionEntry) turnInFlight() bool { return e.inFlight.Load() }

// claimFailureNotice reports whether this caller is the one that gets to tell
// the thread the turn failed. A turn-error and a lost stream can both be true
// of the same turn, and two notices for one failure read as two failures.
func (e *sessionEntry) claimFailureNotice() bool { return e.failed.CompareAndSwap(false, true) }

// claimCapabilitiesLog reports whether this is the first capabilities frame
// seen for the session, and so the one worth logging.
func (e *sessionEntry) claimCapabilitiesLog() bool { return e.described.CompareAndSwap(false, true) }

// claimPermsWatch reports whether this goroutine is the one that starts the
// entry's permission subscription. See sessionEntry.watchingPerms.
func (e *sessionEntry) claimPermsWatch() bool { return e.watchingPerms.CompareAndSwap(false, true) }

// maxAskedPrompts bounds the set of prompts already put in the thread. It is
// far above what a session plausibly raises, and exists only so a very long
// session cannot grow the set without limit. Overflowing it clears the set
// rather than refusing to ask: the failure it reintroduces is a duplicate
// question, which is recoverable, where the alternative is a blocked agent
// nobody was told about.
const maxAskedPrompts = 4096

// settleState is how a question's outcome has been recorded, in increasing
// order of authority. A record may only ever be replaced by a firmer one.
type settleState uint8

const (
	// unsettled: the question is on screen and still waiting.
	unsettled settleState = iota

	// settledElsewhere: a press found nothing pending. The question is over —
	// it timed out, or it was answered at the agent's own console — but this
	// side does not know how, so the record names no decision.
	settledElsewhere

	// settledHere: a decision the backend confirmed, with an approver on it.
	// Nothing outranks this, and it may replace a settledElsewhere written by
	// a simultaneous press that reached its own answer first.
	settledHere
)

// askRecord is one question this thread has been shown, and how it ended.
//
// The question text is kept so the message can be edited to record what was
// decided without losing what was being decided — an audit line reading
// "Allowed once by ana@example.com", with the command it allowed gone, is not
// one. It is held past a settledElsewhere outcome, which a real one can still
// replace and which needs the question just as much, and dropped the moment a
// decision is recorded, because nothing outranks that. What bounds it until
// then is maxAskedPrompts and the clamping promptText does on every
// agent-supplied field it interpolates.
type askRecord struct {
	text    string
	settled settleState
	// kind is the prompt's approval.Kind, kept because one kind's answer can
	// take a turn's time to come back (respondDeadline).
	kind string
	// posted is set once the question is actually in the thread. Only posted
	// questions are persisted: a claim whose send was cut off by a crash
	// would otherwise be restored as asked, and the revived watcher would
	// never post it at all (#86).
	posted bool
}

// claimAsk reports whether this prompt is one the thread has not been shown
// yet, and records it along with what the thread was shown. See
// sessionEntry.asked.
func (e *sessionEntry) claimAsk(id, text, kind string) bool {
	e.qmu.Lock()
	defer e.qmu.Unlock()
	if e.asked[id] != nil {
		return false
	}
	if len(e.asked) >= maxAskedPrompts {
		e.asked = nil
	}
	if e.asked == nil {
		e.asked = make(map[string]*askRecord)
	}
	e.asked[id] = &askRecord{text: text, kind: kind}
	return true
}

// askKind returns the kind of a prompt this entry posted, or "" for one it
// has no record of.
func (e *sessionEntry) askKind(id string) string {
	e.qmu.Lock()
	defer e.qmu.Unlock()
	if rec := e.asked[id]; rec != nil {
		return rec.kind
	}
	return ""
}

// knownSettled reports whether a press on this gateway has already recorded
// the prompt as over: a confirmed decision, or a "no longer pending" that was
// itself unambiguous (the opener's own press, which nothing could refuse).
// Either way a later "no longer pending" is known to mean exactly that.
func (e *sessionEntry) knownSettled(id string) bool {
	e.qmu.Lock()
	defer e.qmu.Unlock()
	rec := e.asked[id]
	return rec != nil && rec.settled != unsettled
}

// markPosted records that a claimed question reached the thread.
func (e *sessionEntry) markPosted(id string) {
	e.qmu.Lock()
	defer e.qmu.Unlock()
	if rec := e.asked[id]; rec != nil {
		rec.posted = true
	}
}

// releaseAsk undoes a claim whose question never made it into the thread.
func (e *sessionEntry) releaseAsk(id string) {
	e.qmu.Lock()
	defer e.qmu.Unlock()
	delete(e.asked, id)
}

// claimSettle claims the right to record how a question ended at the given
// authority, and hands back the question as it was posted so the record can be
// written under it. Callers hold tmu across the claim and the write it
// authorizes; see sessionEntry.tmu.
//
// Two people can press the same buttons at the same moment, and only one of
// their answers reaches a pending prompt — the other comes back "no longer
// pending", which is true and says nothing about who decided what. Whichever of
// the two gets here first, the record that names an approver is the one the
// thread is left showing: a want that outranks what is recorded replaces it, an
// equal or lesser one is refused.
//
// known reports whether this entry posted the question at all. A session
// adopted after a restart holds no record of what the previous process asked,
// and neither does one whose set was cleared by maxAskedPrompts; there is no
// body to write a record under in either case, which is a different thing from
// a record already written and is answered differently.
func (e *sessionEntry) claimSettle(id string, want settleState) (text string, known, ok bool) {
	e.qmu.Lock()
	defer e.qmu.Unlock()
	rec := e.asked[id]
	if rec == nil {
		return "", false, false
	}
	if rec.settled >= want {
		return "", true, false
	}
	rec.settled = want
	text = rec.text
	if want == settledHere {
		// Nothing outranks this, so no later record will need the question
		// again. It is by far the largest thing here and the record outlives the
		// answer, so hand it over and stop holding it.
		rec.text = ""
	}
	return text, true, true
}

// progressTurn is a progress message taken out of service, with the turn state
// that was being drawn into it: when the turn started, for the final line, and
// the turn's calls, for a card kept as the turn's record.
type progressTurn struct {
	ref   chat.MessageRef
	start time.Time
	card  turnCard
	// live says the turn's clock was still running when the message was
	// taken: the turn was cut off from its message, not finished with it.
	live bool
	// completed says the clock, if it had stopped, was stopped by the
	// daemon's turn boundary: the turn finished, and was not given up on.
	completed bool
}

// takeProgress atomically reads and clears the entry's progress message along
// with the rest of the turn's ticker state, and stops the ticker: the message
// it renders into is about to be deleted.
func (e *sessionEntry) takeProgress() chat.MessageRef {
	return e.takeProgressTurn(false).ref
}

// takeProgressTurn is takeProgress, also handing over the turn state the
// message was drawn from. cardOnly takes the message only if it is a card that
// holds calls, and otherwise leaves everything as it was: the check and the
// take are one step, so a card frozen and replaced by a plain clock in between
// is not taken for the card it was.
func (e *sessionEntry) takeProgressTurn(cardOnly bool) progressTurn {
	e.pmu.Lock()
	if cardOnly && (e.progressMsg.ID == "" || !e.card.holds()) {
		e.pmu.Unlock()
		return progressTurn{}
	}
	t := progressTurn{ref: e.progressMsg, start: e.turnStart, card: e.card, live: e.tickStop != nil, completed: e.clockDone}
	stop := e.tickStop
	e.progressMsg, e.tickStop = chat.MessageRef{}, nil
	e.resetTurnLocked(time.Time{})
	e.pmu.Unlock()
	// Outside the lock, and safe against a concurrent caller: whoever nils
	// tickStop under the lock is the only one holding a channel to close.
	if stop != nil {
		close(stop)
	}
	return t
}

// takeProgressIf takes the progress message out of service if it is still
// ref, reporting whether it was. For a caller that edited ref and let go of
// pmu to do it: a later turn, or a move, can have put another message in the
// slot meanwhile — neither waits on emu to do so — and that one is not this
// caller's to clear.
func (e *sessionEntry) takeProgressIf(ref chat.MessageRef) bool {
	e.pmu.Lock()
	if ref.ID == "" || e.progressMsg != ref {
		e.pmu.Unlock()
		return false
	}
	stop := e.tickStop
	e.progressMsg, e.tickStop = chat.MessageRef{}, nil
	e.resetTurnLocked(time.Time{})
	e.pmu.Unlock()
	if stop != nil {
		close(stop)
	}
	return true
}

// resetTurnLocked clears what one turn drew into the progress message, for
// the next to start from: its clock is dated from start (zero for none), and
// its tools, its calls and any redraw still owed for them are dropped. The
// caller holds pmu.
func (e *sessionEntry) resetTurnLocked(start time.Time) {
	e.turnStart, e.tools, e.step = start, nil, 0
	e.subAgent, e.subTools, e.subCall = "", nil, 0
	e.card = turnCard{}
	if e.redrawTimer != nil {
		e.redrawTimer.Stop()
		e.redrawTimer = nil
	}
	e.redrawGen++
	e.redrawAt = time.Time{}
	e.redrawBackoff, e.clockDone, e.tickDead = 0, false, false
}

// stopTicker halts the turn's ticker while leaving the progress message in
// place for the reply to retire. Called when the daemon says the turn is over,
// so a turn that ends without an answer — an error, an interrupt — stops
// claiming to still be working.
func (e *sessionEntry) stopTicker() {
	e.pmu.Lock()
	stop := e.tickStop
	e.tickStop = nil
	e.pmu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// stopTickerAt is stopTicker for the daemon's own turn boundary, recording
// whether the boundary says the turn completed. The clock stopping and why it
// stopped are one step under pmu, because a card is finalised a moment later
// (settleCard) and a new message can take the slot in between: whoever retires
// the card then has to know it belongs to a turn that finished, not to one
// that was given up on (a completed turn's card read "Stopped", caught in
// review).
func (e *sessionEntry) stopTickerAt(completed bool) {
	e.pmu.Lock()
	stop := e.tickStop
	e.tickStop = nil
	e.clockDone = completed
	e.pmu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// cardTurn reports whether the turn in flight keeps its calls as a card: it
// has recorded one. See Router.carded.
func (e *sessionEntry) cardTurn() bool {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	return e.card.started()
}

// resumeTurnClock re-dates a stranded placeholder to now and hands back a stop
// channel for a fresh ticker, so the message left over from the turn that just
// ended becomes the one timing the turn just starting.
//
// The stranded state is #42's second case in the moment between its two turns:
// a placeholder still in the thread (the answer declined to retire it, because
// something was queued behind it) with no ticker left (the boundary that ended
// the answering turn stopped it). Without this the next turn inherits a frozen
// clock — better than the nothing it used to inherit, and still not a clock.
//
// ok is false in the cases that must not be disturbed: no placeholder to
// re-date, and a placeholder whose turn is still being timed. The second is the
// ordinary path — every turn switchboard injects is dequeued while its own
// ticker runs — and re-dating there would throw away startProgress's deliberate
// choice to count from the message arriving rather than from the daemon picking
// it up.
func (e *sessionEntry) resumeTurnClock(start time.Time) (stop chan struct{}, ok bool) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.progressMsg.ID == "" || e.tickStop != nil {
		return nil, false
	}
	if e.card.holds() {
		// Not a card that holds calls: re-dating it would redraw it as a bare
		// clock and take its calls out of the thread. Every boundary that
		// stops a clock finalises such a card first (settleCard), so this is
		// a guard on that, not a path.
		return nil, false
	}
	stop = make(chan struct{})
	e.tickStop = stop
	e.resetTurnLocked(start)
	return stop, true
}

// beginTurn adopts ref as the turn's progress message, dated from start, and
// returns the previous message (for the caller to delete) plus a stop channel
// for the ticker it should now run. Any ticker still running for a previous
// turn is stopped: only the latest message is live.
func (e *sessionEntry) beginTurn(ref chat.MessageRef, start time.Time) (stale chat.MessageRef, stop chan struct{}) {
	prev, stop := e.supersede(ref, start)
	return prev.ref, stop
}

// supersede is beginTurn, handing back the previous message with the turn
// state that was drawn into it, so a card among them can be kept rather than
// deleted (retireStale).
func (e *sessionEntry) supersede(ref chat.MessageRef, start time.Time) (prev progressTurn, stop chan struct{}) {
	e.pmu.Lock()
	prev = progressTurn{ref: e.progressMsg, start: e.turnStart, card: e.card, live: e.tickStop != nil, completed: e.clockDone}
	old := e.tickStop
	stop = make(chan struct{})
	e.progressMsg, e.tickStop = ref, stop
	e.resetTurnLocked(start)
	e.pmu.Unlock()
	if old != nil {
		close(old)
	}
	return prev, stop
}

// strandProgress installs ref as a placeholder with no clock running: one
// posted for a turn that is queued and has not begun, which picking the
// message up re-dates and starts (resumeTurnClock). Dated from now so that
// anything rendering it meanwhile has a clock to show. false when the slot
// has been filled since — a turn that began in the meantime brought its own —
// and the caller deletes what it posted.
func (e *sessionEntry) strandProgress(ref chat.MessageRef, now time.Time) bool {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.progressMsg.ID != "" {
		return false
	}
	e.progressMsg = ref
	e.resetTurnLocked(now)
	return true
}

// tickerDied is called by a ticker giving up at its deadline. It reports
// whether stop is still the ticker timing the entry's progress message — so
// whether the message is this ticker's to act on — and if so notes that the
// turn now has none.
func (e *sessionEntry) tickerDied(stop <-chan struct{}) bool {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.tickStop == nil || (<-chan struct{})(e.tickStop) != stop {
		return false
	}
	e.tickDead = true
	return true
}

// reviveTicker hands back what a new ticker needs when the turn's own gave up
// at its deadline and the turn has gone on to draw a card: ok is false unless
// both are so. A clock with no ticker merely stops. A card with none has
// nothing to finalise it if the turn's boundary never comes — the deadline
// that would have was spent while there was no card to settle — so a card is
// given a ticker, and with it a deadline, of its own.
func (e *sessionEntry) reviveTicker() (start time.Time, stop chan struct{}, ok bool) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if !e.tickDead || e.tickStop == nil || e.progressMsg.ID == "" || !e.card.holds() {
		return time.Time{}, nil, false
	}
	e.tickDead = false
	return e.turnStart, e.tickStop, true
}

// replaceProgress swaps a freshly posted placeholder in for the one the turn
// was using, leaving the clock, the tool trail and the running ticker exactly
// where they were: this moves the message, it does not start a turn.
//
// Compare-and-swap on old, because the caller had to post the replacement
// before it could offer one, and in that window a later turn may have begun (or
// the turn may have ended and taken its placeholder with it). Overwriting
// blindly would leak whatever took our place — the ticker would keep writing to
// a message no reply is going to retire. ok is false when that happened, and
// the caller deletes what it just posted.
func (e *sessionEntry) replaceProgress(old, fresh chat.MessageRef) bool {
	_, ok := e.moveProgress(old, fresh)
	return ok
}

// moveProgress is replaceProgress, also saying what the message moved from is
// now: left is its card as a stretch left behind, if the turn has a card that
// holds calls at the moment of the swap, and nil if it is a plain clock.
//
// At the swap and not before. The caller rendered the old message before it
// posted the new one, and the turn's first call can land in between: what was
// a clock when it was rendered is then a card, drawn running, by the time it
// is to be deleted — and a card that cannot be deleted has to be finished
// instead (dropCard), which needs this.
func (e *sessionEntry) moveProgress(old, fresh chat.MessageRef) (left *chat.Activity, ok bool) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if old.ID == "" || e.progressMsg != old {
		return nil, false
	}
	e.progressMsg = fresh
	if e.card.holds() {
		left = e.card.segment(time.Since(e.turnStart))
	}
	return left, true
}

// noteActivity records the calls the agent just started — so every later tick
// keeps showing them instead of dropping back to a bare clock. It reports
// whether there is a progress message to show them in; false (a turn whose
// placeholder failed to post, or that never had one) and the caller falls back
// to a standalone notice. The caller redraws: recording and rendering are
// apart so that every render reads the state as it is when its edit goes out,
// never a snapshot an edit already sent has overtaken.
//
// card says the turn's calls are being kept as a card, and detail what the
// mode discloses of them (turnCard.add).
func (e *sessionEntry) noteActivity(calls []daemon.ToolCall, card, detail bool) bool {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.progressMsg.ID == "" || e.turnStart.IsZero() {
		return false
	}
	e.tools, e.step = toolNames(calls), e.step+1
	e.subAgent, e.subTools, e.subCall = "", nil, 0
	if card {
		e.card.add(calls, detail, time.Now())
	}
	return true
}

// noteCardResults files a frame of results against the turn's card. changed is
// whether any answered a call it holds; rest is the results that did not,
// which is all of them when there is no progress message to hold a card.
func (e *sessionEntry) noteCardResults(results []daemon.ToolResult) (changed bool, rest []daemon.ToolResult) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.progressMsg.ID == "" || e.turnStart.IsZero() {
		return false, results
	}
	return e.card.file(results, time.Now())
}

// activityNote is one posted tool-activity notice: the message it went into,
// the calls it announced, and their results as they land. res is parallel to
// calls, nil where the result has not arrived.
//
// detail records the mode the notice was rendered in, so a re-render on a
// result reproduces it rather than assuming.
type activityNote struct {
	subagent string
	ref      chat.MessageRef
	calls    []daemon.ToolCall
	res      []*daemon.ToolResult
	detail   bool
	// seen is when the frame's calls reached the gateway, and took[i] how long
	// call i ran to its result, measured from there (or from eventAt when a
	// subagent's persisted event timestamps are available). Gateway-side, so it
	// is the call plus the stream's latency both ways — close enough to tell a
	// slow command from a quick one, which is what it is for.
	seen    time.Time
	eventAt time.Time
	took    []time.Duration
}

func (n *activityNote) render() string {
	if n.subagent != "" {
		return subagentActivityText(n.subagent, n.calls, n.res, n.took)
	}
	return timedActivityText(n.calls, n.res, n.took, n.detail)
}

// pending returns the index of the oldest still-unanswered call of this name,
// or -1 for none. That is the candidate for a result carrying no id to match
// on.
//
// Oldest and not newest: within one frame the calls are in the order the model
// asked for them, and a run of same-named calls answered in that same order is
// the ordinary case. Matching newest-first inverts every pair — three `bash`
// calls answered in order would each be ticked off against the wrong line, so
// the argument shown next to a verdict would be another call's. Neither
// direction is *correct* without an id, but this one is right whenever the
// order is preserved and wrong only when it is not.
func (n *activityNote) pending(name string) int {
	for i, c := range n.calls {
		if c.Name == name && n.res[i] == nil {
			return i
		}
	}
	return -1
}

// noticeMemory bounds how many tool notices a session keeps addressable. A long
// turn posts one per frame, and a result almost always answers the most recent
// few; the cost of forgetting an older one is a notice that keeps its ▸ rather
// than a wrong edit, so this is deliberately small.
const noticeMemory = 32

// noteToolCalls records a posted notice so the results can find it later. The
// calls are copied: the notice outlives the frame by up to noticeMemory more of
// them and is read under a different lock, so it owns what it holds.
//
// turn is the turn that was current when the notice was *sent*. A newer one
// having started since means forgetActivity has already run and this note is
// the previous turn's, so it is dropped rather than appended behind the clear.
func (e *sessionEntry) noteToolCalls(ref chat.MessageRef, calls []daemon.ToolCall, turn int64, detail bool, seen time.Time) {
	e.amu.Lock()
	defer e.amu.Unlock()
	if e.turnSeq.Load() != turn {
		return
	}
	e.notices = append(e.notices, &activityNote{
		ref:    ref,
		calls:  slices.Clone(calls),
		res:    make([]*daemon.ToolResult, len(calls)),
		detail: detail,
		seen:   seen,
		took:   make([]time.Duration, len(calls)),
	})
	e.notices = capNotices(e.notices)
}

// resolveTool files a result against the notice that announced its call and
// re-renders that notice. ok is false when no notice is waiting for it — a
// result from before switchboard connected, one already answered, or a session
// whose mode posts no notices at all.
//
// Matching is by the daemon's call id where there is one. Those are meant to be
// unique, in which case the order the notices are searched in cannot matter —
// but nothing here can check that, and a daemon that numbers its calls per
// frame ("0", "1", …) would repeat them, so the search runs newest-first. That
// is the safe reading of a repeat: the newest notice is the one still being
// answered. Under ids that really are unique it changes nothing.
//
// Without an id it falls back to the oldest unanswered call of the same name,
// searching the notices oldest-first for the same reason pending scans its
// calls that way: calls go out in order and are usually answered in order, so
// searching from either end newest-first pairs them up backwards. The fallback
// can still be wrong for two same-named calls genuinely in flight at once, and
// the cost is a tick against the wrong line of a notice that lists them both —
// visible, bounded, and preferable to leaving every line at ▸ forever.
//
// The caller must hold amu.
func (e *sessionEntry) resolveTool(r daemon.ToolResult) (*activityNote, bool) {
	if r.ID != "" {
		for i := len(e.notices) - 1; i >= 0; i-- {
			n := e.notices[i]
			if n.subagent != "" {
				continue
			}
			for j, c := range n.calls {
				if c.ID == r.ID && n.res[j] == nil {
					res := r
					n.res[j] = &res
					n.took[j] = time.Since(n.seen)
					return n, true
				}
			}
		}
		// An id we have never seen does not fall back on the name: it belongs
		// to a call some other notice announced, or to none.
		return nil, false
	}
	for _, n := range e.notices {
		if n.subagent != "" {
			continue
		}
		if at := n.pending(r.Name); at != -1 {
			res := r
			n.res[at] = &res
			n.took[at] = time.Since(n.seen)
			return n, true
		}
	}
	return nil, false
}

// toolEdit is one notice a batch of results changed: where to send the edit
// and what it should now say.
type toolEdit struct {
	note *activityNote
	ref  chat.MessageRef
	text string
}

// applyToolResults files a whole frame of results and returns one edit per
// notice touched, not one per result. A frame of three results answering the
// same notice is one message edit carrying all three verdicts, rather than
// three edits of which the first two show a state that was already stale when
// it was written.
func (e *sessionEntry) applyToolResults(results []daemon.ToolResult) []toolEdit {
	e.amu.Lock()
	defer e.amu.Unlock()
	var edits []toolEdit
	file := func(r daemon.ToolResult) {
		note, ok := e.resolveTool(r)
		if !ok {
			return
		}
		if slices.ContainsFunc(edits, func(ed toolEdit) bool { return ed.note == note }) {
			return
		}
		edits = append(edits, toolEdit{note: note, ref: note.ref})
	}
	// Ids first, across the whole frame, before any guess by name. Filing in
	// arrival order lets an id-less result take by name the very line an
	// id-carrying result later in the frame owns outright; that later result
	// then finds the line already answered and is dropped, so one call wears
	// another's verdict and a second stays at ▸ for good. The name fallback
	// should only ever be offered what is genuinely left over.
	for _, r := range results {
		if r.ID != "" {
			file(r)
		}
	}
	for _, r := range results {
		if r.ID == "" {
			file(r)
		}
	}
	// Rendered after every result in the frame is filed, so each notice is
	// rendered once, in its final state.
	for i := range edits {
		edits[i].text = edits[i].note.render()
	}
	return edits
}

// tickRender composes one tick: the message to edit and the line to put in it.
// ok is false once the turn has ended, which is what stops a tick that fired
// just as the reply landed from resurrecting a deleted placeholder. Indicator
// mode gets the clock alone — naming tools is what status mode is for.
//
// A turn whose calls are kept as a card (Router.carded) renders as the card
// once it has made one, and the text beside it is the line status mode
// writes, whichever mode it is: the text is what a notification shows, and
// what is left if the card is refused. Only such a turn's record holds calls,
// so the record holding one is the whole test. Before the first call there is
// no Activity, and the message is the clock it always was.
func (e *sessionEntry) tickRender(mode ProgressMode) (ref chat.MessageRef, text string, act *chat.Activity, ok bool) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	return e.renderLocked(mode)
}

// renderLocked is tickRender for a caller that holds pmu.
func (e *sessionEntry) renderLocked(mode ProgressMode) (ref chat.MessageRef, text string, act *chat.Activity, ok bool) {
	if e.progressMsg.ID == "" || e.turnStart.IsZero() {
		return chat.MessageRef{}, "", nil, false
	}
	elapsed := time.Since(e.turnStart)
	if e.card.holds() {
		text = subTickText(elapsed, e.tools, e.step, e.subAgent, e.subTools, e.subCall)
		return e.progressMsg, text, e.card.live(elapsed, mode == ProgressStream), true
	}
	if mode != ProgressStatus {
		return e.progressMsg, tickText(elapsed, nil, 0), nil, true
	}
	return e.progressMsg, subTickText(elapsed, e.tools, e.step, e.subAgent, e.subTools, e.subCall), nil, true
}

// moveRender is tickRender for a message about to be moved: it also renders
// what the message posted from this render is edited into if it turns out not
// to be wanted and cannot be deleted (dropCard) — its card as a stretch left
// behind, every call in it still out. left is nil for a plain clock. What the
// message being moved from is by then is another matter: see moveProgress.
func (e *sessionEntry) moveRender(mode ProgressMode) (ref chat.MessageRef, text string, act, left *chat.Activity, ok bool) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	ref, text, act, ok = e.renderLocked(mode)
	if ok && act != nil {
		left = e.card.segment(time.Since(e.turnStart))
	}
	return ref, text, act, left, ok
}

// editRender is tickRender for an edit about to go out: it also notes the
// time, which is what the card's coalesced redraws are spaced from.
func (e *sessionEntry) editRender(mode ProgressMode) (ref chat.MessageRef, text string, act *chat.Activity, ok bool) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	e.redrawAt = time.Now()
	return e.renderLocked(mode)
}

// noteEdit records how an edit of the progress message went, for the card's
// coalesced redraws: a failure doubles how long they stand off, from base up
// to the ticker's own ceiling, and a success ends the stand-off.
func (e *sessionEntry) noteEdit(ok bool, base time.Duration) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if ok {
		e.redrawBackoff = 0
		return
	}
	e.redrawBackoff = min(max(e.redrawBackoff, base)*2, progressTickMaxBackoff)
}

// NewRouter builds a Router. progress selects long-turn feedback (ProgressOff
// if empty); m may be nil (metrics recording becomes a no-op); logf may be the
// zero value (logging becomes a no-op).
func NewRouter(client *daemon.Client, out sender, progress ProgressMode, m *metrics, logf logging.Logf) *Router {
	if progress == "" {
		progress = ProgressOff
	}
	r := &Router{
		out:           out,
		defaults:      channelSettings{progress: progress},
		metrics:       m,
		logf:          logf,
		minBackoff:    reconnectMinBackoff,
		maxBackoff:    reconnectMaxBackoff,
		streamGrace:   streamLostGrace,
		tickInterval:  progressTickInterval,
		reanchorDelay: defaultReanchorDelay,
		footDelay:     defaultFootDelay,
		tickMaxAge:    progressTickMaxAge,
		sessions:      make(map[string]*sessionEntry),
		dormant:       make(map[string]sessionRecord),
		bindings:      make(map[string]binding),
		boundTo:       make(map[string]string),
		reserving:     make(map[string]string),
		outbound:      make(map[string]*outboundThread),
		overrides:     make(map[string]ProgressMode),
	}
	r.reg.Store(&registry{agents: singleAgent(client, nil)})
	return r
}

// setShowUsage turns the per-turn usage footer on for every channel that does
// not say otherwise. Unlike the progress mode it is not something a channel
// member can flip at runtime — disclosing spend is an operator decision — so it
// is set once at startup, before the adapter begins dispatching.
func (r *Router) setShowUsage(on bool) { r.defaults.showUsage = on }

// setChannels installs the per-channel settings a config file named (#71),
// already resolved against the defaults: an entry here is the whole answer for
// its channel, not a delta to merge at read time. Keyed by the channel ID an
// adapter reports in Message.Channel.
func (r *Router) setChannels(m map[string]channelSettings) { r.byChannel = m }

// settingsFor resolves the settings in effect for a channel.
//
// Three layers, narrowest first: a runtime progress-mode override set by a chat
// command in that channel, then the config file's entry for it, then the
// process-wide defaults. An empty channel — an ingress post with no channel to
// speak of — has neither of the first two and always resolves to the defaults.
//
// The runtime override applies last and only to the progress mode, which is the
// only setting a chat command can change. That ordering is deliberate for the
// others: a config file saying who may approve in a room must not be reachable
// from inside the room.
//
// A channel's agent settings — its default agent and allow list — may instead
// come from the admin API (#156), which is reachable only by an operator; they
// replace the config's for that channel, never the other settings.
func (r *Router) settingsFor(channel string) channelSettings {
	return r.settingsIn(r.reg.Load(), channel)
}

// settingsIn is settingsFor against a given registry snapshot, whose runtime
// agent settings for the channel (#156) replace the config's.
func (r *Router) settingsIn(reg *registry, channel string) channelSettings {
	s := r.defaults
	if channel == "" {
		return s
	}
	if cs, ok := r.byChannel[channel]; ok {
		s = cs
	}
	s = reg.overlay(channel, s)
	r.omu.Lock()
	m, ok := r.overrides[channel]
	r.omu.Unlock()
	if ok {
		s.progress = m
	}
	return s
}

// progressFor is settingsFor narrowed to the progress mode, which is by far its
// most frequent caller: a turn in flight resolves it on every tick.
func (r *Router) progressFor(channel string) ProgressMode {
	return r.settingsFor(channel).progress
}

// setProgress records a per-channel progress-mode override.
//
// A command that overrides a mode the config file set for this channel says so
// in the log. The file is the operator's statement of intent and the command is
// somebody in a room quietly contradicting it, so the divergence should be
// findable later from outside the room — otherwise the only account of why a
// channel stopped matching its own config block is a chat message that has
// scrolled away (#71).
func (r *Router) setProgress(channel string, mode ProgressMode) {
	if cs, ok := r.byChannel[channel]; ok && cs.progress != mode {
		r.logf.Warnf("progress: channel %s overridden to %q, config file says %q", channel, mode, cs.progress)
	}
	r.omu.Lock()
	r.overrides[channel] = mode
	r.omu.Unlock()
	r.markDirty()
}

// HandleCommand processes a gateway control command and returns a short
// acknowledgment for the adapter to surface to the invoker. It never touches
// the daemon: commands configure the gateway. An unknown or malformed command
// yields a helpful ack rather than an error; the error return is reserved for
// future commands that can fail internally.
func (r *Router) HandleCommand(ctx context.Context, cmd chat.Command) (string, error) {
	r.metrics.recordCommand()
	switch cmd.Name {
	case "progress":
		return r.progressCommand(cmd), nil
	case "agent", "agents":
		return r.agentCommand(ctx, cmd), nil
	case "", "help":
		return commandHelp, nil
	default:
		return fmt.Sprintf("Unknown command %q. %s", cmd.Name, commandHelp), nil
	}
}

// commandHelp is the one-line usage surfaced for an empty, "help", or unknown
// command.
var commandHelp = "Try `progress <" + strings.Join(progressModeNames(), "|") + ">` to set this " +
	"channel's long-turn feedback, or `progress` to see the current mode. " +
	"`agent` lists the agents here, and `agent [name] [prompt]` starts a thread with one."

// Router reports its commands' accepted values, so an adapter can name them in
// whatever its platform affords — today that is the text of Google Chat's
// welcome card, and buttons once a click can reach the app (#29).
var _ chat.CommandChoices = (*Router)(nil)

// Choices implements chat.CommandChoices: the values `progress` accepts. The
// list is progressModes, which is also what parseProgressMode validates
// against, so a mode added there cannot go missing from what an adapter shows.
func (r *Router) Choices(name string) []string {
	if name != "progress" {
		return nil
	}
	return progressModeNames()
}

// progressCommand reads or sets the calling channel's progress mode. With no
// argument it reports the mode in effect; with one it validates and records an
// override. It is channel-scoped, so a command with no channel is refused.
func (r *Router) progressCommand(cmd chat.Command) string {
	if cmd.Channel == "" {
		return "The progress mode can only be set from within a channel."
	}
	if len(cmd.Args) == 0 {
		return fmt.Sprintf("Progress mode for this channel is *%s*. Change it with "+
			"`progress <%s>`.", r.progressFor(cmd.Channel), strings.Join(progressModeNames(), "|"))
	}
	mode, err := parseProgressMode(strings.ToLower(cmd.Args[0]))
	if err != nil {
		return fmt.Sprintf("Unknown progress mode %q. Choose one of: %s.",
			cmd.Args[0], strings.Join(progressModeNames(), ", "))
	}
	r.setProgress(cmd.Channel, mode)
	return fmt.Sprintf("Progress mode for this channel set to *%s*.", mode)
}

// Handle processes one inbound turn: ensure a session exists for the
// conversation (creating it and its relay subscription on the first turn),
// then inject the message, which is what runs the turn.
//
// Inject alone — no wake. The daemon's inject already requests a wake as part
// of queueing the message, so pairing the two verbs signalled twice and ran
// two turns for one message: the real one, then a second with an empty prompt
// whose reply landed in the thread as a duplicate. Wake is for rousing a
// session with nothing new to say, which is not this path.
func (r *Router) Handle(ctx context.Context, msg chat.Message) (err error) {
	// One counter per inbound turn, tallied by outcome on the way out.
	defer func() { r.metrics.recordMessage(err) }()

	// And one audit line, on the way out for the same reason: every exit is
	// a turn somebody caused, including the ones that failed.
	var entry *sessionEntry
	refused := false // a personal agent's thread, and not its owner
	defer func() {
		if refused {
			r.auditTurn(msg, entry, errNotOwner) // refused, nothing sent
			return
		}
		r.auditTurn(msg, entry, err)
	}()

	// A personal agent's thread answers its owner only: mast in a task is
	// single-user and holds its owner's credentials and history, so anyone
	// else driving it would act with the owner's access (#172). Before the
	// session is looked up, so a stranger neither wakes the owner's task nor,
	// where the session has to be rebuilt, becomes its owner (caught in
	// review). Audited as refused; not an error to the adapter.
	if o, ok := r.ownerOf(msg.Conversation); ok && !sameOwner(o, msg.Caller) {
		r.refuseNonOwner(ctx, msg.Conversation, msg.Caller)
		refused = true
		return nil
	}

	entry, err = r.session(ctx, msg.Conversation, msg.Channel, msg.Caller)
	if err != nil {
		if errors.Is(err, errNoAgentHere) {
			if sendErr := r.surfaceNotice(ctx, msg.Conversation, "No agent is available in this channel: its allowed agents exclude its default. An admin needs to fix the channel's agent settings."); sendErr != nil {
				r.logf.Errorf("handle %s: surface no agent: %v", msg.Conversation, sendErr)
			}
			return err
		}
		r.surfaceError(ctx, msg.Conversation, err)
		return err
	}
	// A new turn starts from no accounting: anything left banked belongs to a
	// turn that ended without an answer to carry it.
	//
	// Unless a turn is still running, which is the footer half of #42's second
	// case. A second message in a thread whose first is still working would
	// otherwise discard the accounting of a turn that is about to answer, so
	// the first answer posts bare and the second's footer covers both. The
	// bank belongs to the turn in flight; this message's turn has not started.
	//
	// The cost of the guard is a turn wrongly believed to be running — one
	// killed in a way that emitted no frame — holding its partial figures until
	// its backstop fires, where before they were dropped by the next message.
	// Over-attributing one footer is the same trade noteTotals already makes,
	// and the alternative is losing a footer every time someone follows up.
	//
	// awaitingAnswer covers the rest of the window. turn-complete calls endTurn
	// before the agent frame carrying that turn's text, so for the moment
	// between the two the entry reads as idle while the figures it is holding
	// are the ones that answer is about to print.
	entry.touch()
	entry.pins.Add(1)
	defer entry.pins.Add(-1) // the turn is in flight, or failed, by then
	if !entry.turnInFlight() && !entry.awaitingAnswer() {
		entry.resetUsage()
	}
	// Post the progress message before injecting: inject starts the turn, so a
	// fast reply would otherwise beat the placeholder into the thread and strand
	// it there ("Working…" below the answer, with nothing left to clear it). A
	// no-op in off mode.
	r.startProgress(ctx, entry, msg.Conversation)
	// Someone is waiting from the moment the message is handed over, not from
	// the moment the daemon acknowledges it: the relay is a separate goroutine
	// and the turn can fail on the stream before Inject's response is read.
	entry.beginTurnInFlight()
	err = r.injectTurn(ctx, msg.Conversation, entry, msg.Caller, msg.Text)
	if err != nil {
		// The turn never reached the daemon, so nothing on the stream will ever
		// conclude it, and the progress message would linger; undo both here.
		entry.endTurn()
		r.failProgress(ctx, entry, msg.Conversation)
		if isMissingSession(err) && r.sessionGone(ctx, entry, msg.Caller) {
			// The daemon no longer has the session this thread was using. Say
			// so, and drop the entry (and the binding, if it was one): the next
			// message here opens a session of its own, which is the right thing
			// to do and the wrong thing to do silently.
			//
			// Whoever opened it. adopted decides what the thread is told — a
			// session it was following versus one switchboard opened for it —
			// but not whether the entry is still usable: either way it names a
			// session that does not exist, and kept, it fails every later turn
			// the same way until the process restarts (#88).
			//
			// ERROR, not WARN, though the thread recovers on the next message:
			// this one did not, and the rubric is about the turn. It also has
			// to agree with the adapter, which logs Handle's returned error at
			// ERROR and cannot know this case was already accounted for — two
			// levels for one event is worse than the stricter of the two.
			r.logf.Errorf("handle %s: session %s is gone from the daemon: %v",
				msg.Conversation, sessionRef(entry.sess), err)
			// Only the turn that actually dropped the entry says so. Several
			// messages can be in flight in the same thread, and each would
			// otherwise post its own copy of the same notice.
			if r.discard(msg.Conversation, entry, entry.adopted) {
				notice := sessionLostNotice(entry.sess)
				if entry.adopted {
					notice = bindLostNotice(entry.sess)
				}
				if sendErr := r.surfaceNotice(ctx, msg.Conversation, notice); sendErr != nil {
					r.logf.Errorf("handle %s: surface lost session: %v", msg.Conversation, sendErr)
				}
			}
			return err
		}
		if errors.Is(err, errAgentGone) {
			// Not rerouted: one agent per thread, and the person decides where
			// the conversation goes next (#140).
			if sendErr := r.surfaceNotice(ctx, msg.Conversation, agentGoneNotice(entry.agent)); sendErr != nil {
				r.logf.Errorf("handle %s: surface agent gone: %v", msg.Conversation, sendErr)
			}
			return err
		}
		r.surfaceError(ctx, msg.Conversation, err)
		return err
	}
	return nil
}

// sessionGone reports whether a 404 that caller got from the entry's session
// means the session is gone, rather than that caller is not let in.
//
// The daemon cannot be asked to tell the two apart: under ACL enforcement it
// refuses a caller with the same 404 a missing session gets, deliberately, so
// that a refusal reveals nothing about which sessions exist. So the second
// person to post in a thread — not the session's owner, and not a contributor,
// since switchboard adds none — gets exactly the answer a lost session gives,
// and discarding on it would throw away the owner's working session and open
// one the owner will be refused from in turn.
//
// The relay's identity settles it. It is the one this process subscribes as —
// the creator for a session switchboard opened, none for one it adopted — and
// it is admitted, or the relay would not be running. A 404 for that identity
// too is the session; anything else, including a probe that fails some other
// way, is not proof of a loss and leaves the entry alone. The probe is skipped
// when the caller already is that identity, since its 404 is the answer.
func (r *Router) sessionGone(ctx context.Context, e *sessionEntry, caller string) bool {
	if !e.adopted && caller == e.owner {
		return true
	}
	_, err := r.daemonOf(e).HeadSeq(ctx, e.sess, e.owner)
	return isMissingSession(err)
}

// isMissingSession reports whether the daemon answered "no such session"
// rather than "that request was wrong" or "I am having trouble". Only these
// two codes: a 400 or a 403 says something about the request or the caller,
// and telling a thread its session has vanished on the strength of either
// would be a guess dressed as a fact.
func isMissingSession(err error) bool {
	var se *daemon.StatusError
	return errors.As(err, &se) && (se.StatusCode == http.StatusNotFound || se.StatusCode == http.StatusGone)
}

// surfaceError posts a thread-scoped notice when a turn fails before it ever
// reaches the daemon's event stream (session creation or inject) — the
// cases relay can never recover from, so without this the thread would just
// go silent with only a server log to explain why. Distinguishes transient
// failures (5xx, network — worth a retry) from terminal ones (4xx — retrying
// the same message will fail the same way) via daemon.IsTransient. Best
// effort: a failure to post is logged, not returned, since Handle's own error
// is already the signal the caller acts on.
func (r *Router) surfaceError(ctx context.Context, conv string, err error) {
	text := errNoticeTerminal
	if daemon.IsTransient(err) {
		text = errNoticeTransient
	}
	if sendErr := r.surfaceNotice(ctx, conv, text); sendErr != nil {
		r.logf.Errorf("handle %s: surface error: %v (original: %v)", conv, sendErr, err)
	}
}

// surfaceNotice posts a thread-scoped notice. Best effort, like everything
// that reports a failure by talking to the platform that may itself be the
// thing failing; the send error is returned for the caller to log.
func (r *Router) surfaceNotice(ctx context.Context, conv, text string) error {
	_, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: text, Kind: chat.KindNotice})
	return err
}

const (
	errNoticeTransient = "⚠️ That turn didn't go through — the agent backend is having trouble. Please try again shortly."
	errNoticeTerminal  = "⚠️ That turn didn't go through and retrying the same message won't help. Check the logs or contact an admin."

	// The three leads for a turn that died after it started running. Separate
	// from the two above because the turn *did* reach the daemon: "didn't go
	// through" would be wrong, and in the guardrail case so would any
	// suggestion to try again.
	errNoticeTurnTransient = "⚠️ That turn failed before it could answer — the agent backend is having trouble. Please try again shortly."
	errNoticeTurnTerminal  = "⚠️ That turn failed before it could answer, and retrying the same message won't help."
	errNoticeGuardrail     = "🛑 A guardrail stopped that turn. The agent will refuse further turns until an operator resets it."

	// errNoticeStreamLost covers the failure the daemon cannot report, because
	// it is the daemon that went away. Deliberately does not tell the reader
	// the turn failed — the relay resumes from the last event seen, so an
	// answer produced during the outage is still delivered when it reconnects.
	errNoticeStreamLost = "⚠️ Lost contact with the agent while that turn was running. Reconnecting — if the turn finished, its answer will still arrive. If nothing appears, send the message again."
)

// turnErrorNotice renders a daemon-side turn failure for the thread: what it
// means for the reader on the first line, the daemon's own classification on
// the second, and its hint — the actionable next step, when there is an obvious
// one — on the third.
//
// The daemon's message carries the notice: without it this degenerates into
// "something went wrong", which is barely better than the silence it replaces.
// It is clamped here rather than trusted, because the two guardrail trips build
// their message directly instead of through the daemon's length-capping
// classifier, and the watchdog's interpolates a trigger reason of no fixed
// size. Worth knowing that this is upstream provider text going into what may
// be a shared channel.
func turnErrorNotice(te daemon.TurnError) string {
	lead := errNoticeTurnTerminal
	switch {
	case te.Guardrail():
		lead = errNoticeGuardrail
	case te.Retryable:
		lead = errNoticeTurnTransient
	}
	detail := te.Kind
	if te.Code != "" {
		detail += " (" + te.Code + ")"
	}
	if msg := clampNotice(te.Message); msg != "" {
		detail += ": " + msg
	}
	notice := lead + "\n" + detail
	if hint := clampNotice(te.Hint); hint != "" {
		notice += "\n" + hint
	}
	return notice
}

// noticeDetailCap matches the cap the daemon's own error classifier applies, so
// a classified failure reads identically either way and only the paths that
// skip that classifier are actually shortened.
const noticeDetailCap = 240

// clampNotice bounds one line of daemon-supplied text and flattens it, on the
// way into a chat message. Newlines go because the notice's own structure is
// line-based: a multi-line message would read as if the hint had arrived early.
// Cuts on a rune boundary — a chat client rendering half a rune shows a
// replacement character, which looks like corruption rather than truncation.
func clampNotice(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) <= noticeDetailCap {
		return s
	}
	cut := noticeDetailCap - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// failTurn ends a turn that will never answer: retire its progress message so
// the thread is not left with a clock running on nothing, and post the notice
// in its place. Best effort on both — a failure to clear or to post is logged,
// since there is no caller left to return an error to.
//
// claimFailureNotice makes this fire once per turn, and endTurn stops anything
// downstream still treating the turn as live. Note that it is not gated on the
// turn *being* live: a turn that already delivered text can still fail at its
// boundary, and that failure is worth reporting.
//
// A message queued behind the failed turn keeps the placeholder, the same trade
// deliverText makes for an answer with a backlog behind it (#42). A turn that
// died is still a turn the next one has to follow, and the alternative is the
// queued turn running with no clock at all. Only the clock is stopped — this
// path has no turn-complete behind it to have stopped it already — and the
// notice is posted underneath, after which the placeholder is moved back to the
// bottom so the queued turn's clock is where a reader is looking.
//
// Either way a card that holds calls stays in the thread, finalised as
// stopped: it is what the turn did before it died.
func (r *Router) failTurn(ctx context.Context, e *sessionEntry, conv, kind, notice string) {
	if !e.claimFailureNotice() {
		return
	}
	e.endTurn()
	queued := e.backlogged()
	// A card that holds calls is the failed turn's record, not a clock to
	// pass on: it is finalised where it is, and the queued turn is given a
	// placeholder of its own below the notice.
	handed := false
	if queued {
		e.stopTicker()
		handed = r.finaliseCard(ctx, e, conv, false)
	} else {
		r.failProgress(ctx, e, conv)
	}
	r.metrics.recordTurnFailed(kind)
	_, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: notice, Kind: chat.KindNotice})
	if handed {
		// Whether or not the notice landed: the old message is no longer a
		// placeholder, so without this the queued turn has none.
		r.queuePlaceholder(ctx, e, conv)
	}
	if err != nil {
		r.logf.Errorf("relay %s: surface turn failure (%s): %v", conv, kind, err)
		return
	}
	if queued && !handed {
		// Nothing was posted below the placeholder unless the Send above
		// succeeded, and re-anchoring against nothing spends two API calls to
		// change nothing — or leaves a duplicate if the delete half fails.
		r.reanchorProgress(ctx, e, conv)
	}
}

// reliedOnEvents are the daemon events switchboard has a consumer for. A
// daemon that does not advertise one of these is not an error — switchboard
// still works, and older daemons exist — but each absence silently costs a
// feature, so the operator is told which.
//
// daemon.EventAgent is deliberately absent: the daemon does not list its legacy
// event name in the capabilities frame at all, and warning about it would fire
// against every conformant daemon there is. The surface it carries is checked
// under the names the frame does use — a daemon advertising no stream-chunk
// relays no answers, which is the loudest absence of the set and the one it
// would be strangest to leave out.
var reliedOnEvents = []string{
	daemon.EventStreamChunk,
	daemon.EventToolCall,
	daemon.EventStatusUpdate,
	daemon.EventUsage,
	daemon.EventTurnComplete,
	daemon.EventTurnError,
	daemon.EventInbox,
}

// noteCapabilities records what the daemon said about itself when it opened the
// stream: once per session, since every reconnect repeats the frame.
//
// This is the only place the pairing is visible. Switchboard's error notices,
// usage footers and progress boundaries each depend on an event the daemon may
// or may not send, and against a daemon that does not send one the symptom is
// absence — a thread that stays quiet, a reply with no footer, a clock that
// runs to its backstop. None of those looks like a version mismatch from the
// outside, so the mismatch is stated here instead of left to be deduced.
func (r *Router) noteCapabilities(ctx context.Context, conv string, e *sessionEntry, c daemon.Capabilities) {
	// Before the log claim, not after it: this is read on every turn and has to
	// be refreshed by every frame, while the logging below is deliberately
	// once-per-session.
	e.signalsEnd.Store(c.Advertises(daemon.EventTurnComplete))
	// Also before it, and for a related reason: the watcher is started once per
	// entry rather than once per frame, but the claim that decides which frame
	// wins belongs to the watcher, not to the logging.
	r.watchPermsIfOffered(ctx, conv, e, c)
	if !e.claimCapabilitiesLog() {
		return
	}
	server, version := c.Server, c.ProtocolVersion
	if server == "" {
		server = "unidentified daemon"
	}
	if version == "" {
		version = "an unstated protocol version"
	}
	r.logf.Infof("relay %s: connected to %s speaking %s", conv, server, version)
	if missing := c.Missing(reliedOnEvents...); len(missing) > 0 {
		r.logf.Warnf("relay %s: daemon does not advertise %s; the features reading those events "+
			"will stay silent", conv, strings.Join(missing, ", "))
	}
}

// startProgress posts the initial progress message for a turn (every mode but
// off), records it on the entry so relay can edit or clear it, and starts the
// ticker that keeps its clock running. A message still outstanding from a prior turn (a second turn started
// before the first replied) is deleted so only the latest remains. Failures
// are logged, never fatal — a missing progress message must not drop the turn.
func (r *Router) startProgress(ctx context.Context, e *sessionEntry, conv string) {
	if !r.progressFor(e.channel).clocked() {
		return
	}
	// Time the turn from here rather than from the post that is about to
	// happen: the clock is meant to answer "how long have I been waiting", and
	// the wait began when the message arrived, not when the placeholder landed.
	start := time.Now()
	ref, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: workingText, Kind: chat.KindProgress})
	r.metrics.recordReply(err)
	if err != nil {
		r.logf.Warnf("progress %s: post: %v", conv, err)
		return
	}
	stale, stop := e.supersede(ref, start)
	if stale.ref.ID != "" {
		// A previous turn's, left frozen — often by a turn that ended without
		// an answer, so "stopped" rather than "done" (caught in review).
		e.emu.Lock()
		r.retireStale(ctx, conv, stale)
		e.emu.Unlock()
	}
	if r.tickInterval > 0 {
		go r.tick(ctx, e, conv, start, stop)
	}
}

// tick re-renders the turn's progress message on a coarse interval until the
// turn ends, so a long turn that runs silently — no tool calls to report —
// still visibly moves instead of sitting on a placeholder that cannot be told
// apart from a wedged one (#37).
//
// It exits on stop (the reply landed, the daemon called the turn done, or a
// later turn took over) or on ctx, which is the adapter's lifetime and not a
// per-request context — the same assumption relay makes.
//
// An edit is decoration. A failure is logged and backed off, never returned:
// nothing here may cost the turn its answer, and a conversation that is being
// rate limited must not be answered with more edits at the same rate.
func (r *Router) tick(ctx context.Context, e *sessionEntry, conv string, start time.Time, stop <-chan struct{}) {
	interval := r.tickInterval
	timer := time.NewTimer(interval)
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
		case <-stop:
			return
		case <-deadline.C:
			// The clock only. Whether the *turn* is still owed is not something
			// the renderer gets to decide — that is the backstop's job, and it
			// runs in every mode rather than only those that have a ticker.
			//
			// The message stays, frozen at the age it reached. Deleting it
			// would leave the thread showing the question and nothing else,
			// which reads as never having been heard; a stopped clock at least
			// says how far the turn got. The next turn in the thread clears it
			// as stale.
			//
			// Unless it is a card that holds calls, which would spin where a
			// clock merely stops: that is finalised as stopped, and kept.
			r.logf.Warnf("progress %s: no turn boundary after %s; stopping the clock", conv, formatElapsed(time.Since(start)))
			if e.tickerDied(stop) {
				r.settleCard(ctx, e, conv, false)
			}
			return
		case <-timer.C:
		}
		ok, err := r.redrawProgress(ctx, e, conv, r.progressFor(e.channel))
		if !ok {
			return // the turn ended between the tick firing and this read
		}
		if err != nil {
			r.logf.Warnf("progress %s: tick: %v", conv, err)
			interval = min(interval*2, progressTickMaxBackoff)
		} else {
			interval = r.tickInterval
		}
		timer.Reset(interval)
	}
}

// retireSpokenPlaceholder deletes the placeholder at a turn boundary, but only
// for a turn that has already put text in the thread.
//
// The boundaries deliberately freeze the message rather than delete it: for a
// turn that ended with nothing to say, a stopped "⏳ Working… 2m30s" is the
// only record that the question was heard at all. #42's re-anchor breaks that
// reasoning — once the turn has spoken, the thread has its record, and what the
// freeze leaves behind is a dead clock sitting *underneath* the text. That is
// the shape a turn takes when it narrates and then ends without an answer, or
// when the answer itself is misread as narration because this turn's boundary
// never came.
//
// No-op for a turn that never spoke, which is every turn in the common case:
// turn-complete arrives before the answer, so the flag is still clear and the
// placeholder survives to be retired by the answer a moment later.
// No-op too when a message is queued behind this turn. The placeholder is one
// slot for the whole conversation and the queued turn is about to want it, so
// deleting it here hands that turn a thread with no clock — #42's second case,
// reached one step earlier than through deliverText. The clock is already
// stopped by the boundary that got here, so what stays in the thread is a
// frozen elapsed time, which the dequeued event re-dates and restarts.
func (r *Router) retireSpokenPlaceholder(ctx context.Context, e *sessionEntry, conv string) {
	if !e.takeSpoke() {
		return
	}
	if e.backlogged() {
		return
	}
	r.clearProgress(ctx, e, conv)
}

// armTurnBackstop starts the one thing standing between a turn that spoke and
// then died without a boundary frame and an entry that claims to be working for
// the rest of the process's life, announcing a lost turn at the next unrelated
// outage.
//
// Only #42's re-anchor needs it. Every other path out of deliverText ends the
// turn on the spot; this is the one that deliberately leaves it running on the
// strength of an absent turn-complete, and an absence is exactly the kind of
// evidence that can turn out to be wrong — core-agent's cost-ceiling and
// watchdog pre-flights emit no frame at all. It is armed here rather than
// alongside the ticker because off mode has no ticker, and the turn is in
// flight in every mode.
//
// The timer is held against the turn it was armed for, not against the entry:
// a later turn that is legitimately running must not be ended by an hour-old
// timer belonging to the turn before it.
func (r *Router) armTurnBackstop(ctx context.Context, e *sessionEntry, conv string) {
	if !e.claimBackstop() {
		return
	}
	maxAge := r.tickMaxAge
	if maxAge <= 0 {
		maxAge = progressTickMaxAge
	}
	turn := e.turnSeq.Load()
	go func() {
		timer := time.NewTimer(maxAge)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !e.endTurnIf(turn) {
			return // the turn ended on its own, or a later one replaced it
		}
		r.logf.Warnf("relay %s: no turn boundary after %s; giving the turn up", conv, formatElapsed(maxAge))
		e.stopTicker()
		r.settleCard(ctx, e, conv, false)
	}()
}

// resumeProgress hands a stranded placeholder to the turn that has just picked
// up a queued message: the clock restarts from now, and the entry goes back to
// waiting on the daemon.
//
// beginTurnInFlight and not a bare inFlight store, because this really is a new
// turn and every per-turn claim on the entry has to move with it — the
// stream-lost notice, the spoke flag the boundary reads, the backstop, and the
// turn stamp late tool notices are matched against. The one thing it does not
// do is post: the message is already in the thread, put there for the turn
// before this one and kept because this one was coming.
func (r *Router) resumeProgress(ctx context.Context, e *sessionEntry, conv string) {
	stop, ok := e.resumeTurnClock(time.Now())
	if !ok {
		return
	}
	e.beginTurnInFlight()
	if r.tickInterval > 0 {
		go r.tick(ctx, e, conv, time.Now(), stop)
	}
}

// clearProgress retires and forgets the entry's outstanding progress message,
// if any — the turn is done. Called before a reply is relayed so the transient
// message gives way to the answer. No-op when none is outstanding.
func (r *Router) clearProgress(ctx context.Context, e *sessionEntry, conv string) {
	r.retireProgress(ctx, e, conv, true)
}

// failProgress is clearProgress for a turn that did not finish: refused, its
// stream lost, never sent.
func (r *Router) failProgress(ctx context.Context, e *sessionEntry, conv string) {
	r.retireProgress(ctx, e, conv, false)
}

// retireProgress takes the placeholder out of service. Deleted, where that
// leaves nothing behind; where it leaves a trace (Google Chat's "Message
// deleted by its author", reported from the GKE deployment), edited instead
// into a final line — "✔ Done · 12s", or "✖ Stopped · 12s" — which says
// something true where the clock was.
//
// Unless it is a card that holds calls. That is the turn's record, and it is
// finalised where it is and kept (retireTaken).
func (r *Router) retireProgress(ctx context.Context, e *sessionEntry, conv string, done bool) {
	// Under emu: a tick in flight finishes its edit first, and one after this
	// finds the message no longer current and leaves it alone.
	e.emu.Lock()
	defer e.emu.Unlock()
	t := e.takeProgressTurn(false)
	if t.ref.ID == "" {
		return
	}
	r.retireTaken(ctx, conv, t, done)
}

// retireTaken retires a progress message already taken from its entry. A plain
// clock says nothing once the turn is over and goes as it always has
// (retireMessage). A card that holds calls is finalised in place instead and
// left in the thread: "Done" or "Stopped" over every call the turn made, none
// of them left running — in stream mode those calls are detail that was
// permanent before cards, and in either mode they are what the agent ran.
//
// A card is never deleted, whatever becomes of the edit that finalises it:
// the edit is tried again (finishCard), and a card that still cannot be
// finalised is left as it was last drawn and named in the log. It used to be
// retired like a clock, which deleted the turn's record because one edit
// failed (caught in review).
//
// The caller holds emu.
func (r *Router) retireTaken(ctx context.Context, conv string, t progressTurn, done bool) {
	if t.card.holds() {
		r.finishCard(ctx, conv, t.ref, t.card.final(time.Since(t.start), done))
		// The snapshot named this message as a card in flight; it no longer
		// is one, and a restart must not finalise it a second time.
		r.markDirty()
		return
	}
	r.retireMessage(ctx, conv, t.ref, t.start, done)
}

// retireStale retires the progress message a new turn has taken the slot
// from. A clock is deleted, as before. A card that holds calls is kept: as a
// finished stretch if its turn was still running — the turn carries on, and
// what it does next is drawn into the new turn's message, the slot being one
// per conversation — and, if its clock had already stopped, as done or as
// stopped according to what stopped it: the daemon's boundary for a turn that
// completed, or a deadline that gave up on it.
//
// The caller holds emu.
func (r *Router) retireStale(ctx context.Context, conv string, t progressTurn) {
	if t.card.holds() {
		act := t.card.final(time.Since(t.start), t.completed)
		if t.live {
			act = t.card.segment(time.Since(t.start))
		}
		r.finishCard(ctx, conv, t.ref, act)
		r.markDirty()
		return
	}
	r.retireMessage(ctx, conv, t.ref, time.Time{}, false)
}

const (
	// cardFinishAttempts is how many times a card's finalising edit is tried
	// in all. Small: the edit is decoration on a turn that is already over,
	// and a platform that refuses it four times is not having a blip.
	cardFinishAttempts = 4

	// defaultCardRetry is the wait before the second attempt; it doubles for
	// each one after.
	defaultCardRetry = 500 * time.Millisecond

	// cardRetryCap is the longest one wait gets, however long a rate limit
	// asked for: past this the turn's last word has been sitting under a
	// spinning card long enough to say so in the log instead.
	cardRetryCap = time.Minute

	// cardFinishGrace bounds the background retries of one card in all.
	cardFinishGrace = 3 * time.Minute

	// exitCardGrace is how long a shutdown spends finalising the cards still
	// running (finishCardsAtExit). A few seconds: it runs inside whatever
	// grace the process was given to stop, with the listeners still to drain.
	exitCardGrace = 5 * time.Second
)

// finishReply is the edit that makes a message the finished card act. The
// text says what the card does, as Reply.Activity promises: a client that
// cannot draw the card, or a Slack that refuses it, still shows the record
// rather than a bare line nobody asked to keep.
func finishReply(conv string, act *chat.Activity) chat.Reply {
	return chat.Reply{Conversation: conv, Text: cardText(act), Kind: chat.KindActivity, Activity: act}
}

// finishCard edits a message into the finished card act: the last edit a card
// gets, after which nothing will touch it again. So it has to land. A card
// left as it was last drawn is a spinner that never stops, over a turn that is
// over.
//
// One attempt is made here, in line — the caller is usually the relay, with an
// answer waiting to go out underneath. If it fails, the card is owed the edit
// from then on (owe): it is in the snapshot until the edit lands, so neither
// running out of attempts nor the process ending forgets it. The remaining
// attempts are made in the background, which is safe to do unordered: the
// message has already been taken out of service, and no other editor has a
// reference to it.
func (r *Router) finishCard(ctx context.Context, conv string, ref chat.MessageRef, act *chat.Activity) {
	reply := finishReply(conv, act)
	err := r.out.Update(ctx, ref, reply)
	if err == nil {
		return
	}
	if !retryableEdit(err) {
		r.logf.Warnf("progress %s: message %s could not be finalised (%v); it is left as it was last drawn, which may be as running",
			conv, ref.ID, err)
		return
	}
	o := r.owe(conv, ref, act, time.Now())
	r.fmu.Lock()
	r.finishing++
	r.fmu.Unlock()
	go func() {
		defer func() {
			r.fmu.Lock()
			r.finishing--
			r.fmu.Unlock()
		}()
		// Detached from the caller: the relay's context ends with its
		// session, and the edit is owed either way.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cardFinishGrace)
		defer cancel()
		r.settleOwed(o, r.retryFinish(rctx, conv, ref, reply, err))
	}()
}

// finishCardNow is finishCard for a caller with nothing waiting behind it — a
// restart, a shutdown: every attempt is made before it returns. It returns nil
// once one has landed, and otherwise how the last one failed.
func (r *Router) finishCardNow(ctx context.Context, conv string, ref chat.MessageRef, act *chat.Activity) error {
	reply := finishReply(conv, act)
	err := r.out.Update(ctx, ref, reply)
	if err == nil {
		return nil
	}
	return r.retryFinish(ctx, conv, ref, reply, err)
}

// maxOwedCards bounds the cards owed their last edit at once. It is reached
// only by a platform that has refused edits across hundreds of turns; past it
// the oldest is given up, and named in the log.
const maxOwedCards = 256

// owe records that a card is owed the edit that finalises it, and schedules a
// write: from here it is in the snapshot until settleOwed takes it out. since
// is when it first became owed.
func (r *Router) owe(conv string, ref chat.MessageRef, act *chat.Activity, since time.Time) *owedCard {
	o := &owedCard{conv: conv, ref: ref, act: act, since: since}
	r.fmu.Lock()
	if r.owed == nil {
		r.owed = make(map[chat.MessageRef]*owedCard)
	}
	var dropped *owedCard
	if _, again := r.owed[ref]; !again && len(r.owed) >= maxOwedCards {
		for _, c := range r.owed {
			if dropped == nil || c.since.Before(dropped.since) {
				dropped = c
			}
		}
		delete(r.owed, dropped.ref)
	}
	r.owed[ref] = o
	r.fmu.Unlock()
	if dropped != nil {
		r.logf.Warnf("progress %s: too many cards are owed their last edit; giving up on message %s, which is left as it was last drawn, which may be as running",
			dropped.conv, dropped.ref.ID)
	}
	r.markDirty()
	return o
}

// settleOwed records how the attempts at an owed card's edit ended, err being
// nil if one landed. The card stops being owed when the edit has landed, and
// when the platform has refused it for good — the message is gone, or is not
// ours to edit, and asking again on every boot would not change that. Any
// other failure leaves it owed: the next process tries again (restore), as
// does this one on its way out (finishCardsAtExit).
func (r *Router) settleOwed(o *owedCard, err error) {
	if err != nil && retryableEdit(err) {
		return
	}
	r.fmu.Lock()
	if r.owed[o.ref] == o {
		delete(r.owed, o.ref)
	}
	r.fmu.Unlock()
	r.markDirty()
}

// owedCards lists the cards owed their last edit, oldest first, and by message
// among those owed at the same moment, so that a snapshot is the same however
// the map happens to be walked.
func (r *Router) owedCards() []*owedCard {
	r.fmu.Lock()
	out := make([]*owedCard, 0, len(r.owed))
	for _, o := range r.owed {
		out = append(out, o)
	}
	r.fmu.Unlock()
	slices.SortFunc(out, func(a, b *owedCard) int {
		if c := a.since.Compare(b.since); c != 0 {
			return c
		}
		if c := strings.Compare(a.conv, b.conv); c != 0 {
			return c
		}
		return strings.Compare(a.ref.ID, b.ref.ID)
	})
	return out
}

// retryFinish makes the attempts at a finalising edit that follow a failed
// first one, err being how that one failed. It waits between them, longer each
// time, and for at least as long as the platform asked if the failure was a
// rate limit that said (chat.RetryAfter) — answering a rate limit with another
// edit at once is how it stays one. A failure the platform calls permanent is
// not retried.
//
// If none lands, the card is left as it is — never deleted: it holds the
// turn's calls, and a record that is stale is worth more than none — and the
// log names it, so that an operator can find the one message in the thread
// that may still be drawn as running. It returns nil once an attempt has
// landed, and otherwise how the last one failed, which is what tells the
// caller whether the edit is still worth owing (settleOwed).
func (r *Router) retryFinish(ctx context.Context, conv string, ref chat.MessageRef, reply chat.Reply, err error) error {
	wait := r.cardRetry
	if wait <= 0 {
		wait = defaultCardRetry
	}
	for attempt := 1; attempt < cardFinishAttempts && retryableEdit(err); attempt++ {
		pause := wait
		if after, ok := chat.RetryAfter(err); ok && after > pause {
			pause = after
		}
		pause = min(pause, cardRetryCap)
		r.logf.Warnf("progress %s: finalise card %s: %v; trying again in %s", conv, ref.ID, err, pause)
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			r.logf.Warnf("progress %s: message %s could not be finalised (%v, and no time left to try again); it is left as it was last drawn, which may be as running",
				conv, ref.ID, err)
			return err
		case <-timer.C:
		}
		if err = r.out.Update(ctx, ref, reply); err == nil {
			return nil
		}
		wait *= 2
	}
	r.logf.Warnf("progress %s: message %s could not be finalised (%v); it is left as it was last drawn, which may be as running",
		conv, ref.ID, err)
	return err
}

// retryableEdit reports whether an edit that failed with err is worth making
// again: not one the platform has refused for good.
func retryableEdit(err error) bool {
	return !errors.Is(err, chat.ErrNotFound) && !errors.Is(err, chat.ErrDenied) && !errors.Is(err, chat.ErrUnsupported)
}

// finishCardsAtExit finalises every card still running, as the process is
// about to exit: each conversation's progress message that is a card holding
// calls is edited to say a restart interrupted it. Without this a shutdown
// leaves every such card as it was last drawn, and with no state file nothing
// ever comes back for them: a spinner, for good, in each thread.
//
// ctx is the serve context, usually cancelled by now; the edits run detached
// from it, all conversations at once, inside exitCardGrace in all. A card
// that is finalised is taken out of service, so the snapshot written after
// this no longer names it. One that could not be is left named there, with
// its calls, for the next process to finalise (restore) — and without a state
// file, in the log.
func (r *Router) finishCardsAtExit(ctx context.Context) {
	xctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exitCardGrace)
	defer cancel()
	type live struct {
		conv string
		e    *sessionEntry
	}
	var entries []live
	r.mu.Lock()
	for conv, e := range r.sessions {
		select {
		case <-e.ready:
		default:
			continue
		}
		if e.err == nil {
			entries = append(entries, live{conv, e})
		}
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, l := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.interruptCard(xctx, l.e, l.conv)
		}()
	}
	wg.Wait()
	// And the finalising edits of turns that had already ended, still being
	// retried in the background: give them what is left of the grace.
	for waiting := true; waiting; {
		r.fmu.Lock()
		pending := r.finishing
		r.fmu.Unlock()
		if pending == 0 {
			break
		}
		select {
		case <-xctx.Done():
			waiting = false
		case <-time.After(20 * time.Millisecond):
		}
	}
	// Whatever is still owed its last edit gets one more try, now: a card
	// that ran out of attempts during an outage long over would otherwise
	// spin until the next process came up. What is still owed after this is
	// in the last snapshot, for that process — and named here, since without
	// a state file there is none.
	owed := r.owedCards()
	if xctx.Err() == nil {
		for _, o := range owed {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.settleOwed(o, r.out.Update(xctx, o.ref, finishReply(o.conv, o.act)))
			}()
		}
		wg.Wait()
	}
	for _, o := range r.owedCards() {
		r.logf.Warnf("progress %s: exiting while message %s is still owed its last edit; it is left as it was last drawn, which may be as running", o.conv, o.ref.ID)
	}
}

// interruptCard finalises e's progress message as interrupted by a restart, if
// it is a card that holds calls, and takes it out of service once that has
// landed. A clock is left alone, as a shutdown always has left it.
//
// Taking it out of service takes the placeholder off the entry's record, which
// was how the next process knew a turn was in flight here. The turn still is —
// the daemon carries on — so the entry says so itself (leftAtExit), and the
// record with it (sessionRecord.InFlight).
func (r *Router) interruptCard(ctx context.Context, e *sessionEntry, conv string) {
	// Under emu, like every other editor: a redraw in flight lands first,
	// and none can follow.
	e.emu.Lock()
	defer e.emu.Unlock()
	e.pmu.Lock()
	ref, holds := e.progressMsg, e.card.holds()
	var act *chat.Activity
	if holds {
		act = e.card.interrupted()
	}
	e.pmu.Unlock()
	if ref.ID == "" || !holds {
		return
	}
	if r.finishCardNow(ctx, conv, ref, act) != nil {
		return
	}
	// Only if it is still the message that was edited. emu orders editors,
	// not the slot: a later turn, or a move, puts another message in it
	// without waiting here, and that one is not this card.
	if e.takeProgressIf(ref) {
		e.leftAtExit.Store(e.turnInFlight())
	}
}

// finaliseCard finalises the entry's progress message if it is a card that
// holds calls, reporting whether it was. A clock, or no message, is left
// exactly as it is.
func (r *Router) finaliseCard(ctx context.Context, e *sessionEntry, conv string, done bool) bool {
	e.emu.Lock()
	defer e.emu.Unlock()
	t := e.takeProgressTurn(true)
	if t.ref.ID == "" {
		return false
	}
	r.retireTaken(ctx, conv, t, done)
	return true
}

// settleCard is what every boundary that stops a turn's clock without retiring
// its message does about a card. A clock left behind stops, and says how far
// the turn got; a card left behind keeps its spinner for good, claiming a turn
// that is over is still running. So a card that holds calls is finalised at
// the boundary — the answer, if one is still coming, lands under it and finds
// nothing left to retire — and a turn queued behind it is given a placeholder
// of its own, where it would otherwise have inherited this one.
func (r *Router) settleCard(ctx context.Context, e *sessionEntry, conv string, done bool) {
	if !r.finaliseCard(ctx, e, conv, done) {
		return
	}
	if e.backlogged() {
		r.queuePlaceholder(ctx, e, conv)
	}
}

// queuePlaceholder posts a fresh placeholder for a turn that is queued behind
// one whose card has just been finalised. It carries no clock yet: the daemon
// taking the message up is what starts it (resumeProgress).
func (r *Router) queuePlaceholder(ctx context.Context, e *sessionEntry, conv string) {
	ref, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: workingText, Kind: chat.KindProgress})
	r.metrics.recordReply(err)
	if err != nil {
		r.logf.Warnf("progress %s: post for the queued turn: %v", conv, err)
		return
	}
	if !e.strandProgress(ref, time.Now()) {
		if derr := r.out.Delete(ctx, ref); derr != nil {
			r.logf.Warnf("progress %s: drop unneeded placeholder: %v", conv, derr)
		}
	}
}

// finalLine is what a retired placeholder says where its clock was: "✔ Done ·
// 12s", or "✖ Stopped · 12s" for a turn that did not finish. start is when the
// turn began; zero when unknown, and the line carries no time.
func finalLine(start time.Time, done bool) string {
	text := markFailed + " Stopped"
	if done {
		text = markOK + " Done"
	}
	if !start.IsZero() {
		text += " · " + formatElapsed(time.Since(start))
	}
	return text
}

// redrawProgress renders the progress message from the turn's state as it is
// now and edits it, ordered against every other editor (emu). ok is false when
// there is no message to edit — the turn ended, or never had one.
//
// The render is inside the lock, not handed in from outside it. A tick that
// rendered first and edited second could otherwise land after a result's edit
// that rendered later, and put a line the result had ticked off back to
// running until the next edit came along. Rendered under emu, the edits go out
// in order and each shows everything recorded before it.
//
// It is the one place a card is edited while its turn runs, and so where one
// that has grown past what a message should hold rolls over (splitCard).
func (r *Router) redrawProgress(ctx context.Context, e *sessionEntry, conv string, mode ProgressMode) (ok bool, err error) {
	e.emu.Lock()
	defer e.emu.Unlock()
	ref, text, act, ok := e.editRender(mode)
	if !ok {
		return false, nil
	}
	if act != nil && stepLines(act.Steps) > r.cardLineLimit() {
		if split, more := r.splitCard(ctx, e, conv, mode); split {
			// The continuation was posted as the turn stands, so there is
			// nothing left to edit — unless a burst left more than one
			// message's worth, in which case it rolls over again, a bounded
			// number of times: what is still over after that waits for the
			// next redraw.
			for i := 0; more && i < maxRolloversPerRedraw; i++ {
				if split, more = r.splitCard(ctx, e, conv, mode); !split {
					break
				}
			}
			return true, nil
		}
	}
	err = r.out.Update(ctx, ref, chat.Reply{Conversation: conv, Text: text, Kind: chat.KindProgress, Activity: act})
	e.noteEdit(err == nil, r.cardRedrawInterval())
	return true, err
}

// maxRolloversPerRedraw bounds how many further messages one redraw may roll a
// card over into. Each is a post and an edit; a turn that has outrun this many
// has outrun the platform's rate limit too, and is better caught up over the
// next few redraws than in one burst of calls.
const maxRolloversPerRedraw = 4

// requestRedraw asks for the card to be redrawn because a call, a result or a
// subagent frame changed it. Redraws asked for this way are coalesced: the
// first goes out at once, and any that follow inside cardRedrawInterval of the
// last edit become one trailing redraw at the end of it, which renders
// whatever has been recorded by then. So a burst of frames costs one edit per
// interval, and the last state is always drawn.
//
// ctx must outlive the caller's own work — the trailing redraw runs on it
// later — so a subagent poller passes the relay's context, not its own.
func (r *Router) requestRedraw(ctx context.Context, e *sessionEntry, conv string) {
	e.pmu.Lock()
	if e.progressMsg.ID == "" || e.turnStart.IsZero() || e.redrawTimer != nil {
		// Nothing to draw into, or a trailing redraw is already owed and
		// will show this change too.
		e.pmu.Unlock()
		return
	}
	// After a failed edit the interval is the stand-off that failure earned
	// (noteEdit), not the usual one.
	if wait := max(r.cardRedrawInterval(), e.redrawBackoff) - time.Since(e.redrawAt); wait > 0 {
		e.redrawGen++
		gen := e.redrawGen
		e.redrawTimer = time.AfterFunc(wait, func() { r.trailingRedraw(ctx, e, conv, gen) })
		e.pmu.Unlock()
		return
	}
	// Claimed here, under the lock, and not left to the render: a second
	// request arriving before this one has rendered — the relay and a
	// subagent's poller both ask — then finds the interval already running
	// and becomes the trailing redraw, rather than a second edit beside it.
	e.redrawAt = time.Now()
	e.pmu.Unlock()
	r.drawCard(ctx, e, conv)
}

// trailingRedraw is the redraw requestRedraw deferred. gen is the timer's
// own: a turn that has ended or been replaced since reset it, and whatever
// that turn owed is not drawn into the next one's message.
func (r *Router) trailingRedraw(ctx context.Context, e *sessionEntry, conv string, gen uint64) {
	e.pmu.Lock()
	if e.redrawGen != gen || e.redrawTimer == nil {
		e.pmu.Unlock()
		return
	}
	e.redrawTimer = nil
	e.pmu.Unlock()
	if ctx.Err() != nil {
		return
	}
	r.drawCard(ctx, e, conv)
}

// drawCard redraws the progress message for a change to its card. A failed
// edit is logged and healed by the next, which renders the whole card again —
// and the next is asked for here, since the change that wanted this one is
// still undrawn. It comes no sooner than the stand-off the failure earned
// (noteEdit): as with the ticker, a conversation that is being rate limited
// must not be answered with more edits at the same rate. The first edit that
// lands ends the stand-off.
func (r *Router) drawCard(ctx context.Context, e *sessionEntry, conv string) {
	ok, err := r.redrawProgress(ctx, e, conv, r.progressFor(e.channel))
	if !ok || err == nil {
		return
	}
	r.logf.Warnf("progress %s: card: %v", conv, err)
	r.requestRedraw(ctx, e, conv)
}

// cardRedrawInterval is the least time between two coalesced redraws.
func (r *Router) cardRedrawInterval() time.Duration {
	if r.cardRedraw > 0 {
		return r.cardRedraw
	}
	return defaultCardRedraw
}

// cardLineLimit is how many lines a card holds before it rolls over.
func (r *Router) cardLineLimit() int {
	if r.cardMaxLines > 0 {
		return r.cardMaxLines
	}
	return defaultCardMaxLines
}

// splitCard freezes the card where it is, as a finished stretch of the turn,
// and continues the turn in a fresh message below, with the same turn clock.
// It is how a card rolls over when it has grown too long, and how it makes
// way for narration: either way the calls already drawn stay where they were
// drawn, and later ones go to the new message.
//
// What is frozen is every call that has returned. A call still running moves
// to the new message, so the frozen one shows nothing as running and nothing a
// result may still arrive for is forgotten; a delegation still in flight
// heads both (turnCard.planFreeze). split is false when there is nothing to
// freeze, or the platform refused a step — then nothing has changed, and the
// caller carries on with the card as it was. more says the message the turn
// continues in is itself over the line limit: one freeze leaves behind no more
// than a message should hold, and a burst can be more than that.
//
// Post first, then freeze, then swap, as reanchorProgress orders its own move
// and for its reason: the failure this order has is a message posted and taken
// back, where the other's is a turn left with a frozen card and no live one.
//
// The caller holds emu.
func (r *Router) splitCard(ctx context.Context, e *sessionEntry, conv string, mode ProgressMode) (split, more bool) {
	limit := r.cardLineLimit()
	old, plan, frozen, next, unused, ok := e.planSplit(conv, mode, limit)
	if !ok {
		return false, false
	}
	fresh, err := r.out.Send(ctx, next)
	r.metrics.recordReply(err)
	if err != nil {
		r.logf.Warnf("progress %s: continue card: %v", conv, err)
		return false, false
	}
	if err := r.out.Update(ctx, old, frozen); err != nil {
		r.logf.Warnf("progress %s: freeze card: %v", conv, err)
		r.dropCard(ctx, conv, fresh, unused, "drop unused card")
		return false, false
	}
	if !e.commitSplit(old, fresh, plan) {
		// A later turn claimed the slot while this was in the air. It took
		// the whole record with it and finalises the old message itself
		// (retireStale); ours is nobody's.
		r.dropCard(ctx, conv, fresh, unused, "drop orphaned card")
		return true, false
	}
	// The progress message is a different one now, and the snapshot names
	// it: left unsaid, a crash before the next thing that dirties the state
	// would have the next process finalise the frozen message as the card in
	// flight, and never touch the live one (caught in review).
	r.markDirty()
	return true, next.Activity != nil && stepLines(next.Activity.Steps) > limit
}

// planSplit renders both halves of a split without changing anything: the
// message to freeze and what it will say, and the reply the turn continues in.
// unused is what that reply's message is edited into if it turns out not to be
// wanted and cannot be deleted (dropCard); nil when it carries no card. ok is
// false with no card, or one with nothing to freeze.
func (e *sessionEntry) planSplit(conv string, mode ProgressMode, limit int) (old chat.MessageRef, plan *cardFreeze, frozen, next chat.Reply, unused *chat.Activity, ok bool) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.progressMsg.ID == "" || e.turnStart.IsZero() || !e.card.holds() {
		return chat.MessageRef{}, nil, chat.Reply{}, chat.Reply{}, nil, false
	}
	elapsed := time.Since(e.turnStart)
	plan, ok = e.card.planFreeze(elapsed, limit)
	if !ok {
		return chat.MessageRef{}, nil, chat.Reply{}, chat.Reply{}, nil, false
	}
	frozen = chat.Reply{Conversation: conv, Text: cardText(plan.act), Kind: chat.KindActivity, Activity: plan.act}
	next = chat.Reply{
		Conversation: conv,
		Text:         subTickText(elapsed, e.tools, e.step, e.subAgent, e.subTools, e.subCall),
		Kind:         chat.KindProgress,
	}
	if rest := e.card.rest(plan); rest.holds() {
		next.Activity = rest.live(elapsed, mode == ProgressStream)
		unused = rest.segment(elapsed)
	} else if mode != ProgressStatus {
		// No call carried over: the new message is a plain clock until the
		// next one, in the words its mode gives a clock.
		next.Text = tickText(elapsed, nil, 0)
	}
	return e.progressMsg, plan, frozen, next, unused, true
}

// dropCard deletes a progress message that is no longer wanted: one posted
// for a move or a split that then did not happen, or the one a move left
// behind. finished is what the message is edited into if it cannot be
// deleted; nil for a message that is a plain clock.
//
// The edit is what matters. A clock that cannot be deleted stops where it is,
// as it always has, and is litter. A card that cannot be deleted was posted
// running and nothing will ever edit it again, so it would spin for good —
// the one thing a card must never do. So it is finished instead: its lines
// read as left behind, which is what they are, since the turn carries on in
// another message. If that edit fails too the log names the message, so an
// operator can find it, and the card is owed the edit (owe): it is tried again
// as the process exits, and by the next one.
func (r *Router) dropCard(ctx context.Context, conv string, ref chat.MessageRef, finished *chat.Activity, what string) {
	derr := r.out.Delete(ctx, ref)
	if derr == nil {
		return
	}
	r.logf.Warnf("progress %s: %s: %v", conv, what, derr)
	if finished == nil {
		return
	}
	err := r.out.Update(ctx, ref, chat.Reply{Conversation: conv, Text: cardText(finished), Kind: chat.KindActivity, Activity: finished})
	if err == nil {
		r.logf.Warnf("progress %s: message %s could not be deleted; finished it in place instead", conv, ref.ID)
		return
	}
	r.logf.Warnf("progress %s: message %s could be neither deleted (%v) nor finished (%v); it is left drawn as running",
		conv, ref.ID, derr, err)
	if retryableEdit(err) {
		r.owe(conv, ref, finished, time.Now())
	}
}

// commitSplit makes a split the platform has taken the turn's state: the
// frozen calls leave the record and fresh becomes the progress message.
// Compare-and-swap on old, as replaceProgress is and for its reason.
func (e *sessionEntry) commitSplit(old, fresh chat.MessageRef, plan *cardFreeze) bool {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if old.ID == "" || e.progressMsg != old {
		return false
	}
	e.card.commitFreeze(plan)
	e.progressMsg = fresh
	e.redrawAt = time.Now() // fresh was posted as the turn stands: that is a draw
	return true
}

// drawsActivity reports whether the adapter renders Reply.Activity.
func (r *Router) drawsActivity() bool {
	d, ok := r.out.(chat.DrawsActivity)
	return ok && d.DrawsActivity()
}

// cardMode reports whether a turn's tool activity in mode is drawn as a card:
// one message, edited in place for the whole turn and kept when it ends (see
// card.go). It is the one test for that — everything a card does differently
// asks here — so which modes draw a card is this line and nothing else.
//
// It decides how a turn starts. Once a turn has a card it keeps it: see
// carded.
func (r *Router) cardMode(mode ProgressMode) bool {
	return (mode == ProgressStatus || mode == ProgressStream) && r.drawsActivity()
}

// carded reports whether e's turn in flight keeps its calls as a card, which
// is what every path that feeds or draws one asks. A turn starts as cardMode
// says. A turn that has recorded a call on its card stays a card turn to its
// end, whatever cardMode says later: the adapter may have had a card refused
// since (DrawsActivity turns false), or the channel's mode may have been
// changed, and a record that stopped being fed half way would still be kept
// when the turn ended, saying "Done · 1 call" over a turn that made three
// (caught in review). So its calls, results and subagent frames go on being
// recorded, nothing is posted beside it, and where the adapter no longer
// draws the card the text that goes with it is what shows. The next turn
// starts from cardMode again.
func (r *Router) carded(e *sessionEntry, mode ProgressMode) bool {
	return e.cardTurn() || r.cardMode(mode)
}

// retireMessage deletes a placeholder, or finalises it where deleting leaves a
// trace. start is when its turn began; zero when unknown.
func (r *Router) retireMessage(ctx context.Context, conv string, ref chat.MessageRef, start time.Time, done bool) {
	if t, ok := r.out.(chat.DeleteLeavesTrace); ok && t.DeleteLeavesTrace() {
		if err := r.out.Update(ctx, ref, chat.Reply{Conversation: conv, Text: finalLine(start, done), Kind: chat.KindActivity}); err != nil {
			r.logf.Warnf("progress %s: finalise: %v", conv, err)
		}
		return
	}
	if err := r.out.Delete(ctx, ref); err != nil {
		r.logf.Warnf("progress %s: clear: %v", conv, err)
	}
}

// deletesCleanly reports whether deleting a message leaves nothing behind.
func (r *Router) deletesCleanly() bool {
	t, ok := r.out.(chat.DeleteLeavesTrace)
	return !ok || !t.DeleteLeavesTrace()
}

// deliverText relays a model turn's text: retire the transient progress message
// (a no-op unless indicator/status mode left one) and post the turn as its own
// message. Shared by every mode, so a long answer is chunked by the adapter
// rather than squeezed into an in-place edit. The turn's accounting is taken
// (and cleared) here whether or not the footer is enabled, so a session that
// runs with it off never accumulates stale numbers.
//
// Not every relayed text is the answer. A model turn that narrates before
// reaching for a tool ("let me check the logs…") arrives as a completed,
// non-partial agent event indistinguishable from a final answer except for one
// thing: turn-complete has not landed yet. Treating narration as the end of the
// turn is the first half of #42 — it deleted the placeholder, stopped the
// clock and disarmed the stream-lost notice, so the rest of a turn that had
// only just begun ran with no sign it was running at all. So the two are told
// apart, and narration re-anchors the placeholder below itself instead of
// retiring it.
//
// Two conditions have to hold before text is read as anything but the end of
// the turn, and both are about whether the absence of a turn-complete means
// anything. The daemon must say it sends the frame — an older one gives no way
// to draw the distinction at all. And the turn must have spent its whole life
// on one connection: lifecycle frames carry no seq, so a boundary emitted
// during a stream outage is gone for good while the answer behind it is
// replayed from seq, and believing that absence would strand a live clock under
// a delivered answer. Failing either, every text ends the turn, as it did
// before #42.
func (r *Router) deliverText(ctx context.Context, e *sessionEntry, conv, text string, seq int64, hasCalls bool) {
	var (
		usage   *chat.Usage
		settled bool
	)
	if !hasCalls {
		usage, settled = e.takeUsage()
	}
	// Two questions, and #42 is what happens when they are answered as one.
	//
	// ended is about the turn: has this text concluded it, or is the turn still
	// running and this only something it said on the way? That is the first
	// case, and the paragraphs above are its reasoning. When the same frame
	// also carries tool calls (hasCalls), the model has explicitly asked for a
	// tool execution in this step, so the text is narration regardless of
	// signalsEnd or watchedWhole.
	//
	// final is about the thread: is this its last word for now? A turn can be
	// genuinely over — turn-complete banked, answer in hand — while a message
	// queued behind it waits for a turn of its own. The progress placeholder is
	// one slot for the whole conversation, so retiring it on an answer that has
	// a later turn behind it hands that turn a thread with no placeholder and
	// no clock. That is the second case. Backlogged text keeps the placeholder,
	// re-anchoring it below the answer exactly as narration does.
	// A text arriving when no turn is in flight and carrying no tool calls
	// cannot be narration either: the boundary has already ended the turn (a
	// boundary-first answer that lost its usage to a quiet stream), or no human
	// turn began it. Reading an answerless turn's late text as narration would
	// re-anchor a stopped clock below it and leave it there. When hasCalls is
	// true, though, this frame explicitly starts tool work — even on an
	// autonomous or adopted session where turnInFlight is false — so it has not
	// ended the turn, while still avoiding placeholder re-anchoring unless a
	// turn is in flight or backlogged.
	ended := !hasCalls && (!e.turnInFlight() || settled || !e.signalsEnd.Load() || !e.watchedWhole())
	final := ended && !e.backlogged()
	if ended {
		// The counter is about turns, not placeholders: a completed turn was
		// delivered to the thread whether or not another is queued behind it.
		r.metrics.recordTurnRelayed()
		e.stopAllSubagents()
	}
	if final {
		// An answer with nothing behind it concludes the wait: whatever the
		// stream does next, nobody is left waiting on it.
		e.endTurn()
		r.clearProgress(ctx, e, conv)
	}
	// An answer with a message queued behind it keeps the placeholder for the
	// queued turn. Not a card that holds calls: that is this turn's record,
	// finalised where it is before the answer goes out under it, and the
	// queued turn gets a fresh placeholder below the answer.
	handed := ended && !final && r.finaliseCard(ctx, e, conv, true)
	if !r.settingsFor(e.channel).showUsage {
		usage = nil
	}
	if settled {
		e.dropUnfooted() // this text is the answer, footed or not
	}
	ref, err := r.out.Send(ctx, chat.Reply{
		Conversation: conv,
		Text:         text,
		Agent:        r.identityOf(e),
		Usage:        usage,
		Key:          answerKey(e.sess, seq),
		Verify:       seq <= e.verifyThrough.Load(),
	})
	r.metrics.recordReply(err)
	if err != nil {
		// A failed post should not tear down the stream; log and keep relaying.
		r.logf.Errorf("relay %s: send: %v", conv, err)
	}
	if final || (!e.turnInFlight() && !e.backlogged()) {
		return
	}
	if handed {
		// Whether or not the answer landed: the card is no longer a
		// placeholder, so without this the queued turn has none.
		r.queuePlaceholder(ctx, e, conv)
	}
	if e.turnInFlight() {
		// The turn was left running on the strength of a boundary that has not
		// arrived; make sure something ends it if it never does.
		r.armTurnBackstop(ctx, e, conv)
	}
	// The rest only once the narration is actually in the thread. A re-anchor
	// exists to put the clock back underneath something; if nothing was posted
	// there is nothing to get under, and moving the placeholder would spend two
	// API calls to change nothing — or, on a failed delete, leave a duplicate.
	if err != nil {
		return
	}
	// Recorded before the re-anchor, not after: what it buys is the boundary's
	// permission to delete the placeholder instead of freezing it, and the
	// boundary can land while the re-anchor's own Send is still in the air.
	e.noteSpoke()
	if !ended && !hasCalls && ref.ID != "" {
		e.noteUnfooted(ref, text)
	}
	if ended || hasCalls {
		// A backlogged answer (#42) or narration paired with tool calls in the
		// same frame: the clock belongs below this text now, with nothing to
		// wait for. A placeholder just posted for the queued turn is there
		// already.
		if !handed {
			r.reanchorProgress(ctx, e, conv)
		}
		return
	}
	r.scheduleReanchor(ctx, e, conv)
}

// defaultReanchorDelay comfortably covers the gap measured live between an
// answer and the turn-complete behind it (well under a second), and is short
// next to the fifteen-second tick, so real narration still pulls the clock
// below itself promptly.
const defaultReanchorDelay = 1500 * time.Millisecond

// scheduleReanchor re-anchors the placeholder after reanchorDelay, if it is
// still the placeholder that was there when the narration landed. By then the
// text may have turned out to be the answer, and the boundary retired the
// placeholder: nothing to move. Guarded on the placeholder rather than on the
// turn, because a queued message taken up in the meantime (#42) starts a new
// turn on this same placeholder, and that is exactly the clock that belongs
// below the text.
func (r *Router) scheduleReanchor(ctx context.Context, e *sessionEntry, conv string) {
	if r.reanchorDelay <= 0 {
		r.reanchorProgress(ctx, e, conv)
		return
	}
	held := e.currentProgress()
	if held.ID == "" {
		return
	}
	gen := e.reanchorGen.Add(1)
	time.AfterFunc(r.reanchorDelay, func() {
		if ctx.Err() != nil || !e.reanchorGen.CompareAndSwap(gen, 0) || e.currentProgress().ID != held.ID {
			return
		}
		r.reanchorProgress(ctx, e, conv)
	})
}

// flushReanchor runs a pending delayed re-anchor now. Called when a queued
// message is taken up (#42): the next turn has started on this placeholder,
// so the text it was waiting behind cannot have been the end of anything, and
// the clock belongs below it before the previous turn's boundary arrives and
// retires a placeholder still sitting above spoken text.
func (r *Router) flushReanchor(ctx context.Context, e *sessionEntry, conv string) {
	if gen := e.reanchorGen.Load(); gen != 0 && e.reanchorGen.CompareAndSwap(gen, 0) {
		r.reanchorProgress(ctx, e, conv)
	}
}

// currentProgress returns the conversation's progress placeholder, or a zero
// ref when there is none.
func (e *sessionEntry) currentProgress() chat.MessageRef {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	return e.progressMsg
}

// reanchorProgress moves the turn's placeholder to the bottom of the thread
// after something else has been posted below it, so the clock stays where a
// reader is looking. The turn is untouched: same start time, same tool trail,
// same ticker still running against it.
//
// Post first, then swap, then delete. The other order — delete then post —
// leaves the turn with no placeholder at all if the post fails, and the failure
// mode of this one is a duplicate for as long as the delete takes rather than a
// clock that vanishes mid-turn. On a platform that cannot delete at all the
// duplicate is permanent, and one per narration rather than one per turn: both
// shipped adapters implement Delete, so that is a note for the next one.
func (r *Router) reanchorProgress(ctx context.Context, e *sessionEntry, conv string) {
	mode := r.progressFor(e.channel)
	if !mode.clocked() {
		return // off never posted one
	}
	if r.carded(e, mode) {
		// A card that holds calls is not moved: it is frozen where it is, as
		// the stretch of the turn before whatever was just posted, and the
		// turn carries on in a fresh card below with the same clock. Moving
		// it would take calls a reader has seen out of where they saw them.
		// One with nothing finished to leave behind moves whole, below.
		e.emu.Lock()
		split, _ := r.splitCard(ctx, e, conv, mode)
		e.emu.Unlock()
		if split {
			return
		}
	}
	if !r.deletesCleanly() {
		// Moving the clock is a post and a delete, and where a delete leaves
		// a trace every move would leave one in the thread. It stays put.
		return
	}
	old, text, act, posted, ok := e.moveRender(mode)
	if !ok {
		return // no placeholder outstanding: nothing to move
	}
	fresh, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: text, Kind: chat.KindProgress, Activity: act})
	r.metrics.recordReply(err)
	if err != nil {
		r.logf.Warnf("progress %s: re-anchor: %v", conv, err)
		return // the old one is still live and still ticking
	}
	// What the old message is, is asked at the swap and not taken from the
	// render above: the turn's first call can have landed while the new one
	// was being posted, and drawn the clock that was rendered as a running
	// card (moveProgress).
	left, ok := e.moveProgress(old, fresh)
	if !ok {
		// A later turn claimed the slot while this was in the air. Ours is
		// nobody's, so take it back out rather than leave an orphan ticking.
		r.dropCard(ctx, conv, fresh, posted, "drop orphaned re-anchor")
		return
	}
	r.markDirty() // the snapshot names the progress message, and it has moved
	if left == nil {
		r.dropCard(ctx, conv, old, nil, "clear re-anchored")
		return
	}
	// The old message is a card, and until a moment ago it was the one being
	// edited. Under emu, so that a redraw which rendered into it before the
	// swap has landed by now: were it to land after the edit that finishes a
	// card that could not be deleted, it would set it spinning again.
	e.emu.Lock()
	r.dropCard(ctx, conv, old, left, "clear re-anchored")
	e.emu.Unlock()
}

// postActivity surfaces the tools the agent is running (stream and status
// modes). Status mode edits the managed status message in place so the whole
// turn stays one message; stream mode — and status mode with no message left
// to edit — posts a standalone notice.
//
// The in-place edit renders the same line the ticker does — same text, same
// KindProgress — rather than a bare tool notice: the two write to one message,
// and if they disagreed the wording and the card icon would flip back and
// forth every fifteen seconds. A standalone notice is still KindActivity,
// where the distinction earns its icon.
//
// In card mode both modes go to the one message: the calls are recorded on the
// turn's card and the card is redrawn, and stream posts no notice of its own —
// the placeholder is the card. A turn with no placeholder has no card, and
// falls through to the notice either mode would have posted without one.
func (r *Router) postActivity(ctx context.Context, e *sessionEntry, conv string, mode ProgressMode, calls []daemon.ToolCall) {
	card := r.carded(e, mode)
	if card || mode == ProgressStatus {
		if e.noteActivity(calls, card, mode == ProgressStream) {
			if card {
				// A turn whose ticker gave up before its first call gets
				// another now that there is a card for it to settle.
				if start, stop, ok := e.reviveTicker(); ok && r.tickInterval > 0 {
					go r.tick(ctx, e, conv, start, stop)
				}
				r.requestRedraw(ctx, e, conv)
				return
			}
			if _, err := r.redrawProgress(ctx, e, conv, mode); err != nil {
				r.logf.Warnf("relay %s: status activity: %v", conv, err)
			}
			return
		}
	}
	detail := mode == ProgressStream
	// Read before the Send, not after: the notice belongs to the turn that was
	// running when it was posted, and Send is a network call the next turn can
	// start underneath.
	turn := e.turnSeq.Load()
	seen := time.Now() // before the Send, which is the gateway's time and not the call's
	ref, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: activityText(calls, nil, detail), Kind: chat.KindActivity})
	r.metrics.recordReply(err)
	if err != nil {
		r.logf.Warnf("relay %s: activity: %v", conv, err)
		return
	}
	if detail {
		// Only stream mode's notices are worth remembering: they are the only
		// ones that carry per-call state a result can tick off.
		e.noteToolCalls(ref, calls, turn, detail, seen)
	}
}

// defaultFootDelay is how long a turn-complete waits for an answer behind it
// before deciding the text ahead of it was the answer. A boundary-first daemon
// sends the answer within moments of the boundary; a model call for anything
// after it — core-agent's auto_continue turn included — takes longer than this.
const defaultFootDelay = 1500 * time.Millisecond

// scheduleFoot is called at turn-complete, after the boundary's own handling.
// Whether the text already posted this turn was its answer depends on the
// daemon's order, which the boundary alone cannot tell: a boundary-first
// daemon (core-agent before 2.10) sends the answer after it — even after the
// next turn's "streaming", measured — and an answer-first one (2.10) already
// sent it. So the next agent event decides (resolveFoot): a model text is the
// answer, anything else means the pending text was. If the stream goes quiet
// for footDelay instead, footLastAnswer decides on the text it has.
func (r *Router) scheduleFoot(ctx context.Context, e *sessionEntry, conv string) {
	fire := func() {
		if ctx.Err() == nil {
			r.footLastAnswer(ctx, e, conv)
		}
	}
	// Never zero: deciding at the boundary itself would clear the usage a
	// boundary-first daemon's answer is a moment from claiming.
	d := r.footDelay
	if d <= 0 {
		d = defaultFootDelay
	}
	e.armFoot(d, fire)
}

// resolveFoot is called for every agent event while a boundary is pending. A
// final model text claims the usage itself (claimFootByText, then deliverText
// takes it settled); any other event — a user-role message such as an inbox
// echo or auto_continue's empty prompt, a tool call, a tool result — means no
// answer is coming after the boundary, so the pending text is settled now. A
// partial chunk decides nothing: its final event follows.
//
// This relies on core-agent echoing a turn's prompt as a user-role agent event
// before any model output of that turn (it writes it to the eventlog first, and
// agent frames keep eventlog order). Without the echo, a next turn's first
// model text arriving inside the quiet window would be read as the previous
// turn's answer.
func (r *Router) resolveFoot(ctx context.Context, e *sessionEntry, conv string, finalText, partial bool) {
	if !e.footIsPending() || partial {
		return
	}
	if finalText {
		e.claimFootByText()
		return
	}
	r.footLastAnswer(ctx, e, conv)
}

// footLastAnswer settles a turn whose answer went out ahead of its boundary:
// the turn is counted as relayed, the usage is consumed — whatever show-usage
// says, so it cannot linger as a settled turn for the next one to inherit —
// and, with show-usage on, the answer is edited in place to carry its footer.
// Only when the text fits one message: an answer split across several is
// edited as its first part, and rewriting that part with the whole text would
// duplicate the rest.
//
// Over-attribution is possible in one corner, as noteTotals documents for the
// totals themselves: a turn whose boundary was lost in a stream outage, followed
// by one that ends with no text, puts both turns' usage on the first's text.
func (r *Router) footLastAnswer(ctx context.Context, e *sessionEntry, conv string) {
	t, u, ok := e.takeUnfootedAnswer()
	if !ok {
		e.clearStaleSettled()
		return
	}
	r.metrics.recordTurnRelayed()
	if !r.settingsFor(e.channel).showUsage || u.Empty() {
		return
	}
	fit, ok := r.out.(chat.TextFitter)
	if !ok || !fit.FitsOneMessage(t.text) {
		return
	}
	err := r.out.Update(ctx, t.ref, chat.Reply{
		Conversation: conv,
		Text:         t.text,
		Agent:        r.identityOf(e), // the edit keeps the answer signed (caught in review)
		Usage: &chat.Usage{
			Model: u.Model, TokensIn: u.TokensIn, TokensOut: u.TokensOut,
			CostUSD: u.CostUSD, Latency: u.Latency,
		},
	})
	if err != nil {
		r.logf.Warnf("relay %s: add usage footer: %v", conv, err)
	}
}

// postToolResults ticks finished calls off the notice that announced them.
// Editing rather than posting is the point — a turn that makes fifteen calls
// would otherwise put thirty messages in the thread, and the second fifteen
// carry one bit each.
//
// A result nothing is waiting for is dropped in silence. That is the normal
// state of every mode but stream, and of a stream that connected mid-turn.
func (r *Router) postToolResults(ctx context.Context, e *sessionEntry, conv string, results []daemon.ToolResult) {
	for _, ed := range e.applyToolResults(results) {
		if err := r.out.Update(ctx, ed.ref, chat.Reply{Conversation: conv, Text: ed.text, Kind: chat.KindActivity}); err != nil {
			// Logged and left filed. The results stay recorded against the
			// notice, so the next result to land on it re-renders the whole
			// thing and carries this verdict in — an edit that failed heals on
			// the next one. Un-filing them would lose the verdict for good:
			// that later render would draw this call as still running.
			r.logf.Warnf("relay %s: tool result: %v", conv, err)
		}
	}
}

// cardToolResults files a frame of results against the turn's card and asks
// for it to be redrawn. A result the card is not waiting for changes nothing
// and edits nothing — except in stream mode, where a notice posted while the
// turn had no card may be: those results go on to postToolResults.
func (r *Router) cardToolResults(ctx context.Context, e *sessionEntry, conv string, mode ProgressMode, results []daemon.ToolResult) {
	changed, rest := e.noteCardResults(results)
	if changed {
		r.requestRedraw(ctx, e, conv)
	}
	if mode == ProgressStream && len(rest) > 0 {
		r.postToolResults(ctx, e, conv, rest)
	}
}

// session returns the conversation's session, creating it (and starting
// its relay goroutine) on first use. The first caller in a thread owns
// the created session; the SSE relay is attributed to that owner.
//
// Unless the conversation is bound. A thread an unattended agent opened
// through the outbound ingress already has a session — the one working the
// incident — and adopting it is the whole point of the binding: it is what
// puts a human's reply in front of the agent that raised the alarm rather than
// in front of a stranger (#38). An adopted session is not created, starts from
// where the binding says the stream had got to, and is relayed under no
// asserted caller: switchboard did not open it and cannot claim to be whoever
// did.
func (r *Router) session(ctx context.Context, conv, channel, caller string) (*sessionEntry, error) {
	return r.sessionAs(ctx, conv, channel, caller, "")
}

// sessionAs is session with the agent chosen rather than defaulted: a new
// conversation opens on agent (which the channel must allow) instead of the
// channel's default (#140, `/agent`). An existing conversation is returned
// as it is, whatever agent it is on; the caller checks.
func (r *Router) sessionAs(ctx context.Context, conv, channel, caller, agent string) (*sessionEntry, error) {
	r.mu.Lock()
	if e, ok := r.sessions[conv]; ok {
		// Touched under the lock the reaper sweeps under, so a message
		// arriving for an idle conversation either finds it still live and
		// marks it busy, or finds it already dormant and revives it below —
		// never a live entry whose relay is about to be stopped (#87).
		e.touch()
		r.mu.Unlock()
		<-e.ready
		return e, e.err
	}
	// A conversation with a recorded session picks it back up rather than
	// opening another (#86): the relay resumes from the last answer delivered,
	// and the session the daemon has been holding is the one that knows the
	// thread. A record that cannot be read is dropped and the conversation
	// starts fresh, which is all that was possible before there were records.
	if rec, ok := r.dormant[conv]; ok {
		if e := r.reviveLocked(ctx, conv, channel, rec); e != nil {
			return e, e.err
		}
		if agent == "" {
			// Starting over keeps the agent the conversation had — one
			// chosen with `agent` included — rather than the channel's
			// default (caught in review).
			agent = rec.Agent
		}
	}
	// This goroutine owns creation; publish a not-yet-ready entry so
	// concurrent turns on the same conversation wait rather than
	// double-create, and release the map lock before the network call.
	b, adopted := r.bindings[conv]
	e := &sessionEntry{ready: make(chan struct{}), channel: channel, adopted: adopted}
	if adopted {
		e.seeded.Store(true)
	}
	set, cs := r.routing(channel) // one snapshot: the agents and what the channel allows agree
	switch {
	case adopted:
		e.agent = set.stored(b.agent)
	case agent != "":
		e.agent, e.err = set.chosenAgent(cs, agent)
	default:
		e.agent, e.err = set.channelAgent(cs)
	}
	if e.err == nil {
		if a, ok := set.lookup(e.agent); ok && a.ax != nil && a.ax.scope == axScopeOwner {
			// Claimed here, under the lock that publishes the entry, so two
			// first messages cannot both own the thread; refused if it is
			// someone else's (a turn racing the early check in Handle).
			if caller == "" {
				e.err = fmt.Errorf("%s is a personal agent, and this message names no caller", a.label())
			} else if !r.claimOwnerLocked(conv, caller) {
				e.err = errNotOwner
			}
		}
	}
	if e.err != nil {
		// No agent this channel may use. Not published: nothing to wait on and
		// nothing to retry until the configuration changes.
		r.mu.Unlock()
		close(e.ready)
		return e, e.err
	}
	r.sessions[conv] = e
	r.mu.Unlock()

	if adopted {
		e.sess = b.sess
		since := r.adoptFrom(ctx, conv, b)
		// Both watermarks, not just the resume point: since asks the daemon not
		// to replay the backlog, and relayed refuses to post it if it arrives
		// anyway. The incident's own transcript is the one thing that must not
		// end up in the thread.
		e.seq.Store(since)
		e.relayed.Store(since)
		e.delivered.Store(since)
		e.noticed.Store(since)
		e.setSubFloor(since)
		e.touch()
		r.metrics.sessionOpened()
		r.startRelay(ctx, conv, e, "")
		close(e.ready)
		r.markDirty()
		r.logf.Infof("session %s: adopted %s from seq %d", conv, sessionRef(b.sess), since)
		return e, nil
	}

	if a, ok := set.lookup(e.agent); ok && a.ax != nil {
		// An ax agent: the conversation's task, started (or woken, or found)
		// before the session it will hold.
		e.task = a.ax.taskName(a.name, r.gatewayID, conv, caller)
		if _, e.err = r.startAXTask(ctx, conv, a, e); e.err != nil {
			r.mu.Lock()
			delete(r.sessions, conv)
			r.mu.Unlock()
			close(e.ready)
			return e, e.err
		}
		e.touch() // a slow wake is not idle time
	}
	start := time.Now()
	e.sess, e.err = r.daemonOf(e).CreateSession(ctx, caller)
	r.metrics.recordDaemon("create", time.Since(start), e.err)
	if e.err != nil {
		// Drop the failed entry so a later turn can retry.
		r.mu.Lock()
		delete(r.sessions, conv)
		r.mu.Unlock()
		close(e.ready)
		return e, e.err
	}
	e.owner = caller
	e.touch()
	r.metrics.sessionOpened()
	r.startRelay(ctx, conv, e, caller)
	close(e.ready)
	r.markDirty()
	return e, nil
}

// reviveLocked re-attaches a dormant conversation: the record becomes a live
// entry and its relay resumes from the last answer delivered. The caller holds
// r.mu. It returns:
//
//   - the live entry, the lock released;
//   - an entry with err set, the lock released: an ax task that would not
//     start, whose record is kept for the next message to try again;
//   - nil, the lock still held: the record is dropped and the caller opens a
//     new session — for a record that cannot be read, an ax agent's record
//     with no task, or an ax task that had to be created afresh and so no
//     longer holds the session.
func (r *Router) reviveLocked(ctx context.Context, conv, channel string, rec sessionRecord) *sessionEntry {
	e, err := entryFromRecord(rec, channel)
	if err != nil {
		delete(r.dormant, conv)
		r.logf.Warnf("session %s: unreadable record (%v); opening a new session", conv, err)
		return nil
	}
	a, ok := r.agents().lookup(e.agent)
	isAX := ok && a.ax != nil
	if rec.TaskDeleted {
		// Its task was cleaned up: start over in a fresh one, and say why.
		delete(r.dormant, conv)
		label := e.agent
		if ok {
			label = a.label()
		}
		go r.surfaceNotice(ctx, conv, taskDeletedNotice(label, rec.TaskDeletedReason))
		return nil
	}
	if isAX && e.task == "" {
		// Recorded while the agent was not an ax agent: no task holds it.
		delete(r.dormant, conv)
		r.logf.Warnf("session %s: recorded before agent %s ran a task per conversation; opening a new session", conv, e.agent)
		return nil
	}
	if !isAX {
		delete(r.dormant, conv)
	}
	// An ax conversation keeps its record until its task answers: waking one
	// can take minutes, and a snapshot taken meanwhile, which skips entries
	// not yet ready, would otherwise lose the conversation (caught in
	// review).
	stale := time.Since(rec.Touched) > r.reviveWindow()
	e.touch()
	r.sessions[conv] = e
	r.mu.Unlock()
	if isAX {
		created, err := r.startAXTask(ctx, conv, a, e)
		r.mu.Lock()
		ours := r.sessions[conv] == e
		switch {
		case err != nil:
			if ours {
				delete(r.sessions, conv)
			}
			r.mu.Unlock()
			e.err = err
			close(e.ready)
			return e
		case created:
			// The task was gone and has been started afresh, so it does not
			// have the session: start over in it now rather than let the
			// first message find out and be lost (caught in review).
			if ours {
				delete(r.sessions, conv)
			}
			delete(r.dormant, conv)
			e.err = errTaskRestarted
			close(e.ready)
			go r.surfaceNotice(ctx, conv, a.label()+" was restarted, so this conversation starts over.")
			return nil
		}
		delete(r.dormant, conv)
		r.mu.Unlock()
		e.touch() // a slow wake is not idle time (caught in review)
	}
	if _, ok := r.agents().lookup(e.agent); rec.Adopted && stale && ok {
		// An adopted session the thread stopped following long ago has been
		// working the whole time; resuming from where the thread left off
		// would replay all of it at once. The same head re-measure adoption
		// makes, for the same reason (adoptFrom).
		since := r.adoptFrom(ctx, conv, binding{sess: e.sess, since: rec.Relayed, agent: e.agent})
		e.seq.Store(since)
		e.relayed.Store(since)
		e.delivered.Store(since)
		e.noticed.Store(max(since, e.noticed.Load()))
		e.setSubFloor(since)
	}
	r.metrics.sessionOpened()
	r.startRelay(ctx, conv, e, e.owner)
	close(e.ready)
	r.markDirty()
	r.logf.Infof("session %s: re-attached %s from seq %d", conv, sessionRef(e.sess), e.seq.Load())
	if rec.Placeholder != "" {
		go func() {
			pctx, cancel := platformContext(ctx)
			defer cancel()
			ref := chat.MessageRef{Conversation: conv, ID: rec.Placeholder}
			// A card is never taken down. One a restart left is finalised
			// by restore, which strips it from the record before any revive
			// sees it; this is for a record that reaches here with one some
			// other way.
			if rec.Card != nil {
				r.finishOwed(ctx, r.owe(conv, ref, rec.Card.interrupted(), time.Now()))
				return
			}
			// Its turn was cut off by the restart: stopped, as far as this
			// placeholder can honestly say.
			r.retireMessage(pctx, conv, ref, time.Time{}, false)
		}()
	}
	return e
}

// liveOrRevived returns the conversation's session if it has one, live or
// dormant, and never opens a new one: ok is false when there is nothing to
// return. What a press needs — a question can only answer to the session that
// asked it.
func (r *Router) liveOrRevived(ctx context.Context, conv, channel string) (*sessionEntry, bool) {
	r.mu.Lock()
	if e, ok := r.sessions[conv]; ok {
		// Under the reaper's lock, as session() does, so a sweep cannot
		// release the entry between here and the press that is about to use
		// it (#87).
		e.touch()
		r.mu.Unlock()
		<-e.ready
		return e, e.err == nil
	}
	if rec, ok := r.dormant[conv]; ok && !rec.TaskDeleted {
		// Not for a conversation whose task was deleted: a press cannot
		// answer anything there, and reviving would use up the record whose
		// notice the next message is owed (caught in review).
		if e := r.reviveLocked(ctx, conv, channel, rec); e != nil {
			return e, e.err == nil // an ax task that would not start
		}
	}
	r.mu.Unlock()
	return nil, false
}

// answerKey is an answer's identity across processes: the session and the seq
// of the event that carried it. The daemon's seq is per session and never
// reused, so the same answer replayed into a new process gets the same key and
// the adapter can recognize the repeat (chat.Reply.Key).
func answerKey(sess daemon.Session, seq int64) string {
	return fmt.Sprintf("%s#%d", sessionRef(sess), seq)
}

// noteAttempt records, durably and before the post, that an answer is about to
// go out. The one synchronous write on the delivery path, and the thing that
// makes a restart precise: without it the next process cannot tell an answer
// that may have been posted from one that certainly was not, and would have to
// check every one or none. Answers are rare enough that a write each is cheap.
// Without --state-dir there is no next process to tell, and nothing to write.
func (r *Router) noteAttempt(e *sessionEntry, seq int64) {
	e.attempted.Store(seq)
	if r.store == nil {
		return
	}
	if err := r.trySaveState(); err != nil {
		// Posted regardless: the record is about recovery, and holding up an
		// answer for it gets the priority backwards. The cost is that a crash
		// right now could repeat this answer unverified.
		r.logf.Errorf("state: record the attempt at %s: %v", answerKey(e.sess, seq), err)
	}
}

// adoptFrom is the seq an adopted session's relay starts from.
//
// The binding carries one already — measured when the bind was made — but the
// relay does not start then. It starts here, on the first human turn in the
// thread, which for an incident feed can be hours later with the agent working
// the whole time. Resuming from the bind would replay every one of those turns
// into the thread at once: the wall of transcript the bind measured a head to
// avoid, moved from "before the bind" to "between the bind and the reply". So
// the head is measured again, at the moment the thread actually starts
// listening.
//
// A probe that fails leaves the bind's own point. It is the wrong direction to
// be wrong in, but the alternative — guessing forward — drops answers, starting
// with the one to the message being handled right now, and this failure at
// least announces itself when the inject that follows fails too.
func (r *Router) adoptFrom(ctx context.Context, conv string, b binding) int64 {
	head, err := r.daemonFor(b.agent).HeadSeq(ctx, b.sess, "")
	if err != nil {
		r.logf.Warnf("session %s: adopting %s: could not read the head (%v); resuming from the bind at seq %d",
			conv, sessionRef(b.sess), err, b.since)
		return b.since
	}
	return max(head, b.since)
}

// startRelay runs the entry's subscription on a context of its own, so the
// entry can be discarded without leaving its stream behind. ctx is the serve
// context (the relay outlives the turn that started it, by design).
//
// Called before close(e.ready), which is what publishes e.stop to everyone
// else: every other reader of the entry waits on that channel, and a discard
// that read e.stop as nil would leave the relay running against a session the
// conversation has already given up on — reconnecting forever, alongside
// whichever relay the next turn starts.
func (r *Router) startRelay(ctx context.Context, conv string, e *sessionEntry, owner string) {
	ctx, cancel := context.WithCancel(ctx)
	e.stop = cancel
	go r.relay(ctx, conv, e, owner)
}

// discard drops a conversation's entry and stops its relay, so the next turn
// there starts over. It leaves the binding alone unless unbind is set: a
// discarded *adopted* entry means the binding named a session the daemon does
// not have, and keeping it would fail the next turn the same way.
//
// A no-op if the conversation has since moved on to a different entry.
func (r *Router) discard(conv string, e *sessionEntry, unbind bool) bool {
	r.mu.Lock()
	if r.sessions[conv] != e {
		r.mu.Unlock()
		return false
	}
	delete(r.sessions, conv)
	if unbind {
		r.unbind(conv)
	}
	r.mu.Unlock()
	if e.stop != nil {
		e.stop()
	}
	e.stopTicker()
	r.metrics.sessionClosed()
	r.markDirty()
	return true
}

// relay holds the session's SSE subscription and posts each completed
// assistant turn back into the conversation. It reconnects with exponential
// backoff when the stream ends — a dropped stream must never silently strand a
// conversation — resuming from the last seq seen so the daemon replays only new
// turns, and skipping any turn already posted so a boundary replay cannot double
// up (#3). It runs until ctx is cancelled.
func (r *Router) relay(ctx context.Context, conv string, e *sessionEntry, owner string) {
	defer e.stopAllSubagents()
	backoff := r.minBackoff
	// The last moment the stream was known to be carrying traffic — what the
	// grace period is measured from. Written only from inside the callback,
	// which Subscribe invokes synchronously on this goroutine, so it needs no
	// lock.
	//
	// Keyed on *any* event rather than on the connection, because the client
	// offers no connect callback, and on any event rather than on a new agent
	// turn, because a turn spent thinking produces neither. The daemon opens
	// every stream with a capabilities frame, so an established connection
	// refreshes this within a round trip; an outage refreshes nothing.
	lastAlive := time.Now()
	for ctx.Err() == nil {
		progressed := false
		// Whether this connection has seen the daemon report a turn running.
		// It gates acting on "idle": the daemon opens every stream with a status
		// snapshot, and on a session between turns that snapshot says idle —
		// which must not retire a placeholder for a turn that was posted a
		// moment ago and has not reached the daemon yet. It is also reset by
		// each turn boundary, so a status-update the daemon sends for some other
		// reason while idle (a model swap, a perm-mode change) cannot be read as
		// the end of a turn that has since started.
		//
		// Per connection rather than per entry, and deliberately the
		// conservative direction: a turn that both starts and ends inside a
		// stream outage comes back to an idle snapshot this flag discards, so
		// the entry stays in flight until something else ends it. That is
		// exactly what a turn-complete lost in the same gap already does today —
		// not fixed here, and not made worse by #42's re-anchor either, which
		// declines to read anything into a missing boundary once the turn has
		// outlived the connection (sessionEntry.watchedWhole). Erring the other
		// way is worse: it
		// retires a live turn's placeholder and disarms the stream-lost notice
		// meant to cover the outage. Telling the two apart needs a per-turn
		// identity, which is #42.
		streaming := false
		err := r.daemonOf(e).Subscribe(ctx, e.sess, owner, e.seq.Load(), func(ev daemon.Event) error {
			lastAlive = time.Now()
			e.quietReset()
			// The two lifecycle events carrying a turn's accounting arrive
			// before the agent event holding its answer, so they are banked on
			// the entry and attached when that answer is delivered. Neither
			// carries a seq, so neither can be deduplicated on a reconnect the
			// way an agent event is — but usage-update reports running totals
			// rather than increments, so a replayed one contributes a zero
			// delta instead of double-counting.
			switch ev.Type {
			case daemon.EventUsage:
				if t, ok := daemon.SessionUsage(ev.Data); ok {
					e.noteTotals(t)
				}
				return nil
			case daemon.EventTurnComplete:
				if !e.seeded.Load() {
					r.markSeeded(conv, e)
				}
				// The boundary is the event, not its contents: a frame that
				// carries nothing legible still ends the turn everywhere the
				// turn's end is consulted.
				if u, ok := daemon.TurnCompleted(ev.Data); ok {
					e.noteTurnComplete(u)
				} else {
					e.markSettled()
					// Not necessarily malformed: TurnCompleted also reports
					// !ok for a frame naming neither a model nor a latency,
					// which costs the footer a field and nothing else.
					r.logf.Warnf("relay %s: turn-complete carried nothing readable: %s", conv, ev.Data)
				}
				// The daemon's own turn boundary, and the only one that
				// arrives when a turn ends without an answer to deliver.
				// Stop the clock but leave the message: if an answer is
				// still coming it lands within moments and retires it, and
				// if none is, a frozen "Working… 2m30s" is at least not a
				// lie that grows. Fires once per conversational turn even
				// when that turn made tool calls (confirmed live), so a
				// tool-using turn is not cut short.
				e.stopTickerAt(true)
				r.retireSpokenPlaceholder(ctx, e, conv)
				// A clock left for the answer to retire merely stops; a card
				// left that way keeps spinning. It is finalised here, and the
				// answer lands under it.
				r.settleCard(ctx, e, conv, true)
				// Nobody is waiting on the daemon any more, whether or not
				// this turn had anything to say. Without this, a turn that
				// ended with no relayable text — interrupted, empty, or an
				// answer deduplicated as a reconnect replay — left the entry
				// marked in flight forever, and the next outage announced a
				// lost turn that had finished hours earlier.
				e.endTurn()
				// Readable or not, the boundary settles the turn; which text it
				// belonged to is decided by what follows it. Armed last, after
				// this handler's own platform calls, so their latency is not
				// counted as the stream going quiet.
				r.scheduleFoot(ctx, e, conv)
				// This turn is accounted for, so the status-update trailing it
				// is not a second boundary. See the turn-error case.
				streaming = false
				return nil
			case daemon.EventTurnError:
				te, ok := daemon.TurnFailed(ev.Data)
				if !ok {
					r.logf.Warnf("relay %s: unreadable turn-error: %s", conv, ev.Data)
					return nil
				}
				if !e.seeded.Load() {
					r.markSeeded(conv, e)
				}
				r.logf.Errorf("relay %s: turn failed: kind=%s code=%s retryable=%t: %s",
					conv, te.Kind, te.Code, te.Retryable, te.Message)
				// The turn is over and produced no answer, so nothing will
				// ever carry what it spent; drop it rather than let it land on
				// the next reply.
				e.resetUsage()
				// Before failTurn, not after: failTurn posts the notice
				// synchronously on this goroutine, and the daemon's trailing
				// status-update is already queued behind it. Leaving the flag
				// armed across that round trip means a follow-up posted while
				// the notice is in the air gets the stale idle as its boundary —
				// placeholder frozen, entry cleared, stream-lost backstop
				// disarmed for a turn that is still owed.
				streaming = false
				r.failTurn(ctx, e, conv, te.Kind, turnErrorNotice(te))
				return nil
			case daemon.EventInbox:
				// Not a turn boundary and not something the thread is told
				// about: the backlog exists only so an answer can tell whether
				// it is this thread's last word (see deliverText). Unreadable
				// frames are logged rather than ignored — the set going stale
				// is what strands a placeholder, so it is worth knowing that
				// the events feeding it are not being understood.
				c, ok := daemon.InboxChanged(ev.Data)
				if !ok {
					r.logf.Warnf("relay %s: unreadable inbox event: %s", conv, ev.Data)
					return nil
				}
				if !e.seeded.Load() {
					r.markSeeded(conv, e)
				}
				e.noteInbox(c)
				if c.Dequeued() {
					r.flushReanchor(ctx, e, conv)
					// A turn has just taken this message up. In the ordinary
					// case that is the turn switchboard injected moments ago,
					// already being timed, and this does nothing. The case it
					// is here for is the one #42's second half leaves behind:
					// a placeholder kept alive past the previous turn's answer
					// for exactly this turn, with a clock stopped by the
					// boundary that ended that answer.
					r.resumeProgress(ctx, e, conv)
				}
				return nil
			case daemon.EventCapabilities:
				c, ok := daemon.StreamOpened(ev.Data)
				if !ok {
					r.logf.Warnf("relay %s: unreadable capabilities frame: %s", conv, ev.Data)
					return nil
				}
				r.noteCapabilities(ctx, conv, e, c)
				return nil
			case daemon.EventStatusUpdate:
				st, ok := daemon.StatusUpdated(ev.Data)
				if !ok {
					r.logf.Warnf("relay %s: unreadable status-update: %s", conv, ev.Data)
					return nil
				}
				// Before anything else reads the frame: what the daemon says
				// it is doing decides whether the conversation may let go of
				// its streams.
				e.agentBusy.Store(!st.Idle())
				if !st.Idle() && !e.seeded.Load() {
					r.markSeeded(conv, e)
				}
				switch {
				case st.Working():
					streaming = true
				case st.Blocked():
					// The daemon has stopped on something only a human at the
					// agent's own console can answer. core-agent defines these
					// two states but does not emit either today (they have
					// declaration sites and no emission sites), so this is a
					// branch against the protocol rather than against observed
					// traffic. It earns its place anyway: the obvious way to
					// write the case above is "not idle means working", and that
					// spelling turns the first daemon to emit awaiting_permission
					// into a turn boundary at exactly the wrong moment. Blocked
					// is not a boundary — the turn is still owed — and not a
					// thread notice either: relaying an approval prompt to a chat
					// caller is its own feature. Logged because the alternative is
					// an operator watching a turn that will never move with no way
					// to learn why.
					r.logf.Warnf("relay %s: daemon is waiting on a human (%s); the turn is parked",
						conv, st.TurnState)
				case st.Idle():
					// An idle daemon has drained its inbox, so anything still
					// in the backlog is the residue of a dequeued lost to a
					// stream outage. See sessionEntry.clearBacklog.
					//
					// Above the streaming gate, and deliberately: the moment the
					// set can actually be stale is the reconnect, and the first
					// frame a fresh connection sees is a snapshot of a session
					// that is already idle — streaming was never set on this
					// connection, so gating the resync on it would skip the only
					// case it exists for and leave a lost dequeued stranding a
					// frozen placeholder for the life of the entry.
					//
					// Not while an answer is owed, though. turn-complete lands
					// before the text it belongs to and endTurn has already run
					// by then, so the daemon can read as idle in between;
					// clearing there would drop the queued id that the answer is
					// about to check and post it as the thread's last word.
					owed := e.awaitingAnswer()
					if !owed {
						e.clearBacklog()
						e.stopAllSubagents()
					}
					if !streaming {
						return nil
					}
					streaming = false
					// Nothing carried what this turn spent — an answer would have
					// taken the bank on its way out — so drop it rather than let
					// it land on the next reply. Same reasoning as turn-error,
					// and skipped in the same window as the resync above, where
					// the figures belong to an answer still in flight.
					if !owed {
						e.resetUsage()
					}
					// The daemon's turn cleanup emits this on both exit paths of
					// a turn that started, where turn-complete fires only when
					// the turn succeeded and turn-error only when it failed in a
					// way the daemon could classify. Anything that ends a turn
					// outside those two — including a turn-error payload this
					// build could not read, logged and walked away from above —
					// would otherwise leave the entry in flight and the clock
					// running until its hour-long backstop.
					//
					// Not every refusal, though: core-agent's cost-ceiling and
					// watchdog pre-flights return before the cleanup is
					// installed and emit no frame at all, so a turn refused
					// there still runs to the backstop. That gap needs a
					// response on the inject side, not here.
					//
					// The same hazard turn-complete has, and no worse now that
					// both boundaries clear the flag on their way out: what is
					// left is a boundary for the turn that just ended landing
					// after the next turn was marked in flight, retiring its
					// ticker early. A per-turn message identity is the fix, and
					// is #42.
					completed := !e.signalsEnd.Load()
					e.stopTickerAt(completed)
					e.endTurn()
					r.retireSpokenPlaceholder(ctx, e, conv)
					// As at turn-complete, a card is not left spinning. But
					// this is a turn that ended with no turn-complete: from a
					// daemon that sends one, that is a turn that did not
					// succeed — interrupted, or failed in a way nothing
					// classified — and its card says stopped. A daemon that
					// sends none ends every turn this way.
					r.settleCard(ctx, e, conv, completed)
				}
				return nil
			}
			if ev.Type != daemon.EventAgent {
				return nil
			}
			reply, ok := daemon.AgentText(ev.Data)
			calls := daemon.ToolCalls(ev.Data)
			hasCalls := len(calls) > 0
			if reply.Seq > 0 && !e.seeded.Load() {
				r.markSeeded(conv, e)
			}
			r.resolveFoot(ctx, e, conv, ok && !reply.Partial && reply.Text != "" && !hasCalls && reply.Seq > e.relayed.Load(), ok && reply.Partial)
			if reply.Seq > e.seq.Load() {
				e.seq.Store(reply.Seq)
				e.touch()
				progressed = true
			}
			// A completed, non-empty model turn is a reply worth relaying
			// (partial chunks are repeated by the final event). Exactly-once: a
			// reconnect resumes from the last seq seen, but skip anything
			// already posted in case a turn straddled the drop. A single frame
			// may carry both narration text and functionCall parts, so when
			// hasCalls is true we still fall through to surface the tool calls.
			if ok && !reply.Partial && reply.Text != "" {
				if reply.Seq > e.relayed.Load() {
					e.relayed.Store(reply.Seq)
					e.touch()
					r.noteAttempt(e, reply.Seq)
					r.deliverText(ctx, e, conv, reply.Text, reply.Seq, hasCalls)
					if ctx.Err() == nil {
						e.delivered.Store(reply.Seq)
						r.markDirty()
					}
				}
				if !hasCalls {
					return nil
				}
			}
			// Tool activity — surfaced in the modes that show progress, and
			// gated by its own seq watermark so a reconnect replay does not
			// repost it. Its own, not the answer's: sharing one would let a
			// tool result raise the bar the answer has to clear. The mode is
			// resolved per event so a mid-session command takes effect on the
			// next turn. Active subagent watches still observe parent
			// ToolResults even if the mode changed mid-turn so their pollers
			// retire promptly.
			mode := r.progressFor(e.channel)
			card := r.carded(e, mode)
			if (mode == ProgressStream || mode == ProgressStatus || card || e.hasSubagentWatches()) && reply.Seq > e.noticed.Load() {
				// Tool results ride the same event, under the "user" role the
				// daemon gives anything the model did not author (#36). Stream
				// mode ticks them off the notice that announced the call. In
				// card mode they are filed against the turn's card instead,
				// status mode's included — which is the only place status
				// shows a result, so for an adapter that draws no card a
				// status turn does not read them: it would be a parse, a
				// watermark advance and an edit that changes nothing. A live
				// subagent watch reads them whatever the mode, since a parent
				// result is what retires its poller. A turn that has a card
				// keeps feeding it whatever the mode or the adapter has
				// become since (carded).
				var results []daemon.ToolResult
				if mode == ProgressStream || card || e.hasSubagentWatches() {
					results = daemon.ToolResults(ev.Data)
				}
				if len(results) > 0 {
					r.finishSubagentsOnResults(ctx, conv, e, results)
					switch {
					case card:
						e.noticed.Store(reply.Seq)
						r.markDirty()
						r.cardToolResults(ctx, e, conv, mode, results)
					case mode == ProgressStream:
						e.noticed.Store(reply.Seq)
						r.markDirty()
						r.postToolResults(ctx, e, conv, results)
					}
				}
				if hasCalls && (mode == ProgressStream || mode == ProgressStatus || card) {
					e.noticed.Store(reply.Seq)
					r.markDirty()
					r.postActivity(ctx, e, conv, mode, calls)
					r.startSubagentWatches(ctx, conv, e, mode, reply.Seq, calls)
				}
			}
			return nil
		})
		if ctx.Err() != nil {
			return // shutting down: not a reconnectable failure
		}
		// A session the daemon no longer has is not a blip either. There is
		// nothing to reconnect to, ever, and a thread left quietly polling for
		// it looks exactly like a thread where nothing has happened yet. Bound
		// or not: a session switchboard opened itself can be lost by the daemon
		// too — one with no resumer, or one deleted — and its entry is just as
		// dead (#88).
		if isMissingSession(err) {
			r.logf.Warnf("relay %s: session %s is gone from the daemon: %v", conv, sessionRef(e.sess), err)
			if r.discard(conv, e, e.adopted) {
				notice := sessionStreamLostNotice(e.sess)
				if e.adopted {
					notice = bindStreamLostNotice(e.sess)
				}
				// On a context of its own: discard has just cancelled ctx,
				// which is this goroutine's, and the notice is the point.
				notify, cancel := context.WithTimeout(context.WithoutCancel(ctx), platformTimeout)
				// Nothing will ever conclude a turn that was waiting on this
				// stream, so take its placeholder down rather than leave a
				// frozen "Working…" above the notice that explains why.
				e.endTurn()
				r.failProgress(notify, e, conv)
				if sendErr := r.surfaceNotice(notify, conv, notice); sendErr != nil {
					r.logf.Errorf("relay %s: surface lost session: %v", conv, sendErr)
				}
				cancel()
			}
			return
		}
		// An agent removed from the registry since this conversation began
		// has no daemon to follow: reconnecting would retry the refusal
		// forever. The thread is told on its next message (Handle).
		if errors.Is(err, errAgentGone) {
			r.logf.Warnf("relay %s: agent %q is no longer registered; not following %s", conv, e.agent, sessionRef(e.sess))
			return
		}
		// The subscription returned: the stream ended or errored. Reset the
		// backoff if this connection made progress (a healthy stream that
		// blipped reconnects fast; only a persistently failing one backs off).
		if progressed {
			backoff = r.minBackoff
		}
		r.metrics.recordReconnect()
		// Past this point any turn still in flight has outlived the connection
		// that was watching it, and the boundary frames it may have emitted in
		// the meantime are unrecoverable — they carry no seq, so the resume
		// replays the answer without them. See deliverText.
		e.streamGen.Add(1)
		r.logf.Warnf("relay %s: stream ended (%v); resuming from seq %d in %s", conv, err, e.seq.Load(), backoff)
		// A stream that has carried nothing past the grace period, with a turn
		// still waiting on it, is the one failure the daemon cannot announce —
		// it is the daemon that went away. Reconnecting continues regardless;
		// this only stops the thread waiting in silence, and fires once because
		// failTurn ends the turn.
		if down := time.Since(lastAlive); down > r.streamGrace && e.turnInFlight() {
			r.logf.Errorf("relay %s: no stream for %s with a turn in flight; telling the thread", conv, formatElapsed(down))
			r.failTurn(ctx, e, conv, kindStreamLost, errNoticeStreamLost)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if !progressed {
			backoff = min(backoff*2, r.maxBackoff)
		}
	}
}
