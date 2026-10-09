# Multi-agent — one switchboard app in front of several agents

**Status:** proposal (2026-10-06) · **Tracks:** #140 · **Related:** handoff
(#138, `docs/handoff.md`)

## The requirement

One chat app, several agents behind it: core-agent daemons and mast daemons.

- A space or channel can hold conversations with **several agents**.
- A user **picks** an agent. On Google Chat that is a dropdown.
- A user can also simply address the app and get the space's **default**
  agent.
- Agents can be **added through an API**, not only through the config file.

Today a gateway talks to exactly one daemon (`daemon_url`), and nothing routes
a conversation to anything else.

## Why now

The agent has to become part of two things switchboard persists: the routing
record (a thread maps to an agent *and* a session) and the decision id an
approval button carries. Added after real usage, both need migrating, and
buttons already live in existing threads would carry ids without an agent.
Added now, a single-agent deployment is just "one registered agent, also the
default", with nothing to migrate. Everything below degrades to today's
behaviour when only one agent is registered.

## The registry

The registry is the set of agents switchboard can route to. Each entry has:

| field | meaning |
|---|---|
| `name` | short, stable id used in commands and records (`platform`) |
| `display_name` | shown in the picker and on answers |
| `daemon_url` | the agent's attach endpoint |
| `token` | a **reference** to the daemon credential: an env var name, or a Kubernetes Secret name and key. Never the value. |
| `kind` | `core-agent` or `mast`. Informational today; it marks where contract differences would hang. |
| `labels` | free-form, for filtering and allow lists |

**Two sources:**

- **Config file.** An `agents` list in `config.json`. These entries are
  *pinned*: the API can read them but not change or delete them (`409`), so a
  deployment's declared agents cannot be removed at runtime.
- **Admin API.** Agents added at runtime live in a **registry store**, an
  interface shaped like `stateStore`. The first implementation is a file
  beside the state snapshot. On a multi-pod deployment it uses the same shared
  store as #138. A KV store or database comes later, behind the same
  interface.

**Discovery and health.** For each agent, switchboard periodically fetches
the public `/.well-known/agent-card.json` that core-agent daemons serve
(description, skills) and checks the agent is reachable. The card's description
fills the picker, and an unreachable agent is shown as unavailable rather than
offered. An agent without a card, which may be the case for mast, shows its
registry fields only.

**Optional hub import (later).** Given a core-agent hub URL, switchboard lists
the agents registered in its `/peers` (name, endpoint, labels, heartbeat lease)
and imports those matching a label (say `switchboard=true`) as unpinned
entries, removing them when their lease lapses.

## The admin API

It is served on its own listener, `--admin-addr`, kept off the public Gateway,
with a bearer token read from an env var (`--admin-token-env`), as the outbound
ingress does.

| method | path | does |
|---|---|---|
| `GET` | `/v1/agents` | list (pinned and API-managed, with health) |
| `GET` | `/v1/agents/{name}` | one agent |
| `PUT` | `/v1/agents/{name}` | create or replace an API-managed agent |
| `DELETE` | `/v1/agents/{name}` | remove an API-managed agent |
| `GET`, `PUT` | `/v1/defaults` | the global default agent |
| `GET`, `PUT`, `DELETE` | `/v1/channels/{channel}` | a channel's default agent and allow list |

- **Validated on write.** A `PUT` fetches the agent card and checks health, and
  is refused if the agent is unreachable, unless `?force=true`. The token
  reference must resolve in the gateway (the env var is set, or the Secret is
  readable).
- **Audited.** Every change is written to switchboard's audit log, with who
  made it.
- **No secrets cross it.** Credentials are referenced, never sent, matching the
  repo's rule that secrets are never passed as values.

## Defaults and allow lists

- **Global default:** the agent a message goes to when nothing else decides.
- **Per-channel default:** a space or channel can name its own, in config
  (`channels.<id>.default_agent`) or through the API.
- **Per-channel allow list:** `channels.<id>.agents`. If set, only those agents
  are offered in the picker and accepted for that channel.
- **Order:** an explicit choice, then the channel's default, then the global
  default. With an allow list, a default outside it is not used.

## One agent per thread

Each agent conversation is its own thread.

- **A new top-level message** (`@switchboard …`, or a DM) goes to the default
  agent, and the thread is bound to it from then on.
- **`/agent`** starts a conversation with a chosen agent:
  - **Google Chat:** the app posts a card with a **dropdown** (a
    `selectionInput` of type `DROPDOWN`) listing the allowed agents with their
    descriptions, a prompt field, and a Start button. Submitting starts a new
    thread: the app posts "*Conversation with Platform agent*" plus the prompt,
    and the person replies there.
  - **Slack:** the slash command opens a **modal** (`views.open` with the
    command's trigger id, which works over Socket Mode) holding a
    `static_select` and a prompt field; the channel rides in the view's
    `private_metadata`. Submitting starts the thread the same way, and the
    acknowledgment goes to the submitter as an ephemeral message. (A static
    menu holds 100 options; `external_select` if the list ever outgrows it.)
  - `/agent <name> <prompt>` does the same without the picker.
- **Inside an existing thread,** `/agent` does not switch. It answers "*this
  thread talks to Platform agent*" with a button to start a new thread with
  the other agent. One thread, one agent, no ambiguity in the transcript or the
  session.
- **An agent removed or unavailable** for a bound thread: a message there gets
  a notice to start a new thread. Nothing is routed elsewhere silently.

**Attribution.** On Slack, each agent's messages can carry its own name and
icon (per-message `username` / `icon_url`, under the `chat:write.customize`
scope). Google Chat cannot change the app's identity per message, so its answer
cards carry the agent's name in a small header.

## Routing

**Direct, in switchboard.** No routing LLM in the path: the choice is
deterministic, costs no model call, and approvals go straight to the agent that
asked. If "let the system decide" is wanted later, a router agent is simply
another registered agent, perhaps the default. The hub's `/peers` is used for
discovery only. switchboard never proxies through it.

## What changes inside switchboard

- **A daemon client per agent:** URL, credential, the capabilities read on
  connect (protocol version, event types), and switchboard's proxy identity
  registered in that daemon's bearer table.
- **Routing record:** `sessionRecord` gains `Agent`. A record without one
  belongs to the default agent, which is how existing snapshots load unchanged.
- **Decision ids** carry the agent, so a button press reaches the daemon that
  asked. Ids without one resolve to the default agent, so buttons already on
  screen keep working.
- **Relays, prompt watchers, progress, the usage footer** all work per
  conversation, as today, against that conversation's daemon.
- **Ingress bindings** (`--ingress-addr`) name an agent, defaulting to the
  default agent.

## Relation to the handoff design

One owner holding streams to several daemons is fine at this scale. The handoff
(#138) works unchanged: the registry and the routing records travel in the
shared store. If a gateway outgrows one process, "sharding by agent" in
`docs/handoff.md` becomes the natural first split. A push-delivery
("stateless") model would need a contract change in every daemon type behind
switchboard, core-agent and mast alike, which this design deliberately avoids.

## Phasing

1. **Registry and routing:** config-file agents, a client per agent, `Agent`
   in records and decision ids (with the default-agent fallback for old ones),
   defaults, allow lists, attribution.
2. **Admin API:** agents, defaults, channel settings; validation; audit;
   the file-backed registry store.
3. **Picker:** the Chat `/agent` dropdown card, the Slack `/agent` modal,
   `/agent <name> <prompt>`, the in-thread guard.
4. **Discovery:** agent cards, health checks, unavailable agents shown as
   such; optional hub `/peers` import.
5. **Polish:** per-agent identity on Slack, the card header on Chat.
6. **Live:** a second agent registered on the GKE deployment, end to end on
   both platforms.

## Open questions

- **Per-user defaults.** Should a person be able to set their own default
  agent, overriding the channel's? It is easy to add later as one more step in
  the resolution order.
- **mast specifics.** Does mast serve an agent card, and does anything in its
  contract differ for sessions, inject or SSE? The registry's `kind` is where
  that would attach. mast's park and change-set surface (#84) stays separate.
