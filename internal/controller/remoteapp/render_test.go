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
		// ADR 0005: bound_keypair join with relaxed recovery (recovery
		// mode lives on the Central-side token resource, not in tbot.yaml).
		"join_method: bound_keypair",
		// onboarding.token under bound_keypair is the *name* of the
		// Teleport token resource on Central, not a literal secret or a
		// file path. Convention: matches cr.Spec.TokenRef.Name.
		"token: myapp-token",
		// onboarding.bound_keypair.registration_secret_path points at the
		// mounted Secret key. tbot reads the registration secret from
		// disk on first join — the operator never reads Secret.Data.
		"registration_secret_path: /etc/tbot-token/token",
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

func TestRenderDeployment_PodTemplateMountsConfigMapTokenAndEmptyDir(t *testing.T) {
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

	// Token Secret volume mounted by name only — no envFrom or readers.
	if !hasVolume(pod.Volumes, "tbot-token") {
		t.Errorf("missing volume tbot-token; got: %v", volumeNames(pod.Volumes))
	}
	tokenVol := volumeByName(pod.Volumes, "tbot-token")
	if tokenVol == nil || tokenVol.Secret == nil {
		t.Fatalf("tbot-token volume must be a Secret volume; got %+v", tokenVol)
	}
	if tokenVol.Secret.SecretName != cr.Spec.TokenRef.Name {
		t.Errorf("tbot-token volume secretName: want %q, got %q",
			cr.Spec.TokenRef.Name, tokenVol.Secret.SecretName)
	}
	// Reconciler must NOT inject the token via env / envFrom — that would
	// require reading the Secret's contents (forbidden).
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil &&
			e.ValueFrom.SecretKeyRef.Name == cr.Spec.TokenRef.Name {
			t.Errorf("container env %q references token Secret directly; pod must mount the Secret as a volume only", e.Name)
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
	if or.Kind != "RemoteApp" {
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
