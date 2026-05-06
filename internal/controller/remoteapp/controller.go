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
//   - `secrets` (get;list;watch): the operator references the token
//     Secret by name and reads its `metadata.resourceVersion` to stamp
//     the pod-template annotation `…/token-secret-version` (so token
//     rotations roll the Deployment), and verifies the named key for
//     the `TokenSecretBound` status condition. The ideal would be the
//     partial-metadata API (`metadata.k8s.io`), but that omits `data`
//     keys, so we have to read the Secret object to check key presence.
//     The reconciler never accesses `Secret.Data` values; an AST-based
//     static check in `secret_watch_test.go` enforces that invariant.
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
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	if err := r.applyOwned(ctx, cr, renderConfigMap(cr, r.Config), &corev1.ConfigMap{}, mergeConfigMap); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ConfigMap: %w", err)
	}
	view := r.observeTokenSecret(ctx, cr)
	if view.FetchErr != nil {
		return ctrl.Result{}, fmt.Errorf("observe token Secret: %w", view.FetchErr)
	}
	if err := r.applyOwned(ctx, cr, renderDeployment(cr, r.Config, view.ResourceVersion), &appsv1.Deployment{}, mergeDeployment); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Deployment: %w", err)
	}
	if err := r.applyOwned(ctx, cr, renderService(cr, r.Config), &corev1.Service{}, mergeService); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Service: %w", err)
	}

	// Status: derived from k8s-visible state only (ADR 0003). This must
	// run last so observedGeneration only catches up after the owned
	// objects above are applied successfully.
	if err := r.reconcileStatus(ctx, cr, view); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile status: %w", err)
	}

	logger.V(1).Info("reconciled")
	return ctrl.Result{}, nil
}

// applyOwned is the create-or-update scaffold shared by every owned-object
// reconcile. The merge closure is the seam: each owned type expresses
// which fields it controls (and, by omission, which the API server or
// child controllers own). Server-Side Apply is the deeper alternative,
// but this client-side merge keeps existing test contracts intact.
func (r *Reconciler) applyOwned(
	ctx context.Context,
	cr *accessv1alpha1.RemoteApp,
	desired client.Object,
	existing client.Object,
	merge func(existing, desired client.Object),
) error {
	if err := setOwnerRef(cr, desired); err != nil {
		return fmt.Errorf("set owner ref: %w", err)
	}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	merge(existing, desired)
	return r.Update(ctx, existing)
}

// mergeConfigMap copies the rendered fields onto the existing ConfigMap.
// OwnerReferences is rewritten so the controller-style ref is stable.
func mergeConfigMap(existing, desired client.Object) {
	e := existing.(*corev1.ConfigMap)
	d := desired.(*corev1.ConfigMap)
	e.Labels = d.Labels
	e.Data = d.Data
	e.OwnerReferences = d.OwnerReferences
}

// mergeDeployment copies the rendered fields onto the existing Deployment.
// Status and pod-template defaults are left to the Deployment controller.
func mergeDeployment(existing, desired client.Object) {
	e := existing.(*appsv1.Deployment)
	d := desired.(*appsv1.Deployment)
	e.Labels = d.Labels
	e.OwnerReferences = d.OwnerReferences
	e.Spec.Replicas = d.Spec.Replicas
	e.Spec.Selector = d.Spec.Selector
	e.Spec.Strategy = d.Spec.Strategy
	e.Spec.Template = d.Spec.Template
}

// mergeService preserves ClusterIP (allocated by the API server on first
// create — touching it raises "field is immutable" on update).
func mergeService(existing, desired client.Object) {
	e := existing.(*corev1.Service)
	d := desired.(*corev1.Service)
	e.Labels = d.Labels
	e.OwnerReferences = d.OwnerReferences
	e.Spec.Type = d.Spec.Type
	e.Spec.Selector = d.Spec.Selector
	e.Spec.Ports = d.Spec.Ports
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
//
// Watches(&corev1.Pod{}, ...) routes pod-level events to the owning
// RemoteApp via the canonical LabelRemoteAppInstance label. Pod state
// (CrashLoopBackOff, restart count, last termination reason) is what
// populates status.lastError, so the reconciler must re-run on Pod
// events. Pods are owned by the rendered ReplicaSet (transitively the
// Deployment), so an `Owns` on Pod would not catch them — the
// label-driven mapping is the stable seam.
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
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.mapPodToRemoteApp),
		).
		Named("remoteapp").
		Complete(r)
}

// TokenSecretView is the operator's narrow view of the tokenRef Secret.
// Built once per reconcile from a single Get; carries everything the
// reconciler and computeStatus need without re-fetching. Only ObjectMeta
// and the named-key presence are observed — the package-level static
// check in secret_watch_test.go enforces that no controller code reads
// other Secret bytes.
type TokenSecretView struct {
	Name            string
	Key             string
	ResourceVersion string // empty when the Secret was not found
	KeyExists       bool   // false if Secret missing OR key absent
	FetchErr        error  // non-nil only on non-NotFound errors
}

// observeTokenSecret performs the one Secret Get per reconcile. NotFound
// is normalised to (KeyExists=false, ResourceVersion="") to match the
// GitOps-race semantics — the rendered pod stays Pending on the volume
// mount until the Secret appears, and TokenSecretBound surfaces the
// absence in status. Receiver name `s` deliberately stays outside the
// banned-receiver set in TestController_NoSecretDataAccess.
func (r *Reconciler) observeTokenSecret(ctx context.Context, cr *accessv1alpha1.RemoteApp) TokenSecretView {
	view := TokenSecretView{
		Name: cr.Spec.TokenRef.Name,
		Key:  cr.Spec.TokenRef.Key,
	}
	s := &corev1.Secret{}
	key := client.ObjectKey{Namespace: cr.Namespace, Name: cr.Spec.TokenRef.Name}
	if err := r.Get(ctx, key, s); err != nil {
		if apierrors.IsNotFound(err) {
			return view
		}
		view.FetchErr = fmt.Errorf("get token Secret %s/%s: %w", key.Namespace, key.Name, err)
		return view
	}
	view.ResourceVersion = s.ResourceVersion
	_, view.KeyExists = s.Data[cr.Spec.TokenRef.Key]
	return view
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
func (r *Reconciler) mapSecretToRemoteApps(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)

	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	var apps accessv1alpha1.RemoteAppList
	if err := r.List(ctx, &apps, client.InNamespace(sec.Namespace)); err != nil {
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
func (r *Reconciler) reconcileStatus(ctx context.Context, cr *accessv1alpha1.RemoteApp, view TokenSecretView) error {
	pods, err := r.listTbotPods(ctx, cr)
	if err != nil {
		return fmt.Errorf("list tbot pods: %w", err)
	}

	before := cr.Status.DeepCopy()
	newStatus := computeStatus(cr, pods, view, before.Conditions)
	if statusEqual(before, &newStatus) {
		return nil
	}

	cr.Status = newStatus
	return r.Status().Update(ctx, cr)
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
