# switchboard

**Chat-gateway companion for [core-agent](https://github.com/go-steer/core-agent).**

Switchboard bridges chat platforms — **Slack** (first), **Google Chat**
(second) — onto the frozen core-agent daemon contract, so operators can drive
agents from a thread. It is a small, independently released, **distroless**
sidecar that speaks only the daemon's HTTP contract — the same "one contract,
many companions" pattern as [k8s-lookout](https://github.com/go-steer/k8s-lookout).

> **Status:** Slack and Google Chat MVPs. An app-mention in a Slack thread — or
> a message to the app in a Google Chat space — drives a core-agent session and
> the reply lands back in the thread. See [`docs/DESIGN.md`](docs/DESIGN.md).

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

# Run against a local daemon (tokens read from env vars, never bare flags)
export SWITCHBOARD_DAEMON_TOKEN=…       # daemon bearer token
export SWITCHBOARD_SLACK_APP_TOKEN=xapp-…   # Socket Mode app-level token
export SWITCHBOARD_SLACK_BOT_TOKEN=xoxb-…   # bot user OAuth token
switchboard serve --daemon-url http://127.0.0.1:7777
# @-mention the bot in a Slack thread to drive a session; replies land in-thread.
# --caller-id id  asserts the raw Slack user ID instead of the resolved email.

# Build identity
switchboard version
```

### Google Chat

Select the platform with `--platform googlechat`. Ingress is **Pub/Sub** (the
Chat app is configured to publish events to a topic; switchboard pulls them from
a subscription, so no public webhook is exposed), and egress is the Chat REST
API. Credentials come from **Application Default Credentials** — workload
identity in-cluster, or `GOOGLE_APPLICATION_CREDENTIALS` locally — and must
grant Pub/Sub subscribe on the subscription and the Chat bot scope.

```sh
export SWITCHBOARD_DAEMON_TOKEN=…
switchboard serve --platform googlechat \
  --google-project my-gcp-project \
  --google-subscription switchboard-chat-events \
  --daemon-url http://127.0.0.1:7777
# Message the app in a Chat space (or @-mention it in a room); the reply lands
# in the same thread. The asserted caller is the sender's users/NNN resource name.
```

One-time setup: in the Chat API app configuration, set the **Connection
settings** to *Cloud Pub/Sub* and point it at your topic, then create a pull
subscription on that topic for switchboard to consume.

To use the runtime progress control (below) on Google Chat, add a **slash
command** in the same app configuration — name it `switchboard`, give it a
command ID, and switchboard maps its invocation onto the same `chat.Command`
seam as Slack's `/switchboard`. Chat strips the command word before delivery, so
`/switchboard progress status` arrives as the verb `progress` with argument
`status`; the acknowledgment is posted back into the invoking thread.

### Long-turn feedback

While an agent turn runs, switchboard can show liveness. `--progress-mode` sets
the process default:

| Mode | Behavior |
|------|----------|
| `indicator` (default) | posts a "⏳ Working…" placeholder, deleted when the reply lands |
| `status` | keeps one message per turn, edited in place to name the running tool |
| `stream` | posts a "🔧 Running `tool`" notice per tool call, plus each completed turn |
| `off` | silent until the reply is ready |

Operators can override the mode **per channel** at runtime with a command — no
restart:

```
/switchboard progress status     # native slash command
@switchboard progress status     # or a mention subcommand
@switchboard progress            # show the channel's current mode
```

The mention form needs no extra setup. The native `/switchboard` slash command
requires a **one-time Slack app manifest entry** — add under `features`:

```yaml
slash_commands:
  - command: /switchboard
    description: Configure switchboard for this channel
    usage_hint: progress <off|indicator|status|stream>
```

`indicator` and `status` also need the bot's `chat:delete` scope to clear their
transient messages.

### Container

Images are published to **GHCR** and are multi-arch
(`linux/amd64`, `linux/arm64`):

```sh
docker pull ghcr.io/go-steer/switchboard:latest        # newest release
docker pull ghcr.io/go-steer/switchboard:main          # tip of main (every merged PR)
docker run --rm -e SWITCHBOARD_DAEMON_TOKEN ghcr.io/go-steer/switchboard:0.1.0 \
  --daemon-url http://core-agent:7777
```

Or build it locally:

```sh
docker build -t ghcr.io/go-steer/switchboard:dev .
```

The image is `gcr.io/distroless/static-debian12:nonroot` — no shell, no package
manager. Default entrypoint is `switchboard serve`.

**Publishing.** [`release-images.yml`](.github/workflows/release-images.yml)
builds the multi-arch image, stamps build identity into `switchboard version`,
and pushes to GHCR — mirroring core-agent:

- **Every merged PR** (push to `main`) publishes a floating `:main` and an
  immutable `:main-<short-sha>` for development / staging.
- **A semver tag** (`vX.Y.Z`) publishes `X.Y.Z`, `X.Y`, `X`, and `latest`. A
  `-rc`/prerelease tag publishes only its exact version and never moves `latest`.

Every image is signed with **Sigstore keyless** (cosign, GitHub OIDC → Fulcio,
logged in Rekor). Verify before deploying:

```sh
cosign verify ghcr.io/go-steer/switchboard:v0.1.0 \
  --certificate-identity-regexp '^https://github.com/go-steer/switchboard' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

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
