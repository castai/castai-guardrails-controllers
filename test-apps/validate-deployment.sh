#!/bin/bash
# Validation script for TSC and JVM Probe Controllers
# Shows before/after state comparison

set -euo pipefail

NAMESPACE=${NAMESPACE:-default}

echo "╔══════════════════════════════════════════════════════════════════════════╗"
echo "║      CAST AI CONTROLLER TEST VALIDATION                                  ║"
echo "╚══════════════════════════════════════════════════════════════════════════╝"

# Check if jq is installed
if ! command -v jq &> /dev/null; then
    echo "⚠️  Warning: jq not found. Install with: brew install jq (macOS) or apt-get install jq (Linux)"
fi

echo ""
echo "═══════════════════════════════════════════════════════════════════════════"
echo "  TSC CONTROLLER TESTS"
echo "═══════════════════════════════════════════════════════════════════════════"

echo ""
echo "Test 1: test-app-no-tsc (Expected: TSC ADDED by controller)"
echo "  - Replicas: 3 (>= 2, should get TSC)"
echo "  - Expected: topology kubernetes.io/zone maxSkew=1"
tsc=$(kubectl get deployment test-app-no-tsc -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.topologySpreadConstraints}' 2>/dev/null || echo "null")
if [ -n "$tsc" ] && [ "$tsc" != "null" ] && [ "$tsc" != "[]" ]; then
    echo "  ✅ TSC Present: $tsc"
else
    echo "  ❌ TSC Missing - Controller may not have processed yet"
fi

echo ""
echo "Test 2: test-sts-no-tsc (Expected: TSC ADDED by controller)"
tsc=$(kubectl get statefulset test-sts-no-tsc -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.topologySpreadConstraints}' 2>/dev/null || echo "null")
if [ -n "$tsc" ] && [ "$tsc" != "null" ] && [ "$tsc" != "[]" ]; then
    echo "  ✅ TSC Present: $tsc"
else
    echo "  ❌ TSC Missing - Controller may not have processed yet"
fi

echo ""
echo "Test 3: test-app-bypass-tsc (Expected: NO TSC - bypass annotation)"
ann=$(kubectl get deployment test-app-bypass-tsc -n "$NAMESPACE" -o jsonpath='{.metadata.annotations.workloads\.cast\.ai/tsc-bypass}' 2>/dev/null || echo "")
tsc=$(kubectl get deployment test-app-bypass-tsc -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.topologySpreadConstraints}' 2>/dev/null || echo "null")
echo "  - Bypass annotation: ${ann:-(not set)}"
if [ "$tsc" = "null" ] || [ "$tsc" = "" ] || [ "$tsc" = "[]" ]; then
    echo "  ✅ TSC Correctly Bypassed"
else
    echo "  ❌ TSC Unexpectedly Present - Bypass not working"
fi

echo ""
echo "Test 4: test-app-single-replica (Expected: NO TSC - replicas < 2)"
replicas=$(kubectl get deployment test-app-single-replica -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
tsc=$(kubectl get deployment test-app-single-replica -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.topologySpreadConstraints}' 2>/dev/null || echo "null")
echo "  - Replicas: $replicas (< 2, TSC should NOT be added)"
if [ "$tsc" = "null" ] || [ "$tsc" = "" ] || [ "$tsc" = "[]" ]; then
    echo "  ✅ No TSC as expected"
else
    echo "  ❌ TSC Unexpectedly Present"
fi

echo ""
echo "═══════════════════════════════════════════════════════════════════════════"
echo "  JVM PROBE CONTROLLER TESTS"
echo "═══════════════════════════════════════════════════════════════════════════"

check_probe() {
    local deployment=$1
    local expected_framework=$2
    local container=${3:-app}
    
    echo ""
    echo "Test: $deployment (Expected: $expected_framework probes)"
    
    # Get probe specs
    local live=$(kubectl get deployment "$deployment" -n "$NAMESPACE" -o jsonpath="{.spec.template.spec.containers[0].livenessProbe}" 2>/dev/null || echo "null")
    local ready=$(kubectl get deployment "$deployment" -n "$NAMESPACE" -o jsonpath="{.spec.template.spec.containers[0].readinessProbe}" 2>/dev/null || echo "null")
    local startup=$(kubectl get deployment "$deployment" -n "$NAMESPACE" -o jsonpath="{.spec.template.spec.containers[0].startupProbe}" 2>/dev/null || echo "null")
    local bypass=$(kubectl get deployment "$deployment" -n "$NAMESPACE" -o jsonpath='{.metadata.annotations.workloads\.cast\.ai/jvm-probe-bypass}' 2>/dev/null || echo "")
    
    if [ -n "$bypass" ]; then
        echo "  - Bypass annotation: $bypass"
    fi
    
    # Check liveness
    if [ -n "$live" ] && [ "$live" != "null" ]; then
        local live_path=$(echo "$live" | jq -r '.httpGet.path // .tcpSocket.port // "tcp-socket"' 2>/dev/null || echo "present")
        echo "  ✅ Liveness: $live_path"
    else
        echo "  ❌ Liveness: missing"
    fi
    
    # Check readiness
    if [ -n "$ready" ] && [ "$ready" != "null" ]; then
        local ready_path=$(echo "$ready" | jq -r '.httpGet.path // .tcpSocket.port // "tcp-socket"' 2>/dev/null || echo "present")
        echo "  ✅ Readiness: $ready_path"
    else
        echo "  ❌ Readiness: missing"
    fi
    
    # Check startup (always expected for JVM)
    if [ -n "$startup" ] && [ "$startup" != "null" ]; then
        local startup_path=$(echo "$startup" | jq -r '.httpGet.path // .tcpSocket.port // "tcp-socket"' 2>/dev/null || echo "present")
        echo "  ✅ Startup: $startup_path (injected for JVM)"
    else
        if [ -n "$bypass" ]; then
            echo "  ⚠️  Startup: bypassed (correct)"
        else
            echo "  ❌ Startup: missing - should be injected for JVM"
        fi
    fi
}

check_probe "test-jvm-spring" "Spring Boot (/actuator/health/*)"
check_probe "test-jvm-quarkus" "Quarkus (/q/health/*)"
check_probe "test-jvm-generic" "Generic JVM (TCP socket)"
check_probe "test-jvm-bypass" "BYPASSED (no probes)"

echo ""
echo "═══════════════════════════════════════════════════════════════════════════"
echo "  AUTO-FIX TEST (Poor Probes)"
echo "═══════════════════════════════════════════════════════════════════════════"

echo ""
echo "Test: test-jvm-poor-probes (Expected: Probes FIXED by controller)"
echo "  Original config:"
echo "    - Liveness delay: 5s (TOO SHORT for JVM)"
echo "    - Period: 3s (TOO FREQUENT)"
echo "    - FailureThreshold: 1 (TOO AGGRESSIVE)"
echo ""

live=$(kubectl get deployment test-jvm-poor-probes -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].livenessProbe}' 2>/dev/null || echo "null")
if [ -n "$live" ] && [ "$live" != "null" ]; then
    echo "  Current Liveness:"
    echo "$live" | jq -C '{initialDelaySeconds, periodSeconds, timeoutSeconds, failureThreshold, successThreshold}' 2>/dev/null || echo "    (raw: $live)"
    
    delay=$(echo "$live" | jq -r '.initialDelaySeconds // 0' 2>/dev/null || echo "0")
    if [ "$delay" -gt 10 ]; then
        echo "  ✅ Initial delay improved ($delay > 10)"
    else
        echo "  ⚠️  Initial delay still low ($delay) - auto-fix may not have triggered"
    fi
fi

echo ""
echo "═══════════════════════════════════════════════════════════════════════════"
echo "  SUMMARY"
echo "═══════════════════════════════════════════════════════════════════════════"

# Count successes
total_tsc_expected=2  # deployments with >= 2 replicas and no bypass
total_probe_expected=3  # JVM apps without bypass

# Quick summary
kubectl get deployments -n "$NAMESPACE" -o custom-columns='NAME:.metadata.name,REPLICAS:.spec.replicas,TSC:.spec.template.spec.topologySpreadConstraints' --no-headers 2>/dev/null | \
    while read name replicas tsc; do
        if [ "$replicas" -ge 2 ] && [[ "$name" != *"bypass"* ]] && [[ "$name" != *"single"* ]]; then
            if [ -n "$tsc" ] && [ "$tsc" != "<none>" ]; then
                echo "✅ $name: TSC present"
            else
                echo "⏳ $name: TSC pending"
            fi
        fi
    done || true

echo ""
echo "To see detailed changes, run:"
echo "  kubectl get events --all-namespaces --field-selector reason=TSCAdded"
echo "  kubectl get events --all-namespaces --field-selector reason=ProbesAdded"
echo "  kubectl logs -n castai-agent -l app.kubernetes.io/component=tsc-controller"
echo "  kubectl logs -n castai-agent -l app.kubernetes.io/component=jvm-probe-controller"
echo ""
echo "To watch live: ./test-apps/watch-cluster.sh"
