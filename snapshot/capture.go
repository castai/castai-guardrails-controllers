// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package snapshot

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewSnapshotFn is the controller-supplied function that builds a fresh
// snapshot object for the given workload identity.
type NewSnapshotFn[T any] func(WorkloadIdentity) (T, error)

// CaptureIfAbsent is the single choke point for snapshot writes.
//
// Behaviour (plan §4.2):
//   1. Compute collision-safe CRD name from workload identity.
//   2. Get() the CRD by name.
//      - Not found → build new spec via newFn, Create() with finalizer,
//        set status Ready=True in a follow-up UpdateStatus() call.
//      - Found with Ready=True and NOT RolledBack=True → return nil (skip).
//      - Found with RolledBack=True → treat as absent: Delete existing and
//        create fresh.
//      - Found with Ready=False (partial write from a previous crash) →
//        overwrite with fresh capture.
//   3. Any error propagates; the caller MUST NOT proceed with the workload
//      patch if capture failed.
func CaptureIfAbsent[T any](
	ctx context.Context,
	c Client[T],
	acc Accessor[T],
	logger Logger,
	namespace string,
	finalizer string,
	identity WorkloadIdentity,
	newFn NewSnapshotFn[T],
) error {
	name := CollisionSafeName(identity.Kind, identity.Namespace, identity.Name, identity.UID)
	logger.Infof("capture-if-absent: %s/%s", namespace, name)

	existing, err := c.Get(ctx, namespace, name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get existing snapshot: %w", err)
	}

	if err == nil {
		conds := acc.GetConditions(existing)
		if IsReady(conds) && !IsRolledBack(conds) {
			logger.Infof("capture-if-absent: snapshot already Ready, skipping")
			return nil
		}
		if IsRolledBack(conds) {
			logger.Infof("capture-if-absent: snapshot RolledBack, deleting and recapturing")
			if delErr := c.Delete(ctx, namespace, name); delErr != nil && !apierrors.IsNotFound(delErr) {
				return fmt.Errorf("delete rolled-back snapshot: %w", delErr)
			}
		} else {
			logger.Infof("capture-if-absent: snapshot partial (Ready=False), overwriting")
		}
	}

	obj, err := newFn(identity)
	if err != nil {
		return fmt.Errorf("build snapshot: %w", err)
	}

	created, err := c.Create(ctx, namespace, obj)
	if err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}

	if err := AddFinalizer(ctx, c, acc, namespace, acc.NameOf(created), finalizer); err != nil {
		// Best-effort cleanup on failure.
		_ = c.Delete(ctx, namespace, acc.NameOf(created))
		return fmt.Errorf("add finalizer: %w", err)
	}

	conds := acc.GetConditions(created)
	conds = SetCondition(conds, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonCaptured,
		ObservedGeneration: identity.Generation,
	})
	acc.SetConditions(&created, conds)

	if _, err := c.UpdateStatus(ctx, namespace, created); err != nil {
		logger.Warnf("capture-if-absent: update status failed (continuing): %v", err)
	}
	return nil
}
