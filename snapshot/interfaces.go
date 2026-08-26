// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

// Package snapshot provides the shared capture-and-rollback machinery used by
// both tsc-controller and jvm-probe-controller. It abstracts the workload
// type (TSCOriginal vs JVMProbeOriginal) via a small Accessor pattern so each
// controller gets a typed client without duplicating logic.
package snapshot

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
)

// WorkloadIdentity is the minimum a Manager needs to compute a collision-safe
// name and populate the CRD's TargetRef.
type WorkloadIdentity struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	UID        types.UID
	Generation int64
}

// Accessor exposes the field-level operations Manager needs on the generic
// snapshot CRD type T. Each controller provides an Accessor binding T to its
// concrete CRD type.
type Accessor[T any] struct {
	NameOf        func(T) string
	NamespaceOf   func(T) string
	GetTargetRef  func(T) workloadsv1.TargetRef
	GetConditions func(T) []metav1.Condition
	SetConditions func(*T, []metav1.Condition)
	GetFinalizers func(T) []string
	SetFinalizers func(*T, []string)
}

// Client is the narrow subset of typed-client operations the snapshot module
// needs. Implementations wrap generated clientsets (TSCOriginal /
// JVMProbeOriginal). Tests provide fakes.
type Client[T any] interface {
	Get(ctx context.Context, namespace, name string) (T, error)
	Create(ctx context.Context, namespace string, obj T) (T, error)
	Update(ctx context.Context, namespace string, obj T) (T, error)
	UpdateStatus(ctx context.Context, namespace string, obj T) (T, error)
	Delete(ctx context.Context, namespace, name string) error
	List(ctx context.Context, namespace string) ([]T, error)
	Patch(ctx context.Context, namespace, name string, pt PatchType, data []byte) (T, error)
}

// PatchType is the strategy used when patching a snapshot CRD.
type PatchType string

const (
	PatchTypeJSONMerge PatchType = "application/merge-patch+json"
	PatchTypeJSON      PatchType = "application/json-patch+json"
	PatchTypeStrategic PatchType = "application/strategic-merge-patch+json"
)

// Logger is the minimal logging interface the snapshot module uses.
type Logger interface {
	Infof(format string, args ...interface{})
	Errorf(format string, args ...interface{})
	Warnf(format string, args ...interface{})
}

// NopLogger drops every message. Useful in tests.
type NopLogger struct{}

func (NopLogger) Infof(string, ...interface{})  {}
func (NopLogger) Warnf(string, ...interface{})  {}
func (NopLogger) Errorf(string, ...interface{}) {}
