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
// Per-CR ServiceAccount: under the kubernetes join model (ADR 0004) the
// operator renders one ServiceAccount per RemoteApp; the SA's projected
// JWT is the subject the Teleport ProvisionToken's `allow` rule pins.
// The operator does not mount or read the SA's projected token itself —
// the kubelet does that into the tbot pod.
//
// Trust-bundle distribution: ADR 0008 supersedes the per-CR Secret/Role/
// RoleBinding shape from ADR 0007 with a chart-managed singleton tbot
// that writes ONE `tunnelport-spiffe-bundle` Secret in the release
// namespace. The operator therefore does NOT need write verbs on
// `core/secrets`, `rbac/roles`, or `rbac/rolebindings` — those grants
// were specific to the per-CR trust-bundle objects and are removed
// here.
//
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps,verbs=get;list;watch
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;patch
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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

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

// Reconciler renders a ServiceAccount, ConfigMap, Deployment, and Service
// in the CR's namespace, owned by the RemoteApp via OwnerReferences. Per
// ADR 0004 (kubernetes join), the rendered ServiceAccount carries the
// projected JWT identity that the tbot pod presents to Teleport — no
// static-token Secret is involved.
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

	// Verifications is the read side of the TLS verifier's store, used to
	// build the TunnelVerified condition. Nil when verification is
	// disabled, in which case the condition is omitted entirely.
	//
	// The verifier writes no status of its own: it records outcomes here
	// and pushes an event onto VerificationEvents, so RemoteApp.status
	// keeps exactly one writer (reconcileStatus) and the two cannot race
	// each other into a patch conflict loop.
	Verifications VerificationReader

	// VerificationEvents carries one event per RemoteApp whose
	// verification outcome changed. SetupWithManager turns it into a
	// watch, so a probe result lands in status on the next reconcile pass
	// rather than waiting for an unrelated Kubernetes event — the whole
	// point being that a wrong-SAN certificate generates no Kubernetes
	// events at all.
	VerificationEvents <-chan event.TypedGenericEvent[*accessv1alpha1.RemoteApp]
}

// Reconcile renders the four owned objects from the RemoteApp's spec and
// applies them via Server-Side Apply. Spec mutations re-render and are
// propagated on the next reconcile pass.
//
// Per ADR 0004 the operator renders a per-CR ServiceAccount; the tbot
// pod authenticates to Teleport via the projected JWT for that SA. No
// static-token Secret on the consumer cluster is involved.
//
// Apply order: ServiceAccount → ConfigMap → Deployment → Service. The
// Deployment references the SA by name, so SA must be ahead of it.
// ADR 0008 removed the per-CR trust-bundle Secret/Role/RoleBinding
// from this order; consumer trust-bundle distribution is the
// chart-managed singleton bot's responsibility now.
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

	// Collect the first apply error rather than bailing out early. The
	// status pass below needs to know whether any apply failed so it can
	// set the `Reconciled` condition; returning early would skip that
	// signal entirely. We still surface the error via the returned
	// ctrl.Result so controller-runtime requeues with backoff.
	var applyErr error
	for _, step := range []struct {
		name string
		obj  client.Object
	}{
		{kindServiceAccount, renderServiceAccount(cr, r.PodDefaults)},
		{"ConfigMap", renderConfigMap(cr, r.PodDefaults)},
		{"Deployment", renderDeployment(cr, r.PodDefaults)},
		{"Service", renderService(cr, r.PodDefaults)},
	} {
		if err := r.applyOwned(ctx, cr, step.obj); err != nil {
			applyErr = fmt.Errorf("reconcile %s: %w", step.name, err)
			break
		}
	}

	// Status: derived from k8s-visible state only (ADR 0003). This must
	// run last so observedGeneration only catches up after the owned
	// objects above are applied successfully. Always run it — even on
	// apply failure — so the Reconciled condition reflects reality.
	applyErrSummary := ""
	if applyErr != nil {
		applyErrSummary = applyErr.Error()
	}
	if err := r.reconcileStatus(ctx, cr, applyErrSummary); err != nil {
		// If status itself fails, prefer surfacing the apply error (it's
		// the root cause); fall back to the status error.
		if applyErr != nil {
			return ctrl.Result{}, applyErr
		}
		return ctrl.Result{}, fmt.Errorf("reconcile status: %w", err)
	}

	if applyErr != nil {
		return ctrl.Result{}, applyErr
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
// filter in main.go also restricts the Pod informer to the
// `tunnelport.giantswarm.io/role=tbot` label, narrowing the cache to
// pods this operator itself rendered.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&accessv1alpha1.RemoteApp{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ServiceAccount{}).
		// ADR 0008 removed the per-CR trust-bundle Secret/Role/
		// RoleBinding from the owned-object set; consumer trust-bundle
		// distribution is the chart-managed singleton bot's job.
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.mapPodToRemoteApp),
		)

	// The TLS verifier's channel, when wired. A certificate whose SANs
	// stopped matching produces no Kubernetes event of any kind — no pod
	// restart, no object change — so without this source the
	// TunnelVerified condition would only refresh when something
	// unrelated happened to trigger a reconcile.
	if r.VerificationEvents != nil {
		b = b.WatchesRawSource(source.Channel(
			r.VerificationEvents,
			&handler.TypedEnqueueRequestForObject[*accessv1alpha1.RemoteApp]{},
		))
	}

	return b.Named("remoteapp").Complete(r)
}

// mapPodToRemoteApp routes a Pod event to the RemoteApp whose name lives
// in the canonical LabelRemoteAppInstance label. We do not look up the
// owning ReplicaSet/Deployment chain — the label is the stable, slice-2
// contract on every rendered pod template.
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
func (r *Reconciler) reconcileStatus(ctx context.Context, cr *accessv1alpha1.RemoteApp, applyErrSummary string) error {
	pods, err := r.listTbotPods(ctx, cr)
	if err != nil {
		return fmt.Errorf("list tbot pods: %w", err)
	}

	before := cr.Status.DeepCopy()
	newStatus := computeStatus(cr, pods, before.Conditions, applyErrSummary, r.Verifications)
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
