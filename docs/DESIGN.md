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

Correlating an outbound post with a later inbound reply — the async
human-in-the-loop approval round trip — is a larger design and is not part of
this.

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
