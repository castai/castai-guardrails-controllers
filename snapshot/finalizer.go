// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
)

// FinalizerName returns the finalizer string used by a controller named
// controllerName: "workloads.cast.ai/<controllerName>-finalizer".
func FinalizerName(controllerName string) string {
	return fmt.Sprintf("workloads.cast.ai/%s-finalizer", controllerName)
}

// AddFinalizer patches the snapshot to include finalizerName. Idempotent.
func AddFinalizer[T any](ctx context.Context, c Client[T], acc Accessor[T], namespace, name, finalizerName string) error {
	current, err := c.Get(ctx, namespace, name)
	if err != nil {
		return fmt.Errorf("get for finalizer add: %w", err)
	}
	finalizers := acc.GetFinalizers(current)
	for _, f := range finalizers {
		if f == finalizerName {
			return nil
		}
	}
	finalizers = append(finalizers, finalizerName)
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": finalizers,
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal finalizer patch: %w", err)
	}
	if _, err := c.Patch(ctx, namespace, name, PatchTypeJSONMerge, body); err != nil {
		return fmt.Errorf("patch finalizer: %w", err)
	}
	return nil
}

// RemoveFinalizer patches the snapshot to remove finalizerName. Idempotent.
func RemoveFinalizer[T any](ctx context.Context, c Client[T], acc Accessor[T], namespace, name, finalizerName string) error {
	current, err := c.Get(ctx, namespace, name)
	if err != nil {
		return fmt.Errorf("get for finalizer remove: %w", err)
	}
	finalizers := acc.GetFinalizers(current)
	kept := finalizers[:0]
	found := false
	for _, f := range finalizers {
		if f == finalizerName {
			found = true
			continue
		}
		kept = append(kept, f)
	}
	if !found {
		return nil
	}
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": kept,
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal finalizer patch: %w", err)
	}
	if _, err := c.Patch(ctx, namespace, name, PatchTypeJSONMerge, body); err != nil {
		return fmt.Errorf("patch finalizer remove: %w", err)
	}
	return nil
}
