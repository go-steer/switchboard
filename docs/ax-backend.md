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
(ax-server `685b160`, ax-runner `e8bc98d`, mast `922e549`).

**ax-server** is gRPC only (h2c on `:8080`; `pkg/apis/v1alpha1/ax.proto`):
`CreateTask`, `GetTask`, `ListTasks`, `DeleteTask`, `SuspendTask`,
`ResumeTask`, `WatchTasks`.
- **Immutable tasks.** Re-creating a name returns `FailedPrecondition`. There
  is no update.
- **No labels, annotations or owner** on a task. Names are RFC 1123 labels
  of at most 63 characters. The mapping to a thread has to live in the name
  or in switchboard's state.
- **No template or prompt field.** A task is `image`, `command`, `env`,
  `workspaces`, `egress`, `http`, `idle`. Instructions travel as flags, env or
  workspace files.
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
- **Plaintext, no auth** in shipmind's deployment.
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
     "router": "http://atenet-router.ate-system.svc.cluster.local",
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

- **mast in a task is single-user.** The task boundary is the isolation.
  - `thread` scope: one conversation's sandbox, credentials and history.
  - `owner` scope: one person's.
  - `shared` scope: everyone shares a sandbox, so the agent kind should be
    allowed only where that is acceptable; startup warns.
- **`X-Asserted-Caller` is still sent,** for mast's audit and for the day it
  honours a proxy. The owner of an `owner`-scope task is in its name, and
  switchboard's audit records both.
- **ax-server and the router are unauthenticated.** switchboard reaches both
  in-cluster in plaintext, as lookout does; a NetworkPolicy is the
  protection.
  - The purser identity of `docs/agent-identity.md` cannot apply until ax and
    Substrate accept mTLS. Substrate's `static-mtls` ingress mode currently
    breaks ax-server's own path.
  - When they do, an ax agent gains an `auth` block like any other.

### Lifecycle

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
