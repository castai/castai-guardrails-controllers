# PDB Controller

Automatically manages PodDisruptionBudgets (PDBs) for Deployments and StatefulSets.

## Overview

The PDB Controller watches for Deployments and StatefulSets with 2 or more replicas and automatically creates/maintains PDBs. It prevents node drains from blocking by ensuring workloads have appropriate disruption budgets configured.

## Features

- **Automatic PDB Creation**: Creates PDBs for Deployments and StatefulSets with ≥2 replicas
- **Annotation Overrides**: Customize PDB behavior per workload via annotations
- **Poor PDB Detection**: Detects and optionally fixes overly restrictive PDBs
- **Multiple PDB Handling**: Removes redundant castai PDBs when user-defined PDBs exist
- **Garbage Collection**: Removes orphaned PDBs when workloads are deleted
- **Leader Election**: Multiple replicas with leader election for HA
- **Exclusion Rules**: Regex-based filtering by namespace, name, and labels

## Annotations

| Annotation | Description | Example |
|------------|-------------|---------|
| `workloads.cast.ai/pdb-minAvailable` | Minimum pods available | `"1"` or `"50%"` |
| `workloads.cast.ai/pdb-maxUnavailable` | Maximum pods unavailable | `"1"` or `"50%"` |
| `workloads.cast.ai/pdb-unhealthyPodEvictionPolicy` | Eviction policy | `"IfHealthyBudget"` or `"AlwaysAllow"` |
| `workloads.cast.ai/bypass-default-pdb` | Skip PDB creation | `"true"` |

## ConfigMap Configuration

Create a ConfigMap named `castai-pdb-controller-config` in the `castai-agent` namespace:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: castai-pdb-controller-config
  namespace: castai-agent
data:
  # Default PDB values (only one can be set)
  defaultMinAvailable: "1"
  # defaultMaxUnavailable: "1"
  
  # Enable automatic fixing of poor PDBs
  FixPoorPDBs: "true"
  
  # Unhealthy pod eviction policy (Kubernetes 1.26+)
  defaultUnhealthyPodEvictionPolicy: "IfHealthyBudget"
  
  # Log level: debug, info, warn, error
  logLevel: "info"
  
  # Interval settings
  logInterval: "15m"
  pdbScanInterval: "2m"
  garbageCollectInterval: "2m"
  pdbDumpInterval: "5m"
  
  # Exclusion rules (YAML array)
  exclusions: |
    - namespaceRegex: "kube-.*"
      nameRegex: ""
      labels: {}
    - namespaceRegex: ""
      nameRegex: "critical-.*"
      labels:
        app: "database"
```

## Poor PDB Configuration

The controller detects and can automatically fix overly restrictive PDBs:

- `minAvailable` equal to replica count
- `minAvailable` set to `100%`
- `maxUnavailable` set to `0`
- `maxUnavailable` set to `0%`

Enable auto-fix with `FixPoorPDBs: "true"` in the ConfigMap.

## Building

```bash
# Build binary
make build

# Run locally
make run

# Build Docker image
make docker-build IMAGE_TAG=latest

# Push Docker image
make docker-push IMAGE_TAG=latest
```

## Deployment

1. Apply RBAC:
```bash
kubectl apply -f rbac.yaml
```

2. Create ConfigMap (optional):
```bash
kubectl apply -f configmap.yaml
```

3. Deploy controller:
```bash
kubectl apply -f deployment.yaml
```

## Architecture

The controller uses Kubernetes informers to watch for:
- Deployments and StatefulSets (create/update/delete)
- ConfigMap changes for configuration updates

It runs periodic scans for:
- Poor PDB configuration detection
- Multiple PDB detection
- Orphaned PDB garbage collection
- PDB dump to file

Leader election ensures only one replica actively manages PDBs at a time.
