#!/bin/bash
# Build, push, and deploy the TSC Controller from this directory.

set -e

NAMESPACE="${1:-castai-agent}"
IMAGE_TAG="${2:-latest}"
DOCKER_USER="${DOCKER_USER:-marcusarenas}"
IMAGE_NAME="${DOCKER_USER}/castai-tsc-controller:${IMAGE_TAG}"

# Run from the repo root so the Dockerfile's ../../apis, ../../clientset,
# ../../snapshot replace directives resolve.
cd "$(dirname "$0")/../.."

echo "=== Dev Deploy: TSC Controller ==="
echo "Namespace:  $NAMESPACE"
echo "Image Tag:  $IMAGE_TAG"
echo "Image:      $IMAGE_NAME"
echo ""

echo "[1/3] Building image..."
docker build -t "$IMAGE_NAME" -f controllers/tsc-controller/Dockerfile .

echo ""
echo "[2/3] Pushing image..."
docker push "$IMAGE_NAME"

echo ""
echo "[3/3] Helm upgrade --install castai-tsc-controller..."
helm upgrade --install castai-tsc-controller \
  ./helm/castai-tsc-controller/ \
  --namespace "$NAMESPACE" \
  --create-namespace \
  --set image.tag="$IMAGE_TAG" \
  --set webhook.enabled=true \
  --set certManager.enabled=true \
  --set certManager.issuerRef.name=castai-guardrails-selfsigned \
  --set certManager.issuerRef.kind=ClusterIssuer \
  --set tls.manualSecret.enabled=false

echo ""
echo "=== Dev Deploy Complete: TSC Controller ==="
