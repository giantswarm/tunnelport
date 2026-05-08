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
//     The reconciler never accesses `Secret.Data` values; the typed
//     `tokenSecretMeta` accessor (with no `Data` field) is the only
//     shape in which Secret-derived data leaves `observeTokenSecret`,
//     and `TestController_TypedSecretAccessor` in secret_watch_test.go
//     pins that invariant.
//
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps,verbs=get;list;watch
// +kubebuilder:rbac:groups=access.giantswarm.io,resources=remoteapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// IndexFieldTokenRefName is the field-indexer key for `RemoteApp` lookups
// keyed on `spec.tokenRef.name`. Registered in `SetupWithManager`; used by
// `mapSecretToRemoteApps` to drop the per-event O(N) namespace scan to
// O(matches). Exposed as a constant so the indexer key isn't duplicated as
// a string literal in two places.
const IndexFieldTokenRefName = "spec.tokenRef.name"

// fieldManager is the Server-Side Apply field-manager identity this
// controller writes under. Stable across releases: changing it would
// orphan the field-ownership records the API server keeps, causing one
// reconcile after the change to look like first-time ownership for every
// owned field. Other field managers (admission mutators injecting sidecars,
// e.g. service-mesh webhooks) keep their own ownership of the fields they
// write — `ForceOwnership` only takes back fields we *also* write.
const fieldManager = "remoteapp-controller"

// LabelRoleValueTokenSecret is the label value platform engineers must
// stamp on token Secrets for the operator's informer cache to subscribe to
// them. It is also the value the controller's Secret watch predicate
// expects — the cache filter and the predicate together enforce the same
// invariant from two layers (cache.Options.ByObject in cmd/main.go does
// the work; the predicate is defence-in-depth on top of the per-CR
// `spec.tokenRef.name` index).
//
// CRITICAL: the operator does NOT set this label itself — that would be a
// write to a user-managed Secret, which violates the "operator never
// mutates token Secrets" rule documented in CONTEXT.md (Token Secret
// delivery) and the Secret-data invariant pinned by
// `TestController_TypedSecretAccessor` in secret_watch_test.go.
const LabelRoleValueTokenSecret = "token-secret"

// Reconciler renders a ConfigMap, Deployment, and Service in the CR's
// namespace, owned by the RemoteApp via OwnerReferences. It also watches
// the tokenRef Secret and stamps the Secret's resourceVersion onto the
// pod-template annotation `tunnelport.giantswarm.io/token-secret-version`
// so token rotations roll the Deployment via the existing RollingUpdate
// strategy (slice 5). It does NOT populate status (slice 4).
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

// Reconcile renders the three owned objects from the RemoteApp's spec and
// applies them via Server-Side Apply. Spec mutations re-render and are
// propagated on the next reconcile pass.
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

	// ServiceAccount goes first so the Deployment that references it via
	// `spec.template.spec.serviceAccountName` always finds it during pod
	// admission. ADR 0006 slice 1: the SA is the join identity tbot will
	// present once slice 02 flips `join_method` to `kubernetes`.
	if err := r.applyOwned(ctx, cr, renderServiceAccount(cr, r.PodDefaults)); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ServiceAccount: %w", err)
	}
	if err := r.applyOwned(ctx, cr, renderConfigMap(cr, r.PodDefaults)); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ConfigMap: %w", err)
	}
	view := r.observeTokenSecret(ctx, cr)
	if view.FetchErr != nil {
		return ctrl.Result{}, fmt.Errorf("observe token Secret: %w", view.FetchErr)
	}
	if err := r.applyOwned(ctx, cr, renderDeployment(cr, r.PodDefaults, view.ResourceVersion)); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Deployment: %w", err)
	}
	if err := r.applyOwned(ctx, cr, renderService(cr, r.PodDefaults)); err != nil {
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

// SetupWithManager wires this Reconciler to its CR type and the three
// owned object types. Owns(...) gives us watches with predictable
// requeue-on-child-change semantics.
//
// Watch contract for token Secrets: platform engineers MUST label their
// token Secrets with `tunnelport.giantswarm.io/role=token-secret` for the
// operator to observe them. The manager-level `cache.Options.ByObject`
// filter in cmd/main.go scopes the Secret informer to that label
// selector, so unlabelled Secrets never enter the operator's address
// space. The operator never reads `Secret.Data` (structurally enforced
// by the typed `tokenSecretMeta` accessor and pinned by
// `TestController_TypedSecretAccessor`); the manager-cache filter is the
// belt and the per-event predicate below is the braces.
//
// Watches(&corev1.Secret{}, ...) extends the watch surface to token
// Secrets: a Secret create/update/delete fans out via
// mapSecretToRemoteApps to only the RemoteApps that actually reference
// the Secret by `spec.tokenRef.name`. The mapper uses the
// `IndexFieldTokenRefName` field index registered below, so the lookup
// is O(matches) instead of an O(N) namespace scan. The
// `predicate.NewPredicateFuncs(...)` filter additionally drops events
// whose Secret namespace+name doesn't match any RemoteApp's `tokenRef`
// — defence-in-depth on top of the cache-level label filter, in case a
// labelled Secret slips through that isn't actually referenced.
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
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&accessv1alpha1.RemoteApp{},
		IndexFieldTokenRefName,
		func(obj client.Object) []string {
			cr, ok := obj.(*accessv1alpha1.RemoteApp)
			if !ok || cr.Spec.TokenRef.Name == "" {
				return nil
			}
			return []string{cr.Spec.TokenRef.Name}
		},
	); err != nil {
		return fmt.Errorf("index RemoteApp.spec.tokenRef.name: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&accessv1alpha1.RemoteApp{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ServiceAccount{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToRemoteApps),
			builder.WithPredicates(predicate.NewPredicateFuncs(r.secretIsReferenced)),
		).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.mapPodToRemoteApp),
		).
		Named("remoteapp").
		Complete(r)
}

// secretIsReferenced returns true iff some RemoteApp in the Secret's
// namespace references it by `spec.tokenRef.name`. Used as a watch
// predicate: a labelled-but-unreferenced Secret (e.g. a stale token
// Secret left over after its RemoteApp was deleted) still passes the
// cache.Options label filter, but this predicate drops the event before
// it hits the workqueue. Lookup uses the field index registered in
// SetupWithManager, so the cost is O(matches), not O(N).
func (r *Reconciler) secretIsReferenced(obj client.Object) bool {
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return false
	}
	var apps accessv1alpha1.RemoteAppList
	if err := r.List(
		context.Background(), &apps,
		client.InNamespace(sec.Namespace),
		client.MatchingFields{IndexFieldTokenRefName: sec.Name},
	); err != nil {
		// On a cache miss / lookup error we err on the safe side and let
		// the event through — mapSecretToRemoteApps will re-do the same
		// lookup and produce an empty fan-out if nothing actually
		// references it. Better one extra workqueue hop than a silently
		// dropped rotation.
		return true
	}
	return len(apps.Items) > 0
}

// tokenSecretMeta is the operator's narrow, typed view of the tokenRef
// Secret. It is the ONLY shape in which Secret-derived data crosses out of
// observeTokenSecret: by construction it has no Data field, so no caller
// downstream can reach Secret.Data even by accident. Built once per
// reconcile from a single Get; carries everything the reconciler and
// computeStatus need without re-fetching.
//
// Field set is deliberately small. Adding a field that exposes Secret
// bytes (Data, StringData, byte slices keyed off Data) would defeat the
// invariant pinned by TestController_TypedSecretAccessor in
// secret_watch_test.go.
type tokenSecretMeta struct {
	Name            string
	Key             string
	ResourceVersion string // empty when the Secret was not found
	KeyExists       bool   // false if Secret missing OR key absent
	FetchErr        error  // non-nil only on non-NotFound errors
}

// TokenSecretView is the historical name for tokenSecretMeta, retained as
// a type alias so existing call sites (status.go, status_test.go) compile
// unchanged. New code should prefer tokenSecretMeta.
type TokenSecretView = tokenSecretMeta

// observeTokenSecret performs the one Secret Get per reconcile and
// projects the result into a tokenSecretMeta. The fetched *corev1.Secret
// is scoped to this function body and dropped on return — no caller ever
// holds a Secret pointer, so Secret.Data is structurally unreachable from
// the rest of the reconcile path.
//
// NotFound is normalised to (KeyExists=false, ResourceVersion="") to
// match the GitOps-race semantics — the rendered pod stays Pending on
// the volume mount until the Secret appears, and TokenSecretBound
// surfaces the absence in status.
func (r *Reconciler) observeTokenSecret(ctx context.Context, cr *accessv1alpha1.RemoteApp) tokenSecretMeta {
	meta := tokenSecretMeta{
		Name: cr.Spec.TokenRef.Name,
		Key:  cr.Spec.TokenRef.Key,
	}
	s := &corev1.Secret{}
	key := client.ObjectKey{Namespace: cr.Namespace, Name: cr.Spec.TokenRef.Name}
	if err := r.Get(ctx, key, s); err != nil {
		if apierrors.IsNotFound(err) {
			return meta
		}
		meta.FetchErr = fmt.Errorf("get token Secret %s/%s: %w", key.Namespace, key.Name, err)
		return meta
	}
	meta.ResourceVersion = s.ResourceVersion
	_, meta.KeyExists = s.Data[cr.Spec.TokenRef.Key]
	// s goes out of scope here; the *corev1.Secret pointer does not
	// escape this function. Everything else in the reconcile path sees
	// only the typed tokenSecretMeta value above.
	return meta
}

// mapSecretToRemoteApps fans a Secret event out to the RemoteApps that
// reference it via `spec.tokenRef.name`. It lists RemoteApps in the
// Secret's namespace only — `tokenRef` is namespace-local by design
// (CONTEXT.md: "no cross-namespace references"), so a Secret in ns A can
// never trigger reconciles for a RemoteApp in ns B even if both names
// match.
//
// The list uses the `IndexFieldTokenRefName` field index registered in
// SetupWithManager, so the cost is O(matches) on the cache rather than
// the O(N) namespace scan a name-equality filter would do.
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
	if err := r.List(
		ctx, &apps,
		client.InNamespace(sec.Namespace),
		client.MatchingFields{IndexFieldTokenRefName: sec.Name},
	); err != nil {
		logger.Error(err, "list RemoteApps for Secret fan-out", "secret", sec.Namespace+"/"+sec.Name)
		return nil
	}

	out := make([]reconcile.Request, 0, len(apps.Items))
	for i := range apps.Items {
		app := &apps.Items[i]
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
func (r *Reconciler) reconcileStatus(ctx context.Context, cr *accessv1alpha1.RemoteApp, view tokenSecretMeta) error {
	pods, err := r.listTbotPods(ctx, cr)
	if err != nil {
		return fmt.Errorf("list tbot pods: %w", err)
	}

	before := cr.Status.DeepCopy()
	newStatus := computeStatus(cr, pods, view, before.Conditions)
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
