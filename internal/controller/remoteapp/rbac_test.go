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

// TestRBAC_RoleManifestGrantsSecretsForTrustBundle pins slice 03 (ADR
// 0007 §"Trust bundle distribution to consumers", which supersedes the
// ADR 0004 no-Secrets posture for this specific case): the operator
// pre-creates a per-CR `<cr.Name>-spiffe-bundle` Secret with an
// ownerRef back to the CR (tbot owns the Data via its
// `kubernetes_secret` destination). Pre-creating that Secret requires
// the full write verbs on core/secrets at the operator's ClusterRole
// level — narrowed back to a single resourceName at the per-CR
// in-namespace Role the operator also renders.
//
// The token-Secret prohibition from ADR 0004 still holds at the model
// level: no Secret carries a static Teleport join token; the
// kubernetes-join JWT is mounted via the projected SA token volume.
func TestRBAC_RoleManifestGrantsSecretsForTrustBundle(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "rbac", "role.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(body), "- secrets\n") &&
		!strings.Contains(string(body), "  - secrets\n") {
		t.Fatalf("%s missing secrets grant; ADR 0007 trust-bundle "+
			"distribution requires the operator to pre-create "+
			"the per-CR `<name>-spiffe-bundle` Secret.", path)
	}
}

// TestRBAC_RoleManifestGrantsRolesAndRoleBindings pins slice 03 (ADR
// 0007): the operator renders a per-CR Role + RoleBinding that scopes
// the per-CR ServiceAccount's authority on the trust-bundle Secret to a
// single resourceName. Creating those requires write verbs on the
// rbac.authorization.k8s.io group.
func TestRBAC_RoleManifestGrantsRolesAndRoleBindings(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "rbac", "role.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, want := range []string{"- roles\n", "- rolebindings\n"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("%s missing %s grant; ADR 0007 trust-bundle distribution needs it.", path, strings.TrimSpace(want))
		}
	}
}
