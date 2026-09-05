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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// The UpstreamReachable condition and its fold into Ready
// (giantswarm/tunnelport#110). The row that matters most is "verified
// handshake, 504 behind it, pods all Ready": that is the incident in one
// assertion — IdentityIssued, TunnelServing and TunnelVerified all True
// while Ready is False with reason UpstreamUnreachable. Before this
// condition existed, that state was indistinguishable from a healthy
// tunnel from anywhere inside Kubernetes.

func TestComputeStatus_UpstreamReachableCondition(t *testing.T) {
	cr := newRemoteApp()
	self := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}
	fqdn := serviceFQDN(cr.Namespace, cr.Name, DefaultClusterDomain)
	url := "https://" + fqdn + ":8443/"
	lastGood := time.Date(2026, 9, 5, 15, 43, 10, 0, time.UTC)

	verified := func(up UpstreamProbe) map[types.NamespacedName]Verification {
		return map[types.NamespacedName]Verification{
			self: {Result: ResultVerified, ServerName: fqdn, Upstream: up},
		}
	}

	tests := []struct {
		name          string
		verifications VerificationReader
		// wantPresent false means the condition must be absent entirely.
		wantPresent bool
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage []string
		// Ready expectations. wantReadyReason "" means TunnelReady.
		wantReady       bool
		wantReadyReason string
	}{
		{
			name:          "verification not wired",
			verifications: nil,
			wantPresent:   false,
			wantReady:     true,
		},
		{
			name:          "verification enabled, upstream probe off",
			verifications: stubVerifications{enabled: true, upstream: false, results: verified(UpstreamProbe{Result: UpstreamUnreachable, StatusCode: 504, URL: url})},
			wantPresent:   false,
			// With the HTTP half off the surface is exactly pre-#110: a
			// stale unreachable outcome in the store must not touch Ready.
			wantReady: true,
		},
		{
			name:          "no result yet",
			verifications: stubVerifications{enabled: true, upstream: true},
			wantPresent:   true,
			wantStatus:    metav1.ConditionUnknown,
			wantReason:    reasonUpstreamPending,
			wantReady:     true,
		},
		{
			name: "tunnel not ready",
			verifications: stubVerifications{enabled: true, upstream: true, results: map[types.NamespacedName]Verification{
				self: {Result: ResultNotReady, ServerName: fqdn, Upstream: UpstreamProbe{Result: UpstreamNotProbed}},
			}},
			wantPresent: true,
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  reasonNotVerifiedNotReady,
			wantReady:   true,
		},
		{
			// The SAN-drift incident: no verified session, so no request.
			// Unknown here, and Ready stays True — the wrong-SAN state is
			// TunnelVerified's to report, not this condition's to mask.
			name: "certificate invalid, upstream not probed",
			verifications: stubVerifications{enabled: true, upstream: true, results: map[types.NamespacedName]Verification{
				self: {Result: ResultCertInvalid, Detail: "SAN mismatch", ServerName: fqdn, Upstream: UpstreamProbe{Result: UpstreamNotProbed}},
			}},
			wantPresent: true,
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  reasonUpstreamNotVerified,
			wantReady:   true,
		},
		{
			name:          "reachable 200",
			verifications: stubVerifications{enabled: true, upstream: true, results: verified(UpstreamProbe{Result: UpstreamReachable, StatusCode: 200, URL: url})},
			wantPresent:   true,
			wantStatus:    metav1.ConditionTrue,
			wantReason:    reasonUpstreamReachable,
			wantMessage:   []string{"200 OK", url},
			wantReady:     true,
		},
		{
			// An mcp-oauth resource server answering an unauthenticated
			// GET. Reachable.
			name:          "reachable 401",
			verifications: stubVerifications{enabled: true, upstream: true, results: verified(UpstreamProbe{Result: UpstreamReachable, StatusCode: 401, URL: url})},
			wantPresent:   true,
			wantStatus:    metav1.ConditionTrue,
			wantReason:    reasonUpstreamReachable,
			wantMessage:   []string{"401 Unauthorized", url},
			wantReady:     true,
		},
		{
			// The incident.
			name: "unreachable 504 with a last good probe",
			verifications: stubVerifications{
				enabled: true, upstream: true,
				results:  verified(UpstreamProbe{Result: UpstreamUnreachable, StatusCode: 504, URL: url}),
				lastGood: map[types.NamespacedName]time.Time{self: lastGood},
			},
			wantPresent:     true,
			wantStatus:      metav1.ConditionFalse,
			wantReason:      reasonUpstreamUnreachable,
			wantMessage:     []string{"504 Gateway Timeout", url, "last good probe 2026-09-05T15:43:10Z"},
			wantReady:       false,
			wantReadyReason: reasonUpstreamUnreachable,
		},
		{
			name:            "unreachable 502, never good",
			verifications:   stubVerifications{enabled: true, upstream: true, results: verified(UpstreamProbe{Result: UpstreamUnreachable, StatusCode: 502, URL: url})},
			wantPresent:     true,
			wantStatus:      metav1.ConditionFalse,
			wantReason:      reasonUpstreamUnreachable,
			wantMessage:     []string{"502 Bad Gateway", url, "no good probe recorded"},
			wantReady:       false,
			wantReadyReason: reasonUpstreamUnreachable,
		},
		{
			// tbot held the connection while its dial to the proxy hung.
			name: "no response",
			verifications: stubVerifications{enabled: true, upstream: true, results: verified(UpstreamProbe{
				Result: UpstreamUnreachable, URL: url, Detail: "no HTTP response from " + url + " within 10s",
			})},
			wantPresent:     true,
			wantStatus:      metav1.ConditionFalse,
			wantReason:      reasonUpstreamUnreachable,
			wantMessage:     []string{"no HTTP response from " + url + " within 10s", "no good probe recorded"},
			wantReady:       false,
			wantReadyReason: reasonUpstreamUnreachable,
		},
		{
			// Defensive branch: verified, probe on, but no outcome recorded
			// (the probe was switched on after this round). Unknown, never a
			// pass and never a failure.
			name:          "verified without an upstream outcome",
			verifications: stubVerifications{enabled: true, upstream: true, results: verified(UpstreamProbe{Result: UpstreamNotProbed})},
			wantPresent:   true,
			wantStatus:    metav1.ConditionUnknown,
			wantReason:    reasonUpstreamPending,
			wantReady:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeStatus(cr, []corev1.Pod{readyPodFixture()}, nil, "", tc.verifications)

			cond := conditionByType(got.Conditions, accessv1alpha1.ConditionTypeUpstreamReachable)
			if !tc.wantPresent {
				if cond != nil {
					t.Fatalf("UpstreamReachable present (%+v), want absent", *cond)
				}
			} else {
				if cond == nil {
					t.Fatal("UpstreamReachable condition missing")
				}
				if cond.Status != tc.wantStatus {
					t.Errorf("Status = %q, want %q", cond.Status, tc.wantStatus)
				}
				if cond.Reason != tc.wantReason {
					t.Errorf("Reason = %q, want %q", cond.Reason, tc.wantReason)
				}
				for _, want := range tc.wantMessage {
					if !strings.Contains(cond.Message, want) {
						t.Errorf("Message = %q, want it to contain %q", cond.Message, want)
					}
				}
			}

			// The fold. status.ready and the Ready condition must agree,
			// and when the probe is what took Ready down the reason and
			// message must say so — that is the whole point of folding.
			ready := conditionByType(got.Conditions, accessv1alpha1.ConditionTypeReady)
			if ready == nil {
				t.Fatal("Ready condition missing")
			}
			if got.Ready != tc.wantReady {
				t.Errorf("status.ready = %v, want %v", got.Ready, tc.wantReady)
			}
			if (ready.Status == metav1.ConditionTrue) != tc.wantReady {
				t.Errorf("Ready condition = %q, want ready=%v", ready.Status, tc.wantReady)
			}
			wantReadyReason := tc.wantReadyReason
			if wantReadyReason == "" {
				wantReadyReason = reasonTunnelReady
			}
			if ready.Reason != wantReadyReason {
				t.Errorf("Ready reason = %q, want %q", ready.Reason, wantReadyReason)
			}
			if !tc.wantReady {
				if ready.Message != cond.Message {
					t.Errorf("Ready message = %q, want the upstream diagnosis %q", ready.Message, cond.Message)
				}
				if got.LastError != cond.Message {
					t.Errorf("lastError = %q, want the upstream diagnosis", got.LastError)
				}
			} else if got.LastError != "" {
				t.Errorf("lastError = %q, want empty on a usable tunnel", got.LastError)
			}

			// The pod-derived conditions and TunnelVerified are untouched
			// by the upstream verdict: the pods are fully Ready in every
			// row, and whether the certificate verified is its own fact.
			for _, other := range []string{
				accessv1alpha1.ConditionTypeIdentityIssued,
				accessv1alpha1.ConditionTypeTunnelServing,
			} {
				c := conditionByType(got.Conditions, other)
				if c == nil {
					t.Fatalf("%s condition missing", other)
				}
				if c.Status != metav1.ConditionTrue {
					t.Errorf("%s = %q, want True (the pod is fully ready)", other, c.Status)
				}
			}
		})
	}
}

// TestComputeStatus_PodsNotReadyKeepsPodReasonOverStaleUpstream: when no
// pod claims the tunnel is up, Ready is False for the pod's reason, even
// if the store still holds last round's unreachable verdict. The pod
// state explains the outage; blaming the upstream would be a lie about
// which layer to look at.
func TestComputeStatus_PodsNotReadyKeepsPodReasonOverStaleUpstream(t *testing.T) {
	cr := newRemoteApp()
	self := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}
	v := stubVerifications{enabled: true, upstream: true, results: map[types.NamespacedName]Verification{
		self: {Result: ResultVerified, ServerName: "x", Upstream: UpstreamProbe{Result: UpstreamUnreachable, StatusCode: 504, URL: "https://x/"}},
	}}

	crashing := corev1.Pod{Status: corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: tbotContainerName, Ready: false, RestartCount: 3,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoopBackOff}},
		}},
	}}
	got := computeStatus(cr, []corev1.Pod{crashing}, nil, "", v)
	ready := conditionByType(got.Conditions, accessv1alpha1.ConditionTypeReady)
	if got.Ready || ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %v / %+v, want False", got.Ready, ready)
	}
	if ready.Reason != reasonPodNotReady {
		t.Errorf("Ready reason = %q, want %q (the pod explains the outage)", ready.Reason, reasonPodNotReady)
	}
	if !strings.Contains(got.LastError, reasonCrashLoopBackOff) {
		t.Errorf("lastError = %q, want the pod summary", got.LastError)
	}
}

// TestComputeStatus_DisablingUpstreamProbeRemovesStaleCondition pins the
// removal path, the same way TunnelVerified has one: an install that turns
// the HTTP half off ends up with exactly the status shape it had before,
// and Ready goes back to join-level immediately rather than staying False
// on a verdict nobody is refreshing.
func TestComputeStatus_DisablingUpstreamProbeRemovesStaleCondition(t *testing.T) {
	cr := newRemoteApp()
	self := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}
	results := map[types.NamespacedName]Verification{
		self: {Result: ResultVerified, ServerName: "x", Upstream: UpstreamProbe{Result: UpstreamUnreachable, StatusCode: 504, URL: "https://x/"}},
	}

	with := computeStatus(cr, []corev1.Pod{readyPodFixture()}, nil, "", stubVerifications{enabled: true, upstream: true, results: results})
	if with.Ready || conditionByType(with.Conditions, accessv1alpha1.ConditionTypeUpstreamReachable) == nil {
		t.Fatal("setup: expected Ready=false with an UpstreamReachable condition")
	}

	without := computeStatus(cr, []corev1.Pod{readyPodFixture()}, with.Conditions, "", stubVerifications{enabled: true, upstream: false, results: results})
	if cond := conditionByType(without.Conditions, accessv1alpha1.ConditionTypeUpstreamReachable); cond != nil {
		t.Errorf("UpstreamReachable survived disabling: %+v", *cond)
	}
	if !without.Ready {
		t.Error("Ready stayed false after the upstream probe was disabled")
	}
	if conditionByType(without.Conditions, accessv1alpha1.ConditionTypeTunnelVerified) == nil {
		t.Error("TunnelVerified vanished with the upstream probe; the two switches are independent")
	}
}

// TestComputeStatus_UpstreamConditionIsStableAcrossReconciles guards the
// hot-loop suppression in reconcileStatus for both a healthy and a failing
// upstream. The failing case is the one that could churn: its message
// carries the last-good timestamp, which must therefore come from the
// store rather than from the clock at render time.
func TestComputeStatus_UpstreamConditionIsStableAcrossReconciles(t *testing.T) {
	cr := newRemoteApp()
	self := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}
	for name, v := range map[string]stubVerifications{
		"reachable": {enabled: true, upstream: true, results: map[types.NamespacedName]Verification{
			self: {Result: ResultVerified, ServerName: "x", Upstream: UpstreamProbe{Result: UpstreamReachable, StatusCode: 200, URL: "https://x/"}},
		}},
		"unreachable": {
			enabled: true, upstream: true,
			results: map[types.NamespacedName]Verification{
				self: {Result: ResultVerified, ServerName: "x", Upstream: UpstreamProbe{Result: UpstreamUnreachable, StatusCode: 504, URL: "https://x/"}},
			},
			lastGood: map[types.NamespacedName]time.Time{self: time.Now()},
		},
	} {
		t.Run(name, func(t *testing.T) {
			first := computeStatus(cr, []corev1.Pod{readyPodFixture()}, nil, "", v)
			second := computeStatus(cr, []corev1.Pod{readyPodFixture()}, first.Conditions, "", v)
			if !statusEqual(&first, &second) {
				t.Errorf("status churns on an unchanged upstream verdict:\nfirst:  %+v\nsecond: %+v",
					first.Conditions, second.Conditions)
			}
		})
	}
}

// TestComputeStatus_ConditionOrder pins the list order for a fresh CR:
// the roll-up first, then the pods, then the two probes, then the
// operator's own state — the order a reader wants to scan `kubectl
// describe` in.
func TestComputeStatus_ConditionOrder(t *testing.T) {
	cr := newRemoteApp()
	got := computeStatus(cr, []corev1.Pod{readyPodFixture()}, nil, "", stubVerifications{enabled: true, upstream: true})
	want := []string{
		accessv1alpha1.ConditionTypeReady,
		accessv1alpha1.ConditionTypeIdentityIssued,
		accessv1alpha1.ConditionTypeTunnelServing,
		accessv1alpha1.ConditionTypeTunnelVerified,
		accessv1alpha1.ConditionTypeUpstreamReachable,
		accessv1alpha1.ConditionTypeReconciled,
	}
	if len(got.Conditions) != len(want) {
		t.Fatalf("got %d conditions, want %d: %+v", len(got.Conditions), len(want), got.Conditions)
	}
	for i, c := range got.Conditions {
		if c.Type != want[i] {
			t.Errorf("conditions[%d] = %s, want %s", i, c.Type, want[i])
		}
	}
}

func TestStatusCodeText(t *testing.T) {
	if got := statusCodeText(504); got != "504 Gateway Timeout" {
		t.Errorf("statusCodeText(504) = %q", got)
	}
	if got := statusCodeText(799); got != "799" {
		t.Errorf("statusCodeText(799) = %q, want the bare number", got)
	}
}
