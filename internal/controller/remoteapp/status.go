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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// status.go: pure helpers that derive RemoteApp.status content from
// k8s-visible Pod state. Per ADR 0003, none of these helpers may read
// pod logs or accept log content as input — only metadata.

// noTbotPodsMsg is the lastError string returned when the operator's
// Pod list for a RemoteApp is empty — kept as a constant so tests and
// the reasoner agree on the exact wording, and so lint's goconst rule
// is satisfied.
const noTbotPodsMsg = "no tbot pods"

// liveTbotPods returns the subset of pods that are not being torn down,
// i.e. pods whose DeletionTimestamp is nil. Pods with DeletionTimestamp
// set are mid-termination: their PodReady condition still reads
// whatever the kubelet last wrote, which during a StatefulSet rolling
// update can briefly be True even though the pod is on its way out.
// Counting those would let Ready=true lie. Both the Ready check and
// the lastError summary use the filtered set so the two signals can
// never disagree on which pods exist.
func liveTbotPods(pods []corev1.Pod) []corev1.Pod {
	out := pods[:0:0] // distinct backing array; never aliases caller's slice
	for i := range pods {
		if pods[i].DeletionTimestamp != nil {
			continue
		}
		out = append(out, pods[i])
	}
	return out
}

// summarizeStatus picks the representative pod from a tbot pod list and
// returns (ready, lastError). Pods being terminated (non-nil
// DeletionTimestamp) are filtered out first — see liveTbotPods. After
// filtering, at-least-one-ready wins: if any live pod has PodReady=True
// (which itself is wired to tbot's diag /readyz), the RemoteApp is
// Ready and lastError is empty. Otherwise we surface the highest-
// severity error from the live pod set, deterministically.
//
// Determinism matters: the reconciler hot-loop guard compares prev and
// new status, and any flap caused by pod-list ordering would either
// thrash status writes or hide real changes behind churn.
// pickWorstSummary defines the ordering.
func summarizeStatus(pods []corev1.Pod) (bool, string) {
	live := liveTbotPods(pods)
	if len(live) == 0 {
		return false, noTbotPodsMsg
	}
	for i := range live {
		if isPodReady(&live[i]) {
			return true, ""
		}
	}
	return false, pickWorstSummary(live)
}

// errorSeverity ranks pod-error reasons so that lastError reflects the
// most-actionable failure regardless of pod-list ordering. Lower
// numbers are worse. The ordering, in words:
//
//  1. ImagePullBackOff / ErrImagePull / CreateContainerConfigError —
//     misconfiguration that won't self-heal; engineer must intervene.
//  2. CrashLoopBackOff — the process keeps dying after starting, so
//     it's likely a config/auth fault rather than a transient.
//  3. Error / OOMKilled — a single termination, possibly transient,
//     possibly the precursor to (2).
//  4. ContainerCreating / PodInitializing — pulls/mounts in flight,
//     usually transient unless stuck.
//  5. anything else — unranked tail bucket.
//
// Within a tier, ties break on pod name (lexicographic) so the picked
// summary is stable across reconciles.
func errorSeverity(reason string) int {
	switch reason {
	case "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError":
		return 1
	case "CrashLoopBackOff":
		return 2
	case "Error", "OOMKilled":
		return 3
	case "ContainerCreating", "PodInitializing":
		return 4
	default:
		return 5
	}
}

// podErrorReason extracts the canonical reason string used for severity
// ranking. Mirrors summarizePodError's source-of-truth: a Waiting
// container's Reason wins; a Terminated container's Reason is next; the
// pod Phase is the fallback. Empty string means "nothing to rank on".
func podErrorReason(pod *corev1.Pod) string {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Ready {
			continue
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return cs.State.Terminated.Reason
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		if pod.Status.Reason != "" {
			return pod.Status.Reason
		}
		return "Failed"
	}
	return string(pod.Status.Phase)
}

// pickWorstSummary chooses one summary from a non-empty slice of unready
// pods, ordered by errorSeverity then by pod name. Pods whose
// summarizePodError is empty (shouldn't happen for unready pods, but
// guard anyway) are skipped; if everything is empty, fall back to the
// lexicographically-first pod's summary so the choice is still stable.
func pickWorstSummary(pods []corev1.Pod) string {
	idx := make([]int, len(pods))
	for i := range pods {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		pa, pb := &pods[idx[a]], &pods[idx[b]]
		sa, sb := errorSeverity(podErrorReason(pa)), errorSeverity(podErrorReason(pb))
		if sa != sb {
			return sa < sb
		}
		return pa.Name < pb.Name
	})
	for _, i := range idx {
		if msg := summarizePodError(&pods[i]); msg != "" {
			return msg
		}
	}
	return summarizePodError(&pods[idx[0]])
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

// computeStatus derives the full RemoteAppStatus from k8s-visible inputs.
// Pure: no I/O, no logging. meta.SetStatusCondition uses metav1.Now() for
// LastTransitionTime — that's library-imposed and stripped by statusEqual.
func computeStatus(cr *accessv1alpha1.RemoteApp, pods []corev1.Pod, view TokenSecretView, prevConditions []metav1.Condition) accessv1alpha1.RemoteAppStatus {
	ready, lastError := summarizeStatus(pods)

	conditions := append([]metav1.Condition(nil), prevConditions...)

	readyCond := metav1.Condition{
		Type:               accessv1alpha1.ConditionTypeReady,
		Status:             boolToConditionStatus(ready),
		ObservedGeneration: cr.Generation,
		Reason:             readyConditionReason(ready, lastError),
		Message:            lastError,
	}
	if ready {
		readyCond.Message = "tbot tunnel ready"
	}
	meta.SetStatusCondition(&conditions, readyCond)

	tokenBound, tokenReason, tokenMsg := evalTokenSecretBound(cr, view)
	meta.SetStatusCondition(&conditions, metav1.Condition{
		Type:               accessv1alpha1.ConditionTypeTokenSecretBound,
		Status:             boolToConditionStatus(tokenBound),
		ObservedGeneration: cr.Generation,
		Reason:             tokenReason,
		Message:            tokenMsg,
	})

	return accessv1alpha1.RemoteAppStatus{
		Ready:              ready,
		LastError:          lastError,
		ObservedGeneration: cr.Generation,
		Conditions:         conditions,
	}
}

// evalTokenSecretBound classifies the TokenSecretView into the
// TokenSecretBound condition fields. Order matters: a non-NotFound fetch
// error wins over absence; absence wins over key absence.
func evalTokenSecretBound(cr *accessv1alpha1.RemoteApp, view TokenSecretView) (bool, string, string) {
	if view.FetchErr != nil {
		return false, "SecretGetError", view.FetchErr.Error()
	}
	if view.ResourceVersion == "" {
		return false, "SecretNotFound",
			fmt.Sprintf("Secret %q not found in namespace %q", view.Name, cr.Namespace)
	}
	if !view.KeyExists {
		return false, "KeyNotFound",
			fmt.Sprintf("Secret %q has no key %q", view.Name, view.Key)
	}
	return true, "Bound", fmt.Sprintf("Secret %q key %q present", view.Name, view.Key)
}

func boolToConditionStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// readyConditionReason picks a stable Reason string for the Ready
// condition. Reasons are not free-form per Kubernetes conventions —
// stick to a small finite set that automation can pattern-match on.
func readyConditionReason(ready bool, lastError string) string {
	if ready {
		return "TunnelReady"
	}
	if lastError == noTbotPodsMsg {
		return "NoPods"
	}
	return "PodNotReady"
}
