# tunnelport

tunnelport — Giant Swarm operator that wraps a Teleport tbot
application-tunnel + a Kubernetes Service behind a single `RemoteApp` CR,
so workloads on a consumer management cluster can reach a
Teleport-exposed app as if it were a local Service.

**Homepage:** <https://github.com/giantswarm/tunnelport>

## Source Code

* <https://github.com/giantswarm/tunnelport>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| installNamespace | string | `"tunnelport-system"` |  |
| replicaCount | int | `2` |  |
| podDisruptionBudget.enabled | bool | `true` |  |
| podDisruptionBudget.minAvailable | int | `1` |  |
| podDisruptionBudget.unhealthyPodEvictionPolicy | string | `"AlwaysAllow"` |  |
| image.registry | string | `"gsoci.azurecr.io"` |  |
| image.name | string | `"giantswarm/tunnelport"` |  |
| image.tag | string | `""` |  |
| image.pullPolicy | string | `"IfNotPresent"` |  |
| imagePullSecret | string | `"gsoci-pull-secret"` |  |
| resources.requests.cpu | string | `"50m"` |  |
| resources.requests.memory | string | `"64Mi"` |  |
| resources.limits.cpu | string | `"200m"` |  |
| resources.limits.memory | string | `"256Mi"` |  |
| teleport.clusterName | string | `""` | Teleport cluster name (the `Cluster:` line from `tctl status` on Central). Used as the `aud` claim on every rendered tbot pod's projected ServiceAccount JWT — Teleport's `static_jwks` validator pins JWT `aud` to this exact value. DNS-1123 subdomain shape, e.g. teleport.example.com. Required at install time (ADR 0005). |
| teleport.proxyAddr | string | `""` | host:port of the Teleport proxy every rendered tbot pod connects to, e.g. teleport.example.com:443. Flows into `proxy_server` in the rendered tbot.yaml. Required at install time (ADR 0005). |
| tbot.image.registry | string | `"gsoci.azurecr.io"` |  |
| tbot.image.name | string | `"giantswarm/tbot-distroless"` |  |
| tbot.image.tag | string | `"18"` |  |
| tbot.image.digest | string | `"sha256:0d94fd0d9910f32f76914d4a5699b7b89b23e211de39298fc1b4305c8c153755"` |  |
| tbot.insecure | bool | `false` |  |
| tbot.resources.requests.cpu | string | `"50m"` |  |
| tbot.resources.requests.memory | string | `"64Mi"` |  |
| tbot.resources.limits.cpu | string | `"200m"` |  |
| tbot.resources.limits.memory | string | `"256Mi"` |  |
| tls.image.registry | string | `"gsoci.azurecr.io"` |  |
| tls.image.name | string | `"giantswarm/ghostunnel"` |  |
| tls.image.tag | string | `"v1.10.0"` |  |
| tls.image.digest | string | `"sha256:ab209cc0c8eb7020a826ea8052aade2b77d7d9ce3724fab7ec34aba5cdf2e153"` |  |
| tls.reloadInterval | string | `"5m"` |  |
| tls.port | int | `8443` |  |
| crds.install | bool | `true` |  |
| networkPolicy.enabled | bool | `true` |  |
| verification.enabled | bool | `true` |  |
| verification.interval | string | `"2m"` |  |
| verification.timeout | string | `"5s"` |  |
| verification.jitter | string | `"30s"` | Window over which one round's probes are spread: each Ready RemoteApp is probed at a random offset in [0, jitter) from the round start, so a fleet of ~40 tunnels averages one Teleport app session per second instead of opening 40 at the same instant every interval. Clamped to interval/2. |
| verification.concurrency | int | `8` |  |
| verification.clusterDomain | string | `"cluster.local"` |  |
| verification.trustBundleSecretName | string | `""` | Name of a Secret in installNamespace whose `svid_bundle.pem` key holds the CA bundle to verify tunnels against, for installs without the singleton trust-bundle bot (`trustBundle.enabled=false`). Empty uses the bot's Secret (`trustBundle.secretName`). The mount is optional so the manager starts before the Secret exists and reports tunnelport_tls_verification_available 0 until it does. |
| verification.upstream.enabled | bool | `true` | Send one HTTP request through each verified tunnel per round and report the answer as the `UpstreamReachable` condition, folded into `Ready`: 502/503/504 or no response is `Ready=False` with reason `UpstreamUnreachable`; any other status (200, 401, 404) is reachable. Disable to keep TLS verification only and `Ready` join-level. |
| verification.upstream.timeout | string | `"10s"` | Budget for the request through the tunnel and its response. Separate from `timeout` because the request crosses the Teleport proxy and the app service. |
| monitoring.podMonitor.enabled | bool | `true` |  |
| monitoring.podMonitor.interval | string | `"60s"` |  |
| monitoring.podMonitor.scrapeTimeout | string | `"30s"` |  |
| monitoring.podMonitor.labels."observability.giantswarm.io/tenant" | string | `"giantswarm"` |  |
| monitoring.prometheusRule.enabled | bool | `true` |  |
| monitoring.prometheusRule.labels."observability.giantswarm.io/tenant" | string | `"giantswarm"` |  |
| ports.metrics | int | `8080` |  |
| ports.health | int | `8081` |  |
| podSecurityContext.runAsNonRoot | bool | `true` |  |
| podSecurityContext.runAsUser | int | `65532` |  |
| podSecurityContext.runAsGroup | int | `65532` |  |
| podSecurityContext.fsGroup | int | `65532` |  |
| podSecurityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| securityContext.allowPrivilegeEscalation | bool | `false` |  |
| securityContext.capabilities.drop[0] | string | `"ALL"` |  |
| securityContext.readOnlyRootFilesystem | bool | `true` |  |
| securityContext.runAsNonRoot | bool | `true` |  |
| securityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| nodeSelector | object | `{}` |  |
| tolerations | list | `[]` |  |
| affinity | object | `{}` |  |
| trustBundle.enabled | bool | `true` |  |
| trustBundle.secretName | string | `"tunnelport-spiffe-bundle"` |  |
| trustBundle.tokenName | string | `""` |  |
| trustBundle.workloadIdentityLabels.trust-bundle[0] | string | `"tunnelport"` |  |
| trustBundle.credentialTTL | string | `"60m"` |  |
| trustBundle.renewalInterval | string | `"20m"` |  |
| trustBundle.resources.requests.cpu | string | `"50m"` |  |
| trustBundle.resources.requests.memory | string | `"64Mi"` |  |
| trustBundle.resources.limits.cpu | string | `"200m"` |  |
| trustBundle.resources.limits.memory | string | `"256Mi"` |  |
