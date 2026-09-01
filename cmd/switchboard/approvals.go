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

// setApprovers narrows who may answer a permission prompt. The zero policy is
// the open one, so a router nobody calls this on behaves as it did before the
// setting existed — which is also what the default preserves for a deployment
// upgrading into it.
func (r *Router) setApprovers(p approverPolicy) { r.approvers = p }

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
	body := promptText(p)
	if !e.claimAsk(p.ID, body) {
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

// decided phrases a decision in the past tense, for the record left on the
// question after somebody answers it.
//
// Derived from the decision switchboard validated, never from anything the
// press carried as text. The platform round-trips the button's own label and it
// would read better — it names the verb, the tool, the directory — but a line
// claiming what a person authorized is the wrong place to render a string that
// arrived from outside. What is lost is detail the question above it still
// carries.
func decided(d approval.Decision) string {
	switch d {
	case approval.Deny:
		return "🛑 **Denied**"
	case approval.AllowOnce:
		return "✅ **Allowed**, this once"
	case approval.AllowSession:
		return "✅ **Allowed** for the rest of this session"
	case approval.AllowSessionVerb:
		return "✅ **Allowed** for the rest of this session, for commands like this one"
	case approval.AllowSessionTool:
		return "✅ **Allowed** for the rest of this session, for this tool"
	case approval.AllowAlways:
		return "✅ **Allowed and saved**, across restarts"
	}
	// Unreachable: HandlePress validates before answering, and the daemon would
	// have rejected anything else. Named rather than blank so a decision added
	// to the vocabulary reads as unfamiliar instead of as nothing at all.
	return "**" + string(d) + "**"
}

// traceDecision edits the question to record how it ended, taking its buttons
// down with it.
//
// Called for the answer that lands and for the press that finds nothing left to
// answer, at the authority each of those carries: the record naming an approver
// outranks the one that names no decision, whichever press gets here first.
//
// A failure here is never reported as a failed press. The decision has already
// been applied by the time this runs — the agent is unblocked whatever the
// thread ends up showing — and reporting a bookkeeping failure as a failed
// press would invite a second press that answers nothing. What it does instead
// is fall back to saying the outcome beside the question, so that the one
// inbound action with no reply of its own still ends in something to read.
func (r *Router) traceDecision(ctx context.Context, e *sessionEntry, p chat.Press, promptID, outcome string, want settleState) {
	if p.Message.ID == "" {
		// The platform did not say which message the press came from, so there
		// is nothing to edit — but there is still something to say, and this is
		// the one inbound action with no reply of its own.
		r.sayHowItEnded(ctx, p.Conversation, promptID, outcome)
		return
	}
	// Claiming the record and writing it are one step; see sessionEntry.tmu.
	e.tmu.Lock()
	defer e.tmu.Unlock()

	body, known, ok := e.claimSettle(promptID, want)
	if !ok {
		if known {
			// Already recorded at least this firmly. The record on screen is
			// the better one, and a second edit saying less is the thing this
			// ranking exists to prevent.
			return
		}
		// A question this process has no record of: a session adopted after a
		// restart, or one whose set maxAskedPrompts cleared. Editing anyway
		// would replace the question with a bare verdict — the buttons would
		// come down, and what they were for would go with them. Say it beside
		// the question instead. The buttons stay live, which is why this says
		// something on every press rather than only the first: a dead control
		// that answers is better than a dead control that is silent.
		r.sayHowItEnded(ctx, p.Conversation, promptID, outcome)
		return
	}
	text := outcome
	if body != "" {
		text = body + "\n\n" + outcome
	}
	// Bounded, and detached from the press, because tmu is held across it: the
	// press context belongs to the adapter and outlives every individual press,
	// so a platform connection that hangs rather than failing would block every
	// later press on this conversation for as long as it hung.
	ectx, cancel := platformContext(ctx)
	defer cancel()

	// No Decision on the reply: the answers are gone because there is nothing
	// left to answer, and an adapter reading that takes its buttons down.
	err := r.out.Update(ectx, p.Message, chat.Reply{
		Conversation: p.Conversation,
		Text:         text,
		Kind:         chat.KindDecision,
	})
	if err == nil {
		return
	}
	if !errors.Is(err, chat.ErrUnsupported) {
		r.logf("perms %s: record decision on %s: %v", p.Conversation, promptID, err)
	}
	// The record could not go onto the question, so put it beside it. The claim
	// is kept rather than given back: the only press that can still reach this
	// question is one the daemon will answer 404, because the prompt it names is
	// spent — so giving the claim back does not buy a second chance at this
	// record, it buys "no longer pending" written over an applied decision that
	// named an approver.
	r.sayHowItEnded(ctx, p.Conversation, promptID, outcome)
}

// sayHowItEnded posts the outcome beside the question, for the presses that
// have no message to write it onto.
func (r *Router) sayHowItEnded(ctx context.Context, conv, promptID, outcome string) {
	if err := r.surfaceNotice(ctx, conv, outcome); err != nil {
		r.logf("perms %s: say how %s ended: %v", conv, promptID, err)
	}
}

// The things a press can be told, and the reason it has to be told anything: a
// press is the one inbound action with no reply of its own. The
// button flashes, the platform considers it delivered, and a person who is not
// told otherwise reasonably assumes the agent has been unblocked.
const (
	// noticeSettled replaces the question when a press arrives for something no
	// longer pending and no other press recorded an outcome — a prompt that
	// timed out, or one answered at the agent's own console. Deliberately does
	// not name a decision: switchboard does not know what was decided, and
	// guessing on an audit line is worse than saying so.
	noticeSettled     = "⏹️ **No longer pending** — this was answered elsewhere or it expired."
	noticePressFailed = "⚠️ That answer didn't reach the agent. It's still waiting — try pressing again."
	// noticeMaybeApplied is the same failure told honestly when the daemon
	// accepted the answer and then said something unreadable. Telling somebody
	// to press again would be wrong twice over: the prompt is spent, so the
	// retry finds nothing and the thread settles on "expired" over a decision
	// that took effect.
	noticeMaybeApplied = "⚠️ The agent took that answer but didn't confirm it. It may already be in force — check the agent rather than pressing again."
	noticeStalePress   = "⚠️ That question belongs to a session this thread no longer has, so the answer wasn't sent. If the agent is still waiting, it will ask again."
	// noticeNotApprover is the refusal switchboard makes itself, before the
	// daemon hears anything. It does not name who may answer instead: the list
	// is configuration, and reading it back to whoever presses hardest turns a
	// refusal into a directory.
	noticeNotApprover = "⛔ **Not an approver** — that answer wasn't sent. Someone on this gateway's approver list has to answer it."
)

// approversChannel is the --approvers value meaning "anyone who can post here".
const approversChannel = "channel"

// envApprovers is the environment alternative to --approvers. Named here rather
// than spelled at the flag, because runServe also has to notice it being set to
// nothing — which is not the same as it being unset.
const envApprovers = "SWITCHBOARD_APPROVERS"

// approverPolicy decides whether a press may answer a question at all.
//
// The open posture is a value rather than the absence of one, and that is the
// whole design. "Anyone in the conversation" is a real control, not a hole:
// channel and space membership is access control the platform already enforces,
// and a press only ever arrives from somebody the platform rendered the buttons
// to — there is no path by which a non-member's click reaches this process. What
// switchboard cannot see is how wide the room is. A public channel is the whole
// workspace; a Slack Connect channel or a Chat space with external members
// reaches outside the org; and an AllowAlways press writes a grant that outlives
// the session that raised it. So the open posture is something an operator
// spells, and can read back out of the process args, rather than something they
// arrive at by leaving a flag unset.
type approverPolicy struct {
	// allowed is the narrowed set, keyed by the identity switchboard asserts
	// (chat.CallerMode — an email by default, a platform ID under --caller-id)
	// and folded to lower case at both ends. Empty is the open posture.
	allowed map[string]bool
}

// open reports whether anyone who can post in the conversation may answer.
func (p approverPolicy) open() bool { return len(p.allowed) == 0 }

// allows reports whether this asserted identity may answer a prompt.
//
// An empty caller is refused under a narrowed policy and permitted under an open
// one, which is the right way round both times: a press switchboard cannot
// attribute is exactly the press a named list is there to exclude, while under
// the open posture attribution was never what granted it.
func (p approverPolicy) allows(caller string) bool {
	if p.open() {
		return true
	}
	return p.allowed[strings.ToLower(strings.TrimSpace(caller))]
}

// approverJunk is the punctuation that means an entry was never one identity.
// A space is the comma somebody forgot, a semicolon is the separator another
// tool uses, and angle brackets are a mail client's display-name form.
const approverJunk = " \t<>;"

// parseApprovers validates an --approvers value against the identity the
// gateway will actually assert.
//
// Folded to lower case because an email is not case-sensitive in any deployment
// this gateway sees, and a list that silently fails to match on capitalisation
// would fail closed in the least legible way available: the buttons work for
// nobody and the log says only that somebody is not an approver.
//
// Everything else here guards the same failure from the other direction. An
// unparseable list is not a syntax error — every entry this cannot recognise is
// simply an identity that will never match, so a stray semicolon, a display
// name, or a list of emails under --caller-id "id" starts cleanly, announces
// its approvers, and then refuses all of them forever. Startup is the only
// place that mismatch is visible, so it is refused here rather than discovered
// from a thread.
func parseApprovers(s string, mode chat.CallerMode) (approverPolicy, error) {
	allowed := make(map[string]bool)
	channel := false
	for _, f := range strings.Split(s, ",") {
		f = strings.ToLower(strings.TrimSpace(f))
		switch {
		case f == "":
			continue
		case f == approversChannel:
			channel = true
			continue
		case strings.ContainsAny(f, approverJunk):
			return approverPolicy{}, fmt.Errorf("%q is not one identity: separate approvers with commas", f)
		case mode == chat.CallerEmail && !strings.Contains(f, "@"):
			return approverPolicy{}, fmt.Errorf(`%q is not an email, and this gateway asserts emails: list emails, or run --caller-id "id"`, f)
		case mode == chat.CallerID && strings.Contains(f, "@"):
			return approverPolicy{}, fmt.Errorf(`%q is an email, and --caller-id "id" asserts platform IDs: list platform IDs, or drop --caller-id`, f)
		}
		allowed[f] = true
	}
	switch {
	case channel && len(allowed) > 0:
		// Refused rather than resolved either way: read as "these people plus
		// everyone" it is a list that narrows nothing, and read as a name it is
		// an approver called "channel". Neither is worth guessing at on an
		// authorization setting.
		return approverPolicy{}, errors.New(`"channel" cannot be combined with named approvers`)
	case channel:
		return approverPolicy{}, nil
	case len(allowed) == 0:
		return approverPolicy{}, errors.New(`names nobody: pass "channel" to let anyone in the conversation answer`)
	}
	return approverPolicy{allowed: allowed}, nil
}

// promptDetailLimit bounds the agent-controlled detail put in a message. The
// command or path is the whole substance of the question, so this is generous;
// it is here because Detail is unbounded and a megabyte of it in a thread
// helps nobody decide anything.
const promptDetailLimit = 1500

// promptNameLimit bounds the tool and the asking agent. Both are names, so this
// is already far past anything meaningful; it is here because they are
// agent-controlled and unbounded on the wire like Detail is, and because the
// question is retained until it is answered — an unbounded field interpolated
// into it is an unbounded field held for as long as the prompt is pending.
const promptNameLimit = 120

// promptText writes the question. It leads with the specifics — the command,
// the path — because that is what is being decided; the tool name and the
// asking agent are context for it.
//
// Everything it interpolates is clamped, so the result is bounded: see
// askRecord, which keeps it.
func promptText(p approval.Prompt) string {
	var b strings.Builder
	b.WriteString("**Permission needed**")
	if p.Tool != "" {
		b.WriteString(" — `" + clampRunes(p.Tool, promptNameLimit) + "`")
	}
	if detail := strings.TrimSpace(p.Detail); detail != "" {
		b.WriteString("\n\n```\n" + clampRunes(detail, promptDetailLimit) + "\n```")
	}
	if p.Source != "" {
		// Which agent is asking, when it is not the one being talked to. The
		// difference between approving something you just asked for and
		// approving something a subagent you forgot about wants.
		b.WriteString("\n_asked by the `" + clampRunes(p.Source, promptNameLimit) + "` subagent_")
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
	if !r.approvers.allows(p.Caller) {
		// Refused here, before the prompt is even located: a press switchboard
		// will not relay should not get to learn whether the question is still
		// live, or which session this thread is on. Not silent, though, and the
		// buttons stay up — somebody else in the room may be allowed to answer,
		// and a press that vanishes reads as one that worked.
		r.logf("perms %s: press by %q is not an approver", p.Conversation, p.Caller)
		return r.surfaceNotice(ctx, p.Conversation, noticeNotApprover)
	}
	sess, promptID, ok := splitDecisionRef(p.DecisionID)
	if !ok {
		return fmt.Errorf("press in %s names no prompt: %q", p.Conversation, p.DecisionID)
	}
	e, err := r.boundSession(p.Conversation)
	if err != nil {
		// Nothing bound under this conversation at all — most often a restart
		// under a thread whose buttons are still on screen, with nobody having
		// posted since to adopt the session back. The press cannot be answered
		// and the buttons stay live, so it is told the same thing a press for a
		// replaced session is told, for the same reason.
		r.logf("perms %s: press has no session to answer: %v", p.Conversation, err)
		return r.surfaceNotice(ctx, p.Conversation, noticeStalePress)
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
			// failure to report as one — the question is settled either way, and
			// saying so on the question is the whole of what is owed here. If
			// the press that settled it got there first, it has already written
			// a better record and claimSettle declines to replace it.
			r.logf("perms %s: %s is no longer pending", p.Conversation, promptID)
			r.traceDecision(ctx, e, p, promptID, noticeSettled, settledElsewhere)
			return nil
		}
		// Say so in the thread. A press is the one inbound action with no reply
		// of its own: the button flashes, the platform considers it delivered,
		// and without this the person who pressed it watches an agent stay
		// blocked with nothing anywhere to say the answer did not land.
		notice := noticePressFailed
		if errors.Is(err, approval.ErrMaybeApplied) {
			notice = noticeMaybeApplied
		}
		if nerr := r.surfaceNotice(ctx, p.Conversation, notice); nerr != nil {
			r.logf("perms %s: surface failed press: %v", p.Conversation, nerr)
		}
		return fmt.Errorf("answer %s in %s: %w", promptID, p.Conversation, err)
	}
	// Whoever the daemon says it recorded, never who the press said it was. The
	// two agree in every ordinary case; where they do not, the audit line the
	// backend wrote is the one that is true, and the thread should show that
	// one.
	//
	// Clamped and flattened first. It is a name off the daemon's JSON, as
	// unbounded on the wire as the question's own fields, and it goes onto an
	// audit line: an identity carrying a blank line and a bolded phrase would
	// render underneath the real verdict as a second one.
	approver := clampRunes(strings.Join(strings.Fields(ack.Approver), " "), promptNameLimit)
	outcome := decided(d) + " — " + approver
	if approver == "" {
		// The decision applied, but the audit line names nobody — worth a log
		// line, because an approval trail with a hole in it is the kind of
		// thing that is only ever noticed afterwards, and worth saying in the
		// thread for the same reason.
		r.logf("perms %s: %s recorded with no approver named", p.Conversation, d)
		outcome = decided(d) + " — _approver not recorded_"
	} else {
		r.logf("perms %s: %s by %s", p.Conversation, d, approver)
	}
	r.traceDecision(ctx, e, p, promptID, outcome, settledHere)
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
