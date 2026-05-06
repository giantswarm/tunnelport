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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// summarizePodError is the pure helper that turns a pod's k8s-visible
// state (Phase / ContainerStatuses / RestartCount / last termination
// reason) into a one-line status.lastError string. Per ADR 0003 it must
// not consult log content, so all input here is metadata only.
//
// These cases pin the strings that surface in `kubectl get remoteapp`'s
// LastError column. Re-wording any message is a behavior change.

func TestSummarizePodError_HealthyReadyPodReturnsEmptyString(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "tbot", Ready: true, RestartCount: 0,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}

	if got := summarizePodError(pod); got != "" {
		t.Errorf("healthy pod: want empty string, got %q", got)
	}
}

func TestSummarizePodError_PendingWithVolumeMountFailureSurfacesReason(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{
					Type:    corev1.PodScheduled,
					Status:  corev1.ConditionTrue,
					Reason:  "PodScheduled",
					Message: "",
				},
				{
					Type:    corev1.ContainersReady,
					Status:  corev1.ConditionFalse,
					Reason:  "ContainersNotReady",
					Message: `containers with unready status: [tbot]`,
				},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "tbot",
					Ready: false,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "ContainerCreating",
							Message: `MountVolume.SetUp failed for volume "tbot-token" : secret "myapp-token" not found`,
						},
					},
				},
			},
		},
	}

	got := summarizePodError(pod)
	// The user has to see *why* the pod is pending. ContainerCreating on
	// its own is not enough — we must surface the volume-mount message.
	for _, want := range []string{"ContainerCreating", "secret \"myapp-token\" not found"} {
		if !contains(got, want) {
			t.Errorf("pending volume-mount: want substring %q in %q", want, got)
		}
	}
}

func TestSummarizePodError_CrashLoopBackOffIncludesRestartCountAndTermination(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "tbot",
					Ready:        false,
					RestartCount: 5,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "CrashLoopBackOff",
							Message: "back-off 5m0s restarting failed container",
						},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:   "Error",
							ExitCode: 137,
						},
					},
				},
			},
		},
	}

	got := summarizePodError(pod)
	for _, want := range []string{
		"CrashLoopBackOff",
		"5 restarts",
		"Error",
		"137",
	} {
		if !contains(got, want) {
			t.Errorf("crashloop summary: want substring %q in %q", want, got)
		}
	}
}

func TestSummarizePodError_FailedPhaseSurfacesPhaseAndReason(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase:   corev1.PodFailed,
			Reason:  "Evicted",
			Message: "Pod ephemeral local storage usage exceeds the total limit",
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "tbot", Ready: false, RestartCount: 0,
					State: corev1.ContainerState{}},
			},
		},
	}

	got := summarizePodError(pod)
	if !contains(got, "Failed") {
		t.Errorf("failed phase summary: want %q in %q", "Failed", got)
	}
	if !contains(got, "Evicted") {
		t.Errorf("failed phase summary: want %q in %q", "Evicted", got)
	}
}

func TestSummarizePodError_NoPodsReturnsNoPodsRunning(t *testing.T) {
	if got := summarizePodError(nil); got != "no tbot pods" {
		t.Errorf("nil pod: want %q, got %q", "no tbot pods", got)
	}
}

// summarizeStatus is the higher-level helper that picks the
// representative pod from a list. The reconciler passes every owned tbot
// pod; we want: any Ready pod => "" (healthy), otherwise the worst-error
// pod's summary.

func TestSummarizeStatus_AnyReadyPodWinsOverFailingPeer(t *testing.T) {
	healthy := corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "tbot", Ready: true},
			},
		},
	}
	crashing := corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "tbot",
					RestartCount: 12,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
				},
			},
		},
	}

	ready, msg := summarizeStatus([]corev1.Pod{crashing, healthy})
	if !ready {
		t.Errorf("at least one ready pod must win: ready=false, msg=%q", msg)
	}
	if msg != "" {
		t.Errorf("ready summary: want empty, got %q", msg)
	}
}

func TestSummarizeStatus_AllUnreadyReturnsErrorSummary(t *testing.T) {
	pending := corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "tbot", State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
				}},
			},
		},
	}

	ready, msg := summarizeStatus([]corev1.Pod{pending})
	if ready {
		t.Errorf("pending pod must not be ready")
	}
	if !contains(msg, "ContainerCreating") {
		t.Errorf("summary: want %q in %q", "ContainerCreating", msg)
	}
}

func TestSummarizeStatus_NoPodsReportsExplicitState(t *testing.T) {
	ready, msg := summarizeStatus(nil)
	if ready {
		t.Errorf("no pods cannot be ready")
	}
	if msg != "no tbot pods" {
		t.Errorf("empty pod set: want %q, got %q", "no tbot pods", msg)
	}
}

// computeStatus is the pure end-to-end synthesis: pods + token Secret view
// → full RemoteAppStatus. Driven from a table; pins the exact Reason
// strings that automation pattern-matches on.

func TestComputeStatus(t *testing.T) {
	cr := &accessv1alpha1.RemoteApp{
		ObjectMeta: metav1.ObjectMeta{Name: "ra", Namespace: "demo", Generation: 7},
		Spec: accessv1alpha1.RemoteAppSpec{
			TokenRef: accessv1alpha1.TokenRef{Name: "tok", Key: "token"},
		},
	}
	bound := TokenSecretView{Name: "tok", Key: "token", ResourceVersion: "100", KeyExists: true}
	missing := TokenSecretView{Name: "tok", Key: "token"}
	keyAbsent := TokenSecretView{Name: "tok", Key: "token", ResourceVersion: "100", KeyExists: false}
	fetchErr := TokenSecretView{Name: "tok", Key: "token", FetchErr: stubErr("api unavailable")}

	readyPod := corev1.Pod{Status: corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}}
	crashPod := corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			RestartCount: 5,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 137},
			},
		}},
	}}

	cases := []struct {
		name          string
		pods          []corev1.Pod
		view          TokenSecretView
		wantReady     bool
		wantLastError string // substring match
		wantReadyRsn  string
		wantTokenRsn  string
		wantTokenStat metav1.ConditionStatus
	}{
		{name: "no pods, secret bound", pods: nil, view: bound,
			wantReady: false, wantLastError: "no tbot pods",
			wantReadyRsn: "NoPods", wantTokenRsn: "Bound", wantTokenStat: metav1.ConditionTrue},
		{name: "ready pod, secret bound", pods: []corev1.Pod{readyPod}, view: bound,
			wantReady: true, wantLastError: "",
			wantReadyRsn: "TunnelReady", wantTokenRsn: "Bound", wantTokenStat: metav1.ConditionTrue},
		{name: "crashloop pod, secret bound", pods: []corev1.Pod{crashPod}, view: bound,
			wantReady: false, wantLastError: "CrashLoopBackOff",
			wantReadyRsn: "PodNotReady", wantTokenRsn: "Bound", wantTokenStat: metav1.ConditionTrue},
		{name: "ready pod, secret missing", pods: []corev1.Pod{readyPod}, view: missing,
			wantReady: true, wantLastError: "",
			wantReadyRsn: "TunnelReady", wantTokenRsn: "SecretNotFound", wantTokenStat: metav1.ConditionFalse},
		{name: "ready pod, key absent", pods: []corev1.Pod{readyPod}, view: keyAbsent,
			wantReady: true, wantLastError: "",
			wantReadyRsn: "TunnelReady", wantTokenRsn: "KeyNotFound", wantTokenStat: metav1.ConditionFalse},
		{name: "no pods, secret fetch error", pods: nil, view: fetchErr,
			wantReady: false, wantLastError: "no tbot pods",
			wantReadyRsn: "NoPods", wantTokenRsn: "SecretGetError", wantTokenStat: metav1.ConditionFalse},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeStatus(cr, tc.pods, tc.view, nil)
			if got.Ready != tc.wantReady {
				t.Errorf("Ready: want %v, got %v", tc.wantReady, got.Ready)
			}
			if !contains(got.LastError, tc.wantLastError) {
				t.Errorf("LastError: want substring %q, got %q", tc.wantLastError, got.LastError)
			}
			if got.ObservedGeneration != cr.Generation {
				t.Errorf("ObservedGeneration: want %d, got %d", cr.Generation, got.ObservedGeneration)
			}
			rc := findCond(got.Conditions, accessv1alpha1.ConditionTypeReady)
			if rc == nil || rc.Reason != tc.wantReadyRsn {
				t.Errorf("Ready condition reason: want %q, got %v", tc.wantReadyRsn, rc)
			}
			tc2 := findCond(got.Conditions, accessv1alpha1.ConditionTypeTokenSecretBound)
			if tc2 == nil || tc2.Reason != tc.wantTokenRsn || tc2.Status != tc.wantTokenStat {
				t.Errorf("TokenSecretBound condition: want reason=%q status=%q, got %+v",
					tc.wantTokenRsn, tc.wantTokenStat, tc2)
			}
		})
	}
}

func findCond(cs []metav1.Condition, t string) *metav1.Condition {
	for i := range cs {
		if cs[i].Type == t {
			return &cs[i]
		}
	}
	return nil
}

type stubErr string

func (e stubErr) Error() string { return string(e) }

// helpers

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
