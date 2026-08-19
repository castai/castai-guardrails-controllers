# Rollback design for TSC & JVM-Probe Controllers

**Status:** Design proposal — no code changes yet.
**Scope:** `controllers/tsc-controller`, `controllers/jvm-probe-controller`. The PDB controller is out of scope: it manages standalone `PodDisruptionBudget` objects, so rollback there is trivially `kubectl delete pdb`.
**Version:** v2 (CRD-based, mirroring `recommendations.autoscaling.cast.ai` pattern).

---

## 1. Problem statement

`tsc-controller` and `jvm-probe-controller` mutate live `Deployment` / `StatefulSet` objects in-place:

| Controller | Patch type | What it changes |
|---|---|---|
| tsc-controller | `StrategicMergePatchType` on `apps/v1` Deployments & StatefulSets | `spec.template.spec.topologySpreadConstraints` (add, replace, or delete) |
| jvm-probe-controller | `JSONPatchType` on `apps/v1` Deployments & StatefulSets | `spec.template.spec.containers[i].{livenessProbe,readinessProbe,startupProbe}` (add or replace) |

Once a patch is applied, there is no automatic way to reconstruct the pre-patch state. If the controller mis-configures workloads at fleet scale (e.g. a bad JVM `initialDelaySeconds`, a TSC that pins pods to a zone that doesn't exist), an operator has to hand-fix every affected workload.

**Goal:** Before the first mutation of any workload, persist enough of the previous state to reconstruct the affected fields, and ship an operator-facing recovery CLI.

**Non-goals:**
- Restoring workloads to an arbitrary point in time (this is not Velero — we only preserve the last known "pre-castai" state).
- Rolling back changes not made by these controllers.
- Preserving pod-level state — only the workload spec.
- PDB rollback (out of scope; `kubectl delete pdb` is the recovery path today).

---

## 2. Design principles

1. **Only capture what we change.** Full-workload dumps waste storage and leak secrets/env-vars. TSC only touches `topologySpreadConstraints`; JVM-Probe only touches three probe fields per container. Store those slices, not the whole PodSpec.
2. **Capture once, at first mutation.** After the controller has patched a workload, subsequent patches are re-patches of *castai-managed* fields. The snapshot must be the *pre-castai* state, so we only write it the first time we see a workload we're about to mutate.
3. **Reuse the existing CAST AI CRD pattern.** Mirror `recommendations.autoscaling.cast.ai` exactly — same namespace layout, same finalizer lifecycle, same `targetRef` shape, same status-conditions convention. One consistent mental model for operators.
4. **Store in-cluster in namespaced CRDs, not on disk.** `/tmp` in a pod is ephemeral — a restart loses everything. CRDs live in etcd, are backed up by every standard tool, and are queryable via `kubectl`.
5. **Rollback is a separate, deliberate action.** The controllers never auto-rollback. Recovery is triggered via a CLI (`rollback-*.sh`) that reads the CRDs and issues inverse patches.
6. **Zero blast-radius change to the mutation path.** Snapshot writes happen *before* the patch call and must never block the patch on failure — a snapshot write error logs and continues. Losing rollback data for one workload is preferable to failing to reconcile. To prevent silent corruption from lost snapshots (a failed CRD write followed by a successful patch would otherwise let the next reconcile capture the *post-patch* state as "original"), the patch itself sets a `workloads.cast.ai/<controller>-managed` annotation on the workload. Capture is only attempted when *both* the CRD is absent *and* the managed annotation is absent — see §4.
7. **Three-level gating, matching Workload Autoscaler.** Global `mode: recommend | apply` config, per-workload `bypass` annotation (already exists), optional whitelist label. Default mode: `apply`.
8. **No retention machinery.** Finalizer + workload-informer cleanup deletes originals when the workload is deleted. No time-based expiry, no GC loop, no orphan sweep to maintain.

---

## 3. Data model

Two new CRDs, both under the group `workloads.cast.ai/v1`, both **namespace-scoped in `castai-agent`**, mirroring the `Recommendation` CRD layout exactly.

### 3.1 `TSCOriginal` CRD

```yaml
apiVersion: workloads.cast.ai/v1
kind: TSCOriginal
metadata:
  name: default-my-app                     # <workload-namespace>-<workload-name>
  namespace: castai-agent                  # ALL originals live here
  finalizers:
    - workloads.cast.ai/tsc-original       # controls lifecycle
  annotations:
    workloads.cast.ai/tsc-controller-version: "1.4.2"
    workloads.cast.ai/captured-at: "2026-08-14T12:00:00Z"
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: my-app
    namespace: default                     # the workload's actual namespace
    uid: 8beecee7-0098-2bdb-a2aa-2913f0b04309   # protects against name reuse
  original:
    # nil == field was absent (patch was 'add'); non-empty == was 'replace'
    topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
    absent: false                          # true if the field was unset before we patched
status:
  conditions:
    - type: Captured
      status: "True"
      reason: SnapshotStored
      message: "Original TSC captured before first patch."
      observedGeneration: 1
      lastTransitionTime: "2026-08-14T12:00:00Z"
    - type: RolledBack
      status: "False"
      reason: NotRequested
      observedGeneration: 1
      lastTransitionTime: "2026-08-14T12:00:00Z"
```

### 3.2 `JVMProbeOriginal` CRD

```yaml
apiVersion: workloads.cast.ai/v1
kind: JVMProbeOriginal
metadata:
  name: default-my-java-app
  namespace: castai-agent
  finalizers:
    - workloads.cast.ai/jvm-probe-original
  annotations:
    workloads.cast.ai/jvm-probe-controller-version: "1.4.2"
    workloads.cast.ai/captured-at: "2026-08-14T12:00:00Z"
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: my-java-app
    namespace: default
    uid: 1a04486e-a9d7-4c70-8d05-2b822d2c394c
  original:
    containers:
      - name: spring
        livenessProbe: null                # explicit null = was absent
        livenessPresent: false             # disambiguates "absent" vs "not captured"
        readinessProbe:
          httpGet:
            path: /health
            port: 8080
          initialDelaySeconds: 10
        readinessPresent: true
        startupProbe: null
        startupPresent: false
      # containers we did NOT touch are omitted entirely
status:
  conditions:
    - type: Captured
      status: "True"
      reason: SnapshotStored
      observedGeneration: 1
    - type: RolledBack
      status: "False"
      reason: NotRequested
      observedGeneration: 1
```

### 3.3 Naming convention

- CRD name: `<workload-namespace>-<workload-name>-<uid8>` (lowercased, DNS-1123 sanitised), where `uid8` is the first 8 hex chars of `sha256(targetRef.uid)`.
- Rationale: avoids collisions between `default/my-app` and `prod/my-app`, **and** between distinct workloads that sanitise to the same `<ns>-<name>` string (e.g. namespace `a-b` + name `c` vs. namespace `a` + name `b-c`, which both collapse to `a-b-c`). The UID hash makes the CRD name injective for the workload's lifetime.
- Long names: if the full name exceeds 253 chars (DNS limit), we truncate the `<ns>-<name>` prefix to fit, keeping the `-<uid8>` suffix intact.
- The full workload UID remains in `spec.targetRef.uid` for name-reuse verification at rollback time; the hash in the name is purely for uniqueness.

### 3.4 Presence flags — why they matter

The Go `*corev1.Probe` and `[]TopologySpreadConstraint` fields can be `nil` for two reasons:

1. The workload never had them → we want rollback to remove them.
2. We didn't capture that container → we want rollback to leave them alone.

An explicit `*Present bool` (or `absent bool` at the top level) disambiguates. Rollback logic:

| Snapshot state | Live state | Rollback op |
|---|---|---|
| `Present=false` (was absent) | present | `remove` |
| `Present=false` (was absent) | absent | no-op |
| `Present=true` with value V | any | `replace` with V |

---

## 4. Where snapshots are taken

Both controllers share the same integration seam: capture happens **immediately before** the `Patch()` call, only if we're about to actually mutate the workload for the first time.

### 4.1 TSC controller

In `controllers/tsc-controller/cmd/main.go`, the current sequence around lines 566 and 588 is:

```
Get(workload) at line ~410
  → compute needsPatch (line 407)
  → if needsPatch: build patch, Patch(...)
```

Change to:

```
Get(workload) at line ~410
  → compute needsPatch
  → if needsPatch:
      // Capture is a no-op if EITHER the CRD already exists OR the workload
      // already carries workloads.cast.ai/tsc-managed=true. This prevents
      // capturing post-patch state as "original" after a failed CRD write.
      snapshotter.CaptureIfAbsent(ctx, workload, workload.Spec.Template.Spec.TopologySpreadConstraints)  // NEW
      // The patch below sets both the TSC change AND the managed annotation
      // atomically, so a successful patch guarantees the annotation is set.
      Patch(...)  // now also sets metadata.annotations["workloads.cast.ai/tsc-managed"]="true"
```

The `Get()` at line ~410 already gives us the pre-patch state — we just plumb it through. `CaptureIfAbsent` is a no-op if a `TSCOriginal` already exists for this workload UID, or if the workload already carries the managed annotation.

**`handleWorkloadUpdate` (line ~594)** — no additional capture needed. If the workload has ever been patched by us, the original was already captured on the first pass, and `CaptureIfAbsent` short-circuits.

**`CaptureIfAbsent` semantics (both controllers):**

```
if workload.metadata.annotations["workloads.cast.ai/tsc-managed"] == "true" {
    if crdExists {
        return nil       // steady state: already captured
    }
    // Annotation set but CRD missing → snapshot was lost on a previous
    // reconcile. Capturing NOW would record post-patch state as "original".
    emitEvent(workload, "SnapshotLost", "managed annotation present but TSCOriginal missing; skipping capture")
    return nil
}
// Neither annotation nor CRD → this is the true first mutation.
return createTSCOriginal(...)
```

The managed annotation is written by the same `Patch()` that applies the TSC change, so under normal operation the annotation and the CRD are created together (order: CRD first, then Patch). If the CRD write fails and the Patch succeeds, the annotation is set but the CRD is missing — the `SnapshotLost` path catches this on the next reconcile and refuses to capture stale (post-patch) state as the original.

### 4.2 JVM-Probe controller

Two entry points converge on `applyPatches`:
- `main.go` `applyPatches` (line 623) — the initial injection path.
- `probe_monitor.go` `applyPatches` (line 638) — the auto-fix path.

Wrap both with a single `applyPatchesWithSnapshot`:

```go
// pseudocode
func (c *Controller) applyPatchesWithSnapshot(
    ctx context.Context,
    obj runtime.Object,
    patches []map[string]any,
    preState []ContainerProbeSnapshot,
) error {
    if len(patches) == 0 {
        return nil
    }
    // Best-effort snapshot: no-op if CRD exists OR the workload already
    // carries workloads.cast.ai/jvm-probe-managed=true (§4.1 for full
    // semantics — same shape here). Failures never block the patch.
    c.snapshotter.CaptureIfAbsent(ctx, obj, preState)
    // Append a JSON-Patch op that sets metadata.annotations
    // ["workloads.cast.ai/jvm-probe-managed"]="true". The Patch call
    // below then applies probes AND the annotation atomically.
    patches = append(patches, managedAnnotationPatchOp(obj))  // NEW
    return c.applyPatches(ctx, obj, patches)
}
```

`preState` is built while iterating containers (main.go line ~453). Only containers that appear in `containerPatches` get captured — untouched containers are omitted from the snapshot.

### 4.3 Recommend-mode short-circuit

When global config `mode: recommend` is set (see §7), the wrapper writes the snapshot but skips the `Patch()` call:

```go
c.snapshotter.CaptureIfAbsent(ctx, obj, preState)
if c.cfg.Mode == ModeRecommend {
    c.emitEvent(obj, "WouldPatch", "recommend mode: skipping patch")
    return nil
}
return c.applyPatches(ctx, obj, patches)
```

This gives operators dry-run visibility — snapshots + events show exactly what *would* happen — before flipping to `apply`.

---

## 5. Package layout

Both controllers gain a mirrored (not shared — separate Go modules today) API package plus the snapshotter.

```
controllers/tsc-controller/
  api/workloads/v1/
    types.go                       // NEW: TSCOriginal types, DeepCopy, register
    zz_generated_deepcopy.go       // NEW: controller-gen output
    groupversion_info.go           // NEW: scheme registration
  cmd/
    snapshot.go                    // NEW: TSCSnapshotter
    main.go                        // MODIFIED: call CaptureIfAbsent, honour mode
  helm/castai-tsc-controller/
    crds/
      tscoriginals.workloads.cast.ai.yaml   // NEW: CRD manifest
    templates/
      role.yaml                    // MODIFIED: add tscoriginals verbs

controllers/jvm-probe-controller/
  api/workloads/v1/
    types.go                       // NEW: JVMProbeOriginal types
    zz_generated_deepcopy.go
    groupversion_info.go
  cmd/
    snapshot.go                    // NEW: JVMProbeSnapshotter
    main.go                        // MODIFIED: applyPatchesWithSnapshot wrapper
    probe_monitor.go               // MODIFIED: same wrapper on auto-fix path
  helm/castai-jvm-probe-controller/
    crds/
      jvmprobeoriginals.workloads.cast.ai.yaml
    templates/
      role.yaml                    // MODIFIED

controllers/manifests/rollback/    // NEW: operator recovery tooling
  rollback-tsc.sh
  rollback-jvm-probe.sh
  README.md
```

### 5.1 Snapshotter interface (TSC)

```go
package main

type TSCSnapshotter struct {
    dynClient      dynamic.Interface   // or a generated typed client
    controllerVer  string
    namespace      string              // "castai-agent"
}

// CaptureIfAbsent writes a TSCOriginal for the workload if none exists yet.
// Non-blocking: all failures are logged and swallowed. Emits event on failure.
func (s *TSCSnapshotter) CaptureIfAbsent(
    ctx context.Context,
    workload metav1.Object,          // *appsv1.Deployment or *appsv1.StatefulSet
    original []corev1.TopologySpreadConstraint,
)

// Get reads back an original (used by the rollback CLI's Go build, if any).
func (s *TSCSnapshotter) Get(ctx context.Context, ns, name string) (*TSCOriginal, error)

// RemoveFinalizer clears the finalizer so K8s GC deletes the object.
// Called by (a) the workload-delete informer, (b) the rollback CLI post-restore.
func (s *TSCSnapshotter) RemoveFinalizer(ctx context.Context, ns, name string) error

// SetRolledBack updates status.conditions to reflect a completed rollback.
func (s *TSCSnapshotter) SetRolledBack(ctx context.Context, ns, name string) error
```

### 5.2 Workload-delete informer

Both controllers already have Deployment/StatefulSet informers. Add a `DeleteFunc` handler:

```go
DeleteFunc: func(obj interface{}) {
    workload, err := extractWorkload(obj)  // handles DeletedFinalStateUnknown
    if err != nil { return }
    key := fmt.Sprintf("%s-%s", workload.GetNamespace(), workload.GetName())
    if err := snapshotter.RemoveFinalizer(ctx, "castai-agent", key); err != nil {
        logWarn("finalizer-remove", "failed to remove finalizer for %s: %v", key, err)
    }
}
```

Removing the finalizer lets K8s GC delete the CRD. No retention loop needed.

### 5.3 Concurrency: two controller replicas race to capture

- Both replicas call `CaptureIfAbsent` for the same workload → both `Get` return NotFound → both `Create` → one succeeds, the other gets `409 AlreadyExists` → swallow that error and treat as success.
- Both replicas call `RemoveFinalizer` on workload delete → same idempotent pattern via `PATCH` with the current finalizer set.

Idempotent by construction. No leader lock required around snapshotting itself (leader election is already present for the patch loop).

---

## 6. Rollback tooling (operator UX)

No controller endpoint. Rollback is a **standalone shell CLI** shipped under `controllers/manifests/rollback/`:

### 6.1 `rollback-tsc.sh`

```
rollback-tsc.sh --namespace default --workload deployment/my-app          # single workload
rollback-tsc.sh --namespace default --all                                 # every workload with a TSCOriginal in that ns
rollback-tsc.sh --all-namespaces --dry-run                                # preview, no changes
rollback-tsc.sh --namespace default --workload deployment/my-app --pause-controller
rollback-tsc.sh --namespace default --workload deployment/my-app --keep-original
```

Flow:

1. `kubectl get tscoriginals -n castai-agent -o json --field-selector spec.targetRef.namespace=<ns>`.
2. For each targeted workload:
   a. Verify `spec.targetRef.uid` matches the live workload's UID (protects against name reuse). Skip with warning if not.
   b. Read `spec.original.topologySpreadConstraints` and `spec.original.absent`.
   c. Build a strategic-merge patch:
      - If `absent: true` → `{"spec":{"template":{"spec":{"topologySpreadConstraints":null}}}}` (removes field).
      - Else → `{"spec":{"template":{"spec":{"topologySpreadConstraints": <original list>}}}}`.
   d. `kubectl patch <kind> <name> -n <workload-ns> --type=strategic --patch <patch>`.
   e. On success:
      - `kubectl patch tscoriginal <ns>-<name> -n castai-agent --type=merge --patch '{"status":{"conditions":[...RolledBack=True...]}}'`
      - Unless `--keep-original`: `kubectl patch tscoriginal ... --type=json --patch '[{"op":"remove","path":"/metadata/finalizers"}]'` → K8s GC deletes the CRD.
3. Print a summary table (workload, before, after, status).
4. `--pause-controller` scales the controller Deployment (`castai-tsc-controller` or `castai-jvm-probe-controller`, as installed by the standard Helm chart) to 0 replicas first, restores at the end. Prevents the controller from re-patching mid-rollback. Only supports Deployment-based installs (the default and only shipped shape).

### 6.2 `rollback-jvm-probe.sh`

Same UX. Patch construction is more elaborate because it's per-container:

1. Fetch live workload's containers, build a `name → index` map.
2. For each container in `spec.original.containers`:
   - Skip (with warning) if the container name no longer exists on the live workload.
   - For each of `livenessProbe`, `readinessProbe`, `startupProbe`:
     - `*Present == true` → JSON-Patch `replace` op at `/spec/template/spec/containers/<idx>/<probeField>`.
     - `*Present == false` **and** the field is currently present on the live workload → JSON-Patch `remove` op.
     - `*Present == false` and field already absent → no-op.
3. Apply all ops in one `kubectl patch --type=json`.
4. Update CRD status and remove finalizer as in §6.1.

**Container matching by name, not index** — survives container reordering (a common cause of index-based patch bugs).

### 6.3 Why a shell script, not a controller endpoint

- Rollback is a rare, deliberate, human-authorised action. Baking it into the controller invites self-rollback loops.
- `kubectl` + `jq` are already on every operator's laptop.
- The script works even when the controller is stopped or uninstalled — the CRDs are still there.

---

## 7. Rollout & gating (mirrors Workload Autoscaler)

Three-level gating, matching the `Recommendation` pattern verified via research:

### Level 1 — Global mode (ConfigMap)

```yaml
# controllers/tsc-controller ConfigMap
mode: "apply"          # "apply" (default) | "recommend"
```

| Mode | Behaviour |
|---|---|
| `apply` (default) | Normal operation — capture snapshot, then patch. Matches Workload Autoscaler's `immediate` default. |
| `recommend` | Capture snapshot, emit event `WouldPatch`, skip the patch. Operators can inspect `TSCOriginal` objects to see what *would* be changed. |

Hot-reload via the existing ConfigMap informer — no restart needed to flip modes.

### Level 2 — Per-workload bypass (already exists)

- TSC: `workloads.cast.ai/tsc-bypass: "true"` on the workload.
- JVM: `workloads.cast.ai/jvm-probe-bypass: "true"` on the workload.

When set, the controller skips the workload entirely — no snapshot, no patch.

### Level 3 — Optional whitelist label (new, off by default)

```yaml
# ConfigMap
whitelistOnly: "false"    # default
```

When `whitelistOnly: "true"`, only workloads carrying the label are eligible:
- TSC: `workloads.cast.ai/tsc-controller-enabled: "true"`
- JVM: `workloads.cast.ai/jvm-probe-controller-enabled: "true"`

The label must be on both the controller (Deployment/StatefulSet) and the pod template, matching Workload Autoscaler's whitelisting convention.

### 7.1 New ConfigMap knobs (both controllers)

```yaml
mode: "apply"                # apply | recommend
whitelistOnly: "false"       # if true, only labeled workloads are eligible
snapshotEnabled: "true"      # kill-switch — set to false to disable snapshotting entirely
```

**Kill-switch matters:** if snapshotting misbehaves in production, ops can flip `snapshotEnabled: "false"` and hot-reload without redeploying. The patch loop keeps working; rollback data just stops being captured.

---

## 8. RBAC additions

### 8.1 Controller ServiceAccount (per controller)

Add a `Role` in `castai-agent` (not ClusterRole — the CRDs are namespace-scoped and live there):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: castai-tsc-controller-originals
  namespace: castai-agent
rules:
  - apiGroups: ["workloads.cast.ai"]
    resources: ["tscoriginals"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["workloads.cast.ai"]
    resources: ["tscoriginals/status"]
    verbs: ["update", "patch"]
  - apiGroups: ["workloads.cast.ai"]
    resources: ["tscoriginals/finalizers"]
    verbs: ["update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: castai-tsc-controller-originals
  namespace: castai-agent
subjects:
  - kind: ServiceAccount
    name: castai-workload-controllers
    namespace: castai-agent
roleRef:
  kind: Role
  name: castai-tsc-controller-originals
  apiGroup: rbac.authorization.k8s.io
```

`jvm-probe-controller` gets the analogous Role on `jvmprobeoriginals`.

### 8.2 CRD verify (optional)

The controller can pre-flight the CRD's existence:

```yaml
- apiGroups: ["apiextensions.k8s.io"]
  resources: ["customresourcedefinitions"]
  resourceNames: ["tscoriginals.workloads.cast.ai"]
  verbs: ["get"]
```

Fail fast at startup with a clear error if the CRD isn't installed, rather than failing on every reconcile.

### 8.3 Rollback CLI

Uses the operator's own kubeconfig. No controller RBAC involved. Operator needs:
- Read on `tscoriginals` / `jvmprobeoriginals` in `castai-agent`.
- `patch` on `deployments`, `statefulsets` in target namespaces.
- Optional: `patch` on the CRDs (for status update and finalizer removal).

---

## 9. Lifecycle via finalizer (replaces retention)

The finalizer `workloads.cast.ai/tsc-original` (or `jvm-probe-original`) is what makes retention automatic:

```
1. First mutation:
   controller creates TSCOriginal with finalizer set
2. Steady state:
   TSCOriginal exists as long as the workload exists
3. Workload deleted:
   controller's Deployment/StatefulSet informer fires DeleteFunc
   → controller removes the finalizer from the TSCOriginal
   → K8s garbage collector deletes the TSCOriginal
4. Rollback via CLI:
   → CLI patches workload back to original state
   → CLI updates status.conditions[type=RolledBack].status=True
   → unless --keep-original, CLI removes finalizer → GC deletes CRD
```

**Guarantees:**
- No orphan CRDs (workload gone → CRD gone).
- No time-based expiry needed (regressions found months later can still be rolled back).
- No periodic GC loop code to maintain.
- CRD count in the cluster = count of managed workloads (predictable).

**Edge cases:**

| Case | Behaviour |
|---|---|
| Controller crashes between CRD create and workload delete-event | On restart, the informer re-lists workloads. Controller reconciles: any TSCOriginal whose `targetRef.uid` doesn't match a live workload gets its finalizer removed. Runs once at startup. |
| Both replicas try to remove the finalizer simultaneously | Optimistic-concurrency `PATCH` — one wins, the other sees the finalizer already gone → no-op. |
| Workload deleted while controller is down | Same reconciliation-on-startup path catches it (orphaned by UID mismatch). |
| Workload deleted and immediately recreated with the same name | New workload has a new UID. Controller sees `TSCOriginal` with stale UID → removes finalizer, GC deletes CRD → controller then creates a fresh TSCOriginal for the new UID on first patch. Correct behaviour. |

---

## 10. Failure modes & mitigations

| Failure | Impact | Mitigation |
|---|---|---|
| CRD create fails at first-patch time | Rollback data lost for that workload | Best-effort: log `WARN`, emit event `SnapshotFailed`, continue with patch. Snapshot never blocks reconcile. The patch itself sets a managed annotation, so on the next reconcile `CaptureIfAbsent` sees annotation-present + CRD-absent and emits `SnapshotLost` rather than capturing the post-patch state as "original" (§4). |
| CRD `409 AlreadyExists` on Create | Two replicas raced | Swallow, treat as success. |
| Two workloads sanitise to the same `<ns>-<name>` | Would silently overwrite pre-fix | UID hash in CRD name (§3.3) makes names injective per workload UID; distinct workloads always get distinct CRDs. |
| CRD not installed | Every reconcile logs an error | Pre-flight check at startup: log fatal + emit event, refuse to start until CRD is present. Helm install applies the CRD from `crds/`. |
| ConfigMap `mode: recommend` misinterpreted as `apply` | Unwanted patches | Config parser rejects unknown values (fail closed to `recommend`), emits `ConfigInvalid` event. |
| Rollback CLI targets a container that no longer exists | Skipped | CLI matches by container name, logs skip line, continues with other containers. |
| Rollback CLI while controller is running | Controller re-patches immediately after rollback | `--pause-controller` scales the Deployment to 0 first; docs strongly recommend this flag for non-dry-run runs. |
| CRD schema drift between controller versions | Old originals unparseable | v1 schema is stable; if we ever need v2, add a conversion webhook. For now, additions must be backward-compatible (optional fields only). |
| UID mismatch (workload deleted + recreated with same name) | Rollback would apply stale data | CLI verifies `targetRef.uid` matches live workload, skips + warns if not. |

---

## 11. CRD installation

Helm auto-applies files under `crds/` at install time. Chart layout:

```
controllers/tsc-controller/helm/castai-tsc-controller/
  crds/
    tscoriginals.workloads.cast.ai.yaml
  templates/
    ...
```

Same for `jvm-probe-controller`.

**Helm quirk:** `crds/` files are applied on `helm install` but *not* updated on `helm upgrade`. If we ever change the CRD schema, upgrade docs must instruct operators to `kubectl apply -f crds/` explicitly. For v1 the schema is fixed — no upgrade problem.

**`install.sh` update:** the existing installer applies manifests from `controllers/manifests/`. Add a `controllers/manifests/crds/` directory that bundles both CRDs so the plain-kubectl install path works too:

```
controllers/manifests/
  crds/
    tscoriginals.workloads.cast.ai.yaml
    jvmprobeoriginals.workloads.cast.ai.yaml
  ...existing manifests...
```

`install.sh` applies `crds/` first, waits for the CRDs to be Established, then applies the rest. Same pattern the Workload Autoscaler install uses.

---

## 12. Test plan

1. **Unit — snapshotter:** happy path, 409 race, absent-vs-empty distinction, disabled-flag no-op, finalizer add/remove.
2. **Unit — rollback patch builder** (extract into a small Go package for testability even though the CLI is bash): `absent=true` → remove op; `Present=true` → replace op; container-by-name matching survives reordering.
3. **Integration — envtest:**
   - TSC: create Deployment without TSC → controller injects → `TSCOriginal` has `absent: true` → run rollback → Deployment has no TSC → CRD deleted via finalizer.
   - TSC: create Deployment with user TSC `X` → controller replaces with `Y` via annotation → CRD has `X` → rollback → Deployment TSC == `X`.
   - JVM: identical flow, three probe permutations (all absent, some present, all present).
   - JVM: reorder containers between capture and rollback → rollback still targets the right container.
   - Lifecycle: delete workload → informer removes finalizer → CRD gone.
   - Startup reconciliation: create CRD with stale UID → controller start → CRD gone.
4. **Mode gating:**
   - `mode: recommend` → CRD created, workload untouched, event emitted.
   - `whitelistOnly: true` + no label → workload untouched, no CRD.
   - `snapshotEnabled: false` → workload patched, no CRD created.
5. **E2E smoke:** `install.sh` on kind → apply test workloads → `rollback-*.sh --all-namespaces --dry-run` → verify output → real rollback with `--pause-controller` → verify workload restored.
6. **Chaos:** kill controller mid-patch (after CRD create, before workload patch) → on restart, patch is re-applied, CRD already exists → no duplicate write.

---

## 13. Rollout plan

1. **Ship snapshotter behind default-off kill-switch** (`snapshotEnabled: "false"`). Global `mode: "apply"` (matches Workload Autoscaler default). Deploy. Verify no regression to patch loop.
2. **Enable snapshotting in one cluster** (`snapshotEnabled: "true"`). Observe `TSCOriginal` / `JVMProbeOriginal` objects appear over 24h. Sanity-check contents.
3. **Enable `snapshotEnabled: "true"` by default** in the next minor release. Keep kill-switch available for a full release cycle.
4. **Ship rollback scripts** in the same release as step 1, documented as "requires snapshots enabled".

Note: unlike v1 of this design, we do **not** default the global mode to `recommend`. The user explicitly chose `apply` as the default, matching Workload Autoscaler. Operators who want dry-run behaviour flip to `mode: recommend` themselves.

---

## 14. Alternatives considered

| Alternative | Why rejected |
|---|---|
| **ConfigMap-based snapshot store** (v1 of this doc) | User feedback: inconsistent with existing CAST AI CRD pattern; operators already know `recommendations.autoscaling.cast.ai`. |
| **Cluster-scoped CRDs** | Diverges from Workload Autoscaler layout; RBAC needs ClusterRole. Consistency with existing pattern wins. |
| **`kubectl.kubernetes.io/last-applied-configuration` annotation** | Only set by client-side apply; absent on Helm/operator-installed workloads. Gets overwritten by other tooling. |
| **Store snapshot as annotation on the workload itself** | Our own patch would rewrite it. Also noisy in every `kubectl get -o yaml`. |
| **Full workload dump (Velero-lite)** | Leaks secrets/env, wastes storage, unnecessary — we only mutate 1–3 fields. |
| **Delete-and-recreate workload on rollback** | Causes downtime and reschedules pods for reasons unrelated to the rollback. Patch-back is strictly better. |
| **Auto-rollback on error signals** | Too aggressive; creates bimodal behaviour that's hard to reason about. Explicit human-driven rollback is safer. |
| **Time-based CRD retention** | Regressions can surface months later. Storage is cheap; keep forever + finalizer GC on workload delete. |

---

## 15. Confirmed decisions

| Question | Answer |
|---|---|
| Storage backend | Two CRDs in `workloads.cast.ai/v1`, mirroring `Recommendation` |
| CRD namespace | `castai-agent` (mirrors Recommendation exactly) |
| CRD scope | Namespace-scoped |
| Naming | `<workload-namespace>-<workload-name>-<uid8>`, DNS-1123 sanitised; prefix truncated (not suffix) if length exceeds 253 chars |
| Lifecycle | Finalizer + workload-delete informer; no time-based retention |
| Default mode | `apply` (matches Workload Autoscaler's `immediate` default) |
| Rollback interface | Standalone shell CLI, no controller endpoint |
| PDB rollback | Out of scope (`kubectl delete pdb` suffices) |

---

## 16. Open items for implementation phase

Not blockers for design sign-off, but worth flagging:

1. **CRD schema generation** — decide between hand-written CRD YAML vs `controller-gen`. Recommendation: `controller-gen` for consistency with any future controllers, but the surface is small enough that hand-written is viable.
2. **Prometheus metrics** — add `castai_controller_originals_total{controller=...,namespace=...}` and `castai_controller_snapshot_failures_total{controller=...,reason=...}`. Cheap and useful for capacity/alerting.
3. **Multi-cluster CLI** — rollback scripts should accept `--context` for straightforward multi-cluster runs.
4. **CRD conversion strategy** — for now, v1 only. If we ever need v1beta1 → v1 or v1 → v2, add a conversion webhook. Design accommodates it (fields are additive-only).

---

## 17. Summary answer to the original question

> "Is it possible to dump the deploys before making the changes?"

**Yes — via two new namespaced CRDs (`TSCOriginal`, `JVMProbeOriginal`) in `castai-agent`, mirroring the existing `recommendations.autoscaling.cast.ai` pattern.**

Snapshots capture only the specific spec slices each controller mutates (TSC list, or per-container probe triplet), written **once per workload** immediately before the first patch. A finalizer + workload-delete informer handles cleanup automatically — no retention config needed. A standalone `rollback-*.sh` CLI reads the CRDs and issues inverse patches to restore the original state.

The mutation hot path is unchanged apart from one best-effort call that never blocks reconciliation on failure. Rollout follows the Workload Autoscaler pattern (`mode: apply` default, `mode: recommend` for dry-run, per-workload bypass annotations already exist, optional whitelist label).
