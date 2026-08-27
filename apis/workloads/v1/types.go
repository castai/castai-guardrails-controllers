// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TargetRef identifies the workload this snapshot belongs to. UID makes it
// resistant to name-reuse (deleted+recreated with same namespace/name).
type TargetRef struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Namespace  string    `json:"namespace"`
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
}

// CommonStatus fields shared by both snapshot CRDs.
type CommonStatus struct {
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is Spec.Generation as observed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// TSCOriginal captures the pre-castai topology spread constraints of a workload
// so the tsc-controller can restore them on rollback.
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=tsco,singular=tscoriginal,path=tscoriginals
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target Kind",type="string",JSONPath=".spec.targetRef.kind"
// +kubebuilder:printcolumn:name="Target Name",type="string",JSONPath=".spec.targetRef.name"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="RolledBack",type="string",JSONPath=".status.conditions[?(@.type=='RolledBack')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type TSCOriginal struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TSCOriginalSpec   `json:"spec"`
	Status TSCOriginalStatus `json:"status,omitempty"`
}

// TSCOriginalSpec is the spec for TSCOriginal CRDs.
type TSCOriginalSpec struct {
	// +kubebuilder:validation:Required
	TargetRef TargetRef `json:"targetRef"`

	// OriginalTSCs is the pre-castai value of
	// spec.template.spec.topologySpreadConstraints on the target workload.
	// Nil means "the field was absent"; empty slice means "the field was []".
	// +optional
	OriginalTSCs []corev1.TopologySpreadConstraint `json:"originalTSCs,omitempty"`

	// OriginalTSCsPresent distinguishes "field was nil" from "field was empty slice".
	OriginalTSCsPresent bool `json:"originalTSCsPresent"`

	// AppliedTSCs is the post-castai value of
	// spec.template.spec.topologySpreadConstraints on the target workload,
	// captured immediately after the controller's patch succeeded. Stored so
	// operators can inspect the effective change and so that downstream
	// tooling (drift detection, post-mortem analysis) can compare applied vs
	// original without re-reading the live workload. Rollback itself still
	// uses OriginalTSCs.
	// +optional
	AppliedTSCs []corev1.TopologySpreadConstraint `json:"appliedTSCs,omitempty"`

	// AppliedTSCsPresent distinguishes "applied field was nil" from
	// "applied field was empty slice".
	AppliedTSCsPresent bool `json:"appliedTSCsPresent"`

	// +kubebuilder:validation:Required
	CapturedAt metav1.Time `json:"capturedAt"`

	// +optional
	ControllerVersion string `json:"controllerVersion,omitempty"`
}

// TSCOriginalStatus is the status for TSCOriginal CRDs.
type TSCOriginalStatus struct {
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
}

// TSCOriginalList is a list of TSCOriginal.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type TSCOriginalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []TSCOriginal `json:"items"`
}

// JVMProbeOriginal captures the pre-castai probe state of all containers of a
// workload so the jvm-probe-controller can restore them on rollback.
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=jvmo,singular=jvmprobeoriginal,path=jvmprobeoriginals
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target Kind",type="string",JSONPath=".spec.targetRef.kind"
// +kubebuilder:printcolumn:name="Target Name",type="string",JSONPath=".spec.targetRef.name"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="RolledBack",type="string",JSONPath=".status.conditions[?(@.type=='RolledBack')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type JVMProbeOriginal struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JVMProbeOriginalSpec   `json:"spec"`
	Status JVMProbeOriginalStatus `json:"status,omitempty"`
}

// JVMProbeOriginalSpec is the spec for JVMProbeOriginal CRDs.
type JVMProbeOriginalSpec struct {
	// +kubebuilder:validation:Required
	TargetRef TargetRef `json:"targetRef"`

	// OriginalContainers maps container name to its pre-castai probe set.
	OriginalContainers map[string]ContainerProbes `json:"originalContainers"`

	// AppliedContainers maps container name to its post-castai probe set,
	// captured immediately after the controller's patch succeeded. Stored
	// for inspection and drift detection; rollback itself still uses
	// OriginalContainers.
	// +optional
	AppliedContainers map[string]ContainerProbes `json:"appliedContainers,omitempty"`

	// AppliedContainersPresent distinguishes "no applied containers recorded"
	// (e.g. capture happened before patch) from "applied containers was an
	// empty map" (patched away every probe).
	AppliedContainersPresent bool `json:"appliedContainersPresent"`

	// +kubebuilder:validation:Required
	CapturedAt metav1.Time `json:"capturedAt"`

	// +optional
	ControllerVersion string `json:"controllerVersion,omitempty"`
}

// ContainerProbes holds the pre-castai probe values for one container.
// A nil pointer means "the probe field was absent"; the Present flags
// distinguish "was nil" from "field omitted at marshal time".
type ContainerProbes struct {
	LivenessProbe  *corev1.Probe `json:"livenessProbe,omitempty"`
	ReadinessProbe *corev1.Probe `json:"readinessProbe,omitempty"`
	StartupProbe   *corev1.Probe `json:"startupProbe,omitempty"`

	LivenessPresent  bool `json:"livenessPresent"`
	ReadinessPresent bool `json:"readinessPresent"`
	StartupPresent   bool `json:"startupPresent"`
}

// JVMProbeOriginalStatus is the status for JVMProbeOriginal CRDs.
type JVMProbeOriginalStatus struct {
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
}

// JVMProbeOriginalList is a list of JVMProbeOriginal.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type JVMProbeOriginalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []JVMProbeOriginal `json:"items"`
}

// DeepCopyObject methods are generated by deepcopy-gen into zz_generated_deepcopy.go.
