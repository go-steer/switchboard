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

- `ack-with-values.json` — the escaped `&lt;off|…&gt;` in the `decoratedText`.
  Chat accepts only `<b> <i> <s> <a> <br>` there, so the angle brackets in the
  command's argument list have to arrive escaped. The values are in the ack the
  router wrote, not added by the card: no card carries a button row any more
  (#28), and `ack-plain.json` is the other ack — the one confirming a change,
  which names nothing.
- `answer.json` — the only card using `textSyntax: MARKDOWN`. Checked in the
  Card Builder on 2026-08-17: `[label](url)` renders as a live hyperlink, and a
  fenced block renders as monospace with its interior newlines and indentation
  intact. That is what lets `answerCard` pass a model turn's own markup
  through instead of translating it to HTML first — so a diff here that turns
  a link literal is a regression worth chasing, not a rendering quirk.
- `welcome.json` — the first thing anyone installing the app will see. It is
  also where a card's markup stops short of the text path's: `toCardHTML`
  passes backticks through verbatim, because a card has no monospace style to
  map them onto, so the `` `progress <off|…>` `` in the second line renders as
  literal backticks around plain text. The same is true of `ack-with-values`,
  and it has not been checked in the Card Builder — worth a look next time
  someone has it open, and worth fixing in the renderer rather than the
  wording if it reads badly.

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

The card-click payloads cannot be replaced that way: no card the gateway sends
has a button, and Chat delivered no click to the subscription when one did
(#28). A legacy app would have received that click, but the gateway renders no
button in either dialect, so neither can produce real traffic. They stay
hand-written, pinning the decode path for the HTTP ingress in
[#29](https://github.com/go-steer/switchboard/issues/29).

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
email, avatar URL, and `domainId`; the Workspace `customer` id; the space id,
its `displayName` and its `spaceUri`; the message and thread ids, which are
derived from the same base64url id and appear in `message.name` and
`thread.name`; the client's `commonEventObject.timeZone`; and a
`configCompleteRedirectUri` with a token in the query string. Replace each value
with something of the same shape — `addon-live-message.json` is the worked
example, and the fabricated ids in the corpus all follow one template so a real
one left behind stands out — and change nothing else: the shape is the whole
point.

The flag is off by default because payloads carry message text and sender
identity. Do not leave it on in production.

Two add-on field names were transcribed from prose and appear nowhere in the
generated client, so nothing offline could confirm them. Both have since been
confirmed against captured traffic:

- `chat.addedToSpacePayload.interactionAdd`. **Confirmed** by
  `addon-live-mention-add.json` — an @mention-add into a brand-new space really
  does arrive with `"interactionAdd": true`, spelled exactly that way, so the
  suppression fires rather than decoding to `false`. That was the risk this
  bullet was written about, and it is retired. What the pair then showed is a
  different bug, since fixed: `addon-live-mention-add-message.json` is the second
  event Chat sends for the same action, and it is a bare `@Switchboard` with no
  `argumentText`, which the decoder ignored too. The suppression deferred to a
  follow-up that never got answered, so an @mention-add got no reply at all
  (#55). A bare mention now decodes to the welcome, in any space rather than only
  a just-added one, so the follow-up always answers. The pair stays in the corpus
  as the regression: the add half must replay to `ignored` and the message half
  to the welcome post, because both posting is the double-reply the suppression
  exists to prevent.
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
  /tmp/switchboard version
  ```

  `version` prints the commit Go stamped into the binary, and a plain `go build`
  from a **linked `git worktree`** does not stamp the commit you built. Go looks
  for a `.git` *directory*; a worktree's `.git` is a file, so it keeps walking
  up. Which of two things happens depends on where the worktree sits:

  - Nested inside another checkout — the layout this repo's own
    `.claude/worktrees/` uses — Go finds *that* repository and stamps **its**
    HEAD. A real commit, from a real repository, that is not the code in front
    of you, and nothing in the output says so. This is the dangerous one.
  - Anywhere else (`git worktree add ../feature`) there is no `.git` directory
    to find and Go stamps nothing: `commit none, built unknown`. Wrong, but
    visibly wrong.

  Pass the commit yourself when it has to be right:

  ```sh
  go build -ldflags "-X github.com/go-steer/switchboard/internal/version.Commit=$(git rev-parse --short=8 HEAD)" \
    -o /tmp/switchboard ./cmd/switchboard
  ```

  An injected `Commit` short-circuits the VCS stamp entirely, so read what that
  costs before trusting the output: the build date goes (`built unknown`) and so
  does the `, modified` marker — the binary will name an exact commit without
  admitting the tree had uncommitted changes, which is usually the very reason
  you are building locally. `internal/version/version.go` is the precedence
  rule. Add the date back with a second `-X` **inside the same quotes**:

  ```sh
  go build -ldflags "\
    -X github.com/go-steer/switchboard/internal/version.Commit=$(git rev-parse --short=8 HEAD) \
    -X github.com/go-steer/switchboard/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /tmp/switchboard ./cmd/switchboard
  ```

  Nothing restores the dirty flag; only committing does. This is the same
  mechanism the `Dockerfile` uses, which is why the published images have never
  had the problem — though a local `docker build` without `--build-arg COMMIT`
  falls back to its `ARG COMMIT=none` default and lands in the same place.

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
- **Steps 2, 5 and 6 of the demo script need a real model.** Echo covers
  commands, card rendering, progress modes and the welcome card, but not
  markdown, a structured answer or a long one with code in it, because those are
  things the daemon has to actually write.

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

### Outbound ingress

`--ingress-addr` works on Chat as it does on Slack ([#39]). It needs no extra
Chat configuration — posting is the same REST call and the same ADC credentials
egress already uses — but two things differ from Slack and are worth knowing
before the first `403`:

- **The app must already be a member of the space.** Chat has no "invite by
  posting": an app posts only into spaces it has been added to, and adding it is
  a human action in the Chat UI. A post into a space the app is not in comes
  back as `403` or `404` — Chat does not always admit that a space it cannot see
  exists — and either reads exactly like a broken service-account grant. Check
  membership before re-issuing credentials.
- **The space id is not the thread id.** `conversation` is `spaces/AAA` for a
  new top-level message, or `spaces/AAA:spaces/AAA/threads/BBB` to post into an
  existing thread. Thread names come from Chat, so the way to get one is to post
  and keep the `conversation` in the response — it names the thread the message
  landed in.

```sh
export SWITCHBOARD_INGRESS_TOKEN=…
/tmp/switchboard serve --platform googlechat \
  --google-project PROJECT_ID \
  --google-subscription switchboard-chat-sub \
  --ingress-addr 127.0.0.1:8080 \
  --ingress-allow spaces/AAA \
  --daemon-url http://127.0.0.1:7777

curl -sS localhost:8080/v1/messages \
  -H "Authorization: Bearer $SWITCHBOARD_INGRESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"conversation":"spaces/AAA","text":"## disk-pressure\n\nnode-7 is at 91%"}'
# {"conversation":"spaces/AAA:spaces/AAA/threads/BBB","id":"spaces/AAA/messages/CCC"}
```

That text has a heading, so under the default `--googlechat-cards rich` it
arrives as a card — an ingress post renders exactly as an agent answer does.
Send a line without structure to get plain text.

### Outbound-only

The *inbound* half of this page is what a deployment that only posts can skip
([#23]): the topic, the subscription, its `roles/pubsub.subscriber` binding, and
the app's **Connection settings**. Pass `--outbound-only` and switchboard builds
no Pub/Sub client at all.

Everything egress needs stays: the service account and ADC, the Chat app
configuration, and the app's membership of each space it posts into. Switchboard
still builds the Chat REST client from ADC at startup and exits if it cannot, so
`chat.bot` is as required here as in a bridged run. What goes instead is the
*daemon* token — an outbound-only run starts no turn, so `--token-env` is never
read.

```sh
export SWITCHBOARD_INGRESS_TOKEN=…
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/chat-sa-key.json
/tmp/switchboard serve --platform googlechat --outbound-only \
  --ingress-addr 127.0.0.1:8080 \
  --ingress-allow spaces/AAA
# 2026-08-19T16:00:00.123Z switchboard: outbound-only: posting to googlechat, receiving nothing
```

The mode is declared, not inferred from the absence of a subscription: a
deployment whose `--google-subscription` went missing in an edit is a bridge
that broke, and it fails at startup rather than coming back as a process that
posts, passes `/healthz` and answers nobody. With `--outbound-only` the two
Pub/Sub flags are ignored if set — including their `SWITCHBOARD_GOOGLE_PROJECT`
/ `SWITCHBOARD_GOOGLE_SUBSCRIPTION` fallbacks — with a warning saying so.

Without the flag the two go together: one alone is refused at startup rather
than read as "receive from somewhere unnamed". Dropping only the subscription
would otherwise leave a project configured for a client nothing pulls from.

[#23]: https://github.com/go-steer/switchboard/issues/23
[#39]: https://github.com/go-steer/switchboard/issues/39

### Demo script

In the order that shows what is new:

1. **DM the app.** A reply lands, threaded. Baseline still works.
2. **Send markdown**: `**bold**`, a `[link](https://example.com)`, a `## header`,
   a fenced code block. None of the delimiters should be visible — before this
   change they were.
3. **`/progress`** with no argument → an ack card naming the modes it takes.
   No buttons: for an add-on, a click never reached the subscription and errored
   in the UI ([#28](https://github.com/go-steer/switchboard/issues/28)), so a row
   of them would be a control that can only fail. Then **`/progress stream`** → the ack
   confirms the new mode, and names nothing, which is the one thing the row's
   removal cost.
4. **Long turn** → a progress card; **stop the daemon before sending** → an
   error notice card. **Stop it mid-turn** and the notice takes about 90
   seconds of failed reconnects to arrive, because a daemon that comes back
   inside that window should not have interrupted anyone.
5. **Ask for something structured** — "compare Cloud Run and GKE, with headings"
   → a sectioned answer card, which is what the default `rich` is for. Ask for
   something conversational and it stays plain text: an answer with no heading
   and no rule is not laid out as a card in any mode.
6. **Ask for a long answer with code in it** — "write me the Terraform for three
   project services, with the variables" is enough to clear the 4096-character
   ceiling. Under `rich` it is one card and a section too long for a single
   widget spills across several; restart with `--googlechat-cards=status` and
   the same answer arrives as several ordered posts instead. Either way every
   piece renders its code as code: no stray backticks at the seam, which is what
   used to happen when the split landed inside a fenced block.
7. Restart with `--googlechat-cards=off` → everything degrades to text.
8. **@-mention the app into a new room** → the welcome, once, in the thread the
   mention started. A mention-add sends two events and only the second answers:
   the added-to-space half suppresses itself (`interactionAdd`) and the bare
   `@Switchboard` that follows earns the welcome (#55). Two welcomes for one add
   is a regression, and so is silence — the latter used to be the symptom here
   and is easy to misread as a broken Pub/Sub grant. An add that is *not* an
   interaction — from the app directory rather than by mention — gets the welcome
   from the added-to-space event instead, at the top level of the space rather
   than in a thread, but no capture of that path exists yet.
9. **`@Switchboard` on its own in a room it is already in** → the welcome again.
   A bare mention is answered anywhere, not just on an add, so there is a way to
   ask what the app accepts without knowing a command.

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
