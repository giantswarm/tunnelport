# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Replaced legacy microkit/operatorkit scaffold with kubebuilder v4 layout
  (`cmd/main.go`, `api/v1alpha1/`, `config/{crd,rbac,samples,...}`, `PROJECT`).

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

[Unreleased]: https://github.com/giantswarm/tunnelport/tree/master
