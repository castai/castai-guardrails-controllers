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
//  0. If the live workload is already annotated as managed by this
//     controller but the snapshot is missing, emit a SnapshotLost warning
//     and return nil. We can never safely recapture post-patch state.
//  1. Compute collision-safe CRD name from workload identity.
//  2. Get() the CRD by name.
//     - Not found → build new spec via newFn, Create() with finalizer,
//     set status Ready=True in a follow-up UpdateStatus() call.
//     - Found with Ready=True and NOT RolledBack=True → return nil (skip).
//     - Found with RolledBack=True → treat as absent: Delete existing and
//     create fresh.
//     - Found with Ready=False (partial write from a previous crash) →
//     overwrite with fresh capture.
//  3. 409 AlreadyExists from Create is treated as success (another replica
//     raced us to it).
//  4. Any other error (including UpdateStatus) propagates; the caller MUST
//     NOT proceed with the workload patch if capture failed.
func CaptureIfAbsent[T any](
	ctx context.Context,
	c Client[T],
	acc Accessor[T],
	logger Logger,
	namespace string,
	finalizer string,
	controllerName string,
	identity WorkloadIdentity,
	newFn NewSnapshotFn[T],
) error {
	name := CollisionSafeName(identity.Kind, identity.Namespace, identity.Name, identity.UID)
	logger.Infof("capture-if-absent: %s/%s", namespace, name)

	existing, err := c.Get(ctx, namespace, name)
	hasExisting := false
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get existing snapshot: %w", err)
	}
	if err == nil {
		hasExisting = true
	}

	// Lost-snapshot guard: if the workload is already annotated as managed
	// but the snapshot is missing, the snapshot was lost between Create and
	// the workload patch. We can never safely recapture post-patch state.
	if !hasExisting && IsManaged(identity.Annotations, controllerName) {
		logger.Warnf("capture-if-absent: SnapshotLost guard triggered for workload %s/%s — managed annotation present but snapshot %s/%s is missing; skipping capture",
			identity.Namespace, identity.Name, namespace, name)
		logger.Warnf("capture-if-absent: SnapshotLost — workload %s/%s is annotated as managed but snapshot %s/%s is missing; skipping capture to avoid corrupting rollback data",
			identity.Namespace, identity.Name, namespace, name)
		return nil
	}

	if hasExisting {
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
		if apierrors.IsAlreadyExists(err) {
			logger.Infof("capture-if-absent: snapshot already exists (raced by another replica)")
			return nil
		}
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
		Message:            fmt.Sprintf("Original state captured for %s %s/%s", identity.Kind, identity.Namespace, identity.Name),
		ObservedGeneration: identity.Generation,
	})
	acc.SetConditions(&created, conds)
	if acc.SetObservedGeneration != nil {
		acc.SetObservedGeneration(&created, identity.Generation)
	}

	if _, err := c.UpdateStatus(ctx, namespace, created); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	logger.Infof("snapshot created: %s/%s", namespace, acc.NameOf(created))
	return nil
}
