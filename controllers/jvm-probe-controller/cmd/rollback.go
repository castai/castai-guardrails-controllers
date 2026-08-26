// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	"github.com/castai/castai-guardrails-controllers/snapshot"
)

// jvmInverseFn returns a function that restores the original probes on the
// target Deployment or StatefulSet via JSON-Patch.
//
// Per probe field, two cases:
//   - *Present=false at capture → op=remove (castai added the probe)
//   - *Present=true at capture  → op=replace with original value (castai replaced)
//
// Containers missing from the live workload are skipped with a warning.
func jvmInverseFn(clientset kubernetes.Interface) snapshot.TargetLookupFn[*workloadsv1.JVMProbeOriginal] {
	return func(ctx context.Context, snap *workloadsv1.JVMProbeOriginal) (string, bool, error) {
		ref := snap.Spec.TargetRef
		var live metav1.Object
		var containers []corev1.Container
		switch ref.Kind {
		case "Deployment":
			d, err := clientset.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					return "", false, snapshot.ErrTargetGone
				}
				return "", false, err
			}
			live = d
			containers = d.Spec.Template.Spec.Containers
		case "StatefulSet":
			s, err := clientset.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					return "", false, snapshot.ErrTargetGone
				}
				return "", false, err
			}
			live = s
			containers = s.Spec.Template.Spec.Containers
		default:
			return "", false, fmt.Errorf("unsupported target kind %q", ref.Kind)
		}

		if string(live.GetUID()) != string(ref.UID) {
			return string(live.GetUID()), false, snapshot.ErrTargetGone
		}

		if err := applyJVMRollback(ctx, clientset, ref, containers, snap); err != nil {
			return string(live.GetUID()), true, err
		}
		return string(live.GetUID()), true, nil
	}
}

// applyJVMRollback builds and applies the JSON-Patch that restores per-container probes.
func applyJVMRollback(ctx context.Context, clientset kubernetes.Interface, ref workloadsv1.TargetRef, containers []corev1.Container, snap *workloadsv1.JVMProbeOriginal) error {
	var ops []map[string]interface{}
	for liveIdx, liveC := range containers {
		original, ok := snap.Spec.OriginalContainers[liveC.Name]
		if !ok {
			continue
		}
		base := fmt.Sprintf("/spec/template/spec/containers/%d", liveIdx)
		// Only emit `remove` when the probe is actually on the live workload;
		// otherwise the JSON-Patch `remove` would 422 on a missing field.
		ops = append(ops, buildProbeOp(base+"/livenessProbe", liveC.LivenessProbe, original.LivenessProbe, original.LivenessPresent)...)
		ops = append(ops, buildProbeOp(base+"/readinessProbe", liveC.ReadinessProbe, original.ReadinessProbe, original.ReadinessPresent)...)
		ops = append(ops, buildProbeOp(base+"/startupProbe", liveC.StartupProbe, original.StartupProbe, original.StartupPresent)...)
	}

	if len(ops) == 0 {
		return nil
	}

	body, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("marshal inverse patch: %w", err)
	}

	switch ref.Kind {
	case "Deployment":
		_, err = clientset.AppsV1().Deployments(ref.Namespace).Patch(ctx, ref.Name, types.JSONPatchType, body, metav1.PatchOptions{})
	case "StatefulSet":
		_, err = clientset.AppsV1().StatefulSets(ref.Namespace).Patch(ctx, ref.Name, types.JSONPatchType, body, metav1.PatchOptions{})
	default:
		return fmt.Errorf("unsupported target kind %q", ref.Kind)
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return snapshot.ErrTargetGone
		}
		return err
	}
	return nil
}

// buildProbeOp emits a JSON-Patch op for one probe. Only emits `remove` if
// the live workload currently has the probe (JSON-Patch `remove` 422s on
// missing fields).
func buildProbeOp(path string, liveProbe *corev1.Probe, orig *corev1.Probe, wasPresent bool) []map[string]interface{} {
	if !wasPresent {
		if liveProbe == nil {
			// nothing to remove — probe already absent
			return nil
		}
		return []map[string]interface{}{{"op": "remove", "path": path}}
	}
	return []map[string]interface{}{{"op": "replace", "path": path, "value": orig}}
}

// applyInverseJVMPatch is the standalone inverse-patch entry point used by tests.
func applyInverseJVMPatch(ctx context.Context, clientset kubernetes.Interface, snap *workloadsv1.JVMProbeOriginal) error {
	ref := snap.Spec.TargetRef
	var containers []corev1.Container
	switch ref.Kind {
	case "Deployment":
		d, err := clientset.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return snapshot.ErrTargetGone
			}
			return err
		}
		if string(d.UID) != string(ref.UID) {
			return snapshot.ErrTargetGone
		}
		containers = d.Spec.Template.Spec.Containers
	case "StatefulSet":
		s, err := clientset.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return snapshot.ErrTargetGone
			}
			return err
		}
		if string(s.UID) != string(ref.UID) {
			return snapshot.ErrTargetGone
		}
		containers = s.Spec.Template.Spec.Containers
	default:
		return fmt.Errorf("unsupported target kind %q", ref.Kind)
	}
	return applyJVMRollback(ctx, clientset, ref, containers, snap)
}
