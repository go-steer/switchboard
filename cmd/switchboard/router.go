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
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	// progressTickMaxAge stops the clock on a turn that has neither answered
	// nor reported itself complete. turn-complete is the real boundary; this is
	// the backstop for when it never arrives, so a daemon that loses a turn
	// leaves a frozen message rather than a goroutine editing a channel until
	// the process is restarted. Generous, because switchboard cannot tell a
	// genuinely long turn from a lost one and freezing a live one is the worse
	// mistake.
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

// activityText renders a standalone tool-activity notice, e.g. "🔧 Running
// `lookup`" — stream mode, and status mode with no message left to edit.
func activityText(tools []string) string { return "🔧 Running " + quotedTools(tools) }

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
	client   *daemon.Client
	out      sender
	progress ProgressMode
	metrics  *metrics
	logf     func(string, ...any)

	// showUsage turns on the per-turn tokens/cost footer. Off unless the
	// operator asked for it: what a turn cost is spend data, and a shared
	// channel is the wrong place to disclose it by default. Set once at
	// startup via setShowUsage, before the adapter is running.
	showUsage bool

	// Reconnect backoff bounds for the SSE relay; defaulted in NewRouter and
	// overridable in tests so a reconnect can be exercised without real waits.
	minBackoff, maxBackoff time.Duration

	// tickInterval is how often a turn in flight re-renders its progress
	// message with the elapsed clock. Defaulted in NewRouter and overridable in
	// tests so a tick can be observed without a real wait; zero or negative
	// disables the ticker and leaves the placeholder static. tickMaxAge is the
	// backstop that stops a clock the daemon never called time on; both are
	// defaulted in NewRouter and shortened in tests.
	tickInterval, tickMaxAge time.Duration

	mu       sync.Mutex
	sessions map[string]*sessionEntry

	// omu guards overrides, the per-channel progress-mode overrides set at
	// runtime via chat commands (HandleCommand). A channel absent from the map
	// uses the process default (r.progress); progressFor resolves the two.
	omu       sync.Mutex
	overrides map[string]ProgressMode
}

// sessionEntry is a conversation's session plus the state to create it
// exactly once under concurrent inbound turns. ready is closed when
// creation finishes (successfully or not); waiters block on it.
type sessionEntry struct {
	ready   chan struct{}
	sess    daemon.Session
	err     error
	channel string       // platform channel, for resolving the channel's progress mode
	seq     atomic.Int64 // highest agent-event seq seen, fed back as `since` on resume
	relayed atomic.Int64 // highest seq already posted, for exactly-once delivery across reconnects

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
// whole conversational turn is banked.
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
func (e *sessionEntry) takeUsage() *chat.Usage {
	e.umu.Lock()
	defer e.umu.Unlock()
	if !e.settled {
		return nil
	}
	u := e.usage
	e.usage, e.settled = daemon.TurnUsage{}, false
	if u.Empty() {
		return nil
	}
	return &chat.Usage{
		Model:     u.Model,
		TokensIn:  u.TokensIn,
		TokensOut: u.TokensOut,
		CostUSD:   u.CostUSD,
		Latency:   u.Latency,
	}
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
// if empty); m may be nil (metrics recording becomes a no-op); logf may be nil.
func NewRouter(client *daemon.Client, out sender, progress ProgressMode, m *metrics, logf func(string, ...any)) *Router {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if progress == "" {
		progress = ProgressOff
	}
	return &Router{
		client:       client,
		out:          out,
		progress:     progress,
		metrics:      m,
		logf:         logf,
		minBackoff:   reconnectMinBackoff,
		maxBackoff:   reconnectMaxBackoff,
		tickInterval: progressTickInterval,
		tickMaxAge:   progressTickMaxAge,
		sessions:     make(map[string]*sessionEntry),
		overrides:    make(map[string]ProgressMode),
	}
}

// setShowUsage turns the per-turn usage footer on. Unlike the progress mode
// it is not a runtime, per-channel setting — disclosing spend is an operator
// decision, not something a channel member should be able to flip — so it is
// set once at startup, before the adapter begins dispatching.
func (r *Router) setShowUsage(on bool) { r.showUsage = on }

// progressFor resolves the progress mode in effect for a channel: its runtime
// override (set via a chat command) if any, else the process default. An empty
// channel has no per-channel override and always resolves to the default.
func (r *Router) progressFor(channel string) ProgressMode {
	if channel != "" {
		r.omu.Lock()
		m, ok := r.overrides[channel]
		r.omu.Unlock()
		if ok {
			return m
		}
	}
	return r.progress
}

// setProgress records a per-channel progress-mode override.
func (r *Router) setProgress(channel string, mode ProgressMode) {
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
const commandHelp = "Try `progress <off|indicator|status|stream>` to set this " +
	"channel's long-turn feedback, or `progress` to see the current mode."

// Router reports its commands' accepted values, so an adapter on a platform
// with interactive controls can render them as buttons.
var _ chat.CommandChoices = (*Router)(nil)

// Choices implements chat.CommandChoices: the values `progress` accepts. The
// list is derived from the same constants parseProgressMode validates against,
// so a mode added there cannot go missing from the buttons.
func (r *Router) Choices(name string) []string {
	if name != "progress" {
		return nil
	}
	return []string{string(ProgressOff), string(ProgressIndicator), string(ProgressStatus), string(ProgressStream)}
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
			"`progress <off|indicator|status|stream>`.", r.progressFor(cmd.Channel))
	}
	mode, err := parseProgressMode(strings.ToLower(cmd.Args[0]))
	if err != nil {
		return fmt.Sprintf("Unknown progress mode %q. Choose one of: off, indicator, status, stream.", cmd.Args[0])
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
	entry.resetUsage()
	// Post the progress message before injecting: inject starts the turn, so a
	// fast reply would otherwise beat the placeholder into the thread and strand
	// it there ("Working…" below the answer, with nothing left to clear it). A
	// no-op in off and stream modes.
	r.startProgress(ctx, entry, msg.Conversation)
	start := time.Now()
	err = r.client.Inject(ctx, entry.sess, msg.Caller, msg.Text)
	r.metrics.recordDaemon("inject", time.Since(start), err)
	if err != nil {
		// The turn will never run, so the progress message would linger; clear it.
		r.clearProgress(ctx, entry, msg.Conversation)
		r.surfaceError(ctx, msg.Conversation, err)
		return err
	}
	return nil
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
	if _, sendErr := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: text, Kind: chat.KindNotice}); sendErr != nil {
		r.logf("handle %s: surface error: %v (original: %v)", conv, sendErr, err)
	}
}

const (
	errNoticeTransient = "⚠️ That turn didn't go through — the agent backend is having trouble. Please try again shortly."
	errNoticeTerminal  = "⚠️ That turn didn't go through and retrying the same message won't help. Check the logs or contact an admin."
)

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
		r.logf("progress %s: post: %v", conv, err)
		return
	}
	stale, stop := e.beginTurn(ref, start)
	if stale.ID != "" {
		if derr := r.out.Delete(ctx, stale); derr != nil {
			r.logf("progress %s: clear stale: %v", conv, derr)
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
			r.logf("progress %s: no turn boundary after %s; stopping the clock", conv, formatElapsed(time.Since(start)))
			return
		case <-timer.C:
		}
		ref, text, ok := e.tickRender(r.progressFor(e.channel))
		if !ok {
			return // the turn ended between the tick firing and this read
		}
		if err := r.out.Update(ctx, ref, chat.Reply{Conversation: conv, Text: text, Kind: chat.KindProgress}); err != nil {
			r.logf("progress %s: tick: %v", conv, err)
			interval = min(interval*2, progressTickMaxBackoff)
		} else {
			interval = r.tickInterval
		}
		timer.Reset(interval)
	}
}

// clearProgress deletes and forgets the entry's outstanding progress message,
// if any. Called before a reply is relayed so the transient message gives way
// to the answer. No-op when none is outstanding.
func (r *Router) clearProgress(ctx context.Context, e *sessionEntry, conv string) {
	if ref := e.takeProgress(); ref.ID != "" {
		if err := r.out.Delete(ctx, ref); err != nil {
			r.logf("progress %s: clear: %v", conv, err)
		}
	}
}

// deliverText relays a completed model turn: retire the transient progress
// message (a no-op unless indicator/status mode left one) and post the turn as
// its own message. Shared by every mode, so a long answer is chunked by the
// adapter rather than squeezed into an in-place edit. The turn's accounting is
// taken (and cleared) here whether or not the footer is enabled, so a session
// that runs with it off never accumulates stale numbers.
func (r *Router) deliverText(ctx context.Context, e *sessionEntry, conv, text string) {
	r.clearProgress(ctx, e, conv)
	r.metrics.recordTurnRelayed()
	usage := e.takeUsage()
	if !r.showUsage {
		usage = nil
	}
	_, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: text, Usage: usage})
	r.metrics.recordReply(err)
	if err != nil {
		// A failed post should not tear down the stream; log and keep relaying.
		r.logf("relay %s: send: %v", conv, err)
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
func (r *Router) postActivity(ctx context.Context, e *sessionEntry, conv string, mode ProgressMode, tools []string) {
	if mode == ProgressStatus {
		if ref, text, ok := e.noteActivity(tools); ok {
			if err := r.out.Update(ctx, ref, chat.Reply{Conversation: conv, Text: text, Kind: chat.KindProgress}); err != nil {
				r.logf("relay %s: status activity: %v", conv, err)
			}
			return
		}
	}
	_, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: activityText(tools), Kind: chat.KindActivity})
	r.metrics.recordReply(err)
	if err != nil {
		r.logf("relay %s: activity: %v", conv, err)
	}
}

// session returns the conversation's session, creating it (and starting
// its relay goroutine) on first use. The first caller in a thread owns
// the created session; the SSE relay is attributed to that owner.
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
	e := &sessionEntry{ready: make(chan struct{}), channel: channel}
	r.sessions[conv] = e
	r.mu.Unlock()

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
	close(e.ready)
	go r.relay(ctx, conv, e, caller)
	return e, nil
}

// relay holds the session's SSE subscription and posts each completed
// assistant turn back into the conversation. It reconnects with exponential
// backoff when the stream ends — a dropped stream must never silently strand a
// conversation — resuming from the last seq seen so the daemon replays only new
// turns, and skipping any turn already posted so a boundary replay cannot double
// up (#3). It runs until ctx is cancelled.
func (r *Router) relay(ctx context.Context, conv string, e *sessionEntry, owner string) {
	backoff := r.minBackoff
	for ctx.Err() == nil {
		progressed := false
		err := r.client.Subscribe(ctx, e.sess, owner, e.seq.Load(), func(ev daemon.Event) error {
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
				if u, ok := daemon.TurnCompleted(ev.Data); ok {
					e.noteTurnComplete(u)
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
			// that show progress, and gated by the same seq so a reconnect
			// replay does not repost it. The mode is resolved per event so a
			// mid-session command takes effect on the next turn.
			if mode := r.progressFor(e.channel); mode == ProgressStream || mode == ProgressStatus {
				if tools := daemon.ToolCalls(ev.Data); len(tools) > 0 && reply.Seq > e.relayed.Load() {
					e.relayed.Store(reply.Seq)
					r.postActivity(ctx, e, conv, mode, tools)
				}
			}
			return nil
		})
		if ctx.Err() != nil {
			return // shutting down: not a reconnectable failure
		}
		// The subscription returned: the stream ended or errored. Reset the
		// backoff if this connection made progress (a healthy stream that
		// blipped reconnects fast; only a persistently failing one backs off).
		if progressed {
			backoff = r.minBackoff
		}
		r.metrics.recordReconnect()
		r.logf("relay %s: stream ended (%v); resuming from seq %d in %s", conv, err, e.seq.Load(), backoff)
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
