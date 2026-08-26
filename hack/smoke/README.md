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

# Discover the proxy address BEFORE installing the chart. Both halves
# are already known at this point: the IP because the kind node
# container exists, and the port because we pin it ourselves in
# proxy-nodeport.yaml (the chart has no nodePort knob — see that file).
TELEPORT_PROXY_IP=$(docker inspect teleport-control-plane \
  --format '{{ .NetworkSettings.Networks.kind.IPAddress }}')
TELEPORT_PROXY_NODEPORT=$(sed -n \
  's/^[[:space:]]*nodePort:[[:space:]]*\([0-9]\{1,\}\)[[:space:]]*$/\1/p' \
  hack/smoke/teleport/proxy-nodeport.yaml | head -1)
TELEPORT_PROXY_ADDR="${TELEPORT_PROXY_IP}:${TELEPORT_PROXY_NODEPORT}"
echo "TELEPORT_PROXY_ADDR=${TELEPORT_PROXY_ADDR}"

# The pinned NodePort Service goes in first, so the port is reserved
# and the proxy's ingress path exists before any proxy pod does.
kubectl --context kind-teleport create namespace teleport
kubectl --context kind-teleport apply -f hack/smoke/teleport/proxy-nodeport.yaml

# One install, with the real publicAddr already substituted. Do NOT
# install helm-values.yaml unsubstituted and fix it up afterwards: a
# proxy that boots with the REPLACE_WITH_TELEPORT_PROXY_ADDR
# placeholder registers a proxy heartbeat carrying it, the heartbeat
# outlives the pod by its announce TTL, and step 5's app agent then
# derives smoke-app's public address from whichever heartbeat it
# reads. That was the ~50% flake in #92.
sed "s|REPLACE_WITH_TELEPORT_PROXY_ADDR|${TELEPORT_PROXY_ADDR}|" \
  hack/smoke/teleport/helm-values.yaml > /tmp/teleport-values.yaml

helm --kube-context kind-teleport upgrade --install teleport-cluster \
  teleport/teleport-cluster --version "${TELEPORT_CHART_VERSION}" \
  --namespace teleport --values /tmp/teleport-values.yaml \
  --wait --timeout 5m
```

Keep `TELEPORT_PROXY_ADDR` in scope; later steps reference it in
`sed` calls. If you start a new shell, re-run the `docker inspect` +
`sed` pair — the IP is stable for the lifetime of the kind cluster but
does not survive a recreate, and the port is a constant.

What's happening: the chart deploys split auth + proxy `Deployments`
with `proxyListenerMode=multiplex`, so every Teleport protocol shares
one TLS port on the proxy pod. The chart generates its own self-signed
CA on first install — that CA is private to this kind cluster, which
is why the consumer's tbot (`--tbot-insecure=true`) and the producer's
kube-agent (`insecureSkipProxyTLSVerify: true`) skip TLS verification
in step 5+.

The proxy is exposed via the `NodePort` Service applied above, so peer
kind clusters can reach it on the kind-teleport node container's IP
within the `kind` Docker network. The chart's own proxy Service stays
`ClusterIP`.

## 2. Network setup: verify what the proxy advertises

The proxy advertises a `publicAddr` to clients for reverse-tunnel
reconnects. Without overriding it, the proxy hands out
`smoke.tunnelport.local:443` — a name peer kind clusters cannot
resolve — and kube-agent / tbot fail with DNS errors. Step 1
substituted the discovered address; these two checks confirm it took,
and they are worth running by hand because everything downstream
derives its address from them.

```bash
# 1. Our Service's selector still matches the chart's proxy pods.
#    Empty output here means upstream renamed a label.
kubectl --context kind-teleport -n teleport get endpoints \
  teleport-proxy-nodeport -o jsonpath='{.subsets[*].addresses[*].ip}'

# 2. Every proxy heartbeat in the auth backend advertises the real
#    address and none carries the placeholder. Teleport lowercases it.
AUTH_POD=$(kubectl --context kind-teleport -n teleport get pods \
  -l app.kubernetes.io/component=auth -o jsonpath='{.items[0].metadata.name}')
kubectl --context kind-teleport -n teleport exec "$AUTH_POD" -- \
  tctl get proxies | grep -E 'name:|public_addr'
```

The second command must show `${TELEPORT_PROXY_IP}` and must not show
`replace_with_teleport_proxy_addr`. `run.sh` asserts both.

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

## 4. Provision the role and bot identity

Under ADR 0004 the bot token uses the **kubernetes** join method,
which means it's bound to the consumer cluster's JWKS — and we don't
have the consumer cluster yet. So this step only creates the role and
the bot identity; the kubernetes-method token is created in step 6c
after the consumer cluster is up and we can read its JWKS.

```bash
# 4a. Role (allows the bot to tunnel to apps labelled app-name=smoke-app):
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/role.yaml

# 4b. Bot identity (no token yet — the kubernetes-method token is
#     created in step 6c).
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/bot.yaml
```

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

### 6c. Export the consumer cluster's JWKS and create the kubernetes-method bot token

Under ADR 0004 the bot token has no static value — Teleport
authenticates the joining tbot pod by validating its projected
ServiceAccount JWT against a JWKS pinned on the token. The JWKS is
the consumer kind cluster's `/openid/v1/jwks` document.

```bash
# Read the consumer cluster's JWKS (single JSON line).
JWKS_JSON=$(kubectl --context kind-consumer get --raw /openid/v1/jwks | jq -c .)

# Render the kubernetes-method token from the reference YAML, substituting
# the JWKS into the static_jwks block, then create it on Central.
python3 - <<PYEOF | kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- tctl create -f -
import json, os, pathlib
src = pathlib.Path('hack/smoke/teleport/tokens.yaml').read_text()
bot_doc = src.split('\n---\n', 1)[1]
bot_doc = bot_doc.replace('REPLACE_WITH_CONSUMER_JWKS', json.dumps(os.environ['JWKS_JSON']))
print(bot_doc)
PYEOF
```

There is no consumer-side Secret to deliver — the operator no longer
needs one. The rendered tbot pod runs under a per-CR ServiceAccount
(named after the RemoteApp), and the kubelet projects the SA's JWT
into the pod automatically. The token's `allow` rule
(`service_account: "smoke:smoke-app"`) pins exactly that SA.

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

## 8. Restart the tbot pod and verify rejoin

```bash
kubectl --context kind-consumer -n smoke delete pod \
  -l tunnelport.giantswarm.io/role=tbot,tunnelport.giantswarm.io/remoteapp=smoke-app

kubectl --context kind-consumer -n smoke rollout status deployment/smoke-app

kubectl --context kind-consumer -n smoke wait remoteapp/smoke-app \
  --for=jsonpath='{.status.ready}'=true --timeout=2m

kubectl --context kind-consumer -n smoke delete job smoke-curl
kubectl --context kind-consumer apply -f hack/smoke/consumer/curl-pod.yaml
kubectl --context kind-consumer -n smoke wait job/smoke-curl \
  --for=condition=complete --timeout=2m
kubectl --context kind-consumer -n smoke logs job/smoke-curl
```

The body should still be `hello-from-producer`. Under ADR 0004 the
ProvisionToken is an allowlist of ServiceAccount subjects, not a
single-use bot token: the replacement pod's projected SA JWT joins
without any operator-side or platform-team intervention. This step
explicitly proves the property that justifies keeping the cert cache
in `emptyDir` (ADR 0002 stays valid).

## Tearing down

```bash
kind delete cluster --name consumer
kind delete cluster --name producer
kind delete cluster --name teleport
rm /tmp/producer-agent-token /tmp/*.json /tmp/producer-kube-agent-values.yaml /tmp/smoke-bot-token.yaml
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
- *"failed to dial: dial tcp: lookup smoke.tunnelport.local"* — the
  `sed` in step 1 did not substitute, so the proxy is still
  advertising `smoke.tunnelport.local:443` as `publicAddr`. Check that
  `/tmp/teleport-values.yaml` contains the discovered IP+nodeport.
- *"no application smoke-app at smoke-app.replace_with_teleport_proxy_addr
  found"* in the producer's kube-agent — a proxy booted with the
  unsubstituted placeholder at some point and its heartbeat is still
  in the auth backend. Do not try to patch around it by rolling the
  proxy: a roll adds a heartbeat rather than replacing the stale one.
  Tear the teleport cluster down and redo step 1 as a single
  substituted install. This was #92.
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

**The curl Job fails with `Connection refused`.**
The tbot pod is up but the tunnel isn't established. The Job's
`--retry-connrefused` should ride this out; if it doesn't, the bot
token's kubernetes `allow` rule is mismatched against the rendered
ServiceAccount. Check that the token's
`spec.kubernetes.allow[0].service_account` is exactly
`<RemoteApp.namespace>:<RemoteApp.name>` (here `smoke:smoke-app`) —
that's the canonical name the operator stamps on every owned object,
including the per-CR ServiceAccount.

**`status.lastError` shows `invalid bearer token` / `kubernetes join
failed`.**
The consumer cluster's JWKS has rotated since the token was created,
or the JWKS substituted into the token in step 6c was for a different
cluster. Re-run step 6c to refresh the token's JWKS block.

## Production differences

This smoke runbook is deliberately not a deployment guide. The
production-targeting differences:

| Smoke (this runbook) | Production |
|---|---|
| Self-signed CA, `--insecure` everywhere | A real Teleport cluster with a CA bundle the agents trust |
| Hand-rolled `tctl create -f` for the kubernetes-method token | `TeleportBot` / `TeleportRole` / `TeleportToken` via the Giant Swarm Teleport Operator on Central |
| `tctl` from a host shell | Same Central-managed pipeline |
| One bot per smoke test | One bot **per RemoteApp**, with the token's `kubernetes.allow` pinned to that one CR's ServiceAccount (per-app blast-radius isolation, ADR 0004) |
| `teleport.proxyAddr` Helm value is a kind-network IP | A stable hostname resolvable from the consumer MC (e.g. `teleport.example.com:443`), ADR 0005 |
