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
	"fmt"

	"sigs.k8s.io/yaml"

	accessv1alpha1 "github.com/giantswarm/tunnelport/api/v1alpha1"
)

// tbot_config.go: typed schema and marshaling for the tbot.yaml content
// that lands in the rendered ConfigMap. Split out from render.go because
// it speaks tbot's on-disk config format (an external, evolving schema)
// rather than Kubernetes object shape — keeping the two concerns in
// separate files makes the surface that has to track upstream tbot
// releases obvious at a glance.

// tbotFile mirrors the slice of tbot's config schema we use. Field order
// is the on-the-wire YAML order. We encode through sigs.k8s.io/yaml — a
// JSON↔YAML bridge that round-trips Go structs via `encoding/json` and
// then converts the JSON to YAML — so the struct tags are `json:` (not
// `yaml:`). Using sigs.k8s.io/yaml lets us drop the second YAML library
// (gopkg.in/yaml.v3) the operator was previously linking. Schema source:
// gravitational/teleport `lib/tbot/...`:
//
//   - top-level: `version`, `proxy_server`, `insecure`, `onboarding`,
//     `storage`, `diag_addr`, `services` (see `lib/tbot/config/config.go`).
//   - `onboarding.token` accepts either a literal token value OR a file
//     path; tbot dereferences a path automatically. We point it at the
//     mounted Secret's key, so the literal token never leaves the Secret
//     volume — the operator has no need to read `Secret.Data`.
//   - `services.application-tunnel.listen` (NOT `listener`) — the upstream
//     YAML tag is `listen` (see `lib/tbot/services/application/tunnel_config.go`).
//   - `diag_addr` enables tbot's diag HTTP listener that serves `/readyz`,
//     which the pod readiness probe targets.
type tbotFile struct {
	Version     string `json:"version"`
	ProxyServer string `json:"proxy_server"`
	// Insecure uses bool+omitempty so `false` drops the key entirely —
	// the production render must not include `insecure:` at all.
	Insecure   bool           `json:"insecure,omitempty"`
	Onboarding tbotOnboarding `json:"onboarding"`
	Storage    tbotStorage    `json:"storage"`
	DiagAddr   string         `json:"diag_addr"`
	Services   []tbotService  `json:"services"`
}

type tbotOnboarding struct {
	JoinMethod string `json:"join_method"`
	Token      string `json:"token"`
}

type tbotStorage struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

type tbotService struct {
	Type    string `json:"type"`
	AppName string `json:"app_name,omitempty"`
	Listen  string `json:"listen,omitempty"`
}

func tbotConfig(cr *accessv1alpha1.RemoteApp, cfg PodDefaults) string {
	doc := tbotFile{
		Version:     "v2",
		ProxyServer: cr.Spec.ProxyAddr,
		Insecure:    cfg.Insecure,
		Onboarding: tbotOnboarding{
			JoinMethod: "token",
			Token:      fmt.Sprintf("/etc/tbot-token/%s", cr.Spec.TokenRef.Key),
		},
		Storage: tbotStorage{
			Type: "directory",
			Path: mountPathTbotStorage,
		},
		DiagAddr: fmt.Sprintf("0.0.0.0:%d", tbotDiagPort),
		Services: []tbotService{
			{
				Type:    "application-tunnel",
				AppName: cr.Spec.AppName,
				Listen:  fmt.Sprintf("tcp://0.0.0.0:%d", cr.Spec.Port),
			},
		},
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		// Fixed-shape struct of scalars/slices: marshal cannot fail in
		// practice. We panic rather than threading an error return up
		// because the only callers (renderConfigMap, configHash) have
		// signatures fixed by the API surface controller.go calls into,
		// and changing those would force this slice to also touch
		// controller.go (which is owned by another bundle).
		// TODO: propagate this error once renderConfigMap can return one
		// — see Issue #14.
		panic(fmt.Errorf("tbotConfig marshal: %w", err))
	}
	return string(out)
}
