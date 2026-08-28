# Rollback Operator Runbook

**For:** operators running `castai-tsc-controller` and/or `castai-jvm-probe-controller` with the CRD-backed snapshot feature enabled (controllers image that bundles PRs #19 + #20 + #21, or later).
**Read first:** [`docs/rollback-design.md`](rollback-design.md) — this runbook is the operator-facing procedure; the design doc explains the internals.
**Not for:** environments without the CRD feature. Use [`.kimchi/docs/manual-rollback-jvm-probes-and-tsc.md`](../.kimchi/docs/manual-rollback-jvm-probes-and-tsc.md) for `kubectl patch`-based manual rollback on those.

## TL;DR

| Goal | Action |
|---|---|
| Capture snapshots without applying patches (dry-run) | Set `mode=recommend` in the controller ConfigMap. Watch `kubectl get tscoriginals,jvmprobeoriginals -n castai-agent`. |
| Disable the controller without touching workloads | Set `managementEnabled=false` (leave `rollbackOnDisable=false`). |
| Disable the controller AND restore pre-castai state on every workload it patched | Set `rollbackOnDisable=true`, then set `managementEnabled: true → false`. |
| Verify a rollback actually happened | `kubectl get tscoriginals,jvmprobeoriginals -n castai-agent` — look for `RolledBack=True` in the printer columns. |
| Recover from an accidental in-place patch | Edit the workload directly with `kubectl patch` using the inverse patch shape below. |

All keys live in the per-controller ConfigMap in `castai-agent` (default name: `castai-tsc-controller-config`, `castai-jvm-probe-controller-config`). The controller hot-reloads the ConfigMap, so no restart is required for the change to take effect.

> **Helm upgrades vs. `kubectl patch`**: the Helm charts include a `checksum/config` pod annotation. Running `helm upgrade` therefore rolls the controller pods automatically when the ConfigMap changes. If you edit the ConfigMap directly with `kubectl patch`, the pods are **not** restarted automatically; either wait for the hot-reload to apply (usually within seconds) or run `kubectl rollout restart deployment/<controller> -n castai-agent` if the controller appears stale.

---

## 1. Enabling snapshots / safe rollout

The default install already has snapshotting enabled (`snapshotEnabled=true`). To verify on a running cluster:

```bash
kubectl -n castai-agent get cm castai-tsc-controller-config -o jsonpath='{.data.snapshotEnabled}'
# expected: "true"
kubectl -n castai-agent get cm castai-jvm-probe-controller-config -o jsonpath='{.data.snapshotEnabled}'
# expected: "true"
```

What you should see after the controller has been running for one reconcile cycle on a workload it touched:

```bash
kubectl -n castai-agent get tscoriginals
# NAME                       TARGET                READY   ROLLEDBACK   AGE
# default-my-app-ab12cd34    Deployment/my-app     True    False        5m

kubectl -n castai-agent get jvmprobeoriginals
# NAME                       TARGET                       READY   ROLLEDBACK   AGE
# default-my-java-app-9f8e7d6c Deployment/my-java-app    True    False        7m
```

If `READY=False` for a long time, see §6 (Troubleshooting).

**Best-practice rollout for a fresh cluster:**

1. Install with defaults (`mode=recommend`, `snapshotEnabled=true`, `managementEnabled=true`).
2. Watch `kubectl get tscoriginals,jvmprobeoriginals -n castai-agent -w` over one workload rollout cycle.
3. Spot-check one CRD:
   ```bash
   kubectl -n castai-agent get tscoriginal default-my-app-ab12cd34 -o yaml
   ```
   Verify `spec.targetRef.uid` matches `kubectl get deploy/my-app -n default -o jsonpath='{.metadata.uid}'`. If they don't, see §6 ("CRD UID mismatch").
4. Once comfortable, no further action — snapshots accumulate automatically.

---

## 2. Disabling management (rolling back)

This is the **operator-driven rollback** path. It restores pre-castai state on every workload the controller has patched.

### 2.1 Step-by-step

```bash
# 1. Set rollbackOnDisable=true (so the disable transition triggers rollback).
kubectl -n castai-agent edit cm castai-tsc-controller-config
# (or use kubectl patch — see below)
```

Using `kubectl patch` (scriptable):

```bash
kubectl -n castai-agent patch cm castai-tsc-controller-config --type=merge \
  -p '{"data":{"rollbackOnDisable":"true"}}'

# 2. Flip managementEnabled from true to false. THIS is what triggers the
#    rollback loop. Order matters — set rollbackOnDisable first so the
#    transition detection sees both keys in the new state.
kubectl -n castai-agent patch cm castai-tsc-controller-config --type=merge \
  -p '{"data":{"managementEnabled":"false"}}'
```

Repeat for `castai-jvm-probe-controller-config` if you want to roll back both controllers in lockstep.

### 2.2 What happens next

1. The controller's ConfigMap watcher detects `old.managementEnabled=true && new.managementEnabled=false && new.rollbackOnDisable=true`.
2. It spawns `runRollback()` in a goroutine (does not block the informer callback).
3. `runRollback()` lists every `TSCOriginal` (or `JVMProbeOriginal`) in `castai-agent`.
4. For each CRD: builds the inverse patch, applies it, sets `status.conditions[type=RolledBack].status="True"`.
5. Logs and events stream from the controller pod:
   ```bash
   kubectl -n castai-agent logs deploy/castai-tsc-controller --tail=-1 | grep -E 'rollback|tscoriginal'
   ```

### 2.3 Verifying rollback completed

```bash
# Rollback status per CRD (the printer columns show this):
kubectl -n castai-agent get tscoriginals
# NAME                       TARGET                READY   ROLLEDBACK   AGE
# default-my-app-ab12cd34    Deployment/my-app     True    True         12m
#                                                              ^^^^^^^^

# Workload should match its pre-castai state. Compare to your source of truth:
kubectl get deploy/my-app -n default -o jsonpath='{.spec.template.spec.topologySpreadConstraints}'
```

For JVM probes, verify the probe fields are gone or restored:

```bash
kubectl get deploy/my-java-app -n default -o json \
  | jq '.spec.template.spec.containers[] | {name, livenessProbe, readinessProbe, startupProbe}'
# all three should be null OR match your pre-castai probes
```

### 2.4 Idempotency

Re-firing the same trigger (re-applying the same ConfigMap patch, or restarting the controller with `rollbackOnDisable=true, managementEnabled=false`) is a no-op — the rollback loop skips CRDs already marked `RolledBack=True`.

---

## 3. Disabling without rollback

To freeze the controller but leave already-patched workloads alone (the common "we want to think before rolling back" state):

```bash
kubectl -n castai-agent patch cm castai-tsc-controller-config --type=merge \
  -p '{"data":{"managementEnabled":"false"}}'
# rollbackOnDisable stays at its default ("false")
```

The controller:

- Stops mutating workloads.
- Stops capturing new snapshots.
- Leaves existing `TSCOriginal` / `JVMProbeOriginal` CRDs in place.

If you change your mind, flip `managementEnabled` back to `true`. Snapshots that existed before the freeze remain; the controller resumes patching on next reconcile.

---

## 4. Recommend mode (dry-run)

Use `mode=recommend` to capture snapshots and emit events **without applying any patches**. Useful for:

- First-time install in a Prod cluster ("show me what you'd change").
- Validating a new controller version against real workloads.
- Onboarding a new environment where you're not ready to commit to mutations.

### 4.1 Switch to recommend mode

```bash
kubectl -n castai-agent patch cm castai-tsc-controller-config --type=merge \
  -p '{"data":{"mode":"recommend"}}'
```

The controller hot-reloads the new mode. Within one reconcile cycle:

```bash
# Snapshots appear as the controller observes workloads it would patch:
kubectl -n castai-agent get tscoriginals -w

# Events on the workload describe what would have happened:
kubectl get deploy/my-app -n default --watch-only
# (or: kubectl get events --field-selector involvedObject.kind=Deployment)
```

### 4.2 Inspect a snapshot

```bash
kubectl -n castai-agent get tscoriginal default-my-app-ab12cd34 -o yaml
```

```yaml
apiVersion: workloads.cast.ai/v1
kind: TSCOriginal
metadata:
  name: default-my-app-ab12cd34
  namespace: castai-agent
  labels:
    workloads.cast.ai/managed-by: tsc-controller
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    namespace: default
    name: my-app
    uid: 8beecee7-0098-2bdb-a2aa-2913f0b04309
  originalTSCs:
    - maxSkew: 1
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: ScheduleAnyway
  originalTSCsPresent: true
  capturedAt: "2026-08-26T12:00:00Z"
  controllerVersion: "1.5.0"
status:
  conditions:
    - type: Ready
      status: "True"
      reason: SnapshotStored
    - type: RolledBack
      status: "False"
      reason: NotRequested
```

### 4.3 Switching to apply

Once you've reviewed the snapshots:

```bash
kubectl -n castai-agent patch cm castai-tsc-controller-config --type=merge \
  -p '{"data":{"mode":"apply"}}'
```

The controller resumes normal capture + patch. Because snapshots already exist (and are `Ready=True`, `RolledBack=False`), `CaptureIfAbsent` is a no-op — no duplicate CRDs, no `SnapshotLost` events.

---

## 5. Recovering from accidental patch (manual rollback via CRD)

Use this when:

- You need to roll back a **single** workload surgically (not cluster-wide).
- The ConfigMap-driven rollback in §2 doesn't fit (e.g. you want to keep the controller running and just fix one workload).

### 5.1 Read the snapshot

```bash
kubectl -n castai-agent get tscoriginal default-my-app-ab12cd34 -o jsonpath='{.spec.originalTSCs}'
```

If the field was absent pre-castai, you'll see `null` and `originalTSCsPresent: false`. Otherwise, you'll see the original list.

### 5.2 Apply the inverse patch manually

For TSC:

```bash
# Absent case — remove the field entirely:
kubectl -n default patch deploy/my-app --type=strategic --patch '
spec:
  template:
    spec:
      topologySpreadConstraints: null
'

# Present case — restore the original list:
kubectl -n default patch deploy/my-app --type=strategic --patch '
spec:
  template:
    spec:
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: kubernetes.io/hostname
          whenUnsatisfiable: ScheduleAnyway
'
```

For JVM probes, build one JSON-Patch document per container:

```bash
kubectl -n default get deploy/my-java-app -o json \
  | jq --argjson snap "$(kubectl -n castai-agent get jvmprobeoriginal default-my-java-app-9f8e7d6c -o json)" '
    [
      .spec.template.spec.containers | to_entries[] | . as $c |
      ($snap.spec.originalContainers[$c.value.name] // null) as $orig |
      $orig |
      if . == null then empty
      else
        [
          (if .livenessPresent == false then {op:"remove", path:"/spec/template/spec/containers/\($c.key)/livenessProbe"} else empty end),
          (if .livenessPresent == true  then {op:"replace", path:"/spec/template/spec/containers/\($c.key)/livenessProbe", value:.livenessProbe} else empty end),
          (if .readinessPresent == false then {op:"remove", path:"/spec/template/spec/containers/\($c.key)/readinessProbe"} else empty end),
          (if .readinessPresent == true  then {op:"replace", path:"/spec/template/spec/containers/\($c.key)/readinessProbe", value:.readinessProbe} else empty end),
          (if .startupPresent == false then {op:"remove", path:"/spec/template/spec/containers/\($c.key)/startupProbe"} else empty end),
          (if .startupPresent == true  then {op:"replace", path:"/spec/template/spec/containers/\($c.key)/startupProbe", value:.startupProbe} else empty end)
        ]
      end
    ] | add
  ' > /tmp/inverse.json

kubectl -n default patch deploy/my-java-app --type=json --patch-file=/tmp/inverse.json
```

Containers present in the live workload but missing from the snapshot are left alone (castai never touched them).

### 5.3 Mark the CRD as rolled-back

Optional but recommended — keeps the snapshot from being re-applied if the controller re-mutates:

```bash
kubectl -n castai-agent patch tscoriginal default-my-app-ab12cd34 --type=merge --subresource=status \
  --patch '{"status":{"conditions":[{"type":"RolledBack","status":"True","reason":"ManualRollback","observedGeneration":1}]}}'
```

If you'd rather delete the CRD entirely:

```bash
kubectl -n castai-agent delete tscoriginal default-my-app-ab12cd34
```

Note: the controller's workload informer will see the workload on next reconcile and, if `managementEnabled=true`, will attempt to capture a fresh snapshot. That's correct — it'll capture the (now rolled-back) state as the new "original".

---

## 6. Troubleshooting

### 6.1 Snapshots missing for a workload the controller should have patched

```bash
# Is the controller running and not frozen?
kubectl -n castai-agent get deploy castai-tsc-controller
kubectl -n castai-agent get cm castai-tsc-controller-config -o jsonpath='{.data.managementEnabled}'
# Expected: "true"

# Is snapshotting enabled?
kubectl -n castai-agent get cm castai-tsc-controller-config -o jsonpath='{.data.snapshotEnabled}'
# Expected: "true"

# Is the workload excluded by bypass or whitelist?
kubectl get deploy/my-app -n default -o jsonpath='{.metadata.annotations}'
# Look for workloads.cast.ai/tsc-bypass or workloads.cast.ai/tsc-controller-enabled
```

If all the above look right but no CRD appears, check controller logs for `SnapshotFailed`:

```bash
kubectl -n castai-agent logs deploy/castai-tsc-controller --tail=-1 | grep -iE 'snapshot|capture'
```

Common causes:

- **Bypass annotation set** — controller skips entirely (no snapshot, no patch). Remove the annotation.
- **`whitelistOnly: true` and no whitelist label** — controller skips. Either add the label or set `whitelistOnly: false`.
- **Workload has fewer replicas than `minReplicas`** (TSC only) — controller skips. Lower `minReplicas` or scale up.

### 6.2 Rollback appears stuck

`kubectl get tscoriginals,jvmprobeoriginals -n castai-agent` shows `ROLLEDBACK=False` for many CRDs an hour after the trigger.

```bash
# Is the controller pod still running?
kubectl -n castai-agent get pods -l app.kubernetes.io/name=castai-tsc-controller

# Check the controller logs for rollback progress / errors:
kubectl -n castai-agent logs deploy/castai-tsc-controller --tail=-1 | grep -E 'rollback|tscoriginal'

# Look for per-CRD errors:
kubectl -n castai-agent logs deploy/castai-tsc-controller --tail=-1 | grep -E 'RollbackFailed|ErrTargetGone'
```

Common causes:

- **Target workload was deleted between capture and rollback.** Marked `RolledBack=True, reason=TargetGone` — this is the expected terminal state, not a bug.
- **Target workload's UID changed** (deleted + recreated with same namespace/name). Same — marked `TargetGone`. Verify with `kubectl get <kind>/<name> -n <ns> -o jsonpath='{.metadata.uid}'` against the CRD's `spec.targetRef.uid`.
- **kubectl patch 422 / 409** — the workload was edited concurrently. Re-fire rollback; it's idempotent.

If the controller pod is in `CrashLoopBackOff`, fix the controller first (it may be RBAC-bound or unable to reach the API server). The rollback will re-fire on the next ConfigMap transition or on restart.

### 6.3 CRD finalizers blocking delete

`kubectl delete tscoriginal <name>` hangs because the controller set a finalizer (`workloads.cast.ai/tsc-controller-finalizer`). This is by design — the finalizer guarantees retention through the workload lifecycle.

To delete a CRD cleanly, choose one of:

1. **Delete the target workload first.** The controller's workload DeleteFunc handler removes the finalizer on the next informer event, and then the delete completes.
2. **Trigger a rollback.** The rollback loop removes the finalizer after marking the CRD `RolledBack=True`.
3. **Force-delete with the documented escape hatch.** Set the force-delete annotation on the CRD, then delete:
   ```bash
   kubectl -n castai-agent annotate tscoriginal default-my-app-ab12cd34 \
     workloads.cast.ai/force-delete=true --overwrite
   kubectl -n castai-agent delete tscoriginal default-my-app-ab12cd34
   ```
   The controller's CRD-watcher honors the annotation and clears the finalizer.

### 6.4 CRD UID mismatch warning

```text
Warning  SnapshotUIDMismatch  deployment/my-app  TSCOriginal default-my-app-ab12cd34 has uid=X but workload has uid=Y; not overwriting
```

Means the controller observed a workload whose current `metadata.uid` differs from the `TargetRef.uid` baked into the CRD. This is the lost-snapshot guard firing as designed.

**Likely cause:** the workload was deleted and recreated with the same `namespace/name` but a new UID.

**Resolution:**

1. Confirm the recreation by checking rollout history:
   ```bash
   kubectl rollout history deploy/my-app -n default
   ```
2. The stale CRD will be cleaned up on the next controller restart (startup orphan sweep, design doc §5.7).
3. The new workload gets a fresh `TSCOriginal` on its first patch.

### 6.5 `kubectl patch` returns 422 "path does not exist" during manual rollback

You're using JSON Patch `remove` on a probe field that's already gone (the controller never added it, or someone else removed it). Two options:

- Preflight read the workload and emit only the ops for probes that exist (see design doc §6.4 for the table).
- Use a strategic-merge patch with `null` instead — safe when the field is already absent:
  ```bash
  kubectl -n default patch deploy/my-java-app --type=strategic --patch '
  spec:
    template:
      spec:
        containers:
          - name: <container-name>
            livenessProbe: null
            readinessProbe: null
            startupProbe: null
  '
  ```

---

## 7. Verifying rollback

Quick sanity-checklist after triggering a rollback:

```bash
# 1. Every CRD marked RolledBack=True (or TargetGone):
kubectl -n castai-agent get tscoriginals -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="RolledBack")].status}{"\n"}{end}'

# 2. No controller pod in CrashLoopBackOff:
kubectl -n castai-agent get pods -l app.kubernetes.io/name=castai-tsc-controller

# 3. No "RollbackFailed" events in the last hour:
kubectl get events --all-namespaces --field-selector reason=RollbackFailed

# 4. Spot-check a workload's spec — should match your source of truth:
kubectl get deploy/my-app -n default -o yaml | grep -A20 topologySpreadConstraints

# 5. Rollout completed cleanly:
kubectl -n default rollout status deploy/my-app
```

For JVM probes, repeat with `jvmprobeoriginals` and `containers[i].{liveness,readiness,startup}Probe`.

If any of the above fail, see §6 (Troubleshooting).
