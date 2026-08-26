# Rollback design for TSC & JVM-Probe Controllers

**Status:** Implemented (PRs #19, #20, #21) — this document now describes the shipped behaviour. Operator procedure lives in [`docs/rollback-operator-runbook.md`](rollback-operator-runbook.md).
**Scope:** `controllers/tsc-controller`, `controllers/jvm-probe-controller`. The PDB controller is out of scope: it manages standalone `PodDisruptionBudget` objects, so rollback there is trivially `kubectl delete pdb`.
**Version:** v2 (CRD-based, mirroring `recommendations.autoscaling.cast.ai` pattern).

---

## 0. Implementation map

| Layer | Lives in | What it does |
|---|---|---|
| CRDs (`TSCOriginal`, `JVMProbeOriginal`) | `apis/workloads/v1/` + `crds/` + `controllers/crds/helm/castai-guardrails-crds/` | Store per-workload original state in `castai-agent`. Namespaced, namespaced to `castai-agent`. |
| Shared snapshot module | `snapshot/` (`capture.go`, `rollback.go`, `naming.go`, `conditions.go`, `finalizer.go`, `*_client.go`) | Collision-safe naming, `CaptureIfAbsent`, `Rollback` loop, finalizer lifecycle, status-condition helpers. Imported by both controllers. |
| Typed clientsets | `clientset/versioned/typed/workloads/v1/{tscoriginal,jvmprobeoriginal}.go` | Generated typed clients used by `snapshot/` (no `dynamic.Interface`). |
| TSC controller wiring | `controllers/tsc-controller/cmd/{main,configmap,rollback}.go` | ConfigMap parsing, capture call before Strategic Merge Patch, rollback transition detection, delete-handler finalizer cleanup. |
| JVM-Probe controller wiring | `controllers/jvm-probe-controller/cmd/{main,configmap,probe_monitor,rollback}.go` | Same shape as TSC, plus a JSON-Patch `managedAnnotation` op appended atomically with probe ops. |
| RBAC | `controllers/{tsc,jvm-probe}-controller/helm/.../templates/rbac.yaml` | Adds `workloads.cast.ai/<plural>` verbs on the controller's existing namespaced `Role`. |
| Helm sub-chart | `controllers/crds/helm/castai-guardrails-crds/` | Installs both CRDs once. Both controller charts depend on it (`crds.enabled` toggle for pre-installed CRDs). |
| Generated code | `hack/update-codegen.sh`, `hack/verify-codegen.sh` | Runs `deepcopy-gen`, `client-gen`, `lister-gen`, `informer-gen`; CI check ensures checked-in generated files match. |

PR ordering that landed the implementation: #19 (CRDs + snapshot module) → #20 (TSC wiring) → #21 (JVM-Probe wiring). Operator docs (this file's `## Implementation` + the new runbook) are the final piece.

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

## 5. Package layout (shipped)

The shipped layout moves shared types and logic out of per-controller `api/` packages into top-level `apis/`, `clientset/`, and `snapshot/` modules. Both controllers import from these shared modules via `go.mod` `replace` directives during development (pinned semver for release builds).

```
castai-guardrails-controllers/
├── apis/workloads/v1/                          // NEW — PR 1
│   ├── doc.go                                  // +k8s:deepcopy-gen=package
│   ├── register.go                             // SchemeBuilder, AddToScheme
│   ├── types_tscoriginal.go                    // TSCOriginal + Spec/Status/TargetRef
│   ├── types_jvmprobeoriginal.go               // JVMProbeOriginal + ContainerProbes
│   ├── zz_generated_deepcopy.go                // deepcopy-gen output
│   └── zz_generated_register.go                // register-gen output
│
├── clientset/versioned/                        // NEW — PR 1 (generated)
│   ├── clientset.go                            // client-gen output
│   ├── scheme/                                 // generated
│   ├── typed/workloads/v1/
│   │   ├── tscoriginal.go                      // generated typed client
│   │   └── jvmprobeoriginal.go
│   ├── fake/                                   // fake clientset for tests
│   ├── informers/                              // lister-gen + informer-gen
│   └── listers/
│
├── snapshot/                                   // NEW — PR 1 (shared logic)
│   ├── capture.go                              // CaptureIfAbsent + helpers
│   ├── rollback.go                             // Rollback loop + inverse-patch orchestrator
│   ├── naming.go                               // CollisionSafeName (uid-hash suffix)
│   ├── conditions.go                           // SetCondition, IsReady, IsRolledBack
│   ├── finalizer.go                            // AddFinalizer, RemoveFinalizer
│   ├── tsc_client.go                           // Manager[TSCOriginal] wiring
│   ├── jvm_client.go                           // Manager[JVMProbeOriginal] wiring
│   └── *.{go,_test.go}                         // unit tests (≥80% coverage)
│
├── crds/                                       // NEW — PR 1 (raw manifests)
│   ├── workloads.cast.ai_tscoriginals.yaml
│   └── workloads.cast.ai_jvmprobeoriginals.yaml
│
├── controllers/
│   ├── crds/                                   // NEW — PR 1 (helm sub-chart)
│   │   └── helm/castai-guardrails-crds/
│   │       ├── Chart.yaml
│   │       ├── values.yaml
│   │       ├── charts/castai-guardrails-crds-0.1.0.tgz   # sub-chart tarball
│   │       └── templates/
│   │           ├── tscoriginal-crd.yaml
│   │           └── jvmprobeoriginal-crd.yaml
│   │
│   ├── tsc-controller/                         // MODIFIED — PR 2
│   │   ├── cmd/
│   │   │   ├── main.go                         // wires snapshot.Manager, detect rollback transition
│   │   │   ├── configmap.go                    // NEW — managementEnabled/rollbackOnDisable/mode parsing
│   │   │   ├── configmap_test.go               // NEW — parser edge cases
│   │   │   ├── rollback.go                     // NEW — tscInverseFn (strategic-merge inverse patch)
│   │   │   └── rollback_test.go                // NEW — inverse-patch construction
│   │   ├── go.mod                              // + apis, clientset, snapshot module deps
│   │   └── helm/castai-tsc-controller/
│   │       ├── Chart.yaml                      // + dependency on castai-guardrails-crds
│   │       ├── values.yaml                     // + management.{enabled,rollbackOnDisable,mode}, snapshot.enabled
│   │       └── templates/
│   │           ├── configmap.yaml              // + new keys
│   │           ├── rbac.yaml                   // + workloads.cast.ai verbs
│   │           └── deployment.yaml             // unchanged
│   │
│   ├── jvm-probe-controller/                   // MODIFIED — PR 3
│   │   └── ... same shape as tsc-controller ...
│   │
│   └── manifests/                              // unchanged (out-of-tree kustomize)
│
├── docs/
│   ├── rollback-design.md                      // this file — Implementation map updated post-merge
│   └── rollback-operator-runbook.md            // NEW — operator-facing procedure (PR 4)
│
├── hack/                                       // NEW — PR 1
│   ├── update-codegen.sh                       // runs deepcopy-gen, register-gen, client-gen, lister-gen, informer-gen
│   ├── verify-codegen.sh                       // CI check: generated files up-to-date
│   └── boilerplate.go.txt                      // license header for generated files
│
└── go.mod / go.sum                             // unchanged at repo root (per-controller modules)
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

## 6. Rollback flow (shipped)

Rollback is **driven by the controller**, not a separate CLI. The original design (§14, alternatives) reconsidered the CLI and the v2 implementation pushes the rollback loop into the reconcile path. Operator trigger is a single ConfigMap edit; the controller detects the transition and runs the inverse-patch loop asynchronously.

### 6.1 Trigger (shipped)

The reconcile loop is gated by two ConfigMap booleans plus a mode flag. See §7 for full key listing.

| `managementEnabled` | `rollbackOnDisable` | What the controller does |
|:-:|:-:|---|
| `true` (default) | any | Normal reconcile — capture snapshot before patching (unless `mode: recommend`). |
| `false` | `false` (default) | Freeze — skip all mutation, leave already-patched workloads as-is. |
| `false` | `true` | **Triggered rollback**: on the `true → false` transition, run the rollback loop **once**, then behave as freeze. Each CRD restored is marked `status.conditions[type=RolledBack]=True`. |

The transition detector is the ConfigMap watcher. When it sees `old.managementEnabled && !new.managementEnabled && new.rollbackOnDisable`, it fires `runRollback()` in a goroutine — the informer callback must not block.

```go
if oldCfg.ManagementEnabled && !newCfg.ManagementEnabled && newCfg.RollbackOnDisable {
    go c.runRollback()
}
```

### 6.2 The rollback loop

`runRollback()` calls `snapshot.Manager.Rollback(ctx, inverseFn)`. Per CRD:

1. Skip if `status.conditions[type=RolledBack].status == "True"` (idempotent re-run).
2. Resolve the target workload by `TargetRef` (Deployment or StatefulSet).
   - Not found → mark `RolledBack=True, reason=TargetGone`, remove finalizer.
   - UID mismatch (workload deleted + recreated with same name) → same as not-found.
3. Build the inverse patch (see §6.3 and §6.4 for type-specific logic).
4. Apply patch.
5. On success: set `status.conditions[type=RolledBack].status="True", reason=RollbackApplied`. **Keep the finalizer** — see §6.5.
6. On failure: log + emit event, leave the CRD unchanged, continue with the next. Return an aggregated error at the end.

### 6.3 TSC inverse patch (Strategic Merge)

Controller: `tsc-controller`. Built in `controllers/tsc-controller/cmd/rollback.go::tscInverseFn`.

```go
// Inverse of "castai overwrote topologySpreadConstraints":
//   - OriginalTSCsPresent=false  -> topologySpreadConstraints: null  (remove field)
//   - OriginalTSCsPresent=true, OriginalTSCs=nil -> same as above
//   - OriginalTSCsPresent=true, OriginalTSCs=[]  -> set field to []
//   - Otherwise -> set field to OriginalTSCs
```

The inverse patch is wrapped in `{ "spec": { "template": { "spec": { "topologySpreadConstraints": <value-or-null> } } } }` and applied with `types.StrategicMergePatchType`. `kubectl patch` equivalent:

```bash
kubectl -n <workload-ns> patch deploy/<name> --type=strategic \
  --patch '{"spec":{"template":{"spec":{"topologySpreadConstraints":null}}}}'
# or with the original list:
kubectl -n <workload-ns> patch deploy/<name> --type=strategic \
  --patch '{"spec":{"template":{"spec":{"topologySpreadConstraints":[{"maxSkew":1,...}]}}}}'
```

The same shape applies to StatefulSets — `tscInverseFn` switches on `TargetRef.Kind`.

### 6.4 JVM-Probe inverse patch (JSON Patch)

Controller: `jvm-probe-controller`. Built in `controllers/jvm-probe-controller/cmd/rollback.go::jvmInverseFn`.

For each container in the live workload, look up the captured `ContainerProbes` by **container name** (not index — survives reordering):

| Captured state | Rollback op |
|---|---|
| `LivenessPresent=false` (castai added it) | `{"op":"remove","path":"/spec/template/spec/containers/<i>/livenessProbe"}` |
| `LivenessPresent=true` (castai replaced existing) | `{"op":"replace","path":"/spec/template/spec/containers/<i>/livenessProbe","value":<original probe>}` |
| Container name not in snapshot | skip — castai never touched it |

Same three-way switch for `readinessProbe` and `startupProbe`. All ops are concatenated into a single JSON-Patch document and applied with `types.JSONPatchType`:

```bash
kubectl -n <workload-ns> patch deploy/<name> --type=json \
  --patch '[
    {"op":"remove","path":"/spec/template/spec/containers/0/livenessProbe"},
    {"op":"replace","path":"/spec/template/spec/containers/0/readinessProbe","value":{...}},
    {"op":"remove","path":"/spec/template/spec/containers/0/startupProbe"}
  ]'
```

### 6.5 CRD retention and `RolledBack=True`

**Post-rollback, the CRD is kept** — never garbage-collected by the rollback loop. This is intentional:

- Operators want to audit *what was rolled back, when, and why* after the fact.
- If `managementEnabled` is later flipped back to `true`, `CaptureIfAbsent` sees `RolledBack=True` and treats the CRD as absent: it deletes the old CRD and creates a fresh snapshot from the (now rolled-back) workload state. This gives operators a clean “roll back → inspect → resume with a new baseline” cycle.
- A snapshot with `RolledBack=True` and `Ready=True` is never overwritten by `CaptureIfAbsent` until the rolled-back-flag is cleared (which only happens via this delete-and-recreate path).

Operators can still `kubectl delete tscoriginal` / `kubectl delete jvmprobeoriginal` manually — see [`docs/rollback-operator-runbook.md`](rollback-operator-runbook.md) § “Recovering from accidental patch”.

The CRD's `status.conditions` exposes two lifecycle conditions, queried with `kubectl get tsco -o jsonpath='{.items[*].status.conditions}'`:

| Condition | Meaning |
|---|---|
| `Ready=True` | Snapshot was successfully captured and a patch was applied at least once. |
| `RolledBack=True` | Rollback loop successfully restored the target workload from this snapshot. `reason` is one of `RollbackApplied`, `TargetGone`, `BypassAnnotation`. |

### 6.6 Why in-controller instead of a CLI

The original design proposed `rollback-tsc.sh` / `rollback-jvm-probe.sh` (now removed). The v2 implementation moves the loop into the controller because:

- One reconcile-driven code path handles capture, apply, and rollback. No second binary to ship, no separate RBAC context (kubectl auth via user vs. controller SA).
- ConfigMap hot-reload is already implemented in both controllers — piggy-backs for free.
- Operators can put the flip in their GitOps repo (Argo / Flux) and treat rollback declaratively.
- Rollback remains a deliberate, human-authorised action — the `managementEnabled=true → false` transition is what fires it, and flipping it back requires another operator edit.

`.kimchi/docs/manual-rollback-jvm-probes-and-tsc.md` retains the manual `kubectl patch` recipes for environments where the controllers aren't yet wired to the CRD feature.

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

### 7.1 ConfigMap knobs (both controllers, shipped)

All four knobs are hot-reloaded via the existing ConfigMap watch — no controller restart needed.

| Key | Default | Type | Effect |
|---|:-:|---|---|
| `managementEnabled` | `true` | bool | Master switch. `false` freezes the controller — no capture, no patch. |
| `rollbackOnDisable` | `false` | bool | Only meaningful when `managementEnabled=false`. When `true`, the `true→false` transition of `managementEnabled` triggers a one-shot rollback loop that restores every snapshotted workload. CRDs are kept post-rollback with `status.conditions[type=RolledBack]=True`. |
| `mode` | `apply` | enum | `apply` — capture + patch (normal). `recommend` — capture and emit events but do **not** patch. Useful for first-Prod-install dry-run. Unknown values fail the ConfigMap parse. |
| `snapshotEnabled` | `true` | bool | Kill-switch for the snapshot path. `false` disables `CaptureIfAbsent`; the patch loop keeps working but no CRD is written. **Use this as the first lever if snapshots misbehave in production** — flip to `false` and hot-reload, no redeploy. |

Example ConfigMap (TSC; JVM-Probe identical):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: castai-tsc-controller-config
  namespace: castai-agent
data:
  managementEnabled: "true"
  rollbackOnDisable: "false"
  mode: "apply"
  snapshotEnabled: "true"
  # ... existing knobs (minReplicas, labelSelector, ...) ...
```

Env-var overrides (final word) for emergency operations:

- `MANAGEMENT_ENABLED=true|false` — override at startup.
- `MODE=apply|recommend` — override at startup.
- `OPERATOR_NAMESPACE` — override the namespace where CRDs live (default `castai-agent`).

Env vars are read once at startup; flipping them requires a controller restart. Use ConfigMap keys for hot-reload.

**Recommended mode for first Prod install:** `mode=recommend` + `snapshotEnabled=true`. Watch `kubectl get tscoriginals -n castai-agent` and controller events for one rollout cycle, then flip to `mode=apply`. See [`docs/rollback-operator-runbook.md`](rollback-operator-runbook.md) § “Recommend mode (dry-run)”.

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
