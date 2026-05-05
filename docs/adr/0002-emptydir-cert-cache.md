# `emptyDir` for tbot's destination directory; accept re-join on every pod restart

The tbot Deployment mounts an `emptyDir` for the renewable-cert destination
directory. The cert lives only for the pod's lifetime — every pod restart
(rolls, evictions, image bumps, token-Secret rotations) triggers a fresh
join with the static token.

Considered and rejected for v1: a `StatefulSet` with `volumeClaimTemplates`
so renewable certs survive pod restarts. The advantage would be reduced
join-rate pressure on Central during MC-wide pod churn (chart upgrades,
node rolls), and resilience to brief Central-unreachable windows at restart
time. The cost is `StorageClass` coupling on every consumer MC, PVC
provisioning per replica, PVC GC on CR deletion, and the slower/ordered
ergonomics of `StatefulSet` rolls — all to cache a credential tbot is
designed to refetch cheaply.

This is reversible without touching the CR API: `RemoteApp.spec` doesn't
expose any of this. If join-rate pressure becomes a real complaint,
switching to `StatefulSet` is an internal operator change.
