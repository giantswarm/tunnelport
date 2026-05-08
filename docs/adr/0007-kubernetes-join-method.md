# Use `kubernetes` join method with per-`RemoteApp` ServiceAccount (supersedes 0001, 0004, 0005)

After a clean-slate spike (4 parallel investigations, unanimous), the join
method shifts from per-app static tokens / bound_keypair to **`kubernetes`
join**. Each `RemoteApp` renders its own `ServiceAccount`. The pod template
projects the SA's audience-scoped JWT into a tmpfs volume; tbot presents
that JWT to Teleport's join endpoint on every pod start. Teleport
validates against the consumer cluster's JWKS and allocates a fresh
`botInstanceID` per join (`lib/auth/join.go:535-543` in
`gravitational/teleport@v18.7.6`). Each pod is its own bot instance with
its own renewable cert in `emptyDir`.

This is the pattern Teleport's own `examples/chart/tbot` chart ships as
its default (`examples/chart/tbot/tests/__snapshot__/config_test.yaml.snap`
locks `join_method: kubernetes`). Lifting it gives us a battle-tested
trust model without inventing a new one.

## What this supersedes

- **ADR 0001** rejected `kubernetes` join for v1, citing "extra moving
  parts on the consumer side" (per-CR ServiceAccount). The operator
  already renders per-CR `ConfigMap`, `Secret`, `StatefulSet`, and
  `Service`; one more SA is the same shape, not a step change. The
  rejection is reversed.
- **ADR 0004** chose `StatefulSet` + per-pod PVC because `bound_keypair`
  state had to persist. Under `kubernetes` join nothing in `/var/lib/tbot`
  needs to survive a pod restart — every restart re-joins via the
  kubelet-projected JWT. Revert to `Deployment` with `emptyDir`.
- **ADR 0005** chose `bound_keypair` with `recovery.mode: relaxed`. No
  longer the join method; the recovery-mode debate dissolves.
- **ADR 0006** stays Rejected; this design is the cleaner answer it was
  groping toward.

## What the SRE provides, once per `RemoteApp`

A Teleport `kind: token` resource on Central with:

```yaml
kind: token
version: v2
metadata:
  name: <cr.Name>
spec:
  roles: [Bot]
  bot_name: <cr.Name>
  join_method: kubernetes
  kubernetes:
    type: in_cluster
    allow:
      - service_account: "<install-namespace>:<cr.Name>"
```

Delivered via the upstream Teleport-Operator (`TeleportToken` CR) or raw
`tctl create -f`. After this, no SRE intervention: chart bumps, pod
restarts, scale up/down, node loss — all handled by the operator + tbot
without touching the credential pipeline.

## What changes in tunnelport

- `internal/controller/remoteapp/render.go`: render a per-CR
  `ServiceAccount`; revert to `Deployment` (drop `StatefulSet`,
  `volumeClaimTemplates`, PVC retention policy); add a projected-token
  volume to the pod template (`/var/run/secrets/tokens/join-sa-token`,
  audience pinned to the Teleport proxy address, ~10min expiration).
- `internal/controller/remoteapp/tbot_config.go`: `JoinMethod` →
  `kubernetes`; drop `bound_keypair` block; replace
  `onboarding.token: <Central-side resource name>` (under
  `kubernetes` join the token reference still names the Teleport token
  resource — see `lib/tbot/bot/onboarding/config.go`).
- `RemoteAppSpec`: `tokenRef` is removed (no more consumer-MC `Secret`
  carrying registration material). The CRD becomes simpler.
- Helm chart: drop the static-token / registration-secret rotation
  sections; add the one-time per-MC JWKS-trust setup note.

## Trade-offs accepted

- **One-time JWKS trust setup per consumer MC.** Teleport must trust the
  consumer cluster's JWKS endpoint. Standing infrastructure on every GS
  MC; verify per-MC before rolling out.
- **Bot instance churn on Central.** Every pod start = one new bot
  instance. Reaped by Teleport's `bot_instance_ttl` (default 1 week).
  Cosmetic; flag for audit-log retention tuning.
- **Per-RemoteApp ServiceAccount on the consumer MC.** New cluster-scoped
  artifact in tunnelport's render set; cleaned up via OwnerReferences on
  CR delete.

## Open verification before rollout

Does `teleport.giantswarm.io` already trust each consumer MC's JWKS? If
not, a one-time platform-team setup per MC is required before any
`RemoteApp` on that MC can roll on this design. Likely already configured
where any other Teleport `kubernetes`-join consumer exists.
