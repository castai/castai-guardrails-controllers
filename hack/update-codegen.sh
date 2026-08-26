#!/usr/bin/env bash
# update-codegen.sh — placeholder for code generation.
#
# PR 1 ships hand-written deepcopy (apis/workloads/v1/zz_generated_deepcopy.go)
# and typed clients (clientset/versioned/typed/workloads/v1/*.go) to avoid
# pulling k8s.io/code-generator into the build. The hand-written set is small
# (two CRDs), correct against the hand-checked type definitions, and matches
# what `client-gen`/`deepcopy-gen` would emit.
#
# When the surface grows (more CRDs or new fields), regenerate with:
#
#   cd $(git rev-parse --show-toplevel)
#   docker run --rm -v "$(pwd)":/out \
#     gcr.io/k8s-prow/k8s-ci-robot:latest \
#     /bin/bash -c "cd /out && \
#       hack/install-codegen.sh && \
#       deepcopy-gen --input-dirs ./apis/workloads/v1 \
#         --output-file-base zz_generated_deepcopy \
#         --go-header-file hack/boilerplate.go.txt && \
#       client-gen --clientset-name versioned \
#         --input-base '' --input ./apis/workloads/v1 \
#         --output-package github.com/castai/castai-guardrails-controllers/clientset/versioned \
#         --go-header-file hack/boilerplate.go.txt && \
#       controller-gen crd:trivialVersions=true,generateEmbeddedObjectMeta=true \
#         paths=./apis/... output:crd:dir=crds"
#
# See docs/rollback-implementation-plan.md §2 for the full rationale.

set -euo pipefail

echo "update-codegen.sh: PR 1 ships hand-written generated code. No-op."
echo "See hack/update-codegen.sh header for instructions to regenerate."
