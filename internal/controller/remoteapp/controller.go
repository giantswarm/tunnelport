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
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps,verbs=get;list;watch
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
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
	"sigs.k8s.io/controller-runtime/pkg/log"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// Reconciler renders a ConfigMap, Deployment, and Service in the CR's
// namespace, owned by the RemoteApp via OwnerReferences. It does NOT
// populate status (slice 4) or watch token Secrets (slice 5).
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
	if err := r.reconcileDeployment(ctx, cr); err != nil {
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

func (r *Reconciler) reconcileDeployment(ctx context.Context, cr *accessv1alpha1.RemoteApp) error {
	desired := renderDeployment(cr, r.Config)
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
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&accessv1alpha1.RemoteApp{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Named("remoteapp").
		Complete(r)
}
