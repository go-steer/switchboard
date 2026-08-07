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

	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// sender is the egress half of a chat.Adapter the router needs to relay
// replies. Narrowed to one method so the router is testable with a fake.
type sender interface {
	Send(context.Context, chat.Reply) error
}

// Router maps chat conversations onto core-agent sessions and shuttles
// turns across the daemon contract. It is the chat.Handler an adapter
// dispatches inbound messages to. One session per conversation key; each
// session gets one long-lived SSE subscription whose agent output is
// relayed back through the adapter — subscribing once (rather than
// per-turn) is what keeps the daemon from replaying prior turns on every
// message.
type Router struct {
	client *daemon.Client
	out    sender
	logf   func(string, ...any)

	mu       sync.Mutex
	sessions map[string]*sessionEntry
}

// sessionEntry is a conversation's session plus the state to create it
// exactly once under concurrent inbound turns. ready is closed when
// creation finishes (successfully or not); waiters block on it.
type sessionEntry struct {
	ready chan struct{}
	sess  daemon.Session
	err   error
	seq   atomic.Int64 // highest agent-event seq relayed, for resume
}

// NewRouter builds a Router. logf may be nil.
func NewRouter(client *daemon.Client, out sender, logf func(string, ...any)) *Router {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Router{
		client:   client,
		out:      out,
		logf:     logf,
		sessions: make(map[string]*sessionEntry),
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
	return r.client.Wake(ctx, entry.sess, msg.Caller)
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
// assistant turn back into the conversation. It runs until ctx is
// cancelled or the stream ends (reconnect is a later phase, #3).
func (r *Router) relay(ctx context.Context, conv string, e *sessionEntry, owner string) {
	err := r.client.Subscribe(ctx, e.sess, owner, 0, func(ev daemon.Event) error {
		if ev.Type != daemon.EventAgent {
			return nil
		}
		reply, ok := daemon.AgentText(ev.Data)
		if reply.Seq > e.seq.Load() {
			e.seq.Store(reply.Seq)
		}
		// Relay only completed, non-empty model turns: partial chunks
		// are repeated by the final event, and tool-call events carry no
		// text.
		if !ok || reply.Partial || reply.Text == "" {
			return nil
		}
		if serr := r.out.Send(ctx, chat.Reply{Conversation: conv, Text: reply.Text}); serr != nil {
			// A failed post should not tear down the stream; log and keep
			// relaying subsequent turns.
			r.logf("relay %s: send: %v", conv, serr)
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		r.logf("relay %s: stream ended: %v", conv, err)
	}
}
