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

## Build & test

```bash
dev/tools/ci                # full local presubmit sweep, fast-fail order
dev/ci/presubmits/build     # go build ./...
dev/ci/presubmits/test-unit # go test -race ./...
dev/ci/presubmits/vet       # go vet ./...
dev/ci/presubmits/verify-go-format  # gofmt cleanliness (fix: gofmt -w .)
dev/ci/presubmits/verify-mod-tidy   # go.mod/go.sum are tidy
dev/ci/presubmits/verify-vuln       # govulncheck (auto-installs on demand)
```

The default test run needs no network and no credentials.

## Conventions

- Mirror `core-agent` / `k8s-lookout` conventions (package layout, `dev/`
  tooling, multicall binary, `internal/version` stamping); a maintainer of one
  repo should recognize the others. Fixes should be one-line ports.
- **License headers everywhere.** The Apache 2.0 boilerplate attributed to
  Google LLC sits atop every Go / shell / YAML source file. Markdown carries no
  header.
- **Conventional Commits**, small self-contained commits, bodies explaining
  *why* + the verification done. **DCO sign-off** (`git commit -s`).
- **No `Co-Authored-By` trailer and no assistant attribution** — in commits, PR
  titles/bodies, or any committed/published artifact. Author under your own name.
  See [`CONTRIBUTING.md`](./CONTRIBUTING.md).
- **Tests before merging.** Every new package ships unit tests; a bug fix ships a
  regression test.

## How we develop

Single long-lived branch `main`; short-lived `feat/…` `fix/…` `chore/…`
`docs/…` branches → PR → merge once the required CI checks are green. Rebase,
don't merge; `--force-with-lease` on your own branch is fine, never force-push
`main`. Full contributor flow + DCO in [`CONTRIBUTING.md`](./CONTRIBUTING.md).

Conventions worth knowing at agent prompt time:

- **Run presubmits before every push.** `dev/tools/ci` runs the same scripts CI
  runs (`dev/ci/presubmits/*`). A green local run is the green remote run —
  skipping them ships preventable red builds.
- **Adversarial-review gate before every PR.** Before `gh pr create` on any
  change touching Go code: run a skeptic subagent over the staged diff
  (correctness, races, API misuse — verified against real dependency source, not
  memory), fix or pin every finding, and record the outcome in the PR body under
  an `## Adversarial review` heading. For bug fixes, additionally **verify the
  new regression test FAILS on the pre-fix code** (run it against the parent
  commit) — a test that passes on the buggy code is documentation, not a gate.
  Enforced by this convention plus the `review-gate` **required** CI check
  (Go-touching PRs fail without the section; docs-only and bot PRs exempt).
  Optionally copy `dev/claude/settings-review-gate.json` into your local,
  untracked `.claude/settings.json` for a Claude Code hook that blocks
  `gh pr create` at the terminal before CI ever sees it.
- **Admin merge protocol.** `gh pr merge <N> --admin --squash --delete-branch`
  is the maintainer path once CI is green. It is **not** a way to skip review —
  the adversarial gate + green CI *are* the review on this single-maintainer
  repo. Never merge unverified CI.
- **`[Unreleased]` grows on every merged PR.** Any user-visible change adds a
  bullet under `## [Unreleased]` in `CHANGELOG.md` as part of the PR.
- **Harness settings are not committed.** `.claude/` (and a personal `CLAUDE.md`)
  are git-ignored; the repo-native enforcement is this file + the required CI
  check. The only checked-in Claude Code config is the opt-in sample under
  `dev/claude/`.

## Current state

**Scaffolding.** The multicall binary boots (`serve` / `version`), the daemon
wire client and the `pkg/chat` adapter seam exist, and the distroless image
builds. No chat adapters are registered yet — Slack MVP is the next phase (see
`docs/DESIGN.md` §4).
