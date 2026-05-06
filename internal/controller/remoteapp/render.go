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

// PodDefaults carries the operator-level knobs that are NOT on the RemoteApp
// CR but are needed to render owned objects: the tbot container image and
// its resource requests/limits, plus the dev-only `insecure` flag. In
// production these are plumbed from Helm values; the reconciler holds the
// resolved struct directly so cmd/main.go can wire it without a
// controller-shape change.
type PodDefaults struct {
	// TbotImage is the container image reference for the tbot sidecar pod.
	TbotImage string

	// Resources is the requests/limits applied to the tbot container.
	Resources corev1.ResourceRequirements

	// Insecure makes every rendered tbot pod skip Teleport proxy TLS
	// verification. Intended for development environments where the
	// proxy serves a cert whose SAN does not match the address tbot
	// connects to (e.g. kind-based smoke tests reaching the proxy by
	// IP). Adds `insecure: true` to the rendered tbot config. Never
	// set this in production.
	Insecure bool
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
//
// TypeMeta is set explicitly because the apply path uses Server-Side Apply
// (`client.Apply`), which JSON-marshals the object directly and rejects
// payloads without `apiVersion`/`kind`. Pure-render unit tests don't depend
// on TypeMeta, but the apply path does.
func renderConfigMap(cr *accessv1alpha1.RemoteApp, cfg PodDefaults) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels:    canonicalLabels(cr),
		},
		Data: map[string]string{
			"tbot.yaml": tbotConfig(cr, cfg),
		},
	}
}

// Volume / mount names used by the rendered Deployment. Tests pin these as
// the public surface of the pod template.
const (
	volumeNameTbotConfig  = "tbot-config"
	volumeNameTbotToken   = "tbot-token"
	volumeNameTbotStorage = "tbot-storage"
	// volumeNameTbotTmp backs /tmp because the container runs with
	// readOnlyRootFilesystem: true. tbot and its transitive deps may write
	// scratch files (Go runtime, glibc resolver caches, etc.); keeping /tmp
	// writable via emptyDir avoids surprise EROFS without relaxing the
	// root-fs hardening.
	volumeNameTbotTmp = "tbot-tmp"

	mountPathTbotConfig  = "/etc/tbot"
	mountPathTbotToken   = "/etc/tbot-token"
	mountPathTbotStorage = "/var/lib/tbot"
	mountPathTbotTmp     = "/tmp"

	// tbotDiagPort is tbot's diagnostics HTTP listener. We render
	// `diag_addr: 0.0.0.0:tbotDiagPort` in tbot.yaml (see
	// tbot_config.go) so the listener is reachable on the pod IP.
	// kubelet HTTP probes are issued from the kubelet process on the
	// node — not from inside the pod netns — so a localhost-only
	// binding is unreachable to the probe; switching to an exec probe
	// is not viable because the tbot image is distroless and ships no
	// shell.
	//
	// /readyz returns 200 only once the application-tunnel is
	// established, so wiring pod readiness to it makes pod-Ready mean
	// "tunnel-up" rather than just "process-up" — the contract
	// `status.ready` relies on.
	//
	// Trade-off: any pod that can route to a tbot pod's IP can reach
	// /readyz on tbotDiagPort. Per CONTEXT.md "NetworkPolicy", the
	// chart deliberately does NOT render a NetworkPolicy for tenant
	// tbot pods; the rendered pods carry stable selectors
	// (`tunnelport.giantswarm.io/role=tbot`,
	// `tunnelport.giantswarm.io/remoteapp=<name>`) so the platform
	// team's hand-written NetworkPolicy can target them. If tbot's
	// diag surface ever exposes routes beyond /readyz that warrant
	// defence-in-depth, revisit by either splitting probe vs. diag
	// onto two listeners or shipping a default-deny NetworkPolicy
	// alongside the rendered Deployment.
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
// Image and resources come from operator PodDefaults (Helm values via slice 6),
// not from the CR.
//
// tokenSecretVersion is stamped on the pod-template annotation
// `tunnelport.giantswarm.io/token-secret-version`. The reconciler reads
// it from `tokenRef`-Secret's `metadata.resourceVersion`; passing "" leaves
// the annotation present-but-empty so absence and a rotation-to-empty stay
// distinguishable in the pod-template diff. The argument is a separate
// parameter rather than a PodDefaults field because it changes per-reconcile,
// not per-operator-process.
//
// The container declares a readiness probe wired to tbot's diag /readyz
// (port "diag", 3001) so pod-Ready means tunnel-up — that's what
// status.ready mirrors. It also declares a liveness probe (TCPSocket on
// the diag port) so the kubelet restarts the pod if tbot's diag listener
// stops responding entirely; per ADR 0003 we lean on the kubelet for
// recovery rather than re-implementing it in the operator.
//
// The pod and container security contexts are hardened by default
// (runAsNonRoot, readOnlyRootFilesystem, drop ALL capabilities,
// RuntimeDefault seccomp). These values are NOT currently CR-tunable —
// platform teams that need to relax them must fork. Consistent with the
// project's "no escape hatches yet" stance; revisit when a real use case
// surfaces.
func renderDeployment(cr *accessv1alpha1.RemoteApp, cfg PodDefaults, tokenSecretVersion string) *appsv1.Deployment {
	labels := canonicalLabels(cr)
	replicas := int32(1)
	if cr.Spec.Replicas != nil {
		replicas = *cr.Spec.Replicas
	}
	maxSurge := intstr.FromInt(1)
	maxUnavailable := intstr.FromInt(0)

	// Pod-level securityContext: distroless nonroot UID + RuntimeDefault
	// seccomp. runAsUser pinned here (not just runAsNonRoot) so the pod
	// schedules under PSS Restricted even when the image's USER directive
	// is absent or wrong.
	runAsNonRoot := true
	runAsUser := int64(65532) // distroless nonroot UID

	// Container-level securityContext: locked-down defaults. drop ALL
	// caps + readOnlyRootFilesystem + no privilege escalation. /tmp is
	// served by an emptyDir volume so transitive writers don't EROFS.
	allowPrivilegeEscalation := false
	readOnlyRootFilesystem := true

	// tbot speaks only to the Teleport proxy and to the kube-apiserver
	// is irrelevant to its job: it has no need for a ServiceAccount
	// JWT mounted into the pod. Disabling automount keeps the SA token
	// off the pod's filesystem, narrowing the blast radius of a tbot
	// pod compromise to the Teleport credentials it already needs.
	automountServiceAccountToken := false

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
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
						AnnotationConfigHash: configHash(cr, cfg),
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
					AutomountServiceAccountToken: &automountServiceAccountToken,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						RunAsUser:    &runAsUser,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
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
						{
							// /tmp scratch space; required because
							// readOnlyRootFilesystem: true makes the rest
							// of the rootfs immutable.
							Name: volumeNameTbotTmp,
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
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: &allowPrivilegeEscalation,
								ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
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
							// Liveness probe: TCPSocket on the diag port,
							// not HTTPGet. tbot's diag listener serves
							// /readyz (used by the readiness probe above)
							// but a /livez handler isn't part of the
							// documented contract — TCPSocket detects the
							// "diag listener wedged" failure mode without
							// coupling to a specific HTTP path. Per ADR
							// 0003 we want kubelet-driven recovery rather
							// than noisy in-operator logic, so the
							// thresholds are deliberately generous:
							//   initialDelaySeconds: 30 — give the join
							//     handshake room before the first probe.
							//   periodSeconds:       30 — slow cadence.
							//   failureThreshold:    5  — 2.5 minutes of
							//     unresponsiveness before a restart.
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromString(tbotDiagPortName),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       30,
								TimeoutSeconds:      2,
								FailureThreshold:    5,
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
								{
									Name:      volumeNameTbotTmp,
									MountPath: mountPathTbotTmp,
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
//
// `Spec.ClusterIP` is deliberately NOT set: under Server-Side Apply, fields
// the controller doesn't write are not claimed by the controller's
// field-manager, and the API server's allocated ClusterIP is preserved
// across applies automatically. The previous client-side-merge path had to
// surgically copy ClusterIP from the existing object to avoid the
// "field is immutable" error on Update; SSA makes that copy unnecessary.
func renderService(cr *accessv1alpha1.RemoteApp, _ PodDefaults) *corev1.Service {
	labels := canonicalLabels(cr)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
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
func configHash(cr *accessv1alpha1.RemoteApp, cfg PodDefaults) string {
	sum := sha256.Sum256([]byte(tbotConfig(cr, cfg)))
	return hex.EncodeToString(sum[:])
}
