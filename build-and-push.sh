#!/bin/bash
set -e

VERSION="${1:-v1.1.0-safety}"
DOCKER_USER="marcusarenas"

# Get the directory where this script is located
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "=== Building JVM Probe Controller ==="
cd "${SCRIPT_DIR}/controllers/jvm-probe-controller"
docker build -t ${DOCKER_USER}/castai-jvm-probe-controller:${VERSION} .
docker push ${DOCKER_USER}/castai-jvm-probe-controller:${VERSION}

echo ""
echo "=== Building TSC Controller ==="
cd "${SCRIPT_DIR}/controllers/tsc-controller"
docker build -t ${DOCKER_USER}/castai-tsc-controller:${VERSION} .
docker push ${DOCKER_USER}/castai-tsc-controller:${VERSION}

echo ""
echo "=== Build and Push Complete ==="
echo "  - ${DOCKER_USER}/castai-jvm-probe-controller:${VERSION}"
echo "  - ${DOCKER_USER}/castai-tsc-controller:${VERSION}"
