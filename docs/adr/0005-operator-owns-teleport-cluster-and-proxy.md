# Operator owns Teleport cluster name + proxy address (per-MC, not per-CR)

`spec.clusterName` and `spec.proxyAddr` are removed from `RemoteApp`. The
Teleport cluster name (the `aud` claim Teleport's `static_jwks` validator
pins) and the proxy host:port are operator-level configuration on the
tunnelport chart: `--teleport-cluster-name` and `--teleport-proxy-addr`
flags, sourced from Helm values `teleport.clusterName` and
`teleport.proxyAddr`. Both are required at chart install time; the
operator fails fast with a directional error if either is empty.

This narrows the design space ADR 0004 left open. ADR 0004 made these
two fields per-CR, justified as a forward-compatibility property: a
single consumer MC could host RemoteApps pointing at different Teleport
clusters without re-installing the chart. The first production
deployment (graveler → teleport.giantswarm.io) showed that property
costs more than it earns:

- Every RemoteApp on a consumer MC points at the same Teleport cluster
  and the same proxy. Every CR repeats the same two strings. A typo on
  either silently scopes the bug to one CR — the same blast radius an
  operator-level flag has, with worse ergonomics.
- The forward-compat use case (multi-Teleport-cluster on one consumer
  MC) has no concrete user. Building for it ahead of demand creates
  exactly the kind of "knob that exists because we *might* need it"
  this project's CR design (`Service.type` locked to `ClusterIP`,
  `replicas: 2` opt-in, no per-CR tbot-image override) already rejects.
- The multi-Teleport-on-one-MC case, if it ever arrives, has a clean
  answer: run a second tunnelport operator install in its own
  namespace, scoped to a different RemoteApp namespace via the
  existing controller-runtime scoping knobs. Two operator installs is
  not a heavier hammer than re-installing one — it's the same
  surgery, applied symmetrically.

The `Audience` claim on the per-CR projected SA JWT continues to be
the Teleport cluster name. The value just comes from the operator's
flag now, not from `cr.Spec.ClusterName` — the on-the-wire shape of
the JWT and the Teleport `static_jwks` join contract are unchanged.

Operational implications:

- The `--teleport-cluster-name` flag's value MUST match the value
  Teleport's auth server returns for `tctl status` on Central (the
  `Cluster:` line). Mismatch causes 100% join failure across every
  RemoteApp on this MC with the audience-claim error from
  `lib/auth/join_kubernetes.go`. The flag is therefore fail-fast: an
  empty value exits with a directional `setupLog.Error` rather than
  letting the operator come up and crashloop every tbot pod.
- The `--teleport-proxy-addr` flag carries the same fail-fast
  treatment for the same reason: an empty value would render
  `proxy_server: ""` in every tbot config and produce uniformly
  unhelpful "proxy address required" startup errors.
- Operators changing either flag triggers a chart redeploy, which
  rolls the manager pod. The reconciler then re-renders every owned
  ConfigMap (different config hash) and Deployment pod-template
  annotation, rolling each RemoteApp's tbot pods one at a time
  (`maxSurge: 1, maxUnavailable: 0`). No CR mutation needed.

Trade-offs accepted by this change:

- A typo in either flag breaks every RemoteApp on this MC at once,
  not just one CR. This is the standard blast-radius cost of moving
  config from per-instance to operator-wide. We accept it because:
  (a) the values are static per consumer MC and rarely changed,
  (b) the operator's fail-fast validation catches empty values at
  startup before any CR is touched, and (c) the upstream chart
  redeploy that introduces the typo is also the loudest signal.
- The RemoteApp CR can no longer document the cluster-name binding
  inline — operators reading a CR must look at the operator
  installation to know which Teleport cluster the CR resolves to.
  CONTEXT.md "Operator config posture" captures this trade.

The CRD stays `v1alpha1` and there are no production consumers to
migrate. ADR 0004's other decisions (kubernetes join method, per-CR
ServiceAccount, static_jwks trust, projected SA JWT with the Teleport
cluster name as audience) remain in force; this ADR amends only the
location of two configuration fields.
