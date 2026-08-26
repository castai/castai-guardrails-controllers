// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package snapshot

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	workloadsclient "github.com/castai/castai-guardrails-controllers/clientset/versioned/typed/workloads/v1"
)

// TSCClient implements Client[*workloadsv1.TSCOriginal] over a typed REST
// clientset. The client is bound to a single namespace at construction time.
type TSCClient struct {
	inner workloadsclient.TSCOriginalInterface
}

// NewTSCClient builds a TSCClient from a REST config and a namespace.
func NewTSCClient(restConfig *rest.Config, namespace string) (*TSCClient, error) {
	c, err := workloadsclient.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	return &TSCClient{inner: c.TSCOriginals(namespace)}, nil
}

// NewTSCClientFromClient builds a TSCClient from a typed WorkloadsV1 client
// and a namespace. Used by controllers that already constructed the typed
// clientset (e.g. via versioned.NewForConfigOrDie).
func NewTSCClientFromClient(c workloadsclient.WorkloadsV1Interface, namespace string) *TSCClient {
	return &TSCClient{inner: c.TSCOriginals(namespace)}
}

// NewTSCClientFromInterface is a test-friendly constructor.
func NewTSCClientFromInterface(i workloadsclient.TSCOriginalInterface) *TSCClient {
	return &TSCClient{inner: i}
}

func (c *TSCClient) Get(ctx context.Context, _, name string) (*workloadsv1.TSCOriginal, error) {
	return c.inner.Get(ctx, name, metav1.GetOptions{})
}

func (c *TSCClient) Create(ctx context.Context, _ string, obj *workloadsv1.TSCOriginal) (*workloadsv1.TSCOriginal, error) {
	return c.inner.Create(ctx, obj, metav1.CreateOptions{})
}

func (c *TSCClient) Update(ctx context.Context, _ string, obj *workloadsv1.TSCOriginal) (*workloadsv1.TSCOriginal, error) {
	return c.inner.Update(ctx, obj, metav1.UpdateOptions{})
}

func (c *TSCClient) UpdateStatus(ctx context.Context, _ string, obj *workloadsv1.TSCOriginal) (*workloadsv1.TSCOriginal, error) {
	return c.inner.UpdateStatus(ctx, obj, metav1.UpdateOptions{})
}

func (c *TSCClient) Delete(ctx context.Context, _, name string) error {
	return c.inner.Delete(ctx, name, metav1.DeleteOptions{})
}

func (c *TSCClient) List(ctx context.Context, _ string) ([]*workloadsv1.TSCOriginal, error) {
	l, err := c.inner.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*workloadsv1.TSCOriginal, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, &l.Items[i])
	}
	return out, nil
}

func (c *TSCClient) Patch(ctx context.Context, _, name string, pt types.PatchType, data []byte) (*workloadsv1.TSCOriginal, error) {
	return c.inner.Patch(ctx, name, pt, data, metav1.PatchOptions{})
}
