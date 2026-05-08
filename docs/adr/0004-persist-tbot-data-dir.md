# Persist tbot's data directory; bot tokens are single-use (supersedes ADR 0002)

Superseded by: ADR 0006

`RemoteApp` join tokens are created on Central as bot tokens (`tctl bots add`,
or the upstream Teleport Operator's `TeleportBot` + `TeleportToken` flow,
producing a `kind: token` resource with `roles: [Bot]` and `bot_name: <name>`).
Per Teleport docs, **bot tokens are single-use**: after the initial join, the
token is exchanged for a renewable certificate and immediately invalidated. All
subsequent reconnection happens via the certificate, not the token. This is an
explicit exception to the general "tokens are reusable until TTL expires"
model — it applies specifically to tokens with `roles: [Bot]`.

ADR 0002 modelled the token as a "credential tbot is designed to refetch
cheaply" and chose `emptyDir` on that basis. That premise was wrong. Every
pod restart on `emptyDir` loses the renewable certificate, and tbot cannot
rejoin with the already-consumed token. The recovery procedure is
`tctl bots instances add` to mint a fresh single-use token, re-sync it into
the `tokenRef` Secret on the consumer MC, and restart the pod. Across a
fleet of consumer MCs (each running multiple `RemoteApp`s, all churning
on chart bumps and node rolls) this is per-restart SRE toil with no
asymptote. The chart's pod-churn caveat in `helm/tunnelport/README.md`
overstated tbot's restart resilience on this point.

Decision: persist `/var/lib/tbot` across pod restarts. Mechanism is a
follow-up ADR — two viable shapes exist:

1. PVC + keep `join_method: token`. Pod restart with PVC intact: tbot
   renews via the existing certificate. PVC destroyed: still requires
   `tctl bots instances add` + Secret rotation, but PVC loss is rare.
2. PVC + `join_method: bound_keypair` with `recovery.mode: relaxed` (Teleport
   v17+; tbot is pinned at v18). Pod restart with PVC intact: rejoin via
   keypair. PVC destroyed: tbot self-recovers by re-registering the keypair
   against the registration secret. Cost: ~+20 LOC, a per-bot Teleport-side
   `bound_keypair` opt-in, and a CRD addition for `spec.joinMethod`.

Either path requires the rendered object to switch from a `Deployment` with
`emptyDir` to a `StatefulSet` with `volumeClaimTemplates` (per ADR 0002's own
escape clause) — a single RWO PVC behind a `Deployment` would force
`Strategy: Recreate` and break `replicas > 1`, both of which are documented
properties (CONTEXT.md, `helm/tunnelport/README.md`).

Sources for the bot-token claim (verified 2026-05-08):
- https://goteleport.com/docs/reference/deployment/join-methods/
- https://goteleport.com/docs/reference/architecture/machine-id-architecture/
- https://goteleport.com/docs/machine-workload-identity/troubleshooting/
