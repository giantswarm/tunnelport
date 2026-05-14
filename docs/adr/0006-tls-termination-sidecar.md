# TLS termination on the tunnel Service via stunnel sidecar

**Status: superseded by ADR 0007 before deployment.** This design was
drafted, implemented in branch, and withdrawn before any commit
landed in `main` or any consumer MC ran it. ADR 0007 records the
SPIFFE-via-Teleport design that ships instead and the reasoning that
flipped the decision. ADR 0006 is preserved here as the considered
alternative; do not implement from it.

The tunnel Service grows a TLS listener served by an `stunnel` sidecar
container colocated with `tbot` in every RemoteApp pod. The sidecar
listens on `8443`, terminates TLS, and forwards plaintext to tbot's
existing `application-tunnel` on `127.0.0.1:<spec.port>`. The Service
keeps the current plaintext port `8080` and adds `tls/8443` alongside
it. The CR's contract is unchanged: no new fields on `RemoteAppSpec`,
no new chart values beyond the sidecar image and the cert-manager
references named below. Deprecation of the plaintext port is deferred
to a later ADR after consumers have migrated.

Server certificates come from a cert-manager CA chain rooted in GS's
standard `selfsigned-giantswarm` ClusterIssuer, which cert-manager-app
v2.22 already ships on every MC. The chart renders one bootstrap
`Certificate` (`tunnelport-internal-ca`, `isCA: true`, 10y, secret
materialised in cert-manager's `cluster-resource-namespace` — default
`kube-system`) signed by `selfsigned-giantswarm`, plus a derived
`tunnelport-ca` `ClusterIssuer` of `kind: ca` that references that
secret. The operator, on each reconcile, emits a leaf `Certificate`
per RemoteApp signed by `tunnelport-ca`, with SAN
`<cr.name>.<cr.namespace>.svc.cluster.local`. The stunnel container
mounts the resulting Secret read-only and serves it on `8443`. Consumer
trust distribution piggy-backs on cert-manager: a second `Certificate`
re-issued from `tunnelport-ca` in each consumer namespace materialises
the CA bundle as a Secret the consumer pod mounts, and muster's
existing `oauth.server.dex.caFile` knob loads it into a fresh
`*x509.CertPool`.

Four paths were investigated. The three rejected are named below.

Upstream Teleport patch giving `tbot` a TLS listener natively was the
cleanest long-term answer. `lib/tbot/service_application_tunnel.go`
has no TLS config today; a ~30 LOC PR would add one. Nothing blocks
that PR being written and merged, but the round-trip — upstream PR
review, a Teleport release, a GS tbot image bump — is weeks-to-months
and a hard external dependency. Rejected as the right answer that
doesn't unblock the consumer today; revisit when the upstream PR
lands and prune the sidecar in a follow-up ADR.

SPIFFE via Teleport's `workload-identity-x509` service was the
zero-cert-manager option: tbot itself can mint a server certificate
signed by Teleport's internal SPIFFE CA. The trust root is then
producer-side — Teleport central's SPIFFE CA — but the cert is
presented to a verifier in the consumer cluster. Cross-cluster trust
through Teleport central's SPIFFE CA means every consumer MC has to
bundle and rotate that CA out-of-band, coupling consumer trust roots
to producer infrastructure. Rejected as semantically inverted: the
server cert lives in the consumer cluster, and a consumer-cluster
trust anchor (a CA materialised on the consumer MC by cert-manager)
is the right one to verify it.

A chart-referenced pre-existing wildcard Secret — no per-CR
`Certificate` object — would have been smaller in the operator and
in the chart. Rejected because the standing objection that motivated
the wildcard shape ("no internal Issuer is reliably available on a
GS MC") is gone since cert-manager-app v2.22 ships
`selfsigned-giantswarm` everywhere. Per-CR Certificates from a
CA-backed Issuer earn correct SANs
(`<cr.name>.<cr.namespace>.svc.cluster.local`, not a wildcard) and
free leaf rotation; the wildcard Secret gave up both.

A muster-side opt-out — extending the muster / mcp-oauth library
with an `AllowInsecureHTTP` flag — was the fastest path by line
count, roughly 15 LOC across two repos. Rejected for three reasons.
First, it requires forking or extending an external library to
solve a tunnelport-side abstraction leak. Second, it only solves
the muster → tunnel case; any future consumer of the tunnel Service
needs the same opt-out plumbing reinvented in its stack. Third,
the http-only contract of the tunnel Service is the leaky
abstraction worth fixing once, not papering over per-caller. The
contrarian's strongest point — that in-cluster wire encryption is
mostly security theater on a GS MC, given existing CNI policy and
the absence of cross-pod traffic interception in the threat model
— is conceded: the motivation for this ADR is contract cleanliness
and unblocking standards-compliant clients, not a security policy
change. Naming that explicitly here so the next reader does not
inherit a phantom threat model the design does not actually carry.
`trust-manager` was considered as a way to distribute the CA bundle
to consumers and rejected on the same "no concrete user" grounds as
ADR 0004's `oidc` discussion: trust-manager is not deployed anywhere
in `giantswarm/*` today, and the existing muster `caFile` knob plus
a per-consumer-namespace re-issued Certificate is sufficient until
the consumer plurality forces a sharper answer.

Operational accounting. New objects on the consumer MC, per
RemoteApp: one `Certificate` (the leaf), the Secret cert-manager
materialises from it, and one `Certificate` per consumer namespace
that distributes the CA bundle. Chart-wide: one `Certificate`
(`tunnelport-internal-ca`) in cert-manager's
`cluster-resource-namespace`, and one `ClusterIssuer`
(`tunnelport-ca`). New container: `stunnel` in every tbot pod,
sized for TLS-termination duty only — cert-manager itself is
already running on every consumer MC, so no new operator is
introduced. New RBAC on the manager ClusterRole:
`get,create,update,patch` on `certificates.cert-manager.io` and
the `cert-manager.io/v1` scheme registration in the operator's
controller-runtime scheme. One extra owned-object kind shows up in
the reconcile loop alongside `ServiceAccount`, `Deployment`,
`Service`, and `ConfigMap`. Rotation: cert-manager renews the leaf
in place ahead of expiry; the kubelet propagates the updated Secret
to the pod's mount; stunnel re-reads the cert files on change.
That last property must be verified against the stunnel build the
chart selects and documented in the chart README — falling back to
a Deployment roll on cert rotation is acceptable but should be a
deliberate choice, not an accident. The CA root itself is 10y,
rotated manually via a documented runbook; ClusterIssuer-style
internal CAs at 10y are standard GS hygiene.

Forward-compatibility hooks are named but explicitly not added.
Per-CR `spec.tlsEnabled` and `spec.caIssuerRef` would shift the
TLS-on/off decision and the Issuer reference into the CR, allowing
heterogeneous TLS posture across RemoteApps on a single consumer
MC. The current plurality (every RemoteApp on a given MC has the
same answer) makes them busywork today, in the same spirit
ADR 0004 rejected per-CR knobs whose forward-compat use case had
no concrete user. The CRD stays `v1alpha1` and the chart keeps
`helm.sh/resource-policy: keep` on the CRD object, so adding
either field later is a values-migration cost, not a CRD version
bump.

Migration shape is additive. v1 of the sidecar ships with the
plaintext port `8080` still served, the TLS port `8443` added,
and consumers free to pick either. Once every known consumer of
a given RemoteApp has migrated to `8443`, a follow-up ADR will
drop `8080` from the rendered Service — an internal change to
the operator with no CR-API impact. The CRD stays `v1alpha1`;
no production consumers need migrating today.
