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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// Aliases keep the readiness-probe assertion legible without importing
// intstr at every call site.
const (
	intstrTypeInt    = intstr.Int
	intstrTypeString = intstr.String
)

// renderFixtureOpts is the renderer-test-specific override of
// newRemoteApp's defaults: namespace="demo", name="tracer", appName="myapp",
// tokenName="myapp-token". These match the strings the assertions in
// this file pin verbatim. The shared newRemoteApp() defaults match the
// envtest fixtures (name="demo"), so the renderer tests pass these opts
// to keep their assertions stable.
var renderFixtureOpts = []fixtureOpt{
	withName("demo", "tracer"),
	withUID("uid-tracer"),
	withAppName("myapp"),
	withTokenName("myapp-token"),
}

func fixtureRemoteApp() *accessv1alpha1.RemoteApp {
	return newRemoteApp(renderFixtureOpts...)
}

func fixtureConfig() PodDefaults {
	return PodDefaults{
		TbotImage: "registry.example.com/tbot:1.2.3",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		// Operator-level Teleport binding (ADR 0005). Values match the
		// pre-removal CR fixture defaults so existing render assertions
		// against `proxy_server: teleport.example.com:443` keep passing.
		TeleportClusterName: "teleport.example.com",
		TeleportProxyAddr:   "teleport.example.com:443",
		// Ghostunnel sidecar defaults (slice 02, ADR 0007). The reload
		// interval is safe at 5m because tbot renews the SVID every 20m
		// by default — see render_test workload-identity-x509 assertion.
		GhostunnelImage:          "registry.example.com/ghostunnel:v1.2.3",
		GhostunnelReloadInterval: "5m",
		GhostunnelListenPort:     tlsListenPortDefault,
	}
}

func TestRenderConfigMap_ContainsTbotApplicationTunnelConfig(t *testing.T) {
	cr := fixtureRemoteApp()

	cm := renderConfigMap(cr, fixtureConfig())

	if cm.Name != cr.Name {
		t.Fatalf("ConfigMap name: want %q, got %q", cr.Name, cm.Name)
	}
	if cm.Namespace != cr.Namespace {
		t.Fatalf("ConfigMap namespace: want %q, got %q", cr.Namespace, cm.Namespace)
	}

	cfg, ok := cm.Data["tbot.yaml"]
	if !ok {
		t.Fatalf("ConfigMap missing tbot.yaml key; got keys: %v", keys(cm.Data))
	}

	wants := []string{
		"proxy_server: teleport.example.com:443",
		"app_name: myapp",
		"listen: tcp://0.0.0.0:8080",
		"type: application-tunnel",
		// Per ADR 0004 tbot joins via the kubernetes join method using
		// the projected SA JWT; `token` is the literal ProvisionToken
		// name on Central, not a file path.
		"join_method: kubernetes",
		"token: myapp-token",
		// diag_addr binds the /readyz HTTP endpoint that the pod's
		// readiness probe (slice 4) targets.
		"diag_addr: 0.0.0.0:3001",
	}
	for _, want := range wants {
		if !strings.Contains(cfg, want) {
			t.Errorf("tbot.yaml missing %q\n---\n%s\n---", want, cfg)
		}
	}

	// Regression guards: these are upstream-tbot field names that
	// earlier drafts of the renderer invented. If they reappear, tbot
	// will reject the config at startup.
	bannedSubstrings := []string{
		"token_secret_ref", // not a real tbot field; only `token:` exists
		"listener:",        // wrong spelling — upstream tag is `listen`
		"auth_server:",     // we use proxy_server only; auth_server here was a copy-paste
	}
	for _, banned := range bannedSubstrings {
		if strings.Contains(cfg, banned) {
			t.Errorf("tbot.yaml must not contain %q (rejected by tbot at startup)\n---\n%s\n---", banned, cfg)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestRenderConfigMap_Insecure verifies that `Config.Insecure` adds
// `insecure: true` at the top level of the rendered tbot YAML so the
// pod skips Teleport proxy TLS verification. Off by default — the
// regression guard in the main config-content test already asserts
// the line is absent when not set.
func TestRenderConfigMap_Insecure(t *testing.T) {
	cr := fixtureRemoteApp()
	cfg := fixtureConfig()
	cfg.Insecure = true

	cm := renderConfigMap(cr, cfg)
	yaml := cm.Data["tbot.yaml"]

	if !strings.Contains(yaml, "insecure: true") {
		t.Errorf("Config.Insecure=true should add `insecure: true` to tbot.yaml\n---\n%s\n---", yaml)
	}
}

// TestRenderConfigMap_NotInsecureByDefault is the explicit regression
// guard against the insecure flag leaking into production renders.
func TestRenderConfigMap_NotInsecureByDefault(t *testing.T) {
	cr := fixtureRemoteApp()

	cm := renderConfigMap(cr, fixtureConfig())
	yaml := cm.Data["tbot.yaml"]

	if strings.Contains(yaml, "insecure:") {
		t.Errorf("default Config must not render `insecure:` — it would disable TLS verification in production\n---\n%s\n---", yaml)
	}
}

// TestRenderConfigMap_ContainsWorkloadIdentityX509Service asserts the
// second tbot service block per ADR 0007: a `workload-identity-x509`
// service bound to the per-RemoteApp WorkloadIdentity by the
// `remoteapp: ${cr.Name}` label, writing the SVID trio (svid.pem,
// svid_key.pem, svid_bundle.pem) to an in-pod directory destination
// shared with the future ghostunnel sidecar (slice 02).
//
// Selector field shape (`selector:` with `labels:` sub-map) follows the
// tbot v18 docs for the workload-identity-x509 service. The credential
// lifetime defaults (60min TTL / 20min renewal) are the issue 01
// acceptance criterion for surviving ghostunnel's file-watch reload.
func TestRenderConfigMap_ContainsWorkloadIdentityX509Service(t *testing.T) {
	cr := fixtureRemoteApp()

	cm := renderConfigMap(cr, fixtureConfig())
	cfg, ok := cm.Data["tbot.yaml"]
	if !ok {
		t.Fatalf("ConfigMap missing tbot.yaml key; got keys: %v", keys(cm.Data))
	}

	wants := []string{
		"type: workload-identity-x509",
		// Selector binds the SVID to the per-RemoteApp WorkloadIdentity
		// resource labelled `remoteapp: <cr.Name>` (hack/smoke/teleport/
		// workload-identity.yaml).
		"selector:",
		// Value is a one-element list — tbot's upstream selector.labels
		// schema is []string per key (a scalar value fails config parse).
		"- tracer",
		// Directory destination — the same emptyDir mount the ghostunnel
		// sidecar will read from in slice 02.
		"type: directory",
		"path: " + mountPathSVID,
		// Credential lifetime defaults the file-watch reload requires.
		"credential_ttl: 60m",
		"renewal_interval: 20m",
	}
	for _, want := range wants {
		if !strings.Contains(cfg, want) {
			t.Errorf("tbot.yaml missing %q\n---\n%s\n---", want, cfg)
		}
	}

	// Both service blocks must coexist — slice 01 must not regress the
	// existing application-tunnel.
	if !strings.Contains(cfg, "type: application-tunnel") {
		t.Errorf("tbot.yaml lost the application-tunnel service block\n---\n%s\n---", cfg)
	}
}

// TestRenderConfigMap_WorkloadIdentityHasKubernetesSecretDestination
// pins slice 03 (ADR 0007 §"Trust bundle distribution to consumers"):
// the workload-identity-x509 service carries TWO destinations — the
// existing in-pod directory (ghostunnel reads it) AND a kubernetes_secret
// destination writing svid_bundle.pem into the per-CR
// `<cr.Name>-spiffe-bundle` Secret consumer pods mount.
//
// Upstream shape: each workload-identity-x509 service has a SINGULAR
// `destination:` (see Teleport
// `lib/tbot/services/workloadidentity/x509_output_config.go`). To get
// both a directory destination (for ghostunnel) and a kubernetes_secret
// destination (for consumer pods), the renderer emits TWO
// workload-identity-x509 service blocks sharing the same selector.
func TestRenderConfigMap_WorkloadIdentityHasKubernetesSecretDestination(t *testing.T) {
	cr := fixtureRemoteApp()

	cm := renderConfigMap(cr, fixtureConfig())
	cfg, ok := cm.Data["tbot.yaml"]
	if !ok {
		t.Fatalf("ConfigMap missing tbot.yaml key; got keys: %v", keys(cm.Data))
	}

	// Two workload-identity-x509 service blocks, one per destination
	// type. The string appears once per service block (the `type:` tag
	// in each).
	if got := strings.Count(cfg, "type: workload-identity-x509"); got != 2 {
		t.Errorf("expected 2 workload-identity-x509 service blocks, got %d\n---\n%s\n---", got, cfg)
	}

	// Directory destination (slice 01) — preserved.
	if !strings.Contains(cfg, "type: directory") {
		t.Errorf("tbot.yaml lost the directory destination from slice 01\n---\n%s\n---", cfg)
	}
	if !strings.Contains(cfg, "path: "+mountPathSVID) {
		t.Errorf("tbot.yaml directory destination path: want %q\n---\n%s\n---", mountPathSVID, cfg)
	}

	// kubernetes_secret destination (slice 03) — name follows the
	// `${cr.Name}-spiffe-bundle` convention pinned in the ADR.
	if !strings.Contains(cfg, "type: kubernetes_secret") {
		t.Errorf("tbot.yaml missing kubernetes_secret destination (slice 03)\n---\n%s\n---", cfg)
	}
	wantSecret := cr.Name + "-spiffe-bundle"
	if !strings.Contains(cfg, "name: "+wantSecret) {
		t.Errorf("tbot.yaml kubernetes_secret destination name: want %q\n---\n%s\n---", wantSecret, cfg)
	}
}

// TestRenderDeployment_SVIDEmptyDirSharedWithTbot pins the in-pod
// emptyDir volume the workload-identity-x509 service writes the SVID
// trio into. The same volume will be mounted by the ghostunnel sidecar
// in slice 02 (read-only); slice 01 only adds the tbot-side mount.
//
// The mount is NOT readOnly — tbot writes the SVID files into it on
// every renewal.
func TestRenderDeployment_SVIDEmptyDirSharedWithTbot(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig())
	pod := dep.Spec.Template.Spec

	svid := volumeByName(pod.Volumes, volumeNameSVID)
	if svid == nil {
		t.Fatalf("missing %q volume; got: %v", volumeNameSVID, volumeNames(pod.Volumes))
	}
	if svid.EmptyDir == nil {
		t.Errorf("%q must be an EmptyDir (shared with ghostunnel sidecar in slice 02); got %+v", volumeNameSVID, svid)
	}

	c := pod.Containers[0]
	var svidMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == volumeNameSVID {
			svidMount = &c.VolumeMounts[i]
			break
		}
	}
	if svidMount == nil {
		t.Fatalf("tbot container missing %q mount; got: %v", volumeNameSVID, mountNames(c.VolumeMounts))
	}
	if svidMount.MountPath != mountPathSVID {
		t.Errorf("%q mountPath: want %q, got %q", volumeNameSVID, mountPathSVID, svidMount.MountPath)
	}
	if svidMount.ReadOnly {
		t.Errorf("%q mount must NOT be readOnly — tbot writes SVID renewals into it", volumeNameSVID)
	}

	// Existing four volumes must still be present — slice 01 is additive.
	for _, name := range []string{volumeNameTbotConfig, volumeNameTbotJoinSAToken, volumeNameTbotStorage, volumeNameTbotTmp} {
		if !hasVolume(pod.Volumes, name) {
			t.Errorf("slice 01 regressed existing volume %q; got: %v", name, volumeNames(pod.Volumes))
		}
		if !hasMount(c.VolumeMounts, name) {
			t.Errorf("slice 01 regressed existing mount %q; got: %v", name, mountNames(c.VolumeMounts))
		}
	}
}

func TestRenderDeployment_DefaultsAndStrategy(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig())

	if dep.Name != cr.Name {
		t.Fatalf("Deployment name: want %q, got %q", cr.Name, dep.Name)
	}
	if dep.Namespace != cr.Namespace {
		t.Fatalf("Deployment namespace: want %q, got %q", cr.Namespace, dep.Namespace)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("Deployment replicas: want 1 by default, got %v", dep.Spec.Replicas)
	}
	if dep.Spec.Strategy.RollingUpdate == nil {
		t.Fatal("Deployment.Strategy.RollingUpdate must be set")
	}
	if got := dep.Spec.Strategy.RollingUpdate.MaxSurge; got == nil || got.IntValue() != 1 {
		t.Errorf("RollingUpdate.MaxSurge: want 1, got %v", got)
	}
	if got := dep.Spec.Strategy.RollingUpdate.MaxUnavailable; got == nil || got.IntValue() != 0 {
		t.Errorf("RollingUpdate.MaxUnavailable: want 0, got %v", got)
	}
	if got, want := dep.Spec.Selector.MatchLabels[LabelRole], LabelRoleValue; got != want {
		t.Errorf("Selector matchLabels[%s]: want %q, got %q", LabelRole, want, got)
	}
	if got, want := dep.Spec.Template.Labels[LabelRemoteAppInstance], cr.Name; got != want {
		t.Errorf("Pod template label[%s]: want %q, got %q", LabelRemoteAppInstance, want, got)
	}
}

func TestRenderDeployment_RespectsExplicitReplicas(t *testing.T) {
	cr := fixtureRemoteApp()
	r := int32(3)
	cr.Spec.Replicas = &r

	dep := renderDeployment(cr, fixtureConfig())

	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 3 {
		t.Errorf("Deployment replicas: want 3, got %v", dep.Spec.Replicas)
	}
}

func TestRenderDeployment_PodTemplateMountsConfigMapAndEmptyDir(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig())

	pod := dep.Spec.Template.Spec
	// Slice 02 adds the ghostunnel sidecar; the tbot container we inspect
	// in this test remains the first container.
	if len(pod.Containers) != 2 {
		t.Fatalf("expected 2 containers in pod (tbot + ghostunnel), got %d", len(pod.Containers))
	}
	c := pod.Containers[0]

	// ConfigMap volume mounted (read-only, name-only reference).
	if !hasVolume(pod.Volumes, "tbot-config") {
		t.Errorf("missing volume tbot-config; got: %v", volumeNames(pod.Volumes))
	}
	if !hasMount(c.VolumeMounts, "tbot-config") {
		t.Errorf("container missing volumeMount tbot-config; got: %v", mountNames(c.VolumeMounts))
	}

	// Per ADR 0004 the kubernetes-join model uses the projected SA JWT —
	// no static-token Secret volume is mounted into the tbot pod.
	if hasVolume(pod.Volumes, "tbot-token") {
		t.Errorf("tbot-token volume must NOT exist under kubernetes-join model; got volumes: %v", volumeNames(pod.Volumes))
	}
	if hasMount(c.VolumeMounts, "tbot-token") {
		t.Errorf("tbot-token mount must NOT exist under kubernetes-join model; got mounts: %v", mountNames(c.VolumeMounts))
	}
	// Reconciler must NOT inject anything from a Secret via env / envFrom.
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			t.Errorf("container env %q references a Secret directly; the kubernetes-join model has no token Secret", e.Name)
		}
	}
	if len(c.EnvFrom) > 0 {
		t.Errorf("container envFrom must be empty (no Secret/ConfigMap envFrom); got %d entries", len(c.EnvFrom))
	}

	// emptyDir destination dir per ADR 0002 — no PVC.
	dest := volumeByName(pod.Volumes, "tbot-storage")
	if dest == nil {
		t.Fatalf("missing tbot-storage volume; got: %v", volumeNames(pod.Volumes))
	}
	if dest.EmptyDir == nil {
		t.Errorf("tbot-storage must be an EmptyDir per ADR 0002; got %+v", dest)
	}
	if dest.PersistentVolumeClaim != nil {
		t.Errorf("tbot-storage must not be a PVC per ADR 0002")
	}
}

// TestRenderServiceAccount_NameMatchesCR pins the per-CR ServiceAccount
// the kubernetes-join model (ADR 0004) requires.
func TestRenderServiceAccount_NameMatchesCR(t *testing.T) {
	cr := fixtureRemoteApp()

	sa := renderServiceAccount(cr, fixtureConfig())

	if sa.Name != cr.Name {
		t.Errorf("ServiceAccount name: want %q, got %q", cr.Name, sa.Name)
	}
	if sa.Namespace != cr.Namespace {
		t.Errorf("ServiceAccount namespace: want %q, got %q", cr.Namespace, sa.Namespace)
	}
	if got, want := sa.Labels[LabelRole], LabelRoleValue; got != want {
		t.Errorf("ServiceAccount labels[%s]: want %q, got %q", LabelRole, want, got)
	}
	if got, want := sa.Labels[LabelRemoteAppInstance], cr.Name; got != want {
		t.Errorf("ServiceAccount labels[%s]: want %q, got %q", LabelRemoteAppInstance, want, got)
	}
	if sa.Kind != "ServiceAccount" || sa.APIVersion != "v1" {
		t.Errorf("ServiceAccount TypeMeta: want v1/ServiceAccount, got %q/%q", sa.APIVersion, sa.Kind)
	}
}

// TestRenderDeployment_PodTemplateUsesPerCRServiceAccount asserts the pod
// template binds to the rendered ServiceAccount and opts into the
// projected token mount the kubernetes-join model needs.
func TestRenderDeployment_PodTemplateUsesPerCRServiceAccount(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig())
	pod := dep.Spec.Template.Spec

	if pod.ServiceAccountName != cr.Name {
		t.Errorf("PodSpec.ServiceAccountName: want %q (cr.Name), got %q", cr.Name, pod.ServiceAccountName)
	}
	if pod.AutomountServiceAccountToken == nil || !*pod.AutomountServiceAccountToken {
		t.Errorf("PodSpec.AutomountServiceAccountToken: want true (ADR 0004 needs the projected JWT); got %v", pod.AutomountServiceAccountToken)
	}
}

func TestRenderDeployment_UsesOperatorConfigImageAndResources(t *testing.T) {
	cr := fixtureRemoteApp()
	cfg := fixtureConfig()

	dep := renderDeployment(cr, cfg)

	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != cfg.TbotImage {
		t.Errorf("container image: want %q (from config), got %q", cfg.TbotImage, c.Image)
	}
	gotCPU := c.Resources.Requests[corev1.ResourceCPU]
	wantCPU := cfg.Resources.Requests[corev1.ResourceCPU]
	if gotCPU.Cmp(wantCPU) != 0 {
		t.Errorf("container CPU request: want %s, got %s", (&wantCPU).String(), (&gotCPU).String())
	}
	gotMem := c.Resources.Limits[corev1.ResourceMemory]
	wantMem := cfg.Resources.Limits[corev1.ResourceMemory]
	if gotMem.Cmp(wantMem) != 0 {
		t.Errorf("container memory limit: want %s, got %s", (&wantMem).String(), (&gotMem).String())
	}
}

func TestRenderDeployment_ContainerPortMatchesSpecPort(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig())

	c := dep.Spec.Template.Spec.Containers[0]
	if len(c.Ports) == 0 {
		t.Fatalf("container has no ports")
	}
	if c.Ports[0].ContainerPort != cr.Spec.Port {
		t.Errorf("containerPort: want %d, got %d", cr.Spec.Port, c.Ports[0].ContainerPort)
	}
}

// TestRenderDeployment_ReadinessProbeHitsTbotDiagEndpoint pins the
// acceptance criterion that pod-Ready means tunnel-up. tbot's diag
// endpoint listens on 127.0.0.1:3001 and exposes /readyz, which only
// returns 200 once the application-tunnel is established. The rendered
// container must (a) declare a containerPort named "diag" on 3001 so the
// kubelet has a target to probe, and (b) define a readinessProbe with an
// HTTPGet action on that port and path.
func TestRenderDeployment_ReadinessProbeHitsTbotDiagEndpoint(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig())

	c := dep.Spec.Template.Spec.Containers[0]

	// diag port must be present and named so the probe can reference it.
	var diagPort *corev1.ContainerPort
	for i := range c.Ports {
		if c.Ports[i].Name == tbotDiagPortName {
			diagPort = &c.Ports[i]
			break
		}
	}
	if diagPort == nil {
		t.Fatalf("container missing 'diag' containerPort; got: %+v", c.Ports)
	}
	if diagPort.ContainerPort != 3001 {
		t.Errorf("diag containerPort: want 3001, got %d", diagPort.ContainerPort)
	}

	// Readiness probe must be set, and must hit the diag /readyz endpoint
	// so pod-Ready reflects tunnel-up, not just process-up.
	if c.ReadinessProbe == nil {
		t.Fatalf("container missing readinessProbe")
	}
	if c.ReadinessProbe.HTTPGet == nil {
		t.Fatalf("readinessProbe must use HTTPGet (diag endpoint); got %+v", c.ReadinessProbe)
	}
	if c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Errorf("readinessProbe path: want /readyz, got %q", c.ReadinessProbe.HTTPGet.Path)
	}
	// Either named-port reference or numeric 3001 is acceptable; tests
	// pin one to keep the rendered template stable.
	tp := c.ReadinessProbe.HTTPGet.Port
	if tp.Type == intstrTypeString && tp.StrVal != tbotDiagPortName {
		t.Errorf("readinessProbe port (string): want %q, got %q", tbotDiagPortName, tp.StrVal)
	}
	if tp.Type == intstrTypeInt && tp.IntVal != 3001 {
		t.Errorf("readinessProbe port (int): want 3001, got %d", tp.IntVal)
	}
}

// TestRenderDeployment_PodAndContainerSecurityContext pins the hardened
// defaults the rendered tbot pod runs with. The values mirror the
// operator's own pod template (helm/tunnelport/values.yaml): nonroot
// distroless UID, RuntimeDefault seccomp at pod level; drop ALL caps,
// readOnlyRootFilesystem, no privilege escalation at container level.
// They are not currently CR-tunable — this test guards against silent
// drift if anyone removes them.
func TestRenderDeployment_PodAndContainerSecurityContext(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig())

	// Pod-level securityContext.
	pc := dep.Spec.Template.Spec.SecurityContext
	if pc == nil {
		t.Fatalf("PodSpec.SecurityContext must be set")
	}
	if pc.RunAsNonRoot == nil || !*pc.RunAsNonRoot {
		t.Errorf("PodSpec.SecurityContext.RunAsNonRoot: want true, got %v", pc.RunAsNonRoot)
	}
	if pc.RunAsUser == nil || *pc.RunAsUser != 65532 {
		t.Errorf("PodSpec.SecurityContext.RunAsUser: want 65532 (distroless nonroot), got %v", pc.RunAsUser)
	}
	if pc.SeccompProfile == nil || pc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("PodSpec.SecurityContext.SeccompProfile: want type=RuntimeDefault, got %+v", pc.SeccompProfile)
	}

	// Container-level securityContext.
	c := dep.Spec.Template.Spec.Containers[0]
	cc := c.SecurityContext
	if cc == nil {
		t.Fatalf("Container.SecurityContext must be set")
	}
	if cc.AllowPrivilegeEscalation == nil || *cc.AllowPrivilegeEscalation {
		t.Errorf("Container.SecurityContext.AllowPrivilegeEscalation: want false, got %v", cc.AllowPrivilegeEscalation)
	}
	if cc.ReadOnlyRootFilesystem == nil || !*cc.ReadOnlyRootFilesystem {
		t.Errorf("Container.SecurityContext.ReadOnlyRootFilesystem: want true, got %v", cc.ReadOnlyRootFilesystem)
	}
	if cc.Capabilities == nil {
		t.Fatalf("Container.SecurityContext.Capabilities must be set")
	}
	gotDrop := cc.Capabilities.Drop
	if len(gotDrop) != 1 || gotDrop[0] != "ALL" {
		t.Errorf("Container.SecurityContext.Capabilities.Drop: want [ALL], got %v", gotDrop)
	}
}

// TestRenderDeployment_TmpEmptyDirSatisfiesReadOnlyRootFilesystem pins
// the /tmp emptyDir we add to keep tbot writable scratch space available
// despite readOnlyRootFilesystem: true. Without this, Go runtime / glibc
// resolver writes inside the container would EROFS unpredictably.
func TestRenderDeployment_TmpEmptyDirSatisfiesReadOnlyRootFilesystem(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig())
	pod := dep.Spec.Template.Spec

	tmp := volumeByName(pod.Volumes, "tbot-tmp")
	if tmp == nil {
		t.Fatalf("missing tbot-tmp volume; got: %v", volumeNames(pod.Volumes))
	}
	if tmp.EmptyDir == nil {
		t.Errorf("tbot-tmp must be an EmptyDir; got %+v", tmp)
	}

	c := pod.Containers[0]
	var tmpMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == "tbot-tmp" {
			tmpMount = &c.VolumeMounts[i]
			break
		}
	}
	if tmpMount == nil {
		t.Fatalf("container missing tbot-tmp mount; got: %v", mountNames(c.VolumeMounts))
	}
	if tmpMount.MountPath != "/tmp" {
		t.Errorf("tbot-tmp mountPath: want /tmp, got %q", tmpMount.MountPath)
	}
}

// TestRenderDeployment_LivenessProbe pins the kubelet-driven recovery
// contract from ADR 0003: the rendered pod has a liveness probe on
// tbot's diag port so a wedged listener triggers a restart, but the
// thresholds are deliberately generous so transient slowness doesn't
// induce restart storms.
func TestRenderDeployment_LivenessProbe(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig())

	c := dep.Spec.Template.Spec.Containers[0]
	if c.LivenessProbe == nil {
		t.Fatalf("container missing livenessProbe")
	}
	lp := c.LivenessProbe

	// TCPSocket on diag port — no commitment to a /livez HTTP path
	// that may not exist on every tbot version.
	if lp.TCPSocket == nil {
		t.Fatalf("livenessProbe must use TCPSocket on the diag port; got %+v", lp)
	}
	tp := lp.TCPSocket.Port
	if tp.Type == intstrTypeString && tp.StrVal != tbotDiagPortName {
		t.Errorf("livenessProbe TCPSocket port (string): want %q, got %q", tbotDiagPortName, tp.StrVal)
	}
	if tp.Type == intstrTypeInt && tp.IntVal != 3001 {
		t.Errorf("livenessProbe TCPSocket port (int): want 3001, got %d", tp.IntVal)
	}

	// HTTPGet must NOT be set — keeps the probe agnostic to tbot's
	// HTTP surface beyond /readyz (which the readiness probe owns).
	if lp.HTTPGet != nil {
		t.Errorf("livenessProbe must not declare HTTPGet (TCPSocket only); got %+v", lp.HTTPGet)
	}

	if lp.InitialDelaySeconds != 30 {
		t.Errorf("livenessProbe.InitialDelaySeconds: want 30 (allow join handshake), got %d", lp.InitialDelaySeconds)
	}
	if lp.PeriodSeconds != 30 {
		t.Errorf("livenessProbe.PeriodSeconds: want 30, got %d", lp.PeriodSeconds)
	}
	if lp.FailureThreshold != 5 {
		t.Errorf("livenessProbe.FailureThreshold: want 5 (generous; ADR 0003), got %d", lp.FailureThreshold)
	}
}

// TestRenderDeployment_HasGhostunnelSidecar pins the ghostunnel sidecar
// container that terminates TLS for the rendered Service (ADR 0007 /
// slice 02). The sidecar reads the SVID trio tbot wrote into the shared
// `svid` emptyDir (read-only) and serves it as a TLS server cert on
// 0.0.0.0:<GhostunnelListenPort>, forwarding plaintext to the existing
// application-tunnel listener at 127.0.0.1:<cr.Spec.Port>.
//
// `--timed-reload=5m` is safe with tbot's default 20m renewal cadence —
// see the workload-identity-x509 assertion above.
func TestRenderDeployment_HasGhostunnelSidecar(t *testing.T) {
	cr := fixtureRemoteApp()
	cfg := fixtureConfig()

	dep := renderDeployment(cr, cfg)
	containers := dep.Spec.Template.Spec.Containers

	if len(containers) != 2 {
		t.Fatalf("pod must have exactly 2 containers (tbot + ghostunnel); got %d: %v", len(containers), containerNames(containers))
	}
	if containers[0].Name != "tbot" {
		t.Fatalf("first container must remain %q (unchanged); got %q", "tbot", containers[0].Name)
	}
	if containers[1].Name != "ghostunnel" {
		t.Fatalf("second container must be %q; got %q", "ghostunnel", containers[1].Name)
	}

	gt := containers[1]
	if gt.Image != cfg.GhostunnelImage {
		t.Errorf("ghostunnel image: want %q (from config), got %q", cfg.GhostunnelImage, gt.Image)
	}

	wantArgs := []string{
		"--cert=" + mountPathSVID + "/svid.pem",
		"--key=" + mountPathSVID + "/svid_key.pem",
		"--target=127.0.0.1:8080", // cr.Spec.Port via fixtureRemoteApp
		"--listen=0.0.0.0:8443",
		"--timed-reload=5m",
	}
	for _, want := range wantArgs {
		if !slices.Contains(gt.Args, want) {
			t.Errorf("ghostunnel args missing %q; got %v", want, gt.Args)
		}
	}

	// Shared SVID volume mounted read-only — tbot writes, ghostunnel reads.
	var svidMount *corev1.VolumeMount
	for i := range gt.VolumeMounts {
		if gt.VolumeMounts[i].Name == volumeNameSVID {
			svidMount = &gt.VolumeMounts[i]
			break
		}
	}
	if svidMount == nil {
		t.Fatalf("ghostunnel missing %q mount; got %v", volumeNameSVID, mountNames(gt.VolumeMounts))
	}
	if svidMount.MountPath != mountPathSVID {
		t.Errorf("ghostunnel %q mountPath: want %q, got %q", volumeNameSVID, mountPathSVID, svidMount.MountPath)
	}
	if !svidMount.ReadOnly {
		t.Errorf("ghostunnel %q mount must be read-only", volumeNameSVID)
	}

	// Hardened SecurityContext mirrors the tbot container.
	cc := gt.SecurityContext
	if cc == nil {
		t.Fatalf("ghostunnel SecurityContext must be set")
	}
	if cc.AllowPrivilegeEscalation == nil || *cc.AllowPrivilegeEscalation {
		t.Errorf("ghostunnel AllowPrivilegeEscalation: want false, got %v", cc.AllowPrivilegeEscalation)
	}
	if cc.ReadOnlyRootFilesystem == nil || !*cc.ReadOnlyRootFilesystem {
		t.Errorf("ghostunnel ReadOnlyRootFilesystem: want true, got %v", cc.ReadOnlyRootFilesystem)
	}
	if cc.RunAsNonRoot == nil || !*cc.RunAsNonRoot {
		t.Errorf("ghostunnel RunAsNonRoot: want true, got %v", cc.RunAsNonRoot)
	}
	if cc.Capabilities == nil || len(cc.Capabilities.Drop) != 1 || cc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("ghostunnel Capabilities.Drop: want [ALL], got %+v", cc.Capabilities)
	}
	if cc.SeccompProfile == nil || cc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("ghostunnel SeccompProfile: want RuntimeDefault, got %+v", cc.SeccompProfile)
	}
}

// TestRenderTrustBundleSecret_OwnedByRemoteApp pins slice 03 (ADR 0007):
// the operator pre-creates the `<cr.Name>-spiffe-bundle` Secret with the
// ownerRef back to the CR so tbot's writes (via the kubernetes_secret
// destination on the workload-identity-x509 service) hit an existing
// target. tbot owns only the Data on that Secret — initial Data must be
// nil/empty so SSA doesn't claim ownership of `svid_bundle.pem` and
// race tbot's renewer.
func TestRenderTrustBundleSecret_OwnedByRemoteApp(t *testing.T) {
	cr := fixtureRemoteApp()

	sec := renderTrustBundleSecret(cr, fixtureConfig())

	wantName := cr.Name + "-spiffe-bundle"
	if sec.Name != wantName {
		t.Errorf("Secret name: want %q, got %q", wantName, sec.Name)
	}
	if sec.Namespace != cr.Namespace {
		t.Errorf("Secret namespace: want %q, got %q", cr.Namespace, sec.Namespace)
	}
	if sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("Secret type: want Opaque, got %q", sec.Type)
	}
	if sec.Kind != "Secret" || sec.APIVersion != "v1" {
		t.Errorf("Secret TypeMeta: want v1/Secret, got %q/%q", sec.APIVersion, sec.Kind)
	}
	if got, want := sec.Labels[LabelRole], LabelRoleValue; got != want {
		t.Errorf("Secret labels[%s]: want %q, got %q", LabelRole, want, got)
	}
	if got, want := sec.Labels[LabelRemoteAppInstance], cr.Name; got != want {
		t.Errorf("Secret labels[%s]: want %q, got %q", LabelRemoteAppInstance, want, got)
	}

	// Initial Data must be empty — tbot writes svid_bundle.pem on its
	// first renewal. A placeholder key here would create an SSA
	// field-conflict the moment tbot tries to update.
	if len(sec.Data) != 0 {
		t.Errorf("Secret.Data must be nil/empty (tbot writes svid_bundle.pem); got %d keys: %v", len(sec.Data), keys2(sec.Data))
	}
	if len(sec.StringData) != 0 {
		t.Errorf("Secret.StringData must be nil/empty; got %d keys", len(sec.StringData))
	}

	// OwnerRef invariants — same shape as every other rendered object.
	if len(sec.OwnerReferences) != 1 {
		t.Fatalf("OwnerReferences: want 1, got %d", len(sec.OwnerReferences))
	}
	or := sec.OwnerReferences[0]
	if or.UID != cr.UID {
		t.Errorf("ownerRef UID: want %q, got %q", cr.UID, or.UID)
	}
	if or.Name != cr.Name {
		t.Errorf("ownerRef Name: want %q, got %q", cr.Name, or.Name)
	}
	if or.Kind != kindRemoteApp {
		t.Errorf("ownerRef Kind: want RemoteApp, got %q", or.Kind)
	}
	if or.Controller == nil || !*or.Controller {
		t.Errorf("ownerRef.Controller: want true, got %v", or.Controller)
	}
	if or.BlockOwnerDeletion == nil || !*or.BlockOwnerDeletion {
		t.Errorf("ownerRef.BlockOwnerDeletion: want true, got %v", or.BlockOwnerDeletion)
	}
}

// keys2 returns map keys for any-byte-valued map. The existing keys()
// helper above is typed on map[string]string for ConfigMap Data; Secret
// Data is map[string][]byte and needs its own narrow helper.
func keys2(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestRenderTrustBundleRole_NarrowSecretScope pins slice 03 (ADR 0007):
// the per-CR Role grants tbot's per-CR ServiceAccount the minimum verbs
// needed to update one named Secret in the namespace.
//
// Critical invariant: `resourceNames` restricts the grant to the single
// trust-bundle Secret — without it, a compromised tbot pod could
// read/write any Secret in the namespace.
func TestRenderTrustBundleRole_NarrowSecretScope(t *testing.T) {
	cr := fixtureRemoteApp()

	role := renderTrustBundleRole(cr, fixtureConfig())

	wantName := cr.Name + "-spiffe-bundle"
	if role.Name != wantName {
		t.Errorf("Role name: want %q, got %q", wantName, role.Name)
	}
	if role.Namespace != cr.Namespace {
		t.Errorf("Role namespace: want %q, got %q", cr.Namespace, role.Namespace)
	}
	if role.Kind != kindRole || role.APIVersion != "rbac.authorization.k8s.io/v1" {
		t.Errorf("Role TypeMeta: want rbac.authorization.k8s.io/v1/Role, got %q/%q", role.APIVersion, role.Kind)
	}

	if len(role.Rules) != 1 {
		t.Fatalf("Role rules: want exactly 1 rule, got %d: %+v", len(role.Rules), role.Rules)
	}
	rule := role.Rules[0]
	if !slices.Equal(rule.APIGroups, []string{""}) {
		t.Errorf("rule.APIGroups: want [\"\"], got %v", rule.APIGroups)
	}
	if !slices.Equal(rule.Resources, []string{"secrets"}) {
		t.Errorf("rule.Resources: want [secrets], got %v", rule.Resources)
	}
	if !slices.Equal(rule.ResourceNames, []string{wantName}) {
		t.Errorf("rule.ResourceNames: want [%q] (single-Secret scope), got %v", wantName, rule.ResourceNames)
	}
	wantVerbs := []string{"get", "update", "patch"}
	if !slices.Equal(rule.Verbs, wantVerbs) {
		t.Errorf("rule.Verbs: want %v, got %v", wantVerbs, rule.Verbs)
	}

	// OwnerRef: cascade-delete with the CR.
	if len(role.OwnerReferences) != 1 || role.OwnerReferences[0].UID != cr.UID {
		t.Errorf("Role ownerRef: want one ref to CR uid=%q, got %+v", cr.UID, role.OwnerReferences)
	}
}

// TestRenderTrustBundleRoleBinding_BindsRoleToPerCRSA pins slice 03:
// the per-CR RoleBinding ties the per-CR Role to the per-CR
// ServiceAccount (the same SA the tbot pod uses for the kubernetes-join
// JWT, ADR 0004). Same namespace as the CR.
func TestRenderTrustBundleRoleBinding_BindsRoleToPerCRSA(t *testing.T) {
	cr := fixtureRemoteApp()

	rb := renderTrustBundleRoleBinding(cr, fixtureConfig())

	wantName := cr.Name + "-spiffe-bundle"
	if rb.Name != wantName {
		t.Errorf("RoleBinding name: want %q, got %q", wantName, rb.Name)
	}
	if rb.Namespace != cr.Namespace {
		t.Errorf("RoleBinding namespace: want %q, got %q", cr.Namespace, rb.Namespace)
	}
	if rb.Kind != "RoleBinding" || rb.APIVersion != "rbac.authorization.k8s.io/v1" {
		t.Errorf("RoleBinding TypeMeta: want rbac.authorization.k8s.io/v1/RoleBinding, got %q/%q", rb.APIVersion, rb.Kind)
	}

	if rb.RoleRef.APIGroup != "rbac.authorization.k8s.io" || rb.RoleRef.Kind != kindRole || rb.RoleRef.Name != wantName {
		t.Errorf("RoleBinding.RoleRef: want rbac.authorization.k8s.io/Role/%q, got %+v", wantName, rb.RoleRef)
	}

	if len(rb.Subjects) != 1 {
		t.Fatalf("RoleBinding.Subjects: want 1, got %d: %+v", len(rb.Subjects), rb.Subjects)
	}
	s := rb.Subjects[0]
	if s.Kind != rbacv1.ServiceAccountKind {
		t.Errorf("Subject.Kind: want %q, got %q", rbacv1.ServiceAccountKind, s.Kind)
	}
	if s.Name != cr.Name {
		t.Errorf("Subject.Name: want %q (per-CR SA = cr.Name), got %q", cr.Name, s.Name)
	}
	if s.Namespace != cr.Namespace {
		t.Errorf("Subject.Namespace: want %q, got %q", cr.Namespace, s.Namespace)
	}

	if len(rb.OwnerReferences) != 1 || rb.OwnerReferences[0].UID != cr.UID {
		t.Errorf("RoleBinding ownerRef: want one ref to CR uid=%q, got %+v", cr.UID, rb.OwnerReferences)
	}
}

func containerNames(cs []corev1.Container) []string {
	out := make([]string, len(cs))
	for i := range cs {
		out[i] = cs[i].Name
	}
	return out
}

// helpers

func volumeByName(vs []corev1.Volume, name string) *corev1.Volume {
	for i := range vs {
		if vs[i].Name == name {
			return &vs[i]
		}
	}
	return nil
}

func hasVolume(vs []corev1.Volume, name string) bool { return volumeByName(vs, name) != nil }

func volumeNames(vs []corev1.Volume) []string {
	out := make([]string, len(vs))
	for i := range vs {
		out[i] = vs[i].Name
	}
	return out
}

func hasMount(ms []corev1.VolumeMount, name string) bool {
	for _, m := range ms {
		if m.Name == name {
			return true
		}
	}
	return false
}

func mountNames(ms []corev1.VolumeMount) []string {
	out := make([]string, len(ms))
	for i := range ms {
		out[i] = ms[i].Name
	}
	return out
}

func TestSetOwnerRef_StampsControllerRefBackToCR(t *testing.T) {
	cr := fixtureRemoteApp()
	cm := renderConfigMap(cr, fixtureConfig())

	if err := setOwnerRef(cr, cm); err != nil {
		t.Fatalf("setOwnerRef: %v", err)
	}

	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences: want 1, got %d", len(cm.OwnerReferences))
	}
	or := cm.OwnerReferences[0]
	if or.UID != cr.UID {
		t.Errorf("ownerRef UID: want %q, got %q", cr.UID, or.UID)
	}
	if or.Name != cr.Name {
		t.Errorf("ownerRef Name: want %q, got %q", cr.Name, or.Name)
	}
	if or.Kind != kindRemoteApp {
		t.Errorf("ownerRef Kind: want RemoteApp, got %q", or.Kind)
	}
	if or.Controller == nil || !*or.Controller {
		t.Errorf("ownerRef.Controller: want true, got %v", or.Controller)
	}
	if or.BlockOwnerDeletion == nil || !*or.BlockOwnerDeletion {
		t.Errorf("ownerRef.BlockOwnerDeletion: want true, got %v", or.BlockOwnerDeletion)
	}
}

func TestRenderService_ClusterIPWithCanonicalSelector(t *testing.T) {
	cr := fixtureRemoteApp()

	svc := renderService(cr, fixtureConfig())

	if svc.Name != cr.Name {
		t.Fatalf("Service name: want %q, got %q", cr.Name, svc.Name)
	}
	if svc.Namespace != cr.Namespace {
		t.Fatalf("Service namespace: want %q, got %q", cr.Namespace, svc.Namespace)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type: want ClusterIP, got %q", svc.Spec.Type)
	}
	if got, want := svc.Spec.Selector[LabelRole], LabelRoleValue; got != want {
		t.Errorf("Service selector[%s]: want %q, got %q", LabelRole, want, got)
	}
	if got, want := svc.Spec.Selector[LabelRemoteAppInstance], cr.Name; got != want {
		t.Errorf("Service selector[%s]: want %q, got %q", LabelRemoteAppInstance, want, got)
	}
	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("Service ports: want 2 (tbot + tls), got %d", len(svc.Spec.Ports))
	}
	p := svc.Spec.Ports[0]
	if p.Port != cr.Spec.Port {
		t.Errorf("Service port: want %d, got %d", cr.Spec.Port, p.Port)
	}
	if p.TargetPort.IntValue() != int(cr.Spec.Port) {
		t.Errorf("Service targetPort: want %d, got %d", cr.Spec.Port, p.TargetPort.IntValue())
	}
}

// TestRenderService_DualPortPlaintextAndTLS pins the slice-02 service
// shape (ADR 0007): the existing plaintext `tbot/<spec.port>` port is
// preserved (NetworkPolicy / selector backward-compat) and a new `tls`
// port is appended pointing at the ghostunnel sidecar on
// `cfg.GhostunnelListenPort` (default 8443).
func TestRenderService_DualPortPlaintextAndTLS(t *testing.T) {
	cr := fixtureRemoteApp()
	cfg := fixtureConfig()

	svc := renderService(cr, cfg)

	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("Service ports: want 2, got %d", len(svc.Spec.Ports))
	}

	plain := svc.Spec.Ports[0]
	if plain.Name != "tbot" {
		t.Errorf("first port name: want %q (unchanged for selector backward-compat), got %q", "tbot", plain.Name)
	}
	if plain.Port != cr.Spec.Port {
		t.Errorf("first port .Port: want %d (cr.Spec.Port), got %d", cr.Spec.Port, plain.Port)
	}
	if plain.TargetPort.IntValue() != int(cr.Spec.Port) {
		t.Errorf("first port .TargetPort: want %d, got %d", cr.Spec.Port, plain.TargetPort.IntValue())
	}

	tls := svc.Spec.Ports[1]
	if tls.Name != "tls" {
		t.Errorf("second port name: want %q, got %q", "tls", tls.Name)
	}
	if tls.Port != cfg.GhostunnelListenPort {
		t.Errorf("second port .Port: want %d (cfg.GhostunnelListenPort), got %d", cfg.GhostunnelListenPort, tls.Port)
	}
	if tls.TargetPort.IntValue() != int(cfg.GhostunnelListenPort) {
		t.Errorf("second port .TargetPort: want %d, got %d", cfg.GhostunnelListenPort, tls.TargetPort.IntValue())
	}
	if tls.Protocol != corev1.ProtocolTCP {
		t.Errorf("second port .Protocol: want TCP, got %q", tls.Protocol)
	}
}
