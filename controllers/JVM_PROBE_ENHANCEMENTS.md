# JVM Probe Controller - Enhanced Features

## Overview

This document describes the enhanced features added to the JVM Probe Controller for automatic startup probe injection, probe failure monitoring, and auto-fix capabilities.

---

## 🆕 New Features

### 1. **Startup Probe Always Injected**

**Problem**: JVM applications (especially Spring Boot) often have slow startup times. Without a startup probe, Kubernetes may kill the container during startup if liveness/readiness probes fail.

**Solution**: The controller **always injects a startup probe** for detected JVM containers, regardless of whether other probes exist.

**Configuration**:
```yaml
# ConfigMap setting
alwaysInjectStartupProbe: "true"

# Startup probe has longer delays:
# - Spring Boot: 180s initial delay (60s × 3 multiplier)
# - Quarkus: 60s initial delay (30s × 2 multiplier)
# - Generic: 90s initial delay (30s × 3 multiplier)
```

---

### 2. **Probe Failure Monitoring**

**What it does**:
- Watches Kubernetes Events for probe failures
- Monitors Pod status for container restarts
- Tracks failure patterns per container

**Monitored Events**:
- `Unhealthy` - Liveness probe failures
- `ReadinessProbeFailed` - Readiness probe failures
- `LivenessProbeFailed` - Explicit liveness failures
- `StartupProbeFailed` - Startup probe failures
- `Container restarted` - Restarts due to probe failures

**Log Format**:
```
[PROBE FAILURE] default/my-app container=spring-boot probe=liveness count=5 message="Probe failed: connection refused"
[CONTAINER RESTART] default/my-app-abc123 container=spring-boot restarts=3
```

---

### 3. **Auto-Fix for Poor Probes**

**Trigger Conditions**:
- ≥5 probe failures in 5 minutes
- ≥3 container restarts
- Failure rate > 10/minute (high)
- Failure rate > 5/minute (moderate)

**Adjustment Strategy**:

| Failure Rate | Initial Delay Adjustment | Threshold Adjustment |
|--------------|------------------------|---------------------|
| > 10/min | ×3 multiplier (min 120s) | +3 |
| > 5/min | ×2 multiplier (min 60s) | +2 |
| Low rate | +30s | No change |

**Example Fix Output**:
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

**Limits**:
- Max Initial Delay: 300 seconds
- Max Failure Threshold: 10
- Rate limiting: Won't fix same probe within 30 minutes

---

### 4. **Force Overwrite Annotations**

Sometimes existing probes are misconfigured. Use these annotations to force replacement:

| Annotation | Effect |
|------------|--------|
| `workloads.cast.ai/jvm-probe-overwrite-all` | Replace ALL existing probes |
| `workloads.cast.ai/jvm-probe-overwrite-liveness` | Replace liveness probe |
| `workloads.cast.ai/jvm-probe-overwrite-readiness` | Replace readiness probe |
| `workloads.cast.ai/jvm-probe-overwrite-startup` | Replace startup probe |

**Example**:
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: legacy-app
  annotations:
    workloads.cast.ai/jvm-probe-overwrite-all: "true"
```

---

### 5. **Failure Logging**

Enable detailed failure logging for debugging:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  annotations:
    workloads.cast.ai/jvm-probe-log-failures: "true"
    workloads.cast.ai/jvm-probe-failure-log-threshold: "3"  # Optional
```

This will log every probe failure event with detailed context.

---

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                  JVM Probe Controller                            │
├──────────────────────────────────────────────────────────────────┤
│                                                                  │
│  ┌─────────────────┐      ┌─────────────────────────────────┐   │
│  │  Probe Injector │      │    Pod Event Monitor            │   │
│  ├─────────────────┤      ├─────────────────────────────────┤   │
│  │ • Watch Deploys │      │ • Watch Pod Events              │   │
│  │ • Watch STS     │      │ • Watch Container Restarts      │   │
│  │ • Detect JVM    │      │ • Track Failure Patterns        │   │
│  │ • Inject Probes │      │ • Trigger Fixes                 │   │
│  │ • Force Overwrite│     │ • Emit Events                   │   │
│  └─────────────────┘      └─────────────────────────────────┘   │
│         │                             │                          │
│         └──────────────┬──────────────┘                          │
│                        ▼                                         │
│              ┌───────────────────┐                               │
│              │  Failure Queue    │                               │
│              │  (100 requests)   │                               │
│              └───────────────────┘                               │
│                        │                                         │
│                        ▼                                         │
│              ┌───────────────────┐                               │
│              │  Probe Fixer      │                               │
│              │  (calculate &     │                               │
│              │   apply patches)  │                               │
│              └───────────────────┘                               │
│                                                                   │
└──────────────────────────────────────────────────────────────────┘
```

---

## Configuration Reference

### ConfigMap - Full Example

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: castai-jvm-probe-controller-config
  namespace: castai-agent
data:
  frameworks: |-
    {
      "spring-boot": {
        "livenessPath": "/actuator/health/liveness",
        "readinessPath": "/actuator/health/readiness",
        "startupPath": "/actuator/health",
        "defaultPort": 8080,
        "initialDelaySeconds": 60,
        "periodSeconds": 10,
        "timeoutSeconds": 5,
        "failureThreshold": 3,
        "successThreshold": 1,
        "useTCPSocket": false,
        "startupProbeMultiplier": 3
      }
    }
  
  # Always inject startup probes (recommended for JVM)
  alwaysInjectStartupProbe: "true"
  
  # Auto-fix settings
  probeAutoFixEnabled: "true"
  failureThresholdBeforeFix: "5"
  timeWindowForFailures: "5m"
  maxInitialDelaySeconds: "300"
  maxFailureThreshold: "10"
  
  # Standard settings
  logInterval: "15m"
  reconcileInterval: "2m"
  requireBothProbes: "false"
  skipIfAnyProbeExists: "false"
```

---

## Best Practices

### 1. Enable for Slow-Starting Apps

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: spring-boot-app
  annotations:
    workloads.cast.ai/jvm-probe-log-failures: "true"
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: app
          image: myrepo/spring-app:1.0
          env:
            - name: JAVA_OPTS
              value: "-Xmx2g -XX:+UseG1GC"
```

### 2. Fix Legacy Deployments

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: old-app
  annotations:
    workloads.cast.ai/jvm-probe-overwrite-all: "true"
    workloads.cast.ai/jvm-probe-framework: "spring-boot"
    workloads.cast.ai/jvm-probe-log-failures: "true"
```

### 3. Gradually Roll Out

Start with logging only, then enable auto-fix:
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: castai-jvm-probe-controller-config
  namespace: castai-agent
data:
  probeAutoFixEnabled: "false"  # Start here
```

After observing failure patterns, enable:
```yaml
  probeAutoFixEnabled: "true"   # Then enable this
```

---

## Monitoring Auto-Fixes

### View Fix Events
```bash
kubectl get events --field-selector reason=ProbeAutoFixed --sort-by='.lastTimestamp'
```

### View Probe Failure Logs
```bash
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-jvm-probe-controller | grep "PROBE FAILURE"
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-jvm-probe-controller | grep "PROBE FIX APPLIED"
```

### Check Applied Fixes
```bash
# Get deployment with fixed probes
kubectl get deployment my-app -o yaml | grep -A 10 livenessProbe
```

---

## Troubleshooting

### Startup Probe Not Injected
- Check if container is detected as JVM (`jvm-probe-bypass` annotation not present)
- Verify `alwaysInjectStartupProbe: "true"` in ConfigMap

### Auto-Fix Not Triggering
- Check failure count is reaching threshold (5 in 5 min)
- Verify `probeAutoFixEnabled: "true"`
- Check for "fix queue full" errors in logs

### Contain er Keeps Restarting
- Enable detailed logging: `workloads.cast.ai/jvm-probe-log-failures: "true"`
- Force overwrite: `workloads.cast.ai/jvm-probe-overwrite-all: "true"`
- Manually check probe endpoints in pod

---

## Files Added/Modified

| File | Description |
|------|-------------|
| `probe_monitor.go` | NEW - Pod event monitoring and auto-fix logic |
| `annotations.go` | NEW - Overwrite annotation support |
| `main.go` | MODIFIED - Integrated monitor, added overwrite logic |
| `manifests/20-configmap.yaml` | MODIFIED - Added auto-fix settings |
| `README.md` | MODIFIED - Documented new features |
| `JVM_PROBE_ENHANCEMENTS.md` | NEW - This document |
