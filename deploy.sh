#!/bin/bash
# Deploy controllers from castai-guardrails-controllers

set -e

NAMESPACE="${1:-castai-agent}"
IMAGE_TAG="${2:-latest}"

echo "Deploying CAST AI Guardrails Controllers..."
echo "Namespace: $NAMESPACE"
echo "Image Tag: $IMAGE_TAG"

# TSC Controller
echo ""
echo "=== Deploying TSC Controller ==="
helm upgrade --install castai-tsc-controller \
  ./controllers/tsc-controller/castai-tsc-controller/ \
  --namespace "$NAMESPACE" \
  --create-namespace \
  --set image.tag="$IMAGE_TAG"

# PDB Controller
echo ""
echo "=== Deploying PDB Controller ==="
helm upgrade --install castai-pdb-controller \
  ./controllers/pdb-controller/helm/castai-pdb-controller/ \
  --namespace "$NAMESPACE" \
  --set image.tag="$IMAGE_TAG"

# JVM Probe Controller
echo ""
echo "=== Deploying JVM Probe Controller ==="
helm upgrade --install castai-jvm-probe-controller \
  ./controllers/jvm-probe-controller/helm/castai-jvm-probe-controller/ \
  --namespace "$NAMESPACE" \
  --set image.tag="$IMAGE_TAG"

echo ""
echo "=== Deployment Complete ==="
kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/part-of=castai-workload-optimizer
