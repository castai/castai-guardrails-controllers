#!/bin/bash
# Live Pod Spread Watch - Shows pod distribution across nodes and AZs
# Works with bash 3.2+ (macOS default)

set -e

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BLUE='\033[0;34m'
GRAY='\033[0;90m'
NC='\033[0m'
BOLD='\033[1m'

WATCH_INTERVAL=${WATCH_INTERVAL:-3}
NAMESPACE=${NAMESPACE:-default}

print_header() {
    clear
    echo -e "${BOLD}${CYAN}"
    echo "╔════════════════════════════════════════════════════════════════════════════════╗"
    echo "║                    POD SPREAD ANALYZER - LIVE WATCH                            ║"
    echo "║                                                                                ║"
    echo "║  Monitors: Node distribution, AZ spread, TSC effectiveness                     ║"
    echo "╚════════════════════════════════════════════════════════════════════════════════╝"
    echo -e "${NC}"
    echo -e "${GRAY}Timestamp: $(date '+%Y-%m-%d %H:%M:%S') | Namespace: $NAMESPACE${NC}"
    echo ""
}

print_nodes() {
    echo -e "${BOLD}CLUSTER NODES${NC}"
    echo "───────────────────────────────────────────────────────────────────────────────"
    printf "%-40s %-15s %-15s %s\n" "Node Name" "Zone" "Region" "Status"
    echo "───────────────────────────────────────────────────────────────────────────────"
    
    kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.labels.topology\.kubernetes\.io/zone}{"|"}{.metadata.labels.topology\.kubernetes\.io/region}{"|"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | while IFS='|' read -r node zone region status; do
        status_emoji="✓"
        if [ "$status" != "True" ]; then
            status_emoji="✗"
        fi
        
        # Color zone
        zone_colored="$zone"
        case "$zone" in
            *a*) zone_colored="${GREEN}$zone${NC}" ;;
            *b*) zone_colored="${YELLOW}$zone${NC}" ;;
            *c*) zone_colored="${CYAN}$zone${NC}" ;;
        esac
        
        node_short=$(echo "$node" | cut -c1-40)
        printf "%-40s %-15b %-15s %s\n" "$node_short" "$zone_colored" "${region:-N/A}" "$status_emoji"
    done
    echo ""
}

print_zone_summary() {
    echo -e "${BOLD}ZONE DISTRIBUTION SUMMARY${NC}"
    echo "───────────────────────────────────────────────────────────────────────────────"
    
    # Use temp files for counting
    TMPDIR=$(mktemp -d)
    
    # Build node->zone mapping and count
    kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' 2>/dev/null > "$TMPDIR/node_zones.txt"
    
    # Count pods per zone
    total_pods=0
    kubectl get pods -n "$NAMESPACE" -o jsonpath='{.items[*].spec.nodeName}' 2>/dev/null | tr ' ' '\n' | grep -v "^$" | while read -r node; do
        [ -z "$node" ] && continue
        zone=$(grep "^${node}|" "$TMPDIR/node_zones.txt" | cut -d'|' -f2)
        [ -z "$zone" ] && zone="unknown"
        echo "$zone" >> "$TMPDIR/zones.txt"
    done
    
    # Count and display
    if [ -f "$TMPDIR/zones.txt" ]; then
        total_pods=$(wc -l < "$TMPDIR/zones.txt" | tr -d ' ')
        sort "$TMPDIR/zones.txt" | uniq -c | sort -rn | while read -r count zone; do
            zone=$(echo "$zone" | tr -d ' ')
            
            # Color zone
            zone_colored="$zone"
            case "$zone" in
                *a*) zone_colored="${GREEN}$zone${NC}" ;;
                *b*) zone_colored="${YELLOW}$zone${NC}" ;;
                *c*) zone_colored="${CYAN}$zone${NC}" ;;
            esac
            
            # Bar chart
            bar=""
            i=0
            while [ $i -lt $count ] && [ $i -lt 40 ]; do
                bar="${bar}█"
                i=$((i + 1))
            done
            
            # Percentage
            if [ $total_pods -gt 0 ]; then
                percent=$((count * 100 / total_pods))
            else
                percent=0
            fi
            
            echo -e "${zone_colored}: Pods: ${BOLD}${count}${NC}/${total_pods} ${bar} ${percent}%"
        done
    fi
    
    rm -rf "$TMPDIR"
    echo ""
}

print_deployment_spread() {
    echo -e "${BOLD}DEPLOYMENT POD DISTRIBUTION${NC}"
    echo "─────────────────────────────────────────────────────────────────────────────────"
    printf "%-35s %8s %8s %8s %-30s\n" "Workload" "Replicas" "Nodes" "Zones" "Distribution"
    echo "─────────────────────────────────────────────────────────────────────────────────"
    
    TMPDIR=$(mktemp -d)
    
    # Build node->zone mapping
    kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' 2>/dev/null > "$TMPDIR/node_zones.txt"
    
    kubectl get deployments -n "$NAMESPACE" -o json 2>/dev/null | jq -r '.items[] | "\(.metadata.name)|\(.spec.replicas)"' 2>/dev/null | while IFS='|' read -r name replicas; do
        [ -z "$name" ] && continue
        
        # Get pod distribution
        kubectl get pods -n "$NAMESPACE" -l "app=$name" -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.spec.nodeName}{"\n"}{end}' 2>/dev/null > "$TMPDIR/pods.txt"
        
        # Count per node
        cut -d'|' -f2 "$TMPDIR/pods.txt" | sort | uniq -c > "$TMPDIR/node_counts.txt"
        
        unique_nodes=$(wc -l < "$TMPDIR/node_counts.txt" | tr -d ' ')
        [ "$unique_nodes" -eq 0 ] && continue
        
        # Count zones
        cut -d'|' -f2 "$TMPDIR/pods.txt" | while read -r node; do
            grep "^${node}|" "$TMPDIR/node_zones.txt" 2>/dev/null | cut -d'|' -f2
        done | sort -u > "$TMPDIR/used_zones.txt"
        unique_zones=$(wc -l < "$TMPDIR/used_zones.txt" | tr -d ' ')
        
        # Build distribution string
        dist_str=""
        while read -r count node; do
            node_short=$(echo "$node" | cut -d'.' -f1)
            zone=$(grep "^${node}|" "$TMPDIR/node_zones.txt" 2>/dev/null | cut -d'|' -f2 | grep -o '[abc]$' || echo "?")
            
            if [ -n "$dist_str" ]; then dist_str="$dist_str, "; fi
            dist_str="${dist_str}${node_short}:${count}"
        done < "$TMPDIR/node_counts.txt"
        
        printf "%-35s %8s %8s %8s %-30s\n" "${name:0:35}" "$replicas" "$unique_nodes" "$unique_zones" "${dist_str:0:30}"
    done
    
    rm -rf "$TMPDIR"
    echo ""
}

print_pods() {
    echo -e "${BOLD}ALL PODS - NODE & ZONE${NC}"
    echo "─────────────────────────────────────────────────────────────────────────────────────────"
    printf "%-45s %-25s %-35s %-15s\n" "Pod Name" "Workload" "Node" "Zone"
    echo "─────────────────────────────────────────────────────────────────────────────────────────"
    
    TMPDIR=$(mktemp -d)
    
    # Build node->zone mapping
    kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' 2>/dev/null > "$TMPDIR/node_zones.txt"
    
    kubectl get pods -n "$NAMESPACE" -o json 2>/dev/null | jq -r '.items[] | "\(.metadata.name)|\(.metadata.labels.app // "unknown")|\(.spec.nodeName // "Pending")"' 2>/dev/null | while IFS='|' read -r pod app node; do
        [ -z "$pod" ] && continue
        
        if [ -z "$node" ] || [ "$node" = "Pending" ]; then
            zone="${GRAY}Pending${NC}"
            node_display="${YELLOW}Scheduling...${NC}"
        else
            zone=$(grep "^${node}|" "$TMPDIR/node_zones.txt" 2>/dev/null | cut -d'|' -f2)
            zone_colored="$zone"
            case "$zone" in
                *a*) zone_colored="${GREEN}$zone${NC}" ;;
                *b*) zone_colored="${YELLOW}$zone${NC}" ;;
                *c*) zone_colored="${CYAN}$zone${NC}" ;;
            esac
            zone="$zone_colored"
            node_display=$(echo "$node" | cut -d'.' -f1)
        fi
        
        printf "%-45s %-25s %-35s %-15b\n" "${pod:0:45}" "${app:0:25}" "$node_display" "$zone"
    done
    
    rm -rf "$TMPDIR"
    echo ""
}

print_pod_az_details() {
    echo -e "${BOLD}POD DETAILS BY AZ (Name, Zone, Count)${NC}"
    echo "─────────────────────────────────────────────────────────────────────────────────────────────"
    printf "%-45s %-20s %s\n" "Pod Name" "Zone (AZ)" "Count"
    echo "─────────────────────────────────────────────────────────────────────────────────────────────"
    
    TMPDIR=$(mktemp -d)
    
    # Build node->zone mapping
    kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' 2>/dev/null > "$TMPDIR/node_zones.txt"
    
    # Show each pod with its AZ
    kubectl get pods -n "$NAMESPACE" -o json 2>/dev/null | jq -r '.items[] | select(.status.phase=="Running") | "\(.metadata.name)|\(.spec.nodeName)"' 2>/dev/null | sort -t'|' -k2 | while IFS='|' read -r pod node; do
        [ -z "$pod" ] && continue
        
        zone=$(grep "^${node}|" "$TMPDIR/node_zones.txt" 2>/dev/null | cut -d'|' -f2)
        [ -z "$zone" ] && zone="unknown"
        
        # Color zone
        zone_colored="$zone"
        case "$zone" in
            *a*) zone_colored="${GREEN}$zone${NC}" ;;
            *b*) zone_colored="${YELLOW}$zone${NC}" ;;
            *c*) zone_colored="${CYAN}$zone${NC}" ;;
        esac
        
        # Count pods in this zone (for display)
        zone_count=$(grep "^${node}|" "$TMPDIR/node_zones.txt" 2>/dev/null | wc -l | tr -d ' ')
        
        printf "%-45s %-20b %s\n" "${pod:0:45}" "$zone_colored" "(zone: $zone_count)"
    done
    
    rm -rf "$TMPDIR"
    echo ""
    
    # Summary by AZ
    echo -e "${BOLD}AZ SUMMARY${NC}"
    echo "───────────────────────────────────────────────────────────────────────────────"
    
    TMPDIR=$(mktemp -d)
    kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' 2>/dev/null > "$TMPDIR/nodes.txt"
    
    kubectl get pods -n "$NAMESPACE" -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | while read -r node; do
        [ -z "$node" ] && continue
        grep "^${node}|" "$TMPDIR/nodes.txt" 2>/dev/null | cut -d'|' -f2
    done | sort | uniq -c | sort -rn | while read -r count zone; do
        zone=$(echo "$zone" | tr -d ' ')
        zone_colored="$zone"
        case "$zone" in
            *a*) zone_colored="${GREEN}$zone${NC}" ;;
            *b*) zone_colored="${YELLOW}$zone${NC}" ;;
            *c*) zone_colored="${CYAN}$zone${NC}" ;;
        esac
        printf "  %-20s %3d pods\n" "$zone_colored" "$count"
    done
    
    rm -rf "$TMPDIR"
    echo ""
}

print_tsc_status() {
    echo -e "${BOLD}TSC CONTROLLER STATUS${NC}"
    echo "───────────────────────────────────────────────────────────────────────────────"
    
    total=0
    with_tsc=0
    without_tsc=0
    
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        name=$(echo "$line" | cut -d'|' -f1)
        replicas=$(echo "$line" | cut -d'|' -f2)
        tsc=$(echo "$line" | cut -d'|' -f3)
        
        case "$replicas" in
            ''|*[!0-9]*) continue ;;
        esac
        
        total=$((total + 1))
        
        if [ -n "$tsc" ] && [ "$tsc" != "null" ] && [ "$tsc" != "" ]; then
            with_tsc=$((with_tsc + 1))
        else
            without_tsc=$((without_tsc + 1))
        fi
    done < <(kubectl get deployments -n "$NAMESPACE" -o json 2>/dev/null | jq -r '.items[] | "\(.metadata.name)|\(.spec.replicas)|\(.spec.template.spec.topologySpreadConstraints)"' 2>/dev/null)
    
    echo -e "Total Deployments: $total"
    echo -e "${GREEN}With TSC: $with_tsc ✓${NC}"
    echo -e "Waiting for TSC: $without_tsc"
    echo ""
}

watch_mode() {
    while true; do
        print_header
        print_nodes
        print_zone_summary
        print_deployment_spread
        print_pod_az_details
        print_pods
        print_tsc_status
        
        echo -e "${GRAY}Refreshing in ${WATCH_INTERVAL}s... (Ctrl+C to exit)${NC}"
        sleep "$WATCH_INTERVAL"
    done
}

snapshot() {
    print_header
    print_nodes
    print_zone_summary
    print_deployment_spread
    print_pod_az_details
    print_pods
    print_tsc_status
}

# Main
COMMAND=${1:-"watch"}

case $COMMAND in
    watch)
        watch_mode
        ;;
    snapshot)
        snapshot
        ;;
    *)
        echo "Usage: $0 [watch|snapshot]"
        exit 1
        ;;
esac
