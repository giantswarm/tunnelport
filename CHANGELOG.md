# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- The operator now verifies what each tunnel actually serves, closing the
  detection gap in giantswarm/giantswarm#37521 (gap 2). Every
  `verification.interval` (default `2m`, on the elected leader) it dials each
  RemoteApp reporting `status.ready: true` at
  `<name>.<namespace>.svc.<clusterDomain>:<tls.port>` with `ServerName` set to
  that FQDN, and verifies the served chain against the SPIFFE trust bundle it
  mounts from `tunnelport-spiffe-bundle`. Nothing the operator previously
  watched could see a certificate whose SANs stopped matching: tbot joins,
  ghostunnel binds, and the sidecar's `TCPSocket` probe connects regardless —
  which is how 40 tunnels served unverifiable certificates for two days with
  every signal green.
  - New metric `tunnelport_remoteapp_tls_verification{remoteapp_name,
    remoteapp_namespace, result}`, one series per RemoteApp with `result` one of
    `verified`, `cert_invalid`, `unreachable` or `not_ready`. `cert_invalid` and
    `unreachable` are deliberately distinct.
  - New metric `tunnelport_tls_verification_available`, `0` when the operator
    holds no usable trust bundle and therefore cannot judge any certificate.
    Reported only by the replica running the check.
  - New `RemoteApp.status.conditions` entry `TunnelVerified`, with the specific
    X.509 fault as its message, and a `Verified` column on
    `kubectl get remoteapp`. `status.ready` is unchanged — it stays join-level.
  - New values: `verification.{enabled,interval,timeout,concurrency,clusterDomain}`.
    Off automatically when `trustBundle.enabled=false`.
- `monitoring.podMonitor.enabled` (default `true`) renders a PodMonitor for the
  manager pods. Until now the chart rendered no Service and no ServiceMonitor,
  so every `tunnelport_*` metric was served and never collected — a
  PrometheusRule over metrics nobody scrapes cannot fire.
- Three alerts on the PrometheusRule:
  - `TunnelPortTunnelCertificateInvalid`: a tunnel has served a certificate that
    fails verification for 20m (`for:` long enough to ride out a rollout and an
    SVID renewal).
  - `TunnelPortTunnelUnreachable`: a tunnel reports Ready but accepts no
    connection on its TLS port for 20m.
  - `TunnelPortTLSVerificationUnavailable`: the operator has had no usable trust
    bundle for 30m, so the check above cannot fire.
- `RemoteApp.status.conditions` gains `IdentityIssued` and `TunnelServing`, one
  per role in the tunnel, so the Teleport join and the TLS listener are both
  visible (giantswarm/giantswarm#37445). `IdentityIssued=False` with
  `TunnelServing=False` means the join is the cause and TLS the symptom. Both
  show as `Identity` and `Serving` columns in
  `kubectl get remoteapp -o wide`.

- `monitoring.prometheusRule.enabled` (default `true`) renders a PrometheusRule with two alerts:
  - `TunnelPortTunnelCrashLooping`: a tunnel container (`tbot` or `ghostunnel`) is in CrashLoopBackOff.
  - `TunnelPortOperatorDown`: the operator Deployment has no available replicas.
- `monitoring.prometheusRule.labels`, merged into the PrometheusRule's labels.
  It defaults to `observability.giantswarm.io/tenant: giantswarm`. Mimir's
  `rule_selector` matches on that label, and a rule without it is never loaded
  into the ruler (giantswarm/giantswarm#37445). Override the key to route to
  another tenant, or set it to `null` to drop the label.

### Changed

- The manager's metrics endpoint moved to plain HTTP on port 8080
  (`ports.metrics`, `--metrics-secure=false`), which is what the new PodMonitor
  scrapes. The authn/authz variant gates `/metrics` behind
  TokenReview+SubjectAccessReview and would require granting the scraping
  agent's ServiceAccount a `nonResourceURLs: ["/metrics"]` ClusterRole in every
  installation. Exposure is bounded by the chart's NetworkPolicy instead;
  nothing on the endpoint is a credential.
- The manager's NetworkPolicy now allows egress on the tunnel TLS port
  (`tls.port`), which the verification dial needs. Without it every tunnel would
  report `unreachable`.
- The rendered ghostunnel sidecar now carries a TCPSocket readiness probe on
  its `tls` listen port (8443). A tunnel pod stays NotReady until ghostunnel is
  actually accepting TLS connections (which requires tbot to have written the
  SVID), instead of reporting Ready with a broken tunnel.

### Fixed

- The two existing alerts pointed at `runbook_url`s that did not exist, under
  the legacy `ops-recipes` path. Both now point at real runbooks under
  `support-and-ops/runbooks/`, and every alert's URL carries the
  `INSTALLATION`/`CLUSTER` query parameters the intranet runbook pages use.
- `helm/tunnelport/tests/` (promtool unit tests for the PrometheusRule) now runs
  in CI, as a second step of the `chart-test` job. It had been committed but
  never wired up, so no alert's firing behaviour was checked by anything.
- `RemoteApp.status.lastError` now describes the `tbot` container when both
  containers of a tunnel pod are unready. Kubelet orders container statuses
  alphabetically, so the status reported `ghostunnel`, which only exits because
  tbot never wrote the SVID (giantswarm/giantswarm#37445).

- Tunnel pods no longer trip the `restrict-image-registries` and
  `require-emptydir-requests-and-limits` Kyverno policies
  (giantswarm/giantswarm#36885):
  - The tbot and ghostunnel images are now sourced from gsoci
    (`gsoci.azurecr.io/giantswarm/tbot-distroless`,
    `gsoci.azurecr.io/giantswarm/ghostunnel`) instead of `public.ecr.aws`
    and Docker Hub. `tbot.image` and `tls.image` are now structured
    (`registry`/`name`/`tag`/`digest`) so the registry can be overridden to a
    different mirror.
  - Every emptyDir on the rendered tbot pods and the singleton trust-bundle
    pod now declares a `sizeLimit` (50Mi), which bounds scratch growth and
    excludes the volumes from the emptyDir policy — so the resource-less
    ghostunnel sidecar no longer needs per-container ephemeral-storage entries.
- Every rendered object now carries `application.giantswarm.io/team: bumblebee`.
  It rendered as `""` on every released chart, so the two alerts had no owner
  for routing (giantswarm/giantswarm#37445). `Chart.yaml` now carries the
  ownership annotation under the `io.giantswarm.application.team` key that the
  packaged chart keeps.
- `test/helm/chart_test.sh` renders again and runs in CI as the `chart-test`
  job.

- The `e2e-smoke` job is no longer flaky (#92). It failed on roughly half of all
  runs, on unchanged content, because `hack/smoke/run.sh` installed the
  teleport-cluster chart twice — first with `publicAddr` left as its
  `REPLACE_WITH_TELEPORT_PROXY_ADDR` placeholder, since the proxy's NodePort was
  not knowable until the Service existed. A proxy registers its advertised
  address as a heartbeat that outlives the pod, and Teleport derives an app's
  public address from the *first* proxy in that list, so `smoke-app` registered
  at either the real address or the placeholder depending on list order. The
  smoke now pins the proxy's NodePort in a Service it owns and installs the
  chart once, with the address already substituted. It also asserts the address
  `smoke-app` registered at, instead of only its name, so a recurrence fails at
  that step rather than three minutes later as an opaque curl timeout.

## [1.0.4] - 2026-06-16

### Fixed

- Install/upgrade no longer fails with `metadata.labels: Invalid value` when the
  chart is pulled via a Flux `OCIRepository` by digest. Flux appends the OCI
  digest to the chart version as SemVer build metadata (e.g.
  `1.0.3+eab5dd2094f6`), and the `+` is illegal in a Kubernetes label value, so
  the unsanitised `app.kubernetes.io/version` label rejected every
  operator-owned object. The label is now sanitised the same way as
  `helm.sh/chart` (`+` -> `_`). The manager image tag, which also defaults to
  `.Chart.Version`, now strips the build metadata (`+` is illegal in image tags
  too; the pushed tag is the clean SemVer).

### Changed

- CI: bump the `giantswarm/architect` orb from `7.1.0` to `9.4.1`. Among other
  improvements, `push-to-app-catalog` now stamps `appVersion` from the git tag
  by default (`override_app_version`, added in `9.3.0`), so released charts no
  longer carry the `0.0.0` placeholder appVersion. The orb now always builds
  multi-arch (`linux/amd64,linux/arm64`) images via buildx.
- Build: pin the Dockerfile builder stage to `$BUILDPLATFORM` so Go
  cross-compiles natively per target arch instead of running under QEMU
  emulation. Required by the orb's always-multi-arch buildx flow -- emulated
  Go compilation otherwise trips the push job's no-output timeout.

## [1.0.2] - 2026-06-15

### Fixed

- A default install of the released chart no longer lands in `ImagePullBackOff`.
  The image tag now resolves from `.Chart.Version` (which app-build-suite stamps
  from the git tag, matching the pushed image tag) instead of `.Chart.AppVersion`.
  The pinned release orb (`architect@7.1.0`) does not stamp `appVersion`, so it
  stayed at the `0.0.0` placeholder and a default install pulled the
  non-existent `:0.0.0` image. The `app.kubernetes.io/version` label now tracks
  `.Chart.Version` as well (#47).
- The chart's default-deny NetworkPolicy no longer matches the singleton
  trust-bundle tbot pod (ADR 0008). The policy's `podSelector` only carried the
  shared `app.kubernetes.io/{name,instance}` labels, so the trust-bundle bot's
  egress to the Teleport proxy was denied on any NetworkPolicy-enforcing CNI --
  including kind >= v0.24, which made the `e2e-smoke` CI job flaky (pass only
  when kindnet's fail-open enforcement raced the bot's first join). Role-labeled
  pods are now excluded via `matchExpressions`, mirroring the
  PodDisruptionBudget's selector (#41).

## [1.0.1] - 2026-06-10

### Changed

- CI: adopt the dynamic-config setup workflow -- `.circleci/config.yml` is the generated setup config, the pipeline lives in `.circleci/workflows.yml`, and the repo-owned `e2e-smoke` job moved into `.circleci/custom.yml`, merged in at pipeline runtime. Tag publishes are no longer gated on `e2e-smoke`; e2e gates merges via the GitHub required check.

### Fixed

- Bump vulnerable Go module dependencies flagged by nancy.

## [1.0.0] - 2026-05-21

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
- Regenerate `.github/workflows/zz_generated.*.yaml` via devctl to use the centralized reusable workflow, removing the Node-20 `mindsers/changelog-reader-action` dependency.

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
- `hack/smoke/run.sh` wraps the runbook as a single-shot script
  suitable for CI: spins up the three kind clusters in parallel,
  builds and loads the operator image, provisions Teleport state
  via `tctl exec`, applies the sample `RemoteApp`, waits for
  `status.ready=true`, asserts the curl response body matches
  `hello-from-producer`. A bash `trap` tears down every kind
  cluster and tmp file on exit (success or failure); on failure
  the trap also dumps pod state and tbot/kube-agent logs from all
  three clusters. Locally verified: empty Docker to ✅ in under
  four minutes.
- `.circleci/config.yml` gains an `e2e-smoke` job (`machine`
  executor — kind needs real Docker, not the `remote-docker`
  shim) that installs kind/kubectl/helm/jq and runs
  `hack/smoke/run.sh`. The job sits between `architect/go-build`
  and `architect/push-to-registries`, so a red smoke blocks the
  release path. Self-contained — no external Teleport tenant, no
  CI secret context — which sidesteps the original HITL gate on
  test-credentials management.

[Unreleased]: https://github.com/giantswarm/tunnelport/compare/v1.0.4...HEAD
[1.0.4]: https://github.com/giantswarm/tunnelport/compare/v1.0.3...v1.0.4
[1.0.3]: https://github.com/giantswarm/tunnelport/compare/v1.0.2...v1.0.3
[1.0.2]: https://github.com/giantswarm/tunnelport/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/giantswarm/tunnelport/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/giantswarm/tunnelport/releases/tag/v1.0.0
