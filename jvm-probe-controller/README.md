# CAST AI JVM Probe Controller

The JVM Probe Controller automatically detects JVM-based containers and injects appropriate health probes (liveness, readiness, startup) when missing. It includes automatic probe failure detection and auto-fix capabilities for slow-starting JVM applications.

## Overview

This controller watches for Deployments and StatefulSets in your cluster, detects JVM-based containers, and automatically adds appropriate health probes. It supports Spring Boot, Quarkus, Micronaut, and generic JVM applications.

## Features

- **Framework detection** (Spring Boot, Quarkus, Micronaut, generic JVM)
- **Image-based detection** (java, jdk, openjdk, spring, etc.)
- **Environment variable detection** (JAVA_HOME, SPRING_PROFILES_ACTIVE)
- **Configurable probe settings** via ConfigMap
- **Annotation-based overrides** per workload
- **Automatic probe framework paths** based on detected framework
- **Startup probe always injected** for JVM containers (prevents premature termination)
- **Automatic probe failure monitoring** - watches pod events for probe failures
- **Auto-fix poor probe configurations** - adjusts delays/thresholds based on failure patterns
- **Force overwrite existing probes** - replace badly configured probes
- **Leader election** for HA with multiple replicas
- **Rate-limited logging** to prevent log spam

## Supported Frameworks

| Framework | Liveness Path | Readiness Path | Startup Path |
|-----------|---------------|----------------|--------------|
| Spring Boot | `/actuator/health/liveness` | `/actuator/health/readiness` | `/actuator/health` |
| Quarkus | `/q/health/live` | `/q/health/ready` | `/q/health/started` |
| Micronaut | `/health/liveness` | `/health/readiness` | `/health` |
| Generic JVM | TCP socket | TCP socket | TCP socket |

## Installation

### Helm Installation

```bash
# Add CAST AI Helm repository (if available)
helm repo add castai https://charts.cast.ai

# Install JVM Probe Controller
helm install castai-jvm-probe-controller castai/castai-jvm-probe-controller \
  --namespace castai-agent \
  --create-namespace
```

### Custom Installation

```bash
# Clone the repository
cd workload-autopilot/controllers/jvm-probe-controller/helm

# Install with custom values
helm install castai-jvm-probe-controller ./castai-jvm-probe-controller \
  --namespace castai-agent \
  --create-namespace \
  --set replicaCount=2 \
  --set config.autoFixMode=true
```

## Configuration

### Values.yaml

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of controller replicas | `2` |
| `image.repository` | Controller image repository | `castai/jvm-probe-controller` |
| `image.tag` | Controller image tag | `latest` |

### Probe Defaults

| Parameter | Description | Default |
|-----------|-------------|---------|
| `config.probeDefaults.liveness.initialDelaySeconds` | Liveness initial delay | `60` |
| `config.probeDefaults.liveness.periodSeconds` | Liveness period | `10` |
| `config.probeDefaults.liveness.timeoutSeconds` | Liveness timeout | `5` |
| `config.probeDefaults.liveness.failureThreshold` | Liveness failure threshold | `3` |
| `config.probeDefaults.readiness.initialDelaySeconds` | Readiness initial delay | `30` |
| `config.probeDefaults.readiness.periodSeconds` | Readiness period | `10` |
| `config.probeDefaults.readiness.timeoutSeconds` | Readiness timeout | `5` |
| `config.probeDefaults.readiness.failureThreshold` | Readiness failure threshold | `3` |
| `config.probeDefaults.startup.initialDelaySeconds` | Startup initial delay | `10` |
| `config.probeDefaults.startup.periodSeconds` | Startup period | `5` |
| `config.probeDefaults.startup.timeoutSeconds` | Startup timeout | `3` |
| `config.probeDefaults.startup.failureThreshold` | Startup failure threshold | `30` |

### Auto-Fix Settings

| Parameter | Description | Default |
|-----------|-------------|---------|
| `config.autoFixMode` | Enable automatic probe fixing | `true` |
| `config.autoFixThreshold` | Restarts before triggering fix | `3` |
| `config.maxInitialDelay` | Max seconds to increase initial delay to | `300` |
| `config.alwaysInjectStartupProbe` | Always inject startup probe for JVM | `true` |
| `config.logInterval` | Rate limit interval for logs | `15m` |

### Example values.yaml

```yaml
replicaCount: 2

image:
  repository: castai/jvm-probe-controller
  tag: v1.0.0

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
  logInterval: "15m"
  exclusions:
    - namespaceRegex: "^kube-system$"
```

## Annotations

The JVM Probe Controller supports per-workload configuration via annotations:

| Annotation | Description | Example |
|------------|-------------|---------|
| `workloads.cast.ai/jvm-probe-bypass` | Skip this workload | `"true"` |
| `workloads.cast.ai/jvm-probe-framework` | Force framework detection | `"spring-boot"`, `"quarkus"`, `"micronaut"`, `"generic"` |
| `workloads.cast.ai/jvm-probe-port` | Override port | `"8080"` |
| `workloads.cast.ai/jvm-probe-initial-delay` | Initial delay seconds | `"60"` |
| `workloads.cast.ai/jvm-probe-overwrite-all` | Force overwrite all probes | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-liveness` | Force overwrite liveness probe | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-readiness` | Force overwrite readiness probe | `"true"` |
| `workloads.cast.ai/jvm-probe-overwrite-startup` | Force overwrite startup probe | `"true"` |
| `workloads.cast.ai/jvm-probe-log-failures` | Enable detailed failure logging | `"true"` |

### Examples

#### Basic Spring Boot Application

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-spring-app
  annotations:
    workloads.cast.ai/jvm-probe-framework: "spring-boot"
    workloads.cast.ai/jvm-probe-port: "8080"
spec:
  replicas: 3
  selector:
    matchLabels:
      app: my-spring-app
  template:
    metadata:
      labels:
        app: my-spring-app
    spec:
      containers:
        - name: spring
          image: mycompany/spring-boot-app:latest
          ports:
            - containerPort: 8080
```

#### Override Framework Detection

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-quarkus-app
  annotations:
    workloads.cast.ai/jvm-probe-framework: "quarkus"
spec:
  # ...
```

#### Fix Badly Configured Probes

If you have existing probes that are not working well (e.g., too short initial delay for slow-starting apps), use the overwrite annotations:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-legacy-app
  annotations:
    workloads.cast.ai/jvm-probe-overwrite-all: "true"
    workloads.cast.ai/jvm-probe-log-failures: "true"
spec:
  # This will replace ALL existing probes with configured defaults
  # and enable detailed failure logging
  # ...
```

## Auto-Fix Mode

The JVM Probe Controller includes a sophisticated **Pod Event Monitor** that automatically fixes poor probe configurations.

### How It Works

1. **Watches Kubernetes Events** for probe failures (`Unhealthy`, `ProbeFailed`)
2. **Tracks Container Restarts** due to probe failures
3. **Analyzes Failure Patterns** to determine optimal probe settings
4. **Automatically Fixes Poor Probes** by adjusting:
   - `initialDelaySeconds` (up to 300s max)
   - `failureThreshold` (up to 10 max)
   - `timeoutSeconds`

### Failure Detection Triggers

- >= 5 probe failures in 5 minutes
- >= 3 container restarts
- High failure rate (> 10/min)

### Example Auto-Fix Log

When the controller detects failing probes and applies a fix, you'll see logs like:

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

### Failure Logging

The controller logs probe failures in a structured format:

```
[PROBE FAILURE] default/my-app container=spring-boot probe=liveness count=5
[CONTAINER RESTART] default/my-app-xxx container=spring-boot restarts=3
```

Enable detailed failure logging with:
```yaml
annotations:
  workloads.cast.ai/jvm-probe-log-failures: "true"
```

## JVM Detection Methods

The controller uses multiple methods to detect JVM containers:

1. **Image-based detection**:
   - `openjdk:*`
   - `eclipse-temurin:*`
   - `amazoncorretto:*`
   - `springboot:*`
   - `quarkus:*`

2. **Environment variable detection**:
   - `JAVA_HOME` is set
   - `SPRING_PROFILES_ACTIVE` is set
   - `QUARKUS_PROFILE` is set

3. **Command detection**:
   - Container command contains `java`

4. **Port detection**:
   - Common JVM application ports (8080, 8443, 9000, etc.)

## Monitoring

### Logs

```bash
# View controller logs
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-jvm-probe-controller

# Follow logs in real-time
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-jvm-probe-controller -f
```

### Events

```bash
# Watch for probe-related events
kubectl get events --field-selector reason=ProbesAdded

# Watch for auto-fix events
kubectl get events --field-selector reason=ProbeFixApplied
```

### Verify Probes

```bash
# Check if probes were added to a deployment
kubectl get deployment <name> -o jsonpath='{.spec.template.spec.containers[0].livenessProbe}'

kubectl get deployment <name> -o jsonpath='{.spec.template.spec.containers[0].readinessProbe}'

kubectl get deployment <name> -o jsonpath='{.spec.template.spec.containers[0].startupProbe}'
```

## Upgrading

```bash
# Upgrade to a new version
helm upgrade castai-jvm-probe-controller castai/castai-jvm-probe-controller \
  --namespace castai-agent \
  --set image.tag=v1.1.0
```

## Uninstalling

```bash
# Uninstall the controller
helm uninstall castai-jvm-probe-controller -n castai-agent
```

## Troubleshooting

### Controller not injecting probes

1. Check if the container is actually JVM-based
2. Verify it's not in the exclusion list
3. Check controller logs for detection results
4. Verify the ConfigMap exists and is correct

### Probes not working as expected

1. Check the detected framework:
   ```bash
   kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-jvm-probe-controller | grep "Detected framework"
   ```
2. Verify the probe paths match your application
3. Check if annotation overrides are being applied correctly

### Auto-fix not triggering

1. Verify `autoFixMode` is enabled in values
2. Check if there are enough failure events
3. Review controller logs for fix attempts

## License

MIT License - See LICENSE file for details.