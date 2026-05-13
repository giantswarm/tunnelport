# SPIFFE-via-Teleport workload identity for tunnel-Service TLS

**Status: supersedes ADR 0006.**

**Amendment 2026-05-13:** Prototype-time verification cleared the
four checklist items called out below. The JWKS-conversion fallback
and the separate `spiffe-trust-bundle` service framing in the
original draft were inaccurate for tbot v18.x — the
`workload-identity-x509` service emits the trust bundle as
`svid_bundle.pem` (PEM) alongside the SVID, in the same destination.
References to a distinct `spiffe-trust-bundle` service have been
removed; the JWKS risk has been dropped from the Risks paragraph.

The tunnel Service grows a TLS listener served by a `ghostunnel`
sidecar colocated with `tbot` in every RemoteApp pod. The server
certificate the sidecar presents is a SPIFFE X.509-SVID minted by
`tbot`'s `workload-identity-x509` output, signed by Teleport central's
SPIFFE CA. Consumers verify the SVID against a SPIFFE trust bundle
emitted by `tbot`'s `workload-identity-x509` output as
`svid_bundle.pem` alongside the SVID, mounted into the consumer
pod. The sidecar listens on `8443`, terminates TLS, and
forwards plaintext to the existing `application-tunnel` on
`127.0.0.1:<spec.port>`. The Service grows `tls/8443` alongside the
existing plaintext port `8080`; deprecation of the plaintext port is
deferred to a later ADR. No fields are added to `RemoteAppSpec`; the
chart gains values naming the Teleport `WorkloadIdentity` template
and the ghostunnel image. ADR 0005's posture applies — operator-level
configuration only, no per-CR knob until a concrete multi-posture
user appears.

The argument for SPIFFE-via-Teleport over the cert-manager-on-consumer
design ADR 0006 considered is one of trust reuse. Every `tbot` pod on
a consumer MC already extends transitive trust to Teleport central's
CA chain — that is what the kubernetes-join contract of ADR 0004
establishes. Bootstrapping a second, parallel CA on each consumer MC,
managed by cert-manager, exclusively to sign leaves for the very same
`tbot` pods, adds a redundant identity authority alongside the one
the operator depends on already. ADR 0006 conceded that the motivation
was contract cleanliness rather than security policy; if cleanliness
is the criterion, the cleaner shape is to reuse the identity authority
that exists, not to install a new one.

The SVID carries meaningful workload attestation in a way the
cert-manager leaf does not. A leaf from a `tunnelport-ca` ClusterIssuer
attests "some CA managed by this consumer MC's chart install signed
this certificate". An SVID attests "Teleport central, the same
authority that admits this bot into the cluster via the join contract,
issued this certificate for the workload identity
`spiffe://teleport.giantswarm.io/bot/<bot-name>`". A consumer that
verifies the SPIFFE ID — not just the CA chain — accepts only the
right workload's SVID, not any leaf the CA happens to have signed.
The richer verification surface stays in the consumer pod; the
operator does not have to do anything new to enable it.

Trust distribution simplifies asymmetrically. Cert-manager-on-consumer
materialises a `Certificate` re-issued from `tunnelport-ca` in every
consumer namespace so the consumer pod can mount a CA bundle — per
namespace, per consumer MC. SPIFFE-via-Teleport materialises a single
trust bundle artefact emitted by `tbot`'s `workload-identity-x509`
output as `svid_bundle.pem` and rotated by Teleport. Every
consumer MC paired to the same Teleport
cluster shares that one bundle. Cross-MC trust becomes the default,
not a per-MC GitOps chore. The cert-manager design carries the
opposite property: each consumer MC's CA is private, so a future
tunnel federated across MCs requires bundle distribution out-of-band.

Rotation collapses to a property `tbot` already owns. Teleport rotates
its SPIFFE CA on the producer side; `tbot` redownloads the trust
bundle on its existing schedule; the operator does nothing.
Cert-manager would have required a documented 10-year manual rotation
runbook for the bootstrap CA, because cert-manager does not auto-
rotate roots, only leaves. SPIFFE eliminates the runbook.

Three other paths were considered. Upstream Teleport adding a TLS
listener directly to `application-tunnel` (rejected — weeks-to-months
external dependency, revisit if the PR lands). A chart-referenced
pre-existing wildcard Secret (rejected — gives up correct SANs and
free rotation; `selfsigned-giantswarm` ClusterIssuer being available
makes the wildcard shape unnecessary; this path was specific to the
cert-manager design and does not apply here). A muster-side
`AllowInsecureHTTP` flag (rejected — solves only the muster→tunnel
case, requires forking an external library, papers over the http-only
abstraction leak of the tunnel Service per consumer). The contrarian's
point that in-cluster wire encryption is not load-bearing on the GS
threat model is conceded for this ADR as it was for ADR 0006: the
motivation is contract cleanliness and unblocking standards-compliant
clients, not a security policy change.

The operator-side delta. `tbot`'s rendered config grows a second
service block, `workload-identity-x509`, alongside the existing
`application-tunnel`. The new service writes the SVID, key, and trust
bundle into an in-pod emptyDir volume shared with the ghostunnel
sidecar. `ghostunnel`'s `--cert` and `--key` flags point at the
emptyDir entries `tbot` writes. The operator emits no `Certificate`
object — the leaf is materialised by `tbot` in the pod, not by
cert-manager in the cluster. The chart RBAC needs no cert-manager
verbs and the controller-runtime scheme does not register
`cert-manager.io/v1`. One owned-object kind is added to the
reconcile (the Service grows a port; the Deployment grows a
container; nothing new at the API-server level).

The Teleport-side delta. A `WorkloadIdentity` resource per RemoteApp
template-binds the SPIFFE ID `spiffe://<teleport-cluster>/bot/<bot-name>`
to DNS SANs (`<cr.name>.<cr.namespace>.svc.cluster.local`,
`<cr.name>.<cr.namespace>.svc`, `<cr.name>.<cr.namespace>`) so the
SVID validates as a server cert for the tunnel Service's DNS name.
The bot role grants `workload_identity_labels` matching the resource.
Both pieces are GitOps on Teleport central, in the same shape the
ProvisionToken material from ADR 0004 already lives in. Whether the
operator renders the `WorkloadIdentity` resource or whether it is
hand-managed alongside the ProvisionToken is a question for the
implementation — defaulting to chart-managed-on-consumer (operator-
rendered) would be wrong because Teleport central is not on the
consumer cluster's API surface, so the resources must be created
by whatever flow already creates the ProvisionToken (Flux against
Teleport central, or `tctl create` from a privileged operator).
This ADR commits to: the resource exists, the bot has the role,
the SVID gets minted; how those resources land on Central is the
same GitOps shape as the existing ProvisionToken story, not a new
question.

Trust bundle distribution to consumers. `tbot`'s
`workload-identity-x509` service writes `svid_bundle.pem` — the
PEM-encoded SPIFFE trust bundle — into its destination alongside
`svid.pem` and `svid_key.pem`. For the producer pod the destination
is an emptyDir shared with the ghostunnel sidecar; for consumer-side
distribution the destination is a Kubernetes Secret mounted into
consumer pods at a stable path. The consumer's existing `caFile`-
style knob (for muster, `oauth.server.dex.caFile`) loads the file
into a fresh `*x509.CertPool`. The v18 service emits PEM directly —
no JWKS-to-PEM conversion and no init container are needed. A
consumer that wants to verify the SPIFFE ID — not just the chain —
opts in with a separate field on its own configuration; consumers
that only verify the chain transition into the new model silently.

Risks worth naming, all surfaceable at implementation time, none
blocking the architectural commitment. First, `ghostunnel` accepts
SPIFFE SVIDs as server certs in principle, but the verifier on a
stock Go `crypto/tls` client checks `ServerName` against DNS SANs
by default; the `WorkloadIdentity` resource therefore must stamp
DNS SANs on every SVID in addition to the SPIFFE URI SAN.
Confirmable from the Teleport `WorkloadIdentity` resource spec at
prototype time. Second, SVID lifetime defaults under Teleport v18.7
favour short-lived identities (hours, not days); confirm the
renewal cadence ghostunnel needs to tolerate and that the file-
watch reload property holds. Third, Teleport workload identity
must be enabled on the GS Teleport instance
(`teleport.giantswarm.io`); confirm before shipping. None of these
is an architectural objection — they are the prototype's checklist.

Migration shape is additive. The first cut ships with the plaintext
port `8080` still served, the TLS port `8443` added, and consumers
free to pick either. Once every known consumer of a given RemoteApp
has migrated to `8443`, a follow-up ADR drops `8080` from the
rendered Service — an internal change to the operator with no CR-API
impact. The CRD stays `v1alpha1`; no production consumers need
migrating today.
