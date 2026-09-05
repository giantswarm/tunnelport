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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// RemoteAppSpec defines the desired state of a RemoteApp.
//
// See ADR 0004 for the kubernetes-join semantics: tbot authenticates to
// Teleport using the projected ServiceAccount JWT (no static-token Secret
// on the consumer MC), and `TokenName` is the name of the Teleport
// ProvisionToken resource pre-provisioned on Central.
//
// The Teleport proxy host:port and cluster name (the `aud` claim Teleport's
// `static_jwks` validator pins) are operator-level configuration, not CR
// fields — see ADR 0005 for the rationale. A given consumer MC's RemoteApps
// all bind to the same Teleport cluster; multi-Teleport-on-one-MC needs a
// second operator install.
type RemoteAppSpec struct {
	// AppName is the Teleport application name to expose. Teleport application
	// names are RFC 1123 DNS labels, so we constrain to that here.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	AppName string `json:"appName"`

	// Port is the local ClusterIP Service port the rendered tunnel listens on.
	// 3001 is reserved by tbot's hardcoded diagnostic endpoint
	// (see internal/controller/remoteapp/render.go) — colliding with it would
	// silently break readiness probing.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:validation:XValidation:rule="self != 3001",message="port 3001 is reserved for tbot's diagnostic endpoint"
	Port int32 `json:"port"`

	// TokenName is the name of the Teleport ProvisionToken on Central that
	// this RemoteApp's tbot uses to join via the `kubernetes` join method
	// (per ADR 0004). It is a literal token name, not a Kubernetes Secret
	// reference — the operator delivers no static token secret on the
	// consumer MC. Constrained to DNS-1123 subdomain shape because Teleport
	// resource names are validated against the same conventions.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
	TokenName string `json:"tokenName"`

	// Replicas is the desired number of tbot pods for this RemoteApp. The
	// reconciler defaults absence to 1 — the CRD intentionally has no default
	// so absence remains observable in the API.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Probe tunes the operator's end-to-end probe through this tunnel (the
	// UpstreamReachable condition). Absent means the defaults: one
	// `GET /` per verification round.
	// +optional
	Probe *ProbeSpec `json:"probe,omitempty"`
}

// ProbeSpec configures the end-to-end HTTP probe the operator sends through
// the tunnel each verification round. The request travels ghostunnel →
// tbot application-tunnel → Teleport proxy → Teleport app service → app,
// so any HTTP status proves the whole path answers; 502/503/504 and a
// timeout mean it does not.
type ProbeSpec struct {
	// Path is the request path of the probe, `/` when unset. Point it at a
	// cheap endpoint of the app (a health route, say) when `GET /` is
	// expensive or misleading. The status the app returns is not judged:
	// 401 and 404 count as reachable, only gateway failures do not.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern="^/[^\\s]*$"
	Path string `json:"path,omitempty"`
}

// RemoteAppStatus defines the observed state of a RemoteApp.
type RemoteAppStatus struct {
	// Ready is the shorthand consumers read: at least one tbot pod's
	// readiness probe (wired to tbot's diag endpoint) passes, and — when the
	// operator runs the end-to-end probe — the far end answered the last
	// request sent through the tunnel. Mirrors the Ready condition.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// LastError carries the most recent failure summary, e.g.
	// "CrashLoopBackOff (5 restarts), last termination: Error (137)" from
	// k8s-visible pod state, or the upstream probe's diagnosis when the pods
	// are fine but the path behind the tunnel is not. The operator does not
	// read pod logs (see ADR-0003).
	// +optional
	LastError string `json:"lastError,omitempty"`

	// ObservedGeneration is the spec generation the operator most recently
	// reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions report the current state of the RemoteApp. The only
	// standard type is `Ready` — the kubernetes-join model (ADR 0004) has
	// no consumer-side token-Secret state to report.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.appName`
// +kubebuilder:printcolumn:name="Port",type=integer,JSONPath=`.spec.port`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// Verified is a default column, not a priority=1 one, precisely because
// Ready=True with Verified=False is the state that went unnoticed for two
// days in giantswarm/giantswarm#37521. A signal only visible under
// `-o wide` would not have been looked at.
// +kubebuilder:printcolumn:name="Verified",type=string,JSONPath=`.status.conditions[?(@.type=="TunnelVerified")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Identity",type=string,JSONPath=`.status.conditions[?(@.type=="IdentityIssued")].status`,priority=1
// +kubebuilder:printcolumn:name="Serving",type=string,JSONPath=`.status.conditions[?(@.type=="TunnelServing")].status`,priority=1
// Upstream can live under -o wide: unlike Verified it already folds into
// the Ready column, so the default view shows the degradation and the wide
// view says which layer it is.
// +kubebuilder:printcolumn:name="Upstream",type=string,JSONPath=`.status.conditions[?(@.type=="UpstreamReachable")].status`,priority=1
// +kubebuilder:printcolumn:name="LastError",type=string,JSONPath=`.status.lastError`,priority=1
// +kubebuilder:printcolumn:name="Reconciled",type=string,JSONPath=`.status.conditions[?(@.type=="Reconciled")].status`,priority=1

// RemoteApp declares that a Teleport-exposed app should be reachable on this
// management cluster as a local Service. See CONTEXT.md for field semantics.
type RemoteApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RemoteAppSpec   `json:"spec"`
	Status RemoteAppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RemoteAppList contains a list of RemoteApp.
type RemoteAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RemoteApp `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(GroupVersion, &RemoteApp{}, &RemoteAppList{})
		metav1.AddToGroupVersion(scheme, GroupVersion)
		return nil
	})
}
