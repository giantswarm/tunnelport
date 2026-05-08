# Use `bound_keypair` join with `recovery.mode: relaxed` for fleet self-healing

ADR 0004 settled that `/var/lib/tbot` must persist and left the choice between
(A) PVC + `join_method: token` and (C) PVC + `join_method: bound_keypair` for
follow-up. We pick C with `recovery.mode: relaxed`.

Criterion: PVC-loss self-recovery without per-bot SRE intervention. Across the
consumer-MC fleet, PVC loss happens (StorageClass migrations, node-local CSI
failures, accidental `kubectl delete pvc`, DR restores). A's recovery
procedure (`tctl bots instances add` → re-sync `tokenRef` Secret → restart)
does not scale across the fleet. C with `recovery.mode: relaxed` lets tbot
re-register a fresh keypair against the registration secret without operator
intervention.

Acknowledged trade-off: `relaxed` keeps the registration secret in the
`tokenRef` Secret usable for re-registration indefinitely — unlike A, where
the Secret becomes a dead string after first use. A leaked consumer-MC Secret
therefore remains a viable bot-impersonation vector for the secret's lifetime.
Accepted because: (a) the `tokenRef` Secret pipeline (sealed-secrets / SOPS /
ESO) is already ADR 0001's trust root, no new dependency; (b) `relaxed` keeps
Teleport-side join-state verification ON — only `insecure` disables it,
`insecure` is out of scope; (c) per-bot revocation stays a single keypair
rotation on Central.

Rejected: `recovery.mode: standard`. After `limit` retries it collapses back
to A's manual procedure, so picking C without `relaxed` buys fleet ops
nothing.

Rendered-object shape: `StatefulSet` + `volumeClaimTemplates` (per ADR 0004),
not `Deployment` + RWO PVC, to preserve `replicas > 1` and ordered rolls.

CRD impact: none. The operator hardcodes `join_method: bound_keypair` in
the rendered tbot config; `RemoteAppSpec` gains no `joinMethod` field.
Backwards compatibility with the prior `token` join method is explicitly
out of scope — existing `RemoteApp` consumers (e.g. graveler's `dex-garm`,
`mcp-kubernetes-garm`) will be reconfigured Central-side and have their
`tokenRef` Secret values replaced with bound_keypair registration secrets
as part of the rollout. There is no in-operator migration code path.

Per-bot Teleport-side opt-in (the matching `bound_keypair` config on the
bot resource) is a platform-team operation per `RemoteApp`, documented
alongside the existing per-bot token provisioning in
`helm/tunnelport/README.md` §4.
