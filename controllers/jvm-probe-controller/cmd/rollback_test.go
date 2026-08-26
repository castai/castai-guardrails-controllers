// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	"github.com/castai/castai-guardrails-controllers/snapshot"
)

const jvmTestUID = "11111111-2222-3333-4444-555555555555"

func newJVMSnap(probes map[string]workloadsv1.ContainerProbes) *workloadsv1.JVMProbeOriginal {
	return &workloadsv1.JVMProbeOriginal{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "castai-agent"},
		Spec: workloadsv1.JVMProbeOriginalSpec{
			TargetRef: workloadsv1.TargetRef{
				APIVersion: "apps/v1", Kind: "Deployment",
				Namespace: "default", Name: "web",
				UID: types.UID(jvmTestUID),
			},
			OriginalContainers: probes,
		},
	}
}

func TestBuildProbeOp_RemoveWhenAbsent(t *testing.T) {
	live := &corev1.Probe{InitialDelaySeconds: 5}
	ops := buildProbeOp("/containers/0/livenessProbe", live, nil, false)
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(ops))
	}
	if ops[0]["op"] != "remove" {
		t.Errorf("op = %v, want remove", ops[0]["op"])
	}
	if ops[0]["path"] != "/containers/0/livenessProbe" {
		t.Errorf("path = %v, want /containers/0/livenessProbe", ops[0]["path"])
	}
	if _, ok := ops[0]["value"]; ok {
		t.Errorf("value should not be present on remove op")
	}
}

func TestBuildProbeOp_NoOpWhenLiveAlsoAbsent(t *testing.T) {
	// Snapshot says probe absent AND live workload doesn't have one either.
	ops := buildProbeOp("/containers/0/livenessProbe", nil, nil, false)
	if len(ops) != 0 {
		t.Errorf("expected 0 ops (nothing to remove), got %d", len(ops))
	}
}

func TestBuildProbeOp_ReplaceWhenPresent(t *testing.T) {
	p := &corev1.Probe{InitialDelaySeconds: 30}
	live := &corev1.Probe{InitialDelaySeconds: 5}
	ops := buildProbeOp("/containers/0/livenessProbe", live, p, true)
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(ops))
	}
	if ops[0]["op"] != "replace" {
		t.Errorf("op = %v, want replace", ops[0]["op"])
	}
	v, ok := ops[0]["value"].(*corev1.Probe)
	if !ok {
		t.Fatalf("value is not *corev1.Probe: %T", ops[0]["value"])
	}
	if v.InitialDelaySeconds != 30 {
		t.Errorf("value.InitialDelaySeconds = %d, want 30", v.InitialDelaySeconds)
	}
}

func TestApplyInverseJVMPatch_UIDMismatch(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID("different-uid")},
	}
	cs := fake.NewSimpleClientset(dep)
	snap := newJVMSnap(map[string]workloadsv1.ContainerProbes{
		"app": {LivenessPresent: true, LivenessProbe: &corev1.Probe{}},
	})

	err := applyInverseJVMPatch(context.Background(), cs, snap)
	if !errors.Is(err, snapshot.ErrTargetGone) {
		t.Errorf("expected ErrTargetGone, got %v", err)
	}
}

func TestApplyInverseJVMPatch_TargetGone(t *testing.T) {
	cs := fake.NewSimpleClientset()
	snap := newJVMSnap(map[string]workloadsv1.ContainerProbes{})
	snap.Spec.TargetRef.Name = "missing"

	err := applyInverseJVMPatch(context.Background(), cs, snap)
	if !errors.Is(err, snapshot.ErrTargetGone) {
		t.Errorf("expected ErrTargetGone, got %v", err)
	}
}

func TestApplyInverseJVMPatch_RemovesAddedProbe(t *testing.T) {
	// Live workload has a liveness probe (castai added it). Snapshot says
	// LivenessPresent=false → rollback should remove.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID(jvmTestUID)},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", LivenessProbe: &corev1.Probe{InitialDelaySeconds: 5}},
					},
				},
			},
		},
	}
	cs := fake.NewSimpleClientset(dep)
	// Only liveness was added by castai; readiness/startup were absent before and after.
	snap := newJVMSnap(map[string]workloadsv1.ContainerProbes{
		"app": {
			LivenessPresent:  false, // castai added it
			ReadinessPresent: false, // absent at capture (zero-value), but live doesn't have one either — skip remove
			StartupPresent:   false,
		},
	})

	if err := applyInverseJVMPatch(context.Background(), cs, snap); err != nil {
		t.Fatalf("applyInverseJVMPatch: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Template.Spec.Containers[0].LivenessProbe != nil {
		t.Errorf("expected liveness probe removed, still present: %+v", got.Spec.Template.Spec.Containers[0].LivenessProbe)
	}
}

func TestApplyInverseJVMPatch_RestoresReplacedProbe(t *testing.T) {
	// Live workload has castai's liveness probe. Snapshot says
	// LivenessPresent=true with original value → rollback should replace.
	original := &corev1.Probe{InitialDelaySeconds: 99, PeriodSeconds: 7}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID(jvmTestUID)},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", LivenessProbe: &corev1.Probe{InitialDelaySeconds: 5}},
					},
				},
			},
		},
	}
	cs := fake.NewSimpleClientset(dep)
	snap := newJVMSnap(map[string]workloadsv1.ContainerProbes{
		"app": {
			LivenessPresent:  true,
			LivenessProbe:    original,
			ReadinessPresent: false,
			StartupPresent:   false,
		},
	})

	if err := applyInverseJVMPatch(context.Background(), cs, snap); err != nil {
		t.Fatalf("applyInverseJVMPatch: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	lp := got.Spec.Template.Spec.Containers[0].LivenessProbe
	if lp == nil {
		t.Fatalf("liveness probe missing after restore")
	}
	if lp.InitialDelaySeconds != 99 {
		t.Errorf("InitialDelaySeconds = %d, want 99", lp.InitialDelaySeconds)
	}
	if lp.PeriodSeconds != 7 {
		t.Errorf("PeriodSeconds = %d, want 7", lp.PeriodSeconds)
	}
}

func TestApplyInverseJVMPatch_UsesLiveContainerIndex(t *testing.T) {
	// Containers in live workload are in order [sidecar, app] (index 0/1).
	// Snapshot should map by name (not index) so the patch targets live[1].
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID(jvmTestUID)},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "sidecar", LivenessProbe: &corev1.Probe{InitialDelaySeconds: 5}},
						{Name: "app", LivenessProbe: &corev1.Probe{InitialDelaySeconds: 5}},
					},
				},
			},
		},
	}
	cs := fake.NewSimpleClientset(dep)
	snap := newJVMSnap(map[string]workloadsv1.ContainerProbes{
		"app": {
			LivenessPresent:  false, // castai added — remove
			ReadinessPresent: false,
			StartupPresent:   false,
		},
	})

	if err := applyInverseJVMPatch(context.Background(), cs, snap); err != nil {
		t.Fatalf("applyInverseJVMPatch: %v", err)
	}
	got, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	cs_ := got.Spec.Template.Spec.Containers
	if cs_[0].LivenessProbe == nil {
		t.Errorf("sidecar liveness probe unexpectedly removed")
	}
	if cs_[1].LivenessProbe != nil {
		t.Errorf("app liveness probe should have been removed")
	}
}

func TestApplyInverseJVMPatch_UnsupportedKind(t *testing.T) {
	cs := fake.NewSimpleClientset()
	snap := newJVMSnap(map[string]workloadsv1.ContainerProbes{})
	snap.Spec.TargetRef.Kind = "DaemonSet"
	err := applyInverseJVMPatch(context.Background(), cs, snap)
	if err == nil || !strings.Contains(err.Error(), "unsupported target kind") {
		t.Errorf("expected unsupported-kind error, got %v", err)
	}
}

func TestJVMInverseFn_NotFound(t *testing.T) {
	cs := fake.NewSimpleClientset()
	fn := jvmInverseFn(cs)
	_, _, err := fn(context.Background(), newJVMSnap(map[string]workloadsv1.ContainerProbes{}))
	if !errors.Is(err, snapshot.ErrTargetGone) {
		t.Errorf("expected ErrTargetGone, got %v", err)
	}
}

func TestJVMInverseFn_UIDMatch(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID(jvmTestUID)},
	}
	cs := fake.NewSimpleClientset(dep)
	fn := jvmInverseFn(cs)
	uid, ok, err := fn(context.Background(), newJVMSnap(map[string]workloadsv1.ContainerProbes{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Errorf("expected found=true")
	}
	if uid != jvmTestUID {
		t.Errorf("uid = %q, want %q", uid, jvmTestUID)
	}
}

func TestJVMInverseFn_UIDMismatch(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID("other-uid")},
	}
	cs := fake.NewSimpleClientset(dep)
	fn := jvmInverseFn(cs)
	_, _, err := fn(context.Background(), newJVMSnap(map[string]workloadsv1.ContainerProbes{}))
	if !errors.Is(err, snapshot.ErrTargetGone) {
		t.Errorf("expected ErrTargetGone, got %v", err)
	}
}
