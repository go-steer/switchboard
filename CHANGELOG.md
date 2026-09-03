# Changelog

All notable changes to switchboard are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Gateway config file (#71): `-c` / `--config` (or `$SWITCHBOARD_CONFIG`) reads
  every setting from JSON, with precedence **explicit flag > `$SWITCHBOARD_*` >
  file > built-in default**. Twenty-three flags had grown nine environment
  alternatives one at a time, so a Deployment set some settings under `args:`
  and others under `env:` with no rule explaining which; the file gives every
  setting one home. But the reason it exists is the two things a flag cannot do
  at all: hold structure (`ingress_allow` and `googlechat_commands` stop being
  lists and maps flattened into one string), and scope a setting to a channel.
  - **Per-channel settings.** A `channels` block keyed by the platform's channel
    ID scopes `approvals`, `approvers`, `progress_mode` and `show_usage`, over a
    `defaults` block that scopes them process-wide. #65 wanted "the SRE channel
    approves prod, a scratch channel approves nothing" and shipped global
    because a flag has nowhere to put the channel. A block outranks even a flag
    you passed — the flag says "everywhere" and the block says "here" — and a
    key that is not the shape of a channel ID on the platform being bridged is a
    startup error, since `"#sre"` is the obvious thing to write and would match
    nothing while reading as a room that had been narrowed.
  - **A channel's approver list replaces the wider one** rather than extending
    it: the block answers "who may approve here", and the additive reading
    cannot express *fewer* than the default at all. So a channel can widen as
    well as narrow — `["channel"]` puts a room back to anyone who can post in it
    — and the startup banner counts both postures rather than implying the
    default is a floor.
  - One resolver now serves every channel-scopable setting. Before this, the
    progress mode had `progressFor` and everything else was read straight off
    the router, which is the asymmetry that would have made the third scoped
    setting invent a third lookup.
  - **Credentials are refused, not documented against.** A key that reads like a
    credential, or a value that looks like a live one (`xoxb-…`, a PEM header),
    stops the run; tokens are named via `*_token_env` and held in the
    environment, as they already were for flags. A config file is the artifact
    designed to be checked in, which is why this is enforced.
  - Unknown keys are refused, a `--config` naming a file that is not there is a
    startup error, and there is no auto-discovery of a file in the working
    directory. All three are the same failure class: a config that looks applied
    and is not — and the version of it that matters is a narrowed approver list
    quietly becoming an open one.
  - Startup says which file it read and which channels it configured, and a
    `progress` command that contradicts a channel's `progress_mode` logs the
    divergence: a chat command is the one setting reachable from inside a room,
    so the account of why a channel stopped matching its own block should not be
    a message that has scrolled away.
  - **The `deploy/` manifests moved onto the file.** Each overlay carries a
    `config.json` that a `configMapGenerator` turns into the ConfigMap the base
    mounts at `/etc/switchboard/config.json`, and the container's argv is
    `--config` alone. Leaving any setting in `args:` would have been worse than
    verbose: a flag outranks the file, so an operator would edit the ConfigMap,
    roll the pod, and see nothing change. Generated rather than hand-written
    because the name then carries a content hash — switchboard reads the file
    once at startup, so `kubectl apply -k` has to roll the Deployment when the
    config changes, and a ConfigMap edited in place would not. The Google Chat
    overlay's Deployment patch is gone entirely, every arg it appended now being
    a key; the Slack patch keeps only its two `secretKeyRef` entries, which is
    the shape the split was aiming for — credentials in the pod spec, settings
    in the file. `namespace:` moved from the base to both overlays, because a
    generated resource is namespaced by the kustomization that generates it: set
    in the base, the ConfigMap lands in `default` while the Deployment does not,
    and kustomize rewrites a name reference only within one namespace, so the
    pod would mount a hashed name that exists nowhere.
- `--approvals`: relay a blocked tool call into the thread as a question with
  buttons, and send back what someone presses (#40). A permission prompt that
  used to park a turn until whoever started it noticed a console now reaches the
  people already in the conversation, and the answer releases the call. **Off by
  default, and it is a real grant to turn on**: anyone who can post in a
  conversation can answer its prompts, and some of those answers outlive the
  request that raised them. Because that is a grant rather than a rendering
  choice, it is announced on every start, like the outbound-only banner.
  Ignored under `--outbound-only`, which warns — there is no turn to unblock.
  - Subscription is gated on the capabilities frame — can this session ask? —
    rather than on a turn state saying it is asking right now. The backend
    declares an `awaiting_permission` state and emits it nowhere, so waiting for
    it would relay no prompt on any build shipping today. The cost is a second
    stream per session whose agent registered a broker; nothing is lost by
    attaching early, because a new subscriber is seeded with every prompt already
    pending. One watcher per session no matter how often the event stream
    reconnects, and it reconnects on its own without a cursor for the same
    reason.
  - A question is asked once, however often the prompt stream is cut. The
    seeding that removes the need for a cursor also redelivers everything still
    pending on *every* resubscription, and an idle stream is cut routinely, so
    each reconnect would otherwise add another copy of the same unanswered
    question with its own live buttons. The record of what has been asked is
    kept per session and the claim is given back if the post fails, since the
    next reconnect's seeding is the only retry there is. A stream that carried a
    prompt resets the reconnect backoff, as the event relay's does — without
    that, a session whose stream is cut periodically settles at the ceiling and
    the next question waits that long before anybody sees it.
  - A press names the session that asked, not just the prompt. A conversation's
    session can be replaced while a question is still on screen with live
    buttons, and answering it against whatever session the thread holds now
    would apply a decision — possibly a standing one — to something that never
    asked. Refused, and said so in the thread.
  - A press that does not reach the backend says so in the thread. It is the one
    inbound action with no reply of its own: the button flashes, the platform
    considers it delivered, and without a notice the person who pressed it
    watches an agent stay blocked with every appearance of having unblocked it.
  - An answered question is edited to record how it ended and who ended it, and
    the buttons come down with the edit. A question that stays pressable after
    it has been settled is worse than one that never rendered: it invites a
    second press that answers nothing, and it leaves a thread nobody can read
    the outcome off afterwards. The record keeps the question above it — an
    audit line reading "Allowed once by ana@example.com" with the command it
    allowed gone is not one — and it is phrased from the decision switchboard
    validated rather than from the label the platform round-tripped back, so
    nothing arriving from outside is rendered as a claim about what a person
    authorized. The approver named is the one the backend says it recorded; an
    approval it could attribute to nobody says that rather than reading as
    fine. A press that finds nothing left to answer marks the question settled
    without naming a decision, because switchboard does not know which one was
    made. Records are ranked rather than first-come: the one naming an approver
    outranks the one that names no decision and replaces it, whichever of two
    simultaneous presses returns first, and claiming the record and writing it
    are one step so the thread ends up in the order they were claimed.
  - Where the record cannot go onto the question it goes beside it. That covers
    a press naming a question this process never asked — a session adopted
    across a restart, or one the bounded record dropped — a platform that did
    not say which message was pressed, an adapter that cannot edit at all, and
    an edit that simply failed. A press is the one inbound action with no reply
    of its own, so every one of them has to end in something to read. Said on
    every such press rather than only the first, because those buttons stay
    live.
  - An edit that failed keeps its claim instead of giving it back. Handing it to
    a later press sounds generous, but the prompt is spent by then, so the only
    record that press could write is "no longer pending" — over an approval that
    was applied and named an approver. Nor is it reported as a failed press: the
    decision took effect whatever the thread ends up showing.
  - A press arriving where no session is bound at all is told so, rather than
    only logged. Nothing repopulates the session map at startup, so a restart
    leaves live buttons over a thread that cannot answer them until somebody
    posts in it again — the case with the most stale buttons on screen was the
    one saying nothing.
  - Every field interpolated into a question or its record is clamped, not just
    the detail: the tool, the asking agent, and the approver the daemon reports.
    All are unbounded on the wire, and the approver lands on an audit line, so
    it is flattened onto one line too — a name spanning a blank line would
    render as a second verdict under the real one.
  - A question is dropped from the record set the moment a decision is written
    onto it. Nothing outranks that, so nothing needs it again, and it is by far
    the largest thing the set holds.
  - An answer the daemon took and then failed to confirm is no longer reported
    as one that never arrived. `pkg/approval` grew `ErrMaybeApplied` for the two
    ways that happens — an unreadable 2xx body, and a 2xx that declines to
    acknowledge — and the thread is told to check the agent rather than to press
    again. Pressing again would find nothing pending, and the thread would
    settle on "expired" over a decision that took effect.
  - `pkg/approval` exports `Decisions`, the whole vocabulary, so a caller that
    has to be exhaustive over it can iterate the set instead of keeping a copy
    in step by hand. `Decision.Valid` reads from the same place.
  - `pkg/chat` grew the press seam: a `Reply` may carry a `Decision`, and a
    `Handler` takes the presses back. `HandlePress` is on the interface rather
    than an optional capability, so an adapter that renders buttons cannot be
    paired with a handler that has nowhere to send them — a person clicking and
    watching nothing happen is worse than the button never appearing. Answers are
    also written into the message text, so a platform that renders no buttons
    still says what the choices are.
  - Slack renders the question as Block Kit buttons and reads `block_actions`
    back. This needs the app's *Interactivity & Shortcuts* toggle, which is off
    by default and delivers no press without it, so `docs/slack-setup.md` makes
    it a numbered step rather than a footnote. The ack goes out first, before
    the payload is read as ours: Slack allows three seconds and then tells the
    person their request failed, which is exactly the wrong thing to say about
    a decision that is being applied. The press is attributed to whoever pressed
    it, from the callback's own user and never from the message being answered.
    Answers that outlive the request get a confirmation dialog, which is what
    Slack has to spend on friction. Every Block Kit limit is enforced on the way out —
    labels are clipped, unrepresentable options dropped, the element count
    capped — because exceeding one fails the whole post and takes the question
    with it. Buttons post whether or not `--slack-rich-blocks` is set: that flag
    is about how prose looks, this is about whether the question can be answered.
  - Google Chat has no interactive surface yet (#29), so it posts the question
    as prose with the answers listed.
  - A press against a conversation with no live session opens none. It can only
    ever answer a question posted into a live one, so a miss means the session
    went away in between — and a fresh session holds none of the old one's
    pending prompts. A prompt that is no longer pending is not reported as a
    failure: someone else answered first, or it timed out, and the question is
    settled either way.
- `--approvers`: who may answer one of those prompts (#65). It defaults to the
  literal `channel` — anyone who can post in the conversation, which is what
  `--approvals` already meant, so upgrading changes nothing. Naming that default
  is the point of it: the open posture becomes a value an operator can read back
  out of the process args instead of a door left open by omission. Pass a
  comma-separated list of the identities switchboard asserts — emails, or
  platform IDs under `--caller-id id`, matched without regard to case — to
  narrow it, and the startup banner reports how many were named rather than who.
  - `channel` is a defensible default and not a missing check. Presses arrive
    over a connection the platform authenticated, from somebody the platform
    rendered the buttons to, so membership is access control already enforced
    upstream — no membership call needed to rely on it. What switchboard cannot
    see is how wide the room is: a public channel is the whole workspace, and a
    Slack Connect channel reaches outside the org. That is the deployment that
    wants a list.
  - The gate runs before the prompt is located, so a press this gateway will not
    relay does not learn whether the question is still live or which session the
    thread holds. The refusal is posted rather than swallowed — a press that
    vanishes reads as one that worked — and it names no approver back, since
    reading the list out to whoever presses hardest turns a refusal into a
    directory. The buttons stay up: somebody else in the room may be allowed.
  - A list nobody could match is refused at startup instead of discovered from a
    thread. It is the failure that matters here, because it is invisible: an
    entry the gateway will never assert is just an approver who never presses, so
    the run starts, announces its approvers and then refuses every one of them.
    So the list is checked against the caller mode the same run selected — emails
    under `--caller-id "id"` and platform IDs under `email` are both refused — as
    are a display name, a semicolon or a space where a comma belongs, and a list
    that names nobody. `channel` cannot be combined with names, because the two
    ways of reading that disagree about everything that matters.
  - `SWITCHBOARD_APPROVERS` set to the empty string is refused rather than read
    as unset. Every other setting here treats empty as absent, which is right for
    a default and wrong for this one: an unset ConfigMap key renders as `""`, and
    resolving that to `channel` would widen the grant at the moment somebody was
    trying to narrow it.
  - `--approvers` with `--approvals` off warns and does nothing, and there is no
    value meaning "nobody" — leaving `--approvals` off is already that.
  - Slack only for now, and by circumstance rather than by design: the gate is on
    a press, and the add-on framework routes no click trigger to a Chat app, so
    none arrives to gate (#28, #29). It lives in the router rather than an
    adapter, so it covers Chat the day presses do.
- `pkg/approval`: a client for the agent backend's permission broker, the first
  half of turning a blocked tool call into a question someone in a chat thread
  can answer (#40). It streams pending prompts from
  `/sessions/<app>/<id>/perms/stream` and answers them at `perms/respond`, and
  it is a package of its own rather than a fifth verb on `pkg/daemon` because
  these two routes are optional — they exist only for a session whose agent
  registered a broker, and answer `501` for one that did not. Nothing posts
  buttons yet; this is the seam they will sit on.
  - Who approved is asserted in the header and never in the body. The wire
    format has a field for it, but the backend fills the audit line from the
    caller it verified and uses that field only to *check* a client's claim
    against the verdict — so sending it can never widen what gets recorded, only
    earn a `400`. `Respond` reads back what was recorded, so an approval nobody
    could be named on is distinguishable from one that was attributed, rather
    than both arriving as silence.
  - All six decisions the backend accepts are offered, narrowest first, except
    where a decision grants something other than what its label claims. A button
    that means less than it says is worse than an absent one; one that means more
    is worse than both. `allow-session-verb` goes when the gate could not extract
    a verb, since there is nothing to scope it to. Everything wider than
    `allow-once` goes on a control-plane write, because that gate records any
    non-deny answer as `allow-once` and deliberately remembers nothing, so
    "Always allow (saved)" there would report a standing grant on the file
    governing the agent's own permissions when nothing had been saved. On a
    path-scope prompt, `allow-session` and `allow-session-tool` go: the gate
    raising those reads neither on the way back in, so the same out-of-scope path
    keeps prompting — and the tool grant is not merely inert, since the file-write
    gate *does* read it, so approving an out-of-scope write that way would
    silently stop the prompting for every in-scope write by that tool. A prompt of
    an unknown kind gets the full set: withholding is a set of facts about
    specific gates, and guessing at an unfamiliar one risks leaving a blocked
    agent nothing but deny.
  - Labels name what a press actually does. `allow-always` on a path-scope prompt
    reads "Always allow this directory", because the backend widens the path to
    its enclosing directory tree and promotes a write to read-write before
    storing it — one press on a prompt reading `write ~/.ssh/authorized_keys`
    persists read and write over all of `~/.ssh`. `allow-session-tool` on a
    generic prompt does not name the tool at all, because a namespaced toolset
    reports its *namespace* there while the grant covers one underlying tool, so
    interpolating the field would offer "Allow every mcp this session" for a far
    narrower press.
  - `allow-always` is marked as outliving the request, because it persists across
    restarts and its blast radius is the whole backend rather than the thread it
    was pressed in. So is `allow-session-verb`, which scopes to a bash command's
    leading word — approving one `git push --force` also approves every
    `git reset --hard` for the session.
  - Prompt kinds are carried as opaque strings. The backend has already grown a
    fifth (`control_plane_write`), and a prompt of a kind this build has never
    heard of is still answerable — the alternative is an agent blocked forever on
    a question switchboard declined to ask.
- `daemon.Capabilities.Offers`: which optional routes the backend serves, read
  off the capabilities frame that already opens every stream (#40). Whether a
  session will answer `/perms` is therefore known before anything asks, on a
  frame switchboard was reading anyway — the alternative is spending a round trip
  to be told `501`. Distinct from `Advertises`, which is about which events
  arrive on the stream rather than which routes exist.
- Agent-initiated sessions: a POST to the outbound ingress may carry
  `"session":"<app>/<id>"`, and the thread the message lands in becomes that
  session's conversation (#38). A human replying there is injected into the work
  already in flight — carrying their own identity — and its answers come back to
  the thread, instead of the reply opening a fresh session that knows nothing
  about the incident. Switchboard subscribes to nothing on its own: the caller
  opens the session and names the thread. Only what happens *after* the bind is
  relayed, so adopting an hour-old session does not post its transcript into the
  channel — and the resume point is read again when the thread is adopted, so
  neither does the work the agent did between the alert and someone answering
  it. Three refusals, all of them before anything is posted, so a rejected
  bind leaves no unanswerable message behind: a session the agent backend does
  not have (`404`), a conversation that already has one (`409`), and a session
  already bound elsewhere (`409`, naming the thread to post to instead). A bind
  costs one round trip to the agent backend before the message goes out.
  `session` is POST-only (`400` on PATCH) and
  needs an inbound path (`400` on `--outbound-only`). Bindings are in memory,
  bounded at 1024, and lost on restart; a bound session the backend has lost is
  announced in the thread rather than swallowed — whether that surfaces on a
  human's message or on a quiet thread whose relay finds the session gone — and
  the caller's next post rebinds. Binding is a whole-instance capability: the
  ingress token says a trusted caller is on the line, not which one, so any
  holder can bind any session the backend has, and the `404`/`200` split tells
  it which ids exist. Authorization for what a session may do stays in the agent
  backend, per caller.
- `daemon.Client.HeadSeq`: a bounded probe that reports how far a session's
  stream has already got (#38), so a subscription can resume at the end of the
  backlog instead of replaying it. It ends on 300ms of quiet or 5s overall —
  neither is an error once the stream is open, while a cap that runs out before
  it opens is, because a head nobody measured is how a whole transcript ends up
  in a chat thread. A session the daemon does not have answers `404`, which is
  what makes it the existence check a bind needs.
- `switchboard_active_bindings`: how many conversations are tied to a session
  the gateway did not open (#38). It dropping to zero while those threads are
  still live is a restart, and is the shape of the one failure mode bindings
  have.
- `--outbound-only`: post without receiving, and without the credentials
  receiving takes (#23). A digest pipeline that drives the outbound ingress and
  never listens needs no Slack app-level token and no `--google-project` /
  `--google-subscription`, so `serve` opens no Socket Mode WebSocket, builds no
  Pub/Sub client, and on Chat needs no subscribe grant, topic or subscription
  provisioned. It reads no daemon token either — there is no turn to run. The
  banner says `outbound-only: posting to <platform>, receiving nothing` so the
  mode is visible on every start, and an inbound credential still set is ignored
  with a warning naming it. Three configurations are refused instead of guessed
  at: being unable to receive without having passed the flag, half of Chat's
  inbound pair, and `--outbound-only` with no `--ingress-addr`, which could do
  nothing at all while staying healthy to every probe.
- `chat.ErrNoInbound`, returned by an adapter's `Run` when it was built with no
  inbound source (#23). An egress-only adapter is a valid thing to have but not
  a thing to run, so this is a refusal rather than a block on a source that does
  not exist.
- The outbound ingress works on Google Chat (#39). `--ingress-addr` with
  `--platform googlechat` was refused at startup, so every agent-initiated use
  case — a scheduled digest, a monitoring escalation, an approval prompt raised
  at 3am — worked on Slack and was impossible on Chat. The ingress itself was
  already platform-neutral; what was missing was the guard's removal and the
  ref fix below.

### Fixed
- Docs attributed the missing Google Chat buttons to Pub/Sub, and recorded the
  legacy Chat-API dialect as *expected* to share the limit. It does not: legacy
  apps receive `CARD_CLICKED` over the same transport. The limit is the add-on
  framework's, whose connection settings route four triggers and no click —
  which is the half #28 actually measured; the legacy half is known from
  operating Chat and is marked as such wherever it is now written down, rather
  than borrowing #28's provenance. Nothing about the build changes — buttons are
  still unrendered in both dialects, and still wait on the HTTP interaction
  endpoint (#29) — but the
  reason is now the right one, which is what makes the decision reviewable:
  switchboard does not buy interactivity by staying on the framework Google is
  migrating off, since converting an app is one-way and a control that stops
  working at conversion is worse than one that waits for the ingress that will
  outlive it. `docs/DESIGN.md` §3.3, `README.md`, `docs/googlechat-setup.md`,
  and the `googlechat` package docs.
- A failed Slack `users.info` lookup is no longer cached as the caller's
  identity (#65). It falls back to the raw user ID so a turn is still
  attributed, and caching that was fine while the string only reached an audit
  line — but `--approvers` is keyed by it, so one rate-limit response on
  somebody's first press of the day would have mapped them onto `U0123ABC` for
  the life of the process and refused every approval they gave afterwards, with
  one `users.info` line from hours earlier as the only clue and a restart as the
  only cure. Now re-asked next time, which costs a call per turn while the API
  is unhappy and nothing at all when it is not; a lookup that succeeded and had
  no email to give is a fact about the user rather than a blip, so that one is
  still cached.
- Adding the app to a Google Chat space by @mentioning it was answered with
  silence (#55) — the first thing anyone installing it does, and indistinguishable
  from the missing Pub/Sub grant the setup doc warns about. Chat splits that one
  action across two events: the added-to-space half suppresses its welcome
  because a second event is coming, and the second event is a bare
  `@Switchboard` whose `argumentText` Chat strips to nothing, which the decoder
  read as no turn to run and dropped. Each half was right about its own case and
  the composition was a silence. A bare mention now decodes to the welcome —
  in any space, not only a just-added one, so the gateway remembers nothing and
  `@Switchboard` on its own is also how you ask what it accepts. The add still
  answers exactly once: the suppression stays, and only the message half posts.
  A message with no body and no mention, an attachment on its own, is still
  ignored.
- A Google Chat message that overflowed an `append` continued into a malformed
  conversation key (#39). The ingress builds a continuation from
  `ref.Conversation`, and a ref for a post into a bare space did not name the
  thread Chat assigned it — so the ingress fell back on Slack's rule that a
  top-level message's id is the thread it roots, and handed a *message*
  resource name (`spaces/AAA:spaces/AAA/messages/CCC`) to a thread field. The
  Chat adapter now returns a ref naming where the message actually landed. Only
  the bare-space case was affected, which is precisely what an alert posts to.
- An `append` that overflowed a message addressed by a key ending in a colon
  (`C0123:`, which names a channel and no thread) posted its continuation at the
  top level of the channel instead of under the message it continues (#39). Both
  adapters' egress already read a trailing colon as "no thread"; the ingress
  read it as "some thread".

### Changed
- `slack.New` no longer requires an app-level token (#23): an empty one builds
  an egress-only adapter, whose `Run` returns `chat.ErrNoInbound` rather than
  blocking on a socket it never opened. `serve` still refuses to start
  without one unless `--outbound-only` says the run means it, so a deployment
  whose app-token secret is emptied by a bad rotation crash-loops instead of
  quietly answering nobody. An outbound-only Slack run never calls `auth.test`,
  so a bad bot token surfaces on the first post rather than at startup.
- A post into a bare Slack channel is answered with the thread the message
  rooted (`C0123:1723742401.000100`) rather than the bare channel (#39). Both
  adapters now hold the same invariant — a ref names the thread its message
  landed in — so the ingress follows the ref instead of knowing each platform's
  threading rule. Following up with the key the POST returned already worked and
  still does; a caller that stored the bare channel is also unaffected, since
  the thread part plays no role in addressing a message.
- `internal/version.Version` is `v0.3.0-dev`, so a build off main no longer
  claims to be the release that just shipped (#43). Same window
  `verify-version-fallback` keeps short after every tag.
- A `501` from the agent backend is no longer treated as worth retrying (#40).
  It is a `5xx`, but it does not mean the backend is struggling — it means the
  route was never built for this session, and the same request gets the same
  answer for as long as that process lives. Retrying it is a loop, and telling
  an operator to try again is a lie. Every other code keeps the split it had.

## [v0.2.0] — 2026-08-20

A release about one mode. `--progress-mode stream` existed in v0.1.0 and said
almost nothing: every tool call rendered as "🔧 Running `bash`", so a
fifteen-call turn was fifteen identical notices and a parallel frame read
`` `bash`, `bash`, `bash` ``. It now says what each tool was called with and
whether it worked, groups a frame under one header, and edits that notice in
place as the results land.

That makes `stream` the first mode to put tool-authored text in a chat room,
which is a disclosure decision rather than a formatting one, so the posture is
written down in the README and enforced in the parse helper: **one scalar
argument per call**, never the whole object; flattened to one line and clamped
to 120 bytes; passed through a redaction pass for the credential shapes it
recognises. Tool *output* is never shown in any mode — a failure renders as
`exit 2` and nothing more. The redaction is a net, not a guarantee, and the
other four steps are what bound a miss to one clamped field. A deployment that
cannot accept even a clamped argument in the channel should run `status`, which
names tools without arguments, or `indicator`, which shows neither.

Two things carry over from v0.1.0 unchanged. **Google Chat runs over Pub/Sub**,
which keeps the no-public-webhook posture and costs anything needing a
synchronous response: no dialogs, and no card clicks at all (#28). #29 tracks an
HTTP interaction endpoint. And **long-turn progress still defaults to
`indicator`**; `stream` is opt-in per channel, and is now considerably more
worth opting into.

Upgrading from v0.1.0 needs no configuration change. Consumers should pin
`v0.2.0` rather than a pseudo-version; `switchboard version` reports it, whether
the binary came from `go install …@v0.2.0` or from
`ghcr.io/go-steer/switchboard:0.2.0`.

### Added
- `--progress-mode stream` now says what a tool was called with and whether it
  worked (#36). It rendered "🔧 Running `bash`" and nothing else, so a turn that
  made fifteen shell calls was fifteen identical notices, and a frame carrying
  three concurrent ones read `` `bash`, `bash`, `bash` ``. Notices now carry a
  one-line argument summary, group a parallel frame under a header, collapse
  calls a reader could not tell apart to `` `bash` ×3 ``, and are edited in
  place as each result lands — so a turn posts one message per frame rather than
  two per call. Arguments and results were already on the wire; `agentFrame`
  modelled neither.
- Tool results are read off the `agent` event at all (#36). They are authored by
  the tool, so the daemon labels the event `user`, and the role filter that tool
  *calls* need is what kept results invisible. A call now finishes as ✅ or
  ❌ with its exit code, from the two response conventions worth reading
  (a numeric exit code, an error field); an unrecognised shape is reported as
  success, because a tool that answered at all usually ran.
- The disclosure posture for tool arguments is written down, in the README and
  on the parse helper (#36). Arguments are untrusted, unbounded and exactly
  where a secret shows up, so `stream` shows one scalar argument per call,
  clamped to 120 bytes and an ellipsis, flattened to one line and passed
  through a redaction pass; `status` names tools without arguments and
  `indicator` shows neither;
  tool output is never shown in any mode. The redaction is a net, not a
  guarantee — the other four steps are what bound a miss to one clamped field.
  Until now the seam stopped at names with nothing saying why, which read as an
  oversight rather than as a choice.
- Three scrubbed payloads from the live Google Chat run join the replay corpus:
  the two events an @mention-add sends, and a space message carrying the
  `USER_MENTION` annotation that no captured DM could have (#30). The add pair
  confirms `chat.addedToSpacePayload.interactionAdd` — the last add-on field
  name still transcribed from prose rather than read off the wire — and
  incidentally reproduces #55: both halves replay to `ignored`, so an
  @mention-add is answered with silence.

### Fixed
- Tool activity and answers now dedupe against separate seq watermarks (#36).
  They shared one, so a tool frame raised the bar the answer had to clear, and
  an answer at or below that seq was discarded as a replay — the worst thing
  this gateway can do. Seqs only go up on a healthy stream, so this was
  unreachable in practice; the watermark exists for a stream that is not
  healthy, which is exactly when it must not be holding a tool frame's number.
- `verdict` no longer renders an implausible exit code into the thread (#36).
  A tool reporting `{"exit_code": 1e300}` produced a nineteen-digit "exit …"
  from a Go conversion whose result is undefined for an out-of-range float.
  Non-zero is still a failure; only the number is dropped.
- An HTTP tool no longer reports every successful call as a failure (#36).
  `status_code` was in the exit-code key list, where non-zero means failure, so
  `200` rendered ❌ with "exit 200" and a frame of them said "(3 failed)" — the
  verdict inverted for every tool that speaks HTTP. It is now read on its own
  terms, and only in the failing direction: 4xx and 5xx fail as "HTTP 404",
  while anything else says the request arrived and nothing more — the least
  specific signal in the object — so `{"status_code": 200, "error": …}` goes on
  to read the error rather than stopping there calling it a success.
- A falsy `error` field is no longer read as a failure (#36). `{"error": false}`,
  `{"error": 0}` and `{"error": {}}` are how many tools report success, and
  testing the field's presence marked all of them ❌.
- Tool arguments hold the documented 120-byte bound (#36). Redaction ran after
  the clamp, and `<redacted>` is longer than some of what it replaces, so a
  120-byte argument could reach the channel at 208 — README and the parse helper
  both promised a bound the code did not hold. Clamped on both sides now.
- Redaction catches the shapes it names (#36). `Authorization: Bearer …`,
  `AWS_SECRET_ACCESS_KEY=…` and a password in a URL's userinfo all went through
  untouched: the credential words had to be the whole name and sit immediately
  before the separator, which none of those forms do. The test suite used an
  authorization header as its exemplar secret and passed on a different
  alternative matching the token inside it, so the gap read as covered. A
  credential word may now sit inside a longer name, but only at a separator
  boundary — `AWS_SECRET_ACCESS_KEY` yes, `--max-tokens` and curl's `--anyauth`
  no, the last of which would otherwise hide the flag and leave the credential
  beside it. In the other direction `sk-` was matching inside ordinary words
  (`s3://bucket/some-sk-thing`), an empty flag value reached across the space
  and redacted the *next* flag, and a URL's `host:port` looked like userinfo.
  A plural or numbered credential name is recognised too — `--credentials`,
  `--secrets=`, `SECRET1=` — where requiring a separator after the word missed
  all of them. `token` is the one word left unpluralised, because a `tokens` is
  nearly always a count.
- A password in a URL is matched without reaching across the line for its `@`
  (#36). The argument is flattened to one line before redaction, so a run of
  non-space characters can span a whole JSON object: in
  `{"url":"http://host:8080","email":"alice@example.com"}` the search ran from
  the port to an ordinary email address and ate the port, the key and the local
  part, leaving invalid JSON and no secret found.
- An argument is redacted over a wider window than it is shown in, and that
  window never splits a word (#36). Clamping to the visible 120 bytes before
  redacting could cut a credential below the length that makes it recognisable,
  so an `sk-` key's head was published where the whole key would have been
  elided — and widening the window alone did not fix it, since redaction
  *shrinks* what it matches and enough secrets ahead of the key pull the cut
  back into view wherever it is put. On a word boundary a credential is either
  matched whole or absent.
- `statusCode` is read as well as `status_code` (#36), as `exitCode` already was
  beside `exit_code`. It is the field name on a Node response object, so a tool
  built on one reported its 404 as a success.
- A tool verdict survives onto a Google Chat card (#36). Cards strip the
  notice's leading emoji because the widget's icon says the same thing — true
  until tool notices gained verdicts, after which ✅ and ❌ were both deleted and
  replaced by the same static gear, and a card reader could not see that a tool
  had failed. The emoji now selects the icon.
- A frame answering several calls on one notice is one edit, not one per result
  (#36). Each result edited the message separately, and every edit but the last
  wrote a state that was already stale.
- A tool notice can no longer be registered against the wrong turn (#36). It is
  recorded after the send that posted it returns, and the next turn clears the
  list from another goroutine, so a slow send let the previous turn's calls
  collect this turn's results. Notices now carry the turn they were sent in, and
  the turn transition bumps the counter and clears the list under one lock — as
  two steps it left a window between them that a late notice could slip through
  whichever way round they were taken.
- An id-less tool result matches the *oldest* unanswered call of its name, not
  the newest (#36), across notices as well as within one. Calls go out in the
  order the model asked for them, so answering in that order — the ordinary
  case — meant every result was ticked off against the wrong line, and every
  argument shown next to a verdict belonged to another call. A result carrying
  an id switchboard has not seen no longer falls back on the name.
- Every id in a frame is honoured before any name in it is guessed at (#36). An
  id-less result filed first could take by name the very line an id-carrying
  result later in the same frame owned; that one then found the line answered
  and was dropped, so one call wore another's verdict and a second stayed at 🔧.
- A repeated call id resolves against the newest notice holding it (#36). Ids
  are meant to be unique and nothing here can check that, and a daemon numbering
  its calls per frame would repeat them — in which case the frame still being
  answered is the right one.
- A turn that talks before it answers no longer loses its progress clock (#42).
  "Let me check the logs…" arrives as a completed model turn indistinguishable
  from the answer, and was delivered as one: placeholder deleted, clock stopped,
  turn marked done — so the long part of the turn ran with nothing in the thread
  saying it was running, and the stream-lost notice was disarmed while it did.
  Interim text now re-anchors the placeholder below itself and keeps counting
  from the original start. What tells the two apart is turn-complete, so this
  needs a daemon that advertises it; against one that does not, every text still
  ends the turn as before. Overlapping turns in one thread are the other half of
  #42 and are not fixed here — that needs a turn identity switchboard is not
  given (go-steer/core-agent#840).
- A turn boundary now clears the progress placeholder of a turn that has already
  spoken, instead of freezing it (#42). Freezing is right for a silent turn —
  the stopped clock is the only sign the question was heard — but wrong once
  there is text above it, which is the state the re-anchor creates.
- `switchboard_agent_turns_relayed_total` no longer counts interim narration as
  a relayed turn (#42). It is still approximate against a daemon that does not
  advertise turn-complete, where every relayed message ends a turn as before.

### Changed
- `stream` mode's notices stay in the thread by decision rather than by accident
  (#36). `startProgress` is a no-op outside `indicator`/`status`, so nothing was
  ever retiring them; the question of whether `stream` is an ephemeral view or a
  durable trace is now answered — durable, since the trail is the reason to
  choose it over the other two, and README says so.
- The runbooks no longer say `switchboard version` "confirms the build identity
  you are testing" (#30). It does not, in a linked `git worktree`: Go stamps the
  enclosing checkout's HEAD, and the output gives no sign. Both setup docs now
  show the `-ldflags` build that is actually trustworthy.
- `docs/daemon-setup.md` records a Vertex 404 worth recognising (#30):
  `gemini-3.7-flash` 404d on `us-central1` and answered on
  `GOOGLE_CLOUD_LOCATION=global`. One model, one project, one day — noted
  because the symptom reads as a typo or a missing enablement rather than as a
  routing choice.
- `internal/version.Version` is `v0.2.0-dev`, so a build off main no longer
  claims to be the release that just shipped (#43). `verify-version-fallback`
  has demanded this since the `v0.1.0` tag was pushed — the window between the
  tag and this bump is the one thing the check is written to keep short.

## [v0.1.0] — 2026-08-20

The first tagged release. Everything below shipped in it; the sections are the
work as it landed on main rather than a summary written afterwards.

What it is: a chat gateway that puts a core-agent daemon in a Slack or Google
Chat thread. Transport only — it executes no tools and resolves no credentials,
and asserts the caller to the daemon with `X-Asserted-Caller`.

Two things a reader should know before pinning it. **Google Chat runs over
Pub/Sub**, which keeps the no-public-webhook posture and costs anything needing
a synchronous response: no dialogs, and no card clicks at all — the connection
settings do not route the trigger (#28). Cards are output; a setting is changed
by typing the command. #29 tracks an HTTP interaction endpoint. And **long-turn
progress defaults to `indicator`**: a placeholder that ticks and is retired when
the answer lands. `stream` relays every model turn and tool call, which is the
most transparent and the noisiest, and is opt-in per channel.

Consumers should pin `v0.1.0` rather than a pseudo-version; `switchboard
version` reports it, whether the binary came from `go install …@v0.1.0` or from
`ghcr.io/go-steer/switchboard:0.1.0` (#43).

### Added
- Every log line now carries the time it was written, and `--log-format json`
  (`SWITCHBOARD_LOG_FORMAT`) renders one JSON object per line for a collector
  (#49). Until now the entire logging surface was a single closure —
  `fmt.Fprintf(os.Stderr, prog+": "+format+"\n", ...)` — so a line had no time,
  no severity and no structure, and neither `log` nor `log/slog` appeared
  anywhere in the tree.

  A hosted deployment was not quite as blind as that sounds: Cloud Run and a
  k8s collector both stamp an ingestion time on arrival. But that is when the
  line was collected rather than when it happened, and it is missing outright
  from a local run, a redirect to a file, a `kubectl logs` dump taken without
  `--timestamps`, and log output pasted into an issue — which is how most of
  the Google Chat walkthrough findings arrived. The gap fell hardest on the
  behaviour that is hardest to reason about: the relay logs
  `stream ended (%v); resuming from seq %d in %s` on every reconnect, and
  whether a session flapped four times in a minute or four times in an hour is
  the entire diagnostic question. Ordering alone does not answer it.

  The stamp is fixed-width UTC to the millisecond in both formats, rather than
  RFC3339Nano, whose trailing-zero trimming renders a different width per line
  and makes an unreadable left-hand column. JSON goes through
  `slog.JSONHandler` for its escaping, which is not incidental: the relay logs
  raw daemon frames it cannot decode, and `--googlechat-log-events` logs whole
  Chat payloads, so quotes and braces in a message are the normal case — as is
  the occasional newline, which JSON escapes and text passes through verbatim.
  The entry text is under `message`, where Cloud Logging looks for it, rather
  than slog's `msg`; `time` is already both.
  The startup banner and the shutdown line went to stderr directly and now go
  through the logger too, so a JSON stream no longer opens with three
  unparseable lines — nor ends on one, since a startup failure is now logged by
  `serve` rather than printed by `main`, and a crash loop is when that line is
  read. Build identity is logged ahead of the config checks so an operator
  whose flags are rejected still learns which build rejected them. Not
  everything is the logger's: a flag that will not parse and a `--log-format`
  that cannot be rendered happen before there is a logger to say them through,
  `--help` and `version` keep their own output, and the `--metrics-addr` and
  `--ingress-addr` listeners still hand runtime errors to `net/http`'s default
  logger — the README lists them, and #49 tracks the last one.

  Two things a line still does not carry, both deliberate and both tracked in
  #49. There is no **severity**: nothing upstream distinguishes
  `slack: connected as %s` from `handle %s: surface error: %v`, so every record
  arrives at one level, and labelling them all `INFO` would mislabel the
  failures — worse than the `DEFAULT` that Cloud Logging assigns a record with
  no severity at all. Giving the call sites a level to carry is a pass over all
  54 of them. And the messages are still **unstructured**, with the component
  and the conversation key interpolated into the format string where no
  collector can filter on them; turning those into attrs means rewriting every
  message, and should wait until the message set has settled.

  The `Logf func(string, ...any)` hook on the adapter configs, the ingress and
  the router is unchanged, so no call site moved and `t.Logf` still works as a
  test logger.
- The relay reads the daemon's `status-update` and `capabilities` frames (#33).
  `inbox` and `pause` remain advertised and unconsumed.

  `status-update` carries the session's turn state, and its `idle` is the only
  turn boundary that does not depend on the daemon having classified how the
  turn ended: `turn-complete` fires when a turn succeeded and `turn-error` when
  it failed in a way the daemon could name, but the daemon's turn cleanup emits
  `idle` on both exit paths of any turn that started. Anything ending a turn
  outside those two — including a `turn-error` payload this build cannot read,
  which is logged and dropped rather than guessed at — used to leave the session
  marked in flight and its progress clock running to the hour-long backstop, and
  made the next unrelated outage announce a failure for a turn that had ended
  long before. Not every refusal is covered: core-agent's cost-ceiling and
  watchdog pre-flights return before that cleanup is installed and emit no frame
  at all, so a turn refused there still runs to the backstop.

  `awaiting_permission` and `awaiting_elicit` are deliberately not boundaries.
  The daemon has stopped on something only a human at its own console can
  answer, and the turn is still owed, so the state is logged — an operator
  otherwise watches a turn that will never move with no way to learn why — and
  nothing in the thread is retired. Relaying an approval prompt to a chat caller
  is a feature of its own, not a side effect of parsing a frame. core-agent
  defines both states and emits neither today, so this is written against the
  protocol rather than against observed traffic; it earns its place because the
  obvious spelling of the boundary — "not idle means working" — turns the first
  daemon to emit one into a turn boundary at exactly the wrong moment.

  An `idle` is only acted on once the daemon has reported the turn *running* on
  the same connection, and each boundary puts that flag back down. Every stream
  opens with a status snapshot that says idle on a session between turns, which
  would otherwise retire the placeholder of a turn posted a moment earlier and
  not yet injected; and the daemon sends `status-update` for reasons other than
  a turn changing state — a model swap, a permission-mode change — which
  otherwise end whatever turn has started since. The cost of scoping the flag to
  one connection is a turn that both starts and ends inside a stream outage
  staying in flight, which is what a `turn-complete` lost in the same gap
  already does; erring the other way retires a live turn's placeholder and
  disarms the stream-lost notice meant to cover the outage.

  `capabilities` is logged once per session: what the daemon is, what protocol
  it speaks, and which of the events switchboard relies on it does not advertise.
  Nothing negotiates on it — the daemon sends what it sends — but an older daemon
  degrades by *absence*, a thread that stays quiet or a reply with no footer, and
  absence looks nothing like a version mismatch from the outside. The legacy
  `agent` event is excluded from that check, since no conformant daemon
  advertises it; the frame describes the logical surface instead, so the traffic
  it carries is checked under the names that surface uses — a daemon
  advertising no `stream-chunk` relays no answers at all.

  Neither frame is relayed into a thread, and neither touches the seq the agent
  events are indexed by, which drives resume-after-reconnect and replay
  suppression.
- The `indicator` and `status` progress messages now tick: every 15 seconds the
  placeholder is re-rendered with how long the turn has been running, and in
  `status` mode with the tool it is on and the step count — "⏳ Working… 2m30s ·
  running `bash` (step 7)". Previously the only thing
  that ever changed a progress message was a tool call, so a turn that thought
  for four minutes without calling one was indistinguishable from a dead
  session. The clock runs from when the message was handed to the daemon rather
  than from when the placeholder landed, and stops on the daemon's
  `turn-complete` so a turn that ends without an answer freezes instead of
  ticking forever — with an hour-long backstop for the case where even that
  never arrives. Coarse on purpose — every tick is an API edit — and a failed
  edit backs off rather than retrying at the rate that provoked it, and never
  fails the turn. Status mode's tool notice now renders the same line as a
  tick: two writers, one message, one format. The clock still stops early in
  the two cases where the placeholder is retired before the turn is over — a
  model that narrates before calling a tool, and a follow-up question asked
  before the first answer lands — which needs a per-turn message identity the
  relay does not have yet.
- `--show-usage` appends each answer's model, tokens, cost and latency as a
  footer — a Block Kit `context` block on Slack, a card footer on Google Chat.
  The daemon has been reporting all of it and switchboard was dropping it, but
  not in a form that can be read off any single event. The daemon's "turn" is
  one model call, so a turn that makes five tool calls emits six
  `usage-update`s, each with a `last_turn` covering only the call that just
  finished; the conversational turn's tokens and cost are the *delta* of the
  running totals across it. Latency comes from `turn-complete`, which does fire
  once per conversational turn and does span the whole of it. Both events
  arrive *before* the agent event carrying the answer, so the relay banks the
  running difference and attaches the merged result on delivery. Off by
  default, because a turn's
  cost is spend data and a shared channel is the wrong place to disclose it
  uninvited; suppressed outside the rich renders, where a footer line would
  have to survive the per-message chunker. Per-turn only — a session running
  total would be stale the moment it landed on a message nothing will edit
  again. Held until `turn-complete`, so a model that narrates mid-turn does not
  get a footer covering part of its own turn. On Google Chat the footer needs a
  card to ride, so a prose answer — one with no heading, list, or code — carries
  none even in `--googlechat-cards rich`.
- `dev/demo/daemon` stands up a throwaway core-agent for local testing and
  demos: bearer-table auth, switchboard registered in `proxy_identities` so
  `X-Asserted-Caller` is honored, and a generated token printed for
  `$SWITCHBOARD_DAEMON_TOKEN`. Switchboard is a gateway and ships no agent, so
  every live walkthrough needed this config reconstructed by hand from
  `cmd/switchboard/integration_test.go`. Defaults to the echo provider (no model
  credentials); `--model`/`--provider` point it at a real one, since the demo
  steps that exercise markdown and structured answers need something that
  actually talks. Demo-only — it writes a token to disk and runs the agent in
  `yolo` permission mode.

- Google Chat **Workspace add-on** support (`pkg/chat/googlechat`). Google is
  migrating Chat apps from the Chat-API interaction-events framework to add-ons
  that extend Chat, which changes the shape of every event. The adapter detects
  the dialect **per event** and understands both, so the console conversion —
  which is irreversible and applies to all users at once — needs no coordination
  with a switchboard deploy. Pub/Sub remains the ingress, preserving the
  no-public-webhook posture; the cost is that nothing needing a synchronous
  response works, so no dialogs — and, separately, **no card clicks**: the
  connection settings route four triggers (message, app command,
  added-to-space, removed-from-space) and a click is not among them, so a live
  one errored in the UI with nothing reaching the subscription (#28). A click
  would not have needed a synchronous response, since it can be answered by
  patching the hosting message; it simply never arrives. Card updates are
  whole-message patches either way. No card the gateway sends carries a button,
  since one could only ever fail: the welcome names the accepted `progress`
  values in its text instead, still sourced from the handler rather than
  hard-coded, and the command ack is just the ack. Decoding and dispatching a
  click stays implemented and tested for the HTTP ingress in #29.
- Google Chat `cardsV2` rendering, gated by `--googlechat-cards`
  (`off` / `status` / `rich`, default `rich`). `status` renders the gateway's
  own messages — progress, tool activity, error notices, command acks, the
  welcome — as small icon cards; `rich` also lays a structured agent reply out
  as a card (a section per heading, dividers for rules, code fences verbatim).
  Plain text is always sent as the message fallback and a card Chat rejects
  falls back to posting the text, so a rich render never costs a reply. Three
  levels rather than Slack's boolean because gateway cards are short and
  authored here while an answer card lays out arbitrary model output.
- Google Chat replies are now translated into Chat's text dialect
  (`**bold**` → `*bold*`, `[label](url)` → `<url|label>`, headings → bold,
  rules, strikethrough, fences), mirroring the Slack `toMrkdwn` pass. Previously
  markdown from the agent was posted raw and arrived with its delimiters
  showing. Card widgets get a second pass into the small HTML subset Chat
  accepts, with `&<>` escaped — a command ack quoting
  `progress <off|indicator|status|stream>` would otherwise render as broken
  markup.
- `--googlechat-commands` (e.g. `"1=progress,2=help"`) maps Chat app-command IDs
  onto gateway verbs. Chat identifies a command by the numeric ID configured in
  the API console and never by its name, and add-ons never report an invoked
  function name back, so the mapping is how a dedicated `/progress` command is
  recognized. With no mapping the verb is still read from the command's argument
  text, so a single `/switchboard progress status` command keeps working.
- `docs/slack-setup.md`: the Slack runbook that never existed — app creation and
  Socket Mode, the scope each API call actually needs (derived from the adapter,
  not from memory), what the adapter does and does not engage on (`app_mention`
  only: a plain thread reply or an unmentioned DM is ignored), a demo script, and
  a troubleshooting table keyed by symptom. Slack was the first platform
  supported and the only one whose setup lived nowhere but the README's quick
  start.
- `docs/daemon-setup.md`: the core-agent daemon behind `--daemon-url`, extracted
  from the Chat runbook so both platforms share one copy. Adds the piece neither
  runbook had: running Slack and Google Chat **side by side against one daemon**
  — two processes since `--platform` takes one value, one registered identity
  since both now assert email, and separate sessions per platform thread.
- `docs/googlechat-setup.md`: Chat app + Pub/Sub setup, a demo script, and the
  two testing layers that do not need a live app — golden card JSON in
  `testdata/cards` (pasteable into Google's Card Builder to see a real render)
  and an event-replay corpus in `testdata/events` that runs raw payloads through
  the real dispatch path. Both regenerate with
  `go test ./pkg/chat/googlechat -run 'Golden|Replay' -update`.
- `--googlechat-log-events` logs every inbound payload verbatim, so real Chat
  traffic can be captured as decoder fixtures — the one thing hand-written
  fixtures cannot verify. Off by default: payloads carry message text and sender
  identity.
- `chat.Reply.Kind` classifies what the router is sending — agent turn, progress
  placeholder, tool activity, error notice, command ack — so an adapter can
  render the gateway's own chatter in the platform's idiom. Advisory: an adapter
  that ignores it (Slack) behaves exactly as before.
- `chat.CommandChoices`, an optional adapter-facing capability reporting the
  values a gateway setting accepts. It is what lets the Google Chat adapter name
  `progress`'s values on a card with the router remaining the single source of
  truth for the list.
- Outbound ingress (`--ingress-addr`, default disabled): an authenticated HTTP
  surface that lets another service post — and later edit — a message in a
  conversation with no inbound chat event to reply to (scheduled digests,
  monitoring escalations, async approval prompts). `POST /v1/messages`
  `{conversation, text}` returns the `{conversation, id}` ref;
  `PATCH /v1/messages` `{conversation, id, text}` edits it in place (`501` on a
  platform that cannot edit). Callers authenticate with a bearer token from
  `--ingress-token-env` (default `SWITCHBOARD_INGRESS_TOKEN`) — separate from
  the daemon token — and can be confined to a set of conversations with
  `--ingress-allow`. An optional `Idempotency-Key` header makes a retried POST
  return the original ref instead of double-posting. There is no `platform`
  field: the instance's `--platform` implies the target. Slack only for now;
  `--ingress-addr` with `--platform googlechat` is refused at startup. New
  `switchboard_ingress_requests_total{op,outcome}` metric.
- `PATCH /v1/messages` `{conversation, id, append}` adds a line to a message
  instead of replacing it, so a caller keeping a running incident timeline does
  not have to resend the whole body. Switchboard remembers the current text of
  the messages it posted (bounded, in-memory, last 1024); an append to anything
  it does not remember answers `409` and the caller sends full `text`. When the
  combined text would pass the platform's single-message limit the addition is
  posted as a reply in the same thread and answered `200` with the
  continuation's ref, rather than truncating the timeline. `text` and `append`
  are alternatives — a PATCH must carry exactly one.
- `chat.ErrNotFound` and `chat.ErrDenied`: provider-neutral classifications of
  a platform refusal, so the ingress can answer `404`/`403` for a permanent
  failure instead of reporting everything as a retryable `502`. New optional
  `chat.TextFitter` adapter capability (`FitsOneMessage`) backs the append
  rollover; a provider that does not implement it answers `501` to `append`.
- Slack egress accepts a thread-less conversation key (a bare channel ID),
  posting a new top-level message and returning the ts it rooted — so an
  ingress caller with no thread to reply in can post one and thread its
  follow-ups under `"<channel>:<id>"`. A chunked thread-less message now keeps
  its parts together under the first one instead of scattering across the
  channel.
- Thread-scoped error surfacing: a session-create/inject/wake failure now posts
  a notice into the originating conversation instead of only logging, so a
  turn that can't run doesn't just leave the thread silently dead. New
  `daemon.StatusError` (carries the daemon's HTTP status) and
  `daemon.IsTransient(err)` let the notice distinguish a retryable 5xx/network
  failure from a terminal 4xx.
- Health check + Prometheus metrics (`--metrics-addr`, default disabled): when
  set to a `host:port`, `serve` starts an HTTP server exposing `/healthz`
  (liveness, `200 ok`, no scrape dependency) and `/metrics`. The router
  instruments inbound turns and commands, core-agent requests
  (`create`/`inject`/`wake`, with latency), outbound sends, agent turns relayed,
  SSE reconnects, and active sessions — all series prefixed `switchboard_`. A
  metrics bind failure brings the process down so the health surface a
  deployment's probes depend on is never silently absent. The deploy manifests
  set `--metrics-addr=:9090`, expose a named `metrics` port, wire `/healthz` to
  the liveness + readiness probes, and narrow the NetworkPolicy from deny-all to
  admit only that port. Mirrors k8s-lookout's `serveMetrics`.
- Kubernetes deploy manifests (`deploy/`, kustomize): a platform-neutral `base`
  (ServiceAccount, deny-all-ingress NetworkPolicy, and a hardened single-replica
  Deployment — non-root, read-only rootfs, all caps dropped, `RuntimeDefault`
  seccomp) plus two overlays. `overlays/slack` appends `--platform=slack` and
  mounts the Slack app/bot tokens from a Secret; `overlays/googlechat` appends
  `--platform=googlechat` with the project/subscription and adds a Workload
  Identity annotation for ADC (Pub/Sub subscribe + Chat bot scope). The
  core-agent bearer token and Slack tokens ride env vars sourced from Secrets the
  operator creates out-of-band — never flags. No Service or health probe:
  switchboard exposes no listening port (both platforms are outbound), so
  liveness is the process itself. `deploy/README.md` documents the prerequisites
  (namespace, Secrets, Workload Identity) and `kubectl apply -k` usage. Mirrors
  the k8s-lookout `deploy/` layout.
- Image publishing (`.github/workflows/release-images.yml`): builds the
  distroless multicall image for `linux/amd64` and `linux/arm64` off the
  buildx-ready Dockerfile and pushes it to `ghcr.io/go-steer/switchboard`,
  mirroring core-agent. Every push to `main` (every merged PR) publishes a
  floating `:main` and an immutable `:main-<short-sha>`; a semver tag (`vX.Y.Z`)
  publishes `X.Y.Z`, `X.Y`, `X`, and `latest` (a prerelease tag publishes only
  its exact version and never moves `latest`). Build identity
  (`VERSION`/`COMMIT`/`BUILD_DATE`) is injected as build args and surfaces in
  `switchboard version`; a gha build cache is reused across runs. Every image is
  signed with Sigstore keyless (cosign, GitHub OIDC → Fulcio → Rekor), verifiable
  with `cosign verify`.
- Google Chat adapter MVP (`pkg/chat/googlechat`, `--platform googlechat`):
  Pub/Sub ingress (the app publishes events to a topic and switchboard pulls
  them from a subscription, so no public webhook is exposed — matching Slack
  Socket Mode and the distroless posture) and Google Chat REST egress
  (`spaces.messages` create/patch/delete, so every long-turn progress mode
  works). A conversation is keyed on space + thread, so a mention thread maps to
  one core-agent session; the caller asserted to the daemon is the sender's user
  resource name (`users/NNN`) — verified identity and per-caller credential
  resolution stay in core-agent (W0). Long replies are split across in-thread
  posts under Chat's message limit. Credentials come from Application Default
  Credentials (`--platform`, `--google-project`, `--google-subscription`; no
  secrets as flags). Reply formatting follows.
- Google Chat native slash commands (`pkg/chat/googlechat`): a configured slash
  command (e.g. `/switchboard progress status`) is mapped onto the same
  provider-neutral `chat.Command` seam Slack's `/switchboard` uses and routed to
  `Handler.HandleCommand` — so runtime per-channel progress control works on
  Google Chat with no router change. The command is detected from the event's
  `slashCommand` (its `argumentText` is already just the verb and arguments) and
  never reaches the daemon as a turn; the acknowledgment is posted back into the
  invoking thread (Google Chat has no ephemeral async reply). The command word,
  its argument list, and the caller's user resource name flow through unchanged.
- Slack adapter MVP (`pkg/chat/slack`): Socket Mode ingress that engages on
  app-mentions, normalizes them into `chat.Message` (mention markup stripped),
  and posts replies back into the originating channel + thread. Caller identity
  is the user's email (`users.info`, cached) or raw Slack user ID, selected by
  `--caller-id`.
- Session router (`cmd/switchboard`): in-memory `conversation → session` map
  keyed by channel + `thread_ts`; first turn creates a session and opens one
  long-lived SSE subscription that relays completed agent turns back through the
  adapter, subsequent turns inject + wake. `serve` now boots the Slack adapter
  from `--slack-app-token-env` / `--slack-bot-token-env`.
- SSE relay resilience (`cmd/switchboard`): the per-session event subscription
  now reconnects with exponential backoff (1s→30s, reset on progress) when the
  stream drops, resuming from the last seq seen so the daemon replays only new
  turns — a dropped stream no longer silently strands a conversation. Delivery
  is exactly-once: a turn replayed across the reconnect boundary is posted only
  once. First slice of Phase 2 hardening (#3).
- Long-turn progress feedback (`cmd/switchboard`, `--progress-mode`): with the
  default `indicator` mode the router posts a lightweight "⏳ Working…"
  placeholder into the thread when a turn is woken and deletes it as soon as the
  agent's reply is relayed, so a slow turn no longer looks dead; `off` keeps the
  prior silent-until-complete behavior. First slice of the long-turn feedback
  work (#3); `stream` and `status` modes and chat-command control follow. The
  egress seam grew to support this: `chat.Adapter.Send` now returns a
  `chat.MessageRef` and the interface adds `Update`/`Delete` (with an
  `ErrUnsupported` sentinel so a platform lacking them degrades to plain
  `Send`). Indicator mode needs the bot's `chat:delete` scope to clear the
  placeholder; without it the placeholder is left in place and the failure is
  logged.
- Long-turn progress modes `status` and `stream` (`cmd/switchboard`,
  `--progress-mode`): `status` keeps one message per turn — the "⏳ Working…"
  placeholder is edited in place to name the tool the agent is currently
  running (e.g. "🔧 Running `lookup`") and retired when the reply arrives;
  `stream` posts a standalone tool-activity notice per tool call in addition to
  relaying each completed turn, for maximum transparency (and noise). Both are
  driven off `agent`-frame tool calls (unambiguous), gated by the same
  exactly-once seq as turn delivery so a reconnect replay never reposts. Second
  slice of the long-turn feedback work (#3); chat-command control follows.
  `status` needs the bot's `chat:delete` scope (like `indicator`).
- Runtime per-channel progress control via chat commands (`cmd/switchboard`,
  `pkg/chat`, `pkg/chat/slack`): operators can change a channel's long-turn
  feedback mode without a restart. `progress <off|indicator|status|stream>`
  sets it, bare `progress` reports it, and the override is scoped to the
  channel it was issued in (other channels keep the `--progress-mode` default).
  Two surfaces: a native Slack slash command (`/switchboard progress status`,
  acked ephemerally) and a mention subcommand (`@switchboard progress status`,
  acked in-thread). The mention form matches only tightly (a known verb alone
  or with one argument) so a normal turn is never swallowed. This adds a
  provider-neutral `chat.Command` type and a `Handler.HandleCommand` method
  each adapter maps its native command surface onto (Google Chat slash commands
  later, no router change), plus a `Channel` field on `chat.Message` for
  channel-scoped settings. The native `/switchboard` command needs a one-time
  Slack app manifest entry (see README).
- `daemon.ToolCalls` helper to extract tool-call names from `agent` SSE frames.
- `daemon.AgentText` helper to extract assistant text from `agent` SSE frames.
- Slack mrkdwn rendering (`pkg/chat/slack`): replies now convert a model turn's
  standard markdown into Slack mrkdwn before posting — bold/italic/strikethrough,
  headers → bold, links → `<url|label>`, code spans/fences preserved (language
  tag dropped), and broadcast mentions (`<!channel>` etc.) defused. Long turns
  are split into multiple in-thread posts on paragraph boundaries, keeping code
  fences balanced across the split. Ported from hermes-agent's Slack adapter.
- Slack Block Kit rendering (`pkg/chat/slack`, opt-in via `--slack-rich-blocks`):
  replies can render as structured blocks — headers, dividers, nested
  rich_text lists, native table blocks (monospace fallback when a table
  exceeds Slack's limits), blockquotes, preformatted code, and mrkdwn section
  paragraphs. The mrkdwn text is always sent alongside as the notification
  fallback, the payload is clamped to Slack's block/section/header limits, and
  a rejected payload (`invalid_blocks`) automatically retries as plain text so
  a rich render can never lose a message. Ported from hermes-agent's
  `block_kit.py`; mrkdwn stays the default.

### Changed
- `--googlechat-cards` now defaults to **`rich`** rather than `status`, so a
  structured agent reply is laid out as a card out of the box. A card is not
  chunked, which makes `rich` the one mode in which a long fenced answer cannot
  break at a message boundary at all — with the widget truncation above fixed,
  it is the only mode with no content defect, and that is what a default should
  be. The escalation is narrow: `answerCard` returns nil unless the answer has a
  `#`/`##` header or a rule that actually draws, so conversational traffic
  behaves exactly as it did, and a render that goes wrong cannot cost a reply —
  a panic recovers into nil and a card Chat rejects falls back to posting the
  text. `status` stays supported and documented for an operator who wants
  gateway cards without model-authored ones, which is the whole reason this is a
  mode and not a bool.
- Google Chat now asserts the sender's **email address** as `X-Asserted-Caller`,
  matching what the Slack adapter has always done and what the daemon keys
  per-caller credentials by; `--caller-id id` restores the raw `users/NNN`
  resource name, and an event carrying no email falls back to it. One human
  reaching an agent from two chat platforms was previously two identities to
  provision. The address was on the wire all along: the generated `chat/v1`
  `User` type has no `Email` field, so decoding the actor through it threw the
  address away — `pkg/chat/googlechat/testdata/events/addon-live-message.json`
  is the captured payload that shows it.

### Fixed
- `go install`ed binaries no longer report themselves as a dev build (#43).
  `go install module@version` records the module version in the binary, but
  `internal/version` read only the `vcs.*` build settings — and those are
  stamped only when building from a checkout, never for a module-cache build.
  So the one field that *was* populated was the one field not read, and

  ```
  $ switchboard version    # go install ...@v0.0.0-20260819120633-a6e51f013566
  switchboard v0.1.0-dev (commit none, built unknown)
  $ go run ./cmd/switchboard version    # someone's uncommitted tree
  switchboard v0.1.0-dev (commit none, built unknown)
  ```

  were byte-identical, with no second field to fall back on.
  k8s-lookout hit this exactly (lookout#146, fixed in lookout#150).

  `resolveBuildInfo` now reads `debug.BuildInfo.Main.Version` — but *last*,
  behind both `-ldflags` and the VCS stamp, which is the part that is easy to
  get wrong. Since Go 1.24 a build from a checkout stamps `Main.Version` too,
  as a pseudo-version derived from the tags, so preferring it would have
  stripped the `-dev` marker off every developer's `go build` and replaced it
  with a restatement of the SHA already in the next field — and worse, a
  checkout sitting on a release tag would report the bare tag, i.e. a dev build
  claiming to be the release, which is the exact failure the new presubmit
  exists to prevent. What separates the two cases is the VCS stamp, written
  only when there is a checkout to stamp: no commit means a module-cache build,
  and there the module version is the only identity available. A `Version`
  injected by `-ldflags` without a `Commit` is also left alone. Nothing changes
  for a build from a checkout or for a release image.

  A `verify-version-fallback` presubmit comes with it, asserting that main's
  fallback is the next minor `-dev` after the newest release tag. The manual
  bump `internal/version` documents — "bump this manually on main right after
  cutting a release" — is the step that gets forgotten, and the cost of
  forgetting is every dev build claiming to be the release that just shipped.
  Pre-release tags are excluded: `--sort=-v:refname` ranks `v0.2.0-rc1` above
  `v0.2.0`, and `release-images.yml` publishes pre-release tags, so counting
  one would demand a wrong bump on every PR until the tag went away. CI now
  checks out with `fetch-depth: 0`, since a tagless checkout would let the new
  check pass on every branch. The package also gains its first tests.
- A long answer *about* markdown split into nonsense. A fenced block is closed
  only by a bare fence at least as wide as the one that opened it, so the
  `` ``` `` lines inside a ```` ```` ```` block are content — but the chunker
  counted every run of three or more backticks as one delimiter and split on the
  parity. It therefore read the inner opener as closing the outer block, and
  from there every break in the block was inverted: the piece was not closed and
  the next was not reopened, so the rest of the code arrived in the thread as
  prose with stray backticks around it — #31's exact symptom, on the one kind of
  answer most likely to be long enough to split.

  Counting was wrong in the other direction too. A run of backticks is only a
  fence at the start of its line; one with text in front of it is part of the
  prose, and counting it flipped the parity of everything after. Two things kept
  that from being worse than it was: only runs of three or more were counted at
  all, so ordinary `` `code` `` spans were safe, and a *matched* span
  contributed two runs and cancelled itself out. What it took was an unpaired
  one — a sentence naming the syntax, like "the `` ``` `` delimiter opens a
  block" — which is a line that turns up in exactly the answers this all goes
  wrong on. It does not have to be inside a code block to break one further
  down.

  Both are gone: where a block opens and closes is now tracked by a line-based
  scanner that carries the open block's *width*, and a continuation reopens with
  a marker of that same width rather than always three backticks. Reopening a
  ```` ```` ```` block with `` ``` `` closes it instead of continuing it, which
  is the same corruption reached one step later.
- A long Google Chat reply split mid-code-fence, so the backticks rendered
  literally. Chat's per-message ceiling is 4096 characters — counted here in
  bytes, which is conservative for multi-byte text — and an answer over it is
  posted as several messages; the split preferred a newline but knew nothing
  about `` ``` ``, so a break landing inside a fenced block left an odd number of
  fences in each half — raw backticks on screen in both, and the monospace lost
  in one. Slack's renderer had already solved that case, so the splitting itself
  now lives in `pkg/chat` and both adapters call it rather than keeping two
  copies to fix separately. They still differ where they should: Chat allows
  4096 to Slack's 3900.

  Further ways a split could corrupt a code block, found reviewing the shared
  version and fixed there, so Slack gets them too:
  - A break with no newline in reach could land *inside* the `` ``` `` itself.
    Both halves then hold a stray backtick, both count even, and no amount of
    balancing sees it — the same bug by another route. A break now never falls
    through a run of backticks.
  - The newline the break was taken on was trimmed along with every newline
    after it, so a blank line inside a YAML or Python block was deleted and the
    reader had no way to know. Exactly one line ending is consumed now, and the
    pieces rejoin into the answer the model actually wrote.
  - A break landing right after an opening fence posted `` ```/``` `` — an empty
    code block — and opened the real one on the next message. It now moves back
    a line so the opener travels with its code, for an opener carrying a
    language tag (`` ```go ``) as much as a bare one. The seam on the other side
    of the block does the same thing in reverse: a break landing right *before*
    the closing fence seals the piece, reopens on the next, and the answer's own
    closer arrives immediately after, so the empty block lands at the top of the
    continuation instead. That one moves back a line too, and when there is no
    line to move back to — the block's content starts what is left — the closing
    fence is pulled forward into the piece instead, which needs no marker at
    all. Two shapes needed more than moving the break: a block at the very top
    of the answer, where backing off the closer would land on the opener and
    put the empty block there instead; and a block whose first line is longer
    than a whole message, where the opener's own newline is the only one in
    reach. The break stops preferring a line ending for that one and cuts inside
    the long line, so the opener still travels with its code.
  - Fences were counted with `strings.Count`, which reads ```` ```` ```` as one
    delimiter plus a loose backtick and gets the parity backwards. A model
    reaches for a four-backtick fence when the answer is itself about markdown.
    Where a block opens and closes is now tracked by a scanner rather than by
    counting delimiters at all — see the entry above.
  - A break with no newline in reach could land between the two bytes of a
    CRLF, leaving the piece ending on a bare `\r` and the next opening on a bare
    `\n`. Model output reaches the chunker with its line endings as written;
    Chat does not normalise them. A lone `\r` is left alone — it is an old-Mac
    line ending and content, not half of anything.
  - The headroom reserved for the closing marker was three backticks and a
    newline, which is a byte short for the ```` ```` ```` block a model writes
    when the answer is about markdown: a piece closing one came back at 4097
    bytes against Chat's 4096. It is sized to the widest run in the answer now.
    The bound holds while the limit leaves room for both markers and a rune
    between them — it takes a run of two thousand backticks to defeat 4096 — and
    past that no piece can be both balanced and inside the limit, so an
    oversized piece is the better of the two failures.

  Not fixed there but fixed below: `--googlechat-cards rich` sends an answer as
  a card rather than as text, and the card path had a truncation of its own.
- A long answer-card paragraph in `--googlechat-cards rich` was cut at 3500
  bytes and given an ellipsis. A widget is one paragraph run, one fenced block,
  or one section body, so a single long code block was all it took — and the
  same answer in `status` or `off` arrived complete across two posts. Nothing
  was logged, unlike a card Chat *rejects*, which falls back to text; the
  reader's only signal was the `…`. The budget was a presentation argument ("a
  widget this long is already collapsed behind *show more*"), and collapsed is
  readable where truncated is gone. An over-long run now spills into consecutive
  widgets in the same section, split by the same fence-aware logic as the text
  path, since a widget boundary inside a `` ``` `` renders the backticks
  literally much as a message boundary does. The gateway's own widgets keep the
  clamp, as a backstop that cannot fire: their text is authored here, fixed, and
  nowhere near the budget. Splitting them would be no safer anyway — they carry
  HTML, and a clamp is a byte cut that can land inside a tag or an entity just
  as a split can. `maxCardHeader` keeps clamping too — a section header is one
  line in every client, which is a real presentation limit.

  Spilling lets a card grow without bound, so `answerCard` now measures the card
  it built and returns nil past 26,000 bytes, sending the answer as text — which
  splits into as many messages as it needs. Chat caps a whole message, text and
  `cardsV2` together, at 32,000 bytes and tells an app over that to send
  several; the margin covers the fallback text and usage footer riding on the
  same message. Measured here rather than left to Chat, because a rejected card
  costs a write against a per-space quota of one per second before the fallback
  can run. This, not `maxCardWidgets`, is the binding limit: widgets of up to
  3500 bytes reach 32 KB at around a dozen, and would need a quarter of a
  megabyte of answer to reach eighty. A card Chat rejects for size (413) now
  falls back to text like the 400 it already did.
- A turn that failed inside the daemon posted nothing. The daemon emits
  `turn-error` when a turn dies — a bad model name, a rejected credential, a
  rate limit — and nothing in switchboard parsed it, so the thread went quiet
  and the "⏳ Working…" placeholder sat there until something else retired it.
  The relay now surfaces the failure in the thread, carrying the daemon's own
  kind, code, message and hint rather than a generic apology, and taking the
  retry advice from the daemon's `retryable` flag instead of guessing from the
  kind. The two guardrail kinds (`cost_ceiling`, `watchdog`) are called out
  separately, because "try again" is the wrong advice for a turn the agent
  stopped on purpose and will keep refusing until an operator resets it — and
  since only the trip itself is classified as a guardrail, the refusals that
  follow it are recognised by the sentence the daemon puts in all three
  guardrail messages. The notice is separate from the turn rather than
  conditional on it, because a cost ceiling is enforced at the turn *boundary*:
  its `turn-error` arrives after the answer the turn already produced, and that
  is precisely the failure the reader most needs to hear about.
- A turn was silently abandoned when contact with the daemon was lost while it
  was running. This is the half `turn-error` cannot cover: the daemon that would
  report the failure is the thing that went away, so no event ever arrives and
  the relay has to notice the silence itself. After ~90 seconds with nothing at
  all on the stream and a turn still in flight, the thread is told once that
  contact was lost and the placeholder is retired; reconnects continue
  underneath, and an answer that does arrive later is still delivered. The
  grace is measured from the last event of any kind rather than the last
  answer, so neither a turn spent thinking nor a proxy cutting the SSE stream
  every few seconds is mistaken for a daemon that has gone away, and a rolling
  restart that finishes inside the window interrupts nobody.
- Every inbound message ran **two** agent turns and posted the answer twice.
  The router injected and then woke the session, but the daemon's inject
  already requests a wake as part of queueing the message, so the explicit
  wake started a second turn — with an empty prompt, since the inbox had just
  been drained. The router now injects only. This affected both adapters
  (`Router.Handle` is shared), so Slack duplicated replies too; it was easy to
  miss because the second turn's answer is often near-identical to the first.
  The real-daemon integration test now asserts one message yields one reply —
  the fake daemon in the unit tests replays a fixed script and cannot show it.
- Bumped the pinned Go toolchain to 1.26.6 (stdlib fixes for GO-2026-6089/-6090/
  -6091/-5972/-5026/-6218; `govulncheck` was failing the build against 1.26.5).
- Slack Block Kit rendering: bold/italic/strikethrough that wraps an inline code
  span (e.g. `` **Foo (`bar`)** ``) is now styled correctly instead of leaving the
  `**` delimiters literal — emphasis is resolved before code spans, treating code
  and links as opaque. A fenced code block nested under a list item is rendered as
  its own code block (dedented) rather than being flattened into the list item's
  text and mangled inline.
- Slack mention stripping now preserves newlines and indentation: an inbound
  turn's markdown block structure (headers, lists, tables, code fences) is
  newline-driven, and the previous strip collapsed all whitespace, flattening a
  multi-line message into a single unrenderable line.
- `daemon.CreateSession` now reads the daemon's `sessionID` (camelCase) response
  field; it previously looked for `session_id` and always failed against a live
  daemon. Sessions are now addressed by the app-qualified route
  (`/sessions/<app>/<id>/…`) to avoid the shortcut route's 409 on ambiguous ids,
  and `Subscribe` sends `since` + `protocol` query params.

### Added (scaffold)
- Initial scaffold: distroless multicall binary (`serve` / `version`).
- `pkg/daemon` — thin client for the frozen core-agent daemon contract
  (create / inject / wake / SSE stream), with `X-Asserted-Caller` per-turn
  attribution.
- `pkg/chat` — provider-neutral `Adapter` interface and normalized
  `Message` / `Reply` types.
- `docs/DESIGN.md` — switchboard chat-gateway design (W1 of the Hermes
  replacement epic).
- Contributor / agent guardrails ported from core-agent: `AGENTS.md` "How we
  develop", `CONTRIBUTING.md` (DCO, Conventional Commits, no attribution),
  `dev/ci/presubmits/*` + `dev/tools/ci`, the `review-gate` required CI check,
  and the opt-in `dev/claude/settings-review-gate.json` hook sample.
