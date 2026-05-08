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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// Aliases keep the readiness-probe assertion legible without importing
// intstr at every call site.
const (
	intstrTypeInt    = intstr.Int
	intstrTypeString = intstr.String

	// kindRemoteApp is the OwnerReference Kind every owned object carries.
	// Hoisted out of individual test bodies so goconst doesn't trip when
	// new owner-ref assertions land — the kind itself is a single source
	// of truth.
	kindRemoteApp = "RemoteApp"
)

// renderFixtureOpts is the renderer-test-specific override of
// newRemoteApp's defaults: namespace="demo", name="tracer", appName="myapp",
// tokenRef.Name="myapp-token". These match the strings the assertions in
// this file pin verbatim. The shared newRemoteApp() defaults match the
// envtest fixtures (name="demo"), so the renderer tests pass these opts
// to keep their assertions stable.
var renderFixtureOpts = []fixtureOpt{
	withName("demo", "tracer"),
	withUID("uid-tracer"),
	withAppName("myapp"),
	withTokenRefName("myapp-token"),
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
		// ADR 0006: kubernetes join. tbot presents the projected SA token
		// mounted at mountPathTbotSAToken; Central verifies it under
		// `kubernetes.type: static_jwks`.
		"join_method: kubernetes",
		// onboarding.token references the per-RemoteApp
		// `TeleportProvisionToken` on Central. Convention locked here:
		// `tunnelport-${cr.Name}` (ADR 0006). Slice 06 cutover and the
		// runbook in slice 05 reference this exact name shape.
		"token: tunnelport-tracer",
		// onboarding.kubernetes.token_path points tbot at the projected
		// SA-token file slice 01 mounts. The default upstream path is
		// `/var/run/secrets/kubernetes.io/serviceaccount/token`, but we
		// mount at a project-specific path so the audience can be
		// pinned to `tunnelport.giantswarm.io` and a stray default-
		// audience token cannot satisfy the join.
		"token_path: /var/run/secrets/tunnelport.giantswarm.io/serviceaccount/token",
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
	// earlier drafts of the renderer invented (or that ADR 0006
	// supersedes). If they reappear, tbot will reject the config at
	// startup or rejoin via the wrong method.
	bannedSubstrings := []string{
		"token_secret_ref",    // not a real tbot field; only `token:` exists
		"listener:",           // wrong spelling — upstream tag is `listen`
		"auth_server:",        // we use proxy_server only; auth_server here was a copy-paste
		"bound_keypair",       // ADR 0005 join method, superseded by ADR 0006
		"registration_secret", // belonged to bound_keypair onboarding
		"join_method: bound_keypair",
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

func TestRenderDeployment_DefaultsAndStrategy(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig(), "")

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

	dep := renderDeployment(cr, fixtureConfig(), "")

	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 3 {
		t.Errorf("Deployment replicas: want 3, got %v", dep.Spec.Replicas)
	}
}

func TestRenderDeployment_PodTemplateMountsConfigMapAndEmptyDir(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig(), "")

	pod := dep.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("expected 1 container in pod, got %d", len(pod.Containers))
	}
	c := pod.Containers[0]

	// ConfigMap volume mounted (read-only, name-only reference).
	if !hasVolume(pod.Volumes, "tbot-config") {
		t.Errorf("missing volume tbot-config; got: %v", volumeNames(pod.Volumes))
	}
	if !hasMount(c.VolumeMounts, "tbot-config") {
		t.Errorf("container missing volumeMount tbot-config; got: %v", mountNames(c.VolumeMounts))
	}

	// ADR 0006: the legacy `tbot-token` Secret volume from ADR 0005's
	// bound_keypair onboarding is gone. tbot now joins via the
	// `kubernetes` method using the projected SA token mounted by
	// slice 01 (asserted separately in
	// TestRenderDeployment_ProjectedSATokenVolumeMounted).
	if hasVolume(pod.Volumes, "tbot-token") {
		t.Errorf("legacy tbot-token Secret volume must be absent (ADR 0006); got volumes: %v", volumeNames(pod.Volumes))
	}
	if hasMount(c.VolumeMounts, "tbot-token") {
		t.Errorf("legacy tbot-token mount must be absent (ADR 0006); got mounts: %v", mountNames(c.VolumeMounts))
	}
	// Reconciler must NOT inject any token via env / envFrom — kubernetes
	// join reads the SA JWT from a projected file, not from a Secret.
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

func TestRenderDeployment_UsesOperatorConfigImageAndResources(t *testing.T) {
	cr := fixtureRemoteApp()
	cfg := fixtureConfig()

	dep := renderDeployment(cr, cfg, "")

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

	dep := renderDeployment(cr, fixtureConfig(), "")

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

	dep := renderDeployment(cr, fixtureConfig(), "")

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

	dep := renderDeployment(cr, fixtureConfig(), "")

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

	dep := renderDeployment(cr, fixtureConfig(), "")
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

	dep := renderDeployment(cr, fixtureConfig(), "")

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

// TestRenderServiceAccount_NamedAfterCRWithCanonicalLabels pins the
// per-RemoteApp ServiceAccount contract from ADR 0006 slice 1: the SA's
// name matches the CR's name (the same convention Service / Deployment /
// ConfigMap already follow), and it carries the canonical role labels so
// platform teams have a stable selector. The SA exists so a future slice
// can flip tbot's join_method to `kubernetes`; this slice only renders it.
func TestRenderServiceAccount_NamedAfterCRWithCanonicalLabels(t *testing.T) {
	cr := fixtureRemoteApp()

	sa := renderServiceAccount(cr, fixtureConfig())

	if sa.Name != cr.Name {
		t.Errorf("ServiceAccount name: want %q (matches Deployment/Service/ConfigMap convention), got %q", cr.Name, sa.Name)
	}
	if sa.Namespace != cr.Namespace {
		t.Errorf("ServiceAccount namespace: want %q, got %q", cr.Namespace, sa.Namespace)
	}
	if got, want := sa.Labels[LabelRole], LabelRoleValue; got != want {
		t.Errorf("ServiceAccount label[%s]: want %q, got %q", LabelRole, want, got)
	}
	if got, want := sa.Labels[LabelRemoteAppInstance], cr.Name; got != want {
		t.Errorf("ServiceAccount label[%s]: want %q, got %q", LabelRemoteAppInstance, want, got)
	}
	// TypeMeta is required for Server-Side Apply through the same code
	// path that already applies ConfigMap / Deployment / Service.
	if sa.APIVersion != "v1" || sa.Kind != "ServiceAccount" {
		t.Errorf("ServiceAccount TypeMeta: want apiVersion=v1 kind=ServiceAccount, got %q/%q", sa.APIVersion, sa.Kind)
	}
}

// TestRenderServiceAccount_OwnerRefViaSetOwnerRef confirms the SA can carry
// a controller OwnerReference back to the RemoteApp via the same helper
// the other rendered objects use, so Kubernetes garbage collection
// cascade-deletes the SA when the CR is deleted.
func TestRenderServiceAccount_OwnerRefViaSetOwnerRef(t *testing.T) {
	cr := fixtureRemoteApp()
	sa := renderServiceAccount(cr, fixtureConfig())

	if err := setOwnerRef(cr, sa); err != nil {
		t.Fatalf("setOwnerRef: %v", err)
	}

	if len(sa.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences: want 1, got %d", len(sa.OwnerReferences))
	}
	or := sa.OwnerReferences[0]
	if or.UID != cr.UID {
		t.Errorf("ownerRef UID: want %q, got %q", cr.UID, or.UID)
	}
	if or.Kind != kindRemoteApp {
		t.Errorf("ownerRef Kind: want %s, got %q", kindRemoteApp, or.Kind)
	}
	if or.Controller == nil || !*or.Controller {
		t.Errorf("ownerRef.Controller: want true, got %v", or.Controller)
	}
}

// TestRenderDeployment_PodRunsAsRenderedServiceAccount pins the wiring the
// pod template needs so that, when slice 02 flips tbot's join method to
// `kubernetes`, the projected SA token tbot presents is signed for the
// per-RemoteApp identity Central will be configured to accept. The pod
// must (a) declare spec.serviceAccountName = the rendered SA's name
// (= cr.Name) and (b) keep automountServiceAccountToken: false so the
// default mount doesn't leak the token at /var/run/secrets/... — the
// token is delivered via an explicit projected volume so its audience can
// be pinned later.
func TestRenderDeployment_PodRunsAsRenderedServiceAccount(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig(), "")

	pod := dep.Spec.Template.Spec
	if pod.ServiceAccountName != cr.Name {
		t.Errorf("Pod.spec.serviceAccountName: want %q (== rendered SA name), got %q", cr.Name, pod.ServiceAccountName)
	}
	// Default-mount stays off — the projected mount below carries the
	// token at a known path tbot can read explicitly.
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("Pod.spec.automountServiceAccountToken: want false (token delivered via projected volume), got %v", pod.AutomountServiceAccountToken)
	}
}

// TestRenderDeployment_ProjectedSATokenVolumeMounted pins the projected
// volume strategy: the SA token reaches the tbot container's filesystem
// via a projected volume with an explicit audience, mounted at a stable
// path. Slice 02 will read that token from this path. The audience
// `tunnelport.giantswarm.io` matches the value Central's
// `TeleportProvisionToken` will require under `kubernetes.type:
// static_jwks` (ADR 0006).
func TestRenderDeployment_ProjectedSATokenVolumeMounted(t *testing.T) {
	cr := fixtureRemoteApp()

	dep := renderDeployment(cr, fixtureConfig(), "")
	pod := dep.Spec.Template.Spec

	vol := volumeByName(pod.Volumes, volumeNameTbotSAToken)
	if vol == nil {
		t.Fatalf("missing %q volume; got: %v", volumeNameTbotSAToken, volumeNames(pod.Volumes))
	}
	if vol.Projected == nil {
		t.Fatalf("%q must be a projected volume; got %+v", volumeNameTbotSAToken, vol)
	}
	var saSrc *corev1.ServiceAccountTokenProjection
	for i := range vol.Projected.Sources {
		if s := vol.Projected.Sources[i].ServiceAccountToken; s != nil {
			saSrc = s
			break
		}
	}
	if saSrc == nil {
		t.Fatalf("%q has no ServiceAccountToken projection; got %+v", volumeNameTbotSAToken, vol.Projected.Sources)
	}
	if saSrc.Audience != saTokenAudience {
		t.Errorf("ServiceAccountToken audience: want %q, got %q", saTokenAudience, saSrc.Audience)
	}
	if saSrc.ExpirationSeconds == nil || *saSrc.ExpirationSeconds < 600 {
		t.Errorf("ServiceAccountToken expirationSeconds: want >=600, got %v", saSrc.ExpirationSeconds)
	}
	if saSrc.Path == "" {
		t.Errorf("ServiceAccountToken path must be set so the projected file has a deterministic name")
	}

	c := pod.Containers[0]
	var saMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == volumeNameTbotSAToken {
			saMount = &c.VolumeMounts[i]
			break
		}
	}
	if saMount == nil {
		t.Fatalf("container missing %q mount; got: %v", volumeNameTbotSAToken, mountNames(c.VolumeMounts))
	}
	if saMount.MountPath != mountPathTbotSAToken {
		t.Errorf("SA token mountPath: want %q, got %q", mountPathTbotSAToken, saMount.MountPath)
	}
	if !saMount.ReadOnly {
		t.Errorf("SA token mount must be readOnly")
	}
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
		t.Errorf("ownerRef Kind: want %s, got %q", kindRemoteApp, or.Kind)
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
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("Service ports: want 1, got %d", len(svc.Spec.Ports))
	}
	p := svc.Spec.Ports[0]
	if p.Port != cr.Spec.Port {
		t.Errorf("Service port: want %d, got %d", cr.Spec.Port, p.Port)
	}
	if p.TargetPort.IntValue() != int(cr.Spec.Port) {
		t.Errorf("Service targetPort: want %d, got %d", cr.Spec.Port, p.TargetPort.IntValue())
	}
}
