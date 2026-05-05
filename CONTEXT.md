# tunnelport — context

The project that wraps Teleport's `tbot` + a Kubernetes Service behind a single CR,
so workloads on a Giant Swarm management cluster can reach a Teleport-exposed app
as if it were a local Service — no Teleport SDK in caller code.

## Glossary

### Audience

The CR is authored by **platform engineers** on a consumer MC, not by app teams.
The bot identity is shared at MC scope (one `TeleportBot` per consumer MC), the
join trust is cluster-level (kubernetes SA JWT), and access control is per-MC
label match. All of those assume a single trusted operator role per cluster, so
self-serve by app teams is out of scope for v1.

### `RemoteApp`

The CR a platform engineer writes. Declares "this Teleport-exposed app should
be reachable on this MC as a local `Service`." Group:
`access.giantswarm.io/v1alpha1`.

Naming rationale: "remote" carries the consumer-side framing; "app" matches
Teleport's own term for what's on the other end; the network-direction word
"egress" is dropped because it collides with `NetworkPolicy.egress` and leaks
implementation. The project name `tunnelport` describes the *mechanism* (a
tbot tunnel exposed on a port); `RemoteApp` describes what the user *declares*.

### Auth model

Each `RemoteApp` has its own static **join token** on Central, bound to a
dedicated `TeleportBot` whose role is scoped to exactly that one app. The
token is delivered to the consumer MC out-of-band (GitOps + secret sync)
as a `Secret`, and the `RemoteApp` references it via `tokenRef`.

This gives **per-app blast-radius isolation**: if one tbot pod is compromised,
the leaked identity reaches only that one app. The cost is one paired action
on Central per `RemoteApp` (create `TeleportBot` + role + token).

The `kubernetes` join method is rejected for v1: it would require per-CR
`ServiceAccount` orchestration on the consumer side and pinning each token's
allowed-subject on Central, for the same isolation property the static-token
approach already gives.

### tbot topology

One tbot Deployment per `RemoteApp`. tbot is one identity per process, and
identities are per-app (auth model above), so multi-tunnel-per-tbot is not an
option. Replicas of the same `RemoteApp`'s Deployment all use the same token
and join as separate instances of the same `TeleportBot`.

### `Service` exposure

The operator renders a `Service` fully derived from the `RemoteApp`:
name = CR name, port = `.spec.port`, type = `ClusterIP`. No knobs on the CR.
This is deliberate — `LoadBalancer` / `NodePort` would defeat the local-only
purpose, and `Headless` has no meaningful use here (every tbot pod's tunnel
terminates at the same Teleport-side app). If a real headless need surfaces,
add it then; don't ship the knob without a use case.

### Reconciliation on token rotation

The operator **does** auto-roll the tbot Deployment when the referenced
`tokenRef` Secret content changes. Mechanism: a watch on `Secret`s triggers
reconcile, and the operator stamps the Secret's `resourceVersion` onto the
pod-template annotation; the Deployment's RollingUpdate strategy handles the
rest.

Reason: in this operational model, token changes typically reflect bot
identity changes on Central — at which point the running tbot's renewable
certs are invalid anyway, and waiting for them to fail naturally is worse
than rolling immediately. Even in the rarer "same bot, new token value"
case, auto-rolling is a bounded, predictable disturbance.

The operator never reads the Secret's contents — it only references
`(name, key, resourceVersion)`, keeping itself out of the secret-handling
blast radius.

### Cert cache

tbot's destination directory is an `emptyDir` mounted into the pod. The
renewable cert lives only for the pod's lifetime — every pod restart triggers
a re-join with the token. This is a deliberate trade against `StatefulSet` +
`PVC` complexity (PVC provisioning, GC on CR deletion, StorageClass coupling
to the consumer MC). The visible cost is join-rate pressure on Central during
MC-wide pod churn (chart upgrades, node rolls); document this in the chart
README.

If frequent re-joins become a real complaint, switch to `StatefulSet` later —
`RemoteApp.spec` doesn't expose any of this, so it's an internal change.

### Failure surfacing

The operator reports failure from k8s-visible state only — no pod log
reading. `status.lastError` contains things like
`"CrashLoopBackOff (5 restarts), last termination: Error (137)"`. To see why,
the platform engineer runs `kubectl logs` themselves. Reasons: avoids the
`pods/log` RBAC grant, and prevents future contributors from coupling status
classification to tbot's log format (which isn't a stable API). The operator
performs no retries or auto-recovery beyond what the kubelet's restart policy
already provides.

Conditions on `status`: `Ready` (mirrors pod readiness, which is wired to
tbot's tunnel diag endpoint), and `TokenSecretBound` (the named Secret + key
exist). That's it.

### Readiness

`status.ready` is **tunnel-level**: true when at least one tbot pod has its
k8s readiness probe passing, *and that probe is wired to tbot's diag endpoint
reporting tunnel state* — not just process liveness. End-to-end reachability
(Central → producer → app) is not verified by the operator; `lastError` only
captures failures visible on the consumer side.

### Operator config posture

The operator chart has no required cluster-wide config. Every per-CR
parameter — `proxyAddr`, `appName`, `tokenRef` — lives on the `RemoteApp`
itself. Trade-off accepted: the same `proxyAddr` will be repeated across most
CRs in a cluster, and a typo affects only that one CR. Goal is "install the
chart once, then everything is per-CR GitOps."

### Scope of "app"

`RemoteApp` covers **Teleport Application Service** apps only — TCP and HTTP
services exposed via `teleport-kube-agent` in app mode, consumed through tbot's
`application-tunnel`. Database access, Kubernetes access, and SSH all use
different tbot modes and different role fields on Central, and are out of
scope for v1.

If those use cases land later, they ship as their own CRDs (`RemoteDatabase`,
`RemoteKubeCluster`), not as a polymorphic discriminator on `RemoteApp`. The
field sets don't generalize cleanly and a `type:` switch would invite
half-applicable defaults.

### Pod template defaults

- `replicas` defaults to `1`. The Deployment uses
  `strategy.rollingUpdate: { maxSurge: 1, maxUnavailable: 0 }`, so the new
  pod must become `Ready` (tunnel established) before the old pod is
  terminated — no caller-visible downtime for *new* connections during a
  roll. In-flight long-lived TCP/gRPC streams on the old pod still terminate
  when it eventually shuts down; that's true at `replicas: 2` too. HA beyond
  the rolling-update window is opt-in via `spec.replicas: 2`. Bonus property:
  if Central is briefly unreachable during a roll, the new pod stays
  `NotReady` and the old pod keeps serving — graceful auto-rollback on
  transient upstream failures.
- The tbot **image** comes from a Helm value on the operator chart, not from
  the CR. Single global version per consumer MC; the platform team upgrades
  tbot via chart values.
- The tbot container's **resource requests/limits** come from Helm values,
  not from the CR and not hardcoded. Cluster-wide defaults the platform team
  can tune; per-CR sizing is busywork because the tbot sidecar's profile
  doesn't vary per app.

### Token Secret delivery

The platform team provides the `tokenRef` Secret out-of-band — typically via
GitOps with a sealed/sync mechanism. The operator never creates, copies, or
reads the Secret's contents. If the Secret is missing when the CR is created,
the rendered `Deployment` stays `Pending` (volume mount fails) and
`status.TokenSecretBound = false` — handling the GitOps-race case where the
CR arrives before the Secret.

### Operator topology

- One operator Deployment per consumer MC, running in its own namespace
  (e.g. `tunnelport-system`).
- Watches `RemoteApp` CRs cluster-wide. Renders `Deployment` + `Service` +
  `ConfigMap` (tbot config) **in the CR's namespace** — no cross-namespace
  references.
- RBAC: cluster-scoped read on `RemoteApp` and `Secret`; namespace-scoped
  write on the rendered resource types via a single ClusterRole.
- The chart ships the operator and CRD only. It does **not** create
  `RoleBinding`s granting create-`RemoteApp` to anyone — that's an explicit
  per-cluster decision the platform team makes via their own RBAC.

### NetworkPolicy

The operator never renders a `NetworkPolicy` — not even a default-deny.
Reason: enforcement varies by CNI, baseline policies vary by org, and
generating one creates an awkward interaction with whatever the platform team
already runs. The chart README documents the canonical pod labels
(`tunnelport.giantswarm.io/role=tbot`,
`tunnelport.giantswarm.io/remoteapp=<name>`) so the platform team's
hand-written `NetworkPolicy` has a stable selector to target. tbot egress to
`proxyAddr:443` and ingress from approved caller pods is the platform team's
to enforce.

### Roles (cluster topology)

- **Central MC** — runs the Teleport control plane (proxy + auth). Hosts the
  upstream Teleport Operator and the `TeleportApp` / `TeleportBot` CRs.
- **Producer MC** — runs `teleport-kube-agent` (Application Service mode) and
  publishes apps to Central. Managed elsewhere; out of scope.
- **Consumer MC** — runs *this* operator, plus the `tbot` Deployments and
  Services it renders. Workloads on this MC call the Service.
