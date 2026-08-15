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
| inject | `POST /sessions/<sid>/inject` | queue a user turn on the session inbox |
| wake | `POST /sessions/<sid>/wake` | nudge an idle session to run a turn |
| stream | `GET /sessions/<sid>/events` (SSE) | read the session's output turns |

Auth is a static `Bearer` token (switchboard → daemon). **Per-turn attribution**
rides `X-Asserted-Caller`: switchboard sets it to the platform identity of the
human who sent the message; the daemon stamps it as session Owner and resolves
per-caller MCP credentials from it (W0). Switchboard must be listed in the
daemon's `attach.multi_session.proxy_identities` for the assertion to be
honored.

## 3. Architecture

```
   Slack  ──Socket Mode──┐                        ┌─ POST /sessions
                         │   ┌───────────────┐    ├─ POST /sessions/<sid>/inject
                         ├──▶│  switchboard   │───▶├─ POST /sessions/<sid>/wake
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
  inbound turns via inject/wake, subscribes to the SSE stream, and relays output
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

*Currently Slack only* (`--ingress-addr` with `--platform googlechat` is refused
at startup) — the seam is platform-neutral, but only the Slack path is tested.
Correlating an outbound post with a later inbound reply — the async
human-in-the-loop approval round trip — is a larger design and is not part of
this.

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
   interface; no router changes.

## 5. Non-goals

- **No tool execution in switchboard.** It is a transport; all action happens in
  the brain via MCP.
- **No credential logic.** Per-caller credential resolution is W0, inside the
  daemon's MCP outbound path. Switchboard only asserts *who* the caller is.
- **No scheduling / triage.** Those are sibling companions (core-agent-cron /
  k8s-lookout). The outbound ingress (§3.1) does not change this: a caller
  decides *when* to post, switchboard only does the posting.
