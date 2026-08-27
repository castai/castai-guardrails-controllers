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
# Image tag defaults to the version suffix of the latest matching git release
# tag (e.g. tsc-v0.0.3 -> 0.0.3). Falls back to Chart.yaml appVersion when no
# matching tag exists. Override with TSC_IMAGE_TAG=..., JVM_IMAGE_TAG=...,
# PDB_IMAGE_TAG=... in non-interactive mode.
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
# Mode is canonical: "apply" (mutate workloads) or "recommend" (snapshot only).
# Default is "recommend" so a fresh install captures original workload state
# without mutating workloads; operators must explicitly opt in to apply.
TSC_MODE="${TSC_MODE:-recommend}"
JVM_MODE="${JVM_MODE:-recommend}"
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
# Failure logging
# -------------------------
# Writes a timestamped diagnostic log under .kimchi/logs/ capturing the
# environment, the failed helm command, and its full stdout+stderr.
# Called by every fatal helm failure path so users have something to share.
INSTALL_LOG_DIR="${SCRIPT_DIR:-$(pwd)}/.kimchi/logs"

log_install_failure() {
  # log_install_failure <label> <exit_code> <helm_output> [helm_command...]
  local label="$1"
  local rc="$2"
  local helm_output="$3"
  shift 3

  mkdir -p "$INSTALL_LOG_DIR" 2>/dev/null || true
  local ts
  ts="$(date +%Y%m%d-%H%M%S)"
  local log_file="${INSTALL_LOG_DIR}/install-${ts}.log"

  {
    echo "=== CAST AI Workload Controllers — Install Failure ==="
    echo "Timestamp  : $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "Label      : ${label}"
    echo "Exit code  : ${rc}"
    echo ""
    echo "--- Environment ---"
    echo "Helm version : $(helm version --template '{{.Version}}' 2>/dev/null || echo 'unknown')"
    echo "Kubectl ctx  : $(kubectl config current-context 2>/dev/null || echo 'unknown')"
    echo "Cluster      : ${CLUSTER_NAME:-unknown}"
    echo "Namespace    : ${NAMESPACE:-unknown}"
    echo ""
    echo "--- Selected controllers ---"
    echo "INSTALL_TSC=${INSTALL_TSC:-false}"
    echo "INSTALL_JVM=${INSTALL_JVM:-false}"
    echo "INSTALL_PDB=${INSTALL_PDB:-false}"
    [ "${INSTALL_TSC:-false}" = true ] && echo "  TSC_MODE=${TSC_MODE}"
    [ "${INSTALL_JVM:-false}" = true ] && echo "  JVM_MODE=${JVM_MODE}"
    echo ""
    echo "--- Helm command ---"
    printf 'helm'
    for a in "$@"; do
      printf ' %q' "$a"
    done
    echo ""
    echo ""
    echo "--- Helm output (stdout + stderr) ---"
    echo "$helm_output"
  } > "$log_file" 2>&1

  info "Detailed logs saved to: ${log_file}"
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
CRDS_CHART="${SCRIPT_DIR}/controllers/crds/helm/castai-guardrails-crds"

[ -d "$TSC_CHART" ] || fatal "TSC chart not found at $TSC_CHART"
[ -d "$JVM_CHART" ] || fatal "JVM chart not found at $JVM_CHART"
[ -d "$PDB_CHART" ] || fatal "PDB chart not found at $PDB_CHART"
[ -d "$CRDS_CHART" ] || fatal "CRDs chart not found at $CRDS_CHART"

# -------------------------
# Read appVersion from Chart.yaml (used as image tag fallback)
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
# Resolve latest git release tag version for a controller
# -------------------------
# latest_git_tag_version <prefix> <app_version>
# Returns the version suffix of the most recent matching git tag (e.g. for
# prefix=tsc and tag tsc-v0.0.3, returns 0.0.3). Falls back to <app_version>
# when no matching tag exists in the repo.
latest_git_tag_version() {
  local prefix="$1"
  local app_version="$2"
  local tag
  tag="$(git -C "$SCRIPT_DIR" tag --list "${prefix}-v*" --sort=-v:refname 2>/dev/null | head -n1)"
  if [ -n "$tag" ]; then
    echo "${tag#${prefix}-v}"
  else
    echo "$app_version"
  fi
}

TSC_TAG_DEFAULT="$(latest_git_tag_version "tsc" "$TSC_APP")"
JVM_TAG_DEFAULT="$(latest_git_tag_version "jvm" "$JVM_APP")"
PDB_TAG_DEFAULT="$(latest_git_tag_version "pdb" "$PDB_APP")"

# -------------------------
# Determine if interactive
# -------------------------
IS_INTERACTIVE=true
if [ ! -t 0 ] || [ -n "${INSTALL_TSC}${INSTALL_JVM}${INSTALL_PDB}" ]; then
  IS_INTERACTIVE=false
fi

# -------------------------
# Controller selection menu (checkbox-style, Space to toggle, Enter to confirm)
# -------------------------
#
# checkbox_menu <title> <name1> <name2> ...
#
# Renders a live-redrawn multi-select. Navigation: ↑/↓ or j/k. Toggle: Space.
# Confirm: Enter. Cancel: Ctrl-C. Returns selected indices (space-separated,
# 0-based) on stdout, or exits 130 on Ctrl-C.
#
# Pure bash + stty raw + ANSI escapes — no external deps beyond a VT100-ish
# terminal (works on macOS Terminal.app, iTerm2, gnome-terminal, Linux tty).
checkbox_menu() {
  local title="$1"; shift
  local -a items=("$@")
  local n=${#items[@]}
  local -a checked
  local i
  for ((i = 0; i < n; i++)); do checked[i]=0; done

  local cursor=0

  # Save current stty state; restore on any exit path
  local saved_stty
  saved_stty="$(stty -g </dev/tty)"
  # shellcheck disable=SC2064
  trap "stty '$saved_stty' </dev/tty; printf '\033[?25h' >/dev/tty" RETURN INT TERM

  stty -echo -icanon min 1 time 0 </dev/tty
  printf '\033[?25l' >/dev/tty   # hide cursor

  # Save the top-of-menu anchor before any rendering happens. Every redraw()
  # restores the cursor to this point and issues "\033[J" (erase below) so
  # wrapped or stale lines from the previous render cannot leak through on
  # narrow terminals — this is what previously caused the duplicated
  # selection rendering. We deliberately drop the old cursor-up/down
  # arithmetic: save+restore + erase-below handles positioning cleanly even
  # when an item wraps to more than one visual line.
  printf '\n' >/dev/tty
  printf '\033[s' >/dev/tty

  redraw() {
    # Restore cursor to the top-of-menu anchor and wipe everything below it.
    printf '\033[u' >/dev/tty
    printf '\033[J' >/dev/tty

    # Re-render the full menu: blank, title, separator, items, blank, hint footer.
    printf '\n' >/dev/tty
    printf '  %s\n' "$title" >/dev/tty
    printf '  %s\n' "$(printf '%.0s─' $(seq 1 60))" >/dev/tty
    local i
    for ((i = 0; i < n; i++)); do
      printf '\r\033[2K' >/dev/tty         # clear line
      local mark=' '; [ "${checked[i]}" -eq 1 ] && mark='x'
      if [ "$i" -eq "$cursor" ]; then
        printf '  \033[36m❯\033[0m [\033[32m%s\033[0m] %s\n' "$mark" "${items[i]}" >/dev/tty
      else
        printf '    [%s] %s\n' "$mark" "${items[i]}" >/dev/tty
      fi
    done
    printf '\n' >/dev/tty
    printf '  \033[2m↑/↓ or j/k navigate  ·  Space toggles  ·  Enter confirms  ·  Ctrl-C cancels\033[0m\n' >/dev/tty
  }

  redraw

  # Read loop — one byte at a time; decode arrow-key CSI sequences.
  #
  # Bash 3.2 (macOS default) does NOT accept fractional -t timeouts on `read`,
  # so we cannot use `read -t 0.001` to peek the ESC-[ tail of an arrow key.
  # Instead we switch the tty to a very short VMIN/VTIME poll (0 chars, 1 decisecond)
  # right before the CSI peek, then restore blocking mode. This works on any
  # POSIX stty and any bash version.
  local key esc1 esc2
  while :; do
    IFS= read -rsn1 key </dev/tty || break
    case "$key" in
      $'\x1b')  # ESC — could be lone Esc or start of CSI (arrow)
        # Non-blocking peek for the next two bytes.
        stty min 0 time 1 </dev/tty          # up to 100 ms wait, then bail
        esc1=''; esc2=''
        IFS= read -rsn1 esc1 </dev/tty || true
        IFS= read -rsn1 esc2 </dev/tty || true
        stty min 1 time 0 </dev/tty          # back to blocking for the main loop
        if [ "$esc1" = '[' ]; then
          case "$esc2" in
            A) [ "$cursor" -gt 0 ] && cursor=$((cursor - 1)); redraw ;;
            B) [ "$cursor" -lt $((n - 1)) ] && cursor=$((cursor + 1)); redraw ;;
          esac
        fi
        # bare Esc: ignore
        ;;
      j) [ "$cursor" -lt $((n - 1)) ] && cursor=$((cursor + 1)); redraw ;;
      k) [ "$cursor" -gt 0 ] && cursor=$((cursor - 1)); redraw ;;
      ' ')
        checked[cursor]=$((1 - checked[cursor]))
        redraw
        ;;
      '')  # Enter (empty read via -n1 on newline)
        break
        ;;
    esac
  done

  # Restore terminal state (also restored on RETURN via trap)
  stty "$saved_stty" </dev/tty
  printf '\033[?25h' >/dev/tty
  trap - RETURN INT TERM
  printf '\n' >/dev/tty

  # Emit selected indices
  local out=''
  for ((i = 0; i < n; i++)); do
    [ "${checked[i]}" -eq 1 ] && out+="$i "
  done
  printf '%s' "${out% }"
}

if [ "$IS_INTERACTIVE" = true ]; then
  echo ""
  echo "============================================================"
  echo " CAST AI Workload Controllers — Install"
  echo "============================================================"

  while :; do
    selected="$(checkbox_menu "Select controllers to install" \
      "TSC Controller  — Topology Spread Constraints" \
      "JVM Probe       — JVM health/startup/liveness probes" \
      "PDB Controller  — Pod Disruption Budgets")"

    if [ -z "$selected" ]; then
      if confirm "Nothing selected — cancel installation?" y; then
        fatal "Cancelled by user."
      fi
      # loop back and re-render the menu
      continue
    fi

    INSTALL_TSC=false; INSTALL_JVM=false; INSTALL_PDB=false
    for idx in $selected; do
      case "$idx" in
        0) INSTALL_TSC=true ;;
        1) INSTALL_JVM=true ;;
        2) INSTALL_PDB=true ;;
      esac
    done
    break
  done
fi

# Defaults if still unset (no prompt + no env var)
INSTALL_TSC="${INSTALL_TSC:-false}"
INSTALL_JVM="${INSTALL_JVM:-false}"
INSTALL_PDB="${INSTALL_PDB:-false}"

# -------------------------
# Mode prompts (per controller)
# -------------------------
configure_mode() {
  # configure_mode <PREFIX> <default>
  local prefix="$1"
  local default_mode="$2"
  local current_mode="$default_mode"
  local current_mode_var="${prefix}_MODE"

  # Allow non-interactive override via env
  if [ -n "${!current_mode_var:-}" ]; then
    eval "current_mode=\"${!current_mode_var}\""
    return
  fi

  if [ "$IS_INTERACTIVE" = true ]; then
    if confirm "  Install ${prefix} in apply mode? (recommend mode snapshots workloads without mutating)" y; then
      current_mode="apply"
    else
      current_mode="recommend"
    fi
  fi
  eval "${current_mode_var}=\"${current_mode}\""
}

[ "$INSTALL_TSC" = true ] && configure_mode TSC "$TSC_MODE"
[ "$INSTALL_JVM" = true ] && configure_mode JVM "$JVM_MODE"
# PDB has no mode toggle; FixPoorPDBs is enabled by default (controller is live on install).

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
  [ "$INSTALL_TSC" = true ] && echo "  TSC       : tag=${TSC_IMAGE_TAG_OVERRIDE:-$TSC_TAG_DEFAULT}  mode=${TSC_MODE}"
  [ "$INSTALL_JVM" = true ] && echo "  JVM       : tag=${JVM_IMAGE_TAG_OVERRIDE:-$JVM_TAG_DEFAULT}  mode=${JVM_MODE}"
  [ "$INSTALL_PDB" = true ] && echo "  PDB       : tag=${PDB_IMAGE_TAG_OVERRIDE:-$PDB_TAG_DEFAULT}  FixPoorPDBs=true (live)"
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
# Install shared CRDs as a separate Helm release
#
# TSC and JVM controllers both depend on the CRDs defined in the CRD chart.
# Installing them as a standalone release (castai-guardrails-crds) gives them
# a single owner so Helm won't reject later controller installs with
# "invalid ownership metadata" when the second controller tries to import
# CRDs already owned by the first. The controller installs below pass
# --set crds.enabled=false to avoid re-installing the CRDs.
# -------------------------
if [ "$INSTALL_TSC" = true ] || [ "$INSTALL_JVM" = true ]; then
  step "Installing shared CRDs (castai-guardrails-crds)"
  spin_start "  helm upgrade --install castai-guardrails-crds"
  CRDS_HELM_CMD=(upgrade --install castai-guardrails-crds "$CRDS_CHART"
                 --namespace "$NAMESPACE"
                 --create-namespace
                 --wait)
  if helm_out="$(helm "${CRDS_HELM_CMD[@]}" 2>&1)"; then
    spin_ok
    ok "castai-guardrails-crds installed."
  else
    spin_fail
    helm_rc=$?
    warn "Helm output:"
    echo "$helm_out" | sed 's/^/    /' >&2
    log_install_failure "castai-guardrails-crds" "$helm_rc" "$helm_out" "${CRDS_HELM_CMD[@]}"
    fatal "Failed to install castai-guardrails-crds (exit ${helm_rc})."
  fi
fi

# -------------------------
# Install controller helper
# -------------------------
install_chart() {
  # install_chart <release> <chart_dir> <app_version> <mode> <prefix>
  local release="$1"
  local chart="$2"
  local app_version="$3"
  local mode="$4"
  local prefix="$5"

  # Resolve tag: env-var override (non-interactive) > latest matching git tag > appVersion
  local override_var="${prefix}_IMAGE_TAG_OVERRIDE"
  local default_var="${prefix}_TAG_DEFAULT"
  local image_tag
  if [ -n "${!override_var:-}" ]; then
    eval "image_tag=\"\${${override_var}}\""
  else
    image_tag="${!default_var}"
  fi

  step "Installing ${release} (tag=${image_tag}, mode=${mode})"

  # Build helm args as array (safe)
  local -a args
  args=(upgrade -i "$release" "$chart"
        -n "$NAMESPACE" $HELM_SERVERSIDE_APPLY
        --set image.tag="$image_tag"
        --set image.pullPolicy="$IMAGE_PULL_POLICY"
        --set replicaCount=2
        --set management.enabled=true
        --set management.mode="$mode"
        --set management.rollbackOnDisable=false
        --create-namespace)

  # Controller-specific: CRD ownership and additional config.
  case "$prefix" in
    TSC)
      # CRDs are installed as a standalone release (castai-guardrails-crds) by
      # install.sh to avoid ownership conflicts between controllers.
      args+=(--set crds.enabled=false) ;;
    JVM)
      # CRDs are installed as a standalone release (castai-guardrails-crds) by
      # install.sh to avoid ownership conflicts between controllers.
      args+=(--set crds.enabled=false) ;;
    PDB)
      # PDB has no mode toggle. FixPoorPDBs is enabled by default so the
      # controller auto-remediates poor PDBs immediately on install.
      args+=(--set config.FixPoorPDBs="true")
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
    log_install_failure "$release" "$helm_rc" "$helm_out" "${args[@]}"
    fatal "Failed to install ${release} (exit ${helm_rc})."
  fi
}

# -------------------------
# Run installations
# -------------------------
if [ "$INSTALL_TSC" = true ]; then
  install_chart castai-tsc-controller      "$TSC_CHART" "$TSC_APP" "$TSC_MODE" TSC
fi
if [ "$INSTALL_JVM" = true ]; then
  install_chart castai-jvm-probe-controller "$JVM_CHART" "$JVM_APP" "$JVM_MODE" JVM
fi
if [ "$INSTALL_PDB" = true ]; then
  install_chart castai-pdb-controller     "$PDB_CHART" "$PDB_APP" "" PDB
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
step "Watch controller logs:"
echo "    kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=castai-tsc-controller       --tail=50 -f"
echo "    kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=castai-jvm-probe-controller --tail=50 -f"
echo "    kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=castai-pdb-controller      --tail=50 -f"
echo ""
step "PDB is live by default (FixPoorPDBs=true). To re-apply via Helm:"
echo "    helm upgrade castai-pdb-controller ${PDB_CHART} -n ${NAMESPACE} --set config.FixPoorPDBs=\"true\""
echo ""
step "Bypass a single workload with an annotation:"
echo "    workloads.cast.ai/tsc-bypass: \"true\""
echo "    workloads.cast.ai/jvm-probe-bypass: \"true\""
echo "    workloads.cast.ai/bypass-default-pdb: \"true\""
echo ""
echo "============================================================"
echo " Next steps"
echo "============================================================"
echo ""
step "Enable apply mode (default after install is recommend — controllers only snapshot):"
echo "    kubectl -n ${NAMESPACE} patch cm castai-tsc-controller-config       --type merge -p '{\"data\":{\"managementEnabled\":\"true\",\"rollbackOnDisable\":\"false\",\"mode\":\"apply\"}}'"
echo "    kubectl -n ${NAMESPACE} patch cm castai-jvm-probe-controller-config --type merge -p '{\"data\":{\"managementEnabled\":\"true\",\"rollbackOnDisable\":\"false\",\"mode\":\"apply\"}}'"
echo ""
step "Disable and rollback changes:"
echo "    kubectl -n ${NAMESPACE} patch cm castai-tsc-controller-config       --type merge -p '{\"data\":{\"managementEnabled\":\"false\",\"rollbackOnDisable\":\"true\"}}'"
echo "    kubectl -n ${NAMESPACE} patch cm castai-jvm-probe-controller-config --type merge -p '{\"data\":{\"managementEnabled\":\"false\",\"rollbackOnDisable\":\"true\"}}'"
echo ""
step "Recommend mode (capture snapshots but do not patch):"
echo "    kubectl -n ${NAMESPACE} patch cm castai-tsc-controller-config       --type merge -p '{\"data\":{\"managementEnabled\":\"true\",\"mode\":\"recommend\"}}'"
echo "    kubectl -n ${NAMESPACE} patch cm castai-jvm-probe-controller-config --type merge -p '{\"data\":{\"managementEnabled\":\"true\",\"mode\":\"recommend\"}}'"
echo ""
step "Verify snapshots:"
echo "    kubectl get tscoriginals -n ${NAMESPACE}"
echo "    kubectl get jvmprobeoriginals -n ${NAMESPACE}"
echo ""
step "Check rollback status:"
echo "    kubectl get tscoriginals -n ${NAMESPACE} -o jsonpath='{range .items[*]}{.metadata.name}{\"\t\"}{.status.conditions[?(@.type==\"RolledBack\")].status}{\"\n\"}{end}'"
echo ""
step "See docs/rollback-operator-runbook.md for the full runbook."
echo "============================================================"
