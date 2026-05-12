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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// fixtureOpt mutates a *RemoteApp during fixture construction. Every test
// that needs a non-default value composes options instead of hand-rolling
// another flavour of "minimal valid CR" — there used to be five.
type fixtureOpt func(*accessv1alpha1.RemoteApp)

// newRemoteApp returns a minimal valid RemoteApp the renderers and the
// reconciler can be driven from. Callers compose fixtureOpts to tweak the
// fields they care about; the defaults pass CRD validation. UID is set so
// generated OwnerReferences match what the API server would persist when
// the CR is fed straight to the renderers without a Create round trip.
func newRemoteApp(opts ...fixtureOpt) *accessv1alpha1.RemoteApp {
	cr := &accessv1alpha1.RemoteApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "default",
			UID:       types.UID("uid-demo"),
		},
		Spec: accessv1alpha1.RemoteAppSpec{
			AppName:     "demo-app",
			Port:        8080,
			ProxyAddr:   "teleport.example.com:443",
			TokenName:   "demo-token",
			ClusterName: "teleport.example.com",
		},
	}
	for _, o := range opts {
		o(cr)
	}
	return cr
}

// withName sets ObjectMeta.Namespace and ObjectMeta.Name in one call —
// every call site happens to need both.
func withName(namespace, name string) fixtureOpt {
	return func(cr *accessv1alpha1.RemoteApp) {
		cr.Namespace = namespace
		cr.Name = name
	}
}

// withUID overrides the default UID. Useful when the test expects a
// specific OwnerReference UID it set itself rather than the fixture default.
func withUID(uid string) fixtureOpt {
	return func(cr *accessv1alpha1.RemoteApp) {
		cr.UID = types.UID(uid)
	}
}

// withAppName overrides the tbot Application Service app name.
func withAppName(name string) fixtureOpt {
	return func(cr *accessv1alpha1.RemoteApp) {
		cr.Spec.AppName = name
	}
}

// withTokenName overrides spec.tokenName. Under the kubernetes-join
// model (ADR 0004) this is the literal name of the ProvisionToken on
// Central — not a Kubernetes Secret reference.
func withTokenName(name string) fixtureOpt {
	return func(cr *accessv1alpha1.RemoteApp) {
		cr.Spec.TokenName = name
	}
}
