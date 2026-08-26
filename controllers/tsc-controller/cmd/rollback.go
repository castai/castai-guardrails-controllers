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

// tscInverseFn returns a snapshot inverse function that restores the original
// topologySpreadConstraints on the target Deployment or StatefulSet.
//
// Behaviour (matches snapshot/rollback.go lookup contract):
//   - UID mismatch or target gone → return snapshot.ErrTargetGone.
//   - OriginalTSCsPresent=true  → patch field with OriginalTSCs.
//   - OriginalTSCsPresent=false → patch field with nil (Strategic Merge
//     Patch deletes the field).
func tscInverseFn(clientset kubernetes.Interface) snapshot.TargetLookupFn[*workloadsv1.TSCOriginal] {
	return func(ctx context.Context, snap *workloadsv1.TSCOriginal) (string, bool, error) {
		ref := snap.Spec.TargetRef
		live, err := lookupTarget(ctx, clientset, ref)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return "", false, snapshot.ErrTargetGone
			}
			return "", false, err
		}
		liveUID := string(live.GetUID())
		if liveUID != string(ref.UID) {
			return liveUID, false, snapshot.ErrTargetGone
		}
		return liveUID, true, nil
	}
}

// applyInversePatch builds and applies the inverse patch for a single
// TSCOriginal snapshot. It is split from tscInverseFn so callers (tests,
// future controllers) can reuse the patch-building logic without needing a
// kubernetes.Interface.
//
// Patch type: JSON Merge Patch (RFC 7396). The topologySpreadConstraints
// field is a list declared with patchMergeKey=topologyKey in the PodSpec
// schema, which means Strategic Merge Patch would merge by topologyKey
// instead of replacing the list. That would re-introduce TSCs that the
// controller previously injected under a different key. JSON Merge Patch
// replaces the field outright, which is what we want.
func applyInversePatch(ctx context.Context, clientset kubernetes.Interface, snap *workloadsv1.TSCOriginal) error {
	ref := snap.Spec.TargetRef

	// UID verification.
	live, err := lookupTarget(ctx, clientset, ref)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return snapshot.ErrTargetGone
		}
		return err
	}
	if string(live.GetUID()) != string(ref.UID) {
		return snapshot.ErrTargetGone
	}

	patchBytes, err := json.Marshal(buildInversePatchBody(snap))
	if err != nil {
		return fmt.Errorf("marshal inverse patch: %w", err)
	}

	switch ref.Kind {
	case "Deployment":
		_, err = clientset.AppsV1().Deployments(ref.Namespace).Patch(
			ctx, ref.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	case "StatefulSet":
		_, err = clientset.AppsV1().StatefulSets(ref.Namespace).Patch(
			ctx, ref.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
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

// inverseTSCValue returns the value to assign to topologySpreadConstraints in
// the inverse patch: nil when the field was absent pre-capture, the captured
// list otherwise.
func inverseTSCValue(snap *workloadsv1.TSCOriginal) interface{} {
	if !snap.Spec.OriginalTSCsPresent {
		return nil
	}
	if snap.Spec.OriginalTSCs == nil {
		// Present=true but empty slice → set to empty list (no TSCs but
		// field exists). Marshal as [].
		return []corev1.TopologySpreadConstraint{}
	}
	return snap.Spec.OriginalTSCs
}

// buildInversePatchBody builds the JSON Merge Patch body that restores
// (or removes) the topologySpreadConstraints field on a Deployment or
// StatefulSet pod template. See applyInversePatch for why we use JSON
// Merge Patch rather than Strategic Merge Patch here.
func buildInversePatchBody(snap *workloadsv1.TSCOriginal) map[string]interface{} {
	return map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"topologySpreadConstraints": inverseTSCValue(snap),
				},
			},
		},
	}
}

// workloadRefUID is a tiny interface that both Deployment and StatefulSet
// implement for the bits we need from the live object.
type workloadRefUID interface {
	GetUID() types.UID
}

func lookupTarget(ctx context.Context, clientset kubernetes.Interface, ref workloadsv1.TargetRef) (workloadRefUID, error) {
	switch ref.Kind {
	case "Deployment":
		d, err := clientset.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return deploymentUIDAdapter{d}, nil
	case "StatefulSet":
		s, err := clientset.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return statefulSetUIDAdapter{s}, nil
	default:
		return nil, fmt.Errorf("unsupported target kind %q", ref.Kind)
	}
}

// deploymentUIDAdapter wraps *appsv1.Deployment to expose GetUID via the
// metav1.Object interface that the apps types already implement.
type deploymentUIDAdapter struct{ inner interface{ GetUID() types.UID } }

func (a deploymentUIDAdapter) GetUID() types.UID { return a.inner.GetUID() }

type statefulSetUIDAdapter struct{ inner interface{ GetUID() types.UID } }

func (a statefulSetUIDAdapter) GetUID() types.UID { return a.inner.GetUID() }
