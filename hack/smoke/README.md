# Tunnelport smoke test: zero to green

This runbook stands up an end-to-end environment proving the operator
works against real Teleport. By the end you will have a `curl` from a
pod in one kind cluster reach a sample HTTP responder in another, via
a tbot tunnel rendered by the operator.

The smoke test is intentionally **insecure** in a few ways (self-signed
CAs, `--insecure` proxy connections, plain `kubectl create secret`).
None of these belong in production. See the "Production differences"
section at the end.

## Topology

Three kind clusters, all on the default `kind` Docker network:

```
┌────────────────┐   ┌────────────────┐   ┌────────────────┐
│  teleport      │   │  producer      │   │  consumer      │
│  ─ Teleport    │   │  ─ http-echo   │   │  ─ operator    │
│    auth+proxy  │◄──┼─ teleport-     │   │  ─ tbot pod    │
│    (helm)      │   │    kube-agent  │   │    (rendered)  │
│  ─ tctl bot/   │◄──┼─────────────────┼───┤  ─ curl Job   │
│    role/token  │   │                │   │                │
└────────────────┘   └────────────────┘   └────────────────┘
```

- **teleport**: control plane via the official `teleport-cluster` Helm
  chart. Version is derived at runtime from
  `helm/tunnelport/values.yaml`'s `tbot.image` major (latest patch in
  that major from the upstream chart) — so the smoke tracks whatever
  Teleport major the chart's default tbot is built for. Bot/role/
  token are provisioned by `tctl` after the chart is up.
- **producer**: hosts the HTTP responder (`hashicorp/http-echo`) and a
  `teleport-kube-agent` (same version as the control plane above)
  registering it as the Teleport application named `smoke-app`.
- **consumer**: runs the operator and the test workload. The operator
  renders a tbot Deployment from a `RemoteApp` CR. The `curl` Job hits
  the rendered Service.

## Prerequisites

- macOS or Linux with Docker Desktop / Docker Engine
- `kind` ≥ 0.24
- `kubectl`
- `helm` ≥ 3.14
- `teleport`, `tctl`, `tbot` — major version must match the smoke's
  resolved server version (auto-derived from
  `helm/tunnelport/values.yaml`'s `tbot.image` major; see step 1)
  (download: <https://goteleport.com/download/> — pick the matching
  client tarball for your OS)
- `jq` (for parsing `tctl` output)

## 1. Bring up the Teleport control plane

```bash
kind create cluster --config hack/smoke/teleport/kind.yaml

# The teleport-cluster Helm chart lives in the Teleport upstream repo.
helm repo add teleport https://charts.releases.teleport.dev
helm repo update

# Single source of truth: the chart's tbot.image major. Pick the
# latest patch in that major from upstream so the server we test
# against can never drift to a different major from the operator's
# chart-default tbot.
TBOT_MAJOR="$(helm template ./helm/tunnelport |
  grep -oE -- '--tbot-image=[^[:space:]]+' | head -1 |
  sed -E 's|.*tbot-distroless:([0-9]+).*|\1|')"
TELEPORT_CHART_VERSION="$(helm search repo teleport/teleport-cluster --versions -o json |
  jq -r --arg M "${TBOT_MAJOR}" '[.[] | select(.version | startswith($M+"."))] | first | .version')"

# First install — uses the placeholder publicAddr in helm-values.yaml.
# We come back in step 2 with the real proxy address.
helm --kube-context kind-teleport upgrade --install teleport-cluster \
  teleport/teleport-cluster --version "${TELEPORT_CHART_VERSION}" \
  --create-namespace --namespace teleport \
  --values hack/smoke/teleport/helm-values.yaml \
  --wait --timeout 5m
```

What's happening: the chart deploys split auth + proxy `Deployments`
with `proxyListenerMode=multiplex`, so every Teleport protocol shares
one TLS port on the proxy pod. The chart generates its own self-signed
CA on first install — that CA is private to this kind cluster, which
is why the consumer's tbot (`--tbot-insecure=true`) and the producer's
kube-agent (`insecureSkipProxyTLSVerify: true`) skip TLS verification
in step 5+.

The proxy is exposed via a `NodePort` Service so peer kind clusters
can reach it on the kind-teleport node container's IP within the
`kind` Docker network.

## 2. Network setup: discover the proxy address, then re-apply with publicAddr

The proxy advertises a `publicAddr` to clients for reverse-tunnel
reconnects. Without overriding it, the proxy hands out
`smoke.tunnelport.local:443` — a name peer kind clusters cannot
resolve — and kube-agent / tbot fail with DNS errors. We discover the
real address and re-apply the chart with it.

```bash
# Discover the kind container's IP on the `kind` Docker network.
TELEPORT_PROXY_IP=$(docker inspect teleport-control-plane \
  --format '{{ .NetworkSettings.Networks.kind.IPAddress }}')

# The proxy's NodePort is auto-assigned by Kubernetes; read it back.
NODEPORT=$(kubectl --context kind-teleport -n teleport get svc \
  teleport-cluster -o jsonpath='{.spec.ports[0].nodePort}')

TELEPORT_PROXY_ADDR="${TELEPORT_PROXY_IP}:${NODEPORT}"
echo "TELEPORT_PROXY_ADDR=${TELEPORT_PROXY_ADDR}"

# Re-apply the chart with publicAddr set to the discovered address.
sed "s|REPLACE_WITH_TELEPORT_PROXY_ADDR|${TELEPORT_PROXY_ADDR}|" \
  hack/smoke/teleport/helm-values.yaml > /tmp/teleport-values.yaml

helm --kube-context kind-teleport upgrade teleport-cluster \
  teleport/teleport-cluster --version "${TELEPORT_CHART_VERSION}" \
  --namespace teleport --values /tmp/teleport-values.yaml \
  --wait --timeout 3m
```

Keep `TELEPORT_PROXY_ADDR` in scope; later steps reference it in
`sed` calls. If you start a new shell, re-run the `docker inspect` +
`kubectl get svc` pair — the IP and NodePort are stable for the
lifetime of the kind cluster but do not survive a recreate.

## 3. Provision the producer agent token

```bash
# Find the auth pod (the chart splits auth and proxy into separate
# Deployments; only the auth pod has tctl wired in).
AUTH_POD=$(kubectl --context kind-teleport -n teleport get pods \
  -l app.kubernetes.io/component=auth \
  -o jsonpath='{.items[0].metadata.name}')

# Create a Node+App join token; capture the token string for step 5.
kubectl --context kind-teleport -n teleport exec "$AUTH_POD" -- \
  tctl tokens add --type=node,app --format=json \
  > /tmp/producer-agent-token.json
jq -r .token /tmp/producer-agent-token.json > /tmp/producer-agent-token

cat /tmp/producer-agent-token   # sanity: a hex string
```

Why this isn't `tctl create -f hack/smoke/teleport/tokens.yaml`: the
YAML in `tokens.yaml` is a **reference** for the schema. The actual
token strings are generated server-side. If you'd rather use the
declarative path, replace `REPLACE_WITH_PRODUCER_AGENT_TOKEN` in
`tokens.yaml` with a value of your own and `tctl create -f` it
instead.

## 4. Provision the role, bot, and bot token (bound_keypair)

Per ADR 0005 the operator joins via `bound_keypair` with
`recovery.mode: relaxed`. The Central-side flow is fully declarative:
apply the role, the bot, and the token resource, then retrieve the
auto-generated registration secret from the token's `.status` for
delivery to the consumer cluster.

```bash
# 4a. Role (allows the bot to tunnel to apps labelled app-name=smoke-app):
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/role.yaml

# 4b. Bot resource — names the identity, binds it to the role.
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/bot.yaml

# 4c. Token resource — declares join_method: bound_keypair and
#     recovery.mode: relaxed (see hack/smoke/teleport/tokens.yaml).
#     The producer-agent-token entry in the same file is a placeholder
#     for step 3; only the smoke-bot-token entry is needed here. The
#     simplest path is to apply the file as-is and ignore the producer
#     entry — `tctl create -f` is idempotent on identical specs.
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/tokens.yaml || true
# (The `|| true` swallows the producer-agent-token's "literal token
# missing" error if you haven't materialised it yet — that token is
# step 3's responsibility, not step 4's.)

# 4d. Read out the registration secret Teleport generated for the
#     bound_keypair token. This value is what tbot presents on first
#     join; the operator delivers it to the consumer cluster as a
#     plain Secret in step 6c.
kubectl --context kind-teleport -n teleport exec "$AUTH_POD" -- \
  tctl get token/smoke-bot-token --format=json \
  | jq -r '.[0].status.bound_keypair.registration_secret' \
  > /tmp/smoke-bot-registration-secret

cat /tmp/smoke-bot-registration-secret   # sanity: a base64-ish string
```

Why declarative end-to-end: bound_keypair tokens carry policy
(`recovery.mode`, optional `recovery.limit`, `bot_name`, `roles`) that
`tctl bots add` cannot express via flags. `tctl create -f` against the
checked-in YAML is the only path that captures the policy in source
control alongside the rest of the smoke harness.

**Naming convention** (load-bearing): the operator renders
`onboarding.token: <name>` in tbot.yaml using the consumer-side Secret
name (`RemoteApp.spec.tokenRef.name`). That string MUST match the
Central-side token resource's `metadata.name`. In the smoke harness
both are `smoke-bot-token`; in production, the platform team is
responsible for keeping the names in lockstep when provisioning a new
`RemoteApp`.

## 5. Bring up the producer cluster

```bash
kind create cluster --config hack/smoke/producer/kind.yaml

# Sample app:
kubectl --context kind-producer apply -f hack/smoke/producer/http-echo.yaml

# Substitute the agent token + proxy address into the kube-agent values:
sed \
  -e "s|REPLACE_WITH_PRODUCER_AGENT_TOKEN|$(cat /tmp/producer-agent-token)|" \
  -e "s|REPLACE_WITH_TELEPORT_PROXY_ADDR|${TELEPORT_PROXY_ADDR}|" \
  hack/smoke/producer/teleport-kube-agent-values.yaml \
  > /tmp/producer-kube-agent-values.yaml

helm --kube-context kind-producer upgrade --install teleport-kube-agent \
  teleport/teleport-kube-agent --version "${TELEPORT_CHART_VERSION}" \
  --create-namespace --namespace smoke \
  --values /tmp/producer-kube-agent-values.yaml \
  --wait --timeout 3m
```

Verify the app registered with Teleport. The kube-agent registers via
the dynamic `app_servers` registry (heartbeats), not the static `apps`
registry — so use `tctl get app_servers`, not `tctl get apps`:

```bash
kubectl --context kind-teleport -n teleport exec "$AUTH_POD" -- \
  tctl get app_servers
# Should show: kind: app_server / name: smoke-app / uri: http://http-echo.smoke...
```

## 6. Bring up the consumer cluster

```bash
kind create cluster --config hack/smoke/consumer/kind.yaml
```

### 6a. Build and load the operator image

The chart references `gsoci.azurecr.io/giantswarm/tunnelport:<tag>` by
default, but for local smoke runs you want the code in this checkout.
Build a local image and load it into the consumer kind cluster:

```bash
make docker-build IMG=tunnelport:smoke
kind load docker-image tunnelport:smoke --name consumer
```

### 6b. Install the operator

`tbot.insecure=true` is critical: the kind-deployed Teleport proxy
serves a cert with SAN `smoke.tunnelport.local`, but tbot reaches it
by IP. Without this flag the rendered tbot pod cannot pass TLS
verification and will crashloop. Never set `tbot.insecure=true` in
production.

```bash
helm --kube-context kind-consumer upgrade --install tunnelport \
  ./helm/tunnelport \
  --create-namespace --namespace tunnelport-system \
  --set image.registry=docker.io \
  --set image.name=library/tunnelport \
  --set image.tag=smoke \
  --set image.pullPolicy=IfNotPresent \
  --set imagePullSecret="" \
  --set tbot.insecure=true \
  --wait --timeout 2m
```

`tbot.image` is intentionally not overridden — the smoke uses the
chart's default so a default-shaped install is exercised end-to-end.
This is what catches major-version skew between the chart-default
tbot and the Teleport server tested above. The default flows through
to the rendered tbot Deployment in step 6d.

### 6c. Deliver the registration-secret Secret

For the smoke environment we use plain `kubectl create secret`. In
production this is replaced by sealed-secrets / external-secrets-
operator / your team's chosen pattern.

The Secret carries the **bound_keypair registration secret** (step
4d), not a multi-use static token. With `recovery.mode: relaxed`, the
secret remains usable for re-registration indefinitely — see
`helm/tunnelport/README.md` "Registration-secret rotation" for the
production rotation contract.

```bash
kubectl --context kind-consumer create namespace smoke || true
kubectl --context kind-consumer -n smoke create secret generic smoke-bot-token \
  --from-literal=token=$(cat /tmp/smoke-bot-registration-secret)
```

Alternative: edit `hack/smoke/consumer/token-secret.yaml.template`,
replacing `REPLACE_WITH_SMOKE_BOT_REGISTRATION_SECRET` with the literal
value, and `kubectl apply -f` it.

### 6d. Apply the RemoteApp CR

```bash
sed "s|REPLACE_WITH_TELEPORT_PROXY_ADDR|${TELEPORT_PROXY_ADDR}|" \
  hack/smoke/consumer/remoteapp.yaml \
  | kubectl --context kind-consumer apply -f -

# Wait for the operator to flip status.ready=true. The tbot pod's
# readiness probe hits tbot's diag /readyz, so this only flips green
# once the tunnel is established end-to-end.
kubectl --context kind-consumer -n smoke wait remoteapp/smoke-app \
  --for=jsonpath='{.status.ready}'=true --timeout=2m

kubectl --context kind-consumer -n smoke get remoteapp smoke-app -o yaml
```

If the wait times out, see "Troubleshooting" below.

## 7. Run the curl

```bash
kubectl --context kind-consumer apply -f hack/smoke/consumer/curl-pod.yaml

kubectl --context kind-consumer -n smoke wait job/smoke-curl \
  --for=condition=complete --timeout=2m

kubectl --context kind-consumer -n smoke logs job/smoke-curl
```

Expected output:

```
hello-from-producer
```

That string is the literal `-text` flag value passed to
`hashicorp/http-echo` in `producer/http-echo.yaml`. Reading it back
from the consumer cluster proves the data path works: curl → Service
→ tbot pod → Teleport tunnel → producer kube-agent → http-echo.

## Tearing down

```bash
kind delete cluster --name consumer
kind delete cluster --name producer
kind delete cluster --name teleport
rm /tmp/producer-agent-token /tmp/smoke-bot-registration-secret \
   /tmp/*.json /tmp/producer-kube-agent-values.yaml
```

## Troubleshooting

**`status.ready` stays false on the RemoteApp.**
Check the tbot pod's logs for the join failure:

```bash
kubectl --context kind-consumer -n smoke logs -l tunnelport.giantswarm.io/role=tbot
```

The most common smoke-test failures:

- *"trust dial: tls: failed to verify certificate"* — `tbot.insecure`
  is unset in step 6b's `helm install`. Re-run with `--set
  tbot.insecure=true`. (The kube-agent has the equivalent
  `insecureSkipProxyTLSVerify: true` baked into its values file.)
- *"failed to dial: dial tcp: lookup smoke.tunnelport.local"* — step 2
  was skipped or its `helm upgrade` did not pick up the discovered
  proxy address. The proxy is still advertising
  `smoke.tunnelport.local:443` as `publicAddr`. Re-run step 2,
  observing that `/tmp/teleport-values.yaml` contains the discovered
  IP+nodeport, then `kubectl rollout restart` the kube-agent and
  consumer's tbot Deployment.
- *"role smoke-app-tunnel not found"* — step 4a was skipped or the
  role file failed to apply. `tctl get roles | grep smoke` on the
  auth pod.
- *"app smoke-app not found"* — step 5's kube-agent didn't register.
  Use `tctl get app_servers` (NOT `tctl get apps`); the dynamic
  app_service registers via heartbeats, not as a static `apps`
  resource. If empty, check the kube-agent pod logs in the producer
  cluster.
- *"pods is forbidden"* in operator logs — the chart RBAC was
  installed before the chart's `pods` rule was committed. `helm
  upgrade` from the current chart in this repo and the error clears.

**`status.conditions[TokenSecretBound]: false`.**
The operator can't find the Secret or the named key. Confirm:

```bash
kubectl --context kind-consumer -n smoke get secret smoke-bot-token \
  -o jsonpath='{.data.token}' | base64 -d | head -c 8
```

You should see the first 8 hex chars of the token. If the field is
empty, redo step 6c.

**The curl Job fails with `Connection refused`.**
The tbot pod is up but the tunnel isn't established. The Job's
`--retry-connrefused` should ride this out; if it doesn't, the
registration secret in the Secret doesn't match what Teleport has on
the token resource. Re-run step 4d to read the current secret out of
`tctl get token/smoke-bot-token`; the Secret in step 6c must be the
same value. (If the secret has been consumed and you're not in
`recovery.mode: relaxed`, the value rotates and you must re-run 4d
+ 6c — but the smoke harness defaults to relaxed for exactly this
reason.)

**tbot logs `name "smoke-bot-token" not found`.**
The `onboarding.token` field in the rendered tbot.yaml is the *name
of the Teleport token resource*, not a literal value. If it doesn't
match the Central-side resource name from step 4c, tbot can't look up
the join policy. Double-check that `RemoteApp.spec.tokenRef.name`
(consumer-side, `hack/smoke/consumer/remoteapp.yaml`) equals the
`metadata.name` of the smoke-bot-token entry in
`hack/smoke/teleport/tokens.yaml`. The operator renders the former
into the latter's slot in tbot.yaml — the two MUST match.

## Production differences

This smoke runbook is deliberately not a deployment guide. The
production-targeting differences:

| Smoke (this runbook) | Production |
|---|---|
| Self-signed CA, `--insecure` everywhere | A real Teleport cluster with a CA bundle the agents trust |
| `kubectl create secret` for the bot token | sealed-secrets / external-secrets-operator / GitOps secret-sync |
| `tctl` from a host shell | `TeleportBot` / `TeleportRole` / `TeleportToken` via the Giant Swarm Teleport Operator on Central |
| One bot per smoke test | One bot **per RemoteApp**, scoped to a single app via `app_labels`. The operator does not enforce this — it's a Central-side policy decision per ADR 0001 |
| `proxyAddr` is a kind-network IP | A stable hostname resolvable from the consumer MC (e.g. `teleport.example.com:443`) |
