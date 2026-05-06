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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// managerHandle owns the controller-runtime manager goroutine for
// integration tests — start it once, stop it on cleanup.
type managerHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (h *managerHandle) stop() {
	if h == nil {
		return
	}
	h.cancel()
	<-h.done
}

// testConfig is the operator PodDefaults the integration-test manager uses.
// The values are fixed strings/quantities so tests can assert against them.
func testConfig() PodDefaults {
	return PodDefaults{
		TbotImage: "test.example.com/tbot:test",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}
}

// startManager builds a controller-runtime manager wired to the envtest
// API server, registers the RemoteApp Reconciler, and starts it in a
// goroutine. The returned handle's stop() method blocks until the manager
// goroutine exits.
func startManager(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme) (*managerHandle, error) {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		// Disable the metrics endpoint in tests — the random port binding
		// is the only thing flaky about envtest setups.
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return nil, fmt.Errorf("ctrl.NewManager: %w", err)
	}

	r := &Reconciler{
		Client:      mgr.GetClient(),
		PodDefaults: testConfig(),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("Reconciler.SetupWithManager: %w", err)
	}

	mgrCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Start(mgrCtx)
	}()

	// Wait for the cache to be ready so tests don't race the manager's
	// initial sync. Manager.GetCache().WaitForCacheSync respects the
	// caller's ctx.
	cacheCtx, cacheCancel := context.WithCancel(mgrCtx)
	defer cacheCancel()
	if !mgr.GetCache().WaitForCacheSync(cacheCtx) {
		cancel()
		<-done
		return nil, fmt.Errorf("cache did not sync")
	}

	return &managerHandle{cancel: cancel, done: done}, nil
}
