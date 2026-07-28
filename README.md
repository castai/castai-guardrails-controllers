# CAST AI Workload Controllers

Three Kubernetes controllers that **automatically remediate workload configuration** in any cluster. They watch your Deployments and StatefulSets and fix common reliability gaps — missing Pod Disruption Budgets, missing Topology Spread Constraints, and missing/misconfigured JVM health probes — so workloads are spread safely, drain cleanly, and start healthily.

| Controller | What it fixes | Default mode |
|---|---|---|
| **TSC Controller** | Missing `topologySpreadConstraints` | Dry-run (observe) |
| **JVM Probe Controller** | Missing/misconfigured JVM liveness/readiness/**startup** probes | Dry-run (observe) |
| **PDB Controller** | Missing/poor `PodDisruptionBudget`s | **Live** (`FixPoorPDBs=true`) |

All three follow the [castai-pdb-controller](https://github.com/castai/castai-pdb-controller) pattern: leader election, shared informers, rate-limited logging, ConfigMap-driven config with hot-reload, and strategic-merge-patch (non-destructive) updates.

---

## Repository layout

```
castai-guardrails-controllers/
├── install.sh                     # ← primary installer (interactive + non-interactive)
├── controllers/
│   ├── tsc-controller/            # Topology Spread Constraints controller
│   │   ├── cmd/                   # Go source
│   │   ├── helm/castai-tsc-controller/   # Helm chart (Chart.yaml, values.yaml, templates/)
│   │   └── Dockerfile
│   ├── jvm-probe-controller/      # JVM probe injection + auto-fix controller
│   │   ├── cmd/
│   │   ├── helm/castai-jvm-probe-controller/
│   │   └── Dockerfile
│   └── pdb-controller/            # Pod Disruption Budget controller
│       ├── cmd/ (also top-level *.yaml for non-helm)
│       └── helm/castai-pdb-controller/
├── build-and-push.sh              # build & push all controller images
└── README.md                      # this file
```

Each controller also has its own `README.md` under `controllers/<name>/` for exhaustive reference. This file is the overview + quick start + deep-dive summary.

---

## Quick start — install with `install.sh`

**Deployment is done via `install.sh`.** It is the primary and recommended way to install all three controllers. It mirrors the `castctl` UX: pre-flights your tooling, detects the current kubectl context/cluster, asks which controllers to install, installs them via Helm from the local charts in `./controllers/`, and verifies the rollout.

### Prerequisites

- `kubectl` configured with cluster access
- `helm` **3.14+**
- `jq`

### Interactive (default)

```bash
./install.sh
```

You'll get a menu:

```
  [1] TSC Controller     — Topology Spread Constraints
  [2] JVM Probe          — JVM health/startup/liveness probes
  [3] PDB Controller     — Pod Disruption Budgets
  [4] Install ALL
  [5] Cancel
```

Pick one or several (e.g. `1 3`), confirm dry-run per controller (TSC/JVM default to dry-run; **PDB installs live**), and the script installs via Helm and waits for rollout.

### Non-interactive (CI / automation)

```bash
# Install all three
INSTALL_TSC=true INSTALL_JVM=true INSTALL_PDB=true ./install.sh

# Selective install
INSTALL_TSC=true INSTALL_PDB=true ./install.sh

# Override image tag (defaults to each chart's appVersion) and dry-run flags
INSTALL_TSC=true INSTALL_JVM=true TSC_IMAGE_TAG=v1.2.3 JVM_DRY_RUN=false ./install.sh
```

| Env var | Purpose | Default |
|---|---|---|
| `INSTALL_TSC` / `INSTALL_JVM` / `INSTALL_PDB` | Select controllers (non-interactive) | unset |
| `TSC_DRY_RUN` / `JVM_DRY_RUN` | Dry-run on/off for those controllers | `true` |
| `TSC_IMAGE_TAG` / `JVM_IMAGE_TAG` / `PDB_IMAGE_TAG` | Override image tag | chart `appVersion` |
| `NAMESPACE` | Target namespace | `castai-agent` |
| `IMAGE_PULL_POLICY` | Container image pull policy | `IfNotPresent` |

> **PDB has no dry-run mode.** `PDB_DRY_RUN` is ignored; the controller always installs with `config.FixPoorPDBs="true"` (live). See [PDB Controller](#3-pdb-controller-pdb-controller) below.

### What `install.sh` does, step by step

1. Pre-flights `kubectl`, `helm 3.14+`, `jq`.
2. Detects current kubectl context + cluster name.
3. (Interactive) Asks which controllers to install and whether each runs in dry-run.
4. Cleans orphaned cluster-scoped RBAC from prior installs (if the namespace was absent).
5. Creates the `castai-agent` namespace.
6. Runs `helm upgrade --install` for each selected controller from its local chart, setting `image.tag`, `image.pullPolicy`, `replicaCount=2`, and the per-controller config flags.
7. Waits for each Deployment to roll out (180s timeout).

---

## After install: watch logs, go live, bypass workloads

When install finishes you'll see a summary like the one below.

### Watch controller logs

```bash
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-tsc-controller       --tail=50 -f
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-jvm-probe-controller --tail=50 -f
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-pdb-controller      --tail=50 -f
```

### Go live (turn off dry-run) — TSC & JVM

TSC and JVM install in **dry-run** (observe-only) mode. To make them actually mutate workloads, **patch** their ConfigMaps — both controllers hot-reload the ConfigMap, so **no restart is needed**:

```bash
# TSC: configmap key is "dryRun"
kubectl -n castai-agent patch cm castai-tsc-controller-config \
  --type merge -p '{"data":{"dryRun":"false"}}'

# JVM: configmap key is "jvm-dryRun" (note the prefix — not "dryRun")
kubectl -n castai-agent patch cm castai-jvm-probe-controller-config \
  --type merge -p '{"data":{"jvm-dryRun":"false"}}'
```

> Re-enable dry-run later by patching the same key back to `"true"`.

**PDB is already live** (`FixPoorPDBs=true` at install). To re-apply via Helm:

```bash
helm upgrade castai-pdb-controller \
  ./controllers/pdb-controller/helm/castai-pdb-controller \
  -n castai-agent --set config.FixPoorPDBs="true"
```

### Bypass a single workload (opt out per workload)

Add the relevant annotation to a Deployment or StatefulSet:

```yaml
metadata:
  annotations:
    workloads.cast.ai/tsc-bypass: "true"          # skip TSC injection
    workloads.cast.ai/jvm-probe-bypass: "true"   # skip JVM probe injection
    workloads.cast.ai/bypass-default-pdb: "true" # skip PDB management
```

---

## Controllers — deep dive

### 1. TSC Controller (`tsc-controller`)

**What it does:** automatically injects `topologySpreadConstraints` into Deployments and StatefulSets that are missing them, so pods spread across zones and nodes for high availability.

**Features**
- Zone-based spreading by default (`topology.kubernetes.io/zone`)
- Annotation-based overrides per workload
- Regex-based exclusion rules via ConfigMap
- Garbage collection removes TSC when replicas drop below 2
- Leader election for HA
- Rate-limited logging

**Config** (`controllers/tsc-controller/helm/castai-tsc-controller/values.yaml`):

| Key | Description | Default |
|---|---|---|
| `config.dryRun` | Observe-only (log intended changes, no mutations) | `true` |
| `config.defaultConstraints` | Default TSC: `maxSkew`, `topologyKey`, `whenUnsatisfiable` | zone / maxSkew 1 / DoNotSchedule |
| `config.skipSingleReplica` | Skip workloads with <2 replicas | `true` |
| `config.logInterval` | Rate-limit interval for repeated logs | `15m` |
| `config.reconcileInterval` | Reconcile loop interval | `2m` |
| `config.garbageCollectInterval` | GC interval | `5m` |
| `config.exclusions` | Regex rules (namespace/name) to skip | `[]` |

Rendered ConfigMap: `castai-tsc-controller-config`, key `dryRun`.

**Annotations**

| Annotation | Description | Example |
|---|---|---|
| `workloads.cast.ai/tsc-bypass` | Skip this workload | `"true"` |
| `workloads.cast.ai/tsc-maxSkew` | Override maxSkew | `"1"` |
| `workloads.cast.ai/tsc-topologyKey` | Override topology key | `"kubernetes.io/hostname"` |
| `workloads.cast.ai/tsc-whenUnsatisfiable` | Override policy | `"DoNotSchedule"` / `"ScheduleAnyway"` |

**Example**
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  annotations:
    workloads.cast.ai/tsc-maxSkew: "2"
    workloads.cast.ai/tsc-topologyKey: "kubernetes.io/hostname"
spec:
  replicas: 3
  # ...
```

Verify: `kubectl get deploy my-app -o jsonpath='{.spec.template.spec.topologySpreadConstraints}'`

---

### 2. JVM Probe Controller (`jvm-probe-controller`)

**What it does:** detects JVM-based containers and injects appropriate health probes (liveness, readiness, **startup**) when missing. It also **monitors probe failures** and **auto-fixes** slow-starting JVMs by raising `initialDelaySeconds`/`failureThreshold`.

**Features**
- Framework detection: **Spring Boot, Quarkus, Micronaut, generic JVM**
- Detection via image name (word-boundary regex), env vars (`JAVA_HOME`, `SPRING_PROFILES_ACTIVE`, `JAVA_TOOL_OPTIONS`), and container command
- **Startup probe always injected** for JVM containers (prevents premature termination)
- **Probe failure monitoring** via pod events (`Unhealthy`/`ProbeFailed` + restart counts)
- **Auto-fix** adjusts timing fields based on failure patterns (framework-aware)
- Force-overwrite existing probes (per-probe or all)
- Dry-run / observe-only mode
- Liveness probe **opt-in** by default (Spring Boot needs `management.endpoint.health.probes.enabled=true`)

**Detected frameworks → probe paths**

| Framework | Liveness | Readiness | Startup |
|---|---|---|---|
| Spring Boot | `/actuator/health/liveness` | `/actuator/health/readiness` | `/actuator/health` |
| Quarkus | `/q/health/live` | `/q/health/ready` | `/q/health/started` |
| Micronaut | `/health/liveness` | `/health/readiness` | `/health` |
| Generic JVM | TCP socket | TCP socket | TCP socket |

> **Spring Boot liveness** is opt-in (`injectLivenessProbe: "false"` by default) because `/actuator/health/liveness` only exists with `management.endpoint.health.probes.enabled=true` (Spring Boot 2.3+). Enable per-workload with `workloads.cast.ai/jvm-probe-inject-liveness: "true"`.

**Config** (`controllers/jvm-probe-controller/helm/castai-jvm-probe-controller/values.yaml`):

| Key | Description | Default |
|---|---|---|
| `config.dryRun` | Observe-only | `true` |
| `config.logIntendedChanges` | Log intended changes | `true` |
| `config.injectLivenessProbe` | Inject liveness probe | `false` (opt-in) |
| `config.injectReadinessProbe` | Inject readiness probe | `true` |
| `config.injectStartupProbe` | Inject startup probe | `true` |
| `config.requireBothProbes` | Inject liveness/readiness only if BOTH missing | `true` |
| `config.skipIfAnyProbeExists` | Skip if any probe already present | `false` |
| `config.frameworks` | Per-framework paths/timing (JSON) | spring-boot/quarkus/micronaut/generic |
| `config.exclusions` | Regex rules to skip | `[]` |

Rendered ConfigMap: `castai-jvm-probe-controller-config`. **Dry-run key is `jvm-dryRun`** (prefixed), not `dryRun`.

**Annotations**

| Annotation | Description | Example |
|---|---|---|
| `workloads.cast.ai/jvm-probe-bypass` | Skip this workload | `"true"` |
| `workloads.cast.ai/jvm-probe-framework` | Force framework detection | `"spring-boot"` |
| `workloads.cast.ai/jvm-probe-port` | Override port | `"8080"` |
| `workloads.cast.ai/jvm-probe-initial-delay` | Initial delay seconds | `"60"` |
| `workloads.cast.ai/jvm-probe-overwrite-all` | Force overwrite all probes | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-liveness` | Overwrite liveness | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-readiness` | Overwrite readiness | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-startup` | Overwrite startup | `"true"` |
| `workloads.cast.ai/jvm-probe-log-failures` | Detailed failure logging | `"true"` |
| `workloads.cast.ai/jvm-probe-inject-liveness` | Override liveness injection | `"true"`/`"false"` |
| `workloads.cast.ai/jvm-probe-inject-readiness` | Override readiness injection | `"true"`/`"false"` |
| `workloads.cast.ai/jvm-probe-inject-startup` | Override startup injection | `"true"`/`"false"` |

**Auto-fix** — the Pod Event Monitor watches probe failures and restarts. Triggers: ≥5 probe failures in 5 min, ≥3 container restarts, or high failure rate (>10/min). When triggered it raises `initialDelaySeconds` (up to 300s) and `failureThreshold` (up to 10), preserving the existing handler (exec/tcpSocket/grpc) and using framework-correct paths. Enable detailed logging with `workloads.cast.ai/jvm-probe-log-failures: "true"`.

**Example**
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-java-app
  annotations:
    workloads.cast.ai/jvm-probe-framework: "spring-boot"
    workloads.cast.ai/jvm-probe-port: "8081"
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: app
          image: mycompany/spring-boot-app:latest
```

Verify:
```bash
kubectl get deploy my-java-app -o jsonpath='{.spec.template.spec.containers[0].startupProbe}'
```

---

### 3. PDB Controller (`pdb-controller`)

**What it does:** automatically manages `PodDisruptionBudget`s for Deployments and StatefulSets with ≥2 replicas — creating them when absent, fixing poor ones, and garbage-collecting orphans.

**Features**
- Automatic PDB creation for workloads with ≥2 replicas
- Annotation overrides per workload (`minAvailable`, `maxUnavailable`, eviction policy)
- **Poor PDB detection** — `minAvailable == replicas`, `minAvailable: 100%`, `maxUnavailable: 0` etc.
- **Auto-fix poor PDBs** — deletes and recreates with safe defaults (**enabled by default**: `FixPoorPDBs=true`)
- Multiple-PDB handling — removes redundant CAST AI PDBs when a user-defined PDB exists
- Garbage collection of orphaned PDBs
- Leader election for HA
- Regex-based exclusion rules (namespace/name/labels)
- Configurable log levels (`debug`/`info`/`warn`/`error`)

**Config** (`controllers/pdb-controller/helm/castai-pdb-controller/values.yaml`):

| Key | Description | Default |
|---|---|---|
| `config.FixPoorPDBs` | Auto-fix poor PDBs (true) or warn-only (false) | `"true"` ✅ **enabled by default** |
| `config.defaultMinAvailable` | Default minAvailable (use one of min/max) | `"1"` |
| `config.defaultMaxUnavailable` | Default maxUnavailable (mutually exclusive) | `null` |
| `config.defaultUnhealthyPodEvictionPolicy` | `IfHealthyBudget` / `AlwaysAllow` / `""` | `""` |
| `config.logLevel` | `debug` / `info` / `warn` / `error` | `info` |
| `config.logInterval` | Rate-limit interval | `15m` |
| `config.pdbScanInterval` | PDB scan interval | `2m` |
| `config.garbageCollectInterval` | GC interval | `2m` |
| `config.pdbDumpInterval` | PDB dump interval | `5m` |
| `config.exclusions` | Regex rules (namespace/name/labels) | `.*-temp$` example |

Rendered ConfigMap: `castai-pdb-controller-config`, key `FixPoorPDBs`.

> **No dry-run mode.** Unlike TSC/JVM, the PDB controller is live on install. Set `config.FixPoorPDBs="false"` for warn-only behaviour.

**Annotations**

| Annotation | Description | Example |
|---|---|---|
| `workloads.cast.ai/pdb-minAvailable` | Min pods available | `"1"` / `"50%"` |
| `workloads.cast.ai/pdb-maxUnavailable` | Max pods unavailable | `"1"` / `"25%"` |
| `workloads.cast.ai/pdb-unhealthyPodEvictionPolicy` | Eviction policy (K8s 1.26+) | `IfHealthyBudget` / `AlwaysAllow` |
| `workloads.cast.ai/bypass-default-pdb` | Opt out of PDB management | `"true"` |

**Example**
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  annotations:
    workloads.cast.ai/pdb-minAvailable: "50%"
spec:
  replicas: 4
  # ...
```

Verify: `kubectl get pdb -n <ns>`

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    CAST AI Workload Controllers                 │
├─────────────────────────────────────────────────────────────────┤
│  ┌─────────────────────┐  ┌─────────────────────┐  ┌──────────┐ │
│  │   TSC Controller    │  │  JVM Probe Ctrl     │  │  PDB     │ │
│  │ • Watch Deploys/STS │  │ • Watch Deploys/STS │  │ • Watch  │ │
│  │ • Inject TSC        │  │ • Detect JVM        │  │   Deploys│ │
│  │ • GC (replicas<2)   │  │ • Inject probes     │  │   /STS   │ │
│  │ • Leader Election   │  │ • Auto-fix failures │  │ • Fix    │ │
│  │                     │  │ • Leader Election   │  │   poor   │ │
│  │                     │  │                     │  │   PDBs   │ │
│  └──────────┬──────────┘  └─────────┬───────────┘  └────┬─────┘ │
│             └────────────┬──────────┴───────────────────┘        │
│                          ▼                                        │
│              ┌───────────────────────┐                           │
│              │ SharedInformer        │                           │
│              │ (client-go) + CM watch│                           │
│              └───────────────────────┘                           │
└─────────────────────────────────────────────────────────────────┘
```

All three use: **leader election** (one active replica), **shared informers** (efficient API caching), **ConfigMap watch** (hot-reload, no restart), **rate-limited logging**, and **strategic merge patch** (non-destructive updates).

---

## Controller comparison

| | PDB Controller | TSC Controller | JVM Probe Controller |
|---|---|---|---|
| Target resource | PodDisruptionBudget | topologySpreadConstraints | Container probes |
| Watches | Deployments, StatefulSets | Deployments, StatefulSets | Deployments, StatefulSets, Pods, Events |
| Default mode | **Live** (`FixPoorPDBs=true`) | Dry-run (`dryRun=true`) | Dry-run (`jvm-dryRun=true`) |
| Go-live action | none (already live) | patch `dryRun`→`false` | patch `jvm-dryRun`→`false` |
| Auto-fix | Poor PDB configs | N/A | Failing/slow-starting probes |
| Exclusion rules | regex (ns/name/labels) | regex (ns/name) | regex (ns/name) |
| Garbage collection | orphaned PDBs | TSC when replicas<2 | N/A |
| Leader election | yes | yes | yes |

---

## GitOps (ArgoCD / Flux)

These controllers patch Deployment/StatefulSet specs directly via JSON Patch. GitOps tools will detect drift and may revert changes, causing a reconciliation loop. Workarounds:

1. Add the relevant bypass annotation for GitOps-managed workloads (`workloads.cast.ai/jvm-probe-bypass`, `tsc-bypass`, or `bypass-default-pdb`).
2. Use annotation overrides to declare desired config declaratively in Git.
3. Run TSC/JVM in dry-run mode and apply changes via GitOps PRs.

---

## Manual / advanced install (per controller)

`install.sh` is recommended, but you can install a single controller directly from its local chart:

```bash
# TSC (dry-run by default)
helm install castai-tsc-controller \
  ./controllers/tsc-controller/helm/castai-tsc-controller \
  -n castai-agent --create-namespace

# JVM (dry-run by default)
helm install castai-jvm-probe-controller \
  ./controllers/jvm-probe-controller/helm/castai-jvm-probe-controller \
  -n castai-agent --create-namespace

# PDB (FixPoorPDBs=true by default → live)
helm install castai-pdb-controller \
  ./controllers/pdb-controller/helm/castai-pdb-controller \
  -n castai-agent --create-namespace
```

### Building images from source

```bash
./build-and-push.sh            # all controllers
# or per controller:
cd controllers/tsc-controller && make docker-build
cd controllers/jvm-probe-controller && make docker-build
cd controllers/pdb-controller && make docker-build
```

---

## Monitoring

```bash
# Logs
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-tsc-controller
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-jvm-probe-controller
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-pdb-controller

# Kubernetes events emitted by the controllers
kubectl get events --field-selector reason=TSCAdded
kubectl get events --field-selector reason=ProbesAdded
kubectl get events --field-selector reason=ProbeFixApplied
kubectl get events --field-selector reason=PDBCreated
```

---

## Uninstall

```bash
# Remove the controllers
helm uninstall castai-tsc-controller      -n castai-agent
helm uninstall castai-jvm-probe-controller -n castai-agent
helm uninstall castai-pdb-controller     -n castai-agent

# Also remove CAST-created PDBs (optional cleanup)
kubectl get poddisruptionbudget --all-namespaces \
  -o custom-columns="NAMESPACE:.metadata.namespace,NAME:.metadata.name" \
  | awk '$2 ~ /^castai-.*-pdb$/ {print "kubectl delete poddisruptionbudget -n " $1 " " $2}' \
  | sh
```

---

## License

MIT License — see `LICENSE`.
