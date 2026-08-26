#!/usr/bin/env bash
set -euo pipefail

# Renders templates/prometheusrule.yaml, extracts the bare rule groups
# (promtool wants `groups:`, not the PrometheusRule CRD wrapper), and runs
# the promtool unit tests against them. Requires: helm, yq, promtool.
#
# Wired into CI as part of the `chart-test` job (.circleci/custom.yml).
# It cannot ride along in `make test`: the architect Go image carries
# neither promtool nor the helm plugins.

# mikefarah/yq, not the similarly named python jq wrapper -- the `eval`
# syntax below is specific to it, and the wrapper fails with a confusing
# jq parse error instead of saying which yq it wanted.
if ! yq --version 2>&1 | grep -q 'mikefarah/yq'; then
  echo "error: mikefarah/yq is required (see https://github.com/mikefarah/yq)" >&2
  exit 1
fi

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
chart="$here/.."
rendered="$here/rendered.rules.yaml"

trap 'rm -f "$rendered"' EXIT

helm template tp "$chart" \
  --set teleport.clusterName=teleport.example.com \
  --set teleport.proxyAddr=teleport.example.com:443 \
  --set trustBundle.tokenName=ci \
  --show-only templates/prometheusrule.yaml \
  | yq eval '.spec' - > "$rendered"

promtool check rules "$rendered"
promtool test rules "$here/prometheusrule.test.yaml"
