# Credential auth-relay — passing OAuth requests back through chat

**Status:** proposal / cross-repo (2026-08-08) · **Spans:** core-agent (W0) + switchboard (W1)

For per-user MCP credentials, the brain sometimes needs the human to authorize a
downstream service (OAuth). The human only ever touches chat, so the brain needs
a way to prompt them **through the chat surface** — without switchboard ever
handling a token. This note proposes that seam. It is a *proposal to evolve the
core-agent daemon contract*; nothing here ships in switchboard until the daemon
side lands.

This design is a direct consequence of switchboard's non-goals
([`DESIGN.md`](./DESIGN.md) §5): **no credential logic in switchboard.** The
auth-relay keeps that true — switchboard relays a *prompt*, not a secret.

This is the **W1 (gateway) view**. A mirroring **W0-side design should live in
core-agent** for the parts it owns — the OAuth client, the callback endpoint,
the token store, and identity verification (§5) — since those evolve the daemon
contract itself. The two docs should cross-reference; this one is the source of
truth for what crosses the chat seam.

## 1. Division of labor

The rule: **the brain owns credentials; the gateway owns the chat surface.**

**core-agent (W0) owns everything credential:**
- the OAuth client, scopes, and consent-URL generation
- a public HTTPS **callback/redirect endpoint** that exchanges the auth code
- the per-user token store and refresh
- resolving tokens into MCP calls
- binding and verifying *who* authorized (see §5)

**switchboard (W1) stays pure transport.** Its only new job is to **relay an
authorization request outbound**: recognize a new event on the session stream
and render its URL + prompt into the originating chat thread. It never runs an
OAuth client, hosts a callback, or sees a code/token/client-secret.

```
   turn needs creds core-agent doesn't have for this caller
        │
        ▼
   core-agent  ──authorization-required (SSE)──▶  switchboard
        ▲                                              │  render prompt + URL
        │                                              ▼  into the thread
        │                                         chat user clicks
        │                                              │
        └──── browser → core-agent callback ◀──────────┘
             (code exchange, token store, turn resumes)
```

The redirect lands on **core-agent**, never switchboard. Token exchange and
storage happen entirely in the brain; the answer to the turn then arrives on the
same SSE stream as any other reply.

## 2. The one new contract seam

Today `GET /sessions/<sid>/events` streams `agent` frames (model text + tool
calls) and typed lifecycle events. This proposal adds **one event type**:

```
event: authorization-required
data: {
  "seq": 42,
  "caller": "users/123",              // the caller this authorization is for
  "provider": "github",              // downstream service needing auth
  "authUrl": "https://core-agent.example/oauth/start?state=<opaque>",
  "prompt": "Connect your GitHub account to continue.",
  "expiresAt": "2026-08-08T12:05:00Z"
}
```

- `authUrl` is a core-agent URL that begins the consent flow; its `state` is
  opaque to switchboard (see §5).
- `prompt` is human-readable text switchboard shows alongside the link.
- `caller` lets switchboard target/verify delivery (see §5).
- `seq` participates in the same monotonic sequence as `agent` frames, so the
  existing exactly-once / resume-from-`since` machinery covers it for free.

**Backward-compatible.** This is an *additive* event name, so it's a minor
protocol bump (e.g. `1.4.0 → 1.5.0`), not a break of the "frozen" contract: the
SSE reader already skips event names it doesn't recognize, so an old switchboard
ignores it and a new switchboard against an old daemon simply never sees one.

## 3. Turn lifecycle: park-and-resume

Two options for what the daemon does after emitting the event:

- **(a) Park and auto-resume (recommended).** The daemon suspends the turn,
  emits `authorization-required`, and — once the callback completes — resumes the
  turn and streams the answer normally. The relay is **one-directional**:
  switchboard only ever *shows* the prompt; the answer arrives like any reply. No
  new inbound verb, no gateway-side state.
- **(b) Fail-and-retry.** The daemon fails the turn with the prompt; the user
  re-sends after authorizing. Simpler for the daemon, worse UX (the user must
  remember to retry), and it risks re-running side effects.

Recommend **(a)**. It keeps switchboard stateless about auth and needs no change
to the four existing verbs.

## 4. Switchboard-side changes (when the daemon ships it)

Small and confined to the seams switchboard already owns:

- **`pkg/daemon`** — a `EventAuthorizationRequired` constant and an
  `AuthRequest(data string) (AuthRequest, bool)` parser, mirroring `AgentText` /
  `ToolCalls`.
- **`pkg/chat`** — an optional `Auth *AuthPrompt` field on `Reply` (or a
  dedicated `AuthPrompt` type). Adapters **may** render it richly — a Google Chat
  card with an OpenLink button, a Slack link/button block — and **must** fall
  back to `prompt` + `authUrl` as plain text, matching the `ErrUnsupported`
  degrade-don't-break philosophy already in the seam.
- **router** (`cmd/switchboard`) — on an `authorization-required` event, relay it
  through the adapter as an auth prompt into the originating thread. No
  credential or session state is held; it is one more thing to relay.

> Note on Google Chat: the native `ActionResponse{REQUEST_CONFIG}` auth flow is
> tied to a *synchronous* app-command interaction and can't be used for an async
> relayed message. A card with an OpenLink button (or a plain link) is the
> portable choice across platforms.

## 5. Security considerations (mostly core-agent's)

The relay posts a URL into a chat thread, which surfaces a real risk that lives
in the brain:

- **Wrong-clicker binding.** In a shared channel, a *different* human could click
  "connect your account" and bind **their** OAuth identity to the original
  caller's session. The callback **must** verify the authenticated identity
  against the `caller` the `state` was minted for and reject a mismatch. This is
  non-negotiable and is core-agent's responsibility.
- **`state` hygiene.** `state` must bind caller + session + a single-use nonce
  and be unguessable; consent URLs must be short-lived (`expiresAt`).
- **Prompt delivery scope (gateway choice).** To reduce the wrong-clicker window,
  switchboard should consider delivering the prompt **ephemerally or via DM** to
  the caller rather than into the shared thread, where the platform supports it.
- **No token ever transits switchboard.** By construction: switchboard only ever
  holds `authUrl` + `prompt`.

## 6. Open questions for core-agent

1. Park-and-resume (§3a) vs fail-and-retry (§3b) — confirm (a).
2. Final event name and schema (§2).
3. Callback endpoint: hosting, ingress, and TLS for the daemon's new public
   redirect surface — the daemon has had no public HTTP ingress before.
4. `state` binding + callback identity verification (§5).
5. Timeout / cleanup when the user never authorizes (does the parked turn expire?
   is a follow-up event emitted so the gateway can retire the prompt?).
6. Whether one turn can require *several* authorizations (multiple providers) and
   how those serialize.

## 7. Non-goals (unchanged)

This proposal does **not** move any credential logic into switchboard. Token
exchange, storage, refresh, and identity verification stay in the brain (W0),
consistent with [`DESIGN.md`](./DESIGN.md) §5. Switchboard gains exactly one
capability: relaying an authorization *prompt* into chat.
