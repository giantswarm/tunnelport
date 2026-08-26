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
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"
)

// gather renders the store's current series as one line of Prometheus
// exposition text per series, sorted. Going through a real registry
// (rather than reading the map) is the point: it is the wire format the
// PrometheusRule expressions are written against, and the only place a
// wrong metric or label name shows up.
func gather(t *testing.T, s *VerificationStore) string {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(s); err != nil {
		t.Fatalf("register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	lines := make([]string, 0, 8)
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if m.Gauge == nil {
				t.Fatalf("metric %s is not a gauge", mf.GetName())
			}
			var sb strings.Builder
			sb.WriteString(mf.GetName())
			if labels := m.GetLabel(); len(labels) > 0 {
				parts := make([]string, 0, len(labels))
				for _, l := range labels {
					parts = append(parts, l.GetName()+`="`+l.GetValue()+`"`)
				}
				sort.Strings(parts)
				sb.WriteString("{" + strings.Join(parts, ",") + "}")
			}
			sb.WriteString(" ")
			sb.WriteString(strconv.FormatFloat(m.Gauge.GetValue(), 'g', -1, 64))
			lines = append(lines, sb.String())
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func key(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

// TestVerificationStore_LabelsAreNamespacePrefixed is the assertion that
// pays for itself. Under a PodMonitor, a metric-borne label called
// `namespace` loses to the target's own and gets silently renamed to
// `exported_namespace`, which breaks every alert expression written
// against it — the exact trap giantswarm/muster#1076 documented. Pin the
// exposed names on the wire so a well-meaning rename cannot land quietly.
func TestVerificationStore_LabelsAreNamespacePrefixed(t *testing.T) {
	s := NewVerificationStore(true)
	s.Replace(map[types.NamespacedName]Verification{
		key("agent-platform", "mcp-kubernetes"): {Result: ResultVerified},
	})

	got := gather(t, s)
	want := `tunnelport_remoteapp_tls_verification{remoteapp_name="mcp-kubernetes",remoteapp_namespace="agent-platform",result="verified"} 1
tunnelport_tls_verification_available 1
`
	if got != want {
		t.Errorf("exposition mismatch\n got: %s\nwant: %s", got, want)
	}
	if strings.Contains(got, `namespace="`) && !strings.Contains(got, `remoteapp_namespace="`) {
		t.Error("metric carries a bare `namespace` label; it would be renamed to exported_namespace")
	}
}

// TestVerificationStore_DeletedRemoteAppStopsReporting is the reason the
// store is a scrape-time Collector rather than a GaugeVec. A GaugeVec
// keeps whatever it was last Set to until something Deletes the exact
// label tuple, so a deleted RemoteApp would keep its series and pin
// TunnelPortTunnelCertificateInvalid on a resource nobody can fix.
func TestVerificationStore_DeletedRemoteAppStopsReporting(t *testing.T) {
	s := NewVerificationStore(true)
	s.Replace(map[types.NamespacedName]Verification{
		key("smoke", "kept"):    {Result: ResultCertInvalid},
		key("smoke", "deleted"): {Result: ResultCertInvalid},
	})
	if got := gather(t, s); !strings.Contains(got, `remoteapp_name="deleted"`) {
		t.Fatalf("setup: deleted app missing from first round: %s", got)
	}

	// The next round simply does not include it — no delete hook, no
	// bookkeeping.
	s.Replace(map[types.NamespacedName]Verification{
		key("smoke", "kept"): {Result: ResultCertInvalid},
	})

	got := gather(t, s)
	if strings.Contains(got, `remoteapp_name="deleted"`) {
		t.Errorf("deleted RemoteApp still reporting:\n%s", got)
	}
	if !strings.Contains(got, `remoteapp_name="kept"`) {
		t.Errorf("surviving RemoteApp stopped reporting:\n%s", got)
	}
}

// TestVerificationStore_RecoveryDropsTheOldResultSeries covers the other
// half of the same property: because the result is a label, a tunnel that
// recovers must not leave its `cert_invalid` series behind at value 1.
// Exactly one series per RemoteApp, always.
func TestVerificationStore_RecoveryDropsTheOldResultSeries(t *testing.T) {
	s := NewVerificationStore(true)
	s.Replace(map[types.NamespacedName]Verification{
		key("smoke", "app"): {Result: ResultCertInvalid},
	})
	s.Replace(map[types.NamespacedName]Verification{
		key("smoke", "app"): {Result: ResultVerified},
	})

	got := gather(t, s)
	if strings.Contains(got, `result="cert_invalid"`) {
		t.Errorf("recovered tunnel still reports cert_invalid:\n%s", got)
	}
	if !strings.Contains(got, `result="verified"`) {
		t.Errorf("recovered tunnel does not report verified:\n%s", got)
	}
	if n := strings.Count(got, "tunnelport_remoteapp_tls_verification"); n != 1 {
		t.Errorf("got %d verification series for one RemoteApp, want exactly 1:\n%s", n, got)
	}
}

// TestVerificationStore_BundleUnavailable pins the honest-unknown rule at
// the metric level: no bundle means no verdicts at all plus an explicit
// 0 on the availability gauge, which is what
// TunnelPortTLSVerificationUnavailable fires on.
func TestVerificationStore_BundleUnavailable(t *testing.T) {
	s := NewVerificationStore(true)
	s.Replace(map[types.NamespacedName]Verification{
		key("smoke", "app"): {Result: ResultVerified},
	})
	s.SetBundleUnavailable()

	got := gather(t, s)
	want := "tunnelport_tls_verification_available 0\n"
	if got != want {
		t.Errorf("exposition = %q, want only the availability gauge at 0", got)
	}
}

// TestVerificationStore_StandbyReplicaReportsNothing pins the guard that
// keeps a hot standby from firing the alert it is not responsible for.
// The verifier is leader-elected, so on a standby no round ever runs; an
// un-guarded availability gauge would sit at its zero value and report 0
// forever.
func TestVerificationStore_StandbyReplicaReportsNothing(t *testing.T) {
	s := NewVerificationStore(true)
	if got := gather(t, s); got != "" {
		t.Errorf("a store that has never run a round exposes %q, want nothing", got)
	}
}

// TestVerificationStore_DisabledCollectsNothing pins the off switch: an
// install that does not run the check must have the same observable
// surface it had before the check existed, so there is no ambiguous zero
// for anyone to alert on.
func TestVerificationStore_DisabledCollectsNothing(t *testing.T) {
	s := NewVerificationStore(false)
	if s.Enabled() {
		t.Fatal("Enabled() is true on a disabled store")
	}
	if err := s.Register(); err != nil {
		t.Fatalf("Register on a disabled store: %v", err)
	}
	// Registering into the shared controller-runtime registry must have
	// been a no-op, so a second call cannot collide.
	if err := s.Register(); err != nil {
		t.Fatalf("second Register on a disabled store: %v", err)
	}
	s.Replace(map[types.NamespacedName]Verification{
		key("smoke", "app"): {Result: ResultCertInvalid},
	})
	if got := gather(t, s); got != "" {
		t.Errorf("disabled store exposes %q, want nothing", got)
	}
}

// TestVerificationStore_ReplaceCopies guards against the caller mutating
// the map it handed over — the round builds one map per pass, but an
// aliased map would let a later write race the scrape goroutine.
func TestVerificationStore_ReplaceCopies(t *testing.T) {
	s := NewVerificationStore(true)
	handed := map[types.NamespacedName]Verification{
		key("smoke", "app"): {Result: ResultVerified},
	}
	s.Replace(handed)
	handed[key("smoke", "sneaked-in")] = Verification{Result: ResultCertInvalid}

	if got := gather(t, s); strings.Contains(got, "sneaked-in") {
		t.Errorf("store aliases the caller's map:\n%s", got)
	}
}
