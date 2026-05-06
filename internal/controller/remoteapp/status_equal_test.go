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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

func TestStatusEqual(t *testing.T) {
	t1 := metav1.NewTime(time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC))
	t2 := metav1.NewTime(time.Date(2026, 5, 6, 13, 0, 0, 0, time.UTC))

	baseStatus := func() *accessv1alpha1.RemoteAppStatus {
		return &accessv1alpha1.RemoteAppStatus{
			Ready:              true,
			LastError:          "",
			ObservedGeneration: 3,
			Conditions: []metav1.Condition{
				{
					Type:               accessv1alpha1.ConditionTypeReady,
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 3,
					Reason:             "TunnelReady",
					Message:            "tbot tunnel ready",
					LastTransitionTime: t1,
				},
			},
		}
	}

	tests := []struct {
		name string
		a    *accessv1alpha1.RemoteAppStatus
		b    *accessv1alpha1.RemoteAppStatus
		want bool
	}{
		{
			name: "identical except condition LastTransitionTime are equal",
			a:    baseStatus(),
			b: func() *accessv1alpha1.RemoteAppStatus {
				s := baseStatus()
				s.Conditions[0].LastTransitionTime = t2
				return s
			}(),
			want: true,
		},
		{
			name: "differing condition Reason are not equal",
			a:    baseStatus(),
			b: func() *accessv1alpha1.RemoteAppStatus {
				s := baseStatus()
				s.Conditions[0].Reason = "PodNotReady"
				return s
			}(),
			want: false,
		},
		{
			name: "differing LastError are not equal",
			a:    baseStatus(),
			b: func() *accessv1alpha1.RemoteAppStatus {
				s := baseStatus()
				s.LastError = "CrashLoopBackOff"
				return s
			}(),
			want: false,
		},
		{
			name: "both nil are equal",
			a:    nil,
			b:    nil,
			want: true,
		},
		{
			name: "one nil is not equal",
			a:    nil,
			b:    baseStatus(),
			want: false,
		},
		{
			name: "other nil is not equal",
			a:    baseStatus(),
			b:    nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("statusEqual = %v, want %v", got, tt.want)
			}
		})
	}
}
