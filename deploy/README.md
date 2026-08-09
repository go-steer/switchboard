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
    slack/              --platform=slack + Slack token Secret
    googlechat/         --platform=googlechat + Workload Identity
```

The base is **not applyable on its own** — pick a platform overlay.

## Prerequisites (not created by these manifests)

1. **Namespace** — everything targets `agent-triage` (change with
   `namespace:` in `base/kustomization.yaml`), the namespace core-agent
   runs in. switchboard reaches the daemon at
   `core-agent.agent-triage.svc.cluster.local:7777`; override
   `--daemon-url` for a different Service or a remote daemon.

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

3. Edit `overlays/googlechat/patch-serviceaccount.yaml` (GSA email) and
   `overlays/googlechat/patch-deployment.yaml` (`PROJECT_ID`,
   `SUBSCRIPTION_ID`), then:

   ```sh
   kubectl apply -k deploy/overlays/googlechat
   ```

## Image pinning

The base pins `ghcr.io/go-steer/switchboard:main` (the floating tag
published on every merge). For anything beyond staging, override it with
a released, cosign-verifiable tag via the overlay's `images:` field —
e.g. `newTag: v0.1.0` — once the first release is cut.

## Notes

- **One replica.** A conversation maps to a core-agent session held in an
  in-memory map; a second replica would split the map and double-consume
  the platform stream. `strategy: Recreate` avoids overlap on rollout.
  Multi-replica waits on a durable session map (DESIGN.md).
- **Health + metrics.** `--metrics-addr=:9090` serves `/healthz` (backing
  the liveness + readiness probes) and `/metrics` (Prometheus) on the
  named `metrics` port. That port is switchboard's only inbound surface;
  the NetworkPolicy admits it (kubelet probes bypass NetworkPolicy) and
  denies everything else. Narrow the ingress `from` to your monitoring
  namespace if Prometheus runs in-cluster. `serve` still exits non-zero on
  a fatal error (bad token, adapter failure, metrics bind failure) so the
  container also restarts on real failures.
- **Progress mode** is `--progress-mode=indicator` in the base; change it
  there or per-channel at runtime via the `progress` chat command.
