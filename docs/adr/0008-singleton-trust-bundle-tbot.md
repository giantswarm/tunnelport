# Singleton trust-bundle tbot replaces per-CR bundle Secrets

**Status: supersedes the "Trust bundle distribution to consumers"
paragraph of ADR 0007.** ADR 0007 still governs SVID issuance for the
per-CR `application-tunnel`/`workload-identity-x509` bots and the
ghostunnel sidecar; this ADR replaces only how the SPIFFE trust bundle
is distributed to consumer pods. The per-CR `<cr.Name>-spiffe-bundle`
Secret, the second `workload-identity-x509` service block in
`tbot_config.go` that writes it, and its per-CR `Role` /
`RoleBinding` are removed in the same change. No production consumer
references the per-CR Secret name today — the smoke probe at
`hack/smoke/consumer/tls-probe.yaml:39` is the only call site — so
the migration is one-cut, not multi-release.

The chart grows one additional Deployment, `tunnelport-trust-bundle`,
colocated with the operator in the release namespace. It runs a single
tbot whose only purpose is to materialise `svid_bundle.pem` into one
chart-managed Secret, `tunnelport-spiffe-bundle`, also in the release
namespace. Every consumer in that namespace — muster first, any
future co-deployed consumer next — mounts that one Secret and uses
the file as its CA bundle for verifying tunnel SVIDs from any
RemoteApp on the cluster. The per-RemoteApp tbots stop emitting a
Kubernetes-Secret destination; their `workload-identity-x509` service
collapses to the single `directory` destination ghostunnel already
reads from.

The argument is that every per-CR bundle Secret on the same MC
contains identical bytes. ADR 0007's "Trust distribution simplifies
asymmetrically" paragraph already named the property: every consumer
MC paired to the same Teleport cluster shares one trust bundle.
Writing that bundle once per RemoteApp materialises N copies of the
same content as N distinct Kubernetes objects, costs N
`workload-identity-x509` renewals every cycle (one per producer pod),
and forces every consumer to know which RemoteApp's Secret to mount.
The redundancy is invisible while there is one RemoteApp per cluster;
it becomes the operator-experience cost at N>1, which is precisely
the deployment shape (muster + several RemoteApps in one namespace)
this design now has to support.

Same-namespace co-deployment is the precondition that makes this
shape free of fan-out. Kubernetes Secrets are namespaced, and the
v18 `workload-identity-x509` service supports exactly one destination
per service block; cross-namespace distribution therefore requires
either N service blocks (N SVID renewals to copy identical bytes
into N Secrets), an operator mirror controller, or a third-party
fan-out (trust-manager). The committed deployment model puts muster
and tunnelport in the same release namespace, so none of those
mechanisms are introduced. If a second consumer namespace appears
later, the design grows by adding a second service block on the
singleton tbot — additive, no controller, no new dependency — and
the chart's `trustBundle.extraNamespaces` value names them. That
extension point is named here; it is not built.

Three alternatives were considered and rejected.

A controller in the operator that watches a single source Secret and
mirrors its `Data` into named target namespaces was the smallest
machinery that survives a future move to multi-namespace consumers.
Rejected because the same-namespace deployment model makes the
mirror redundant today, and the singleton-tbot-with-extra-service-
blocks growth path solves the multi-namespace case without a new
controller. The contrarian's case — that a mirror controller is a
better long-term shape than a tbot service block per namespace —
is acknowledged: it trades issuance load for controller complexity.
The pivot point is the consumer-namespace count. At the current
plurality (one), neither is justified; at N≈3 the mirror gets cheaper;
the design commits to revisiting before adding more than ~3 service
blocks.

trust-manager fanning out a `Bundle` resource from the singleton
Secret to consumer namespaces was the option ADR 0006 considered
and rejected on "no concrete user" grounds; the same rejection
applies here for the same reason — same-namespace deployment means
no consumer asks for cross-namespace fan-out. Adding trust-manager
as a chart dependency for a code path that never executes on the
deployment topology we ship is the wrong trade.

Keeping the per-CR Secrets as a deprecated path during a release
cycle was considered. Rejected because no consumer references them
outside the in-repo smoke probe (verified by grep across
`muster/`, `graveler-remoteapps/`, and the consumer side of
`hack/smoke/`). A single-cut delete drops `renderTrustBundleSecret`,
`renderTrustBundleRole`, `renderTrustBundleRoleBinding`, the
ServiceAccount's Secret-scoped RBAC, and the second
`workload-identity-x509` service block in `tbotConfig` —
net code reduction in `internal/controller/remoteapp/`. A staged
deprecation would keep all of that for one release for nobody's
benefit.

The operator-side delta. `tbot_config.go:147-171` loses the
second `workloadIdentityX509Service` call (the
`kubernetes_secret` destination). `render.go:712-849`'s
`trustBundleSecretName`, `renderTrustBundleSecret`,
`renderTrustBundleRole`, and `renderTrustBundleRoleBinding` are
deleted along with their unit tests. The per-CR ServiceAccount's
namespaced Secret RBAC goes with them; the per-CR Role/RoleBinding
applies remove from the reconcile graph. Owned-object kinds drop
from five to three (ConfigMap, Service, Deployment — plus
ServiceAccount, no longer needing the trust-bundle Role/RoleBinding).
`render_test.go`'s coverage of the trust-bundle Secret/Role/RoleBinding
inverts: the assertion becomes "these objects are not rendered".

The chart-side delta. `helm/tunnelport/templates/` grows
`trust-bundle-deployment.yaml`, `-serviceaccount.yaml`,
`-role.yaml`, `-rolebinding.yaml`, and `-configmap.yaml`. All
gate on `.Values.trustBundle.enabled` (default `true`). The
ConfigMap carries a fixed tbot.yaml with one
`workload-identity-x509` service of `kubernetes_secret`
destination pointing at `.Values.trustBundle.secretName`
(default `tunnelport-spiffe-bundle`). The Deployment is a
single-replica tbot, identical image to the per-CR bots'
image, joining Teleport with the singleton bot's ProvisionToken
name from `.Values.trustBundle.tokenName`. The Role is scoped
to `resourceNames: [tunnelport-spiffe-bundle]` so the singleton
bot cannot read or write any other Secret. The Deployment
declares an explicit ownerless-by-design posture: no owner CR
(this is a chart-level singleton, not a per-CR object), so Helm
owns its lifecycle, not the operator.

The Teleport-central delta. One new ProvisionToken
`tunnelport-trust-bundle`, kubernetes join method, allowing the
singleton tbot's ServiceAccount (same shape as the per-RemoteApp
tokens from ADR 0004). One new `WorkloadIdentity`
`tunnelport-trust-bundle`, labelled with a distinct selector key
— `trust-bundle: tunnelport`, not the per-CR `remoteapp:` —
so per-CR bot identities and the trust-bundle bot identity cannot
accidentally cross-match. The new WorkloadIdentity carries no
DNS SANs, only the SPIFFE URI: the SVID is never presented to a
verifier, so the DNS-SAN-stamping rationale from ADR 0007 does
not apply. One bot role grants
`workload_identity_labels: {trust-bundle: tunnelport}` to the
new bot. Three new manifests in `hack/smoke/teleport/` for the
smoke shape; the same shape in `giantswarm/teleport-tac` for
production. The existing per-RemoteApp Teleport resources are
unchanged.

Smoke shape. `hack/smoke/consumer/tls-probe.yaml:39` flips from
`smoke-app-spiffe-bundle` to `tunnelport-spiffe-bundle`. The
initContainer wait path is unchanged. The smoke now proves what
this ADR asserts — the singleton bundle verifies SVIDs minted by
a different bot for a different WorkloadIdentity — which is the
load-bearing claim: same Teleport SPIFFE CA, one bundle, every
SVID validates. A failing smoke under this configuration falsifies
the redundancy claim and reverts this ADR.

Risks worth naming, all surfaceable at implementation time, none
blocking the architectural commitment.

The singleton tbot is a single bootstrap point. If it cannot reach
Teleport on first chart install, no consumer can verify any SVID
until the bundle Secret first materialises. Per-CR SVID issuance is
unaffected — those bots have their own join paths. Mitigation is
inherent: once the Secret materialises, it persists across tbot
restarts; trust roots rotate on Teleport's CA schedule (years), so
steady-state availability of the singleton bot is not on the hot
path. Bootstrap-time visibility — the bot reaches Ready and the
Secret has a non-empty `svid_bundle.pem` key — is the operational
property to monitor, not steady-state uptime.

The singleton tbot mints an SVID nobody consumes. v18.x
`workload-identity-x509` does not separate bundle emission from
issuance — the bundle is a side-effect of issuance, as ADR 0007's
2026-05-13 amendment recorded after the `spiffe-trust-bundle`
framing was retracted. The cost is one SVID issuance per
`renewal_interval` (20m), Teleport-side load negligible, and a
file on disk in the singleton pod that nothing reads. If a future
tbot exposes a bundle-only service, the chart's ConfigMap is the
one place to switch to it; the architectural commitment of this
ADR is unaffected.

The single-namespace assumption is a forward-compat hook, not a
hard limit. The ADR commits to: same release namespace today; the
singleton tbot grows additional service blocks when a second
consumer namespace appears, parametrised by a chart value
(`trustBundle.extraNamespaces`); a mirror controller is the
fallback if that list grows past ~3. The CRD stays `v1alpha1` and
no per-CR knob is added — same posture as ADR 0005 / 0007 on
"no per-CR forward-compat knob without a concrete user".

Migration shape. One PR. Operator code shrinks (deletes outnumber
additions). Chart code grows by five templates plus three values.
Teleport-central gets three new manifests. Smoke flips one Secret
name in one YAML. No CRD change. No production consumer migration —
no consumer references the per-CR Secret name. The deploy story is
chart upgrade plus Teleport-side GitOps; no manual cluster
intervention.
