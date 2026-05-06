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
// Slice 4 adds two read-only verb sets that the status loop requires:
//
//   - `pods` (get;list;watch): the lastError summary is derived from
//     pod Phase / ContainerStatuses / RestartCount / last termination
//     reason. We never request `pods/log`.
//   - `secrets` (get;list;watch): the TokenSecretBound condition needs
//     to verify both the Secret's existence *and* the named key. The
//     ideal would be the partial-metadata API (`metadata.k8s.io`), but
//     that omits `data` keys, so we have to read the Secret object to
//     check key presence. The reconciler must NOT pass the Secret's
//     value bytes to anything except that key-presence check — see
//     reconcileTokenSecretBound for the constraint.
//
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps,verbs=get;list;watch
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
package remoteapp

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

	// Status: derived from k8s-visible state only (ADR 0003). This must
	// run last so observedGeneration only catches up after the owned
	// objects above are applied successfully.
	if err := r.reconcileStatus(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile status: %w", err)
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
//
// Slice 4 also watches Pods labelled with the canonical
// LabelRemoteAppInstance: pod-level state (CrashLoopBackOff, restart
// count, last termination reason) is what populates status.lastError, so
// the reconciler must re-run on Pod events. Pods are owned by the
// rendered ReplicaSet (transitively the Deployment), so an `Owns` on
// Pod would not catch them — we use a Watches(...) with a
// label-driven mapping instead.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&accessv1alpha1.RemoteApp{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.mapPodToRemoteApp),
		).
		Named("remoteapp").
		Complete(r)
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

// reconcileStatus computes the RemoteApp's status from k8s-visible state
// and writes it via the Status subresource. ADR 0003 forbids reading
// pod logs; this method touches Pod metadata only.
//
// Order:
//  1. Compute TokenSecretBound from Secret + key existence.
//  2. List the RemoteApp's tbot pods and derive (ready, lastError).
//  3. Set Ready and TokenSecretBound conditions, observedGeneration,
//     and the top-level Ready / LastError shorthand fields.
//  4. r.Status().Update — sub-resource client to avoid spec/status
//     conflicts.
func (r *Reconciler) reconcileStatus(ctx context.Context, cr *accessv1alpha1.RemoteApp) error {
	// Snapshot the pre-update status so we can no-op when nothing changed
	// (avoids a hot-loop reconcile when a Pod event lands but state is
	// already accurate).
	before := cr.Status.DeepCopy()

	// 1. TokenSecretBound: read the named Secret and check the key.
	tokenBound, tokenReason, tokenMsg := r.evalTokenSecretBound(ctx, cr)

	// 2. Pod state.
	pods, err := r.listTbotPods(ctx, cr)
	if err != nil {
		return fmt.Errorf("list tbot pods: %w", err)
	}
	ready, lastError := summarizeStatus(pods)

	// 3. Build the new status.
	newStatus := cr.Status.DeepCopy()
	newStatus.Ready = ready
	newStatus.LastError = lastError
	newStatus.ObservedGeneration = cr.Generation

	readyCond := metav1.Condition{
		Type:               accessv1alpha1.ConditionTypeReady,
		Status:             boolToConditionStatus(ready),
		ObservedGeneration: cr.Generation,
		Reason:             readyConditionReason(ready, lastError),
		Message:            lastError,
	}
	if ready {
		readyCond.Message = "tbot tunnel ready"
	}
	meta.SetStatusCondition(&newStatus.Conditions, readyCond)

	tokenCond := metav1.Condition{
		Type:               accessv1alpha1.ConditionTypeTokenSecretBound,
		Status:             boolToConditionStatus(tokenBound),
		ObservedGeneration: cr.Generation,
		Reason:             tokenReason,
		Message:            tokenMsg,
	}
	meta.SetStatusCondition(&newStatus.Conditions, tokenCond)

	if statusEqual(before, newStatus) {
		return nil
	}

	cr.Status = *newStatus
	return r.Status().Update(ctx, cr)
}

// evalTokenSecretBound checks the referenced Secret and the named key.
// We never log or pass through the Secret's bytes — only key presence
// matters. Returns (bound, reason, human-readable message).
func (r *Reconciler) evalTokenSecretBound(ctx context.Context, cr *accessv1alpha1.RemoteApp) (bool, string, string) {
	s := &corev1.Secret{}
	key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.TokenRef.Name}
	if err := r.Get(ctx, key, s); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "SecretNotFound",
				fmt.Sprintf("Secret %q not found in namespace %q", cr.Spec.TokenRef.Name, cr.Namespace)
		}
		return false, "SecretGetError", err.Error()
	}
	if _, ok := s.Data[cr.Spec.TokenRef.Key]; !ok {
		return false, "KeyNotFound",
			fmt.Sprintf("Secret %q has no key %q", cr.Spec.TokenRef.Name, cr.Spec.TokenRef.Key)
	}
	return true, "Bound", fmt.Sprintf("Secret %q key %q present", cr.Spec.TokenRef.Name, cr.Spec.TokenRef.Key)
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

// statusEqual is a structural equality check that ignores LastTransitionTime
// on conditions (which would otherwise force a write on every reconcile).
// meta.SetStatusCondition already preserves LastTransitionTime when only
// non-time fields change; this guard avoids the corner case where every
// reconcile bumps it.
func statusEqual(a, b *accessv1alpha1.RemoteAppStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Ready != b.Ready ||
		a.LastError != b.LastError ||
		a.ObservedGeneration != b.ObservedGeneration ||
		len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		ac, bc := a.Conditions[i], b.Conditions[i]
		if ac.Type != bc.Type ||
			ac.Status != bc.Status ||
			ac.Reason != bc.Reason ||
			ac.Message != bc.Message ||
			ac.ObservedGeneration != bc.ObservedGeneration {
			return false
		}
	}
	return true
}

func boolToConditionStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// readyConditionReason picks a stable Reason string for the Ready
// condition. Reasons are not free-form per Kubernetes conventions —
// stick to a small finite set that automation can pattern-match on.
func readyConditionReason(ready bool, lastError string) string {
	if ready {
		return "TunnelReady"
	}
	if lastError == noTbotPodsMsg {
		return "NoPods"
	}
	return "PodNotReady"
}
