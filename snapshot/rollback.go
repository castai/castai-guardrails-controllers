// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package snapshot

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrTargetGone is returned by an inverseFn when the target workload is gone
// or its UID no longer matches the snapshot's TargetRef.
var ErrTargetGone = errors.New("target workload gone")

// TargetLookupFn returns the live UID of the workload referenced by the
// snapshot, or ErrTargetGone if the workload no longer exists. Implementations
// call e.g. c.kubeClient.AppsV1().Deployments(ref.Namespace).Get(...).
type TargetLookupFn[T any] func(ctx context.Context, snap T) (liveUID string, found bool, err error)

// Rollback iterates every snapshot in the configured namespace and applies
// the inverse patch to each referenced workload.
//
// Per-CRD flow (plan §4.2):
//  1. Skip if RolledBack=True already (idempotency).
//  2. Look up target workload by TargetRef.
//     - Not found or UID mismatch → mark RolledBack=True with reason
//     TargetGone and remove finalizer.
//  3. Apply inverse patch via inverseFn.
//  4. On success: set RolledBack=True, reason=RollbackApplied, remove finalizer.
//  5. On failure: leave RolledBack=False, log, continue. Aggregated error
//     returned at end.
func Rollback[T any](
	ctx context.Context,
	c Client[T],
	acc Accessor[T],
	logger Logger,
	namespace string,
	finalizer string,
	lookup TargetLookupFn[T],
	inverseFn func(ctx context.Context, snap T) error,
) error {
	items, err := c.List(ctx, namespace)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	logger.Infof("rollback: starting in namespace %s with %d snapshot(s) to consider", namespace, len(items))

	var errs []error
	rolledBack := 0
	skipped := 0
	for _, snap := range items {
		name := acc.NameOf(snap)
		conds := acc.GetConditions(snap)
		if IsRolledBack(conds) {
			logger.Infof("rollback: skip %s (already RolledBack)", name)
			skipped++
			continue
		}
		liveUID, found, lookupErr := lookup(ctx, snap)
		if lookupErr != nil && !errors.Is(lookupErr, ErrTargetGone) {
			errs = append(errs, fmt.Errorf("lookup %s: %w", name, lookupErr))
			logger.Errorf("rollback: lookup %s: %v", name, lookupErr)
			continue
		}
		if !found || errors.Is(lookupErr, ErrTargetGone) || liveUID != string(acc.GetTargetRef(snap).UID) {
			logger.Infof("rollback: target gone for %s, marking RolledBack", name)
			conds = SetCondition(conds, metav1.Condition{
				Type:    ConditionRolledBack,
				Status:  metav1.ConditionTrue,
				Reason:  ReasonTargetGone,
				Message: fmt.Sprintf("Target workload %s/%s is gone or UID mismatch; no rollback needed", acc.GetTargetRef(snap).Namespace, acc.GetTargetRef(snap).Name),
			})
			acc.SetConditions(&snap, conds)
			if acc.SetObservedGeneration != nil {
				acc.SetObservedGeneration(&snap, acc.GenerationOf(snap))
			}
			if _, uerr := c.UpdateStatus(ctx, namespace, snap); uerr != nil {
				errs = append(errs, fmt.Errorf("update status %s: %w", name, uerr))
				continue
			}
			if rerr := RemoveFinalizer(ctx, c, acc, namespace, name, finalizer); rerr != nil {
				errs = append(errs, fmt.Errorf("remove finalizer %s: %w", name, rerr))
				continue
			}
			rolledBack++
			continue
		}

		if ierr := inverseFn(ctx, snap); ierr != nil {
			logger.Errorf("rollback: inverse %s failed: %v", name, ierr)
			errs = append(errs, fmt.Errorf("inverse %s: %w", name, ierr))
			conds = SetCondition(conds, metav1.Condition{
				Type:    ConditionRolledBack,
				Status:  metav1.ConditionFalse,
				Reason:  ReasonRollbackFailed,
				Message: fmt.Sprintf("Inverse patch failed for %s/%s: %v", acc.GetTargetRef(snap).Namespace, acc.GetTargetRef(snap).Name, ierr),
			})
			acc.SetConditions(&snap, conds)
			if acc.SetObservedGeneration != nil {
				acc.SetObservedGeneration(&snap, acc.GenerationOf(snap))
			}
			if _, uerr := c.UpdateStatus(ctx, namespace, snap); uerr != nil {
				logger.Warnf("rollback: status update after failure for %s: %v", name, uerr)
			}
			continue
		}

		conds = SetCondition(conds, metav1.Condition{
			Type:    ConditionRolledBack,
			Status:  metav1.ConditionTrue,
			Reason:  ReasonRollbackApplied,
			Message: fmt.Sprintf("Rollback patch applied to %s/%s", acc.GetTargetRef(snap).Namespace, acc.GetTargetRef(snap).Name),
		})
		acc.SetConditions(&snap, conds)
		if acc.SetObservedGeneration != nil {
			acc.SetObservedGeneration(&snap, acc.GenerationOf(snap))
		}
		if _, uerr := c.UpdateStatus(ctx, namespace, snap); uerr != nil {
			errs = append(errs, fmt.Errorf("update status %s: %w", name, uerr))
			continue
		}
		if rerr := RemoveFinalizer(ctx, c, acc, namespace, name, finalizer); rerr != nil {
			errs = append(errs, fmt.Errorf("remove finalizer %s: %w", name, rerr))
			continue
		}
		rolledBack++
	}

	logger.Infof("rollback: complete (rolledBack=%d, skipped=%d, errors=%d)", rolledBack, skipped, len(errs))
	logger.Infof("rollback: completed namespace=%s rolledBack=%d skipped=%d errors=%d", namespace, rolledBack, skipped, len(errs))
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// IsNotFound is a small helper to test for a 404 from the typed client.
// (apierrors.IsNotFound is exported; this just keeps callers terse.)
func IsNotFound(err error) bool { return apierrors.IsNotFound(err) }
