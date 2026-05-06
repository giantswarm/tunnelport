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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// Aliases keep the readiness-probe assertion legible without importing
// intstr at every call site.
const (
	intstrTypeInt    = intstr.Int
	intstrTypeString = intstr.String
)

// fixtureRemoteApp returns a representative RemoteApp the renderers can be
// driven from. UID is set so generated OwnerReferences match what the API
// server would persist.
func fixtureRemoteApp() *accessv1alpha1.RemoteApp {
	return &accessv1alpha1.RemoteApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tracer",
			Namespace: "demo",
			UID:       "uid-tracer",
		},
		Spec: accessv1alpha1.RemoteAppSpec{
			AppName:   "myapp",
			Port:      8080,
			ProxyAddr: "teleport.example.com:443",
			TokenRef: accessv1alpha1.TokenRef{
				Name: "myapp-token",
				Key:  "token",
			},
		},
	}
}

func fixtureConfig() Config {
	return Config{
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
		"tcp://0.0.0.0:8080",
		"type: application-tunnel",
		"join_method: token",
		"name: myapp-token",
	}
	for _, want := range wants {
		if !strings.Contains(cfg, want) {
			t.Errorf("tbot.yaml missing %q\n---\n%s\n---", want, cfg)
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
		if c.Ports[i].Name == "diag" {
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
	if tp.Type == intstrTypeString && tp.StrVal != "diag" {
		t.Errorf("readinessProbe port (string): want %q, got %q", "diag", tp.StrVal)
	}
	if tp.Type == intstrTypeInt && tp.IntVal != 3001 {
		t.Errorf("readinessProbe port (int): want 3001, got %d", tp.IntVal)
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
