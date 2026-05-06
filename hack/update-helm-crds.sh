#!/usr/bin/env bash
#
# Regenerate `helm/tunnelport/templates/crds.yml` from `config/crd/bases/`.
# The chart's CRD bundle has to track exactly what `make manifests`
# produces, so this target is wired into CI / pre-commit and runs after
# the kubebuilder generator.
#
# Why a script and not Helm's built-in `crds/` directory:
#
#   * we want a `crds.install` toggle (Helm's `crds/` is mandatory and
#     un-templated),
#   * we want `helm.sh/resource-policy: keep` so the CRD outlives
#     `helm uninstall` (Helm's `crds/` ignores resource-policy),
#   * we want the standard GS chart labels for visibility in
#     `kubectl get crd`.
#
# The script is intentionally awk-only — no PyYAML, no yq dep — because
# the input shape is the predictable controller-gen output, and we want
# this to run in the slimmest CI image.
#
# Transform applied per CRD file in config/crd/bases/:
#
#   1. Insert `helm.sh/resource-policy: keep` and a couple of GS chart
#      labels into the existing `metadata.annotations` / `metadata.labels`
#      blocks (creating them if missing).
#   2. Concatenate all CRDs into a single bundle separated by `---`.
#   3. Wrap the whole bundle in {{- if .Values.crds.install }} ... {{- end }}.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRD_SRC_DIR="${REPO_ROOT}/config/crd/bases"
CHART_DIR="${REPO_ROOT}/helm/tunnelport"
OUT_FILE="${CHART_DIR}/templates/crds.yml"

if [ ! -d "${CRD_SRC_DIR}" ]; then
  echo "error: ${CRD_SRC_DIR} does not exist; run 'make manifests' first" >&2
  exit 1
fi

# Bail loudly if anyone managed to get a CRD file with `metadata.labels`
# already populated — controller-gen never emits that, and the awk
# transform below assumes it. If this fires, tighten the script before
# regenerating.
for f in "${CRD_SRC_DIR}"/*.yaml; do
  if grep -q '^  labels:' "${f}"; then
    echo "error: ${f} already has metadata.labels; update hack/update-helm-crds.sh to merge instead of insert" >&2
    exit 2
  fi
done

mkdir -p "$(dirname "${OUT_FILE}")"

{
  cat <<'HEADER'
# GENERATED. Do not edit by hand.
# Source: config/crd/bases/*.yaml
# Regenerate with: make update-helm-crds
#
# The CRD is bundled here (rather than under Helm's built-in `crds/`
# directory) so the chart can:
#   * gate installation on `crds.install` (default true),
#   * stamp `helm.sh/resource-policy: keep` so the CRD survives
#     `helm uninstall` along with any live RemoteApp instances,
#   * carry the standard GS chart labels.
{{- if .Values.crds.install }}
HEADER

  for f in "${CRD_SRC_DIR}"/*.yaml; do
    echo "---"
    awk '
      # State: in_meta=1 between `metadata:` and the next top-level
      # key; in_annotations=1 inside the metadata.annotations block.
      BEGIN { in_meta = 0; in_annotations = 0; injected = 0 }

      /^metadata:/ {
        print
        in_meta = 1
        next
      }

      # Detect leaving metadata: a line at the document-root indent
      # level that is not a comment and not blank. metadata children
      # are 2-space indented, so anything starting at column 0 with a
      # letter ends the block.
      in_meta && /^[A-Za-z]/ {
        if (!injected) {
          # metadata had no annotations or labels block at all (rare
          # but possible). Inject both before leaving.
          print "  annotations:"
          print "    helm.sh/resource-policy: keep"
          print "  labels:"
          print "    app.kubernetes.io/managed-by: Helm"
          print "    application.giantswarm.io/team: bumblebee"
          injected = 1
        }
        in_meta = 0
        in_annotations = 0
        print
        next
      }

      in_meta && /^  annotations:/ {
        print
        in_annotations = 1
        # Inject our annotation immediately so it lands inside the
        # block regardless of how many existing entries follow.
        print "    helm.sh/resource-policy: keep"
        next
      }

      # The annotations block ends when we hit the next 2-space-indented
      # key (e.g. `  labels:` or `  name:`). At that point also inject
      # our labels under `  labels:` (creating it if absent).
      in_annotations && /^  [A-Za-z]/ {
        in_annotations = 0
        if (/^  labels:/) {
          print
          print "    app.kubernetes.io/managed-by: Helm"
          print "    application.giantswarm.io/team: bumblebee"
          injected = 1
          next
        }
        # No labels block — inject one before continuing.
        print "  labels:"
        print "    app.kubernetes.io/managed-by: Helm"
        print "    application.giantswarm.io/team: bumblebee"
        injected = 1
        print
        next
      }

      { print }

      END {
        if (in_meta && !injected) {
          # File ended while still inside metadata — defensive branch.
          print "  labels:"
          print "    app.kubernetes.io/managed-by: Helm"
          print "    application.giantswarm.io/team: bumblebee"
        }
      }
    ' "${f}" \
    | sed '/^---$/d'
  done

  echo "{{- end }}"
} > "${OUT_FILE}"

echo "wrote ${OUT_FILE}"
