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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// envtest has no kubelet/scheduler, so pods are never created from
// Deployments. To exercise the status branches we hand-craft Pods owned
// by the rendered Deployment with the canonical label, and write Pod
// Status via the status subresource. The same approach is documented
// upstream: kubebuilder.io/reference/envtest, "Limitations".

// makePod creates a pod that the reconciler's Pod watch (via the
// canonical LabelRemoteAppInstance) will route back to the parent
// RemoteApp. The pod is labelled with the RemoteApp name so it shows up
// in the same listing the reconciler does.
func makePod(ctx context.Context, t *testing.T, ns, raName, name string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				LabelRole:              LabelRoleValue,
				LabelRemoteAppInstance: raName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "tbot", Image: "irrelevant"},
			},
		},
	}
	if err := testClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	return pod
}

// setPodStatus writes the pod's Status subresource. envtest lets us do
// this directly because there's no kubelet to compete with.
func setPodStatus(ctx context.Context, t *testing.T, pod *corev1.Pod, mut func(s *corev1.PodStatus)) {
	t.Helper()
	got := &corev1.Pod{}
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(pod), got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	mut(&got.Status)
	if err := testClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	*pod = *got
}

// makeTokenSecret creates a Secret with the named key so the
// TokenSecretBound condition can flip to True.
func makeTokenSecret(ctx context.Context, t *testing.T, ns, name, key string) {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{key: []byte("test-token-value")},
	}
	if err := testClient.Create(ctx, s); err != nil {
		t.Fatalf("create token Secret: %v", err)
	}
}

func getRemoteApp(ctx context.Context, t *testing.T, key types.NamespacedName) *accessv1alpha1.RemoteApp {
	t.Helper()
	got := &accessv1alpha1.RemoteApp{}
	if err := testClient.Get(ctx, key, got); err != nil {
		t.Fatalf("get RemoteApp: %v", err)
	}
	return got
}

// TestStatus_ObservedGenerationTracksSpecChanges pins the standard
// kubebuilder pattern: status.observedGeneration must catch up to
// metadata.generation after a successful reconcile.
func TestStatus_ObservedGenerationTracksSpecChanges(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "obsgen")
	key := client.ObjectKeyFromObject(cr)

	// First reconcile: observedGeneration catches up to gen 1.
	eventually(t, func() (bool, error) {
		got := getRemoteApp(ctx, t, key)
		if got.Status.ObservedGeneration != got.Generation {
			return false, fmt.Errorf("observedGeneration=%d, generation=%d",
				got.Status.ObservedGeneration, got.Generation)
		}
		return true, nil
	})

	// Mutate spec: generation bumps.
	got := getRemoteApp(ctx, t, key)
	gen1 := got.Generation
	port := int32(9091)
	got.Spec.Port = port
	if err := testClient.Update(ctx, got); err != nil {
		t.Fatalf("update spec: %v", err)
	}

	eventually(t, func() (bool, error) {
		got := getRemoteApp(ctx, t, key)
		if got.Generation <= gen1 {
			return false, fmt.Errorf("generation did not bump: %d", got.Generation)
		}
		if got.Status.ObservedGeneration != got.Generation {
			return false, fmt.Errorf("observedGeneration=%d, generation=%d",
				got.Status.ObservedGeneration, got.Generation)
		}
		return true, nil
	})
}

// TestStatus_TokenSecretBoundFalseWhenSecretMissing exercises the
// pre-Secret state: CR exists, Secret doesn't, condition reflects it.
func TestStatus_TokenSecretBoundFalseWhenSecretMissing(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "no-secret")
	key := client.ObjectKeyFromObject(cr)

	eventually(t, func() (bool, error) {
		got := getRemoteApp(ctx, t, key)
		c := meta.FindStatusCondition(got.Status.Conditions, accessv1alpha1.ConditionTypeTokenSecretBound)
		if c == nil {
			return false, fmt.Errorf("TokenSecretBound condition not yet set")
		}
		if c.Status != metav1.ConditionFalse {
			return false, fmt.Errorf("TokenSecretBound: want False, got %q (reason=%q)", c.Status, c.Reason)
		}
		return true, nil
	})
}

// TestStatus_TokenSecretBoundTrueWhenSecretAndKeyExist exercises the
// happy-path bind: Secret exists *and* the named key is present.
func TestStatus_TokenSecretBoundTrueWhenSecretAndKeyExist(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "with-secret")
	makeTokenSecret(ctx, t, ns, cr.Spec.TokenRef.Name, cr.Spec.TokenRef.Key)
	key := client.ObjectKeyFromObject(cr)

	eventually(t, func() (bool, error) {
		got := getRemoteApp(ctx, t, key)
		c := meta.FindStatusCondition(got.Status.Conditions, accessv1alpha1.ConditionTypeTokenSecretBound)
		if c == nil {
			return false, fmt.Errorf("TokenSecretBound condition not yet set")
		}
		if c.Status != metav1.ConditionTrue {
			return false, fmt.Errorf("TokenSecretBound: want True, got %q (reason=%q)", c.Status, c.Reason)
		}
		return true, nil
	})
}

// TestStatus_TokenSecretBoundFalseWhenKeyMissing covers the case where
// the Secret exists but the operator-named key isn't present.
func TestStatus_TokenSecretBoundFalseWhenKeyMissing(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "wrong-key")
	// Secret exists with a different key name.
	makeTokenSecret(ctx, t, ns, cr.Spec.TokenRef.Name, "not-the-right-key")
	key := client.ObjectKeyFromObject(cr)

	eventually(t, func() (bool, error) {
		got := getRemoteApp(ctx, t, key)
		c := meta.FindStatusCondition(got.Status.Conditions, accessv1alpha1.ConditionTypeTokenSecretBound)
		if c == nil {
			return false, fmt.Errorf("TokenSecretBound condition not yet set")
		}
		if c.Status != metav1.ConditionFalse {
			return false, fmt.Errorf("TokenSecretBound: want False, got %q (reason=%q)", c.Status, c.Reason)
		}
		return true, nil
	})
}

// TestStatus_HealthyPodFlipsReadyTrue creates a pod, marks it Ready, and
// verifies the RemoteApp status flips Ready=true and lastError=empty.
// envtest has no kubelet, so the test owns the Pod's Status subresource.
func TestStatus_HealthyPodFlipsReadyTrue(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "healthy")
	pod := makePod(ctx, t, ns, cr.Name, "healthy-pod")
	setPodStatus(ctx, t, pod, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodRunning
		s.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
		}
		s.ContainerStatuses = []corev1.ContainerStatus{
			{
				Name:  "tbot",
				Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			},
		}
	})

	eventually(t, func() (bool, error) {
		got := getRemoteApp(ctx, t, client.ObjectKeyFromObject(cr))
		if !got.Status.Ready {
			return false, fmt.Errorf("status.ready: want true, got false (lastError=%q)", got.Status.LastError)
		}
		if got.Status.LastError != "" {
			return false, fmt.Errorf("status.lastError: want empty, got %q", got.Status.LastError)
		}
		c := meta.FindStatusCondition(got.Status.Conditions, accessv1alpha1.ConditionTypeReady)
		if c == nil || c.Status != metav1.ConditionTrue {
			return false, fmt.Errorf("Ready condition not True: %+v", c)
		}
		return true, nil
	})
}

// TestStatus_PendingPodSurfacesVolumeMountFailure mirrors the GitOps-race
// case: the CR exists, the Secret doesn't, the pod sits in
// ContainerCreating with the kubelet's mount-failure message.
func TestStatus_PendingPodSurfacesVolumeMountFailure(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "pending")
	pod := makePod(ctx, t, ns, cr.Name, "pending-pod")
	setPodStatus(ctx, t, pod, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodPending
		s.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}
		s.ContainerStatuses = []corev1.ContainerStatus{
			{
				Name:  "tbot",
				Ready: false,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "ContainerCreating",
						Message: `MountVolume.SetUp failed for volume "tbot-token" : secret "demo-token" not found`,
					},
				},
			},
		}
	})

	eventually(t, func() (bool, error) {
		got := getRemoteApp(ctx, t, client.ObjectKeyFromObject(cr))
		if got.Status.Ready {
			return false, fmt.Errorf("status.ready must be false")
		}
		if !strings.Contains(got.Status.LastError, "ContainerCreating") {
			return false, fmt.Errorf("lastError missing ContainerCreating: %q", got.Status.LastError)
		}
		if !strings.Contains(got.Status.LastError, `secret "demo-token" not found`) {
			return false, fmt.Errorf("lastError missing mount message: %q", got.Status.LastError)
		}
		return true, nil
	})
}

// TestStatus_CrashLoopingPodSurfacesRestartsAndExitCode covers the
// crashloop branch — restart count and last termination reason must be
// in lastError, but logs must NOT be (ADR 0003).
func TestStatus_CrashLoopingPodSurfacesRestartsAndExitCode(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "crashloop")
	pod := makePod(ctx, t, ns, cr.Name, "crashloop-pod")
	setPodStatus(ctx, t, pod, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodRunning
		s.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}
		s.ContainerStatuses = []corev1.ContainerStatus{
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
		}
	})

	eventually(t, func() (bool, error) {
		got := getRemoteApp(ctx, t, client.ObjectKeyFromObject(cr))
		if got.Status.Ready {
			return false, fmt.Errorf("status.ready must be false")
		}
		for _, want := range []string{"CrashLoopBackOff", "5 restarts", "Error", "137"} {
			if !strings.Contains(got.Status.LastError, want) {
				return false, fmt.Errorf("lastError missing %q: %q", want, got.Status.LastError)
			}
		}
		return true, nil
	})
}

// TestStatus_PodEventReenqueuesParentRemoteApp pins the watch behavior:
// changing a Pod's Status (without touching the RemoteApp) must trigger
// a reconcile, otherwise lastError would lag pod state arbitrarily.
func TestStatus_PodEventReenqueuesParentRemoteApp(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNS(t, ctx)

	cr := makeRemoteApp(ctx, t, ns, "pod-watch")
	pod := makePod(ctx, t, ns, cr.Name, "pod-watch-pod")

	// Start with an unready pod so lastError reflects it.
	setPodStatus(ctx, t, pod, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodPending
		s.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}
		s.ContainerStatuses = []corev1.ContainerStatus{
			{Name: "tbot", Ready: false, State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
			}},
		}
	})

	// Wait for that to land in status before we flip the pod ready.
	eventually(t, func() (bool, error) {
		got := getRemoteApp(ctx, t, client.ObjectKeyFromObject(cr))
		if !strings.Contains(got.Status.LastError, "ContainerCreating") {
			return false, fmt.Errorf("lastError not yet ContainerCreating: %q", got.Status.LastError)
		}
		return true, nil
	})

	// Flip pod to Ready WITHOUT touching the RemoteApp. If the watch
	// works, status flips Ready=true within pollTimeout.
	setPodStatus(ctx, t, pod, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodRunning
		s.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
		}
		s.ContainerStatuses = []corev1.ContainerStatus{
			{Name: "tbot", Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}
	})

	eventually(t, func() (bool, error) {
		got := getRemoteApp(ctx, t, client.ObjectKeyFromObject(cr))
		if !got.Status.Ready {
			return false, fmt.Errorf("status.ready did not flip to true on pod event (lastError=%q)", got.Status.LastError)
		}
		return true, nil
	})
}
