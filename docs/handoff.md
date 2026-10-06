# Zero-downtime gateway restarts — handing off between pods

**Status:** proposal (2026-10-06) · **Tracks:** #138 · **Builds on:** durable
state (#86/#87), the GKE deployment (#131)

## The problem

A gateway restart — a rollout, a node drain, an eviction — loses Google Chat
messages.

The gateways run `strategy: Recreate`, because their state (`--state-dir`, one
JSON snapshot of every conversation's session and delivery watermarks) sits on a
ReadWriteOnce volume with one writer. The old pod stops, then the new one
starts, and until the load balancer has registered the new pod it answers `503`.
On the GKE deployment that gap is about 20 seconds on every rollout.

**Google Chat does not redeliver.** Measured on 2026-10-06: with the Chat
gateway scaled to zero, a DM showed "switchboard not responding" in Chat, and
in the 5½ minutes after the gateway returned no request arrived and no turn
ran. The person has to notice and resend.

Slack is less exposed: Socket Mode holds events for an app that is
reconnecting. Chat over Pub/Sub is buffered by the subscription. But pods come
and go regardless of platform, so the fix belongs in the design, not in tuning
the gap down.

## The rule that does not change

**Exactly one process owns the conversations at any moment.** The owner runs
each conversation's relay (the daemon stream whose answers it posts) and prompt
watcher, and writes the state. Two owners would post every answer twice. So
the gap cannot be closed by running two full gateways side by side. It closes
by letting a second pod *receive* before it *owns*.

## Roles

| role | receives events | runs relays and watchers | writes state |
|---|---|---|---|
| **owner** | yes, handled | yes | yes |
| **standby** | yes, **held** in a bounded queue | no | no |

A process starts as a standby and becomes the owner when it acquires the lock.
With one replica and no rollout in progress, that happens at once, and nothing
changes from today.

## The handoff

During a rollout (`RollingUpdate`, `maxSurge: 1`, `maxUnavailable: 0`):

1. **The new pod starts as a standby.** It is Ready (its HTTP endpoint answers)
   but owns nothing. Kubernetes counts it available only once the load
   balancer routes to it. GKE injects the
   `cloud.google.com/load-balancer-neg-ready` readiness gate for NEG-backed
   Services, and the deployed Chat pod already carries it. This step is what
   removes today's `503`.
2. **Kubernetes stops the old pod.** On `SIGTERM` the old owner reports
   not-ready, so the load balancer stops sending it new traffic. For a drain
   window it keeps handling what still arrives, as owner, so nothing it
   accepts is held.
3. **The old owner steps down.** It stops its relays and prompt watchers,
   writes a final snapshot synchronously (bypassing the 500 ms debounce), and
   releases the lock.
4. **The standby takes over.** It acquires the lock and loads the state. It
   re-attaches the conversations that were active, resuming each relay from its
   `Relayed` watermark and each watcher with its `Asked` records. Then it
   processes the held events in arrival order.

Between steps 3 and 4 nobody processes, for well under a second, and events in
that window are held, not refused.

A turn in flight is unaffected: it runs on the daemon. Its answer is posted by
the new owner, resumed from the watermark, and the existing effectively-once
delivery (the `Attempted` watermark plus platform request ids, #99) keeps it
from appearing twice.

## Shared state and the lock

The ReadWriteOnce volume cannot be mounted by both pods, so the state moves to
something both can reach. The choice is **Kubernetes-native**:

- **The lock** is a `coordination.k8s.io` `Lease`, the standard leader-election
  object. The holder renews it every few seconds. A lease not renewed for its
  duration (around 15 s) can be taken.
- **The state** is a `Secret` holding the same JSON snapshot (`routerState`, at
  `stateVersion` as today). It is a Secret, not a ConfigMap, because the
  snapshot names who spoke in every conversation. The snapshot is small
  (`dormantMaxAge` prunes it) and far below a Secret's 1 MiB limit.
- **Writes are conditional and fenced.** Every write is an update conditioned on
  the `resourceVersion` just read. The snapshot also records the lease holder
  and lease transitions it was written under. A pod whose lease has been taken
  finds its next write refused, so it can never overwrite the new owner's
  state.
- **It sits behind the existing interface.** It is a second implementation of
  `stateStore` (`load`, `save`, `where`), plus the lease. `--state-dir` (a file)
  stays for the laptop rig and single-pod deployments, with the lock reduced to
  a no-op there.

Both objects live in the gateway's own namespace. Each gateway gets its own
pair (the Chat and Slack gateways are separate deployments), and RBAC lets its
ServiceAccount `get`, `update` and `create` exactly those two names.

*Considered: a GCS object with generation-match preconditions.* It is more
portable outside Kubernetes but adds a GCP dependency and credentials, and the
lease would still need its own mechanism. It can be added later behind the same
interface.

## The held-event queue

- **Bounded** in count (a few hundred) and in time held. An event a standby
  never gets to own — no owner appears within the bound — is refused with the
  `503` shutdown already uses (`refuseDraining`). That is no worse than today.
- **Ordered**, and processed by the new owner exactly as if it had arrived
  then.
- **Clicks.** A button press held by a standby is answered in the HTTP response
  if ownership arrives within the click budget, and otherwise its card edit
  goes over REST — the fallback `answerClick` already has.

## Other cases

| case | behaviour |
|---|---|
| **Owner crashes** (no clean step-down) | The lease expires and a standby, if one is running, takes over, processing what it held. Without a standby, it is today's behaviour. |
| **Slack** | A standby does not open its Socket Mode connection until it owns: two connections split the app's events between them. Slack holds events while the app reconnects. |
| **Chat over Pub/Sub** | A standby does not pull until it owns. The subscription buffers. |
| **Placeholder clock** | A revived entry currently deletes the ⏳ placeholder left by a turn in flight, because its ticker died with the process. Across a clean handoff the new owner resumes the ticker instead, so a mid-turn rollout is invisible. After a crash it keeps today's behaviour. |
| **Outbound ingress** (`--ingress-addr`) | Same rule. The ingress listener holds requests while standby, and the bindings travel in the snapshot as today. |

## Deployment changes

- `strategy: RollingUpdate` with `maxSurge: 1`, `maxUnavailable: 0`, replacing
  `Recreate`.
- A `preStop` delay and `terminationGracePeriodSeconds` long enough for the
  drain window and the step-down.
- RBAC: a Role granting the gateway's ServiceAccount access to its Lease and
  Secret, plus a RoleBinding.
- The state PVC is dropped, and with it `components/durable-state` for this
  mode. The file-backed component stays for single-pod use.
- A config setting selects the store: the file under `state_dir`, or the
  Kubernetes objects by name.

## Testing

- **Unit:** the store's conditional writes; a fenced write refused after the
  lease moves; lease acquire, renew and expiry; the held-event queue's bounds
  and order.
- **In-process:** two routers sharing a fake store, handing off in the middle
  of a turn. The answer is posted exactly once, an event the standby received
  is handled, and an open approval's buttons keep working.
- **Live, on the GKE deployment:** a rollout during a turn, plus a Chat message
  sent mid-rollout. Nothing shows "not responding", and the turn's answer
  arrives once.

## Delivery

In reviewable steps, each with its own review gate: the store and lease; the
standby role, queue and handoff; the deployment change and the live test.

## If one process is not enough

Not built here, but the design above is meant to grow into it, so the path is
written down.

### When one process is actually the limit

Later than it looks. A gateway is almost all network waiting: each active
conversation costs a couple of idle daemon streams and a few goroutines, so one
process should carry thousands of active conversations. That is an estimate,
not a measured load test. The ceilings that bind first are elsewhere:

- **Platform rate limits** are per app or per space, not per process. More pods
  do not raise them (#107: Chat's per-space write quota).
- **The daemon.** One core-agent daemon serves every session. If it is the
  bottleneck, the answer is more daemons, with switchboard routing
  conversations across them, not more gateways.
- **Availability.** One owner is still one point of failure for the time
  between a crash and its lease expiring.

So the real triggers are a very large fleet, several daemons, or availability
requirements beyond what a handoff gives.

### Stage 1: static sharding by deployment

No code. Run separate gateway deployments for separate sets of workspaces,
spaces or channels, each with its own Chat or Slack app or its own `channels`
configuration. Each is a single-owner gateway with this design's handoff. It is
crude, but it is enough when the load splits along organisational lines.

### Stage 2: sharded ownership (active-active)

The general answer. **One owner per conversation still holds**, so ordering and
effectively-once delivery work unchanged. What changes is how many owners there
are.

- **Shards and leases.** Conversations hash into a fixed number of shards (say
  64), each with its own Lease. Every pod claims a fair share of shards and runs
  relays and watchers only for the conversations in shards it owns.
- **Events are forwarded to the owner.** This is the new part. The platforms do
  not route by conversation: Chat's load balancer picks any pod, and Slack
  spreads Socket Mode events across an app's connections at random. The pod
  that receives an event forwards it over the cluster network to the owner of
  that conversation's shard, found from the leases.
- **Rebalancing is this design's handoff, per shard.** When a pod joins or
  leaves, shards move. Each move is release, final write, acquire, resume, and
  events for a shard in motion are held, as above.
- **Storage moves to per-conversation records.** One Secret snapshot does not
  scale to this. The state becomes records in a real store (Firestore, Cloud
  SQL or Redis) behind the same `stateStore` interface. If core-agent#995 lands
  (the daemon accepting a caller-supplied session key), much of this state can
  be recovered from the daemon instead of kept here.

### Stage 3: several daemons

The shard map can also say which daemon a conversation lives on. That covers
the case where the daemon, not the gateway, is the bottleneck.

### Seams to build now

The single-owner design is stage 2 with one shard. Three choices in the first
implementation keep the upgrade from being a rewrite:

1. **The lock is asked per conversation.** The interface takes a key ("who
   owns conversation X?"). Today every key resolves to the one global Lease.
2. **The state store keeps a per-conversation shape at its interface,** even
   though the first implementation writes one snapshot.
3. **The held-event queue is "deliver to the owner",** not "wait until I own".
   Today the owner is always this process once it holds the lock. In stage 2
   the same path forwards to another pod.

## Out of scope

Building any of the stages above. This design delivers the single-owner handoff
with the three seams, and nothing more.
