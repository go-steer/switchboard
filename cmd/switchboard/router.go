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
// runs. off keeps today's behavior (nothing until the turn completes);
// indicator posts a lightweight placeholder on wake and clears it when the
// turn's reply arrives. (stream and status are later phases.)
type ProgressMode string

const (
	ProgressOff       ProgressMode = "off"
	ProgressIndicator ProgressMode = "indicator"
)

// indicatorText is the placeholder posted under ProgressIndicator while a
// turn is in flight; it is deleted once the real reply is relayed.
const indicatorText = "⏳ Working…"

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

	// pmu guards pending, the progress placeholder awaiting the next relayed
	// turn (ProgressIndicator). Handle posts it; relay clears it when the
	// turn's reply is delivered. Zero value means no placeholder outstanding.
	pmu     sync.Mutex
	pending chat.MessageRef
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
	// Post the progress placeholder before waking: a reply can only follow
	// wake, so placing it first guarantees relay sees the placeholder before it
	// delivers the turn (no orphaned "Working…" from a fast reply).
	r.showIndicator(ctx, entry, msg.Conversation)
	if err := r.client.Wake(ctx, entry.sess, msg.Caller); err != nil {
		// The turn will never run, so the placeholder would linger; clear it.
		r.clearIndicator(ctx, entry, msg.Conversation)
		return err
	}
	return nil
}

// showIndicator posts the ProgressIndicator placeholder for a turn and records
// it on the entry so relay can clear it when the reply arrives. A no-op unless
// ProgressIndicator is selected. A placeholder still outstanding from a prior
// turn (a second turn started before the first replied) is deleted so only the
// latest remains. Failures are logged, never fatal — a missing indicator must
// not drop the turn.
func (r *Router) showIndicator(ctx context.Context, e *sessionEntry, conv string) {
	if r.progress != ProgressIndicator {
		return
	}
	ref, err := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: indicatorText})
	if err != nil {
		r.logf("progress %s: post indicator: %v", conv, err)
		return
	}
	e.pmu.Lock()
	stale := e.pending
	e.pending = ref
	e.pmu.Unlock()
	if stale.ID != "" {
		if derr := r.out.Delete(ctx, stale); derr != nil {
			r.logf("progress %s: clear stale indicator: %v", conv, derr)
		}
	}
}

// clearIndicator deletes and forgets the entry's outstanding progress
// placeholder, if any. Called just before a real reply is relayed so the
// placeholder gives way to the answer. No-op when none is outstanding.
func (r *Router) clearIndicator(ctx context.Context, e *sessionEntry, conv string) {
	e.pmu.Lock()
	ref := e.pending
	e.pending = chat.MessageRef{}
	e.pmu.Unlock()
	if ref.ID != "" {
		if err := r.out.Delete(ctx, ref); err != nil {
			r.logf("relay %s: clear indicator: %v", conv, err)
		}
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
			// Relay only completed, non-empty model turns: partial chunks
			// are repeated by the final event, and tool-call events carry no
			// text.
			if !ok || reply.Partial || reply.Text == "" {
				return nil
			}
			// Exactly-once: a reconnect resumes from the last seq seen, but
			// skip anything already posted in case a turn straddled the drop.
			if reply.Seq <= e.relayed.Load() {
				return nil
			}
			e.relayed.Store(reply.Seq)
			// A real reply is ready: retire the progress placeholder (if any)
			// before posting the answer.
			r.clearIndicator(ctx, e, conv)
			if _, serr := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: reply.Text}); serr != nil {
				// A failed post should not tear down the stream; log and keep
				// relaying subsequent turns.
				r.logf("relay %s: send: %v", conv, serr)
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
