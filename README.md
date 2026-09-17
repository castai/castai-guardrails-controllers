# CAST AI Workload Controllers

> **Disclaimer**
>
> This repository is **open-source software** and is **not part of the CAST AI product** or a CAST AI commercial offering. It was built by engineers to close real-world reliability gaps — the kind that usually show up when workloads are created quickly, without Kubernetes best practices fully in place.
>
> TSC and JVM run as **Pod mutating admission webhooks**: they inject `topologySpreadConstraints` and JVM health probes at Pod creation time, without modifying Deployment or StatefulSet specs. PDB manages `PodDisruptionBudget` resources and does not modify workload specs either. Because mutations take effect when Pods are created, your rollout strategy matters: a workload using `Recreate` will recycle when a rollout triggers new Pods. For TSC, start in **recommend** mode (snapshot-only) before switching to **apply** mode.

Three Kubernetes controllers that **automatically remediate workload configuration** in any cluster. **TSC and JVM** run as **Pod mutating admission webhooks** and inject missing `topologySpreadConstraints` and JVM health probes when Pods are created. **PDB** watches Deployments and StatefulSets and manages `PodDisruptionBudget`s. Together they help workloads spread safely, drain cleanly, and start healthily — without modifying parent workload specs.

| Controller | What it fixes | Default mode |
|---|---|---|
| **TSC Controller** | Missing `topologySpreadConstraints` (via Pod admission webhook) | **Recommend** (snapshot-only) |
| **JVM Probe Controller** | Missing JVM **startup** probes (via Pod admission webhook) | **Apply** (mutating webhook) |
| **PDB Controller** | Missing/poor `PodDisruptionBudget`s | **Live** (`FixPoorPDBs=true`) |

> **Admission webhook architecture:** TSC and JVM no longer patch Deployments or StatefulSets. They run as **Pod mutating admission webhooks**, injecting `topologySpreadConstraints` and JVM startup probes at Pod creation time. This means changes take effect at the **Pod level**, making the controllers fully compatible with GitOps/ArgoCD because the parent workload specs remain unchanged.

PDB follows the [castai-pdb-controller](https://github.com/castai/castai-pdb-controller) pattern: leader election, shared informers, rate-limited logging, ConfigMap-driven config with hot-reload, and strategic-merge-patch (non-destructive) updates to `PodDisruptionBudget` resources. TSC and JVM run as **mutating admission webhooks**: they receive Pod CREATE requests from the API server and return JSON Patches, so they never modify the parent Deployment or StatefulSet resource.

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

Pick one or several (e.g. `1 3`). TSC can run in **apply** or **recommend** mode (default **recommend**); the JVM Probe Controller installs as a live mutating webhook; **PDB installs live**. The script installs via Helm and waits for rollout.

### Non-interactive (CI / automation)

```bash
# Install all three
INSTALL_TSC=true INSTALL_JVM=true INSTALL_PDB=true ./install.sh

# Selective install
INSTALL_TSC=true INSTALL_PDB=true ./install.sh

# Override image tag (defaults to each chart's appVersion) and TSC mode
INSTALL_TSC=true INSTALL_JVM=true TSC_IMAGE_TAG=v1.2.3 TSC_MODE=apply ./install.sh

# Install JVM with the mutating admission webhook disabled
INSTALL_JVM=true JVM_WEBHOOK_ENABLED=false ./install.sh
```

| Env var | Purpose | Default |
|---|---|---|
| `INSTALL_TSC` / `INSTALL_JVM` / `INSTALL_PDB` | Select controllers (non-interactive) | unset |
| `TSC_MODE` | TSC mode: `apply` (mutate Pods) or `recommend` (snapshot-only) | `recommend` |
| `TSC_WEBHOOK_ENABLED` | Register the TSC Pod admission webhook (`true`/`false`) | `true` |
| `JVM_WEBHOOK_ENABLED` | Register the JVM Pod admission webhook (`true`/`false`) | `true` |
| `TSC_IMAGE_TAG` / `JVM_IMAGE_TAG` / `PDB_IMAGE_TAG` | Override image tag | chart `appVersion` |
| `NAMESPACE` | Target namespace | `castai-agent` |
| `IMAGE_PULL_POLICY` | Container image pull policy | `IfNotPresent` |

> **PDB has no dry-run mode.** `PDB_DRY_RUN` is ignored; the controller always installs with `config.FixPoorPDBs="true"` (live). See [PDB Controller](#3-pdb-controller-pdb-controller) below.

### What `install.sh` does, step by step

1. Pre-flights `kubectl`, `helm 3.14+`, `jq`.
2. Detects current kubectl context + cluster name.
3. (Interactive) Asks which controllers to install and whether the TSC controller runs in `apply` or `recommend` mode.
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

### Controller modes — TSC

TSC installs in **recommend** mode by default. In this mode the controller captures snapshots of your workloads but does **not** mutate Pods. To make it actually inject `topologySpreadConstraints`, switch to **apply** mode by patching its ConfigMap — the controller hot-reloads the ConfigMap, so **no restart is needed**:

```bash
# Enable apply mode (mutate Pods)
kubectl -n castai-agent patch cm castai-tsc-controller-config \
  --type merge -p '{"data":{"managementEnabled":"true","rollbackOnDisable":"false","mode":"apply"}}'
```

To disable patching and automatically roll back changes made by the controller:

```bash
kubectl -n castai-agent patch cm castai-tsc-controller-config \
  --type merge -p '{"data":{"managementEnabled":"false","rollbackOnDisable":"true"}}'
```

To return to recommend mode (snapshot only):

```bash
kubectl -n castai-agent patch cm castai-tsc-controller-config \
  --type merge -p '{"data":{"managementEnabled":"true","mode":"recommend"}}'
```

**Verify snapshots**

```bash
kubectl get tscoriginals -n castai-agent
```

**Check rollback status**

```bash
kubectl get tscoriginals -n castai-agent \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="RolledBack")].status}{"\n"}{end}'
```

See [`docs/rollback-operator-runbook.md`](docs/rollback-operator-runbook.md) for the full runbook.

**PDB is already live** (`FixPoorPDBs=true` at install). To re-apply via Helm:

```bash
helm upgrade castai-pdb-controller \
  ./controllers/pdb-controller/helm/castai-pdb-controller \
  -n castai-agent --set config.FixPoorPDBs="true"
```

### Controller modes — JVM Probe Controller

The JVM Probe Controller installs as a **Pod mutating admission webhook** and is enabled by default. It has no ConfigMap mode toggle; instead, you enable or disable mutations by registering or unregistering the webhook.

To disable the webhook (stop mutating Pods) without uninstalling the controller:

```bash
helm upgrade castai-jvm-probe-controller \
  ./controllers/jvm-probe-controller/helm/castai-jvm-probe-controller \
  -n castai-agent --set webhook.enabled=false
```

To re-enable it:

```bash
helm upgrade castai-jvm-probe-controller \
  ./controllers/jvm-probe-controller/helm/castai-jvm-probe-controller \
  -n castai-agent --set webhook.enabled=true
```

You can also install with the webhook disabled from the start:

```bash
INSTALL_JVM=true JVM_WEBHOOK_ENABLED=false ./install.sh
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

**What it does:** runs as a **Pod mutating admission webhook**. It injects `topologySpreadConstraints` into Pods created from Deployments and StatefulSets that are missing them, so pods spread across zones and nodes for high availability.

**Features**
- Pod CREATE admission webhook
- Zone-based spreading by default (`topology.kubernetes.io/zone`)
- Annotation-based overrides per workload
- Regex-based exclusion rules via ConfigMap
- Skips single-replica workloads
- Leader election for HA
- Rate-limited logging

**Config** (`controllers/tsc-controller/helm/castai-tsc-controller/values.yaml`):

| Key | Description | Default |
|---|---|---|
| `management.enabled` | Enable management (snapshot/rollback) | `true` |
| `management.mode` | `apply` (mutate Pods) or `recommend` (snapshot only) | `recommend` |
| `management.rollbackOnDisable` | Roll back controller changes when management is disabled | `false` |
| `config.defaultConstraints` | Default TSC: `maxSkew`, `topologyKey`, `whenUnsatisfiable` | zone / maxSkew 1 / DoNotSchedule |
| `config.skipSingleReplica` | Skip workloads with <2 replicas | `true` |
| `config.logInterval` | Rate-limit interval for repeated logs | `15m` |
| `config.reconcileInterval` | Reconcile loop interval | `2m` |
| `config.garbageCollectInterval` | GC interval | `5m` |
| `config.exclusions` | Regex rules (namespace/name) to skip | `[]` |
| `webhook.enabled` | Register the Pod admission webhook | `true` |
| `webhook.failurePolicy` | `Ignore` or `Fail` | `Ignore` |
| `certManager.enabled` | Use cert-manager for webhook TLS | `false` |

Rendered ConfigMap: `castai-tsc-controller-config`, keys `managementEnabled`, `mode`, `rollbackOnDisable`.

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

Verify on a running Pod:
```bash
kubectl get pod -l app=my-app -o jsonpath='{.items[0].spec.topologySpreadConstraints}'
```

---

### 2. JVM Probe Controller (`jvm-probe-controller`)

**What it does:** runs as a **Pod mutating admission webhook**. It detects JVM-based containers and, when a startup probe is missing, copies the existing liveness or readiness probe into a startup probe. This preserves each workload's unique health-check path, port, and handler type (`httpGet`, `tcpSocket`, `exec`, or `grpc`).

**Features**
- Framework detection: **Spring Boot, Quarkus, Micronaut, generic JVM**
- Detection via image name (word-boundary regex), env vars (`JAVA_HOME`, `SPRING_PROFILES_ACTIVE`, `JAVA_TOOL_OPTIONS`), and container command
- **Startup probe injected** for JVM containers by copying the existing liveness/readiness probe
- **Handler-type preservation** — `httpGet`, `tcpSocket`, `exec`, and `grpc` probes are copied unchanged
- **Named-port fallback** — HTTP/TCP probes prefer named container ports (`http`, `web`, `https`, `http-web`) so they survive container port-number changes
- **Automatic probe alignment** (opt-in) — when a startup probe is injected, removes redundant `initialDelaySeconds` from liveness/readiness and extends `failureThreshold` to cover the configured startup window
- **Autonomous per-Pod mutation** — each Pod gets a startup probe tailored to its own probe configuration
- Force-overwrite existing probes (per-probe or all)
- Liveness/readiness **opt-in** injection when missing
- Liveness probe **opt-in** by default (Spring Boot needs `management.endpoint.health.probes.enabled=true`)

**Detected frameworks → default probe paths**

| Framework | Default liveness | Default readiness | Default startup |
|---|---|---|---|
| Spring Boot | `/actuator/health/liveness` | `/actuator/health/readiness` | `/actuator/health` |
| Quarkus | `/q/health/live` | `/q/health/ready` | `/q/health/started` |
| Micronaut | `/health/liveness` | `/health/readiness` | `/health` |
| Generic JVM | TCP socket | TCP socket | TCP socket |

> **Spring Boot liveness** is opt-in (`injectLivenessProbe: "false"` by default) because `/actuator/health/liveness` only exists with `management.endpoint.health.probes.enabled=true` (Spring Boot 2.3+). Enable per-workload with `workloads.cast.ai/jvm-probe-inject-liveness: "true"`.

**Config** (`controllers/jvm-probe-controller/helm/castai-jvm-probe-controller/values.yaml`):

| Key | Description | Default |
|---|---|---|
| `webhook.enabled` | Enable the mutating webhook | `true` |
| `webhook.failurePolicy` | `Ignore` or `Fail` | `Ignore` |
| `webhook.timeoutSeconds` | Admission webhook timeout | `5` |
| `webhook.namespaceSelector` | Restrict which namespaces are mutated | `{}` |
| `webhook.objectSelector` | Restrict which Pods are mutated | `{}` |
| `certManager.enabled` | Use cert-manager for webhook TLS | `true` |
| `certManager.issuerRef.name` | cert-manager issuer name | `""` |
| `tls.manualSecret.enabled` | Use a manually-created TLS Secret | `false` |
| `config.logIntendedChanges` | Log intended changes | `true` |
| `config.injectLivenessProbe` | Inject liveness probe when missing | `false` (opt-in) |
| `config.injectReadinessProbe` | Inject readiness probe when missing | `true` |
| `config.injectStartupProbe` | Build startup probe when no liveness/readiness exists | `true` |
| `config.frameworks` | Per-framework paths/timing (JSON) | spring-boot/quarkus/micronaut/generic |
| `config.exclusions` | Regex rules to skip | `[]` |

Rendered ConfigMap: `castai-jvm-probe-controller-config`.

> **Probe alignment (opt-in).** Set ConfigMap key `jvm-alignProbes: "true"` to align liveness/readiness probes when a startup probe is injected: `initialDelaySeconds` is removed and `failureThreshold` is extended so `periodSeconds * failureThreshold` covers at least `jvm-minProbeWindowSeconds` (default `60`), capped at `jvm-maxFailureThreshold` (default `10`).

**Annotations** (on the Pod template)

| Annotation | Description | Example |
|---|---|---|
| `workloads.cast.ai/jvm-probe-bypass` | Skip this workload | `"true"` |
| `workloads.cast.ai/jvm-probe-framework` | Force framework detection | `"spring-boot"` |
| `workloads.cast.ai/jvm-probe-port` | Override port | `"8080"` |
| `workloads.cast.ai/jvm-probe-initial-delay` | Initial delay seconds | `"60"` |
| `workloads.cast.ai/jvm-probe-startup-period` | Startup probe period seconds | `"15"` |
| `workloads.cast.ai/jvm-probe-startup-failure-threshold` | Startup probe failure threshold | `"40"` |
| `workloads.cast.ai/jvm-probe-clear-delays` | Remove liveness/readiness `initialDelaySeconds` | `"true"` |
| `workloads.cast.ai/jvm-probe-align` | Enable automatic liveness/readiness probe alignment | `"true"`/`"false"` |
| `workloads.cast.ai/jvm-probe-overwrite-all` | Force overwrite all probes | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-liveness` | Overwrite liveness | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-readiness` | Overwrite readiness | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-startup` | Overwrite startup | `"true"` |
| `workloads.cast.ai/jvm-probe-inject-liveness` | Override liveness injection | `"true"`/`"false"` |
| `workloads.cast.ai/jvm-probe-inject-readiness` | Override readiness injection | `"true"`/`"false"` |
| `workloads.cast.ai/jvm-probe-inject-startup` | Override startup injection | `"true"`/`"false"` |

**How the mutation works**

For each JVM container in a new Pod:

1. If a liveness probe exists, copy it to `startupProbe`.
2. Else if a readiness probe exists, copy it to `startupProbe`.
3. Else if startup injection is enabled, build a startup probe from the detected framework defaults.
4. Tune the startup probe: remove `initialDelaySeconds`, set `periodSeconds=15`, `failureThreshold=40`, `successThreshold=1`.
5. Named-port fallback: if the copied probe is `httpGet`/`tcpSocket` and the container exposes a named port (`http`, `web`, `https`, or `http-web`), use the port name so the probe survives container port-number changes.
6. If probe alignment is enabled, remove `initialDelaySeconds` from liveness/readiness and extend `failureThreshold` so `periodSeconds * failureThreshold` covers the configured startup window.

Because the startup probe is a copy, the original handler (`httpGet`, `tcpSocket`, `exec`, or `grpc`) and path/port are preserved.

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
          livenessProbe:
            httpGet:
              path: /actuator/health/liveness
              port: 8081
```

Verify the resulting Pod:
```bash
kubectl get pod -l app=my-java-app -o jsonpath='{.items[0].spec.containers[0].startupProbe}'
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
│  │ • Pod webhook       │  │ • Pod webhook       │  │ • Watch  │ │
│  │ • Inject TSC        │  │ • Copy liveness/    │  │   Deploys│ │
│  │ • Leader election   │  │   readiness →       │  │   /STS   │ │
│  │                     │  │   startupProbe      │  │ • Fix    │ │
│  │                     │  │ • Preserves handler │  │   poor   │ │
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

PDB uses: **leader election** (one active replica), **shared informers** (efficient API caching), **ConfigMap watch** (hot-reload, no restart), **rate-limited logging**, and **strategic merge patch** (non-destructive) updates to `PodDisruptionBudget` resources.

TSC and JVM use a **mutating admission webhook**: they receive Pod CREATE requests from the API server and return JSON Patches. They do not watch or patch Deployments/StatefulSets, so they are GitOps-friendly and never cause ArgoCD drift on parent workload specs.

---

## Controller comparison

| | PDB Controller | TSC Controller | JVM Probe Controller |
|---|---|---|---|
| Target resource | PodDisruptionBudget | `topologySpreadConstraints` on Pods | Container startup probes |
| Trigger | Deployments, StatefulSets | Pod CREATE admission requests | Pod CREATE admission requests |
| Default mode | **Live** (`FixPoorPDBs=true`) | Recommend (`mode=recommend`) | **Apply** (webhook) |
| Go-live action | none (already live) | patch `mode`→`apply` | webhook enabled by default |
| Modifies parent workload | no (creates PDBs) | **no** (mutates Pod only) | **no** (mutates Pod only) |
| GitOps/ArgoCD drift | possible (PDBs managed out-of-band) | **none** | **none** |
| Exclusion rules | regex (ns/name/labels) | regex (ns/name) + webhook selectors | regex (ns/name) + webhook selectors |
| Garbage collection | orphaned PDBs | N/A | N/A |
| Leader election | yes | yes | no |

---

## GitOps (ArgoCD / Flux)

The **TSC and JVM Probe Controllers do not cause GitOps drift**: they mutate Pods at admission time, so the parent Deployment/StatefulSet spec in Git remains unchanged and ArgoCD/Flux stay in sync.

The **PDB Controller can cause GitOps drift** because it manages `PodDisruptionBudget` resources that may not be declared in your Git repository. GitOps tools may show those PDBs as out-of-sync.

Workarounds for PDB:

1. Add the bypass annotation for GitOps-managed workloads (`workloads.cast.ai/bypass-default-pdb`).
2. Declare the desired PDB declaratively in Git (use annotation overrides such as `workloads.cast.ai/pdb-minAvailable`).

---

## Manual / advanced install (per controller)

`install.sh` is recommended, but you can install a single controller directly from its local chart:

```bash
# TSC (recommend mode by default — snapshot only; webhook installed but not mutating)
helm install castai-tsc-controller \
  ./controllers/tsc-controller/helm/castai-tsc-controller \
  -n castai-agent --create-namespace

# JVM (apply/webhook mode by default)
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
