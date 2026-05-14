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
)
