/*
Copyright 2026 Giant Swarm.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package remoteapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRBAC_RoleManifestExcludesPodsLog pins ADR 0003: the operator must
// never request `pods/log`. The kubebuilder marker block in
// controller.go must keep this rule, and `make manifests` regenerates
// config/rbac/role.yaml from those markers — so checking the rendered
// manifest catches both regressions: an accidental marker addition, or
// someone editing role.yaml by hand.
func TestRBAC_RoleManifestExcludesPodsLog(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "rbac", "role.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.Contains(string(body), "pods/log") {
		t.Fatalf("%s contains pods/log; ADR 0003 forbids that RBAC. "+
			"Remove the kubebuilder marker that introduced it.", path)
	}
}

// TestRBAC_RoleManifestGrantsPodsReadOnly mirrors the slice-4 contract:
// status reporting needs pod metadata read access, but only get/list/watch.
// No write verbs, no `pods/log`.
func TestRBAC_RoleManifestGrantsPodsReadOnly(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "rbac", "role.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(body), "- pods\n") &&
		!strings.Contains(string(body), "  - pods\n") {
		t.Fatalf("%s missing read access on pods; status reconciliation "+
			"needs get/list/watch on pods (no logs).", path)
	}
	// Forbidden write verbs on pods.
	for _, bad := range []string{"create\n  - pods", "update\n  - pods", "delete\n  - pods", "patch\n  - pods"} {
		if strings.Contains(string(body), bad) {
			t.Errorf("%s grants write verb on pods (%q); ADR 0003 limits us to read-only.", path, bad)
		}
	}
}

// TestRBAC_RoleManifestExcludesSecrets pins ADR 0006: the operator no
// longer reads, lists, or watches `Secret` resources. Slice 03 removed
// the `tokenRef` Secret-watch plumbing along with the field itself; with
// the kubernetes-join model there is no consumer-side Secret to inspect.
// The kubebuilder marker block in controller.go must keep that rule,
// and `make manifests` regenerates config/rbac/role.yaml from those
// markers — so checking the rendered manifest catches a regression
// where someone re-adds a `secrets` marker.
func TestRBAC_RoleManifestExcludesSecrets(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "rbac", "role.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.Contains(string(body), "- secrets\n") {
		t.Fatalf("%s grants access on secrets; ADR 0006 removes all "+
			"Secret-handling — the operator no longer needs that grant. "+
			"Remove the kubebuilder marker that introduced it.", path)
	}
}

// TestRBAC_RoleManifestGrantsServiceAccountsWrite pins slice 01: the
// operator renders a per-`RemoteApp` `ServiceAccount` so tbot can present
// its projected JWT to Central via the kubernetes join method. Without
// write access the rendered SA never lands and tbot cannot join.
func TestRBAC_RoleManifestGrantsServiceAccountsWrite(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "rbac", "role.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(body), "serviceaccounts") {
		t.Fatalf("%s missing access on serviceaccounts; slice 01 needs "+
			"create/update/delete on serviceaccounts to render the "+
			"per-RemoteApp SA used by the kubernetes join (ADR 0006).", path)
	}
}
