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

[Unreleased]: https://github.com/giantswarm/tunnelport/tree/master
