# Agent identity — switchboard authenticates as itself, and the admin API

**Status:** proposal (2026-10-12) · **Tracks:** #155 (S1 #156, S2 #157, S3 #158,
S4 #159; core-agent#1313) · **Builds on:** multi-agent (#140, `docs/multi-agent.md`) ·
**Depends on:** purser phase 2 (go-steer/purser#19)

## The problem

Multi-agent phase 2 is an admin API that adds agents at runtime. Designing it
showed that the hard part is not the API but what an agent's entry has to
carry.

- **Every agent costs a shared secret.** switchboard reaches each daemon with
  a bearer token named by `token_env`, so an agent is a URL *plus* a token the
  gateway already holds. A token added at runtime has nowhere to come from: an
  env var means editing the Deployment and restarting, which defeats adding
  agents at runtime. A mounted directory of token files works, but it gives
  every pairing of gateway and agent its own long-lived secret to provision,
  rotate and revoke.
- **The admin API would be one more shared token.** The design so far protects
  it with a bearer token from `--admin-token-env`, as the outbound ingress is.
  An audit line can then only say "whoever had the token" made a change.
- **The daemon trusts a token, not a workload.** core-agent's authenticator
  lets a caller send `X-Asserted-Caller` only if it is on a proxy allowlist,
  and today that allowlist is keyed by token. Whoever holds switchboard's token
  can assert any user.

go-steer/purser exists to replace exactly this. It turns a TLS connection or an
HTTP request into a `Caller` from a SPIFFE X.509-SVID, a standard-CA client
certificate or an OIDC token, and its `authz.Rules` grant the right to proxy
by identity. purser's design already names switchboard as a consumer of the
client half.

## The decision

**switchboard authenticates to every agent as itself, with one workload
identity, and each daemon grants that identity the right to assert callers.**

- On GKE the gateway's pod gets a managed workload identity SVID, delivered as
  files under `/var/run/secrets/workload-spiffe-credentials/`. Elsewhere it is
  a SPIRE Workload API socket, or a cert-manager certificate from a standard
  CA.
- Each daemon has one rule: this identity may connect and may proxy. It lives
  in the daemon's own config, once, not once per pairing.
- **Adding an agent becomes a URL plus the identity the agent is expected to
  present**, both public. No secret is provisioned, mounted or rotated for it.
- Bearer tokens stay, per agent, for a daemon that has not moved to purser.
  An existing `token_env` entry keeps working unchanged.

The admin API is protected by purser too: OIDC for people, mTLS for
automation, and an `authz` rule for who is an admin. Its audit line records the
authenticated caller.

This does not move credential logic into switchboard. The hard rule in
AGENTS.md keeps *caller* credential resolution in core-agent, and that is
unchanged: switchboard still only asserts the chat user in
`X-Asserted-Caller`. What changes is how switchboard proves who *it* is, which
is the client role purser assigns it.

## Agent entries

An agent names how switchboard authenticates to it. The method is per agent so
one gateway can front migrated and unmigrated daemons at once.

```json
{"agents": [
  {"name": "platform", "display_name": "Platform agent",
   "daemon_url": "https://gke-platform-agent:7777",
   "auth": {"method": "spiffe",
            "server_id": "spiffe://gke-demos-345619.svc.id.goog/ns/switchboard/sa/core-agent"}},
  {"name": "general", "display_name": "General agent",
   "daemon_url": "https://general-agent:7777",
   "auth": {"method": "pki", "server_name": "general-agent.switchboard.svc"}},
  {"name": "legacy", "daemon_url": "http://legacy:7777",
   "token_env": "LEGACY_DAEMON_TOKEN"}
]}
```

| method | switchboard presents | it verifies the daemon by |
|---|---|---|
| `spiffe` | its SVID | the daemon's SPIFFE ID, `server_id` (required, as purser's client profile requires an authorizer) |
| `pki` | its client certificate | the CA pool, and `server_name` when it differs from the dialled host |
| `bearer` | `Authorization: Bearer` from `token_env` | nothing beyond TLS, if the URL is https |

- An entry with only `token_env` is `bearer`, so existing configs are
  unchanged.
- `server_id` and `server_name` are not secrets. They are what make mutual
  authentication mutual: without them, "any workload in the trust domain"
  includes every compromised peer that can win a race for the address.

**The gateway's own identity** is configured once, by flag or config file, and
used for every `spiffe` or `pki` agent:

| flag | meaning |
|---|---|
| `--identity` | `spiffe-files`, `spiffe-workload-api`, `pki`, or `none` (the default: bearer only) |
| `--spiffe-dir` | for `spiffe-files`; defaults to GKE's managed workload identity path |
| `--spiffe-endpoint-socket` | for `spiffe-workload-api` |
| `--tls-cert-file`, `--tls-key-file`, `--tls-ca-file` | for `pki` |

These are paths, not secret values, so they don't fall under the rule against
secrets as flags. Certificates rotate in place: purser reads the SVID per
handshake, and the `pki` profile takes a `GetClientCertificate` callback.

## How it plugs in

- **`pkg/daemon` does not change shape.** Its `Config` already takes an
  `HTTPClient`. An mTLS agent gets a client built on purser's
  `client.Transport`, and a bearer agent keeps `BearerToken`. The contract
  client stays the four verbs, introspecting no auth.
- **The approval client** gets the same transport as its agent's daemon
  client.
- **Distroless stays intact.** purser and go-spiffe are pure Go, so
  `CGO_ENABLED=0` and the static image are unaffected.

## What the daemons need

This is the dependency. switchboard can build and test its side against
purser's in-memory CA (`authtest`) now, but cannot use it against a real
daemon until:

1. **The daemon accepts mTLS on its attach listener**, in the SPIFFE profile
   and the PKI profile. This is purser phase 2: mast first
   (go-steer/purser#19, go-steer/mast#469), then core-agent. core-agent's
   `BuildMultiSessionAuthn` gains `mtls` and `oidc` cases.
2. **Proxy rights come from `authz.Rules`**, so a rule matching switchboard's
   identity grants `CanProxyAs`. `X-Asserted-Caller` keeps its meaning.
3. **The daemon serves TLS in-cluster.** The GKE example's daemons serve plain
   HTTP today; mTLS needs a TLS listener with a certificate the gateway can
   verify (an SVID on GKE).

Until a daemon has all three, its entry stays `bearer`.

## The admin API

It is the API in `docs/multi-agent.md`, with these changes.

- **Authentication by purser**, on its own listener (`--admin-addr`):
  `httpmw.NewCaller` with OIDC for people (a Google sign-in, say) and/or mTLS
  for automation. `httpmw.CheckBind` refuses a non-loopback address unless a
  credential-enforcing gate is in the chain. A static bearer token is accepted
  only on loopback, for development.
- **Authorization by `authz.Rules`:** a rule grants the admin bit, for example
  a group's email domain or a named service identity. Every request needs
  admin, reads included (changed in #157): with an issuer like Google, an
  authenticated caller is anyone with an account, and a read shows every
  daemon URL.
- **Agents by URL and identity.** `PUT /v1/agents/{name}` takes the entry
  above. It is validated on write: the URL parses, the method is known, a
  `bearer` entry's token env is set, and an mTLS entry has its `server_id` or
  CA. Reachability and the agent card are phase 4 (discovery). Config-file
  entries are pinned (`409`).
- **The registry becomes runtime-mutable.** The router reads agents and
  channel settings from a snapshot swapped atomically on each change, so a
  turn never sees half an update. Removing an agent leaves its threads to the
  existing "this agent is gone" notice; nothing is rerouted.
- **A store beside the state snapshot**, shaped like `stateStore`, holding API
  agents, the global default and API channel settings. On a multi-pod
  deployment it is #138's shared store.
- **Audit:** each change is an `admin` record in the existing audit log, with
  the purser `Caller` (identity, auth source, and who proxied if anyone), the
  path, and the before and after.

## Plan and dependencies

| # | work | repo | depends on |
|---|---|---|---|
| D1 | purser phase 2 for mast: users-file mode fix, ACL normalization, migration | purser, mast | — (go-steer/purser#19, go-steer/mast#469) |
| D2 | purser phase 2 for core-agent: `mtls` and `oidc` authenticators, proxy rights from `authz.Rules`, TLS on the attach listener | core-agent | D1 (the order purser#19 proposes) |
| S1 | Runtime-mutable registry and its store | switchboard | — |
| S2 | Admin API with purser authentication, authorization and audit | switchboard | S1 |
| S3 | Gateway identity and per-agent `auth` methods over purser's client half, tested against `authtest` | switchboard | — to build; D2 to use against core-agent |
| S4 | GKE example on managed workload identity: both agents over `spiffe`, the admin API behind OIDC | switchboard | S2, S3, D2 |

S1, S2 and S3 need nothing from core-agent and can proceed while other
core-agent work finishes. Discovery (agent cards, health) stays phase 4 of
#140.

## Open questions

- **Outbound OIDC.** purser's client half is mTLS only. A gateway that cannot
  get a certificate but can mint an ID token (a Google service account) would
  need a token source; it can be added later as another `auth` method.
- **mast's listeners.** purser#19 notes that exposing OIDC and mTLS on mast's
  listeners is a separate mast change from the migration itself. A mast agent
  stays `bearer` until it lands.
- **Managed workload identity on the demo cluster.** It needs the cluster in a
  fleet with the feature on. Until then the GKE example can use the `pki`
  method with cert-manager.
- **One identity per gateway.** Every agent sees the same switchboard
  identity, so the Slack and Chat gateways are distinguishable only if they
  run as different service accounts, as they do in the GKE example.
