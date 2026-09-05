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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// TestVerificationStore_UpstreamProbeStatusSeries pins the wire shape of
// tunnelport_remoteapp_upstream_probe_status: the HTTP status as the
// value, the classification as a label, and no series at all for a
// tunnel that was not probed — so a series that exists is always a
// verdict and `== 504` / `result="unreachable"` both work as alert
// expressions.
func TestVerificationStore_UpstreamProbeStatusSeries(t *testing.T) {
	s := NewVerificationStore(true, true)
	s.Replace(map[types.NamespacedName]Verification{
		key("agent-platform", "mcp-capi"): {
			Result:   ResultVerified,
			Upstream: UpstreamProbe{Result: UpstreamUnreachable, StatusCode: 504, URL: "https://x/"},
		},
		key("agent-platform", "dex"): {
			Result:   ResultVerified,
			Upstream: UpstreamProbe{Result: UpstreamReachable, StatusCode: 401, URL: "https://y/"},
		},
		key("agent-platform", "hung"): {
			Result:   ResultVerified,
			Upstream: UpstreamProbe{Result: UpstreamUnreachable, StatusCode: 0, Detail: "no HTTP response"},
		},
		key("agent-platform", "starting"): {
			Result:   ResultNotReady,
			Upstream: UpstreamProbe{Result: UpstreamNotProbed},
		},
		key("agent-platform", "wrong-san"): {
			Result:   ResultCertInvalid,
			Upstream: UpstreamProbe{Result: UpstreamNotProbed},
		},
	})

	got := gather(t, s)
	for _, want := range []string{
		`tunnelport_remoteapp_upstream_probe_status{remoteapp_name="mcp-capi",remoteapp_namespace="agent-platform",result="unreachable"} 504`,
		`tunnelport_remoteapp_upstream_probe_status{remoteapp_name="dex",remoteapp_namespace="agent-platform",result="reachable"} 401`,
		`tunnelport_remoteapp_upstream_probe_status{remoteapp_name="hung",remoteapp_namespace="agent-platform",result="unreachable"} 0`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing series %q in:\n%s", want, got)
		}
	}
	for _, notProbed := range []string{"starting", "wrong-san"} {
		if strings.Contains(got, `tunnelport_remoteapp_upstream_probe_status{remoteapp_name="`+notProbed+`"`) {
			t.Errorf("%s was not probed and must not report an upstream series:\n%s", notProbed, got)
		}
	}
	// The TLS series are unaffected: still exactly one per RemoteApp.
	if n := strings.Count(got, "tunnelport_remoteapp_tls_verification{"); n != 5 {
		t.Errorf("%d tls_verification series, want 5:\n%s", n, got)
	}
}

// TestVerificationStore_UpstreamDisabledCollectsNoUpstreamSeries pins the
// HTTP half's off switch at the metrics level: an install that turns it
// off has no upstream series to misread, even if a stray outcome carried
// one.
func TestVerificationStore_UpstreamDisabledCollectsNoUpstreamSeries(t *testing.T) {
	s := NewVerificationStore(true, false)
	if s.UpstreamProbeEnabled() {
		t.Fatal("UpstreamProbeEnabled() = true on a store built with it off")
	}
	// A disabled store never receives upstream outcomes (RunOnce hands the
	// prober no path), so the only thing to pin is the reader's answer.
	if all := NewVerificationStore(false, true); all.UpstreamProbeEnabled() {
		t.Error("UpstreamProbeEnabled() = true while verification itself is off")
	}
}

// TestVerificationStore_LastUpstreamSuccess pins the bookkeeping behind
// "last good probe" in the condition message: stamped on a reachable
// round, kept through failing rounds, pruned with a deleted RemoteApp, and
// retained across a lost trust bundle (history, not a verdict).
func TestVerificationStore_LastUpstreamSuccess(t *testing.T) {
	s := NewVerificationStore(true, true)
	clock := time.Date(2026, 9, 5, 15, 43, 10, 0, time.UTC)
	s.now = func() time.Time { return clock }
	app := key("agent-platform", "mcp-capi")
	gone := key("agent-platform", "deleted")

	if _, ok := s.LastUpstreamSuccess(app); ok {
		t.Fatal("a fresh store reports a last success")
	}

	reachable := Verification{Result: ResultVerified, Upstream: UpstreamProbe{Result: UpstreamReachable, StatusCode: 200}}
	unreachable := Verification{Result: ResultVerified, Upstream: UpstreamProbe{Result: UpstreamUnreachable, StatusCode: 504}}

	s.Replace(map[types.NamespacedName]Verification{app: reachable, gone: reachable})
	got, ok := s.LastUpstreamSuccess(app)
	if !ok || !got.Equal(clock) {
		t.Fatalf("after a reachable round: (%v, %v), want (%v, true)", got, ok, clock)
	}

	// Time moves on and the upstream breaks: the stamp must not move.
	clock = clock.Add(2 * time.Minute)
	s.Replace(map[types.NamespacedName]Verification{app: unreachable})
	got, ok = s.LastUpstreamSuccess(app)
	if !ok || !got.Equal(time.Date(2026, 9, 5, 15, 43, 10, 0, time.UTC)) {
		t.Errorf("after a failing round: (%v, %v), want the earlier stamp kept", got, ok)
	}
	if _, ok := s.LastUpstreamSuccess(gone); ok {
		t.Error("a RemoteApp absent from the round kept its last-success stamp")
	}

	// The bundle vanishes: verdicts go, history stays.
	s.SetBundleUnavailable()
	if _, ok := s.Result(app); ok {
		t.Error("a result survived the loss of the trust bundle")
	}
	if _, ok := s.LastUpstreamSuccess(app); !ok {
		t.Error("the last-success stamp was dropped with the trust bundle; it is history, not a verdict")
	}

	// Recovery re-stamps with the current clock.
	clock = clock.Add(2 * time.Minute)
	s.Replace(map[types.NamespacedName]Verification{app: reachable})
	got, ok = s.LastUpstreamSuccess(app)
	if !ok || !got.Equal(clock) {
		t.Errorf("after recovery: (%v, %v), want (%v, true)", got, ok, clock)
	}
}
