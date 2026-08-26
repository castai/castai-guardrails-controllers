// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group used by workload snapshot CRDs.
const GroupName = "workloads.cast.ai"

// SchemeGroupVersion is the group/version used to register these objects.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1"}

// Resource takes an unqualified resource and returns a Group qualified GroupResource.
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

var (
	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme is a registration function for the Workloads types.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Kind names for the two snapshot CRDs.
const (
	KindTSCOriginal      = "TSCOriginal"
	KindJVMProbeOriginal = "JVMProbeOriginal"
)

// addKnownTypes registers the workload snapshot types with the given scheme.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&TSCOriginal{},
		&TSCOriginalList{},
		&JVMProbeOriginal{},
		&JVMProbeOriginalList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
