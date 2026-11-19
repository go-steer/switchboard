# dev/rig — the live test rig

A core-agent daemon and the two gateways, on your machine, against the real
Slack and Google Chat apps. It is for a test that only a live platform can
answer: what a card looks like, whether a press arrives, what an event
really carries. Unit and integration tests need none of this
(`dev/tools/ci` runs offline).

The rig can live on any number of machines, but only **one runs it at a
time**: see [One machine at a time](#one-machine-at-a-time).

| Script | Does |
|---|---|
| `dev/rig/build` | builds switchboard from this checkout and core-agent from its `main`, into the state directory |
| `dev/rig/daemon` | core-agent on `127.0.0.1:7777` ([`dev/demo/daemon`](../demo/daemon) with your config filled in) |
| `dev/rig/slack` | the Slack gateway, on the rig's own Slack app |
| `dev/rig/chat` | the Google Chat gateway over Pub/Sub; `--http` for the HTTP ingress |
| `dev/rig/tunnel` | a public HTTPS URL for `chat --http` (cloudflared quick tunnel) |
| `dev/rig/status` | what is built, what is running, whether core-agent `main` has moved |
| `dev/rig/stop` | stops everything the rig started on this machine |

Every script takes `switchboard serve` (or `dev/demo/daemon`) flags after
its own, and a repeated flag overrides the rig's: `dev/rig/daemon
--permissions ask`, `dev/rig/slack --progress-mode stream`.

## Where things live

Nothing a run needs to keep secret, and nothing it produces, is in the
checkout:

| | Path | Holds |
|---|---|---|
| scripts | `dev/rig/` | this; the same on every machine |
| config | `~/.config/switchboard-rig/` | `rig.env`, the Chat key, the Slack tokens. **Secrets: `chmod 700`, files `600`** |
| state | `~/.local/state/switchboard-rig/` | binaries, the daemon's token and session database, the tunnel URL, logs |

`SWITCHBOARD_RIG_ENV` points at a different `rig.env`; `RIG_STATE` in it
moves the state directory. The scripts refuse a secret file anyone else on
the machine can read.

State is per machine and disposable: delete it and the next `build` and
`daemon` recreate it, with a new daemon token and an empty session
database. The daemon token guards a loopback port, so it never travels.
The config is what a second machine needs a copy of.

Two guards back this up: `.gitignore` covers `dev/rig/*.env` and
`dev/rig/*.json`, and the `verify-no-secrets` presubmit fails on anything
shaped like a private key or a real Slack, Google API or GitHub token, in
tracked files and in untracked ones git is not ignoring.

## One machine at a time

Three things deliver an event to exactly one receiver, and none of them can
be seen from the other machine:

- **Slack Socket Mode** hands each event to one of the app's open
  connections. Two gateways on the same app split the mentions between them,
  and each looks broken from where you sit.
- **The Chat console's endpoint** names one place, Pub/Sub topic or HTTP
  URL. Whatever it names receives; nothing else does.
- **A Pub/Sub subscription** gives each message to one puller. Two gateways
  on `switchboard-chat-sub` split the messages.

So moving the rig is: `dev/rig/stop` on the old machine, then start on the
new one. `dev/rig/status` shows only this machine. If mentions are going
unanswered and nothing here is wrong, the first question is whether a rig
is still up somewhere else.

For the same reason the rig's Slack app is its own: the GKE demo gateway
uses the main `switchboard` app, and a rig on that app would split events
with it.

## Setting up a machine

Needs: `go` (any recent one; the toolchain directive fetches the rest),
`git`, `curl`, and `gcloud` for a real model. `cloudflared` only for the
HTTP ingress (downloaded on Linux; `brew install cloudflared` on macOS).

1. **Config.** Copy the template and fill it in:

   ```sh
   mkdir -p ~/.config/switchboard-rig && chmod 700 ~/.config/switchboard-rig
   cp dev/rig/rig.env.example ~/.config/switchboard-rig/rig.env
   ```

   Set `RIG_CALLERS` to the email addresses that will talk to the rig. The
   rest is filled in for the go-steer rig in `gke-demos-345619`.

2. **Secrets.** Two files, both `chmod 600`, at the paths `rig.env` names.
   Either copy them from a machine that has them:

   ```sh
   scp -p other:.config/switchboard-rig/{switchboard-chat.json,slack.env} ~/.config/switchboard-rig/
   ```

   or make them fresh:

   - `slack.env`: the rig app's two tokens, from
     [api.slack.com/apps](https://api.slack.com/apps) → the app → *Basic
     Information* → App-Level Tokens (`xapp-…`, scope `connections:write`)
     and *OAuth & Permissions* → Bot User OAuth Token (`xoxb-…`):

     ```sh
     SWITCHBOARD_SLACK_APP_TOKEN=xapp-…
     SWITCHBOARD_SLACK_BOT_TOKEN=xoxb-…
     ```

   - `switchboard-chat.json`: a key for the Chat app's service account.
     Every key minted is one more live credential, so copy an existing one
     where you can, and delete keys for machines you have retired
     (`gcloud iam service-accounts keys list`).

     ```sh
     gcloud iam service-accounts keys create ~/.config/switchboard-rig/switchboard-chat.json \
       --iam-account switchboard-chat@gke-demos-345619.iam.gserviceaccount.com
     chmod 600 ~/.config/switchboard-rig/switchboard-chat.json
     ```

3. **The model's credentials.** With the default `vertex` provider, the
   daemon calls Vertex as you:

   ```sh
   gcloud auth application-default login
   ```

   `RIG_MODEL=echo` needs nothing and is enough to test transport.

4. **Build** and check:

   ```sh
   dev/rig/build
   dev/rig/status
   ```

## Running it

A terminal each, the daemon first. After that, any of them can be
restarted on its own.

```sh
dev/rig/daemon
dev/rig/slack
dev/rig/chat            # or: dev/rig/tunnel, then dev/rig/chat --http
```

Both gateways share the one daemon, so a Slack thread and a Chat thread are
separate conversations against the same agent.

### Slack

Mention the rig app in a channel it is in (go-steer:
**@switchboard-dev-rig** in **#switchboard-rig-testing**). The bot is
invited to a channel like any user.

### Google Chat over Pub/Sub

Carries messages, cards and the welcome. Button presses and dialogs do not
arrive over Pub/Sub; for those, use HTTP.

Point the Chat app at Pub/Sub first (Google Cloud console → Google Chat
API → Configuration → *Connection settings* → Cloud Pub/Sub, topic
`projects/gke-demos-345619/topics/switchboard-chat-events`), and check the
subscription still exists (see [Pub/Sub subscriptions
expire](#pubsub-subscriptions-expire)).

### Google Chat over HTTP

```sh
dev/rig/tunnel           # prints https://<random>.trycloudflare.com
dev/rig/chat --http
```

Set the console's *Connection settings* → **HTTP endpoint URL** to the
tunnel's URL plus `/chat`; the tunnel prints the exact value. The URL is
random and changes each time the tunnel starts, so leave the tunnel up and
restart the gateway behind it.

Every request is answered `200` at once and the answer arrives later in the
thread; a rejected one is a `401` and an `ERROR` line naming the audience
it checked. An audience mismatch means the console and
`dev/rig/chat --http`'s `--googlechat-endpoint-url` disagree.

The tunnel URL is public. Requests to it are verified (Chat signs every
one), but keep it up only while you are testing.

### Testing approvals

```sh
dev/rig/daemon --permissions ask
dev/rig/slack --approvals
dev/rig/chat --http      # Chat buttons need the HTTP ingress
```

`ask` also turns off core-agent's built-in allow list, so even `ls` asks.

## Handing back

The Chat console stays wherever you last pointed it, so put it back when
you finish, or nothing receives Chat events (go-steer: the GKE demo
gateway, `RIG_CHAT_HOME_ENDPOINT` in `rig.env`):

1. `dev/rig/stop`
2. Console → *Connection settings* → **HTTP endpoint URL** =
   `https://switchboard-chat.demo.gke.ninja/chat`

The Slack app needs nothing; it is the rig's alone.

## Keeping it current

- **switchboard** is built from the checkout you run `build` in. After
  switching branches, `dev/rig/build switchboard`.
- **core-agent** is built from `main` (`RIG_CORE_AGENT_REF`), stamped with
  the commit, along with the persona from the same tree. `build` skips it
  when `main` has not moved; `status` says when it has. Test against
  current `main`: behaviour there drifts, and an old binary hides it.

## Traps

- **`GOOGLE_CLOUD_LOCATION=global`** for `gemini-3.7-flash`, which is
  global-only. A regional location answers `404 model_not_found`, which
  reads like a typo in the model name. `RIG_VERTEX_LOCATION` sets it.
- **`pkill -f switchboard`** hits the wrapper shells and anything else with
  the word in its command line. `dev/rig/stop` matches on the state
  directory's `bin/` path instead.
- **`--googlechat-log-events`** (on by default in `RIG_CHAT_ARGS`) writes
  message text and sender identity to the logs. They stay in the state
  directory; scrub a captured event before it becomes a committed fixture.
- **Callers.** A message from anyone not in `RIG_CALLERS` is refused by the
  daemon as "identity is not provisioned". Add them and restart the daemon.

### Pub/Sub subscriptions expire

A subscription nobody pulls from is deleted after 31 days by default, and
while the console points at HTTP nothing does. The gateway then stops with
`NotFound ... resource=switchboard-chat-sub`. Recreating it needs the
subscription and the service account's right to pull from it, which went
with it:

```sh
gcloud pubsub subscriptions create switchboard-chat-sub --project gke-demos-345619 \
  --topic switchboard-chat-events --expiration-period never
gcloud pubsub subscriptions add-iam-policy-binding switchboard-chat-sub --project gke-demos-345619 \
  --member serviceAccount:switchboard-chat@gke-demos-345619.iam.gserviceaccount.com \
  --role roles/pubsub.subscriber
```

`--expiration-period never` is what stops it happening again.

### Without a key file (untested)

With `RIG_CHAT_CREDENTIALS` unset, the Chat gateway uses Application Default
Credentials, which have to act as the Chat app's service account:

```sh
gcloud auth application-default login \
  --impersonate-service-account switchboard-chat@gke-demos-345619.iam.gserviceaccount.com
```

That needs `roles/iam.serviceAccountTokenCreator` on the account for you,
and it has not been tried here. It also changes the ADC the daemon uses, so
Vertex calls would go out as the Chat account too. Point one of the two at
a separate credentials file, or keep the key.
