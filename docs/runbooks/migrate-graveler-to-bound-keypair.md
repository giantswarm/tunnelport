# Migrate graveler `RemoteApp`s from `token` to `bound_keypair` join

Operator-migration runbook. Closes [#9](https://github.com/giantswarm/tunnelport/issues/9)
when executed. Background: ADR 0004 (bot tokens are single-use; emptyDir is
unsafe) and ADR 0005 (bound_keypair + `recovery.mode: relaxed`).

> One-off migration runbook for the tunnelport operator's first real
> consumer MC — not an oncall runbook. If the GS docs/intranet team wants
> it under `content/docs/support-and-ops/runbooks/`, the formal Hugo
> shortcodes can be applied as a follow-up; for now, plain Markdown
> mirrors the `docs/adr/*` style of this repo.

## Scope

graveler is a CAPA testing installation
(`/Users/pau/workspace/github/catalog/installations.yaml`) with two
`RemoteApp` CRs deployed in the `muster` namespace:

| `RemoteApp` (CR name) | `tokenRef` Secret      | Source-of-truth (Flux)                                                                                                          |
|-----------------------|------------------------|---------------------------------------------------------------------------------------------------------------------------------|
| `garm-dex`            | `garm-dex-token`       | `giantswarm-management-clusters/management-clusters/graveler/extras/muster/remoteapps/garm-dex.yaml`                            |
| `garm-mcp-kubernetes` | `garm-mcp-kubernetes-token` | `giantswarm-management-clusters/management-clusters/graveler/extras/muster/remoteapps/garm-mcp-kubernetes.yaml`            |

Both CRs target `proxyAddr: teleport.giantswarm.io:443` and have
`muster.giantswarm.io/management-cluster: garm` labels.

Operator chart on graveler: HelmRelease `tunnelport` in namespace
`flux-giantswarm`, sourced from
`management-cluster-bases/extras/tunnelport` (referenced via
`giantswarm-management-clusters/management-clusters/graveler/extras/tunnelport/kustomization.yaml`),
backed by an `OCIRepository` pinned to `semver: ">=0.0.0"` against
`oci://gsoci.azurecr.io/charts/giantswarm/tunnelport`. **The chart is
auto-tracking — releasing a new tag to the OCI registry will cause Flux
to roll graveler within ~10 min** (HelmRelease `interval`).

Central-side bot provisioning is **raw `tctl`**: no `TeleportBot` or
`TeleportToken` CRs exist anywhere in the workspace for these two bots.
This runbook reflects that. If the platform team has switched to the
upstream Teleport Operator since this runbook was written, replace the
`tctl create -f` steps with the equivalent CR edits.

## Pre-flight (decision points)

### 1. Decide: slice-1-only release vs. slice-1+slice-2 release

| Path                        | Operator behaviour on graveler                                                                                                   | Prereq                                                                                                                           |
|-----------------------------|----------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------|
| **Slice 1 only** (#7)       | tbot pod = `Deployment` + `emptyDir`. Every restart re-registers via the registration secret (`recovery.mode: relaxed` allows it). | None beyond bound_keypair on Central.                                                                                            |
| **Slice 1 + Slice 2** (#8)  | tbot pod = `StatefulSet` + 1Gi RWO PVC. Restarts preserve identity; re-registration only on first start or PVC loss.                | A default `StorageClass` on graveler (CAPA → typically `ebs-sc` marked default). **Verify before rolling.**                      |

Recommended default: **wait for slice 2 to merge and ship both in one
tag**. Single tag avoids two graveler reconciles and tests both behaviours
end-to-end. Pick slice-1-only if slice 2 stalls and the per-restart
re-registration noise is acceptable for the testing pipeline.

### 2. Verify graveler StorageClass (only if rolling slice 2)

```sh
kubectl --context graveler get storageclass
```

Expect at least one with `(default)` annotation. CAPA MCs at GS typically
have `ebs-sc` marked default. If none is default, **stop** — adding a
default SC is its own change.

### 3. Capture current state

```sh
kubectl --context graveler -n muster get remoteapp -o wide
kubectl --context graveler -n muster get pods -l tunnelport.giantswarm.io/role=tbot -o wide
kubectl --context graveler -n muster get secrets garm-dex-token garm-mcp-kubernetes-token \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.resourceVersion}{"\n"}{end}'
```

Record the chart version Flux currently has applied:

```sh
kubectl --context graveler -n flux-giantswarm get helmrelease tunnelport \
  -o jsonpath='{.status.history[0].chartVersion}{"\n"}'
```

That string is the version you roll back to if needed.

### 4. Confirm where the `tokenRef` Secrets come from

This is the **largest unresolved question**. There is **no
`garm-*-token.enc.yaml`** in `giantswarm-management-clusters` for either
Secret. The likely possibilities, in order:

1. Out-of-band creation (a platform engineer ran `kubectl create secret`
   manually on graveler at first deploy).
2. Sealed-secret / SOPS file in a different repo not present in this
   workspace.
3. Generated by a controller not visible from this workspace.

> **TODO** — confirm with the platform team **before** executing this
> runbook. The Secret-rotation step below assumes path (1) or (2) — if
> something else owns the Secret, edit the relevant section accordingly.

## Step 1 — Central-side: rewrite each bot to use bound_keypair

Run from a host with `tctl` and Central credentials. For **each** of the
two bots (`garm-dex`, `garm-mcp-kubernetes`):

### 1a. Delete the existing static-token resource

```sh
tctl rm token/garm-dex-token
tctl rm token/garm-mcp-kubernetes-token
```

(Token resource names mirror the Secret names by convention — see PR #10
and the new `helm/tunnelport/README.md` §4. If the Central-side names
diverge from the K8s Secret names, list current tokens with
`tctl get tokens` first.)

### 1b. Create the bound_keypair token (per bot)

Apply the following YAML (substitute `<bot-name>` and adjust `metadata.name`
per bot — keep the convention `metadata.name == <K8s tokenRef Secret name>`):

```yaml
kind: token
version: v2
metadata:
  name: garm-dex-token   # or garm-mcp-kubernetes-token
spec:
  roles: [Bot]
  bot_name: garm-dex     # or garm-mcp-kubernetes
  join_method: bound_keypair
  bound_keypair:
    recovery:
      mode: relaxed
```

Apply with `tctl create -f`.

### 1c. Read back the registration secret

```sh
tctl get token/garm-dex-token --format=json \
  | jq -r '.spec.bound_keypair.registration_secret'
```

That value is what goes into the K8s Secret in step 2. Repeat for
`garm-mcp-kubernetes-token`. **Treat these as live credentials** — do not
echo to shared logs.

### 1d. Verify each bot is configured for bound_keypair

```sh
tctl get bot/garm-dex          --format=json | jq '.spec.traits, .spec.roles'
tctl get bot/garm-mcp-kubernetes --format=json | jq '.spec.traits, .spec.roles'
```

The bot resources themselves do not need to change for v18 — the join
method and recovery mode are enforced by the **token resource**, not the
bot. (Verified against tunnelport PR #10 + Teleport v18 docs.)

## Step 2 — Consumer-MC: rotate the `tokenRef` Secret values

Replace the static-token value in each Secret with the registration secret
from step 1c.

> **Path depends on what owns the Secret today (see Pre-flight §4).**

### 2a. If the Secret is SOPS-encrypted in `giantswarm-management-clusters`

Add a SOPS-encrypted `garm-dex-token.enc.yaml` and
`garm-mcp-kubernetes-token.enc.yaml` to
`giantswarm-management-clusters/management-clusters/graveler/extras/muster/`,
listed in the kustomization (mirrors the existing
`oauth-credentials.enc.yaml` and `valkey-credentials.enc.yaml` in the same
directory). Encrypt with the graveler age recipient
(`age_secret_keys.yaml` in the same MC tree). Commit + push + wait for
Flux `kustomize-controller` to apply (≤10 min).

### 2b. If the Secret was hand-rolled out of band

Treat that as a regression — file an issue to bring it under GitOps before
proceeding. Do **not** run `kubectl edit secret`; that breaks the
GitOps source of truth and Flux will not undo a manual edit (the resource
isn't in any kustomization).

If the migration must happen before the GitOps move, the ad-hoc command
is `kubectl --context graveler -n muster create secret generic
garm-dex-token --from-literal=token=<value> --dry-run=client -o yaml |
kubectl --context graveler apply -f -`. Document the regression.

### 2c. Verify the rotation reached the cluster

```sh
kubectl --context graveler -n muster get secret garm-dex-token \
  -o jsonpath='{.metadata.resourceVersion}{"\n"}'
```

The `resourceVersion` should differ from what you captured in pre-flight
§3.

The tunnelport operator's auto-roll annotation will bounce the tbot
Deployment when the Secret's `resourceVersion` changes (per
`AnnotationTokenSecretVersion` — `helm/tunnelport/README.md` and ADR
trail). After steps 1 + 2 land, **the new tbot pods will fail to join
with the old chart** (chart still emits `join_method: token`). That is
expected — the failure window closes as soon as step 3 lands.

## Step 3 — Cut + roll the operator chart

### 3a. Tag a release on `giantswarm/tunnelport`

The release pipeline only runs on tags matching `/^v.*/`
(`.circleci/config.yml` `push-to-app-catalog` job filter). On `main`:

```sh
git tag vX.Y.Z   # version chosen per repo cadence
git push origin vX.Y.Z
```

Confirm the tag's commit includes PR #10 (slice 1) and, if rolling both,
the slice-2 PR. CI will publish the chart to `control-plane-catalog`
(consumed by the OCI registry the graveler HelmRelease tracks).

### 3b. Wait for Flux to reconcile graveler

```sh
kubectl --context graveler -n flux-giantswarm get ocirepository tunnelport \
  -o jsonpath='{.status.artifact.revision}{"\n"}'
kubectl --context graveler -n flux-giantswarm get helmrelease tunnelport \
  -o jsonpath='{.status.history[0].chartVersion}{"\n"}'
```

`OCIRepository.spec.interval` and `HelmRelease.spec.interval` are both
`10m`. End-to-end reconcile is typically ≤20 min after the tag CI
completes. To force, `flux reconcile source oci tunnelport
--namespace=flux-giantswarm` then `flux reconcile helmrelease tunnelport
--namespace=flux-giantswarm`.

## Step 4 — Verification

### 4a. tbot pod health

```sh
kubectl --context graveler -n muster get pods \
  --selector=tunnelport.giantswarm.io/role=tbot \
  --output=wide
kubectl --context graveler -n muster get remoteapp \
  --output=jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.lastError}{"\n"}{end}'
```

`status.lastError` should be empty for both. Pods should be `1/1 Running`.

### 4b. Functional check — tunnel reaches the app

The canonical health-check approach in this repo is the smoke harness'
`curl` from a sidecar pod into the rendered tbot Service
(`hack/smoke/consumer/curl-pod.yaml` + the `kubectl ... -- curl ...` step
in `hack/smoke/README.md` §7). On graveler, the equivalent is whatever
muster uses to hit `garm-dex` and `garm-mcp-kubernetes` — confirm with
the muster owner before the runbook moves to the support-and-ops site.
Place-holder smoke from a debug pod:

```sh
kubectl --context graveler -n muster run curl-check --rm -it --restart=Never \
  --image=curlimages/curl -- \
  curl -sS http://garm-dex.muster.svc.cluster.local:8080/healthz
```

A 200/401 (depending on dex's auth) means the tunnel is up. Connection
refused or DNS error means the Service or tbot pod isn't ready yet.

### 4c. Teleport audit log

In Teleport (Web UI → Audit Log, or `tctl audit ls --type=bot.join`):
expect one `bot.join` event per bot since the chart bump, with
`join_method: bound_keypair`. Repeated `bot.join` events on every pod
restart are expected and **acceptable** on the slice-1-only path (this is
ADR 0005's documented trade-off). On the slice-1+2 path, expect zero
extra `bot.join` after the initial registration.

## Step 5 — Rollback

**Trigger conditions:** trigger rollback only if (a) tbot pods on
graveler `CrashLoopBackOff` with `status.lastError` referencing a
join failure that does **not** clear within 30 min, AND (b) the failure
isn't traceable to a known cause (Secret rotation race, graveler Flux
lag, propagation delay on Central). Transient `bot.join` errors during
the rollout are not rollback triggers — the operator self-recovers via
relaxed recovery.

**Forward-fix is preferred.** Most failure modes here have a same-shape
fix (re-rotate the Secret, re-run `tctl create -f` for the token,
`flux reconcile`). Only roll back if forward-fix would take longer than
the rollback or if the operator chart itself is broken.

### Rollback steps

1. **Pin the HelmRelease back to the prior chart version.** Edit
   `giantswarm-management-clusters/management-clusters/graveler/extras/tunnelport/kustomization.yaml`
   to add a kustomize patch that pins `OCIRepository.spec.ref.semver` to
   the previous chart version (recorded in pre-flight §3). Commit + push
   + wait for Flux. (`semver: ">=0.0.0"` will keep auto-tracking
   otherwise.)
2. **Revert the Secret rotation.** Restore the static-token values from
   step 1's pre-flight backup (or from the previous SOPS file's git
   history). Commit + push + wait for Flux to apply.
3. **Restore the static-token resources on Central.** `tctl rm` the
   bound_keypair tokens; `tctl create -f` the original static tokens
   from whatever YAML or `tctl tokens add` invocation produced them
   originally. (If the platform team did not capture the original
   token values, the rollback path is "issue new static tokens with the
   same names" — the operator only cares about the K8s Secret content.)

## Step 6 — Post-rollout cleanup

Once graveler has been healthy on the new chart for at least one hour:

- **Audit Central** for any leftover static-token resources for these two
  bots — `tctl get tokens | grep garm-` should show only the
  bound_keypair tokens. Remove anything stale.
- **Update the muster runbook / dashboards** that mention "static
  token" to match the new model (registration secret + bound_keypair).
  No file in this workspace currently does — flag it for the muster team.
- **Close [#9](https://github.com/giantswarm/tunnelport/issues/9)** with
  a comment linking the chart-tag commit + the graveler Flux reconcile
  evidence.

## Decision log (fill in during execution)

| When | Who | Decision                                      | Reason |
|------|-----|-----------------------------------------------|--------|
|      |     | slice-1-only / slice-1+2                       |        |
|      |     | StorageClass on graveler (if slice-2)          |        |
|      |     | Secret-source path (SOPS / hand-rolled / other)|        |
|      |     | Chart tag chosen for the rollout               |        |
