# switchboard — chat-gateway companion for core-agent

**Status:** design / scaffolding (2026-08-06) · **Target:** core-agent v2.9

Switchboard is the **chat front door** for [core-agent](https://github.com/go-steer/core-agent).
It bridges chat platforms — Slack first, Google Chat second — onto the frozen
core-agent daemon contract so operators can drive agents from a thread.

It is workstream **W1** of the umbrella epic *"replace Hermes as the full
kube-agents runtime"* — see
[`docs/hermes-replacement-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/hermes-replacement-design.md)
in core-agent for the full W0–W6 picture. This doc is scoped to switchboard
itself.

## 1. Why a separate companion

Core-agent is the distroless *brain*: daemon, sessions, MCP tool-calling,
subagents. It deliberately ships **no** chat integration. Rather than fork chat
adapters into the brain (and drag Slack/Google SDKs, OAuth, and webhook servers
into the distroless image), switchboard follows the **"one contract, many
companions"** pattern already proven by
[k8s-lookout](https://github.com/go-steer/k8s-lookout): a small, independently
released sidecar that speaks only the daemon's HTTP contract.

Consequences:

- **Independent build track.** Switchboard depends only on the *frozen, shipped*
  daemon contract, so it can be built in parallel with the in-daemon
  credential work (W0). The two meet at the already-shipped `X-Asserted-Caller`
  seam — no blocking dependency in either direction.
- **Own release cadence & image.** `ghcr.io/go-steer/switchboard`, its own
  semver, its own CVE surface — the brain stays lean.
- **Still distroless.** Same `gcr.io/distroless/static-debian12:nonroot` posture
  as the brain and k8s-lookout.

## 2. The daemon contract switchboard speaks

Four verbs, all already shipped by core-agent (`pkg/daemon`):

| Verb | Endpoint | Purpose |
|------|----------|---------|
| create | `POST /sessions` | open a session for a new conversation; returns `session_id` |
| inject | `POST /sessions/<sid>/inject` | queue a user turn on the session inbox **and run it** |
| wake | `POST /sessions/<sid>/wake` | run a turn with no new input to give the session |
| stream | `GET /sessions/<sid>/events` (SSE) | read the session's output turns |

Inject requests a wake itself, so the two are alternatives, not a pair: an
inbound chat message is one inject and nothing else. Sending both signals the
session twice and runs a second, empty-prompted turn whose reply duplicates the
first in the thread.

Auth is a static `Bearer` token (switchboard → daemon). **Per-turn attribution**
rides `X-Asserted-Caller`: switchboard sets it to the platform identity of the
human who sent the message; the daemon stamps it as session Owner and resolves
per-caller MCP credentials from it (W0). Switchboard must be listed in the
daemon's `attach.multi_session.proxy_identities` for the assertion to be
honored.

### 2.1 Optional routes, and knowing before you ask

Two more endpoints exist for *some* sessions — the permission broker that turns
a blocked tool call into a question (`pkg/approval`, #40):

| Route | Purpose |
|-------|---------|
| `GET /sessions/<app>/<sid>/perms/stream` (SSE) | prompts the agent is blocked on |
| `POST /sessions/<app>/<sid>/perms/respond` | the answer, releasing the call |

They are not a fifth and sixth verb, and they are deliberately not in
`pkg/daemon`. Every session has the four; these two exist only when the agent
behind a session registered a prompt broker, and answer `501` when it did not.
Keeping them in a separate package makes that optionality structural rather than
something a caller discovers from a status code.

Which sessions have them is already on the wire. The capabilities frame that
opens every `/events` stream carries a feature map, and `perms_stream` is a key
in it — so `daemon.Capabilities.Offers` answers the question from a frame
switchboard was reading anyway, and nothing has to spend a request to be told
`501`. That is also why a `501` is classified as terminal rather than transient:
it is the one `5xx` that describes what was built rather than how the daemon is
feeling, and a retry loop against it never ends.

**Who approved is a header, not a field.** `/perms/respond` fills its audit line
from the caller the daemon verified — the same `X-Asserted-Caller` seam the four
verbs use — and the body's `approver` field exists only so a client's claim can
be *checked* against that verdict. It can never widen what is recorded; it can
only disagree and earn a `400`. Switchboard therefore asserts the pressing human
in the header and omits the field entirely. The response echoes what was
recorded, which is the part that matters for a shared channel: an approval the
daemon could attribute to nobody has to be distinguishable from one it could,
and both arriving as silence is precisely the failure an audit line exists to
prevent.

The prompt stream has no cursor — no seq, no `since`, no replay window — and
needs none, because the daemon seeds each new subscriber with the prompts still
pending. A prompt nobody is waiting on is not worth redelivering. So
reconnecting is just resubscribing, and a watcher that attaches after the gate
has already stopped a call still receives it.

That seeding has a cost the daemon cannot pay for switchboard: *every*
resubscription redelivers everything still pending, and a question nobody has
got round to answering is exactly the kind that is still pending. An idle SSE
stream is cut routinely, so without a record of what has already been posted the
same question accumulates one copy per reconnect, each with its own live
buttons. The router keeps that record per session and claims a prompt id before
posting it, giving the claim back if the post fails — the next reconnect's
seeding is the only retry there is, and holding a claim for a question that
never reached the thread would turn a transient posting failure into an agent
blocked on a question nobody was shown. The set is bounded; overflowing it
clears it rather than refusing to ask, because a duplicate question is
recoverable and a silently dropped one is not.

### 2.2 Why the capability, and not the turn state

The obvious trigger for subscribing is the moment a session says it is blocked:
hold one stream instead of two, and open it only for the sessions that ever ask.
The daemon's turn-state vocabulary even has the word for it,
`awaiting_permission`.

It emits it nowhere. The constant that names it is its only occurrence in the
whole tree. A gateway that waits for it relays no prompt, ever, on any build
shipping today — the feature would be dead code that passes its own tests.

So the question switchboard asks is "*can* this session ask?", which the
capabilities frame answers on every connection, rather than "is it asking?".
The cost is a second SSE connection per session whose agent registered a broker,
and it is bounded by the sessions that could have raised a prompt at all.
Nothing is lost by attaching before there is anything to see, because of the
seeding above — and nothing is doubled either, because of the record of what has
been asked that goes with it. The watch is claimed once per session no matter
how often the event stream reconnects; without that, a flapping relay leaves a
watcher behind on every pass, each holding a stream nobody closes.

### 2.3 The press seam

A decision is platform-neutral: a `Reply` may carry one, an adapter renders it
however its platform can, and presses come back through `Handler.HandlePress`.
That method is on the interface rather than in an optional capability, so an
adapter that renders buttons cannot be paired with a handler with nowhere to
send them — a person clicking and watching nothing happen is a worse failure
than the button never appearing. The answers are also written into the message
text, so a platform that renders no interactive surface still says what the
choices are and somebody can act on it another way.

Two things about a press are load-bearing. It is attributed to whoever made it,
resolved from the platform's own callback and never from the message being
answered — switchboard posted that message, and the whole point is that a
*person* replied to it. And it resolves to a session the conversation already
has, never a new one: a press can only ever answer a question posted into a live
conversation, so a miss means the session went away in between, and a fresh
session holds none of the pending prompts the old one did.

A press is also the one inbound action with no reply of its own. The button
flashes, the platform considers it delivered, and nothing in the thread says
what happened — so switchboard edits the question to record how it ended and who
ended it, and the edit takes the buttons down. An edit is `KindDecision` with no
`Decision` on it: there is nothing left to answer, and an adapter reading that
must dismantle whatever interactive surface it built for the original. A button
that survives the decision it was for is worse than one that never rendered.

Three things about that record. It keeps the question above it, because a line
reading "Allowed once by ana@example.com" with the command it allowed gone is
not an audit line. It is phrased from the decision switchboard validated, never
from the label the platform round-tripped — a claim about what a person
authorized is the wrong place to render a string that arrived from outside — and
it names the approver the backend says it recorded rather than the one the press
asserted.

And it is ranked rather than first-come. Two people can press at the same
moment; only one answer reaches a pending prompt, and the other comes back "no
longer pending", which is true and says nothing about who decided what. Those
are two independent round trips and either can return first, so the record that
names an approver outranks the one that does not and replaces it if it arrives
second. Claiming the right to write a record and writing it are one step, held
under a per-conversation lock, so the order the thread ends up in is the order
the records were claimed in and not the order two edits happened to land.

The record cannot always go onto the question. A press may name a question
switchboard has no record of asking — a session adopted across a restart, or one
the bounded record dropped — and then there is no body to write it under, and
replacing the question with a bare verdict would take the buttons down along with
what they were for. The platform may not say which message the press came from.
The edit may fail, or the adapter may not support editing at all. In every one of
those the outcome is posted *beside* the question rather than onto it, because a
press has to end in something to read.

An edit that failed keeps its claim rather than giving it back. Giving it back
looks like the generous choice — the buttons are still up, so let a later press
write the record instead — but the prompt behind that question is spent, so the
only answer a later press can get is "no longer pending", and the record it
would write is *weaker* than the one that was lost. Better to say the real
outcome beside the question once than to let an applied, attributed approval be
overwritten with "expired".

A press with no session bound under its conversation at all is refused the same
way a press for a replaced session is, and told so: nothing repopulates the
session map at startup, so a restart leaves live buttons over a thread that
cannot answer them until somebody posts in it again.

The one thing a press must never be told is "try again" when trying again would
make things worse. A daemon that accepts the answer and then returns something
unreadable has probably applied the decision and lost only the confirmation of
it, so `pkg/approval` reports that as `ErrMaybeApplied` rather than as an
ordinary failure. A retry there finds the prompt spent, and the thread would end
up recording "no longer pending" over a decision that took effect — so the
thread is told to go and look at the agent instead.

Relaying is off unless `--approvals` is set, and that default is not caution
about a young feature. Turning it on means, by default, that anyone who can post
in a conversation can answer its permission prompts, and some of those answers —
`allow-always` above all — outlive the request that raised them. That is a grant
an operator makes deliberately, per deployment, with the channel's membership in
mind.

Membership is the reason the default is defensible rather than lax. A press only
ever arrives from somebody the platform rendered the buttons to, over a
connection the platform authenticated, so channel membership is access control
already enforced upstream and switchboard needs no membership call to lean on
it. What it cannot see is how wide the room is: a public channel is the whole
workspace, and a Slack Connect channel reaches past the org entirely.
`--approvers` is the narrowing — a comma-separated list of the identities
switchboard asserts, matched case-insensitively against `Press.Caller`, the same
string that goes out as `X-Asserted-Caller`. Its default is the literal
`channel`, so the open posture is a value an operator can spell rather than an
omission nobody notices.

A list that cannot match is the failure mode worth designing against, because it
is indistinguishable at runtime from a list that is working: every entry the
gateway will never see is simply an approver who never presses, so the deployment
starts cleanly, announces its approvers, and refuses all of them. So the list is
validated at startup against the caller mode the same run selected — emails under
`--caller-id "id"` are refused, as are platform IDs under `email`, along with the
punctuation that means an entry was never one identity. `SWITCHBOARD_APPROVERS`
set to the empty string is refused for the same reason and not read as unset:
that is how an absent ConfigMap key renders, and resolving it to `channel` would
widen the grant at the moment somebody was trying to set it.

The same reasoning reaches back into the Slack adapter. `resolveCaller` falls
back to the raw user ID when `users.info` fails, which used to cost an odd audit
identity and now decides an authorization question, so that fallback is no longer
cached: one rate-limit response would otherwise pin an approver to their user ID
for the life of the process.

The gate runs in `HandlePress` before the prompt is looked up, so a press this
gateway will not relay cannot learn whether the question is still live or which
session the thread holds. A refusal is posted, not swallowed — a press that
vanishes reads as one that worked — and the buttons are left standing, because
someone else in the room may be entitled to answer. There is no value meaning
"nobody": leaving `--approvals` off already means that. Being in the router
rather than an adapter, it is a Slack control only for as long as Slack is the
only platform delivering a press: the add-on framework routes no click trigger
to a Chat app (§3.3), so `--approvers` narrows nothing there until the HTTP
interaction endpoint lands (#29, designed in §3.4), and covers it with no
further work when it does.

None of this is the backend's authorization moving here. Switchboard is gating a
surface it invented, on the identity it already asserts; core-agent still decides
what that caller may *do* with the decision, and is free to refuse it again.

## 3. Architecture

```
   Slack  ──Socket Mode──┐                        ┌─ POST /sessions
                         │   ┌───────────────┐    │
                         ├──▶│  switchboard   │───▶├─ POST /sessions/<sid>/inject
                         │   │  router        │    └─ GET  /sessions/<sid>/events (SSE)
   Google Chat ─Pub/Sub──┘   └───────────────┘         │
                                     ▲                   │
                                     └───── replies ─────┘
                                     (X-Asserted-Caller = chat user)
```

- **outbound ingress** (`cmd/switchboard`, optional, `--ingress-addr`) — the
  same adapter egress reached from outside instead of from a thread. See §3.1.
- **`pkg/chat`** — provider-neutral `Adapter` interface (ingress + egress) plus
  the normalized `Message` / `Reply` types. Slack and Google Chat each implement
  it under `pkg/chat/slack` and `pkg/chat/googlechat`.
- **`pkg/daemon`** — the thin wire client for the four verbs above.
- **router** (`cmd/switchboard`) — owns the **conversation → session** map
  (a Slack channel+thread or a Google Chat space+thread ⇒ one session), forwards
  inbound turns via inject, subscribes to the SSE stream, and relays output
  turns back through the adapter.

### 3.1 Outbound ingress

Every reply above echoes an inbound event's conversation, so switchboard can
only post into a thread chat created. The outbound ingress is the other
direction: an authenticated HTTP surface — off unless `--ingress-addr` is set —
that lets another service post into a conversation with no inbound event to
reply to, and edit that message afterwards.

```
POST  /v1/messages  {"conversation":"C0123","text":"…"}  → 200 {"conversation","id"}
PATCH /v1/messages  {"conversation","id","text"}         → 204   (501 if the platform cannot edit)
PATCH /v1/messages  {"conversation","id","append":"…"}   → 204, or 200 {"conversation","id"} on rollover
```

It is deliberately the *mechanism already there*: `chat.Adapter`'s
`Send`/`Update` with a `MessageRef`, the same pair the router uses for
long-turn progress edits (`startProgress`/`clearProgress`). No `platform`
field — the instance's `--platform` implies the target. Its bearer token is
separate from the daemon token (different direction, different trust), a
conversation allowlist can confine it, and an `Idempotency-Key` keeps a
caller's retry from double-posting.

**Append.** `text` replaces; `append` adds a line. The distinction earns its
keep because an incident timeline grows a line at a time, and making the caller
resend a 40-line body to add one makes the caller the owner of state it did not
want. Slack has no append call — `chat.update` takes the full replacement — so
somebody must hold the current text. Reading it back would need a
`channels:history` scope this bot deliberately does not have (and would let a
human's edit in the Slack UI be silently clobbered), so switchboard remembers
it: a bounded map (last 1024 messages) of the text of messages *it* posted,
keyed by channel and message id. An append to anything else answers `409`
"send the full text" — a restart, another replica, or a message from elsewhere
all land there, and the caller's fallback is the `text` it already has. This
is the first state the ingress keeps; it is deliberately a cache, and losing
it degrades to `409` rather than to a wrong edit.

The limit case is the one that would otherwise corrupt the record: when the
combined text no longer fits one message, switchboard posts the addition as a
reply in the same thread and answers `200` with the continuation's ref, which
the caller appends to from then on. Truncating would drop the *oldest* lines of
a timeline, which is exactly the part an incident review wants. Whether a text
fits is the platform's business, so it is asked through an optional
`chat.TextFitter` capability rather than by widening `Adapter`; a provider that
does not implement it answers `501` to `append` and keeps `text` working.
Messages the adapter had to split across several posts are *not* remembered —
the ref names only the first part, so appending would edit a fragment.

The motivating caller is core-sre-agent's monitoring loop, whose escalations
run for minutes: post once, then edit as each assessment lands. Post-then-edit
is the valuable half, not an extra. This does not weaken §5's "no scheduling" —
the scheduling stays entirely with the caller; switchboard supplies transport.
It is also orthogonal to the inbound story: however a companion comes to speak
the daemon contract, a scheduled digest still has no inbound event to reply to.

**The allowlist is the whole authorization model.** A caller holding the bearer
token may `PATCH` *any* message the bot posted in an allowlisted conversation,
including replies the router sent on a human's behalf — the ingress does not
track which messages came from which caller, and Slack does not distinguish
them. That is acceptable while the token is a single trusted service's, and it
is why `--ingress-allow` should name the conversations rather than being left
open. Per-caller ownership would need caller identity on the ingress, which is
the same design as correlating outbound posts with inbound replies (below) and
is deferred with it.

The ingress works on both platforms (#39). It speaks `chat.Reply` and
`chat.MessageRef` to whichever adapter `serve` built and knows nothing about
either one; what was platform-specific was the *key* it constructs when an
appended message overflows. It named the continuation's thread as
`"<conversation>:<id>"`, which is right only on Slack, where a top-level
message's id is the `thread_ts` of the thread it roots. On Chat an id is a
message resource name, and the key came out malformed.

The invariant that replaces it is symmetric: **a ref names the thread its
message landed in**, on both platforms. Each adapter knows its own rule — Slack
can derive the thread from the ts it just got back, Chat has to read the thread
the platform assigned — and the ingress follows the ref rather than
reconstructing it. A caller that posted to a bare channel or space is answered
with the threaded key and follows up with that; the ingress remembers it too, so
an append addressed by the bare key still continues in the right place.

Ingress and egress are also separable at deployment (#23). A digest pipeline
that posts and never listens should not have to hold the credentials receiving
takes — a Slack app-level token and the event subscriptions that come with one,
a Pub/Sub subscription and a subscribe grant — nor tie its uptime to a
connection it never reads. So `--outbound-only` selects the mode: without it,
`serve` runs the adapter; with it, `serve` builds an egress-only adapter, skips
the router and the daemon client entirely, and waits on the same context `Run`
would have. `chat.Adapter`'s method set is unchanged; `Run` gains one documented
return, `chat.ErrNoInbound`. The decision lives in `serve`, which knows the
config, and each adapter's `Run` returns `chat.ErrNoInbound` if called anyway —
a refusal, not a resting state, so a future caller that gets the branch wrong
hears about it instead of blocking forever on a source that does not exist.

Declared, not inferred, and that is the load-bearing part. Reading a missing
app token or subscription as "this one only posts" would make the *recommended*
shape — a bridge that also serves the ingress — degrade silently under an
emptied or badly rotated secret: it would come back posting, passing `/healthz`,
answering nobody, with no log line to alert on. A flag makes the two cases
distinguishable, so the misconfiguration is a startup failure and the deployment
shape is a deliberate one. Given the flag, contradictory inbound credentials are
ignored with a warning rather than treated as a conflict, because a run whose
mode depends on which of two contradictory inputs wins is a run nobody can
reason about.

Three shapes are refused rather than guessed at. Being unable to receive without
having said so is the one above. Half of Chat's inbound pair (a project without
a subscription, or the reverse) is a typo. And `--outbound-only` with no ingress
means the process can do nothing at all: it would start, log a banner and stay
healthy to every probe while no work was possible, which is strictly worse than
exiting.

**Agent-initiated sessions** close the loop the ingress opened (#38). An agent
that posts "rollout is wedged, roll back?" and gets a reply in that thread would,
without this, have the reply open a *fresh* session that knows nothing about the
incident: the thread has no session, so the router creates one, and the agent
that asked the question never hears the answer. So a POST may name the session
it is speaking for, and switchboard records the thread the message landed in as
that session's conversation:

```
POST /v1/messages {"conversation":"C0123","text":"…","session":"core-agent/incident-7"}
```

Switchboard still subscribes to nothing on its own: the caller opened the
session, the caller decides which thread it belongs in, and the ingress is the
only place a binding is created. The alternative — switchboard watching the
daemon for sessions that want a human — would make it a scheduler, which §5
rules out.

**The bind is two-phase, because the conversation is not known until the post.**
A post to a bare channel or space creates the thread it lands in; on Chat the
thread id is assigned by the platform, so there is nothing to bind against
beforehand. `PrepareBind` runs before the post and is the half that can be
refused; `CommitBind` runs after, keyed on the ref that came back. Every
refusal is therefore pre-post, which is the property that matters: a caller
whose bind was rejected has not also left a question in a channel that nobody's
answer can reach.

Three things are refused. The session must exist — otherwise the thread would
take a human's reply and inject it into nothing. The conversation must not
already have a session, and the session must not already own another
conversation; the first would record a binding that is never consulted, and the
second would have two relays double-posting every answer.

Checking those is not enough, because a platform call sits between the check and
the record. Two posts naming one session would both pass and both bind, so
`PrepareBind` *reserves* the session and the commit (or an abort, if the post
failed) releases it; a bind racing one already in flight is refused with the
same 409. The reservation is taken inside the idempotent operation, so a retry
of a post that never answered replays its original outcome rather than
colliding with itself.

What cannot be refused is the far side of that call. If the message lands in a
thread that acquired a session in the meantime, the bind is dropped and logged —
the post has already gone out, and taking the thread would strand whoever is
using it. The caller sees a successful post and an unbound session, which is
visible in the logs and in `switchboard_active_bindings`, and not to the caller
itself; reporting it would mean a status code that contradicts the message the
caller can see in the channel.

**Adoption must not replay the transcript.** A subscription is resumed from a
sequence number, and `since=0` means the start of the daemon's replay window —
adopting an hour-old incident session that way would dump the agent's whole
transcript into the channel. `SessionStatus` carries no sequence number, so the
head can only be measured: `daemon.HeadSeq` opens a bounded probe subscription,
takes the last agent frame's seq, and ends after 300ms of silence or 5s
overall. Once the stream is open neither deadline is an error — a session
mid-turn simply yields a head slightly behind the truth, which replays a few
frames rather than an hour. A cap that expires *before* it opens is: nothing was
measured, and a head of zero would be the backlog this paragraph exists to
avoid, wearing a return value. The quiet window is likewise armed when the
daemon accepts the stream, not when the call is made, or a daemon slower to
answer than the window would have its own probe cancelled. The probe doubles as
the existence check, which is why the bind can be strict about a session the
daemon does not have: the 404 arrives on the same request. The cost of all this
is one SSE round trip on the request that binds, paid before the message is
posted.

The head is then measured *again* when the thread is adopted, and the later of
the two is used. Nothing relays a bound thread until a human replies in it,
which for an incident feed is hours after the alert, with the agent working the
whole time. Resuming from the bind would replay every one of those turns into
the thread at once — the same wall of transcript this section exists to avoid,
moved from before the bind to between the bind and the reply. The re-probe
costs a second round trip, on a request that is already opening a subscription
and injecting a turn; if it fails the bind's number is used, since a resume
point measured too early is still better than none.

Alternatives considered and rejected: inferring "live" from a status update (the
daemon need not emit one at turn start, and a silent thread is the worst
outcome), suppressing output until switchboard's own inject returns (racy —
replay can still be arriving), `since=MaxInt64` (drops live frames too), making
the caller supply the resume point (it does not know it, and the caller is
`curl`-shaped by design), and holding a subscription open from bind time (one
SSE connection per bound thread, for a thread that may never be answered).

The adopted relay **asserts no caller**. Switchboard did not open the session
and cannot claim to be whoever did; the human's identity still reaches the
agent, on the injected turn, which is where it belongs.

**Bindings are in-process and bounded** (last 1024, oldest evicted, every
eviction logged), and they do not survive a restart. Switchboard cannot detect
an orphaned thread on its own — an inbound chat event carries no trace of who
started the conversation — so the honest behaviour is the one it has: after a
restart the thread is just a thread, and the next human message there opens a
new session. What it will not do is fail silently. A bound session the daemon
no longer has is announced *in the thread* and the binding and its entry are
dropped, so the next message plainly starts fresh. That arrives two ways, and
both are announced: an inject that 404s, where a human's message went nowhere
("that message was not delivered anywhere"), and the relay's own subscription
404ing while nobody is typing, where nothing was lost but the thread is waiting
on an answer that is never coming. The second is not a reconnect: retrying it
would poll forever, and a thread silently waiting on a session that no longer
exists reads exactly like one waiting on a session that is thinking.
Recovery costs the caller nothing it was not already
doing: its next post carries `session` again and rebinds the thread — provided
it addresses the thread, which is the conversation its earlier post was answered
with. Durable
bindings would need the same store as durable sessions, and are deferred with
them.

**Binding is a whole-instance capability, on purpose.** The ingress token says
*a* trusted caller is on the line, not *which* one, and nothing checks that the
holder is entitled to the session id it names: any token holder can bind any
session the daemon has, and a human replying in that thread then has their
identity asserted into it. The strictness of the pre-post refusals is a
consequence — a bind naming a session the daemon does not have is a `404`, one
it does have is a `200`, so a token holder can also use the endpoint to
enumerate which session ids exist. Both follow from where authorization lives:
per-caller credential resolution is the daemon's (W0's), inside the MCP
outbound path, and the daemon offers switchboard no hook to ask "may this
caller drive that session?". Adding a switchboard-side answer would mean
inventing a second identity model beside the one that already exists, in the
component explicitly held to have none. So the token is trusted the way it is
everywhere else in this document: hold it and you can post as the app to any
allowed conversation. What binding adds is that you can also point a thread at
an existing session. Deployments where that matters run the ingress on a
network only the agent backend can reach, which is what the `--outbound-only`
and allowlist controls above are for.

What remains open on this seam: an agent's `alert`-shaped tool call
(`{level, summary, details}`) still has to be rendered into the ingress's
`{conversation, text}` by its caller — switchboard does not define that
vocabulary — and the ingress remains unauthenticated as to *which* caller holds
the token, so the allowlist above is still the whole authorization model.

### 3.2 Reply kinds and adapter capabilities

The router classifies each thing it sends with a `Reply.Kind` — an agent turn,
a progress placeholder, a tool-activity notice, an error notice, a command
acknowledgment, and a permission question (`KindDecision`, §2.3). The kind is
advisory: an adapter that ignores it posts the same text it always did, which is
exactly what the Slack adapter does. An adapter that honors it can render the
gateway's own chatter in the platform's idiom without switchboard growing
per-platform branches in the router.

`KindDecision` is the one kind that carries structure with it — `Reply.Decision`
— and it is still advisory in the same sense. An adapter that renders nothing
special posts the question as text, with the answers listed in the body, and the
reader can at least see that the agent is blocked and on what. The one part that
is not advisory is the *edit* that settles a question: a `KindDecision` reply
with no `Decision` must leave nothing pressable behind (§2.3).

Two optional capabilities work the same way — an adapter type-asserts for them
and degrades if they are absent:

- `chat.TextFitter` — does this text fit one message on this platform?
- `chat.CommandChoices` — what values does this gateway setting accept? This is
  what lets an adapter name `progress`'s values — in the message text, or as
  buttons on a platform where a click reaches the app — without hard-coding the
  router's vocabulary; the router is the single source of truth for the list.
  It is asked outside a command, too — Google Chat's welcome names the values
  before anyone has run anything — so a handler that does not report them
  leaves that message with nothing to name.

### 3.3 Google Chat: dialects, Pub/Sub, and cards

Google is migrating Chat apps from the Chat-API **interaction events** framework
to **Workspace add-ons that extend Chat**. The two deliver structurally different
events — a flat object with a `type` discriminator versus a `chat` wrapper with
one of `messagePayload` / `appCommandPayload` / `buttonClickedPayload` /
`addedToSpacePayload`. Converting an app in the console is **irreversible** and
applies to all users at once, so the adapter detects the dialect per event and
normalizes both into one internal `inbound`. The console change and a switchboard
deploy therefore need no coordination in either direction.

Pub/Sub is a supported add-on architecture, which is what preserves the
no-public-ingress, distroless posture. Two costs come with it:

- **Nothing that needs a response channel.** A pulled event has none, so the app
  cannot answer an interaction — it can only act and then patch. That rules out
  dialogs. **Callback buttons** are ruled out too, but by something else, and the
  difference is what decides where the fix has to live: a click needs no response
  channel — patching the hosting message afterwards would have answered it — it
  is simply **never delivered**. An add-on's Connection settings enumerate
  exactly four routable triggers (added-to-space, message, removed-from-space,
  app command), and a click is not among them; the subscription receives nothing
  and the user sees "Switchboard is unable to process your request".
  Established by live testing,
  [#28](https://github.com/go-steer/switchboard/issues/28). So no card the
  gateway sends carries one — nor any other widget a user can operate. The
  welcome's row of `progress` values became a list in its text; the command ack
  is now just the ack, and the acks where a value list earns its place (the one
  reporting the current mode, `help`, and the unknown-value error) already spell
  the values out themselves. An `openLink` button should be unaffected, since it
  sends no event to the app at all, but that has not been tried here.
- **Whole-message patches.** Updating a card means `messages.patch` on the
  hosting message with a field mask covering both `text` and `cardsV2`, so a
  message cannot end up carrying a stale card beside new text. This is the live
  path for the progress placeholder, which is edited in place as a turn runs.
  Delivery may be retried, so a patch is written to be idempotent — the same
  event twice leaves the card in the same end state.

**The callback-button limit belongs to the add-on framework, not to Pub/Sub.**
The legacy Chat-API dialect delivers `CARD_CLICKED` over the same transport —
known from operating the platform rather than from a test here, so it carries
none of #28's provenance and should not be read as if it did. Google's list of
what a Pub/Sub app gives up names dialogs and synchronous single-card updates
and does not mention delivery, which is consistent with that but does not on its
own establish it.

So there is a configuration in which buttons work today, and switchboard does
not take it. Google is migrating Chat apps onto add-ons, converting an app is
one-way, and buying interactivity by staying on the framework being retired
trades a durable limitation for a dated one. Nor could the choice be made per
event: the decoder normalizes both dialects into one `inbound` and the rest of
the package never learns which one a turn arrived in (`event.go`), while an
agent-initiated post has no inbound event to infer a dialect from at all — a
button that renders only for legacy is not something this design can express.
Legacy events therefore stay *decodable*, since apps that have not converted are
still out there and the split costs one normalizer, but the add-on dialect is
what this gateway is designed against.

Callback buttons are therefore what an **HTTP interaction endpoint** buys
([#29](https://github.com/go-steer/switchboard/issues/29)). Only the *rendering*
of a button was dropped, and dropped for both dialects, so the decode and
dispatch path is unreached in either one today; it is kept and still tested
because #29 is the ingress it is waiting for.

When a click does arrive, add-ons never report an **invoked function name** back
to the app, so a button's identity travels in `action.parameters` — which the
legacy dialect also carries, so one encoding serves both.

The values a setting accepts are still reported by the handler through
`chat.CommandChoices` rather than hard-coded in the adapter. Which is the point
of the capability: it names a surface, not a widget, so an adapter renders those
values however its platform can — as the welcome's text here, both on the card
and on the text fallback that stands in for it, and as buttons wherever a click
gets through.

Rendering is `--googlechat-cards`-gated (`off` / `status` / `rich`, default
`rich`) rather than a boolean, because the two card families carry different
risk: gateway cards are short and authored here, while an answer card lays out
arbitrary model output — an operator who wants the first without the second
sets `status`. Text is always sent as the message fallback, and a card
Chat rejects with a 400 falls back to posting the text — a rich render never
costs a reply.

### 3.4 Google Chat over HTTP: the interaction endpoint

Pub/Sub buys the no-public-ingress posture and pays for it in interactivity
(§3.3). [#29](https://github.com/go-steer/switchboard/issues/29) adds an HTTP
interaction endpoint as a **second ingress chosen per deployment**, not as a
replacement: Pub/Sub stays the default, because for a deployment that never
needed a button the endpoint is a public attack surface bought for nothing.

Most of the work is not the transport. `decodeEvent` already takes a `[]byte`
and both dialects already normalize into one `inbound`, so the two ingresses
converge one function in. What is coupled to Pub/Sub is `dispatch`, which owns
the `*pubsub.Message` and its ack; the handler path is the same decode and the
same `chat.Handler` calls with a different envelope and a different way of
saying "done". Four things do not carry over, and they are the design.

**The wire is not the same JSON.** HTTP delivery serializes proto-JSON, which
renders numbers as floats — `"appCommandId": 100.0` — where Pub/Sub delivers
`100`. `commandID.UnmarshalJSON` parses with `strconv.ParseInt(s, 10, 64)`,
which rejects `100.0`, and it deliberately never returns an error: an
unreadable ID leaves the zero value, which maps to no configured command and is
ignored. Over Pub/Sub that tolerance loses one malformed command. Over HTTP it
would lose **every** command, silently, in a deployment whose hand-written test
fixtures all pass — the failure is invisible from inside the repo, which is the
same reason `--googlechat-log-events` exists. The decoder has to accept the
float spelling, and the case belongs in the fixtures for both transports.

**Authentication has no Pub/Sub analogue.** Over Pub/Sub, authorization *is* the
subscription's IAM — only Chat publishes, only switchboard pulls, and nothing
reaches the process that Google did not put there. An HTTP endpoint is reachable
by anyone, so the request has to prove it came from Chat. Google's
[Verify requests from Chat](https://developers.google.com/workspace/chat/verify-requests-from-chat)
documents only the *legacy* Chat-app case, and the add-on dialect this gateway
is designed against is not on that page. What follows is measured against a
working Workspace add-on rather than documented — a separate codebase, so it
carries less provenance than #28's live testing here, and the first deployment
should confirm it with `--googlechat-log-events` before it is trusted:

- Every request carries a Google-signed ID token in **both** the
  `Authorization: Bearer` header and the body's
  `authorizationEventObject.systemIdToken`.
- `iss` is `accounts.google.com`, `aud` is **the endpoint URL Chat called**, and
  `email` is the hosting project's add-ons service agent,
  `service-<project number>@gcp-sa-gsuiteaddons.iam.gserviceaccount.com`.
- So the audience is derived from the request rather than configured, with an
  explicit override for a proxy that rewrites `Host`. Validation is
  `google.golang.org/api/idtoken` against Google's published certificates. The
  legacy project-number audience mode needs a *different* certificate URL
  (`chat@system.gserviceaccount.com`) that `idtoken` cannot be pointed at —
  one more place the two dialects diverge in favour of the add-on one.
- **The email is pinned.** A valid token from that service-account *shape*
  proves the caller is an add-on, not that it is this one, and the shape is
  derivable from any project number. Pinning it to the deployment's own is what
  makes the check mean something.
- The token is verification material and is never forwarded. The asserted caller
  stays the Chat user's email, exactly as over Pub/Sub.

**Thirty seconds, and nothing after the response.** Chat gives the endpoint ~30
seconds, and a turn routinely takes longer, so the endpoint acknowledges and the
turn's output arrives as the same separate REST posts Pub/Sub already makes.
What HTTP adds is the ability to answer *the click itself* in the response,
which is what a press actually needs — an approval button must visibly do
something at the moment it is pressed rather than a round-trip later. The
platform hazard to design around is that side effects deferred to a goroutine
*after* the response are not reliably run: on Cloud Run's default CPU
allocation the instance is throttled the moment the response completes. Anything
that must happen happens before the write.

**Buttons render only where a click can be delivered.** #52 removed every button
rather than ship controls that could only fail, so #29 owns putting them back —
gated on the ingress, because a button rendered by a Pub/Sub deployment still
produces "Switchboard is unable to process your request". Rendering also gains
an input it never had: in the add-on HTTP runtime `onClick.action.function` must
be the **full HTTPS endpoint URL**, and a bare function name fails client-side
with no request sent at all, so the card builder has to know the endpoint. The
button's logical identity still travels in `action.parameters` (§3.3), which is
what keeps one encoding serving both dialects.

Responses use the add-on envelope —
`hostAppDataAction.chatDataAction.createMessageAction` / `updateMessageAction` —
not the legacy `actionResponse` with `cardsV2`. `RenderActions` is specifically
the dialog lifecycle, and switchboard renders no dialogs.

**What shipped, and three things the build settled.** The transport is in
(`pkg/chat/googlechat/http.go`): the mode flag, the listener, the verification
above, and a `200 {}` written before the turn runs. Buttons are not — that is
the rest of #29, and the paragraph above is still its design.

The turn *does* run after the response here, which is the opposite of "anything
that must happen happens before the write" — deliberately, and only because
switchboard ships as a Kubernetes Deployment (`deploy/`) where a post-response goroutine is
scheduled normally. It is the one design decision above that a Cloud Run
deployment would have to revisit, so it is written into the code at the point
that assumes it rather than left for someone to rediscover.

The path is fixed at `/chat` rather than configurable, because it is half of the
audience: a path that can differ between the console and the process turns one
typo into an authentication error on every request, diagnosed as a delivery
failure. And the endpoint answers `200` even to an event it cannot act on, which
is the same reasoning as `dispatch`'s unconditional ack — a non-2xx buys a
redelivery, and a redelivered message event is a duplicate turn rather than a
second chance. Shutdown is where the two ingresses genuinely differ: Pub/Sub's
`Receive` returns once its handlers do, while a turn here has already outlived
its request, so the server drains the in-flight ones under a bounded grace and
says so if it gives up.

### 3.5 Configuration, and why it grew a file

Flags were enough while every setting was a scalar the whole process shared.
Two pressures ended that.

The first is structure. `--approvers`, `--ingress-allow` and
`--googlechat-commands` are a list, a list and a map, each flattened into one
string and parsed back out — and most of `parseApprovers` is the cost of that
round trip, not of the rule it enforces.

The second is scope, and it is the one that mattered. #65 shipped `--approvers`
process-wide because a flag has nowhere to put a channel, but the setting wanted
scoping from the start: the room where the grant should be wide is rarely the
room where it should be narrow. The adapters had already anticipated this —
`Message.Channel` carries the platform's channel ID and its doc comment named
"channel-scoped gateway settings" as the reason — and one setting had quietly
grown a resolver of its own, `progressFor`, for the runtime progress-mode
override. Everything else was read straight off `Router`. That asymmetry is the
shape of a third bespoke lookup, so #71 collapsed it: one `settingsFor(channel)`
serves `approvals`, `approvers`, `progress_mode` and `show_usage`, and adding
the next scopable setting is a field on `channelSettings`.

**Resolution is layered narrowest-first, and only one layer is reachable at
runtime.** A channel's block in the file is a complete answer for that channel,
resolved once at startup against the process defaults rather than merged on
every read; on top of that sits the runtime progress-mode override a chat
command sets, which is deliberately the only setting a command can change. Who
may approve in a room must not be reachable from inside the room.

**Precedence is explicit flag > `$SWITCHBOARD_*` > file > built-in default**, and
making that expressible is why the flags no longer take their defaults from
`envOr`. A flag whose default came from the environment cannot be told apart
after `Parse` from one nobody passed, so no layer could ever sit underneath it —
the environment would win by pretending to be the default. `flag.FlagSet.Visit`
walks only the flags actually on argv, which is the one place the distinction
survives.

A `channels` block is the one thing that outranks an explicitly passed flag, and
that is not an inconsistency in the ladder — it is a different axis. The ladder
orders *where a value came from* for one scope; a channel block is a narrower
scope, and the narrower scope wins because it is the one that named the room.
The alternative reading, where a flag on the command line silently disables
every per-channel rule in the file, is a config that looks applied and is not.

Four refusals are load-bearing, and they are all the same failure: a config
that looks applied and is not.

- **A `channels` key must look like a channel ID on the platform being
  bridged.** `Message.Channel` carries what the platform sends — `C0SRE0000`, or
  `spaces/AAAA` — and `"#sre"` is what a person writes. A block keyed by the
  name matches nothing and reads, in the file and in review, as a room that has
  been narrowed.

- **Credentials never go in the file.** AGENTS.md keeps tokens out of argv by
  indirection — a flag names the variable, the environment holds the value — and
  a config file is the artifact *designed* to be committed. So a key that reads
  like a credential, or a value shaped like a live one, is a startup error rather
  than a line in the docs. Keys ending `_env` are the sanctioned form and are
  exempt from the name rule, not from the value rule.
- **Unknown keys are refused.** A misspelled `approver` decodes to nothing, and
  the run then starts cleanly, announces settings that look right, and enforces
  none of them — the same shape as an approver list nobody can match.
- **No auto-discovery, and a named file that is missing is fatal.** core-agent
  reads `.agents/config.json` from the working directory, which is a convenience
  for a CLI. A long-lived gateway is different: silently changing who may approve
  a production change because of a file that appeared beside it, or falling back
  to flag defaults when the file it was pointed at is gone, is how a narrowed
  approver list becomes an open one with nothing in the log to say so.

Everything else mirrors core-agent, because AGENTS.md says to: JSON rather than
YAML, and `-c` as well as `--config` — the long form is not a courtesy, since
core-agent shipped the short form alone and a distroless Deployment written as
`args: ["--config=..."]` exited at flag-parse during a live demo
(go-steer/core-agent#209).

Two settings stay process-wide on purpose. `slack_rich_blocks` and
`googlechat_cards` are read by the adapter when it is constructed, before there
is a channel to ask about, and the adapter has no route back into the router to
resolve one. Accepting them in a channel block and ignoring them is exactly what
`DisallowUnknownFields` exists to prevent, so they are not accepted there.

### Conversation ↔ session mapping

The mapping key is the platform's stable thread identifier. Same key across turns
⇒ same session ⇒ conversational continuity. The initial store is in-memory;
durability (survive a switchboard restart) is a later phase and can reuse the
same file-backed pattern as W6's `PeerRegistry`.

## 4. Phasing

0. **Scaffold** (this repo) — multicall binary, distroless image, contract
   client, adapter interface. *Done when `switchboard serve` boots and the
   daemon client has tests against an `httptest` server.*
1. **Slack MVP** — Socket Mode ingress, in-memory session map, inject → SSE →
   reply round-trip, `X-Asserted-Caller` from the Slack user identity.
2. **Interactive hardening** — long-running turns, backpressure, thread-scoped
   error surfacing, reconnect on SSE drop.
3. **Google Chat** — Pub/Sub ingress adapter behind the same `Adapter`
   interface, speaking both the Chat-API interaction-events dialect and the
   Workspace add-on dialect, with `cardsV2` rendering. See §3.3.

## 5. Non-goals

- **No tool execution in switchboard.** It is a transport; all action happens in
  the brain via MCP.
- **No credential logic.** Per-caller credential resolution is W0, inside the
  daemon's MCP outbound path. Switchboard only asserts *who* the caller is.
- **No scheduling / triage.** Those are sibling companions (core-agent-cron /
  k8s-lookout). The outbound ingress (§3.1) does not change this: a caller
  decides *when* to post, switchboard only does the posting.
