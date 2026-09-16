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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// defaultTestConfig returns a TSCConfig that matches the chart defaults:
// management enabled, apply mode, skipSingleReplica true, and the two
// zone + hostname defaults.
func defaultTestConfig() *TSCConfig {
	return &TSCConfig{
		ManagementEnabled: true,
		Mode:              ModeApply,
		SnapshotEnabled:   false,
		SkipSingleReplica: true,
		OperatorNamespace: "castai-agent",
		DefaultConstraints: []corev1.TopologySpreadConstraint{
			{MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: corev1.ScheduleAnyway},
			{MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.ScheduleAnyway},
		},
	}
}

func int32Ptr(v int32) *int32 { return &v }

func newDeployment(name, ns string, replicas int32, annotations, labels map[string]string, podOwnerRef string) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			UID:         types.UID("dep-" + podOwnerRef),
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(replicas),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
			},
		},
	}
	return d
}

func newStatefulSet(name, ns string, replicas int32, annotations, labels map[string]string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			UID:         types.UID("sts-" + name),
			Annotations: annotations,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(replicas),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
			},
		},
	}
}

func newReplicaSet(name, ns string, ownerDeployment string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       types.UID("rs-" + name),
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Deployment",
				Name: ownerDeployment,
				UID:  types.UID("dep-" + ownerDeployment),
			}},
		},
	}
}

func podOwnedByRS(name, ns, rsName string, tscs []corev1.TopologySpreadConstraint, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "ReplicaSet",
				Name: rsName,
				UID:  types.UID("rs-" + rsName),
			}},
		},
		Spec: corev1.PodSpec{
			TopologySpreadConstraints: tscs,
		},
	}
}

func podOwnedBySts(name, ns, stsName string, tscs []corev1.TopologySpreadConstraint, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "StatefulSet",
				Name: stsName,
				UID:  types.UID("sts-" + stsName),
			}},
		},
		Spec: corev1.PodSpec{
			TopologySpreadConstraints: tscs,
		},
	}
}

func findPatch(patches []jsonPatchOp, path string) *jsonPatchOp {
	for i := range patches {
		if patches[i].Path == path {
			return &patches[i]
		}
	}
	return nil
}

func TestBuildPodTSCPatches_NoOwnerNoPatch(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
	}
	cs := fake.NewSimpleClientset()
	result, err := buildPodTSCPatches(pod, defaultTestConfig(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("mutationApplied = true, want false")
	}
}

func TestBuildPodTSCPatches_DeploymentPatchMatchesGoalJSON(t *testing.T) {
	dep := newDeployment("web", "default", 3, nil, map[string]string{"app": "web"}, "web")
	rs := newReplicaSet("web-xyz", "default", "web")
	cs := fake.NewSimpleClientset(dep, rs)
	pod := podOwnedByRS("web-abc", "default", "web-xyz", nil, nil)
	result, err := buildPodTSCPatches(pod, defaultTestConfig(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Fatalf("expected mutation applied")
	}
	patch := findPatch(result.patches, "/spec/topologySpreadConstraints")
	if patch == nil {
		t.Fatalf("missing patch")
	}
	if patch.Op != "add" {
		t.Errorf("op = %q, want add", patch.Op)
	}
	// Marshal and verify exact shape.
	body, err := json.Marshal(patch.Value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(body)
	want := []string{
		`"topologyKey":"topology.kubernetes.io/zone"`,
		`"topologyKey":"kubernetes.io/hostname"`,
		`"maxSkew":1`,
		`"matchLabelKeys":["pod-template-hash"]`,
		`"nodeAffinityPolicy":"Honor"`,
		`"nodeTaintsPolicy":"Honor"`,
		`"whenUnsatisfiable":"ScheduleAnyway"`,
		`"labelSelector":{}`,
	}
	for _, w := range want {
		if !strings.Contains(s, w) {
			t.Errorf("patch missing %q\nfull body:\n%s", w, s)
		}
	}
}

func TestBuildPodTSCPatches_StatefulSet(t *testing.T) {
	cs := fake.NewSimpleClientset(newStatefulSet("db", "default", 3, nil, map[string]string{"app": "db"}))
	pod := podOwnedBySts("db-0", "default", "db", nil, nil)
	result, err := buildPodTSCPatches(pod, defaultTestConfig(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Errorf("expected mutation applied for StatefulSet-owned Pod")
	}
}

func TestBuildPodTSCPatches_AlreadyHasTSCsNoPatch(t *testing.T) {
	cs := fake.NewSimpleClientset(newDeployment("web", "default", 3, nil, nil, "web"), newReplicaSet("web-xyz", "default", "web"))
	pod := podOwnedByRS("web-abc", "default", "web-xyz", []corev1.TopologySpreadConstraint{
		{MaxSkew: 1, TopologyKey: "x", WhenUnsatisfiable: corev1.DoNotSchedule},
	}, nil)
	result, err := buildPodTSCPatches(pod, defaultTestConfig(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("expected no mutation when Pod already has TSCs")
	}
}

func TestBuildPodTSCPatches_BypassAnnotation(t *testing.T) {
	cs := fake.NewSimpleClientset(newDeployment("web", "default", 3, nil, nil, "web"), newReplicaSet("web-xyz", "default", "web"))
	pod := podOwnedByRS("web-abc", "default", "web-xyz", nil, map[string]string{AnnotationBypass: "true"})
	result, err := buildPodTSCPatches(pod, defaultTestConfig(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("expected bypass annotation to suppress mutation")
	}
}

func TestBuildPodTSCPatches_ExcludedNamespace(t *testing.T) {
	setExclusionRules(t, []ExclusionRule{{NamespaceRegex: "^kube-"}})
	cs := fake.NewSimpleClientset(newDeployment("web", "kube-system", 3, nil, nil, "web"), newReplicaSet("web-xyz", "kube-system", "web"))
	pod := podOwnedByRS("web-abc", "kube-system", "web-xyz", nil, nil)
	result, err := buildPodTSCPatches(pod, defaultTestConfig(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("expected excluded namespace to suppress mutation")
	}
}

func TestBuildPodTSCPatches_ExcludedNameRegex(t *testing.T) {
	setExclusionRules(t, []ExclusionRule{{NameRegex: "^skip-.*"}})
	cs := fake.NewSimpleClientset(newDeployment("skip-this", "default", 3, nil, nil, "skip-this"), newReplicaSet("skip-this-xyz", "default", "skip-this"))
	pod := podOwnedByRS("skip-this-abc", "default", "skip-this-xyz", nil, nil)
	result, err := buildPodTSCPatches(pod, defaultTestConfig(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("expected name regex exclusion to suppress mutation")
	}

	// Sanity check: a workload whose name does NOT match the regex must
	// still receive a patch, otherwise the rule is over-broad.
	cs2 := fake.NewSimpleClientset(newDeployment("keep-this", "default", 3, nil, nil, "keep-this"), newReplicaSet("keep-this-xyz", "default", "keep-this"))
	pod2 := podOwnedByRS("keep-this-abc", "default", "keep-this-xyz", nil, nil)
	result2, err := buildPodTSCPatches(pod2, defaultTestConfig(), cs2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result2.mutationApplied {
		t.Errorf("expected non-matching name to receive mutation")
	}
}

func TestBuildPodTSCPatches_SingleReplicaSkipped(t *testing.T) {
	cs := fake.NewSimpleClientset(newDeployment("web", "default", 1, nil, nil, "web"), newReplicaSet("web-xyz", "default", "web"))
	pod := podOwnedByRS("web-abc", "default", "web-xyz", nil, nil)
	cfg := defaultTestConfig()
	cfg.SkipSingleReplica = true
	result, err := buildPodTSCPatches(pod, cfg, cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("expected single-replica skip")
	}
}

func TestBuildPodTSCPatches_SingleReplicaNotSkippedWhenFalse(t *testing.T) {
	cs := fake.NewSimpleClientset(newDeployment("web", "default", 1, nil, nil, "web"), newReplicaSet("web-xyz", "default", "web"))
	pod := podOwnedByRS("web-abc", "default", "web-xyz", nil, nil)
	cfg := defaultTestConfig()
	cfg.SkipSingleReplica = false
	result, err := buildPodTSCPatches(pod, cfg, cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.mutationApplied {
		t.Errorf("expected mutation when skipSingleReplica=false")
	}
}

func TestBuildPodTSCPatches_RecommendMode(t *testing.T) {
	cs := fake.NewSimpleClientset(newDeployment("web", "default", 3, nil, nil, "web"), newReplicaSet("web-xyz", "default", "web"))
	pod := podOwnedByRS("web-abc", "default", "web-xyz", nil, nil)
	cfg := defaultTestConfig()
	cfg.Mode = ModeRecommend
	result, err := buildPodTSCPatches(pod, cfg, cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("expected recommend mode to suppress patch")
	}
}

func TestBuildPodTSCPatches_ManagementDisabled(t *testing.T) {
	cs := fake.NewSimpleClientset(newDeployment("web", "default", 3, nil, nil, "web"), newReplicaSet("web-xyz", "default", "web"))
	pod := podOwnedByRS("web-abc", "default", "web-xyz", nil, nil)
	cfg := defaultTestConfig()
	cfg.ManagementEnabled = false
	result, err := buildPodTSCPatches(pod, cfg, cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("expected disabled management to suppress patch")
	}
}

func TestBuildPodTSCPatches_ConstraintsJSONOverride(t *testing.T) {
	override := `[{"maxSkew":5,"topologyKey":"my-key","whenUnsatisfiable":"DoNotSchedule","labelSelector":{"matchLabels":{"x":"y"}}}]`
	cs := fake.NewSimpleClientset(newDeployment("web", "default", 3, map[string]string{
		AnnotationConstraints: override,
	}, nil, "web"), newReplicaSet("web-xyz", "default", "web"))
	pod := podOwnedByRS("web-abc", "default", "web-xyz", nil, nil)
	result, err := buildPodTSCPatches(pod, defaultTestConfig(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	patch := findPatch(result.patches, "/spec/topologySpreadConstraints")
	if patch == nil {
		t.Fatalf("expected patch")
	}
	body, _ := json.Marshal(patch.Value)
	s := string(body)
	for _, want := range []string{`"maxSkew":5`, `"topologyKey":"my-key"`, `"x":"y"`} {
		if !strings.Contains(s, want) {
			t.Errorf("override patch missing %q in %s", want, s)
		}
	}
}

func TestBuildPodTSCPatches_SimpleOverrides(t *testing.T) {
	cs := fake.NewSimpleClientset(newDeployment("web", "default", 3, map[string]string{
		AnnotationMaxSkew:    "2",
		AnnotationTopologyKey: "kubernetes.io/hostname",
	}, nil, "web"), newReplicaSet("web-xyz", "default", "web"))
	pod := podOwnedByRS("web-abc", "default", "web-xyz", nil, nil)
	result, err := buildPodTSCPatches(pod, defaultTestConfig(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	patch := findPatch(result.patches, "/spec/topologySpreadConstraints")
	if patch == nil {
		t.Fatalf("expected patch")
	}
	body, _ := json.Marshal(patch.Value)
	s := string(body)
	if !strings.Contains(s, `"maxSkew":2`) {
		t.Errorf("expected maxSkew=2 in %s", s)
	}
	if !strings.Contains(s, `"topologyKey":"kubernetes.io/hostname"`) {
		t.Errorf("expected hostname in %s", s)
	}
}

func TestMutatePod_RoundTrip(t *testing.T) {
	cs := fake.NewSimpleClientset(newDeployment("web", "default", 3, nil, nil, "web"), newReplicaSet("web-xyz", "default", "web"))
	pod := podOwnedByRS("web-abc", "default", "web-xyz", nil, nil)
	prev := getClientset()
	setClientset(cs)
	t.Cleanup(func() { setClientset(prev) })

	raw, _ := json.Marshal(pod)
	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:    "u1",
			Object: runtime.RawExtension{Raw: raw},
		},
	}
	resp, err := mutatePod(context.Background(), review, defaultTestConfig())
	if err != nil {
		t.Fatalf("mutatePod: %v", err)
	}
	if !resp.Allowed {
		t.Errorf("Allowed = false")
	}
	if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Errorf("PatchType = %v", resp.PatchType)
	}
	if len(resp.Patch) == 0 {
		t.Errorf("Patch empty")
	}
}

func TestMutatePod_BadBody(t *testing.T) {
	resp, err := mutatePod(context.Background(), &admissionv1.AdmissionReview{}, defaultTestConfig())
	if err == nil {
		t.Errorf("expected error for nil request")
	}
	if resp != nil {
		t.Errorf("expected nil response on error")
	}
}

func TestHandlePodAdmission_HTTP(t *testing.T) {
	cs := fake.NewSimpleClientset(newDeployment("web", "default", 3, nil, nil, "web"), newReplicaSet("web-xyz", "default", "web"))
	prev := getClientset()
	setClientset(cs)
	t.Cleanup(func() { setClientset(prev) })

	saved := config
	defer func() { config = saved }()
	config = defaultTestConfig()

	pod := podOwnedByRS("web-abc", "default", "web-xyz", nil, nil)
	raw, _ := json.Marshal(pod)
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{UID: "h", Object: runtime.RawExtension{Raw: raw}},
	}
	body, _ := json.Marshal(review)
	req := httptest.NewRequest(http.MethodPost, "/mutate/pods", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handlePodAdmission(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}
	var got admissionv1.AdmissionReview
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Response == nil || !got.Response.Allowed {
		t.Errorf("response missing or not allowed")
	}
	if len(got.Response.Patch) == 0 {
		t.Errorf("expected non-empty patch")
	}
}

func TestHandlePodAdmission_BadJSON(t *testing.T) {
	saved := config
	defer func() { config = saved }()
	config = defaultTestConfig()
	req := httptest.NewRequest(http.MethodPost, "/mutate/pods", strings.NewReader("not-json"))
	w := httptest.NewRecorder()
	handlePodAdmission(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (failurePolicy=Ignore)", w.Code)
	}
}

func TestHandlePodAdmission_GetMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/mutate/pods", nil)
	w := httptest.NewRecorder()
	handlePodAdmission(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestBuildPodTSCPatches_IdempotentOnMutatedPod(t *testing.T) {
	cs := fake.NewSimpleClientset(newDeployment("web", "default", 3, nil, nil, "web"), newReplicaSet("web-xyz", "default", "web"))
	pod := podOwnedByRS("web-abc", "default", "web-xyz", []corev1.TopologySpreadConstraint{
		{MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: corev1.ScheduleAnyway},
	}, nil)
	result, err := buildPodTSCPatches(pod, defaultTestConfig(), cs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.mutationApplied {
		t.Errorf("second admission on already-mutated Pod should be no-op")
	}
}
