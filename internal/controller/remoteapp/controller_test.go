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
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

const (
	pollInterval = 50 * time.Millisecond
	pollTimeout  = 5 * time.Second
)

// uniqueNS creates a fresh namespace per test so concurrent runs and
// previous-test residue don't cross-contaminate.
func uniqueNS(t *testing.T, ctx context.Context) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "remoteapp-test-"},
	}
	if err := testClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = testClient.Delete(context.Background(), ns)
	})
	return ns.Name
}

// makeRemoteApp builds a minimal valid RemoteApp in ns and Creates it.
// Built on top of the shared newRemoteApp fixture (see fixtures_test.go);
// tests that need a non-default spec compose fixtureOpts at the call site
// or mutate the returned object before re-Update. Servers strip the
// fixture's default UID on Create — the API server assigns a fresh one.
func makeRemoteApp(ctx context.Context, t *testing.T, ns, name string, opts ...fixtureOpt) *accessv1alpha1.RemoteApp {
	t.Helper()
	cr := newRemoteApp(append([]fixtureOpt{withName(ns, name)}, opts...)...)
	cr.UID = "" // let the API server assign one on Create.
	if err := testClient.Create(ctx, cr); err != nil {
		t.Fatalf("create RemoteApp: %v", err)
	}
	return cr
}

func eventuallyGet(t *testing.T, ctx context.Context, key client.ObjectKey, obj client.Object) {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for {
		err := testClient.Get(ctx, key, obj)
		if err == nil {
			return
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("get %s/%s: %v", key.Namespace, key.Name, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s %s/%s", obj.GetObjectKind().GroupVersionKind().Kind, key.Namespace, key.Name)
		}
		time.Sleep(pollInterval)
	}
}

// eventually runs cond at pollInterval until it returns true or the
// deadline passes. The last error from cond surfaces in the failure.
func eventually(t *testing.T, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	var lastErr error
	for {
		ok, err := cond()
		if ok {
			return
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr != nil {
				t.Fatalf("eventually: condition not met: %v", lastErr)
			}
			t.Fatalf("eventually: condition not met within %s", pollTimeout)
		}
		time.Sleep(pollInterval)
	}
}

func TestReconciler_AppliesRemoteAppRendersAllThreeOwnedObjects(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "all-three")

	cm := &corev1.ConfigMap{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, cm)

	dep := &appsv1.Deployment{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, dep)

	svc := &corev1.Service{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, svc)

	// Each owned object carries an OwnerReference back to the CR with
	// Controller=true and BlockOwnerDeletion=true so kubectl-driven
	// cascade deletes wait for the children to GC.
	for _, obj := range []client.Object{cm, dep, svc} {
		ors := obj.GetOwnerReferences()
		if len(ors) != 1 {
			t.Errorf("%T %s: ownerReferences want 1, got %d", obj, obj.GetName(), len(ors))
			continue
		}
		or := ors[0]
		if or.UID != cr.UID {
			t.Errorf("%T %s: ownerRef UID want %q, got %q", obj, obj.GetName(), cr.UID, or.UID)
		}
		if or.Kind != "RemoteApp" {
			t.Errorf("%T %s: ownerRef Kind want RemoteApp, got %q", obj, obj.GetName(), or.Kind)
		}
		if or.Controller == nil || !*or.Controller {
			t.Errorf("%T %s: ownerRef.Controller want true, got %v", obj, obj.GetName(), or.Controller)
		}
		if or.BlockOwnerDeletion == nil || !*or.BlockOwnerDeletion {
			t.Errorf("%T %s: ownerRef.BlockOwnerDeletion want true, got %v", obj, obj.GetName(), or.BlockOwnerDeletion)
		}
	}

	// ConfigMap content reflects the spec.
	body := cm.Data["tbot.yaml"]
	for _, want := range []string{"proxy_server: teleport.example.com:443", "app_name: demo-app", "tcp://0.0.0.0:8080"} {
		if !strings.Contains(body, want) {
			t.Errorf("tbot.yaml missing %q\n---\n%s", want, body)
		}
	}

	// Service spec reflects spec.port.
	if got := svc.Spec.Ports[0].Port; got != cr.Spec.Port {
		t.Errorf("Service port: want %d, got %d", cr.Spec.Port, got)
	}

	// Deployment uses operator config image, not anything from the CR.
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != testConfig().TbotImage {
		t.Errorf("Deployment image: want %q (from operator config), got %q", testConfig().TbotImage, got)
	}
}

// samePodTemplate reports whether two Deployments share the same pod
// template by every field, not just the rolling-fingerprint subset the
// Deployment controller hashes on. The previous bespoke comparison
// inspected only image, args, ports, volume name + ConfigMap/Secret name
// — silently ignoring SecurityContext, Resources, Env, Probes, etc., so
// regressions on those fields could not be caught here. equality.Semantic
// is the same comparator the API server uses for resource.Quantity / time
// equality, so two CPU quantities written as "10m" and "0.01" still
// compare equal — that's the only place a literal byte-equal check would
// have been wrong.
func samePodTemplate(a, b appsv1.Deployment) bool {
	return equalPodTemplate(a.Spec.Template, b.Spec.Template)
}

func equalPodTemplate(a, b corev1.PodTemplateSpec) bool {
	return equality.Semantic.DeepEqual(a, b)
}

func TestReconciler_PortChangeUpdatesAllThreeAndRollsDeployment(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "port-change")

	// Wait for initial render.
	depBefore := &appsv1.Deployment{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, depBefore)
	svcBefore := &corev1.Service{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, svcBefore)
	cmBefore := &corev1.ConfigMap{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, cmBefore)

	// Mutate spec.port to a new value.
	const newPort = int32(9090)
	updatePort := func() error {
		got := &accessv1alpha1.RemoteApp{}
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(cr), got); err != nil {
			return err
		}
		got.Spec.Port = newPort
		return testClient.Update(ctx, got)
	}
	if err := updatePort(); err != nil {
		t.Fatalf("update port: %v", err)
	}

	// Service port should reflect new spec.port.
	eventually(t, func() (bool, error) {
		svc := &corev1.Service{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, svc); err != nil {
			return false, err
		}
		if len(svc.Spec.Ports) != 1 {
			return false, fmt.Errorf("expected 1 port, got %d", len(svc.Spec.Ports))
		}
		if svc.Spec.Ports[0].Port != newPort {
			return false, fmt.Errorf("Service port still %d", svc.Spec.Ports[0].Port)
		}
		return true, nil
	})

	// ConfigMap should mention the new listener port.
	eventually(t, func() (bool, error) {
		cm := &corev1.ConfigMap{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, cm); err != nil {
			return false, err
		}
		want := fmt.Sprintf("tcp://0.0.0.0:%d", newPort)
		if !strings.Contains(cm.Data["tbot.yaml"], want) {
			return false, fmt.Errorf("ConfigMap missing %q", want)
		}
		return true, nil
	})

	// Deployment containerPort and arguments reflect the new port —
	// pod template differs from the pre-update template, so the
	// Deployment will roll.
	eventually(t, func() (bool, error) {
		dep := &appsv1.Deployment{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, dep); err != nil {
			return false, err
		}
		c := dep.Spec.Template.Spec.Containers[0]
		if len(c.Ports) == 0 || c.Ports[0].ContainerPort != newPort {
			return false, fmt.Errorf("Deployment container port not yet %d", newPort)
		}
		if samePodTemplate(*depBefore, *dep) {
			return false, fmt.Errorf("pod template did not change after spec.port update — Deployment would not roll")
		}
		return true, nil
	})
}

func TestReconciler_ReplicasChangeScalesWithoutRolling(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "replicas-change")

	depBefore := &appsv1.Deployment{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, depBefore)
	if depBefore.Spec.Replicas == nil || *depBefore.Spec.Replicas != 1 {
		t.Fatalf("initial replicas: want 1, got %v", depBefore.Spec.Replicas)
	}

	// Bump replicas to 3.
	want := int32(3)
	got := &accessv1alpha1.RemoteApp{}
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(cr), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Spec.Replicas = &want
	if err := testClient.Update(ctx, got); err != nil {
		t.Fatalf("update replicas: %v", err)
	}

	eventually(t, func() (bool, error) {
		dep := &appsv1.Deployment{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, dep); err != nil {
			return false, err
		}
		if dep.Spec.Replicas == nil || *dep.Spec.Replicas != want {
			return false, fmt.Errorf("replicas not yet %d", want)
		}
		// Pod template should be unchanged — replicas changes don't roll.
		if !samePodTemplate(*depBefore, *dep) {
			return false, fmt.Errorf("pod template changed on replicas update; deployment would roll unnecessarily")
		}
		return true, nil
	})
}

func TestReconciler_AppNameAndProxyAddrChangeUpdateConfigMapAndRollDeployment(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "appname-proxy-change")

	// Initial render.
	depBefore := &appsv1.Deployment{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, depBefore)
	cmBefore := &corev1.ConfigMap{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, cmBefore)
	bodyBefore := cmBefore.Data["tbot.yaml"]

	// Mutate appName + proxyAddr.
	got := &accessv1alpha1.RemoteApp{}
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(cr), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Spec.AppName = "renamed-app"
	got.Spec.ProxyAddr = "teleport.new.example.com:443"
	if err := testClient.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}

	// ConfigMap reflects new app name + proxy addr.
	eventually(t, func() (bool, error) {
		cm := &corev1.ConfigMap{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, cm); err != nil {
			return false, err
		}
		body := cm.Data["tbot.yaml"]
		if body == bodyBefore {
			return false, fmt.Errorf("ConfigMap data unchanged")
		}
		if !strings.Contains(body, "app_name: renamed-app") {
			return false, fmt.Errorf("ConfigMap missing new app_name")
		}
		if !strings.Contains(body, "proxy_server: teleport.new.example.com:443") {
			return false, fmt.Errorf("ConfigMap missing new proxy_server")
		}
		return true, nil
	})

	// Deployment must roll. The pod template references the ConfigMap by
	// name only, so the args themselves don't change — but the pod
	// template's config-hash annotation does, which triggers a new
	// pod-template-hash and a rolling update.
	eventually(t, func() (bool, error) {
		dep := &appsv1.Deployment{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, dep); err != nil {
			return false, err
		}
		hashBefore := depBefore.Spec.Template.Annotations[AnnotationConfigHash]
		hashAfter := dep.Spec.Template.Annotations[AnnotationConfigHash]
		if hashBefore == "" {
			return false, fmt.Errorf("config-hash annotation missing on initial Deployment")
		}
		if hashAfter == hashBefore {
			return false, fmt.Errorf("config-hash unchanged after appName/proxyAddr update; Deployment would not roll")
		}
		return true, nil
	})
}

// TestReconciler_RendersOwnedServiceAccountAndPodUsesIt asserts the
// per-RemoteApp ServiceAccount lifecycle through the reconciler — the
// renderer-level test pins the static shape, this one pins that the
// reconcile loop actually creates the SA, owns it, and wires the
// Deployment's serviceAccountName at the same time. Reusing-across-
// reconciles is covered by re-fetching the SA after a benign spec edit
// and confirming the same UID survives.
func TestReconciler_RendersOwnedServiceAccountAndPodUsesIt(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "with-sa")

	sa := &corev1.ServiceAccount{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, sa)

	if sa.Name != cr.Name {
		t.Errorf("ServiceAccount name: want %q (== cr.Name), got %q", cr.Name, sa.Name)
	}
	if got, want := sa.Labels[LabelRole], LabelRoleValue; got != want {
		t.Errorf("ServiceAccount label[%s]: want %q, got %q", LabelRole, want, got)
	}

	// OwnerReference back to the CR with controller + blockOwnerDeletion,
	// matching the contract every other rendered object honours.
	ors := sa.GetOwnerReferences()
	if len(ors) != 1 {
		t.Fatalf("ServiceAccount ownerReferences: want 1, got %d", len(ors))
	}
	or := ors[0]
	if or.UID != cr.UID || or.Kind != "RemoteApp" ||
		or.Controller == nil || !*or.Controller ||
		or.BlockOwnerDeletion == nil || !*or.BlockOwnerDeletion {
		t.Errorf("ServiceAccount owner ref invariant violated: %+v", or)
	}

	// Deployment runs as the SA we just rendered.
	dep := &appsv1.Deployment{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, dep)
	if got := dep.Spec.Template.Spec.ServiceAccountName; got != cr.Name {
		t.Errorf("Deployment.spec.template.spec.serviceAccountName: want %q (== rendered SA), got %q", cr.Name, got)
	}

	// Reconcile-stable: bumping spec.replicas re-runs the loop without
	// re-creating the SA. UID must survive — proves applyOwned reuses
	// the existing SA rather than racing a delete/create cycle.
	saUIDBefore := sa.UID
	got := &accessv1alpha1.RemoteApp{}
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(cr), got); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	r := int32(2)
	got.Spec.Replicas = &r
	if err := testClient.Update(ctx, got); err != nil {
		t.Fatalf("update replicas: %v", err)
	}
	eventually(t, func() (bool, error) {
		current := &corev1.ServiceAccount{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, current); err != nil {
			return false, err
		}
		if current.UID != saUIDBefore {
			return false, fmt.Errorf("ServiceAccount UID changed across reconciles: before=%s after=%s", saUIDBefore, current.UID)
		}
		// Simultaneously confirm the Deployment still references it.
		d := &appsv1.Deployment{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, d); err != nil {
			return false, err
		}
		if d.Spec.Template.Spec.ServiceAccountName != cr.Name {
			return false, fmt.Errorf("Deployment serviceAccountName drifted: %q", d.Spec.Template.Spec.ServiceAccountName)
		}
		return true, nil
	})
}

func TestReconciler_OwnerReferencesEnableCascadeDelete(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "cascade")

	// Wait for all three to exist.
	cm := &corev1.ConfigMap{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, cm)
	dep := &appsv1.Deployment{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, dep)
	svc := &corev1.Service{}
	eventuallyGet(t, ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, svc)

	// envtest does NOT run the kube-controller-manager's GC controller, so
	// we cannot observe cascade deletion happen. Instead, we verify the
	// invariant the API server's GC depends on: the OwnerReferences on
	// each child carry Controller=true and BlockOwnerDeletion=true and
	// point at the live RemoteApp's UID. Real-cluster cascade behavior is
	// covered by e2e (slice 6 / future).
	for _, obj := range []client.Object{cm, dep, svc} {
		ors := obj.GetOwnerReferences()
		if len(ors) != 1 {
			t.Fatalf("%T: ownerRefs len = %d, want 1", obj, len(ors))
		}
		or := ors[0]
		if or.UID != cr.UID || or.Controller == nil || !*or.Controller ||
			or.BlockOwnerDeletion == nil || !*or.BlockOwnerDeletion {
			t.Errorf("%T owner ref invariant violated: %+v", obj, or)
		}
	}
}
