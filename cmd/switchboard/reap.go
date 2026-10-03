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
	"time"
)

// Idle reaping (#87).
//
// A live entry is not a map row: it owns a relay goroutine, a persistent SSE
// stream to the daemon and, where prompts are relayed, a second one. That is
// right for a conversation that keeps answering, and it applied unchanged to a
// thread somebody said one thing in three weeks ago — a few hundred touched
// threads were a few hundred permanent streams, released only by a restart.
//
// So an entry idle past --session-idle-ttl is released: its streams close and
// it goes back to the dormant tier the state file restores into (#86), from
// which the next message — or a press on a question it asked — re-attaches it.
// Only with --state-dir: before there were durable records, dropping an entry
// threw away the only record that the conversation had a session at all, and
// the thread came back as a new session while the old one leaked.

// defaultIdleTTL is how long a conversation may go without traffic before its
// streams are released. Long enough that a thread being actively worked —
// including one waiting on an agent that is thinking — is never touched, and
// short enough that a day's worth of one-off questions does not hold a
// workspace's worth of streams open overnight.
const defaultIdleTTL = 12 * time.Hour

// reapEvery is how often the reaper sweeps for a given TTL: a quarter of it,
// capped, so an entry is released within about a quarter-TTL of going idle
// without sweeping more often than there is any point to.
func reapEvery(ttl time.Duration) time.Duration {
	return min(max(ttl/4, time.Second), 5*time.Minute)
}

// setIdleTTL turns reaping on. Zero leaves it off. Called once at startup.
func (r *Router) setIdleTTL(ttl time.Duration) { r.idleTTL = ttl }

// runReaper sweeps until ctx ends.
func (r *Router) runReaper(ctx context.Context) {
	if r.idleTTL <= 0 {
		return
	}
	t := time.NewTicker(reapEvery(r.idleTTL))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n := r.reapIdle(now); n > 0 {
				r.logf.Infof("state: released %d idle conversation(s); each re-attaches on its next message", n)
			}
		}
	}
}

// reapIdle releases every entry idle past the TTL at now, and reports how many.
//
// An entry with a turn in flight, or an answer it is waiting on, is never idle
// whatever its clock says: the agent may be thinking, and releasing the stream
// would drop the answer on the floor — exactly the failure the durable record
// exists to prevent, reproduced on purpose.
func (r *Router) reapIdle(now time.Time) int {
	cutoff := now.Add(-r.idleTTL).UnixNano()
	var released []*sessionEntry
	r.mu.Lock()
	for conv, e := range r.sessions {
		select {
		case <-e.ready:
		default:
			continue // still opening: not idle, just new
		}
		if e.err != nil || e.touched.Load() > cutoff || e.turnInFlight() || e.awaitingAnswer() {
			continue
		}
		r.dormant[conv] = e.record()
		delete(r.sessions, conv)
		released = append(released, e)
	}
	r.mu.Unlock()
	for _, e := range released {
		// Outside the lock, as discard does: stopping the relay is a context
		// cancel, but the ticker's stop takes the entry's own lock.
		if e.stop != nil {
			e.stop()
		}
		e.stopTicker()
		r.metrics.sessionClosed()
	}
	if len(released) > 0 {
		r.markDirty()
	}
	return len(released)
}
