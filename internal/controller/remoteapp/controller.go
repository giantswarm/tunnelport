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
// Read-only verb sets the controller requires:
//
//   - `pods` (get;list;watch): the lastError summary is derived from
//     pod Phase / ContainerStatuses / RestartCount / last termination
//     reason. We never request `pods/log`.
//
// ADR 0006: the operator no longer references any token Secret. tbot
// joins via `kubernetes` using the per-RemoteApp ServiceAccount the
// reconciler renders; the projected SA token is delivered to the pod by
// the kubelet without operator involvement. The Secret read grant and
// the `secret_watch` plumbing that supported `spec.tokenRef` are gone.
//
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps,verbs=get;list;watch
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
package remoteapp

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// fieldManager is the Server-Side Apply field-manager identity this
// controller writes under. Stable across releases: changing it would
// orphan the field-ownership records the API server keeps, causing one
// reconcile after the change to look like first-time ownership for every
// owned field. Other field managers (admission mutators injecting sidecars,
// e.g. service-mesh webhooks) keep their own ownership of the fields they
// write — `ForceOwnership` only takes back fields we *also* write.
const fieldManager = "remoteapp-controller"

// Reconciler renders a ConfigMap, Deployment, Service, and ServiceAccount
// in the CR's namespace, owned by the RemoteApp via OwnerReferences.
//
// ADR 0006: tbot joins Central via the `kubernetes` join method using
// the per-RemoteApp ServiceAccount's projected JWT (audience
// `tunnelport.giantswarm.io`). There is no consumer-side token Secret
// to watch or rotate — the kubelet rotates the projected SA token
// automatically.
type Reconciler struct {
	client.Client

	// PodDefaults carries the operator-level knobs (tbot image and resource
	// requests/limits) that are NOT on the CR. Slice 6 will plumb this
	// from Helm values.
	PodDefaults PodDefaults

	// Recorder emits Kubernetes Events against the RemoteApp CR. Wired
	// from the manager in main.go; the reconcile loop does not yet call
	// it — that arrives in a later bundle.
	Recorder events.EventRecorder
}

// Reconcile renders the four owned objects from the RemoteApp's spec and
// applies them via Server-Side Apply. Spec mutations re-render and are
// propagated on the next reconcile pass.
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

	// ServiceAccount goes first so the Deployment that references it via
	// `spec.template.spec.serviceAccountName` always finds it during pod
	// admission. ADR 0006: the SA is the join identity tbot presents to
	// Central via the `kubernetes` join method.
	if err := r.applyOwned(ctx, cr, renderServiceAccount(cr, r.PodDefaults)); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ServiceAccount: %w", err)
	}
	if err := r.applyOwned(ctx, cr, renderConfigMap(cr, r.PodDefaults)); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ConfigMap: %w", err)
	}
	if err := r.applyOwned(ctx, cr, renderDeployment(cr, r.PodDefaults)); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Deployment: %w", err)
	}
	if err := r.applyOwned(ctx, cr, renderService(cr, r.PodDefaults)); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Service: %w", err)
	}

	// Status: derived from k8s-visible state only (ADR 0003). This must
	// run last so observedGeneration only catches up after the owned
	// objects above are applied successfully.
	if err := r.reconcileStatus(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile status: %w", err)
	}

	logger.V(1).Info("reconciled")
	return ctrl.Result{}, nil
}

// applyOwned writes the rendered owned object to the API server via
// Server-Side Apply. The controller's field-manager (`fieldManager`)
// claims ownership of every field the rendered `desired` object carries
// and *only* those fields — anything an admission mutator injects
// (sidecar containers, sidecar volume mounts, sidecar-injected
// annotations) belongs to its own field-manager and is preserved across
// our applies. `client.ForceOwnership` resolves the case where a field
// we now write was previously owned by a different manager (the most
// common cause is a manual `kubectl edit` against this controller's
// objects); the field migrates back to us.
//
// Invariants encoded by *not* writing fields:
//   - Service.Spec.ClusterIP — renderService doesn't set it, so the
//     API-server-allocated value is preserved automatically. The
//     previous client-side merge had to copy it from the existing
//     object to avoid "field is immutable".
//   - ConfigMap data keys we don't render — preserved.
//   - Deployment containers other than "tbot" (sidecar injection) —
//     preserved.
//
// TypeMeta is required on the apply payload because we route through
// `client.ApplyConfigurationFromUnstructured` which JSON-marshals via
// the unstructured converter and the API server rejects SSA bodies that
// lack `apiVersion`/`kind`. The render functions stamp TypeMeta inline;
// this method does not re-stamp.
func (r *Reconciler) applyOwned(ctx context.Context, cr *accessv1alpha1.RemoteApp, desired client.Object) error {
	if err := setOwnerRef(cr, desired); err != nil {
		return fmt.Errorf("set owner ref: %w", err)
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	if err != nil {
		return fmt.Errorf("convert to unstructured: %w", err)
	}
	ac := client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: u})
	return r.Apply(ctx, ac,
		client.FieldOwner(fieldManager),
		client.ForceOwnership,
	)
}

// SetupWithManager wires this Reconciler to its CR type and the four
// owned object types. Owns(...) gives us watches with predictable
// requeue-on-child-change semantics.
//
// Watches(&corev1.Pod{}, ...) routes pod-level events to the owning
// RemoteApp via the canonical LabelRemoteAppInstance label. Pod state
// (CrashLoopBackOff, restart count, last termination reason) is what
// populates status.lastError, so the reconciler must re-run on Pod
// events. Pods are owned by the rendered ReplicaSet (transitively the
// Deployment), so an `Owns` on Pod would not catch them — the
// label-driven mapping is the stable seam. The manager-level cache
// filter in cmd/main.go also restricts the Pod informer to the
// `tunnelport.giantswarm.io/role=tbot` label, narrowing the cache to
// pods this operator itself rendered.
//
// ADR 0006: there is no Secret watch. The legacy plumbing that mapped
// Secret events back to RemoteApps via `spec.tokenRef.name` was the
// rotation seam for the old static-token model; with `kubernetes` join
// there is no consumer-side Secret to watch.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&accessv1alpha1.RemoteApp{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ServiceAccount{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.mapPodToRemoteApp),
		).
		Named("remoteapp").
		Complete(r)
}

// mapPodToRemoteApp routes a Pod event to the RemoteApp whose name lives
// in the canonical LabelRemoteAppInstance label. We do not look up the
// owning ReplicaSet/Deployment chain — the label is the stable contract
// on every rendered pod template.
func (r *Reconciler) mapPodToRemoteApp(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[LabelRole] != LabelRoleValue {
		return nil
	}
	name := labels[LabelRemoteAppInstance]
	if name == "" {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}},
	}
}

// reconcileStatus is I/O-only: list pods, build the new status via the
// pure computeStatus function, write via Status subresource if changed.
// ADR 0003 forbids reading pod logs; only Pod metadata is touched.
//
// The write uses `client.MergeFrom` against a snapshot taken right
// before the patch, so the strategic-merge patch only carries the
// status diff — no spec fields, no stale baseline. `MergeFrom`
// without `MergeFromWithOptimisticLock` does not include
// resourceVersion in the patch body, which is what we want for status:
// any concurrent reconcile pass re-derives status from k8s-visible
// state and converges. Snapshotting just before the assignment
// (rather than at the top of Reconcile) keeps the diff window
// minimal and removes the prose about racing parallel writers, which
// MergeFrom doesn't actually guard against.
//
// Server-Side Apply for status is deliberately not used: status SSA requires
// every condition list element to carry the controller's field-manager
// imprint, and `meta.SetStatusCondition` doesn't speak that protocol — we'd
// have to fork the helper. MergeFrom gets the convergence semantics we need
// without that surgery.
func (r *Reconciler) reconcileStatus(ctx context.Context, cr *accessv1alpha1.RemoteApp) error {
	pods, err := r.listTbotPods(ctx, cr)
	if err != nil {
		return fmt.Errorf("list tbot pods: %w", err)
	}

	before := cr.Status.DeepCopy()
	newStatus := computeStatus(cr, pods, before.Conditions)
	if statusEqual(before, &newStatus) {
		return nil
	}

	patchBase := cr.DeepCopy()
	cr.Status = newStatus
	return r.Status().Patch(ctx, cr, client.MergeFrom(patchBase))
}

// listTbotPods returns the pods labelled as belonging to this RemoteApp.
// We list by label selector rather than walking the Deployment ->
// ReplicaSet -> Pod chain to keep the reconciler decoupled from the
// shape of the in-cluster Deployment hierarchy.
func (r *Reconciler) listTbotPods(ctx context.Context, cr *accessv1alpha1.RemoteApp) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	err := r.List(ctx, pods,
		client.InNamespace(cr.Namespace),
		client.MatchingLabels{
			LabelRole:              LabelRoleValue,
			LabelRemoteAppInstance: cr.Name,
		},
	)
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// statusEqual returns true when two RemoteAppStatus values are equivalent
// for hot-loop-suppression purposes. LastTransitionTime is stripped from
// each condition before comparison: meta.SetStatusCondition already
// preserves it across non-time changes, and ignoring it here prevents
// reconcile loops on time-only diffs. Anything else added to
// RemoteAppStatus is compared automatically by Semantic.DeepEqual — no
// per-field maintenance needed.
func statusEqual(a, b *accessv1alpha1.RemoteAppStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	ac, bc := a.DeepCopy(), b.DeepCopy()
	for i := range ac.Conditions {
		ac.Conditions[i].LastTransitionTime = metav1.Time{}
	}
	for i := range bc.Conditions {
		bc.Conditions[i].LastTransitionTime = metav1.Time{}
	}
	return equality.Semantic.DeepEqual(ac, bc)
}
