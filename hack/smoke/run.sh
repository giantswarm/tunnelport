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

# TELEPORT_CHART_VERSION: the upstream `teleport-cluster` /
# `teleport-kube-agent` chart version installed by the smoke. Empty
# by default — resolved from the chart's `tbot.image` major after the
# upstream helm repo is added (see `resolve_teleport_chart_version`
# below) so the smoke and the chart can never drift apart on a major.
# Override with TELEPORT_CHART_VERSION=18.4.0 etc. for local debugging.
TELEPORT_CHART_VERSION="${TELEPORT_CHART_VERSION:-}"
# tbot.image is intentionally NOT overridden at install time — the
# smoke renders with the chart's default so a default-shaped consumer
# install is exercised end-to-end. This is what catches major-version
# skew between the chart-default tbot and the Teleport server tested
# above. Override with `--set tbot.image=...` at the helm install
# call if you need to test a non-default image locally.
OPERATOR_IMAGE="${OPERATOR_IMAGE:-tunnelport:smoke}"
EXPECTED_BODY="${EXPECTED_BODY:-hello-from-producer}"

# Per-step timeouts (seconds). Total target wall-clock <10min.
KIND_WAIT="${KIND_WAIT:-180}"
HELM_WAIT="${HELM_WAIT:-180}"
READY_WAIT="${READY_WAIT:-180}"
CURL_WAIT="${CURL_WAIT:-120}"

TMP=/tmp
PRODUCER_TOKEN_FILE="${TMP}/smoke-producer-agent-token"
TELEPORT_VALUES_FILE="${TMP}/smoke-teleport-values.yaml"
KUBE_AGENT_VALUES_FILE="${TMP}/smoke-kube-agent-values.yaml"
SMOKE_BOT_TOKEN_FILE="${TMP}/smoke-bot-token.yaml"

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
  rm -f "$PRODUCER_TOKEN_FILE" "$TELEPORT_VALUES_FILE" "$KUBE_AGENT_VALUES_FILE" "$SMOKE_BOT_TOKEN_FILE"
  rm -f "${TMP}/smoke-producer-agent-token.json"
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
#
# macOS doesn't expose `fs.inotify.*` (it's a Linux-only feature), so we
# only attempt the bump where the sysctl exists. We also use `sudo -n`
# to avoid blocking on a password prompt in non-interactive runs; if
# passwordless sudo is unavailable we just warn and continue. On CI hosts
# the sysctl is typically already raised system-wide via cloud-init.
if [[ "$(uname -s)" == "Linux" ]] && command -v sudo >/dev/null 2>&1; then
  if sysctl -n fs.inotify.max_user_watches >/dev/null 2>&1; then
    step "Raising inotify limits (kind multi-cluster requirement)"
    if ! sudo -n sysctl -w fs.inotify.max_user_watches=524288 >/dev/null 2>&1; then
      warn "could not raise fs.inotify.max_user_watches (sudo unavailable); continuing"
    fi
    if ! sudo -n sysctl -w fs.inotify.max_user_instances=512 >/dev/null 2>&1; then
      warn "could not raise fs.inotify.max_user_instances (sudo unavailable); continuing"
    fi
  fi
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

# Resolve TELEPORT_CHART_VERSION from the tunnelport chart's tbot.image
# major. This makes the chart's `tbot.image` the single source of truth
# for "which Teleport major are we on" — the smoke can never drift to
# a different major from the chart default it tests. We pick the latest
# patch within that major from the upstream `teleport-cluster` chart.
if [ -z "${TELEPORT_CHART_VERSION}" ]; then
  TBOT_MAJOR="$(helm template ./helm/tunnelport |
    grep -oE -- '--tbot-image=[^[:space:]]+' | head -1 |
    sed -E 's|.*tbot-distroless:([0-9]+).*|\1|')"
  if ! printf '%s' "${TBOT_MAJOR}" | grep -qE '^[0-9]+$'; then
    warn "Could not derive tbot major from chart (got: '${TBOT_MAJOR}'). Set TELEPORT_CHART_VERSION explicitly."
    exit 1
  fi
  TELEPORT_CHART_VERSION="$(helm search repo teleport/teleport-cluster --versions -o json |
    jq -r --arg M "${TBOT_MAJOR}" '[.[] | select(.version | startswith($M+"."))] | first | .version')"
  if [ -z "${TELEPORT_CHART_VERSION}" ] || [ "${TELEPORT_CHART_VERSION}" = "null" ]; then
    warn "No teleport-cluster chart version matching major ${TBOT_MAJOR} found upstream."
    exit 1
  fi
  echo "Resolved TELEPORT_CHART_VERSION=${TELEPORT_CHART_VERSION} from chart's tbot.image major ${TBOT_MAJOR}."
fi

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

step "Provisioning Teleport role, bot, and producer agent token via tctl"
AUTH_POD="$(kubectl --context kind-teleport -n teleport get pods \
  -l app.kubernetes.io/component=auth -o jsonpath='{.items[0].metadata.name}')"

# Role first — bot creation references it.
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/role.yaml >/dev/null

# Producer agent (Node+App) join token — still a static token; the
# producer side of the smoke is out of scope for the kubernetes-join
# migration (ADR 0004 is about the consumer-side tbot only).
kubectl --context kind-teleport -n teleport exec "$AUTH_POD" -- \
  tctl tokens add --type=node,app --format=json \
  > "${TMP}/smoke-producer-agent-token.json"
jq -r .token "${TMP}/smoke-producer-agent-token.json" > "${PRODUCER_TOKEN_FILE}"

# Bot identity. Under ADR 0004 the bot's join token is created
# separately (kubernetes join method + static_jwks pinned to the
# consumer cluster) — we cannot create it yet because we need the
# consumer kind cluster's JWKS first.
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/bot.yaml >/dev/null

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
  --set tbot.insecure=true \
  --wait --timeout "${HELM_WAIT}s" >/dev/null

step "Exporting consumer cluster JWKS and creating the kubernetes-join bot token"
# ADR 0004: the bot token's `static_jwks` block is the consumer kind
# cluster's `/openid/v1/jwks` document. Teleport auth validates the
# tbot pod's projected SA JWT against that JWKS at join time.
JWKS_JSON="$(kubectl --context kind-consumer get --raw /openid/v1/jwks | jq -c .)"
if [[ -z "${JWKS_JSON}" || "${JWKS_JSON}" == "null" ]]; then
  warn "consumer cluster's /openid/v1/jwks returned no document"
  exit 1
fi
export JWKS_JSON

# Render the kubernetes-join token from tokens.yaml with the JWKS
# substituted, then create it on Central. We take only the smoke-bot-
# token document — the producer-agent-token in the same file is the
# reference shape for that side of the smoke; the actual producer
# token was generated server-side via `tctl tokens add` above (with a
# random name) and is unrelated to this apply. The `allow` rule pins
# the per-CR ServiceAccount the operator renders for this RemoteApp
# ("smoke:smoke-app").
python3 - <<'PYEOF' > "${SMOKE_BOT_TOKEN_FILE}"
import json, os, pathlib
src = pathlib.Path('hack/smoke/teleport/tokens.yaml').read_text()
docs = src.split('\n---\n', 1)
bot_doc = docs[1] if len(docs) == 2 else docs[0]
# JSON-encode the JWKS string so embedded quotes survive the YAML
# round-trip safely.
bot_doc = bot_doc.replace('REPLACE_WITH_CONSUMER_JWKS', json.dumps(os.environ['JWKS_JSON']))
print(bot_doc)
PYEOF

kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < "${SMOKE_BOT_TOKEN_FILE}" >/dev/null

step "Applying the RemoteApp CR"
kubectl --context kind-consumer create namespace smoke >/dev/null 2>&1 || true
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
