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

package v1alpha1

import "testing"

// Condition type strings are part of the public API: downstream operators,
// dashboards, and Helm chart values reference them. Pin the values here so a
// rename triggers a compile-visible change in the test diff, not a silent
// break of consumers.

func TestConditionTypes(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{got: ConditionTypeReady, want: "Ready"},
		{got: ConditionTypeTokenSecretBound, want: "TokenSecretBound"},
	}

	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("condition type changed: got %q, want %q", c.got, c.want)
		}
	}
}
