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

package trustbundle

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace  = "tunnelport-system"
	testSecretName = "tunnelport-spiffe-bundle"
)

func newReconciler(objs ...client.Object) *Reconciler {
	scheme := clientgoscheme.Scheme
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
	return &Reconciler{Client: c, SecretName: testSecretName, Namespace: testNamespace}
}

func bundleSecret(data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecretName},
		Data:       map[string][]byte{bundleKey: data},
	}
}

// consumerDeployment builds a Deployment in the test namespace opted in via
// the requested label placement. podLabel mirrors the dex `podLabels` path;
// metaLabel mirrors a Deployment-metadata opt-in.
func consumerDeployment(name string, podLabel, metaLabel bool) *appsv1.Deployment {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      name,
			Labels:    map[string]string{},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "img"}},
				},
			},
		},
	}
	if podLabel {
		dep.Spec.Template.Labels[LabelTrustBundleConsumer] = labelTrustBundleConsumerValue
	}
	if metaLabel {
		dep.Labels[LabelTrustBundleConsumer] = labelTrustBundleConsumerValue
	}
	return dep
}

func reconcileOnce(t *testing.T, r *Reconciler) {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testSecretName}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func getDeployment(t *testing.T, r *Reconciler, name string) *appsv1.Deployment {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, dep); err != nil {
		t.Fatalf("get deployment %s: %v", name, err)
	}
	return dep
}

func TestBundleHash(t *testing.T) {
	a := bundleHash([]byte("ca-set-one"))
	again := bundleHash([]byte("ca-set-one"))
	b := bundleHash([]byte("ca-set-two"))
	if a != again {
		t.Errorf("bundleHash not deterministic: %q vs %q", a, again)
	}
	if a == b {
		t.Errorf("bundleHash collided on distinct content: %q", a)
	}
}

// TestReconcile_RollsConsumerStampsHash: a consumer with no annotation gets
// the bundle hash stamped onto its pod template, while a non-consumer in the
// same namespace is left untouched.
func TestReconcile_RollsConsumerStampsHash(t *testing.T) {
	bundle := []byte("-----BEGIN CERTIFICATE-----\nrootA\n-----END CERTIFICATE-----")
	r := newReconciler(
		bundleSecret(bundle),
		consumerDeployment("dex", true, false),
		consumerDeployment("bystander", false, false),
	)

	reconcileOnce(t, r)

	want := bundleHash(bundle)
	dex := getDeployment(t, r, "dex")
	if got := dex.Spec.Template.Annotations[AnnotationTrustBundleHash]; got != want {
		t.Errorf("consumer not stamped: annotation=%q want %q", got, want)
	}
	// Selection must not disturb non-consumers.
	bystander := getDeployment(t, r, "bystander")
	if _, ok := bystander.Spec.Template.Annotations[AnnotationTrustBundleHash]; ok {
		t.Errorf("non-consumer was stamped; selection leaked")
	}
	// The SSA apply payload carries only the pod-template annotation, so
	// the consumer's pre-existing pod-template fields must survive the
	// merge (the opt-in label and the container). On a real apiserver SSA
	// also preserves sibling fields owned by other managers (selector,
	// replicas); the fake client's structured-merge is only faithful at
	// the sub-tree we touch, so we assert preservation there.
	if dex.Spec.Template.Labels[LabelTrustBundleConsumer] != labelTrustBundleConsumerValue {
		t.Errorf("SSA dropped the consumer pod-template label")
	}
	if len(dex.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("SSA dropped the consumer pod-template containers")
	}
}

// TestReconcile_DeDupeNoRollOnUnchangedContent pins the restart-storm guard:
// a second reconcile with the same bundle content must not re-write the
// consumer (the ~20m tbot renewal rewrites the Secret but not the CA set).
func TestReconcile_DeDupeNoRollOnUnchangedContent(t *testing.T) {
	bundle := []byte("stable-ca-set")
	r := newReconciler(
		bundleSecret(bundle),
		consumerDeployment("muster", true, false),
	)

	reconcileOnce(t, r)
	first := getDeployment(t, r, "muster")
	rvAfterFirst := first.ResourceVersion

	// Second pass: same content -> de-dupe -> no write -> RV unchanged.
	reconcileOnce(t, r)
	second := getDeployment(t, r, "muster")
	if second.ResourceVersion != rvAfterFirst {
		t.Errorf("consumer re-written on unchanged content: rv %q -> %q (restart storm)", rvAfterFirst, second.ResourceVersion)
	}
}

// TestReconcile_RollsAgainOnCAChange: when the CA set actually changes, the
// stamped hash changes too, which is what rolls the consumer.
func TestReconcile_RollsAgainOnCAChange(t *testing.T) {
	r := newReconciler(
		bundleSecret([]byte("ca-set-v1")),
		consumerDeployment("dex", true, false),
	)
	reconcileOnce(t, r)
	h1 := getDeployment(t, r, "dex").Spec.Template.Annotations[AnnotationTrustBundleHash]

	// Rotate the CA set.
	sec := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testSecretName}, sec); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	sec.Data[bundleKey] = []byte("ca-set-v2")
	if err := r.Update(context.Background(), sec); err != nil {
		t.Fatalf("update secret: %v", err)
	}

	reconcileOnce(t, r)
	h2 := getDeployment(t, r, "dex").Spec.Template.Annotations[AnnotationTrustBundleHash]

	if h1 == h2 {
		t.Errorf("hash unchanged after CA rotation (%q); consumer would not roll", h2)
	}
	if want := bundleHash([]byte("ca-set-v2")); h2 != want {
		t.Errorf("stamped hash %q does not match new CA set hash %q", h2, want)
	}
}

// TestReconcile_ConsumerLabelPlacements: the opt-in label is honoured on
// either the Deployment metadata or the pod template.
func TestReconcile_ConsumerLabelPlacements(t *testing.T) {
	bundle := []byte("ca")
	want := bundleHash(bundle)
	r := newReconciler(
		bundleSecret(bundle),
		consumerDeployment("via-pod", true, false),
		consumerDeployment("via-meta", false, true),
		consumerDeployment("neither", false, false),
	)

	reconcileOnce(t, r)

	for _, name := range []string{"via-pod", "via-meta"} {
		if got := getDeployment(t, r, name).Spec.Template.Annotations[AnnotationTrustBundleHash]; got != want {
			t.Errorf("%s not stamped: %q want %q", name, got, want)
		}
	}
	if _, ok := getDeployment(t, r, "neither").Spec.Template.Annotations[AnnotationTrustBundleHash]; ok {
		t.Errorf("unlabelled deployment was stamped")
	}
}

// TestReconcile_SecretMissingIsNoOp: no trust-bundle Secret yet -> no error,
// nothing stamped.
func TestReconcile_SecretMissingIsNoOp(t *testing.T) {
	r := newReconciler(
		consumerDeployment("dex", true, false),
	)
	reconcileOnce(t, r)
	if _, ok := getDeployment(t, r, "dex").Spec.Template.Annotations[AnnotationTrustBundleHash]; ok {
		t.Errorf("stamped a consumer with no trust-bundle Secret present")
	}
}

// TestReconcile_NoBundleKeyIsNoOp: a Secret lacking svid_bundle.pem is not a
// CA set to hash; consumers are left alone.
func TestReconcile_NoBundleKeyIsNoOp(t *testing.T) {
	r := newReconciler(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecretName},
			Data:       map[string][]byte{"other.pem": []byte("x")},
		},
		consumerDeployment("dex", true, false),
	)
	reconcileOnce(t, r)
	if _, ok := getDeployment(t, r, "dex").Spec.Template.Annotations[AnnotationTrustBundleHash]; ok {
		t.Errorf("stamped a consumer from a Secret with no %s key", bundleKey)
	}
}

// TestReconcile_IgnoresOtherSecret: an enqueue for a different Secret name is
// ignored by the in-Reconcile guard.
func TestReconcile_IgnoresOtherSecret(t *testing.T) {
	r := newReconciler(
		bundleSecret([]byte("ca")),
		consumerDeployment("dex", true, false),
	)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "some-other-secret"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := getDeployment(t, r, "dex").Spec.Template.Annotations[AnnotationTrustBundleHash]; ok {
		t.Errorf("acted on an unrelated Secret enqueue")
	}
}
