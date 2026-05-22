# Cluster Status - CAST AI Controller Test Environment

## Current Cluster State

### Nodes (5 total)
| Node | Status | IP | Zone |
|------|--------|-----|------|
| ip-192-168-10-33 | Ready,SchedulingDisabled | 192.168.10.33 | us-east-2 |
| ip-192-168-15-250 | **Ready** | 192.168.15.250 | us-east-2 |
| ip-192-168-22-78 | **Ready** | 192.168.22.78 | us-east-2 |
| ip-192-168-4-192 | NotReady | 192.168.4.192 | us-east-2 |
| ip-192-168-73-82 | **Ready** | 192.168.73.82 | us-east-2 |

**Available for scheduling: 3 nodes** (2 nodes are cordoned/not ready)

### Deployed Controllers (castai-agent namespace)
- ✅ castai-tsc-controller (2 replicas, leader elected)
- ✅ castai-jvm-probe-controller (2 replicas, leader elected)
- ✅ castai-pdb-controller (2 replicas, just deployed)

### Test Applications (default namespace)

#### TSC Controller Tests
| Deployment | Replicas | TSC Status | Notes |
|------------|----------|------------|-------|
| test-app-no-tsc | 3/3 | ⏳ Pending | Should get TSC injected |
| test-sts-no-tsc | 3/3 | ⏳ Pending | Should get TSC injected |
| test-app-bypass-tsc | 3/3 | ❌ Bypassed (correct) | Annotation prevents TSC |
| test-app-single-replica | 1/1 | ❌ No TSC (correct) | Replicas < 2 |
| test-multi-zone-a | 3/3 | ⏳ Pending | Zone spreading test |
| test-multi-zone-b | 3/3 | ⏳ Pending | Zone spreading test |
| test-multi-zone-c | 3/3 | ⏳ Pending | Zone spreading test |

#### JVM Probe Controller Tests
| Deployment | Replicas | Probes | Notes |
|------------|----------|--------|-------|
| test-jvm-spring | 2/2 | ⏳ Pending | Spring Boot paths expected |
| test-jvm-quarkus | 2/2 | ⏳ Pending | Quarkus paths expected |
| test-jvm-generic | 2/2 | ⏳ Pending | TCP socket expected |
| test-jvm-bypass | 2/2 | ❌ None (correct) | Bypass annotation |
| test-jvm-poor-probes | 1/2 | ⏳ Auto-fix expected | Intentionally bad config |
| test-jvm-overwrite | 2/2 | ⏳ Overwrite expected | Force overwrite annotation |
| test-jvm-bulk-1 | 2/2 | ⏳ Pending | Spring probes |
| test-jvm-bulk-2 | 2/2 | ⏳ Pending | Quarkus probes |
| test-jvm-bulk-3 | 2/2 | ⏳ Pending | Micronaut probes |

#### PDB Controller Tests
| Workload | PDB Status |
|----------|------------|
| test-sts-multi | ✅ castai-test-sts-multi-pdb created |
| test-sts-no-tsc | ✅ castai-test-sts-no-tsc-pdb created |

### Pod Distribution
```
Running pods by node:
- ip-192-168-15-250: ~10 pods
- ip-192-168-22-78: ~10 pods  
- ip-192-168-73-82: ~10 pods
Total Running: ~30 pods
```

## Validation Commands

```bash
# Node distribution
kubectl get pods -n default -o wide | awk '{print $7}' | sort | uniq -c

# TSC status
kubectl get deployments -n default -o json | jq '.items[] | "\(.metadata.name): TSC=\(.spec.template.spec.topologySpreadConstraints // "none")"'

# Probe status
kubectl get deployments -n default -o json | jq '.items[] | select(.spec.template.spec.containers[0].livenessProbe) | "\(.metadata.name): probes present"'

# PDB status
kubectl get pdb -n default

# Controller logs
kubectl logs -n castai-agent -l app.kubernetes.io/component=tsc-controller --tail=20
kubectl logs -n castai-agent -l app.kubernetes.io/component=jvm-probe-controller --tail=20
kubectl logs -n castai-agent -l app.kubernetes.io/component=pdb-controller --tail=20
```

## Next Steps

Wait for controllers to reconcile (~2 minutes) then run:
```bash
./test-apps/validate-deployment.sh
./test-apps/watch-cluster.sh
```
