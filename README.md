[![CircleCI](https://circleci.com/gh/giantswarm/tunnelport.svg?&style=shield)](https://circleci.com/gh/giantswarm/tunnelport)

# tunnelport

A Kubernetes operator that wraps Teleport's `tbot` + a `Service` behind a single
`RemoteApp` CR, so workloads on a consumer management cluster can reach a
Teleport-exposed app as if it were a local `Service` — no Teleport SDK in caller
code.

## What it does

A platform engineer writes one `RemoteApp`. The operator renders a pod
containing `tbot` (which dials Teleport and exposes an `application-tunnel`
on `127.0.0.1`) plus a `ghostunnel` sidecar that terminates TLS in front of
it, behind a `ClusterIP` `Service`. Workloads `curl
https://<remoteapp-name>:8443` and traffic is tunneled to the app behind
Teleport. The TLS server cert is a SPIFFE X.509-SVID minted by `tbot` from
Teleport central's SPIFFE CA — callers verify it against a single
chart-managed trust bundle Secret.

```
  Consumer MC                                            Central MC         Producer MC
  ┌────────────────────────────────────────────────┐    ┌──────────┐       ┌────────────────┐
  │                                                │    │ Teleport │       │ teleport-kube- │
  │  caller pod                                    │    │  proxy   │       │   agent        │
  │     │   https://payments:8443                  │    │   +      │       │   (app mode)   │
  │     │   ▲ verifies SVID against mounted bundle │    │  auth    │       │       │        │
  │     ▼   │   (tunnelport-spiffe-bundle Secret)  │    │   +      │       │       ▼        │
  │  Service (ClusterIP) — tls/8443                │    │ SPIFFE   │       │  backend app   │
  │     │     (+ http/8080, deprecated)            │    │   CA     │       └────────────────┘
  │     ▼                                          │    └────▲─────┘             ▲
  │  ┌──────────── RemoteApp pod ──────────────┐   │         │ mTLS               │
  │  │  ghostunnel ─TLS term─► tbot            │───┼─────────┴─ app-tunnel ───────┘
  │  │   (SVID + key from      (application-   │   │         ▲
  │  │    emptyDir, minted by   tunnel,        │   │         │ kubernetes-join:
  │  │    tbot's wid-x509)      127.0.0.1)     │   │         │ projected SA JWT,
  │  └─────────────────────────────────────────┘   │         │ pinned via static_jwks
  │     ▲                                          │         │
  │     │ owns Deployment + Service + CM + SA      │         │
  │   RemoteApp CR  (access.giantswarm.io/v1alpha1)│         │
  │     spec: { appName, port, tokenName }         │         │
  │                                                │         │
  │  ┌─ tunnelport-trust-bundle (chart singleton) ─┐         │
  │  │   tbot ─► Secret: tunnelport-spiffe-bundle  ├─────────┘
  │  │           svid_bundle.pem — mounted by      │   joins same way,
  │  │           every caller pod above            │   emits the trust
  │  └─────────────────────────────────────────────┘   bundle once per MC
  └────────────────────────────────────────────────┘
```

The Teleport cluster name + proxy address are operator-level chart values
(`teleport.clusterName`, `teleport.proxyAddr`), not CR fields — see
[`docs/adr/0005-operator-owns-teleport-cluster-and-proxy.md`](./docs/adr/0005-operator-owns-teleport-cluster-and-proxy.md).

One `tbot` Deployment per `RemoteApp` (per-app blast-radius isolation). Under
the kubernetes-join model
([ADR 0004](./docs/adr/0004-kubernetes-join-method.md)) the operator renders
a per-CR `ServiceAccount`; the rendered tbot pod authenticates to Teleport
using that SA's projected JWT, validated against the `static_jwks` pinned on
the Teleport `ProvisionToken`. No static-token `Secret` is delivered to the
consumer cluster.

The tunnel `Service` is fronted by a `ghostunnel` sidecar that terminates
TLS on `8443` using a SPIFFE X.509-SVID
([ADR 0007](./docs/adr/0007-spiffe-via-teleport-workload-identity.md)) minted
by the same `tbot` from a Teleport `WorkloadIdentity`. A chart-managed
singleton `tunnelport-trust-bundle` Deployment runs one extra `tbot` that
materialises the SPIFFE trust bundle into a single Secret
(`tunnelport-spiffe-bundle`,
[ADR 0008](./docs/adr/0008-singleton-trust-bundle-tbot.md)); every consumer
on the MC mounts that one Secret to verify SVIDs from any `RemoteApp`.

See [`CONTEXT.md`](./CONTEXT.md) for the full design.

## Scope

`RemoteApp` covers Teleport **Application Service** apps (TCP/HTTP) only.
Database, Kubernetes, and SSH access are out of scope.
