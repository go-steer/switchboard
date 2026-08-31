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
	"time"

	"github.com/go-steer/switchboard/pkg/approval"
	"github.com/go-steer/switchboard/pkg/chat"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// setApprovals turns permission relaying on, and is the only thing that does:
// a nil client leaves every session's prompts where they were, waiting on a
// console nobody in the thread is sitting at.
//
// A setter rather than a NewRouter parameter for the same reason showUsage is
// one — it is an operator decision made once at startup, before the adapter
// begins dispatching, and every test that does not care about permissions
// should not have to name it.
func (r *Router) setApprovals(c *approval.Client) { r.approvals = c }

// watchPermsIfOffered starts this session's permission watcher, at most once.
//
// The trigger is the capabilities frame, not the daemon's awaiting_permission
// turn state, and that is not the obvious choice. A turn state saying "this
// session is blocked on a human right now" is exactly the signal that would
// let switchboard hold one stream instead of two, and open it only for the
// sessions that ever ask. core-agent declares that state and emits it nowhere:
// it has one occurrence in the whole tree, the constant that names it. Waiting
// for it means relaying no prompt, ever, on any build shipping today.
//
// So the question becomes "can this session ask?" instead of "is it asking?",
// which the capabilities frame answers on every connection. The cost is a
// second SSE connection per session whose agent registered a broker. Nothing
// is lost by attaching early: a subscriber is seeded with every prompt already
// pending, so a watcher that attaches after the gate has stopped a call still
// receives it.
func (r *Router) watchPermsIfOffered(ctx context.Context, conv string, e *sessionEntry, c daemon.Capabilities) {
	if r.approvals == nil || !c.Offers(daemon.FeaturePermsStream) {
		return
	}
	if !e.claimPermsWatch() {
		return
	}
	go r.watchPerms(ctx, conv, e)
}

// watchPerms holds the session's permission subscription and posts each
// pending prompt into the conversation as a question with buttons.
//
// It reconnects like the event relay does, and for the same reason: a prompt
// that arrives while the stream is down is a turn parked until someone notices.
// Resuming needs no cursor — the broker seeds a new subscriber with everything
// still pending, and drops what has been answered. What it does not know is
// what has already been *posted*: an unanswered question is still pending, so
// every reconnect redelivers it. postPrompt is what makes that idempotent.
func (r *Router) watchPerms(ctx context.Context, conv string, e *sessionEntry) {
	backoff := r.minBackoff
	for ctx.Err() == nil {
		// Whether this connection carried a prompt: its own evidence that it
		// worked, which is what separates a healthy stream that blipped from
		// one that never opens. An intermediary cuts an idle stream with an
		// unexpected EOF, so without this the backoff only ever doubles, and a
		// session whose stream is cut every few minutes settles at the ceiling
		// — where the next permission question waits that long before anyone
		// sees it. The event relay resets on the same reasoning.
		delivered := false
		err := r.approvals.Stream(ctx, e.sess, "", func(p approval.Prompt) error {
			delivered = true
			r.postPrompt(ctx, conv, e, p)
			return nil
		})
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, approval.ErrNotSupported):
			// The frame said the route was there and the route says otherwise.
			// Permanent for this session either way, and retrying it is a loop.
			r.logf("perms %s: session advertised permission prompts but serves none", conv)
			return
		case err != nil && !daemon.IsTransient(err):
			r.logf("perms %s: giving up on permission prompts: %v", conv, err)
			return
		case err != nil:
			r.logf("perms %s: prompt stream: %v; retrying in %s", conv, err, backoff)
		}
		// A clean end counts too: the daemon closes this stream on shutdown and
		// on the agent finishing, neither of which is a connection that failed.
		// Named once, because the reset and the back-off below are the two
		// halves of it and must not drift apart.
		progressed := err == nil || delivered
		if progressed {
			backoff = r.minBackoff
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

// decisionRef qualifies a prompt id with the session that raised it, so a press
// can be checked against the session it is about to be sent to rather than
// merely against the conversation it arrived in. A conversation's session can
// be replaced under a thread whose buttons are still on screen — an adopted
// binding the daemon has lost is discarded, and the next message there opens a
// new one — and an unqualified id would then be answered against a session that
// never asked. See Router.HandlePress.
func decisionRef(sess daemon.Session, promptID string) string {
	return sessionRef(sess) + "#" + promptID
}

// splitDecisionRef reverses decisionRef. Neither half of a session reference
// can contain '#', so the first one separates them.
func splitDecisionRef(ref string) (sess, promptID string, ok bool) {
	sess, promptID, ok = strings.Cut(ref, "#")
	return sess, promptID, ok && sess != "" && promptID != ""
}

// postPrompt puts one pending permission prompt into the thread, once.
func (r *Router) postPrompt(ctx context.Context, conv string, e *sessionEntry, p approval.Prompt) {
	if !e.claimAsk(p.ID) {
		// Already on screen. Every resubscription is seeded with everything
		// still pending, which is what lets this watcher reconnect without a
		// cursor — and would otherwise put the same question in the thread
		// again on every reconnect, each copy with its own live buttons.
		return
	}
	opts := approval.Options(p)
	d := &chat.Decision{ID: decisionRef(e.sess, p.ID)}
	for _, o := range opts {
		d.Options = append(d.Options, chat.DecisionOption{
			Value: string(o.Decision),
			Label: o.Label,
			Broad: o.Broad,
		})
	}
	body := promptText(p)
	if _, err := r.out.Send(ctx, chat.Reply{
		Conversation: conv,
		Text:         body + "\n\n" + chat.DecisionText(d),
		Kind:         chat.KindDecision,
		Decision:     d,
	}); err != nil {
		// Give the claim back. The prompt is still pending on the daemon, so
		// the next reconnect is seeded with it again — which is the only retry
		// there is, and holding the claim would turn a transient posting
		// failure into a question that is never asked.
		e.releaseAsk(p.ID)
		r.logf("perms %s: post prompt %s: %v", conv, p.ID, err)
		return
	}
	r.logf("perms %s: asked about %s (%s)", conv, p.Tool, p.Kind)
}

// The two things a press can be told, and the reason it has to be told
// anything: a press is the one inbound action with no reply of its own. The
// button flashes, the platform considers it delivered, and a person who is not
// told otherwise reasonably assumes the agent has been unblocked.
const (
	noticePressFailed = "⚠️ That answer didn't reach the agent. It's still waiting — try pressing again."
	noticeStalePress  = "⚠️ That question belongs to a session this thread no longer has, so the answer wasn't sent. If the agent is still waiting, it will ask again."
)

// promptDetailLimit bounds the agent-controlled detail put in a message. The
// command or path is the whole substance of the question, so this is generous;
// it is here because Detail is unbounded and a megabyte of it in a thread
// helps nobody decide anything.
const promptDetailLimit = 1500

// promptText writes the question. It leads with the specifics — the command,
// the path — because that is what is being decided; the tool name and the
// asking agent are context for it.
func promptText(p approval.Prompt) string {
	var b strings.Builder
	b.WriteString("**Permission needed**")
	if p.Tool != "" {
		b.WriteString(" — `" + p.Tool + "`")
	}
	if detail := strings.TrimSpace(p.Detail); detail != "" {
		b.WriteString("\n\n```\n" + clampRunes(detail, promptDetailLimit) + "\n```")
	}
	if p.Source != "" {
		// Which agent is asking, when it is not the one being talked to. The
		// difference between approving something you just asked for and
		// approving something a subagent you forgot about wants.
		b.WriteString("\n_asked by the `" + p.Source + "` subagent_")
	}
	return b.String()
}

// clampRunes bounds a string to n runes without splitting one.
func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// HandlePress answers a permission prompt with the decision someone pressed.
//
// The approver is the presser, carried through as the asserted caller, and the
// daemon records the identity it verifies from that rather than anything in
// the body — so this cannot attribute an approval to somebody who did not give
// it, even if the press arrived claiming otherwise.
func (r *Router) HandlePress(ctx context.Context, p chat.Press) error {
	if r.approvals == nil {
		return errors.New("permission prompts are not enabled on this gateway")
	}
	d := approval.Decision(p.Option)
	if !d.Valid() {
		// Our own buttons, so this is a mangled payload rather than a person
		// doing something unexpected. Refusing beats guessing: the nearest
		// wrong guess is an approval.
		return fmt.Errorf("press on %s carried an answer that is not a decision: %q", p.Conversation, p.Option)
	}
	sess, promptID, ok := splitDecisionRef(p.DecisionID)
	if !ok {
		return fmt.Errorf("press in %s names no prompt: %q", p.Conversation, p.DecisionID)
	}
	e, err := r.boundSession(p.Conversation)
	if err != nil {
		return err
	}
	if sess != sessionRef(e.sess) {
		// The conversation is on a different session than the one that asked.
		// A bound session the daemon lost is discarded and the next message in
		// the thread opens a new one, which leaves the old question on screen
		// with live buttons; answering it here would apply a decision — possibly
		// a standing one — to whatever the *new* session happens to have
		// pending under the same id.
		r.logf("perms %s: press answers %s, which is no longer this conversation's session", p.Conversation, sess)
		return r.surfaceNotice(ctx, p.Conversation, noticeStalePress)
	}
	ack, err := r.approvals.Respond(ctx, e.sess, p.Caller, promptID, d)
	if err != nil {
		if errors.Is(err, approval.ErrNotFound) {
			// Someone else got there first, or the prompt timed out. Not a
			// failure to report as one — the question is settled either way.
			r.logf("perms %s: %s is no longer pending", p.Conversation, promptID)
			return nil
		}
		// Say so in the thread. A press is the one inbound action with no reply
		// of its own: the button flashes, the platform considers it delivered,
		// and without this the person who pressed it watches an agent stay
		// blocked with nothing anywhere to say the answer did not land.
		if nerr := r.surfaceNotice(ctx, p.Conversation, noticePressFailed); nerr != nil {
			r.logf("perms %s: surface failed press: %v", p.Conversation, nerr)
		}
		return fmt.Errorf("answer %s in %s: %w", promptID, p.Conversation, err)
	}
	if !ack.Attributed() {
		// The decision applied, but the audit line names nobody — worth a log
		// line, because an approval trail with a hole in it is the kind of
		// thing that is only ever noticed afterwards.
		r.logf("perms %s: %s recorded with no approver named", p.Conversation, d)
	} else {
		r.logf("perms %s: %s by %s", p.Conversation, d, ack.Approver)
	}
	return nil
}

// boundSession finds the session a conversation already has, without creating
// one. A press can only ever be an answer to a question switchboard posted
// into a live conversation, so a miss here means the session went away between
// the ask and the answer — which is a thing to report, not a session to open.
func (r *Router) boundSession(conv string) (*sessionEntry, error) {
	r.mu.Lock()
	e, ok := r.sessions[conv]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no live session for %s; the question it answers has expired", conv)
	}
	<-e.ready
	if e.err != nil {
		return nil, e.err
	}
	return e, nil
}
