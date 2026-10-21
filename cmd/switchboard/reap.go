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
	return r.release(now, func(e *sessionEntry) bool { return e.touched.Load() <= cutoff })
}

// axSweepEvery is how often ax conversations are checked for release.
var axSweepEvery = 10 * time.Second

// runAXRelease releases ax conversations once their streams have lingered
// past the agent's stream_linger with nothing going on (#171): an open
// stream is traffic to ax, so a conversation holding one would keep its task
// from ever suspending. Released, it re-attaches on its next message, which
// wakes the task. Runs whatever --session-idle-ttl says; a gateway with no
// ax agents finds nothing to release.
func (r *Router) runAXRelease(ctx context.Context) {
	t := time.NewTicker(axSweepEvery)
	defer t.Stop()
	var lastDelete time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			r.releaseIdleAX(now)
			if now.Sub(lastDelete) >= axDeleteEvery {
				lastDelete = now
				r.deleteQuietTasks(ctx, now)
			}
		}
	}
}

// axDeleteEvery is how often quiet conversations' tasks are considered for
// deletion; deleting is an ax-server call, and nothing about it is urgent.
var axDeleteEvery = time.Minute

// deleteQuietTasks deletes the thread-scope tasks of dormant conversations
// quiet past their agent's delete_after (#173), and marks their records so
// the next message there starts over and is told why. Only thread scope: a
// personal or shared task holds more than one conversation.
func (r *Router) deleteQuietTasks(ctx context.Context, now time.Time) int {
	type doomed struct {
		conv, task string
		a          *agent
	}
	set := r.agents()
	var due []doomed
	r.mu.Lock()
	for conv, rec := range r.dormant {
		if rec.Task == "" || rec.TaskDeleted {
			continue
		}
		a, ok := set.lookup(rec.Agent)
		if !ok || a.ax == nil || a.ax.scope != axScopeThread || a.ax.deleteAfter <= 0 || now.Sub(rec.Touched) <= a.ax.deleteAfter {
			continue
		}
		due = append(due, doomed{conv, rec.Task, a})
	}
	r.mu.Unlock()
	deleted := 0
	for _, d := range due {
		dctx, cancel := context.WithTimeout(ctx, platformTimeout)
		err := d.a.ax.client.Delete(dctx, d.task)
		cancel()
		if err != nil {
			r.logf.Warnf("ax: delete quiet task %s of %s: %v", d.task, d.conv, err)
			continue
		}
		r.markTaskDeleted(d.task)
		r.logf.Infof("ax: deleted task %s of %s, quiet for over %s", d.task, d.conv, d.a.ax.deleteAfter)
		deleted++
	}
	return deleted
}

// markTaskDeleted marks every record of task deleted, live entries included:
// a live one is released first, its streams stopped, so its next message
// finds the mark rather than a task that is not there.
func (r *Router) markTaskDeleted(task string) {
	var stopped []*sessionEntry
	r.mu.Lock()
	for conv, e := range r.sessions {
		select {
		case <-e.ready:
		default:
			continue
		}
		if e.task == task {
			r.dormant[conv] = e.record()
			delete(r.sessions, conv)
			stopped = append(stopped, e)
		}
	}
	for conv, rec := range r.dormant {
		if rec.Task == task && !rec.TaskDeleted {
			rec.TaskDeleted = true
			r.dormant[conv] = rec
		}
	}
	r.mu.Unlock()
	for _, e := range stopped {
		if e.stop != nil {
			e.stop()
		}
		e.stopTicker()
		r.metrics.sessionClosed()
	}
	r.markDirty()
}

// releaseIdleAX releases the ax conversations idle for their agent's linger.
func (r *Router) releaseIdleAX(now time.Time) int {
	set := r.agents()
	return r.release(now, func(e *sessionEntry) bool {
		a, ok := set.lookup(e.agent)
		// A template without spec.idle never suspends its tasks, so letting
		// go of the streams would only cost a revive (caught in review).
		if !ok || a.ax == nil || !a.ax.idle || e.task == "" {
			return false
		}
		return e.touched.Load() <= now.Add(-a.ax.linger).UnixNano()
	})
}

// busy reports what keeps a conversation's streams open beyond switchboard's
// own idea of a turn: the daemon saying it is working or parked, a turn being
// set up, or a question on screen that nobody has answered — the prompt
// stream is what would carry the next one (caught in review).
func (e *sessionEntry) busy() bool {
	if e.agentBusy.Load() || e.pins.Load() > 0 {
		return true
	}
	e.qmu.Lock()
	defer e.qmu.Unlock()
	for _, a := range e.asked {
		if a.settled == unsettled && a.posted {
			return true
		}
	}
	return false
}

// release moves to dormant every live conversation idle reports as idle and
// that is not busy, stopping its streams; each re-attaches on its next
// message. It also prunes records too old to keep.
func (r *Router) release(now time.Time, idle func(*sessionEntry) bool) int {
	var released []*sessionEntry
	r.mu.Lock()
	for conv, e := range r.sessions {
		select {
		case <-e.ready:
		default:
			continue // still opening: not idle, just new
		}
		// Never an adopted session: an unattended agent posting into its
		// thread has no human turn to keep it live, and releasing its relay
		// would silently stop its output reaching the thread until somebody
		// spoke there. Bindings are already capped (maxBindings).
		if e.adopted || e.err != nil || e.turnInFlight() || e.awaitingAnswer() || e.busy() || !idle(e) {
			continue
		}
		r.dormant[conv] = e.record()
		delete(r.sessions, conv)
		released = append(released, e)
	}
	// And forget the conversations nobody has spoken in for a month, so the
	// record does not grow with every thread ever touched (dormantMaxAge).
	pruned := false
	for conv, rec := range r.dormant {
		if now.Sub(rec.Touched) > dormantMaxAge {
			delete(r.dormant, conv)
			pruned = true
		}
	}
	for conv, o := range r.owners {
		if now.Sub(o.Since) > ownerMaxAge {
			delete(r.owners, conv)
			pruned = true
		}
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
	if len(released) > 0 || pruned {
		r.markDirty()
	}
	return len(released)
}
