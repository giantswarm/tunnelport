# `kubernetes` join method per RemoteApp; supersedes ADR 0001

Each `RemoteApp`'s tbot Deployment authenticates to Teleport via the
`kubernetes` join method. The operator renders one consumer-side
`ServiceAccount` per CR (name = CR name, namespace = CR namespace). Each
CR's Central-side `TeleportBot` has a `ProvisionToken` with
`spec.kubernetes.type: static_jwks` (the consumer MC's JWKS embedded
inline) and `spec.kubernetes.allow: [<cr.namespace>:<cr.name>]`. Per-app
blast-radius isolation is unchanged: a compromised tbot pod yields a
credential reaching only that one app, because the bot's role is still
scoped to one Teleport application.

This supersedes ADR 0001. ADR 0001's accounting (extra ServiceAccount
orchestration on the consumer side, same 1-paired-action cost on Central)
still holds — what changed is the consumer-side trade-off. An
operator-rendered ServiceAccount is strictly less operational surface than
a sealed-sync Secret pipeline per RemoteApp: the platform team no longer
runs token-Secret rotation, the SA JWT auto-rotates via the kubelet's
projected token, and tbot consumes it transparently. The operator stops
watching user-managed Secrets, stops surfacing `TokenSecretBound`, and
sheds the `secrets` ClusterRole verbs along with the Secret-watch
field-index, predicate, mapper, and `AnnotationTokenSecretVersion`
pod-template annotation.

Trust mode is `static_jwks`. `in_cluster` is rejected: it requires Teleport
(Central MC) to reach each consumer MC's `tokenreviews` endpoint, which is
incompatible with GS's typical network topology (consumer kube-apiservers
are private). The cost `static_jwks` carries today is that every
`ProvisionToken` embedding a consumer MC's JWKS must be re-rendered when
that MC rotates its signing keys — `oidc` would eliminate this. `oidc`
(Teleport v18+) remains a future opt-in if that re-render cost becomes
painful — additive change to this ADR, not a re-decision.

Out of scope for the operator: exporting each consumer MC's JWKS to
Central. That is a platform-team GitOps step — same sealed/sync pipeline
the platform team already runs for Central-side resources. The smoke
harness (`hack/smoke/run.sh`) dumps the consumer cluster's JWKS via
`kubectl --raw /openid/v1/jwks` and `sed`s it into the ProvisionToken
before `tctl create`; production paths are platform-team-owned.

Implicit operational coupling worth naming: the ProvisionToken's
`kubernetes.allow` list pins `<cr.namespace>:<cr.name>`, so the platform
engineer must commit to the CR's name+namespace before creating the bot
on Central. This is the same coupling the static-token method had (the
Secret name had to match `spec.tokenRef.name`), shifted from a Secret
name into the `kubernetes.allow` `service_account` entry, and now pins
CR namespace **as well as** name. The old token method only required the
Secret *name* to match.

CRD change: `spec.tokenRef` (Secret name + key) is removed and replaced
with two fields:

- `spec.tokenName string` — the Central-side `ProvisionToken` name.
- `spec.clusterName string` — the Teleport cluster name. Required because
  Teleport's `static_jwks` validator pins the SA JWT's `aud` claim to
  the Teleport cluster name (`a.GetDomainName()` in
  `lib/auth/join_kubernetes.go`). The kubelet's default automounted SA
  token at `/var/run/secrets/kubernetes.io/serviceaccount/token` carries
  the kube-apiserver's default audience and is rejected. The operator
  therefore renders a projected `serviceAccountToken` volume on the tbot
  pod with `audience: <cr.spec.clusterName>` and points `tbot` at it via
  `KUBERNETES_TOKEN_PATH`, mirroring the upstream `tbot` Helm chart
  pattern. The projected token's TTL is 600s — under Teleport's 30-minute
  `static_jwks` ceiling and above the kubelet's 10-minute minimum, so any
  future tuning must stay in `[600s, 1800s]`. The `aud` value is per-CR
  (not a chart-wide knob) so multi-Teleport-cluster setups on a single
  consumer MC stay possible without re-installing the chart — a
  forward-compatibility property, not a sizing one.

The `TokenSecretBound` status condition is removed. Two conditions now
surface:

- `Ready` — driven by tbot pod readiness against the tunnel diag
  endpoint; reflects join-level state (the tunnel either reaches Teleport
  and serves traffic, or it does not).
- `Reconciled` — operator-internal state: True when every owned-object
  apply in the most recent reconcile pass succeeded, False (with
  `Reason: ReconcileError`) when any failed. Distinct from `Ready` on
  purpose: a successful reconcile does not imply the tunnel is up
  (tbot may still be starting), and a failed reconcile does not imply
  the tunnel is down (a prior successful apply may still be serving
  traffic). Surfacing both gives platform engineers an unambiguous
  signal whether the operator itself is making progress, independent of
  whether the join is working.

The CRD stays `v1alpha1` — the alpha contract permits breaking changes —
and there are no production consumers to migrate.
