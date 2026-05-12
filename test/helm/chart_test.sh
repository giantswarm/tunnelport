#!/usr/bin/env bash
#
# Chart assertions for helm/tunnelport.
#
# Run with: bash test/helm/chart_test.sh
#
# These tests are about the *chart* — what the rendered YAML looks like
# given the default values, not about the operator code. They do four
# things:
#
#   1. `helm lint` clean.
#   2. RBAC has no `pods/log` and no `secrets` write verbs (ADR 0003 / 0001).
#   3. `tbot.image` and `tbot.resources` Helm values flow through to the
#      manager Deployment's CLI flags.
#   4. CRD bundle is gated on `crds.install` and carries
#      `helm.sh/resource-policy: keep`.
#
# We use `grep` on the rendered YAML rather than yq so the test runs in
# any CI image with bash + helm.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART="${REPO_ROOT}/helm/tunnelport"

PASS=0
FAIL=0
FAILS=()

assert() {
  local name="$1"
  local body="$2"
  if eval "$body" >/dev/null 2>&1; then
    PASS=$((PASS + 1))
    printf '  ok  %s\n' "$name"
  else
    FAIL=$((FAIL + 1))
    FAILS+=("$name")
    printf '  FAIL %s\n' "$name" >&2
    eval "$body" || true
  fi
}

# Teleport binding (ADR 0005). Both values are REQUIRED by values.schema.json
# and the Deployment template uses `required` to fail fast. The chart_test
# passes shape-valid placeholders so every `helm template` invocation
# produces output; chart consumers must supply their own values.
TELEPORT_FLAGS=(
  --set teleport.clusterName=teleport.example.com
  --set teleport.proxyAddr=teleport.example.com:443
)

echo "==> helm lint"
helm lint "${CHART}" "${TELEPORT_FLAGS[@]}"

echo "==> rendering default values"
RENDERED="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}")"

echo "==> RBAC assertions"

# 1. No `pods/log` resource (only allowed in comments).
assert "no pods/log resource line" \
  "! printf '%s' \"\${RENDERED}\" | grep -E '^[[:space:]]*-[[:space:]]+pods/log$'"

# 2. The `secrets` resource appears only in a get;list;watch rule. We
# verify by extracting the secrets rule block and checking the verbs
# present don't include any write verb.
assert "secrets rule has only read verbs" "
  block=\$(printf '%s' \"\${RENDERED}\" \
    | awk '
        /^  - apiGroups:/ { in_rule=1; rule=\"\"; has_secrets=0 }
        in_rule { rule = rule \$0 ORS }
        in_rule && /^[[:space:]]+- secrets\$/ { has_secrets=1 }
        in_rule && /^  - apiGroups:/ && NR > 1 {
          if (has_secrets_prev) print prev_rule
          prev_rule=rule
          has_secrets_prev=has_secrets
          rule=\$0 ORS
          has_secrets=0
        }
        END { if (has_secrets) print rule }
      ')
  if printf '%s' \"\$block\" | grep -E '^[[:space:]]+- (create|update|patch|delete)\$' >/dev/null; then
    echo \"FOUND WRITE VERB ON SECRETS:\"
    printf '%s\n' \"\$block\"
    exit 1
  fi
"

echo "==> tbot value flow assertions"

# 3a. Default tbot.image flows through to a --tbot-image flag on the manager.
assert "default tbot.image flows to --tbot-image" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--tbot-image=public.ecr.aws/gravitational/tbot-distroless:18'"

# 3b. tbot.resources.requests.cpu flows.
assert "tbot.resources.requests.cpu flows to --tbot-cpu-request" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--tbot-cpu-request=50m'"
assert "tbot.resources.requests.memory flows to --tbot-memory-request" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--tbot-memory-request=64Mi'"
assert "tbot.resources.limits.cpu flows to --tbot-cpu-limit" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--tbot-cpu-limit=200m'"
assert "tbot.resources.limits.memory flows to --tbot-memory-limit" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--tbot-memory-limit=256Mi'"

# 3c. Overridden values flow through too — guards against accidental hardcoding.
RENDERED_OVERRIDE="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}" \
  --set tbot.image=ghcr.io/example/tbot:v999 \
  --set tbot.resources.requests.cpu=123m \
  --set tbot.resources.limits.memory=999Mi)"
assert "overridden tbot.image flows" \
  "printf '%s' \"\${RENDERED_OVERRIDE}\" | grep -E -- '--tbot-image=ghcr.io/example/tbot:v999'"
assert "overridden tbot.resources.requests.cpu flows" \
  "printf '%s' \"\${RENDERED_OVERRIDE}\" | grep -E -- '--tbot-cpu-request=123m'"
assert "overridden tbot.resources.limits.memory flows" \
  "printf '%s' \"\${RENDERED_OVERRIDE}\" | grep -E -- '--tbot-memory-limit=999Mi'"

echo "==> Teleport binding flow assertions (ADR 0005)"

assert "teleport.clusterName flows to --teleport-cluster-name" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--teleport-cluster-name=teleport.example.com'"
assert "teleport.proxyAddr flows to --teleport-proxy-addr" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--teleport-proxy-addr=teleport.example.com:443'"

# Missing teleport values: schema + the deployment's `required` should
# both refuse to render. Either error is acceptable — the chart MUST
# NOT silently render with empty flags that would crashloop every tbot.
assert "helm template fails without teleport values" \
  "! helm template tunnelport \"${CHART}\" >/dev/null 2>&1"

echo "==> CRD bundle assertions"

# 4a. CRD is rendered by default.
assert "CRD rendered with crds.install=true (default)" \
  "printf '%s' \"\${RENDERED}\" | grep -q '^kind: CustomResourceDefinition\$'"

# 4b. CRD carries helm.sh/resource-policy: keep.
assert "CRD has helm.sh/resource-policy: keep" \
  "printf '%s' \"\${RENDERED}\" | grep -E 'helm.sh/resource-policy:[[:space:]]+keep'"

# 4c. With crds.install=false the CRD disappears.
RENDERED_NO_CRDS="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}" --set crds.install=false)"
assert "CRD suppressed with crds.install=false" \
  "! printf '%s' \"\${RENDERED_NO_CRDS}\" | grep -q '^kind: CustomResourceDefinition\$'"

# 4d. The other resources still render with crds.install=false.
assert "Deployment still renders with crds.install=false" \
  "printf '%s' \"\${RENDERED_NO_CRDS}\" | grep -q '^kind: Deployment\$'"

echo "==> imagePullSecrets assertion"
assert "manager pod references gsoci-pull-secret by default" \
  "printf '%s' \"\${RENDERED}\" \
    | awk '/kind: Deployment/{flag=1} flag && /imagePullSecrets:/{found=1} flag && found && /name: gsoci-pull-secret/{print; exit 0}; END{exit !found}'"

echo "==> Chart annotation assertion"
assert "Chart.yaml carries application.giantswarm.io/team: bumblebee" \
  "grep -E 'application.giantswarm.io/team:[[:space:]]+bumblebee' \"${CHART}/Chart.yaml\""

echo
echo "==> summary: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -ne 0 ]; then
  printf 'failed:\n'
  for f in "${FAILS[@]}"; do printf '  - %s\n' "$f"; done
  exit 1
fi
