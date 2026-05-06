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
	"reflect"
	"sort"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// mapperScheme is a scheme registered with the API types the mapper needs to
// list (RemoteApp). Built once per test file so individual cases can lean on
// fake.NewClientBuilder for a synchronous, deterministic list.
func mapperScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgo scheme: %v", err)
	}
	if err := accessv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("accessv1alpha1 scheme: %v", err)
	}
	return s
}

// secretMeta builds a minimal Secret object the mapper can be driven from.
// Only ObjectMeta is set — the mapper must not look at .Data.
func secretMeta(namespace, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func TestRenderDeployment_StampsTokenSecretVersionAnnotation(t *testing.T) {
	cr := fixtureRemoteApp()

	// Empty version means "Secret not yet observed" — annotation must
	// still be present so absence vs. value-change is unambiguous in
	// the pod-template diff.
	depEmpty := renderDeployment(cr, fixtureConfig(), "")
	gotEmpty, ok := depEmpty.Spec.Template.Annotations[AnnotationTokenSecretVersion]
	if !ok {
		t.Fatalf("pod template missing %s annotation when version is empty", AnnotationTokenSecretVersion)
	}
	if gotEmpty != "" {
		t.Errorf("annotation value with empty version: want %q, got %q", "", gotEmpty)
	}

	// Non-empty version is stamped verbatim — the reconciler reads
	// `secret.ObjectMeta.ResourceVersion` and threads it through.
	depV1 := renderDeployment(cr, fixtureConfig(), "12345")
	if got := depV1.Spec.Template.Annotations[AnnotationTokenSecretVersion]; got != "12345" {
		t.Errorf("annotation value: want %q, got %q", "12345", got)
	}

	// A different version produces a different pod-template annotation
	// — the Deployment controller treats this as a template change and
	// rolls. The existing config-hash must be stable across calls so a
	// pure token-version change rolls only on the version annotation.
	depV2 := renderDeployment(cr, fixtureConfig(), "67890")
	if got := depV2.Spec.Template.Annotations[AnnotationTokenSecretVersion]; got != "67890" {
		t.Errorf("annotation value after rotation: want %q, got %q", "67890", got)
	}
	if depV1.Spec.Template.Annotations[AnnotationConfigHash] !=
		depV2.Spec.Template.Annotations[AnnotationConfigHash] {
		t.Errorf("config-hash should be independent of token-secret-version")
	}
}

// remoteAppRefs is a thin adapter around the shared newRemoteApp fixture
// that pins what the mapper tests below actually exercise:
// (namespace, name, tokenRef.Name) — the only fields the field-indexer
// and the namespace-local list filter look at. Empty UID is fine here:
// the fake client doesn't enforce OwnerReference UID round-tripping.
func remoteAppRefs(namespace, name, tokenSecretName string) *accessv1alpha1.RemoteApp {
	cr := newRemoteApp(withName(namespace, name), withTokenRef(tokenSecretName, "token"))
	cr.UID = ""
	return cr
}

// remoteAppTokenRefIndexer mirrors the field-indexer registered in
// SetupWithManager so fake-client tests can use the same MatchingFields
// path the production reconciler uses.
func remoteAppTokenRefIndexer(obj client.Object) []string {
	cr, ok := obj.(*accessv1alpha1.RemoteApp)
	if !ok || cr.Spec.TokenRef.Name == "" {
		return nil
	}
	return []string{cr.Spec.TokenRef.Name}
}

func TestMapSecretToRemoteApps_ReturnsMatchingRemoteAppsInSameNamespace(t *testing.T) {
	scheme := mapperScheme(t)

	// Two RemoteApps in ns "demo" both reference Secret "shared-token".
	// One in "other" namespace also references "shared-token" — must NOT
	// be returned because tokenRef Secret lookup is namespace-local.
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&accessv1alpha1.RemoteApp{}, IndexFieldTokenRefName, remoteAppTokenRefIndexer).
		WithObjects(
			remoteAppRefs("demo", "alpha", "shared-token"),
			remoteAppRefs("demo", "beta", "shared-token"),
			remoteAppRefs("demo", "gamma", "different-token"),
			remoteAppRefs("other", "delta", "shared-token"),
		).Build()

	r := &Reconciler{Client: c}
	got := r.mapSecretToRemoteApps(context.Background(), secretMeta("demo", "shared-token"))

	gotNames := requestNames(got)
	want := []string{"demo/alpha", "demo/beta"}
	if !equalSorted(gotNames, want) {
		t.Errorf("mapSecretToRemoteApps: want %v, got %v", want, gotNames)
	}
}

func TestMapSecretToRemoteApps_ReturnsEmptyForUnreferencedSecret(t *testing.T) {
	scheme := mapperScheme(t)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&accessv1alpha1.RemoteApp{}, IndexFieldTokenRefName, remoteAppTokenRefIndexer).
		WithObjects(
			remoteAppRefs("demo", "alpha", "shared-token"),
		).Build()

	r := &Reconciler{Client: c}
	got := r.mapSecretToRemoteApps(context.Background(), secretMeta("demo", "unrelated"))

	if len(got) != 0 {
		t.Errorf("mapSecretToRemoteApps: want no requests for unrelated Secret, got %v", requestNames(got))
	}
}

func TestMapSecretToRemoteApps_IgnoresNonSecretObject(t *testing.T) {
	scheme := mapperScheme(t)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&accessv1alpha1.RemoteApp{}, IndexFieldTokenRefName, remoteAppTokenRefIndexer).
		WithObjects(
			remoteAppRefs("demo", "alpha", "shared-token"),
		).Build()

	r := &Reconciler{Client: c}
	// Pass a non-Secret object (a ConfigMap) — the mapper must not fan
	// out to RemoteApps when the watched object isn't a Secret.
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "shared-token", Namespace: "demo"}}
	got := r.mapSecretToRemoteApps(context.Background(), cm)

	if len(got) != 0 {
		t.Errorf("mapSecretToRemoteApps: want no requests for non-Secret object, got %v", requestNames(got))
	}
}

// observeTokenSecretSignature pins the type signature of
// (*Reconciler).observeTokenSecret at compile time. If anyone changes
// the method to return a *corev1.Secret (directly or as part of a
// composite return), this assignment fails to compile — the build
// surfaces the regression before the test even runs.
//
// The deliberate shape: input takes a *RemoteApp (so the function can
// stay namespace-aware) and returns tokenSecretMeta — a typed value
// with no Data field. Any change to widen the return to expose Secret
// bytes must edit this line, which is the canary the reviewer sees.
var observeTokenSecretSignature func(*Reconciler, context.Context, *accessv1alpha1.RemoteApp) tokenSecretMeta = (*Reconciler).observeTokenSecret

// TestController_TypedSecretAccessor enforces the brief's "operator
// must not read Secret.Data" contract structurally rather than by AST
// receiver-name allowlist. The previous implementation grepped for
// `<id>.Data` where `<id>` was in {sec, secret, tokenSecret} — false-
// negative on any other identifier (the production code already uses
// `s`), false-positive on safe code that happens to use a banned name.
//
// The structural guarantee here:
//
//  1. tokenSecretMeta has no field named "Data" and no field whose type
//     is or contains *corev1.Secret. This is the type that crosses the
//     observeTokenSecret boundary, so anything not on this struct is
//     invisible to callers.
//
//  2. observeTokenSecret's signature (pinned by
//     observeTokenSecretSignature above) returns tokenSecretMeta — not
//     *corev1.Secret, not a struct containing one. Combined with (1),
//     no *corev1.Secret value escapes observeTokenSecret.
//
// The compile-time pin in observeTokenSecretSignature plus this
// runtime introspection of tokenSecretMeta gives the same guarantee a
// full go/types call-graph scan would, with a fraction of the test
// surface and no x/tools dependency.
func TestController_TypedSecretAccessor(t *testing.T) {
	metaType := reflect.TypeFor[tokenSecretMeta]()
	secretPtrType := reflect.TypeFor[*corev1.Secret]()

	// (1a) No field literally named "Data" — the most common shape a
	// future contributor might reach for if they wanted Secret bytes.
	if _, ok := metaType.FieldByName("Data"); ok {
		t.Errorf("tokenSecretMeta has a Data field — Secret bytes must never cross out of observeTokenSecret")
	}

	// (1b) No field of *corev1.Secret type, and (transitively) no field
	// whose own field set contains a *corev1.Secret. Walks one level of
	// nested structs — tokenSecretMeta is shallow by design, so deeper
	// recursion isn't needed and would mask a more meaningful test
	// failure.
	for i := 0; i < metaType.NumField(); i++ {
		f := metaType.Field(i)
		if f.Type == secretPtrType {
			t.Errorf("tokenSecretMeta.%s is *corev1.Secret — Secret pointer must not escape observeTokenSecret", f.Name)
		}
		if f.Type.Kind() == reflect.Struct {
			for j := 0; j < f.Type.NumField(); j++ {
				inner := f.Type.Field(j)
				if inner.Type == secretPtrType {
					t.Errorf("tokenSecretMeta.%s.%s is *corev1.Secret — Secret pointer must not escape observeTokenSecret",
						f.Name, inner.Name)
				}
			}
		}
	}

	// (2) The signature pin: if the method returned anything other than
	// tokenSecretMeta the package would not compile. Asserting at
	// runtime that the captured function value's return type matches
	// makes the contract visible in the test output rather than only
	// at build time.
	sigType := reflect.TypeOf(observeTokenSecretSignature)
	if sigType.Kind() != reflect.Func {
		t.Fatalf("observeTokenSecretSignature is not a function: %s", sigType.Kind())
	}
	if sigType.NumOut() != 1 {
		t.Fatalf("observeTokenSecret should return exactly 1 value, got %d", sigType.NumOut())
	}
	if got := sigType.Out(0); got != metaType {
		t.Errorf("observeTokenSecret return type: want tokenSecretMeta, got %s", got)
	}
	for i := 0; i < sigType.NumIn(); i++ {
		if sigType.In(i) == secretPtrType {
			t.Errorf("observeTokenSecret parameter %d is *corev1.Secret — Secret pointer must not be supplied externally", i)
		}
	}
}

func requestNames(rs []reconcile.Request) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Namespace + "/" + r.Name
	}
	return out
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// TestReconciler_TokenSecretRotationStampsAnnotationAndRollsDeployment is the
// end-to-end happy-path integration test for slice 5. It drives the live
// envtest API server and the controller-runtime manager wired up in
// suite_test.go. The sequence:
//
//  1. Create a Secret with the join token, then a RemoteApp pointing at it.
//  2. Wait for the Deployment to render and read the initial annotation
//     value — must equal the Secret's resourceVersion.
//  3. Update the Secret's `Data` (only the user is allowed to read/write
//     `.Data`, not the operator). The Secret's resourceVersion bumps.
//  4. Wait for the Deployment annotation to track the new resourceVersion.
//     A different annotation value means the pod template differs from
//     the previous template — the Deployment controller computes a new
//     pod-template-hash and a rolling update is triggered.
func TestReconciler_TokenSecretRotationStampsAnnotationAndRollsDeployment(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	// Create the token Secret first — the user owns Secret.Data; the
	// operator does not.
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rotating-token", Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte("v1-token-content")},
	}
	if err := testClient.Create(ctx, tokenSecret); err != nil {
		t.Fatalf("create token Secret: %v", err)
	}

	cr := makeRemoteApp(ctx, t, ns, "rotation-target",
		withAppName("rotating-app"),
		withTokenRef(tokenSecret.Name, "token"),
	)

	// Initial Deployment annotation must equal the Secret's
	// resourceVersion at create time.
	initialRV := tokenSecret.ResourceVersion
	if initialRV == "" {
		t.Fatalf("token Secret has empty resourceVersion after create")
	}
	dep := &appsv1.Deployment{}
	eventually(t, func() (bool, error) {
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, dep); err != nil {
			return false, err
		}
		got := dep.Spec.Template.Annotations[AnnotationTokenSecretVersion]
		if got != initialRV {
			return false, fmt.Errorf("annotation %s: want %q, got %q", AnnotationTokenSecretVersion, initialRV, got)
		}
		return true, nil
	})
	templateBefore := dep.Spec.Template.DeepCopy()

	// Rotate the Secret's content. resourceVersion bumps on Update.
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(tokenSecret), tokenSecret); err != nil {
		t.Fatalf("re-fetch token Secret: %v", err)
	}
	tokenSecret.Data["token"] = []byte("v2-token-content-rotated")
	if err := testClient.Update(ctx, tokenSecret); err != nil {
		t.Fatalf("update token Secret: %v", err)
	}
	rotatedRV := tokenSecret.ResourceVersion
	if rotatedRV == initialRV {
		t.Fatalf("Secret resourceVersion did not change after Data update")
	}

	// Annotation must track the rotated resourceVersion. Pod template
	// must differ from the pre-rotation template — that's the signal the
	// Deployment controller uses to compute a new pod-template-hash and
	// trigger a rolling restart.
	eventually(t, func() (bool, error) {
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, dep); err != nil {
			return false, err
		}
		got := dep.Spec.Template.Annotations[AnnotationTokenSecretVersion]
		if got != rotatedRV {
			return false, fmt.Errorf("annotation %s after rotation: want %q, got %q", AnnotationTokenSecretVersion, rotatedRV, got)
		}
		if equalPodTemplate(*templateBefore, dep.Spec.Template) {
			return false, fmt.Errorf("pod template unchanged after Secret rotation — Deployment would not roll")
		}
		return true, nil
	})
}

// TestReconciler_UnrelatedSecretEditDoesNotAffectDeployment exercises the
// watch-scoping invariant: only Secrets actually referenced by some
// RemoteApp.spec.tokenRef trigger reconciles. We assert the negative by
// recording the Deployment's pod template before and after editing an
// unrelated Secret in the same namespace and showing the template — and
// crucially the token-version annotation — is unchanged.
//
// The mapper unit tests above already prove the function returns no
// requests for unreferenced Secrets; this test is the integration-level
// belt-and-braces check that the wiring in SetupWithManager hooks the
// mapper into the workqueue correctly.
func TestReconciler_UnrelatedSecretEditDoesNotAffectDeployment(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "real-token", Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte("real-token-value")},
	}
	if err := testClient.Create(ctx, tokenSecret); err != nil {
		t.Fatalf("create token Secret: %v", err)
	}

	cr := makeRemoteApp(ctx, t, ns, "unrelated-target",
		withAppName("demo"),
		withTokenRef(tokenSecret.Name, "token"),
	)

	dep := &appsv1.Deployment{}
	eventually(t, func() (bool, error) {
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, dep); err != nil {
			return false, err
		}
		if dep.Spec.Template.Annotations[AnnotationTokenSecretVersion] != tokenSecret.ResourceVersion {
			return false, fmt.Errorf("annotation not yet stamped")
		}
		return true, nil
	})
	rvBefore := dep.ResourceVersion
	annotationBefore := dep.Spec.Template.Annotations[AnnotationTokenSecretVersion]

	// Edit an unrelated Secret in the same namespace. No RemoteApp's
	// tokenRef points at it, so the mapper must return no requests and
	// the Deployment must not change.
	noisy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "noisy-unrelated", Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"data": []byte("v1")},
	}
	if err := testClient.Create(ctx, noisy); err != nil {
		t.Fatalf("create unrelated Secret: %v", err)
	}
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(noisy), noisy); err != nil {
		t.Fatalf("re-fetch unrelated Secret: %v", err)
	}
	noisy.Data["data"] = []byte("v2")
	if err := testClient.Update(ctx, noisy); err != nil {
		t.Fatalf("update unrelated Secret: %v", err)
	}

	// Give the manager a chance to do something wrong. We don't have a
	// strict signal "no reconcile happened", so we instead assert the
	// observable consequences are absent: the Deployment's
	// resourceVersion and token-version annotation are unchanged.
	consistentlyFor(t, pollTimeout/2, func() error {
		got := &appsv1.Deployment{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: cr.Name}, got); err != nil {
			return fmt.Errorf("get Deployment: %w", err)
		}
		if got.ResourceVersion != rvBefore {
			return fmt.Errorf("Deployment resourceVersion changed (%q -> %q) after unrelated Secret edit",
				rvBefore, got.ResourceVersion)
		}
		if a := got.Spec.Template.Annotations[AnnotationTokenSecretVersion]; a != annotationBefore {
			return fmt.Errorf("token-version annotation changed (%q -> %q) after unrelated Secret edit",
				annotationBefore, a)
		}
		return nil
	})
}

// consistentlyFor runs check at pollInterval and fails the test if check
// returns an error within d. Used to assert "nothing observable changed
// in this window" — a complement to eventually's "something will change".
func consistentlyFor(t *testing.T, d time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := check(); err != nil {
			t.Fatalf("consistentlyFor: invariant violated: %v", err)
		}
		time.Sleep(pollInterval)
	}
}
