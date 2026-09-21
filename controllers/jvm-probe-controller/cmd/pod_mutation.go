// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Defaults applied to the injected startup probe when no annotation override
// is present.
const (
	defaultStartupPeriodSeconds       int32 = 15
	defaultStartupFailureThreshold    int32 = 40
	startupProbeSuccessThresholdValue int32 = 1
)

// jsonPatchOp is the subset of RFC 6902 operations the webhook emits.
type jsonPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
	From  string      `json:"from,omitempty"`
}

// podMutationResult holds the patch set produced for a single Pod admission
// request along with a flag indicating whether any mutation was applied.
// mutationApplied gates the managed-annotation patch and the decision to
// attach a non-empty Patch field to the AdmissionResponse.
type podMutationResult struct {
	patches         []jsonPatchOp
	mutationApplied bool
}

// mutatePod is the entry point invoked from the HTTP handler. It decodes the
// AdmissionReview's Pod, builds the patch set, and returns an
// AdmissionResponse suitable for writing back to the API server.
//
// The function is safe for dry-run requests: when req.DryRun is true the
// patch is still computed and returned, but the API server does not
// persist the mutation. The webhook only describes mutations — it does
// not apply them.
func mutatePod(_ context.Context, review *admissionv1.AdmissionReview, cfg *JVMConfig) (*admissionv1.AdmissionResponse, error) {
	if review == nil || review.Request == nil {
		return nil, fmt.Errorf("nil admission review or request")
	}
	req := review.Request

	resp := &admissionv1.AdmissionResponse{
		UID:     req.UID,
		Allowed: true,
	}

	// Admission requests may arrive with the object already decoded or as
	// raw JSON; the Raw field is always populated on the server side. If
	// it is absent there is nothing to mutate — return an allow response
	// without a patch.
	if len(req.Object.Raw) == 0 {
		return resp, nil
	}

	pod := &corev1.Pod{}
	if err := json.Unmarshal(req.Object.Raw, pod); err != nil {
		return nil, fmt.Errorf("decode pod: %w", err)
	}

	// Defense-in-depth: even if buildPodProbePatches were called with a
	// managed config, the entry point refuses to mutate when
	// ManagementEnabled is off. This keeps the disable path testable at
	// the HTTP boundary as well.
	if cfg != nil && !cfg.ManagementEnabled {
		logDebug("jvm-disabled", "Skipping %s/%s, management disabled", pod.Namespace, pod.Name)
		return resp, nil
	}

	result, err := buildPodProbePatches(pod, cfg)
	if err != nil {
		return nil, err
	}
	if !result.mutationApplied || len(result.patches) == 0 {
		return resp, nil
	}

	patchBytes, err := json.Marshal(result.patches)
	if err != nil {
		return nil, fmt.Errorf("marshal patch: %w", err)
	}
	pt := admissionv1.PatchTypeJSONPatch
	resp.PatchType = &pt
	resp.Patch = patchBytes
	return resp, nil
}

// buildPodProbePatches walks the Pod's containers and returns the JSON Patch
// operations that should be applied. It is the core mutation algorithm and
// is kept pure: it does not mutate its input Pod, only builds a patch list.
//
// Algorithm (per container, in order):
//  1. Bypass annotation → no patches anywhere.
//  2. Non-JVM container → skip.
//  3. Apply Pod-level framework override.
//  4. Honour overwrite-* annotations: treat the corresponding probe as
//     missing so it is regenerated.
//  5. If liveness/readiness is missing and injection is enabled, build via
//     BuildProbesForFramework.
//  6. If startup is missing (or overwritten): copy liveness → readiness →
//     built-from-framework, deep-copying to avoid aliasing.
//  7. If clear-delays is set and a probe exists, remove initialDelaySeconds.
//  8. Tune startup probe timing (period/failureThreshold/successThreshold).
//  9. Add the managed Pod annotation when any container was mutated.
func buildPodProbePatches(pod *corev1.Pod, cfg *JVMConfig) (*podMutationResult, error) {
	if pod == nil || cfg == nil {
		return &podMutationResult{}, nil
	}

	// ManagementEnabled is the master switch. When it is off the webhook
	// must not produce any patches — it must remain a no-op even for
	// otherwise-mutating inputs.
	if !cfg.ManagementEnabled {
		logDebug("jvm-disabled", "Skipping %s/%s, management disabled", pod.Namespace, pod.Name)
		return &podMutationResult{}, nil
	}

	annotations := pod.Annotations
	if IsBypassAnnotation(annotations) {
		return &podMutationResult{}, nil
	}

	frameworkOverride := GetFrameworkOverride(annotations)
	overwriteAll := ShouldOverwriteAll(annotations)
	overwriteLiveness := ShouldOverwriteLiveness(annotations) || overwriteAll
	overwriteReadiness := ShouldOverwriteReadiness(annotations) || overwriteAll
	overwriteStartup := ShouldOverwriteStartup(annotations) || overwriteAll
	clearDelays := ShouldClearDelays(annotations)

	// Annotation overrides for startup timing.
	startupPeriod := int32FromAnnotation(annotations, AnnotationJVMProbeStartupPeriod, defaultStartupPeriodSeconds)
	startupFailure := int32FromAnnotation(annotations, AnnotationJVMProbeStartupFailureThreshold, defaultStartupFailureThreshold)

	result := &podMutationResult{}

	// We resolve container indices by position after iterating in slice
	// order. This keeps the path generation simple and deterministic and
	// matches the spec ("target containers by name, not by index" — but
	// the JSON Patch path still uses the resolved index).
	for idx := range pod.Spec.Containers {
		container := &pod.Spec.Containers[idx]

		containerInfo := DetectJVMContainer(*container)
		if !containerInfo.IsJVM {
			continue
		}
		framework := containerInfo.Framework
		if frameworkOverride != "" {
			framework = frameworkOverride
		}

		containerResult, err := buildContainerPatches(
			container, framework, containerInfo, idx, annotations, cfg,
			overwriteLiveness, overwriteReadiness, overwriteStartup,
			clearDelays, startupPeriod, startupFailure,
		)
		if err != nil {
			return nil, err
		}
		if len(containerResult) == 0 {
			continue
		}
		result.patches = append(result.patches, containerResult...)
		result.mutationApplied = true
	}

	if result.mutationApplied {
		// Mark the Pod as managed so downstream tooling can observe the
		// mutation. Idempotency is preserved: on a second invocation the
		// container logic produces no patches so this annotation is not
		// re-emitted.
		if pod.Annotations[AnnotationJVMProbeManaged] != "true" {
			// RFC 6902 forbids an "add" patch that targets a child path
			// when the parent object does not exist. When the Pod has no
			// metadata.annotations at all, we must first create the parent
			// map before patching the single key inside it. Otherwise the
			// API server rejects the patch with "doc is missing path:
			// /metadata/annotations: missing value" and Pod creation
			// crash-loops.
			if len(pod.Annotations) == 0 {
				result.patches = append(result.patches, jsonPatchOp{
					Op:   "add",
					Path: "/metadata/annotations",
					Value: map[string]string{
						AnnotationJVMProbeManaged: "true",
					},
				})
			} else {
				result.patches = append(result.patches, jsonPatchOp{
					Op:    "add",
					Path:  "/metadata/annotations/" + escapeJSONPatchKey(AnnotationJVMProbeManaged),
					Value: "true",
				})
			}
		}
	}

	return result, nil
}

// buildContainerPatches returns the per-container patch operations. Working
// copies of each probe are kept on the stack so we can describe "add" and
// "replace" without aliasing the input container's pointers.
func buildContainerPatches(
	container *corev1.Container,
	framework string,
	containerInfo ContainerInfo,
	containerIndex int,
	annotations map[string]string,
	cfg *JVMConfig,
	overwriteLiveness, overwriteReadiness, overwriteStartup bool,
	clearDelays bool,
	startupPeriod, startupFailureThreshold int32,
) ([]jsonPatchOp, error) {
	var patches []jsonPatchOp

	basePath := fmt.Sprintf("/spec/containers/%d", containerIndex)

	// Step 1: build framework-based probes up-front so the same call can
	// serve both as "fill a missing probe" and as the startup fallback when
	// neither liveness nor readiness exists.
	// If an overwrite annotation is set, force the corresponding injection
	// flag so a probe is regenerated even when the config default is off.
	effectiveAnnotations := annotations
	if overwriteLiveness || overwriteReadiness || overwriteStartup {
		effectiveAnnotations = make(map[string]string, len(annotations)+3)
		for k, v := range annotations {
			effectiveAnnotations[k] = v
		}
		if overwriteLiveness {
			effectiveAnnotations[AnnotationJVMInjectLiveness] = "true"
		}
		if overwriteReadiness {
			effectiveAnnotations[AnnotationJVMInjectReadiness] = "true"
		}
		if overwriteStartup {
			effectiveAnnotations[AnnotationJVMInjectStartup] = "true"
		}
	}
	liveness, readiness, _ := BuildProbesForFramework(framework, containerInfo, effectiveAnnotations, cfg)

	// Decide which probes need to be replaced vs added. "have*" is the
	// effective current state after overwrite annotations are applied.
	haveLiveness := container.LivenessProbe != nil && !overwriteLiveness
	haveReadiness := container.ReadinessProbe != nil && !overwriteReadiness
	haveStartup := container.StartupProbe != nil && !overwriteStartup

	// workingLiveness / workingReadiness are the probes we will actually
	// emit (either the existing one or a freshly-built one). If overwrite
	// is set or the probe is missing, fall back to the framework probe.
	workingLiveness := container.LivenessProbe
	if !haveLiveness && liveness != nil {
		workingLiveness = liveness
	}
	workingReadiness := container.ReadinessProbe
	if !haveReadiness && readiness != nil {
		workingReadiness = readiness
	}

	// Step 2: emit add/replace for liveness and readiness when applicable.
	// patchExists = raw existence on the cluster, used to choose add vs
	// replace. haveLiveness / haveReadiness reflect "do we want to keep the
	// existing probe" — they include the overwrite flag, which is why
	// patchExists differs from them when overwrite=true.
	patchLivenessExists := container.LivenessProbe != nil
	if overwriteLiveness || container.LivenessProbe == nil {
		if liveness != nil {
			patches = append(patches, buildProbePatch(basePath+"/livenessProbe", patchLivenessExists, liveness)...)
		}
	} else if clearDelays && container.LivenessProbe.InitialDelaySeconds != 0 {
		// Clear-delays annotation requests removal of initialDelaySeconds
		// from the existing liveness probe. We only emit the remove when
		// the field is set; the condition guarantees we do not generate a
		// patch that targets an absent field.
		patches = append(patches, removeField(basePath+"/livenessProbe/initialDelaySeconds"))
	}

	patchReadinessExists := container.ReadinessProbe != nil
	if overwriteReadiness || container.ReadinessProbe == nil {
		if readiness != nil {
			patches = append(patches, buildProbePatch(basePath+"/readinessProbe", patchReadinessExists, readiness)...)
		}
	} else if clearDelays && container.ReadinessProbe.InitialDelaySeconds != 0 {
		patches = append(patches, removeField(basePath+"/readinessProbe/initialDelaySeconds"))
	}

	// Step 3: build the startup probe if missing or overwritten. Order is
	// liveness → readiness → framework-built. A deep copy is mandatory so
	// the source probe is not mutated when we tune timing below.
	var startupSrc *corev1.Probe
	switch {
	case workingLiveness != nil:
		startupSrc = workingLiveness
	case workingReadiness != nil:
		startupSrc = workingReadiness
	case shouldInjectStartup(annotations, cfg):
		// Fall back to a framework-built startup probe only when both
		// liveness and readiness are absent. This mirrors the algorithm
		// specified in the migration plan.
		startupSrc = buildStartupFromFramework(framework, containerInfo, annotations, cfg)
	}

	if !haveStartup && startupSrc != nil {
		startup := tuneStartupProbe(CloneProbeOrEmpty(startupSrc), startupPeriod, startupFailureThreshold)
		patches = append(patches, buildProbePatch(basePath+"/startupProbe", container.StartupProbe != nil, startup)...)
	} else if overwriteStartup && startupSrc != nil {
		// Overwrite startup when explicitly requested, even if it already
		// exists. We still tune timing on the replacement.
		startup := tuneStartupProbe(CloneProbeOrEmpty(startupSrc), startupPeriod, startupFailureThreshold)
		patches = append(patches, jsonPatchOp{
			Op:    "replace",
			Path:  basePath + "/startupProbe",
			Value: startup,
		})
	}

	// Step 4: probe alignment. Only run when a startup probe was injected
	// or regenerated for this container in this call. haveStartup reflects
	// "probe is already present after honouring overwriteStartup", so
	// !haveStartup && startupSrc != nil covers both first-time injection
	// and overwrite-driven regeneration. Alignment strips
	// initialDelaySeconds and extends failureThreshold so the liveness and
	// readiness probes cover at least MinProbeWindowSeconds of observation
	// time. The alignment is applied to the final probe state — existing
	// or framework-regenerated — to keep both code paths consistent.
	if !haveStartup && startupSrc != nil && cfg != nil && ShouldAlignProbes(annotations, cfg.AlignProbes) {
		if workingLiveness != nil {
			// Deep-copy so alignProbe cannot re-emit a remove patch for
			// initialDelaySeconds when clear-delays has already scheduled
			// one for the same field (RFC 6902 forbids duplicate removes).
			livenessCopy := workingLiveness.DeepCopy()
			if clearDelays && container.LivenessProbe != nil {
				livenessCopy.InitialDelaySeconds = 0
			}
			patches = append(patches, alignProbe(basePath+"/livenessProbe", livenessCopy, cfg.MinProbeWindowSeconds, cfg.MaxFailureThreshold)...)
		}
		if workingReadiness != nil {
			readinessCopy := workingReadiness.DeepCopy()
			if clearDelays && container.ReadinessProbe != nil {
				readinessCopy.InitialDelaySeconds = 0
			}
			patches = append(patches, alignProbe(basePath+"/readinessProbe", readinessCopy, cfg.MinProbeWindowSeconds, cfg.MaxFailureThreshold)...)
		}
	}

	return patches, nil
}

// tuneStartupProbe applies the timing invariants required for a startup
// probe: no initialDelay, periodSeconds from the annotation (default 15),
// failureThreshold from the annotation (default 40), and successThreshold=1
// (Kubernetes requires this for startup probes).
func tuneStartupProbe(p *corev1.Probe, periodSeconds, failureThreshold int32) *corev1.Probe {
	if p == nil {
		p = &corev1.Probe{}
	}
	p.InitialDelaySeconds = 0
	p.PeriodSeconds = periodSeconds
	p.FailureThreshold = failureThreshold
	p.SuccessThreshold = startupProbeSuccessThresholdValue
	return p
}

// alignProbe emits the JSON Patch operations that align a single
// liveness/readiness probe with the controller's window policy. Alignment:
//
//  1. Strips initialDelaySeconds when present so the startup probe gates
//     startup cleanly.
//  2. Extends failureThreshold so periodSeconds * failureThreshold is at
//     least minWindow seconds, capped at maxFailure.
//
// The function does not modify the probe; it returns patch operations that
// the caller appends to the result. When the probe is nil or the window is
// already met, it returns nil so no spurious patches are emitted.
func alignProbe(path string, probe *corev1.Probe, minWindow, maxFailure int32) []jsonPatchOp {
	if probe == nil {
		return nil
	}
	var ops []jsonPatchOp
	if probe.InitialDelaySeconds > 0 {
		ops = append(ops, removeField(path+"/initialDelaySeconds"))
	}
	if probe.PeriodSeconds > 0 && int32(probe.PeriodSeconds)*probe.FailureThreshold < minWindow {
		needed := int32(math.Ceil(float64(minWindow) / float64(probe.PeriodSeconds)))
		newFailure := probe.FailureThreshold
		if needed > newFailure {
			newFailure = needed
		}
		if newFailure > maxFailure {
			newFailure = maxFailure
		}
		if newFailure != probe.FailureThreshold {
			ops = append(ops, jsonPatchOp{
				Op:    "replace",
				Path:  path + "/failureThreshold",
				Value: newFailure,
			})
		}
	}
	return ops
}

// buildStartupFromFramework is the framework-based fallback for the startup
// probe when no liveness or readiness probe exists. It reuses
// BuildProbesForFramework's logic by calling it once and discarding the
// liveness and readiness results.
func buildStartupFromFramework(framework string, containerInfo ContainerInfo, annotations map[string]string, cfg *JVMConfig) *corev1.Probe {
	_, _, startup := BuildProbesForFramework(framework, containerInfo, annotations, cfg)
	return startup
}

// shouldInjectStartup reports whether startup probe injection is enabled via
// the Pod annotation or, in its absence, the config default.
func shouldInjectStartup(annotations map[string]string, cfg *JVMConfig) bool {
	if v, ok := annotations[AnnotationJVMInjectStartup]; ok {
		return v == "true" || v == "yes" || v == "1"
	}
	if cfg == nil {
		return false
	}
	return cfg.InjectStartupProbe
}

// buildProbePatch returns the add or replace op for a probe field. When the
// probe already exists the patch uses "replace" so it overwrites rather
// than erroring out with "member already exists".
func buildProbePatch(path string, exists bool, probe *corev1.Probe) []jsonPatchOp {
	if probe == nil {
		return nil
	}
	op := "add"
	if exists {
		op = "replace"
	}
	return []jsonPatchOp{{Op: op, Path: path, Value: probe}}
}

// removeField returns a "remove" patch. Callers are responsible for
// emitting this only when the target field is present — the algorithm in
// buildContainerPatches gates each emit on the corresponding field being
// non-zero.
func removeField(path string) jsonPatchOp {
	return jsonPatchOp{Op: "remove", Path: path}
}

// CloneProbeOrEmpty returns a deep copy of probe, or a fresh empty probe
// when probe is nil. Centralising this here lets the caller avoid nil
// checks at every emit site.
func CloneProbeOrEmpty(probe *corev1.Probe) *corev1.Probe {
	if probe == nil {
		return &corev1.Probe{}
	}
	return CloneProbe(probe)
}

// escapeJSONPatchKey escapes characters that are special in JSON Pointer
// reference tokens (RFC 6901 §3): '~' → '~0', '/' → '~1'. Annotation keys
// are typically safe but we still escape defensively because the
// workloads.cast.ai/jvm-probe-* prefix contains '/' which is a literal in
// the key value.
func escapeJSONPatchKey(key string) string {
	out := make([]byte, 0, len(key))
	for i := 0; i < len(key); i++ {
		switch key[i] {
		case '~':
			out = append(out, '~', '0')
		case '/':
			out = append(out, '~', '1')
		default:
			out = append(out, key[i])
		}
	}
	return string(out)
}

// int32FromAnnotation parses an int32-valued annotation with a fallback.
// Invalid values fall back to the default (we do not fail the admission
// request — the rest of the configuration still applies).
func int32FromAnnotation(annotations map[string]string, key string, fallback int32) int32 {
	v, ok := annotations[key]
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return int32(n)
}

// handlePodAdmission is the HTTP entry point for /mutate/pods. It reads the
// AdmissionReview from the body, takes a snapshot of the JVMConfig under
// configLock (so a concurrent hot-reload does not race), and writes the
// response AdmissionReview back to the API server.
//
// Errors during decoding or patch generation are logged and translated into
// an "Allowed: true" response without a patch — the failurePolicy on the
// MutatingWebhookConfiguration is Ignore so we never block Pod creation.
func handlePodAdmission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// requestUID captures the AdmissionRequest UID when available so we can
	// echo it back on every response, including error paths. The API server
	// expects UID to match the request UID for allowed responses.
	var requestUID types.UID

	review := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(review); err != nil {
		logWarn("mutate", "Failed to decode AdmissionReview: %v", err)
		writeAdmissionResponse(w, review, &admissionv1.AdmissionResponse{
			UID:     requestUID,
			Allowed: true,
		})
		return
	}
	if review.Request != nil {
		requestUID = review.Request.UID
	}

	configLock.RLock()
	cfg := config
	configLock.RUnlock()
	if cfg == nil {
		cfg = func() *JVMConfig { d := DefaultJVMConfig(); return &d }()
	}

	resp, err := mutatePod(r.Context(), review, cfg)
	if err != nil {
		logWarn("mutate", "mutatePod returned error: %v", err)
		resp = &admissionv1.AdmissionResponse{
			UID:     requestUID,
			Allowed: true,
		}
	}
	writeAdmissionResponse(w, review, resp)
}

// writeAdmissionResponse is a small helper for the HTTP layer: it sets the
// content type and writes the marshalled AdmissionReview.
func writeAdmissionResponse(w http.ResponseWriter, review *admissionv1.AdmissionReview, response *admissionv1.AdmissionResponse) {
	out := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
	}
	// Only echo the request TypeMeta back when it is populated. Admission
	// requests typically arrive with an empty TypeMeta, and copying an
	// empty APIVersion/Kind would produce a malformed response.
	if review != nil && (review.APIVersion != "" || review.Kind != "") {
		out.TypeMeta = review.TypeMeta
	}
	out.Response = response
	w.Header().Set("Content-Type", "application/json")
	if response == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// failurePolicy=Ignore means we never block Pod creation. We always
	// return 200 here so the API server accepts the review.
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}
