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
	"strings"

	"github.com/go-steer/switchboard/pkg/daemon"
)

// Bindings: the conversations whose session switchboard did not create.
//
// The router's model is that a conversation *makes* a session — someone
// mentions the app, and switchboard opens one to answer them. An unattended
// agent inverts it. A watcher notices something at 3am, opens its own session
// against core-agent, works the incident, and posts what it found through the
// outbound ingress (#21). The thread that appears in chat has no session
// behind it as far as switchboard is concerned, so a human replying in it gets
// a *fresh* session: the one holding the incident and the one answering the
// question are not the same, and nobody is told (#38).
//
// A binding closes that loop. The caller names the session on the post:
//
//	POST /v1/messages {"conversation":"...","text":"...","session":"<app>/<id>"}
//
// and switchboard records conversation → session, so the first inbound turn in
// that thread adopts the session instead of creating one. It subscribes to
// nothing on its own: the harness decides what to say and when to say it, which
// is the whole point of doing this on the post rather than as a separate attach
// verb. All switchboard promises is that the reply lands in the right place.
//
// # What a bind checks
//
// Binding happens in two halves because the conversation it has to be recorded
// under is only known once the message has landed — a post to a bare channel or
// space creates the thread it lands in. PrepareBind runs first, before the
// post, and is where a bind can fail; CommitBind runs after, against the
// conversation the adapter named, and cannot.
//
// Everything that can be refused is therefore refused before anything is
// posted:
//
//   - The session must exist. PrepareBind reads its head seq from the daemon,
//     which answers 404 for a session it does not have. A thread bound to a
//     session that is not there would take a human's reply and drop it.
//   - The conversation must not already have a session. If a human was talking
//     in this thread first, that session owns it; a binding would be recorded
//     and then quietly never consulted, which is the failure mode this whole
//     change exists to remove.
//   - The session must not already own another conversation. Two threads
//     relaying one session would each post every answer it produces, to
//     readers who asked different questions.
//
// # What the head seq is for
//
// The daemon replays from the start of its window when a subscriber asks for
// everything, and the reason to adopt a session is that it has *been running* —
// so "everything" is the incident's whole transcript, delivered into a chat
// thread as a wall of answers to questions nobody in that thread asked. The
// bind therefore records where the session is now (daemon.HeadSeq), and the
// relay starts from there. What the agent did before the thread existed stays
// where it happened.
//
// It is not free: the protocol has no way to ask, so the head is measured by
// opening the stream and reading until it goes quiet, and that happens before
// the message is posted. A settled session costs the quiet window; one mid-turn
// costs the cap. The alternative — post first, measure after — would put the
// backlog in the thread whenever the measurement failed, which is the outcome
// this exists to prevent.
//
// # What is lost on restart
//
// Bindings live in this process and nowhere else, like every other map here.
// A restart drops them, and a reply to an orphaned thread then opens a fresh
// session — today's behaviour, and switchboard cannot tell that it is wrong,
// because a chat event carries no trace of who started the thread. What it can
// do is not fail silently anywhere it *can* see the problem: a bind that cannot
// be honoured is refused with a reason (above), an eviction is logged, and a
// session that goes missing after it was bound is announced in the thread
// rather than papered over with a new one (see Router.Handle). The recovery is
// the caller re-binding: its next update carries the session again, and finds
// the thread unbound and free to take. That works for a caller that addresses
// the *thread* — the conversation its first post was answered with. One that
// keeps addressing the bare channel starts a new thread every time, and is
// refused as soon as one of them holds the session (errSessionBound), which is
// the honest answer: the session it wants to be reachable in is reachable
// somewhere else.
const maxBindings = 1024

// binding is one conversation's adopted session and where in its event stream
// the thread should pick up.
type binding struct {
	sess daemon.Session
	// since is the session's head seq when the binding was made: the resume
	// point that keeps the backlog out of the thread. See above.
	since int64
}

// The refusals a bind can carry that are not the daemon's. All of them mean the
// caller asked for something that would have looked like it worked.
var (
	errConversationBound = errors.New("this conversation already has a session")
	errSessionBound      = errors.New("this session is already bound to another conversation")
	errBindInFlight      = errors.New("a bind for this session is already in progress")
)

// bindConflict is errSessionBound carrying the conversation that holds the
// session — the one thing the caller needs to fix the request, since it is the
// thread key its own earlier post was answered with.
type bindConflict struct{ conv string }

func (e *bindConflict) Error() string        { return errSessionBound.Error() }
func (e *bindConflict) Is(target error) bool { return target == errSessionBound }

// sessionRef renders a session the way the ingress names it on the wire, and
// the key bindings are indexed by.
func sessionRef(s daemon.Session) string { return s.App + "/" + s.ID }

// parseSessionRef reads an "<app>/<id>" session reference.
//
// Both halves land in a URL path the daemon client builds by concatenation, so
// this is deliberately stricter than "non-empty": a slash, a query character or
// a dot segment would address something other than the session named, and the
// caller is a program that can spell its own session id.
func parseSessionRef(s string) (daemon.Session, error) {
	app, id, ok := strings.Cut(s, "/")
	if !ok {
		return daemon.Session{}, errors.New(`want "<app>/<id>"`)
	}
	for _, part := range []string{app, id} {
		if part == "" {
			return daemon.Session{}, errors.New(`want "<app>/<id>", with both halves set`)
		}
		if part == "." || part == ".." {
			return daemon.Session{}, errors.New("a path segment must name something")
		}
		if strings.ContainsFunc(part, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
				r == '-' || r == '_' || r == '.')
		}) {
			return daemon.Session{}, errors.New("a path segment may hold only letters, digits, '-', '_' and '.'")
		}
	}
	return daemon.Session{App: app, ID: id}, nil
}

// PrepareBind is the half of a bind that can fail: it checks that this session
// may own this conversation, reserves the session against a second bind racing
// it, and reads the session's head seq from the daemon, which is also what
// proves the session exists.
//
// It records no binding — the conversation a post lands in is not known until
// it has landed — but it does hold the reservation, so every call that returns
// nil must be ended by exactly one CommitBind or AbortBind.
//
// conv is the conversation the caller addressed, which may be a bare channel or
// space; CommitBind is given the one the message actually landed in.
func (r *Router) PrepareBind(ctx context.Context, conv string, sess daemon.Session) (int64, error) {
	if err := r.reserveBind(conv, sess); err != nil {
		return 0, err
	}
	// No asserted caller: this reads a session belonging to whoever created it,
	// and switchboard has no standing to claim it is any particular person. The
	// turns it later injects are attributed to the human who typed them.
	head, err := r.client.HeadSeq(ctx, sess, "")
	if err != nil {
		r.AbortBind(sess)
		return 0, err
	}
	return head, nil
}

// AbortBind ends a reservation PrepareBind took for a bind that will not
// happen: the post failed, or the caller went away before it was made. Without
// it the session would stay unbindable until the process restarted.
func (r *Router) AbortBind(sess daemon.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.reserving, sessionRef(sess))
}

// reserveBind checks the two rules above — one session per conversation, one
// conversation per session — and claims the session for the caller if they
// hold.
//
// The claim is what makes the rules true under concurrency. Checking alone
// leaves a window between the check and the CommitBind that follows a platform
// call, and two posts naming the same session for two conversations would both
// pass it, both post, and both bind: two relays, every answer twice, and an
// inverse index that can only name one of them. A reservation is exclusive
// even for the same conversation, because a caller racing itself is a caller
// that cannot say which post's binding won either.
func (r *Router) reserveBind(conv string, sess daemon.Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// An entry already relaying this very session is this same binding in use —
	// a caller posting a second update into a thread it bound earlier, after
	// someone replied in it. Re-binding that is a no-op rather than a conflict.
	if e, ok := r.sessions[conv]; ok && !runningSession(e, sess) {
		return errConversationBound
	}
	ref := sessionRef(sess)
	if other, ok := r.boundTo[ref]; ok && other != conv {
		return &bindConflict{conv: other}
	}
	if _, ok := r.reserving[ref]; ok {
		return errBindInFlight
	}
	r.reserving[ref] = conv
	return nil
}

// runningSession reports whether e has finished opening and is an adopted entry
// relaying sess — the state in which a bind of conv to sess is already true and
// asking for it again changes nothing.
//
// It reads the entry rather than r.bindings deliberately. An eviction can take
// the binding out from under a thread that is still relaying it, and a check
// against the map would then answer "that conversation has some other session"
// to the one caller who could put the record back, for as long as the thread
// lives.
//
// The read of e.sess is non-blocking on purpose: ready is what publishes it, and
// the caller holds r.mu, which the goroutine that closes ready may still need.
// An entry that is not ready therefore counts as a conflict, which it is —
// somebody else is opening a session in this conversation right now.
func runningSession(e *sessionEntry, sess daemon.Session) bool {
	if !e.adopted {
		return false
	}
	select {
	case <-e.ready:
		return e.err == nil && e.sess == sess
	default:
		return false
	}
}

// CommitBind records conv → sess and ends the reservation PrepareBind took. It
// runs after the message has landed, so it cannot refuse the *caller*:
// everything refusable was settled by PrepareBind, and the two are separated by
// one platform call. What can still have changed in that window — a human
// getting the first word in the new thread — is logged and left alone, because
// that session is already answering them.
//
// conv is where the message landed, which is not always what PrepareBind was
// asked about: a post to a bare channel or space creates its thread. So the
// two checks below are made again here, against the conversation the binding is
// actually being recorded under.
func (r *Router) CommitBind(conv string, sess daemon.Session, since int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.reserving, sessionRef(sess))
	if e, ok := r.sessions[conv]; ok && !runningSession(e, sess) {
		r.logf("bind %s: not bound to %s: the conversation acquired a session first",
			conv, sessionRef(sess))
		return
	}
	if old, ok := r.bindings[conv]; ok {
		if old.sess == sess {
			// Same session, later post: move the resume point up rather than
			// leave the thread pointed at where the incident was an hour ago.
			r.bindings[conv] = binding{sess: sess, since: since}
			return
		}
		// Landed in a thread that belongs to a different session. Taking it
		// would strand that session's caller — its thread would answer to
		// someone else — and this post has already gone out either way, so the
		// honest outcome is an unbound message and a line saying so.
		r.logf("bind %s: not bound to %s: the thread already belongs to %s",
			conv, sessionRef(sess), sessionRef(old.sess))
		return
	}
	r.bindOrder = append(r.bindOrder, conv)
	r.metrics.bindRecorded()
	r.bindings[conv] = binding{sess: sess, since: since}
	r.boundTo[sessionRef(sess)] = conv
	r.logf("bind %s -> session %s from seq %d", conv, sessionRef(sess), since)
	r.evictBindings()
}

// evictBindings holds the map to maxBindings, oldest first. The caller holds
// r.mu.
//
// Loudly: an evicted thread still looks bound to the caller that bound it, and
// the next reply in it opens a session of its own. That is the same silent
// wrong answer this file exists to prevent, and the only honest thing a bounded
// map can do about it is say which thread it dropped.
func (r *Router) evictBindings() {
	for len(r.bindOrder) > maxBindings {
		oldest := r.bindOrder[0]
		r.bindOrder = r.bindOrder[1:]
		b, ok := r.bindings[oldest]
		if !ok {
			continue
		}
		delete(r.bindings, oldest)
		delete(r.boundTo, sessionRef(b.sess))
		r.metrics.bindDropped()
		r.logf("bind %s: evicted (over %d bindings); a reply there will start a fresh session",
			oldest, maxBindings)
	}
}

// unbind forgets a conversation's binding. The caller holds r.mu.
func (r *Router) unbind(conv string) {
	b, ok := r.bindings[conv]
	if !ok {
		return
	}
	delete(r.bindings, conv)
	delete(r.boundTo, sessionRef(b.sess))
	for i, c := range r.bindOrder {
		if c == conv {
			r.bindOrder = append(r.bindOrder[:i], r.bindOrder[i+1:]...)
			break
		}
	}
	r.metrics.bindDropped()
}

// errNoticeBindLost is what a thread is told when the session it was bound to
// is gone from the daemon. Deliberately not the generic terminal notice: this
// one names a specific thing an operator can go and look at, and says what
// happens next, because what happens next — the following message opening a
// session that knows nothing about the incident — is otherwise indistinguishable
// from everything working.
func bindLostNotice(sess daemon.Session) string {
	return fmt.Sprintf("⚠️ This thread was tied to agent session `%s`, and the agent backend "+
		"no longer has it. That message was not delivered anywhere. The next one here will "+
		"start a new session, which will not know what happened in this thread.",
		sessionRef(sess))
}

// bindStreamLostNotice is the same fact found the other way round: not by a
// message failing to reach the session, but by its event stream ending in a 404
// while nobody was typing. Worth its own wording, because no message was lost —
// what is lost is everything the thread was waiting to hear, and a thread that
// simply goes quiet is indistinguishable from an agent still thinking.
func bindStreamLostNotice(sess daemon.Session) string {
	return fmt.Sprintf("⚠️ This thread was tied to agent session `%s`, and the agent backend "+
		"no longer has it. Nothing further will arrive here from it. The next message here "+
		"will start a new session, which will not know what happened in this thread.",
		sessionRef(sess))
}
