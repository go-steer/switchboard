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

## Prerequisites

- A Slack workspace where you can install an app. A free workspace is fine.
- A **core-agent daemon** and its bearer token — [daemon-setup.md](daemon-setup.md).
- **switchboard itself**, built from this checkout:

  ```sh
  go build -o /tmp/switchboard ./cmd/switchboard
  /tmp/switchboard version   # confirms the build identity you are testing
  ```

## Create the app

At <https://api.slack.com/apps> → **Create New App** → **From scratch**:

1. **Socket Mode** → enable. Slack generates an **app-level token** (`xapp-…`)
   with the `connections:write` scope. Copy it now; it is not shown again.
2. **Event Subscriptions** → enable, and under *Subscribe to bot events* add
   **`app_mention`**. Socket Mode needs no Request URL — leave it empty.
3. **OAuth & Permissions** → add the bot scopes in the table below.
4. *(optional)* **Slash Commands** → create `/switchboard`. The command word is
   discarded and the rest is parsed as `verb args…`, so one command covers every
   gateway verb. Requires the `commands` scope.
5. **Install to Workspace** → this mints the **bot token** (`xoxb-…`).
6. **Invite the bot to a channel**: `/invite @switchboard`. Slack will not let it
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

Two of these are conditional. `--caller-id id` asserts the raw Slack user ID and
never calls `users.info`, so both `users:read*` scopes drop away — at the cost of
provisioning `U0123ABC` in the daemon instead of an address a human can guess.
And `--progress-mode off` or `stream` never edits or deletes, though `chat:write`
covers posting regardless.

## Tokens

Both are read from **env vars**, never accepted as flag values:

```sh
export SWITCHBOARD_SLACK_APP_TOKEN=xapp-…   # Socket Mode app-level token
export SWITCHBOARD_SLACK_BOT_TOKEN=xoxb-…   # bot user OAuth token
export SWITCHBOARD_DAEMON_TOKEN=…           # the one dev/demo/daemon printed
```

Rename the vars with `--slack-app-token-env` / `--slack-bot-token-env` /
`--token-env` if your secret manager insists on different names.

## Run it

```sh
/tmp/switchboard serve --daemon-url http://127.0.0.1:7777
```

`--platform slack` is the default, so it is optional. On start the adapter calls
`auth.test` and logs `slack: connected as <name> (<id>)` — if that line is
missing, the tokens are wrong and nothing downstream will work.

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
6. **Stop the daemon mid-turn.** An error notice appears in the thread rather
   than the thread going silent, and its wording distinguishes a transient
   failure from a terminal one.
7. **Ask for a very long answer** (over ~3900 characters). It is split into
   several ordered in-thread posts rather than being truncated.

Not on this list, because Slack has no equivalent: the Chat welcome card. Slack
apps get no "added to space" event the gateway acts on.

## Troubleshooting

| Symptom | Cause |
|---------|-------|
| No `slack: connected as …` line at start | bad `xoxb-` token, or Socket Mode not enabled |
| Mention does nothing at all | `app_mentions:read` missing, or the event not subscribed |
| Mention works, reply never appears | bot not invited to the channel (`not_in_channel`) |
| Turns attributed to `U0123ABC` instead of an address | `users:read.email` missing — the log says `has no email (need users:read.email?)` |
| Thread shows `asserted-caller header rejected: identity is not provisioned` | the caller is not in the daemon's bearer table — [register it](daemon-setup.md#register-every-human-who-will-talk-to-the-app) |
| Rich blocks look flat | `--slack-rich-blocks` not set, or the payload was rejected and fell back (logged) |

## What is covered without a workspace

`pkg/chat/slack` unit-tests the mrkdwn translation, the Block Kit renderer
(including the fallbacks), mention stripping, conversation-key round-tripping,
and the error classification. All of it runs on a checkout with no Slack account.

What it cannot tell you is whether Slack *renders* the blocks the way the
renderer assumes. Google Chat has a zero-cost answer to that question — golden
card JSON that pastes into Card Builder, [layer A](googlechat-setup.md#a-card-goldens) —
and **Slack has no equivalent today**. Block Kit Builder would be the place to
paste it, but nothing in the repo emits pasteable JSON; `TestToSlackBlocksMarshal`
marshals blocks and asserts on the result without writing a file. Worth adding.
