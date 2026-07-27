#!/bin/bash
# ============================================================
# CAST AI Workload Controllers — Interactive Installer
# ------------------------------------------------------------
# Mirrors the Cast AI castctl / connect-and-enable-castai.sh UX:
#   - Pre-flights (kubectl, helm 3.14+, jq)
#   - Detects the current kubectl context / cluster name
#   - Asks which controllers to install
#   - Per-controller: image tag, replicas, dry-run mode
#   - Installs via Helm from local charts in ./controllers/
#   - Verifies rollout
#
# Interactive by default; non-interactive when env vars are preset
# (matches castctl --non-interactive).
#
# Usage:
#   ./install.sh
#
# Non-interactive example:
#   INSTALL_TSC=true \
#   INSTALL_JVM=true \
#   INSTALL_PDB=true \
#   TSC_DRY_RUN=true \
#   JVM_DRY_RUN=true \
#   PDB_DRY_RUN=true \
#   ./install.sh
# ============================================================

set -euo pipefail

# -------------------------
# Defaults (override via env)
# -------------------------
NAMESPACE="${NAMESPACE:-castai-agent}"

INSTALL_TSC="${INSTALL_TSC:-}"
INSTALL_JVM="${INSTALL_JVM:-}"
INSTALL_PDB="${INSTALL_PDB:-}"

TSC_IMAGE_TAG="${TSC_IMAGE_TAG:-0.0.2}"
JVM_IMAGE_TAG="${JVM_IMAGE_TAG:-0.0.2}"
PDB_IMAGE_TAG="${PDB_IMAGE_TAG:-0.5}"

TSC_REPLICAS="${TSC_REPLICAS:-2}"
JVM_REPLICAS="${JVM_REPLICAS:-2}"
PDB_REPLICAS="${PDB_REPLICAS:-2}"

TSC_DRY_RUN="${TSC_DRY_RUN:-true}"
JVM_DRY_RUN="${JVM_DRY_RUN:-true}"
PDB_DRY_RUN="${PDB_DRY_RUN:-true}"

# Auto-detected if empty
IMAGE_PULL_POLICY="${IMAGE_PULL_POLICY:-IfNotPresent}"

# -------------------------
# Helpers
# -------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

fatal() {
  echo -e "${RED}ERROR: $1${NC}" >&2
  exit 1
}

info() {
  echo -e "${BLUE}[INFO]${NC} $1"
}

ok() {
  echo -e "${GREEN}[OK]${NC} $1"
}

warn() {
  echo -e "${YELLOW}[WARN]${NC} $1"
}

prompt() {
  # prompt <question> <default_value>
  # Reads from stdin; uses read -r for safety.
  local q="$1"
  local default="$2"
  local reply
  read -r -p "$(echo -e "${BLUE}$q${NC} [$default]: ")" reply
  reply="${reply:-$default}"
  echo "$reply"
}

confirm() {
  # confirm <question> [default y/n]
  local q="$1"
  local default="${2:-y}"
  local reply
  read -r -p "$(echo -e "${YELLOW}$q${NC} [y/n, default $default]: ")" reply
  reply="${reply:-$default}"
  case "$reply" in
    [Yy]*) return 0 ;;
    *)     return 1 ;;
  esac
}

# -------------------------
# Pre-flight checks
# -------------------------
info "Running pre-flight checks..."

command -v kubectl >/dev/null 2>&1 || fatal "kubectl is not installed."
command -v helm    >/dev/null 2>&1 || fatal "helm is not installed."
command -v jq      >/dev/null 2>&1 || fatal "jq is not installed."

HELM_VERSION_OUTPUT="$(helm version --template "{{.Version}}")"
HELM_VERSION="${HELM_VERSION_OUTPUT#[vV]}"
HELM_VERSION_MAJOR="${HELM_VERSION%%.*}"
HELM_VERSION_MINOR_PATCH="${HELM_VERSION#*.}"
HELM_VERSION_MINOR="${HELM_VERSION_MINOR_PATCH%%.*}"

if [ "$HELM_VERSION_MAJOR" -lt 3 ] || { [ "$HELM_VERSION_MAJOR" -eq 3 ] && [ "$HELM_VERSION_MINOR" -lt 14 ]; }; then
  fatal "Helm 3.14.0+ required. Current version: ${HELM_VERSION_OUTPUT}"
fi

# Helm 4: server-side apply can cause issues — disable to match upstream script
HELM_SERVERSIDE_APPLY=""
HELM_ALL_STATES="--all"
if [ "$HELM_VERSION_MAJOR" -ge 4 ] 2>/dev/null; then
  HELM_SERVERSIDE_APPLY="--server-side=false"
  HELM_ALL_STATES=""
fi

ok "Pre-flight checks passed (helm ${HELM_VERSION_OUTPUT})."

# -------------------------
# Detect cluster context
# -------------------------
KCTL_CTX="$(kubectl config current-context 2>/dev/null || true)"
[ -z "$KCTL_CTX" ] && fatal "No kubectl context configured. Run 'kubectl config use-context <name>' first."

CLUSTER_NAME="$(kubectl config view --minify -o jsonpath='{.contexts[0].context.cluster}' 2>/dev/null || true)"
[ -z "$CLUSTER_NAME" ] && fatal "Could not resolve cluster name from kubecontext."

info "kubectl context : $KCTL_CTX"
info "Cluster name    : $CLUSTER_NAME"

# -------------------------
# Resolve script directory (for chart paths)
# -------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TSC_CHART="${SCRIPT_DIR}/controllers/tsc-controller/helm/castai-tsc-controller"
JVM_CHART="${SCRIPT_DIR}/controllers/jvm-probe-controller/helm/castai-jvm-probe-controller"
PDB_CHART="${SCRIPT_DIR}/controllers/pdb-controller/helm/castai-pdb-controller"

[ -d "$TSC_CHART" ] || fatal "TSC chart not found at $TSC_CHART"
[ -d "$JVM_CHART" ] || fatal "JVM chart not found at $JVM_CHART"
[ -d "$PDB_CHART" ] || fatal "PDB chart not found at $PDB_CHART"

# -------------------------
# Determine if interactive
# -------------------------
# Heuristic: if TTY and any INSTALL_* var is empty, go interactive.
IS_INTERACTIVE=true
if [ ! -t 0 ] || [ -n "${INSTALL_TSC}${INSTALL_JVM}${INSTALL_PDB}" ]; then
  IS_INTERACTIVE=false
fi

# -------------------------
# Controller selection menu
# -------------------------
if [ "$IS_INTERACTIVE" = true ]; then
  echo ""
  echo "============================================================"
  echo " CAST AI Workload Controllers — Install"
  echo "============================================================"
  echo ""
  echo "  [1] TSC Controller     — Topology Spread Constraints"
  echo "  [2] JVM Probe          — JVM health/startup/liveness probes"
  echo "  [3] PDB Controller     — Pod Disruption Budgets"
  echo "  [4] Install ALL"
  echo "  [5] Cancel"
  echo ""
  raw="$(prompt "Select controllers to install (e.g. '1 3', '4', or '5')" "")"

  case "$raw" in
    5|q|quit|cancel) fatal "Cancelled." ;;
    4|all|"") INSTALL_TSC=true; INSTALL_JVM=true; INSTALL_PDB=true ;;
    *)
      INSTALL_TSC=false
      INSTALL_JVM=false
      INSTALL_PDB=false
      for tok in $raw; do
        case "$tok" in
          1) INSTALL_TSC=true ;;
          2) INSTALL_JVM=true ;;
          3) INSTALL_PDB=true ;;
          *) warn "Ignoring unknown selection: '$tok'" ;;
        esac
      done
      ;;
  esac
fi

# Defaults if still unset (no prompt + no env var)
INSTALL_TSC="${INSTALL_TSC:-false}"
INSTALL_JVM="${INSTALL_JVM:-false}"
INSTALL_PDB="${INSTALL_PDB:-false}"

# -------------------------
# Per-controller configuration
# -------------------------
configure_controller() {
  # configure_controller <name> <default_tag> <default_replicas> <default_dry>
  local name="$1"
  local default_tag="$2"
  local default_replicas="$3"
  local default_dry="$4"

  echo ""
  info "Configuring ${name} controller:"

  # Image tag
  local current_tag_var="${name^^}_IMAGE_TAG"
  local current_tag="${!current_tag_var:-$default_tag}"
  if [ "$IS_INTERACTIVE" = true ]; then
    current_tag="$(prompt "  Image tag" "$current_tag")"
  fi
  eval "${name^^}_IMAGE_TAG=\"$current_tag\""

  # Replicas
  local current_replicas_var="${name^^}_REPLICAS"
  local current_replicas="${!current_replicas_var:-$default_replicas}"
  if [ "$IS_INTERACTIVE" = true ]; then
    current_replicas="$(prompt "  Replicas" "$current_replicas")"
  fi
  eval "${name^^}_REPLICAS=\"$current_replicas\""

  # Dry-run
  local current_dry_var="${name^^}_DRY_RUN"
  local current_dry="${!current_dry_var:-$default_dry}"
  if [ "$IS_INTERACTIVE" = true ]; then
    if confirm "  Install in dry-run mode? (logs intended changes, no mutations)" y; then
      current_dry="true"
    else
      current_dry="false"
    fi
  fi
  eval "${name^^}_DRY_RUN=\"$current_dry\""

  # Echo resolved values
  echo -e "  ${BLUE}→ tag=${current_tag}  replicas=${current_replicas}  dryRun=${current_dry}${NC}"
}

[ "$INSTALL_TSC" = true ] && configure_controller tsc "$TSC_IMAGE_TAG" "$TSC_REPLICAS" "$TSC_DRY_RUN"
[ "$INSTALL_JVM" = true ] && configure_controller jvm "$JVM_IMAGE_TAG" "$JVM_REPLICAS" "$JVM_DRY_RUN"
[ "$INSTALL_PDB" = true ] && configure_controller pdb "$PDB_IMAGE_TAG" "$PDB_REPLICAS" "$PDB_DRY_RUN"

# -------------------------
# Confirmation
# -------------------------
if [ "$IS_INTERACTIVE" = true ]; then
  echo ""
  echo "============================================================"
  echo " Plan"
  echo "============================================================"
  echo "  Namespace : $NAMESPACE"
  echo "  Cluster   : $CLUSTER_NAME"
  [ "$INSTALL_TSC" = true ] && echo "  TSC       : tag=$TSC_IMAGE_TAG  replicas=$TSC_REPLICAS  dryRun=$TSC_DRY_RUN"
  [ "$INSTALL_JVM" = true ] && echo "  JVM       : tag=$JVM_IMAGE_TAG  replicas=$JVM_REPLICAS  dryRun=$JVM_DRY_RUN"
  [ "$INSTALL_PDB" = true ] && echo "  PDB       : tag=$PDB_IMAGE_TAG  replicas=$PDB_REPLICAS  dryRun=$PDB_DRY_RUN"
  echo ""

  if ! confirm "Proceed with installation?" y; then
    fatal "Cancelled by user."
  fi
fi

# -------------------------
# Pre-install cleanup (cluster-scoped orphans from prior installs)
# -------------------------
if ! kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
  info "Namespace '$NAMESPACE' not found — cleaning any leftover cluster-scoped resources from prior installs..."

  kubectl delete clusterrole castai-workload-controllers --ignore-not-found
  kubectl delete clusterrolebinding castai-workload-controllers --ignore-not-found
  ok "Orphan cleanup complete."
fi

# -------------------------
# Create namespace
# -------------------------
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
ok "Namespace '$NAMESPACE' ready."

# -------------------------
# Install controller helper
# -------------------------
install_chart() {
  # install_chart <release> <chart_dir> <image_tag> <replicas> <dry_run> <extra_set_args...>
  local release="$1"
  local chart="$2"
  local tag="$3"
  local replicas="$4"
  local dry="$5"
  shift 5

  info "Installing $release (tag=$tag, replicas=$replicas, dryRun=$dry)..."
  helm upgrade -i "$release" "$chart" \
    -n "$NAMESPACE" $HELM_SERVERSIDE_APPLY \
    --set image.tag="$tag" \
    --set image.pullPolicy="$IMAGE_PULL_POLICY" \
    --set replicaCount="$replicas" \
    --set config.dryRun="$dry" \
    --create-namespace \
    "$@"
  ok "$release installed."
}

# -------------------------
# Run installations
# -------------------------
if [ "$INSTALL_TSC" = true ]; then
  install_chart castai-tsc-controller "$TSC_CHART" "$TSC_IMAGE_TAG" "$TSC_REPLICAS" "$TSC_DRY_RUN"
fi

if [ "$INSTALL_JVM" = true ]; then
  install_chart castai-jvm-probe-controller "$JVM_CHART" "$JVM_IMAGE_TAG" "$JVM_REPLICAS" "$JVM_DRY_RUN"
fi

if [ "$INSTALL_PDB" = true ]; then
  # PDB chart doesn't expose config.dryRun — translate to FixPoorPDBs logging-only
  if [ "$PDB_DRY_RUN" = true ]; then
    PDB_FIX="false"
  else
    PDB_FIX="true"
  fi
  install_chart castai-pdb-controller "$PDB_CHART" "$PDB_IMAGE_TAG" "$PDB_REPLICAS" "$PDB_DRY_RUN" \
    --set config.FixPoorPDBs="$PDB_FIX"
fi

# -------------------------
# Verify
# -------------------------
echo ""
info "Waiting for rollouts..."
for release in castai-tsc-controller castai-jvm-probe-controller castai-pdb-controller; do
  if helm list $HELM_ALL_STATES -n "$NAMESPACE" -f "^${release}$" -o json 2>/dev/null | jq -e 'length > 0' >/dev/null; then
    if kubectl rollout status "deployment/${release}" -n "$NAMESPACE" --timeout=180s >/dev/null 2>&1; then
      ok "${release} rollout complete."
    else
      warn "${release} rollout did not complete in time. Check: kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=${release}"
    fi
  fi
done

# -------------------------
# Summary
# -------------------------
echo ""
echo "============================================================"
ok "Installation finished."
echo ""
echo "Status:"
kubectl -n "$NAMESPACE" get deploy,cm -l app.kubernetes.io/part-of=castai 2>/dev/null \
  | grep -E "castai-(tsc|jvm-probe|pdb)-controller" || true

echo ""
echo "Next steps:"
echo "  1. Watch dry-run logs:"
echo "       kubectl logs -n $NAMESPACE -l app.kubernetes.io/name=castai-tsc-controller --tail=50 -f"
echo "       kubectl logs -n $NAMESPACE -l app.kubernetes.io/name=castai-jvm-probe-controller --tail=50 -f"
echo "       kubectl logs -n $NAMESPACE -l app.kubernetes.io/name=castai-pdb-controller --tail=50 -f"
echo ""
echo "  2. After reviewing dry-run output, go live by editing ConfigMaps:"
echo "       kubectl -n $NAMESPACE edit cm castai-tsc-controller-config   # set dryRun: \"false\""
echo "       kubectl -n $NAMESPACE edit cm castai-jvm-probe-controller-config   # set dryRun: \"false\""
echo "       (PDB controller: set config.FixPoorPDBs=\"true\" via Helm values)"
echo ""
echo "  3. Bypass a single workload with an annotation, e.g.:"
echo "       workloads.cast.ai/tsc-bypass: \"true\""
echo "       workloads.cast.ai/jvm-probe-bypass: \"true\""
echo "       workloads.cast.ai/bypass-default-pdb: \"true\""
echo "============================================================"
