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
                             ▲       (X-Asserted-Caller = the chat user)
                             │
   another service ──────────┘  POST/PATCH /v1/messages (optional outbound ingress)
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

# Version, and the commit Go stamped in — which is not always the commit you
# built: see docs/googlechat-setup.md if you build from a linked git worktree.
switchboard version
```

Slack app creation, the scopes each API call needs, and a demo script:
[docs/slack-setup.md](docs/slack-setup.md). The daemon that has to be behind
`--daemon-url` — switchboard ships no agent — is
[docs/daemon-setup.md](docs/daemon-setup.md), which also covers running Slack
and Google Chat side by side against one daemon.

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
  --googlechat-commands 1=progress \
  --daemon-url http://127.0.0.1:7777
# Message the app in a Chat space (or @-mention it in a room); the reply lands
# in the same thread. The asserted caller is the sender's email, as on Slack;
# --caller-id id asserts the raw users/NNN resource name instead.
```

One-time setup: in the Chat API app configuration, set the **Connection
settings** to *Cloud Pub/Sub* and point it at your topic, then create a pull
subscription on that topic for switchboard to consume.

#### Workspace add-on mode

Google is moving Chat apps from the Chat-API *interaction events* framework to
**Google Workspace add-ons that extend Chat**, which changes the shape of every
event: the payload arrives wrapped in a `chat` object with one of
`messagePayload` / `appCommandPayload` / `buttonClickedPayload` /
`addedToSpacePayload` rather than a flat event with a `type` discriminator.

Switchboard **detects the dialect per event** and understands both. That matters
because converting an app to add-on mode in the console is irreversible and
applies to every user at once — so the rollout does not have to be synchronized
with a switchboard deploy, and a rollback of switchboard does not strand the app.

Pub/Sub remains a supported add-on architecture, so the no-public-webhook
posture is unchanged. The trade-off it buys: an add-on served over Pub/Sub can
do nothing that needs a synchronous HTTP response, so no **dialogs**. And **no
callback buttons**, which is a separate limit rather than a consequence of that
one — a click could have been answered by patching the message afterwards, but
it never arrives: the connection settings route four triggers (message, app
command, added-to-space, removed-from-space), and a live click was answered with
*"Switchboard is unable to process your request"* while nothing at all reached
the subscription ([#28](https://github.com/go-steer/switchboard/issues/28)).
Cards here are therefore output only: the welcome names the progress values in
its text and you type the one you want. An `openLink` button should be fine,
since it sends no event to the app, but that is untested here. Updating a card
means patching the whole hosting message, and a patch is idempotent so a
redelivery is harmless.

The missing clicks are the **add-on** dialect's limit, not Pub/Sub's — a legacy
Chat-API app does receive them over the same transport, which is known from
operating Chat rather than tested here as #28's half was. Switchboard still
renders no button there: add-ons are where Google is taking Chat apps, the
conversion is one-way, and the decoder normalizes both dialects away before
anything downstream could tell them apart, so a legacy-only button is not
expressible anyway. Clickable controls need the HTTP interaction endpoint tracked
in [#29](https://github.com/go-steer/switchboard/issues/29).

#### App commands

Chat identifies an app command by the **numeric ID** you assign it in the API
console, not by its name, and add-ons never report an invoked function name back.
Map the IDs onto gateway verbs with `--googlechat-commands`:

```sh
--googlechat-commands "1=progress,2=help"
```

With no mapping, switchboard falls back to reading the verb from the command's
argument text — so a single command named `switchboard` still works:
`/switchboard progress status` arrives as the verb `progress` with argument
`status`. Either way the acknowledgment is posted back into the invoking thread
(Chat has no ephemeral async reply).

#### Cards

`--googlechat-cards` chooses how much is rendered as `cardsV2`. Plain text is
always sent as the message's fallback, and a card Chat rejects falls back to
posting the text — a rich render never costs a reply.

| Mode | Behavior |
|------|----------|
| `rich` (default) | everything `status` does, and lays a structured agent reply out as a card too (a section per heading, dividers for rules, code fences kept verbatim); an unstructured reply stays as text, and a section too long for one widget spills across several rather than being cut |
| `status` | the gateway's own messages — progress, tool activity, error notices, command acks, the welcome — render as small icon cards; the welcome names the accepted `progress` values in its text. Model output stays as text |
| `off` | text only |

Regardless of mode, replies are translated into Chat's text dialect
(`**bold**` → `*bold*`, `[label](url)` → `<url|label>`, headings → bold), so
markdown from the agent does not arrive with its delimiters showing.

Full setup, card-preview and event-replay testing, and a demo script:
[docs/googlechat-setup.md](docs/googlechat-setup.md).

### Config file

Every setting below is a flag, and every flag can also be written into a JSON
file:

```sh
switchboard serve -c /etc/switchboard/config.json
# --config is the same setting; $SWITCHBOARD_CONFIG names the path too.
```

**Precedence, narrowest first: an explicit flag, then `$SWITCHBOARD_*`, then the
file, then the built-in default.** "Explicit" means the flag was on the command
line — passing a flag its default value still beats the file, because you said
it.

**One exception, and it is the point of the file: a `channels` block beats
everything, including a flag you passed.** The flag says "everywhere" and the
block says "here", and the narrower scope is the one that was meant. So
`--approvers ana@example.com` with a block naming `ben@example.com` in
`C0SRE0000` gives Ana every channel except that one, and Ben that one.

Two things a flag cannot do are the reason the file exists. It can hold
structure, so the lists and maps that flags flatten into one string are written
as lists and maps. And it can **scope a setting to a channel**:

```json
{
  "daemon_url": "http://127.0.0.1:7777",
  "platform": "slack",
  "slack_bot_token_env": "TEAM_SLACK_BOT_TOKEN",
  "ingress_allow": ["C0123ABCD", "C0456EFGH:1723742401.001900"],
  "googlechat_commands": {"1": "progress", "2": "help"},

  "defaults": {
    "progress_mode": "indicator",
    "approvals": false
  },
  "channels": {
    "C0SRE0000": {
      "name": "#sre",
      "approvals": true,
      "approvers": ["ana@example.com", "ben@example.com"],
      "progress_mode": "status"
    },
    "C0SCRATCH": {
      "name": "#scratch",
      "approvals": false,
      "show_usage": true
    }
  }
}
```

`channels` is keyed by the **channel ID** the platform reports — a Slack
`C0123ABCD`, a Chat `spaces/AAAA` — not by name: resolving a name costs an API
call and a scope switchboard does not otherwise need. A key that is not that
shape is a startup error, because `"#sre"` is the obvious thing to write and
would match nothing while looking like it had narrowed the channel. `name` is a
comment for whoever reads the file next, since JSON has none; nothing reads it.
A block
inherits every setting it does not mention from `defaults`, and `defaults`
inherits from the flags. Four settings are scopable: `approvals`, `approvers`,
`progress_mode`, `show_usage`. The two render modes (`slack_rich_blocks`,
`googlechat_cards`) are process-wide — the adapter reads them when it is built,
before there is a channel to ask about.

Four deliberate refusals, all of the same kind: a config that looks applied and
is not.

- **A `channels` key that is not a channel ID is refused** — see above.
- **A `--config` naming a file that is not there is a startup error**, not a
  fallback to the defaults. Starting anyway is how a narrowed approver list
  quietly becomes an open one.
- **An unknown key is refused.** A misspelled `approver` that decodes to nothing
  starts cleanly, announces settings that look right, and does nothing.
- **A credential in the file is refused.** Tokens are named, never held —
  `"slack_bot_token_env": "TEAM_SLACK_BOT_TOKEN"`, with the value in the
  environment. A key that reads like a credential, or a value that looks like a
  live one (`xoxb-…`, a PEM header), stops the run. A config file is the artifact
  designed to be checked in, which is exactly why this is enforced rather than
  documented.

There is **no auto-discovery**: switchboard reads a file only when told to.
core-agent picks up `.agents/config.json` from the working directory, which is a
convenience for a CLI; a long-lived gateway changing who may approve a production
change because of a file that appeared beside it is not.

### Long-turn feedback

While an agent turn runs, switchboard can show liveness. `--progress-mode` sets
the process default:

| Mode | Behavior |
|------|----------|
| `indicator` (default) | posts a "⏳ Working…" placeholder with a running clock, deleted when the reply lands |
| `status` | keeps one message per turn, edited in place with the clock and the running tool |
| `stream` | posts a notice per tool frame — tool, argument, result — plus each completed turn |
| `off` | silent until the reply is ready |

In `indicator` and `status` the placeholder ticks: every 15 seconds it is
re-rendered with how long the turn has been running, and in `status` with the
tool it is on.

```
⏳ Working… 45s
⏳ Working… 2m30s · running `bash` (step 7)
```

The clock is what makes a long turn readable. A turn that runs for four minutes
without calling a tool has nothing else to say for itself, and a static
"⏳ Working…" looks exactly like a turn that died. The clock counts from when
the message was handed to the daemon, not from when the placeholder landed, and
stops on the daemon's own turn boundary — `turn-complete`, or the `status-update`
reporting the session idle, which is the one the daemon emits however the turn
ended — so a turn that ends without an answer freezes rather than ticking
forever. Fifteen seconds is deliberately coarse: each tick is an API edit. A
failed edit backs off and is never allowed to fail the turn.

A model that narrates before calling a tool ("let me check…") sends that text as
a message, which used to retire the placeholder mid-turn. It now **re-anchors**:
the placeholder moves below the narration and keeps the same clock, because
`turn-complete` has not arrived yet and that is what tells the answer apart from
anything said on the way to it. This needs a daemon that advertises
`turn-complete`, and a turn that has not outlived its SSE connection — a
boundary lost in a stream outage cannot be told from one that never came, so
either way the older behaviour applies and the first text ends the turn. Once a
turn has spoken, its boundary deletes the placeholder rather than freezing it:
the freeze exists so a silent turn leaves some trace, and a turn with text above
it already has one.

A second question asked before the first has answered used to stop the clock the
same way, and for the same reason: one placeholder per conversation, so the
first answer retired the message the second turn was counting on. Switchboard
now watches the daemon's `inbox` events — `queued` when a message lands,
`dequeued` when a turn picks it up — and an answer with something still queued
behind it re-anchors the placeholder rather than retiring it, exactly as
narration does. Picking the queued message up restarts the clock on the
placeholder the previous turn left behind.

This needs a daemon that advertises `inbox`; without it the older behaviour
applies and the second turn runs without a clock. The events carry no `seq`, so
one lost in a stream outage cannot be replayed — a `dequeued` that never arrives
would leave switchboard believing a message is queued forever. The daemon
reporting the session idle is what resynchronises it: an idle session has
drained its inbox by definition.

What is still not fixed is attributing an answer to a *particular* one of
several turns. Switchboard can tell that another turn is owed but not which turn
any given answer belongs to, because the daemon's `turn-complete` names a
prompt id minted at turn start rather than the one it returned from `inject`
(go-steer/core-agent#943). In a thread running two overlapping turns the usage
footers can therefore land on the wrong answer (#42).

#### What `stream` shows

`stream` is the mode that exists to show detail, so it shows the tool, one
argument, and the verdict once the call finishes. One notice per *frame*, not
per call: a frame carrying three concurrent shells is three lines under one
header, and the results tick those lines off by editing the notice in place
rather than posting again.

```
🔧 Running `bash` — kubectl get pods -A
✅ Ran `bash` — kubectl get pods -A

❌ Ran 3 tools (1 failed)
• ✅ `bash` — kubectl get pods -A
• ❌ `bash` (exit 2) — kubectl get ns --context nope
• ✅ `bash` — sleep 30
```

Calls that a reader could not tell apart — same tool, same argument, same
verdict — collapse to `` `bash` ×3 `` rather than repeating the word.

**The notices are permanent, by decision.** `indicator` and `status` clean up
after themselves; `stream` accumulates, and the trail it leaves is the reason to
choose it over the other two. Retiring the notices when the answer lands would
make `stream` a noisier `indicator`. A thread that wants no residue should use
`indicator` or `off`.

**Arguments are a disclosure decision.** Tool arguments are untrusted,
unbounded, and exactly where a secret turns up: a shell command line with a
token in it, a private path, the contents of a file being written. Only `stream`
shows them, and only like this:

- one argument per call — never the whole object, so a token in a second field
  is not disclosed;
- scalars only, so nested objects and arrays are skipped rather than serialised;
- flattened to one line and clamped to 120 bytes plus an ellipsis, after
  redaction as well as before — `<redacted>` is longer than some of what it
  replaces, so a bound applied only beforehand is not a bound. The cut that
  comes *first* never splits a word, because redaction cannot recognise a
  credential it has been handed half of;
- passed through a redaction pass for the credential shapes it recognises
  (`--token=…`, `--password …`, `AWS_SECRET_ACCESS_KEY=…`, `"api_key": …`,
  `Authorization: Bearer …`, a password in a URL's userinfo, bare
  `ghp_`/`xox`/`sk-`/`AKIA`/`ya29.`/`AIza` tokens, JWTs, PEM headers).

That last step is a net, not a guarantee — no pattern set recognises every
secret, and the steps above are what keeps the blast radius of a miss to one
clamped field. Tool *output* is never shown in any mode: a failure renders as
`exit 2` and nothing more. A deployment that cannot accept even a clamped
argument in the channel should run `status`, which names the tools it runs and
shows no arguments, or `indicator`, which shows neither.

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

### Per-turn usage

`--show-usage` appends what the turn cost to each answer — model, tokens in and
out, dollars, wall-clock:

```
gemini-3.7-flash · 5,000 in / 1 out · $0.0038 · 3.1s
```

It is **off by default**: a turn's cost is spend data, and a shared channel —
particularly one with a customer in it — is the wrong place to disclose it
without being asked.

The footer rides the rich render on each platform: a Block Kit `context` block
under `--slack-rich-blocks`, a card footer under `--googlechat-cards rich`.
Outside those it is suppressed rather than appended as a line of text, which
would have to survive the per-message chunker to arrive intact. Switchboard
warns at startup if `--show-usage` is set with no rich render to attach it to.

On Google Chat that means the footer appears only on answers that earn a card.
A plain paragraph with no headings, list, or code goes out as text, and carries
no footer even in `--googlechat-cards rich`. Slack has no such gap: every answer
is rendered as blocks under `--slack-rich-blocks`.

Only the finished turn is reported, never a session running total: the footer
lands on a message that will never be edited again, so a number that changes
every turn would be wrong there within minutes. The turn is the *conversational*
one — a question that drives eight tool calls reports what all nine model calls
cost, not just the last.

### When a turn fails

A turn that never answers has to say so, or a thread just goes quiet with a
placeholder running on nothing. Three cases, three notices:

| Failure | When it is detected | What the thread is told |
|---|---|---|
| Session creation or inject failed | immediately — the turn never started | it didn't go through, and whether a retry is worth it |
| The daemon reported `turn-error` | as the event arrives | the daemon's own classification, message, and hint |
| Contact with the daemon was lost mid-turn | after ~90s with nothing on the stream | contact was lost; a finished answer will still arrive |

The daemon's `turn-error` carries a `retryable` flag and, when there is an
obvious next step, a hint — which IAM role the runtime service account is
missing, which model name to check. Both are passed through to the thread,
along with the kind and provider status code, because "something went wrong" is
barely an improvement on silence. Note that this puts upstream provider error
text into what may be a shared channel, so it is flattened to one line and
length-capped on the way in.

A turn can answer *and then* fail: the cost ceiling is enforced at the turn
boundary, so its `turn-error` arrives after the text the turn produced. The
notice is therefore a separate thing from the turn — an answered turn does not
silence it — while still being said at most once per turn, since a `turn-error`
and a lost stream can both be true of the same one.

Two kinds are called out separately. A tripped **cost ceiling** or **watchdog**
is a guardrail: the agent refuses further turns until an operator resets it, so
the notice says that instead of suggesting a retry that cannot work. Only the
trip itself is classified as one — every message sent afterwards is refused
before it runs, and comes back as kind `unknown` — so the refusal is also
recognised by the sentence the daemon puts in all three guardrail messages. If
that ever stops matching, the reader gets the generic terminal notice, which is
what they would have got anyway.

The lost-contact case is the one the daemon cannot report, because the daemon is
what went away. The relay keeps reconnecting regardless — it never gives up, and
resumes from the last event seen — so the notice is careful to say that an
answer produced during the outage will still be delivered. The 90-second grace
is there so a rolling restart resolves without interrupting anyone.

It is measured from the last event of *any* kind, not from the last answer. A
turn spent thinking produces nothing for minutes at a time, and a proxy or an
idle timeout can cut the SSE stream repeatedly while the daemon is perfectly
healthy; since the daemon opens every stream with a capabilities frame, a
connection that keeps being re-established keeps proving itself, and only a
genuine outage accumulates.

### Approving what the agent asks for

An agent that hits its permission gate stops and waits for a human. Without
`--approvals` that human is whoever is watching the daemon's own console, and
from the thread's side the turn simply stops producing anything. With it, the
question lands in the thread instead:

```
**Permission needed** — `bash`

    rm -rf /tmp/build

_asked by the `builder` subagent_

[ Deny ]  [ Allow once ]  [ Allow for this session ]  [ Allow every rm this session ]  …
```

Pressing an answer releases the blocked call, and the daemon records the press
against the person who made it — resolved from the platform's own callback, so
an approval cannot be attributed to anyone but the presser.

The question is then edited in place to say how it ended, and the buttons come
down with it:

```
**Permission needed** — `bash`

    rm -rf /tmp/build

_asked by the `builder` subagent_

✅ **Allowed**, this once — ana@example.com
```

The question stays above the answer on purpose: a line naming an approver, with
the command it approved gone, is not something anyone can audit. The approver is
whoever the backend says it recorded, and an approval it could attribute to
nobody says so rather than reading as fine. A press against a question that is
no longer pending — it timed out, or it was answered at the daemon's console —
marks it settled without claiming a decision, since switchboard has no way to
know which one was made. Where the record cannot be written onto the question at
all — the process was restarted and no longer knows what it asked, the platform
did not say which message was pressed, or the edit failed — it is posted beside
it instead. A press always ends in something to read.

**It is off by default, and turning it on is a real grant.** By default anyone
who can post in the conversation can answer its prompts, and some of those
answers outlive the request: `allow-always` persists across restarts and applies
to the whole agent backend, not the thread it was pressed in. It is ignored under
`--outbound-only`, which warns — there is no turn to unblock.

Because it is a grant, it is announced on every start:

```
2026-08-31T09:00:00.000Z switchboard: approvals: permission prompts go to the conversation, and anyone who can post there can answer them
```

#### Who may answer

`--approvers` decides that, and it defaults to `channel` — anyone who can post
in the conversation, which is the line the banner above is reporting.

That default is a posture rather than an oversight. Channel membership is access
control the platform already enforces: a press only ever arrives over an
authenticated connection, from somebody the platform rendered the buttons to, so
a curated private channel is a perfectly good approver set and switchboard needs
no membership call to rely on it. What it cannot see is how wide the room is — a
public channel is the whole workspace, and a Slack Connect channel reaches
outside the org entirely.

Name the people instead when the room is wider than the grant deserves:

```sh
--approvers "ana@example.com,ben@example.com"   # or $SWITCHBOARD_APPROVERS
```

The identities are the ones switchboard asserts — emails by default, platform
IDs under `--caller-id id` — matched without regard to case. A press from anyone
else is refused before it reaches the agent, logged, and answered in the thread;
the buttons stay up, because somebody else in the room may be allowed to answer:

```
⛔ **Not an approver** — that answer wasn't sent. Someone on this gateway's approver list has to answer it.
```

Startup then reports the count rather than the names, because a log line is the
wrong place to publish who can approve production changes:

```
2026-08-31T09:00:00.000Z switchboard: approvals: permission prompts go to the conversation, and 2 named approver(s) may answer them
```

A config file can name approvers **per channel**, which is what the setting
wanted from the start — the room the grant is wide in is rarely the room it is
narrow in:

```json
"channels": {
  "C0SRE0000": {"approvals": true, "approvers": ["ana@example.com"]},
  "C0SCRATCH": {"approvals": false}
}
```

A channel's list **replaces** the wider one rather than adding to it: the block
answers "who may approve here", and the additive reading has no way to say
*fewer* than the default at all. The corollary is that a channel can widen as
well as narrow — `"approvers": ["channel"]` puts a room back to anyone who can
post in it — so a narrowed default is not a floor. Startup says so, counted
rather than named:

```
2026-08-31T09:00:00.000Z switchboard: approvals: permission prompts go to the conversation, and 2 named approver(s) may answer them
2026-08-31T09:00:00.000Z switchboard: approvals: 1 configured channel(s) let anyone answer, 3 name their own approvers
```

A list is checked at startup against the identity this run will actually assert,
and a run whose list cannot match is refused rather than started: emails under
`--caller-id id`, a display name, a semicolon where a comma belongs, or
`SWITCHBOARD_APPROVERS` set to nothing at all. Each of those otherwise starts
cleanly, announces its approvers, and then refuses every one of them — a total
approval outage whose only symptom is the notice above.

**This is a Slack control today.** The gate is on a press, and no press reaches
this gateway from Chat: the add-on framework routes no click trigger, so Chat
posts the question as prose with the answers listed and a Chat-only deployment
has nothing for `--approvers` to narrow yet (see *Workspace add-on mode* above,
and [#29](https://github.com/go-steer/switchboard/issues/29) for the HTTP
interaction endpoint that changes it). The gate is in the router rather than the
adapter, so it covers Chat the moment presses arrive.

This is a gateway-side gate on a surface switchboard invented, not a
re-implementation of the backend's authorization: core-agent still decides what
the caller may do once the answer reaches it. There is no `--approvers` value
meaning "nobody" — leaving `--approvals` off already does that.

Six answers exist, and switchboard offers the ones that mean what they say for
the prompt in front of you:

| Answer | Scope |
|---|---|
| `deny` | this call |
| `allow-once` | this call |
| `allow-session` | the rest of the session |
| `allow-session-verb` | every bash command with the same leading word, this session |
| `allow-session-tool` | every call to this tool, this session |
| `allow-always` | saved, across restarts |

Some are withheld where the gate would grant less — or more — than the label
claims: everything wider than `allow-once` on a write to the agent's own
permission config, which records any non-deny as a one-off and saves nothing;
the two session grants on a path prompt, which that gate never reads back.
`allow-session-verb` appears only when there is a verb to scope it to. See the
[design notes](docs/DESIGN.md#21-optional-routes-and-knowing-before-you-ask)
for why each one goes.

On Slack the answers are buttons, and the ones that outlive the request ask
again before applying. The app needs **Interactivity & Shortcuts** enabled for
Slack to deliver a press at all — it is off by default and easy to miss, so
[slack-setup.md](docs/slack-setup.md#create-the-app) makes it a step. Google
Chat has no interactive surface yet ([#29]), so the question arrives as text
with the answers listed — enough to see that the agent is blocked and on what.
Either way the choices are in the message body too, so nothing depends on the
buttons rendering.

A press that cannot be delivered says so in the thread rather than leaving a
flashed button and a still-blocked agent: if the answer does not reach the
daemon, or the thread has moved on to a different session since the question was
posted, the reason is posted as a notice.

Only sessions whose agent actually has a permission broker are watched, which
switchboard reads off the capabilities frame the daemon already sends. Nothing
is relayed for a session that would answer `501`.

[#29]: https://github.com/go-steer/switchboard/issues/29

### Outbound ingress

Everything above starts with someone in a thread. `--ingress-addr` opens the
other direction: an authenticated HTTP surface that lets another service post a
message — and edit it afterwards — with no inbound event to reply to. A
scheduled digest, a monitoring escalation, an approval prompt raised at 3am.
The scheduling stays with the caller; switchboard is still only the transport.

```sh
SWITCHBOARD_INGRESS_TOKEN=... switchboard serve \
  --ingress-addr :8080 \
  --ingress-allow C0123ABCD,C0456EFGH
```

| Flag | Meaning |
|------|---------|
| `--ingress-addr` | listener address (`host:port`); empty (default) = disabled |
| `--ingress-token-env` | env var holding the caller's bearer token (default `SWITCHBOARD_INGRESS_TOKEN`) — deliberately **not** the daemon token: different direction, different trust |
| `--ingress-allow` | comma-separated conversations callers may post into; empty = anywhere the bot can reach (`serve` warns) |

**Post**, and keep the ref it hands back:

```sh
curl -sS localhost:8080/v1/messages \
  -H "Authorization: Bearer $SWITCHBOARD_INGRESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: digest-2026-08-15' \
  -d '{"conversation":"C0123ABCD","text":"*Cluster digest* — 5 subjects, 2 escalations"}'
# 200 {"conversation":"C0123ABCD","id":"1723742401.001900"}
```

**Edit** it as later results land — the half that matters when the work is slow:
post once, then refine, rather than a thread of partial posts.

```sh
curl -sS -X PATCH localhost:8080/v1/messages \
  -H "Authorization: Bearer $SWITCHBOARD_INGRESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"conversation":"C0123ABCD","id":"1723742401.001900","text":"*Cluster digest* — done"}'
# 204
```

**Append** a line instead, when the message is a growing timeline and the caller
would rather not keep resending the whole body:

```sh
curl -sS -X PATCH localhost:8080/v1/messages \
  -H "Authorization: Bearer $SWITCHBOARD_INGRESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"conversation":"C0123ABCD","id":"1723742401.001900","append":"• 12:07 kubelet restarted cleanly"}'
# 204
```

`text` and `append` are alternatives; a PATCH must carry exactly one. Two things
to know about `append`, both consequences of neither platform having an
"append" call of its own — an edit replaces the whole message, so switchboard
has to know what is already there:

- **It only works on messages this process posted.** The current text of the
  last 1024 posted messages is held in memory; append to anything else — a
  message from before a restart, or one posted by another replica — answers
  `409`, and the caller should send the full `text`. Nothing is ever read back
  from the platform, so an append cannot clobber an edit made in the chat UI.
- **It rolls over rather than truncating.** When the combined text would pass
  the platform's single-message limit, switchboard posts the new line as a reply
  in the same thread and answers `200` with the continuation's ref. Keep appending to
  *that* ref from then on; the timeline continues under the original message
  instead of silently losing its head.

**Bind an agent session to the thread**, when the caller is an agent that wants
to be answered. A post with `session` records the thread it lands in as that
session's conversation, so a human replying there reaches the work already in
flight instead of opening a fresh session that knows nothing about it:

```sh
curl -sS localhost:8080/v1/messages \
  -H "Authorization: Bearer $SWITCHBOARD_INGRESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"conversation":"C0123ABCD","text":"Rollout is wedged. Roll back?","session":"core-agent/incident-7"}'
# 200 {"conversation":"C0123ABCD:1723742401.001900","id":"1723742401.001900"}
```

The reply in that thread is injected into `core-agent/incident-7` carrying the
human's identity, and the session's answers come back to the thread — the same
loop a `@switchboard` mention gets, with the session opened by someone else.
Points worth knowing before you rely on it:

- **`session` is `<app>/<id>`,** the same pair the agent backend knows the
  session by. It is only valid on POST: a thread is decided when a message is
  posted, and a PATCH addressed to a bare channel does not name one.
- **The thread comes back in the answer.** Bind against a bare channel and the
  binding is recorded for the thread the post created, which is the
  `conversation` in the response. Reuse that key for later posts about the same
  incident; re-posting the same `session` to the same thread is accepted and
  just moves the resume point forward. Addressing the *bare channel* a second
  time with the same `session` is a `409` instead — that session already has a
  thread, and a second one relaying it would answer both — and the refusal
  names the thread to post to.
- **Only what happens next is relayed.** Switchboard reads how far the session
  has got before it posts, and reads it again when someone replies in the
  thread, starting from the later of the two — neither the hour before the
  alert nor the hours between the alert and the reply lands in the channel.
- **The session has to exist, and both ends have to be free.** A session the
  agent backend does not have is a `404`; a thread that already has a session,
  or a session already bound to another thread, is a `409`. All three are
  refused *before* anything is posted, so a rejected bind leaves no message
  behind to be confused by.
- **A bind costs a round trip.** Finding where the session has got to means
  opening its event stream and reading until it goes quiet, which happens
  before the message is sent: a settled session costs a few hundred
  milliseconds, and one mid-turn can cost up to five seconds. A post that binds
  nothing is unaffected.
- **Any token holder can bind any session.** The token says a trusted caller is
  on the line, not which one, and nothing checks that the holder is entitled to
  the session id it names — so the `404`/`200` split above also tells a holder
  which session ids exist. Authorization for what a session may *do* lives in
  the agent backend, per caller; switchboard has none of its own. Treat the
  ingress token accordingly, and keep the endpoint on a network only the agent
  backend can reach.
- **Bindings do not survive a restart.** They are held in memory. After a
  restart the thread is just a thread: the next human message there starts a
  new session, and switchboard says so in the thread rather than dropping the
  message silently. Recovery is the agent posting again with `session`, to the
  thread key — a caller that re-posts on every update does that anyway, and it
  finds the thread unbound and free to take.
- **A session that disappears is announced, not retried.** If the agent backend
  forgets a bound session — restarted, expired — the thread is told, in the
  thread, and the binding is dropped so the next message plainly starts fresh.
  That holds whether the loss surfaces on a human's message or on a quiet
  thread waiting for an answer.
- **It needs somewhere for the reply to go.** On an `--outbound-only` run there
  is no inbound path, so `session` is a `400`.

Details worth knowing:

- **No `platform` field.** An instance bridges the one platform it was started
  with, so the target is implied. Both platforms are supported.
- **`conversation` is the platform's conversation key.** A bare channel or space
  posts a new top-level message; a full key posts into an existing thread.

  | Platform | Top-level | In a thread |
  |----------|-----------|-------------|
  | Slack | `C0123ABCD` | `C0123ABCD:1723742401.001900` |
  | Google Chat | `spaces/AAA` | `spaces/AAA:spaces/AAA/threads/BBB` |

  Follow up using the `conversation` a POST **answers with**, not the one you
  sent: it names the thread the message actually landed in. Post to `C0123ABCD`
  and the answer is `C0123ABCD:1723742401.001900`; post to `spaces/AAA` and it is
  `spaces/AAA:spaces/AAA/threads/BBB`. On Slack you could have built that
  yourself — a top-level message's `id` is the `thread_ts` of the thread it
  roots — but on Chat the thread is assigned by the platform and there is
  nothing to build it from.
- **The bot has to be in the conversation.** A Slack bot must be invited to the
  channel; a Chat app must be a member of the space. Neither platform lets an
  app post its way in, and the refusal reads like a credentials problem —
  `403` from Slack, and from Chat a `403` or a `404`, depending on whether it
  will admit the space exists. Check membership before re-issuing credentials.
- **A post renders like an answer.** It goes through the same egress the router
  replies with, so a digest looks like a reply rather than announcing where it
  came from — Block Kit under `--slack-rich-blocks`, and under
  `--googlechat-cards rich` a card if the markdown has structure (headings,
  rules) or plain text if it does not. There is no `card` field.
- **`Idempotency-Key` is optional.** A POST carrying one that has been seen
  posts nothing and returns the original ref, so a scheduler retrying a request
  it never saw the answer to does not double-post. The map is in-memory and
  bounded (last 1024 keys), and a *failed* post is never cached.
- **Errors.** A `4xx` means *fix the request*; only `502`/`504` are worth
  retrying unchanged.

  | Status | Meaning |
  |--------|---------|
  | `400` | malformed body, `text`/`append` not exactly one on PATCH, a malformed `session`, `session` on a PATCH, or `session` on an `--outbound-only` run |
  | `401` | missing or wrong bearer token |
  | `403` | conversation not allowlisted, or the platform refused (bot not in the channel, channel archived) |
  | `404` | no such conversation or message, or a `session` the agent backend does not have |
  | `405` | method other than POST/PATCH on `/v1/messages` |
  | `409` | `Idempotency-Key` reused with a different body, `append` to a message this process has no remembered text for, or a `session` bind where either end is already taken |
  | `413` | body over 1 MiB |
  | `415` | `Content-Type` is not `application/json` |
  | `501` | the platform cannot do this (editing, or `append` where the text limit is unknown) |
  | `502` | the platform rejected the message for some other reason, or the agent backend would not confirm a `session` |
  | `504` | timed out waiting on an identical in-flight request with the same `Idempotency-Key` |

  Platform errors are logged in full and summarized to the caller; the
  platform's own wording never reaches the response body.

The listener is off unless `--ingress-addr` is set, and it is a *separate* port
from `--metrics-addr`: metrics are unauthenticated and usually reachable by the
whole scrape network, this is not.

#### Outbound-only deployments

A digest pipeline that posts and never listens does not need the credentials
receiving takes, and `--outbound-only` says so ([#23]). The process then runs
egress-only: it posts through the ingress and answers nobody, because no chat
user can reach it — the only way in is the ingress, and the ingress does not
carry replies.

| Platform | What is then never created | And so never needed |
|----------|----------------------------|---------------------|
| Slack | the Socket Mode WebSocket | an app-level token, or any event subscription |
| Google Chat | the Pub/Sub client | a topic, a subscription, or `roles/pubsub.subscriber` |

It is a flag rather than an inference from a missing credential, and that is the
whole design: the recommended shape is *bidirectional* — a bridge that also
serves the ingress — and if an emptied or badly rotated app-token secret were
read as "this one only posts", that deployment would come back up posting,
passing `/healthz` and answering nobody, with nothing in the logs to page on. So
a run that cannot receive and did not say so fails at startup instead. In the
other direction the flag wins: an inbound credential still lying around is
ignored, with a warning naming it.

The daemon is not involved either. An outbound-only run holds no session and
starts no turn, so `--token-env` is not read at all and no core-agent bearer
token has to be provisioned for it.

Egress credentials are untouched: the Slack bot token, or ADC and the Chat bot
scope, are still required and are still what a post authenticates with.

```sh
export SWITCHBOARD_INGRESS_TOKEN=…
export SWITCHBOARD_SLACK_BOT_TOKEN=xoxb-…   # posting only; no xapp- token
switchboard serve --outbound-only --ingress-addr :8080 --ingress-allow C0123ABCD
# 2026-08-19T16:00:00.123Z switchboard: outbound-only: posting to slack, receiving nothing
```

The banner says so on every start, because a gateway nobody can talk to and a
broken one look identical otherwise. Three configurations are refused:

- **Neither direction.** `--outbound-only` with no `--ingress-addr` is a process
  that could neither receive nor be asked to post, so it exits rather than
  idling while every probe reports it healthy.
- **Unable to receive, without saying so.** No app token on Slack, or no
  `--google-subscription` on Chat, without `--outbound-only`.
- **Half an inbound pair on Chat.** `--google-project` without
  `--google-subscription`, or the reverse, is a typo, not a deployment shape.

[#23]: https://github.com/go-steer/switchboard/issues/23

### Health & metrics

`--metrics-addr=host:port` (empty by default, disabled) starts a small HTTP
server with two endpoints:

- `/healthz` — liveness probe, always `200 ok`; no dependency on the scrape path.
- `/metrics` — Prometheus exposition of switchboard's counters and gauges.

```
switchboard serve --metrics-addr :9090   # or SWITCHBOARD_METRICS_ADDR=:9090
```

The exported series (all prefixed `switchboard_`):

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `switchboard_messages_total` | counter | `outcome` | inbound chat turns handled |
| `switchboard_commands_total` | counter | — | chat control commands handled |
| `switchboard_daemon_requests_total` | counter | `op`, `outcome` | core-agent requests (`create`/`inject`) |
| `switchboard_daemon_request_duration_seconds` | histogram | `op` | daemon request latency |
| `switchboard_replies_sent_total` | counter | `outcome` | outbound sends to the platform |
| `switchboard_agent_turns_relayed_total` | counter | — | completed agent turns relayed to chat |
| `switchboard_agent_turns_failed_total` | counter | `kind` | turns that failed instead of answering |
| `switchboard_stream_reconnects_total` | counter | — | SSE relay reconnects |
| `switchboard_active_sessions` | gauge | — | conversation→session entries held |
| `switchboard_ingress_requests_total` | counter | `op`, `outcome` | outbound-ingress requests (`post`/`patch`/`other`) |

`kind` on the failure counter is the daemon's own classification
(`auth_error`, `rate_limited`, `watchdog`, …), plus `stream_lost` for a turn
abandoned because contact was lost. Anything switchboard does not recognise
folds into `unknown`, so a future daemon inventing a kind cannot grow the label
set without a change here.

When the metrics server is disabled the collectors still accumulate in-process;
they are just not exposed. The Kubernetes manifests set `--metrics-addr=:9090`
and wire `/healthz` to the liveness + readiness probes.

### Logs

Everything goes to stderr, one write per event, each stamped with the time it
was written in UTC:

```
2026-08-19T16:00:00.123Z switchboard: bridging slack -> http://127.0.0.1:7777
2026-08-19T16:04:19.881Z switchboard: relay C0123:1723742400.0001: stream ended (EOF); resuming from seq 41 in 2s
```

`--log-format json` (or `SWITCHBOARD_LOG_FORMAT=json`) renders the same events
one JSON object per line instead, for a collector:

```json
{"time":"2026-08-19T16:04:19.881Z","message":"relay C0123:1723742400.0001: stream ended (EOF); resuming from seq 41 in 2s"}
```

The entry text is under `message` rather than `log/slog`'s `msg`, which is
where Cloud Logging looks for it; `time` is both slog's default key and the one
Cloud Logging reads a timestamp from, so it is unchanged.

`json` is also the format to pick when a message might contain a newline — a
Chat payload under `--googlechat-log-events`, say. The JSON encoder escapes it
(and replaces invalid UTF-8 with U+FFFD), so the record stays on one line;
`text` passes the message through verbatim, so such an event spans as many
lines as it contains.

A startup that fails goes through the logger too — a crash loop being when
those lines are read. The build identity is logged before the config is
checked, for the same reason: an operator whose flags are rejected still learns
which build rejected them. Some output is still not the logger's, so a strict
collector will see lines it cannot parse in a `json` run:

- a flag that will not parse, and a `--log-format` that cannot be rendered —
  both happen before there is a logger to say them through;
- `--help`, the flag defaults the `flag` package prints, and the unknown-
  subcommand message;
- `version` and `--version`, which print build identity to **stdout**;
- panics and connection errors from the `--metrics-addr` and `--ingress-addr`
  listeners, which `net/http` writes through its own default logger
  ([#49](https://github.com/go-steer/switchboard/issues/49) tracks routing
  those through this one). A listener *failing to bind* is logged normally.

Two things a log line does not carry yet, both tracked in
[#49](https://github.com/go-steer/switchboard/issues/49). There is no
**severity**: no call site distinguishes a connect notice from a send failure,
so rather than label every record `INFO` — which would be a lie about the
failures — the JSON carries no severity field at all and Cloud Logging assigns
`DEFAULT`. And the messages are **not structured**: the component and the
conversation key are interpolated into the text, so a collector cannot filter
on them. Both want a pass over all the call sites, which is a larger change
than putting a time on the front.

A deployment does get an ingestion timestamp from Cloud Run or a k8s collector
regardless. That is when the line was *collected*, though, and it is absent
entirely from a local run, a redirect to a file, or a `kubectl logs` dump taken
without `--timestamps` — which is why the line carries its own.

### Container

Images are published to **GHCR** and are multi-arch
(`linux/amd64`, `linux/arm64`):

```sh
docker pull ghcr.io/go-steer/switchboard:latest        # newest release
docker pull ghcr.io/go-steer/switchboard:main          # tip of main (every merged PR)
docker run --rm -e SWITCHBOARD_DAEMON_TOKEN ghcr.io/go-steer/switchboard:0.3.0 \
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
cosign verify ghcr.io/go-steer/switchboard:0.3.0 \
  --certificate-identity-regexp '^https://github.com/go-steer/switchboard' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

### Kubernetes

Kustomize manifests live in [`deploy/`](deploy/): a platform-neutral `base` plus
`overlays/slack` and `overlays/googlechat`. switchboard runs alongside core-agent
in the `agent-triage` namespace, reads nothing from the Kubernetes API, and
exposes only `:9090` for `/healthz` + `/metrics` (both chat platforms are
outbound; the outbound ingress is off unless you enable it — see
[`deploy/README.md`](deploy/README.md)). After creating the prerequisite Secrets:

```sh
kubectl apply -k deploy/overlays/slack        # or overlays/googlechat
```

See [`deploy/README.md`](deploy/README.md) for prerequisites (namespace, Secrets,
Google Chat Workload Identity) and image pinning.

## Layout

| Path | What |
|------|------|
| `cmd/switchboard` | multicall binary — `serve` (default), `version` |
| `pkg/daemon` | thin client for the core-agent daemon contract (create / inject / wake / SSE) |
| `pkg/chat` | provider-neutral `Adapter` interface + normalized message types |
| `internal/version` | build-identity stamping |
| `deploy/` | kustomize base + Slack / Google Chat overlays |
| `docs/DESIGN.md` | switchboard design |
| `docs/daemon-setup.md` | the core-agent daemon behind it, and running both platforms at once |
| `docs/slack-setup.md` | Slack app setup, scopes, demo script |
| `docs/googlechat-setup.md` | Chat app + Pub/Sub setup, card/event testing, demo script |

## License

Apache 2.0 — see [LICENSE](LICENSE).
