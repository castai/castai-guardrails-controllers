// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

// Hand-written typed client for TSCOriginal. See hack/update-codegen.sh.

package v1

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	"github.com/castai/castai-guardrails-controllers/clientset/versioned/scheme"
)

// TSCOriginalsGetter exposes the TSCOriginalInterface for a given REST client.
type TSCOriginalsGetter interface {
	TSCOriginals(namespace string) TSCOriginalInterface
}

// TSCOriginalInterface is the typed client interface for TSCOriginal CRDs.
type TSCOriginalInterface interface {
	List(ctx context.Context, opts metav1.ListOptions) (*workloadsv1.TSCOriginalList, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*workloadsv1.TSCOriginal, error)
	Create(ctx context.Context, obj *workloadsv1.TSCOriginal, opts metav1.CreateOptions) (*workloadsv1.TSCOriginal, error)
	Update(ctx context.Context, obj *workloadsv1.TSCOriginal, opts metav1.UpdateOptions) (*workloadsv1.TSCOriginal, error)
	UpdateStatus(ctx context.Context, obj *workloadsv1.TSCOriginal, opts metav1.UpdateOptions) (*workloadsv1.TSCOriginal, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*workloadsv1.TSCOriginal, error)
}

// tscOriginalClient is the REST-backed implementation of TSCOriginalInterface.
type tscOriginalClient struct {
	restClient rest.Interface
	ns         string
}

func newTSCOriginalClient(c rest.Interface, namespace string) *tscOriginalClient {
	return &tscOriginalClient{restClient: c, ns: namespace}
}

func (c *tscOriginalClient) List(ctx context.Context, opts metav1.ListOptions) (*workloadsv1.TSCOriginalList, error) {
	var out workloadsv1.TSCOriginalList
	err := c.restClient.
		Get().
		Namespace(c.ns).
		Resource("tscoriginals").
		VersionedParams(&opts, scheme.ParameterCodec).
		Do(ctx).
		Into(&out)
	return &out, err
}

func (c *tscOriginalClient) Get(ctx context.Context, name string, opts metav1.GetOptions) (*workloadsv1.TSCOriginal, error) {
	var out workloadsv1.TSCOriginal
	err := c.restClient.
		Get().
		Namespace(c.ns).
		Resource("tscoriginals").
		Name(name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Do(ctx).
		Into(&out)
	return &out, err
}

func (c *tscOriginalClient) Create(ctx context.Context, obj *workloadsv1.TSCOriginal, opts metav1.CreateOptions) (*workloadsv1.TSCOriginal, error) {
	var out workloadsv1.TSCOriginal
	err := c.restClient.
		Post().
		Namespace(c.ns).
		Resource("tscoriginals").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(obj).
		Do(ctx).
		Into(&out)
	return &out, err
}

func (c *tscOriginalClient) Update(ctx context.Context, obj *workloadsv1.TSCOriginal, opts metav1.UpdateOptions) (*workloadsv1.TSCOriginal, error) {
	var out workloadsv1.TSCOriginal
	err := c.restClient.
		Put().
		Namespace(c.ns).
		Resource("tscoriginals").
		Name(obj.Name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(obj).
		Do(ctx).
		Into(&out)
	return &out, err
}

func (c *tscOriginalClient) UpdateStatus(ctx context.Context, obj *workloadsv1.TSCOriginal, opts metav1.UpdateOptions) (*workloadsv1.TSCOriginal, error) {
	var out workloadsv1.TSCOriginal
	err := c.restClient.
		Put().
		Namespace(c.ns).
		Resource("tscoriginals").
		Name(obj.Name).
		SubResource("status").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(obj).
		Do(ctx).
		Into(&out)
	return &out, err
}

func (c *tscOriginalClient) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	return c.restClient.
		Delete().
		Namespace(c.ns).
		Resource("tscoriginals").
		Name(name).
		Body(&opts).
		Do(ctx).
		Error()
}

func (c *tscOriginalClient) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*workloadsv1.TSCOriginal, error) {
	var out workloadsv1.TSCOriginal
	req := c.restClient.
		Patch(pt).
		Namespace(c.ns).
		Resource("tscoriginals").
		Name(name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(data)
	for _, s := range subresources {
		req = req.SubResource(s)
	}
	err := req.Do(ctx).Into(&out)
	return &out, err
}
