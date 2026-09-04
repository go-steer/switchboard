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
	"unicode/utf8"

	"github.com/go-steer/switchboard/internal/logging"
	"github.com/go-steer/switchboard/pkg/approval"
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

// progressModeNames is progressModes as plain strings — the form both the
// capability and the message text want.
func progressModeNames() []string {
	names := make([]string, len(progressModes))
	for i, m := range progressModes {
		names[i] = string(m)
	}
	return names
}

// workingText is the initial progress message posted on wake under the
// indicator and status modes while a turn is in flight. It is always a
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
		quoted[i] = "`" + t + "`"
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
}

// groupCalls collapses adjacent identical calls, where identical means the
// reader could not tell them apart anyway: same name, same summary, same
// verdict. Calls that differ in any of those stay separate lines — the point of
// the argument summary is that three concurrent shells become three legible
// lines rather than one count.
func groupCalls(calls []daemon.ToolCall, res []*daemon.ToolResult) []toolGroup {
	var groups []toolGroup
	for i, c := range calls {
		var r *daemon.ToolResult
		if i < len(res) {
			r = res[i]
		}
		if n := len(groups); n > 0 {
			if prev := groups[n-1]; prev.call.Name == c.Name && prev.call.Arg == c.Arg && sameVerdict(prev.res, r) {
				groups[n-1].n++
				continue
			}
		}
		groups = append(groups, toolGroup{call: c, res: r, n: 1})
	}
	return groups
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

// toolIcon is the state of one group at a glance: still running, done, failed.
func toolIcon(r *daemon.ToolResult) string {
	switch {
	case r == nil:
		return "🔧"
	case r.Failed:
		return "❌"
	}
	return "✅"
}

// toolLine renders one group: icon, tool, how many of it, why it failed, and —
// in stream mode only — what it was called with. verb puts "Running"/"Ran"
// after the icon, for a notice that is one line and has no header to carry it.
func toolLine(g toolGroup, detail, verb bool) string {
	line := toolIcon(g.res)
	if verb {
		if g.res == nil {
			line += " Running"
		} else {
			line += " Ran"
		}
	}
	line += " `" + g.call.Name + "`"
	if g.n > 1 {
		line += " ×" + strconv.Itoa(g.n)
	}
	if g.res != nil && g.res.Failed && g.res.Detail != "" {
		line += " (" + g.res.Detail + ")"
	}
	if detail && g.call.Arg != "" {
		line += " — " + g.call.Arg
	}
	return line
}

// activityText renders a standalone tool-activity notice — stream mode, and
// status mode with no message left to edit. res is parallel to calls and holds
// each call's result once it has landed, so the same function renders the
// notice when it is posted and again as each result ticks a line off:
//
//	🔧 Running `bash` — kubectl get pods -A          (stream, one call)
//	✅ Ran `bash` — kubectl get pods -A              (…once it finishes)
//	🔧 Running 3 tools                               (stream, a parallel frame)
//	• ✅ `bash` — kubectl get pods -A
//	• ❌ `bash` (exit 2) — kubectl get ns --context nope
//	• 🔧 `bash` — sleep 30
//	🔧 Running `bash` ×3                             (status: names, no arguments)
//
// detail is the mode gate. Only stream carries argument summaries: status edits
// one message in place and wants a short line, and a reader who chose indicator
// or status did not ask to see what the agent is running things with.
func activityText(calls []daemon.ToolCall, res []*daemon.ToolResult, detail bool) string {
	if !detail {
		// The terse shape, unchanged but for the count: names, comma-joined,
		// with runs of the same name collapsed rather than repeated.
		var parts []string
		for _, g := range groupCalls(stripArgs(calls), nil) {
			part := "`" + g.call.Name + "`"
			if g.n > 1 {
				part += " ×" + strconv.Itoa(g.n)
			}
			parts = append(parts, part)
		}
		return "🔧 Running " + strings.Join(parts, ", ")
	}
	groups := groupCalls(calls, res)
	if len(groups) == 1 {
		return toolLine(groups[0], true, true)
	}
	lines := make([]string, 0, len(groups)+1)
	lines = append(lines, activityHeader(groups))
	for _, g := range groups {
		lines = append(lines, "• "+toolLine(g, true, false))
	}
	return strings.Join(lines, "\n")
}

// activityHeader summarises a multi-call notice in its first line, so a reader
// scrolling past sees the state of the frame without reading the bullets.
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
	head := "🔧 Running " + strconv.Itoa(total) + " tools"
	if done == total {
		head = "✅ Ran " + strconv.Itoa(total) + " tools"
		if failed > 0 {
			head = "❌ Ran " + strconv.Itoa(total) + " tools (" + strconv.Itoa(failed) + " failed)"
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
	client  *daemon.Client
	out     sender
	metrics *metrics
	logf    logging.Logf

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

	mu       sync.Mutex
	sessions map[string]*sessionEntry

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

	// omu guards overrides, the per-channel progress-mode overrides set at
	// runtime via chat commands (HandleCommand). This is the narrowest of the
	// three layers settingsFor resolves and the only one reachable at runtime;
	// a channel absent from the map falls back to its byChannel block, or to
	// defaults.
	omu       sync.Mutex
	overrides map[string]ProgressMode

	// approvals relays the daemon's permission prompts into the thread and
	// sends back what someone decided. Nil leaves the feature off for every
	// channel, whatever a config file says — see setApprovals. Set once at
	// startup, before dispatch begins.
	//
	// Whether a given channel uses it is channelSettings.approvals: the client
	// is one connection to one daemon and cannot be per-channel, while the
	// decision to put prompts in front of a particular room can be.
	approvals *approval.Client
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
}

// sessionEntry is a conversation's session plus the state to create it
// exactly once under concurrent inbound turns. ready is closed when
// creation finishes (successfully or not); waiters block on it.
type sessionEntry struct {
	ready chan struct{}
	sess  daemon.Session
	err   error

	// adopted marks a session switchboard did not create but was told about by
	// the outbound ingress (#38). It changes two things: the entry starts at the
	// session's head rather than at zero, and a daemon that has never heard of
	// the session is a broken binding to announce rather than a turn to retry.
	// Set once, at construction, under Router.mu.
	adopted bool

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
	// progress message, which only exists in two of the four progress modes.
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

	// The ticker's state for the turn in flight, under pmu because it renders
	// into that same progressMsg and must not become a second source of truth
	// for it. turnStart is when the turn was handed to the daemon — what the
	// elapsed clock counts from, not the moment the placeholder landed; tools
	// and step are the last tool activity seen, replayed by each tick so the
	// clock does not erase it; tickStop closes the ticker goroutine down.
	turnStart time.Time
	tools     []string
	step      int
	tickStop  chan struct{}

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
	umu     sync.Mutex
	usage   daemon.TurnUsage
	totals  daemon.UsageTotals
	have    bool
	settled bool
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
func (e *sessionEntry) endTurn() bool { return e.inFlight.CompareAndSwap(true, false) }

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
}

// claimAsk reports whether this prompt is one the thread has not been shown
// yet, and records it along with what the thread was shown. See
// sessionEntry.asked.
func (e *sessionEntry) claimAsk(id, text string) bool {
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
	e.asked[id] = &askRecord{text: text}
	return true
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

// takeProgress atomically reads and clears the entry's progress message along
// with the rest of the turn's ticker state, and stops the ticker: the message
// it renders into is about to be deleted.
func (e *sessionEntry) takeProgress() chat.MessageRef {
	e.pmu.Lock()
	ref, stop := e.progressMsg, e.tickStop
	e.progressMsg, e.tickStop = chat.MessageRef{}, nil
	e.turnStart, e.tools, e.step = time.Time{}, nil, 0
	e.pmu.Unlock()
	// Outside the lock, and safe against a concurrent caller: whoever nils
	// tickStop under the lock is the only one holding a channel to close.
	if stop != nil {
		close(stop)
	}
	return ref
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
// ok is false in the two cases that must not be disturbed: no placeholder to
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
	stop = make(chan struct{})
	e.tickStop = stop
	e.turnStart, e.tools, e.step = start, nil, 0
	return stop, true
}

// beginTurn adopts ref as the turn's progress message, dated from start, and
// returns the previous message (for the caller to delete) plus a stop channel
// for the ticker it should now run. Any ticker still running for a previous
// turn is stopped: only the latest message is live.
func (e *sessionEntry) beginTurn(ref chat.MessageRef, start time.Time) (stale chat.MessageRef, stop chan struct{}) {
	e.pmu.Lock()
	stale, prev := e.progressMsg, e.tickStop
	stop = make(chan struct{})
	e.progressMsg, e.tickStop = ref, stop
	e.turnStart, e.tools, e.step = start, nil, 0
	e.pmu.Unlock()
	if prev != nil {
		close(prev)
	}
	return stale, stop
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
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if old.ID == "" || e.progressMsg != old {
		return false
	}
	e.progressMsg = fresh
	return true
}

// noteActivity records the tools the agent just started — so every later tick
// keeps showing them instead of dropping back to a bare clock — and returns
// the message to edit with the line to put in it. ok is false when there is no
// progress message to render into (stream mode, or a status turn whose
// placeholder failed to post), and the caller falls back to a standalone
// notice.
func (e *sessionEntry) noteActivity(tools []string) (ref chat.MessageRef, text string, ok bool) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.progressMsg.ID == "" || e.turnStart.IsZero() {
		return chat.MessageRef{}, "", false
	}
	e.tools, e.step = tools, e.step+1
	return e.progressMsg, tickText(time.Since(e.turnStart), e.tools, e.step), true
}

// activityNote is one posted tool-activity notice: the message it went into,
// the calls it announced, and their results as they land. res is parallel to
// calls, nil where the result has not arrived.
//
// detail records the mode the notice was rendered in, so a re-render on a
// result reproduces it rather than assuming.
type activityNote struct {
	ref    chat.MessageRef
	calls  []daemon.ToolCall
	res    []*daemon.ToolResult
	detail bool
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
// few; the cost of forgetting an older one is a notice that keeps its 🔧 rather
// than a wrong edit, so this is deliberately small.
const noticeMemory = 32

// noteToolCalls records a posted notice so the results can find it later. The
// calls are copied: the notice outlives the frame by up to noticeMemory more of
// them and is read under a different lock, so it owns what it holds.
//
// turn is the turn that was current when the notice was *sent*. A newer one
// having started since means forgetActivity has already run and this note is
// the previous turn's, so it is dropped rather than appended behind the clear.
func (e *sessionEntry) noteToolCalls(ref chat.MessageRef, calls []daemon.ToolCall, turn int64, detail bool) {
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
	})
	if n := len(e.notices) - noticeMemory; n > 0 {
		e.notices = append([]*activityNote(nil), e.notices[n:]...)
	}
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
// visible, bounded, and preferable to leaving every line at 🔧 forever.
//
// The caller must hold amu.
func (e *sessionEntry) resolveTool(r daemon.ToolResult) (*activityNote, bool) {
	if r.ID != "" {
		for i := len(e.notices) - 1; i >= 0; i-- {
			n := e.notices[i]
			for j, c := range n.calls {
				if c.ID == r.ID && n.res[j] == nil {
					res := r
					n.res[j] = &res
					return n, true
				}
			}
		}
		// An id we have never seen does not fall back on the name: it belongs
		// to a call some other notice announced, or to none.
		return nil, false
	}
	for _, n := range e.notices {
		if at := n.pending(r.Name); at != -1 {
			res := r
			n.res[at] = &res
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
	// another's verdict and a second stays at 🔧 for good. The name fallback
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
		n := edits[i].note
		edits[i].text = activityText(n.calls, n.res, n.detail)
	}
	return edits
}

// tickRender composes one tick: the message to edit and the line to put in it.
// ok is false once the turn has ended, which is what stops a tick that fired
// just as the reply landed from resurrecting a deleted placeholder. Indicator
// mode gets the clock alone — naming tools is what status mode is for.
func (e *sessionEntry) tickRender(mode ProgressMode) (ref chat.MessageRef, text string, ok bool) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.progressMsg.ID == "" || e.turnStart.IsZero() {
		return chat.MessageRef{}, "", false
	}
	tools, step := e.tools, e.step
	if mode != ProgressStatus {
		tools, step = nil, 0
	}
	return e.progressMsg, tickText(time.Since(e.turnStart), tools, step), true
}

// NewRouter builds a Router. progress selects long-turn feedback (ProgressOff
// if empty); m may be nil (metrics recording becomes a no-op); logf may be the
// zero value (logging becomes a no-op).
func NewRouter(client *daemon.Client, out sender, progress ProgressMode, m *metrics, logf logging.Logf) *Router {
	if progress == "" {
		progress = ProgressOff
	}
	return &Router{
		client:       client,
		out:          out,
		defaults:     channelSettings{progress: progress},
		metrics:      m,
		logf:         logf,
		minBackoff:   reconnectMinBackoff,
		maxBackoff:   reconnectMaxBackoff,
		streamGrace:  streamLostGrace,
		tickInterval: progressTickInterval,
		tickMaxAge:   progressTickMaxAge,
		sessions:     make(map[string]*sessionEntry),
		bindings:     make(map[string]binding),
		boundTo:      make(map[string]string),
		reserving:    make(map[string]string),
		overrides:    make(map[string]ProgressMode),
	}
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
func (r *Router) settingsFor(channel string) channelSettings {
	s := r.defaults
	if channel == "" {
		return s
	}
	if cs, ok := r.byChannel[channel]; ok {
		s = cs
	}
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
}

// HandleCommand processes a gateway control command and returns a short
// acknowledgment for the adapter to surface to the invoker. It never touches
// the daemon: commands configure the gateway. An unknown or malformed command
// yields a helpful ack rather than an error; the error return is reserved for
// future commands that can fail internally.
func (r *Router) HandleCommand(_ context.Context, cmd chat.Command) (string, error) {
	r.metrics.recordCommand()
	switch cmd.Name {
	case "progress":
		return r.progressCommand(cmd), nil
	case "", "help":
		return commandHelp, nil
	default:
		return fmt.Sprintf("Unknown command %q. %s", cmd.Name, commandHelp), nil
	}
}

// commandHelp is the one-line usage surfaced for an empty, "help", or unknown
// command.
var commandHelp = "Try `progress <" + strings.Join(progressModeNames(), "|") + ">` to set this " +
	"channel's long-turn feedback, or `progress` to see the current mode."

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

	entry, err := r.session(ctx, msg.Conversation, msg.Channel, msg.Caller)
	if err != nil {
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
	if !entry.turnInFlight() && !entry.awaitingAnswer() {
		entry.resetUsage()
	}
	// Post the progress message before injecting: inject starts the turn, so a
	// fast reply would otherwise beat the placeholder into the thread and strand
	// it there ("Working…" below the answer, with nothing left to clear it). A
	// no-op in off and stream modes.
	r.startProgress(ctx, entry, msg.Conversation)
	// Someone is waiting from the moment the message is handed over, not from
	// the moment the daemon acknowledges it: the relay is a separate goroutine
	// and the turn can fail on the stream before Inject's response is read.
	entry.beginTurnInFlight()
	start := time.Now()
	err = r.client.Inject(ctx, entry.sess, msg.Caller, msg.Text)
	r.metrics.recordDaemon("inject", time.Since(start), err)
	if err != nil {
		// The turn never reached the daemon, so nothing on the stream will ever
		// conclude it, and the progress message would linger; undo both here.
		entry.endTurn()
		r.clearProgress(ctx, entry, msg.Conversation)
		if entry.adopted && isMissingSession(err) {
			// The thread was bound to a session the daemon no longer has. Say
			// so, and drop the binding with the entry: the next message here
			// opens a session of its own, which is the right thing to do and
			// the wrong thing to do silently.
			//
			// ERROR, not WARN, though the thread recovers on the next message:
			// this one did not, and the rubric is about the turn. It also has
			// to agree with the adapter, which logs Handle's returned error at
			// ERROR and cannot know this case was already accounted for — two
			// levels for one event is worse than the stricter of the two.
			r.logf.Errorf("handle %s: bound session %s is gone from the daemon: %v",
				msg.Conversation, sessionRef(entry.sess), err)
			// Only the turn that actually dropped the entry says so. Several
			// messages can be in flight in the same thread, and each would
			// otherwise post its own copy of the same notice.
			if r.discard(msg.Conversation, entry, true) {
				if sendErr := r.surfaceNotice(ctx, msg.Conversation, bindLostNotice(entry.sess)); sendErr != nil {
					r.logf.Errorf("handle %s: surface lost binding: %v", msg.Conversation, sendErr)
				}
			}
			return err
		}
		r.surfaceError(ctx, msg.Conversation, err)
		return err
	}
	return nil
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
func (r *Router) failTurn(ctx context.Context, e *sessionEntry, conv, kind, notice string) {
	if !e.claimFailureNotice() {
		return
	}
	e.endTurn()
	queued := e.backlogged()
	if queued {
		e.stopTicker()
	} else {
		r.clearProgress(ctx, e, conv)
	}
	r.metrics.recordTurnFailed(kind)
	if _, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: notice, Kind: chat.KindNotice}); err != nil {
		r.logf.Errorf("relay %s: surface turn failure (%s): %v", conv, kind, err)
		return
	}
	if queued {
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

// startProgress posts the initial progress message for a turn (indicator and
// status modes), records it on the entry so relay can edit or clear it, and
// starts the ticker that keeps its clock running. A no-op in off and stream
// modes. A message still outstanding from a prior turn (a second turn started
// before the first replied) is deleted so only the latest remains. Failures
// are logged, never fatal — a missing progress message must not drop the turn.
func (r *Router) startProgress(ctx context.Context, e *sessionEntry, conv string) {
	if mode := r.progressFor(e.channel); mode != ProgressIndicator && mode != ProgressStatus {
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
	stale, stop := e.beginTurn(ref, start)
	if stale.ID != "" {
		if derr := r.out.Delete(ctx, stale); derr != nil {
			r.logf.Warnf("progress %s: clear stale: %v", conv, derr)
		}
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
			// runs in every mode rather than only the two that have a ticker.
			//
			// The message stays, frozen at the age it reached. Deleting it
			// would leave the thread showing the question and nothing else,
			// which reads as never having been heard; a stopped clock at least
			// says how far the turn got. The next turn in the thread clears it
			// as stale.
			r.logf.Warnf("progress %s: no turn boundary after %s; stopping the clock", conv, formatElapsed(time.Since(start)))
			return
		case <-timer.C:
		}
		ref, text, ok := e.tickRender(r.progressFor(e.channel))
		if !ok {
			return // the turn ended between the tick firing and this read
		}
		if err := r.out.Update(ctx, ref, chat.Reply{Conversation: conv, Text: text, Kind: chat.KindProgress}); err != nil {
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
// alongside the ticker because two of the four progress modes have no ticker,
// and the turn is in flight in all four.
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

// clearProgress deletes and forgets the entry's outstanding progress message,
// if any. Called before a reply is relayed so the transient message gives way
// to the answer. No-op when none is outstanding.
func (r *Router) clearProgress(ctx context.Context, e *sessionEntry, conv string) {
	if ref := e.takeProgress(); ref.ID != "" {
		if err := r.out.Delete(ctx, ref); err != nil {
			r.logf.Warnf("progress %s: clear: %v", conv, err)
		}
	}
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
func (r *Router) deliverText(ctx context.Context, e *sessionEntry, conv, text string) {
	usage, settled := e.takeUsage()
	// Two questions, and #42 is what happens when they are answered as one.
	//
	// ended is about the turn: has this text concluded it, or is the turn still
	// running and this only something it said on the way? That is the first
	// case, and the paragraphs above are its reasoning.
	//
	// final is about the thread: is this its last word for now? A turn can be
	// genuinely over — turn-complete banked, answer in hand — while a message
	// queued behind it waits for a turn of its own. The progress placeholder is
	// one slot for the whole conversation, so retiring it on an answer that has
	// a later turn behind it hands that turn a thread with no placeholder and
	// no clock. That is the second case. Backlogged text keeps the placeholder,
	// re-anchoring it below the answer exactly as narration does.
	ended := settled || !e.signalsEnd.Load() || !e.watchedWhole()
	final := ended && !e.backlogged()
	if ended {
		// The counter is about turns, not placeholders: a completed turn was
		// delivered to the thread whether or not another is queued behind it.
		r.metrics.recordTurnRelayed()
	}
	if final {
		// An answer with nothing behind it concludes the wait: whatever the
		// stream does next, nobody is left waiting on it.
		e.endTurn()
		r.clearProgress(ctx, e, conv)
	}
	if !r.settingsFor(e.channel).showUsage {
		usage = nil
	}
	_, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: text, Usage: usage})
	r.metrics.recordReply(err)
	if err != nil {
		// A failed post should not tear down the stream; log and keep relaying.
		r.logf.Errorf("relay %s: send: %v", conv, err)
	}
	if final {
		return
	}
	// The turn was left running on the strength of a boundary that has not
	// arrived; make sure something ends it if it never does.
	r.armTurnBackstop(ctx, e, conv)
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
	r.reanchorProgress(ctx, e, conv)
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
	if mode != ProgressIndicator && mode != ProgressStatus {
		return // stream and off never posted one
	}
	old, text, ok := e.tickRender(mode)
	if !ok {
		return // no placeholder outstanding: nothing to move
	}
	fresh, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: text, Kind: chat.KindProgress})
	r.metrics.recordReply(err)
	if err != nil {
		r.logf.Warnf("progress %s: re-anchor: %v", conv, err)
		return // the old one is still live and still ticking
	}
	if !e.replaceProgress(old, fresh) {
		// A later turn claimed the slot while this was in the air. Ours is
		// nobody's, so take it back out rather than leave an orphan ticking.
		if derr := r.out.Delete(ctx, fresh); derr != nil {
			r.logf.Warnf("progress %s: drop orphaned re-anchor: %v", conv, derr)
		}
		return
	}
	if derr := r.out.Delete(ctx, old); derr != nil {
		r.logf.Warnf("progress %s: clear re-anchored: %v", conv, derr)
	}
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
func (r *Router) postActivity(ctx context.Context, e *sessionEntry, conv string, mode ProgressMode, calls []daemon.ToolCall) {
	if mode == ProgressStatus {
		if ref, text, ok := e.noteActivity(toolNames(calls)); ok {
			if err := r.out.Update(ctx, ref, chat.Reply{Conversation: conv, Text: text, Kind: chat.KindProgress}); err != nil {
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
	ref, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: activityText(calls, nil, detail), Kind: chat.KindActivity})
	r.metrics.recordReply(err)
	if err != nil {
		r.logf.Warnf("relay %s: activity: %v", conv, err)
		return
	}
	if detail {
		// Only stream mode's notices are worth remembering: they are the only
		// ones that carry per-call state a result can tick off.
		e.noteToolCalls(ref, calls, turn, detail)
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
	r.mu.Lock()
	if e, ok := r.sessions[conv]; ok {
		r.mu.Unlock()
		<-e.ready
		return e, e.err
	}
	// This goroutine owns creation; publish a not-yet-ready entry so
	// concurrent turns on the same conversation wait rather than
	// double-create, and release the map lock before the network call.
	b, adopted := r.bindings[conv]
	e := &sessionEntry{ready: make(chan struct{}), channel: channel, adopted: adopted}
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
		e.noticed.Store(since)
		r.metrics.sessionOpened()
		r.startRelay(ctx, conv, e, "")
		close(e.ready)
		r.logf.Infof("session %s: adopted %s from seq %d", conv, sessionRef(b.sess), since)
		return e, nil
	}

	start := time.Now()
	e.sess, e.err = r.client.CreateSession(ctx, caller)
	r.metrics.recordDaemon("create", time.Since(start), e.err)
	if e.err != nil {
		// Drop the failed entry so a later turn can retry.
		r.mu.Lock()
		delete(r.sessions, conv)
		r.mu.Unlock()
		close(e.ready)
		return e, e.err
	}
	r.metrics.sessionOpened()
	r.startRelay(ctx, conv, e, caller)
	close(e.ready)
	return e, nil
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
	head, err := r.client.HeadSeq(ctx, b.sess, "")
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
	return true
}

// relay holds the session's SSE subscription and posts each completed
// assistant turn back into the conversation. It reconnects with exponential
// backoff when the stream ends — a dropped stream must never silently strand a
// conversation — resuming from the last seq seen so the daemon replays only new
// turns, and skipping any turn already posted so a boundary replay cannot double
// up (#3). It runs until ctx is cancelled.
func (r *Router) relay(ctx context.Context, conv string, e *sessionEntry, owner string) {
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
		err := r.client.Subscribe(ctx, e.sess, owner, e.seq.Load(), func(ev daemon.Event) error {
			lastAlive = time.Now()
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
				e.stopTicker()
				r.retireSpokenPlaceholder(ctx, e, conv)
				// Nobody is waiting on the daemon any more, whether or not
				// this turn had anything to say. Without this, a turn that
				// ended with no relayable text — interrupted, empty, or an
				// answer deduplicated as a reconnect replay — left the entry
				// marked in flight forever, and the next outage announced a
				// lost turn that had finished hours earlier.
				e.endTurn()
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
				e.noteInbox(c)
				if c.Dequeued() {
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
					e.stopTicker()
					e.endTurn()
					r.retireSpokenPlaceholder(ctx, e, conv)
				}
				return nil
			}
			if ev.Type != daemon.EventAgent {
				return nil
			}
			reply, ok := daemon.AgentText(ev.Data)
			if reply.Seq > e.seq.Load() {
				e.seq.Store(reply.Seq)
				progressed = true
			}
			// A completed, non-empty model turn is a reply worth relaying
			// (partial chunks are repeated by the final event). Exactly-once: a
			// reconnect resumes from the last seq seen, but skip anything
			// already posted in case a turn straddled the drop.
			if ok && !reply.Partial && reply.Text != "" {
				if reply.Seq <= e.relayed.Load() {
					return nil
				}
				e.relayed.Store(reply.Seq)
				r.deliverText(ctx, e, conv, reply.Text)
				return nil
			}
			// Otherwise it may be tool activity — surfaced only in the modes
			// that show progress, and gated by its own seq watermark so a
			// reconnect replay does not repost it. Its own, not the answer's:
			// sharing one would let a tool result raise the bar the answer has
			// to clear. The mode is resolved per event so a mid-session command
			// takes effect on the next turn.
			if mode := r.progressFor(e.channel); mode == ProgressStream || mode == ProgressStatus {
				if reply.Seq <= e.noticed.Load() {
					return nil
				}
				if calls := daemon.ToolCalls(ev.Data); len(calls) > 0 {
					e.noticed.Store(reply.Seq)
					r.postActivity(ctx, e, conv, mode, calls)
					return nil
				}
				// Tool results ride the same event, under the "user" role the
				// daemon gives anything the model did not author (#36). Stream
				// mode only: it is the one mode that registers a notice for a
				// result to tick off, so anywhere else this would be a parse
				// and a watermark advance for a message never sent.
				if mode == ProgressStream {
					if results := daemon.ToolResults(ev.Data); len(results) > 0 {
						e.noticed.Store(reply.Seq)
						r.postToolResults(ctx, e, conv, results)
					}
				}
			}
			return nil
		})
		if ctx.Err() != nil {
			return // shutting down: not a reconnectable failure
		}
		// A bound session the daemon no longer has is not a blip either. There
		// is nothing to reconnect to, ever, and a thread left quietly polling
		// for it looks exactly like a thread where nothing has happened yet.
		// Only bound sessions: one switchboard opened itself cannot outlive the
		// entry that holds it.
		if e.adopted && isMissingSession(err) {
			r.logf.Warnf("relay %s: bound session %s is gone from the daemon: %v", conv, sessionRef(e.sess), err)
			if r.discard(conv, e, true) {
				// On a context of its own: discard has just cancelled ctx,
				// which is this goroutine's, and the notice is the point.
				notify, cancel := context.WithTimeout(context.WithoutCancel(ctx), platformTimeout)
				if sendErr := r.surfaceNotice(notify, conv, bindStreamLostNotice(e.sess)); sendErr != nil {
					r.logf.Errorf("relay %s: surface lost binding: %v", conv, sendErr)
				}
				cancel()
			}
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
