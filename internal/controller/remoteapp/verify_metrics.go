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

package remoteapp

import (
	"maps"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metric and label names published by the verification store. These are
// a public contract: helm/tunnelport/templates/prometheusrule.yaml
// matches on them literally and the runbook quotes them, so a rename
// here is a chart change too.
const (
	// MetricTLSVerification carries one series per RemoteApp with the
	// probe outcome in the `result` label and a constant value of 1.
	//
	// The state lives in a label rather than in the value (and rather
	// than in one boolean gauge per state) so each RemoteApp contributes
	// exactly one series at any scrape: the previous outcome's series
	// disappears on its own, with no zeroing bookkeeping, and the alert
	// expression stays a plain equality match.
	MetricTLSVerification = "tunnelport_remoteapp_tls_verification"

	// MetricUpstreamProbeStatus carries the HTTP status the far end
	// answered the last probe through the tunnel with — 0 when nothing
	// came back — with the classification in the `result` label. One
	// series per probed RemoteApp; none for tunnels that were not probed
	// (not Ready, handshake failed, probe disabled), so a series that
	// exists is always a verdict. An alert can key on either the label
	// (`result="unreachable"`) or the value (`== 504`).
	MetricUpstreamProbeStatus = "tunnelport_remoteapp_upstream_probe_status"

	// MetricTLSVerificationAvailable is 1 when the operator holds a
	// usable SPIFFE trust bundle and can therefore judge certificates at
	// all, 0 when it cannot, and absent on a replica that has not run a
	// round (see VerificationStore.probed). Cluster-scoped: one series,
	// no per-RemoteApp labels, because "the check is blind" is one fact
	// about the install rather than N facts about tunnels — alerting per
	// RemoteApp would fan one misconfiguration out across every tunnel on
	// the MC.
	//
	// It exists because the alternative to reporting this is reporting
	// nothing, and "no series" is indistinguishable from "no RemoteApps".
	// A monitoring gap that hides itself is the failure mode
	// giantswarm/giantswarm#37521 is about; this is the guard against
	// reintroducing it one level up.
	MetricTLSVerificationAvailable = "tunnelport_tls_verification_available"

	// LabelRemoteAppName and LabelRemoteAppNamespace identify one
	// RemoteApp.
	//
	// Deliberately NOT the bare `namespace`. Under a Prometheus Operator
	// PodMonitor/ServiceMonitor, target-metadata labels (namespace, pod,
	// service, ...) win over metric-borne labels of the same name unless
	// honorLabels is set, and the metric's own value is silently renamed
	// to `exported_namespace` — which breaks both the alert expressions
	// and the `{{ $labels.namespace }}` in their descriptions. muster hit
	// exactly this in giantswarm/muster#1076; prefixing is the fix.
	LabelRemoteAppName      = "remoteapp_name"
	LabelRemoteAppNamespace = "remoteapp_namespace"

	// LabelResult carries the VerificationResult (or UpstreamResult)
	// verbatim.
	LabelResult = "result"
)

// VerificationStore holds the latest probe outcome for every RemoteApp
// and publishes them to Prometheus.
//
// It is a prometheus.Collector rather than a GaugeVec on purpose, and for
// the same reason giantswarm/muster#1076 chose an *observable* OTel gauge
// over a synchronous one: series must be computed at scrape time so that
// what no longer exists stops reporting. A GaugeVec retains every series
// it was ever Set on until something calls Delete with the exact label
// tuple, so a deleted RemoteApp — or one whose `result` label changed —
// would keep exporting its last value and pin an alert on a resource
// that is gone. Collect() emits from the current map, so forgetting is
// the default and remembering is the special case.
type VerificationStore struct {
	// mu guards every field below. Taken by the probe round (Replace /
	// SetBundleUnavailable), by the reconciler (Result,
	// LastUpstreamSuccess) and by the Prometheus registry's scrape
	// goroutine (Collect).
	mu sync.RWMutex

	// results is the last outcome per RemoteApp. Absence means "not
	// covered by a round yet", which is Unknown rather than failure.
	results map[types.NamespacedName]Verification

	// lastUpstreamSuccess is when each RemoteApp's upstream last
	// answered with a non-gateway status. Kept apart from results so a
	// healthy round does not change the comparable outcome and
	// re-enqueue the fleet; consulted only while the upstream is down.
	// Pruned with results, so a deleted RemoteApp leaves nothing behind.
	lastUpstreamSuccess map[types.NamespacedName]time.Time

	// upstreamDownSince is when the current outage of each RemoteApp's
	// upstream began: stamped by the first unreachable round, carried
	// through rounds that did not probe (pods restarting mid-outage),
	// cleared by the next reachable round. upstreamRecoveredAt is when
	// that clearing round ran. Together they let the reconciler tell a
	// recovery apart from an ordinary Unknown→True — the condition alone
	// cannot, because a pod roll during the outage replaces False with
	// Unknown and the eventual True then looks like any fresh tunnel.
	upstreamDownSince   map[types.NamespacedName]time.Time
	upstreamRecoveredAt map[types.NamespacedName]time.Time

	// bundleAvailable mirrors whether the last round managed to load a
	// trust bundle.
	bundleAvailable bool

	// probed records that at least one round has completed, which is what
	// makes bundleAvailable meaningful.
	//
	// It exists because the verifier is a leader-election runnable: on a
	// standby replica no round ever runs, so an un-guarded
	// bundleAvailable would sit at its zero value and export
	// tunnelport_tls_verification_available 0 for the lifetime of the
	// pod — a permanent false alarm from the replica that is working
	// exactly as designed. Emitting no series until a round has run
	// leaves the leader as the only reporter, which is also what makes
	// `== 0` a safe alert expression. "No leader at all" is
	// TunnelPortOperatorDown's job, not this metric's.
	probed bool

	// enabled is fixed at construction. A disabled store collects
	// nothing at all — not even a zero — so an install that does not run
	// the check has no series to misread, and computeStatus omits the
	// conditions entirely.
	enabled bool

	// upstreamProbe is fixed at construction and mirrors
	// VerifyConfig.UpstreamProbe, so the reconciler can tell "not probed
	// this round" (Unknown) from "never probed by design" (no condition).
	upstreamProbe bool

	// now is the clock lastUpstreamSuccess is stamped with. Tests pin it.
	now func() time.Time

	verificationDesc *prometheus.Desc
	upstreamDesc     *prometheus.Desc
	availableDesc    *prometheus.Desc
}

// NewVerificationStore returns a store. Pass enabled=false to keep the
// operator's observable surface byte-identical to what it was before TLS
// verification existed; upstreamProbe=false does the same for the HTTP
// half only.
func NewVerificationStore(enabled, upstreamProbe bool) *VerificationStore {
	return &VerificationStore{
		results:             make(map[types.NamespacedName]Verification),
		lastUpstreamSuccess: make(map[types.NamespacedName]time.Time),
		upstreamDownSince:   make(map[types.NamespacedName]time.Time),
		upstreamRecoveredAt: make(map[types.NamespacedName]time.Time),
		enabled:             enabled,
		upstreamProbe:       enabled && upstreamProbe,
		now:                 time.Now,
		verificationDesc: prometheus.NewDesc(
			MetricTLSVerification,
			"Result of the operator's TLS verification of each RemoteApp tunnel: "+
				"1 for the current result, no series for the results it is not in. "+
				"`verified` means the certificate served on the tunnel's TLS port "+
				"chains to the SPIFFE trust bundle and covers the Service FQDN; "+
				"`cert_invalid` means it does not; `unreachable` means nothing "+
				"accepted a connection; `not_ready` means the tunnel does not claim "+
				"to be serving and was not probed.",
			[]string{LabelRemoteAppName, LabelRemoteAppNamespace, LabelResult},
			nil,
		),
		upstreamDesc: prometheus.NewDesc(
			MetricUpstreamProbeStatus,
			"HTTP status the far end answered the operator's last probe through "+
				"the tunnel with (ghostunnel, tbot, Teleport proxy, app service, app), "+
				"0 when no response arrived. `result` is `reachable` for any status "+
				"other than 502/503/504 and `unreachable` for those or no response. "+
				"No series for tunnels that were not probed.",
			[]string{LabelRemoteAppName, LabelRemoteAppNamespace, LabelResult},
			nil,
		),
		availableDesc: prometheus.NewDesc(
			MetricTLSVerificationAvailable,
			"1 when the operator holds a usable SPIFFE trust bundle and can verify "+
				"tunnel certificates, 0 when it cannot and every tunnel's "+
				"verification result is therefore unknown.",
			nil,
			nil,
		),
	}
}

// Register adds the store to controller-runtime's Prometheus registry —
// the same registry that backs the manager's /metrics endpoint, so the
// verification series are scraped by whatever already scrapes the
// operator. No-op when the store is disabled.
func (s *VerificationStore) Register() error {
	if !s.enabled {
		return nil
	}
	return ctrlmetrics.Registry.Register(s)
}

// Enabled implements VerificationReader.
func (s *VerificationStore) Enabled() bool { return s.enabled }

// UpstreamProbeEnabled implements VerificationReader.
func (s *VerificationStore) UpstreamProbeEnabled() bool { return s.upstreamProbe }

// Result implements VerificationReader.
func (s *VerificationStore) Result(key types.NamespacedName) (Verification, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.results[key]
	return v, ok
}

// LastUpstreamSuccess implements VerificationReader.
func (s *VerificationStore) LastUpstreamSuccess(key types.NamespacedName) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.lastUpstreamSuccess[key]
	return t, ok
}

// UpstreamDownSince implements VerificationReader.
func (s *VerificationStore) UpstreamDownSince(key types.NamespacedName) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.upstreamDownSince[key]
	return t, ok
}

// UpstreamRecoveredAt implements VerificationReader.
func (s *VerificationStore) UpstreamRecoveredAt(key types.NamespacedName) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.upstreamRecoveredAt[key]
	return t, ok
}

// Replace swaps in the outcomes of one complete probe round and records
// that the trust bundle was usable.
//
// Wholesale replacement is what makes deletion work without a delete
// hook: a RemoteApp that vanished between rounds is simply absent from
// the new map, so its series is gone at the next scrape — and so are its
// timestamps.
//
// The upstream bookkeeping per RemoteApp: a reachable round stamps the
// last success and, if an outage was open, closes it and stamps the
// recovery; an unreachable round opens an outage if none is open; a round
// that did not probe (tunnel not Ready, handshake failed) carries
// everything over unchanged, so a pod roll in the middle of an outage
// neither ends it nor starts a new one.
func (s *VerificationStore) Replace(results map[types.NamespacedName]Verification) {
	next := make(map[types.NamespacedName]Verification, len(results))
	maps.Copy(next, results)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	successes := make(map[types.NamespacedName]time.Time, len(s.lastUpstreamSuccess))
	down := make(map[types.NamespacedName]time.Time, len(s.upstreamDownSince))
	recovered := make(map[types.NamespacedName]time.Time, len(s.upstreamRecoveredAt))
	carry := func(from map[types.NamespacedName]time.Time, to map[types.NamespacedName]time.Time, key types.NamespacedName) {
		if t, ok := from[key]; ok {
			to[key] = t
		}
	}
	for key, v := range next {
		switch v.Upstream.Result {
		case UpstreamReachable:
			successes[key] = now
			if _, wasDown := s.upstreamDownSince[key]; wasDown {
				recovered[key] = now
			} else {
				carry(s.upstreamRecoveredAt, recovered, key)
			}
		case UpstreamUnreachable:
			carry(s.lastUpstreamSuccess, successes, key)
			carry(s.upstreamRecoveredAt, recovered, key)
			if t, ok := s.upstreamDownSince[key]; ok {
				down[key] = t
			} else {
				down[key] = now
			}
		default:
			carry(s.lastUpstreamSuccess, successes, key)
			carry(s.upstreamDownSince, down, key)
			carry(s.upstreamRecoveredAt, recovered, key)
		}
	}
	s.results = next
	s.lastUpstreamSuccess = successes
	s.upstreamDownSince = down
	s.upstreamRecoveredAt = recovered
	s.bundleAvailable = true
	s.probed = true
}

// SetBundleUnavailable drops every per-RemoteApp outcome and flips the
// availability gauge to 0.
//
// Dropping rather than keeping is the honest choice: without a bundle the
// operator has no opinion on any certificate, and retaining the last
// round's verdicts would keep asserting a judgement it can no longer
// make. The gauge is what says so out loud. The last-success timestamps
// are history rather than a verdict and stay, so a failure reported after
// the bundle returns can still say when the upstream last answered.
func (s *VerificationStore) SetBundleUnavailable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = make(map[types.NamespacedName]Verification)
	s.bundleAvailable = false
	s.probed = true
}

// Describe implements prometheus.Collector.
func (s *VerificationStore) Describe(ch chan<- *prometheus.Desc) {
	ch <- s.verificationDesc
	ch <- s.upstreamDesc
	ch <- s.availableDesc
}

// Collect implements prometheus.Collector. Called on every scrape; the
// series it emits are derived from the store's current contents, which is
// what makes stale series impossible.
func (s *VerificationStore) Collect(ch chan<- prometheus.Metric) {
	// The off switch is absolute rather than relying on nothing ever
	// calling Replace on a disabled store: Register is already a no-op
	// when disabled, and a collector that would emit if someone wired it
	// up anyway is one refactor away from an install exporting series it
	// opted out of.
	if !s.enabled {
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.probed {
		available := 0.0
		if s.bundleAvailable {
			available = 1.0
		}
		ch <- prometheus.MustNewConstMetric(s.availableDesc, prometheus.GaugeValue, available)
	}

	for key, v := range s.results {
		ch <- prometheus.MustNewConstMetric(
			s.verificationDesc, prometheus.GaugeValue, 1,
			key.Name, key.Namespace, string(v.Result),
		)
		switch v.Upstream.Result {
		case UpstreamReachable, UpstreamUnreachable:
			ch <- prometheus.MustNewConstMetric(
				s.upstreamDesc, prometheus.GaugeValue, float64(v.Upstream.StatusCode),
				key.Name, key.Namespace, string(v.Upstream.Result),
			)
		}
	}
}
