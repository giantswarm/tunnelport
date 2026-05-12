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

package crdacceptance

// This package deliberately constructs CRs with inline literals. Most of
// the cases mutate exactly one field to an invalid value to assert the
// CRD's validation rules; reading them as `newRemoteApp(withFooBroken(...))`
// would obscure the under-test invariant. The shared fixture in
// internal/controller/remoteapp/fixtures_test.go is also test-only (no
// cross-package import path), so we'd need to copy it here for the few
// happy-path cases — not worth the duplication.

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

func TestRemoteApp_AcceptsValidCR(t *testing.T) {
	cr := &accessv1alpha1.RemoteApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tracer",
			Namespace: "default",
		},
		Spec: accessv1alpha1.RemoteAppSpec{
			AppName:     "myapp",
			Port:        8080,
			ProxyAddr:   "teleport.example.com:443",
			TokenName:   "myapp-token",
			ClusterName: "teleport.example.com",
		},
	}

	if err := k8sClient.Create(context.Background(), cr); err != nil {
		t.Fatalf("expected valid RemoteApp to be accepted, got: %v", err)
	}
}

func TestRemoteApp_ReplicasIsOptionalAndNotDefaultedByCRD(t *testing.T) {
	cr := &accessv1alpha1.RemoteApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-replicas",
			Namespace: "default",
		},
		Spec: accessv1alpha1.RemoteAppSpec{
			AppName:     "myapp",
			Port:        8080,
			ProxyAddr:   "teleport.example.com:443",
			TokenName:   "myapp-token",
			ClusterName: "teleport.example.com",
		},
	}

	if err := k8sClient.Create(context.Background(), cr); err != nil {
		t.Fatalf("expected RemoteApp without replicas to be accepted, got: %v", err)
	}

	got := &accessv1alpha1.RemoteApp{}
	key := client.ObjectKey{Namespace: "default", Name: "no-replicas"}
	if err := k8sClient.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get after create: %v", err)
	}

	if got.Spec.Replicas != nil {
		t.Fatalf("expected Spec.Replicas to remain nil (no CRD default); got: %d", *got.Spec.Replicas)
	}
}

func TestRemoteApp_StatusSubresourceDoesNotBumpGeneration(t *testing.T) {
	ctx := context.Background()

	cr := &accessv1alpha1.RemoteApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "status-subresource",
			Namespace: "default",
		},
		Spec: accessv1alpha1.RemoteAppSpec{
			AppName:     "myapp",
			Port:        8080,
			ProxyAddr:   "teleport.example.com:443",
			TokenName:   "myapp-token",
			ClusterName: "teleport.example.com",
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create: %v", err)
	}

	key := client.ObjectKey{Namespace: "default", Name: "status-subresource"}
	got := &accessv1alpha1.RemoteApp{}
	if err := k8sClient.Get(ctx, key, got); err != nil {
		t.Fatalf("get after create: %v", err)
	}
	genAfterCreate := got.Generation

	got.Status.Ready = true
	got.Status.LastError = "boot"
	if err := k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("status update: %v", err)
	}

	if err := k8sClient.Get(ctx, key, got); err != nil {
		t.Fatalf("get after status update: %v", err)
	}
	if got.Generation != genAfterCreate {
		t.Fatalf("status update bumped generation: before=%d after=%d", genAfterCreate, got.Generation)
	}
	if !got.Status.Ready {
		t.Fatalf("expected Status.Ready=true after status update")
	}

	got.Spec.Port = 9090
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("spec update: %v", err)
	}
	if err := k8sClient.Get(ctx, key, got); err != nil {
		t.Fatalf("get after spec update: %v", err)
	}
	if got.Generation == genAfterCreate {
		t.Fatalf("spec update did not bump generation: still %d", got.Generation)
	}
}

func TestRemoteApp_RejectsMissingTokenName(t *testing.T) {
	cr := &accessv1alpha1.RemoteApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-tokenname-empty",
			Namespace: "default",
		},
		Spec: accessv1alpha1.RemoteAppSpec{
			AppName:     "myapp",
			Port:        8080,
			ProxyAddr:   "teleport.example.com:443",
			TokenName:   "",
			ClusterName: "teleport.example.com",
		},
	}

	if err := k8sClient.Create(context.Background(), cr); err == nil {
		t.Fatal("expected empty tokenName to be rejected, got nil error")
	}
}

func TestRemoteApp_RejectsEmptyProxyAddr(t *testing.T) {
	cr := &accessv1alpha1.RemoteApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty-proxy",
			Namespace: "default",
		},
		Spec: accessv1alpha1.RemoteAppSpec{
			AppName:     "myapp",
			Port:        8080,
			ProxyAddr:   "",
			TokenName:   "myapp-token",
			ClusterName: "teleport.example.com",
		},
	}

	if err := k8sClient.Create(context.Background(), cr); err == nil {
		t.Fatal("expected empty proxyAddr to be rejected, got nil error")
	}
}

// TestRemoteApp_RejectsInvalidTokenName covers the DNS-1123-subdomain
// constraint added in the ADR-0004 review pass: TokenName must match the
// DNS subdomain pattern and stay under 254 characters. Teleport resource
// names follow the same conventions.
func TestRemoteApp_RejectsInvalidTokenName(t *testing.T) {
	cases := []struct {
		name      string
		tokenName string
	}{
		{name: "uppercase", tokenName: "BadToken"},
		{name: "underscore", tokenName: "bad_token"},
		{name: "leading-dash", tokenName: "-bad"},
		{name: "trailing-dot", tokenName: "bad."},
		{name: "too-long", tokenName: strings.Repeat("a", 254)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &accessv1alpha1.RemoteApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bad-tokenname-" + tc.name,
					Namespace: "default",
				},
				Spec: accessv1alpha1.RemoteAppSpec{
					AppName:     "myapp",
					Port:        8080,
					ProxyAddr:   "teleport.example.com:443",
					TokenName:   tc.tokenName,
					ClusterName: "teleport.example.com",
				},
			}

			if err := k8sClient.Create(context.Background(), cr); err == nil {
				t.Fatalf("expected tokenName=%q to be rejected, got nil error", tc.tokenName)
			}
		})
	}
}

// TestRemoteApp_RejectsInvalidClusterName covers the same DNS-1123
// constraint on ClusterName.
func TestRemoteApp_RejectsInvalidClusterName(t *testing.T) {
	cases := []struct {
		name        string
		clusterName string
	}{
		{name: "uppercase", clusterName: "Teleport.Example.Com"},
		{name: "underscore", clusterName: "teleport_example.com"},
		{name: "too-long", clusterName: strings.Repeat("a", 254)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &accessv1alpha1.RemoteApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bad-clustername-" + tc.name,
					Namespace: "default",
				},
				Spec: accessv1alpha1.RemoteAppSpec{
					AppName:     "myapp",
					Port:        8080,
					ProxyAddr:   "teleport.example.com:443",
					TokenName:   "myapp-token",
					ClusterName: tc.clusterName,
				},
			}

			if err := k8sClient.Create(context.Background(), cr); err == nil {
				t.Fatalf("expected clusterName=%q to be rejected, got nil error", tc.clusterName)
			}
		})
	}
}

func TestRemoteApp_RejectsInvalidPort(t *testing.T) {
	cases := []struct {
		name string
		port int32
	}{
		{name: "zero", port: 0},
		{name: "above max", port: 70000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &accessv1alpha1.RemoteApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bad-port-" + tc.name,
					Namespace: "default",
				},
				Spec: accessv1alpha1.RemoteAppSpec{
					AppName:     "myapp",
					Port:        tc.port,
					ProxyAddr:   "teleport.example.com:443",
					TokenName:   "myapp-token",
					ClusterName: "teleport.example.com",
				},
			}

			err := k8sClient.Create(context.Background(), cr)
			if err == nil {
				t.Fatalf("expected port=%d to be rejected, got nil error", tc.port)
			}
		})
	}
}
