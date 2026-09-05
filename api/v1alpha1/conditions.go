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
	// ConditionTypeReady is the roll-up a consumer reads: the tunnel is
	// usable end to end. It is True when at least one tbot pod's readiness
	// probe (wired to tbot's diag endpoint) passes AND, when the operator
	// runs the end-to-end probe, the far end answered the last probe through
	// the tunnel (see ConditionTypeUpstreamReachable). It is False with
	// reason PodNotReady / NoPods when no pod claims the tunnel is up, and
	// False with reason UpstreamUnreachable when the pods are fine but the
	// path behind the tunnel is not — the state of giantswarm/tunnelport#110,
	// where every pod-level signal stayed green for thirteen minutes while
	// every request through the tunnel got a 504 from the Teleport app
	// service.
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

	// ConditionTypeTunnelVerified reports whether the certificate the
	// tunnel actually serves is one a caller can verify: it chains to the
	// SPIFFE trust bundle AND covers the Service DNS name callers dial.
	//
	// Every other condition here can be True while this one is False,
	// and that combination is not hypothetical — it is
	// giantswarm/giantswarm#37521. 40 tunnels served SVIDs whose
	// `dns_sans` had not followed a namespace rename: tbot joined
	// (IdentityIssued=True), ghostunnel bound its listener
	// (TunnelServing=True), the sidecar's TCPSocket probe connected
	// happily (Ready=True), and every caller failed hostname
	// verification. TunnelServing answers "does the listener accept
	// connections"; this answers "is what it serves usable", which no
	// amount of watching Kubernetes objects can establish.
	//
	// Unlike its siblings the source is an active TLS handshake the
	// operator performs against its own rendered Service, not pod state
	// — see internal/controller/remoteapp/verify.go for the position on
	// ADR 0003 and for why "cannot verify" is reported as Unknown rather
	// than False. Absent entirely when TLS verification is disabled.
	ConditionTypeTunnelVerified = "TunnelVerified"

	// ConditionTypeUpstreamReachable reports whether a request sent
	// *through* the tunnel got an answer from the far end: ghostunnel, tbot's
	// application-tunnel, the Teleport proxy, the Teleport app service and
	// the app itself all had to cooperate for the probe's `GET
	// spec.probe.path` (default `/`) to come back with any HTTP status.
	//
	// TunnelVerified stops at the consumer-side listener, and every
	// pod-level condition stops earlier still. giantswarm/tunnelport#110 is
	// the gap: a Teleport app service whose connection to its auth server
	// had gone stale answered every new session with 504 Gateway Timeout
	// for thirteen minutes, ghostunnel forwarded the requests faithfully,
	// and the RemoteApps stayed Ready/Verified/Identity/Serving throughout.
	//
	// True means the upstream answered with any status other than a
	// gateway failure — 200, 401 from an OAuth resource server, and 404 all
	// prove the path works. False, with reason UpstreamUnreachable, means
	// 502/503/504 or no HTTP response within the probe's budget; the
	// message carries the status, the probed URL and the time of the last
	// good probe. Unknown means the probe did not run: no round yet, the
	// tunnel not Ready, or the TLS handshake failed (there is no verified
	// session to send a request over). Absent when the probe is disabled.
	//
	// Unlike TunnelVerified this condition folds into Ready: a tunnel whose
	// far end cannot answer is not usable, and consumers such as muster's
	// MCPServer and humans running `kubectl get remoteapp` look at Ready.
	ConditionTypeUpstreamReachable = "UpstreamReachable"
)
