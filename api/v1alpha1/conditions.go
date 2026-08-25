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

const (
	// ConditionTypeReady mirrors pod readiness, which is wired to tbot's diag
	// endpoint reporting tunnel state. It surfaces join-level state: the
	// tunnel either reaches Teleport and serves traffic, or it does not.
	ConditionTypeReady = "Ready"

	// ConditionTypeReconciled surfaces operator-internal state: whether the
	// last reconcile pass successfully applied every owned object
	// (ServiceAccount, ConfigMap, Deployment, Service) to the API server.
	// It is intentionally distinct from `Ready` — a reconcile can succeed
	// (Reconciled=True) while the tunnel itself is not yet up (Ready=False
	// because tbot is still starting), and a reconcile can fail
	// (Reconciled=False) while the tunnel is still serving traffic from a
	// previously-successful apply (Ready=True).
	ConditionTypeReconciled = "Reconciled"

	// ConditionTypeIdentityIssued reports whether tbot holds a usable
	// Teleport identity. It is named for the role rather than the container,
	// so swapping which container performs the join does not rename a piece
	// of the API.
	//
	// The source is the tbot container's readiness, whose probe is wired to
	// tbot's diag endpoint. That is k8s-visible state, which is the only
	// input ADR 0003 permits status to derive from: the ADR rejected
	// conditions of this shape when they would come from parsing tbot's
	// logs, not the conditions themselves. So this asserts what the probe
	// reports, and never that a credential was seen on disk.
	ConditionTypeIdentityIssued = "IdentityIssued"

	// ConditionTypeTunnelServing reports whether the TLS listener that
	// fronts the tunnel accepts connections. Also named for the role: the
	// sidecar that terminates TLS today is ghostunnel (ADR 0007), and the
	// condition outlives that choice.
	//
	// It is distinct from IdentityIssued because the two fail in a fixed
	// order. The listener cannot bind without the SVID, so
	// IdentityIssued=False with TunnelServing=False means the join is the
	// cause and TLS is the symptom, while IdentityIssued=True with
	// TunnelServing=False is a genuine TLS-side fault.
	ConditionTypeTunnelServing = "TunnelServing"
)
