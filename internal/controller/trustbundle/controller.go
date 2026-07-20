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

// Package trustbundle contains the controller-runtime reconciler that
// rolls trust-bundle *consumer* Deployments when the SPIFFE trust bundle's
// CA set changes.
//
// Background: the chart-managed singleton trust-bundle tbot (ADR 0008)
// writes the SPIFFE trust-domain bundle into one Kubernetes Secret
// (`trustBundle.secretName`, default `tunnelport-spiffe-bundle`) in the
// operator's install namespace. Consumers co-deployed in that namespace
// (Dex, muster, ...) mount the Secret and use `svid_bundle.pem` as the CA
// bundle for verifying tunnel SVIDs. The Secret is read live by mounts,
// but processes that build an in-memory CA pool at startup (Dex's Teleport
// OIDC connector reads `rootCAs` once at connector open) keep a stale pool
// when Teleport rotates its CA while the process runs. This controller
// supplies the missing restart trigger: a content-addressed annotation
// stamped onto each consumer's pod template, so a CA-set change rolls the
// consumer the same way `tunnelport.giantswarm.io/config-hash` rolls tbot.
//
// The trust-bundle tbot rewrites the Secret every renewal cycle (~20m),
// but the bundle *content* only changes on an actual CA rotation. Hashing
// the `svid_bundle.pem` content (not the Secret's resourceVersion) and
// de-duping against the consumer's existing annotation is what keeps a
// plain renewal from triggering a restart storm.
//
// Watch scope: the controller watches only the trust-bundle Secret, not
// the consumer Deployments. A freshly created (or recreated) consumer
// already mounts the live bundle at startup, so it never has a stale
// in-memory CA pool to correct — the annotation is purely an in-flight
// *roll* trigger. A new consumer that happens to come up between Secret
// writes is therefore harmless until the next Secret event stamps it
// (which any renewal Update delivers, since its empty annotation differs
// from the current hash). Watching Deployments too would buy nothing but
// a larger informer, so it is deliberately omitted.
//
// RBAC. The reconciler reads the trust-bundle Secret and patches consumer
// Deployments' pod-template annotation. It never writes the Secret and
// never reads any other Secret's data. The secrets marker below drives the
// generated config/rbac/role.yaml (kustomize/dev path); the production Helm
// chart binds this read as a namespace-scoped Role in the install namespace
// (`<release>-trust-bundle-reader`), not a cluster-wide grant — keep it that
// way, the reloader only ever reads one Secret in one namespace.
//
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch
package trustbundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	// fieldManager is the Server-Side Apply field-manager identity this
	// controller writes the consumer pod-template annotation under. It is
	// deliberately distinct from the remoteapp controller's manager so the
	// two never contend for field ownership: this controller owns *only*
	// the trust-bundle-hash annotation on consumer Deployments it did not
	// render. chart-operator's own 3-way merge (and any other manager's
	// fields) are preserved — SSA only claims the single annotation we
	// write here.
	fieldManager = "trust-bundle-reloader"

	// AnnotationTrustBundleHash is stamped on a consumer Deployment's pod
	// template. Its value is the SHA-256 of the trust bundle's
	// `svid_bundle.pem`. Changing it changes the pod-template-hash, which
	// makes the Deployment roll — the same mechanism the config-hash
	// annotation uses for tbot pods.
	AnnotationTrustBundleHash = "tunnelport.giantswarm.io/trust-bundle-hash"

	// LabelTrustBundleConsumer selects the Deployments this controller
	// rolls. A consumer opts in by carrying this label set to "true"
	// either on the Deployment's own metadata or on its pod template
	// (charts commonly expose only a `podLabels` knob — e.g. the dex
	// subchart — so we accept the label in either place rather than
	// forcing consumers to set it on Deployment metadata).
	LabelTrustBundleConsumer = "tunnelport.giantswarm.io/trust-bundle-consumer"

	// labelTrustBundleConsumerValue is the only value that opts a
	// Deployment in. Anything else (including absence) is ignored.
	labelTrustBundleConsumerValue = "true"

	// bundleKey is the Secret data key whose content is the CA bundle.
	// The singleton trust-bundle tbot writes the SPIFFE trust-domain
	// bundle here.
	bundleKey = "svid_bundle.pem"
)

// Reconciler rolls trust-bundle consumer Deployments when the CA set in the
// trust-bundle Secret changes. It watches exactly one Secret (SecretName in
// Namespace) and patches Deployments in Namespace that opt in via
// LabelTrustBundleConsumer.
type Reconciler struct {
	client.Client

	// SecretName is the trust-bundle Secret the singleton tbot writes
	// (chart value `trustBundle.secretName`).
	SecretName string

	// Namespace is the operator's install namespace — where the
	// trust-bundle Secret and its consumers live (chart value
	// `installNamespace`). Both the Secret watch and the consumer
	// Deployment selection are scoped to it; the controller never
	// touches objects in any other namespace.
	Namespace string

	// Recorder emits a Kubernetes Event against a consumer Deployment when
	// a CA-set change rolls it, so the roll is visible via `kubectl
	// describe`/`kubectl get events` without log-diving. Optional: a nil
	// Recorder disables event emission (used by unit tests that do not
	// assert on events).
	Recorder events.EventRecorder
}

// Reconcile reads the trust-bundle Secret, hashes its `svid_bundle.pem`
// content, and stamps that hash onto every opted-in consumer Deployment's
// pod template whose current annotation differs. De-duping on the content
// hash means a plain ~20m tbot renewal (same CA set, rewritten Secret) is a
// no-op, while an actual CA rotation rolls each consumer exactly once.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("trustbundle", req.NamespacedName)

	// The watch predicate already pins this to our one Secret, but guard
	// anyway so a stray enqueue can't act on the wrong object.
	if req.Namespace != r.Namespace || req.Name != r.SecretName {
		return ctrl.Result{}, nil
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, req.NamespacedName, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// No bundle yet (or it was deleted): nothing to roll toward.
			// Consumers keep their current annotation; when the Secret
			// reappears we reconcile again.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get trust-bundle secret: %w", err)
	}

	bundle := secret.Data[bundleKey]
	if len(bundle) == 0 {
		// A Secret without the bundle key is not a CA set we can hash;
		// stamping an empty-bundle hash would be a meaningless roll.
		logger.V(1).Info("trust-bundle secret has no bundle key; skipping", "key", bundleKey)
		return ctrl.Result{}, nil
	}
	hash := bundleHash(bundle)

	deployments := &appsv1.DeploymentList{}
	if err := r.List(ctx, deployments, client.InNamespace(r.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list deployments: %w", err)
	}

	var consumers, rolled int
	for i := range deployments.Items {
		dep := &deployments.Items[i]
		if !isConsumer(dep) {
			continue
		}
		consumers++
		if currentTrustBundleHash(dep) == hash {
			continue // CA set unchanged for this consumer — no roll.
		}
		if err := r.stampConsumer(ctx, dep.Namespace, dep.Name, hash); err != nil {
			return ctrl.Result{}, fmt.Errorf("roll consumer %s/%s: %w", dep.Namespace, dep.Name, err)
		}
		rolled++
		logger.Info("rolled trust-bundle consumer on CA-set change",
			"deployment", dep.Name, "hash", hash)
		if r.Recorder != nil {
			r.Recorder.Eventf(dep, nil, corev1.EventTypeNormal, "TrustBundleRolled", "Roll",
				"rolled on trust-bundle CA-set change; svid_bundle.pem hash %s", hash)
		}
	}

	if rolled > 0 {
		logger.Info("trust-bundle reconcile complete", "consumers", consumers, "rolled", rolled)
	} else {
		logger.V(1).Info("trust-bundle reconcile complete; no CA-set change", "consumers", consumers)
	}
	return ctrl.Result{}, nil
}

// isConsumer reports whether a Deployment opted into trust-bundle rolls by
// carrying LabelTrustBundleConsumer="true" on its own metadata or on its
// pod template. Accepting both placements lets charts that expose only a
// `podLabels` knob participate without a Deployment-metadata override.
func isConsumer(dep *appsv1.Deployment) bool {
	return dep.Labels[LabelTrustBundleConsumer] == labelTrustBundleConsumerValue ||
		dep.Spec.Template.Labels[LabelTrustBundleConsumer] == labelTrustBundleConsumerValue
}

// stampConsumer Server-Side-Applies the trust-bundle-hash annotation onto a
// consumer Deployment's pod template. The apply payload carries only the
// annotation, so SSA claims ownership of that single field and leaves every
// field owned by the consumer's own deploying manager (chart-operator, Flux,
// ...) untouched. ForceOwnership migrates the annotation back to us if a
// previous manager owned it.
func (r *Reconciler) stampConsumer(ctx context.Context, namespace, name, hash string) error {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("apps/v1")
	u.SetKind("Deployment")
	u.SetNamespace(namespace)
	u.SetName(name)
	if err := unstructured.SetNestedStringMap(u.Object,
		map[string]string{AnnotationTrustBundleHash: hash},
		"spec", "template", "metadata", "annotations",
	); err != nil {
		return fmt.Errorf("build apply payload: %w", err)
	}
	ac := client.ApplyConfigurationFromUnstructured(u)
	return r.Apply(ctx, ac,
		client.FieldOwner(fieldManager),
		client.ForceOwnership,
	)
}

// SetupWithManager wires the reconciler to the single trust-bundle Secret.
// The predicate restricts the watch to that one Secret so the manager's
// Secret informer never fans out to unrelated Secret events; main.go also
// scopes the Secret cache to this namespace+name for the same reason.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	target := types.NamespacedName{Namespace: r.Namespace, Name: r.SecretName}
	onlyTarget := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == target.Namespace && obj.GetName() == target.Name
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}, builder.WithPredicates(onlyTarget)).
		Named("trustbundle").
		Complete(r)
}

// currentTrustBundleHash returns the trust-bundle-hash annotation already on
// a Deployment's pod template, or "" if absent. Comparing it to the freshly
// computed hash is the de-dupe that prevents re-rolling on a plain renewal.
func currentTrustBundleHash(dep *appsv1.Deployment) string {
	return dep.Spec.Template.Annotations[AnnotationTrustBundleHash]
}

// bundleHash returns the hex SHA-256 of the CA bundle content. Content-
// addressed (not resourceVersion-addressed) so a Secret rewrite that does
// not change the CA set produces the same hash and therefore no roll.
func bundleHash(bundle []byte) string {
	sum := sha256.Sum256(bundle)
	return hex.EncodeToString(sum[:])
}
