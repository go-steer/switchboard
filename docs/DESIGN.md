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

- **`pkg/chat`** — provider-neutral `Adapter` interface (ingress + egress) plus
  the normalized `Message` / `Reply` types. Slack and Google Chat each implement
  it under `pkg/chat/slack` and `pkg/chat/googlechat`.
- **`pkg/daemon`** — the thin wire client for the four verbs above.
- **router** (`cmd/switchboard`) — owns the **conversation → session** map
  (a Slack channel+thread or a Google Chat space+thread ⇒ one session), forwards
  inbound turns via inject/wake, subscribes to the SSE stream, and relays output
  turns back through the adapter.

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
  k8s-lookout).
