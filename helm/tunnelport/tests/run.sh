#!/usr/bin/env bash
set -euo pipefail

# Renders templates/prometheusrule.yaml, extracts the bare rule groups
# (promtool wants `groups:`, not the PrometheusRule CRD wrapper), and runs
# the promtool unit tests against them. Requires: helm, yq, promtool.

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
