// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	"github.com/castai/castai-guardrails-controllers/snapshot"
)

const (
	jvmReconcileTestUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	jvmReconcileTestNS  = "castai-agent"
)

type fakeJVMClient struct {
	existing map[string]*workloadsv1.JVMProbeOriginal
}

func (f *fakeJVMClient) Get(_ context.Context, _, name string) (*workloadsv1.JVMProbeOriginal, error) {
	if snap, ok := f.existing[name]; ok {
		return snap, nil
	}
	return nil, apierrors.NewNotFound(
		schema.GroupResource{Group: "workloads.cast.ai", Resource: "jvmsnapshots"},
		name,
	)
}

func newJVMTestDeployment(namespace, name string, uid types.UID, annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			UID:         uid,
			Annotations: annotations,
		},
	}
}

func jvmManagedAnnotation() string {
	return snapshot.ManagedAnnotationName(ControllerName)
}

func TestReconcileManagedAnnotations_JVM_StripsWhenSnapshotMissing(t *testing.T) {
	ann := jvmManagedAnnotation()
	dep := newJVMTestDeployment("default", "web", jvmReconcileTestUID, map[string]string{ann: "true"})
	cs := fake.NewSimpleClientset(dep)
	snap := &fakeJVMClient{existing: map[string]*workloadsv1.JVMProbeOriginal{}}

	if err := reconcileManagedAnnotations(context.Background(), cs, jvmReconcileTestNS, snap); err != nil {
		t.Fatalf("reconcileManagedAnnotations returned error: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if _, ok := got.Annotations[ann]; ok {
		t.Errorf("expected %s annotation to be removed, still present in %v", ann, got.Annotations)
	}
}

func TestReconcileManagedAnnotations_JVM_KeepsWhenSnapshotExists(t *testing.T) {
	ann := jvmManagedAnnotation()
	dep := newJVMTestDeployment("default", "web", jvmReconcileTestUID, map[string]string{ann: "true"})
	cs := fake.NewSimpleClientset(dep)
	crdName := snapshot.CollisionSafeName("Deployment", "default", "web", jvmReconcileTestUID)
	snap := &fakeJVMClient{existing: map[string]*workloadsv1.JVMProbeOriginal{
		crdName: {ObjectMeta: metav1.ObjectMeta{Name: crdName, Namespace: jvmReconcileTestNS}},
	}}

	if err := reconcileManagedAnnotations(context.Background(), cs, jvmReconcileTestNS, snap); err != nil {
		t.Fatalf("reconcileManagedAnnotations returned error: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.Annotations[ann] != "true" {
		t.Errorf("expected annotation to remain \"true\", got %q", got.Annotations[ann])
	}
}

func TestReconcileManagedAnnotations_JVM_StatefulSet(t *testing.T) {
	ann := jvmManagedAnnotation()
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "db",
			Namespace:   "default",
			UID:         jvmReconcileTestUID,
			Annotations: map[string]string{ann: "true"},
		},
	}
	cs := fake.NewSimpleClientset(sts)
	snap := &fakeJVMClient{existing: map[string]*workloadsv1.JVMProbeOriginal{}}

	if err := reconcileManagedAnnotations(context.Background(), cs, jvmReconcileTestNS, snap); err != nil {
		t.Fatalf("reconcileManagedAnnotations returned error: %v", err)
	}
	got, err := cs.AppsV1().StatefulSets("default").Get(context.Background(), "db", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}
	if _, ok := got.Annotations[ann]; ok {
		t.Errorf("expected %s annotation to be removed from StatefulSet", ann)
	}
}
