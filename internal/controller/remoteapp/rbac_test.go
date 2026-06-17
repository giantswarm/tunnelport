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

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
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

// TestRBAC_RoleManifestGrantsServiceAccountsWrite pins the ADR 0004
// trade-off: the operator renders a per-CR ServiceAccount whose
// projected JWT the Teleport ProvisionToken's `allow` rule pins.
// Creating that SA requires the full write verbs.
func TestRBAC_RoleManifestGrantsServiceAccountsWrite(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "rbac", "role.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(body), "serviceaccounts") {
		t.Fatalf("%s missing serviceaccounts grant; ADR 0004 requires "+
			"the operator to render a per-CR ServiceAccount.", path)
	}
}

// TestRBAC_RoleManifestSecretsReadOnlyNoRolesRoleBindings pins the RBAC
// posture after the trust-bundle reloader landed:
//
//   - `core/secrets`: read-only (get;list;watch) only. The reloader reads
//     the single trust-bundle Secret to hash its CA bundle; it never writes
//     any Secret. A write verb on secrets is forbidden.
//   - `rbac.authorization.k8s.io/roles` and `rolebindings`: still absent
//     entirely (ADR 0008 removed the per-CR trust-bundle Role/RoleBinding;
//     no controller touches them).
//
// `make manifests` (controller-gen) regenerates config/rbac/role.yaml from
// the kubebuilder markers, so this catches both a marker regression and a
// hand-edit of role.yaml.
func TestRBAC_RoleManifestSecretsReadOnlyNoRolesRoleBindings(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "rbac", "role.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(body, &role); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	writeVerbs := map[string]bool{"create": true, "update": true, "patch": true, "delete": true, "deletecollection": true}
	sawSecrets := false
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			switch res {
			case "roles", "rolebindings":
				t.Errorf("%s still grants %q; ADR 0008 removed the per-CR trust-bundle Role/RoleBinding, so no verbs are needed.", path, res)
			case "secrets":
				sawSecrets = true
				for _, v := range rule.Verbs {
					if writeVerbs[v] || v == "*" {
						t.Errorf("%s grants write verb %q on secrets; the trust-bundle reloader is read-only.", path, v)
					}
				}
			}
		}
	}
	if !sawSecrets {
		t.Errorf("%s missing read access on secrets; the trust-bundle reloader needs get;list;watch.", path)
	}
}
