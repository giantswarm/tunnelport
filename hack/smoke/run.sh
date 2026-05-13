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

# Per-step timeouts (seconds). Total target wall-clock <12min.
KIND_WAIT="${KIND_WAIT:-180}"
HELM_WAIT="${HELM_WAIT:-180}"
# Operator install (ADR 0008) now waits for the singleton trust-bundle
# tbot Deployment to be Ready as part of `helm --wait`, which means
# the bot must reach Teleport and issue its first SVID before helm
# returns. On CI's Ubuntu kind the first-connection retry path is
# slower than local; 300s leaves room. The other helm installs
# (Teleport, kube-agent) still use the shorter HELM_WAIT.
OPERATOR_HELM_WAIT="${OPERATOR_HELM_WAIT:-300}"
READY_WAIT="${READY_WAIT:-180}"
CURL_WAIT="${CURL_WAIT:-120}"

TMP=/tmp
PRODUCER_TOKEN_FILE="${TMP}/smoke-producer-agent-token"
TELEPORT_VALUES_FILE="${TMP}/smoke-teleport-values.yaml"
KUBE_AGENT_VALUES_FILE="${TMP}/smoke-kube-agent-values.yaml"
SMOKE_BOT_TOKEN_FILE="${TMP}/smoke-bot-token.yaml"
TRUST_BUNDLE_TOKEN_FILE="${TMP}/tunnelport-trust-bundle-token.yaml"

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
  rm -f "$PRODUCER_TOKEN_FILE" "$TELEPORT_VALUES_FILE" "$KUBE_AGENT_VALUES_FILE" "$SMOKE_BOT_TOKEN_FILE" "$TRUST_BUNDLE_TOKEN_FILE"
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
  printf '\n--- consumer/tunnelport-system trust-bundle secret (ADR 0008) ---\n' >&2
  kubectl --context kind-consumer -n tunnelport-system get secret tunnelport-spiffe-bundle \
    -o jsonpath='{"data keys: "}{.data}{"\n"}' 2>&1 >&2 || true
  printf '\n--- consumer/tunnelport-system trust-bundle tbot logs ---\n' >&2
  kubectl --context kind-consumer -n tunnelport-system logs -l tunnelport.giantswarm.io/role=trust-bundle-bot --tail=40 2>&1 | tail -50 >&2 || true
  printf '\n--- consumer/tunnelport-system tls-probe job ---\n' >&2
  kubectl --context kind-consumer -n tunnelport-system logs job/smoke-curl-tls --all-containers=true --tail=30 2>&1 | tail -40 >&2 || true
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
  # Placeholder teleport.* values: this template is parsed for the tbot
  # major only — the resolved value isn't installed anywhere. Schema
  # (ADR 0005) requires both fields to be non-empty for templating to
  # succeed, so we pass shape-valid throwaways. trustBundle is disabled
  # for the probe because ADR 0008 requires `trustBundle.tokenName` when
  # enabled — the probe only inspects the operator Deployment's
  # `--tbot-image` flag, which is rendered regardless of trustBundle.
  TBOT_MAJOR="$(helm template ./helm/tunnelport \
      --set teleport.clusterName=tbot-major-probe \
      --set teleport.proxyAddr=tbot-major-probe:443 \
      --set trustBundle.enabled=false |
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

step "Provisioning Teleport role, WorkloadIdentity, bot, and producer agent token via tctl"
AUTH_POD="$(kubectl --context kind-teleport -n teleport get pods \
  -l app.kubernetes.io/component=auth -o jsonpath='{.items[0].metadata.name}')"

# Roles first — bot creation references them; the per-RemoteApp role
# also gates SVID issuance for the per-RemoteApp WorkloadIdentity
# (ADR 0007). The trust-bundle role gates issuance against the
# singleton bot's WorkloadIdentity (ADR 0008); it's distinct from the
# per-RemoteApp role and uses a different selector key
# (`trust-bundle:` not `remoteapp:`).
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/role.yaml >/dev/null
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/trust-bundle-role.yaml >/dev/null

# WorkloadIdentity: per ADR 0007, one resource per RemoteApp. tbot's
# workload-identity-x509 service mints the smoke RemoteApp's tunnel
# SVID against this; the role's workload_identity_labels match scopes
# issuance to this resource alone.
# (The trust-bundle WorkloadIdentity for ADR 0008 lives in the same
# file as its role above.)
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/workload-identity.yaml >/dev/null

# Producer agent (Node+App) join token — still a static token; the
# producer side of the smoke is out of scope for the kubernetes-join
# migration (ADR 0004 is about the consumer-side tbot only).
kubectl --context kind-teleport -n teleport exec "$AUTH_POD" -- \
  tctl tokens add --type=node,app --format=json \
  > "${TMP}/smoke-producer-agent-token.json"
jq -r .token "${TMP}/smoke-producer-agent-token.json" > "${PRODUCER_TOKEN_FILE}"

# Bot identities (per-RemoteApp + singleton trust-bundle). Under
# ADR 0004 each bot's join token is created separately (kubernetes
# join + static_jwks pinned to the consumer cluster) — we cannot
# create them yet because we need the consumer kind cluster's JWKS
# first.
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/bot.yaml >/dev/null
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < hack/smoke/teleport/trust-bundle-bot.yaml >/dev/null

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

step "Exporting consumer cluster JWKS and creating the kubernetes-join bot tokens"
# ADR 0004 / ADR 0008: both kubernetes-join tokens carry the consumer
# kind cluster's `/openid/v1/jwks` document in `static_jwks.jwks`.
# Teleport auth validates each tbot pod's projected SA JWT against that
# JWKS at join time; the per-token `allow` rule narrows which
# ServiceAccount subjects each token admits.
#
# This step runs BEFORE `helm install` (below) so that the singleton
# trust-bundle tbot the chart ships can join Teleport on its first try
# — `helm --wait` waits for Ready, and a CrashLoopBackOff caused by a
# missing token on Central would push us past HELM_WAIT.
JWKS_JSON="$(kubectl --context kind-consumer get --raw /openid/v1/jwks | jq -c .)"
if [[ -z "${JWKS_JSON}" || "${JWKS_JSON}" == "null" ]]; then
  warn "consumer cluster's /openid/v1/jwks returned no document"
  exit 1
fi
export JWKS_JSON

# Render both kubernetes-join tokens from tokens.yaml with the JWKS
# substituted. tokens.yaml carries three documents in order:
#   [0] producer-agent-token       (NOT rendered — created on Central
#                                    via `tctl tokens add` above with a
#                                    random value; this doc is reference
#                                    shape only)
#   [1] smoke-bot-token            (per-RemoteApp tbot — ADR 0004)
#   [2] tunnelport-trust-bundle-token (singleton tbot — ADR 0008)
# Both [1] and [2] need the JWKS substituted; they pin DIFFERENT
# ServiceAccount subjects via their respective `allow` rules.
export REPO_ROOT SMOKE_BOT_TOKEN_FILE TRUST_BUNDLE_TOKEN_FILE
# Quoted heredoc — body is literal so backticks in comments (e.g. the
# static_jwks reference below) are not interpreted as shell command
# substitution.
python3 - <<'PYEOF'
import os, pathlib, textwrap
# Use REPO_ROOT so the substitution works regardless of cwd (defence in
# depth — the script already cd's to REPO_ROOT at top, but a relative
# pathlib.Path silently yields an empty doc if that ever regresses).
src = pathlib.Path(os.environ['REPO_ROOT'], 'hack/smoke/teleport/tokens.yaml').read_text()
docs = src.split('\n---\n')
if len(docs) < 3:
    raise SystemExit(f'tokens.yaml: expected 3 documents, got {len(docs)}')
# The JWKS field is a YAML block scalar (`jwks: |`). Indent the
# compact JSON to match — 8 spaces lines up under the `static_jwks`
# nesting in tokens.yaml.
jwks_block = textwrap.indent(os.environ['JWKS_JSON'], '        ')
for doc, out_path in [
    (docs[1], os.environ['SMOKE_BOT_TOKEN_FILE']),
    (docs[2], os.environ['TRUST_BUNDLE_TOKEN_FILE']),
]:
    rendered = doc.replace('        REPLACE_WITH_CONSUMER_JWKS', jwks_block)
    pathlib.Path(out_path).write_text(rendered)
PYEOF

# Create both tokens on Central before helm install so the singleton
# trust-bundle tbot can join on its first reconcile.
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < "${SMOKE_BOT_TOKEN_FILE}" >/dev/null
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create -f - < "${TRUST_BUNDLE_TOKEN_FILE}" >/dev/null

step "Installing the operator on the consumer cluster"
# teleport.clusterName matches the Teleport cluster name configured in
# hack/smoke/teleport/helm-values.yaml; teleport.proxyAddr is the
# kind-discovered NodePort proxy address (ADR 0005).
# trustBundle.tokenName names the ProvisionToken created above —
# without it the singleton tbot the chart ships fails to join (ADR 0008).
helm --kube-context kind-consumer upgrade --install tunnelport \
  ./helm/tunnelport \
  --create-namespace --namespace tunnelport-system \
  --set image.registry=docker.io \
  --set image.name=library/tunnelport \
  --set image.tag="${OPERATOR_IMAGE##*:}" \
  --set image.pullPolicy=IfNotPresent \
  --set imagePullSecret="" \
  --set tbot.insecure=true \
  --set teleport.clusterName=smoke.tunnelport.local \
  --set teleport.proxyAddr="${TELEPORT_PROXY_ADDR}" \
  --set trustBundle.tokenName=tunnelport-trust-bundle-token \
  --wait --timeout "${OPERATOR_HELM_WAIT}s" >/dev/null

step "Applying the RemoteApp CR"
# RemoteApp no longer carries proxyAddr / clusterName (ADR 0005) — the
# operator install above passed both as flags. The CR is applied as-is.
kubectl --context kind-consumer create namespace smoke >/dev/null 2>&1 || true
kubectl --context kind-consumer apply -f hack/smoke/consumer/remoteapp.yaml >/dev/null

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

# ---------------------------------------------------------------------
# TLS-side assertions (ADR 0007 / spiffe-tunnel-tls slices 02 + 03).
# The plaintext :8080 path above keeps slice 01 honest — the WI service
# block in tbot.yaml must not break the existing application-tunnel.
# Below we additionally assert the ghostunnel sidecar serves a valid
# SVID on :8443 (slice 02) and that a pod mounting the
# operator-rendered ${cr.Name}-spiffe-bundle Secret can curl the
# tunnel over HTTPS with full-chain verification (slice 03).
# ---------------------------------------------------------------------

step "Running TLS assertion (cacert-verified HTTPS curl on :8443)"
kubectl --context kind-consumer apply -f hack/smoke/consumer/tls-probe.yaml >/dev/null

# The initContainer waits for tbot to populate svid_bundle.pem in the
# mounted Secret; the curl container retries the request until the
# tunnel is up. CURL_WAIT is generous to cover both budgets. A
# successful --cacert HTTPS curl transitively verifies: tbot wrote
# the SVID files (slice 01), ghostunnel terminates TLS using them
# (slice 02), the trust-bundle Secret carries the SPIFFE CA chain
# (slice 03), and the SVID's SAN matches the Service hostname (curl
# does standard hostname verification).
kubectl --context kind-consumer -n tunnelport-system wait job/smoke-curl-tls \
  --for=condition=complete --timeout="${CURL_WAIT}s"
TLS_BODY="$(kubectl --context kind-consumer -n tunnelport-system logs job/smoke-curl-tls -c curl 2>&1 | tail -1)"
echo "Got TLS body: ${TLS_BODY}"
if [[ "${TLS_BODY}" != "${EXPECTED_BODY}" ]]; then
  warn "TLS curl body mismatch: expected '${EXPECTED_BODY}', got '${TLS_BODY}'"
  exit 1
fi

# ---------------------------------------------------------------------
# Re-join after pod restart. Under ADR 0004 (kubernetes join method) the
# ProvisionToken is NOT a single-use bot token — it's an allowlist of
# ServiceAccount subjects. Killing the tbot pod drops the emptyDir cert
# cache; the kubelet's projected SA token volume provides a fresh JWT
# at the new pod's start; tbot rejoins without operator intervention.
# This step explicitly verifies that property, since it's the exact
# scenario PR #10's (now-reverted) ADR 0004 was trying to fix with PVCs.
# ---------------------------------------------------------------------

step "Restarting tbot pods and re-verifying the tunnel"
PRE_RESTART_POD="$(kubectl --context kind-consumer -n smoke get pods \
  -l tunnelport.giantswarm.io/role=tbot \
  -o jsonpath='{.items[0].metadata.name}')"
echo "Pre-restart pod: ${PRE_RESTART_POD}"

# Delete every tbot pod for this RemoteApp; the Deployment recreates them
# with a fresh emptyDir cert cache. --wait=true blocks until the API
# server acknowledges deletion.
kubectl --context kind-consumer -n smoke delete pod \
  -l tunnelport.giantswarm.io/role=tbot,tunnelport.giantswarm.io/remoteapp=smoke-app \
  --wait=true >/dev/null

# Wait for the replacement pod to come up. RolloutStatus is the cheap
# signal that the Deployment converged on its desired replicas Ready.
kubectl --context kind-consumer -n smoke rollout status deployment/smoke-app \
  --timeout="${READY_WAIT}s" >/dev/null

# RemoteApp.status.ready can flap briefly during the swap. wait again
# so the post-restart curl runs against a known-Ready RemoteApp.
kubectl --context kind-consumer -n smoke wait remoteapp/smoke-app \
  --for=jsonpath='{.status.ready}'=true --timeout="${READY_WAIT}s"

POST_RESTART_POD="$(kubectl --context kind-consumer -n smoke get pods \
  -l tunnelport.giantswarm.io/role=tbot \
  -o jsonpath='{.items[0].metadata.name}')"
echo "Post-restart pod: ${POST_RESTART_POD}"
if [[ "${POST_RESTART_POD}" == "${PRE_RESTART_POD}" ]]; then
  warn "Pod name unchanged after delete — restart did not happen"
  exit 1
fi

# Re-run the curl. Completed Jobs are immutable; delete first.
kubectl --context kind-consumer -n smoke delete job smoke-curl >/dev/null
kubectl --context kind-consumer apply -f hack/smoke/consumer/curl-pod.yaml >/dev/null
kubectl --context kind-consumer -n smoke wait job/smoke-curl \
  --for=condition=complete --timeout="${CURL_WAIT}s"

POST_RESTART_BODY="$(kubectl --context kind-consumer -n smoke logs job/smoke-curl 2>&1 | tail -1)"
echo "Got body after restart: ${POST_RESTART_BODY}"
if [[ "${POST_RESTART_BODY}" != "${EXPECTED_BODY}" ]]; then
  warn "Post-restart curl body mismatch: expected '${EXPECTED_BODY}', got '${POST_RESTART_BODY}'"
  exit 1
fi

step "✅ SMOKE PASSED"
SMOKE_RESULT=ok
