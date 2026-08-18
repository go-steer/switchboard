# Google Chat: setup, testing, and demo

How to stand up the Google Chat integration end to end, and how to test it at
three levels of cost. Layers A and B need nothing but a checkout. Layer C needs
a Google Workspace domain and a GCP project.

Design rationale for the choices below is in [DESIGN.md](DESIGN.md) §3.3.

## What each layer can and cannot prove

The unit tests in `pkg/chat/googlechat` are thorough about adapter *logic*, and
structurally unable to say anything about two things: whether the events we
decode are the events Google sends, and whether the cards we build render the
way we think. Those need layers A and C respectively.

| Layer | Cost | Answers |
|-------|------|---------|
| A — card goldens | none | does a card look right, and did a change alter what users see? |
| B — event replay | none | given a payload, does the gateway route and render it correctly? |
| C — live app | Workspace + GCP | are these the real payloads, and does Chat accept these cards? |

## A. Card goldens

`pkg/chat/googlechat/testdata/cards/*.json` holds the exact JSON each card
builder emits, regenerated with:

```sh
go test ./pkg/chat/googlechat -run Golden -update
```

Two uses. In review, a diff in these files is a visible change to what users
see — the reason to pin them rather than assert on struct fields. And each file
pastes directly into Google's **Card Builder** web tool, which renders a card
from JSON with no app, no project, and no deploy. That is the cheapest way to
check a card before anyone tries it for real.

Worth looking at specifically:

- `ack-with-choices.json` — the button row, and the escaped `&lt;off|…&gt;` in
  the `decoratedText`. Chat accepts only `<b> <i> <s> <a> <br>` there, so the
  angle brackets in the command's argument list have to arrive escaped.
- `answer.json` — the only card using `textSyntax: MARKDOWN`. Checked in the
  Card Builder on 2026-08-17: `[label](url)` renders as a live hyperlink, and a
  fenced block renders as monospace with its interior newlines and indentation
  intact. That is what lets `answerCard` pass a model turn's own markup
  through instead of translating it to HTML first — so a diff here that turns
  a link literal is a regression worth chasing, not a rendering quirk.
- `welcome.json` — the first thing anyone installing the app will see.

## B. Event replay

`pkg/chat/googlechat/testdata/events/*.json` is a corpus of raw Pub/Sub
payloads, one per file, in both dialects. `TestReplay` pushes each through the
real `dispatch` path against fakes and records what the gateway did in
`testdata/replay/`:

```sh
go test ./pkg/chat/googlechat -run Replay          # assert
go test ./pkg/chat/googlechat -run Replay -update  # regenerate
```

An outcome of `"ignored"` is a result worth pinning too — a change that starts
answering the app's own messages shows up as a diff.

The corpus ships with hand-written payloads, which is exactly its weakness: they
prove the decoder matches somebody's reading of the documentation. Replace them
with real traffic as soon as you have a live app (below). Comparing
`addon-slash-command.json` with `legacy-slash-command.json` is the dialect
invariant in one glance — equivalent events must produce identical outcomes.

### Capturing real payloads

Run the gateway with `--googlechat-log-events`. Every inbound payload is logged
verbatim on one line, prefixed `googlechat: event `:

```sh
/tmp/switchboard serve --platform googlechat … --googlechat-log-events 2>&1 \
  | grep -o 'googlechat: event .*' \
  | sed 's/^googlechat: event //' > /tmp/events.jsonl
```

Split that into one file per interaction under `testdata/events/`, naming
captured files `addon-live-*.json` so their provenance stays visible, and rerun
`-run Replay -update`. **The diff against the hand-written goldens is the actual
deliverable of layer C** — it is the only thing that can tell you the decoder
was wrong.

Scrub before committing. A real payload carries the sender's display name,
email, avatar URL, and `domainId`; the space name and `spaceUri`; and a
`configCompleteRedirectUri` with a token in the query string. Replace each value
with something of the same shape — `addon-live-message.json` is the worked
example — and change nothing else: the shape is the whole point.

The flag is off by default because payloads carry message text and sender
identity. Do not leave it on in production.

Two add-on field names were transcribed from prose and appear nowhere in the
generated client, so nothing offline could confirm them:

- `chat.addedToSpacePayload.interactionAdd`. **Still unconfirmed** — it takes an
  @mention-add into a brand-new space. If the real name differs it decodes to
  `false`, and every such add double-posts a welcome *and* an answer.
- `chat.user`. **Confirmed** by `addon-live-message.json`, along with two things
  the corpus had wrong: the actor object carries an `email`, which the generated
  `chat/v1` `User` type has no field for and therefore silently dropped (hence
  `wireUser` in `event.go`), and `appCommandMetadata.appCommandId` really does
  arrive as a bare integer while the same message's `slashCommand.commandId` is
  a quoted one.

## C. Live setup

### Prerequisites

- A **Google Workspace** account. Chat apps cannot be installed by a consumer
  gmail account — this is a hard gate.
- A GCP project with the **Google Chat API** and **Pub/Sub** enabled.
- A **service account** in that project, and ADC resolving to it. Not your own
  `gcloud auth application-default login` — see
  [Credentials](#credentials-app-auth). Credentials are never passed as flags.
- A **built core-agent binary** and a bearer token for it. Switchboard is a
  gateway, not an agent; without a daemon behind `--daemon-url` it has nothing
  to say. See [The daemon side](#the-daemon-side) below.
- **switchboard itself**, built from this checkout — nothing below is on your
  `PATH` by default:

  ```sh
  go build -o /tmp/switchboard ./cmd/switchboard
  /tmp/switchboard version   # confirms the build identity you are testing
  ```

  The published image works too (`ghcr.io/go-steer/switchboard:main`), but a
  local binary is easier to restart between demo steps, and several of them ask
  you to.

### Pub/Sub

```sh
gcloud pubsub topics create switchboard-chat-events
gcloud pubsub subscriptions create switchboard-chat-sub \
  --topic switchboard-chat-events
```

Then grant Chat's push service account permission to publish to the topic. This
is the step that silently breaks everything when missed: the app looks correctly
configured and no events ever arrive. Confirm the exact principal in the Chat
API configuration page rather than trusting a copy-pasted address — Google
documents it on the Pub/Sub connection settings screen.

### Credentials (app auth)

Switchboard posts **as the Chat app**, which Chat calls app authentication and
which only a service account can do. The adapter asks for the `chat.bot` scope
(`googlechat.go`), and a user credential can never hold it: scopes on a user
credential are fixed at login, and `chat.bot` is not grantable to a human
account at all. `gcloud auth application-default login` therefore gets you a
working Pub/Sub pull and a `403 ACCESS_TOKEN_SCOPE_INSUFFICIENT` on the first
message the app tries to send.

```sh
gcloud iam service-accounts create switchboard-chat \
  --display-name "switchboard Chat app" --project PROJECT_ID

# ADC resolves to the SA for BOTH clients, so it needs the subscription too
gcloud pubsub subscriptions add-iam-policy-binding switchboard-chat-sub \
  --member serviceAccount:switchboard-chat@PROJECT_ID.iam.gserviceaccount.com \
  --role roles/pubsub.subscriber --project PROJECT_ID

gcloud iam service-accounts keys create /tmp/switchboard-chat.json \
  --iam-account switchboard-chat@PROJECT_ID.iam.gserviceaccount.com
export GOOGLE_APPLICATION_CREDENTIALS=/tmp/switchboard-chat.json
```

No Chat-specific IAM role is involved: what makes this service account *the
app* is living in the project where the Chat API is configured.

If org policy blocks service-account keys (`iam.disableServiceAccountKeyCreation`
— common in a managed org), impersonate instead:

```sh
gcloud iam service-accounts add-iam-policy-binding \
  switchboard-chat@PROJECT_ID.iam.gserviceaccount.com \
  --member user:you@example.com \
  --role roles/iam.serviceAccountTokenCreator --project PROJECT_ID

gcloud auth application-default login \
  --impersonate-service-account switchboard-chat@PROJECT_ID.iam.gserviceaccount.com
```

In-cluster, workload identity binds the same service account and none of this
applies.

### Chat app configuration

In the Google Cloud console, under the Chat API's **Configuration** page:

1. App name, avatar, description.
2. **Connection settings → Cloud Pub/Sub**, topic
   `projects/PROJECT_ID/topics/switchboard-chat-events`.
3. Enable **Receive 1:1 messages** and **Join spaces and group conversations**.
4. Add app commands. Each gets a numeric ID you choose — Chat identifies a
   command by that ID and never by name, so it has to match
   `--googlechat-commands`. Either add one command per verb
   (`/progress` = 1) or a single catch-all `/switchboard` and let the verb come
   from the argument text.
5. Visibility: make it available to yourself or a test group.

### The daemon side

Two different credentials are in play, and they are easy to conflate:

| Hop | Credential | Supplied by |
|-----|------------|-------------|
| Chat ↔ switchboard | GCP service account (ADC) | [Credentials](#credentials-app-auth) above |
| switchboard → core-agent | static bearer token | `$SWITCHBOARD_DAEMON_TOKEN` (rename with `--token-env`) |

The second hop is identical on every platform and is documented once, in
**[daemon-setup.md](daemon-setup.md)**: building core-agent, the three config
settings that have to line up (`proxy_identities` is the one that fails
quietly), `dev/demo/daemon`, and echo versus a real model. The short version:

```sh
go build -o /tmp/core-agent ./cmd/core-agent      # in a core-agent checkout
dev/demo/daemon --bin /tmp/core-agent --port 7777 --caller you@example.com
```

Two Chat-specific notes on top of that page:

- **Register the sender's identity before the demo.** With
  `allow_anonymous: false` an unknown caller is rejected, and Chat surfaces it
  as an error *card* in the thread: `POST /sessions: asserted-caller header
  rejected: identity is not provisioned`. With the default `--caller-id email`
  that identity is the address on the event payload; with `--caller-id id` it is
  the raw `users/1234567890`, which you cannot know before the first message —
  the first rejection is where you learn it.
- **Steps 2 and 7 of the demo script need a real model.** Echo covers commands,
  buttons, progress modes and the welcome card, but not markdown or a structured
  answer, because those are things the daemon has to actually write.

### Run it

Switchboard pulls from Pub/Sub, so it needs no inbound network path — a laptop
behind NAT can serve a real Chat app. That property is the whole reason the
add-on is served over Pub/Sub rather than HTTP.

```sh
export SWITCHBOARD_DAEMON_TOKEN=…   # the one dev/demo/daemon printed
/tmp/switchboard serve --platform googlechat \
  --google-project PROJECT_ID \
  --google-subscription switchboard-chat-sub \
  --googlechat-commands 1=progress \
  --googlechat-log-events \
  --daemon-url http://127.0.0.1:7777
```

### Demo script

In the order that shows what is new:

1. **DM the app.** A reply lands, threaded. Baseline still works.
2. **Send markdown**: `**bold**`, a `[link](https://example.com)`, a `## header`,
   a fenced code block. None of the delimiters should be visible — before this
   change they were.
3. **`/progress`** with no argument → an ack card with a button per mode.
4. **Click a button** → the card is rewritten *in place*; no second message.
5. **Click the same button again** → the card ends in the same state. Pub/Sub
   redelivers, so idempotence is a property to actually check.
6. **Long turn** → a progress card; **stop the daemon mid-turn** → an error
   notice card.
7. Restart with `--googlechat-cards=rich`, ask for something structured → a
   sectioned answer card.
8. Restart with `--googlechat-cards=off` → everything degrades to text.
9. **@-mention the app into a new room** → the welcome card, exactly once. A
   mention-add sends two events and only one may be answered.

### Testing both dialects

Converting an app to a Workspace add-on is **irreversible** and applies to every
user at once, so it cannot be A/B tested on one app. Use two apps in two
projects:

| | App 1 | App 2 |
|---|---|---|
| Framework | Chat API interaction events | converted to Workspace add-on |
| Topic | its own | its own |
| Subscription | its own | its own |

Run a switchboard instance against each and walk the same demo script through
both. Identical behaviour is the claim the per-event dialect detection makes,
and this is the only way to check it. Capture from both — the corpus should end
up with a real payload pair for every interaction type.

Convert App 2 last, once you have captured its pre-conversion payloads: those
legacy fixtures are not recoverable afterwards.
