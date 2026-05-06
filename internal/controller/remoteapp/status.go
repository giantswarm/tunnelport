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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// status.go: pure helpers that derive RemoteApp.status content from
// k8s-visible Pod state. Per ADR 0003, none of these helpers may read
// pod logs or accept log content as input — only metadata.

// noTbotPodsMsg is the lastError string returned when the operator's
// Pod list for a RemoteApp is empty — kept as a constant so tests and
// the reasoner agree on the exact wording, and so lint's goconst rule
// is satisfied.
const noTbotPodsMsg = "no tbot pods"

// summarizeStatus picks the representative pod from a tbot pod list and
// returns (ready, lastError). At-least-one-ready wins: if any pod has
// PodReady=True (which itself is wired to tbot's diag /readyz), the
// RemoteApp is Ready and lastError is empty. Otherwise we surface the
// most-informative error from the pod set.
//
// Picking strategy when no pod is Ready: prefer the pod whose
// summarizePodError yields a non-empty string; fall back to the first
// pod's summary. With replicas typically = 1, this is just "the pod".
func summarizeStatus(pods []corev1.Pod) (bool, string) {
	if len(pods) == 0 {
		return false, noTbotPodsMsg
	}
	for i := range pods {
		if isPodReady(&pods[i]) {
			return true, ""
		}
	}
	// All unready: pick the first pod with a non-empty summary, else the
	// first pod. summarizePodError(nil) is reserved for "no pods at all".
	for i := range pods {
		if msg := summarizePodError(&pods[i]); msg != "" {
			return false, msg
		}
	}
	return false, summarizePodError(&pods[0])
}

// summarizePodError turns a single pod's k8s-visible state into a one-
// line lastError string. Empty string means the pod is healthy.
//
// Format conventions (pinned by tests):
//   - "CrashLoopBackOff (5 restarts), last termination: Error (137)"
//   - "ContainerCreating: <volume mount message>"
//   - "Failed: Evicted"
//   - "" when Ready.
func summarizePodError(pod *corev1.Pod) string {
	if pod == nil {
		return noTbotPodsMsg
	}
	if isPodReady(pod) {
		return ""
	}

	// Container-level state is the most informative input we have when
	// the pod is unready. Walk the container statuses and build a
	// summary from the first one that's not Ready.
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Ready {
			continue
		}
		if msg := containerWaitingSummary(cs); msg != "" {
			return msg
		}
		if msg := containerTerminatedSummary(cs); msg != "" {
			return msg
		}
	}

	// Fall back to phase-level information. Failed/Evicted come through
	// here when no container state survives.
	if pod.Status.Phase == corev1.PodFailed {
		reason := pod.Status.Reason
		if reason == "" {
			reason = "Failed"
		}
		return fmt.Sprintf("Failed: %s", reason)
	}
	if pod.Status.Phase != "" {
		return string(pod.Status.Phase)
	}
	return "unknown pod state"
}

// isPodReady returns true iff the PodReady condition is True. The
// reconciler relies on this being the same signal that the readiness
// probe (wired to tbot's diag /readyz) flips.
func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// containerWaitingSummary builds the "<Reason>(, N restarts)?(, last
// termination: ...)?: <Message>" string for a Waiting container. The
// CrashLoopBackOff path attaches restart count and last termination
// reason because that's the actionable signal a platform engineer
// needs to decide whether to look at logs.
func containerWaitingSummary(cs *corev1.ContainerStatus) string {
	w := cs.State.Waiting
	if w == nil {
		return ""
	}
	parts := []string{w.Reason}
	if cs.RestartCount > 0 {
		parts[0] = fmt.Sprintf("%s (%d restarts)", w.Reason, cs.RestartCount)
	}
	if t := cs.LastTerminationState.Terminated; t != nil {
		parts = append(parts, fmt.Sprintf("last termination: %s (%d)", t.Reason, t.ExitCode))
	}
	out := strings.Join(parts, ", ")
	if w.Message != "" {
		// We include the kubelet's mount/pull/etc message verbatim — that's
		// k8s-visible state, not log content (the logs of the failing
		// container are off-limits per ADR 0003).
		out = fmt.Sprintf("%s: %s", out, w.Message)
	}
	return out
}

// containerTerminatedSummary handles a non-Waiting, non-Ready container
// — typically right after a crash but before kubelet schedules the next
// restart and flips it back to Waiting/CrashLoopBackOff.
func containerTerminatedSummary(cs *corev1.ContainerStatus) string {
	t := cs.State.Terminated
	if t == nil {
		return ""
	}
	return fmt.Sprintf("Terminated: %s (%d)", t.Reason, t.ExitCode)
}
