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

// activityText renders a tool-activity notice, e.g. "🔧 Running `lookup`".
func activityText(tools []string) string {
	quoted := make([]string, len(tools))
	for i, t := range tools {
		quoted[i] = "`" + t + "`"
	}
	return "🔧 Running " + strings.Join(quoted, ", ")
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

	// Reconnect backoff bounds for the SSE relay; defaulted in NewRouter and
	// overridable in tests so a reconnect can be exercised without real waits.
	minBackoff, maxBackoff time.Duration

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
}

// takeProgress atomically reads and clears the entry's progress message.
func (e *sessionEntry) takeProgress() chat.MessageRef {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	ref := e.progressMsg
	e.progressMsg = chat.MessageRef{}
	return ref
}

// currentProgress reads the entry's progress message without clearing it.
func (e *sessionEntry) currentProgress() chat.MessageRef {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	return e.progressMsg
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
		client:     client,
		out:        out,
		progress:   progress,
		metrics:    m,
		logf:       logf,
		minBackoff: reconnectMinBackoff,
		maxBackoff: reconnectMaxBackoff,
		sessions:   make(map[string]*sessionEntry),
		overrides:  make(map[string]ProgressMode),
	}
}

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
// conversation (creating it and its relay subscription on the first
// turn), then inject the message and wake the session to run it.
func (r *Router) Handle(ctx context.Context, msg chat.Message) (err error) {
	// One counter per inbound turn, tallied by outcome on the way out.
	defer func() { r.metrics.recordMessage(err) }()

	entry, err := r.session(ctx, msg.Conversation, msg.Channel, msg.Caller)
	if err != nil {
		r.surfaceError(ctx, msg.Conversation, err)
		return err
	}
	start := time.Now()
	err = r.client.Inject(ctx, entry.sess, msg.Caller, msg.Text)
	r.metrics.recordDaemon("inject", time.Since(start), err)
	if err != nil {
		r.surfaceError(ctx, msg.Conversation, err)
		return err
	}
	// Post the progress message before waking: a reply can only follow wake, so
	// placing it first guarantees relay sees it before delivering the turn (no
	// orphaned "Working…" from a fast reply). A no-op in off and stream modes.
	r.startProgress(ctx, entry, msg.Conversation)
	start = time.Now()
	err = r.client.Wake(ctx, entry.sess, msg.Caller)
	r.metrics.recordDaemon("wake", time.Since(start), err)
	if err != nil {
		// The turn will never run, so the progress message would linger; clear it.
		r.clearProgress(ctx, entry, msg.Conversation)
		r.surfaceError(ctx, msg.Conversation, err)
		return err
	}
	return nil
}

// surfaceError posts a thread-scoped notice when a turn fails before it ever
// reaches the daemon's event stream (session creation, inject, or wake) — the
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
	if _, sendErr := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: text}); sendErr != nil {
		r.logf("handle %s: surface error: %v (original: %v)", conv, sendErr, err)
	}
}

const (
	errNoticeTransient = "⚠️ That turn didn't go through — the agent backend is having trouble. Please try again shortly."
	errNoticeTerminal  = "⚠️ That turn didn't go through and retrying the same message won't help. Check the logs or contact an admin."
)

// startProgress posts the initial progress message for a turn (indicator and
// status modes) and records it on the entry so relay can edit or clear it. A
// no-op in off and stream modes. A message still outstanding from a prior turn
// (a second turn started before the first replied) is deleted so only the
// latest remains. Failures are logged, never fatal — a missing progress
// message must not drop the turn.
func (r *Router) startProgress(ctx context.Context, e *sessionEntry, conv string) {
	if mode := r.progressFor(e.channel); mode != ProgressIndicator && mode != ProgressStatus {
		return
	}
	ref, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: workingText})
	r.metrics.recordReply(err)
	if err != nil {
		r.logf("progress %s: post: %v", conv, err)
		return
	}
	e.pmu.Lock()
	stale := e.progressMsg
	e.progressMsg = ref
	e.pmu.Unlock()
	if stale.ID != "" {
		if derr := r.out.Delete(ctx, stale); derr != nil {
			r.logf("progress %s: clear stale: %v", conv, derr)
		}
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
// adapter rather than squeezed into an in-place edit.
func (r *Router) deliverText(ctx context.Context, e *sessionEntry, conv, text string) {
	r.clearProgress(ctx, e, conv)
	r.metrics.recordTurnRelayed()
	_, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: text})
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
func (r *Router) postActivity(ctx context.Context, e *sessionEntry, conv string, mode ProgressMode, tools []string) {
	text := activityText(tools)
	if mode == ProgressStatus {
		if ref := e.currentProgress(); ref.ID != "" {
			if err := r.out.Update(ctx, ref, chat.Reply{Conversation: conv, Text: text}); err != nil {
				r.logf("relay %s: status activity: %v", conv, err)
			}
			return
		}
	}
	_, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: text})
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
