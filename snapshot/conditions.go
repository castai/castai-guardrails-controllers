// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package snapshot

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types reported on snapshot CRDs.
const (
	ConditionReady      = "Ready"
	ConditionRolledBack = "RolledBack"
)

// Condition reasons for Ready.
const (
	ReasonCaptured    = "Captured"
	ReasonCaptureFail = "CaptureFailed"
)

// Condition reasons for RolledBack.
const (
	ReasonRollbackApplied = "RollbackApplied"
	ReasonTargetGone      = "TargetGone"
	ReasonRollbackFailed  = "RollbackFailed"
)

// SetCondition adds or updates a condition of the given type on conditions.
func SetCondition(conditions []metav1.Condition, c metav1.Condition) []metav1.Condition {
	c.LastTransitionTime = metav1.NewTime(time.Now().UTC())
	for i, existing := range conditions {
		if existing.Type == c.Type {
			if existing.Status != c.Status {
				conditions[i] = c
			} else {
				existing.Reason = c.Reason
				existing.Message = c.Message
				existing.ObservedGeneration = c.ObservedGeneration
				conditions[i] = existing
			}
			return conditions
		}
	}
	return append(conditions, c)
}

// IsReady reports whether the Ready condition is True.
func IsReady(conditions []metav1.Condition) bool {
	return getConditionStatus(conditions, ConditionReady) == metav1.ConditionTrue
}

// IsRolledBack reports whether the RolledBack condition is True.
func IsRolledBack(conditions []metav1.Condition) bool {
	return getConditionStatus(conditions, ConditionRolledBack) == metav1.ConditionTrue
}

func getConditionStatus(conditions []metav1.Condition, t string) metav1.ConditionStatus {
	for _, c := range conditions {
		if c.Type == t {
			return c.Status
		}
	}
	return metav1.ConditionUnknown
}
