# Static join token per `RemoteApp`; `kubernetes` join method rejected for v1

> **Superseded by [ADR 0004](./0004-kubernetes-join-method.md).** The
> trade-off this ADR records was reversed: an operator-rendered
> ServiceAccount per `RemoteApp` is strictly less operational surface
> than a sealed-sync Secret pipeline per `RemoteApp`. The kubernetes
> join method (with `static_jwks` trust) is now the only path.

Each `RemoteApp` references its own static Teleport join token (delivered as
a `Secret` via the platform team's existing GitOps + secret-sync pipeline)
that is bound on Central to a dedicated `TeleportBot` whose role matches
exactly that one app. This gives per-app blast-radius isolation: a
compromised tbot pod yields a credential reaching only that one app.

The `kubernetes` join method (tbot authenticates via the consumer cluster's
ServiceAccount JWT, which Central verifies against the cluster's JWKS) was
considered and rejected for v1. To get the same blast-radius isolation it
would require the operator to render a per-CR `ServiceAccount` *and* require
Central to pin each token's allowed-subject to that exact SA — the same
1-paired-action-on-Central cost as static tokens, but with extra moving
parts on the consumer side and an extra cross-cluster trust setup. The
trade-off is operational simplicity now, at the cost of relying on the
existing secret-sync pipeline being trustworthy. Modern Teleport docs lean
toward kubernetes-join, so this ADR exists so the next reader doesn't
"upgrade" us to it without re-examining the trade.
