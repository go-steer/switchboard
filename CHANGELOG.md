# Changelog

All notable changes to switchboard are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `dev/demo/daemon` stands up a throwaway core-agent for local testing and
  demos: bearer-table auth, switchboard registered in `proxy_identities` so
  `X-Asserted-Caller` is honored, and a generated token printed for
  `$SWITCHBOARD_DAEMON_TOKEN`. Switchboard is a gateway and ships no agent, so
  every live walkthrough needed this config reconstructed by hand from
  `cmd/switchboard/integration_test.go`. Defaults to the echo provider (no model
  credentials); `--model`/`--provider` point it at a real one, since the demo
  steps that exercise markdown and structured answers need something that
  actually talks. Demo-only — it writes a token to disk and runs the agent in
  `yolo` permission mode.

- Google Chat **Workspace add-on** support (`pkg/chat/googlechat`). Google is
  migrating Chat apps from the Chat-API interaction-events framework to add-ons
  that extend Chat, which changes the shape of every event. The adapter detects
  the dialect **per event** and understands both, so the console conversion —
  which is irreversible and applies to all users at once — needs no coordination
  with a switchboard deploy. Pub/Sub remains the ingress, preserving the
  no-public-webhook posture; the cost is no dialogs (they need a synchronous
  response) and whole-message patches for card updates. Card clicks are
  delivered over Pub/Sub and handled idempotently, since delivery may retry.
- Google Chat `cardsV2` rendering, gated by `--googlechat-cards`
  (`off` / `status` / `rich`, default `status`). `status` renders the gateway's
  own messages — progress, tool activity, error notices, command acks — as small
  icon cards, and offers a setting's valid values as buttons; `rich` also lays a
  structured agent reply out as a card (a section per heading, dividers for
  rules, code fences verbatim). Plain text is always sent as the message
  fallback and a card Chat rejects falls back to posting the text, so a rich
  render never costs a reply. Three levels rather than Slack's boolean because
  gateway cards are short and authored here while an answer card lays out
  arbitrary model output.
- Google Chat replies are now translated into Chat's text dialect
  (`**bold**` → `*bold*`, `[label](url)` → `<url|label>`, headings → bold,
  rules, strikethrough, fences), mirroring the Slack `toMrkdwn` pass. Previously
  markdown from the agent was posted raw and arrived with its delimiters
  showing. Card widgets get a second pass into the small HTML subset Chat
  accepts, with `&<>` escaped — a command ack quoting
  `progress <off|indicator|status|stream>` would otherwise render as broken
  markup.
- `--googlechat-commands` (e.g. `"1=progress,2=help"`) maps Chat app-command IDs
  onto gateway verbs. Chat identifies a command by the numeric ID configured in
  the API console and never by its name, and add-ons never report an invoked
  function name back, so the mapping is how a dedicated `/progress` command is
  recognized. With no mapping the verb is still read from the command's argument
  text, so a single `/switchboard progress status` command keeps working.
- `docs/googlechat-setup.md`: Chat app + Pub/Sub setup, a demo script, and the
  two testing layers that do not need a live app — golden card JSON in
  `testdata/cards` (pasteable into Google's Card Builder to see a real render)
  and an event-replay corpus in `testdata/events` that runs raw payloads through
  the real dispatch path. Both regenerate with
  `go test ./pkg/chat/googlechat -run 'Golden|Replay' -update`.
- `--googlechat-log-events` logs every inbound payload verbatim, so real Chat
  traffic can be captured as decoder fixtures — the one thing hand-written
  fixtures cannot verify. Off by default: payloads carry message text and sender
  identity.
- `chat.Reply.Kind` classifies what the router is sending — agent turn, progress
  placeholder, tool activity, error notice, command ack — so an adapter can
  render the gateway's own chatter in the platform's idiom. Advisory: an adapter
  that ignores it (Slack) behaves exactly as before.
- `chat.CommandChoices`, an optional adapter-facing capability reporting the
  values a gateway setting accepts. It is what lets the Google Chat adapter
  offer `progress` as buttons with the router remaining the single source of
  truth for the list.
- Outbound ingress (`--ingress-addr`, default disabled): an authenticated HTTP
  surface that lets another service post — and later edit — a message in a
  conversation with no inbound chat event to reply to (scheduled digests,
  monitoring escalations, async approval prompts). `POST /v1/messages`
  `{conversation, text}` returns the `{conversation, id}` ref;
  `PATCH /v1/messages` `{conversation, id, text}` edits it in place (`501` on a
  platform that cannot edit). Callers authenticate with a bearer token from
  `--ingress-token-env` (default `SWITCHBOARD_INGRESS_TOKEN`) — separate from
  the daemon token — and can be confined to a set of conversations with
  `--ingress-allow`. An optional `Idempotency-Key` header makes a retried POST
  return the original ref instead of double-posting. There is no `platform`
  field: the instance's `--platform` implies the target. Slack only for now;
  `--ingress-addr` with `--platform googlechat` is refused at startup. New
  `switchboard_ingress_requests_total{op,outcome}` metric.
- `PATCH /v1/messages` `{conversation, id, append}` adds a line to a message
  instead of replacing it, so a caller keeping a running incident timeline does
  not have to resend the whole body. Switchboard remembers the current text of
  the messages it posted (bounded, in-memory, last 1024); an append to anything
  it does not remember answers `409` and the caller sends full `text`. When the
  combined text would pass the platform's single-message limit the addition is
  posted as a reply in the same thread and answered `200` with the
  continuation's ref, rather than truncating the timeline. `text` and `append`
  are alternatives — a PATCH must carry exactly one.
- `chat.ErrNotFound` and `chat.ErrDenied`: provider-neutral classifications of
  a platform refusal, so the ingress can answer `404`/`403` for a permanent
  failure instead of reporting everything as a retryable `502`. New optional
  `chat.TextFitter` adapter capability (`FitsOneMessage`) backs the append
  rollover; a provider that does not implement it answers `501` to `append`.
- Slack egress accepts a thread-less conversation key (a bare channel ID),
  posting a new top-level message and returning the ts it rooted — so an
  ingress caller with no thread to reply in can post one and thread its
  follow-ups under `"<channel>:<id>"`. A chunked thread-less message now keeps
  its parts together under the first one instead of scattering across the
  channel.
- Thread-scoped error surfacing: a session-create/inject/wake failure now posts
  a notice into the originating conversation instead of only logging, so a
  turn that can't run doesn't just leave the thread silently dead. New
  `daemon.StatusError` (carries the daemon's HTTP status) and
  `daemon.IsTransient(err)` let the notice distinguish a retryable 5xx/network
  failure from a terminal 4xx.
- Health check + Prometheus metrics (`--metrics-addr`, default disabled): when
  set to a `host:port`, `serve` starts an HTTP server exposing `/healthz`
  (liveness, `200 ok`, no scrape dependency) and `/metrics`. The router
  instruments inbound turns and commands, core-agent requests
  (`create`/`inject`/`wake`, with latency), outbound sends, agent turns relayed,
  SSE reconnects, and active sessions — all series prefixed `switchboard_`. A
  metrics bind failure brings the process down so the health surface a
  deployment's probes depend on is never silently absent. The deploy manifests
  set `--metrics-addr=:9090`, expose a named `metrics` port, wire `/healthz` to
  the liveness + readiness probes, and narrow the NetworkPolicy from deny-all to
  admit only that port. Mirrors k8s-lookout's `serveMetrics`.
- Kubernetes deploy manifests (`deploy/`, kustomize): a platform-neutral `base`
  (ServiceAccount, deny-all-ingress NetworkPolicy, and a hardened single-replica
  Deployment — non-root, read-only rootfs, all caps dropped, `RuntimeDefault`
  seccomp) plus two overlays. `overlays/slack` appends `--platform=slack` and
  mounts the Slack app/bot tokens from a Secret; `overlays/googlechat` appends
  `--platform=googlechat` with the project/subscription and adds a Workload
  Identity annotation for ADC (Pub/Sub subscribe + Chat bot scope). The
  core-agent bearer token and Slack tokens ride env vars sourced from Secrets the
  operator creates out-of-band — never flags. No Service or health probe:
  switchboard exposes no listening port (both platforms are outbound), so
  liveness is the process itself. `deploy/README.md` documents the prerequisites
  (namespace, Secrets, Workload Identity) and `kubectl apply -k` usage. Mirrors
  the k8s-lookout `deploy/` layout.
- Image publishing (`.github/workflows/release-images.yml`): builds the
  distroless multicall image for `linux/amd64` and `linux/arm64` off the
  buildx-ready Dockerfile and pushes it to `ghcr.io/go-steer/switchboard`,
  mirroring core-agent. Every push to `main` (every merged PR) publishes a
  floating `:main` and an immutable `:main-<short-sha>`; a semver tag (`vX.Y.Z`)
  publishes `X.Y.Z`, `X.Y`, `X`, and `latest` (a prerelease tag publishes only
  its exact version and never moves `latest`). Build identity
  (`VERSION`/`COMMIT`/`BUILD_DATE`) is injected as build args and surfaces in
  `switchboard version`; a gha build cache is reused across runs. Every image is
  signed with Sigstore keyless (cosign, GitHub OIDC → Fulcio → Rekor), verifiable
  with `cosign verify`.
- Google Chat adapter MVP (`pkg/chat/googlechat`, `--platform googlechat`):
  Pub/Sub ingress (the app publishes events to a topic and switchboard pulls
  them from a subscription, so no public webhook is exposed — matching Slack
  Socket Mode and the distroless posture) and Google Chat REST egress
  (`spaces.messages` create/patch/delete, so every long-turn progress mode
  works). A conversation is keyed on space + thread, so a mention thread maps to
  one core-agent session; the caller asserted to the daemon is the sender's user
  resource name (`users/NNN`) — verified identity and per-caller credential
  resolution stay in core-agent (W0). Long replies are split across in-thread
  posts under Chat's message limit. Credentials come from Application Default
  Credentials (`--platform`, `--google-project`, `--google-subscription`; no
  secrets as flags). Reply formatting follows.
- Google Chat native slash commands (`pkg/chat/googlechat`): a configured slash
  command (e.g. `/switchboard progress status`) is mapped onto the same
  provider-neutral `chat.Command` seam Slack's `/switchboard` uses and routed to
  `Handler.HandleCommand` — so runtime per-channel progress control works on
  Google Chat with no router change. The command is detected from the event's
  `slashCommand` (its `argumentText` is already just the verb and arguments) and
  never reaches the daemon as a turn; the acknowledgment is posted back into the
  invoking thread (Google Chat has no ephemeral async reply). The command word,
  its argument list, and the caller's user resource name flow through unchanged.
- Slack adapter MVP (`pkg/chat/slack`): Socket Mode ingress that engages on
  app-mentions, normalizes them into `chat.Message` (mention markup stripped),
  and posts replies back into the originating channel + thread. Caller identity
  is the user's email (`users.info`, cached) or raw Slack user ID, selected by
  `--caller-id`.
- Session router (`cmd/switchboard`): in-memory `conversation → session` map
  keyed by channel + `thread_ts`; first turn creates a session and opens one
  long-lived SSE subscription that relays completed agent turns back through the
  adapter, subsequent turns inject + wake. `serve` now boots the Slack adapter
  from `--slack-app-token-env` / `--slack-bot-token-env`.
- SSE relay resilience (`cmd/switchboard`): the per-session event subscription
  now reconnects with exponential backoff (1s→30s, reset on progress) when the
  stream drops, resuming from the last seq seen so the daemon replays only new
  turns — a dropped stream no longer silently strands a conversation. Delivery
  is exactly-once: a turn replayed across the reconnect boundary is posted only
  once. First slice of Phase 2 hardening (#3).
- Long-turn progress feedback (`cmd/switchboard`, `--progress-mode`): with the
  default `indicator` mode the router posts a lightweight "⏳ Working…"
  placeholder into the thread when a turn is woken and deletes it as soon as the
  agent's reply is relayed, so a slow turn no longer looks dead; `off` keeps the
  prior silent-until-complete behavior. First slice of the long-turn feedback
  work (#3); `stream` and `status` modes and chat-command control follow. The
  egress seam grew to support this: `chat.Adapter.Send` now returns a
  `chat.MessageRef` and the interface adds `Update`/`Delete` (with an
  `ErrUnsupported` sentinel so a platform lacking them degrades to plain
  `Send`). Indicator mode needs the bot's `chat:delete` scope to clear the
  placeholder; without it the placeholder is left in place and the failure is
  logged.
- Long-turn progress modes `status` and `stream` (`cmd/switchboard`,
  `--progress-mode`): `status` keeps one message per turn — the "⏳ Working…"
  placeholder is edited in place to name the tool the agent is currently
  running (e.g. "🔧 Running `lookup`") and retired when the reply arrives;
  `stream` posts a standalone tool-activity notice per tool call in addition to
  relaying each completed turn, for maximum transparency (and noise). Both are
  driven off `agent`-frame tool calls (unambiguous), gated by the same
  exactly-once seq as turn delivery so a reconnect replay never reposts. Second
  slice of the long-turn feedback work (#3); chat-command control follows.
  `status` needs the bot's `chat:delete` scope (like `indicator`).
- Runtime per-channel progress control via chat commands (`cmd/switchboard`,
  `pkg/chat`, `pkg/chat/slack`): operators can change a channel's long-turn
  feedback mode without a restart. `progress <off|indicator|status|stream>`
  sets it, bare `progress` reports it, and the override is scoped to the
  channel it was issued in (other channels keep the `--progress-mode` default).
  Two surfaces: a native Slack slash command (`/switchboard progress status`,
  acked ephemerally) and a mention subcommand (`@switchboard progress status`,
  acked in-thread). The mention form matches only tightly (a known verb alone
  or with one argument) so a normal turn is never swallowed. This adds a
  provider-neutral `chat.Command` type and a `Handler.HandleCommand` method
  each adapter maps its native command surface onto (Google Chat slash commands
  later, no router change), plus a `Channel` field on `chat.Message` for
  channel-scoped settings. The native `/switchboard` command needs a one-time
  Slack app manifest entry (see README).
- `daemon.ToolCalls` helper to extract tool-call names from `agent` SSE frames.
- `daemon.AgentText` helper to extract assistant text from `agent` SSE frames.
- Slack mrkdwn rendering (`pkg/chat/slack`): replies now convert a model turn's
  standard markdown into Slack mrkdwn before posting — bold/italic/strikethrough,
  headers → bold, links → `<url|label>`, code spans/fences preserved (language
  tag dropped), and broadcast mentions (`<!channel>` etc.) defused. Long turns
  are split into multiple in-thread posts on paragraph boundaries, keeping code
  fences balanced across the split. Ported from hermes-agent's Slack adapter.
- Slack Block Kit rendering (`pkg/chat/slack`, opt-in via `--slack-rich-blocks`):
  replies can render as structured blocks — headers, dividers, nested
  rich_text lists, native table blocks (monospace fallback when a table
  exceeds Slack's limits), blockquotes, preformatted code, and mrkdwn section
  paragraphs. The mrkdwn text is always sent alongside as the notification
  fallback, the payload is clamped to Slack's block/section/header limits, and
  a rejected payload (`invalid_blocks`) automatically retries as plain text so
  a rich render can never lose a message. Ported from hermes-agent's
  `block_kit.py`; mrkdwn stays the default.

### Changed
- Google Chat now asserts the sender's **email address** as `X-Asserted-Caller`,
  matching what the Slack adapter has always done and what the daemon keys
  per-caller credentials by; `--caller-id id` restores the raw `users/NNN`
  resource name, and an event carrying no email falls back to it. One human
  reaching an agent from two chat platforms was previously two identities to
  provision. The address was on the wire all along: the generated `chat/v1`
  `User` type has no `Email` field, so decoding the actor through it threw the
  address away — `pkg/chat/googlechat/testdata/events/addon-live-message.json`
  is the captured payload that shows it.

### Fixed
- Bumped the pinned Go toolchain to 1.26.6 (stdlib fixes for GO-2026-6089/-6090/
  -6091/-5972/-5026/-6218; `govulncheck` was failing the build against 1.26.5).
- Slack Block Kit rendering: bold/italic/strikethrough that wraps an inline code
  span (e.g. `` **Foo (`bar`)** ``) is now styled correctly instead of leaving the
  `**` delimiters literal — emphasis is resolved before code spans, treating code
  and links as opaque. A fenced code block nested under a list item is rendered as
  its own code block (dedented) rather than being flattened into the list item's
  text and mangled inline.
- Slack mention stripping now preserves newlines and indentation: an inbound
  turn's markdown block structure (headers, lists, tables, code fences) is
  newline-driven, and the previous strip collapsed all whitespace, flattening a
  multi-line message into a single unrenderable line.
- `daemon.CreateSession` now reads the daemon's `sessionID` (camelCase) response
  field; it previously looked for `session_id` and always failed against a live
  daemon. Sessions are now addressed by the app-qualified route
  (`/sessions/<app>/<id>/…`) to avoid the shortcut route's 409 on ambiguous ids,
  and `Subscribe` sends `since` + `protocol` query params.

### Added (scaffold)
- Initial scaffold: distroless multicall binary (`serve` / `version`).
- `pkg/daemon` — thin client for the frozen core-agent daemon contract
  (create / inject / wake / SSE stream), with `X-Asserted-Caller` per-turn
  attribution.
- `pkg/chat` — provider-neutral `Adapter` interface and normalized
  `Message` / `Reply` types.
- `docs/DESIGN.md` — switchboard chat-gateway design (W1 of the Hermes
  replacement epic).
- Contributor / agent guardrails ported from core-agent: `AGENTS.md` "How we
  develop", `CONTRIBUTING.md` (DCO, Conventional Commits, no attribution),
  `dev/ci/presubmits/*` + `dev/tools/ci`, the `review-gate` required CI check,
  and the opt-in `dev/claude/settings-review-gate.json` hook sample.
