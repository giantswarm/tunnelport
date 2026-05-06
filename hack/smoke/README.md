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
  chart, pinned to **18.7.3**. Bot/role/token are provisioned by
  `tctl` after the chart is up.
- **producer**: hosts the HTTP responder (`hashicorp/http-echo`) and a
  `teleport-kube-agent` (chart **18.7.3**) registering it as the
  Teleport application named `smoke-app`.
- **consumer**: runs the operator and the test workload. The operator
  renders a tbot Deployment from a `RemoteApp` CR. The `curl` Job hits
  the rendered Service.

## Prerequisites

- macOS or Linux with Docker Desktop / Docker Engine
- `kind` ≥ 0.24
- `kubectl`
- `helm` ≥ 3.14
- `teleport`, `tctl`, `tbot` ≥ **18.7.3**
  (download: <https://goteleport.com/download/> — pick the matching
  client tarball for your OS)
- `jq` (for parsing `tctl` output)

## 1. Bring up the Teleport control plane

```bash
kind create cluster --config hack/smoke/teleport/kind.yaml

# The teleport-cluster Helm chart lives in the Teleport upstream repo.
helm repo add teleport https://charts.releases.teleport.dev
helm repo update

helm --kube-context kind-teleport upgrade --install teleport-cluster \
  teleport/teleport-cluster --version 18.7.3 \
  --create-namespace --namespace teleport \
  --values hack/smoke/teleport/helm-values.yaml

kubectl --context kind-teleport -n teleport rollout status \
  statefulset/teleport-cluster --timeout=5m
```

What's happening: the chart deploys a single-replica Teleport auth +
proxy with `proxyListenerMode=multiplex`, so every Teleport protocol
shares port 443 on the cluster's pod IP. The chart generates its own
self-signed CA on first install — that CA is private to this kind
cluster, which is why the producer and consumer connect with
`--insecure` later.

## 2. Network setup: discover the proxy address

All three kind clusters share the `kind` Docker network. The producer
and consumer reach the Teleport proxy by the teleport cluster's
control-plane node IP within that network.

```bash
TELEPORT_PROXY_IP=$(docker inspect kind-teleport-control-plane \
  --format '{{ .NetworkSettings.Networks.kind.IPAddress }}')

# Used by both the producer's kube-agent values and the consumer's
# RemoteApp CR. Format: "<ip>:443".
TELEPORT_PROXY_ADDR="${TELEPORT_PROXY_IP}:443"
echo "TELEPORT_PROXY_ADDR=${TELEPORT_PROXY_ADDR}"
```

Keep that env var in scope; later steps reference it in `sed` calls.
If you start a new shell, re-run the `docker inspect` command — the
IP is stable for the lifetime of the kind cluster but does not survive
a recreate.

## 3. Provision the producer agent token

```bash
# Open a shell on the auth pod and create a Node+App join token. The
# token value is captured back to the host filesystem.
kubectl --context kind-teleport -n teleport exec -i \
  statefulset/teleport-cluster -- \
  tctl tokens add --type=node,app --format=json \
  | tee /tmp/producer-agent-token.json \
  | jq -r .token > /tmp/producer-agent-token

cat /tmp/producer-agent-token   # sanity: a hex string
```

Why this isn't `tctl create -f hack/smoke/teleport/tokens.yaml`: the
YAML in `tokens.yaml` is a **reference** for the schema. The actual
token strings are generated server-side. If you'd rather use the
declarative path, replace `REPLACE_WITH_PRODUCER_AGENT_TOKEN` in
`tokens.yaml` with a value of your own and `tctl create -f` it
instead.

## 4. Provision the bot, role, and bot token

```bash
# 4a. Role (allows the bot to tunnel to apps labelled app-name=smoke-app):
kubectl --context kind-teleport -n teleport exec -i \
  statefulset/teleport-cluster -- tctl create -f - \
  < hack/smoke/teleport/role.yaml

# 4b. Bot identity:
kubectl --context kind-teleport -n teleport exec -i \
  statefulset/teleport-cluster -- tctl create -f - \
  < hack/smoke/teleport/bot.yaml

# 4c. Static join token bound to the bot. Generated server-side like
#     the agent token above:
kubectl --context kind-teleport -n teleport exec -i \
  statefulset/teleport-cluster -- \
  tctl bots tokens add smoke-bot --format=json \
  | tee /tmp/smoke-bot-token.json \
  | jq -r .token > /tmp/smoke-bot-token

cat /tmp/smoke-bot-token   # sanity: a hex string
```

`tctl bots tokens add` produces a token whose `roles: [Bot]` and
`bot_name: smoke-bot` are wired correctly without you authoring the
YAML.

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
  teleport/teleport-kube-agent --version 18.7.3 \
  --create-namespace --namespace smoke \
  --values /tmp/producer-kube-agent-values.yaml

kubectl --context kind-producer -n smoke rollout status \
  statefulset/teleport-kube-agent --timeout=5m
```

Verify the registration on the Teleport side:

```bash
kubectl --context kind-teleport -n teleport exec -i \
  statefulset/teleport-cluster -- tctl get apps
# Should show: smoke-app   http://http-echo.smoke.svc.cluster.local:5678
```

## 6. Bring up the consumer cluster

```bash
kind create cluster --config hack/smoke/consumer/kind.yaml
```

### 6a. Install the operator

```bash
# Use the chart from this repo:
helm --kube-context kind-consumer upgrade --install tunnelport \
  ./helm/tunnelport \
  --create-namespace --namespace tunnelport-system \
  --set image.repository=gsoci.azurecr.io/giantswarm/tunnelport \
  --set tbot.image=public.ecr.aws/gravitational/tbot-distroless:18.7.3

kubectl --context kind-consumer -n tunnelport-system rollout status \
  deployment/tunnelport --timeout=2m
```

The `tbot.image` value flows through to the rendered tbot Deployment
in step 6c.

### 6b. Deliver the bot token Secret

For the smoke environment we use plain `kubectl create secret`. In
production this is replaced by sealed-secrets / external-secrets-
operator / your team's chosen pattern.

```bash
kubectl --context kind-consumer create namespace smoke || true
kubectl --context kind-consumer -n smoke create secret generic smoke-bot-token \
  --from-literal=token=$(cat /tmp/smoke-bot-token)
```

Alternative: edit `hack/smoke/consumer/token-secret.yaml.template`,
replacing `REPLACE_WITH_SMOKE_BOT_TOKEN` with the literal token, and
`kubectl apply -f` it.

### 6c. Apply the RemoteApp CR

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
rm /tmp/producer-agent-token /tmp/smoke-bot-token /tmp/*.json /tmp/producer-kube-agent-values.yaml
```

## Troubleshooting

**`status.ready` stays false on the RemoteApp.**
Check the tbot pod's logs for the join failure:

```bash
kubectl --context kind-consumer -n smoke logs -l tunnelport.giantswarm.io/role=tbot
```

The most common smoke-test failures:

- *"trust dial: tls: failed to verify certificate"* — the consumer's
  tbot is hitting a different Teleport proxy than the producer. Check
  that `TELEPORT_PROXY_ADDR` in step 6c matches step 5. If you
  recreated the teleport kind cluster, the IP changed; redo step 2.
- *"role smoke-app-tunnel not found"* — step 4a was skipped or the
  role file failed to apply. `tctl get roles | grep smoke` on the
  Teleport pod.
- *"app smoke-app not found"* — step 5's kube-agent didn't register.
  `tctl get apps` on the Teleport pod; if empty, check the kube-agent
  pod logs in the producer cluster.

**`status.conditions[TokenSecretBound]: false`.**
The operator can't find the Secret or the named key. Confirm:

```bash
kubectl --context kind-consumer -n smoke get secret smoke-bot-token \
  -o jsonpath='{.data.token}' | base64 -d | head -c 8
```

You should see the first 8 hex chars of the token. If the field is
empty, redo step 6b.

**The curl Job fails with `Connection refused`.**
The tbot pod is up but the tunnel isn't established. The Job's
`--retry-connrefused` should ride this out; if it doesn't, the bot
token in the Secret doesn't match the one Teleport issued. The
`smoke-bot-token` files written in step 4c and the Secret in step 6b
must be the same string.

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
