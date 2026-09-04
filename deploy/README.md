# Deploying switchboard

Kustomize manifests for running switchboard in Kubernetes next to a
core-agent daemon. switchboard is **transport only** — it reads nothing
from the Kubernetes API and its only listening port is `:9090`
(`/healthz` + `/metrics`); it dials the core-agent daemon over pod
networking and a chat platform's API over the internet.

```
deploy/
  base/                 platform-neutral: SA, metrics-only NetworkPolicy, Deployment
  overlays/
    slack/              config.json + Slack token Secret
    googlechat/         config.json + Workload Identity
```

The base is **not applyable on its own** — pick a platform overlay.

## Where the settings live

Everything switchboard does is configured by **one JSON file per overlay**,
`overlays/<platform>/config.json`, mounted at `/etc/switchboard/config.json`.
The container's only argument is `--config` pointing at it.

That is deliberate, and it is the one rule to keep in mind when editing these
manifests: **precedence is flag > `$SWITCHBOARD_*` > file**, so a setting also
named in `args:` would outrank the file, and editing the ConfigMap would then
do nothing at all. Add settings to `config.json`, not to `args:`.

The ConfigMap is produced by a `configMapGenerator`, so its name carries a hash
of the content. switchboard reads the file once at startup; the hash is what
makes `kubectl apply -k` roll the Deployment when — and only when — the config
actually changed. A hand-written ConfigMap edited in place would leave the old
settings running until something unrelated restarted the pod.

Two things stay out of the file. **Credentials**, which ride env vars sourced
from Secrets — the file names the variable (`"slack_bot_token_env": "..."`) and
never holds the value, and switchboard refuses to start on a file that looks
like it does. And the **`metrics` containerPort**, which has to agree with
`metrics_addr` in the file; they are in two files now, which is the one seam
this arrangement introduces.

JSON has no comments, which the YAML args did. For a channel block there is a
`name` key that exists purely to say which room an ID is; nothing reads it.

### Per-channel settings

`approvals`, `approvers`, `progress_mode` and `show_usage` can be scoped to a
channel, over a `defaults` block that sets them process-wide:

```json
{
  "platform": "slack",
  "defaults": { "approvals": false, "progress_mode": "indicator" },
  "channels": {
    "C0SRE0000": {
      "name": "#sre",
      "approvals": true,
      "approvers": ["ana@example.com", "ben@example.com"]
    },
    "C0SCRATCH": { "name": "#scratch", "show_usage": true }
  }
}
```

Keyed by the **channel ID the platform reports** — a Slack `C0123ABCD`, a Chat
`spaces/AAAA` — never by name; a key that is not that shape is refused at
startup, because `"#sre"` would match nothing while reading like a room that
had been locked down. A channel's `approvers` **replaces** the wider list
rather than adding to it, so a block can widen a room as well as narrow one.
A `channels` block also outranks a flag, which is the one exception to the
precedence rule above.

## Prerequisites (not created by these manifests)

1. **Namespace** — everything targets `agent-triage` (change with
   `namespace:` in the *overlay's* `kustomization.yaml`, not the base's —
   see the comment there for why the base does not set it), the namespace
   core-agent runs in. switchboard reaches the daemon at
   `core-agent.agent-triage.svc.cluster.local:7777`; change `daemon_url`
   in `config.json` for a different Service or a remote daemon.

2. **core-agent bearer token Secret** (both platforms):

   ```sh
   kubectl -n agent-triage create secret generic switchboard-daemon-token \
     --from-literal=token='<the daemon bearer token>'
   ```

3. **Platform credentials** — see the per-platform sections below.

Secrets are always referenced, never committed: switchboard takes tokens
only through env vars sourced from Secrets, never as flags (AGENTS.md).

## Slack

Socket Mode is a pure outbound WebSocket — no cloud IAM, so the base
ServiceAccount is used unchanged. Create the token Secret, then apply:

```sh
kubectl -n agent-triage create secret generic switchboard-slack \
  --from-literal=app-token='xapp-...' \
  --from-literal=bot-token='xoxb-...'

kubectl apply -k deploy/overlays/slack
# or, without cloning:
kubectl apply -k "github.com/go-steer/switchboard/deploy/overlays/slack?ref=main"
```

## Google Chat

Ingress (Pub/Sub subscribe) and egress (Chat REST, bot scope) both
authenticate via **Application Default Credentials**, resolved through
Workload Identity — no JSON key is mounted.

1. A Google service account (GSA) with `roles/pubsub.subscriber` on the
   events subscription, configured as the Chat app's identity.
2. Bind the KSA to it:

   ```sh
   gcloud iam service-accounts add-iam-policy-binding \
     switchboard@PROJECT_ID.iam.gserviceaccount.com \
     --role roles/iam.workloadIdentityUser \
     --member "serviceAccount:PROJECT_ID.svc.id.goog[agent-triage/switchboard]"
   ```

3. In the Chat API console, set **Connection settings** to *Cloud Pub/Sub*
   pointed at your topic, and add the app commands you want (each gets a
   numeric id). Either interaction framework works — switchboard detects the
   Chat-API and Workspace add-on event dialects per event — so converting the
   app to add-on mode does not have to be coordinated with a deploy.
4. Edit `overlays/googlechat/patch-serviceaccount.yaml` (GSA email) and
   `overlays/googlechat/config.json` (`google_project`,
   `google_subscription`, and the `googlechat_commands` id mapping), then:

   ```sh
   kubectl apply -k deploy/overlays/googlechat
   ```

## Image pinning

The base pins `ghcr.io/go-steer/switchboard:main` (the floating tag
published on every merge). For anything beyond staging, override it with
a released, cosign-verifiable tag via the overlay's `images:` field —
`newTag: 0.4.0` for the current release — the image tags carry no `v`,
because `release-images.yml` builds them with metadata-action's
`{{version}}`, which strips it. Verify the signature with:

```sh
cosign verify ghcr.io/go-steer/switchboard:0.4.0 \
  --certificate-identity-regexp '^https://github.com/go-steer/switchboard' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Notes

- **One replica.** A conversation maps to a core-agent session held in an
  in-memory map; a second replica would split the map and double-consume
  the platform stream. `strategy: Recreate` avoids overlap on rollout.
  Multi-replica waits on a durable session map (DESIGN.md).
- **Health + metrics.** `"metrics_addr": ":9090"` serves `/healthz` (backing
  the liveness + readiness probes) and `/metrics` (Prometheus) on the
  named `metrics` port. That port is switchboard's only inbound surface;
  the NetworkPolicy admits it (kubelet probes bypass NetworkPolicy) and
  denies everything else. Narrow the ingress `from` to your monitoring
  namespace if Prometheus runs in-cluster. `serve` still exits non-zero on
  a fatal error (bad token, adapter failure, metrics bind failure) so the
  container also restarts on real failures.
- **Progress mode** is `defaults.progress_mode: "indicator"` in each
  overlay's `config.json`; set it per channel in a `channels` block, or at
  runtime via the `progress` chat command. A command that contradicts a
  channel's configured mode logs the divergence, so the file stays the
  account of record.
- **Outbound ingress** (letting another in-cluster service post into a
  conversation) is **off** in these manifests. Enabling it means opening a
  second inbound surface, so it takes four deliberate steps in your own
  overlay:

  1. A Secret with the caller's bearer token — separate from the daemon
     token — and an env var sourcing it:

     ```sh
     kubectl -n agent-triage create secret generic switchboard-ingress-token \
       --from-literal=token="$(openssl rand -hex 32)"
     ```

  2. Three keys in `config.json` — `ingress_allow` is a real list here,
     rather than the comma-separated string the flag had to flatten it
     into:

     ```json
     "ingress_addr": ":8080",
     "ingress_token_env": "SWITCHBOARD_INGRESS_TOKEN",
     "ingress_allow": ["C0123ABCD"]
     ```

     plus the env var and the port, which stay in the Deployment because
     one is a Secret and the other is a `containerPort`:

     ```yaml
     env:
       - name: SWITCHBOARD_INGRESS_TOKEN
         valueFrom:
           secretKeyRef: {name: switchboard-ingress-token, key: token}
     ports:
       - {name: ingress, containerPort: 8080, protocol: TCP}
     ```

  3. A NetworkPolicy ingress rule admitting **only** the calling workload —
     unlike the metrics port, this one should have a `from`:

     ```yaml
     - ports: [{port: ingress, protocol: TCP}]
       from:
         - podSelector:
             matchLabels: {app.kubernetes.io/name: core-sre-agent}
     ```

  4. A Service, since there is none today, so callers have a name to dial:
     `http://switchboard.agent-triage.svc.cluster.local:8080/v1/messages`.

  Leave `ingress_allow` unset only if callers really may post anywhere the
  bot is a member; `serve` logs a warning when it is empty. Treat the allowlist
  as the authorization model: a caller holding the token can edit *any* message
  the bot posted in those conversations, including the router's own replies.

  Two consequences for scaling: the ingress keeps the idempotency and
  append-text maps **in this pod's memory**, so `append` and `Idempotency-Key`
  only work against the replica that took the original POST. With the
  single-replica Deployment in the base that is a non-issue; if you add
  replicas, either front the ingress with a session-affinity Service or have
  callers fall back to full `text` on the `409`.

- **Outbound-only Deployments.** If a workload only posts, set
  `"outbound_only": true` in its `config.json` — **together with the outbound
  ingress above**, which is then the only way in. Without `ingress_addr` the
  setting is refused at startup and the pod crash-loops, since a Deployment
  that can neither receive nor be asked to post has nothing to do. With it,
  switchboard runs egress-only: no Socket Mode WebSocket on Slack, no
  Pub/Sub client on Chat.

  The inbound credential then goes unused, so drop it too — `app-token` from
  the `switchboard-slack` Secret, the env var sourcing it, and
  `slack_app_token_env` from the file; or `google_project` /
  `google_subscription` from the Chat overlay's `config.json`, in which case
  the GSA needs no `roles/pubsub.subscriber` and no topic or subscription has
  to exist. The daemon token goes the same way: an outbound-only pod runs no
  turn and never reads `SWITCHBOARD_DAEMON_TOKEN`, so remove `token_env` from
  the file, the `env` entry in `base/51-deployment.yaml` that sources it, and
  the `switchboard-daemon-token` Secret — the entry is a required
  `secretKeyRef`, and leaving it pointed at a Secret you deleted parks the pod
  in `CreateContainerConfigError` before switchboard ever runs.

  What stays on Chat is everything egress needs: it must still be the GSA
  configured as the Chat app's identity, and the Workload Identity annotation
  in `patch-serviceaccount.yaml` stays, because egress authenticates through
  ADC and switchboard builds the Chat client at startup.

  Only the setting selects the mode — an emptied Secret does not. A bridged
  pod whose app-token key goes missing crash-loops instead of coming back as a
  process that posts, passes `/healthz` and answers nobody, which is the one
  degradation a probe cannot see. Same reason for the other refusal: on Chat
  without `outbound_only` the two Pub/Sub keys go together, and one alone is
  rejected. The pod logs `outbound-only: posting to …, receiving nothing` on
  start.

  One thing the mode gives up: a Slack bridge calls `auth.test` when it opens
  the socket, and an outbound-only pod never does, so a bad `xoxb-` token is
  not caught at startup — every ingress POST answers `502` instead. The
  caller is a program and sees that immediately, but the pod itself stays
  `Ready`.
