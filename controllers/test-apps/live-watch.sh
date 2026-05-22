#!/bin/bash
# Enhanced Live Cluster Monitor with Before/After Comparison
# Usage: ./live-watch.sh [watch|snapshot]

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
PURPLE='\033[0;35m'
BOLD='\033[1m'
NC='\033[0m'

WATCH_INTERVAL=${WATCH_INTERVAL:-3}
NAMESPACE=${NAMESPACE:-default}

# Print header
print_header() {
    clear
    echo -e "${BOLD}${CYAN}"
    cat << 'BANNER'
╔══════════════════════════════════════════════════════════════════════════════════╗
║              🔴 LIVE CLUSTER MONITOR - MULTI-REPLICA TEST APPS                   ║
║                                                                                  ║
║  Watching: Pods | Nodes | TSC Status | Probe Status | Node Distribution         ║
╚══════════════════════════════════════════════════════════════════════════════════╝
BANNER
    echo -e "${NC}"
    echo -e "${YELLOW}⏱️  Last Updated: $(date '+%Y-%m-%d %H:%M:%S')${NC}"
    echo -e "${YELLOW}📍 Namespace: ${NAMESPACE} | Refresh: ${WATCH_INTERVAL}s${NC}\n"
}

# Pods table
show_pods() {
    echo -e "${BOLD}${BLUE}═══ PODS STATUS ═══${NC}\n"
    
    printf "  ${BOLD}%-45s %-7s %-15s %-30s${NC}\n" "NAME" "READY" "STATUS" "NODE"
    echo "  $(printf '%.0s-' {1..100})"
    
    kubectl get pods -n "$NAMESPACE" -o wide 2>/dev/null | tail -n +2 | while read -r line; do
        name=$(echo "$line" | awk '{print $1}')
        ready=$(echo "$line" | awk '{print $2}')
        status=$(echo "$line" | awk '{print $3}')
        node=$(echo "$line" | awk '{print $7}')
        
        if [ "$status" = "Running" ] && [ "$ready" = "1/1" ]; then
            printf "  %-45s ${GREEN}%-7s${NC} ${GREEN}%-15s${NC} %-30s\n" "$name" "$ready" "$status" "$node"
        elif [ "$status" = "Pending" ]; then
            printf "  %-45s ${YELLOW}%-7s${NC} ${YELLOW}%-15s${NC} %-30s\n" "$name" "$ready" "$status" "$node"
        else
            printf "  %-45s ${RED}%-7s${NC} ${RED}%-15s${NC} %-30s\n" "$name" "$ready" "$status" "$node"
        fi
    done
    
    local total=$(kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    local running=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l | tr -d ' ')
    local pending=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Pending --no-headers 2>/dev/null | wc -l | tr -d ' ')
    echo -e "\n  ${CYAN}Total: $total${NC} | ${GREEN}Running: $running${NC} | ${YELLOW}Pending: $pending${NC}"
}

# Node distribution per app
show_node_distribution() {
    echo -e "\n${BOLD}${BLUE}═══ NODE DISTRIBUTION PER APP ═══${NC}\n"
    
    printf "  ${BOLD}%-30s %-10s %-30s${NC}\n" "APP NAME" "REPLICAS" "NODE SPREAD"
    echo "  $(printf '%.0s-' {1..75})"
    
    for app in test-app-no-tsc test-sts-no-tsc test-app-bypass-tsc test-app-single-replica; do
        local count=$(kubectl get pods -n "$NAMESPACE" -l app="$app" --no-headers 2>/dev/null | wc -l | tr -d ' ')
        if [ "$count" -eq 0 ]; then continue; fi
        
        local nodes=$(kubectl get pods -n "$NAMESPACE" -l app="$app" -o jsonpath='{.items[*].spec.nodeName}' 2>/dev/null | tr ' ' '\n' | sort -u | wc -l | tr -d ' ')
        local node_names=$(kubectl get pods -n "$NAMESPACE" -l app="$app" -o jsonpath='{.items[*].spec.nodeName}' 2>/dev/null | tr ' ' '\n' | sort -u | tr '\n' ',' | sed 's/,$//')
        
        if [ "$nodes" -gt 1 ]; then
            printf "  ${CYAN}%-30s${NC} ${GREEN}%-10s${NC} ${GREEN}%s nodes${NC}: %s\n" "$app" "$count" "$nodes" "$node_names"
        else
            printf "  ${CYAN}%-30s${NC} ${YELLOW}%-10s${NC} ${RED}%s node${NC}: %s\n" "$app" "$count" "$nodes" "$node_names"
        fi
    done
}

# TSC status
show_tsc_status() {
    echo -e "\n${BOLD}${BLUE}═══ TOPOLOGY SPREAD CONSTRAINTS (TSC) STATUS ═══${NC}\n"
    
    printf "  ${BOLD}%-35s %-10s %-25s${NC}\n" "DEPLOYMENT/STATEFULSET" "REPLICAS" "TSC STATUS"
    echo "  $(printf '%.0s-' {1..75})"
    
    for deploy in test-app-no-tsc test-app-bypass-tsc test-app-single-replica; do
        local replicas=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
        local tsc=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.topologySpreadConstraints}' 2>/dev/null)
        
        if [ -n "$tsc" ] && [ "$tsc" != "[]" ]; then
            local topology=$(echo "$tsc" | jq -r '.[0].topologyKey // "unknown"' 2>/dev/null)
            printf "  ${CYAN}%-35s${NC} ${YELLOW}%-10s${NC} ${GREEN}✓ PRESENT${NC} (${topology})\n" "$deploy" "$replicas"
        else
            if [ "$deploy" == "test-app-bypass-tsc" ] || [ "$deploy" == "test-app-single-replica" ]; then
                printf "  ${CYAN}%-35s${NC} ${YELLOW}%-10s${NC} ${YELLOW}⊘ EXPECTED MISSING${NC}\n" "$deploy" "$replicas"
            else
                printf "  ${CYAN}%-35s${NC} ${YELLOW}%-10s${NC} ${RED}✗ MISSING${NC} (needs injection)\n" "$deploy" "$replicas"
            fi
        fi
    done
    
    # Check StatefulSet
    local sts_replicas=$(kubectl get statefulset test-sts-no-tsc -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
    local sts_tsc=$(kubectl get statefulset test-sts-no-tsc -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.topologySpreadConstraints}' 2>/dev/null)
    if [ -n "$sts_tsc" ] && [ "$sts_tsc" != "[]" ]; then
        printf "  ${CYAN}%-35s${NC} ${YELLOW}%-10s${NC} ${GREEN}✓ PRESENT${NC}\n" "test-sts-no-tsc" "$sts_replicas"
    else
        printf "  ${CYAN}%-35s${NC} ${YELLOW}%-10s${NC} ${RED}✗ MISSING${NC} (needs injection)\n" "test-sts-no-tsc" "$sts_replicas"
    fi
}

# JVM Probe status
show_jvm_probes() {
    echo -e "\n${BOLD}${BLUE}═══ JVM PROBES STATUS ═══${NC}\n"
    
    printf "  ${BOLD}%-35s %-12s %-12s %-12s${NC}\n" "DEPLOYMENT" "LIVENESS" "READINESS" "STARTUP"
    echo "  $(printf '%.0s-' {1..75})"
    
    for deploy in test-jvm-spring test-jvm-quarkus test-jvm-generic test-jvm-bypass test-jvm-poor-probes test-jvm-overwrite; do
        local containers=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[*].name}' 2>/dev/null || true)
        if [ -z "$containers" ]; then continue; fi
        
        for container in $containers; do
            local has_live=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath="{.spec.template.spec.containers[?(@.name=='$container')].livenessProbe}" 2>/dev/null)
            local has_ready=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath="{.spec.template.spec.containers[?(@.name=='$container')].readinessProbe}" 2>/dev/null)
            local has_startup=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath="{.spec.template.spec.containers[?(@.name=='$container')].startupProbe}" 2>/dev/null)
            
            local live_icon="${RED}✗${NC}"
            local ready_icon="${RED}✗${NC}"
            local startup_icon="${RED}✗${NC}"
            
            [ -n "$has_live" ] && live_icon="${GREEN}✓${NC}"
            [ -n "$has_ready" ] && ready_icon="${GREEN}✓${NC}"
            [ -n "$has_startup" ] && startup_icon="${GREEN}✓${NC}"
            
            if [ "$deploy" == "test-jvm-bypass" ]; then
                printf "  ${CYAN}%-35s${NC} ${YELLOW}%-12s${NC} ${YELLOW}%-12s${NC} ${YELLOW}%-12s${NC}\n" "$deploy/$container" "⊘ bypass" "⊘ bypass" "⊘ bypass"
            else
                printf "  ${CYAN}%-35s${NC} %b %-10s %b %-10s %b %-10s\n" "$deploy/$container" "$live_icon" "" "$ready_icon" "" "$startup_icon" ""
            fi
        done
    done
}

# Cluster nodes
show_nodes() {
    echo -e "\n${BOLD}${BLUE}═══ CLUSTER NODES ═══${NC}\n"
    kubectl get nodes -o wide 2>/dev/null | awk 'NR==1 || /^ip-/' | while read -r line; do
        if echo "$line" | grep -q "NAME"; then
            printf "  ${BOLD}%-45s %-10s %-15s${NC}\n" "NODE NAME" "STATUS" "ZONE"
            echo "  $(printf '%.0s-' {1..75})"
        else
            name=$(echo "$line" | awk '{print $1}')
            status=$(echo "$line" | awk '{print $2}')
            zone=$(echo "$line" | grep -o 'us-east-[0-9][a-z]' || echo "N/A")
            if [ "$status" = "Ready" ]; then
                printf "  %-45s ${GREEN}%-10s${NC} ${CYAN}%-15s${NC}\n" "$name" "$status" "$zone"
            else
                printf "  %-45s ${YELLOW}%-10s${NC} ${CYAN}%-15s${NC}\n" "$name" "$status" "$zone"
            fi
        fi
    done
}

# Watch loop
watch_loop() {
    while true; do
        print_header
        show_pods
        show_node_distribution
        show_tsc_status
        show_jvm_probes
        show_nodes
        
        echo -e "\n${PURPLE}══════════════════════════════════════════════════════════════════════════════════${NC}"
        echo -e "${YELLOW}Press Ctrl+C to exit${NC}"
        sleep "$WATCH_INTERVAL"
    done
}

# Main
case "${1:-watch}" in
    watch)
        echo "Starting live watch mode (Ctrl+C to exit)..."
        sleep 1
        watch_loop
        ;;
    snapshot)
        print_header
        show_pods
        show_node_distribution
        show_tsc_status
        show_jvm_probes
        show_nodes
        ;;
    *)
        echo "Usage: $0 [watch|snapshot]"
        echo ""
        echo "Commands:"
        echo "  watch      - Continuously monitor the cluster (default)"
        echo "  snapshot   - Take a single snapshot"
        echo ""
        echo "Environment Variables:"
        echo "  NAMESPACE       - Target namespace (default: default)"
        echo "  WATCH_INTERVAL  - Refresh interval in seconds (default: 3)"
        exit 1
        ;;
esac
