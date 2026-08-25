# Onboarding — Teleport-side setup for tunnelport

This document covers everything an SRE provisions on **Teleport central**
before tunnelport can establish a tunnel from a consumer cluster. The
operator on the consumer side never talks to Teleport directly; it only
renders `tbot` + `ghostunnel` workloads that join Teleport using
credentials whose trust roots you set up here.

Audience: an SRE with `tctl` access (or equivalent GitOps path) against
the Teleport cluster. Everything below assumes a single Teleport
control plane shared by one or more consumer clusters.

## What you're provisioning, and why

Tunnelport splits trust into three independent concerns. The SRE owns
all three on the Teleport side; the operator on the consumer side is
purely a consumer of them.

| Concern | Teleport resources |
|---|---|
| **How `tbot` proves who it is** | `ProvisionToken` (kubernetes join, `static_jwks`) |
| **What `tbot` is allowed to reach** | `bot` + `role` (scoped `app_labels` / `workload_identity_labels`) |
| **The TLS cert callers verify on `:8443`** | `workload_identity` (SPIFFE SVID via `tbot`'s `workload-identity-x509` service) |

Per consumer cluster you provision the resources **once** for the
chart-managed singleton trust-bundle bot, and **once per RemoteApp** for
each tunnel.

## Step 0 — Export the consumer cluster's JWKS (one-off per consumer)

`tbot` joins Teleport using the kubernetes join method. Because the
consumer cluster's kube-apiserver is not directly reachable from
Teleport, you cannot use the `in_cluster` validator. Instead Teleport
validates the projected ServiceAccount JWT against a JWKS document you
embed into every `ProvisionToken`.

On the consumer cluster:

```sh
kubectl --raw /openid/v1/jwks
```

Capture the JSON exactly as returned. This value is reused for every
`ProvisionToken` you author for this consumer cluster. Re-export and
re-apply all tokens together if the consumer cluster ever rotates its
service-account signing keys.

For an app that already has a token because another consumer reaches it,
merge these keys into that token's existing key set rather than
authoring a second token. See "A second consumer reaching the same app"
in Step 2c.

> The JWKS is non-secret. It's a public verification key set; treat it
> the same way you'd treat any OIDC discovery document.

## Step 1 — Provision the trust-bundle singleton (one-off per consumer)

Tunnelport's chart runs one **trust-bundle `tbot`** per consumer
cluster. Its only job is to mint a SPIFFE bundle file and write it to a
Secret (`tunnelport-spiffe-bundle`) in the release namespace, so callers
on the consumer cluster have one place to look up the CA chain for
every RemoteApp's TLS cert.

You need a dedicated bot identity for it — distinct from any per-app
bot so a compromise of the trust-bundle credential cannot also reach an
application.

### 1a. Role

The trust-bundle bot must be able to **list and read** `workload_identity`
resources on Teleport (otherwise SVID issuance fails with
`access denied to perform action "list" on "workload_identity"`).
It does **not** need any `app_labels` — it never opens an application
tunnel — so the role's `allow` block carries only the
`workload_identity_labels` selector and a placeholder `logins` entry
(Teleport's role validator rejects empty logins).

```yaml
kind: role
version: v7
metadata:
  name: tunnelport-trust-bundle
spec:
  allow:
    workload_identity_labels:
      trust-bundle: ["tunnelport"]
    rules:
      - resources: [workload_identity]
        verbs: [list, read]
    logins:
      - tunnelporttrustbundle   # placeholder, never consumed
```

The selector key (`trust-bundle:`) is intentionally **different** from
the key the per-RemoteApp roles use (`remoteapp:`). Different keys mean
the trust-bundle bot and the per-app bots cannot cross-match each
other's `WorkloadIdentity` resources, so a leaked credential from one
family cannot be used to mint an SVID belonging to the other.

### 1b. Bot

```yaml
kind: bot
version: v1
metadata:
  name: tunnelport-trust-bundle-bot
spec:
  roles:
    - tunnelport-trust-bundle
```

### 1c. ProvisionToken

The chart renders a `ServiceAccount` named `tunnelport-trust-bundle` in
its release namespace. Pin the token's `allow` rule to exactly that
`<namespace>:<sa-name>` (substitute the namespace you set as the chart's
`installNamespace`).

```yaml
kind: token
version: v2
metadata:
  name: tunnelport-trust-bundle-token
spec:
  roles: [Bot]
  bot_name: tunnelport-trust-bundle-bot
  join_method: kubernetes
  kubernetes:
    type: static_jwks
    static_jwks:
      jwks: |
        <paste the JWKS you exported in Step 0>
    allow:
      - service_account: "<installNamespace>:tunnelport-trust-bundle"
```

### 1d. WorkloadIdentity

The SVID minted by this bot is **never presented to a verifier** — only
its CA chain is. That has two consequences for the resource you write:

- No DNS SANs. The cert is not used as a server cert.
- Label key `trust-bundle: tunnelport` (matches the role's selector).

```yaml
kind: workload_identity
version: v1
metadata:
  name: tunnelport-trust-bundle
  labels:
    trust-bundle: tunnelport
spec:
  spiffe:
    id: /bot/tunnelport-trust-bundle-bot
```

Apply all four resources:

```sh
tctl create -f tunnelport-trust-bundle-role.yaml
tctl create -f tunnelport-trust-bundle-bot.yaml
tctl create -f tunnelport-trust-bundle-token.yaml
tctl create -f tunnelport-trust-bundle-svid.yaml
```

Then point the operator's chart values at the token name:

```yaml
trustBundle:
  enabled: true
  tokenName: tunnelport-trust-bundle-token
```

## Step 2 — Provision per-app resources (once per app, not once per consumer)

Every app reached by a `RemoteApp` needs its **own** Teleport bot, role,
provision token, and `WorkloadIdentity`. The naming below assumes a CR
named `<app>` deployed in namespace `<ns>`; substitute as appropriate.

The unit is the app, not the CR. Two consumer clusters reaching the same
app share one set of these four objects, and the token carries one JWKS
key per consumer. Section 2c covers that case. Run Step 2 once per app,
then only Step 0 and the 2c merge for each further consumer.

Per-app isolation is deliberate: a leaked credential from one
RemoteApp's `tbot` pod reaches only that one Teleport application.
Sharing a bot across *apps* would widen blast radius without operational
benefit. Sharing one across consumers of the *same* app does not: they
already reach the same Teleport application by design.

### 2a. Role

The role binds two things at once:

1. **Application access** — `app_labels` must match the labels stamped
   by the upstream `teleport-kube-agent` onto the specific Teleport app
   you want this tunnel to reach, and nothing else. Verify the
   live labels before authoring:

   ```sh
   tctl get app_servers | yq '.spec.app.labels'
   ```

2. **SVID issuance** — `workload_identity_labels` narrows to the
   per-app `WorkloadIdentity` you'll define in §2d. Use the
   `remoteapp:` key consistently.

The role also needs `list`/`read` on `workload_identity` (see §1a) and
a non-empty `logins` placeholder.

```yaml
kind: role
version: v7
metadata:
  name: <app>-tunnel
spec:
  allow:
    app_labels:
      # Replace with the exact label selector that pins the one app
      # you want this RemoteApp to reach.
      app: ["<app-label-1>"]
      # additional pinning labels as needed
    workload_identity_labels:
      remoteapp: ["<app>"]
    rules:
      - resources: [workload_identity]
        verbs: [list, read]
    logins:
      - tunneluser   # placeholder, application-tunnel does not consume it
```

> Be conservative with `app_labels`. Each label you add narrows what
> the role can reach; a too-loose selector is the most common way a
> RemoteApp ends up over-privileged.

### 2b. Bot

```yaml
kind: bot
version: v1
metadata:
  name: <app>-bot
spec:
  roles:
    - <app>-tunnel
```

### 2c. ProvisionToken

The operator on the consumer cluster renders a `ServiceAccount` named
exactly `<cr.name>` in the CR's namespace (`<ns>:<app>`). Pin the
token's `allow` rule to that:

```yaml
kind: token
version: v2
metadata:
  name: <app>-bot-token
spec:
  roles: [Bot]
  bot_name: <app>-bot
  join_method: kubernetes
  kubernetes:
    type: static_jwks
    static_jwks:
      jwks: |
        <same JWKS exported in Step 0>
    allow:
      - service_account: "<ns>:<app>"
```

The token name (`<app>-bot-token`) is what the consumer-side platform
engineer puts in `RemoteApp.spec.tokenName`.

#### A second consumer reaching the same app

One token serves every consumer of one app. A `static_jwks` token is
**not** pinned to a single consumer cluster: `jwks` is a key set, and
Teleport picks the key by the `kid` in the presented ServiceAccount
token. So do not create a per-consumer token, and do not edit the
existing token's `jwks` in place to the new consumer's document, which
would break the consumer already using it.

Instead, merge the new consumer's keys into the `keys` array and add one
`allow` rule for its service account:

```yaml
kind: token
version: v2
metadata:
  name: <app>-bot-token
spec:
  roles: [Bot]
  bot_name: <app>-bot
  join_method: kubernetes
  kubernetes:
    type: static_jwks
    static_jwks:
      jwks: |
        {"keys":[
          {"kid":"<consumer-1 kid>", ...},
          {"kid":"<consumer-2 kid>", ...}
        ]}
    allow:
      - service_account: "<ns>:<app>"        # consumer 1
      - service_account: "<ns>:<app>"        # consumer 2, same ns and CR name
```

The `allow` rules are identical when both consumers install into the
same namespace and name the CR the same, which is the normal case. Keep
both entries anyway: the list is what you edit when a consumer leaves,
and a single entry gives no hint that two clusters depend on it.

The bot, role and `WorkloadIdentity` are reusable as they are. The
`dns_sans` and the `remoteapp:` selector already match, because both
consumers render the same Service name in the same namespace.

Getting this wrong is silent for weeks. `tbot` keeps running on a cached
bot identity and only has to re-join when its pod is recreated, so a
token that admits the wrong cluster does not fail until the next pod
restart. Run the Step 3 JWKS check before you hand the CR over.

### 2d. WorkloadIdentity

The SVID this bot mints **is** presented as a server cert on the
consumer-side `Service`'s `:8443`, so the DNS SANs must match the
Service's in-cluster DNS names. The operator renders the Service as
`<cr.name>.<ns>.svc.cluster.local`, so all three short/long forms of
that name go in.

```yaml
kind: workload_identity
version: v1
metadata:
  name: <app>-svid
  labels:
    remoteapp: <app>
spec:
  spiffe:
    id: /bot/<app>-bot
    x509:
      dns_sans:
        - <app>.<ns>.svc.cluster.local
        - <app>.<ns>.svc
        - <app>.<ns>
```

Apply:

```sh
tctl create -f <app>-role.yaml
tctl create -f <app>-bot.yaml
tctl create -f <app>-token.yaml
tctl create -f <app>-svid.yaml
```

A consumer-side platform engineer can now author a `RemoteApp`
referencing `tokenName: <app>-bot-token` and the tunnel will come up.

## Step 3 — Verification

Run these against Teleport after applying the per-RemoteApp resources
and before signaling the consumer-side team to apply the CR.

### Token is reachable and well-formed

```sh
tctl get token/<app>-bot-token
```

Check:
- `spec.join_method: kubernetes`
- `spec.kubernetes.type: static_jwks`
- `spec.kubernetes.allow[].service_account` matches `<ns>:<app>`
  exactly. A mismatch here is the most frequent join failure cause and
  surfaces consumer-side as `tbot` `CrashLoopBackOff` with a join error
  in the pod logs.

### The token admits this consumer's signing key

The token's JWKS must contain the consumer cluster's current
service-account signing key. Compare the two key sets directly:

```sh
tctl get token/<app>-bot-token --format=json \
  | jq -r '.[0].spec.kubernetes.static_jwks.jwks | fromjson.keys[].kid'
kubectl --context <consumer> get --raw /openid/v1/jwks | jq -r '.keys[].kid'
```

Every `kid` from the consumer must appear in the token's list. Run it
once per consumer that references the token.

This is the check whose absence cost five weeks in
giantswarm/giantswarm#37445: a token carrying only the *other* consumer's
key looks correct in `tctl get`, `allow.service_account` matches, the
role and `WorkloadIdentity` are right, and the tunnel still cannot join.

### Bot and role are wired

```sh
tctl get bot/<app>-bot
tctl get role/<app>-tunnel
```

Check `bot.spec.roles` contains the role name, and the role's
`allow.app_labels` matches the app you intended.

### WorkloadIdentity selector binds to the role

```sh
tctl get workload_identity/<app>-svid -o json | jq '.metadata.labels'
```

The label set must satisfy the role's `workload_identity_labels`
selector (`remoteapp: <app>`). Without this match, `tbot` joins
successfully but SVID issuance fails — the symptom on the consumer
side is `ghostunnel` failing to load its cert.

### Trust-bundle bot is healthy (one-time, after Step 1)

After the chart is installed on the consumer cluster, the trust-bundle
`tbot` pod should join Teleport once and write
`tunnelport-spiffe-bundle` into the release namespace. Verify the bot
shows as connected:

```sh
tctl bots ls | grep tunnelport-trust-bundle-bot
```

If it never connects, the `ProvisionToken`'s `allow` rule is the first
thing to check — it must name `<installNamespace>:tunnelport-trust-bundle`
exactly, where `<installNamespace>` matches the chart's
`installNamespace` value.

## Rotation

### Consumer cluster signing-key rotation

When the consumer cluster rotates its service-account signing keys, the
JWKS embedded in every `ProvisionToken` (trust-bundle token + all
per-RemoteApp tokens for that consumer) becomes stale. Re-export the
JWKS (Step 0) and patch all tokens together. Until they're updated,
every `tbot` on that consumer will fail to join on its next pod
restart.

### Per-RemoteApp credential rotation

No SRE action required. The credential `tbot` presents is the
kubelet-rotated projected ServiceAccount JWT; rotation is fully owned
by the consumer cluster's kubelet. There is no static token on the
consumer side to rotate, leak, or sync.

### Teleport SPIFFE CA rotation

When Teleport rotates its SPIFFE CA, the trust-bundle bot's next SVID
issuance picks up the new chain and rewrites `tunnelport-spiffe-bundle`
on the consumer side. Callers that mount the bundle from that Secret
pick up the new chain on the next reload. No per-app action needed.

## Security checklist before you sign off

- [ ] Per-RemoteApp role's `app_labels` selector pins the one app you
      intended (not a wildcard or a partial match).
- [ ] Per-RemoteApp role's `workload_identity_labels.remoteapp` selector
      uses a value unique to this RemoteApp.
- [ ] Trust-bundle role's selector key is `trust-bundle:`, **not**
      `remoteapp:` — the two families must not cross-match.
- [ ] Every `ProvisionToken`'s `allow.service_account` names exactly
      one `<ns>:<sa>` pair; no wildcards.
- [ ] The WorkloadIdentity's `dns_sans` exactly match the consumer
      Service's DNS names. Mismatches cause silent TLS verification
      failures at callers.
- [ ] One bot per RemoteApp. No shared bot identities across tunnels.
- [ ] The JWKS embedded in every token for a given consumer is the
      consumer's current `/openid/v1/jwks` output.
