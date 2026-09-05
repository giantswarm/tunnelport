# tunnelport — context

The project that wraps Teleport's `tbot` + a Kubernetes Service behind a single CR,
so workloads on a Giant Swarm management cluster can reach a Teleport-exposed app
as if it were a local Service — no Teleport SDK in caller code.

## Glossary

### Audience

The CR is authored by **platform engineers** on a consumer MC, not by app teams.
Bot identity is **per-RemoteApp** (one Central-side `TeleportBot` per CR; see
"Auth model" below), and access control is per-MC label match on the role
bound to each bot. All of this assumes a single trusted operator role per
cluster, so self-serve by app teams is out of scope for v1.

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

Each `RemoteApp`'s tbot Deployment authenticates to Teleport via the
**`kubernetes` join method** (see [ADR 0004](./docs/adr/0004-kubernetes-join-method.md)).
The operator renders one consumer-side `ServiceAccount` per CR (name =
CR name, namespace = CR namespace). On Central, each CR has a dedicated
`TeleportBot` whose `ProvisionToken` carries `spec.kubernetes.type:
static_jwks` (with the consumer MC's JWKS embedded inline) and
`spec.kubernetes.allow: [<cr.namespace>:<cr.name>]`. The bot's role is
scoped to exactly that one Teleport application.

This gives **per-app blast-radius isolation**: a compromised tbot pod
yields a credential reaching only that one app. The cost is one paired
action on Central per `RemoteApp` (create `TeleportBot` + role +
`ProvisionToken` with the allowlist entry). The `RemoteApp` references
the Central-side ProvisionToken by name via `spec.tokenName`. The
Teleport cluster name — used as the `aud` claim of the projected SA
JWT — comes from the operator's `--teleport-cluster-name` flag rather
than the CR (see [ADR 0005](./docs/adr/0005-operator-owns-teleport-cluster-and-proxy.md)).
Teleport's `static_jwks` validator pins JWT `aud` to the Teleport
cluster name; the kubelet's default mounted SA token uses the
kube-apiserver's audience and is rejected, so the operator renders a
projected `serviceAccountToken` volume with the right audience.

The SA JWT is auto-rotated by the kubelet's projected-token mechanism;
tbot consumes it transparently. The operator does **not** observe or
manage any rotating secret, surfaces no `TokenSecretBound`-style
condition, and never reads `Secret.Data` of any object on the cluster.

JWKS export from each consumer MC to Central is a platform-team GitOps
step — out of scope for the operator. The smoke harness exports the
JWKS via `kubectl --raw /openid/v1/jwks` inline; production paths use
the platform team's sealed/sync mechanism for Central-side resources.

ADR 0001 (the earlier static-token model) is superseded.

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

### Reconciliation on rotation

The SA JWT mounted into each tbot pod is auto-rotated by the kubelet's
projected-token mechanism. tbot consumes the rotated JWT transparently
on the next join; the operator does not observe or trigger anything on
JWT rotation, because the pod-side credential lifecycle is the kubelet's
concern.

The only Deployment-roll trigger the operator stamps onto the pod
template is `tunnelport.giantswarm.io/config-hash` — a SHA-256 of the
rendered `tbot.yaml`. Anything that changes the on-disk tbot config
(`spec.appName`, `spec.port`, `spec.tokenName`, or the operator-level
proxy / cluster-name flags) rolls the Deployment via its existing
`RollingUpdate` strategy. There is no per-token-rotation annotation,
because nothing on the consumer side "rotates" in a way that needs
operator-driven roll. A change to either operator-level flag rolls
*every* RemoteApp's tbot pods on the next reconcile pass — accepted
as the standard blast-radius cost of MC-wide config (ADR 0005).

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

Conditions on `status`:

- `Ready` — the roll-up: pod readiness wired to tbot's tunnel diag
  endpoint, and (see "End-to-end probe") whether the last request
  through the tunnel got an answer.
- `Reconciled` — operator-internal state, surfaces whether the most
  recent reconcile pass applied every owned object successfully.
  Distinct from `Ready`: a successful reconcile does not imply the
  tunnel is up, and a failed reconcile does not imply the tunnel is
  down (a prior successful apply may still be serving traffic). Reason
  is `ReconcileSucceeded` or `ReconcileError`.

(The earlier `TokenSecretBound` condition is gone with the static-token
model — see ADR 0004.)

### Active TLS verification

Everything above is passive: it derives from k8s-visible state, and
every one of those signals can be green while callers cannot use the
tunnel at all. That is not hypothetical — it is
giantswarm/giantswarm#37521. 40 tunnels served SVIDs whose Teleport-side
`dns_sans` had not followed a namespace rename. tbot joined, the SVID was
issued, ghostunnel bound `:8443`, the sidecar's `TCPSocket` readiness
probe connected (a TCP connect never looks at a certificate), and
`Ready`, `IdentityIssued` and `TunnelServing` were all True for two days.
The only observable was 21 broken MCPServers one hop downstream.

So the operator asks the tunnel what it is serving. A leader-elected
manager Runnable dials every RemoteApp reporting `status.ready: true` at
`<name>.<namespace>.svc.<cluster-domain>:<tls-port>` with `ServerName`
pinned to that FQDN, and verifies the chain against the SPIFFE trust
bundle mounted from the chart's singleton `tunnelport-spiffe-bundle`
Secret. Outcome per RemoteApp: `verified`, `cert_invalid` (connected, no
verified session), `unreachable` (nothing accepted a connection), or
`not_ready` (not probed). It surfaces as the `TunnelVerified` condition
and as `tunnelport_remoteapp_tls_verification`.

Four decisions that carry the design:

- **`cert_invalid` and `unreachable` stay apart.** They have different
  first moves, and collapsing them would return the SAN-drift failure to
  the same bucket as an ordinary outage — which the crashloop alert
  already covers.
- **"Cannot verify" is never "the certificate is bad."** No bundle, no
  round yet, or a tunnel that is not Ready each yield Unknown or a
  distinct result. Blindness is reported separately, via
  `tunnelport_tls_verification_available`, so the check cannot fail
  silently — which would be the same class of bug it exists to catch.
- **`TunnelVerified` does not feed `status.ready`.** `Ready=True` with
  `Verified=False` is a legitimate, highly actionable state (it is the
  incident), and the alert on `cert_invalid` covers it. The end-to-end
  probe below is the one active check that does fold into Ready, for
  reasons of its own.
- **The ADR 0003 position.** ADR 0003 restricts status to k8s-visible
  state for two stated reasons: avoid the `pods/log` RBAC grant, and
  never couple status classification to tbot's log format. An outbound
  TLS handshake against the operator's own rendered Service trips
  neither — it needs no additional RBAC (the RemoteApp list is already
  watched; the bundle is a mounted file, never a Secret read through the
  API) and X.509/TLS is a stable contract, unlike a log format. What it
  does change is that one condition derives from an active probe, which
  is why the honest-unknown rule and the `verification.enabled` off
  switch above are part of the deal rather than nice-to-haves.

### End-to-end probe

The TLS check stops at the consumer-side listener. giantswarm/tunnelport#110
is what lies behind it: a Teleport app service whose gRPC connection to its
auth server had gone stale answered every new app session with `504 Gateway
Timeout` for thirteen minutes. ghostunnel forwarded each request faithfully
(its log shows the bytes going out and the 179-byte 504 coming back), the
handshake verified, the pods were Ready, and four RemoteApps stayed
`Ready/Verified/Identity/Serving` while every caller failed. The only place
the 504 was visible was the producer's `teleport-kube-agent` log.

So, on the same verified session, the operator sends one request through
the tunnel: `GET spec.probe.path` (default `/`) via ghostunnel → tbot
application-tunnel → Teleport proxy → app service → app, with a budget of
`verification.upstream.timeout` (default 10s). What comes back is the
`UpstreamReachable` condition and the
`tunnelport_remoteapp_upstream_probe_status` gauge (value: the HTTP status,
0 for none; label `result`):

- **502, 503, 504 or no response** → `UpstreamReachable=False`, reason
  `UpstreamUnreachable`. These are what the Teleport proxy and app service
  return when *they* cannot reach the next hop.
- **Any other status** → `True`. 200, a 401 from an mcp-oauth resource
  server, 404, even the app's own 500 all prove the path through Teleport
  answers; the probe judges the tunnel, not the app.
- **No request sent** — no round yet, tunnel not Ready, handshake did not
  verify — → `Unknown`, with a reason that says which. "I did not ask" is
  never "nobody answered".

The message carries the status, the probed URL (so a responder can `curl`
it) and, while failing, the time of the last good probe. A Kubernetes Event
(`events.k8s.io/v1`, reason `UpstreamUnreachable` / `UpstreamReachable`)
marks each outage boundary, so a failure one hop downstream can be
attributed to the tunnel path from `kubectl get events` alone. "Boundary"
is judged against the verifier's store, not the condition alone: the
store keeps an outage open across rounds that did not probe (pods rolling
mid-outage turn `False` into `Unknown`) and stamps its end, so a recovery
after such a gap still gets its one `Normal` Event, while `Unknown→True`
on a fresh RemoteApp, a pod restart or a leader handover stays silent.

Two decisions that differ from the TLS check:

- **It folds into `Ready`.** A tunnel whose far end answers nothing but
  504 is not usable, and the consumers of Ready — muster's MCPServer,
  `kubectl get remoteapp` — are where people looked during #110 and saw
  green. `Ready=False` with reason `UpstreamUnreachable` puts the
  degradation where they already look and names the layer;
  `status.lastError` carries the same diagnosis. The consequence inside
  the operator: the verifier's "does the tunnel claim to serve?" gate can
  no longer read `status.ready` (it would un-probe a failing tunnel and
  never see it recover), so it reads pod readiness directly through the
  same `summarizeStatus` the reconciler uses.
- **The load is spread.** ~40 RemoteApps on a consumer MC would otherwise
  open 40 Teleport app sessions at the same instant every two minutes. Each
  round schedules its probes at random offsets within
  `verification.jitter` (default 30s, clamped to half the interval), and
  reuses the TLS probe's connection rather than opening a second one.

Off switch: `verification.upstream.enabled=false` removes the condition
and returns `Ready` to join-level, independently of the TLS check.

### Readiness

`status.ready` is the roll-up consumers read: true when at least one tbot
pod has its k8s readiness probe passing — *and that probe is wired to
tbot's diag endpoint reporting tunnel state*, not just process liveness —
and, with the end-to-end probe on, when the far end answered the last
request sent through the tunnel. `lastError` captures the pod-level
failure summary or, when the pods are fine and the probe is what took
Ready down, the probe's diagnosis.

### Operator config posture

Two Teleport-binding values are operator-level, not per-CR: the
Teleport cluster name (the `aud` claim Teleport's `static_jwks`
validator pins) and the proxy host:port. Both flow into the chart as
required values (`teleport.clusterName`, `teleport.proxyAddr`) and
land on the manager pod as `--teleport-cluster-name` /
`--teleport-proxy-addr`. The operator fails fast at startup if either
is empty.

Everything else stays per-CR: `appName`, `port`, `tokenName`, and the
optional `replicas`. A given consumer MC therefore hosts RemoteApps
that all target the same Teleport cluster; multi-Teleport on one MC
is an explicit non-goal — the answer is a second operator install in
its own namespace. See [ADR 0005](./docs/adr/0005-operator-owns-teleport-cluster-and-proxy.md)
for the rationale (in particular, why the per-CR forward-compat that
ADR 0004 reserved was withdrawn after first production deployment).

Trade-off accepted: a typo in either operator-level flag breaks every
RemoteApp on this MC at once, not just one CR. We accept that because
the values are static per MC, the failure mode is loud and fast
(startup error or 100% join failure across every tbot), and the
upstream chart redeploy that introduces the typo is also the loudest
signal.

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

### Central-side provisioning (out of scope for the operator)

The platform team creates the per-CR `TeleportBot` + `ProvisionToken` on
Central via their existing Central-config GitOps. The ProvisionToken
carries `spec.kubernetes.allow: [<cr.namespace>:<cr.name>]` and
`spec.kubernetes.static_jwks.jwks` (the consumer MC's JWKS, exported
once per consumer MC and refreshed on signing-key rotation). Exporting
that JWKS is a one-time-per-MC platform-team step; the operator stays
consumer-side-only and never talks to Central. See ADR 0004.

### Operator topology

- One operator Deployment per consumer MC, running in its own namespace
  (e.g. `tunnelport-system`).
- Watches `RemoteApp` CRs cluster-wide. Renders `ServiceAccount` +
  `Deployment` + `Service` + `ConfigMap` (tbot config) **in the CR's
  namespace** — no cross-namespace references.
- RBAC: cluster-scoped read on `RemoteApp`; namespace-scoped write on the
  rendered resource types (including `serviceaccounts`) via a single
  ClusterRole. **Does not** grant any verbs on `secrets`.
- The chart ships the operator and CRD only. It does **not** create
  `RoleBinding`s granting create-`RemoteApp` to anyone — that's an explicit
  per-cluster decision the platform team makes via their own RBAC.

### NetworkPolicy

Two distinct things, decided differently:

- **For tenant resources (the rendered tbot pods)**: the operator does **not**
  render a `NetworkPolicy`. Reason: enforcement varies by CNI, baseline
  policies vary by org, and generating one creates an awkward interaction
  with whatever the platform team already runs. The chart README documents
  the canonical pod labels (`tunnelport.giantswarm.io/role=tbot`,
  `tunnelport.giantswarm.io/remoteapp=<name>`) so the platform team's
  hand-written `NetworkPolicy` has a stable selector to target. tbot egress
  to `proxyAddr:443` and ingress from approved caller pods is the platform
  team's to enforce.
- **For the operator's own manager pod**: the chart **does** render a
  `NetworkPolicy` locking down the operator Deployment, per GS convention
  for kubebuilder operators. Default-deny with explicit allow rules for
  kube-apiserver egress and metrics scraping ingress. This is operator
  hygiene, not tenant policy — the rule above only applies to the tbot
  pods we render on behalf of `RemoteApp`s.

### Roles (cluster topology)

- **Central MC** — runs the Teleport control plane (proxy + auth). Hosts the
  upstream Teleport Operator and the `TeleportApp` / `TeleportBot` CRs.
- **Producer MC** — runs `teleport-kube-agent` (Application Service mode) and
  publishes apps to Central. Managed elsewhere; out of scope.
- **Consumer MC** — runs *this* operator, plus the `tbot` Deployments and
  Services it renders. Workloads on this MC call the Service.
