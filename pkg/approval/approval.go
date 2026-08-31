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

// Package approval is switchboard's client for core-agent's permission
// broker: the seam that turns an agent blocked on "may I run this?" into a
// question a human in a chat thread can answer.
//
//	GET  /sessions/<app>/<sid>/perms/stream    -> SSE stream of pending prompts
//	POST /sessions/<app>/<sid>/perms/respond   -> the answer
//
// It is deliberately NOT part of pkg/daemon. That client speaks the four
// verbs core-agent has frozen, and every session has them; these two routes
// are optional — they exist only when the agent behind a session registered a
// prompt broker, and answer 501 when it did not. A separate package keeps
// that difference structural instead of leaving a caller to discover it from
// a status code.
//
// # Knowing before you ask
//
// The daemon says which sessions offer this on the capabilities frame that
// opens every /events stream, as daemon.FeaturePermsStream. Read it there.
// Probing with a request and reading the 501 works, but it spends a round
// trip to learn something that already arrived.
//
// # Who approved
//
// The approver is the caller switchboard asserts on the request, resolved by
// core-agent's own middleware — never a name in the body. Respond therefore
// takes the pressing human as an argument and puts them in the header, and
// reads back what the daemon recorded so a caller can tell an attributed
// approval from an anonymous one. See Ack.
//
// # Reconnecting
//
// There is no cursor on this stream: no seq, no since, no replay window. A
// reader that reconnects has not lost anything, because Subscribe on the far
// side seeds every new subscriber with the prompts still pending — the ones
// that matter, since a prompt nobody is waiting on is not worth redelivering.
// So reconnecting is resubscribing, and a caller may attach lazily, when a
// session reports it is blocked, without racing a prompt that arrived first.
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-steer/switchboard/internal/sse"
	"github.com/go-steer/switchboard/pkg/daemon"
)

// Client speaks the two permission routes. Safe for concurrent use.
type Client struct {
	cfg  daemon.Config
	http *http.Client
}

// New returns a Client for the same daemon, token and HTTP client as the
// four-verb client — there is one daemon and one credential, and asking a
// caller to describe it twice invites the two descriptions to disagree.
func New(cfg daemon.Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// No client-wide Timeout, ever: it bounds reading the body, and reading
	// the body is the whole of Stream — a 30s timeout on a shared client
	// would cut every prompt stream at thirty seconds and look like the
	// daemon hanging up. A caller's client is honoured for everything else
	// and copied with the deadline dropped, rather than rejected, because the
	// natural way to reach this constructor is to hand it the same
	// daemon.Config the four-verb client got. Respond does not lose anything:
	// it takes its deadline from its own context.
	hc := cfg.HTTPClient
	switch {
	case hc == nil:
		hc = &http.Client{}
	case hc.Timeout != 0:
		clone := *hc
		clone.Timeout = 0
		hc = &clone
	}
	return &Client{cfg: cfg, http: hc}, nil
}

// respondTimeout bounds one /perms/respond round-trip. Generous, because
// failing it means a human pressed a button and the agent stayed blocked.
const respondTimeout = 30 * time.Second

// Prompt is one pending permission request: a tool call the agent's gate
// stopped, waiting for someone to say what should happen to it.
type Prompt struct {
	// ID correlates this prompt with the Respond that answers it. Opaque;
	// generated per request and not reused.
	ID string

	// Kind is the shape of what is being asked, as a wire-stable string —
	// see the Kind constants. NEW VALUES ARE EXPECTED: core-agent has added
	// one already. Nothing may switch exhaustively on it, and an unrecognised
	// kind must still produce an answerable prompt, because the alternative
	// is an agent blocked forever on a question switchboard declined to ask.
	Kind string

	// Tool is the name of the tool being gated, e.g. "bash" — EXCEPT on
	// generic prompts raised by a namespaced toolset, where it is the policy
	// namespace ("mcp", "skill", "spawn_agent") and the underlying tool's name
	// never reaches this field. The daemon still scopes an AllowSessionTool
	// grant per underlying tool in that case, so this is the one field whose
	// value can be broader than the grant it appears to describe. Options
	// declines to interpolate it into a label on generic prompts for exactly
	// that reason.
	Tool string

	// Detail is the human-readable specifics — the command line, the path.
	// Agent-controlled text, and unbounded: clamp it before display.
	Detail string

	// Verb is the leading command word of a bash invocation — "git" for
	// "git push origin main", and also for "GIT_DIR=/x git push", since the
	// gate steps over leading KEY=VAL assignments to find it. Note how wide
	// that is: AllowSessionVerb on this prompt authorizes every `git *` for
	// the session, not every `git push`, so anything that describes the grant
	// to a human must say so.
	//
	// EMPTY WHEN THE GATE COULD NOT EXTRACT ONE (path scripts, quoted
	// commands, anything that is not a bash prompt), which is why Options
	// suppresses the verb-scoped decision rather than offering a button that
	// would widen nothing.
	Verb string

	// Source is which agent is asking, when it is not the one the human is
	// talking to: empty for the parent agent's own tool calls, and the
	// subagent's name (e.g. "watch-prod-cluster") when a background subagent
	// triggered the prompt. Worth surfacing — it is the only field that tells
	// the reader whether the thing asking to run this is the agent in front of
	// them or one they forgot they had spawned.
	Source string

	// PersistTool and PersistKey identify what an AllowAlways would write to
	// the daemon's grant store.
	//
	// NOT, on a path-scope prompt, the thing that gets granted. There the
	// daemon widens PersistKey to the enclosing directory tree before storing
	// it — the path itself if it is a directory, otherwise its parent — so a
	// prompt naming one file persists a rule covering every sibling of that
	// file. See Options, which is why the standing-grant label on that kind
	// does not name the path.
	PersistTool string
	PersistKey  string

	// Access is the file operation being requested, in the daemon's short
	// form: "r", "w" or "none" ("rw" is in the daemon's vocabulary but no gate
	// currently raises a prompt carrying it). It is NOT empty on prompts that
	// have no meaningful access mode — those carry "none", because the daemon
	// renders the mode before serializing and the zero value stringifies. A
	// caller testing for "" to decide whether to show it will show it always,
	// and one comparing against "read" or "write" will never match.
	//
	// An AllowAlways on a write prompt persists read AND write, not write:
	// the daemon promotes the mode on the way to the store, on the reasoning
	// that everything which writes a file also reads it back.
	Access string

	// At is when the gate raised the prompt, UTC.
	At time.Time
}

// The prompt kinds core-agent ships today. This list is not exhaustive and
// must not be treated as one; it is here so the common cases can be
// recognised, not so the uncommon ones can be rejected.
const (
	KindBash              = "bash"
	KindFileWrite         = "file_write"
	KindPathScope         = "path_scope"
	KindControlPlaneWrite = "control_plane_write"
	KindGeneric           = "generic"
)

// Decision is one of the six answers /perms/respond accepts. The zero value
// is not one of them: an unset decision must never read as an approval.
type Decision string

// The decision vocabulary, in increasing order of what it grants.
const (
	// Deny refuses this one call. The tool call fails; the turn continues.
	Deny Decision = "deny"

	// AllowOnce permits exactly this call and nothing after it.
	AllowOnce Decision = "allow-once"

	// AllowSession permits this exact request for the rest of the session.
	AllowSession Decision = "allow-session"

	// AllowSessionVerb permits every command starting with this prompt's verb
	// for the rest of the session. The verb is a bash command's leading word,
	// so approving one `git push --force` this way also approves every
	// `git reset --hard` and `git config` that follows. Only meaningful when
	// the gate extracted a verb.
	//
	// The grant fires only for a later command the daemon can parse as a
	// single simple command: `git status; rm -rf /` still prompts, because a
	// chained command is not one the verb describes. Narrower than the
	// sentence above, in other words — which is the safe direction for a doc
	// whose job is to keep a UI from understating what a press does.
	AllowSessionVerb Decision = "allow-session-verb"

	// AllowSessionTool permits the whole tool for the rest of the session.
	// "The tool" is the underlying one the human was shown, even where the
	// prompt's Tool field carries only a namespace — see Prompt.Tool.
	AllowSessionTool Decision = "allow-session-tool"

	// AllowAlways persists the grant to the daemon's store, so it outlives
	// the session and the process. Its blast radius is the daemon, not the
	// thread the button was in: one press in a shared channel is a standing
	// grant for everyone who can reach that daemon afterwards.
	//
	// On a path-scope prompt it is wider still, in two ways the prompt does
	// not show: the daemon stores the enclosing DIRECTORY TREE rather than the
	// path named, and promotes a write to read-write. One press on a prompt
	// reading "write /home/u/.ssh/authorized_keys" persists read and write
	// over all of /home/u/.ssh. Anything putting this on a button for that
	// kind has to say so; see Options.
	AllowAlways Decision = "allow-always"
)

// Decisions is the whole vocabulary, in the order declared above — increasing
// in what it grants.
//
// Exported so that a caller which has to be exhaustive over the set can be
// exhaustive over *this* set rather than over a list it keeps in step by hand.
// A gateway rendering a decision needs a phrase for every one of them; the
// difference between iterating this and copying the six out is whether adding a
// seventh is caught by a test or discovered in a thread.
//
// Returns a fresh slice, so a caller cannot quietly edit the vocabulary.
func Decisions() []Decision {
	return []Decision{Deny, AllowOnce, AllowSession, AllowSessionVerb, AllowSessionTool, AllowAlways}
}

// Valid reports whether d is one of the six. Checked before sending so a
// typo is a local error rather than a 400 discovered a round trip later.
func (d Decision) Valid() bool {
	for _, v := range Decisions() {
		if d == v {
			return true
		}
	}
	return false
}

// Allows reports whether the decision lets the call proceed.
//
// Written as a list of the values that do, rather than as "anything but
// Deny", so that everything this package does not recognise — the zero value,
// a button id that arrived mangled, a decision a future daemon invents —
// fails closed. The inverse reads more naturally and is wrong in the one
// direction that matters: it would report an unset decision as an approval.
func (d Decision) Allows() bool {
	switch d {
	case AllowOnce, AllowSession, AllowSessionVerb, AllowSessionTool, AllowAlways:
		return true
	}
	return false
}

// Option is one answer offered for a specific prompt: the value to send back
// and the label to put on it.
type Option struct {
	Decision Decision

	// Label is button-sized and already names the specifics — "Allow every
	// commit this session" rather than "allow-session-verb" — because the
	// person pressing it is reading the button, not the vocabulary.
	Label string

	// Broad marks a decision whose effect outlives the request: the rest of
	// the session, or (for AllowAlways) the daemon. Callers that want to
	// style the wide answers differently, or hide them from a channel where
	// anyone can press them, can do it on this rather than by re-deriving
	// which values are wide.
	Broad bool
}

// labelCap bounds the agent-supplied fragment interpolated into a label.
// Chat platforms cap button text — Slack at 75 characters — and a tool name
// long enough to blow that cap would turn every option into an API rejection.
const labelCap = 24

// Options returns the answers worth offering for this prompt, in increasing
// order of what they grant, so the narrowest answer is the first one under
// the reader's eye and the standing grant is last.
//
// Answers are withheld where the daemon accepts them but they grant something
// other than what their label claims. A button whose press means less than it
// says is worse than an absent button; one that means MORE is worse than both,
// and is why the standing grant on a path-scope prompt is relabelled rather
// than left to read as if it covered the one path named.
//
//   - AllowSessionVerb, when the prompt carries no verb. There is nothing to
//     scope it to, so it widens nothing. Core-agent's reference client hides
//     it on the same condition.
//   - Every answer wider than AllowOnce, on a control-plane write. That gate
//     records ANY non-deny answer as allow-once and deliberately remembers
//     nothing, so the elevated prompt returns on the next such write. Offering
//     "Always allow (saved)" there would tell someone they had installed a
//     standing grant on the file that governs the agent's own permissions,
//     when they had approved exactly one write and nothing was saved.
//   - AllowSession and AllowSessionTool on a path-scope prompt. The gate that
//     raises those consults neither grant on the way back in, so both leave
//     the same out-of-scope path prompting exactly as before. AllowSessionTool
//     is the worse of the two: it is inert for the path that was shown, but it
//     is NOT inert generally — the file-write gate does read it, so approving
//     an out-of-scope write this way silently stops the prompting for every
//     IN-scope write by that tool. Wrong in both directions at once.
//
// That leaves a path-scope prompt with deny, once, and always — which is
// exactly the set whose members do what they say there.
//
// A prompt of a kind this package has never heard of gets the full set. The
// suppression above is a set of facts about specific gates; absent that
// knowledge the daemon's own vocabulary is the better guess, and the failure
// it risks — an offered answer that turns out to be narrower than its label —
// is the milder one next to leaving a blocked agent nothing but deny.
func Options(p Prompt) []Option {
	opts := []Option{
		{Decision: Deny, Label: "Deny"},
		{Decision: AllowOnce, Label: "Allow once"},
	}
	if p.Kind == KindControlPlaneWrite {
		return opts
	}
	if p.Kind == KindPathScope {
		// Named for the tree it actually persists, not the path in the
		// prompt. "Saved" alone would be true and still mislead: the reader
		// has one path in front of them and no reason to think the button
		// under it means the directory holding it.
		return append(opts, Option{
			Decision: AllowAlways,
			Label:    "Always allow this directory",
			Broad:    true,
		})
	}
	opts = append(opts, Option{Decision: AllowSession, Label: "Allow for this session", Broad: true})
	if p.Verb != "" {
		opts = append(opts, Option{
			Decision: AllowSessionVerb,
			Label:    "Allow every " + clip(p.Verb) + " this session",
			Broad:    true,
		})
	}
	opts = append(opts,
		Option{Decision: AllowSessionTool, Label: toolLabel(p), Broad: true},
		Option{Decision: AllowAlways, Label: "Always allow (saved)", Broad: true},
	)
	return opts
}

// toolLabel names the tool an AllowSessionTool would trust, when the prompt
// carries a name that means what it looks like.
//
// On a generic prompt it does not, and the label says "this tool" instead: a
// namespaced toolset reports its namespace as the tool, so interpolating it
// yields "Allow every mcp this session" for a grant covering one MCP tool. The
// underlying name is recoverable from the head of Detail, but only by parsing
// prose the daemon formats for humans, and a label that is occasionally wrong
// about scope is worse than one that is always vague about identity.
func toolLabel(p Prompt) string {
	if p.Tool == "" || p.Kind == KindGeneric {
		return "Allow this tool all session"
	}
	return "Allow every " + clip(p.Tool) + " this session"
}

// clip bounds an agent-supplied fragment for use inside a button label.
func clip(s string) string {
	r := []rune(s)
	if len(r) <= labelCap {
		return s
	}
	return string(r[:labelCap-1]) + "…"
}

// ErrNotSupported reports that the agent behind this session registered no
// prompt broker, so these routes do not exist for it. Not a failure — a
// session that never asks permission has nothing to answer — which is why it
// is a distinct error and not a StatusError a caller has to inspect.
var ErrNotSupported = errors.New("approval: session does not offer permission prompts")

// ErrNotFound reports that what the request addressed is gone. Which thing
// depends on the call, and the daemon answers 404 for both, so a caller that
// needs to tell them apart must look at the route rather than the sentinel:
//
//   - From Respond, the prompt: already answered, cancelled with its turn, or
//     never issued. Usually a race worth handling gracefully — two people
//     pressed a button, or one pressed after the agent gave up waiting — so
//     the answer is to say the question is closed, not to retry.
//   - From Stream, the session, since there is no prompt id in that request
//     at all. That is the same "the session went away" the four verbs report,
//     and it wants the same handling: stop, and say so.
var ErrNotFound = errors.New("approval: the prompt or session addressed is gone")

// ErrMaybeApplied reports a failure of Respond that must not be read as
// "nothing happened". The daemon accepted the request and then said something
// unusable, so the decision has very likely taken effect; what did not survive
// is the confirmation of it, which is the part naming who it was recorded on.
//
// A distinct sentinel because the honest thing to tell a person differs. The
// ordinary failure is "that did not reach the agent, press again" — advice that
// is actively wrong here, since the prompt is spent and the retry will come
// back ErrNotFound, leaving nothing to say afterwards but that the question
// expired.
var ErrMaybeApplied = errors.New("approval: the decision may have been applied")

// Stream delivers pending permission prompts for a session until ctx is
// cancelled, the daemon closes the stream, or fn returns an error — which is
// returned unwrapped, so a caller can stop on its own sentinel.
//
// Returns ErrNotSupported if the session has no broker, and ErrNotFound if
// the daemon has no such session — there is no prompt id in this request, so
// a 404 here is about the session and wants the same handling as a session
// that vanishes from under the four verbs. A clean end of stream is a nil
// error: the daemon closes it on shutdown and on the agent finishing, and
// neither is a failure. There is no keep-alive on this stream, so a caller
// holding one open across an idle period should expect an intermediary to
// eventually cut it and be ready to resubscribe — which costs nothing, per the
// package comment.
//
// assertedCaller is the identity switchboard claims for the read. It does not
// attribute anything; only Respond does that.
func (c *Client) Stream(ctx context.Context, sess daemon.Session, assertedCaller string, fn func(Prompt) error) error {
	req, err := c.request(ctx, http.MethodGet, sess, "stream", assertedCaller, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.statusErr(resp)
	}

	return sse.Scan(resp.Body, func(e sse.Event) error {
		// One event name is defined on this stream. Anything else is a
		// keep-alive or a frame from a later protocol, and skipping it is how
		// this client survives the daemon growing one.
		if e.Type != eventPrompt {
			return nil
		}
		var f promptFrame
		if err := json.Unmarshal([]byte(e.Data), &f); err != nil {
			// A frame that will not parse cannot be answered, and cannot be
			// correlated to say so. Dropping it leaves the agent blocked
			// until its own context expires, which is the same place a
			// missing subscriber leaves it — whereas failing the stream also
			// abandons every prompt that would have come after.
			return nil
		}
		if f.ID == "" {
			return nil
		}
		return fn(f.prompt())
	})
}

// Ack is what the daemon recorded for an answered prompt.
type Ack struct {
	// Approver is the identity core-agent attributed the decision to, as
	// resolved by its own caller middleware. EMPTY IS A REAL ANSWER: the
	// decision took effect, but the approval log will not name anyone —
	// switchboard asserted no caller, or the daemon is not configured to
	// trust the assertion. Worth surfacing, because an approval nobody is
	// named on is exactly what the audit trail exists to prevent.
	Approver string
}

// Attributed reports whether the daemon recorded who approved.
func (a Ack) Attributed() bool { return a.Approver != "" }

// Respond answers a prompt as approver, releasing the blocked tool call.
//
// approver is asserted in the header, where core-agent's caller-resolution
// middleware can see it and decide whether to believe it. The wire format has
// a field for it in the body too; this client never sets it. That field exists
// only so a client's claim can be CHECKED against the verified caller, and a
// disagreement is a 400 — it can never widen what gets recorded, so sending it
// can only turn a working request into a rejected one.
//
// Returns ErrNotFound if the prompt is no longer pending and ErrNotSupported
// if the session has no broker. An invalid decision is rejected locally.
func (c *Client) Respond(ctx context.Context, sess daemon.Session, approver, id string, d Decision) (Ack, error) {
	if id == "" {
		return Ack{}, errors.New("approval: prompt id is required")
	}
	if !d.Valid() {
		return Ack{}, fmt.Errorf("approval: %q is not a decision this daemon accepts", string(d))
	}

	ctx, cancel := context.WithTimeout(ctx, respondTimeout)
	defer cancel()

	body, err := json.Marshal(respondRequest{ID: id, Decision: string(d)})
	if err != nil {
		return Ack{}, fmt.Errorf("approval: marshal request: %w", err)
	}
	req, err := c.request(ctx, http.MethodPost, sess, "respond", approver, body)
	if err != nil {
		return Ack{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Ack{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Ack{}, c.statusErr(resp)
	}
	var out respondResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// The decision has probably already taken effect — the daemon said
		// 2xx before this body went wrong — but what came back is unreadable,
		// and the one thing it carries is who the approval was recorded on.
		// Reporting that as an empty Approver would be indistinguishable from
		// the daemon telling us it recorded nobody, which is the distinction
		// this whole path exists to preserve. So it is an error, and the
		// caller is warned not to read the failure as "nothing happened".
		return Ack{}, fmt.Errorf("%w: the daemon's reply was unreadable: %w", ErrMaybeApplied, err)
	}
	if !out.Acknowledged {
		// A 2xx that declines to acknowledge contradicts itself. No daemon
		// sends this today, and the reason to check is that the alternative
		// is reporting it as a successful anonymous approval — telling a
		// thread the call was released when the only thing that said so was
		// the status line.
		return Ack{}, fmt.Errorf("%w: the daemon returned success but did not acknowledge it", ErrMaybeApplied)
	}
	return Ack{Approver: out.Approver}, nil
}
