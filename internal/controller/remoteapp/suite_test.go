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
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// envtest bootstrap shared by every controller test in this package.
// Pattern mirrors internal/crdacceptance/suite_test.go: load CRDs from
// config/crd/bases, start the API server, build a typed client.

var (
	testEnv     *envtest.Environment
	restCfg     *rest.Config
	testScheme  = runtime.NewScheme()
	testClient  client.Client
	testManager *managerHandle
)

func TestMain(m *testing.M) {
	// envtest assets aren't always provisioned: the golden CI go-build runs a
	// plain `go test ./...` without them. Skip the envtest-backed suite when
	// KUBEBUILDER_ASSETS is unset. The full suite runs in the architect/go-test
	// CI job (make test-ci) and locally via `make test-ci`, both of which
	// provision envtest and export KUBEBUILDER_ASSETS.
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS unset; skipping envtest suite")
		return
	}

	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "scheme: %v\n", err)
		os.Exit(1)
	}
	if err := accessv1alpha1.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "scheme: %v\n", err)
		os.Exit(1)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start: %v\n", err)
		os.Exit(1)
	}
	restCfg = cfg

	testClient, err = client.New(restCfg, client.Options{Scheme: testScheme})
	if err != nil {
		_ = testEnv.Stop()
		fmt.Fprintf(os.Stderr, "client init: %v\n", err)
		os.Exit(1)
	}

	// Start the controller manager once — tests trigger the reconciler by
	// creating/updating CRs and observing the resulting cluster state.
	testManager, err = startManager(context.Background(), restCfg, testScheme)
	if err != nil {
		_ = testEnv.Stop()
		fmt.Fprintf(os.Stderr, "manager start: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	testManager.stop()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "envtest stop: %v\n", err)
	}
	os.Exit(code)
}
