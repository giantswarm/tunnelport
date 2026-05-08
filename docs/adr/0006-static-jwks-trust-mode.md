# `kubernetes.type: static_jwks` for tunnelport's join trust

Supersedes: ADR 0001 (static-token join per `RemoteApp`), ADR 0004 (persist
tbot data dir), ADR 0005 (bound_keypair join with relaxed recovery).

This ADR commits to two coupled decisions for tunnelport's join:

1. **Join method.** tbot joins Central via the `kubernetes` join method —
   each tbot pod presents its mounted `ServiceAccount` token, and Central
   verifies it. This replaces the `bound_keypair` join adopted in ADR 0005
   and the `tokenRef` Secret delivery model from ADR 0001. The operator
   renders a per-`RemoteApp` `ServiceAccount` on the consumer MC; tbot
   joins as that SA's identity, removing the need to persist registration
   state across restarts (ADR 0004).
2. **Trust mode.** The `TeleportProvisionToken` resources tunnelport relies
   on use `kubernetes.type: static_jwks` (not `oidc`, not `in_cluster`).

## Why static_jwks, not oidc

GS CAPI MCs do not expose their kube-apiserver's OIDC discovery endpoint
to the public internet by default. `oidc` mode would require Teleport's
auth service (`teleport.giantswarm.io`) to reach each consumer MC's
discovery URL — a network/security ask much bigger than re-running an
idempotent bootstrap script. `mc-bootstrap/scripts/setup-teleport.sh:23`
already establishes the static_jwks pattern: `kubectl get --raw=/openid/v1/jwks`
against the MC at bootstrap time, embedded in a `TeleportProvisionToken`
committed to `giantswarm/teleport-fleet`. This is the platform's
canonical approach: ~27 MCs run kube-agent on this pattern today
(`teleport-fleet/kubernetes/shared/templates/bot-*-token.yaml`). Epic
35456's `PLAN.md` (TB-3) ratifies the same pattern for the
muster-aggregator bot. tunnelport rides on this established mechanism,
not a parallel one.

## Consumer-MC scope

"Consumer MC" = an MC where muster (and therefore tunnelport) is
deployed. Today: graveler. Tomorrow: gazelle. PLAN.md explicitly bounds
growth to ≤3 consumer MCs before escalating to operator-managed
Teleport state. Onboarding cost scales with consumer MCs, not the full
GS fleet — and graveler + gazelle already have `bot-${MC}` tokens in
`teleport-fleet` from kube-agent. Tunnelport's incremental cost is
N PRs to `teleport-fleet` adding per-RemoteApp tokens, where N = number
of `RemoteApp` CRs.

## One token per `RemoteApp`, not multi-SA per token

Each `RemoteApp` gets its own `TeleportProvisionToken` and its own bot
resource on Central, named `tunnelport-${cr.Name}`. Each token's
`kubernetes.allow` permits exactly one ServiceAccount on the consumer
MC: the per-`RemoteApp` SA tunnelport renders. Each bot's role is
scoped to its target Teleport application.

This preserves the per-app blast-radius spirit of ADR 0001: a leaked SA
token, or a compromised tbot pod, yields access to only one app. The
alternative — one bot per MC with a multi-SA allow list (per the
`teleport-operator + teleport-tbot` pattern in
`bot-garm-token.yaml:11-12`) — is fewer PRs but coarser blast radius.
Rejected.

## Rotation / refresh

GS's CAPI MCs do not auto-rotate kube-apiserver SA signing keys
(`cluster-aws`'s kubeadm-managed control planes don't run
`kubeadm certs renew sa.key`; no MC-level rotation policy found in
`mc-bootstrap` or `cluster-api-provider-*`). 27 MCs have run on bootstrap
JWKS snapshots for 6+ months with zero refresh commits in
`teleport-fleet`.

If a rotation does occur: new tbot pods fail to join (existing pods
keep tunneling on cached certs until TTL expires, surfacing as
`RemoteApp.status.lastError` per ADR 0003). Recovery: re-run
`setup-teleport.sh` for that MC, PR `teleport-fleet`, Flux applies.
~30-60min runbook step.

Per-MC escape hatch: any MC where `static_jwks` becomes unreliable can
flip to `kubernetes.type: oidc` — precedent at `teleport-fleet` commit
`3b19be1` (gandreasmc4, IRSA-style issuer). Single-PR change scoped to
that MC.

`static_jwks` supports multi-key JWKS payloads natively (RFC 7517 array
shape — `lib/kube/token/join.go` in `gravitational/teleport@v18.7.6`,
RFD-0143). If rotation becomes recurring for any MC, snapshotting
old + new keys during a transition window is supported without code.

## Operational cost (concrete)

Per `RemoteApp`: 1 PR to `teleport-fleet/kubernetes/shared/templates/`
adding `bot-${MC}-tunnelport-${app}-token.yaml` reusing the MC's
existing JWKS payload, plus 1 manual `tctl bots add` per environment
(prod + test) — `mc-bootstrap`'s setup script automates *token* creation
but not *bot* creation. The manual `tctl bots add` step is friction
worth automating later via the upstream Teleport Operator's `TeleportBot`
CR; flagged as out-of-scope follow-up.

For graveler today: 2 PRs to `teleport-fleet` (`garm-dex`,
`garm-mcp-kubernetes`), 2 × 2 manual `tctl bots add` invocations. Both
reuse graveler's JWKS payload from `bot-graveler-token.yaml`.
