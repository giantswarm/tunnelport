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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// TestReconciler_TokenRefNameMutationRetargetsDeploymentVolumeAndAnnotation
// is the envtest companion to the value-rotation test in
// secret_watch_test.go. It exercises the orthogonal axis: the platform
// engineer points spec.tokenRef.Name at a different Secret object (e.g.
// retiring a leaked token by switching to a fresh one), rather than
// updating the existing Secret's bytes.
//
// Sequence:
//
//  1. Create token Secret tok-a, then a RemoteApp referencing it.
//  2. Wait for the Deployment volume to reference tok-a and the annotation
//     to track tok-a's resourceVersion.
//  3. Create Secret tok-b with the same key.
//  4. Update cr.spec.tokenRef.Name to tok-b. Re-reconcile happens
//     automatically via the CR watch.
//  5. Assert: the Deployment's tbot-token volume secretName is now tok-b
//     and the token-version annotation matches tok-b's resourceVersion.
//
// Step 6 (separate, in-process check): re-running the field-indexer on a
// CR that now points at tok-b means a Secret event on the orphaned tok-a
// must NOT fan out to that CR. That's the watch-scoping invariant the
// indexer guarantees, asserted here against a fake client wired up with
// the same indexer the production reconciler registers.
func TestReconciler_TokenRefNameMutationRetargetsDeploymentVolumeAndAnnotation(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	// (1) Token Secret tok-a + a RemoteApp referencing it.
	tokA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tok-a", Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte("v1")},
	}
	if err := testClient.Create(ctx, tokA); err != nil {
		t.Fatalf("create tok-a: %v", err)
	}
	cr := makeRemoteApp(ctx, t, ns, "rename-target",
		withTokenRefName(tokA.Name),
	)

	// (2) Initial Deployment must reference tok-a in the tbot-token
	// volume, and the token-version annotation must track tok-a's
	// resourceVersion.
	dep := &appsv1.Deployment{}
	eventually(t, func() (bool, error) {
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(cr), dep); err != nil {
			return false, err
		}
		vol := findTbotTokenVolume(dep)
		if vol == nil {
			return false, fmt.Errorf("Deployment missing tbot-token volume")
		}
		if vol.Secret == nil || vol.Secret.SecretName != tokA.Name {
			return false, fmt.Errorf("tbot-token secretName: want %q, got %+v", tokA.Name, vol.Secret)
		}
		if got := dep.Spec.Template.Annotations[AnnotationTokenSecretVersion]; got != tokA.ResourceVersion {
			return false, fmt.Errorf("annotation: want %q (tok-a RV), got %q", tokA.ResourceVersion, got)
		}
		return true, nil
	})

	// (3) Create the replacement Secret tok-b with the same key.
	tokB := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tok-b", Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte("v2")},
	}
	if err := testClient.Create(ctx, tokB); err != nil {
		t.Fatalf("create tok-b: %v", err)
	}

	// (4) Update cr.spec.tokenRef.Name to point at tok-b. The CR watch
	// triggers a reconcile; no manual nudging needed.
	got := &accessv1alpha1.RemoteApp{}
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(cr), got); err != nil {
		t.Fatalf("re-fetch CR: %v", err)
	}
	got.Spec.TokenRef.Name = tokB.Name
	if err := testClient.Update(ctx, got); err != nil {
		t.Fatalf("update tokenRef.Name: %v", err)
	}

	// (5) The Deployment's volume must now reference tok-b and the
	// annotation must match tok-b's resourceVersion.
	eventually(t, func() (bool, error) {
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(cr), dep); err != nil {
			return false, err
		}
		vol := findTbotTokenVolume(dep)
		if vol == nil || vol.Secret == nil {
			return false, fmt.Errorf("Deployment missing tbot-token volume")
		}
		if vol.Secret.SecretName != tokB.Name {
			return false, fmt.Errorf("tbot-token secretName: want %q (tok-b), got %q", tokB.Name, vol.Secret.SecretName)
		}
		if got := dep.Spec.Template.Annotations[AnnotationTokenSecretVersion]; got != tokB.ResourceVersion {
			return false, fmt.Errorf("annotation: want %q (tok-b RV), got %q", tokB.ResourceVersion, got)
		}
		return true, nil
	})

	// (6) Watch-scoping invariant: a Secret event on the orphaned tok-a
	// must NOT fan out to the CR (which now references tok-b). We assert
	// this against a fake client wired with the same indexer the
	// production reconciler registers via SetupWithManager — the field
	// index is the seam mapSecretToRemoteApps drives off, so checking the
	// fan-out shape there is equivalent to the live envtest path without
	// the workqueue timing dependency.
	scheme := mapperScheme(t)
	fakeC := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&accessv1alpha1.RemoteApp{}, IndexFieldTokenRefName, remoteAppTokenRefIndexer).
		WithObjects(remoteAppRefs(ns, cr.Name, tokB.Name)). // CR now points at tok-b
		Build()
	r := &Reconciler{Client: fakeC}
	requests := r.mapSecretToRemoteApps(ctx, secretMeta(ns, tokA.Name))
	if len(requests) != 0 {
		t.Errorf("orphaned tok-a event must not fan out to the CR (now points at tok-b); got %d request(s): %v",
			len(requests), requestNames(requests))
	}
}

// findTbotTokenVolume returns the named token volume the renderer attaches
// to the tbot pod, or nil if absent. Volume name is the stable contract
// pinned by render_test.go's mount/volume assertions.
func findTbotTokenVolume(dep *appsv1.Deployment) *corev1.Volume {
	for i := range dep.Spec.Template.Spec.Volumes {
		v := &dep.Spec.Template.Spec.Volumes[i]
		if v.Name == "tbot-token" {
			return v
		}
	}
	return nil
}
