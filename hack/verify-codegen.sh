#!/usr/bin/env bash
# verify-codegen.sh — checks that the hand-written deepcopy is present and
# non-empty. Exits 0 on success, non-zero otherwise.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
deepcopy="$repo_root/apis/workloads/v1/zz_generated_deepcopy.go"

if [[ ! -f "$deepcopy" ]]; then
  echo "verify-codegen: FAIL — $deepcopy not found" >&2
  exit 1
fi

if [[ ! -s "$deepcopy" ]]; then
  echo "verify-codegen: FAIL — $deepcopy is empty" >&2
  exit 1
fi

# Sanity-check it actually contains the generated deepcopy methods.
if ! grep -q "DeepCopyInto" "$deepcopy"; then
  echo "verify-codegen: FAIL — $deepcopy missing DeepCopyInto methods" >&2
  exit 1
fi

echo "verify-codegen: OK — $deepcopy present, non-empty, contains generated methods"
exit 0
