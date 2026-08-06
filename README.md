# switchboard

**Chat-gateway companion for [core-agent](https://github.com/go-steer/core-agent).**

Switchboard bridges chat platforms — **Slack** (first), **Google Chat**
(second) — onto the frozen core-agent daemon contract, so operators can drive
agents from a thread. It is a small, independently released, **distroless**
sidecar that speaks only the daemon's HTTP contract — the same "one contract,
many companions" pattern as [k8s-lookout](https://github.com/go-steer/k8s-lookout).

> **Status:** scaffolding. The wire client and adapter seam exist; chat adapters
> land next. See [`docs/DESIGN.md`](docs/DESIGN.md).

## Where it fits

Switchboard is workstream **W1** of the umbrella effort to replace Hermes as the
full [kube-agents](https://github.com/gke-labs/kube-agents) runtime. The full
picture (credentials, cron, GitOps, triage) lives in core-agent's
[`docs/hermes-replacement-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/hermes-replacement-design.md).

```
Slack / Google Chat  ──▶  switchboard  ──▶  core-agent daemon (sessions + MCP + subagents)
                                     (X-Asserted-Caller = the chat user)
```

Core-agent stays the distroless *brain* with no chat code; switchboard is the
front door. Per-turn attribution rides `X-Asserted-Caller`, so the daemon can
resolve **per-caller** MCP credentials — not one shared bot identity.

## Quick start

```sh
# Build
go build ./cmd/switchboard

# Run against a local daemon (token read from an env var, never a bare flag)
export SWITCHBOARD_DAEMON_TOKEN=…
switchboard serve --daemon-url http://127.0.0.1:7777

# Build identity
switchboard version
```

### Container

```sh
docker build -t ghcr.io/go-steer/switchboard:dev .
docker run --rm -e SWITCHBOARD_DAEMON_TOKEN ghcr.io/go-steer/switchboard:dev \
  --daemon-url http://core-agent:7777
```

The image is `gcr.io/distroless/static-debian12:nonroot` — no shell, no package
manager. Default entrypoint is `switchboard serve`.

## Layout

| Path | What |
|------|------|
| `cmd/switchboard` | multicall binary — `serve` (default), `version` |
| `pkg/daemon` | thin client for the core-agent daemon contract (create / inject / wake / SSE) |
| `pkg/chat` | provider-neutral `Adapter` interface + normalized message types |
| `internal/version` | build-identity stamping |
| `docs/DESIGN.md` | switchboard design |

## License

Apache 2.0 — see [LICENSE](LICENSE).
