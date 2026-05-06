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
)

// TokenRef points to a Secret holding the static join token for this RemoteApp's
// dedicated TeleportBot. The operator never reads the Secret's contents — it
// only references (name, key, resourceVersion).
type TokenRef struct {
	// Name of the Secret in the same namespace as the RemoteApp.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Key inside the Secret holding the join token value.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// RemoteAppSpec defines the desired state of a RemoteApp.
type RemoteAppSpec struct {
	// AppName is the Teleport application name to expose.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	AppName string `json:"appName"`

	// Port is the local ClusterIP Service port the rendered tunnel listens on.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// ProxyAddr is the host:port of the Teleport proxy this RemoteApp connects to.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ProxyAddr string `json:"proxyAddr"`

	// TokenRef references a Secret in the same namespace holding the static
	// join token for this RemoteApp's TeleportBot.
	// +kubebuilder:validation:Required
	TokenRef TokenRef `json:"tokenRef"`

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

	// Conditions report the current state of the RemoteApp. Standard types
	// are Ready and TokenSecretBound.
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
	SchemeBuilder.Register(&RemoteApp{}, &RemoteAppList{})
}
