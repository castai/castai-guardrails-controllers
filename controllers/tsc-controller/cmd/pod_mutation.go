// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// jsonPatchOp is the subset of RFC 6902 operations the webhook emits.
type jsonPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// podMutationResult holds the patch set produced for a single Pod admission
// request along with a flag indicating whether any mutation was applied.
// mutationApplied gates the decision to attach a non-empty Patch field to
// the AdmissionResponse.
type podMutationResult struct {
	patches         []jsonPatchOp
	mutationApplied bool
}

// workloadOwner captures the resolved workload (Deployment or StatefulSet)
// that owns a Pod. Annotations and labels are read from the parent rather
// than the Pod so the legacy per-workload annotation contract is preserved.
type workloadOwner struct {
	kind        string // "Deployment" or "StatefulSet"
	name        string
	namespace   string
	annotations map[string]string
	labels      map[string]string
	replicas    int32 // 0 = unknown / no spec
	// found reports whether the workload was resolved. A false result means
	// the Pod is unmanaged (no recognised owner) and must be skipped.
	found bool
}

// mutatePod is the entry point invoked from the HTTP handler. It decodes the
// AdmissionReview's Pod, builds the patch set, and returns an
// AdmissionResponse suitable for writing back to the API server.
//
// The function is safe for dry-run requests: when req.DryRun is true the
// patch is still computed and returned, but the API server does not
// persist the mutation. The webhook only describes mutations — it does
// not apply them.
//
// cfg must be non-nil; callers (HTTP handler) take a snapshot of the
// global config under configLock before invoking this function. Owner
// lookups use the package-level clientset (the production wiring) or
// whatever clientset the caller has installed — tests that need a fake
// clientset reassign the global pointer.
func mutatePod(_ context.Context, review *admissionv1.AdmissionReview, cfg *TSCConfig) (*admissionv1.AdmissionResponse, error) {
	if review == nil || review.Request == nil {
		return nil, fmt.Errorf("nil admission review or request")
	}
	req := review.Request

	resp := &admissionv1.AdmissionResponse{
		UID:     req.UID,
		Allowed: true,
	}

	// Admission requests may arrive with the object already decoded or as
	// raw JSON; the Raw field is always populated on the server side. If
	// it is absent there is nothing to mutate — return an allow response
	// without a patch.
	if len(req.Object.Raw) == 0 {
		return resp, nil
	}

	pod := &corev1.Pod{}
	if err := json.Unmarshal(req.Object.Raw, pod); err != nil {
		return nil, fmt.Errorf("decode pod: %w", err)
	}

	result, err := buildPodTSCPatches(pod, cfg, getClientset())
	if err != nil {
		return nil, err
	}
	if !result.mutationApplied || len(result.patches) == 0 {
		return resp, nil
	}

	patchBytes, err := json.Marshal(result.patches)
	if err != nil {
		return nil, fmt.Errorf("marshal patch: %w", err)
	}
	pt := admissionv1.PatchTypeJSONPatch
	resp.PatchType = &pt
	resp.Patch = patchBytes
	return resp, nil
}

// buildPodTSCPatches is the core mutation algorithm and is kept pure with
// respect to its inputs: it does not mutate the Pod argument or the cfg
// argument, only builds a patch list. The Kubernetes client is used only
// for owner-reference lookup and is otherwise untouched.
//
// Algorithm:
//  1. management.enabled=false or mode=recommend → no patch.
//  2. Pod carries tsc-bypass annotation → no patch.
//  3. Pod already has non-empty topologySpreadConstraints → no patch.
//  4. Resolve the owning workload (Deployment via ReplicaSet, or
//     StatefulSet). If none → no patch.
//  5. Apply exclusion rules against the workload's namespace/name/labels.
//  6. If skipSingleReplica and replicas<2 → no patch.
//  7. Build constraints from config (annotation overrides come from the
//     workload).
//  8. Emit a single `add` patch at /spec/topologySpreadConstraints.
//
// The function is the primary entry point for unit tests; the HTTP handler
// in handlePodAdmission calls into mutatePod, which calls this function.
func buildPodTSCPatches(pod *corev1.Pod, cfg *TSCConfig, cs kubernetes.Interface) (*podMutationResult, error) {
	if pod == nil || cfg == nil {
		return &podMutationResult{}, nil
	}

	// Step 1: management enabled?
	if !cfg.ManagementEnabled {
		logDebug("tsc-disabled", "Skipping %s/%s, management disabled", pod.Namespace, pod.Name)
		return &podMutationResult{}, nil
	}

	// Step 2: bypass annotation on the Pod template. The plan explicitly
	// permits the user to set tsc-bypass on the Pod template as an
	// emergency opt-out without editing the workload.
	if pod.Annotations[AnnotationBypass] == "true" {
		logDebug("tsc-bypass", "Skipping %s/%s, bypass annotation present", pod.Namespace, pod.Name)
		return &podMutationResult{}, nil
	}

	// Step 3: Pod already carries TSCs (likely inherited from the parent
	// workload's pod template). Webhook must not duplicate them.
	if len(pod.Spec.TopologySpreadConstraints) > 0 {
		return &podMutationResult{}, nil
	}

	// Step 4: resolve the owning workload via ownerReferences. Pods with
	// no recognised owner (e.g., bare Pods, Jobs, DaemonSets) are skipped.
	owner, err := resolveWorkloadOwner(pod, cs)
	if err != nil {
		// Per the plan, lookup errors must default to skipping (safe).
		logWarn("tsc-owner-lookup", "Failed to resolve workload owner for %s/%s: %v", pod.Namespace, pod.Name, err)
		return &podMutationResult{}, nil
	}
	if !owner.found {
		logDebug("tsc-no-owner", "Skipping %s/%s, no recognised workload owner", pod.Namespace, pod.Name)
		return &podMutationResult{}, nil
	}

	// Step 5: exclusion rules — must be evaluated against the workload,
	// not the Pod, per the plan.
	if isExcluded(owner.namespace, owner.name, owner.labels) {
		logDebug("tsc-excluded", "Skipping %s/%s/%s, excluded by rule", owner.kind, owner.namespace, owner.name)
		return &podMutationResult{}, nil
	}

	// Step 6: skipSingleReplica (read from cfg, NOT from the Pod). The
	// legacy behaviour skipped workloads with fewer than two replicas.
	// Kubernetes defaults a nil Spec.Replicas to 1, and replicasOrZero
	// returns 0 for that case, so the comparison < 2 covers both the
	// "replicas=1" and "replicas unset" shapes.
	if cfg.SkipSingleReplica && owner.replicas < 2 {
		logDebug("tsc-low-replicas", "Skipping %s/%s/%s, replicas=%d", owner.kind, owner.namespace, owner.name, owner.replicas)
		return &podMutationResult{}, nil
	}

	// Step 7: build the constraint list. Annotation overrides come from
	// the workload annotations.
	constraints := buildPodTSCConstraints(owner.namespace, owner.name, owner.annotations, owner.labels, cfg)
	if len(constraints) == 0 {
		return &podMutationResult{}, nil
	}

	// Step 8: dry-run/apply. mode=recommend logs but does not patch.
	if cfg.Mode == ModeRecommend {
		logInfo("tsc-recommend", "[RECOMMEND] Would add %d TSC(s) to %s/%s/%s",
			len(constraints), owner.kind, owner.namespace, owner.name)
		return &podMutationResult{}, nil
	}

	return &podMutationResult{
		patches: []jsonPatchOp{{
			Op:    "add",
			Path:  "/spec/topologySpreadConstraints",
			Value: constraints,
		}},
		mutationApplied: true,
	}, nil
}

// resolveWorkloadOwner walks the Pod's ownerReferences and resolves the
// owning Deployment or StatefulSet. The function performs up to two API
// round-trips:
//  1. Direct owner is ReplicaSet → fetch the ReplicaSet to find its
//     Deployment parent.
//  2. Direct owner is StatefulSet → fetch the StatefulSet directly.
//
// Any Pod whose direct owner is not a ReplicaSet or StatefulSet is
// considered unmanaged (owner.found = false). Pods with no owner
// references at all are also unmanaged. A ReplicaSet whose Deployment
// parent cannot be resolved (e.g., the ReplicaSet has no owner or the
// Deployment lookup failed) is treated as unmanaged — this matches the
// legacy behaviour where only Deployment/StatefulSet workloads were
// patched.
func resolveWorkloadOwner(pod *corev1.Pod, cs kubernetes.Interface) (workloadOwner, error) {
	if pod == nil || cs == nil {
		return workloadOwner{}, nil
	}
	if len(pod.OwnerReferences) == 0 {
		return workloadOwner{}, nil
	}

	// Look for the first recognised owner. When multiple owners are set
	// (rare in practice) we pick the most specific one: a StatefulSet is
	// preferred over a ReplicaSet, which is preferred over anything else.
	var (
		replicaSetName  string
		statefulSetName string
		replicaSetFound bool
	)
	for i := range pod.OwnerReferences {
		ref := pod.OwnerReferences[i]
		switch ref.Kind {
		case "StatefulSet":
			statefulSetName = ref.Name
		case "ReplicaSet":
			replicaSetName = ref.Name
			replicaSetFound = true
		}
	}

	if statefulSetName != "" {
		sts, err := cs.AppsV1().StatefulSets(pod.Namespace).Get(context.Background(), statefulSetName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return workloadOwner{}, nil
			}
			return workloadOwner{}, err
		}
		return workloadOwner{
			kind:        "StatefulSet",
			name:        sts.Name,
			namespace:   sts.Namespace,
			annotations: sts.Annotations,
			labels:      sts.Spec.Template.Labels,
			replicas:    replicasOrZero(sts.Spec.Replicas),
			found:       true,
		}, nil
	}

	if replicaSetFound {
		rs, err := cs.AppsV1().ReplicaSets(pod.Namespace).Get(context.Background(), replicaSetName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return workloadOwner{}, nil
			}
			return workloadOwner{}, err
		}
		// Walk one level up to find the Deployment owner. If absent, the
		// Pod is a bare ReplicaSet (e.g., manually created); treat as
		// unmanaged.
		var depName string
		for i := range rs.OwnerReferences {
			if rs.OwnerReferences[i].Kind == "Deployment" {
				depName = rs.OwnerReferences[i].Name
				break
			}
		}
		if depName == "" {
			return workloadOwner{}, nil
		}
		dep, err := cs.AppsV1().Deployments(pod.Namespace).Get(context.Background(), depName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return workloadOwner{}, nil
			}
			return workloadOwner{}, err
		}
		return workloadOwner{
			kind:        "Deployment",
			name:        dep.Name,
			namespace:   dep.Namespace,
			annotations: dep.Annotations,
			labels:      dep.Spec.Template.Labels,
			replicas:    replicasOrZero(dep.Spec.Replicas),
			found:       true,
		}, nil
	}

	return workloadOwner{}, nil
}

// replicasOrZero returns *replicas or 0 if the pointer is nil. A zero
// value combined with the < 2 check in buildPodTSCPatches matches the
// legacy behaviour where an absent Spec.Replicas defaulted to 1 and
// triggered the skipSingleReplica path.
func replicasOrZero(replicas *int32) int32 {
	if replicas == nil {
		return 0
	}
	return *replicas
}

// ptr is a small helper for taking the address of a literal — used to
// build TopologySpreadConstraint values whose NodeAffinityPolicy /
// NodeTaintsPolicy fields are *NodeInclusionPolicy.
func ptr[T any](v T) *T { return &v }

// buildPodTSCConstraints constructs the constraint list to be injected
// into a Pod. The function is pure and depends only on its inputs plus
// cfg.DefaultConstraints (read under configLock by callers — note that
// buildPodTSCPatches passes cfg without holding the lock; the HTTP
// handler is responsible for taking a snapshot under configLock before
// invoking the chain).
//
// Precedence (highest first):
//  1. Workload annotation tsc-constraints (full JSON override) →
//     returned verbatim. The user controls every field, including
//     LabelSelector and MatchLabelKeys.
//  2. Workload annotations tsc-maxSkew, tsc-topologyKey,
//     tsc-whenUnsatisfiable → emit a single constraint using the
//     default empty LabelSelector / matchLabelKeys=[pod-template-hash]
//     pair.
//  3. cfg.DefaultConstraints → emit one Pod-format constraint per
//     configured default, with the same empty LabelSelector /
//     matchLabelKeys pair.
func buildPodTSCConstraints(namespace, name string, annotations, labels map[string]string, cfg *TSCConfig) []corev1.TopologySpreadConstraint {
	_ = namespace
	_ = name
	_ = labels
	if cfg == nil {
		return nil
	}

	// 1. Full JSON override wins.
	if annotations != nil {
		if overrideJSON, ok := annotations[AnnotationConstraints]; ok && overrideJSON != "" {
			var override []corev1.TopologySpreadConstraint
			if err := json.Unmarshal([]byte(overrideJSON), &override); err == nil && len(override) > 0 {
				return override
			}
		}

		// 2. Per-field overrides. If at least one is set, emit a single
		// constraint using the supplied values and the default
		// label-selector / match-label-keys policy.
		maxSkew, topologyKey, whenUnsatisfiable, hasAnyOverride := readSimpleOverrides(annotations)
		if hasAnyOverride {
			return []corev1.TopologySpreadConstraint{
				defaultConstraintTemplate(maxSkew, topologyKey, whenUnsatisfiable),
			}
		}
	}

	// 3. Default constraints from the ConfigMap. Each default constraint
	// is converted into the Pod-shape (empty LabelSelector +
	// matchLabelKeys=[pod-template-hash] + Honor policies) so the
	// emitted JSON matches the customer's required output exactly.
	if len(cfg.DefaultConstraints) == 0 {
		return nil
	}
	out := make([]corev1.TopologySpreadConstraint, 0, len(cfg.DefaultConstraints))
	for _, c := range cfg.DefaultConstraints {
		out = append(out, defaultConstraintTemplate(c.MaxSkew, c.TopologyKey, c.WhenUnsatisfiable))
	}
	return out
}

// readSimpleOverrides parses the tsc-maxSkew, tsc-topologyKey and
// tsc-whenUnsatisfiable annotations. The third return value reports
// whether at least one override was present; callers use this to
// distinguish "user wants a single custom constraint" from "user wants
// the ConfigMap defaults".
func readSimpleOverrides(annotations map[string]string) (maxSkew int32, topologyKey string, whenUnsatisfiable corev1.UnsatisfiableConstraintAction, hasOverride bool) {
	maxSkew = 1
	topologyKey = "topology.kubernetes.io/zone"
	// Default to ScheduleAnyway to match the ConfigMap defaults and the
	// Goal JSON patch — a user who only overrides tsc-maxSkew or
	// tsc-topologyKey should keep the controller's default scheduling
	// policy unless they explicitly opt in to DoNotSchedule.
	whenUnsatisfiable = corev1.ScheduleAnyway

	if v, ok := annotations[AnnotationMaxSkew]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxSkew = int32(n)
			hasOverride = true
		}
	}
	if v, ok := annotations[AnnotationTopologyKey]; ok && v != "" {
		topologyKey = v
		hasOverride = true
	}
	if v, ok := annotations[AnnotationWhenUnsat]; ok && v != "" {
		if v == string(corev1.ScheduleAnyway) {
			whenUnsatisfiable = corev1.ScheduleAnyway
			hasOverride = true
		} else if v == string(corev1.DoNotSchedule) {
			whenUnsatisfiable = corev1.DoNotSchedule
			hasOverride = true
		}
	}
	return maxSkew, topologyKey, whenUnsatisfiable, hasOverride
}

// defaultConstraintTemplate returns a TopologySpreadConstraint in the
// shape required by the customer's working solution: empty
// LabelSelector (renders as `{}` in JSON), matchLabelKeys fixed to
// ["pod-template-hash"], and Honor policies for nodeAffinityPolicy /
// nodeTaintsPolicy. This is the shape the plan's Goal JSON Patch emits
// for the default case.
func defaultConstraintTemplate(maxSkew int32, topologyKey string, whenUnsatisfiable corev1.UnsatisfiableConstraintAction) corev1.TopologySpreadConstraint {
	return corev1.TopologySpreadConstraint{
		MaxSkew:            maxSkew,
		TopologyKey:        topologyKey,
		WhenUnsatisfiable:  whenUnsatisfiable,
		NodeAffinityPolicy: ptr(corev1.NodeInclusionPolicyHonor),
		NodeTaintsPolicy:   ptr(corev1.NodeInclusionPolicyHonor),
		LabelSelector:      &metav1.LabelSelector{},
		MatchLabelKeys:     []string{"pod-template-hash"},
	}
}

// handlePodAdmission is the HTTP entry point for /mutate/pods. It reads the
// AdmissionReview from the body, takes a snapshot of the TSCConfig under
// configLock (so a concurrent hot-reload does not race), and writes the
// response AdmissionReview back to the API server.
//
// Errors during decoding or patch generation are logged and translated into
// an "Allowed: true" response without a patch — the failurePolicy on the
// MutatingWebhookConfiguration is Ignore so we never block Pod creation.
func handlePodAdmission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// requestUID captures the AdmissionRequest UID when available so we can
	// echo it back on every response, including error paths. The API server
	// expects UID to match the request UID for allowed responses.
	var requestUID types.UID

	review := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(review); err != nil {
		logWarn("mutate", "Failed to decode AdmissionReview: %v", err)
		writeAdmissionResponse(w, review, &admissionv1.AdmissionResponse{
			UID:     requestUID,
			Allowed: true,
		})
		return
	}
	if review.Request != nil {
		requestUID = review.Request.UID
	}

	configLock.RLock()
	cfg := config
	configLock.RUnlock()
	if cfg == nil {
		cfg = &TSCConfig{
			ManagementEnabled: true,
			Mode:              ModeApply,
			SkipSingleReplica: true,
		}
	}

	resp, err := mutatePod(r.Context(), review, cfg)
	if err != nil {
		logWarn("mutate", "mutatePod returned error: %v", err)
		resp = &admissionv1.AdmissionResponse{
			UID:     requestUID,
			Allowed: true,
		}
	}
	writeAdmissionResponse(w, review, resp)
}

// writeAdmissionResponse is a small helper for the HTTP layer: it sets the
// content type and writes the marshalled AdmissionReview.
func writeAdmissionResponse(w http.ResponseWriter, review *admissionv1.AdmissionReview, response *admissionv1.AdmissionResponse) {
	out := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
	}
	// Only echo the request TypeMeta back when it is populated. Admission
	// requests typically arrive with an empty TypeMeta, and copying an
	// empty APIVersion/Kind would produce a malformed response.
	if review != nil && (review.APIVersion != "" || review.Kind != "") {
		out.TypeMeta = review.TypeMeta
	}
	out.Response = response
	w.Header().Set("Content-Type", "application/json")
	if response == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// failurePolicy=Ignore means we never block Pod creation. We always
	// return 200 here so the API server accepts the review.
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}
