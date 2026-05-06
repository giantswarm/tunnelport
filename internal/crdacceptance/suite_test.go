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

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// envtest bootstrap shared by every validation test in this package.
// Tests assert the API server's reaction to RemoteApp CRs against the
// real CRD generated from kubebuilder markers.

var (
	testEnv   *envtest.Environment
	restCfg   *rest.Config
	k8sClient client.Client
	scheme    = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if err := accessv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "scheme registration: %v\n", err)
		os.Exit(1)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start: %v\n", err)
		os.Exit(1)
	}
	restCfg = cfg

	k8sClient, err = client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = testEnv.Stop()
		fmt.Fprintf(os.Stderr, "client init: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "envtest stop: %v\n", err)
	}
	os.Exit(code)
}
