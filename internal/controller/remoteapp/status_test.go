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
