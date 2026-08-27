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
	reconcileTestUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	reconcileTestNS  = "castai-agent"
)

// fakeTSCClient is an in-memory tscSnapshotLookup used by the
// reconcileManagedAnnotations tests.
type fakeTSCClient struct {
	existing map[string]*workloadsv1.TSCOriginal
}

func (f *fakeTSCClient) Get(_ context.Context, _, name string) (*workloadsv1.TSCOriginal, error) {
	if snap, ok := f.existing[name]; ok {
		return snap, nil
	}
	return nil, apierrors.NewNotFound(
		schema.GroupResource{Group: "workloads.cast.ai", Resource: "tscoriginals"},
		name,
	)
}

func newTestDeployment(namespace, name string, uid types.UID, annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			UID:         uid,
			Annotations: annotations,
		},
	}
}

func TestReconcileManagedAnnotations_StripsWhenSnapshotMissing(t *testing.T) {
	dep := newTestDeployment("default", "web", reconcileTestUID, map[string]string{
		AnnotationTSCManaged: "true",
	})
	cs := fake.NewSimpleClientset(dep)
	snap := &fakeTSCClient{existing: map[string]*workloadsv1.TSCOriginal{}}

	if err := reconcileManagedAnnotations(context.Background(), cs, reconcileTestNS, snap); err != nil {
		t.Fatalf("reconcileManagedAnnotations returned error: %v", err)
	}

	got, err := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if _, ok := got.Annotations[AnnotationTSCManaged]; ok {
		t.Errorf("expected %s annotation to be removed, still present in %v", AnnotationTSCManaged, got.Annotations)
	}
}

func TestReconcileManagedAnnotations_KeepsWhenSnapshotExists(t *testing.T) {
	dep := newTestDeployment("default", "web", reconcileTestUID, map[string]string{
		AnnotationTSCManaged: "true",
	})
	cs := fake.NewSimpleClientset(dep)
	crdName := snapshot.CollisionSafeName("Deployment", "default", "web", reconcileTestUID)
	snap := &fakeTSCClient{existing: map[string]*workloadsv1.TSCOriginal{
		crdName: {ObjectMeta: metav1.ObjectMeta{Name: crdName, Namespace: reconcileTestNS}},
	}}

	if err := reconcileManagedAnnotations(context.Background(), cs, reconcileTestNS, snap); err != nil {
		t.Fatalf("reconcileManagedAnnotations returned error: %v", err)
	}

	got, err := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.Annotations[AnnotationTSCManaged] != "true" {
		t.Errorf("expected annotation to remain \"true\", got %q", got.Annotations[AnnotationTSCManaged])
	}
}

func TestReconcileManagedAnnotations_NoopWithoutAnnotation(t *testing.T) {
	// Deployment without the managed annotation — must not be touched even
	// if the snapshot is missing (it's a free workload).
	dep := newTestDeployment("default", "web", reconcileTestUID, nil)
	cs := fake.NewSimpleClientset(dep)
	snap := &fakeTSCClient{existing: map[string]*workloadsv1.TSCOriginal{}}

	if err := reconcileManagedAnnotations(context.Background(), cs, reconcileTestNS, snap); err != nil {
		t.Fatalf("reconcileManagedAnnotations returned error: %v", err)
	}
}

func TestReconcileManagedAnnotations_StatefulSet(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db",
			Namespace: "default",
			UID:       reconcileTestUID,
			Annotations: map[string]string{
				AnnotationTSCManaged: "true",
			},
		},
	}
	cs := fake.NewSimpleClientset(sts)
	snap := &fakeTSCClient{existing: map[string]*workloadsv1.TSCOriginal{}}

	if err := reconcileManagedAnnotations(context.Background(), cs, reconcileTestNS, snap); err != nil {
		t.Fatalf("reconcileManagedAnnotations returned error: %v", err)
	}
	got, err := cs.AppsV1().StatefulSets("default").Get(context.Background(), "db", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}
	if _, ok := got.Annotations[AnnotationTSCManaged]; ok {
		t.Errorf("expected %s annotation to be removed from StatefulSet", AnnotationTSCManaged)
	}
}

func TestReconcileManagedAnnotations_Mixed(t *testing.T) {
	// Three workloads:
	//   - web: managed + no snapshot → stripped
	//   - api: managed + snapshot present → kept
	//   - free: not managed → untouched
	uidWeb := types.UID("uid-web")
	uidAPI := types.UID("uid-api")
	uidFree := types.UID("uid-free")
	cs := fake.NewSimpleClientset(
		newTestDeployment("default", "web", uidWeb, map[string]string{AnnotationTSCManaged: "true"}),
		newTestDeployment("default", "api", uidAPI, map[string]string{AnnotationTSCManaged: "true"}),
		newTestDeployment("default", "free", uidFree, nil),
	)
	apiSnapName := snapshot.CollisionSafeName("Deployment", "default", "api", uidAPI)
	snap := &fakeTSCClient{existing: map[string]*workloadsv1.TSCOriginal{
		apiSnapName: {ObjectMeta: metav1.ObjectMeta{Name: apiSnapName, Namespace: reconcileTestNS}},
	}}

	if err := reconcileManagedAnnotations(context.Background(), cs, reconcileTestNS, snap); err != nil {
		t.Fatalf("reconcileManagedAnnotations returned error: %v", err)
	}

	web, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if _, ok := web.Annotations[AnnotationTSCManaged]; ok {
		t.Errorf("web: expected annotation stripped")
	}
	api, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if api.Annotations[AnnotationTSCManaged] != "true" {
		t.Errorf("api: expected annotation kept, got %v", api.Annotations)
	}
	free, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "free", metav1.GetOptions{})
	if free.Annotations != nil {
		t.Errorf("free: expected no annotations, got %v", free.Annotations)
	}
}
