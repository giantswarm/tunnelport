# test/

This directory holds test assets that don't belong in package-local `*_test.go` files.

- `helm/` — bash assertions for the chart (RBAC, CRD gating, manager flag flow).

## Where's E2E?

The kubebuilder-scaffolded `test/e2e/` Ginkgo suite was removed: it only
verified that the controller-manager pod was up, didn't exercise the tunnel
data path, and never converged with the real E2E signal.

The actual end-to-end test is **`hack/smoke/run.sh`**. It spins up three
kind clusters (teleport, producer, consumer) to faithfully reproduce the
deployment topology — distinct API servers are load-bearing for the
kubernetes-join + JWKS path the operator depends on (see ADRs 0007/0008).
CircleCI gates merges to `main` on this job.

Controller-level tests live next to the code under
`internal/controller/<name>/` and run against envtest.
CRD acceptance lives in `internal/crdacceptance/`.
