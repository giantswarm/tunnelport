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

// stubVerifications is a VerificationReader with fixed answers, so the
// condition logic can be tested without a prober. `upstream` is off unless
// a test says otherwise, so the TunnelVerified rows below keep exercising
// the surface an install without the HTTP probe has.
type stubVerifications struct {
	enabled  bool
	upstream bool
	results  map[types.NamespacedName]Verification
	lastGood map[types.NamespacedName]time.Time
}

func (s stubVerifications) Enabled() bool { return s.enabled }

func (s stubVerifications) UpstreamProbeEnabled() bool { return s.enabled && s.upstream }

func (s stubVerifications) Result(key types.NamespacedName) (Verification, bool) {
	v, ok := s.results[key]
	return v, ok
}

func (s stubVerifications) LastUpstreamSuccess(key types.NamespacedName) (time.Time, bool) {
	t, ok := s.lastGood[key]
	return t, ok
}

// readyPodFixture is a tunnel pod with both containers ready, so every
// pre-existing condition comes out True and the only thing under test is
// TunnelVerified.
func readyPodFixture() corev1.Pod {
	return corev1.Pod{
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: tbotContainerName, Ready: true},
				{Name: ghostunnelContainerName, Ready: true},
			},
		},
	}
}

// TestComputeStatus_TunnelVerifiedCondition walks every outcome the
// verifier can produce, plus the two "nothing to say" cases.
//
// The row that matters most is "cert_invalid on a fully Ready tunnel":
// that is giantswarm/giantswarm#37521 in one assertion — Ready,
// IdentityIssued and TunnelServing all True while TunnelVerified is
// False. Before this condition existed, that state was indistinguishable
// from a healthy tunnel from anywhere inside Kubernetes.
func TestComputeStatus_TunnelVerifiedCondition(t *testing.T) {
	cr := newRemoteApp()
	self := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}
	fqdn := serviceFQDN(cr.Namespace, cr.Name, DefaultClusterDomain)

	tests := []struct {
		name string
		// verifications is the reader handed to computeStatus.
		verifications VerificationReader
		// wantPresent false means the condition must be absent entirely.
		wantPresent bool
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name:          "verification not wired",
			verifications: nil,
			wantPresent:   false,
		},
		{
			name:          "verification disabled",
			verifications: stubVerifications{enabled: false},
			wantPresent:   false,
		},
		{
			name:          "no result yet",
			verifications: stubVerifications{enabled: true},
			wantPresent:   true,
			wantStatus:    metav1.ConditionUnknown,
			wantReason:    reasonVerificationPending,
		},
		{
			name: "verified",
			verifications: stubVerifications{enabled: true, results: map[types.NamespacedName]Verification{
				self: {Result: ResultVerified, ServerName: fqdn},
			}},
			wantPresent: true,
			wantStatus:  metav1.ConditionTrue,
			wantReason:  reasonCertificateVerified,
			wantMessage: fqdn,
		},
		{
			// The incident's state.
			name: "certificate invalid",
			verifications: stubVerifications{enabled: true, results: map[types.NamespacedName]Verification{
				self: {
					Result:     ResultCertInvalid,
					Detail:     "served certificate is not valid for " + fqdn + " (SAN mismatch): presented other.ns.svc.cluster.local",
					ServerName: fqdn,
				},
			}},
			wantPresent: true,
			wantStatus:  metav1.ConditionFalse,
			wantReason:  reasonCertificateInvalid,
			wantMessage: "SAN mismatch",
		},
		{
			name: "unreachable",
			verifications: stubVerifications{enabled: true, results: map[types.NamespacedName]Verification{
				self: {
					Result:     ResultUnreachable,
					Detail:     "cannot connect to " + fqdn + ":8443: connection refused",
					ServerName: fqdn,
				},
			}},
			wantPresent: true,
			wantStatus:  metav1.ConditionFalse,
			wantReason:  reasonTunnelUnreachable,
			wantMessage: "cannot connect",
		},
		{
			// Not probed, so not judged. Reported as Unknown rather than
			// False: "I have not checked" must never look like "the
			// certificate is bad", or the check would cry wolf on every
			// rollout.
			name: "not ready",
			verifications: stubVerifications{enabled: true, results: map[types.NamespacedName]Verification{
				self: {Result: ResultNotReady, ServerName: fqdn},
			}},
			wantPresent: true,
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  reasonNotVerifiedNotReady,
		},
		{
			// Defensive branch: a result value added without updating the
			// switch must surface as unclassified, never as a pass.
			name: "unknown result value",
			verifications: stubVerifications{enabled: true, results: map[types.NamespacedName]Verification{
				self: {Result: VerificationResult("something_new"), ServerName: fqdn},
			}},
			wantPresent: true,
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  reasonVerificationPending,
			wantMessage: "unclassified",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeStatus(cr, []corev1.Pod{readyPodFixture()}, nil, "", tc.verifications)

			cond := conditionByType(got.Conditions, accessv1alpha1.ConditionTypeTunnelVerified)
			if !tc.wantPresent {
				if cond != nil {
					t.Fatalf("TunnelVerified present (%+v), want absent", *cond)
				}
				return
			}
			if cond == nil {
				t.Fatal("TunnelVerified condition missing")
			}
			if cond.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", cond.Status, tc.wantStatus)
			}
			if cond.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", cond.Reason, tc.wantReason)
			}
			if tc.wantMessage != "" && !strings.Contains(cond.Message, tc.wantMessage) {
				t.Errorf("Message = %q, want it to contain %q", cond.Message, tc.wantMessage)
			}

			// The load-bearing part: the pre-existing conditions must be
			// unaffected. TunnelVerified is additive — it does not soften
			// Ready, and Ready does not soften it.
			for _, other := range []string{
				accessv1alpha1.ConditionTypeReady,
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
			if !got.Ready {
				t.Error("status.ready is false; the verification result must not feed it")
			}
		})
	}
}

// TestComputeStatus_DisablingVerificationRemovesStaleCondition pins the
// removal path. A permanent Unknown on every CR after the feature is
// turned off would be worse than no condition at all — and an install
// that flips it off must end up with exactly the status shape it had
// before the feature existed.
func TestComputeStatus_DisablingVerificationRemovesStaleCondition(t *testing.T) {
	cr := newRemoteApp()
	self := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}

	enabled := stubVerifications{enabled: true, results: map[types.NamespacedName]Verification{
		self: {Result: ResultCertInvalid, Detail: "bad", ServerName: "x"},
	}}
	with := computeStatus(cr, []corev1.Pod{readyPodFixture()}, nil, "", enabled)
	if conditionByType(with.Conditions, accessv1alpha1.ConditionTypeTunnelVerified) == nil {
		t.Fatal("setup: TunnelVerified missing while enabled")
	}

	// Feed the previous conditions back in with verification off, which is
	// what a chart upgrade setting verification.enabled=false produces.
	without := computeStatus(cr, []corev1.Pod{readyPodFixture()}, with.Conditions, "", stubVerifications{enabled: false})
	if cond := conditionByType(without.Conditions, accessv1alpha1.ConditionTypeTunnelVerified); cond != nil {
		t.Errorf("TunnelVerified survived disabling: %+v", *cond)
	}
}

// TestComputeStatus_VerifiedConditionIsStableAcrossReconciles guards the
// hot-loop suppression in reconcileStatus: an unchanged verification
// result must produce a status statusEqual considers identical, or every
// reconcile pass would patch and re-trigger itself.
func TestComputeStatus_VerifiedConditionIsStableAcrossReconciles(t *testing.T) {
	cr := newRemoteApp()
	self := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}
	v := stubVerifications{enabled: true, results: map[types.NamespacedName]Verification{
		self: {Result: ResultVerified, ServerName: serviceFQDN(cr.Namespace, cr.Name, DefaultClusterDomain)},
	}}

	first := computeStatus(cr, []corev1.Pod{readyPodFixture()}, nil, "", v)
	second := computeStatus(cr, []corev1.Pod{readyPodFixture()}, first.Conditions, "", v)
	if !statusEqual(&first, &second) {
		t.Errorf("status churns on an unchanged verification result:\nfirst:  %+v\nsecond: %+v",
			first.Conditions, second.Conditions)
	}
}
