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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// The Event boundary logic (recordUpstreamTransition), driven with fixed
// conditions and a stub store. The rows that matter are the ones the
// condition alone gets wrong: a pod roll in the middle of an outage turns
// False into Unknown, and the eventual True must still count as the end
// of that outage — once — while an Unknown→True on a fresh RemoteApp, a
// pod restart or a leader handover must stay silent.

func upstreamCond(status metav1.ConditionStatus, at time.Time) []metav1.Condition {
	return []metav1.Condition{{
		Type:               accessv1alpha1.ConditionTypeUpstreamReachable,
		Status:             status,
		Reason:             "test",
		Message:            "upstream " + string(status),
		LastTransitionTime: metav1.NewTime(at),
	}}
}

func TestRecordUpstreamTransition(t *testing.T) {
	cr := newRemoteApp()
	key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}
	t0 := time.Date(2026, 9, 5, 15, 40, 0, 0, time.UTC)
	min := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

	tests := []struct {
		name   string
		before []metav1.Condition
		after  []metav1.Condition
		store  stubVerifications
		// want is the substring of the one expected Event, "" for none.
		want string
	}{
		{
			name:   "no condition after: nothing",
			before: upstreamCond(metav1.ConditionFalse, min(0)),
			after:  nil,
		},
		{
			name:   "unchanged False: nothing",
			before: upstreamCond(metav1.ConditionFalse, min(0)),
			after:  upstreamCond(metav1.ConditionFalse, min(0)),
			store:  stubVerifications{downSince: map[types.NamespacedName]time.Time{key: min(0)}},
		},
		{
			// The incident begins.
			name:   "True to False: Warning",
			before: upstreamCond(metav1.ConditionTrue, min(0)),
			after:  upstreamCond(metav1.ConditionFalse, min(5)),
			store:  stubVerifications{downSince: map[types.NamespacedName]time.Time{key: min(5)}},
			want:   "Warning UpstreamUnreachable",
		},
		{
			// True → (pods restart) Unknown at min 3 → outage begins at min 5.
			name:   "Unknown to False with an outage that began after the Unknown: Warning",
			before: upstreamCond(metav1.ConditionUnknown, min(3)),
			after:  upstreamCond(metav1.ConditionFalse, min(5)),
			store:  stubVerifications{downSince: map[types.NamespacedName]time.Time{key: min(5)}},
			want:   "Warning UpstreamUnreachable",
		},
		{
			// False at min 0 (already warned) → pods roll, Unknown at min 3 →
			// pods back, still down: the same outage, no second Warning.
			name:   "Unknown to False inside an outage already reported: nothing",
			before: upstreamCond(metav1.ConditionUnknown, min(3)),
			after:  upstreamCond(metav1.ConditionFalse, min(5)),
			store:  stubVerifications{downSince: map[types.NamespacedName]time.Time{key: min(0)}},
		},
		{
			// Fresh RemoteApp, or condition absent: first verdict is a failure.
			name:  "absent to False: Warning",
			after: upstreamCond(metav1.ConditionFalse, min(5)),
			store: stubVerifications{downSince: map[types.NamespacedName]time.Time{key: min(5)}},
			want:  "Warning UpstreamUnreachable",
		},
		{
			// Direct recovery.
			name:   "False to True with a recovery: Normal",
			before: upstreamCond(metav1.ConditionFalse, min(0)),
			after:  upstreamCond(metav1.ConditionTrue, min(10)),
			store:  stubVerifications{recovered: map[types.NamespacedName]time.Time{key: min(10)}},
			want:   "Normal UpstreamReachable",
		},
		{
			// The ATS smoke's shape: False at min 0 → pods swapped, Unknown at
			// min 6 → the new pod answers at min 10. One recovery Event.
			name:   "Unknown to True with a recovery after the Unknown: Normal",
			before: upstreamCond(metav1.ConditionUnknown, min(6)),
			after:  upstreamCond(metav1.ConditionTrue, min(10)),
			store:  stubVerifications{recovered: map[types.NamespacedName]time.Time{key: min(10)}},
			want:   "Normal UpstreamReachable",
		},
		{
			// A pod restart on a healthy tunnel long after the last outage
			// ended: Unknown at min 20, True at min 21, recovery stamp at 10.
			name:   "Unknown to True with an old recovery: nothing",
			before: upstreamCond(metav1.ConditionUnknown, min(20)),
			after:  upstreamCond(metav1.ConditionTrue, min(21)),
			store:  stubVerifications{recovered: map[types.NamespacedName]time.Time{key: min(10)}},
		},
		{
			// Fresh RemoteApp or leader handover: no outage ever recorded.
			name:   "Unknown to True without any recovery: nothing",
			before: upstreamCond(metav1.ConditionUnknown, min(0)),
			after:  upstreamCond(metav1.ConditionTrue, min(1)),
		},
		{
			name:  "absent to True: nothing",
			after: upstreamCond(metav1.ConditionTrue, min(1)),
		},
		{
			// Unknown carries no verdict either way.
			name:   "False to Unknown: nothing",
			before: upstreamCond(metav1.ConditionFalse, min(0)),
			after:  upstreamCond(metav1.ConditionUnknown, min(3)),
			store:  stubVerifications{downSince: map[types.NamespacedName]time.Time{key: min(0)}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := events.NewFakeRecorder(4)
			store := tc.store
			store.enabled, store.upstream = true, true
			r := &Reconciler{Recorder: rec, Verifications: store}

			r.recordUpstreamTransition(cr, tc.before, tc.after)

			var got []string
			for {
				select {
				case e := <-rec.Events:
					got = append(got, e)
					continue
				default:
				}
				break
			}
			switch {
			case tc.want == "" && len(got) != 0:
				t.Errorf("emitted %v, want nothing", got)
			case tc.want != "" && len(got) != 1:
				t.Errorf("emitted %v, want exactly one Event containing %q", got, tc.want)
			case tc.want != "" && !strings.Contains(got[0], tc.want):
				t.Errorf("emitted %q, want it to contain %q", got[0], tc.want)
			}
		})
	}
}

// TestRecordUpstreamTransition_NilRecorderOrStoreIsSafe: the envtest
// manager and an install with verification off wire neither.
func TestRecordUpstreamTransition_NilRecorderOrStoreIsSafe(t *testing.T) {
	cr := newRemoteApp()
	before := upstreamCond(metav1.ConditionFalse, time.Now())
	after := upstreamCond(metav1.ConditionTrue, time.Now())
	(&Reconciler{}).recordUpstreamTransition(cr, before, after)
	(&Reconciler{Recorder: events.NewFakeRecorder(1)}).recordUpstreamTransition(cr, before, after)
	(&Reconciler{Verifications: stubVerifications{enabled: true, upstream: true}}).recordUpstreamTransition(cr, before, after)
}
