// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

// Hand-written typed client for JVMProbeOriginal. See hack/update-codegen.sh.

package v1

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	"github.com/castai/castai-guardrails-controllers/clientset/versioned/scheme"
)

// JVMProbeOriginalsGetter exposes the JVMProbeOriginalInterface for a given REST client.
type JVMProbeOriginalsGetter interface {
	JVMProbeOriginals(namespace string) JVMProbeOriginalInterface
}

// JVMProbeOriginalInterface is the typed client interface for JVMProbeOriginal CRDs.
type JVMProbeOriginalInterface interface {
	List(ctx context.Context, opts metav1.ListOptions) (*workloadsv1.JVMProbeOriginalList, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*workloadsv1.JVMProbeOriginal, error)
	Create(ctx context.Context, obj *workloadsv1.JVMProbeOriginal, opts metav1.CreateOptions) (*workloadsv1.JVMProbeOriginal, error)
	Update(ctx context.Context, obj *workloadsv1.JVMProbeOriginal, opts metav1.UpdateOptions) (*workloadsv1.JVMProbeOriginal, error)
	UpdateStatus(ctx context.Context, obj *workloadsv1.JVMProbeOriginal, opts metav1.UpdateOptions) (*workloadsv1.JVMProbeOriginal, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*workloadsv1.JVMProbeOriginal, error)
}

// jvmProbeOriginalClient is the REST-backed implementation.
type jvmProbeOriginalClient struct {
	restClient rest.Interface
	ns         string
}

func newJVMProbeOriginalClient(c rest.Interface, namespace string) *jvmProbeOriginalClient {
	return &jvmProbeOriginalClient{restClient: c, ns: namespace}
}

func (c *jvmProbeOriginalClient) List(ctx context.Context, opts metav1.ListOptions) (*workloadsv1.JVMProbeOriginalList, error) {
	var out workloadsv1.JVMProbeOriginalList
	err := c.restClient.
		Get().
		Namespace(c.ns).
		Resource("jvmprobeoriginals").
		VersionedParams(&opts, scheme.ParameterCodec).
		Do(ctx).
		Into(&out)
	return &out, err
}

func (c *jvmProbeOriginalClient) Get(ctx context.Context, name string, opts metav1.GetOptions) (*workloadsv1.JVMProbeOriginal, error) {
	var out workloadsv1.JVMProbeOriginal
	err := c.restClient.
		Get().
		Namespace(c.ns).
		Resource("jvmprobeoriginals").
		Name(name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Do(ctx).
		Into(&out)
	return &out, err
}

func (c *jvmProbeOriginalClient) Create(ctx context.Context, obj *workloadsv1.JVMProbeOriginal, opts metav1.CreateOptions) (*workloadsv1.JVMProbeOriginal, error) {
	var out workloadsv1.JVMProbeOriginal
	err := c.restClient.
		Post().
		Namespace(c.ns).
		Resource("jvmprobeoriginals").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(obj).
		Do(ctx).
		Into(&out)
	return &out, err
}

func (c *jvmProbeOriginalClient) Update(ctx context.Context, obj *workloadsv1.JVMProbeOriginal, opts metav1.UpdateOptions) (*workloadsv1.JVMProbeOriginal, error) {
	var out workloadsv1.JVMProbeOriginal
	err := c.restClient.
		Put().
		Namespace(c.ns).
		Resource("jvmprobeoriginals").
		Name(obj.Name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(obj).
		Do(ctx).
		Into(&out)
	return &out, err
}

func (c *jvmProbeOriginalClient) UpdateStatus(ctx context.Context, obj *workloadsv1.JVMProbeOriginal, opts metav1.UpdateOptions) (*workloadsv1.JVMProbeOriginal, error) {
	var out workloadsv1.JVMProbeOriginal
	err := c.restClient.
		Put().
		Namespace(c.ns).
		Resource("jvmprobeoriginals").
		Name(obj.Name).
		SubResource("status").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(obj).
		Do(ctx).
		Into(&out)
	return &out, err
}

func (c *jvmProbeOriginalClient) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	return c.restClient.
		Delete().
		Namespace(c.ns).
		Resource("jvmprobeoriginals").
		Name(name).
		Body(&opts).
		Do(ctx).
		Error()
}

func (c *jvmProbeOriginalClient) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*workloadsv1.JVMProbeOriginal, error) {
	var out workloadsv1.JVMProbeOriginal
	req := c.restClient.
		Patch(pt).
		Namespace(c.ns).
		Resource("jvmprobeoriginals").
		Name(name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(data)
	for _, s := range subresources {
		req = req.SubResource(s)
	}
	err := req.Do(ctx).Into(&out)
	return &out, err
}
