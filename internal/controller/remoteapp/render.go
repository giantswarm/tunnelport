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
	"strconv"

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
	// content changes (spec.appName / spec.port / spec.tokenName, or the
	// operator-level Teleport binding from ADR 0005) cause the
	// pod-template-hash to change, which makes the Deployment roll. Without
	// this, ConfigMap-only updates would not propagate until pods restart
	// for unrelated reasons.
	AnnotationConfigHash = "tunnelport.giantswarm.io/config-hash"
)

// PodDefaults carries the operator-level knobs that are NOT on the RemoteApp
// CR but are needed to render owned objects: the tbot container image, its
// resource requests/limits, the dev-only `insecure` flag, and the
// MC-wide Teleport binding (cluster name + proxy host:port — ADR 0005).
// In production these are plumbed from Helm values; the reconciler holds
// the resolved struct directly so cmd/main.go can wire it without a
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

	// TeleportClusterName is the Teleport cluster name (the value
	// `tctl status` reports as `Cluster:` on Central). Used as the
	// `aud` claim on every rendered tbot pod's projected SA JWT;
	// Teleport's `static_jwks` join validator pins JWT `aud` to this
	// exact value (`a.GetDomainName()` in `lib/auth/join_kubernetes.go`).
	// Required at chart install time — cmd/main.go fails fast on empty
	// (ADR 0005).
	TeleportClusterName string

	// TeleportProxyAddr is the host:port of the Teleport proxy every
	// rendered tbot pod connects to. Flows into `proxy_server` in the
	// rendered tbot.yaml. Required at chart install time — cmd/main.go
	// fails fast on empty (ADR 0005).
	TeleportProxyAddr string

	// GhostunnelImage is the container image reference for the ghostunnel
	// sidecar that terminates TLS for the rendered Service (ADR 0007 /
	// slice 02).
	GhostunnelImage string

	// GhostunnelReloadInterval is the value passed to ghostunnel's
	// `--timed-reload` flag. Kept as a string so Go-duration values like
	// `5m` pass through unchanged; 5m is safe with tbot's default 20m SVID
	// renewal cadence (ADR 0007 §"Migration shape").
	GhostunnelReloadInterval string

	// GhostunnelListenPort is the TLS port the ghostunnel sidecar listens
	// on inside the pod and that the rendered Service exposes as `tls`.
	GhostunnelListenPort int32
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

// renderServiceAccount returns the per-CR ServiceAccount the rendered tbot
// pod runs under. Per ADR 0004 the SA's projected JWT is the subject the
// Teleport ProvisionToken's kubernetes `allow` rule pins (e.g.
// `service_account: "<namespace>:<cr.Name>"`), so a leaked tbot pod can
// only join *that one* CR's join token.
//
// The SA has no RoleBinding in this package — ADR 0008 removed the
// per-CR trust-bundle Role/RoleBinding that previously narrowed the
// SA's authority to one Secret. Consumer trust-bundle distribution is
// now the chart-managed singleton bot's responsibility. The pod
// template's `automountServiceAccountToken: true` is what causes the
// kubelet to mount the projected JWT.
//
// Name = CR name (same convention as the rendered Deployment / Service /
// ConfigMap, per `canonicalLabels`).
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
// application-tunnel configuration for this RemoteApp. The token value
// in tbot.yaml is the Teleport ProvisionToken's *name* (ADR 0004); tbot
// authenticates with its projected SA JWT, so there is no static-token
// Secret to mount.
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
	// volumeNameTbotJoinSAToken backs the projected ServiceAccount JWT
	// the kubernetes-join model (ADR 0004) requires. The kubelet projects
	// a fresh JWT whose `aud` claim matches `cfg.TeleportClusterName`
	// (the operator's `--teleport-cluster-name` flag, ADR 0005) and
	// whose TTL is below Teleport's 30-minute static_jwks ceiling. The
	// default automounted SA token at
	// /var/run/secrets/kubernetes.io/serviceaccount/token carries the
	// kube-apiserver's default audience, which Teleport rejects.
	volumeNameTbotJoinSAToken = "join-sa-token"
	// volumeNameTbotTmp backs /tmp because the container runs with
	// readOnlyRootFilesystem: true. tbot and its transitive deps may write
	// scratch files (Go runtime, glibc resolver caches, etc.); keeping /tmp
	// writable via emptyDir avoids surprise EROFS without relaxing the
	// root-fs hardening.
	volumeNameTbotTmp = "tbot-tmp"

	mountPathTbotConfig      = "/etc/tbot"
	mountPathTbotStorage     = "/var/lib/tbot"
	mountPathTbotJoinSAToken = "/var/run/secrets/tokens"
	tbotJoinSATokenFileName  = "join-sa-token"
	mountPathTbotTmp         = "/tmp"

	// volumeNameSVID backs the in-pod directory tbot's workload-identity-x509
	// service writes the SVID trio (svid.pem, svid_key.pem, svid_bundle.pem)
	// into. Slice 02 mounts the same volume read-only into the ghostunnel
	// sidecar so the sidecar can serve the SVID as a TLS server cert
	// (ADR 0007).
	volumeNameSVID = "svid"
	// mountPathSVID is /var/run/spiffe — the SPIFFE Workload API convention
	// for on-disk SVID material, also where the ghostunnel sidecar (slice 02)
	// expects to find cert/key/bundle.
	mountPathSVID = "/var/run/spiffe"

	// joinSATokenExpirationSeconds is the kubelet's projected SA token
	// TTL. 600 is the kubelet minimum (lower values are silently raised)
	// and well under Teleport's 30-minute static_jwks ceiling; the
	// upstream tbot Helm chart uses the same value.
	joinSATokenExpirationSeconds int64 = 600

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

	// tlsListenPortDefault is the default port the ghostunnel sidecar
	// (slice 02 / ADR 0007) listens on inside the pod and that the
	// rendered Service exposes as the `tls` port. The chart's
	// `tls.port` value overrides this at install time.
	tlsListenPortDefault int32 = 8443

	// ghostunnelContainerName is the name of the TLS-terminating sidecar
	// container (slice 02 / ADR 0007).
	ghostunnelContainerName = "ghostunnel"

	// servicePortNameTLS is the Service port name fronting the ghostunnel
	// sidecar. The plaintext port keeps its existing name `tbot` for
	// selector / NetworkPolicy backward compatibility.
	servicePortNameTLS = "tls"
)

// renderDeployment returns the Deployment that runs tbot for this RemoteApp.
// Replicas defaults to 1 when spec.Replicas is nil. The strategy is
// RollingUpdate with maxSurge=1, maxUnavailable=0 — the new pod must become
// Ready before the old one is killed, so new connections see no downtime.
//
// The pod template mounts:
//   - the tbot config ConfigMap (read-only, name reference),
//   - an emptyDir for tbot's renewable-cert destination directory (per
//     ADR 0002 — no PVC, no StatefulSet).
//
// Per ADR 0004 tbot authenticates to Teleport via the projected
// ServiceAccount JWT (the kubernetes join method). The pod template runs
// under a per-CR ServiceAccount (renderServiceAccount) and the
// `automountServiceAccountToken` toggle is left at true so the kubelet
// mounts the projected JWT at the well-known path tbot looks for.
//
// Image and resources come from operator PodDefaults (Helm values via slice 6),
// not from the CR.
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

	// Per ADR 0004 tbot authenticates to Teleport via the kubernetes
	// join method, which requires the projected ServiceAccount JWT to
	// be mounted into the pod. The kubelet only mounts the projected
	// token when `automountServiceAccountToken` is true (or unset),
	// so we explicitly opt in here. The blast-radius cost is bounded:
	// the dedicated per-CR ServiceAccount (renderServiceAccount) has
	// no RoleBinding and therefore no in-cluster authority beyond
	// "exists as an identity"; its only job is to be the subject the
	// Teleport ProvisionToken's `allow` rule pins.
	automountServiceAccountToken := true

	// Ghostunnel sidecar (slice 02 / ADR 0007). Terminates TLS for the
	// rendered Service using the SVID tbot writes into the shared `svid`
	// emptyDir. `--timed-reload=5m` is safe because tbot's
	// workload-identity-x509 renewal cadence is 20m (renderConfigMap).
	listenPort := cfg.GhostunnelListenPort
	if listenPort == 0 {
		listenPort = tlsListenPortDefault
	}
	ghostunnelContainer := corev1.Container{
		Name:  ghostunnelContainerName,
		Image: cfg.GhostunnelImage,
		Args: []string{
			"server",
			"--cert=" + mountPathSVID + "/svid.pem",
			"--key=" + mountPathSVID + "/svid_key.pem",
			"--target=127.0.0.1:" + itoa(cr.Spec.Port),
			"--listen=0.0.0.0:" + itoa(listenPort),
			"--timed-reload=" + cfg.GhostunnelReloadInterval,
			"--disable-authentication",
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          servicePortNameTLS,
				ContainerPort: listenPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &allowPrivilegeEscalation,
			ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
			RunAsNonRoot:             &runAsNonRoot,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      volumeNameSVID,
				MountPath: mountPathSVID,
				ReadOnly:  true,
			},
		},
	}

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
						// or operator-level Teleport binding change (ADR
						// 0005) — which only updates the ConfigMap data,
						// not the pod template directly — still rolls the
						// Deployment via pod-template-hash.
						AnnotationConfigHash: configHash(cr, cfg),
					},
				},
				Spec: corev1.PodSpec{
					// Per-CR ServiceAccount (renderServiceAccount). The
					// projected JWT this SA receives is the subject the
					// Teleport ProvisionToken's `allow` rule pins to —
					// ADR 0004.
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
						{
							// Projected ServiceAccount JWT with the
							// Teleport cluster name as audience and a
							// short TTL — the shape `static_jwks`
							// validation requires (ADR 0004).
							Name: volumeNameTbotJoinSAToken,
							VolumeSource: corev1.VolumeSource{
								Projected: &corev1.ProjectedVolumeSource{
									Sources: []corev1.VolumeProjection{
										{
											ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
												Path:              tbotJoinSATokenFileName,
												Audience:          cfg.TeleportClusterName,
												ExpirationSeconds: ptr(joinSATokenExpirationSeconds),
											},
										},
									},
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
						{
							// SVID emptyDir shared with the ghostunnel
							// sidecar in slice 02 (ADR 0007). tbot's
							// workload-identity-x509 service writes
							// svid.pem / svid_key.pem / svid_bundle.pem
							// here on every renewal.
							Name: volumeNameSVID,
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
							// `tbot` is set explicitly so the renderer is not
							// coupled to the chosen image's ENTRYPOINT. Notably
							// `public.ecr.aws/gravitational/teleport-distroless`
							// has `teleport` as its entrypoint, which would
							// reject `start -c …` with "unexpected start".
							Command: []string{"tbot"},
							// KUBERNETES_TOKEN_PATH tells tbot to read its
							// join JWT from the projected SA token volume
							// (audience = cfg.TeleportClusterName, the
							// operator-level flag from ADR 0005). Without
							// this, tbot falls back to
							// /var/run/secrets/kubernetes.io/serviceaccount/token,
							// whose audience is the kube-apiserver's
							// default — Teleport rejects that as
							// "invalid audience claim".
							Env: []corev1.EnvVar{
								{
									Name:  "KUBERNETES_TOKEN_PATH",
									Value: mountPathTbotJoinSAToken + "/" + tbotJoinSATokenFileName,
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
									Name:      volumeNameTbotJoinSAToken,
									MountPath: mountPathTbotJoinSAToken,
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
									// tbot writes the SVID renewals here;
									// must NOT be read-only.
									Name:      volumeNameSVID,
									MountPath: mountPathSVID,
								},
							},
						},
						ghostunnelContainer,
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
func renderService(cr *accessv1alpha1.RemoteApp, cfg PodDefaults) *corev1.Service {
	labels := canonicalLabels(cr)
	// Slice 02 / ADR 0007: append a `tls` port fronting the ghostunnel
	// sidecar. The plaintext port name `tbot` is unchanged to preserve
	// any external NetworkPolicy refs that target it.
	tlsPort := cfg.GhostunnelListenPort
	if tlsPort == 0 {
		tlsPort = tlsListenPortDefault
	}
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
				{
					Name:       servicePortNameTLS,
					Port:       tlsPort,
					TargetPort: intstr.FromInt(int(tlsPort)),
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

// ptr returns a pointer to v. The Kubernetes core types still use pointer
// fields for optional scalars, and the local helper keeps the call sites
// in renderDeployment readable without importing a third-party helper.
func ptr[T any](v T) *T {
	return &v
}

// itoa formats an int32 as a base-10 string. Used to compose ghostunnel's
// `host:port` args without dragging fmt.Sprintf into the renderer hot path.
func itoa(v int32) string {
	return strconv.FormatInt(int64(v), 10)
}
