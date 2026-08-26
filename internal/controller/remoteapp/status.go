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
	"k8s.io/apimachinery/pkg/types"

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

// Pod waiting/terminated reasons referenced by errorSeverity. Defining
// these as constants both removes goconst churn and gives tests a
// shared source-of-truth string to assert against.
const (
	reasonCrashLoopBackOff  = "CrashLoopBackOff"
	reasonError             = "Error"
	reasonContainerCreating = "ContainerCreating"
)

// RemoteApp condition reasons. Kept finite and constant per Kubernetes
// convention so automation can pattern-match on them.
const (
	reasonTunnelReady = "TunnelReady"
	reasonPodNotReady = "PodNotReady"
	reasonNoPods      = "NoPods"

	reasonIdentityIssued    = "IdentityIssued"
	reasonIdentityNotIssued = "IdentityNotIssued"
	reasonTunnelServing     = "TunnelServing"
	reasonTunnelNotServing  = "TunnelNotServing"

	// Reasons for the TunnelVerified condition. The two Unknown reasons
	// are as load-bearing as the False one: reporting "cannot verify" as
	// a failure would make the check cry wolf on every fresh install,
	// and reporting it as success would recreate the blind spot
	// giantswarm/giantswarm#37521 is about.
	reasonCertificateVerified = "CertificateVerified"
	reasonCertificateInvalid  = "CertificateInvalid"
	reasonTunnelUnreachable   = "TunnelUnreachable"
	reasonVerificationPending = "VerificationPending"
	reasonNotVerifiedNotReady = "TunnelNotReady"
)

// liveTbotPods returns the subset of pods that are not being torn down,
// i.e. pods whose DeletionTimestamp is nil. Pods with DeletionTimestamp
// set are mid-termination: their PodReady condition still reads
// whatever the kubelet last wrote, which during a rolling update with
// maxSurge=1/maxUnavailable=0 can briefly be True even though the pod
// is on its way out. Counting those would let Ready=true lie. Both the
// Ready check and the lastError summary use the filtered set so the
// two signals can never disagree on which pods exist.
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
	case reasonCrashLoopBackOff:
		return 2
	case reasonError, "OOMKilled":
		return 3
	case reasonContainerCreating, "PodInitializing":
		return 4
	default:
		return 5
	}
}

// unreadyContainers returns the pod's unready container statuses with the
// tbot container first. A tunnel pod runs tbot plus the ghostunnel sidecar,
// and ghostunnel cannot bind its TLS listener until tbot has written the
// SVID, so when both are unready tbot holds the cause and ghostunnel only
// the symptom. Kubelet orders ContainerStatuses alphabetically, which puts
// ghostunnel first and would otherwise send every reader of status.lastError
// to investigate TLS instead of the Teleport join.
func unreadyContainers(pod *corev1.Pod) []*corev1.ContainerStatus {
	out := make([]*corev1.ContainerStatus, 0, len(pod.Status.ContainerStatuses))
	tbot := -1
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Ready {
			continue
		}
		if cs.Name == tbotContainerName {
			tbot = len(out)
		}
		out = append(out, cs)
	}
	if tbot > 0 {
		out[0], out[tbot] = out[tbot], out[0]
	}
	return out
}

// podErrorReason extracts the canonical reason string used for severity
// ranking. Mirrors summarizePodError's source-of-truth: it walks the same
// tbot-first container order, a Waiting container's Reason wins, a
// Terminated container's Reason is next, and the pod Phase is the
// fallback. Empty string means "nothing to rank on".
func podErrorReason(pod *corev1.Pod) string {
	for _, cs := range unreadyContainers(pod) {
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
	ordered := append([]corev1.Pod(nil), pods...)
	sortBySeverityThenName(ordered)
	for i := range ordered {
		if msg := summarizePodError(&ordered[i]); msg != "" {
			return msg
		}
	}
	return summarizePodError(&ordered[0])
}

// sortBySeverityThenName orders pods in place by errorSeverity, ties broken
// on pod name, so every caller that has to choose one pod out of many
// chooses the same one.
func sortBySeverityThenName(pods []corev1.Pod) {
	sort.SliceStable(pods, func(a, b int) bool {
		sa, sb := errorSeverity(podErrorReason(&pods[a])), errorSeverity(podErrorReason(&pods[b]))
		if sa != sb {
			return sa < sb
		}
		return pods[a].Name < pods[b].Name
	})
}

// summarizePodError turns a single pod's k8s-visible state into a one-
// line lastError string. Empty string means the pod is healthy.
//
// Format conventions (pinned by tests):
//   - "CrashLoopBackOff (5 restarts), last termination: Error (137)"
//   - "ContainerCreating: <volume mount message>"
//   - "Failed: Evicted"
//   - "" when Ready.
//
// The string describes one container: tbot when tbot is unready, otherwise
// the sidecar. See unreadyContainers for why.
func summarizePodError(pod *corev1.Pod) string {
	if pod == nil {
		return noTbotPodsMsg
	}
	if isPodReady(pod) {
		return ""
	}

	// Container-level state is the most informative input we have when
	// the pod is unready. Walk the unready containers, tbot first, and
	// build a summary from the first one that yields anything.
	for _, cs := range unreadyContainers(pod) {
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

// summarizeRole reports whether one named container is ready on any live
// pod, and if not, the summary of that container's state on the pod ranked
// worst by pickWorstSummary's ordering. It mirrors summarizeStatus's
// at-least-one-ready roll-up so the per-role conditions and the Ready
// condition cannot disagree about which pods exist.
//
// The empty string as the second return means "no pod carries a container by
// this name", which is distinct from "the container is unready": a pod that
// has not created its containers yet reports neither.
func summarizeRole(pods []corev1.Pod, container string) (bool, string) {
	live := liveTbotPods(pods)
	if len(live) == 0 {
		return false, noTbotPodsMsg
	}

	var unready []corev1.Pod
	for i := range live {
		cs := findContainerStatus(&live[i], container)
		if cs == nil {
			continue
		}
		if cs.Ready {
			return true, ""
		}
		unready = append(unready, live[i])
	}
	if len(unready) == 0 {
		return false, ""
	}

	sortBySeverityThenName(unready)
	for i := range unready {
		cs := findContainerStatus(&unready[i], container)
		if msg := containerWaitingSummary(cs); msg != "" {
			return false, msg
		}
		if msg := containerTerminatedSummary(cs); msg != "" {
			return false, msg
		}
	}
	return false, "not ready"
}

// findContainerStatus returns the pod's status entry for one container name,
// or nil when the pod carries no container by that name.
func findContainerStatus(pod *corev1.Pod, container string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == container {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// roleCondition builds one per-role condition from summarizeRole's output.
// A role whose container is absent from every pod yields Status=Unknown
// rather than False: the operator has nothing to report, which is not the
// same as a failure.
func roleCondition(condType string, generation int64, ready bool, summary, trueReason, falseReason, readyMessage string) metav1.Condition {
	cond := metav1.Condition{
		Type:               condType,
		ObservedGeneration: generation,
	}
	switch {
	case ready:
		cond.Status = metav1.ConditionTrue
		cond.Reason = trueReason
		cond.Message = readyMessage
	case summary == "":
		cond.Status = metav1.ConditionUnknown
		cond.Reason = falseReason
		cond.Message = "no container status yet"
	case summary == noTbotPodsMsg:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonNoPods
		cond.Message = summary
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = falseReason
		cond.Message = summary
	}
	return cond
}

// computeStatus derives the full RemoteAppStatus from k8s-visible inputs.
// Pure: no I/O, no logging. meta.SetStatusCondition uses metav1.Now() for
// LastTransitionTime — that's library-imposed and stripped by statusEqual.
//
// Per ADR 0004 the operator emits two conditions:
//
//   - `Ready` — join-level state, derived from tbot pod readiness against
//     the tunnel diag endpoint.
//   - `Reconciled` — operator-internal state: True if every owned-object
//     apply in the most recent reconcile pass succeeded; False with
//     Reason=ReconcileError if any failed. Distinct from `Ready`: a
//     successful reconcile doesn't imply the tunnel is up (tbot may still
//     be starting), and a failed reconcile doesn't imply the tunnel is
//     down (a prior successful apply may still be serving traffic).
//
// applyErrSummary is the operator-internal summary of the most recent
// reconcile pass's apply errors (empty string means all applies
// succeeded). The caller — reconcileStatus — collects it before calling.
//
// verifications is the read side of the TLS verifier's store, or nil when
// verification is not wired. It is the one input here that is not
// k8s-visible state; see verify.go for the ADR 0003 position and
// setVerifiedCondition for how absence of a result is reported.
func computeStatus(cr *accessv1alpha1.RemoteApp, pods []corev1.Pod, prevConditions []metav1.Condition, applyErrSummary string, verifications VerificationReader) accessv1alpha1.RemoteAppStatus {
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

	// Per-role conditions, so a reader sees the join and the TLS listener at
	// once instead of only whichever one lastError had room for.
	identityReady, identitySummary := summarizeRole(pods, tbotContainerName)
	meta.SetStatusCondition(&conditions, roleCondition(
		accessv1alpha1.ConditionTypeIdentityIssued, cr.Generation,
		identityReady, identitySummary,
		reasonIdentityIssued, reasonIdentityNotIssued,
		"tbot reports a usable identity",
	))

	servingReady, servingSummary := summarizeRole(pods, ghostunnelContainerName)
	meta.SetStatusCondition(&conditions, roleCondition(
		accessv1alpha1.ConditionTypeTunnelServing, cr.Generation,
		servingReady, servingSummary,
		reasonTunnelServing, reasonTunnelNotServing,
		"TLS listener is accepting connections",
	))

	// TunnelVerified: the one condition whose source is an active probe
	// rather than pod state. Deliberately placed after TunnelServing —
	// the pair reads as "the listener accepts connections, and here is
	// whether what it serves is usable".
	setVerifiedCondition(&conditions, cr, verifications)

	reconciledCond := metav1.Condition{
		Type:               accessv1alpha1.ConditionTypeReconciled,
		ObservedGeneration: cr.Generation,
	}
	if applyErrSummary == "" {
		reconciledCond.Status = metav1.ConditionTrue
		reconciledCond.Reason = "ReconcileSucceeded"
		reconciledCond.Message = "all owned objects applied"
	} else {
		reconciledCond.Status = metav1.ConditionFalse
		reconciledCond.Reason = "ReconcileError"
		reconciledCond.Message = applyErrSummary
	}
	meta.SetStatusCondition(&conditions, reconciledCond)

	return accessv1alpha1.RemoteAppStatus{
		Ready:              ready,
		LastError:          lastError,
		ObservedGeneration: cr.Generation,
		Conditions:         conditions,
	}
}

// setVerifiedCondition writes (or removes) the TunnelVerified condition
// from the TLS verifier's latest outcome.
//
// Three cases, and the difference between them is the whole design:
//
//   - Verification not wired or disabled: the condition is *removed*, not
//     set to Unknown. An operator that does not run the check must not
//     leave a permanent Unknown on every CR, and removal also cleans up
//     after an install that turns the feature off again.
//   - Wired but nothing to report — no round has covered this RemoteApp
//     yet, or the tunnel does not claim to be serving so it was not
//     probed: Unknown. "I have not checked" is not "the certificate is
//     bad", and conflating the two would make the condition useless
//     during every rollout.
//   - A real outcome: True for verified, False for cert_invalid and
//     unreachable, with the prober's one-line diagnosis as the message.
//     That message is the fast path for a human — in the SAN-drift
//     incident it names both the expected FQDN and the SANs actually
//     presented, which is the whole diagnosis in one line of
//     `kubectl describe`.
func setVerifiedCondition(conditions *[]metav1.Condition, cr *accessv1alpha1.RemoteApp, verifications VerificationReader) {
	if verifications == nil || !verifications.Enabled() {
		meta.RemoveStatusCondition(conditions, accessv1alpha1.ConditionTypeTunnelVerified)
		return
	}

	cond := metav1.Condition{
		Type:               accessv1alpha1.ConditionTypeTunnelVerified,
		ObservedGeneration: cr.Generation,
	}

	result, ok := verifications.Result(types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name})
	switch {
	case !ok:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = reasonVerificationPending
		cond.Message = "no TLS verification result yet"
	case result.Result == ResultVerified:
		cond.Status = metav1.ConditionTrue
		cond.Reason = reasonCertificateVerified
		cond.Message = fmt.Sprintf("served certificate verifies for %s", result.ServerName)
	case result.Result == ResultNotReady:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = reasonNotVerifiedNotReady
		cond.Message = "tunnel is not ready; certificate not verified"
	case result.Result == ResultCertInvalid:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonCertificateInvalid
		cond.Message = result.Detail
	case result.Result == ResultUnreachable:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonTunnelUnreachable
		cond.Message = result.Detail
	default:
		// Unreachable in practice: every VerificationResult is handled
		// above. Reported as Unknown rather than silently dropped so a
		// future result value added without touching this switch shows up
		// as "not classified" instead of as a passing tunnel.
		cond.Status = metav1.ConditionUnknown
		cond.Reason = reasonVerificationPending
		cond.Message = fmt.Sprintf("unclassified verification result %q", result.Result)
	}
	meta.SetStatusCondition(conditions, cond)
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
		return reasonTunnelReady
	}
	if lastError == noTbotPodsMsg {
		return reasonNoPods
	}
	return reasonPodNotReady
}
