# AGENTS.md — switchboard

Instructions for AI agents working in this repository.

## Start here

[`docs/DESIGN.md`](./docs/DESIGN.md) — the switchboard chat-gateway design
(daemon contract, adapter seam, conversation↔session mapping, phase plan). The
umbrella effort it belongs to (W0–W6) is core-agent's
[`docs/hermes-replacement-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/hermes-replacement-design.md);
read that for cross-companion context, this for switchboard itself.

## What this project is

One multicall Go binary, `switchboard`, that bridges chat platforms (Slack, then
Google Chat) onto the frozen `core-agent` daemon contract. It is a **transport**:
it maps a conversation onto a core-agent session and shuttles turns across the
daemon's four verbs. All tool execution happens in the core-agent brain via MCP —
never here.

## Hard rules (violations are bugs)

- **Distroless posture:** the shipped image is
  `gcr.io/distroless/static-debian12:nonroot` — no shell, no package manager.
  `CGO_ENABLED=0`, fully static binary. Do not add anything that needs a libc or
  a shell at runtime.
- **No secrets as bare flags:** the daemon bearer token and any platform tokens
  are read from env vars (`--token-env NAME`), never accepted as flag values.
- **No credential logic here:** per-caller credential resolution is core-agent's
  job (W0), inside the daemon MCP outbound path. Switchboard only *asserts* the
  caller via `X-Asserted-Caller`.
- **Contract client is thin:** `pkg/daemon` speaks only the four shipped verbs
  (create / inject / wake / SSE). It introspects no auth and lists no sessions.
- **Provider seam:** platform specifics stay behind `pkg/chat.Adapter`. Adding a
  platform must not touch the router.

## Conventions

- Mirror `core-agent` / `k8s-lookout` conventions (package layout, `dev/`
  tooling, multicall binary, `internal/version` stamping); a maintainer of one
  repo should recognize the others. Fixes should be one-line ports.
- Apache 2.0 license headers on all Go source files. Markdown carries no header.
- **Adversarial-review gate before every PR:** record the outcome under an
  `## Adversarial review` heading in the PR body; a bug-fix must ship a test that
  fails on the pre-fix code.
- Run the presubmits before pushing (same checks CI runs).
- Merge your own PRs with `--admin` only once CI is green; never merge unverified
  CI.

## Current state

**Scaffolding.** The multicall binary boots (`serve` / `version`), the daemon
wire client and the `pkg/chat` adapter seam exist, and the distroless image
builds. No chat adapters are registered yet — Slack MVP is the next phase (see
`docs/DESIGN.md` §4).
