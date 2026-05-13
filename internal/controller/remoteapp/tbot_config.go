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

// tbotWISelectorLabelRemoteApp is the label key the per-RemoteApp
// WorkloadIdentity resource on Teleport central is stamped with
// (hack/smoke/teleport/workload-identity.yaml). The tbot
// workload-identity-x509 service's `selector.labels` map pins this key
// to the CR's name so the SVID is minted against exactly one identity.
const tbotWISelectorLabelRemoteApp = "remoteapp"

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
//   - `onboarding.join_method` is `kubernetes` (ADR 0004). `onboarding.token`
//     is the literal name of the ProvisionToken on Central — NOT a file
//     path. tbot reads its own ServiceAccount JWT from the projected
//     volume the kubelet mounts automatically and presents it to Teleport
//     auth, which validates the JWT against the ProvisionToken's
//     `static_jwks` and `allow` rules.
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
	// Fields below back the workload-identity-x509 service block (ADR
	// 0007). Schema source: gravitational/teleport
	// `lib/tbot/services/workloadidentity/x509_output_config.go`:
	// `Destination` is singular — exactly one destination per service.
	// The trust-bundle Secret (slice 03) is therefore a SECOND
	// workload-identity-x509 service in the same tbot.yaml, sharing
	// the same selector but with a `kubernetes_secret` destination
	// instead of `directory`. credential_ttl / renewal_interval are
	// inlined (CredentialLifetime in upstream) at the service level.
	Selector        *tbotWISelector    `json:"selector,omitempty"`
	Destination     *tbotWIDestination `json:"destination,omitempty"`
	CredentialTTL   string             `json:"credential_ttl,omitempty"`
	RenewalInterval string             `json:"renewal_interval,omitempty"`
}

// tbotWISelector mirrors the workload-identity-x509 service's `selector`
// field. The `labels` map matches a single WorkloadIdentity resource on
// Teleport central; the per-RemoteApp resource is labelled
// `remoteapp: <cr.Name>` (hack/smoke/teleport/workload-identity.yaml).
//
// Values are []string, not scalar — tbot's upstream schema is list-valued
// so a single key can match multiple WorkloadIdentity resources at once.
// A scalar value fails tbot's config parse with
// `cannot unmarshal !!str into []string`. We always emit a one-element
// list (singular RemoteApp scoping is enforced by the role's
// workload_identity_labels — same shape — in hack/smoke/teleport/role.yaml).
type tbotWISelector struct {
	Labels map[string][]string `json:"labels,omitempty"`
}

// tbotWIDestination is one element of the workload-identity-x509 service's
// `destinations:` list. Slice 01 emits the `directory` variant (ghostunnel
// reads the SVID trio from a shared emptyDir); slice 03 adds the
// `kubernetes_secret` variant — tbot writes `svid_bundle.pem` directly into
// a Kubernetes Secret so consumer pods can mount the bundle for chain
// verification (ADR 0007 §"Trust bundle distribution to consumers").
//
// `path` is the on-disk directory for the `directory` variant.
// `name` is the Secret name (in tbot's own namespace, the same namespace
// the RemoteApp lives in) for the `kubernetes_secret` variant.
type tbotWIDestination struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
	Name string `json:"name,omitempty"`
}

func tbotConfig(cr *accessv1alpha1.RemoteApp, cfg PodDefaults) string {
	doc := tbotFile{
		Version:     "v2",
		ProxyServer: cfg.TeleportProxyAddr,
		Insecure:    cfg.Insecure,
		Onboarding: tbotOnboarding{
			JoinMethod: "kubernetes",
			Token:      cr.Spec.TokenName,
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
			// First workload-identity-x509 service: directory destination.
			// Slice 01 / ADR 0007. The ghostunnel sidecar (slice 02) reads
			// svid.pem / svid_key.pem from the shared emptyDir mount.
			workloadIdentityX509Service(cr, &tbotWIDestination{
				Type: "directory",
				Path: mountPathSVID,
			}),
			// Second workload-identity-x509 service: kubernetes_secret
			// destination. Slice 03 / ADR 0007 §"Trust bundle distribution".
			// tbot writes svid_bundle.pem into the per-CR Secret the
			// operator pre-creates with an ownerRef back to the RemoteApp.
			// A second service block is required because upstream
			// workload-identity-x509 supports exactly one `destination:`
			// per service — see the struct doc on `tbotService.Destination`.
			workloadIdentityX509Service(cr, &tbotWIDestination{
				Type: "kubernetes_secret",
				Name: trustBundleSecretName(cr),
			}),
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

// workloadIdentityX509Service returns a workload-identity-x509 service
// bound to the per-RemoteApp WorkloadIdentity (label-matched by
// `remoteapp: <cr.Name>`) with the supplied destination. Upstream's
// X509OutputConfig has exactly one Destination per service, so the
// directory and kubernetes_secret destinations are emitted as two
// separate service blocks rather than two entries on one (ADR 0007).
func workloadIdentityX509Service(cr *accessv1alpha1.RemoteApp, dest *tbotWIDestination) tbotService {
	return tbotService{
		Type: "workload-identity-x509",
		Selector: &tbotWISelector{
			Labels: map[string][]string{
				tbotWISelectorLabelRemoteApp: {cr.Name},
			},
		},
		Destination: dest,
		// 60min TTL / 20min renewal — bounds blast radius and matches
		// ghostunnel's file-watch reload cadence (Issue 01 criterion).
		// Strings flow through tbot's yaml.v3 time.Duration parsing.
		CredentialTTL:   "60m",
		RenewalInterval: "20m",
	}
}
