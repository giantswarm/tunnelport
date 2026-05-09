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

	// SATokenAudience is the audience claim baked into every projected
	// SA token mounted on a tbot pod. Teleport's `kubernetes` join
	// (`kubernetes.type: static_jwks`, per ADR 0006) validates the JWT
	// `aud` claim against the Teleport cluster's name — there is no
	// per-token audience override on the Teleport side, so the
	// operator MUST set this to whatever the consumer MC's Teleport
	// cluster expects. Cluster-wide value, set via Helm; not exposed
	// on the CR.
	SATokenAudience string
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

// renderServiceAccount returns the per-`RemoteApp` ServiceAccount the tbot
// Deployment runs as. It is the kubernetes-join identity tbot presents to
// Teleport (ADR 0006): the projected token (audience
// `saTokenAudience`) carries this SA's namespace+name+UID, and Central's
// `TeleportProvisionToken` validates the JWT under `kubernetes.type:
// static_jwks` and matches it against an allow-list naming this SA.
//
// The SA name matches the CR name — same convention the rendered
// Deployment, Service, and ConfigMap follow. On Central, the
// `TeleportBot` and `TeleportProvisionToken` are named
// `tunnelport-${cr.Name}` (ADR 0006); the operator references that name
// from `tbot.yaml`'s `onboarding.token` via `teleportProvisionTokenName`
// in tbot_config.go. The token's `kubernetes.allow` rule lists this SA
// as the single permitted subject (per-app blast-radius isolation).
//
// TypeMeta is set explicitly because `applyOwned` routes through Server-
// Side Apply, which requires `apiVersion`/`kind` on the payload.
func renderServiceAccount(cr *accessv1alpha1.RemoteApp, _ PodDefaults) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels:    canonicalLabels(cr),
		},
	}
}

// renderConfigMap returns the ConfigMap holding tbot.yaml — tbot's
// application-tunnel configuration for this RemoteApp. tbot's join
// credential is the projected SA token mounted by the rendered
// Deployment (ADR 0006); no Secret bytes are read or referenced by
// the rendered ConfigMap.
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
	volumeNameTbotStorage = "tbot-storage"
	// volumeNameTbotTmp backs /tmp because the container runs with
	// readOnlyRootFilesystem: true. tbot and its transitive deps may write
	// scratch files (Go runtime, glibc resolver caches, etc.); keeping /tmp
	// writable via emptyDir avoids surprise EROFS without relaxing the
	// root-fs hardening.
	volumeNameTbotTmp = "tbot-tmp"
	// volumeNameTbotSAToken carries the per-RemoteApp ServiceAccount's
	// projected JWT (ADR 0006). tbot reads it as the kubernetes-join
	// credential — see `tbot_config.go`'s
	// `onboarding.kubernetes.token_path`. Audience is pinned via
	// `saTokenAudience` so the token Central accepts is always the one
	// tbot mounts.
	volumeNameTbotSAToken = "tbot-sa-token"

	mountPathTbotConfig  = "/etc/tbot"
	mountPathTbotStorage = "/var/lib/tbot"
	mountPathTbotTmp     = "/tmp"
	// mountPathTbotSAToken is the directory the projected SA token
	// volume mounts at; the file inside it is named `saTokenFileName`.
	// We mount at the upstream default SA path because tbot's
	// `kubernetes` join reads from that path unconditionally — the
	// `onboarding.kubernetes.token_path` knob in newer Teleport is not
	// honored by the chart-default tbot release we ship against. The
	// audience (`saTokenAudience`) is still project-specific via the
	// projected volume's explicit `Audience` field, so a stray
	// default-audience SA token elsewhere on the MC cannot satisfy the
	// join. `automountServiceAccountToken: false` ensures the kubelet
	// does not also auto-mount a default-audience token at this path.
	mountPathTbotSAToken = "/var/run/secrets/kubernetes.io/serviceaccount"

	// saTokenFileName is the projected token's filename inside
	// mountPathTbotSAToken. `token` matches the upstream SA-token
	// filename so tbot's default token-read path resolves cleanly.
	saTokenFileName = "token"

	// saTokenAudienceDefault is the fallback audience when PodDefaults
	// does not specify one. Production deployments override this via
	// the `tbot.saTokenAudience` Helm value to match their Teleport
	// cluster's name (Teleport's static_jwks join validates the JWT
	// `aud` against the cluster name and exposes no per-token override).
	saTokenAudienceDefault = "teleport.giantswarm.io"
)

// saTokenAudienceOrDefault returns the configured SA-token audience or
// the historical default when PodDefaults leaves it empty. Centralised
// here so the rendered Deployment, configHash, and any future caller
// always agree on the value used.
func saTokenAudienceOrDefault(cfg PodDefaults) string {
	if cfg.SATokenAudience != "" {
		return cfg.SATokenAudience
	}
	return saTokenAudienceDefault
}

const (
	// saTokenExpirationSeconds is the projected token's lifetime.
	// kubelet auto-rotates the projected file before expiry, so this
	// is the worst-case staleness window, not the rotation cadence.
	// Capped at 600s (10m) because Teleport's `kubernetes.type:
	// static_jwks` join rejects SA tokens with a TTL >= 30m, and 10m
	// is the kubelet-enforced floor for projected SA tokens.
	saTokenExpirationSeconds int64 = 600

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
//   - the projected ServiceAccount token (audience
//     `tunnelport.giantswarm.io`) at `mountPathTbotSAToken`. tbot
//     reads this file as its kubernetes-join credential per ADR 0006,
//   - an emptyDir for tbot's renewable-cert destination directory (per
//     ADR 0002 — no PVC, no StatefulSet).
//
// Image and resources come from operator PodDefaults (Helm values via slice 6),
// not from the CR.
//
// ADR 0006: there is no longer a tokenRef Secret to track. The kubelet
// auto-rotates the projected SA token in-place; the rendered pod template
// is independent of any consumer-side Secret. ConfigMap content changes
// are still picked up via `AnnotationConfigHash` so spec edits roll the
// Deployment.
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
func renderDeployment(cr *accessv1alpha1.RemoteApp, cfg PodDefaults) *appsv1.Deployment {
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

	// The pod runs as the per-RemoteApp ServiceAccount renderServiceAccount
	// emits (ADR 0006 slice 1). Default-mount stays off — the SA token is
	// delivered via an explicit projected volume below so its audience can
	// be pinned to `saTokenAudience` (Central will only accept that
	// audience under `kubernetes.type: static_jwks`). Two outcomes:
	//   - the legacy /var/run/secrets/kubernetes.io/serviceaccount path is
	//     absent (no default-audience token leaks),
	//   - the projected file at `mountPathTbotSAToken` is the *only* SA
	//     token tbot ever sees.
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
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:           cr.Name,
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
						// ADR 0006: tbot authenticates with Central via the
						// projected SA token volume below; there is no
						// consumer-side Secret volume of any kind.
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
						{
							// Projected ServiceAccountToken with an
							// explicit audience (`saTokenAudience`).
							// tbot reads the file via
							// `onboarding.kubernetes.token_path` (set
							// to `mountPathTbotSAToken/saTokenFileName`
							// in tbot_config.go) and presents the JWT
							// to Central, which validates it under
							// `kubernetes.type: static_jwks` (ADR
							// 0006). kubelet auto-rotates the file
							// before `saTokenExpirationSeconds` lapses.
							Name: volumeNameTbotSAToken,
							VolumeSource: corev1.VolumeSource{
								Projected: &corev1.ProjectedVolumeSource{
									Sources: []corev1.VolumeProjection{
										{
											ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
												Audience:          saTokenAudienceOrDefault(cfg),
												ExpirationSeconds: ptrInt64(saTokenExpirationSeconds),
												Path:              saTokenFileName,
											},
										},
									},
								},
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
							// `tbot` is set explicitly so the renderer is not
							// coupled to the chosen image's ENTRYPOINT. Notably
							// `public.ecr.aws/gravitational/teleport-distroless`
							// has `teleport` as its entrypoint, which would
							// reject `start -c …` with "unexpected start".
							Command: []string{"tbot"},
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
									Name:      volumeNameTbotStorage,
									MountPath: mountPathTbotStorage,
								},
								{
									Name:      volumeNameTbotTmp,
									MountPath: mountPathTbotTmp,
								},
								{
									Name:      volumeNameTbotSAToken,
									MountPath: mountPathTbotSAToken,
									ReadOnly:  true,
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

// ptrInt64 returns a pointer to its argument. Used by renderDeployment to
// fill optional `*int64` fields (e.g. ProjectedServiceAccountToken's
// ExpirationSeconds) without sprinkling temporary local variables.
func ptrInt64(v int64) *int64 { return &v }

// configHash returns a stable, content-addressable hash of the tbot config
// so the pod template re-hashes whenever a CR field that lands in the
// ConfigMap changes. The hash is the digest of the rendered YAML, not of
// the Spec, so additions to the rendering (e.g. new tbot fields) trigger a
// roll iff they actually change the on-disk config.
func configHash(cr *accessv1alpha1.RemoteApp, cfg PodDefaults) string {
	sum := sha256.Sum256([]byte(tbotConfig(cr, cfg)))
	return hex.EncodeToString(sum[:])
}
