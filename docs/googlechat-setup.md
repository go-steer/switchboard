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
switchboard serve --platform googlechat … --googlechat-log-events 2>&1 \
  | grep -o 'googlechat: event .*' \
  | sed 's/^googlechat: event //' > /tmp/events.jsonl
```

Split that into one file per interaction under `testdata/events/`, scrub any
real user text, and rerun `-run Replay -update`. **The diff against the
hand-written goldens is the actual deliverable of layer C** — it is the only
thing that can tell you the decoder was wrong.

The flag is off by default because payloads carry message text and sender
identity. Do not leave it on in production.

Two add-on field names to check first, because they are transcribed from prose
and appear nowhere in the generated client — nothing offline can confirm them:

- `chat.addedToSpacePayload.interactionAdd`. If the real name differs it
  decodes to `false`, and every @mention-add double-posts a welcome *and* an
  answer into a brand-new space.
- `chat.user`. For a quick command there is no message, so this is the only
  source of the caller; a wrong name sends the daemon an empty caller.

## C. Live setup

### Prerequisites

- A **Google Workspace** account. Chat apps cannot be installed by a consumer
  gmail account — this is a hard gate.
- A GCP project with the **Google Chat API** and **Pub/Sub** enabled.
- `gcloud auth application-default login` locally, or workload identity
  in-cluster. Credentials are never passed as flags.

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

### Run it

Switchboard pulls from Pub/Sub, so it needs no inbound network path — a laptop
behind NAT can serve a real Chat app. That property is the whole reason the
add-on is served over Pub/Sub rather than HTTP.

```sh
export SWITCHBOARD_DAEMON_TOKEN=…
switchboard serve --platform googlechat \
  --google-project PROJECT_ID \
  --google-subscription switchboard-chat-sub \
  --googlechat-commands 1=progress \
  --googlechat-log-events \
  --daemon-url http://127.0.0.1:7777
```

For the daemon, either a real core-agent or the echo-provider configuration that
`cmd/switchboard/integration_test.go` already stands up — the echo provider is
enough for every step of the demo below and needs no model credentials.

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
