package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestRequestsWithinLimits(t *testing.T) {
	requirements := func(cpuRequest, memRequest, cpuLimit, memLimit string) corev1.ResourceRequirements {
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpuRequest),
				corev1.ResourceMemory: resource.MustParse(memRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpuLimit),
				corev1.ResourceMemory: resource.MustParse(memLimit),
			},
		}
	}

	cases := []struct {
		name      string
		container string
		r         corev1.ResourceRequirements
		wantErr   string
	}{
		{
			name:      "chart defaults",
			container: "ghostunnel",
			r:         requirements("10m", "32Mi", "200m", "256Mi"),
		},
		{
			name:      "request equal to limit",
			container: "tbot",
			r:         requirements("200m", "256Mi", "200m", "256Mi"),
		},
		{
			name:      "cpu request above limit",
			container: "ghostunnel",
			r:         requirements("300m", "32Mi", "200m", "256Mi"),
			wantErr:   "--ghostunnel-cpu-request 300m is above --ghostunnel-cpu-limit 200m",
		},
		{
			name:      "memory request above limit, across units",
			container: "tbot",
			r:         requirements("50m", "1Gi", "200m", "512Mi"),
			wantErr:   "--tbot-memory-request 1Gi is above --tbot-memory-limit 512Mi",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requestsWithinLimits(tc.container, tc.r)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
