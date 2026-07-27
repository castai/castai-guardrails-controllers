#!/bin/bash
# ============================================================
# CAST AI Workload Controllers — Interactive Installer
# ------------------------------------------------------------
# Mirrors the castctl / connect-and-enable-castai.sh UX:
#   - Pre-flights (kubectl, helm 3.14+, jq)
#   - Detects the current kubectl context / cluster name
#   - Asks which controllers to install
#   - Installs via Helm from local charts in ./controllers/
#   - Verifies rollout
#
# Image tag defaults to Chart.yaml appVersion (always the version
# deployed alongside the chart). Override with TSC_IMAGE_TAG=...
# in non-interactive mode.
#
# Interactive by default; non-interactive when INSTALL_* env vars
# are preset (matches castctl --non-interactive).
#
# Usage:
#   ./install.sh
#
# Non-interactive example:
#   INSTALL_TSC=true INSTALL_JVM=true INSTALL_PDB=true \
#   ./install.sh
# ============================================================

set -euo pipefail

# -------------------------
# Defaults (override via env)
# -------------------------
NAMESPACE="${NAMESPACE:-castai-agent}"
IMAGE_PULL_POLICY="${IMAGE_PULL_POLICY:-IfNotPresent}"

INSTALL_TSC="${INSTALL_TSC:-}"
INSTALL_JVM="${INSTALL_JVM:-}"
INSTALL_PDB="${INSTALL_PDB:-}"

# Per-controller overrides (non-interactive only — interactive uses defaults)
TSC_DRY_RUN="${TSC_DRY_RUN:-true}"
JVM_DRY_RUN="${JVM_DRY_RUN:-true}"
PDB_DRY_RUN="${PDB_DRY_RUN:-true}"
TSC_IMAGE_TAG_OVERRIDE="${TSC_IMAGE_TAG:-}"
JVM_IMAGE_TAG_OVERRIDE="${JVM_IMAGE_TAG:-}"
PDB_IMAGE_TAG_OVERRIDE="${PDB_IMAGE_TAG:-}"

# -------------------------
# Colors (castctl style: cyan [INFO], green [OK], red ERROR, yellow WARN)
# -------------------------
CYAN='\033[0;36m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
BLUE='\033[0;34m'
DIM='\033[2m'
NC='\033[0m'

fatal() {
  echo -e "${RED}ERROR: $1${NC}" >&2
  exit 1
}

info() {
  echo -e "${CYAN}[INFO]${NC} $1"
}

ok() {
  echo -e "${GREEN}[OK]${NC} $1"
}

warn() {
  echo -e "${YELLOW}[WARN]${NC} $1"
}

step() {
  echo -e "${BLUE}→${NC} $1"
}

# -------------------------
# Spinner — bash 3.2-safe
# -------------------------
# Spawns a backgrounded printf loop on the same line; caller must
# call spin_stop when done. Works on macOS /bin/bash 3.2.57.
SPIN_PID=""

spin_start() {
  local msg="$1"
  printf "  %s " "$msg" >&2
  # Run spinner in background; redirect to stderr on the same line
  (
    i=0
    chars='|/-\'
    while :; do
      printf "\b%s" "${chars:i++%4:1}" >&2
      sleep 0.1
    done
  ) &
  SPIN_PID=$!
  # Make sure the spinner dies on script exit
  trap 'spin_stop' EXIT
}

spin_stop() {
  if [ -n "$SPIN_PID" ] && kill -0 "$SPIN_PID" 2>/dev/null; then
    kill "$SPIN_PID" 2>/dev/null || true
    wait "$SPIN_PID" 2>/dev/null || true
  fi
  SPIN_PID=""
  printf "\b \b" >&2  # erase spinner glyph
}

spin_ok() {
  spin_stop
  printf "${GREEN}✓${NC}\n" >&2
}

spin_fail() {
  spin_stop
  printf "${RED}✗${NC}\n" >&2
}

# -------------------------
# Input helpers
# -------------------------
prompt() {
  # prompt <question> <default_value>
  local q="$1"
  local default="$2"
  local reply
  read -r -p "$(echo -e "${CYAN}${q}${NC} [${default}]: ")" reply
  reply="${reply:-$default}"
  echo "$reply"
}

confirm() {
  # confirm <question> [default y/n]
  local q="$1"
  local default="${2:-y}"
  local reply
  read -r -p "$(echo -e "${YELLOW}${q}${NC} [y/n, default ${default}]: ")" reply
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

# Helm 4: disable server-side apply (matches castai-helm-onboard.sh)
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

info "kubectl context : ${KCTL_CTX}"
info "Cluster name    : ${CLUSTER_NAME}"

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
# Read appVersion from Chart.yaml (single source of truth)
# -------------------------
chart_app_version() {
  local chart_dir="$1"
  # Pull "appVersion: ..." line from Chart.yaml; strip quotes
  awk '/^appVersion:/{gsub(/^appVersion: */,""); gsub(/["'\'']/,""); print; exit}' "$chart_dir/Chart.yaml"
}

TSC_APP="$(chart_app_version "$TSC_CHART")"
JVM_APP="$(chart_app_version "$JVM_CHART")"
PDB_APP="$(chart_app_version "$PDB_CHART")"

# -------------------------
# Determine if interactive
# -------------------------
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

  while :; do
    raw="$(prompt "Select controllers to install (e.g. '1 3', '4', or '5')" "")"
    case "$raw" in
      5|q|quit|cancel) fatal "Cancelled." ;;
      ""|4|all) INSTALL_TSC=true; INSTALL_JVM=true; INSTALL_PDB=true; break ;;
      *)
        INSTALL_TSC=false; INSTALL_JVM=false; INSTALL_PDB=false; valid=true
        for tok in $raw; do
          case "$tok" in
            1) INSTALL_TSC=true ;;
            2) INSTALL_JVM=true ;;
            3) INSTALL_PDB=true ;;
            *) warn "Ignoring unknown selection: '$tok'"; valid=false ;;
          esac
        done
        [ "$valid" = true ] && break
        ;;
    esac
  done
fi

# Defaults if still unset (no prompt + no env var)
INSTALL_TSC="${INSTALL_TSC:-false}"
INSTALL_JVM="${INSTALL_JVM:-false}"
INSTALL_PDB="${INSTALL_PDB:-false}"

# -------------------------
# Dry-run prompts (per controller)
# -------------------------
configure_dry_run() {
  # configure_dry_run <PREFIX> <default>
  local prefix="$1"
  local default_dry="$2"
  local current_dry="$default_dry"
  local current_dry_var="${prefix}_DRY_RUN"

  # Allow non-interactive override via env
  if [ -n "${!current_dry_var:-}" ]; then
    eval "current_dry=\"${current_dry_var}\""
    return
  fi

  if [ "$IS_INTERACTIVE" = true ]; then
    if confirm "  Install ${prefix} in dry-run mode? (logs intended changes, no mutations)" y; then
      current_dry="true"
    else
      current_dry="false"
    fi
  fi
  eval "${current_dry_var}=\"${current_dry}\""
}

[ "$INSTALL_TSC" = true ] && configure_dry_run TSC "$TSC_DRY_RUN"
[ "$INSTALL_JVM" = true ] && configure_dry_run JVM "$JVM_DRY_RUN"
[ "$INSTALL_PDB" = true ] && configure_dry_run PDB "$PDB_DRY_RUN"

# -------------------------
# Confirmation
# -------------------------
if [ "$IS_INTERACTIVE" = true ]; then
  echo ""
  echo "============================================================"
  echo " Plan"
  echo "============================================================"
  echo "  Namespace : ${NAMESPACE}"
  echo "  Cluster   : ${CLUSTER_NAME}"
  [ "$INSTALL_TSC" = true ] && echo "  TSC       : tag=${TSC_APP:-latest}  dryRun=${TSC_DRY_RUN}"
  [ "$INSTALL_JVM" = true ] && echo "  JVM       : tag=${JVM_APP:-latest}  dryRun=${JVM_DRY_RUN}"
  [ "$INSTALL_PDB" = true ] && echo "  PDB       : tag=${PDB_APP:-latest}  dryRun=${PDB_DRY_RUN}"
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

  for name in castai-workload-controllers castai-tsc-controller castai-jvm-probe-controller castai-pdb-controller; do
    kubectl delete clusterrole "$name" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete clusterrolebinding "$name" --ignore-not-found >/dev/null 2>&1 || true
  done
  ok "Orphan cleanup complete."
fi

# -------------------------
# Create namespace (with spinner)
# -------------------------
spin_start "Creating namespace '${NAMESPACE}'..."
if kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml 2>/dev/null \
   | kubectl apply -f - >/dev/null 2>&1; then
  spin_ok
  ok "Namespace '${NAMESPACE}' ready."
else
  spin_fail
  fatal "Failed to create namespace '${NAMESPACE}'."
fi

# -------------------------
# Install controller helper
# -------------------------
install_chart() {
  # install_chart <release> <chart_dir> <app_version> <dry_run> <prefix>
  local release="$1"
  local chart="$2"
  local app_version="$3"
  local dry="$4"
  local prefix="$5"

  # Resolve tag: env-var override (non-interactive) > Chart.AppVersion
  local override_var="${prefix}_IMAGE_TAG_OVERRIDE"
  local image_tag
  if [ -n "${!override_var:-}" ]; then
    eval "image_tag=\"\${${override_var}}\""
  else
    image_tag="$app_version"
  fi

  step "Installing ${release} (tag=${image_tag}, dryRun=${dry})"

  # Build helm args as array (safe)
  local -a args
  args=(upgrade -i "$release" "$chart"
        -n "$NAMESPACE" $HELM_SERVERSIDE_APPLY
        --set image.tag="$image_tag"
        --set image.pullPolicy="$IMAGE_PULL_POLICY"
        --set replicaCount=2
        --create-namespace)

  # Controller-specific: dry-run propagation
  case "$prefix" in
    TSC)
      args+=(--set config.dryRun="$dry" --set config.enableTSCManagement="true") ;;
    JVM)
      args+=(--set config.dryRun="$dry"
             --set config.logIntendedChanges="$dry"
             --set config.enableProbeManagement="true") ;;
    PDB)
      # PDB chart has no dryRun; translate to FixPoorPDBs
      if [ "$dry" = "true" ]; then
        args+=(--set config.FixPoorPDBs="false")
      else
        args+=(--set config.FixPoorPDBs="true")
      fi
      ;;
  esac

  # Run with spinner; capture helm output for error reporting only
  spin_start "  helm upgrade --install ${release}"
  local helm_out helm_rc
  if helm_out="$(helm "${args[@]}" 2>&1)"; then
    spin_ok
    ok "${release} installed."
  else
    spin_fail
    helm_rc=$?
    warn "Helm output:"
    echo "$helm_out" | sed 's/^/    /' >&2
    fatal "Failed to install ${release} (exit ${helm_rc})."
  fi
}

# -------------------------
# Run installations
# -------------------------
if [ "$INSTALL_TSC" = true ]; then
  install_chart castai-tsc-controller      "$TSC_CHART" "$TSC_APP" "$TSC_DRY_RUN" TSC
fi
if [ "$INSTALL_JVM" = true ]; then
  install_chart castai-jvm-probe-controller "$JVM_CHART" "$JVM_APP" "$JVM_DRY_RUN" JVM
fi
if [ "$INSTALL_PDB" = true ]; then
  install_chart castai-pdb-controller     "$PDB_CHART" "$PDB_APP" "$PDB_DRY_RUN" PDB
fi

# -------------------------
# Verify rollouts
# -------------------------
echo ""
info "Verifying rollouts..."
for release in castai-tsc-controller castai-jvm-probe-controller castai-pdb-controller; do
  if helm list $HELM_ALL_STATES -n "$NAMESPACE" -f "^${release}$" -o json 2>/dev/null \
     | jq -e 'length > 0' >/dev/null; then
    spin_start "  waiting for ${release}"
    if kubectl rollout status "deployment/${release}" -n "$NAMESPACE" --timeout=180s >/dev/null 2>&1; then
      spin_ok
      ok "${release} rolled out."
    else
      spin_fail
      warn "${release} rollout did not complete in 180s."
      step "Check: kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=${release}"
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
step "Watch dry-run logs:"
echo "    kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=castai-tsc-controller       --tail=50 -f"
echo "    kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=castai-jvm-probe-controller --tail=50 -f"
echo "    kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=castai-pdb-controller      --tail=50 -f"
echo ""
step "Go live (turn off dry-run):"
echo "    kubectl -n ${NAMESPACE} edit cm castai-tsc-controller-config       # set dryRun: \"false\""
echo "    kubectl -n ${NAMESPACE} edit cm castai-jvm-probe-controller-config  # set dryRun: \"false\""
echo "    helm upgrade castai-pdb-controller ${PDB_CHART} -n ${NAMESPACE} --set config.FixPoorPDBs=\"true\""
echo ""
step "Bypass a single workload with an annotation:"
echo "    workloads.cast.ai/tsc-bypass: \"true\""
echo "    workloads.cast.ai/jvm-probe-bypass: \"true\""
echo "    workloads.cast.ai/bypass-default-pdb: \"true\""
echo "============================================================"
