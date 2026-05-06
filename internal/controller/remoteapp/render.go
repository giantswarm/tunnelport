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

// Package remoteapp contains the controller-runtime reconciler that
// materialises a RemoteApp CR into a ConfigMap, Deployment, and Service,
// owned by the CR via OwnerReferences. The rendering is split into pure
// functions (renderConfigMap / renderDeployment / renderService) so they
// can be unit-tested without a live API server.
package remoteapp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// Canonical labels stamped on every owned object's metadata and on the
// Deployment's pod template. The chart README documents these as the stable
// selectors platform teams can target with NetworkPolicies.
const (
	LabelRole              = "tunnelport.giantswarm.io/role"
	LabelRoleValue         = "tbot"
	LabelRemoteAppInstance = "tunnelport.giantswarm.io/remoteapp"

	// AnnotationConfigHash is stamped on the pod template so that ConfigMap
	// content changes (spec.appName / spec.proxyAddr / spec.port) cause the
	// pod-template-hash to change, which makes the Deployment roll. Without
	// this, ConfigMap-only updates would not propagate until pods restart
	// for unrelated reasons.
	AnnotationConfigHash = "tunnelport.giantswarm.io/config-hash"

	// AnnotationTokenSecretVersion holds the tokenRef Secret's
	// resourceVersion observed at the most recent reconcile. A rotation
	// of the Secret bumps resourceVersion, which the reconciler stamps
	// here, which causes the pod-template-hash to change and the
	// Deployment to roll via its RollingUpdate strategy. The operator
	// reads only `metadata.resourceVersion` of the Secret — never
	// `Secret.Data`.
	AnnotationTokenSecretVersion = "tunnelport.giantswarm.io/token-secret-version"
)

// Config carries the operator-level knobs that are NOT on the RemoteApp CR
// but are needed to render owned objects: the tbot container image and its
// resource requests/limits. In production these are plumbed from Helm values
// (slice 6); for now the reconciler holds the resolved struct directly so
// slice 6 can wire it without a controller-shape change.
type Config struct {
	// TbotImage is the container image reference for the tbot sidecar pod.
	TbotImage string

	// Resources is the requests/limits applied to the tbot container.
	Resources corev1.ResourceRequirements
}

// renderScheme is a private scheme used by setOwnerRef so unit tests can
// stamp OwnerReferences without plumbing a *runtime.Scheme through every
// call. It registers the core/apps types plus our own RemoteApp.
var renderScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = accessv1alpha1.AddToScheme(s)
	return s
}()

// setOwnerRef stamps a controller-style OwnerReference from cr onto obj so
// that the API server's GC cascade-deletes the owned object when the CR is
// deleted. Both Controller and BlockOwnerDeletion are true — there is one
// reconciler responsible for these objects, and we want kubectl-driven
// cascade deletes to wait for owned-object cleanup.
func setOwnerRef(cr *accessv1alpha1.RemoteApp, obj metav1.Object) error {
	return controllerutil.SetControllerReference(cr, obj, renderScheme)
}

// canonicalLabels returns the labels stamped on owned objects and used as
// the Service selector / Deployment matchLabels. Keep this stable: changing
// it breaks Service selection on existing Deployments.
func canonicalLabels(cr *accessv1alpha1.RemoteApp) map[string]string {
	return map[string]string{
		LabelRole:              LabelRoleValue,
		LabelRemoteAppInstance: cr.Name,
	}
}

// renderConfigMap returns the ConfigMap holding tbot.yaml — tbot's
// application-tunnel configuration for this RemoteApp. The token Secret is
// referenced by name only; its contents are never read by the operator.
func renderConfigMap(cr *accessv1alpha1.RemoteApp, _ Config) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels:    canonicalLabels(cr),
		},
		Data: map[string]string{
			"tbot.yaml": tbotConfig(cr),
		},
	}
}

// Volume / mount names used by the rendered Deployment. Tests pin these as
// the public surface of the pod template.
const (
	volumeNameTbotConfig  = "tbot-config"
	volumeNameTbotToken   = "tbot-token"
	volumeNameTbotStorage = "tbot-storage"

	mountPathTbotConfig  = "/etc/tbot"
	mountPathTbotToken   = "/etc/tbot-token"
	mountPathTbotStorage = "/var/lib/tbot"

	// tbotDiagPort is tbot's diagnostics HTTP listener. tbot binds it on
	// 127.0.0.1:3001 by default and exposes /readyz which only returns 200
	// once the application-tunnel is established. Wiring k8s readiness to
	// this endpoint makes pod-Ready mean "tunnel-up" rather than just
	// "process-up", which is the contract status.ready relies on.
	//
	// HTTPGet vs exec: the diag endpoint binds to localhost only, but the
	// kubelet executes HTTP probes from the pod network namespace, so a
	// straight HTTPGet on the named container port works without needing
	// tbot to bind on 0.0.0.0. We pick HTTPGet over exec because no shell
	// is required in the tbot image.
	tbotDiagPort     int32 = 3001
	tbotDiagPortName       = "diag"
	tbotDiagReadyz         = "/readyz"
)

// renderDeployment returns the Deployment that runs tbot for this RemoteApp.
// Replicas defaults to 1 when spec.Replicas is nil. The strategy is
// RollingUpdate with maxSurge=1, maxUnavailable=0 — the new pod must become
// Ready before the old one is killed, so new connections see no downtime.
//
// The pod template mounts:
//   - the tbot config ConfigMap (read-only, name reference),
//   - the token Secret (read-only volume; the operator does NOT read its
//     contents — only references it by name per ADR equivalent),
//   - an emptyDir for tbot's renewable-cert destination directory (per
//     ADR 0002 — no PVC, no StatefulSet).
//
// Image and resources come from operator Config (Helm values via slice 6),
// not from the CR.
//
// tokenSecretVersion is stamped on the pod-template annotation
// `tunnelport.giantswarm.io/token-secret-version`. The reconciler reads
// it from `tokenRef`-Secret's `metadata.resourceVersion`; passing "" leaves
// the annotation present-but-empty so absence and a rotation-to-empty stay
// distinguishable in the pod-template diff. The argument is a separate
// parameter rather than a Config field because it changes per-reconcile,
// not per-operator-process.
//
// The container declares a readiness probe wired to tbot's diag /readyz
// (port "diag", 3001) so pod-Ready means tunnel-up — that's what
// status.ready mirrors.
func renderDeployment(cr *accessv1alpha1.RemoteApp, cfg Config, tokenSecretVersion string) *appsv1.Deployment {
	labels := canonicalLabels(cr)
	replicas := int32(1)
	if cr.Spec.Replicas != nil {
		replicas = *cr.Spec.Replicas
	}
	maxSurge := intstr.FromInt(1)
	maxUnavailable := intstr.FromInt(0)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       &maxSurge,
					MaxUnavailable: &maxUnavailable,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						// Hash of the rendered tbot config so a spec.appName
						// or spec.proxyAddr change — which only updates the
						// ConfigMap data, not the pod template directly —
						// still rolls the Deployment via pod-template-hash.
						AnnotationConfigHash: configHash(cr),
						// resourceVersion of the tokenRef Secret. Stamped
						// every reconcile; a rotation flips the value, the
						// pod-template-hash changes, and the Deployment
						// rolls via its existing RollingUpdate strategy.
						// Empty when the Secret hasn't been observed yet
						// — keeps the key present so the diff is unambiguous.
						AnnotationTokenSecretVersion: tokenSecretVersion,
					},
				},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: volumeNameTbotConfig,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cr.Name,
									},
								},
							},
						},
						{
							Name: volumeNameTbotToken,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									// Name reference only; the operator
									// never reads this Secret's contents.
									SecretName: cr.Spec.TokenRef.Name,
								},
							},
						},
						{
							Name: volumeNameTbotStorage,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:      "tbot",
							Image:     cfg.TbotImage,
							Resources: cfg.Resources,
							Args: []string{
								"start",
								"-c", mountPathTbotConfig + "/tbot.yaml",
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "tunnel",
									ContainerPort: cr.Spec.Port,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          tbotDiagPortName,
									ContainerPort: tbotDiagPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							// Readiness wired to tbot's diag /readyz. Pod
							// transitions to Ready only when the tunnel
							// is established — that's what status.ready
							// (slice 4) mirrors. Slice 5 is responsible
							// for any liveness probe; this slice owns
							// readiness only.
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: tbotDiagReadyz,
										Port: intstr.FromString(tbotDiagPortName),
									},
								},
								// Conservative defaults: tbot needs a few
								// seconds for the join + tunnel handshake
								// at startup. These can move to chart
								// values later if needed.
								InitialDelaySeconds: 2,
								PeriodSeconds:       5,
								TimeoutSeconds:      2,
								FailureThreshold:    3,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      volumeNameTbotConfig,
									MountPath: mountPathTbotConfig,
									ReadOnly:  true,
								},
								{
									Name:      volumeNameTbotToken,
									MountPath: mountPathTbotToken,
									ReadOnly:  true,
								},
								{
									Name:      volumeNameTbotStorage,
									MountPath: mountPathTbotStorage,
								},
							},
						},
					},
				},
			},
		},
	}
}

// renderService returns the ClusterIP Service that fronts this RemoteApp's
// tbot Deployment. Name = CR name, port = spec.port, selector matches the
// canonical pod-template labels. CONTEXT.md locks the type to ClusterIP.
func renderService(cr *accessv1alpha1.RemoteApp, _ Config) *corev1.Service {
	labels := canonicalLabels(cr)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "tbot",
					Port:       cr.Spec.Port,
					TargetPort: intstr.FromInt(int(cr.Spec.Port)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// configHash returns a stable, content-addressable hash of the tbot config
// so the pod template re-hashes whenever a CR field that lands in the
// ConfigMap changes. The hash is the digest of the rendered YAML, not of
// the Spec, so additions to the rendering (e.g. new tbot fields) trigger a
// roll iff they actually change the on-disk config.
func configHash(cr *accessv1alpha1.RemoteApp) string {
	sum := sha256.Sum256([]byte(tbotConfig(cr)))
	return hex.EncodeToString(sum[:])
}

// tbotConfig renders the tbot YAML config for an application-tunnel mode
// pod. Format mirrors the upstream tbot config schema: a top-level
// onboarding block with token join method, plus a services list with one
// application-tunnel entry. The token's value lives only in the mounted
// Secret — we reference it by file path inside the pod, not by value.
func tbotConfig(cr *accessv1alpha1.RemoteApp) string {
	// Path matches the volumeMount used in renderDeployment.
	tokenPath := fmt.Sprintf("/etc/tbot-token/%s", cr.Spec.TokenRef.Key)

	return fmt.Sprintf(`version: v2
auth_server: %[1]s
proxy_server: %[1]s
onboarding:
  join_method: token
  token: %[2]s
  token_secret_ref:
    name: %[3]s
    key: %[4]s
storage:
  type: directory
  path: /var/lib/tbot
services:
  - type: application-tunnel
    app_name: %[5]s
    listener: tcp://0.0.0.0:%[6]d
`,
		cr.Spec.ProxyAddr,
		tokenPath,
		cr.Spec.TokenRef.Name,
		cr.Spec.TokenRef.Key,
		cr.Spec.AppName,
		cr.Spec.Port,
	)
}
