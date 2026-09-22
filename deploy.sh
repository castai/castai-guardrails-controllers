#!/bin/bash
# Deploy CAST AI Guardrails Controllers via Helm.
#
# Usage:
#   ./deploy.sh [NAMESPACE] [IMAGE_TAG] [--build]
#
# Args:
#   NAMESPACE   Kubernetes namespace to deploy into (default: castai-agent)
#   IMAGE_TAG   Container image tag (default: latest)
#
# Flags / env:
#   --build                 Build & push controller images before deploying.
#   BUILD_IMAGES=true       Same as --build (flag also honored).
#
# Image registry:
#   DOCKER_USER             Docker Hub user used when --build is enabled
#                           (default: marcusarenas, matching build-and-push.sh).

set -e

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
NAMESPACE="castai-agent"
IMAGE_TAG="latest"
BUILD_IMAGES="${BUILD_IMAGES:-false}"

POSITIONAL=()
for arg in "$@"; do
  case "$arg" in
    --build)
      BUILD_IMAGES=true
      ;;
    --help|-h)
      sed -n '2,18p' "$0"
      exit 0
      ;;
    *)
      POSITIONAL+=("$arg")
      ;;
  esac
done

if [ "${#POSITIONAL[@]}" -ge 1 ] && [ -n "${POSITIONAL[0]}" ]; then
  NAMESPACE="${POSITIONAL[0]}"
fi
if [ "${#POSITIONAL[@]}" -ge 2 ] && [ -n "${POSITIONAL[1]}" ]; then
  IMAGE_TAG="${POSITIONAL[1]}"
fi

DOCKER_USER="${DOCKER_USER:-marcusarenas}"

# Resolve repo root from this script's location so cd works regardless of cwd.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "Deploying CAST AI Guardrails Controllers..."
echo "  Namespace : $NAMESPACE"
echo "  Image Tag : $IMAGE_TAG"
echo "  Build     : $BUILD_IMAGES"
if [ "$BUILD_IMAGES" = "true" ]; then
  echo "  Docker User: $DOCKER_USER"
fi

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
# build_and_push <controller_dir> <image_name>
#   Runs docker build from the repository root (so ../../apis, ../../clientset,
#   ../../snapshot replace directives resolve), then docker push.
#   Produces ${DOCKER_USER}/<image_name>:${IMAGE_TAG}
build_and_push() {
  local controller_dir="$1"
  local image_name="$2"
  local full_image="${DOCKER_USER}/${image_name}:${IMAGE_TAG}"

  echo "  -> Building ${full_image}"
  docker build -t "${full_image}" -f "${SCRIPT_DIR}/controllers/${controller_dir}/Dockerfile" "${SCRIPT_DIR}"
  echo "  -> Pushing ${full_image}"
  docker push "${full_image}"
}

# deploy_controller <release_name> <helm_chart_path> <image_name> <controller_dir> [extra helm args...]
#   Runs `helm upgrade --install` for the given controller. When BUILD_IMAGES
#   is true, builds & pushes the image first and overrides image.repository so
#   the chart references the freshly-built image.
deploy_controller() {
  local release_name="$1"
  local helm_chart="$2"
  local image_name="$3"
  local controller_dir="$4"
  shift 4

  local repo_args=()
  if [ "$BUILD_IMAGES" = "true" ]; then
    build_and_push "$controller_dir" "$image_name"
    repo_args=(--set "image.repository=${DOCKER_USER}/${image_name}")
  fi

  echo "  -> Helm upgrade --install ${release_name}"
  helm upgrade --install "${release_name}" \
    "${helm_chart}" \
    --namespace "${NAMESPACE}" \
    --set "image.tag=${IMAGE_TAG}" \
    "${repo_args[@]}" \
    "$@"
}

# ---------------------------------------------------------------------------
# Deployments
# ---------------------------------------------------------------------------
echo ""
echo "=== Deploying TSC Controller ==="
deploy_controller castai-tsc-controller \
  ./controllers/tsc-controller/helm/castai-tsc-controller/ \
  castai-tsc-controller \
  tsc-controller \
  --create-namespace \
  --set webhook.enabled=true \
  --set certManager.enabled=true \
  --set certManager.issuerRef.name=castai-guardrails-selfsigned \
  --set certManager.issuerRef.kind=ClusterIssuer \
  --set tls.manualSecret.enabled=false

echo ""
echo "=== Deploying PDB Controller ==="
deploy_controller castai-pdb-controller \
  ./controllers/pdb-controller/helm/castai-pdb-controller/ \
  castai-pdb-controller \
  pdb-controller

echo ""
echo "=== Deploying JVM Probe Controller ==="
deploy_controller castai-jvm-probe-controller \
  ./controllers/jvm-probe-controller/helm/castai-jvm-probe-controller/ \
  castai-jvm-probe-controller \
  jvm-probe-controller \
  --set webhook.enabled=true \
  --set certManager.enabled=true \
  --set certManager.issuerRef.name=castai-guardrails-selfsigned \
  --set certManager.issuerRef.kind=ClusterIssuer \
  --set tls.manualSecret.enabled=false

echo ""
echo "=== Deployment Complete ==="
kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/part-of=castai-workload-optimizer
