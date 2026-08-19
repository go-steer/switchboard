# Changelog

All notable changes to switchboard are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Every log line now carries the time it was written, and `--log-format json`
  (`SWITCHBOARD_LOG_FORMAT`) renders one JSON object per line for a collector
  (#49). Until now the entire logging surface was a single closure —
  `fmt.Fprintf(os.Stderr, prog+": "+format+"\n", ...)` — so a line had no time,
  no severity and no structure, and neither `log` nor `log/slog` appeared
  anywhere in the tree.

  A hosted deployment was not quite as blind as that sounds: Cloud Run and a
  k8s collector both stamp an ingestion time on arrival. But that is when the
  line was collected rather than when it happened, and it is missing outright
  from a local run, a redirect to a file, a `kubectl logs` dump taken without
  `--timestamps`, and log output pasted into an issue — which is how most of
  the Google Chat walkthrough findings arrived. The gap fell hardest on the
  behaviour that is hardest to reason about: the relay logs
  `stream ended (%v); resuming from seq %d in %s` on every reconnect, and
  whether a session flapped four times in a minute or four times in an hour is
  the entire diagnostic question. Ordering alone does not answer it.

  The stamp is fixed-width UTC to the millisecond in both formats, rather than
  RFC3339Nano, whose trailing-zero trimming renders a different width per line
  and makes an unreadable left-hand column. JSON goes through
  `slog.JSONHandler` for its escaping, which is not incidental: the relay logs
  raw daemon frames it cannot decode, and `--googlechat-log-events` logs whole
  Chat payloads, so quotes and braces in a message are the normal case — as is
  the occasional newline, which JSON escapes and text passes through verbatim.
  The entry text is under `message`, where Cloud Logging looks for it, rather
  than slog's `msg`; `time` is already both.
  The startup banner and the shutdown line went to stderr directly and now go
  through the logger too, so a JSON stream no longer opens with three
  unparseable lines — nor ends on one, since a startup failure is now logged by
  `serve` rather than printed by `main`, and a crash loop is when that line is
  read. Build identity is logged ahead of the config checks so an operator
  whose flags are rejected still learns which build rejected them. Not
  everything is the logger's: a flag that will not parse and a `--log-format`
  that cannot be rendered happen before there is a logger to say them through,
  `--help` and `version` keep their own output, and the `--metrics-addr` and
  `--ingress-addr` listeners still hand runtime errors to `net/http`'s default
  logger — the README lists them, and #49 tracks the last one.

  Two things a line still does not carry, both deliberate and both tracked in
  #49. There is no **severity**: nothing upstream distinguishes
  `slack: connected as %s` from `handle %s: surface error: %v`, so every record
  arrives at one level, and labelling them all `INFO` would mislabel the
  failures — worse than the `DEFAULT` that Cloud Logging assigns a record with
  no severity at all. Giving the call sites a level to carry is a pass over all
  54 of them. And the messages are still **unstructured**, with the component
  and the conversation key interpolated into the format string where no
  collector can filter on them; turning those into attrs means rewriting every
  message, and should wait until the message set has settled.

  The `Logf func(string, ...any)` hook on the adapter configs, the ingress and
  the router is unchanged, so no call site moved and `t.Logf` still works as a
  test logger.
- The relay reads the daemon's `status-update` and `capabilities` frames (#33).
  `inbox` and `pause` remain advertised and unconsumed.

  `status-update` carries the session's turn state, and its `idle` is the only
  turn boundary that does not depend on the daemon having classified how the
  turn ended: `turn-complete` fires when a turn succeeded and `turn-error` when
  it failed in a way the daemon could name, but the daemon's turn cleanup emits
  `idle` on both exit paths of any turn that started. Anything ending a turn
  outside those two — including a `turn-error` payload this build cannot read,
  which is logged and dropped rather than guessed at — used to leave the session
  marked in flight and its progress clock running to the hour-long backstop, and
  made the next unrelated outage announce a failure for a turn that had ended
  long before. Not every refusal is covered: core-agent's cost-ceiling and
  watchdog pre-flights return before that cleanup is installed and emit no frame
  at all, so a turn refused there still runs to the backstop.

  `awaiting_permission` and `awaiting_elicit` are deliberately not boundaries.
  The daemon has stopped on something only a human at its own console can
  answer, and the turn is still owed, so the state is logged — an operator
  otherwise watches a turn that will never move with no way to learn why — and
  nothing in the thread is retired. Relaying an approval prompt to a chat caller
  is a feature of its own, not a side effect of parsing a frame. core-agent
  defines both states and emits neither today, so this is written against the
  protocol rather than against observed traffic; it earns its place because the
  obvious spelling of the boundary — "not idle means working" — turns the first
  daemon to emit one into a turn boundary at exactly the wrong moment.

  An `idle` is only acted on once the daemon has reported the turn *running* on
  the same connection, and each boundary puts that flag back down. Every stream
  opens with a status snapshot that says idle on a session between turns, which
  would otherwise retire the placeholder of a turn posted a moment earlier and
  not yet injected; and the daemon sends `status-update` for reasons other than
  a turn changing state — a model swap, a permission-mode change — which
  otherwise end whatever turn has started since. The cost of scoping the flag to
  one connection is a turn that both starts and ends inside a stream outage
  staying in flight, which is what a `turn-complete` lost in the same gap
  already does; erring the other way retires a live turn's placeholder and
  disarms the stream-lost notice meant to cover the outage.

  `capabilities` is logged once per session: what the daemon is, what protocol
  it speaks, and which of the events switchboard relies on it does not advertise.
  Nothing negotiates on it — the daemon sends what it sends — but an older daemon
  degrades by *absence*, a thread that stays quiet or a reply with no footer, and
  absence looks nothing like a version mismatch from the outside. The legacy
  `agent` event is excluded from that check, since no conformant daemon
  advertises it; the frame describes the logical surface instead, so the traffic
  it carries is checked under the names that surface uses — a daemon
  advertising no `stream-chunk` relays no answers at all.

  Neither frame is relayed into a thread, and neither touches the seq the agent
  events are indexed by, which drives resume-after-reconnect and replay
  suppression.
- The `indicator` and `status` progress messages now tick: every 15 seconds the
  placeholder is re-rendered with how long the turn has been running, and in
  `status` mode with the tool it is on and the step count — "⏳ Working… 2m30s ·
  running `bash` (step 7)". Previously the only thing
  that ever changed a progress message was a tool call, so a turn that thought
  for four minutes without calling one was indistinguishable from a dead
  session. The clock runs from when the message was handed to the daemon rather
  than from when the placeholder landed, and stops on the daemon's
  `turn-complete` so a turn that ends without an answer freezes instead of
  ticking forever — with an hour-long backstop for the case where even that
  never arrives. Coarse on purpose — every tick is an API edit — and a failed
  edit backs off rather than retrying at the rate that provoked it, and never
  fails the turn. Status mode's tool notice now renders the same line as a
  tick: two writers, one message, one format. The clock still stops early in
  the two cases where the placeholder is retired before the turn is over — a
  model that narrates before calling a tool, and a follow-up question asked
  before the first answer lands — which needs a per-turn message identity the
  relay does not have yet.
- `--show-usage` appends each answer's model, tokens, cost and latency as a
  footer — a Block Kit `context` block on Slack, a card footer on Google Chat.
  The daemon has been reporting all of it and switchboard was dropping it, but
  not in a form that can be read off any single event. The daemon's "turn" is
  one model call, so a turn that makes five tool calls emits six
  `usage-update`s, each with a `last_turn` covering only the call that just
  finished; the conversational turn's tokens and cost are the *delta* of the
  running totals across it. Latency comes from `turn-complete`, which does fire
  once per conversational turn and does span the whole of it. Both events
  arrive *before* the agent event carrying the answer, so the relay banks the
  running difference and attaches the merged result on delivery. Off by
  default, because a turn's
  cost is spend data and a shared channel is the wrong place to disclose it
  uninvited; suppressed outside the rich renders, where a footer line would
  have to survive the per-message chunker. Per-turn only — a session running
  total would be stale the moment it landed on a message nothing will edit
  again. Held until `turn-complete`, so a model that narrates mid-turn does not
  get a footer covering part of its own turn. On Google Chat the footer needs a
  card to ride, so a prose answer — one with no heading, list, or code — carries
  none even in `--googlechat-cards rich`.
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
- `docs/slack-setup.md`: the Slack runbook that never existed — app creation and
  Socket Mode, the scope each API call actually needs (derived from the adapter,
  not from memory), what the adapter does and does not engage on (`app_mention`
  only: a plain thread reply or an unmentioned DM is ignored), a demo script, and
  a troubleshooting table keyed by symptom. Slack was the first platform
  supported and the only one whose setup lived nowhere but the README's quick
  start.
- `docs/daemon-setup.md`: the core-agent daemon behind `--daemon-url`, extracted
  from the Chat runbook so both platforms share one copy. Adds the piece neither
  runbook had: running Slack and Google Chat **side by side against one daemon**
  — two processes since `--platform` takes one value, one registered identity
  since both now assert email, and separate sessions per platform thread.
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
- `--googlechat-cards` now defaults to **`rich`** rather than `status`, so a
  structured agent reply is laid out as a card out of the box. A card is not
  chunked, which makes `rich` the one mode in which a long fenced answer cannot
  break at a message boundary at all — with the widget truncation above fixed,
  it is the only mode with no content defect, and that is what a default should
  be. The escalation is narrow: `answerCard` returns nil unless the answer has a
  `#`/`##` header or a rule that actually draws, so conversational traffic
  behaves exactly as it did, and a render that goes wrong cannot cost a reply —
  a panic recovers into nil and a card Chat rejects falls back to posting the
  text. `status` stays supported and documented for an operator who wants
  gateway cards without model-authored ones, which is the whole reason this is a
  mode and not a bool.
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
- `go install`ed binaries no longer report themselves as a dev build (#43).
  `go install module@version` records the module version in the binary, but
  `internal/version` read only the `vcs.*` build settings — and those are
  stamped only when building from a checkout, never for a module-cache build.
  So the one field that *was* populated was the one field not read, and

  ```
  $ switchboard version    # go install ...@v0.0.0-20260819120633-a6e51f013566
  switchboard v0.1.0-dev (commit none, built unknown)
  $ go run ./cmd/switchboard version    # someone's uncommitted tree
  switchboard v0.1.0-dev (commit none, built unknown)
  ```

  were byte-identical, with no second field to fall back on.
  k8s-lookout hit this exactly (lookout#146, fixed in lookout#150).

  `resolveBuildInfo` now reads `debug.BuildInfo.Main.Version` — but *last*,
  behind both `-ldflags` and the VCS stamp, which is the part that is easy to
  get wrong. Since Go 1.24 a build from a checkout stamps `Main.Version` too,
  as a pseudo-version derived from the tags, so preferring it would have
  stripped the `-dev` marker off every developer's `go build` and replaced it
  with a restatement of the SHA already in the next field — and worse, a
  checkout sitting on a release tag would report the bare tag, i.e. a dev build
  claiming to be the release, which is the exact failure the new presubmit
  exists to prevent. What separates the two cases is the VCS stamp, written
  only when there is a checkout to stamp: no commit means a module-cache build,
  and there the module version is the only identity available. A `Version`
  injected by `-ldflags` without a `Commit` is also left alone. Nothing changes
  for a build from a checkout or for a release image.

  A `verify-version-fallback` presubmit comes with it, asserting that main's
  fallback is the next minor `-dev` after the newest release tag. The manual
  bump `internal/version` documents — "bump this manually on main right after
  cutting a release" — is the step that gets forgotten, and the cost of
  forgetting is every dev build claiming to be the release that just shipped.
  Pre-release tags are excluded: `--sort=-v:refname` ranks `v0.2.0-rc1` above
  `v0.2.0`, and `release-images.yml` publishes pre-release tags, so counting
  one would demand a wrong bump on every PR until the tag went away. CI now
  checks out with `fetch-depth: 0`, since a tagless checkout would let the new
  check pass on every branch. The package also gains its first tests.
- A long answer *about* markdown split into nonsense. A fenced block is closed
  only by a bare fence at least as wide as the one that opened it, so the
  `` ``` `` lines inside a ```` ```` ```` block are content — but the chunker
  counted every run of three or more backticks as one delimiter and split on the
  parity. It therefore read the inner opener as closing the outer block, and
  from there every break in the block was inverted: the piece was not closed and
  the next was not reopened, so the rest of the code arrived in the thread as
  prose with stray backticks around it — #31's exact symptom, on the one kind of
  answer most likely to be long enough to split.

  Counting was wrong in the other direction too. A run of backticks is only a
  fence at the start of its line; one with text in front of it is part of the
  prose, and counting it flipped the parity of everything after. Two things kept
  that from being worse than it was: only runs of three or more were counted at
  all, so ordinary `` `code` `` spans were safe, and a *matched* span
  contributed two runs and cancelled itself out. What it took was an unpaired
  one — a sentence naming the syntax, like "the `` ``` `` delimiter opens a
  block" — which is a line that turns up in exactly the answers this all goes
  wrong on. It does not have to be inside a code block to break one further
  down.

  Both are gone: where a block opens and closes is now tracked by a line-based
  scanner that carries the open block's *width*, and a continuation reopens with
  a marker of that same width rather than always three backticks. Reopening a
  ```` ```` ```` block with `` ``` `` closes it instead of continuing it, which
  is the same corruption reached one step later.
- A long Google Chat reply split mid-code-fence, so the backticks rendered
  literally. Chat's per-message ceiling is 4096 characters — counted here in
  bytes, which is conservative for multi-byte text — and an answer over it is
  posted as several messages; the split preferred a newline but knew nothing
  about `` ``` ``, so a break landing inside a fenced block left an odd number of
  fences in each half — raw backticks on screen in both, and the monospace lost
  in one. Slack's renderer had already solved that case, so the splitting itself
  now lives in `pkg/chat` and both adapters call it rather than keeping two
  copies to fix separately. They still differ where they should: Chat allows
  4096 to Slack's 3900.

  Further ways a split could corrupt a code block, found reviewing the shared
  version and fixed there, so Slack gets them too:
  - A break with no newline in reach could land *inside* the `` ``` `` itself.
    Both halves then hold a stray backtick, both count even, and no amount of
    balancing sees it — the same bug by another route. A break now never falls
    through a run of backticks.
  - The newline the break was taken on was trimmed along with every newline
    after it, so a blank line inside a YAML or Python block was deleted and the
    reader had no way to know. Exactly one line ending is consumed now, and the
    pieces rejoin into the answer the model actually wrote.
  - A break landing right after an opening fence posted `` ```/``` `` — an empty
    code block — and opened the real one on the next message. It now moves back
    a line so the opener travels with its code, for an opener carrying a
    language tag (`` ```go ``) as much as a bare one. The seam on the other side
    of the block does the same thing in reverse: a break landing right *before*
    the closing fence seals the piece, reopens on the next, and the answer's own
    closer arrives immediately after, so the empty block lands at the top of the
    continuation instead. That one moves back a line too, and when there is no
    line to move back to — the block's content starts what is left — the closing
    fence is pulled forward into the piece instead, which needs no marker at
    all. Two shapes needed more than moving the break: a block at the very top
    of the answer, where backing off the closer would land on the opener and
    put the empty block there instead; and a block whose first line is longer
    than a whole message, where the opener's own newline is the only one in
    reach. The break stops preferring a line ending for that one and cuts inside
    the long line, so the opener still travels with its code.
  - Fences were counted with `strings.Count`, which reads ```` ```` ```` as one
    delimiter plus a loose backtick and gets the parity backwards. A model
    reaches for a four-backtick fence when the answer is itself about markdown.
    Where a block opens and closes is now tracked by a scanner rather than by
    counting delimiters at all — see the entry above.
  - A break with no newline in reach could land between the two bytes of a
    CRLF, leaving the piece ending on a bare `\r` and the next opening on a bare
    `\n`. Model output reaches the chunker with its line endings as written;
    Chat does not normalise them. A lone `\r` is left alone — it is an old-Mac
    line ending and content, not half of anything.
  - The headroom reserved for the closing marker was three backticks and a
    newline, which is a byte short for the ```` ```` ```` block a model writes
    when the answer is about markdown: a piece closing one came back at 4097
    bytes against Chat's 4096. It is sized to the widest run in the answer now.
    The bound holds while the limit leaves room for both markers and a rune
    between them — it takes a run of two thousand backticks to defeat 4096 — and
    past that no piece can be both balanced and inside the limit, so an
    oversized piece is the better of the two failures.

  Not fixed there but fixed below: `--googlechat-cards rich` sends an answer as
  a card rather than as text, and the card path had a truncation of its own.
- A long answer-card paragraph in `--googlechat-cards rich` was cut at 3500
  bytes and given an ellipsis. A widget is one paragraph run, one fenced block,
  or one section body, so a single long code block was all it took — and the
  same answer in `status` or `off` arrived complete across two posts. Nothing
  was logged, unlike a card Chat *rejects*, which falls back to text; the
  reader's only signal was the `…`. The budget was a presentation argument ("a
  widget this long is already collapsed behind *show more*"), and collapsed is
  readable where truncated is gone. An over-long run now spills into consecutive
  widgets in the same section, split by the same fence-aware logic as the text
  path, since a widget boundary inside a `` ``` `` renders the backticks
  literally much as a message boundary does. The gateway's own widgets keep the
  clamp, as a backstop that cannot fire: their text is authored here, fixed, and
  nowhere near the budget. Splitting them would be no safer anyway — they carry
  HTML, and a clamp is a byte cut that can land inside a tag or an entity just
  as a split can. `maxCardHeader` keeps clamping too — a section header is one
  line in every client, which is a real presentation limit.

  Spilling lets a card grow without bound, so `answerCard` now measures the card
  it built and returns nil past 26,000 bytes, sending the answer as text — which
  splits into as many messages as it needs. Chat caps a whole message, text and
  `cardsV2` together, at 32,000 bytes and tells an app over that to send
  several; the margin covers the fallback text and usage footer riding on the
  same message. Measured here rather than left to Chat, because a rejected card
  costs a write against a per-space quota of one per second before the fallback
  can run. This, not `maxCardWidgets`, is the binding limit: widgets of up to
  3500 bytes reach 32 KB at around a dozen, and would need a quarter of a
  megabyte of answer to reach eighty. A card Chat rejects for size (413) now
  falls back to text like the 400 it already did.
- A turn that failed inside the daemon posted nothing. The daemon emits
  `turn-error` when a turn dies — a bad model name, a rejected credential, a
  rate limit — and nothing in switchboard parsed it, so the thread went quiet
  and the "⏳ Working…" placeholder sat there until something else retired it.
  The relay now surfaces the failure in the thread, carrying the daemon's own
  kind, code, message and hint rather than a generic apology, and taking the
  retry advice from the daemon's `retryable` flag instead of guessing from the
  kind. The two guardrail kinds (`cost_ceiling`, `watchdog`) are called out
  separately, because "try again" is the wrong advice for a turn the agent
  stopped on purpose and will keep refusing until an operator resets it — and
  since only the trip itself is classified as a guardrail, the refusals that
  follow it are recognised by the sentence the daemon puts in all three
  guardrail messages. The notice is separate from the turn rather than
  conditional on it, because a cost ceiling is enforced at the turn *boundary*:
  its `turn-error` arrives after the answer the turn already produced, and that
  is precisely the failure the reader most needs to hear about.
- A turn was silently abandoned when contact with the daemon was lost while it
  was running. This is the half `turn-error` cannot cover: the daemon that would
  report the failure is the thing that went away, so no event ever arrives and
  the relay has to notice the silence itself. After ~90 seconds with nothing at
  all on the stream and a turn still in flight, the thread is told once that
  contact was lost and the placeholder is retired; reconnects continue
  underneath, and an answer that does arrive later is still delivered. The
  grace is measured from the last event of any kind rather than the last
  answer, so neither a turn spent thinking nor a proxy cutting the SSE stream
  every few seconds is mistaken for a daemon that has gone away, and a rolling
  restart that finishes inside the window interrupts nobody.
- Every inbound message ran **two** agent turns and posted the answer twice.
  The router injected and then woke the session, but the daemon's inject
  already requests a wake as part of queueing the message, so the explicit
  wake started a second turn — with an empty prompt, since the inbox had just
  been drained. The router now injects only. This affected both adapters
  (`Router.Handle` is shared), so Slack duplicated replies too; it was easy to
  miss because the second turn's answer is often near-identical to the first.
  The real-daemon integration test now asserts one message yields one reply —
  the fake daemon in the unit tests replays a fixed script and cannot show it.
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
