// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package snapshot

import (
	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TSCOriginalAccessor binds Accessor[*TSCOriginal] for the tsc-controller.
var TSCOriginalAccessor = Accessor[*workloadsv1.TSCOriginal]{
	NameOf: func(o *workloadsv1.TSCOriginal) string {
		return o.Name
	},
	NamespaceOf: func(o *workloadsv1.TSCOriginal) string {
		return o.Namespace
	},
	GetTargetRef: func(o *workloadsv1.TSCOriginal) workloadsv1.TargetRef {
		return o.Spec.TargetRef
	},
	GetConditions: func(o *workloadsv1.TSCOriginal) []metav1.Condition {
		return append([]metav1.Condition(nil), o.Status.Conditions...)
	},
	SetConditions: func(o **workloadsv1.TSCOriginal, cs []metav1.Condition) {
		(*o).Status.Conditions = cs
	},
	GetFinalizers: func(o *workloadsv1.TSCOriginal) []string {
		return append([]string(nil), o.Finalizers...)
	},
	SetFinalizers: func(o **workloadsv1.TSCOriginal, fs []string) {
		(*o).Finalizers = fs
	},
}

// JVMProbeOriginalAccessor binds Accessor[*JVMProbeOriginal].
var JVMProbeOriginalAccessor = Accessor[*workloadsv1.JVMProbeOriginal]{
	NameOf: func(o *workloadsv1.JVMProbeOriginal) string {
		return o.Name
	},
	NamespaceOf: func(o *workloadsv1.JVMProbeOriginal) string {
		return o.Namespace
	},
	GetTargetRef: func(o *workloadsv1.JVMProbeOriginal) workloadsv1.TargetRef {
		return o.Spec.TargetRef
	},
	GetConditions: func(o *workloadsv1.JVMProbeOriginal) []metav1.Condition {
		return append([]metav1.Condition(nil), o.Status.Conditions...)
	},
	SetConditions: func(o **workloadsv1.JVMProbeOriginal, cs []metav1.Condition) {
		(*o).Status.Conditions = cs
	},
	GetFinalizers: func(o *workloadsv1.JVMProbeOriginal) []string {
		return append([]string(nil), o.Finalizers...)
	},
	SetFinalizers: func(o **workloadsv1.JVMProbeOriginal, fs []string) {
		(*o).Finalizers = fs
	},
}
