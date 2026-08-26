#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Authors of castai-guardrails-controllers
#
# Verifies that hack/update-codegen.sh produces byte-identical output to the
# committed tree. Copies the repo to a temp directory, regenerates there, and
# diffs the generated files against the original tree.
#
# Exit codes:
#   0 — no diff (generated tree matches committed tree)
#   1 — diff found (printed to stdout)
#   2 — tool missing or unexpected failure
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d -t castai-guardrails-verify-codegen.XXXXXX)"
trap 'rm -rf "${TMP_DIR}"' EXIT

if ! command -v controller-gen >/dev/null 2>&1; then
  echo "controller-gen not found in PATH" >&2
  exit 2
fi

echo "==> Copying repo to ${TMP_DIR}"
# Use cp -R to preserve all metadata; tar would also work but is heavier.
cp -R "${ROOT_DIR}/." "${TMP_DIR}/"

echo "==> Running hack/update-codegen.sh in temp copy"
(cd "${TMP_DIR}" && bash hack/update-codegen.sh >/dev/null)

echo "==> Diffing generated artifacts"
# Files we care about (relative paths):
REL_PATHS=(
  "apis/workloads/v1/zz_generated.deepcopy.go"
  "crds/workloads.cast.ai_tscoriginals.yaml"
  "crds/workloads.cast.ai_jvmprobeoriginals.yaml"
  "controllers/crds/helm/castai-guardrails-crds/templates/workloads.cast.ai-tscoriginals.yaml"
  "controllers/crds/helm/castai-guardrails-crds/templates/workloads.cast.ai-jvmprobeoriginals.yaml"
)

diff_failed=0
for rel in "${REL_PATHS[@]}"; do
  if ! diff -u "${ROOT_DIR}/${rel}" "${TMP_DIR}/${rel}" > "${TMP_DIR}/diff.${rel//\//_}.out"; then
    echo "DIFF: ${rel}"
    cat "${TMP_DIR}/diff.${rel//\//_}.out"
    diff_failed=1
  else
    echo "OK: ${rel}"
  fi
done

if [[ "${diff_failed}" -ne 0 ]]; then
  echo
  echo "verify-codegen.sh: FAILED — generated artifacts differ from committed tree."
  echo "Run hack/update-codegen.sh and commit the regenerated files."
  exit 1
fi

echo
echo "verify-codegen.sh: OK — generated artifacts match committed tree."
exit 0
