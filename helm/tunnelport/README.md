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
| `secrets` | `get;list;watch` | The operator must watch the user-named `tokenRef` Secret in **whatever namespace** the platform team places the `RemoteApp` CR. Per-namespace `RoleBinding`s would force chart consumers to re-grant on every new `RemoteApp` namespace. |
| `pods` | `get;list;watch` | Status synthesis reads pod-level state (readiness, last termination, restart count) of the rendered tbot Deployments to populate `RemoteApp.status.lastError` and the `Ready` condition — also in the CR's namespace, which can be anywhere in the cluster. |
| `access.giantswarm.io/remoteapps` | `get;list;watch` | The CR is cluster-scoped-watched by design (one operator, many consumer namespaces). |

The operator **never** writes to `secrets` (no `create`/`update`/
`patch`/`delete`), and **never** reads `pods/log` (ADR 0003). Owned-
object writes (`deployments`, `services`, `configmaps`) are the only
mutations it performs, all scoped to the CR's own namespace.

### The operator never reads `Secret.Data`

`get;list;watch` on Secrets is a Kubernetes-API blunt instrument — there
is no verb that distinguishes metadata-only access from data access on
Secrets. The "never read data" property is therefore enforced **in
code**, not in RBAC:

* The operator references each token Secret by `(name, key,
  resourceVersion)` only. The Secret's contents are mounted into the
  tbot pod by the kubelet; the operator process never sees them.
* This is verified by an AST-level test:
  [`internal/controller/remoteapp/secret_watch_test.go`](../../internal/controller/remoteapp/secret_watch_test.go)
  parses every Go file in the controller package and fails the build
  if it spots a `Secret.Data` selector expression. Anyone who tries to
  add `secret.Data[...]` to the reconciler gets a red CI signal.

If you are concerned about the breadth of the Secret read grant, your
defence-in-depth options are: (a) audit-logging on `secrets`
`get`/`list` from the operator's ServiceAccount, (b) a Kyverno /
ValidatingAdmissionPolicy guard, or (c) running the operator in a
dedicated MC where the only Secrets in scope are the token Secrets you
already trust the operator with.

### Pod-read scope is filtered at runtime by label selector

The pod read grant is also cluster-scoped, but the controller-runtime
informer cache the operator builds **will be** scoped to a label
selector — the cache subscribes only to pods carrying
`tunnelport.giantswarm.io/role=tbot` (the label the reconciler stamps
onto every tbot pod it renders). In effect the operator process holds
metadata for *its own* tbot pods only, even though the API grant is
broader. (At time of writing this label-selector cache scoping is being
added in a separate bundle; until that lands the cache holds the full
cluster pod set in memory but the *behaviour* is unchanged — status
synthesis still only consults pods owned by `RemoteApp` CRs.)

### Registration-secret rotation: a GitOps responsibility you inherit

Per ADR 0005, every `RemoteApp` joins via `bound_keypair` with
`recovery.mode: relaxed` on the Central-side token resource. The
`tokenRef` Secret carries the **registration secret** Teleport generates
when the token is created (`tctl get token/<name> --format=json |
jq -r '.[0].status.bound_keypair.registration_secret'`), delivered
into the consumer MC out-of-band via your team's chosen pattern
(sealed-secrets, sops, ESO, …).

Operational consequences for the platform team's GitOps pipeline:

* The registration secret is **long-lived** under `relaxed` recovery
  — it remains usable for re-registration indefinitely (ADR 0005
  accepted this trade-off explicitly: it's what enables fleet
  self-recovery without per-bot SRE intervention). Treat the
  consumer-MC Secret as a persistent credential, not a one-shot.
* You own the rotation cadence. If the secret leaks, rotate the bot's
  keypair on Central (which forces the next join to re-bind) and
  re-deliver a fresh registration secret value via the same pipeline.
* When the `tokenRef` Secret content changes, the operator auto-rolls
  the tbot Deployment by stamping the Secret's `resourceVersion` onto
  the pod-template annotation — but it cannot help you if the new
  value is invalid. tbot pods will `CrashLoopBackOff` and
  `status.lastError` will surface the kubelet-visible failure; a human
  has to pick that up.
* The `RemoteApp.spec.tokenRef.name` MUST match the Central-side
  Teleport `kind: token` resource's `metadata.name`. The operator
  renders this string into `onboarding.token` in tbot.yaml as the
  *name of the token resource*, not a value or a path; a name skew
  surfaces as `name "<…>" not found` in tbot logs.

If your GitOps pipeline cannot guarantee a rotation cadence shorter
than your secret-leak detection window, the operator is not the layer
that fixes that — it is the layer that magnifies the consequence.

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
| `networkPolicy.enabled` | `true` | The operator-pod NetworkPolicy. See "NetworkPolicy responsibilities" below for the **rendered tbot pod** policy story (different concern). |

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

### 2. NetworkPolicy for the rendered tbot pods

The operator renders a tbot `Deployment` + `Service` per `RemoteApp` in
the CR's namespace. The chart **does not** render a `NetworkPolicy`
covering those tbot pods — enforcement varies by CNI, baseline policies
vary by org, and a chart-generated policy would interact awkwardly with
whatever the platform team already runs.

The platform team writes the tbot pod policy themselves. The rendered
tbot pods carry stable selectors so a hand-written `NetworkPolicy` can
target them:

| Label | Value |
|---|---|
| `tunnelport.giantswarm.io/role` | `tbot` |
| `tunnelport.giantswarm.io/remoteapp` | `<RemoteApp name>` |

A typical policy allows:

* **Egress to `proxyAddr:443`** — the Teleport proxy host on Central.
  Without this the tbot can't reach Teleport.
* **Ingress from the approved caller pods** to `Service` port =
  `RemoteApp.spec.port`. This is the "who is allowed to use this
  tunnel" decision, made per-app by the platform team.

### 3. Token Secret delivery

`RemoteApp.spec.tokenRef` references a `Secret` carrying the static
join token bound to that `RemoteApp`'s `TeleportBot`. The chart **does
not create, copy, or template** that Secret.

> **Required label:** every token Secret you deliver MUST carry the
> label `tunnelport.giantswarm.io/role=token-secret`. The operator's
> informer cache subscribes to that label selector only — Secrets
> without it are invisible to the operator (no watch events, no `Get`
> via the cache, `status.TokenSecretBound` will stay `False`). This is
> a deliberate cache-scoping measure: it keeps the operator's
> in-memory Secret set narrow, and it keeps unrelated Secrets in the
> namespace out of its blast radius. The operator never writes this
> label itself (it never mutates user-managed Secrets), so your GitOps
> templating, sealing tool, or ExternalSecret manifest must include
> it.

The expected flow on the consumer MC:

1. The platform team produces the join-token value out of band (e.g.
   `tctl tokens add ...` against Central, or a Teleport-Operator-
   managed `TeleportBot` + `tctl bots tokens` step).
2. The token value is stored encrypted in Git via a sealing/sync
   mechanism (sealed-secrets, sops, External Secrets Operator) and
   delivered into the same namespace as the `RemoteApp` CR, **with
   `metadata.labels.tunnelport.giantswarm.io/role` set to
   `token-secret`** so the operator's cache subscribes to it.
3. The `RemoteApp.spec.tokenRef.{name,key}` references the resulting
   `Secret`; the operator mounts it into the tbot pod by **name only**.

The operator never reads the Secret's contents — it only sees
`(name, key, resourceVersion)`. RBAC grants `get;list;watch` on
`secrets` cluster-wide because there is no Kubernetes verb that
distinguishes metadata-only access from data access on Secrets; the
"never read data" property is enforced by code review and maintained by
the operator's reconciler test suite, not by RBAC. ADR 0001 records
this trade-off.

If the Secret is missing when the CR is created, the rendered tbot
pod stays `Pending` (volume mount fails) and
`status.TokenSecretBound = false` — handling the GitOps-race case
where the CR arrives before the Secret.

### 4. Central-side preconditions per `RemoteApp`

For each `RemoteApp` you deploy on the consumer MC, the platform team
must produce on **Central** (typically via the upstream Teleport
Operator, `TeleportBot` + `TeleportRole` + `TeleportToken` CRs):

* a **`TeleportBot`** dedicated to this one app — one bot per
  `RemoteApp`, so a leaked tbot identity reaches only that one app
  (per-app blast-radius isolation, ADR 0001);
* a **role** assigned to that bot whose `app_labels` selector matches
  exactly the one Teleport `App` `RemoteApp.spec.appName` refers to,
  and nothing else;
* a **`bound_keypair` token** (Teleport `kind: token`) bound to that
  bot, with `spec.bound_keypair.recovery.mode: relaxed` per ADR 0005.
  The token resource's `metadata.name` MUST equal
  `RemoteApp.spec.tokenRef.name` — the operator renders that string
  into `onboarding.token` in tbot.yaml as the *name of the token
  resource* on Central. The auto-generated registration secret
  (`status.bound_keypair.registration_secret`) is what ends up in the
  `tokenRef` Secret on the consumer MC.

If any of those is missing or mis-scoped, the tbot pod will fail to
join Central; the operator surfaces this via
`status.lastError` (k8s-visible state only — see ADR 0003), and the
platform engineer runs `kubectl logs` on the tbot pod themselves to see
the join error.

---

## NetworkPolicy responsibilities

Two distinct policy domains:

| Concern | Owner | Where |
|---|---|---|
| The **operator's own manager pod** | This chart | `templates/networkpolicy.yaml`, gated on `networkPolicy.enabled` (default `true`). Default-deny with explicit allow rules for kube-apiserver egress, DNS egress, and metrics ingress. Operator hygiene per GS convention. |
| The **rendered tbot pods** (one Deployment per `RemoteApp`) | Platform team, hand-written | Selector targets the stable labels listed in §2 above. Egress to `proxyAddr:443`, ingress from approved caller pods. |

The chart's NetworkPolicy is *operator hygiene*, not tenant policy.

---

## Pod-churn caveat (per ADR 0004 + 0005)

tbot's destination directory is an `emptyDir`, not a PVC. Per ADR 0005
the operator joins via `bound_keypair` with `recovery.mode: relaxed`,
so every tbot pod restart triggers a fresh **re-registration** against
the registration secret — tbot generates a new keypair, binds it to
the bot on Central, and resumes the tunnel. No SRE intervention is
required (this was the entire point of ADR 0005, replacing the
single-use bot-token model documented in ADR 0004).

The cost is paid against Central, not the consumer MC: **rolling many
`RemoteApp` Deployments simultaneously** — chart upgrades that bump
`tbot.image`, MC-wide node rolls, large-fleet redeploys — **drives
re-registration churn** through the Teleport auth pod. tbot retries
join with backoff and the disturbance is bounded, but at fleet scale
this becomes the dominant join-rate term.

If re-registration churn becomes a recurring problem, the operator
can switch to `StatefulSet` + `volumeClaimTemplates` to persist the
keypair across restarts (ADR 0004 names this as the rendered-object
shape; tracked separately as the perf follow-up). `RemoteApp.spec`
exposes none of this, so the migration is an internal operator
change.

---

## Operator RBAC summary

Generated from `templates/rbac.yaml`:

| Resource | Verbs | Notes |
|---|---|---|
| `access.giantswarm.io/remoteapps` | `get;list;watch` | Spec is read-only. |
| `access.giantswarm.io/remoteapps/status` | `get;update;patch` | Slice 4 populates this. |
| `secrets` | `get;list;watch` | **Metadata-only by code** — see "Token Secret delivery". No `create`/`update`/`patch`/`delete`. |
| `apps/deployments` | `get;list;watch;create;update;patch;delete` | Owned objects. |
| `services`, `configmaps` | `get;list;watch;create;update;patch;delete` | Owned objects. |
| `coordination.k8s.io/leases` | full | Leader election. |
| `events` | `create;patch` | Status companion. |

**Deliberately not granted:**

* `pods/log` — ADR 0003. The operator never reads tbot container logs.
* `secrets` write verbs — ADR 0001. Token Secrets are delivered
  out-of-band; the operator neither produces nor mutates them.

---

## Generated CRD bundle

`templates/crds.yml` is regenerated from `config/crd/bases/` by
`make update-helm-crds`. CI verifies the chart copy matches the
controller-gen output. Do not hand-edit `templates/crds.yml`.
