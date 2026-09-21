// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	jsonpatch "github.com/evanphx/json-patch"
)

// applyPatches applies the given patch set to a copy of pod and returns the
// mutated Pod. Used by tests that need to observe the post-patch state.
func applyPatches(t *testing.T, pod *corev1.Pod, patches []jsonPatchOp) *corev1.Pod {
	t.Helper()
	originalJSON, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	patchesJSON, err := json.Marshal(patches)
	if err != nil {
		t.Fatalf("marshal patches: %v", err)
	}
	patchSet, err := jsonpatch.DecodePatch(patchesJSON)
	if err != nil {
		t.Fatalf("decode patches: %v", err)
	}
	mutatedJSON, err := patchSet.Apply(originalJSON)
	if err != nil {
		t.Fatalf("apply patches: %v", err)
	}
	out := &corev1.Pod{}
	if err := json.Unmarshal(mutatedJSON, out); err != nil {
		t.Fatalf("unmarshal mutated pod: %v", err)
	}
	return out
}

// jvmContainer returns a container whose image clearly identifies it as a
// JVM workload. The default framework is detected as "spring-boot" because
// the image contains "spring".
func jvmContainer(name string, mutate func(*corev1.Container)) corev1.Container {
	c := corev1.Container{
		Name:  name,
		Image: "spring-boot-app:1.0",
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
	}
	if mutate != nil {
		mutate(&c)
	}
	return c
}

func defaultTestConfig() *JVMConfig {
	cfg := DefaultJVMConfig()
	return &cfg
}

// idempotentTestConfig returns a config that only injects the startup probe,
// so the second call in TestBuildPodProbePatches_IdempotentOnMutatedPod is a
// true no-op once the startup probe has been installed. The default config
// also injects the readiness probe, which would cause the second call to
// produce an extra patch (the readiness probe and the managed annotation).
func idempotentTestConfig() *JVMConfig {
	cfg := DefaultJVMConfig()
	cfg.InjectReadinessProbe = false
	cfg.InjectLivenessProbe = false
	cfg.InjectStartupProbe = true
	return &cfg
}

// httpGetLiveness, tcpSocketLiveness, etc. are constructors used to express
// "container has only this probe type". They are intentionally minimal so
// the assertions can focus on handler-type preservation.

func httpGetLiveness(path string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromInt(8080),
			},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       10,
	}
}

func tcpSocketLiveness() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(8080)},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       10,
	}
}

func execLiveness() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "true"}},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       10,
	}
}

func grpcLiveness() *corev1.Probe {
	svc := "grpc.health.v1.Health"
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			GRPC: &corev1.GRPCAction{Service: &svc, Port: 9090},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       10,
	}
}

// findPatch returns the first patch with the given path, or nil.
func findPatch(patches []jsonPatchOp, path string) *jsonPatchOp {
	for i := range patches {
		if patches[i].Path == path {
			return &patches[i]
		}
	}
	return nil
}

// patchExists reports whether any patch targets path.
func patchExists(patches []jsonPatchOp, path string) bool {
	return findPatch(patches, path) != nil
}

// ---------------------------------------------------------------------------
// Acceptance criterion 1: Non-JVM Pod → no patch.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_NonJVMPodNoPatch(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "nginx", Image: "nginx:1.25"}, // not JVM
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("mutationApplied = true, want false")
	}
	if len(result.patches) != 0 {
		t.Errorf("patches = %d, want 0", len(result.patches))
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 12: Bypass annotation → no patch.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_BypassNoPatch(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationJVMBypass: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{jvmContainer("app", nil)},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("bypass annotation should suppress mutation; got %d patches", len(result.patches))
	}
}

// ---------------------------------------------------------------------------
// Regression test: a JVM Pod with no metadata.annotations at all must still
// produce a valid patch. RFC 6902 forbids an "add" patch targeting a child
// of an absent object, so the controller must emit a parent-level "add"
// patch that creates /metadata/annotations as a map containing the managed
// annotation. Without this guard the API server rejects the patch with
// "doc is missing path: /metadata/annotations/...: missing value" and Pod
// creation crash-loops.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_NoAnnotationsCreatesParentMap(t *testing.T) {
	// Pod with no annotations at all (Annotations field is nil).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: nil,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Fatalf("expected mutationApplied = true")
	}

	// The parent-level add patch must be present and target
	// /metadata/annotations (not a child path), because the object does
	// not exist yet.
	parentPatch := findPatch(result.patches, "/metadata/annotations")
	if parentPatch == nil {
		t.Fatalf("expected parent-level add patch at /metadata/annotations, got: %+v", result.patches)
	}
	if parentPatch.Op != "add" {
		t.Errorf("parent patch op = %q, want add", parentPatch.Op)
	}
	annotations, ok := parentPatch.Value.(map[string]string)
	if !ok {
		t.Fatalf("parent patch value type = %T, want map[string]string", parentPatch.Value)
	}
	if annotations[AnnotationJVMProbeManaged] != "true" {
		t.Errorf("managed annotation value = %q, want \"true\"; got map: %+v", annotations[AnnotationJVMProbeManaged], annotations)
	}

	// The patch set must apply cleanly. This is the key regression
	// assertion: prior to the fix, this returned an error from the
	// json-patch library because the parent object was missing.
	mutated := applyPatches(t, pod, result.patches)
	if mutated.Annotations == nil {
		t.Fatalf("mutated Pod has nil annotations after applying patches")
	}
	if mutated.Annotations[AnnotationJVMProbeManaged] != "true" {
		t.Errorf("mutated Pod annotations = %+v, want managed=\"true\"", mutated.Annotations)
	}
	// Container-level mutation must still have taken effect.
	if mutated.Spec.Containers[0].StartupProbe == nil {
		t.Errorf("expected startupProbe to be installed on the container")
	}

	// The buggy single-key child patch must NOT be present: emitting both
	// would cause the API server to reject the request with a duplicate
	// path error.
	if patchExists(result.patches, "/metadata/annotations/"+escapeJSONPatchKey(AnnotationJVMProbeManaged)) {
		t.Errorf("did not expect single-key child patch when annotations map is absent; got: %+v", result.patches)
	}
}

// Empty (non-nil) annotations map must follow the same parent-add path as a
// nil map: an empty map serialises the same way at the API server, and the
// "add" operation on a child path would still fail because Kubernetes
// reconstructs the object from typed Go fields where an empty map is
// indistinguishable from a missing one once the patch is applied.
func TestBuildPodProbePatches_EmptyAnnotationsCreatesParentMap(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Fatalf("expected mutationApplied = true")
	}
	parentPatch := findPatch(result.patches, "/metadata/annotations")
	if parentPatch == nil {
		t.Fatalf("expected parent-level add patch for empty annotations, got: %+v", result.patches)
	}
	// Patch set must apply without error.
	applyPatches(t, pod, result.patches)
}

// ---------------------------------------------------------------------------
// Acceptance criterion 2: liveness present, startup missing → copy and tune.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_LivenessCopiedToStartup(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/actuator/health/liveness")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Fatalf("expected mutationApplied = true")
	}

	startupPatch := findPatch(result.patches, "/spec/containers/0/startupProbe")
	if startupPatch == nil {
		t.Fatalf("expected startupProbe patch, got patches: %+v", result.patches)
	}
	if startupPatch.Op != "add" {
		t.Errorf("startup op = %q, want add (probe was missing)", startupPatch.Op)
	}

	probe := decodeProbe(t, startupPatch.Value)
	if probe.HTTPGet == nil || probe.HTTPGet.Path != "/actuator/health/liveness" {
		t.Errorf("startup HTTPGet = %+v, want path /actuator/health/liveness", probe.HTTPGet)
	}
	if probe.InitialDelaySeconds != 0 {
		t.Errorf("startup InitialDelaySeconds = %d, want 0", probe.InitialDelaySeconds)
	}
	if probe.PeriodSeconds != defaultStartupPeriodSeconds {
		t.Errorf("startup PeriodSeconds = %d, want %d", probe.PeriodSeconds, defaultStartupPeriodSeconds)
	}
	if probe.FailureThreshold != defaultStartupFailureThreshold {
		t.Errorf("startup FailureThreshold = %d, want %d", probe.FailureThreshold, defaultStartupFailureThreshold)
	}
	if probe.SuccessThreshold != 1 {
		t.Errorf("startup SuccessThreshold = %d, want 1", probe.SuccessThreshold)
	}

	// Managed annotation patch should also be present. When the Pod has
	// no annotations at all the controller emits a parent-level "add"
	// patch at /metadata/annotations; otherwise it emits the child-key
	// patch. Both forms convey the same managed state.
	if !patchExists(result.patches, "/metadata/annotations/"+escapeJSONPatchKey(AnnotationJVMProbeManaged)) &&
		!patchExists(result.patches, "/metadata/annotations") {
		t.Errorf("managed annotation patch missing")
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 3: liveness missing, readiness present, startup
// missing → copy readiness to startup.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_ReadinessCopiedToStartupWhenLivenessMissing(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.ReadinessProbe = httpGetLiveness("/actuator/health/readiness")
					c.ReadinessProbe.SuccessThreshold = 3 // force a non-1 value
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	startupPatch := findPatch(result.patches, "/spec/containers/0/startupProbe")
	if startupPatch == nil {
		t.Fatalf("expected startupProbe patch, got patches: %+v", result.patches)
	}
	probe := decodeProbe(t, startupPatch.Value)
	if probe.HTTPGet == nil || probe.HTTPGet.Path != "/actuator/health/readiness" {
		t.Errorf("startup HTTPGet = %+v, want path /actuator/health/readiness", probe.HTTPGet)
	}
	// SuccessThreshold must be forced to 1 for startup probes (acceptance 5).
	if probe.SuccessThreshold != 1 {
		t.Errorf("startup SuccessThreshold = %d, want 1", probe.SuccessThreshold)
	}
}

// ---------------------------------------------------------------------------
// Acceptance criteria 4 and 5: probe handler types preserved; successThreshold
// forced to 1 when readiness is the source.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_HandlerTypesPreserved(t *testing.T) {
	cases := []struct {
		name   string
		source *corev1.Probe
		check  func(t *testing.T, p *corev1.Probe)
	}{
		{
			name:   "httpGet",
			source: httpGetLiveness("/health"),
			check: func(t *testing.T, p *corev1.Probe) {
				if p.HTTPGet == nil || p.HTTPGet.Path != "/health" {
					t.Errorf("expected httpGet at /health, got %+v", p.HTTPGet)
				}
			},
		},
		{
			name:   "tcpSocket",
			source: tcpSocketLiveness(),
			check: func(t *testing.T, p *corev1.Probe) {
				if p.TCPSocket == nil {
					t.Errorf("expected TCPSocket preserved, got %+v", p)
				}
			},
		},
		{
			name:   "exec",
			source: execLiveness(),
			check: func(t *testing.T, p *corev1.Probe) {
				if p.Exec == nil || len(p.Exec.Command) == 0 {
					t.Errorf("expected Exec preserved, got %+v", p)
				}
			},
		},
		{
			name:   "grpc",
			source: grpcLiveness(),
			check: func(t *testing.T, p *corev1.Probe) {
				if p.GRPC == nil || p.GRPC.Service == nil || *p.GRPC.Service != "grpc.health.v1.Health" {
					t.Errorf("expected GRPC preserved, got %+v", p)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						jvmContainer("app", func(c *corev1.Container) {
							c.LivenessProbe = tc.source
						}),
					},
				},
			}
			result, err := buildPodProbePatches(pod, defaultTestConfig())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			patch := findPatch(result.patches, "/spec/containers/0/startupProbe")
			if patch == nil {
				t.Fatalf("no startup patch")
			}
			tc.check(t, decodeProbe(t, patch.Value))
		})
	}
}

func TestBuildPodProbePatches_SuccessThresholdForcedToOne(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.ReadinessProbe = httpGetLiveness("/ready")
					c.ReadinessProbe.SuccessThreshold = 5
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	patch := findPatch(result.patches, "/spec/containers/0/startupProbe")
	if patch == nil {
		t.Fatalf("no startup patch")
	}
	probe := decodeProbe(t, patch.Value)
	if probe.SuccessThreshold != 1 {
		t.Errorf("SuccessThreshold = %d, want 1 (forced)", probe.SuccessThreshold)
	}
	// Readiness itself is not touched (it stays at 5).
	if pod.Spec.Containers[0].ReadinessProbe.SuccessThreshold != 5 {
		t.Errorf("readiness SuccessThreshold mutated: got %d", pod.Spec.Containers[0].ReadinessProbe.SuccessThreshold)
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 6: Multiple containers → correct indices in paths.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_MultipleContainers(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "nginx", Image: "nginx:1.25"}, // index 0, not JVM
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
				}), // index 1, JVM
				{Name: "sidecar", Image: "envoy:v1"}, // index 2, not JVM
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patchExists(result.patches, "/spec/containers/0/startupProbe") {
		t.Errorf("nginx should not be mutated")
	}
	if !patchExists(result.patches, "/spec/containers/1/startupProbe") {
		t.Errorf("app container at index 1 should be mutated")
	}
	if patchExists(result.patches, "/spec/containers/2/startupProbe") {
		t.Errorf("sidecar should not be mutated")
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 7: remove operations do not fail when the target
// field is absent — the algorithm only emits remove when the field is
// present (InitialDelaySeconds != 0).
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_RemoveOnlyWhenFieldPresent(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationJVMProbeClearDelays: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
					// InitialDelaySeconds defaults to 30 in helper.
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !patchExists(result.patches, "/spec/containers/0/livenessProbe/initialDelaySeconds") {
		t.Errorf("expected remove patch for livenessProbe.initialDelaySeconds")
	}

	// And the negative case: zero InitialDelaySeconds → no remove patch.
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationJVMProbeClearDelays: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
					c.LivenessProbe.InitialDelaySeconds = 0
				}),
			},
		},
	}
	result2, err := buildPodProbePatches(pod2, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patchExists(result2.patches, "/spec/containers/0/livenessProbe/initialDelaySeconds") {
		t.Errorf("did not expect remove patch when InitialDelaySeconds is already 0")
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 8: Overwrite annotations force regeneration.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_OverwriteLivenessForcesRegeneration(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationOverwriteLiveness: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/custom")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	patch := findPatch(result.patches, "/spec/containers/0/livenessProbe")
	if patch == nil {
		t.Fatalf("expected liveness probe patch")
	}
	if patch.Op != "replace" {
		t.Errorf("overwrite op = %q, want replace", patch.Op)
	}
	probe := decodeProbe(t, patch.Value)
	// Should NOT be /custom — overwritten by framework probe.
	if probe.HTTPGet != nil && probe.HTTPGet.Path == "/custom" {
		t.Errorf("expected framework path, got /custom (overwrite did not regenerate)")
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 9: clear-delays annotation removes
// liveness/readiness initialDelaySeconds.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_ClearDelaysRemovesInitialDelay(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationJVMProbeClearDelays: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
					c.ReadinessProbe = httpGetLiveness("/ready")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !patchExists(result.patches, "/spec/containers/0/livenessProbe/initialDelaySeconds") {
		t.Errorf("expected remove patch for livenessProbe.initialDelaySeconds")
	}
	if !patchExists(result.patches, "/spec/containers/0/readinessProbe/initialDelaySeconds") {
		t.Errorf("expected remove patch for readinessProbe.initialDelaySeconds")
	}

	// Negative case: missing readiness probe → no remove patch for it.
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationJVMProbeClearDelays: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
				}),
			},
		},
	}
	result2, err := buildPodProbePatches(pod2, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patchExists(result2.patches, "/spec/containers/0/readinessProbe/initialDelaySeconds") {
		t.Errorf("did not expect readiness remove patch when readiness is missing")
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 10: Annotation overrides for startup period/failure.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_AnnotationOverridesStartupTiming(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationJVMProbeStartupPeriod:           "7",
				AnnotationJVMProbeStartupFailureThreshold: "12",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	patch := findPatch(result.patches, "/spec/containers/0/startupProbe")
	if patch == nil {
		t.Fatalf("expected startupProbe patch")
	}
	probe := decodeProbe(t, patch.Value)
	if probe.PeriodSeconds != 7 {
		t.Errorf("PeriodSeconds = %d, want 7", probe.PeriodSeconds)
	}
	if probe.FailureThreshold != 12 {
		t.Errorf("FailureThreshold = %d, want 12", probe.FailureThreshold)
	}
	if probe.SuccessThreshold != 1 {
		t.Errorf("SuccessThreshold = %d, want 1", probe.SuccessThreshold)
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 11: Idempotency. Second invocation on a Pod that
// already has a startup probe copied from liveness returns an empty patch.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_IdempotentOnMutatedPod(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
				}),
			},
		},
	}
	first, err := buildPodProbePatches(pod, idempotentTestConfig())
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !first.mutationApplied {
		t.Fatalf("expected first call to mutate")
	}
	// Simulate applying the patch: install startupProbe on the container,
	// then re-run the algorithm.
	pod.Spec.Containers[0].StartupProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt(8080)},
		},
		PeriodSeconds:    defaultStartupPeriodSeconds,
		FailureThreshold: defaultStartupFailureThreshold,
		SuccessThreshold: 1,
	}
	second, err := buildPodProbePatches(pod, idempotentTestConfig())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second.mutationApplied {
		t.Errorf("second call should be a no-op; got %d patches", len(second.patches))
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion: JVM Pod with no probes → framework-based startup
// probe injection only. No liveness or readiness is present; the algorithm
// must still produce a startup probe built from BuildProbesForFramework.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_FrameworkStartupWhenNoProbes(t *testing.T) {
	// With InjectLiveness=false and InjectReadiness=false, neither
	// liveness nor readiness is framework-built. The algorithm falls
	// through to shouldInjectStartup → buildStartupFromFramework, which
	// returns the framework's dedicated startup probe path.
	cfg := DefaultJVMConfig()
	cfg.InjectLivenessProbe = false
	cfg.InjectReadinessProbe = false
	cfg.InjectStartupProbe = true
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", nil), // no probes
			},
		},
	}
	result, err := buildPodProbePatches(pod, &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Fatalf("expected framework-based startup probe to be injected")
	}
	patch := findPatch(result.patches, "/spec/containers/0/startupProbe")
	if patch == nil {
		t.Fatalf("expected startupProbe patch, got: %+v", result.patches)
	}
	probe := decodeProbe(t, patch.Value)
	if probe.HTTPGet == nil {
		t.Fatalf("expected framework-built httpGet startup probe, got %+v", probe)
	}
	if probe.HTTPGet.Path != "/actuator/health" {
		t.Errorf("startup HTTPGet.Path = %q, want /actuator/health", probe.HTTPGet.Path)
	}
	if probe.PeriodSeconds != defaultStartupPeriodSeconds {
		t.Errorf("PeriodSeconds = %d, want %d", probe.PeriodSeconds, defaultStartupPeriodSeconds)
	}
	if probe.FailureThreshold != defaultStartupFailureThreshold {
		t.Errorf("FailureThreshold = %d, want %d", probe.FailureThreshold, defaultStartupFailureThreshold)
	}
	if probe.SuccessThreshold != 1 {
		t.Errorf("SuccessThreshold = %d, want 1", probe.SuccessThreshold)
	}
	// No liveness or readiness patches expected when source probes are absent
	// and config defaults disable liveness injection.
	if patchExists(result.patches, "/spec/containers/0/livenessProbe") {
		t.Errorf("did not expect livenessProbe patch (liveness disabled in config)")
	}
	if patchExists(result.patches, "/spec/containers/0/readinessProbe") {
		t.Errorf("did not expect readinessProbe patch (readiness disabled in config)")
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion: overwrite-readiness annotation forces regeneration.
// Mirrors TestBuildPodProbePatches_OverwriteLivenessForcesRegeneration.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_OverwriteReadinessForcesRegeneration(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationOverwriteReadiness: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.ReadinessProbe = httpGetLiveness("/custom-ready")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	patch := findPatch(result.patches, "/spec/containers/0/readinessProbe")
	if patch == nil {
		t.Fatalf("expected readiness probe patch")
	}
	if patch.Op != "replace" {
		t.Errorf("overwrite op = %q, want replace", patch.Op)
	}
	probe := decodeProbe(t, patch.Value)
	if probe.HTTPGet != nil && probe.HTTPGet.Path == "/custom-ready" {
		t.Errorf("expected framework path, got /custom-ready (overwrite did not regenerate)")
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion: overwrite-startup annotation forces regeneration of
// an existing startup probe.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_OverwriteStartupForcesRegeneration(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationOverwriteStartup: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
					c.StartupProbe = httpGetLiveness("/old-startup")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	patch := findPatch(result.patches, "/spec/containers/0/startupProbe")
	if patch == nil {
		t.Fatalf("expected startup probe patch")
	}
	if patch.Op != "replace" {
		t.Errorf("op = %q, want replace (probe existed)", patch.Op)
	}
	probe := decodeProbe(t, patch.Value)
	if probe.HTTPGet != nil && probe.HTTPGet.Path == "/old-startup" {
		t.Errorf("expected new path, got /old-startup (overwrite did not regenerate)")
	}
	if probe.SuccessThreshold != 1 {
		t.Errorf("SuccessThreshold = %d, want 1", probe.SuccessThreshold)
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion: overwrite-all forces regeneration of all three probes.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_OverwriteAllForcesRegeneration(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationOverwriteAll: "true",
				// Inject all three probes so regeneration has something to emit.
				AnnotationJVMInjectLiveness:  "true",
				AnnotationJVMInjectReadiness: "true",
				AnnotationJVMInjectStartup:   "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/old-live")
					c.ReadinessProbe = httpGetLiveness("/old-ready")
					c.StartupProbe = httpGetLiveness("/old-start")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, defaultTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, path := range []string{
		"/spec/containers/0/livenessProbe",
		"/spec/containers/0/readinessProbe",
		"/spec/containers/0/startupProbe",
	} {
		patch := findPatch(result.patches, path)
		if patch == nil {
			t.Errorf("missing patch at %s", path)
			continue
		}
		if patch.Op != "replace" {
			t.Errorf("patch %s: op = %q, want replace", path, patch.Op)
		}
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 13 (covers #2,#3 in the spec): JSON Patch type is
// JSONPatch and round-trips correctly through the AdmissionReview handler.
// ---------------------------------------------------------------------------

func TestMutatePod_RoundTrip(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
				}),
			},
		},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}

	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID: "test-uid",
			Object: runtime.RawExtension{
				Raw: raw,
			},
		},
	}
	resp, err := mutatePod(context.Background(), review, defaultTestConfig())
	if err != nil {
		t.Fatalf("mutatePod: %v", err)
	}
	if !resp.Allowed {
		t.Errorf("Allowed = false, want true")
	}
	if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Errorf("PatchType = %v, want JSONPatch", resp.PatchType)
	}
	if len(resp.Patch) == 0 {
		t.Fatalf("Patch is empty")
	}
	var ops []map[string]interface{}
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatalf("patch is not valid JSON: %v\n%s", err, string(resp.Patch))
	}
	if len(ops) == 0 {
		t.Fatalf("patch has no ops")
	}
}

// ---------------------------------------------------------------------------
// HTTP handler integration: an AdmissionReview submitted via the handler
// returns an Allowed AdmissionResponse with a JSON Patch.
// ---------------------------------------------------------------------------

func TestHandlePodAdmission_HTTPIntegration(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
				}),
			},
		},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID: "http-test",
			Object: runtime.RawExtension{
				Raw: raw,
			},
		},
	}
	body, _ := json.Marshal(review)

	// Ensure the global config used by the handler is set.
	saved := config
	defer func() { config = saved }()
	config = defaultTestConfig()

	req := httptest.NewRequest(http.MethodPost, "/mutate/pods", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handlePodAdmission(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got admissionv1.AdmissionReview
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Response == nil || !got.Response.Allowed {
		t.Fatalf("Allowed = false in response")
	}
	if len(got.Response.Patch) == 0 {
		t.Errorf("expected non-empty patch in response")
	}
}

// ---------------------------------------------------------------------------
// HTTP handler error path: a request without AdmissionReview body still
// returns 200 and Allowed=true (failurePolicy=Ignore).
// ---------------------------------------------------------------------------

func TestHandlePodAdmission_BadBodyIsIgnored(t *testing.T) {
	saved := config
	defer func() { config = saved }()
	config = defaultTestConfig()

	req := httptest.NewRequest(http.MethodPost, "/mutate/pods", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	handlePodAdmission(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// ---------------------------------------------------------------------------
// HTTP handler: dry-run admission requests produce the same patch.
// ---------------------------------------------------------------------------

func TestHandlePodAdmission_DryRunSafe(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
				}),
			},
		},
	}
	raw, _ := json.Marshal(pod)
	review := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:    "dryrun",
			DryRun: pointer(true),
			Object: runtime.RawExtension{Raw: raw},
		},
	}
	resp, err := mutatePod(context.Background(), &review, defaultTestConfig())
	if err != nil {
		t.Fatalf("mutatePod: %v", err)
	}
	if !resp.Allowed {
		t.Errorf("Allowed = false on dry-run")
	}
	if len(resp.Patch) == 0 {
		t.Errorf("dry-run should still emit a patch; API server decides whether to persist")
	}
}

// ---------------------------------------------------------------------------
// Chunk 2: Probe alignment tests
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_AlignRemovesInitialDelay(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.AlignProbes = true
	// InjectStartupProbe is already true in the default config.

	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/actuator/health/liveness")
					c.ReadinessProbe = httpGetLiveness("/actuator/health/readiness")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Fatalf("expected mutationApplied = true")
	}
	if !patchExists(result.patches, "/spec/containers/0/livenessProbe/initialDelaySeconds") {
		t.Errorf("expected remove patch for livenessProbe/initialDelaySeconds, got: %+v", result.patches)
	}
	if !patchExists(result.patches, "/spec/containers/0/readinessProbe/initialDelaySeconds") {
		t.Errorf("expected remove patch for readinessProbe/initialDelaySeconds, got: %+v", result.patches)
	}
	// Sanity-check the remove op shape.
	if p := findPatch(result.patches, "/spec/containers/0/livenessProbe/initialDelaySeconds"); p != nil && p.Op != "remove" {
		t.Errorf("livenessProbe/initialDelaySeconds op = %q, want remove", p.Op)
	}
	if p := findPatch(result.patches, "/spec/containers/0/readinessProbe/initialDelaySeconds"); p != nil && p.Op != "remove" {
		t.Errorf("readinessProbe/initialDelaySeconds op = %q, want remove", p.Op)
	}
}

func TestBuildPodProbePatches_AlignExtendsFailureWindow(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.AlignProbes = true
	cfg.MinProbeWindowSeconds = 60
	cfg.MaxFailureThreshold = 10
	// InjectStartupProbe is already true in the default config.

	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/health",
								Port: intstr.FromInt(8080),
							},
						},
						InitialDelaySeconds: 0,
						PeriodSeconds:       10,
						FailureThreshold:    3,
					}
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Fatalf("expected mutationApplied = true")
	}
	patch := findPatch(result.patches, "/spec/containers/0/livenessProbe/failureThreshold")
	if patch == nil {
		t.Fatalf("expected replace patch for livenessProbe/failureThreshold, got: %+v", result.patches)
	}
	if patch.Op != "replace" {
		t.Errorf("op = %q, want replace", patch.Op)
	}
	// math.Ceil(60/10) = 6, capped at MaxFailureThreshold=10.
	got, ok := patch.Value.(int32)
	if !ok {
		t.Fatalf("failureThreshold value is not int32: %T (%v)", patch.Value, patch.Value)
	}
	if got != 6 {
		t.Errorf("failureThreshold = %v, want 6", got)
	}
}

func TestBuildPodProbePatches_AlignStripsFrameworkDelay(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.AlignProbes = true
	cfg.InjectLivenessProbe = true
	cfg.InjectReadinessProbe = true
	cfg.InjectStartupProbe = true

	// Pod has no existing probes; framework defaults will be applied first
	// (initialDelaySeconds=60 for spring-boot) and alignment must then
	// strip that delay from the liveness/readiness probes.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"other": "value"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", nil),
			},
		},
	}
	result, err := buildPodProbePatches(pod, &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Fatalf("expected mutationApplied = true")
	}
	for _, path := range []string{
		"/spec/containers/0/livenessProbe",
		"/spec/containers/0/readinessProbe",
		"/spec/containers/0/startupProbe",
	} {
		if !patchExists(result.patches, path) {
			t.Errorf("expected add patch at %s, got: %+v", path, result.patches)
		}
	}

	// The "add" patches carry the framework probes with initialDelaySeconds
	// still set; alignment emits a follow-up "remove" patch that strips
	// the field. Apply the patches to a copy of the Pod to observe the
	// post-alignment state, then assert the framework's 60-second delay
	// is gone from liveness and readiness.
	mutated := applyPatches(t, pod, result.patches)
	container := mutated.Spec.Containers[0]
	if container.LivenessProbe == nil {
		t.Fatalf("livenessProbe missing after applying patches")
	}
	if container.LivenessProbe.InitialDelaySeconds != 0 {
		t.Errorf("liveness InitialDelaySeconds = %d, want 0 (alignment should have stripped framework delay)", container.LivenessProbe.InitialDelaySeconds)
	}
	if container.ReadinessProbe == nil {
		t.Fatalf("readinessProbe missing after applying patches")
	}
	if container.ReadinessProbe.InitialDelaySeconds != 0 {
		t.Errorf("readiness InitialDelaySeconds = %d, want 0 (alignment should have stripped framework delay)", container.ReadinessProbe.InitialDelaySeconds)
	}
}

func TestBuildPodProbePatches_AlignDisabledByDefault(t *testing.T) {
	cfg := DefaultJVMConfig()
	// AlignProbes is false in DefaultJVMConfig.

	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Fatalf("expected mutationApplied = true (startup probe still injected)")
	}
	if patchExists(result.patches, "/spec/containers/0/livenessProbe/initialDelaySeconds") {
		t.Errorf("did not expect alignment remove patch when AlignProbes=false; got: %+v", result.patches)
	}
}

func TestBuildPodProbePatches_AlignAndClearDelaysNoDuplicateRemove(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.AlignProbes = true
	cfg.InjectStartupProbe = true

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationJVMProbeClearDelays: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
					c.ReadinessProbe = httpGetLiveness("/ready")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Fatalf("expected mutationApplied = true")
	}

	// Exactly one remove patch per probe path: clear-delays schedules the
	// remove, and alignment must NOT add a second one for the same path.
	for _, path := range []string{
		"/spec/containers/0/livenessProbe/initialDelaySeconds",
		"/spec/containers/0/readinessProbe/initialDelaySeconds",
	} {
		count := 0
		for _, p := range result.patches {
			if p.Path == path {
				count++
				if p.Op != "remove" {
					t.Errorf("patch %s: op = %q, want remove", path, p.Op)
				}
			}
		}
		if count != 1 {
			t.Errorf("patch count for %s = %d, want 1 (duplicate removes would fail JSON Patch application); patches: %+v", path, count, result.patches)
		}
	}

	// The patch must apply cleanly — a duplicate remove would fail here with
	// "missing value".
	applyPatches(t, pod, result.patches)
}

func TestBuildPodProbePatches_AlignOnlyWithStartup(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.AlignProbes = true
	cfg.InjectStartupProbe = false

	// Existing startup probe means haveStartup=true, so the controller will
	// not inject/regenerate startup and alignment must not run.
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/health")
					c.StartupProbe = httpGetLiveness("/health")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patchExists(result.patches, "/spec/containers/0/livenessProbe/initialDelaySeconds") {
		t.Errorf("did not expect alignment remove patch when no startup probe is injected; got: %+v", result.patches)
	}
	if patchExists(result.patches, "/spec/containers/0/startupProbe") {
		t.Errorf("did not expect startupProbe patch when InjectStartupProbe=false")
	}
}

// ---------------------------------------------------------------------------
// Chunk 6: when ManagementEnabled is false the webhook must produce no
// patches, even for a JVM Pod with probes.
// ---------------------------------------------------------------------------

func TestBuildPodProbePatches_ManagementDisabled(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.ManagementEnabled = false

	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				jvmContainer("app", func(c *corev1.Container) {
					c.LivenessProbe = httpGetLiveness("/actuator/health/liveness")
				}),
			},
		},
	}
	result, err := buildPodProbePatches(pod, &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("mutationApplied = true, want false (ManagementEnabled=false)")
	}
	if len(result.patches) != 0 {
		t.Errorf("patches = %d, want 0 (ManagementEnabled=false); got: %+v", len(result.patches), result.patches)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func decodeProbe(t *testing.T, v interface{}) *corev1.Probe {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal probe value: %v", err)
	}
	p := &corev1.Probe{}
	if err := json.Unmarshal(raw, p); err != nil {
		t.Fatalf("unmarshal probe: %v\n%s", err, string(raw))
	}
	return p
}

func pointer[T any](v T) *T { return &v }
