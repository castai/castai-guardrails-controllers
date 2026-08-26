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

// TSCClient implements Client[*workloadsv1.TSCOriginal] over a typed REST clientset.
type TSCClient struct {
	inner      workloadsclient.TSCOriginalInterface
	ns         string
}

// NewTSCClient builds a TSCClient from a REST config. The REST client targets
// the workloads.cast.ai/v1 group.
func NewTSCClient(restConfig *rest.Config, namespace string) (*TSCClient, error) {
	c, err := workloadsclient.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	return &TSCClient{inner: c.TSCOriginals(namespace), ns: namespace}, nil
}

// NewTSCClientFromInterface is a test-friendly constructor.
func NewTSCClientFromInterface(i workloadsclient.TSCOriginalInterface, namespace string) *TSCClient {
	return &TSCClient{inner: i, ns: namespace}
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

func (c *TSCClient) Patch(ctx context.Context, _, name string, pt PatchType, data []byte) (*workloadsv1.TSCOriginal, error) {
	return c.inner.Patch(ctx, name, types.PatchType(pt), data, metav1.PatchOptions{})
}
