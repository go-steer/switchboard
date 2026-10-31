# The ax backend — switchboard starts agents as ax tasks

**Status:** proposal (2026-10-21) · **Tracks:** #168 (X1 #169, X2 #170, X3 #171,
X4 #172, X5 #173; X6 go-steer/shipmind#39) · **Builds on:** multi-agent (#140, `docs/multi-agent.md`), the runtime
registry (#156) · **Wanted by:** shipmind Milestone 3 ("switchboard starts
agents", go-steer/shipmind `docs/DESIGN.md`) · **Target:** v0.9

## The requirement

Today every agent behind switchboard is a daemon someone already runs, at a
fixed `daemon_url`. On shipmind's substrate profile an agent is an **ax task**:
a sandboxed Agent Substrate actor that suspends when idle and wakes on the
next request. shipmind's Milestone 3 asks for:

- **Picking an agent in Chat starts an ax task for that thread**, or reuses
  the caller's **personal agent**, a task per owner.
- **The task is woken through Substrate's router**, so a thread nobody has
  touched for a week costs nothing until the next message.

k8s-lookout's ax sink already does the same for incidents
(`internal/axsink`, `docs/ax-sink-design.md`). This design follows its
shape where it can.

## What ax and mast provide

These are the facts the design rests on, measured at shipmind's pins
(ax-server `685b160`, ax-runner `e8bc98d`, mast `922e549`). _Since #182:_
`internal/axapi` is the fork rebased onto upstream ax for Agent Substrate
v1.0 (`substrate-v1-12dbe16`); what changed is in "The rebased fork" below.

**ax-server** is gRPC only (h2c on `:8080`; `pkg/apis/v1alpha1/ax.proto`):
`CreateTask`, `GetTask`, `ListTasks`, `DeleteTask`, `SuspendTask`,
`ResumeTask` (and `WatchTask`, gone after the rebase).
- **Immutable tasks.** Re-creating a name returns `FailedPrecondition`. There
  is no update.
- **No labels, annotations or owner** on a task. Names are RFC 1123 labels
  of at most 63 characters. The mapping to a thread has to live in the name
  or in switchboard's state.
- **No template or prompt field.** A task is `image`, `command`, `env`,
  `workspaces`, `egress`, `http`, `idle`. Instructions travel as flags, env or
  workspace files. (After the rebase, one `workspace`, and a free-form
  `config` the runner reads.)
- **Nothing expires.** A task lives until it is deleted.
- **No authentication.** No interceptors on the server, and the CLI dials
  insecure. Protection is the network.

**A task serves its own API** (`spec.http.port`, ax 587e58b; both shipmind pins
carry it).
- **Pass-through:** the runner on port 80 forwards everything except
  `/healthz`, `/readyz` and `/metadata/...` to the task's server, SSE
  included.
- **Readiness:** `/readyz` holds at 503 until that server listens (b52d147),
  so a request that wakes a task waits instead of failing.

**The atenet router** (`atenet-router.ate-system:80`):
- **Routing:** it routes on one header, `ate-target-actor: <atespace>/<task>`.
- **Waking:** it resumes a suspended actor before proxying.
- **Plaintext, no auth** in shipmind's deployment. _Since Agent Substrate
  v1.0 (#181):_ TLS only on 443, admitting a podidentity pod certificate whose
  SPIFFE ID is allowlisted; see "Identity" below.
- **Route timeout:** 5 minutes, which also cuts an SSE stream.
- **Parking budget:** 5s by default for a waking request; shipmind raises it
  to 60s.

**Idle suspend** (`spec.idle`):
- **When:** a task is suspended once nothing has passed through for
  `suspendAfter`.
- **An open SSE stream counts as activity.**

**mast in a task** (#528–#531, shipmind's `agents/sre-triage/ax/task.yaml.tmpl`):
- **Serving:** `mast --attach-listen=127.0.0.1:8484` with
  `spec.http.port: 8484` serves the contract switchboard speaks.
- **Bearer:** `MAST_ATTACH_TOKEN`, carried in the task's env.
- **Sessions:** many per process, in a session DB that survives suspend.
- **Single-user:** no owner, no ACL, and `X-Asserted-Caller` is not
  honoured with token auth.

### The rebased fork (#182)

The fork was rebased onto upstream ax (`f0228f1`) for Agent Substrate v1.0.
switchboard carries the proto at `substrate-v1-12dbe16`:

- **Field numbers.** The fork's own TaskSpec fields moved to 100 and up:
  `egress` 100, `http` 101, `idle` 102, `on_completion` 103. Upstream took 7
  for `config`, where the fork had `egress`. A client with the old copy would
  send `http` under a number the server reads as something else, so the copy
  and the server move together. The YAML keys didn't change.
  `pkg/ax/wire_test.go` pins the numbers.
- **One workspace.** `spec.workspaces` (a list) became `spec.workspace` (one
  binding, the same field number). A template still using `workspaces:` is
  refused at startup.
- **Status is read live from Substrate.** Phase `Unknown`, with Ready reason
  `SubstrateUnavailable`, means ax couldn't reach Substrate. `Ensure` then
  fails with `ax.ErrSubstrateUnavailable` and ax's reason rather than
  attempting a resume, which needs Substrate too. `Failed` with
  `ActorNotFound` means the task's actor is gone; `Ensure` resumes it, which
  re-creates the actor.
- **Idle suspension** is recorded in an ax-owned `AutoSuspended` condition
  (`IdleSuspended`, `CompletedSuspended`; `False` with `ResumedByRequest`
  after a wake), not in a Ready reason. switchboard reads neither: it knows a
  task idles from its template.
- **Validation.** `checkSpec` mirrors the fork's `validateTaskHTTP` and
  `validateTaskIdle`: a port in 1–65535 that is neither the runner's (80) nor
  its metadata port (8099), `suspendAfter` of at least 10s, a `busyPath`
  starting with `/`, and `onCompletion` of `Keep` or `Suspend`. It also
  requires `spec.http.port`, which ax doesn't.

## The decision

**An ax agent is a task template, not a daemon. Each conversation's task is
named, created on demand, and reached through the router. The contract
between switchboard and the agent does not change.**

### Agent entries

An agent declares `kind: "ax"` and an `ax` block instead of a `daemon_url`:

```json
{"agents": [
  {"name": "triage", "display_name": "SRE triage", "kind": "ax",
   "token_env": "SWITCHBOARD_AGENT_TOKEN_TRIAGE",
   "ax": {
     "server": "ax-server.ax-system.svc.cluster.local:8080",
     "router": "https://atenet-router.ate-system.svc.cluster.local",
     "atespace": "default",
     "task_template_file": "/etc/switchboard/ax/triage.yaml",
     "scope": "thread"
   }}
]}
```

- **`task_template_file`:** a full ax Task, as lookout's `--ax-task-template`
  takes it, decoded strictly at startup.
  - switchboard sets only `metadata.name` and `metadata.atespace`.
  - The template must set `spec.http.port`, or nothing can be reached.
  - It should set `spec.idle`.
  - It is a file, not inline config, because it carries the agent's token in
    `env`; shipmind renders it into a Secret.
- **`token_env`:** the agent's bearer, the same value as the template's
  `MAST_ATTACH_TOKEN`. Runtime ax agents follow the
  `--admin-token-env-prefix` rule like any other.
- **`scope`:**
  - `thread`: a task per conversation; the default.
  - `owner`: a task per caller, their personal agent.
  - `shared`: one task for everyone, as lookout's cluster scope.

### Naming

A task's name is how switchboard finds it again, since a task has no labels.
It's deterministic, so a restart, or a second gateway pod (#138), arrives at
the same task:

- `thread`: `sb-<agent>-<hash16(gateway, conversation)>`
- `owner`: `sb-<agent>-u-<hash16(caller)>`
- `shared`: `sb-<agent>`

`gateway` is a stable gateway ID (new setting, default the platform), so the
Slack and Chat gateways in one cluster never collide. Names are clamped to 63
characters.

### Starting and reaching a task

This happens in `sessionAs`, before the session is created, following lookout's
`startTask`:

1. **`CreateTask`** from the template with the derived name. On
   `FailedPrecondition` the task exists: `GetTask`, and reuse it.
2. **`ResumeTask`** if it is suspended. The router would also wake it on the
   first request, but resuming first overlaps the wake with step 3.
3. **Wait for `GET /readyz`** through the router (a bounded budget, default
   2m). The thread sees the existing progress placeholder meanwhile.
4. **The contract, unchanged.**
   - The agent's `daemon.Client` has `BaseURL` set to the router, and an HTTP
     transport that adds `ate-target-actor`.
   - `POST /sessions`, inject, wake and SSE then work as for any daemon, and
     so does the approval client.
   - `pkg/daemon` stays the thin contract client: the header is the
     transport's business.

The task name joins the session record (`state.go`), so a restored thread
goes back to its task. A record without one belongs to a non-ax agent, as
today.

### Streams scoped to the turn

_As built (#171):_ switchboard's existing release-and-revive path does this.
- **When:** an ax conversation is released, closing both streams,
  `ax.stream_linger` (default 1m, at least 10s) after its last traffic.
- **Only if nothing is going on:**
  - no turn is running;
  - no answer is pending;
  - the daemon's last status-update wasn't working or parked;
  - no turn is being set up;
  - no approval question is unanswered.

  switchboard's own idea of a turn can read idle while the daemon still works
  (after a reconnect, narration is taken as the answer), so the daemon's word
  counts too.
- **Revival:** the next message revives the conversation, which wakes the task
  and resumes the stream from its cursor.
- **Suspension timing:** ax's idle clock starts when the stream closes, so a
  task suspends about linger plus `suspendAfter` after its last turn.
- **Not every template:** a template without `spec.idle` isn't swept.
- **The sweep** runs every 10s, with or without `--state-dir`.

switchboard holds each conversation's event stream open between turns, and its
approval stream too, until `--session-idle-ttl` (12h by default). An open
stream is activity to ax, so **a task behind switchboard would never
suspend**, which defeats the point of ax. For ax agents:

- **Event stream:** open at inject, and close once the turn has ended and the
  usage footer has landed. Reopen at the next turn from the stored cursor
  (`?since`), which the relay already does on reconnect.
- **Approval stream:** opened with the turn, closed with it. Prompts arise
  during turns.
- **Router cut:** its 5-minute cut is a reconnect for a long turn, which the
  relay handles today. The design's tests have to cover it, because now it
  will happen.

What is lost: output the agent produces *between* turns, which nothing in
the contract sends today. The doc records it so the trade is deliberate.

### Identity

_As built (#172):_ an owner-scope conversation answers its owner only. A
message from anyone else in that thread gets a notice and is not sent: mast in
a task is single-user, and the personal agent holds its owner's credentials
and history.

- **mast in a task is single-user.** The task boundary is the isolation.
  - `thread` scope: one conversation's sandbox, credentials and history.
  - `owner` scope: one person's.
  - `shared` scope: everyone shares a sandbox, so the agent kind should be
    allowed only where that is acceptable; startup warns.
- **`X-Asserted-Caller` is still sent,** for mast's audit and for the day it
  honours a proxy. The owner of an `owner`-scope task is in its name, and
  switchboard's audit records both.
- **ax-server is unauthenticated.** switchboard reaches it in-cluster in
  plaintext, as lookout does; a NetworkPolicy is the protection.
- **The router takes mTLS only** from Agent Substrate v1.0, which removed its
  plaintext listener. _As built (#181):_
  - **Client:** switchboard presents its pod certificate, from a
    `podCertificate` projection (signer `podidentity.podcert.ate.dev/identity`,
    `credentialBundlePath`: a PKCS#8 key, then the chain). Its identity is
    `spiffe://cluster.local/ns/<namespace>/sa/<serviceaccount>`, which has to
    be on ate-setup's `--ingress-allowed-spiffe-ids`; the router serves nothing
    to an identity not listed.
  - **Server:** the router's certificate comes from the servicedns signer and
    carries only `atenet-router.ate-system.svc`. switchboard verifies it
    against a `clusterTrustBundle` projection of that signer
    (`podcert.ate.dev/canarying: live`) and that name, whatever address it
    dials. The servicedns CA certifies every Service-selected pod for its own
    name, so the name check is what pins the router.
  - **Rotation:** purser v0.5.0's PKI client profile does both halves.
    `mtls.CertificateFileSource` re-reads the bundle, and
    `mtls.CertPoolFileSource` re-reads the trust bundle, consulted per
    handshake (`RootCAsSource`). A pod certificate lives 24h at most and a
    restart is never needed.
  - **Settings:** `ax.router` (default the https in-cluster address),
    `ax.router_credential_bundle_file`, `ax.router_trust_bundle_file` and
    `ax.router_server_name`, each defaulting to what
    `deploy/components/ax-router-mtls` mounts. One client per set of files
    serves every agent, so they share connections.
  - **An `http://` router** is still reached in plaintext, for development
    and tests.
  - **Not `auth`:** an agent's `auth` block (`docs/agent-identity.md`) names
    how switchboard reaches a *daemon*. The router authenticates the pod, not
    the gateway's configured identity, so it has settings of its own.

### Lifecycle

_As built (#173):_
- **The sweep:** once a minute, it deletes the task of a dormant thread-scope
  conversation quiet past `delete_after`.
  - `delete_after` is capped a day under the 30-day record lifetime, so the
    sweep sees a task before its record is pruned. A delete that keeps failing
    for that day still orphans the task, which the admin API can remove.
  - A per-task gate, plus a re-check under the lock, stops a delete racing a
    conversation that is waking its task. A start waits for any delete in
    flight.
- **The record** is kept, marked with why, so the next message starts over and
  says so.
- **Drifted thread tasks** are found before they are woken, then deleted and
  re-created.
- **The admin API** lists an agent's tasks by name pattern, since ax keeps no
  labels, and deletes only names of that pattern.
  - A task this gateway has no record of may be another gateway's, so deleting
    it takes `force`.

ax deletes nothing on its own, and lookout leaves deletion open (#567).
switchboard owns its tasks:

- **Suspend** is ax's job (`spec.idle`). switchboard never suspends.
- **Delete, `thread` scope:** delete once the conversation has had no traffic
  for `ax.delete_after` (default 14 days; `0` keeps tasks).
  - A later message in that thread gets a fresh task and session, and is told
    so, the way a lost session is today.
  - Driven by the state records, since switchboard is what knows a thread
    went quiet.
- **Delete, `owner` and `shared` scope:** never automatic. An admin API
  `DELETE /v1/agents/{name}/tasks/{task}` and a listing
  (`GET /v1/agents/{name}/tasks`, from switchboard's records) cover the rest.
- **Removing an ax agent** deletes none of its tasks; the admin decides.
- **A template change reaches only new tasks.** Tasks are immutable, so a
  task created before a change keeps its old spec, including an old token,
  which then fails every request with 401.
  - `pkg/ax` reports such a task as drifted when it reuses it, and
    switchboard logs it.
  - Replacing drifted thread tasks (delete, then create) belongs with the
    lifecycle work (X5).

### What stays the same

- **The chat adapters and the provider seam** are untouched.
- **The router's routing,** the picker, attribution, the admin API's agent
  CRUD, and the registry's pinning and validation all treat an ax agent like
  any other.
- **The daemon contract** and `pkg/daemon`'s four verbs.

### New dependency

- **The ax API client:** ax-server's gRPC client from
  `github.com/mastersingh24/ax/pkg/apis/v1alpha1`, pinned to the commit
  shipmind runs, in a new `pkg/ax` package. It's the only code that talks to
  ax-server.
- **The static binary is unaffected:** grpc is already a dependency, and it's
  pure Go.

## Plan and dependencies

| # | work | depends on |
|---|---|---|
| X1 | `pkg/ax`: the ax-server client (create-or-get, resume, readiness through the router, delete), tested against a fake gRPC server | — |
| X2 | `kind: "ax"` agents, `thread` scope: template loading, naming, task start in `sessionAs`, the router transport, the task in state records | X1 |
| X3 | Turn-scoped streams for ax agents, including the 5-minute router cut | X2 |
| X4 | `owner` and `shared` scopes | X2 |
| X5 | Lifecycle: `delete_after` for thread tasks, the admin API task listing and delete | X2 |
| X6 | shipmind: switchboard fronting ax agents in the substrate profile, end to end (go-steer/shipmind, Milestone 3) | X2, X3 |

X2 with X3 is the useful minimum: a thread starts a task, and the task can
suspend. X4 and X5 can follow within v0.9 or after.

## Open questions

- **Which agent a thread gets by default** on the substrate profile, and
  whether non-ax agents sit beside ax ones there.
- **The wake budget.** A cold wake after a long suspend can exceed the
  router's parking budget. shipmind raises it to 60s; should switchboard
  require it, or keep its own readiness wait as the only guard?
- **Snapshot scope for personal agents.** `full` snapshots restore only on the
  same CPU platform and grow with the task. mast advises data-only snapshots
  for long-lived agents, which is a template decision, but the owner scope
  makes it common.
- **The thread a fresh task replaces.** After `delete_after`, the old session
  is gone with the task. Is "start over in this thread" right, or should the
  thread be closed?
