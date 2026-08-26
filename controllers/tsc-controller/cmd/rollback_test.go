// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	"github.com/castai/castai-guardrails-controllers/snapshot"
)

const testUID = "11111111-2222-3333-4444-555555555555"

func newSnapWithTSCs(present bool, tscs []corev1.TopologySpreadConstraint) *workloadsv1.TSCOriginal {
	return &workloadsv1.TSCOriginal{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "castai-agent"},
		Spec: workloadsv1.TSCOriginalSpec{
			TargetRef: workloadsv1.TargetRef{
				APIVersion: "apps/v1", Kind: "Deployment",
				Namespace: "default", Name: "web",
				UID: types.UID(testUID),
			},
			OriginalTSCs:        tscs,
			OriginalTSCsPresent: present,
		},
	}
}

func TestBuildInversePatchBody_RemovesTSCWhenAbsent(t *testing.T) {
	snap := newSnapWithTSCs(false, nil)
	body := buildInversePatchBody(snap)
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"topologySpreadConstraints":null`) {
		t.Errorf("expected null topologySpreadConstraints in patch, got %s", s)
	}
}

func TestBuildInversePatchBody_RestoresTSCWhenPresent(t *testing.T) {
	tscs := []corev1.TopologySpreadConstraint{{
		MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.DoNotSchedule,
	}}
	snap := newSnapWithTSCs(true, tscs)
	body := buildInversePatchBody(snap)
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"topologySpreadConstraints":[{`) {
		t.Errorf("expected non-empty TSC list, got %s", s)
	}
	if !strings.Contains(s, `"maxSkew":1`) {
		t.Errorf("expected maxSkew in patch, got %s", s)
	}
	if !strings.Contains(s, `"spec":{"template":{"spec":{`) {
		t.Errorf("expected nested spec.template.spec path, got %s", s)
	}
}

func TestApplyInversePatch_UIDMismatch(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID("different-uid")},
	}
	cs := fake.NewSimpleClientset(dep)
	snap := newSnapWithTSCs(true, []corev1.TopologySpreadConstraint{{MaxSkew: 1}})

	err := applyInversePatch(context.Background(), cs, snap)
	if !errors.Is(err, snapshot.ErrTargetGone) {
		t.Errorf("expected ErrTargetGone, got %v", err)
	}
}

func TestApplyInversePatch_TargetGone(t *testing.T) {
	cs := fake.NewSimpleClientset()
	snap := newSnapWithTSCs(false, nil)
	snap.Spec.TargetRef.Name = "missing"

	err := applyInversePatch(context.Background(), cs, snap)
	if !errors.Is(err, snapshot.ErrTargetGone) {
		t.Errorf("expected ErrTargetGone, got %v", err)
	}
}

func TestApplyInversePatch_AppliesRestore(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID(testUID)},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{MaxSkew: 5}},
				},
			},
		},
	}
	cs := fake.NewSimpleClientset(dep)
	snap := newSnapWithTSCs(true, []corev1.TopologySpreadConstraint{{
		MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.DoNotSchedule,
	}})

	if err := applyInversePatch(context.Background(), cs, snap); err != nil {
		t.Fatalf("applyInversePatch: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Spec.Template.Spec.TopologySpreadConstraints) != 1 {
		t.Fatalf("expected 1 TSC, got %d", len(got.Spec.Template.Spec.TopologySpreadConstraints))
	}
	if got.Spec.Template.Spec.TopologySpreadConstraints[0].MaxSkew != 1 {
		t.Errorf("MaxSkew = %d, want 1", got.Spec.Template.Spec.TopologySpreadConstraints[0].MaxSkew)
	}
}

func TestApplyInversePatch_UnsupportedKind(t *testing.T) {
	cs := fake.NewSimpleClientset()
	snap := newSnapWithTSCs(true, nil)
	snap.Spec.TargetRef.Kind = "DaemonSet"
	err := applyInversePatch(context.Background(), cs, snap)
	if err == nil || strings.Contains(err.Error(), "unsupported target kind") == false {
		t.Errorf("expected unsupported-kind error, got %v", err)
	}
}

func TestTSCInverseFn_NotFound(t *testing.T) {
	cs := fake.NewSimpleClientset()
	fn := tscInverseFn(cs)
	_, _, err := fn(context.Background(), newSnapWithTSCs(false, nil))
	if !errors.Is(err, snapshot.ErrTargetGone) {
		t.Errorf("expected ErrTargetGone, got %v", err)
	}
}

func TestTSCInverseFn_UIDMatch(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID(testUID)},
	}
	cs := fake.NewSimpleClientset(dep)
	fn := tscInverseFn(cs)
	uid, ok, err := fn(context.Background(), newSnapWithTSCs(false, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Errorf("expected found=true")
	}
	if uid != testUID {
		t.Errorf("uid = %q, want %q", uid, testUID)
	}
}

func TestTSCInverseFn_UIDMismatch(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID("other-uid")},
	}
	cs := fake.NewSimpleClientset(dep)
	fn := tscInverseFn(cs)
	_, _, err := fn(context.Background(), newSnapWithTSCs(false, nil))
	if !errors.Is(err, snapshot.ErrTargetGone) {
		t.Errorf("expected ErrTargetGone, got %v", err)
	}
}

// silence unused import warnings when build tags vary
var _ = runtime.Object(nil)
