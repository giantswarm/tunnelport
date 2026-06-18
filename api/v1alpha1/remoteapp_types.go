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
}

// RemoteAppStatus defines the observed state of a RemoteApp.
type RemoteAppStatus struct {
	// Ready is the tunnel-level readiness shorthand: true when at least one
	// tbot pod's readiness probe (wired to tbot's diag endpoint) passes.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// LastError carries the most recent k8s-visible failure summary, e.g.
	// "CrashLoopBackOff (5 restarts), last termination: Error (137)".
	// The operator does not read pod logs (see ADR-0003).
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
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
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
