# The daemon behind switchboard

Switchboard is a gateway and ships no agent, so `--daemon-url` needs a
**core-agent** daemon behind it or there is nothing to say. This is the daemon
side of both platform runbooks — [Slack](slack-setup.md) and
[Google Chat](googlechat-setup.md) — and none of it is platform-specific: the
same daemon serves either, or [both at once](#both-platforms-one-daemon).

## Two credentials, not one

They are easy to conflate, and they fail differently:

| Hop | Credential | Supplied by |
|-----|------------|-------------|
| chat platform ↔ switchboard | Slack app + bot tokens, or a GCP service account | the platform runbook |
| switchboard → core-agent | static bearer token | `$SWITCHBOARD_DAEMON_TOKEN` (rename with `--token-env`) |

The daemon token is **mandatory** — switchboard refuses to start without it
(`cmd/switchboard/main.go`), and `daemon.New` rejects an empty one. It rides as
`Authorization: Bearer …` on every verb. Never pass it as a flag value.

## Build the binary

Even the echo provider is a *core-agent* flag, so a built core-agent binary is
a prerequisite whatever model you end up running:

```sh
go build -o /tmp/core-agent ./cmd/core-agent   # in a core-agent checkout
```

## Three things have to line up

Before a turn can flow, the daemon's `.agents/config.json` needs:

- `attach.multi_session.enabled` and a `listen` address matching `--daemon-url`.
- `auth.kind = "bearer_table"` with a table file holding switchboard's identity
  and the token it presents.
- **`proxy_identities` listing that identity.** Without it the token still
  authenticates, `X-Asserted-Caller` is ignored, and every turn is attributed to
  switchboard rather than the human who sent it. This is the one that fails
  quietly.

`dev/demo/daemon` writes all of that and runs the binary:

```sh
dev/demo/daemon --bin /tmp/core-agent --port 7777
```

It prints the `SWITCHBOARD_DAEMON_TOKEN` to export, and keeps its config,
bearer table, and session db in `./.demo-daemon` (git-ignored) so restarts do
not rotate the token. Both files are rewritten on every run — change them with
flags, not by editing.

## Register every human who will talk to the app

With `allow_anonymous: false`, an identity the bearer table does not know is
rejected — confirmed against a live daemon, which answers `POST /sessions:
asserted-caller header rejected: identity is not provisioned`, surfaced into the
thread as an error notice. Register the caller and restart:

```sh
dev/demo/daemon --bin /tmp/core-agent --caller you@example.com   # repeatable
```

Which identity to register depends on `--caller-id`, which defaults to `email`
on **both** platforms: the address Slack resolves via `users.info`, or the one
Google Chat carries on the event payload. With `--caller-id id` it is the raw
platform user ID instead — `U0123ABC` on Slack, `users/1234567890` on Chat — and
those are values you cannot know before the first message, so the first
rejection is where you learn them.

## Echo, or a real model

The default model is `echo`, which needs no credentials and covers every demo
step that exercises the *gateway*: commands, cards, progress modes, threading,
error notices. Steps that turn on what the daemon actually *says* — markdown
rendering, a structured answer laid out as blocks or cards — want a real one:

```sh
export ANTHROPIC_API_KEY=…
dev/demo/daemon --bin /tmp/core-agent --model claude-opus-5 --provider anthropic
```

The daemon inherits the shell's environment, which is how the API key reaches
it, so export it in the same terminal. Anything after `--` is appended to
core-agent's command line verbatim.

`--provider` defaults to `echo` **only while the model is still `echo`**.
Naming a real model and inheriting `--provider=echo` would silently run echo
anyway, so once `--model` names something else the script says nothing and lets
core-agent decide. Its accepted values are core-agent's, not switchboard's
(`cmd/core-agent/main.go`):

| Value | Provider |
|-------|----------|
| `gemini` | Gemini API direct (`GOOGLE_API_KEY` / `GEMINI_API_KEY`) |
| `vertex` | Gemini on Vertex (project + location, or `GOOGLE_CLOUD_PROJECT` / `GOOGLE_CLOUD_LOCATION` — see the location trap below) |
| `anthropic` | `api.anthropic.com` (`ANTHROPIC_API_KEY`) |
| `anthropic-vertex` | Claude on Vertex |
| `echo` | no credentials; echoes the prompt back |
| `scripted` | replays a JSONL transcript (`--script`) |

Omitting `--provider` entirely is a real option: core-agent auto-detects from
the environment, and says which signals it looked for if it cannot.

**`vertex` and the location trap.** Observed here on 2026-08-19, running the
demo rig against project `gke-demos-345619`: `gemini-3.7-flash` returned 404
with `GOOGLE_CLOUD_LOCATION=us-central1` and answered normally with `global`.
That is one model in one project on one day, not a documented rule — the
authority is the model's own availability list, which is where to check before
pinning a region. It is written down because of how the failure reads: a 404
from a model name you know exists looks like a typo or a missing API
enablement, so the location is the last thing you suspect. If a model 404s,
try `global` before you go looking for anything else.

## What keeps this honest

The config `dev/demo/daemon` emits is the same shape
`cmd/switchboard/integration_test.go` stands up, so the integration suite is
what stops it rotting. It also stands up a real daemon rather than a fake, which
is the only place some failures are visible at all — the duplicate-turn bug
(one message, two replies) reproduced there and nowhere else:

```sh
CORE_AGENT_BIN=/tmp/core-agent go test -tags=integration ./cmd/switchboard \
  -run Integration -v
```

Against a daemon you did not build yourself, the first thing switchboard logs
for a conversation says which one it reached:

```
relay C0123:1723742400.0001: connected to core-agent/0.9.2 speaking 1.5.0
```

If that daemon is too old to send an event switchboard reads, the next line
names it — and names the consequence, which is a feature that silently does
nothing rather than an error:

```
relay C0123:1723742400.0001: daemon does not advertise status-update, turn-error;
the features reading those events will stay silent
```

Nothing is negotiated: the daemon sends what it sends. The line exists because
the symptom of a missing event is an *absence* — a thread that stays quiet after
a failed turn, a reply with no usage footer — and an absence looks nothing like
a version mismatch from the outside.

## Both platforms, one daemon

`--platform` takes one value, so Slack and Google Chat are **two switchboard
processes** against one daemon — not one process serving both. Since both
assert the sender's email, one registered identity covers them, and the same
human reaching the agent from either window is one caller to core-agent.

```sh
# terminal 1 — the daemon
export ANTHROPIC_API_KEY=…
dev/demo/daemon --bin /tmp/core-agent --port 7777 \
  --model claude-opus-5 --provider anthropic \
  --caller you@example.com

# terminal 2 — Slack
export SWITCHBOARD_DAEMON_TOKEN=…          # the one the daemon printed
export SWITCHBOARD_SLACK_APP_TOKEN=xapp-…
export SWITCHBOARD_SLACK_BOT_TOKEN=xoxb-…
/tmp/switchboard serve --daemon-url http://127.0.0.1:7777

# terminal 3 — Google Chat
export SWITCHBOARD_DAEMON_TOKEN=…          # the same one
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/chat-sa-key.json
/tmp/switchboard serve --platform googlechat \
  --google-project PROJECT_ID \
  --google-subscription switchboard-chat-sub \
  --googlechat-commands 1=progress \
  --daemon-url http://127.0.0.1:7777
```

Both processes present the same daemon token and therefore the same proxy
identity, which is correct — the *human* is asserted per turn, not the gateway.
They do not collide on ports: `--metrics-addr` and `--ingress-addr` both default
to disabled, so nothing is bound unless you ask for it.

What they do not share is the conversation→session map, which lives in each
process. A Slack thread and a Chat thread are separate core-agent sessions with
separate history, even for the same person — the identity is shared, the
conversation is not. That is the interesting thing to watch in a two-platform
run, and the reason to do one: per-caller MCP credentials resolve from the
identity, so a tool authorized in Slack should work in Chat without a second
authorization.

## Not for deployment

`dev/demo/daemon` is for demos and local testing. It writes a bearer token to
disk and prints it to the terminal, runs the agent with `permissions.mode=yolo`,
and puts everything in a scratch directory. None of that belongs in a
deployment; see [deploy/](../deploy) for the shipped posture.
