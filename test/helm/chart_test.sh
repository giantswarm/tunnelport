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
#   5. Every rendered object carries a non-empty
#      `application.giantswarm.io/team` label, which alert routing needs.
#   6. The PrometheusRule carries `observability.giantswarm.io/tenant`,
#      which the Mimir ruler selects on.
#   7. The metrics endpoint is actually scrapeable: a PodMonitor exists,
#      selects the manager (not the trust-bundle bot), and names the same
#      port the manager binds. A metric nobody scrapes is a metric nobody
#      can alert on (giantswarm/giantswarm#37521).
#   8. TLS verification wiring: the flags, the trust-bundle mount, the
#      NetworkPolicy egress the dial needs, and the off switches.
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
  --set trustBundle.tokenName=chart-test
)

echo "==> helm lint"
helm lint "${CHART}" "${TELEPORT_FLAGS[@]}"

echo "==> rendering default values"
# shellcheck disable=SC2034 # referenced by assert "..." strings below.
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
# Registry and name only: the tag and digest move on every renovate bump, and
# pinning them here buys nothing the values.yaml does not already state.
assert "default tbot.image flows to --tbot-image" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--tbot-image=gsoci[.]azurecr[.]io/giantswarm/tbot-distroless:'"

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
# shellcheck disable=SC2034 # referenced by assert "..." strings below.
RENDERED_OVERRIDE="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}" \
  --set tbot.image.registry=ghcr.io \
  --set tbot.image.name=example/tbot \
  --set tbot.image.tag=v999 \
  --set tbot.image.digest= \
  --set tbot.resources.requests.cpu=123m \
  --set tbot.resources.limits.memory=999Mi)"
assert "overridden tbot.image flows" \
  "printf '%s' \"\${RENDERED_OVERRIDE}\" | grep -E -- '--tbot-image=ghcr.io/example/tbot:v999\$'"
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
  "printf '%s' \"\${RENDERED}\" | grep '^kind: CustomResourceDefinition\$'"

# 4b. CRD carries helm.sh/resource-policy: keep.
assert "CRD has helm.sh/resource-policy: keep" \
  "printf '%s' \"\${RENDERED}\" | grep -E 'helm.sh/resource-policy:[[:space:]]+keep'"

# 4c. With crds.install=false the CRD disappears.
# shellcheck disable=SC2034 # referenced by assert "..." strings below.
RENDERED_NO_CRDS="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}" --set crds.install=false)"
assert "CRD suppressed with crds.install=false" \
  "! printf '%s' \"\${RENDERED_NO_CRDS}\" | grep '^kind: CustomResourceDefinition\$'"

# 4d. The other resources still render with crds.install=false.
assert "Deployment still renders with crds.install=false" \
  "printf '%s' \"\${RENDERED_NO_CRDS}\" | grep '^kind: Deployment\$'"

echo "==> imagePullSecrets assertion"
# No early `exit` in the awk: `assert` evaluates its body under
# `set -o pipefail`, so an awk that stops reading gives the upstream
# printf a SIGPIPE (141) and fails the whole pipeline once the rendered
# chart is large enough for printf not to finish first. Read to EOF and
# decide in END.
assert "manager pod references gsoci-pull-secret by default" \
  "printf '%s' \"\${RENDERED}\" \
    | awk '/kind: Deployment/{flag=1} flag && /imagePullSecrets:/{found=1} flag && found && /name: gsoci-pull-secret/{ok=1} END{exit !ok}'"

echo "==> tenant label assertions"
# Mimir's rule_selector matches on observability.giantswarm.io/tenant. Without
# it the PrometheusRule is created and its alerts never evaluate, which is a
# failure no amount of `kubectl get prometheusrule` reveals. The default lives
# in monitoring.prometheusRule.labels, so helm's map merge is what preserves it
# for an installation that sets some other label.
assert "PrometheusRule carries the giantswarm tenant label by default" \
  "printf '%s' \"\${RENDERED}\" | grep -E 'observability.giantswarm.io/tenant:[[:space:]]+\"?giantswarm\"?'"

# shellcheck disable=SC2034 # referenced by assert "..." strings below.
RENDERED_TENANT_OVERRIDE="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}" \
  --set 'monitoring.prometheusRule.labels.observability\.giantswarm\.io/tenant=customer')"
assert "an explicit tenant label overrides the default" \
  "printf '%s' \"\${RENDERED_TENANT_OVERRIDE}\" | grep -E 'observability.giantswarm.io/tenant:[[:space:]]+\"?customer\"?'"

# shellcheck disable=SC2034 # referenced by assert "..." strings below.
RENDERED_EXTRA_LABEL="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}" \
  --set monitoring.prometheusRule.labels.foo=bar)"
assert "an unrelated extra label does not displace the tenant default" \
  "printf '%s' \"\${RENDERED_EXTRA_LABEL}\" | grep -E 'observability.giantswarm.io/tenant:[[:space:]]+\"?giantswarm\"?'"

# shellcheck disable=SC2034 # referenced by assert "..." strings below.
RENDERED_NO_TENANT="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}" \
  --set 'monitoring.prometheusRule.labels.observability\.giantswarm\.io/tenant=null')"
# Scoped to the PrometheusRule: the PodMonitor carries the same label from its
# own value, so a whole-render grep would now be answering a different
# question than the one this assertion is about.
assert "setting the tenant key to null drops the label from the PrometheusRule" \
  "! printf '%s' \"\${RENDERED_NO_TENANT}\" | awk '
      /kind: PrometheusRule/ { pr=1 }
      pr && /observability.giantswarm.io\\/tenant:/ { found=1 }
      pr && /^spec:/ { pr=0 }
      END { exit !found }'"

echo "==> team label assertions"
# Alert routing keys on application.giantswarm.io/team, so an empty value is as
# bad as a missing one. `team_values` prints the label's value once per rendered
# object, with any surrounding quotes stripped, so the assertions below care
# about the value and not about how the template chose to quote it.
team_values() {
  printf '%s' "$1" \
    | sed -n 's|^[[:space:]]*application\.giantswarm\.io/team:[[:space:]]*||p' \
    | sed -e 's|^"\(.*\)"$|\1|' -e "s|^'\(.*\)'$|\1|"
}

# The Chart.yaml annotation feeds both the app catalog and the label helper, so
# assert the annotation and the rendered label separately: the two use different
# key spellings and app-build-suite rewrites one of them at package time.
assert "Chart.yaml carries io.giantswarm.application.team: bumblebee" \
  "grep -E 'io.giantswarm.application.team:[[:space:]]+bumblebee' \"${CHART}/Chart.yaml\""

assert "every rendered object carries team=bumblebee" \
  "team_values \"\${RENDERED}\" | grep -c . >/dev/null && ! team_values \"\${RENDERED}\" | grep -v '^bumblebee\$'"

# Which of the two annotation spellings a chart tree carries depends on whether
# app-build-suite has packaged it. Render a copy carrying the other spelling and
# assert the label still lands: an annotation lookup that resolves in only one
# of the two trees renders an empty label in the other, and an empty team label
# routes nowhere.
OTHER_SPELLING="$(mktemp -d)"
trap 'rm -rf "${OTHER_SPELLING}"' EXIT
cp -r "${CHART}" "${OTHER_SPELLING}/tunnelport"
sed -i 's|io.giantswarm.application.team: bumblebee|application.giantswarm.io/team: bumblebee|' \
  "${OTHER_SPELLING}/tunnelport/Chart.yaml"
# shellcheck disable=SC2034 # referenced by assert "..." strings below.
RENDERED_OTHER="$(helm template tunnelport "${OTHER_SPELLING}/tunnelport" "${TELEPORT_FLAGS[@]}")"

assert "team label survives the other annotation spelling" \
  "team_values \"\${RENDERED_OTHER}\" | grep -c . >/dev/null && ! team_values \"\${RENDERED_OTHER}\" | grep -v '^bumblebee\$'"

echo "==> metrics scrape assertions"

# The whole gap-2 signal is worthless if nothing collects it, and until
# this chart shipped a PodMonitor nothing did: there is no Service for the
# manager and there was no ServiceMonitor either, so every tunnelport_*
# metric was served and dropped.
assert "a PodMonitor is rendered by default" \
  "printf '%s' \"\${RENDERED}\" | grep '^kind: PodMonitor\$'"

# Port name, not number: the PodMonitor must follow .Values.ports.metrics
# rather than pin a literal that a values override could desync.
assert "PodMonitor scrapes the named metrics port" \
  "printf '%s' \"\${RENDERED}\" | awk '/kind: PodMonitor/{f=1} f && /- port: metrics/{found=1} END{exit !found}'"

# The manager serves plain HTTP on the metrics port; the secure variant
# would need the scraping agent's SA to hold a nonResourceURLs grant.
assert "manager serves metrics over plain HTTP" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--metrics-secure=false'"

assert "manager binds the metrics port from .Values.ports.metrics" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--metrics-bind-address=:8080'"

# The selector labels alone also match the singleton trust-bundle tbot
# (ADR 0008), which serves no metrics at all and would show up as a
# permanently-down target. Same exclusion as the NetworkPolicy and PDB.
assert "PodMonitor excludes the trust-bundle bot" \
  "printf '%s' \"\${RENDERED}\" | awk '/kind: PodMonitor/{f=1} f && /operator: DoesNotExist/{found=1} END{exit !found}'"

# The monitoring agent selects PodMonitors by a label on the PodMonitor
# object: `observability.giantswarm.io/tenant In [<tenant>]`, or
# `application.giantswarm.io/team Exists` via a selector its own config calls
# legacy. A PodMonitor matching neither is created and silently never
# scraped, which is indistinguishable from a broken exporter.
assert "PodMonitor carries the giantswarm tenant label by default" \
  "printf '%s' \"\${RENDERED}\" | awk '
      /kind: PodMonitor/ { pm=1 }
      pm && /observability.giantswarm.io\\/tenant:[[:space:]]+\"?giantswarm\"?/ { found=1 }
      pm && /^spec:/ { pm=0 }
      END { exit !found }'"

# shellcheck disable=SC2034 # referenced by assert "..." strings below.
RENDERED_PM_TENANT="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}" \
  --set 'monitoring.podMonitor.labels.observability\.giantswarm\.io/tenant=customer')"
assert "an explicit PodMonitor tenant label overrides the default" \
  "printf '%s' \"\${RENDERED_PM_TENANT}\" | awk '
      /kind: PodMonitor/ { pm=1 }
      pm && /observability.giantswarm.io\\/tenant:[[:space:]]+\"?customer\"?/ { found=1 }
      pm && /^spec:/ { pm=0 }
      END { exit !found }'"

# shellcheck disable=SC2034 # referenced by assert "..." strings below.
RENDERED_NO_PODMONITOR="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}" \
  --set monitoring.podMonitor.enabled=false)"
assert "PodMonitor suppressed with monitoring.podMonitor.enabled=false" \
  "! printf '%s' \"\${RENDERED_NO_PODMONITOR}\" | grep '^kind: PodMonitor\$'"

echo "==> TLS verification assertions (giantswarm/giantswarm#37521 gap 2)"

assert "verification is on by default" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--verify-tunnels=true'"

# The flag and the volumeMount are generated from one helper
# (tunnelport.trustBundleMountPath); a flag pointing at an unmounted path
# would report "cannot verify" forever, which is a silent failure of the
# check for silent failures.
assert "verification flag points at the mounted bundle path" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--verify-trust-bundle-file=/etc/tunnelport/spiffe/svid_bundle.pem'"

assert "manager mounts the trust-bundle Secret at that path" \
  "printf '%s' \"\${RENDERED}\" | awk '
      /^kind: Deployment\$/ { d=1 }
      d && /mountPath: \/etc\/tunnelport\/spiffe/ { mount=1 }
      d && /secretName: tunnelport-spiffe-bundle/ { vol=1 }
      END { exit !(mount && vol) }'"

assert "verification values flow to the manager flags" \
  "printf '%s' \"\${RENDERED}\" | grep -E -- '--verify-interval=2m' \
    && printf '%s' \"\${RENDERED}\" | grep -E -- '--verify-timeout=5s' \
    && printf '%s' \"\${RENDERED}\" | grep -E -- '--cluster-domain=cluster.local'"

# shellcheck disable=SC2034 # referenced by assert "..." strings below.
RENDERED_VERIFY_OVERRIDE="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}" \
  --set verification.interval=30s \
  --set verification.clusterDomain=k8s.example)"
assert "overridden verification values flow" \
  "printf '%s' \"\${RENDERED_VERIFY_OVERRIDE}\" | grep -E -- '--verify-interval=30s' \
    && printf '%s' \"\${RENDERED_VERIFY_OVERRIDE}\" | grep -E -- '--cluster-domain=k8s.example'"

# Without egress on the tunnel TLS port the dial reaches nothing and every
# tunnel reports `unreachable` -- a whole-fleet false alarm produced by the
# policy rather than by the tunnels.
assert "NetworkPolicy allows egress to the tunnel TLS port" \
  "printf '%s' \"\${RENDERED}\" | awk '
      /^kind: NetworkPolicy\$/ { np=1 }
      np && /^  egress:/ { eg=1 }
      eg && /- port: 8443/ { found=1 }
      END { exit !found }'"

# Two off switches, and both must take the whole mechanism down together:
# a dangling mount or a --verify-tunnels=true with no bundle would leave
# the operator reporting "I cannot tell" forever.
# shellcheck disable=SC2034 # referenced by assert "..." strings below.
RENDERED_NO_VERIFY="$(helm template tunnelport "${CHART}" "${TELEPORT_FLAGS[@]}" \
  --set verification.enabled=false)"
assert "verification.enabled=false disables the flag" \
  "printf '%s' \"\${RENDERED_NO_VERIFY}\" | grep -E -- '--verify-tunnels=false'"
assert "verification.enabled=false drops the trust-bundle mount" \
  "! printf '%s' \"\${RENDERED_NO_VERIFY}\" | grep -F 'mountPath: /etc/tunnelport/spiffe'"

# shellcheck disable=SC2034 # referenced by assert "..." strings below.
RENDERED_NO_BUNDLE="$(helm template tunnelport "${CHART}" \
  --set teleport.clusterName=teleport.example.com \
  --set teleport.proxyAddr=teleport.example.com:443 \
  --set trustBundle.enabled=false)"
assert "trustBundle.enabled=false disables verification too" \
  "printf '%s' \"\${RENDERED_NO_BUNDLE}\" | grep -E -- '--verify-tunnels=false'"
assert "trustBundle.enabled=false drops the trust-bundle mount" \
  "! printf '%s' \"\${RENDERED_NO_BUNDLE}\" | grep -F 'mountPath: /etc/tunnelport/spiffe'"

echo "==> new alert assertions"

# Each new alert must exist and carry a runbook_url under /runbooks/ --
# the legacy /ops-recipes/ paths the two original alerts pointed at were
# dead links, which is worse than no link at all.
for alert in TunnelPortTunnelCertificateInvalid TunnelPortTunnelUnreachable TunnelPortTLSVerificationUnavailable; do
  assert "alert ${alert} is rendered" \
    "printf '%s' \"\${RENDERED}\" | grep -F -- '- alert: ${alert}'"
done

assert "no alert points at the legacy ops-recipes path" \
  "! printf '%s' \"\${RENDERED}\" | grep -F 'ops-recipes'"

assert "every runbook_url lives under /runbooks/" \
  "n_urls=\$(printf '%s' \"\${RENDERED}\" | grep -c 'runbook_url:') \
   && n_ok=\$(printf '%s' \"\${RENDERED}\" | grep -c 'runbook_url:.*support-and-ops/runbooks/') \
   && [ \"\${n_urls}\" -eq \"\${n_ok}\" ] && [ \"\${n_urls}\" -eq 5 ]"

echo
echo "==> summary: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -ne 0 ]; then
  printf 'failed:\n'
  for f in "${FAILS[@]}"; do printf '  - %s\n' "$f"; done
  exit 1
fi
