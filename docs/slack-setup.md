# Slack: setup, testing, and demo

How to stand up the Slack integration end to end. Ingress is **Socket Mode** —
an outbound WebSocket — so switchboard needs no public webhook and a laptop
behind NAT can serve a real app.

The daemon behind it is the same one Google Chat uses and is documented once, in
[daemon-setup.md](daemon-setup.md). Read that first; this page is only the
Slack half. Design rationale is in [DESIGN.md](DESIGN.md).

## What the adapter engages on

`app_mention`, and nothing else. This matters more than it sounds:

- A mention roots a conversation on its **thread**; further mentions in that
  same thread continue it (same conversation key ⇒ same core-agent session).
- A **plain thread reply with no mention is ignored** — a later phase. So is a
  DM that does not mention the app. Every turn needs an `@switchboard`.
- A mention that *tightly* matches a gateway verb — `@switchboard progress`, or
  `@switchboard progress status` — configures the gateway instead of running an
  agent turn. Three or more tokens is treated as agent input, so
  `@switchboard progress on the ticket?` still reaches the daemon.
- A native slash command (`/switchboard progress status`) is handled too, if you
  configure one. It is acked **ephemerally**, visible only to the invoker.
- One thing arrives that is not a message: a **button press** on a permission
  question, when the gateway runs with `--approvals`. It answers a question
  switchboard asked; it never starts a turn. Slack only delivers it if the app
  has *Interactivity & Shortcuts* enabled (step 3 below).

## Prerequisites

- A Slack workspace where you can install an app. A free workspace is fine.
- A **core-agent daemon** and its bearer token — [daemon-setup.md](daemon-setup.md).
- **switchboard itself**, built from this checkout:

  ```sh
  go build -o /tmp/switchboard ./cmd/switchboard
  /tmp/switchboard version
  ```

  `version` prints the commit Go stamped into the binary, and a plain `go build`
  from a linked `git worktree` does not stamp the commit you built: it reports
  the enclosing checkout's HEAD if there is one and nothing at all if there is
  not, and the output gives no sign either way. Pass the commit yourself when it
  has to be right:

  ```sh
  go build -ldflags "-X github.com/go-steer/switchboard/internal/version.Commit=$(git rev-parse --short=8 HEAD)" \
    -o /tmp/switchboard ./cmd/switchboard
  ```

  That costs you `built unknown` and the `, modified` dirty marker, since an
  injected commit short-circuits the whole VCS stamp.
  [docs/googlechat-setup.md](googlechat-setup.md) has the full account, including
  how to put the date back.

## Create the app

At <https://api.slack.com/apps> → **Create New App** → **From scratch**:

1. **Socket Mode** → enable. Slack generates an **app-level token** (`xapp-…`)
   with the `connections:write` scope. Copy it now; it is not shown again.
2. **Event Subscriptions** → enable, and under *Subscribe to bot events* add
   **`app_mention`**. Socket Mode needs no Request URL — leave it empty.
3. **Interactivity & Shortcuts** → enable. Socket Mode needs no Request URL here
   either — leave it empty. This toggle is what makes Slack deliver button
   presses, and it is off by default: without it a permission question posted by
   `--approvals` renders its buttons, they click, and nothing is ever sent. Skip
   this step if you are not running `--approvals`.
4. **OAuth & Permissions** → add the bot scopes in the table below.
5. *(optional)* **Slash Commands** → create `/switchboard`. The command word is
   discarded and the rest is parsed as `verb args…`, so one command covers every
   gateway verb. Requires the `commands` scope.
6. **Install to Workspace** → this mints the **bot token** (`xoxb-…`).
7. **Invite the bot to a channel**: `/invite @switchboard`. Slack will not let it
   post into a channel it is not a member of, and the failure (`not_in_channel`)
   arrives on the *reply*, not on the mention — so it looks like the app read
   your message and ignored it.

## Scopes, and what each one is for

Derived from the API calls the adapter actually makes (`pkg/chat/slack/slack.go`):

| Scope | Token | Needed for |
|-------|-------|------------|
| `connections:write` | app-level (`xapp-`) | opening the Socket Mode connection |
| `app_mentions:read` | bot (`xoxb-`) | receiving `app_mention` at all |
| `chat:write` | bot | `chat.postMessage`, and `chat.update` / `chat.delete` for progress messages |
| `users:read` | bot | `users.info`, to resolve the caller |
| `users:read.email` | bot | the email on that user record |
| `commands` | bot | the optional native slash command |

Only `chat:write` is unconditional. `--caller-id id` asserts the raw Slack user
ID and never calls `users.info`, so both `users:read*` scopes drop away — at the
cost of provisioning `U0123ABC` in the daemon instead of an address a human can
guess. `--progress-mode off` or `stream` never edits or deletes, though
`chat:write` covers posting regardless. And an `--outbound-only` deployment
(below) reaches none of the inbound path, which drops `connections:write`,
`app_mentions:read` and `commands` as well.

## Tokens

Both are read from **env vars**, never accepted as flag values:

```sh
export SWITCHBOARD_SLACK_APP_TOKEN=xapp-…   # Socket Mode app-level token
export SWITCHBOARD_SLACK_BOT_TOKEN=xoxb-…   # bot user OAuth token
export SWITCHBOARD_DAEMON_TOKEN=…           # the one dev/demo/daemon printed
```

Rename the vars with `--slack-app-token-env` / `--slack-bot-token-env` /
`--token-env` if your secret manager insists on different names.

The app-level token is the *inbound* half and nothing else. A deployment that
only posts — a digest job driving the outbound ingress — can do without it, by
passing `--outbound-only` ([#23]). switchboard then opens no WebSocket, and
steps 1 to 3 of *Create the app* (Socket Mode, `app_mention`, interactivity) are
not needed at all. Nor are most of the scopes: `connections:write`, `app_mentions:read`, both
`users:read*` and `commands` are all reached only from the inbound path, so an
outbound-only bot token needs `chat:write` and nothing else. It still has to be
invited to the channels it posts into. The daemon token goes too — there is no
turn to run, so `$SWITCHBOARD_DAEMON_TOKEN` is not read. Startup says which mode
it is in:

```
2026-08-19T16:00:00.123Z INFO  switchboard: outbound-only: posting to slack, receiving nothing
```

It has to be said on purpose. An app token that is merely *missing* is a
failure, not a mode: a bridge whose secret was emptied by a bad rotation must
come back refusing to start, not quietly posting and answering nobody. Pass
`--outbound-only` with an app token still set and it is ignored, with a warning
naming the var.

There is no `auth.test` at start in this mode — that call is part of opening the
socket — so a wrong `xoxb-` token is not caught until the first post, which
fails with `502`.

`--outbound-only` without `--ingress-addr` leaves no direction at all, and
`serve` exits saying so rather than idling.

[#23]: https://github.com/go-steer/switchboard/issues/23

## Run it

```sh
/tmp/switchboard serve --daemon-url http://127.0.0.1:7777
```

`--platform slack` is the default, so it is optional. On start a bridged run
calls `auth.test` and logs `slack: connected as <name> (<id>)` — if that line is
missing, the tokens are wrong and nothing downstream will work. An outbound-only
run never opens the socket, so it never makes that call either.

## Demo script

In the order that shows what is there:

1. **@-mention the app in a channel.** The reply lands in a thread rooted on your
   message. Mention it again *in that thread* and ask it to recall something from
   the first turn — same thread is the same session, which is the whole
   conversation model.
2. **Send markdown**: `**bold**`, a `[link](https://example.com)`, a `## header`,
   a fenced code block. Slack has no markdown, so this is translated to mrkdwn;
   no delimiters should be visible. (Needs a real model — echo cannot produce
   markup. See [daemon-setup.md](daemon-setup.md#echo-or-a-real-model).)
3. **Restart with `--slack-rich-blocks`** and ask for something structured — a
   table, a nested list. Block Kit renders headers, lists, tables and code as
   real elements instead of flat text. The mrkdwn always rides along as the
   notification fallback, and a payload Slack rejects (`invalid_blocks`) silently
   retries as text, so a rich render can never cost you a reply.
4. **Progress modes.** Default is `indicator`: a "⏳ Working…" placeholder that is
   deleted when the answer arrives. `--progress-mode status` edits one message in
   place to name the running tool; `stream` posts a notice per tool; `off` shows
   nothing. Ask for something slow enough to watch.
5. **`@switchboard progress status`** — a mention command; the ack posts in the
   thread. Then **`/switchboard progress status`** if you configured the slash
   command — that ack is ephemeral, visible only to you.
6. **Stop the daemon before sending.** An error notice appears in the thread
   rather than the thread going silent, and its wording distinguishes a
   transient failure from a terminal one.
7. **Stop the daemon *mid-turn*.** After about 90 seconds of failed reconnects
   the thread is told contact was lost and the progress message is retired.
   Bring the daemon back up before then and the relay resumes silently, which
   is the point — a rolling restart should not interrupt anyone.
8. **Ask for a very long answer** (over ~3900 characters), with a fenced code
   block in it. It is split into several ordered in-thread posts rather than
   being truncated, and a block the split lands inside is closed and reopened
   so every post renders its code as code.

Not on this list, because Slack has no equivalent: the Chat welcome card. Slack
apps get no "added to space" event the gateway acts on.

## Troubleshooting

| Symptom | Cause |
|---------|-------|
| No `slack: connected as …` line at start | bad `xoxb-` token, or Socket Mode not enabled — unless the banner says `outbound-only`, in which case no socket is opened and the line is not expected |
| Mention does nothing at all | `app_mentions:read` missing, or the event not subscribed |
| Mention works, reply never appears | bot not invited to the channel (`not_in_channel`) |
| Turns attributed to `U0123ABC` instead of an address | `users:read.email` missing — the log says `has no email (need users:read.email?)` |
| Thread shows `asserted-caller header rejected: identity is not provisioned` | the caller is not in the daemon's bearer table — [register it](daemon-setup.md#register-every-human-who-will-talk-to-the-app) |
| Rich blocks look flat | `--slack-rich-blocks` not set, or the payload was rejected and fell back (logged) |
| Permission buttons click but nothing happens, and no `perms …` line is logged | **Interactivity & Shortcuts** not enabled — Slack is not delivering the press. The agent stays blocked until it is |
| No permission question ever appears | `--approvals` not set, or the session's agent registered no prompt broker (logged as `session advertised permission prompts but serves none` when the frame and the route disagree) |
| A listed approver is told **Not an approver** | the log line `press by "…" is not an approver` says what identity actually arrived. If it is a `U0123ABC`, `users.info` is failing for that user — see the `users:read.email` row above — and `--approvers` is keyed by whatever `--caller-id` selects, not by whichever of the two you happened to write down |

## What is covered without a workspace

`pkg/chat/slack` unit-tests the mrkdwn translation, the Block Kit renderer
(including the fallbacks and the permission buttons), mention stripping,
conversation-key round-tripping, the interaction payload a press arrives as, and
the error classification. All of it runs on a checkout with no Slack account.

What it cannot tell you is whether Slack *renders* the blocks the way the
renderer assumes. Google Chat has a zero-cost answer to that question — golden
card JSON that pastes into Card Builder, [layer A](googlechat-setup.md#a-card-goldens) —
and **Slack has no equivalent today**. Block Kit Builder would be the place to
paste it, but nothing in the repo emits pasteable JSON; `TestToSlackBlocksMarshal`
marshals blocks and asserts on the result without writing a file. Worth adding.
