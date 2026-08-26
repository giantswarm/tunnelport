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
							Message: `MountVolume.SetUp failed for volume "tbot-config" : configmap "myapp" not found`,
						},
					},
				},
			},
		},
	}

	got := summarizePodError(pod)
	// The user has to see *why* the pod is pending. ContainerCreating on
	// its own is not enough — we must surface the volume-mount message.
	for _, want := range []string{"ContainerCreating", "configmap \"myapp\" not found"} {
		if !strings.Contains(got, want) {
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
		if !strings.Contains(got, want) {
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
	if !strings.Contains(got, "Failed") {
		t.Errorf("failed phase summary: want %q in %q", "Failed", got)
	}
	if !strings.Contains(got, "Evicted") {
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
	if !strings.Contains(msg, "ContainerCreating") {
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

// TestSummarizeStatus_TerminatingPodIsExcludedFromReadyCount pins
// Issue #10: during a rolling update with maxSurge=1/maxUnavailable=0
// the kubelet can leave PodReady=True on a pod that already has
// DeletionTimestamp set. Counting that pod as Ready would let
// status.ready briefly lie. The summary must also be drawn from the
// non-terminating set, otherwise lastError would describe a pod that
// is on its way out instead of the one we'll actually be running.
func TestSummarizeStatus_TerminatingPodIsExcludedFromReadyCount(t *testing.T) {
	now := metav1.Now()
	terminatingReady := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "terminating",
			DeletionTimestamp: &now,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "tbot", Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
	pendingNew := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "new"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "tbot", Ready: false, State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
				}},
			},
		},
	}

	ready, msg := summarizeStatus([]corev1.Pod{terminatingReady, pendingNew})
	if ready {
		t.Errorf("terminating Ready pod must not count toward Ready: msg=%q", msg)
	}
	if !strings.Contains(msg, "ContainerCreating") {
		t.Errorf("summary must come from the non-terminating pod (ContainerCreating), got %q", msg)
	}
}

// TestSummarizeStatus_PicksHighestSeverityRegardlessOfOrder pins
// Issue #11: lastError must depend on pod state, not slice order. With
// CrashLoopBackOff, ContainerCreating, and ImagePullBackOff in a
// non-canonical order, ImagePullBackOff (highest severity) must win.
func TestSummarizeStatus_PicksHighestSeverityRegardlessOfOrder(t *testing.T) {
	crash := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "b-crash"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "tbot", RestartCount: 3,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				},
			}},
		},
	}
	creating := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "a-creating"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "tbot",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
				},
			}},
		},
	}
	imgPull := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "c-imgpull"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "tbot",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason: "ImagePullBackOff", Message: "Back-off pulling image",
					},
				},
			}},
		},
	}

	// Try several non-canonical orders; ImagePullBackOff must win every time.
	orderings := [][]corev1.Pod{
		{crash, creating, imgPull},
		{imgPull, crash, creating},
		{creating, imgPull, crash},
	}
	for i, pods := range orderings {
		ready, msg := summarizeStatus(pods)
		if ready {
			t.Fatalf("ordering %d: no pod is Ready, got ready=true", i)
		}
		if !strings.Contains(msg, "ImagePullBackOff") {
			t.Errorf("ordering %d: want ImagePullBackOff to win, got %q", i, msg)
		}
	}
}

// A tunnel pod runs tbot plus the ghostunnel sidecar, and kubelet orders
// ContainerStatuses alphabetically, so "ghostunnel" precedes "tbot". When
// both are unready, lastError must name tbot: ghostunnel exits only because
// tbot never wrote the SVID. Pins giantswarm/giantswarm#37445 item 3.

// tunnelPod builds a two-container tunnel pod whose statuses arrive in
// kubelet's alphabetical order, each container waiting with the given reason.
// An empty reason marks that container Ready.
func tunnelPod(name, ghostunnelReason, tbotReason string) corev1.Pod {
	container := func(cName, reason string, restarts int32) corev1.ContainerStatus {
		if reason == "" {
			return corev1.ContainerStatus{
				Name: cName, Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}
		}
		return corev1.ContainerStatus{
			Name: cName, Ready: false, RestartCount: restarts,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{
					Reason:  reason,
					Message: "back-off 5m0s restarting failed container=" + cName,
				},
			},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1},
			},
		}
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				container("ghostunnel", ghostunnelReason, 2474),
				container("tbot", tbotReason, 2470),
			},
		},
	}
}

func TestSummarizePodError_PrefersTbotOverSidecar(t *testing.T) {
	tests := []struct {
		name             string
		ghostunnelReason string
		tbotReason       string
		wantContainer    string
	}{
		{
			name:             "both unready names tbot",
			ghostunnelReason: reasonCrashLoopBackOff,
			tbotReason:       reasonCrashLoopBackOff,
			wantContainer:    "tbot",
		},
		{
			name:             "tbot unready alone names tbot",
			ghostunnelReason: "",
			tbotReason:       reasonCrashLoopBackOff,
			wantContainer:    "tbot",
		},
		{
			name:             "sidecar unready alone names the sidecar",
			ghostunnelReason: reasonCrashLoopBackOff,
			tbotReason:       "",
			wantContainer:    "ghostunnel",
		},
		{
			name:             "tbot wins even with a lower-severity reason",
			ghostunnelReason: "ImagePullBackOff",
			tbotReason:       reasonContainerCreating,
			wantContainer:    "tbot",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := tunnelPod("mcp-kubernetes-garm-6f9cdfd5b-5fdzc", tc.ghostunnelReason, tc.tbotReason)

			got := summarizePodError(&pod)
			want := "container=" + tc.wantContainer
			if !strings.Contains(got, want) {
				t.Errorf("summarizePodError: want substring %q in %q", want, got)
			}
			// The restart counts differ per container, so the count in the
			// message is a second, independent check on which one was picked.
			if tc.wantContainer == "tbot" && !strings.Contains(got, "2470 restarts") {
				t.Errorf("summarizePodError: want tbot's restart count in %q", got)
			}
		})
	}
}

// The severity ranking reads the same tbot-first order. With ghostunnel at
// the higher-severity reason, the ranked reason must still be tbot's.
func TestPodErrorReason_PrefersTbotOverSidecar(t *testing.T) {
	pod := tunnelPod("mcp-kubernetes-garm-6f9cdfd5b-5fdzc", "ImagePullBackOff", reasonCrashLoopBackOff)

	if got, want := podErrorReason(&pod), reasonCrashLoopBackOff; got != want {
		t.Errorf("podErrorReason: got %q, want %q", got, want)
	}
}

// A pod with neither container named tbot must still summarise, rather than
// fall through to the phase-level branch.
func TestSummarizePodError_NoTbotContainerStillSummarises(t *testing.T) {
	pod := corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "ghostunnel", Ready: false,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoopBackOff},
				},
			}},
		},
	}

	if got := summarizePodError(&pod); !strings.Contains(got, reasonCrashLoopBackOff) {
		t.Errorf("summarizePodError: want %q in %q", reasonCrashLoopBackOff, got)
	}
}

// The two per-role conditions exist so both halves of a tunnel failure are
// visible at once. The ordering they encode is what #37445 item 3 was about:
// the TLS listener cannot bind without the SVID, so IdentityIssued=False
// alongside TunnelServing=False means the join is the cause.

func conditionByType(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

func TestComputeStatus_PerRoleConditions(t *testing.T) {
	cr := newRemoteApp()

	tests := []struct {
		name            string
		pods            []corev1.Pod
		wantIdentity    metav1.ConditionStatus
		wantServing     metav1.ConditionStatus
		wantIdentityMsg string
		wantServingMsg  string
	}{
		{
			name:            "both containers crashlooping blames the join",
			pods:            []corev1.Pod{tunnelPod("p", reasonCrashLoopBackOff, reasonCrashLoopBackOff)},
			wantIdentity:    metav1.ConditionFalse,
			wantServing:     metav1.ConditionFalse,
			wantIdentityMsg: "2470 restarts",
			wantServingMsg:  "2474 restarts",
		},
		{
			name:            "identity up and the listener down is a real TLS fault",
			pods:            []corev1.Pod{tunnelPod("p", reasonCrashLoopBackOff, "")},
			wantIdentity:    metav1.ConditionTrue,
			wantServing:     metav1.ConditionFalse,
			wantIdentityMsg: "usable identity",
			wantServingMsg:  "2474 restarts",
		},
		{
			name:            "identity down while the listener still serves",
			pods:            []corev1.Pod{tunnelPod("p", "", reasonCrashLoopBackOff)},
			wantIdentity:    metav1.ConditionFalse,
			wantServing:     metav1.ConditionTrue,
			wantIdentityMsg: "2470 restarts",
			wantServingMsg:  "accepting connections",
		},
		{
			name:            "no pods reports both as false",
			pods:            nil,
			wantIdentity:    metav1.ConditionFalse,
			wantServing:     metav1.ConditionFalse,
			wantIdentityMsg: noTbotPodsMsg,
			wantServingMsg:  noTbotPodsMsg,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeStatus(cr, tc.pods, nil, "", nil)

			identity := conditionByType(got.Conditions, accessv1alpha1.ConditionTypeIdentityIssued)
			serving := conditionByType(got.Conditions, accessv1alpha1.ConditionTypeTunnelServing)
			if identity == nil || serving == nil {
				t.Fatalf("want both per-role conditions, got %v", got.Conditions)
			}
			if identity.Status != tc.wantIdentity {
				t.Errorf("IdentityIssued: got %q, want %q (message %q)", identity.Status, tc.wantIdentity, identity.Message)
			}
			if serving.Status != tc.wantServing {
				t.Errorf("TunnelServing: got %q, want %q (message %q)", serving.Status, tc.wantServing, serving.Message)
			}
			if !strings.Contains(identity.Message, tc.wantIdentityMsg) {
				t.Errorf("IdentityIssued message: want %q in %q", tc.wantIdentityMsg, identity.Message)
			}
			if !strings.Contains(serving.Message, tc.wantServingMsg) {
				t.Errorf("TunnelServing message: want %q in %q", tc.wantServingMsg, serving.Message)
			}
		})
	}
}

// A pod that has not created its containers yet reports neither role, which
// is Unknown rather than False: the operator has nothing to say, and that is
// not the same as a failure.
func TestComputeStatus_PerRoleConditionsUnknownBeforeContainerStatuses(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}

	got := computeStatus(newRemoteApp(), []corev1.Pod{pod}, nil, "", nil)
	for _, ct := range []string{
		accessv1alpha1.ConditionTypeIdentityIssued,
		accessv1alpha1.ConditionTypeTunnelServing,
	} {
		cond := conditionByType(got.Conditions, ct)
		if cond == nil {
			t.Fatalf("%s: condition missing", ct)
		}
		if cond.Status != metav1.ConditionUnknown {
			t.Errorf("%s: got %q, want Unknown (message %q)", ct, cond.Status, cond.Message)
		}
	}
}

// computeStatus is the pure end-to-end synthesis: pods → full
// RemoteAppStatus. Driven from a table; pins the exact Reason strings
// automation pattern-matches on. Per ADR 0004 computeStatus emits two
// conditions: `Ready` (join-level) and `Reconciled` (operator-internal).
// The cases here exercise happy-path Reconciled=True; the
// ReconcileError path is covered by
// TestComputeStatus_ReconciledFalseOnApplyError below.

func TestComputeStatus(t *testing.T) {
	// computeStatus consumes only ObjectMeta from the CR; the rest of
	// the fixture's defaults are inert here. Bumping Generation to 7
	// inline rather than carrying a named option for it — only this
	// one test needs that knob.
	cr := newRemoteApp(withName("demo", "ra"), withTokenName("tok"))
	cr.Generation = 7

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
		wantReady     bool
		wantLastError string // substring match
		wantReadyRsn  string
	}{
		{name: "no pods", pods: nil,
			wantReady: false, wantLastError: "no tbot pods", wantReadyRsn: "NoPods"},
		{name: "ready pod", pods: []corev1.Pod{readyPod},
			wantReady: true, wantLastError: "", wantReadyRsn: "TunnelReady"},
		{name: "crashloop pod", pods: []corev1.Pod{crashPod},
			wantReady: false, wantLastError: "CrashLoopBackOff", wantReadyRsn: "PodNotReady"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeStatus(cr, tc.pods, nil, "", nil)
			if got.Ready != tc.wantReady {
				t.Errorf("Ready: want %v, got %v", tc.wantReady, got.Ready)
			}
			if !strings.Contains(got.LastError, tc.wantLastError) {
				t.Errorf("LastError: want substring %q, got %q", tc.wantLastError, got.LastError)
			}
			if got.ObservedGeneration != cr.Generation {
				t.Errorf("ObservedGeneration: want %d, got %d", cr.Generation, got.ObservedGeneration)
			}
			rc := findCond(got.Conditions, accessv1alpha1.ConditionTypeReady)
			if rc == nil || rc.Reason != tc.wantReadyRsn {
				t.Errorf("Ready condition reason: want %q, got %v", tc.wantReadyRsn, rc)
			}
			// Reconciled=True on the happy paths: apply succeeded
			// (applyErrSummary=""), so every case in this table must
			// see Reconciled=True with reason ReconcileSucceeded.
			rec := findCond(got.Conditions, accessv1alpha1.ConditionTypeReconciled)
			if rec == nil {
				t.Fatalf("Reconciled condition missing: %+v", got.Conditions)
			}
			if rec.Status != metav1.ConditionTrue {
				t.Errorf("Reconciled status: want True, got %s", rec.Status)
			}
			if rec.Reason != "ReconcileSucceeded" {
				t.Errorf("Reconciled reason: want ReconcileSucceeded, got %q", rec.Reason)
			}
		})
	}
}

// TestComputeStatus_ReconciledFalseOnApplyError pins that when the
// reconciler passes an apply-error summary, computeStatus emits
// Reconciled=False with Reason=ReconcileError and the summary as the
// Message. The Ready condition is independent of this — `Ready` reflects
// tbot pod state, `Reconciled` reflects operator-internal state.
func TestComputeStatus_ReconciledFalseOnApplyError(t *testing.T) {
	cr := newRemoteApp(withName("demo", "ra"), withTokenName("tok"))
	cr.Generation = 3

	readyPod := corev1.Pod{Status: corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}}

	got := computeStatus(cr, []corev1.Pod{readyPod}, nil, "reconcile Deployment: forbidden", nil)

	// Ready is independent of apply: the previously-applied tbot pod is
	// up, so Ready stays True even though this pass failed to apply.
	if !got.Ready {
		t.Errorf("Ready: want true (tbot pod is Ready independent of apply), got false")
	}
	rec := findCond(got.Conditions, accessv1alpha1.ConditionTypeReconciled)
	if rec == nil {
		t.Fatalf("Reconciled condition missing: %+v", got.Conditions)
	}
	if rec.Status != metav1.ConditionFalse {
		t.Errorf("Reconciled status: want False, got %s", rec.Status)
	}
	if rec.Reason != "ReconcileError" {
		t.Errorf("Reconciled reason: want ReconcileError, got %q", rec.Reason)
	}
	if !strings.Contains(rec.Message, "reconcile Deployment: forbidden") {
		t.Errorf("Reconciled message: want substring %q, got %q", "reconcile Deployment: forbidden", rec.Message)
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
