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
# returns. tbot's exponential backoff is ~60s; 600s leaves room for
# ~10 attempts in the worst case.
#
# History (#41): this step used to flake with persistent
# `dial tcp <node-ip>:<nodeport>: i/o timeout` from the trust-bundle
# tbot. Root cause was NOT the network path: the chart's default-deny
# NetworkPolicy on the manager pod also matched the trust-bundle pod
# (shared app.kubernetes.io/{name,instance} selector labels), and
# kind ≥ v0.24 enforces NetworkPolicy (kube-network-policies embedded
# in kindnetd, nfqueue, fail-open). The policy's egress allows only
# DNS/443/6443, so the bot's dial to the proxy NodePort
# (30000-32767) was dropped whenever enforcement was active; runs
# passed only when the bot's first join raced ahead of kindnet's
# pod-IP sync (fail-open window). Fixed by excluding role-labeled
# pods from the policy's podSelector.
OPERATOR_HELM_WAIT="${OPERATOR_HELM_WAIT:-600}"
READY_WAIT="${READY_WAIT:-180}"
CURL_WAIT="${CURL_WAIT:-120}"

# TLS-verification knobs. The chart default is a 2-minute probe cadence,
# which is right in production and far too slow for a 10-minute smoke; the
# assertions below need several rounds. 15s keeps the whole
# break-the-SAN-and-recover sequence inside a couple of minutes.
VERIFY_INTERVAL="${VERIFY_INTERVAL:-15s}"
# Budget for a verification outcome to appear on the CR and in /metrics.
# Generous because the manager reads the trust bundle from a mounted
# Secret and the kubelet propagates tbot's write into that mount on its own
# sync loop (up to ~1 min), not instantly.
VERIFY_WAIT="${VERIFY_WAIT:-180}"
# The manager's plain-HTTP metrics port (chart value ports.metrics).
METRICS_PORT="${METRICS_PORT:-8080}"

# prometheus-operator release supplying the PrometheusRule CRD. The chart
# renders a PrometheusRule whenever monitoring.prometheusRule.enabled is
# true and fails the install if the CRD is absent, so the bare kind
# consumer must have it before the operator install below.
PROMETHEUS_OPERATOR_VERSION="${PROMETHEUS_OPERATOR_VERSION:-v0.83.0}"

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
  printf '\n--- consumer/smoke remoteapp conditions ---\n' >&2
  kubectl --context kind-consumer -n smoke get remoteapp smoke-app \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}): {.message}{"\n"}{end}' 2>&1 >&2 || true
  printf '\n--- consumer tunnelport_* metrics (TLS verification) ---\n' >&2
  # The verification series are the fastest way to tell a broken tunnel
  # from a broken check: no tunnelport_tls_verification_available line at
  # all means no replica has run a round (no leader, or the feature is
  # off), 0 means the trust bundle is unreadable, and 1 with no
  # per-RemoteApp series means nothing was Ready to probe.
  scrape_manager_metrics 2>/dev/null | grep -E '^tunnelport_' >&2 || warn "  (no tunnelport_* series)"
  printf '\n--- consumer/tunnelport-system manager logs ---\n' >&2
  kubectl --context kind-consumer -n tunnelport-system logs \
    -l 'app.kubernetes.io/name=tunnelport,!tunnelport.giantswarm.io/role' \
    --tail=40 2>&1 | tail -60 >&2 || true
}

SMOKE_RESULT=fail
# shellcheck disable=SC2154 # rc is assigned in the trap body via rc=$?.
trap 'rc=$?; if [[ "$SMOKE_RESULT" != "ok" ]]; then dump_diag; fi; teardown; exit $rc' EXIT

# ---------------------------------------------------------------------
# TLS-verification helpers (giantswarm/giantswarm#37521 gap 2).
# ---------------------------------------------------------------------

# scrape_manager_metrics prints the concatenated /metrics of every manager
# pod. Every pod, not just the leader: the verifier is a leader-election
# runnable, so only one replica reports the verification series and which
# one is not knowable from here. The `!tunnelport.giantswarm.io/role`
# selector excludes the singleton trust-bundle tbot, which shares the
# chart's selector labels and serves no metrics at all.
scrape_manager_metrics() {
  local ips
  ips="$(kubectl --context kind-consumer -n tunnelport-system get pods \
    -l 'app.kubernetes.io/name=tunnelport,!tunnelport.giantswarm.io/role' \
    --field-selector=status.phase=Running \
    -o jsonpath='{range .items[*]}{.status.podIP} {end}')"
  if [[ -z "${ips// /}" ]]; then
    return 1
  fi
  kubectl --context kind-consumer -n tunnelport-system delete pod smoke-metrics \
    --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl --context kind-consumer -n tunnelport-system run smoke-metrics \
    --rm -i --restart=Never --quiet --image=curlimages/curl:8.10.1 -- \
    sh -c "for ip in ${ips}; do curl -s --max-time 5 http://\$ip:${METRICS_PORT}/metrics || true; done" \
    2>/dev/null
}

# wait_for_metric polls the manager metrics until a line matches the given
# extended regex. Here-strings rather than pipes throughout: run.sh runs
# under `set -o pipefail`, and `grep -q` closing the pipe early would give
# the upstream command a SIGPIPE and fail the pipeline on a match.
wait_for_metric() {
  local label="$1" pattern="$2" timeout="$3"
  local deadline=$((SECONDS + timeout)) out=""
  while ((SECONDS < deadline)); do
    out="$(scrape_manager_metrics || true)"
    if grep -qE "${pattern}" <<<"${out}"; then
      printf '  metric ok: %s\n' "${label}"
      grep -E '^tunnelport_(remoteapp_tls_verification|tls_verification_available)' <<<"${out}" || true
      return 0
    fi
    sleep 5
  done
  warn "timed out after ${timeout}s waiting for metric: ${label} (/${pattern}/)"
  warn "last scrape, verification series only:"
  grep -E '^tunnelport_' <<<"${out}" >&2 || warn "  (no tunnelport_* series at all)"
  return 1
}

# assert_condition checks one RemoteApp condition's status and reason.
assert_condition() {
  local ctype="$1" want_status="$2" want_reason="$3"
  local got_status got_reason got_message
  got_status="$(kubectl --context kind-consumer -n smoke get remoteapp smoke-app \
    -o jsonpath="{.status.conditions[?(@.type==\"${ctype}\")].status}")"
  got_reason="$(kubectl --context kind-consumer -n smoke get remoteapp smoke-app \
    -o jsonpath="{.status.conditions[?(@.type==\"${ctype}\")].reason}")"
  got_message="$(kubectl --context kind-consumer -n smoke get remoteapp smoke-app \
    -o jsonpath="{.status.conditions[?(@.type==\"${ctype}\")].message}")"
  if [[ "${got_status}" != "${want_status}" || "${got_reason}" != "${want_reason}" ]]; then
    warn "condition ${ctype}: got ${got_status}/${got_reason}, want ${want_status}/${want_reason}"
    warn "  message: ${got_message}"
    return 1
  fi
  printf '  condition ok: %s=%s (%s)\n' "${ctype}" "${got_status}" "${got_reason}"
  [[ -n "${got_message}" ]] && printf '    message: %s\n' "${got_message}"
  return 0
}

# wait_for_condition polls until a condition reaches status/reason.
wait_for_condition() {
  local ctype="$1" want_status="$2" want_reason="$3" timeout="$4"
  local deadline=$((SECONDS + timeout))
  while ((SECONDS < deadline)); do
    if assert_condition "${ctype}" "${want_status}" "${want_reason}" >/dev/null 2>&1; then
      assert_condition "${ctype}" "${want_status}" "${want_reason}"
      return 0
    fi
    sleep 5
  done
  warn "timed out after ${timeout}s waiting for ${ctype}=${want_status}/${want_reason}"
  assert_condition "${ctype}" "${want_status}" "${want_reason}" || true
  return 1
}

# restart_tunnel_and_wait_ready deletes the tunnel pods so tbot re-mints
# its SVID against whatever the WorkloadIdentity now says, then waits for
# the replacement to pass its probes.
#
# That the pod becomes Ready again is not incidental here — it is half the
# assertion. The ghostunnel readiness probe is a TCPSocket connect, so it
# passes with a wrong-SAN certificate, which is precisely why the
# verification dial had to exist.
restart_tunnel_and_wait_ready() {
  kubectl --context kind-consumer -n smoke delete pod \
    -l tunnelport.giantswarm.io/role=tbot,tunnelport.giantswarm.io/remoteapp=smoke-app \
    --wait=true >/dev/null
  kubectl --context kind-consumer -n smoke rollout status deployment/smoke-app \
    --timeout="${READY_WAIT}s" >/dev/null
  kubectl --context kind-consumer -n smoke wait remoteapp/smoke-app \
    --for=jsonpath='{.status.ready}'=true --timeout="${READY_WAIT}s" >/dev/null
}


# ---------------------------------------------------------------------
# Steps.
# ---------------------------------------------------------------------

step "Building operator image (${OPERATOR_IMAGE})"
# The Dockerfile packages a prebuilt static binary named the way
# architect/go-build names it (tunnelport-linux-<arch>); it no longer
# compiles. In CI the e2e job attaches the go-build workspace, so the
# binary is already there. Locally, build it for the host architecture
# when it is missing -- `docker build` targets the host platform, so
# TARGETARCH in the Dockerfile resolves to the same value.
host_arch="$(uname -m)"
case "${host_arch}" in
  x86_64) host_arch=amd64 ;;
  aarch64|arm64) host_arch=arm64 ;;
esac
if [[ ! -f "tunnelport-linux-${host_arch}" ]]; then
  CGO_ENABLED=0 GOOS=linux GOARCH="${host_arch}" go build -o "tunnelport-linux-${host_arch}" .
fi
docker build -t "${OPERATOR_IMAGE}" . >/dev/null

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

step "Installing Teleport"
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

# Single-phase install (#92). The proxy must never run with the
# unsubstituted REPLACE_WITH_TELEPORT_PROXY_ADDR placeholder, not even
# briefly: a proxy registers its advertised address as a proxy
# heartbeat in the auth backend, and that heartbeat outlives the pod by
# its announce TTL. The producer's app agent then derives smoke-app's
# address in Teleport's FindPublicAddr (lib/srv/app/watcher.go), which
# reads `GetProxies()` and takes **servers[0]** — the *first* proxy in
# the list, not the newest. So one placeholder-serving proxy start is
# enough to register smoke-app at
# `smoke-app.replace_with_teleport_proxy_addr` and fail the curl
# assertion minutes later, depending only on list order: a coin flip.
#
# Note this rules out the obvious-looking fixes. `helm upgrade --wait`
# already rolls the proxy (the chart puts a `checksum/config`
# annotation on the Deployment), so the running pod was never the
# problem — the stale backend entry was. And rolling the proxy again
# cannot help: a roll *adds* a heartbeat, it does not evict the stale
# one.
#
# So both halves of TELEPORT_PROXY_ADDR are resolved BEFORE the install:
#   - the IP: the kind node container already exists (created above), so
#     `docker inspect` works.
#   - the port: pinned by hack/smoke/teleport/proxy-nodeport.yaml, a
#     Service we own, because the chart has no nodePort knob (see the
#     comments in that file and in helm-values.yaml). It is applied
#     below, before the chart, so kube reserves the port for us.
#
# That manifest is the single source of truth for the port; read it back
# rather than restating it here, so the two can never drift.
TELEPORT_PROXY_NODEPORT="$(sed -n 's/^[[:space:]]*nodePort:[[:space:]]*\([0-9]\{1,\}\)[[:space:]]*$/\1/p' \
  hack/smoke/teleport/proxy-nodeport.yaml | head -1)"
if ! printf '%s' "${TELEPORT_PROXY_NODEPORT}" | grep -qE '^[0-9]+$'; then
  warn "Could not read the pinned nodePort from hack/smoke/teleport/proxy-nodeport.yaml (got: '${TELEPORT_PROXY_NODEPORT}')."
  exit 1
fi
TELEPORT_PROXY_IP="$(docker inspect teleport-control-plane --format '{{ .NetworkSettings.Networks.kind.IPAddress }}')"
TELEPORT_PROXY_ADDR="${TELEPORT_PROXY_IP}:${TELEPORT_PROXY_NODEPORT}"
echo "TELEPORT_PROXY_ADDR=${TELEPORT_PROXY_ADDR}"

# The namespace and the pinned Service come first so the proxy's
# ingress path exists before any proxy pod does. `helm --wait` below
# then covers both. The Service selects the chart's proxy pods, so it
# sits Endpoint-less until they are Ready — asserted right after.
kubectl --context kind-teleport create namespace teleport \
  --dry-run=client -o yaml | kubectl --context kind-teleport apply -f - >/dev/null
kubectl --context kind-teleport apply -f hack/smoke/teleport/proxy-nodeport.yaml >/dev/null

sed "s|REPLACE_WITH_TELEPORT_PROXY_ADDR|${TELEPORT_PROXY_ADDR}|" \
  hack/smoke/teleport/helm-values.yaml > "${TELEPORT_VALUES_FILE}"
helm --kube-context kind-teleport upgrade --install teleport-cluster \
  teleport/teleport-cluster --version "${TELEPORT_CHART_VERSION}" \
  --namespace teleport --values "${TELEPORT_VALUES_FILE}" \
  --wait --timeout "${HELM_WAIT}s" >/dev/null

step "Asserting the proxy is reachable on the pinned NodePort"
# Endpoints on our Service prove its selector still matches the chart's
# proxy pods. If upstream ever renames those labels this is where it
# surfaces, instead of as an i/o timeout from a peer cluster's tbot.
PROXY_ENDPOINTS=""
for _ in $(seq 1 30); do
  PROXY_ENDPOINTS="$(kubectl --context kind-teleport -n teleport get endpoints teleport-proxy-nodeport \
    -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || true)"
  [[ -n "${PROXY_ENDPOINTS}" ]] && break
  sleep 2
done
if [[ -z "${PROXY_ENDPOINTS}" ]]; then
  warn "Service teleport-proxy-nodeport has no endpoints after 60s — its selector no longer matches the chart's proxy pods."
  kubectl --context kind-teleport -n teleport get pods \
    -l app.kubernetes.io/component=proxy --show-labels >&2 || true
  exit 1
fi
echo "teleport-proxy-nodeport endpoints: ${PROXY_ENDPOINTS}"

step "Provisioning Teleport role, WorkloadIdentity, bot, and producer agent token via tctl"
AUTH_POD="$(kubectl --context kind-teleport -n teleport get pods \
  -l app.kubernetes.io/component=auth -o jsonpath='{.items[0].metadata.name}')"

# Guard the #92 invariant at its source, before anything derives an
# address from it: every proxy heartbeat in the auth backend advertises
# the address we substituted, and none carries the placeholder. Teleport
# lowercases it, hence -i.
PROXIES_LAST=""
PROXIES_OK=0
for _ in $(seq 1 30); do
  PROXIES_LAST="$(kubectl --context kind-teleport -n teleport exec "$AUTH_POD" -- \
    tctl get proxies 2>/dev/null || true)"
  if printf '%s' "${PROXIES_LAST}" | grep -qF "${TELEPORT_PROXY_IP}" &&
     ! printf '%s' "${PROXIES_LAST}" | grep -qi 'replace_with_teleport_proxy_addr'; then
    PROXIES_OK=1
    break
  fi
  sleep 2
done
if [[ "${PROXIES_OK}" -ne 1 ]]; then
  warn "No proxy heartbeat advertising ${TELEPORT_PROXY_ADDR}, or one still carries the publicAddr placeholder (#92)."
  printf '%s\n' "${PROXIES_LAST}" | grep -E 'name:|public_addr' >&2 || true
  exit 1
fi
echo "All proxy heartbeats advertise ${TELEPORT_PROXY_IP}."

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

step "Verifying smoke-app registered with Teleport at the expected public address"
# Heartbeat-based registration; appears in app_servers, not the static `apps`.
#
# Assert the ADDRESS, not just the name (#92). smoke-app sets no
# explicit public_addr, so Teleport derives one as
# `DefaultAppPublicAddr(appName, addr.Host())` — literally
# "<app>.<host of the proxy's advertised address>", port stripped.
# A proxy serving the placeholder therefore still registers a perfectly
# well-named `smoke-app`, just at
# `smoke-app.replace_with_teleport_proxy_addr`. Grepping for the name
# alone passed on exactly those runs, and the real fault surfaced three
# minutes later as an opaque `timed out waiting for the condition on
# jobs/smoke-curl`.
EXPECTED_APP_ADDR="smoke-app.${TELEPORT_PROXY_IP}"
APP_SERVERS_LAST=""
APP_SERVERS_OK=0
for _ in $(seq 1 30); do
  APP_SERVERS_LAST="$(kubectl --context kind-teleport -n teleport exec "$AUTH_POD" -- \
    tctl get app_servers 2>/dev/null || true)"
  if printf '%s' "${APP_SERVERS_LAST}" | grep -q 'name: smoke-app' &&
     printf '%s' "${APP_SERVERS_LAST}" | grep -qF "${EXPECTED_APP_ADDR}"; then
    APP_SERVERS_OK=1
    break
  fi
  sleep 2
done
if [[ "${APP_SERVERS_OK}" -ne 1 ]]; then
  if printf '%s' "${APP_SERVERS_LAST}" | grep -qi 'replace_with_teleport_proxy_addr'; then
    warn "smoke-app registered at the publicAddr placeholder instead of ${EXPECTED_APP_ADDR} — the #92 regression is back."
  else
    warn "smoke-app did not register as an app_server at ${EXPECTED_APP_ADDR} within 60s"
  fi
  printf '%s\n' "${APP_SERVERS_LAST}" | grep -E 'name:|public_addr|uri:' >&2 || true
  exit 1
fi
echo "smoke-app present in app_servers at ${EXPECTED_APP_ADDR}."

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

step "Probing consumer→teleport proxy reachability"
# Sanity gate: fail loud and fast if the consumer→teleport NodePort
# path is genuinely partitioned, instead of burning the operator
# install's 600s budget. Note what this probe does NOT prove: it runs
# as an unlabeled pod, so it is not subject to the chart's
# NetworkPolicy. The #41 flake passed this probe every time while the
# trust-bundle tbot (which carries the chart's selector labels) was
# egress-blocked by that policy — see the OPERATOR_HELM_WAIT comment
# above for the full story.
kubectl --context kind-consumer run smoke-reach \
  --rm -i --restart=Never --image=curlimages/curl:8.10.1 -- \
  sh -c "for i in \$(seq 1 60); do
           curl -sk --max-time 3 https://${TELEPORT_PROXY_ADDR}/webapi/find >/dev/null 2>&1 && exit 0
           sleep 2
         done; exit 1" >/dev/null

step "Installing the prometheus-operator CRDs on the consumer cluster"
# The chart ships a PrometheusRule and a PodMonitor (both enabled by
# default) with no CRD-capability gate, so the install fails unless
# monitoring.coreos.com/v1 is registered first. Applying the real CRDs
# rather than disabling the templates also means the API server validates
# both objects, so a schema mistake in either surfaces here.
for crd in prometheusrules podmonitors; do
  kubectl --context kind-consumer apply --server-side -f \
    "https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/${PROMETHEUS_OPERATOR_VERSION}/example/prometheus-operator-crd/monitoring.coreos.com_${crd}.yaml" >/dev/null
done

step "Installing the operator on the consumer cluster"
# teleport.clusterName matches the Teleport cluster name configured in
# hack/smoke/teleport/helm-values.yaml; teleport.proxyAddr is the
# kind node container IP plus the pinned proxy NodePort (ADR 0005).
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
  --set verification.interval="${VERIFY_INTERVAL}" \
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


# ---------------------------------------------------------------------
# TLS verification (giantswarm/giantswarm#37521 gap 2).
#
# Everything above proves the tunnel works. This section proves the
# operator can tell when it does not — specifically in the one way nothing
# else it watches can see. The curl-with---cacert probe above would also
# catch a wrong-SAN certificate, but it is a fixture a human runs; these
# steps assert the always-on signal an alert can fire on.
#
# The load-bearing assertion is the contrast in the negative case: pod
# Ready, TunnelServing True, TCP probe green — and TunnelVerified False
# with the metric on cert_invalid. That combination is the incident.
# ---------------------------------------------------------------------

step "Asserting the tunnel verifies (positive case)"

# The manager reads the trust bundle from the mounted Secret, and the
# kubelet propagates tbot's write into that mount on its own sync loop, so
# the first rounds can legitimately report "no bundle". Wait for the
# operator to be able to judge at all before asking it for a verdict —
# otherwise a slow mount looks like a broken tunnel.
wait_for_metric "trust bundle available" \
  '^tunnelport_tls_verification_available 1$' "${VERIFY_WAIT}"

wait_for_metric "smoke-app verified" \
  '^tunnelport_remoteapp_tls_verification\{.*remoteapp_name="smoke-app".*result="verified".*\} 1$' \
  "${VERIFY_WAIT}"

wait_for_condition TunnelVerified True CertificateVerified "${VERIFY_WAIT}"

step "Breaking one SAN on Teleport Central (negative case)"
# Same WorkloadIdentity, same label, same role — only the dns_sans
# namespace segment changes. `--force` overwrites the resource created
# earlier in this run.
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create --force -f - < hack/smoke/teleport/workload-identity-wrong-san.yaml >/dev/null
echo "workload_identity smoke-app-svid now advertises smoke-app.agentic-platform.svc.cluster.local"

# Restart so tbot mints a fresh SVID against the edited resource. The
# rollout completing is itself an assertion: the tunnel comes back Ready
# with a certificate no caller can verify.
restart_tunnel_and_wait_ready
echo "Tunnel pod is Ready again — with the wrong-SAN SVID."

step "Asserting the wrong-SAN tunnel is detected while every old signal stays green"

# The old signals. All three were True throughout the real incident, and
# they must still be True here, or this test would be proving something
# easier than the thing that actually happened.
if [[ "$(kubectl --context kind-consumer -n smoke get remoteapp smoke-app \
    -o jsonpath='{.status.ready}')" != "true" ]]; then
  warn "status.ready is not true; the negative case is not reproducing the incident"
  exit 1
fi
assert_condition Ready True TunnelReady
assert_condition TunnelServing True TunnelServing
assert_condition IdentityIssued True IdentityIssued
echo "  Ready / IdentityIssued / TunnelServing are all True — exactly as in the incident."

# The new signal. Both halves: the CR condition a platform engineer reads,
# and the metric the alert fires on.
wait_for_condition TunnelVerified False CertificateInvalid "${VERIFY_WAIT}"

wait_for_metric "smoke-app reports cert_invalid" \
  '^tunnelport_remoteapp_tls_verification\{.*remoteapp_name="smoke-app".*result="cert_invalid".*\} 1$' \
  "${VERIFY_WAIT}"

# The condition message has to name the mismatch, not just assert one:
# that string is the diagnosis a responder acts on, and in the incident it
# is what identifies the stale namespace.
VERIFIED_MSG="$(kubectl --context kind-consumer -n smoke get remoteapp smoke-app \
  -o jsonpath='{.status.conditions[?(@.type=="TunnelVerified")].message}')"
echo "TunnelVerified message: ${VERIFIED_MSG}"
for want in "SAN mismatch" "smoke-app.smoke.svc.cluster.local" "smoke-app.agentic-platform.svc.cluster.local"; do
  if [[ "${VERIFIED_MSG}" != *"${want}"* ]]; then
    warn "TunnelVerified message does not mention '${want}'"
    exit 1
  fi
done
echo "  message names both the expected FQDN and the SAN actually presented."

step "Restoring the SAN and asserting the signal clears"
# Recovery matters as much as detection: a signal that latches would keep
# the alert firing after the fix and train people to ignore it.
kubectl --context kind-teleport -n teleport exec -i "$AUTH_POD" -- \
  tctl create --force -f - < hack/smoke/teleport/workload-identity.yaml >/dev/null
restart_tunnel_and_wait_ready

wait_for_condition TunnelVerified True CertificateVerified "${VERIFY_WAIT}"
wait_for_metric "smoke-app verified again" \
  '^tunnelport_remoteapp_tls_verification\{.*remoteapp_name="smoke-app".*result="verified".*\} 1$' \
  "${VERIFY_WAIT}"

# And the cert_invalid series must be gone rather than sitting at 1
# alongside the new one — the property that keeps a recovered tunnel from
# pinning its alert forever.
LAST_METRICS="$(scrape_manager_metrics || true)"
if grep -qE '^tunnelport_remoteapp_tls_verification\{.*remoteapp_name="smoke-app".*result="cert_invalid"' <<<"${LAST_METRICS}"; then
  warn "the cert_invalid series survived recovery; the alert would never resolve"
  grep -E '^tunnelport_remoteapp_tls_verification' <<<"${LAST_METRICS}" >&2 || true
  exit 1
fi
echo "  the cert_invalid series is gone; exactly one series per RemoteApp."

step "✅ SMOKE PASSED"
SMOKE_RESULT=ok
