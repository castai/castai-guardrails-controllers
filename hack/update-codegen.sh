#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Authors of castai-guardrails-controllers
#
# Regenerates generated artifacts from the Go types in apis/workloads/v1:
#
#   * deepcopy methods       (controller-gen object)
#   * CRD YAML manifests     (controller-gen crd)
#   * Helm CRD templates     (wrapped in {{- if .Values.installCRDs }} ... {{- end }})
#
# The typed clientset at clientset/versioned/ is intentionally hand-written
# and is NOT regenerated here — PR 1's surface is small enough (two CRDs)
# that hand-written clients build/test faster than pulling k8s.io/code-generator
# into the module graph. When the surface grows, replace this block with a
# client-gen invocation.
#
# Tool: sigs.k8s.io/controller-tools/cmd/controller-gen v0.16.4.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

if ! command -v controller-gen >/dev/null 2>&1; then
  echo "controller-gen not found in PATH; install with:" >&2
  echo "  go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.4" >&2
  exit 1
fi

# Clean previous generated artifacts so stale files don't accumulate.
rm -f apis/workloads/v1/zz_generated_deepcopy.go \
      apis/workloads/v1/zz_generated.deepcopy.go \
      crds/workloads.cast.ai_*.yaml

echo "==> controller-gen object (deepcopy methods)"
controller-gen \
  object:headerFile=hack/boilerplate.go.txt \
  paths=./apis/... \
  output:dir=apis/workloads/v1

echo "==> controller-gen crd (CRD manifests)"
controller-gen \
  crd:maxDescLen=0 \
  paths=./apis/... \
  output:crd:dir=./crds

# Strip the leading comment block controller-gen prepends; keep the YAML
# document itself, plus a single SPDX/Copyright header so the output is
# self-contained. Also inject the helm.sh/resource-policy: keep annotation
# so `helm uninstall` does not delete the CRDs.
for f in crds/workloads.cast.ai_tscoriginals.yaml crds/workloads.cast.ai_jvmprobeoriginals.yaml; do
  tmp="${f}.tmp"
  {
    echo "# SPDX-License-Identifier: Apache-2.0"
    echo "# Copyright Authors of castai-guardrails-controllers"
    echo "---"
    awk 'BEGIN{p=0} /^---$/{p=1; next} p{print}' "${f}"
  } > "${tmp}"
  mv "${tmp}" "${f}"
  # Insert helm.sh/resource-policy: keep on the line after the
  # controller-gen.kubebuilder.io/version annotation. Use awk for portability.
  awk '
    /^    controller-gen\.kubebuilder\.io\/version:/ {
      print
      print "    helm.sh/resource-policy: keep"
      next
    }
    { print }
  ' "${f}" > "${f}.tmp2"
  mv "${f}.tmp2" "${f}"
done

echo "==> Updating Helm CRD templates (installCRDs guard)"
HELM_CRD_DIR="controllers/crds/helm/castai-guardrails-crds/templates"
# Remove any stale Helm templates (hand-written names from earlier rounds)
# so we only ship the controller-gen derived templates.
find "${HELM_CRD_DIR}" -maxdepth 1 -type f -name '*.yaml' ! -name 'workloads.cast.ai-*' -delete
for crd in workloads.cast.ai_tscoriginals.yaml workloads.cast.ai_jvmprobeoriginals.yaml; do
  src="crds/${crd}"
  dst="${HELM_CRD_DIR}/$(basename "${crd%.yaml}" | tr '_' '-').yaml"
  {
    echo '{{- if .Values.installCRDs }}'
    cat "${src}"
    echo '{{- end }}'
  } > "${dst}"
done

echo "==> Done."
find apis/workloads/v1 -name 'zz_generated*.go' -print
find crds -name '*.yaml' -print
find "${HELM_CRD_DIR}" -name '*.yaml' -print
