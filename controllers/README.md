# CAST AI Workload Controllers

Three Kubernetes controllers for automatic workload management, following the [castai-pdb-controller](https://github.com/castai/castai-pdb-controller) pattern.

## Controllers

### 1. TopologySpreadConstraints Controller (`tsc-controller`)

Automatically injects TopologySpreadConstraints into Deployments and StatefulSets that are missing them, ensuring pods are distributed across zones and nodes for high availability.

#### Features
- **Zone-based spreading** by default (topology.kubernetes.io/zone)
- **Annotation-based overrides** per workload
- **Exclusion rules** via ConfigMap (regex-based)
- **Garbage collection** removes TSC when replicas < 2
- **Leader election** for HA
- **Rate-limited logging** to prevent log spam

#### Annotations

| Annotation | Description | Example |
|------------|-------------|---------|
| `workloads.cast.ai/tsc-bypass` | Skip this workload | `"true"` |
| `workloads.cast.ai/tsc-maxSkew` | Override maxSkew | `"1"` |
| `workloads.cast.ai/tsc-topologyKey` | Override topology key | `"topology.kubernetes.io/zone"` |
| `workloads.cast.ai/tsc-whenUnsatisfiable` | Override policy | `"DoNotSchedule"` or `"ScheduleAnyway"` |

**Example:**
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  annotations:
    workloads.cast.ai/tsc-maxSkew: "2"
    workloads.cast.ai/tsc-topologyKey: "kubernetes.io/hostname"
```

---

### 2. JVM Probe Controller (`jvm-probe-controller`)

Automatically detects JVM-based containers and injects appropriate health probes (liveness, readiness, **startup**) when missing. Includes **automatic probe failure detection** and **auto-fix capabilities**.

#### Features
- **Framework detection** (Spring Boot, Quarkus, Micronaut, generic JVM)
- **Image-based detection** (java, jdk, openjdk, spring, etc.)
- **Environment variable detection** (JAVA_HOME, SPRING_PROFILES_ACTIVE)
- **Configurable probe settings** via ConfigMap
- **Annotation-based overrides**
- **Probe framework paths** automatically set based on detected framework
- **🆕 Startup probe always injected** for JVM containers (prevents premature termination)
- **🆕 Automatic probe failure monitoring** - watches pod events for probe failures
- **🆕 Auto-fix poor probe configurations** - adjusts delays/thresholds based on failure patterns
- **🆕 Force overwrite existing probes** - replace badly configured probes

#### Detected Frameworks

| Framework | Liveness | Readiness | Startup |
|-----------|----------|-----------|---------|
| Spring Boot | `/actuator/health/liveness` | `/actuator/health/readiness` | `/actuator/health` |
| Quarkus | `/q/health/live` | `/q/health/ready` | `/q/health/started` |
| Micronaut | `/health/liveness` | `/health/readiness` | `/health` |
| Generic JVM | TCP socket | TCP socket | TCP socket |

#### Annotations

| Annotation | Description | Example |
|------------|-------------|---------|
| `workloads.cast.ai/jvm-probe-bypass` | Skip this workload | `"true"` |
| `workloads.cast.ai/jvm-probe-framework` | Force framework detection | `"spring-boot"`, `"quarkus"`, `"micronaut"`, `"generic"` |
| `workloads.cast.ai/jvm-probe-port` | Override port | `"8080"` |
| `workloads.cast.ai/jvm-probe-initial-delay` | Initial delay seconds | `"60"` |
| `workloads.cast.ai/jvm-probe-overwrite-all` | **Force overwrite all probes** | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-liveness` | Force overwrite liveness probe | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-readiness` | Force overwrite readiness probe | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-startup` | Force overwrite startup probe | `"true"` |
| `workloads.cast.ai/jvm-probe-log-failures` | **Enable detailed failure logging** | `"true"` |

**Example - Basic Usage:**
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-java-app
  annotations:
    workloads.cast.ai/jvm-probe-framework: "spring-boot"
    workloads.cast.ai/jvm-probe-port: "8081"
```

**Example - Overwriting Bad Probes:**
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-legacy-app
  annotations:
    workloads.cast.ai/jvm-probe-overwrite-all: "true"  # Replace all existing probes
    workloads.cast.ai/jvm-probe-log-failures: "true"   # Log probe failures for visibility
```

---

## Architecture

Both controllers follow the [castai-pdb-controller](https://github.com/castai/castai-pdb-controller) pattern:

```
┌─────────────────────────────────────────────────────────────────┐
│                    CAST AI Workload Controllers                 │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  ┌─────────────────────┐     ┌─────────────────────────────┐   │
│  │   TSC Controller    │     │    JVM Probe Controller     │   │
│  ├─────────────────────┤     ├─────────────────────────────┤   │
│  │ • Watch Deployments │     │ • Watch Deployments         │   │
│  │ • Watch StatefulSets│     │ • Watch StatefulSets        │   │
│  │ • Inject TSC        │     │ • Detect JVM containers     │   │
│  │ • Leader Election   │     │ • Inject probes             │   │
│  │ • Config Hot-Reload │     │ • Leader Election           │   │
│  └─────────────────────┘     └─────────────────────────────┘   │
│           │                            │                        │
│           └────────────┬───────────────┘                        │
│                        ▼                                        │
│              ┌───────────────────┐                              │
│              │  SharedInformer   │                              │
│              │  (client-go)      │                              │
│              └───────────────────┘                              │
│                        │                                        │
│           ┌────────────┼────────────┐                          │
│           ▼            ▼            ▼                          │
│    ┌──────────┐  ┌──────────┐  ┌──────────┐                   │
│    │ConfigMaps│  │ Deploys  │  │Stateful  │                   │
│    │          │  │          │  │ Sets     │                   │
│    └──────────┘  └──────────┘  └──────────┘                   │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### Key Components

1. **Leader Election**: Only one pod acts as leader at a time
2. **Shared Informers**: Efficient caching of API server responses
3. **Rate-Limited Logging**: Prevents log spam via throttling
4. **ConfigMap Watch**: Hot-reload of configuration without restart
5. **Strategic Merge Patch**: Non-destructive updates to existing workloads

---

## Installation

### Prerequisites
- Kubernetes 1.21+
- kubectl configured with cluster access

### Quick Install
```bash
kubectl apply -f manifests/
```

This creates:
- Namespace: `castai-agent`
- ServiceAccount: `castai-workload-controllers`
- ClusterRole & ClusterRoleBinding
- ConfigMaps for both controllers
- Deployments (2 replicas each with leader election)

### Building from Source

```bash
# Build TSC Controller
cd tsc-controller
go mod download
go build -o tsc-controller ./cmd/

# Build JVM Probe Controller  
cd ../jvm-probe-controller
go mod download
go build -o jvm-probe-controller ./cmd/

# Docker build
cd ../tsc-controller
make docker-build

cd ../jvm-probe-controller
make docker-build
```

---

## Configuration

### TSC Controller ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: castai-tsc-controller-config
  namespace: castai-agent
data:
  defaultConstraints: |-
    [
      {
        "maxSkew": 1,
        "topologyKey": "topology.kubernetes.io/zone",
        "whenUnsatisfiable": "DoNotSchedule"
      }
    ]
  logInterval: "15m"
  reconcileInterval: "2m"
  garbageCollectInterval: "5m"
  exclusions: |-
    [
      {"namespaceRegex": "^kube-.*"},
      {"nameRegex": "^coredns"}
    ]
```

### JVM Probe Controller ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: castai-jvm-probe-controller-config
  namespace: castai-agent
data:
  # Framework configurations - startup probes always injected for JVM
  frameworks: |-
    {
      "spring-boot": {
        "livenessPath": "/actuator/health/liveness",
        "readinessPath": "/actuator/health/readiness",
        "startupPath": "/actuator/health",
        "initialDelaySeconds": 60,
        "startupProbeMultiplier": 3
      }
    }
  requireBothProbes: "false"   # Will inject any missing probe
  alwaysInjectStartupProbe: "true"  # Critical for JVM slow-start
  # Probe auto-fix settings
  probeAutoFixEnabled: "true"
  failureThresholdBeforeFix: "5"
  timeWindowForFailures: "5m"
  maxInitialDelaySeconds: "300"
  maxFailureThreshold: "10"
  exclusions: |-
    [
      {"namespaceRegex": "^kube-system$"}
    ]
```

### JVM Probe Failure Monitoring & Auto-Fix

The JVM Probe Controller includes a sophisticated **Pod Event Monitor** that:

1. **Watches Kubernetes Events** for probe failures (`Unhealthy`, `ProbeFailed`)
2. **Tracks Container Restarts** due to probe failures
3. **Analyzes Failure Patterns** to determine optimal probe settings
4. **Automatically Fixes Poor Probes** by adjusting:
   - `initialDelaySeconds` (up to 300s max)
   - `failureThreshold` (up to 10 max)
   - `timeoutSeconds`

**Failure Detection Triggers:**
- ≥5 probe failures in 5 minutes
- ≥3 container restarts
- High failure rate (>10/min)

**When triggered, you'll see logs like:**
```
╔════════════════════════════════════════════════════════════════╗
║              PROBE FIX APPLIED                                 ║
╠════════════════════════════════════════════════════════════════╣
║ Workload: default/my-app
║ Container: spring-boot
║ Probe Type: liveness
║ Reason: High failure rate - container needs more startup time
║ Changes:
║   InitialDelay: 30 → 120 seconds
║   FailureThreshold: 3 → 6
╚════════════════════════════════════════════════════════════════╝
```

**Failure Logging Format:**
```
[PROBE FAILURE] default/my-app container=spring-boot probe=liveness count=5
[CONTAINER RESTART] default/my-app-xxx container=spring-boot restarts=3
```

---

## Testing

### TSC Controller Test

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-tsc
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app: test-tsc
  template:
    metadata:
      labels:
        app: test-tsc
    spec:
      containers:
        - name: nginx
          image: nginx:alpine
```

After deployment, check:
```bash
kubectl get deployment test-tsc -o jsonpath='{.spec.template.spec.topologySpreadConstraints}'
```

### JVM Probe Controller Test

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-jvm
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test-jvm
  template:
    metadata:
      labels:
        app: test-jvm
    spec:
      containers:
        - name: spring
          image: openjdk:17  # JVM-based image
          env:
            - name: SPRING_PROFILES_ACTIVE
              value: "production"
```

After deployment, check:
```bash
kubectl get deployment test-jvm -o jsonpath='{.spec.template.spec.containers[0].livenessProbe}'
```

---

### 3. PDB Controller (`pdb-controller`)

Automatically manages PodDisruptionBudgets (PDBs) for Deployments and StatefulSets with ≥2 replicas, ensuring workloads have appropriate disruption budgets configured to prevent node drain blocking.

#### Features
- **Automatic PDB Creation** for workloads with ≥2 replicas
- **Annotation overrides** per workload (minAvailable, maxUnavailable, eviction policy)
- **Poor PDB Detection** - identifies overly restrictive PDBs (minAvailable=100%, maxUnavailable=0%)
- **Auto-fix Poor PDBs** - deletes and recreates with defaults when FixPoorPDBs is enabled
- **Multiple PDB Handling** - removes redundant castai PDBs when user-defined PDBs exist
- **Garbage Collection** - removes orphaned PDBs when workloads are deleted
- **Leader election** for HA
- **Exclusion rules** via ConfigMap (regex-based namespace/name/label filtering)
- **Rate-limited logging** to prevent log spam

#### Annotations

| Annotation | Description | Example |
|------------|-------------|---------|
| `workloads.cast.ai/pdb-minAvailable` | Minimum pods available | `"1"` or `"50%"` |
| `workloads.cast.ai/pdb-maxUnavailable` | Maximum pods unavailable | `"1"` or `"50%"` |
| `workloads.cast.ai/pdb-unhealthyPodEvictionPolicy` | Eviction policy | `"IfHealthyBudget"` or `"AlwaysAllow"` |
| `workloads.cast.ai/bypass-default-pdb` | Skip PDB creation | `"true"` |

**Example:**
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  annotations:
    workloads.cast.ai/pdb-minAvailable: "50%"
```

#### PDB Controller ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: castai-pdb-controller-config
  namespace: castai-agent
data:
  defaultMinAvailable: "1"
  # defaultMaxUnavailable: "1"  # Only one can be set
  FixPoorPDBs: "true"
  defaultUnhealthyPodEvictionPolicy: "IfHealthyBudget"
  logLevel: "info"
  logInterval: "15m"
  pdbScanInterval: "2m"
  garbageCollectInterval: "2m"
  pdbDumpInterval: "5m"
  exclusions: |-
    - namespaceRegex: "kube-.*"
      nameRegex: ""
      labels: {}
```

---

## Monitoring

### Logs
```bash
# TSC Controller logs
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-tsc-controller

# JVM Controller logs
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-jvm-probe-controller

# PDB Controller logs
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-pdb-controller
```

### Metrics
All controllers emit Kubernetes events:
```bash
kubectl get events --field-selector reason=TSCAdded
kubectl get events --field-selector reason=ProbesAdded
kubectl get events --field-selector reason=PDBCreated
```

---

## Controller Comparison

| Feature | PDB Controller | TSC Controller | JVM Probe Controller |
|---------|----------------|----------------|---------------------|
| Target Resource | **PodDisruptionBudget** | **TopologySpreadConstraints** | **Container Probes** |
| Watches | Deployments, StatefulSets | Deployments, StatefulSets | Deployments, StatefulSets, Pods, Events |
| Leader Election | Yes | Yes | Yes |
| ConfigMap Config | Yes | Yes | Yes |
| Rate-Limited Logging | Yes | Yes | Yes |
| Garbage Collection | Yes (orphaned PDBs) | Yes (replicas < 2) | Yes (auto-fix of failing probes) |
| Reconciliation Loop | Yes | Yes | Yes |
| **Auto-Fix Capabilities** | **Poor PDB configs** | N/A | **Failing probes** |
| **Exclusion Rules** | Yes (regex-based) | Yes (regex-based) | Yes (regex-based) |
| **Startup Probes** | N/A | N/A | Always injected for JVM |
| **Failure Monitoring** | N/A | N/A | Event-based detection |
| **Force Overwrite** | N/A | N/A | Annotation-based |

---

## Helm Installation

All three controllers are available as Helm charts for easy deployment.

### Quick Install (All Controllers)

```bash
# Create namespace
kubectl create namespace castai-agent

# Install all controllers
kubectl apply -f manifests/
```

### Individual Helm Charts

#### TSC Controller

```bash
# Install from local chart
cd tsc-controller/helm/castai-tsc-controller
helm install castai-tsc-controller ./castai-tsc-controller \
  --namespace castai-agent \
  --create-namespace
```

For detailed configuration, see [tsc-controller/README.md](tsc-controller/README.md).

#### PDB Controller

```bash
# Install from local chart
cd pdb-controller/helm/castai-pdb-controller
helm install castai-pdb-controller ./castai-pdb-controller \
  --namespace castai-agent \
  --create-namespace
```

For detailed configuration, see [pdb-controller/README.md](pdb-controller/README.md).

#### JVM Probe Controller

```bash
# Install from local chart
cd jvm-probe-controller/helm/castai-jvm-probe-controller
helm install castai-jvm-probe-controller ./castai-jvm-probe-controller \
  --namespace castai-agent \
  --create-namespace
```

For detailed configuration, see [jvm-probe-controller/README.md](jvm-probe-controller/README.md).

### Helm Values Examples

#### TSC Controller

```yaml
# values-tsc.yaml
replicaCount: 2

config:
  defaultConstraints:
    maxSkew: 1
    topologyKey: "topology.kubernetes.io/zone"
    whenUnsatisfiable: "DoNotSchedule"
  skipSingleReplica: true
  logInterval: "15m"
  reconcileInterval: "2m"
  garbageCollectInterval: "5m"
```

#### PDB Controller

```yaml
# values-pdb.yaml
replicaCount: 2

config:
  defaultPDB:
    minAvailable: 1
    unhealthyPodEvictionPolicy: "IfHealthyBudget"
  enableForkPattern: true
  detectPoorPDBs: true
  fixPoorPDBs: true
  skipSingleReplica: true
```

#### JVM Probe Controller

```yaml
# values-jvm-probe.yaml
replicaCount: 2

config:
  probeDefaults:
    liveness:
      initialDelaySeconds: 60
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
    readiness:
      initialDelaySeconds: 30
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
    startup:
      initialDelaySeconds: 10
      periodSeconds: 5
      timeoutSeconds: 3
      failureThreshold: 30
  autoFixMode: true
  autoFixThreshold: 3
  maxInitialDelay: 300
  alwaysInjectStartupProbe: true
```

## License

MIT License - See LICENSE file for details.
