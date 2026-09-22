#!/bin/bash
# Build, push, and deploy the JVM Probe Controller from this directory.

set -e

NAMESPACE="${1:-castai-agent}"
IMAGE_TAG="${2:-latest}"
DOCKER_USER="${DOCKER_USER:-marcusarenas}"
IMAGE_NAME="${DOCKER_USER}/castai-jvm-probe-controller:${IMAGE_TAG}"

# Run from the repo root so the Dockerfile's ../../apis, ../../clientset,
# ../../snapshot replace directives resolve.
cd "$(dirname "$0")/../.."

echo "=== Dev Deploy: JVM Probe Controller ==="
echo "Namespace:  $NAMESPACE"
echo "Image Tag:  $IMAGE_TAG"
echo "Image:      $IMAGE_NAME"
echo ""

echo "[1/3] Building image..."
docker build -t "$IMAGE_NAME" -f controllers/jvm-probe-controller/Dockerfile .

echo ""
echo "[2/3] Pushing image..."
docker push "$IMAGE_NAME"

echo ""
echo "[3/3] Helm upgrade --install castai-jvm-probe-controller..."
helm upgrade --install castai-jvm-probe-controller \
  ./helm/castai-jvm-probe-controller/ \
  --namespace "$NAMESPACE" \
  --set image.tag="$IMAGE_TAG" \
  --set webhook.enabled=true \
  --set certManager.enabled=true \
  --set certManager.issuerRef.name=castai-guardrails-selfsigned \
  --set certManager.issuerRef.kind=ClusterIssuer \
  --set tls.manualSecret.enabled=false

echo ""
echo "=== Dev Deploy Complete: JVM Probe Controller ==="
