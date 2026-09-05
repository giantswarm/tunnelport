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
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// The reconciler's half of giantswarm/tunnelport#110 against a real API
// server: a probe outcome lands in the store, the channel nudges the
// reconciler, and the CR ends up with Ready=False / UpstreamUnreachable,
// a matching Event, and — once the outcome flips back — Ready=True and a
// recovery Event. The verifier itself is not running here (no cluster DNS
// resolves a Service FQDN in envtest); its round logic is covered by
// verify_upstream_test.go, and the pieces meet in the ATS smoke.

// publishOutcome records one outcome for cr in the shared store and nudges
// the reconciler, exactly as RunOnce does after a round.
func publishOutcome(cr *accessv1alpha1.RemoteApp, v Verification) {
	key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}
	testVerifications.Replace(map[types.NamespacedName]Verification{key: v})
	nudge := &accessv1alpha1.RemoteApp{}
	nudge.Namespace, nudge.Name = cr.Namespace, cr.Name
	testVerificationEvents <- event.TypedGenericEvent[*accessv1alpha1.RemoteApp]{Object: nudge}
}

// markPodReady flips both tunnel containers and the pod to Ready.
func markPodReady(ctx context.Context, t *testing.T, pod *corev1.Pod) {
	t.Helper()
	setPodStatus(ctx, t, pod, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodRunning
		s.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
		s.ContainerStatuses = []corev1.ContainerStatus{
			{Name: tbotContainerName, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			{Name: ghostunnelContainerName, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}
	})
}

// waitForUpstream polls until the RemoteApp's Ready and UpstreamReachable
// conditions read as wanted and returns the CR.
func waitForUpstream(ctx context.Context, t *testing.T, key types.NamespacedName, wantReady bool, wantReadyReason string, wantUpstream metav1.ConditionStatus) *accessv1alpha1.RemoteApp {
	t.Helper()
	var got *accessv1alpha1.RemoteApp
	eventually(t, func() (bool, error) {
		got = getRemoteApp(ctx, t, key)
		ready := meta.FindStatusCondition(got.Status.Conditions, accessv1alpha1.ConditionTypeReady)
		up := meta.FindStatusCondition(got.Status.Conditions, accessv1alpha1.ConditionTypeUpstreamReachable)
		if ready == nil || up == nil {
			return false, fmt.Errorf("conditions missing: %+v", got.Status.Conditions)
		}
		if got.Status.Ready != wantReady || (ready.Status == metav1.ConditionTrue) != wantReady || ready.Reason != wantReadyReason {
			return false, fmt.Errorf("ready=%v cond=%s/%s, want %v/%s", got.Status.Ready, ready.Status, ready.Reason, wantReady, wantReadyReason)
		}
		if up.Status != wantUpstream {
			return false, fmt.Errorf("UpstreamReachable=%s/%s, want %s", up.Status, up.Reason, wantUpstream)
		}
		return true, nil
	})
	return got
}

// waitForEvent polls the events.k8s.io/v1 API for an Event with the given
// reason regarding the RemoteApp. The recorder is asynchronous, hence the
// poll; events.k8s.io rather than core because that is the API the
// manager's recorder writes and therefore the group the chart's
// ClusterRole has to grant.
func waitForEvent(ctx context.Context, t *testing.T, cr *accessv1alpha1.RemoteApp, reason string) eventsv1.Event {
	t.Helper()
	var found eventsv1.Event
	eventually(t, func() (bool, error) {
		list := &eventsv1.EventList{}
		if err := testClient.List(ctx, list, client.InNamespace(cr.Namespace)); err != nil {
			return false, err
		}
		for _, ev := range list.Items {
			if ev.Regarding.Kind == "RemoteApp" && ev.Regarding.Name == cr.Name && ev.Reason == reason {
				found = ev
				return true, nil
			}
		}
		return false, fmt.Errorf("no %s Event for %s yet (%d events in namespace)", reason, cr.Name, len(list.Items))
	})
	return found
}

func TestStatus_UpstreamUnreachableFoldsIntoReadyAndEmitsEvents(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "upstream")
	key := client.ObjectKeyFromObject(cr)
	pod := makePod(ctx, t, ns, cr.Name, "upstream-pod")
	markPodReady(ctx, t, pod)

	// Pods Ready, nothing probed yet: Ready=True, upstream Unknown.
	waitForUpstream(ctx, t, key, true, reasonTunnelReady, metav1.ConditionUnknown)

	fqdn := serviceFQDN(ns, cr.Name, DefaultClusterDomain)
	url := "https://" + fqdn + ":8443/"

	// A good round first, so the failure below has a "last good probe".
	publishOutcome(cr, Verification{
		Result: ResultVerified, ServerName: fqdn,
		Upstream: UpstreamProbe{Result: UpstreamReachable, StatusCode: 401, URL: url},
	})
	got := waitForUpstream(ctx, t, key, true, reasonTunnelReady, metav1.ConditionTrue)
	up := meta.FindStatusCondition(got.Status.Conditions, accessv1alpha1.ConditionTypeUpstreamReachable)
	if !strings.Contains(up.Message, "401 Unauthorized") || !strings.Contains(up.Message, url) {
		t.Errorf("reachable message = %q, want the status and URL", up.Message)
	}

	// The incident: handshake verified, 504 behind it.
	publishOutcome(cr, Verification{
		Result: ResultVerified, ServerName: fqdn,
		Upstream: UpstreamProbe{Result: UpstreamUnreachable, StatusCode: 504, URL: url},
	})
	got = waitForUpstream(ctx, t, key, false, reasonUpstreamUnreachable, metav1.ConditionFalse)
	up = meta.FindStatusCondition(got.Status.Conditions, accessv1alpha1.ConditionTypeUpstreamReachable)
	for _, want := range []string{"504 Gateway Timeout", url, "last good probe 20"} {
		if !strings.Contains(up.Message, want) {
			t.Errorf("unreachable message = %q, want it to contain %q", up.Message, want)
		}
	}
	if got.Status.LastError != up.Message {
		t.Errorf("lastError = %q, want the upstream diagnosis", got.Status.LastError)
	}
	// The pod-level conditions must still be True: this is the incident's
	// shape, and the test is only worth having if it reproduces it.
	for _, other := range []string{accessv1alpha1.ConditionTypeIdentityIssued, accessv1alpha1.ConditionTypeTunnelServing, accessv1alpha1.ConditionTypeTunnelVerified} {
		if c := meta.FindStatusCondition(got.Status.Conditions, other); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("%s = %+v, want True while the upstream is down", other, c)
		}
	}

	warning := waitForEvent(ctx, t, cr, reasonUpstreamUnreachable)
	if warning.Type != corev1.EventTypeWarning {
		t.Errorf("UpstreamUnreachable Event type = %q, want Warning", warning.Type)
	}
	if !strings.Contains(warning.Note, "504") {
		t.Errorf("Event note = %q, want the status", warning.Note)
	}
	if warning.Action != eventActionProbe {
		t.Errorf("Event action = %q, want %q", warning.Action, eventActionProbe)
	}

	// The tunnel pods roll in the middle of the outage: a round finds no
	// Ready pod, the verdict goes Unknown, Ready follows the pods. This is
	// the shape the ATS smoke produces when it swaps the fake upstream, and
	// it must not swallow the recovery Event below.
	publishOutcome(cr, Verification{
		Result: ResultNotReady, ServerName: fqdn,
		Upstream: UpstreamProbe{Result: UpstreamNotProbed},
	})
	waitForUpstream(ctx, t, key, true, reasonTunnelReady, metav1.ConditionUnknown)

	// Recovery: back to Ready with a Normal Event, despite the Unknown in
	// between.
	publishOutcome(cr, Verification{
		Result: ResultVerified, ServerName: fqdn,
		Upstream: UpstreamProbe{Result: UpstreamReachable, StatusCode: 200, URL: url},
	})
	got = waitForUpstream(ctx, t, key, true, reasonTunnelReady, metav1.ConditionTrue)
	if got.Status.LastError != "" {
		t.Errorf("lastError = %q after recovery, want empty", got.Status.LastError)
	}
	recovered := waitForEvent(ctx, t, cr, reasonUpstreamReachable)
	if recovered.Type != corev1.EventTypeNormal {
		t.Errorf("UpstreamReachable Event type = %q, want Normal", recovered.Type)
	}
}
