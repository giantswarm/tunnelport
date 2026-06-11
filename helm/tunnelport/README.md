# tunnelport

Helm chart that installs the [tunnelport](https://github.com/giantswarm/tunnelport)
operator on a **consumer management cluster** (the MC that wants to
reach Teleport-exposed apps as local Services).

The operator watches `RemoteApp` CRs cluster-wide and renders a tbot
`Deployment` + `Service` + `ConfigMap` per CR, in the CR's namespace.
See [`CONTEXT.md`](https://github.com/giantswarm/tunnelport/blob/main/CONTEXT.md)
for the design rationale.

## Install

```sh
helm install tunnelport oci://gsoci.azurecr.io/control-plane-catalog/tunnelport \
  --namespace tunnelport-system --create-namespace
```

The default values run the operator with leader-election, a default-deny
NetworkPolicy on the manager pod, and the `gsoci-pull-secret`
`imagePullSecrets` reference (the cluster-managed pull-secret for
`gsoci.azurecr.io`).

---

## Security model — RBAC blast radius

Read this **before** you install the chart. The operator runs with a
single ClusterRole and holds two cluster-scoped read grants that are
broader than what most operators need; both are deliberate trade-offs
documented below. The full grant table is under
[Operator RBAC summary](#operator-rbac-summary).

### Cluster-scoped permissions the operator holds

| Resource | Verbs | Why cluster-scoped |
|---|---|---|
| `pods` | `get;list;watch` | Status synthesis reads pod-level state (readiness, last termination, restart count) of the rendered tbot Deployments to populate `RemoteApp.status.lastError` and the `Ready` condition — in the CR's namespace, which can be anywhere in the cluster. |
| `serviceaccounts` | `get;list;watch;create;update;patch;delete` | The operator renders one `ServiceAccount` per `RemoteApp` (ADR 0004 kubernetes-join model). The SA's projected JWT is the subject the Teleport `ProvisionToken`'s `allow` rule pins. |
| `access.giantswarm.io/remoteapps` | `get;list;watch` | The CR is cluster-scoped-watched by design (one operator, many consumer namespaces). |

The operator **never** reads `pods/log` (ADR 0003). It does not need
`secrets` at all (ADR 0004 — there is no static-token Secret to read).
Owned-object writes (`serviceaccounts`, `deployments`, `services`,
`configmaps`) are the only mutations it performs, all scoped to the
CR's own namespace.

### Pod-read scope is filtered at runtime by label selector

The pod read grant is cluster-scoped, but the controller-runtime
informer cache the operator builds is scoped to a label selector —
the cache subscribes only to pods carrying
`tunnelport.giantswarm.io/role=tbot` (the label the reconciler stamps
onto every tbot pod it renders). The operator process holds metadata
for *its own* tbot pods only, even though the API grant is broader.

### Token lifecycle: Central-side, no consumer rotation burden

Each `RemoteApp` names a Teleport `ProvisionToken` (`spec.tokenName`).
The token is configured with `join_method: kubernetes` and
`static_jwks` trust pinned to the consumer MC's `/openid/v1/jwks`
document. tbot authenticates with the projected SA JWT — there is no
static token value on the consumer cluster to rotate, leak, or sync.

Operational consequences:

* Token rotation happens at the JWKS level on Central. If the consumer
  cluster's signing key rotates, the platform team re-exports the new
  JWKS and patches the `ProvisionToken` on Central (ADR 0004).
* The bot's role/scope is defined on Central (`TeleportBot` +
  `TeleportRole`), same as before.
* If the `static_jwks.allow` rule mismatches the per-CR
  `ServiceAccount`, the tbot pod will `CrashLoopBackOff` and
  `status.lastError` will surface the kubelet-visible failure. The
  fix is to align the token's `allow.service_account` with the
  rendered SA's `<namespace>:<cr.name>`.

---

## Values surface (top-level keys only — see `values.yaml` for the full set)

| Key | Default | Notes |
|---|---|---|
| `installNamespace` | `tunnelport-system` | Where the operator pod lives. |
| `image.registry` / `image.name` / `image.tag` | `gsoci.azurecr.io/giantswarm/tunnelport`, tag falls back to `.Chart.AppVersion` | Operator container image. |
| `imagePullSecret` | `gsoci-pull-secret` | The cluster-managed pull-secret for `gsoci.azurecr.io`. The chart **references** it by name; the Secret itself must be provisioned out-of-band. |
| `resources` | 50m/64Mi → 200m/256Mi | Operator container requests/limits. |
| `tbot.image` | `public.ecr.aws/gravitational/teleport-distroless:16@sha256:...` | Single global tbot image for **every** rendered `RemoteApp` Deployment. **Pinned by digest** so a re-push behind the floating `:16` tag can't silently change tbot's config schema underneath the operator. RemoteApp.spec deliberately has no per-CR override. |
| `tbot.resources.requests` / `tbot.resources.limits` | 50m/64Mi → 200m/256Mi | Cluster-wide defaults for every rendered tbot pod. Same no-per-CR-override rule. |
| `crds.install` | `true` | Set to `false` if the CRD is delivered by a separate bootstrap chart. The chart's CRD bundle carries `helm.sh/resource-policy: keep`, so live `RemoteApp`s survive `helm uninstall`. |
| `networkPolicy.enabled` | `true` | The operator-pod NetworkPolicy. Scoped to the manager pod only — pods carrying a `tunnelport.giantswarm.io/role` label (the trust-bundle bot) are excluded. See "NetworkPolicy responsibilities" below for the **tbot pod** policy story (different concern). |

`tbot.image` and `tbot.resources` flow into the operator manager's CLI
flags (`--tbot-image`, `--tbot-cpu-request`, `--tbot-memory-request`,
`--tbot-cpu-limit`, `--tbot-memory-limit`) at start-up. The reconciler
reads them once and stamps them onto every tbot Deployment it renders.

---

## What this chart deliberately does NOT do

Four operational concerns are explicitly **outside the operator's
remit**. They each need a separate decision per consumer MC.

### 1. RoleBindings granting `RemoteApp` creation

The chart ships **no `Role` / `RoleBinding` granting create-`RemoteApp`
to anyone**. Who is allowed to author `RemoteApp` CRs is a per-cluster
trust decision that has nothing to do with the operator: in some
clusters only the platform team should write them, in others a tenant
namespace might be permitted, in others a GitOps controller's service
account does it.

The platform team layers their own RBAC on top of the chart. The
operator's own ServiceAccount **only** reads `RemoteApp`s; it never
creates them.

### 2. NetworkPolicy for the tbot pods

The operator renders a tbot `Deployment` + `Service` per `RemoteApp` in
the CR's namespace, and the chart ships a singleton trust-bundle tbot
(ADR 0008) in `installNamespace`. The chart **does not** render a
`NetworkPolicy` covering any of those tbot pods — enforcement varies by
CNI, baseline policies vary by org, the Teleport proxy port is
install-specific, and a chart-generated policy would interact awkwardly
with whatever the platform team already runs. (The chart's own
default-deny policy on the manager pod explicitly excludes them via
`tunnelport.giantswarm.io/role` — every tbot needs egress to
`proxyAddr`, which the manager policy would deny.)

The platform team writes the tbot pod policy themselves. The pods carry
stable selectors so a hand-written `NetworkPolicy` can target them:

| Label | Value | Pod |
|---|---|---|
| `tunnelport.giantswarm.io/role` | `tbot` | per-`RemoteApp` tbot |
| `tunnelport.giantswarm.io/remoteapp` | `<RemoteApp name>` | per-`RemoteApp` tbot |
| `tunnelport.giantswarm.io/role` | `trust-bundle-bot` | singleton trust-bundle tbot |

A typical policy allows:

* **Egress to `proxyAddr:443`** — the Teleport proxy host on Central.
  Without this the tbot can't reach Teleport.
* **Ingress from the approved caller pods** to `Service` port =
  `RemoteApp.spec.port`. This is the "who is allowed to use this
  tunnel" decision, made per-app by the platform team.

### 3. Central-side preconditions per `RemoteApp`

`RemoteApp.spec.tokenName` references a Teleport `ProvisionToken` on
Central (ADR 0004). The chart **does not create or template** any
consumer-side Secret — there is no static-token Secret to deliver.

For each `RemoteApp` you deploy on the consumer MC, the platform team
must produce on **Central** (typically via the upstream Teleport
Operator, `TeleportBot` + `TeleportRole` + `TeleportToken` CRs):

* a **`TeleportBot`** dedicated to this one app — one bot per
  `RemoteApp`, so a leaked tbot identity reaches only that one app
  (per-app blast-radius isolation);
* a **role** assigned to that bot whose `app_labels` selector matches
  exactly the one Teleport `App` `RemoteApp.spec.appName` refers to,
  and nothing else;
* a **`ProvisionToken`** of kind `kubernetes` whose
  `spec.kubernetes.static_jwks` is pinned to the consumer MC's
  `/openid/v1/jwks` document, and whose `spec.kubernetes.allow` rule
  names exactly the per-CR ServiceAccount the operator will render
  (`<namespace>:<cr.name>`).

If any of those is missing or mis-scoped, the tbot pod will fail to
join Central; the operator surfaces this via `status.lastError`
(k8s-visible state only — see ADR 0003), and the platform engineer
runs `kubectl logs` on the tbot pod themselves to see the join error.

---

## NetworkPolicy responsibilities

Two distinct policy domains:

| Concern | Owner | Where |
|---|---|---|
| The **operator's own manager pod** | This chart | `templates/networkpolicy.yaml`, gated on `networkPolicy.enabled` (default `true`). Default-deny with explicit allow rules for kube-apiserver egress, DNS egress, and metrics ingress. Operator hygiene per GS convention. |
| The **rendered tbot pods** (one Deployment per `RemoteApp`) | Platform team, hand-written | Selector targets the stable labels listed in §2 above. Egress to `proxyAddr:443`, ingress from approved caller pods. |

The chart's NetworkPolicy is *operator hygiene*, not tenant policy.

---

## Pod-churn caveat (per ADR 0002)

tbot's destination directory is an `emptyDir`, not a PVC. Every tbot
pod restart triggers a fresh join via the kubernetes join method
(projected SA JWT). **Rolling many
`RemoteApp` Deployments simultaneously** — chart upgrades that bump
`tbot.image`, MC-wide node rolls, large-fleet redeploys — **may briefly
hit Central's join-rate limits** while the new pods establish their
identities. tbot retries join with backoff; the disturbance is bounded
but visible.

If join-rate pressure becomes a recurring problem, the operator can
switch to `StatefulSet` + `volumeClaimTemplates` to persist the
renewable cert across restarts. `RemoteApp.spec` exposes none of this,
so the migration is an internal operator change.

---

## Operator RBAC summary

Generated from `templates/rbac.yaml`:

| Resource | Verbs | Notes |
|---|---|---|
| `access.giantswarm.io/remoteapps` | `get;list;watch` | Spec is read-only. |
| `access.giantswarm.io/remoteapps/status` | `get;update;patch` | Slice 4 populates this. |
| `apps/deployments` | `get;list;watch;create;update;patch;delete` | Owned objects. |
| `services`, `configmaps`, `serviceaccounts` | `get;list;watch;create;update;patch;delete` | Owned objects (one SA per RemoteApp, ADR 0004). |
| `pods` | `get;list;watch` | Status synthesis only — never `pods/log`. |
| `coordination.k8s.io/leases` | full | Leader election. |
| `events` | `create;patch` | Status companion. |

**Deliberately not granted:**

* `pods/log` — ADR 0003. The operator never reads tbot container logs.
* `secrets` — ADR 0004. There is no consumer-side token Secret to read.

---

## Generated CRD bundle

`templates/crds.yml` is regenerated from `config/crd/bases/` by
`make update-helm-crds`. CI verifies the chart copy matches the
controller-gen output. Do not hand-edit `templates/crds.yml`.
