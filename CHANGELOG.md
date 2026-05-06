# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Replaced legacy microkit/operatorkit scaffold with kubebuilder v4 layout
  (`cmd/main.go`, `api/v1alpha1/`, `config/{crd,rbac,samples,...}`, `PROJECT`).
- ClusterRole `manager-role` is now generated entirely from kubebuilder
  markers on the `remoteapp` controller package; the previous hand-written
  `pods` rule is removed (ADR 0003 forbids `pods/log` and the operator
  doesn't need pod read access at this point).
- Replaced the legacy `helm/template-operator/` boilerplate scaffold (PSPs,
  unrelated Secret/Service templates) with a purpose-built `helm/tunnelport/`
  chart.
- `manager-role` now also grants read-only access on `pods` and `secrets`
  (`get;list;watch`) so the reconciler can derive `status.lastError`
  from pod metadata and verify the token Secret's named key for the
  `TokenSecretBound` condition. `pods/log` is still excluded per ADR
  0003 (verified by a manifest test).
- The rendered tbot Deployment carries a readiness probe pointing at
  tbot's diag `/readyz` endpoint on a new named container port `diag`
  (3001), so pod-`Ready` reflects tunnel-up rather than process-up.

### Added

- `RemoteApp` API type in group `access.giantswarm.io/v1alpha1` with required
  `appName`, `port` (1–65535), `proxyAddr`, and `tokenRef.{name,key}` fields
  plus optional `replicas`. Status subresource carries `ready`, `lastError`,
  `observedGeneration`, and `conditions` (with constants `Ready` and
  `TokenSecretBound`). CRD generated to `config/crd/bases/`, sample CR at
  `config/samples/access_v1alpha1_remoteapp.yaml`.
- envtest-driven validation suite under `internal/apivalidation/` covering
  acceptance, per-field rejection, status subresource generation semantics,
  and sample-CR drift.
- `RemoteApp` reconciler under `internal/controller/remoteapp/` that renders
  three owned objects in the CR's namespace: a `ConfigMap` carrying the tbot
  `application-tunnel` config, a `Deployment` (replicas defaults to 1,
  RollingUpdate `maxSurge: 1, maxUnavailable: 0`, pod template mounts the
  ConfigMap, the token Secret by name only, and an `emptyDir` for tbot's
  destination dir per ADR 0002), and a `ClusterIP` `Service`. tbot image
  and resource requests/limits flow from operator config (`--tbot-image`
  and `--tbot-{cpu,memory}-{request,limit}` flags on the manager) — slice
  6 will plumb these from Helm values. Owner references on all three
  objects carry `Controller=true` and `BlockOwnerDeletion=true`. The pod
  template stamps `tunnelport.giantswarm.io/config-hash` so spec changes
  that affect the ConfigMap (port, appName, proxyAddr) trigger a rolling
  update; replicas-only changes scale without rolling.
- Helm chart at `helm/tunnelport/` packaging the operator for consumer
  management clusters: ServiceAccount, ClusterRole + ClusterRoleBinding
  (cluster-wide `get;list;watch` on `RemoteApp` and `secrets`, plus
  full CRUD on owned `Deployment`/`Service`/`ConfigMap`; **no**
  `pods/log` and **no** Secret write verbs), single-replica manager
  Deployment with `--tbot-image` / `--tbot-{cpu,memory}-{request,limit}`
  flags wired from `tbot.image` and `tbot.resources` values, and a
  default-deny NetworkPolicy on the operator pod (DNS + apiserver egress,
  metrics ingress only). The chart bundles the CRD inline at
  `templates/crds.yml` with `helm.sh/resource-policy: keep` and a
  `crds.install` toggle (default `true`); `make manifests` regenerates
  it via `hack/update-helm-crds.sh`. `make verify-helm-crds` and
  `make helm-lint` are available; `test/helm/chart_test.sh` asserts the
  RBAC and value-flow contracts. The chart README documents the four
  operator-non-owned concerns (`RemoteApp`-create RBAC, tenant-pod
  NetworkPolicy, token Secret delivery, Central-side preconditions) and
  the ADR-0002 join-rate caveat.
- Watch on `Secret` resources, scoped via a Secret-to-RemoteApp mapper
  (`mapSecretToRemoteApps`) that returns `reconcile.Request`s only for
  `RemoteApp`s whose `spec.tokenRef.name` matches the event's Secret in
  the same namespace. Unrelated Secret churn produces an empty fan-out
  and never reaches the workqueue. The reconciler reads only the
  Secret's `metadata.resourceVersion` (never `Secret.Data`) and stamps
  it onto the pod-template annotation
  `tunnelport.giantswarm.io/token-secret-version`, so a token rotation
  changes the pod-template-hash and the Deployment rolls via its
  existing `RollingUpdate` strategy. RBAC adds `get;list;watch` on
  `secrets`; no `update`/`patch`/`delete`. An AST-based static check in
  the controller package's tests rejects any `secret.Data` access.
- Status reconciliation: `RemoteApp.status` is now populated from
  k8s-visible state only (ADR 0003). `status.ready` mirrors at-least-one
  pod-`Ready`; `status.lastError` is derived from pod `Phase` /
  `ContainerStatuses` / `RestartCount` / last termination reason
  (e.g. `"CrashLoopBackOff (5 restarts), last termination: Error (137)"`);
  `status.observedGeneration` follows the standard kubebuilder pattern;
  `status.conditions` carries `Ready` and `TokenSecretBound` as
  `metav1.Condition`s. The reconciler watches owned pods via a
  label-driven `Watches(&corev1.Pod{}, ...)` keyed on
  `tunnelport.giantswarm.io/remoteapp` so pod-state changes refresh
  status promptly.

### Changed

- `renderDeployment` now takes a `tokenSecretVersion string` argument
  threaded from the reconciler's pre-render Secret read.
- Fixed the rendered tbot YAML config to match the upstream tbot
  schema. The previous renderer would have been rejected by
  `tbot start`. Specifically: `listener:` is now `listen:` (per
  `lib/tbot/services/application/tunnel_config.go`), the invented
  `token_secret_ref:` block is removed (the existing `token:` field
  is a path that tbot dereferences), the redundant `auth_server:`
  copy of `proxy_server:` is dropped, and `diag_addr: 0.0.0.0:3001`
  is now set so the readiness probe added in slice 4 has a listener
  to hit. Regression-guarded in `render_test.go`.

### Added

- `--tbot-insecure` flag on the operator manager (`tbot.insecure`
  Helm value) renders tbot configs with `insecure: true` so pods
  skip Teleport proxy TLS verification. Development-only; required
  for kind-based smoke tests where the proxy is reached by IP and
  the cert SAN does not match. Off by default; a regression test in
  `render_test.go` asserts the line never appears in a default
  render.
- Chart `ClusterRole` now grants `get;list;watch` on `pods`. Slice 4
  added the Pod watch on the Go side; the chart RBAC was missed in
  slice 6 and the operator hit `pods is forbidden` at startup until
  patched.
- Smoke-test scaffold under `hack/smoke/` covering a three-cluster
  kind topology (teleport / producer / consumer): kind configs,
  teleport-cluster + teleport-kube-agent Helm values pinned to
  **18.7.3**, role / bot / token YAML for `tctl`, a sample
  `hashicorp/http-echo` producer, the `RemoteApp` CR, and a curl
  Job that asserts on the response body. The runbook
  (`hack/smoke/README.md`) walks a developer from zero to a green
  curl: kind clusters, network discovery, `tctl` provisioning,
  plain `kubectl create secret` for the bot token, troubleshooting,
  and a smoke-vs-production differences table.

[Unreleased]: https://github.com/giantswarm/tunnelport/tree/master
