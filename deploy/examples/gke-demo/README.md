# gke-demo: switchboard in front of a gke-platform-agent, on GKE

This is a standing deployment for testing switchboard end to end (#131). In one
`switchboard` namespace it runs:

- **A switchboard-dedicated gke-platform-agent daemon.** It runs core-agent
  `:main` with the gke-platform-agent content image (`v4`). Its config is
  core-agent's `config.hub.json` with two changes: `permissions.mode: "ask"`,
  so gated calls become approvals in the chat thread, and `sa:switchboard` as
  the only proxy identity. It has no watcher, because its only clients are the
  two gateways (enforced by a NetworkPolicy).
- **switchboard's Google Chat gateway** (`:main`), on the HTTP ingress, behind
  a GKE Gateway at `https://switchboard-chat.demo.gke.ninja/chat`. The cert
  comes from the Certificate Manager map `star-demo-gke-ninja`. Chat API calls
  use Workload Identity as the Chat app's service account.
- **switchboard's Slack gateway** (`:main`), on Socket Mode, with no inbound
  endpoint.

Both gateways keep their routing table on a PVC (`components/durable-state`),
so a rollout keeps every thread's session. Both run with `--approvals`,
`--show-usage` and `stream` progress.

It is separate from the `gke-platform-agent` namespace on purpose. That
instance runs `yolo` with a watcher, and switching it to ask mode would stall
every watcher-driven turn, because those turns have no thread to surface a
prompt in.

## Two agents

Both gateways register two agents (#140) and route by them:

- **`platform`** is the gke-platform-agent daemon above, and the default: a
  plain message goes to it.
- **`general`** is a small general-purpose core-agent (`general-agent/`) on
  Gemini's cheap tier (`gemini-3.5-flash-lite`). It has no cluster tools,
  ask-mode approvals, `checkpoint.mode: operator`, a short persona, and a
  per-turn cost ceiling of $0.10.

Reach `general` with `/agent general <prompt>` on Google Chat (slash command
ID 100, mapped in `googlechat_commands`) or `/switchboard agent general
<prompt>` on Slack. Both agents accept the same gateway token: `general`'s
bearer table reuses switchboard's `sa:switchboard` token.

## Before `kubectl apply -k`

These steps are done by hand, once. Variables:

```sh
PROJECT=gke-demos-345619
PROJECT_NUMBER=1067056737933
NS=switchboard
DAEMON=principal://iam.googleapis.com/projects/$PROJECT_NUMBER/locations/global/workloadIdentityPools/$PROJECT.svc.id.goog/subject/ns/$NS/sa/core-agent-daemon
```

**1. Address and DNS.** Reserve a global static IP and point the hostname at it:

```sh
gcloud compute addresses create switchboard-chat-ip --global --project $PROJECT
IP=$(gcloud compute addresses describe switchboard-chat-ip --global --project $PROJECT --format='value(address)')
gcloud dns record-sets create switchboard-chat.demo.gke.ninja. --zone demo-gke-ninja \
  --type A --ttl 300 --rrdatas "$IP" --project $PROJECT
```

**2. IAM for the daemon.** These are the roles the `gke-platform-agent`
namespace's daemon holds, minus telemetry, granted to this namespace's
principal:

```sh
for role in roles/aiplatform.user roles/mcp.toolUser projects/$PROJECT/roles/gkeAgentClusterViewer; do
  gcloud projects add-iam-policy-binding $PROJECT --member="$DAEMON" --role="$role" --condition=None
done
gcloud iam service-accounts add-iam-policy-binding $PROJECT_NUMBER-compute@developer.gserviceaccount.com \
  --member="$DAEMON" --role=roles/iam.serviceAccountUser --project $PROJECT
```

**3. Workload Identity for the Chat gateway.** Let its KSA act as the Chat
app's service account:

```sh
gcloud iam service-accounts add-iam-policy-binding switchboard-chat@$PROJECT.iam.gserviceaccount.com \
  --member="serviceAccount:$PROJECT.svc.id.goog[$NS/switchboard-chat]" \
  --role=roles/iam.workloadIdentityUser --project $PROJECT
```

**3b. The general agent's IAM.** Vertex only:

```sh
gcloud projects add-iam-policy-binding $PROJECT --condition=None --role=roles/aiplatform.user \
  --member="principal://iam.googleapis.com/projects/$PROJECT_NUMBER/locations/global/workloadIdentityPools/$PROJECT.svc.id.goog/subject/ns/$NS/sa/general-agent"
```

**4. Secrets.** These are never checked in. The token switchboard presents is
the `sa:switchboard` entry in the daemon's bearer table. The callers it asserts
are listed with tokens of their own, as in the laptop rig.

```sh
kubectl create namespace $NS
SB_TOKEN=$(openssl rand -hex 32)
cat > /tmp/users.json <<EOF
{"version": 1, "users": [
  {"identity": "sa:switchboard", "token": "$SB_TOKEN"},
  {"identity": "garisingh@google.com", "token": "$(openssl rand -hex 32)"},
  {"identity": "garisingh@gke.ninja", "token": "$(openssl rand -hex 32)"}
]}
EOF
kubectl -n $NS create secret generic core-agent-users --from-file=users.json=/tmp/users.json
kubectl -n $NS create secret generic switchboard-daemon-token --from-literal=token="$SB_TOKEN"
# The general agent's bearer table: the same table, so the same switchboard token.
kubectl -n $NS create secret generic general-agent-users --from-file=users.json=/tmp/users.json
kubectl -n $NS create secret generic switchboard-slack \
  --from-literal=app-token="$SWITCHBOARD_SLACK_APP_TOKEN" --from-literal=bot-token="$SWITCHBOARD_SLACK_BOT_TOKEN"
rm /tmp/users.json
```

## Apply, then cut over

```sh
kubectl apply -k deploy/examples/gke-demo
kubectl -n switchboard get gateway switchboard-chat   # wait for PROGRAMMED=True and the address
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://switchboard-chat.demo.gke.ninja/chat   # 401: reachable, auth enforced
```

Then:

- **Chat.** In the Chat API console (Configuration → Connection settings), set
  the HTTP endpoint URL to `https://switchboard-chat.demo.gke.ninja/chat`.
- **Slack.** Stop any other gateway using the same Slack app. Slack splits
  Socket Mode events across connections, so a second consumer silently takes
  some of the messages.

## Known limits

- **"Always allow (saved)" lasts only the session.** The daemon's agents
  directory is the read-only content image, so a saved grant has nowhere to
  persist.
- **core-agent and switchboard both track `:main` with `imagePullPolicy:
  Always`.** `kubectl -n switchboard rollout restart deploy` picks up the
  newest builds. Pin a `main-<sha>` tag in a `kustomization.yaml` to hold one.
- **The content image is pinned to `v4`,** the one the `gke-platform-agent`
  namespace runs. The hub config here was taken from core-agent `main`, so a
  newer content image may need it refreshed.
