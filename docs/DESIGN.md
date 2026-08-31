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
reconnecting is just resubscribing, and switchboard can attach lazily, when a
session reports it is blocked, without racing a prompt that arrived first.

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
acknowledgment. The kind is advisory: an adapter that ignores it posts the same
text it always did, which is exactly what the Slack adapter does. An adapter that
honors it can render the gateway's own chatter in the platform's idiom without
switchboard growing per-platform branches in the router.

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
  dialogs. **Callback buttons** are ruled out by something worse, and not by
  this: a click needs no response channel — patching the hosting message
  afterwards would have answered it — but it is **never delivered**. An add-on's
  Connection settings enumerate exactly four routable triggers (added-to-space,
  message, removed-from-space, app command), and a click is not among them; the
  subscription receives nothing and the user sees "Switchboard is unable to
  process your request".
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

Callback buttons are therefore what an **HTTP interaction endpoint** buys
([#29](https://github.com/go-steer/switchboard/issues/29)), not something Pub/Sub
gives up gracefully. Only the *rendering* of a button was dropped; the decode and
dispatch path for a click is kept and still tested, since that is the ingress it
is waiting for, and because the legacy Chat-API dialect over Pub/Sub is expected
— but not confirmed — to be subject to the same limit.

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
