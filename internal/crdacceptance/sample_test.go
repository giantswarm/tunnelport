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
	"context"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// The committed sample CR must remain valid against the CRD. Without this
// guard, marker changes in remoteapp_types.go can silently invalidate the
// sample, leaving consumers (kustomize bases, docs, dev workflows) broken.

func TestSampleRemoteApp_IsAcceptedByAPIServer(t *testing.T) {
	path := filepath.Join("..", "..", "config", "samples", "access_v1alpha1_remoteapp.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}

	cr := &accessv1alpha1.RemoteApp{}
	if err := yaml.Unmarshal(raw, cr); err != nil {
		t.Fatalf("decode sample: %v", err)
	}

	cr.Namespace = "default"
	cr.ResourceVersion = ""

	if err := k8sClient.Create(context.Background(), cr); err != nil {
		t.Fatalf("apiserver rejected sample CR: %v", err)
	}
}
