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

// RBAC for this controller's package. Resource verbs are the minimum the
// reconcile loop needs. ADR 0003 forbids `pods/log` and we deliberately
// do not list it here.
//
// The `secrets` rule is `get;list;watch` only — the operator references
// the token Secret by name and reads its `metadata.resourceVersion` to
// stamp the pod-template annotation, but never accesses `Secret.Data`.
// Slice 5's `secret_watch_test.go` enforces the no-data-access invariant
// with a static check.
//
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps,verbs=get;list;watch
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
package remoteapp

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// Reconciler renders a ConfigMap, Deployment, and Service in the CR's
// namespace, owned by the RemoteApp via OwnerReferences. It also watches
// the tokenRef Secret and stamps the Secret's resourceVersion onto the
// pod-template annotation `tunnelport.giantswarm.io/token-secret-version`
// so token rotations roll the Deployment via the existing RollingUpdate
// strategy (slice 5). It does NOT populate status (slice 4).
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Config carries the operator-level knobs (tbot image and resource
	// requests/limits) that are NOT on the CR. Slice 6 will plumb this
	// from Helm values.
	Config Config
}

// Reconcile renders the three owned objects from the RemoteApp's spec and
// applies them via server-side merge semantics (CreateOrUpdate). Spec
// mutations re-render and are propagated on the next reconcile pass.
//
// The reconciler never reads spec.tokenRef's Secret contents — only the
// Secret's name is referenced when constructing the pod's volume mount.
//
// RBAC: the operator must NOT request `pods/log` (ADR 0003). The
// package-level markers in this file request only what this slice's
// reconcile loop touches.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("remoteapp", req.NamespacedName)

	cr := &accessv1alpha1.RemoteApp{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		if apierrors.IsNotFound(err) {
			// CR was deleted; OwnerReferences GC the children.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get RemoteApp: %w", err)
	}

	if !cr.DeletionTimestamp.IsZero() {
		// No finalizer logic in this slice — owners cascade-delete via GC.
		return ctrl.Result{}, nil
	}

	if err := r.reconcileConfigMap(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ConfigMap: %w", err)
	}
	tokenSecretVersion, err := r.observeTokenSecretVersion(ctx, cr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("observe token Secret version: %w", err)
	}
	if err := r.reconcileDeployment(ctx, cr, tokenSecretVersion); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Deployment: %w", err)
	}
	if err := r.reconcileService(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Service: %w", err)
	}

	logger.V(1).Info("reconciled")
	return ctrl.Result{}, nil
}

func (r *Reconciler) reconcileConfigMap(ctx context.Context, cr *accessv1alpha1.RemoteApp) error {
	desired := renderConfigMap(cr, r.Config)
	if err := setOwnerRef(cr, desired); err != nil {
		return fmt.Errorf("set owner ref: %w", err)
	}

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Only update mutable fields. Labels and Data are the load-bearing
	// state for this slice; OwnerReferences stay stable across reconciles.
	existing.Labels = desired.Labels
	existing.Data = desired.Data
	existing.OwnerReferences = desired.OwnerReferences
	return r.Update(ctx, existing)
}

func (r *Reconciler) reconcileDeployment(ctx context.Context, cr *accessv1alpha1.RemoteApp, tokenSecretVersion string) error {
	desired := renderDeployment(cr, r.Config, tokenSecretVersion)
	if err := setOwnerRef(cr, desired); err != nil {
		return fmt.Errorf("set owner ref: %w", err)
	}

	existing := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Mutate only the fields we render. The Deployment controller fills
	// in things like Status and pod-template defaults — leave those.
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Strategy = desired.Spec.Strategy
	existing.Spec.Template = desired.Spec.Template
	return r.Update(ctx, existing)
}

func (r *Reconciler) reconcileService(ctx context.Context, cr *accessv1alpha1.RemoteApp) error {
	desired := renderService(cr, r.Config)
	if err := setOwnerRef(cr, desired); err != nil {
		return fmt.Errorf("set owner ref: %w", err)
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// ClusterIP is allocated by the API server on first create; preserve
	// it across updates to avoid spurious "field is immutable" errors.
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	existing.Spec.Type = desired.Spec.Type
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = desired.Spec.Ports
	return r.Update(ctx, existing)
}

// SetupWithManager wires this Reconciler to its CR type and the three
// owned object types. Owns(...) gives us watches with predictable
// requeue-on-child-change semantics.
//
// Watches(&corev1.Secret{}, ...) extends the watch surface to token
// Secrets: any Secret create/update/delete triggers mapSecretToRemoteApps,
// which fans out only to the RemoteApps that actually reference that
// Secret by `spec.tokenRef.name`. Unrelated Secret churn produces an
// empty []reconcile.Request and is dropped before it touches the
// workqueue.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&accessv1alpha1.RemoteApp{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToRemoteApps),
		).
		Named("remoteapp").
		Complete(r)
}

// observeTokenSecretVersion fetches the tokenRef Secret and returns its
// `metadata.resourceVersion`. The reconciler stamps the value onto the
// pod-template annotation `tunnelport.giantswarm.io/token-secret-version`
// (slice 5), so a content rotation — which always changes resourceVersion
// — rolls the Deployment via its existing RollingUpdate strategy.
//
// A missing Secret returns "" rather than an error: the GitOps-race case
// where the CR is applied before the Secret is delivered is expected,
// and the rendered pod stays Pending on the volume mount until the
// Secret appears (CONTEXT.md, "Token Secret delivery"). Slice 4 surfaces
// the absence on `status.TokenSecretBound`; slice 5 stays out of status.
//
// Only ObjectMeta is read. The function intentionally does NOT type the
// access through `.Data`, and the package-level static check in
// secret_watch_test.go enforces no `secret.Data`-style references exist
// anywhere in the controller code.
func (r *Reconciler) observeTokenSecretVersion(ctx context.Context, cr *accessv1alpha1.RemoteApp) (string, error) {
	key := client.ObjectKey{Namespace: cr.Namespace, Name: cr.Spec.TokenRef.Name}
	var sec corev1.Secret
	if err := r.Get(ctx, key, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("get token Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	// Read only metadata. Accessing `sec.Data` here would violate the
	// no-data-access invariant enforced by the static check in
	// secret_watch_test.go.
	return sec.ResourceVersion, nil
}

// mapSecretToRemoteApps fans a Secret event out to the RemoteApps that
// reference it via `spec.tokenRef.name`. It lists RemoteApps in the
// Secret's namespace only — `tokenRef` is namespace-local by design
// (CONTEXT.md: "no cross-namespace references"), so a Secret in ns A can
// never trigger reconciles for a RemoteApp in ns B even if both names
// match.
//
// Returning an empty slice for unreferenced Secrets is what scopes the
// watch: controller-runtime drops empty fan-outs before they hit the
// workqueue, so unrelated Secret churn does NOT cause Reconcile calls.
//
// The function reads only the Secret's ObjectMeta (Namespace, Name); it
// must not touch `Secret.Data`. Slice 5's secret_watch_test.go enforces
// this with a static-grep test.
func (r *Reconciler) mapSecretToRemoteApps(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)

	// Defensive type assertion: the Watches binding only fires for
	// Secrets, but if the wiring ever drifts we'd rather drop the event
	// than panic or fan out to every RemoteApp.
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	var apps accessv1alpha1.RemoteAppList
	if err := r.List(ctx, &apps, client.InNamespace(sec.Namespace)); err != nil {
		// Listing the cache should not fail in steady state. Log and
		// drop — controller-runtime will re-fire on the next Secret
		// event, and the periodic full-sync recovers any missed roll.
		logger.Error(err, "list RemoteApps for Secret fan-out", "secret", sec.Namespace+"/"+sec.Name)
		return nil
	}

	out := make([]reconcile.Request, 0, len(apps.Items))
	for i := range apps.Items {
		app := &apps.Items[i]
		if app.Spec.TokenRef.Name != sec.Name {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: client.ObjectKey{
				Namespace: app.Namespace,
				Name:      app.Name,
			},
		})
	}
	return out
}
