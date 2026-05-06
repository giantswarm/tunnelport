#!/usr/bin/env bash
# Headless end-to-end smoke for the tunnelport operator. Wraps the
# steps documented in hack/smoke/README.md as a single script suitable
# for CI. Self-contained: spins up three local kind clusters
# (teleport / producer / consumer), provisions Teleport state via
# `tctl`, deploys the operator chart from this checkout, applies a
# sample RemoteApp, then asserts that a curl pod in the consumer
# cluster reads back the literal body served by the producer's
# http-echo.
#
# Exit codes:
#   0 — green: curl returned the expected body.
#   non-zero — anything else. The trap calls teardown so kind
#   clusters do not leak between CI runs.
#
# Usage:
#   hack/smoke/run.sh                  # full smoke
#   SMOKE_KEEP_CLUSTERS=1 hack/smoke/run.sh   # leave clusters up on success for debugging
#
# Requires on the host: docker, kind, kubectl, helm, jq.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

# ---------------------------------------------------------------------
# Knobs. Pinned versions match the runbook (hack/smoke/README.md).
# ---------------------------------------------------------------------

TELEPORT_CHART_VERSION="${TELEPORT_CHART_VERSION:-18.4.0}"
TBOT_IMAGE="${TBOT_IMAGE:-public.ecr.aws/gravitational/tbot-distroless:18.4.0}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-tunnelport:smoke}"
EXPECTED_BODY="${EXPECTED_BODY:-hello-from-producer}"

# Per-step timeouts (seconds). Total target wall-clock <10min.
KIND_WAIT="${KIND_WAIT:-180}"
HELM_WAIT="${HELM_WAIT:-180}"
READY_WAIT="${READY_WAIT:-180}"
CURL_WAIT="${CURL_WAIT:-120}"

TMP=/tmp
PRODUCER_TOKEN_FILE="${TMP}/smoke-producer-agent-token"
BOT_TOKEN_FILE="${TMP}/smoke-bot-token"
TELEPORT_VALUES_FILE="${TMP}/smoke-teleport-values.yaml"
KUBE_AGENT_VALUES_FILE="${TMP}/smoke-kube-agent-values.yaml"

# ---------------------------------------------------------------------
# Logging + lifecycle.
# ---------------------------------------------------------------------

step() { printf '\n=== %s ===\n' "$*"; }
warn() { printf '!!! %s\n' "$*" >&2; }

teardown() {
  if [[ "${SMOKE_KEEP_CLUSTERS:-0}" == "1" && "${SMOKE_RESULT:-fail}" == "ok" ]]; then
    step "Keeping clusters up (SMOKE_KEEP_CLUSTERS=1, smoke passed)"
    return
  fi
  step "Tearing down kind clusters and tmp files"
  for c in consumer producer teleport; do
    kind delete cluster --name "$c" >/dev/null 2>&1 || true
  done
  rm -f "$PRODUCER_TOKEN_FILE" "$BOT_TOKEN_FILE" "$TELEPORT_VALUES_FILE" "$KUBE_AGENT_VALUES_FILE"
  rm -f "${TMP}/smoke-bot-token.json" "${TMP}/smoke-producer-agent-token.json"
}

dump_diag() {
  warn "Smoke failed — dumping diagnostics"
  for ctx in kind-teleport kind-producer kind-consumer; do
    printf '\n--- %s pods ---\n' "$ctx" >&2
    kubectl --context "$ctx" get pods -A 2>&1 | head -30 >&2 || true
  done
  printf '\n--- consumer/smoke remoteapp ---\n' >&2
  kubectl --context kind-consumer -n smoke get remoteapp smoke-app -o yaml 2>&1 | tail -30 >&2 || true
  printf '\n--- consumer/smoke tbot pod logs ---\n' >&2
  kubectl --context kind-consumer -n smoke logs -l tunnelport.giantswarm.io/role=tbot --tail=40 2>&1 | tail -50 >&2 || true
  printf '\n--- producer/smoke kube-agent logs ---\n' >&2
  kubectl --context kind-producer -n smoke logs -l app=teleport-kube-agent --tail=40 2>&1 | tail -50 >&2 || true
}

SMOKE_RESULT=fail
trap 'rc=$?; if [[ "$SMOKE_RESULT" != "ok" ]]; then dump_diag; fi; teardown; exit $rc' EXIT

# ---------------------------------------------------------------------
# Steps.
# ---------------------------------------------------------------------

step "Building operator image (${OPERATOR_IMAGE})"
make docker-build IMG="${OPERATOR_IMAGE}" >/dev/null

# Three kind clusters in parallel exhaust the default inotify limits on
# Ubuntu hosts (max_user_watches=8192, max_user_instances=128). kube-proxy
# and local-path-provisioner then crashloop on the 2nd/3rd cluster. The
# kind project documents these as the recommended values:
# https://kind.sigs.k8s.io/docs/user/known-issues/
if command -v sudo >/dev/null 2>&1; then
  step "Raising inotify limits (kind multi-cluster requirement)"
  sudo sysctl -w fs.inotify.max_user_watches=524288 >/dev/null
  sudo sysctl -w fs.inotify.max_user_instances=512 >/dev/null
fi

step "Creating kind clusters in parallel"
kind create cluster --config hack/smoke/teleport/kind.yaml --wait "${KIND_WAIT}s" >/dev/null &
PID_TELEPORT=$!
kind create cluster --config hack/smoke/producer/kind.yaml --wait "${KIND_WAIT}s" >/dev/null &
PID_PRODUCER=$!
kind create cluster --config hack/smoke/consumer/kind.yaml --wait "${KIND_WAIT}s" >/dev/null &
PID_CONSUMER=$!
wait "$PID_TELEPORT" "$PID_PRODUCER" "$PID_CONSUMER"
echo "All three kind clusters ready."

step "Loading operator image into consumer cluster"
kind load docker-image "${OPERATOR_IMAGE}" --name consumer >/dev/null

step "Installing Teleport (first pass — placeholder publicAddr)"
helm repo add teleport https://charts.releases.teleport.dev >/dev/null 2>&1 || true
helm repo update >/dev/null
helm --kube-context kind-teleport upgrade --install teleport-cluster \
  teleport/teleport-cluster --version "${TELEPORT_CHART_VERSION}" \
  --create-namespace --namespace teleport \
  --values hack/smoke/teleport/helm-values.yaml \
  --wait --timeout "${HELM_WAIT}s" >/dev/null

step "Discovering proxy address and re-applying with publicAddr"
TELEPORT_PROXY_IP="$(docker inspect teleport-control-plane --format '{{ .NetworkSettings.Networks.kind.IPAddress }}')"
NODEPORT="$(kubectl --context kind-teleport -n teleport get svc teleport-cluster -o jsonpath='{.spec.ports[0].nodePort}')"
TELEPORT_PROXY_ADDR="${TELEPORT_PROXY_IP}:${NODEPORT}"
echo "TELEPORT_PROXY_ADDR=${TELEPORT_PROXY_ADDR}"

sed "s|REPLACE_WITH_TELEPORT_PROXY_ADDR|${TELEPORT_PROXY_ADDR}|" \
  hack/smoke/teleport/helm-values.yaml > "${TELEPORT_VALUES_FILE}"
helm --kube-context kind-teleport upgrade teleport-cluster \
  teleport/teleport-cluster --version "${TELEPORT_CHART_VERSION}" \
  --namespace teleport --values "${TELEPORT_VALUES_FILE}" \
  --wait --timeout "${HELM_WAIT}s" >/dev/null

step "Provisioning Teleport role, bot, and tokens via tctl"
AUTH_POD="$(kubectl --context kind-teleport -n teleport get pods \
  -l app.kubernetes.io/component=auth -o jsonpath='{.items[0].metadata.name}')"

# Role first — bot creation references it.
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/role.yaml >/dev/null

# Producer agent (Node+App) join token.
kubectl --context kind-teleport -n teleport exec "$AUTH_POD" -- \
  tctl tokens add --type=node,app --format=json \
  > "${TMP}/smoke-producer-agent-token.json"
jq -r .token "${TMP}/smoke-producer-agent-token.json" > "${PRODUCER_TOKEN_FILE}"

# Bot identity + bot token in one call. `tctl bots add` is the v18 idiom;
# the token comes back as `token_id` in the JSON.
kubectl --context kind-teleport -n teleport exec "$AUTH_POD" -- \
  tctl bots add smoke-bot --roles=smoke-app-tunnel --format=json \
  > "${TMP}/smoke-bot-token.json"
jq -r .token_id "${TMP}/smoke-bot-token.json" > "${BOT_TOKEN_FILE}"

step "Bringing up the producer (http-echo + teleport-kube-agent)"
kubectl --context kind-producer apply -f hack/smoke/producer/http-echo.yaml >/dev/null
sed \
  -e "s|REPLACE_WITH_PRODUCER_AGENT_TOKEN|$(cat "${PRODUCER_TOKEN_FILE}")|" \
  -e "s|REPLACE_WITH_TELEPORT_PROXY_ADDR|${TELEPORT_PROXY_ADDR}|" \
  hack/smoke/producer/teleport-kube-agent-values.yaml > "${KUBE_AGENT_VALUES_FILE}"
helm --kube-context kind-producer upgrade --install teleport-kube-agent \
  teleport/teleport-kube-agent --version "${TELEPORT_CHART_VERSION}" \
  --create-namespace --namespace smoke \
  --values "${KUBE_AGENT_VALUES_FILE}" \
  --wait --timeout "${HELM_WAIT}s" >/dev/null

step "Verifying smoke-app registered with Teleport"
# Heartbeat-based registration; appears in app_servers, not the static `apps`.
APP_SERVERS_OK=0
for _ in $(seq 1 30); do
  if kubectl --context kind-teleport -n teleport exec "$AUTH_POD" -- \
      tctl get app_servers 2>/dev/null | grep -q 'name: smoke-app'; then
    APP_SERVERS_OK=1
    break
  fi
  sleep 2
done
if [[ "${APP_SERVERS_OK}" -ne 1 ]]; then
  warn "smoke-app did not register as an app_server within 60s"
  exit 1
fi
echo "smoke-app present in app_servers."

step "Installing the operator on the consumer cluster"
helm --kube-context kind-consumer upgrade --install tunnelport \
  ./helm/tunnelport \
  --create-namespace --namespace tunnelport-system \
  --set image.registry=docker.io \
  --set image.name=library/tunnelport \
  --set image.tag="${OPERATOR_IMAGE##*:}" \
  --set image.pullPolicy=IfNotPresent \
  --set imagePullSecret="" \
  --set tbot.image="${TBOT_IMAGE}" \
  --set tbot.insecure=true \
  --wait --timeout "${HELM_WAIT}s" >/dev/null

step "Delivering the bot token Secret to the consumer cluster"
kubectl --context kind-consumer create namespace smoke >/dev/null 2>&1 || true
kubectl --context kind-consumer -n smoke create secret generic smoke-bot-token \
  --from-literal=token="$(cat "${BOT_TOKEN_FILE}")" \
  --dry-run=client -o yaml | kubectl --context kind-consumer apply -f - >/dev/null

step "Applying the RemoteApp CR"
sed "s|REPLACE_WITH_TELEPORT_PROXY_ADDR|${TELEPORT_PROXY_ADDR}|" \
  hack/smoke/consumer/remoteapp.yaml \
  | kubectl --context kind-consumer apply -f - >/dev/null

step "Waiting for status.ready=true on the RemoteApp"
kubectl --context kind-consumer -n smoke wait remoteapp/smoke-app \
  --for=jsonpath='{.status.ready}'=true --timeout="${READY_WAIT}s"

step "Running curl assertion"
kubectl --context kind-consumer apply -f hack/smoke/consumer/curl-pod.yaml >/dev/null
kubectl --context kind-consumer -n smoke wait job/smoke-curl \
  --for=condition=complete --timeout="${CURL_WAIT}s"

ACTUAL_BODY="$(kubectl --context kind-consumer -n smoke logs job/smoke-curl 2>&1 | tail -1)"
echo "Got body: ${ACTUAL_BODY}"
if [[ "${ACTUAL_BODY}" != "${EXPECTED_BODY}" ]]; then
  warn "Curl body mismatch: expected '${EXPECTED_BODY}', got '${ACTUAL_BODY}'"
  exit 1
fi

step "✅ SMOKE PASSED"
SMOKE_RESULT=ok
