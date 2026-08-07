# Contributing to switchboard

Thanks for contributing. switchboard follows the same contributor flow
as [core-agent](https://github.com/go-steer/core-agent) — a maintainer of
one repo should recognize the other.

## Workflow

- Single long-lived branch: `main`. Work on short-lived feature branches
  (`feat/…`, `fix/…`, `chore/…`, `docs/…`) → PR against `main` → merge
  once CI's required checks are green.
- **Rebase, don't merge.** Keep feature branches rebased on `main`;
  `git push --force-with-lease` on your own branch is normal. Never
  force-push `main`.

## Commits

- **Conventional Commits** subject lines: `feat:`, `fix:`, `docs:`,
  `chore:`, `refactor:`, `test:`, `ci:`, `build:`. Bodies explain *why*
  and call out the verification done.
- **DCO sign-off** on every commit: `git commit -s` (adds a
  `Signed-off-by:` trailer certifying the [Developer Certificate of
  Origin](https://developercertificate.org/)).
- **No `Co-Authored-By` trailer, and no assistant attribution anywhere.**
  Maintainer preference: author the work under your own name. Do not add
  `Co-Authored-By:` lines, "Generated with …" footers, or any
  tool/assistant credit to commits, PR titles/bodies, or other committed
  or published artifacts.

## Before you push

- Run `dev/tools/ci` — the full presubmit sweep, the same scripts CI
  runs (`dev/ci/presubmits/*`). A green local run is the green remote
  run.
- **Adversarial-review gate** on any PR touching Go code — see
  [`AGENTS.md`](./AGENTS.md) "How we develop". Record the outcome under an
  `## Adversarial review` heading in the PR body; the `review-gate`
  required CI check enforces it.

## Tests

Every new package ships with unit tests. A new feature without a test
is not done; a bug fix without a regression test lets the bug come back.

## License

By contributing you agree your contributions are licensed under the
project's [Apache 2.0](./LICENSE) license. Every Go / shell / YAML source
file carries the Apache 2.0 header attributed to Google LLC.
