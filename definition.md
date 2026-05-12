Teleport App Egress Operator

  Goal

  Workloads in a GS MC consume Teleport-exposed apps as local Kubernetes Services, no Teleport SDK in caller code. Wraps tbot + Service behind a single CR.

  Architecture context

  - Teleport control plane runs on Central MC.
  - Producer MCs run teleport-kube-agent (already managed elsewhere).
  - TeleportApp CRs on Central are managed by the upstream Teleport Operator — out of scope here.
  - This operator runs in each consumer MC.

  CRD: TeleportAppEgress (access.giantswarm.io/v1alpha1)

  spec:
    appName: payments              # Teleport app name
    port: 8080                     # local listen + Service port
    proxyAddr: teleport.gs:443     # or from a cluster ConfigMap default
    bot:                           # how tbot joins
      name: egress-bot             # TeleportBot name on Central
      joinMethod: kubernetes
      tokenName: egress-bot-token  # Teleport ProvisionToken name on Central (ADR 0004)
    service:
      name: payments               # defaults to .metadata.name
      type: ClusterIP
    replicas: 2
  status:
    ready: bool
    lastError: string
    certExpiry: timestamp

  Reconcile loop

  For each CR, render and own:
  1. Secret — tbot config (application-tunnel, listen tcp://0.0.0.0:<port>, app_name, join config).
  2. Deployment — tbot image, mounts config Secret, runs as non-root, healthcheck on tbot diag port.
  3. Service — selects the tbot pod, exposes port.
  4. Update status from Deployment readiness + tbot diag endpoint.

  OwnerReferences on all rendered objects; CR delete cascades.

  Auth model

  - One TeleportBot per consumer MC, not per app.
  - Bot role grants access to apps via label match (e.g. egress-to=<mc-name>), so adding an app = label it on Central, no bot churn.
  - Join method: kubernetes (uses ServiceAccount JWT) — no static tokens to rotate.

  Out of scope

  - Producer-side TeleportApp (upstream Teleport Operator handles it).
  - NetworkPolicy generation — platform team's concern; document the required pattern (only allow approved callers → tbot pod).
  - The tbot binary / image (use upstream).

  Open decisions

  - One tbot per app vs one tbot with N tunnels. Per-app = cleaner blast radius and per-app metrics; multi-tunnel = fewer pods. Default to per-app for simplicity, revisit if pod count becomes a problem.
  - HA. replicas: 2 works because each tbot maintains its own tunnel; the Service load-balances. Verify there's no leader-election requirement for tunnel mode.
  - Defaults source. Per-cluster defaults (proxyAddr, bot name) via a singleton TeleportEgressConfig CR or a ConfigMap? Lean ConfigMap for v1.
  - Reusing one bot identity across CRs in the same namespace — yes, but watch for cert/identity file collisions; isolate per-CR output dirs.

  Failure modes to handle in status

  - Proxy unreachable → tbot crashloop, surface tbot logs tail in status.lastError.
  - App not found / RBAC denied on Teleport side.
  - Token expired (token method) or SA JWT misconfigured (kubernetes method).
  - Two CRs claiming the same service.name in a namespace — reject in admission.

  Stack

  - Go + kubebuilder / controller-runtime.
  - Helm chart for the operator itself (one chart, deployed to each consumer MC by the platform).
  - E2E: kind cluster + real Teleport (cloud or local) + a sample TeleportApp on a producer kind cluster.

  First milestone (1–2 weeks)

  1. CRD + scaffold.
  2. Happy-path reconcile: one CR → working tbot Deployment + Service.
  3. Smoke test: pod in consumer cluster curl http://<service>:<port> reaches the producer app.
  4. Surface tbot readiness in CR status.

  Iterate: HA, multiple-CRs-per-namespace, status richness, observability.

  References

  - tbot Helm chart: https://goteleport.com/docs/reference/helm-reference/tbot/
  - application-tunnel config: https://goteleport.com/docs/reference/cli/tbot/
  - Machine ID + App Access: https://goteleport.com/docs/enroll-resources/machine-id/access-guides/applications/
  - Teleport Operator (CRD prior art): https://goteleport.com/docs/zero-trust-access/infrastructure-as-code/teleport-operator/
  - teleport-kube-agent (producer side): https://goteleport.com/docs/reference/helm-reference/teleport-kube-agent/
