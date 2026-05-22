# CAST AI TSC Controller

The TSC (Topology Spread Constraints) Controller automatically injects TopologySpreadConstraints into Deployments and StatefulSets that are missing them, ensuring pods are distributed across zones and nodes for high availability.

## Overview

This controller watches for Deployments and StatefulSets in your cluster and automatically adds TopologySpreadConstraints to distribute pods evenly across topology domains (e.g., availability zones). This improves application availability and fault tolerance.

## Features

- **Zone-based spreading** by default (topology.kubernetes.io/zone)
- **Node-based spreading** support
- **Annotation-based overrides** per workload
- **Exclusion rules** via ConfigMap (regex-based)
- **Garbage collection** removes TSC when replicas < 2
- **Leader election** for HA with multiple replicas
- **Rate-limited logging** to prevent log spam
- **skipSingleReplica** option to skip workloads with only 1 replica

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                   TSC Controller                                │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  ┌─────────────────────┐     ┌─────────────────────────────┐   │
│  │  Deployment/STS     │     │    ConfigMap (Config)       │   │
│  │  Informer           │     │                             │   │
│  └──────────┬──────────┘     └──────────────┬──────────────┘   │
│             │                                │                   │
│             ▼                                ▼                   │
│  ┌─────────────────────────────────────────────────────────────┐│
│  │                   Reconcile Loop                            ││
│  │  1. Check if TSC already exists                             ││
│  │  2. Evaluate exclusions                                     ││
│  │  3. Apply default constraints                               ││
│  │  4. Apply annotation overrides                              ││
│  │ 5. Patch workload with TSC                                  ││
│  └─────────────────────────────────────────────────────────────┘│
│                              │                                   │
│             ┌────────────────┼────────────────┐                 │
│             ▼                ▼                ▼                 │
│      ┌──────────┐     ┌──────────┐     ┌──────────┐            │
│      │ Deploy   │     │ Stateful │     │  Audit   │            │
│      │          │     │ Set      │     │  Logs    │            │
│      └──────────┘     └──────────┘     └──────────┘            │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

## Installation

### Helm Installation

```bash
# Add CAST AI Helm repository (if available)
helm repo add castai https://charts.cast.ai

# Install TSC Controller
helm install castai-tsc-controller castai/castai-tsc-controller \
  --namespace castai-agent \
  --create-namespace
```

### Custom Installation

```bash
# Clone the repository
cd workload-autopilot/controllers/tsc-controller/helm

# Install with custom values
helm install castai-tsc-controller ./castai-tsc-controller \
  --namespace castai-agent \
  --create-namespace \
  --set replicaCount=2 \
  --set config.defaultConstraints.maxSkew=1
```

## Configuration

### Values.yaml

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of controller replicas | `2` |
| `image.repository` | Controller image repository | `castai/tsc-controller` |
| `image.tag` | Controller image tag | `latest` |
| `config.defaultConstraints.maxSkew` | Maximum pod skew between topology domains | `1` |
| `config.defaultConstraints.topologyKey` | Topology key for spreading | `topology.kubernetes.io/zone` |
| `config.defaultConstraints.whenUnsatisfiable` | Action when constraints can't be met | `DoNotSchedule` |
| `config.skipSingleReplica` | Skip workloads with only 1 replica | `true` |
| `config.logInterval` | Rate limit interval for logs | `15m` |
| `config.reconcileInterval` | Reconcile scan interval | `2m` |
| `config.garbageCollectInterval` | Garbage collection interval | `5m` |
| `config.exclusions` | List of exclusion rules | `[]` |

### Example values.yaml

```yaml
replicaCount: 2

image:
  repository: castai/tsc-controller
  tag: v1.0.0

config:
  defaultConstraints:
    maxSkew: 1
    topologyKey: "topology.kubernetes.io/zone"
    whenUnsatisfiable: "DoNotSchedule"
  skipSingleReplica: true
  logInterval: "15m"
  reconcileInterval: "2m"
  garbageCollectInterval: "5m"
  exclusions:
    - namespaceRegex: "^kube-.*"
    - nameRegex: "^coredns-.*"
```

## Annotations

The TSC Controller supports per-workload configuration via annotations:

| Annotation | Description | Example |
|------------|-------------|---------|
| `workloads.cast.ai/tsc-bypass` | Skip this workload | `"true"` |
| `workloads.cast.ai/tsc-maxSkew` | Override maxSkew | `"2"` |
| `workloads.cast.ai/tsc-topologyKey` | Override topology key | `"kubernetes.io/hostname"` |
| `workloads.cast.ai/tsc-whenUnsatisfiable` | Override policy | `"ScheduleAnyway"` |

### Examples

#### Override maxSkew for specific deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  annotations:
    workloads.cast.ai/tsc-maxSkew: "2"
spec:
  replicas: 3
  # ...
```

#### Use node-based spreading instead of zone-based

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  annotations:
    workloads.cast.ai/tsc-topologyKey: "kubernetes.io/hostname"
spec:
  replicas: 3
  # ...
```

#### Skip single replica workloads

The controller automatically skips workloads with only 1 replica (configurable via `skipSingleReplica`):

```yaml
# This deployment will NOT get TSC because it has only 1 replica
apiVersion: apps/v1
kind: Deployment
metadata:
  name: single-replica-app
spec:
  replicas: 1
  # ...
```

#### Bypass controller completely

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  annotations:
    workloads.cast.ai/tsc-bypass: "true"
spec:
  # This workload will be ignored by the controller
  # ...
```

## How skipSingleReplica Works

The `skipSingleReplica` option (enabled by default) causes the controller to skip workloads with only 1 replica. This is useful because:

1. **No benefit**: Single replica workloads don't benefit from topology spread since there's only one pod
2. **Unnecessary patches**: Adding TSC to single-replica workloads creates unnecessary API server load
3. **Cleaner resources**: Avoids adding constraints that can never be satisfied (maxSkew=1 requires at least 2 pods in each topology domain)

The controller monitors replica count changes and will:
- Skip adding TSC when replicas < 2
- Garbage collect existing TSC when replicas are scaled down to 1

## Monitoring

### Logs

```bash
# View controller logs
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-tsc-controller

# Follow logs in real-time
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-tsc-controller -f
```

### Events

```bash
# Watch for TSC-related events
kubectl get events --field-selector reason=TSCAdded

# Watch for all controller events
kubectl get events -n castai-agent --field-selector involvedObject.kind=Deployment | grep TSC
```

## Upgrading

```bash
# Upgrade to a new version
helm upgrade castai-tsc-controller castai/castai-tsc-controller \
  --namespace castai-agent \
  --set image.tag=v1.1.0
```

## Uninstalling

```bash
# Uninstall the controller
helm uninstall castai-tsc-controller -n castai-agent

# Or if installed from local chart
helm uninstall castai-tsc-controller -n castai-agent
```

## Troubleshooting

### Controller not injecting TSC

1. Check if the workload is in the exclusion list
2. Verify replicas > 1 (or `skipSingleReplica` is disabled)
3. Check controller logs for exclusion reasons
4. Verify the ConfigMap exists and is correct

### TSC not working as expected

1. Check pod topology spread constraints:
   ```bash
   kubectl get deployment <name> -o jsonpath='{.spec.template.spec.topologySpreadConstraints}'
   ```
2. Verify topology keys match your cluster labels
3. Check if `whenUnsatisfiable` is set appropriately

## License

MIT License - See LICENSE file for details.