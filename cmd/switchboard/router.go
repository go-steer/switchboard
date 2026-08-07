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
	logf     func(string, ...any)

	// Reconnect backoff bounds for the SSE relay; defaulted in NewRouter and
	// overridable in tests so a reconnect can be exercised without real waits.
	minBackoff, maxBackoff time.Duration

	mu       sync.Mutex
	sessions map[string]*sessionEntry
}

// sessionEntry is a conversation's session plus the state to create it
// exactly once under concurrent inbound turns. ready is closed when
// creation finishes (successfully or not); waiters block on it.
type sessionEntry struct {
	ready   chan struct{}
	sess    daemon.Session
	err     error
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
// if empty); logf may be nil.
func NewRouter(client *daemon.Client, out sender, progress ProgressMode, logf func(string, ...any)) *Router {
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
		logf:       logf,
		minBackoff: reconnectMinBackoff,
		maxBackoff: reconnectMaxBackoff,
		sessions:   make(map[string]*sessionEntry),
	}
}

// Handle processes one inbound turn: ensure a session exists for the
// conversation (creating it and its relay subscription on the first
// turn), then inject the message and wake the session to run it.
func (r *Router) Handle(ctx context.Context, msg chat.Message) error {
	entry, err := r.session(ctx, msg.Conversation, msg.Caller)
	if err != nil {
		return err
	}
	if err := r.client.Inject(ctx, entry.sess, msg.Caller, msg.Text); err != nil {
		return err
	}
	// Post the progress message before waking: a reply can only follow wake, so
	// placing it first guarantees relay sees it before delivering the turn (no
	// orphaned "Working…" from a fast reply). A no-op in off and stream modes.
	r.startProgress(ctx, entry, msg.Conversation)
	if err := r.client.Wake(ctx, entry.sess, msg.Caller); err != nil {
		// The turn will never run, so the progress message would linger; clear it.
		r.clearProgress(ctx, entry, msg.Conversation)
		return err
	}
	return nil
}

// startProgress posts the initial progress message for a turn (indicator and
// status modes) and records it on the entry so relay can edit or clear it. A
// no-op in off and stream modes. A message still outstanding from a prior turn
// (a second turn started before the first replied) is deleted so only the
// latest remains. Failures are logged, never fatal — a missing progress
// message must not drop the turn.
func (r *Router) startProgress(ctx context.Context, e *sessionEntry, conv string) {
	if r.progress != ProgressIndicator && r.progress != ProgressStatus {
		return
	}
	ref, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: workingText})
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
	if _, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: text}); err != nil {
		// A failed post should not tear down the stream; log and keep relaying.
		r.logf("relay %s: send: %v", conv, err)
	}
}

// postActivity surfaces the tools the agent is running (stream and status
// modes). Status mode edits the managed status message in place so the whole
// turn stays one message; stream mode — and status mode with no message left
// to edit — posts a standalone notice.
func (r *Router) postActivity(ctx context.Context, e *sessionEntry, conv string, tools []string) {
	text := activityText(tools)
	if r.progress == ProgressStatus {
		if ref := e.currentProgress(); ref.ID != "" {
			if err := r.out.Update(ctx, ref, chat.Reply{Conversation: conv, Text: text}); err != nil {
				r.logf("relay %s: status activity: %v", conv, err)
			}
			return
		}
	}
	if _, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: text}); err != nil {
		r.logf("relay %s: activity: %v", conv, err)
	}
}

// session returns the conversation's session, creating it (and starting
// its relay goroutine) on first use. The first caller in a thread owns
// the created session; the SSE relay is attributed to that owner.
func (r *Router) session(ctx context.Context, conv, caller string) (*sessionEntry, error) {
	r.mu.Lock()
	if e, ok := r.sessions[conv]; ok {
		r.mu.Unlock()
		<-e.ready
		return e, e.err
	}
	// This goroutine owns creation; publish a not-yet-ready entry so
	// concurrent turns on the same conversation wait rather than
	// double-create, and release the map lock before the network call.
	e := &sessionEntry{ready: make(chan struct{})}
	r.sessions[conv] = e
	r.mu.Unlock()

	e.sess, e.err = r.client.CreateSession(ctx, caller)
	if e.err != nil {
		// Drop the failed entry so a later turn can retry.
		r.mu.Lock()
		delete(r.sessions, conv)
		r.mu.Unlock()
		close(e.ready)
		return e, e.err
	}
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
			// replay does not repost it.
			if r.progress == ProgressStream || r.progress == ProgressStatus {
				if tools := daemon.ToolCalls(ev.Data); len(tools) > 0 && reply.Seq > e.relayed.Load() {
					e.relayed.Store(reply.Seq)
					r.postActivity(ctx, e, conv, tools)
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
