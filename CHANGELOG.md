# Changelog

All notable changes to switchboard are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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

### Fixed
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
