# Giant Swarm operator conventions — reference for tunnelport

A scan of how GS-owned kubebuilder-style operators are actually built in 2025–2026,
written before slice 1 of `tunnelport` lands. This is a "what convention exists,
when can we follow it, where do we deviate" doc — not a tutorial.

## Exemplars

The repos read for this doc, picked for being maintained (commits in last
~6 months) and broadly representative:

| Repo | Why it's a good exemplar |
|---|---|
| [`giantswarm/silence-operator`](https://github.com/giantswarm/silence-operator) | Most modern kubebuilder layout I found; recent v2 CRD with proper validation markers; canonical `Makefile.kubebuilder.mk`; CRD-bundled-in-chart pattern. |
| [`giantswarm/observability-operator`](https://github.com/giantswarm/observability-operator) | Multi-version CRD with conversion webhook; canonical `metav1.Condition` usage in reconcile; `internal/controller/` layout. |
| [`giantswarm/klaus-operator`](https://github.com/giantswarm/klaus-operator) | Recent (2025) operator that *deliberately* didn't go full kubebuilder — no `PROJECT` file, custom `Makefile.custom.mk` for CRD gen; useful as a "minimal viable GS operator" data point. |
| [`giantswarm/organization-operator`](https://github.com/giantswarm/organization-operator) | Tiny, single-CRD reconciler; closest in size profile to what tunnelport will ship. |
| [`giantswarm/teleport-operator`](https://github.com/giantswarm/teleport-operator) | Same domain (Teleport); has no CRDs of its own (reconciles `capi/Cluster`) but the chart, RBAC, and `architect` CI shape are reusable references. |
| [`giantswarm/dns-operator-azure`](https://github.com/giantswarm/dns-operator-azure) | Older `controllers/` layout with `Makefile.kubebuilder.mk`; useful as a contrast to silence-operator's `internal/controller/` style. |

There is also a dedicated GS skill — `gs-base:repository-creation` — which
documents the `giantswarm/template` and `giantswarm/template-app` template
repos plus `devctl repo setup`. That is the *starting point* for the bootstrap
mechanics (renovate, CODEOWNERS, branch protection, generated workflows); the
operator-specific conventions below are layered on top.

## 1. Project layout

The dominant modern layout (silence-operator, observability-operator,
organization-operator) matches vanilla `kubebuilder init` v4:

```
api/<vN>/                          types + zz_generated.deepcopy.go
cmd/main.go                        manager entry point   (silence)
main.go (root)                     manager entry point   (most others)
internal/controller/               reconcilers + suite_test.go
config/crd/bases/                  generated CRD YAMLs (controller-gen output)
config/rbac/                       generated role.yaml
config/samples/                    sample CRs + kustomization.yaml
hack/boilerplate.go.txt            Apache-2.0 header for code-gen
helm/<repo-name>/                  the operator's own chart
PROJECT                            kubebuilder marker file (DO NOT EDIT)
```

Notes that diverge from upstream kubebuilder defaults:
- Repos use both `cmd/main.go` (silence-operator, observability-operator) and
  `main.go` at root (klaus-operator, teleport-operator, dns-operator-azure).
  Vanilla v4 puts it at `cmd/main.go`. **No strong GS convention.** Pick one
  and don't agonise.
- The `internal/controller/` package name is the modern default; older repos
  (`teleport-operator`, `dns-operator-azure`) still use top-level
  `controllers/`. New code should use `internal/controller/`.
- `helm/<repo-name>/` is universal — chart name == repo name. Inside it,
  `templates/` holds the deployment, RBAC, ServiceAccount, NetworkPolicy, and
  (the surprise — see §6) **the CRDs themselves**.

**So for tunnelport: do** the silence-operator layout. `cmd/main.go`,
`api/v1alpha1/`, `internal/controller/`, `config/{crd,rbac,samples}/`,
`helm/tunnelport/`. Keep the kubebuilder-generated `PROJECT` file — `make
manifests` and several devctl tools key off it.

## 2. Build & tooling

Drawn from `silence-operator` `go.mod` + `Makefile.kubebuilder.mk` (which is
the most prescriptive Makefile in the sample):

| Concern | Convention | Tunnelport choice |
|---|---|---|
| Go version | `go 1.25` or `1.26` (silence: 1.25, observability: 1.26) | Match silence-operator: `1.25.x`. |
| controller-runtime | `v0.23.x` (silence) / `v0.21.x`–`v0.23.x` elsewhere | `v0.23.x`. |
| `k8s.io/{api,apimachinery,client-go}` | `v0.35.x` / `v0.36.x` (latest minor) | `v0.35.x` to match controller-runtime. |
| `controller-gen` | `v0.18.0`, pinned in Makefile | Pin same. |
| `kustomize` | `v5.6.0` | Pin same. |
| Lint | `golangci-lint` v2.x — `silence-operator` runs `-E gosec -E goconst --timeout=15m` in `make lint` | Same. |
| Pre-commit | `dnephin/pre-commit-golang` (go-fmt, go-mod-tidy, golangci-lint, go-imports `-local <module>`) plus `pre-commit/pre-commit-hooks` for whitespace/EOF | Same — pull `.pre-commit-config.yaml` from silence-operator nearly verbatim. |
| Image base | `gcr.io/distroless/static:nonroot`, `USER 65532:65532` | Same. |
| Image registry | `gsoci.azurecr.io/giantswarm/<name>` is the canonical primary registry now; some older charts also push to `quay.io/giantswarm` | Use `gsoci.azurecr.io/giantswarm/tunnelport`. |
| Versioning | SemVer; tags `vX.Y.Z`; Chart `version` and `appVersion` rewritten by ABS at release time (see `.abs/main.yaml`) | Same. |

`make manifests` is expected to regenerate `config/crd/bases/*.yaml` and
`config/rbac/role.yaml` from kubebuilder markers — see silence-operator's
`Makefile.kubebuilder.mk` `generate-crds` / `generate-rbac` targets, which the
slice-1 brief implicitly assumes.

**So for tunnelport: do** copy `Makefile.kubebuilder.mk` from
`silence-operator` largely as-is. It already implements `make manifests`,
`make generate`, `make test` (envtest + ginkgo), `make lint`,
`make verify-generate`. Plus a one-line top-level `Makefile` that
`include Makefile.*.mk`.

## 3. CI

**The org default for new operators is CircleCI with the `architect` orb,
not GitHub Actions and not Tekton.** Every operator I read
(silence-operator, observability-operator, klaus-operator, teleport-operator,
dns-operator-azure) has a `.circleci/config.yml` using
`giantswarm/architect@7.x` as the workhorse.

Typical pipeline (silence-operator is canonical):

```yaml
orbs:
  architect: giantswarm/architect@7.1.0
workflows:
  build:
    jobs:
      - go-tests           # CGO_ENABLED=0 make test
      - e2e-tests          # kind-based, optional
      - architect/go-build (binary, path=./cmd, on tags)
      - architect/push-to-registries (multi-arch on tags)
      - architect/push-to-app-catalog (chart=<repo>, app_catalog=control-plane-catalog)
```

`.github/workflows/` does exist in every repo, but it's exclusively
**`zz_generated.*` workflows owned by `devctl`** — gitleaks, OSSF scorecard,
release-PR creation, project-board automation, team-label sync. Not the
build/test/release pipeline. Don't author new GitHub Actions in here unless
you're adding something genuinely outside CircleCI's scope.

The `architect/push-to-app-catalog` step is what gets the chart from a `vX.Y.Z`
git tag into the GS app catalog — see §8.

**So for tunnelport: do** CircleCI + `architect` orb. Use the
`control-plane-catalog` (operators that run on management clusters live there;
`giantswarm-catalog` is the public/customer-facing one — `klaus-operator` uses
that, but tunnelport is platform-team infrastructure).

**Resolved:** `control-plane-catalog` (decision recorded under "Resolved
choices" at the bottom of this doc).

## 4. RBAC

Universal: **kubebuilder rbac markers above the reconciler**, generating into
`config/rbac/role.yaml` via `controller-gen rbac:roleName=manager-role`.
Example from `klaus-operator`:

```go
// +kubebuilder:rbac:groups=klaus.giantswarm.io,resources=klausmcpservers,verbs=get;list;watch
// +kubebuilder:rbac:groups=klaus.giantswarm.io,resources=klausmcpservers/status,verbs=get;update;patch
```

The chart's `templates/rbac.yaml` (or `clusterrole.yaml`) is then *hand-written*
to mirror those rules — it's not auto-derived from `config/rbac/role.yaml` in
any of the repos I read. Both files exist; both are kept in sync by the dev.
That's a known minor friction point but it's the convention.

Cluster-scoped `ClusterRole` + `ClusterRoleBinding` is the default pattern for
operators that watch CRs cluster-wide (silence-operator, klaus-operator,
observability-operator — all of which match tunnelport's stated topology).
Namespace-scoped `Role` is reserved for things like webhook configs.

**So for tunnelport: do** put kubebuilder rbac markers on the reconciler
struct as the source of truth, and write the helm `clusterrole.yaml` by hand
to match. `tunnelport` needs:

- Cluster-scoped read on `RemoteApp` and its status/finalizers subresources.
- Cluster-scoped read+watch on `Secret` (name+resourceVersion only — see CONTEXT.md).
- Namespace-scoped (but granted via ClusterRole) write on `Deployment`,
  `Service`, `ConfigMap`, `Event`.
- `coordination.k8s.io` `leases` for leader election.

Explicitly **no `pods/log`** — see ADR-0003. This is a deliberate deviation
from any future temptation to log-scrape; document it in the markers' comments.

## 5. Status conventions

Two patterns coexist in the org. New code should pick the second one.

- **Plain custom fields** (observability-operator's `GrafanaOrganization`,
  organization-operator's `Organization`) — just a struct of typed fields, no
  conditions slice. Simpler but loses the ecosystem's standard "is this thing
  Ready" semantics.
- **`metav1.Condition` slice + ObservedGeneration** (klaus-operator's
  `KlausMCPServer`, observability-operator's `AgentCredential`) — the modern
  upstream-Kubernetes convention. Status struct looks like:

  ```go
  type FooStatus struct {
      // +optional
      // +listType=map
      // +listMapKey=type
      Conditions []metav1.Condition `json:"conditions,omitempty"`
      // +optional
      ObservedGeneration int64 `json:"observedGeneration,omitempty"`
  }
  ```

  Conditions are set with `apimeta.SetStatusCondition` from
  `k8s.io/apimachinery/pkg/api/meta`, which handles transition-time and
  dedup correctly. Each `metav1.Condition` carries its own `ObservedGeneration`,
  but a top-level `Status.ObservedGeneration` is also set.

  No shared "GS conditions helper library" exists. Everyone calls
  `apimeta.SetStatusCondition` directly. (The legacy `giantswarm/microerror`
  + `giantswarm/micrologger` from the operatorkit era is **not** used in
  modern kubebuilder operators — controller-runtime's logger and standard
  `errors`/`fmt.Errorf` are the convention now.)

**So for tunnelport: do** the conditions-slice + per-condition
`ObservedGeneration` pattern. The slice-1 brief already specifies
`conditions []metav1.Condition`, `observedGeneration int64`, `ready bool`,
`lastError string` — that aligns perfectly with klaus-operator. The two named
condition types from CONTEXT.md (`Ready`, `TokenSecretBound`) become string
constants in `api/v1alpha1`.

The brief's `status.ready bool` is slightly redundant with a `Ready` condition
— it's there because CONTEXT.md describes it as a printer-column shorthand.
Keep it; just update both fields in the same `Status().Update` call and don't
let them drift.

## 6. Helm packaging

Universal: **one chart per operator repo, `helm/<repo-name>/`**, with:

```
helm/tunnelport/
  Chart.yaml            (annotations: io.giantswarm.application.team: <team>)
  values.yaml
  values.schema.json    (validated by zz_generated.check_values_schema.yaml workflow)
  templates/
    _helpers.tpl
    crds.yml            (or crds/ — see below)
    deployment.yaml
    rbac.yaml / clusterrole.yaml + clusterrolebinding.yaml
    service-account.yaml
    networkpolicy.yaml  (operator's own pod, gated on .Values.networkPolicy.enabled)
    pod-monitor.yaml    (gated on .Values.podMonitor.enabled)
```

**CRD packaging is split by approach:**

- `silence-operator` (the canonical modern way): the chart bundles the CRD
  inline in `templates/crds.yml`, gated by `{{- if .Values.crds.install -}}`,
  with Helm annotation `helm.sh/resource-policy: keep` on the CRD so it
  survives `helm uninstall`. A custom `make update-helm-crds` target
  regenerates this file from `config/crd/bases/` after `make manifests` —
  there's an explicit `verify-helm-crds` check in CI to catch drift.
- `klaus-operator`: puts CRDs in `helm/klaus-operator/crds/` (Helm's
  built-in CRD location). Simpler, but Helm's `crds/` directory is
  deliberately under-managed — no upgrade story and no `resource-policy`
  control.

**The silence-operator approach is what to copy** — it's more recent and the
on-uninstall-don't-delete-CRDs property matters when the operator manages
real workloads.

Image and tag injection follows the same shape everywhere:

```yaml
image:
  registry: gsoci.azurecr.io
  name: giantswarm/<repo>
  tag: ""        # falls back to .Chart.AppVersion
```

```yaml
image: "{{ .Values.image.registry }}/{{ .Values.image.name }}:{{ default .Chart.AppVersion .Values.image.tag }}"
```

The `[[ .Version ]]` / `[[ .AppVersion ]]` placeholders in
`klaus-operator/Chart.yaml` are app-build-suite (`abs`) substitution markers,
filled from the git tag at chart-publish time per `.abs/main.yaml`.
silence-operator hard-codes them in `Chart.yaml` and lets ABS overwrite
during release.

**So for tunnelport: do** silence-operator's `templates/crds.yml`
+ `crds.install: true` + `helm.sh/resource-policy: keep`
+ `make update-helm-crds` flow. One chart, named `tunnelport`. Image at
`gsoci.azurecr.io/giantswarm/tunnelport`.

**Deviation from convention to call out:** CONTEXT.md ("NetworkPolicy")
explicitly says the operator does **not** render `NetworkPolicy` for the
managed `tbot` Deployments. silence-operator and observability-operator both
do render a `NetworkPolicy` *for the operator's own pod* — that's fine,
follow that convention. The deviation is only about CRs not generating
`NetworkPolicy` resources for their tenant pods. The chart README needs to
explain the canonical labels (`tunnelport.giantswarm.io/role=tbot`,
`tunnelport.giantswarm.io/remoteapp=<name>`) so platform teams can write their
own.

## 7. Reconcile logging / metrics

`controller-runtime` defaults — no GS-specific wrapper. Reconcilers use:

```go
import "sigs.k8s.io/controller-runtime/pkg/log"
logger := log.FromContext(ctx)
```

Logger is configured in `main.go` via `log/zap`. No `giantswarm/micrologger`
in any of the modern repos.

Metrics: the controller-runtime default metrics endpoint on `:8080` (or
`:8443` for `--metrics-secure`), exposed via a `PodMonitor` in the chart.
Custom Prometheus counters use `prometheus/client_golang` directly
(`operatormetrics.AgentCredentialReconcileErrors.WithLabelValues(...).Inc()`
in observability-operator).

Health probes: `/healthz` and `/readyz` on `:8081` via
`controller-runtime/pkg/healthz`.

Events: `client-go`'s `record.EventRecorder`, sourced from the manager — see
`klaus-operator`'s `r.Recorder.Event(&server, corev1.EventTypeWarning, ...)`.
Events are not load-bearing for status; conditions are. Both are emitted in
parallel.

**Important:** the legacy `operatorkit` framework
(`giantswarm/operatorkit` + `giantswarm/microerror` + `giantswarm/micrologger`)
is now considered deprecated for new operators. `app-operator` and other older
repos still use it. New operators are kubebuilder/controller-runtime native.

**So for tunnelport: do** controller-runtime defaults — `log.FromContext`,
`zap` setup in main, `:8080` metrics, `:8081` healthz, no `microerror`/`micrologger`.

## 8. Release & distribution

Tag-driven via CircleCI + `architect` orb (already shown in §3). The flow:

1. PR merged to `main`.
2. `zz_generated.create_release_pr.yaml` workflow opens a release PR when
   `CHANGELOG.md` accumulates a new "Unreleased" section.
3. Maintainer merges; `zz_generated.create_release.yaml` cuts a `vX.Y.Z` tag.
4. CircleCI's tag-filter workflow runs:
   - `architect/go-build` produces the binary.
   - `architect/push-to-registries-multiarch` publishes the image to
     `gsoci.azurecr.io` (and historically `quay.io`).
   - `architect/push-to-app-catalog` (with `executor: app-build-suite`)
     packages the chart from `helm/<name>/` and pushes to the configured
     `app_catalog` (`control-plane-catalog` for operators that run on MCs,
     `giantswarm-catalog` for end-user-facing apps).
5. The chart is then consumable by `app-operator` on each MC via an `App` CR
   (the standard GS app-platform flow). `flux-operator-app` and
   `gitops-template`-based delivery is the typical end-to-end path.

`.abs/main.yaml` configures `app-build-suite` to rewrite the chart's `version`
and `appVersion` from the git tag at release time (see silence-operator's:
`replace-app-version-with-git: true`).

**So for tunnelport: do** the same flow. `.abs/main.yaml` plus an
`architect/push-to-app-catalog` step in the CircleCI workflow. Then the chart
shows up in `control-plane-catalog` and a downstream `App` CR (managed by
GitOps elsewhere, not this repo) installs it on each consumer MC.

## 9. Repository hygiene

The `gs-base:repository-creation` skill covers this in detail; the operator-
specific repo setup ends up with this set of mandatory top-level files:

| File | Source |
|---|---|
| `LICENSE` | Apache-2.0, dropped in by template. |
| `DCO` | Developer Certificate of Origin. |
| `SECURITY.md` | One-line link to giantswarm.io/responsible-disclosure. |
| `CHANGELOG.md` | "Keep a Changelog" format, `## [Unreleased]` tip; updated by every PR. |
| `CODEOWNERS` | Generated from `giantswarm/github`'s team file — `* @giantswarm/team-<name>` — DO NOT hand-edit. |
| `README.md` | Project description + Helm install snippets (OCI registry first, then helm-repo). silence-operator's is a good template. |
| `renovate.json5` | `extends: ["github>giantswarm/renovate-presets:default.json5", "github>giantswarm/renovate-presets:lang-go.json5"]` |
| `.pre-commit-config.yaml` | "maintained centrally at giantswarm/github/languages/go" — copy from silence-operator. |
| `.golangci.yml` | Minimal v2 config, e.g. klaus-operator's. |
| `.dockerignore`, `.nancy-ignore`, `.nancy-ignore.generated` | Generated by `devctl`. |
| `.github/workflows/zz_generated.*` | Generated by `devctl gen workflow`. Do not edit. |
| `Makefile`, `Makefile.gen.app.mk`, `Makefile.gen.go.mk` | Generated by `devctl gen makefile`. |
| `Makefile.kubebuilder.mk` | Hand-authored, copied from silence-operator. |
| `hack/boilerplate.go.txt` | Apache-2.0 header used by controller-gen for `zz_generated.deepcopy.go`. |
| `PROJECT` | kubebuilder marker — don't hand-edit, but track in git. |

**No `OWNERS` file** in any of these repos (that's a Kubernetes-OWNERS style
that GS doesn't use). `CODEOWNERS` is the team-ownership file.

License header convention is the standard upstream Apache-2.0 boilerplate
("Copyright YYYY. Licensed under the Apache License, Version 2.0..."), placed
on every Go file by controller-gen via `hack/boilerplate.go.txt`. The
`Copyright` year is auto-stamped via `controller-gen object:headerFile=...,year=$(date +%Y)`.

**So for tunnelport: do** start from `giantswarm/template` + `devctl repo
setup` per the `gs-base:repository-creation` skill, then layer the
silence-operator-style operator scaffolding (kubebuilder `init` + `create
api`, then port `Makefile.kubebuilder.mk`).

## Resolved choices

Decisions made by the maintainer (2026-05-05); no longer open.

| # | Topic | Decision |
|---|---|---|
| 1 | **Owning team** | `bumblebee` — drives `CODEOWNERS`, the `application.giantswarm.io/team: bumblebee` annotation in `Chart.yaml`, and project-board routing. |
| 2 | **App catalog destination** | `control-plane-catalog` — operator runs on management clusters only. |
| 3 | **Image-pull secret** | Inherit the cluster-managed `gsoci.azurecr.io` pull-secret (the standard MC pattern). The chart references it via `imagePullSecrets:` on the manager pod template; the chart does not render the Secret itself. |
| 4 | **Scaffold** | Full kubebuilder v4 — `kubebuilder init` + `kubebuilder create api`, with `cmd/main.go`, marker-driven RBAC, the standard `Makefile.kubebuilder.mk` split. Match silence-operator's layout. |

Still floating, but lower-stakes:

- **architect-orb pin floor** — silence-operator pins `giantswarm/architect@7.1.0` at this writing. Default to whatever the latest 7.x is when slice-7 lands; float-vs-pin is a CI hygiene call, not a design call.
