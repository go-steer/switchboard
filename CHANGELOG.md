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
- `daemon.AgentText` helper to extract assistant text from `agent` SSE frames.
- Slack mrkdwn rendering (`pkg/chat/slack`): replies now convert a model turn's
  standard markdown into Slack mrkdwn before posting — bold/italic/strikethrough,
  headers → bold, links → `<url|label>`, code spans/fences preserved (language
  tag dropped), and broadcast mentions (`<!channel>` etc.) defused. Long turns
  are split into multiple in-thread posts on paragraph boundaries, keeping code
  fences balanced across the split. Ported from hermes-agent's Slack adapter.

### Fixed
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
