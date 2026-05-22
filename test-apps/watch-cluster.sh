#!/bin/bash
# Live Cluster Watch Tool for TSC/JVM Controller Testing
# Shows pods and their node distribution in real-time

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color
BOLD='\033[1m'

# Configuration
WATCH_INTERVAL=${WATCH_INTERVAL:-2}
NAMESPACE=${NAMESPACE:-default}
CLEAR_SCREEN=${CLEAR_SCREEN:-true}

# Header
print_header() {
    echo -e "${BOLD}${CYAN}"
    cat << 'EOF'
╔══════════════════════════════════════════════════════════════════════════════╗
║           CAST AI CONTROLLER TEST - LIVE CLUSTER WATCH                       ║
║                                                                              ║
║  Watching: Pods, Nodes, Topology Spread Constraints, Probes                  ║
╚══════════════════════════════════════════════════════════════════════════════╝
EOF
    echo -e "${NC}"
}

# Get current timestamp
get_timestamp() {
    date '+%Y-%m-%d %H:%M:%S'
}

# Print section header
section() {
    echo -e "\n${BOLD}${BLUE}═══ $1 ═══${NC}"
}

# Get pods with detailed info
show_pods() {
    section "PODS in namespace: ${NAMESPACE}"
    
    kubectl get pods -n "$NAMESPACE" -o wide 2>/dev/null | \
        awk -v green="$GREEN" -v red="$RED" -v yellow="$YELLOW" -v nc="$NC" '
    NR==1 { print }
    NR>1 {
        status = $3
        color = nc
        if (status == "Running") color = green
        else if (status == "Pending") color = yellow
        else if (status == "CrashLoopBackOff" || status == "Error") color = red
        
        # Color the status column
        for (i=1; i<=NF; i++) {
            if (i == 3) printf "%s%s%s ", color, $i, nc
            else printf "%s ", $i
        }
        print ""
    }'
}

# Show node distribution for a deployment
show_node_distribution() {
    local name=$1
    local kind=$2
    
    echo -e "\n${YELLOW}Node Distribution for ${kind}/${name}:${NC}"
    
    kubectl get pods -n "$NAMESPACE" -l "app=${name}" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | \
        sort | uniq -c | sort -rn | \
        awk -v cyan="$CYAN" -v nc="$NC" '{ printf "  %s%-3s pods%s on node: %s\n", cyan, $1, nc, $2 }'
}

# Check if pod has TopologySpreadConstraints
show_tsc_info() {
    section "TOPOLOGY SPREAD CONSTRAINTS (TSC)"
    
    # Shows workloads and their TSC status
    echo -e "\n${BOLD}Deployments:${NC}"
    kubectl get deployments -n "$NAMESPACE" -o json 2>/dev/null | \
    jq -r '.items[] | 
        select(.spec.replicas >= 2) |
        "\(.metadata.name): replicas=\(.spec.replicas) | TSC=" + 
        if (.spec.template.spec.topologySpreadConstraints // [] | length) > 0 then 
            "✓ PRESENT"
        else 
            "✗ MISSING"
        end
    ' 2>/dev/null || echo "  jq required for detailed TSC view"
    
    # Show actual TSC values
    for deploy in $(kubectl get deployments -n "$NAMESPACE" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
        local tsc=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.topologySpreadConstraints}' 2>/dev/null)
        if [ -n "$tsc" ] && [ "$tsc" != "[]" ]; then
            echo -e "\n${GREEN}${deploy}:${NC}"
            echo "$tsc" | jq -C '.[] | {maxSkew, topologyKey, whenUnsatisfiable}' 2>/dev/null || echo "  $tsc"
        fi
    done
    
    echo -e "\n${BOLD}StatefulSets:${NC}"
    kubectl get statefulsets -n "$NAMESPACE" -o json 2>/dev/null | \
    jq -r '.items[] | 
        select(.spec.replicas >= 2) |
        "\(.metadata.name): replicas=\(.spec.replicas) | TSC=" + 
        if (.spec.template.spec.topologySpreadConstraints // [] | length) > 0 then 
            "✓ PRESENT"
        else 
            "✗ MISSING"
        end
    ' 2>/dev/null || true
}

# Show probe information
show_probe_info() {
    section "CONTAINER PROBES (JVM Controller)"
    
    kubectl get deployments -n "$NAMESPACE" -o json 2>/dev/null | \
    jq -r '.items[] | 
        .metadata.name as $deploy |
        .spec.template.spec.containers[] | 
        select(.image | test("(java|jdk|jre|openjdk|spring|quarkus|micronaut)"; "i")) |
        "\($deploy)/\(.name):" +
        "  Live=\(.livenessProbe // "✗")" +
        "  Ready=\(.readinessProbe // "✗")" +
        "  Startup=\(.startupProbe // "✗")"
    ' 2>/dev/null | \
    while read -r line; do
        if [[ $line == *"/"*":" ]]; then
            echo -e "\n${CYAN}${line}${NC}"
        else
            # Colorize probe status
            line=$(echo "$line" | sed "s/✗/${RED}✗${NC}/g" | sed "s/Live=/\n  Live=/g" | sed "s/Ready=/\n  Ready=/g" | sed "s/Startup=/\n  Startup=/g")
            echo -e "$line"
        fi
    done || echo "  jq required for detailed probe view"
    
    # Show probe details for each container
    for deploy in $(kubectl get deployments -n "$NAMESPACE" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
        local containers=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[*].name}' 2>/dev/null)
        for container in $containers; do
            local has_probe=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath="{.spec.template.spec.containers[?(@.name=='$container')].livenessProbe}" 2>/dev/null)
            if [ -n "$has_probe" ]; then
                echo -e "\n${GREEN}${deploy}/${container} probes:${NC}"
                local live=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath="{.spec.template.spec.containers[?(@.name=='$container')].livenessProbe}" 2>/dev/null | jq -C '{initialDelaySeconds, periodSeconds, timeoutSeconds, failureThreshold}' 2>/dev/null)
                local ready=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath="{.spec.template.spec.containers[?(@.name=='$container')].readinessProbe}" 2>/dev/null | jq -C '{initialDelaySeconds, periodSeconds, timeoutSeconds, failureThreshold}' 2>/dev/null)
                local startup=$(kubectl get deployment "$deploy" -n "$NAMESPACE" -o jsonpath="{.spec.template.spec.containers[?(@.name=='$container')].startupProbe}" 2>/dev/null | jq -C '{initialDelaySeconds, periodSeconds, timeoutSeconds}' 2>/dev/null)
                
                [ -n "$live" ] && echo -e "  Liveness: $live"
                [ -n "$ready" ] && echo -e "  Readiness: $ready"
                [ -n "$startup" ] && [ "$startup" != "null" ] && echo -e "  Startup: $startup"
            fi
        done
    done
}

# Show node topology info
show_nodes() {
    section "CLUSTER NODES & TOPOLOGY"
    
    echo -e "\n${BOLD}Node List:${NC}"
    kubectl get nodes -o wide 2>/dev/null
    
    echo -e "\n${BOLD}Node Labels (zone/region):${NC}"
    kubectl get nodes -o json 2>/dev/null | \
    jq -r '.items[] | 
        "\(.metadata.name):" +
        " zone=\(.metadata.labels."topology.kubernetes.io/zone" // "N/A")" +
        " region=\(.metadata.labels."topology.kubernetes.io/region" // "N/A")" +
        " instance=\(.metadata.labels."node.kubernetes.io/instance-type" // "N/A")"
    ' 2>/dev/null | column -t || true
}

# Show controller events
show_controller_events() {
    section "CONTROLLER EVENTS"
    
    echo -e "${BOLD}Recent TSC Controller Events:${NC}"
    kubectl get events --all-namespaces --field-selector reason=TSCAdded 2>/dev/null | tail -5 || echo "  No TSC events found"
    
    echo -e "\n${BOLD}Recent JVM Probe Controller Events:${NC}"
    kubectl get events --all-namespaces --field-selector reason=ProbesAdded 2>/dev/null | tail -5 || echo "  No probe events found"
    
    echo -e "\n${BOLD}Recent Probe Fix Events:${NC}"
    kubectl get events --all-namespaces --field-selector reason=ProbeAutoFixed 2>/dev/null | tail -5 || echo "  No probe fix events found"
}

# Summary counters
show_summary() {
    section "SUMMARY"
    
    local total_pods=$(kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    local running_pods=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l | tr -d ' ')
    local pending_pods=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Pending --no-headers 2>/dev/null | wc -l | tr -d ' ')
    
    local total_deploys=$(kubectl get deployments -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    local with_tsc=$(kubectl get deployments -n "$NAMESPACE" -o json 2>/dev/null | \
        jq '[.items[] | select(.spec.template.spec.topologySpreadConstraints // [] | length > 0)] | length' 2>/dev/null || echo "0")
    
    echo -e "  ${CYAN}Total Pods:${NC} $total_pods  ${GREEN}(Running: $running_pods)${NC}  ${YELLOW}(Pending: $pending_pods)${NC}"
    echo -e "  ${CYAN}Total Deployments:${NC} $total_deploys"
    echo -e "  ${GREEN}With TSC:${NC} $with_tsc"
}

# Main watch loop
watch_loop() {
    while true; do
        if [ "$CLEAR_SCREEN" = "true" ]; then
            clear
        fi
        
        print_header
        echo -e "${YELLOW}Last Updated: $(get_timestamp)${NC}"
        echo -e "${YELLOW}Namespace: ${NAMESPACE} | Refresh: ${WATCH_INTERVAL}s${NC}"
        
        show_pods
        show_tsc_info
        show_probe_info
        show_nodes
        show_controller_events
        show_summary
        
        echo -e "\n${BOLD}Press Ctrl+C to exit${NC}"
        sleep "$WATCH_INTERVAL"
    done
}

# Single snapshot mode
snapshot() {
    print_header
    show_pods
    show_tsc_info
    show_probe_info
    show_nodes
    show_controller_events
    show_summary
}

# Usage
usage() {
    echo "Usage: $0 [OPTIONS] [COMMAND]"
    echo ""
    echo "Commands:"
    echo "  watch       Continuously watch cluster (default)"
    echo "  snapshot    Take a single snapshot"
    echo ""
    echo "Options:"
    echo "  -n, --namespace NAMESPACE   Target namespace (default: default)"
    echo "  -i, --interval SECONDS      Refresh interval (default: 2)"
    echo "  -h, --help                  Show this help"
    echo ""
    echo "Environment Variables:"
    echo "  NAMESPACE                   Target namespace"
    echo "  WATCH_INTERVAL              Refresh interval in seconds"
    echo ""
    echo "Examples:"
    echo "  $0                          # Watch default namespace"
    echo "  $0 watch                    # Watch continuously"
    echo "  $0 snapshot                 # Single snapshot"
    echo "  $0 -n production watch      # Watch production namespace"
    echo "  NAMESPACE=dev $0            # Watch dev namespace"
}

# Parse arguments
COMMAND="watch"
while [[ $# -gt 0 ]]; do
    case $1 in
        watch|snapshot)
            COMMAND="$1"
            shift
            ;;
        -n|--namespace)
            NAMESPACE="$2"
            shift 2
            ;;
        -i|--interval)
            WATCH_INTERVAL="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            usage
            exit 1
            ;;
    esac
done

# Execute command
case $COMMAND in
    watch)
        echo "Starting watch mode (Ctrl+C to exit)..."
        watch_loop
        ;;
    snapshot)
        snapshot
        ;;
    *)
        echo "Unknown command: $COMMAND"
        usage
        exit 1
        ;;
esac
