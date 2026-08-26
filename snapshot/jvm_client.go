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

// JVMClient implements Client[*workloadsv1.JVMProbeOriginal] over a typed REST clientset.
type JVMClient struct {
	inner workloadsclient.JVMProbeOriginalInterface
}

// NewJVMClient builds a JVMClient from a REST config.
func NewJVMClient(restConfig *rest.Config, namespace string) (*JVMClient, error) {
	c, err := workloadsclient.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	return &JVMClient{inner: c.JVMProbeOriginals(namespace)}, nil
}

// NewJVMClientFromInterface is a test-friendly constructor.
func NewJVMClientFromInterface(i workloadsclient.JVMProbeOriginalInterface) *JVMClient {
	return &JVMClient{inner: i}
}

func (c *JVMClient) Get(ctx context.Context, _, name string) (*workloadsv1.JVMProbeOriginal, error) {
	return c.inner.Get(ctx, name, metav1.GetOptions{})
}

func (c *JVMClient) Create(ctx context.Context, _ string, obj *workloadsv1.JVMProbeOriginal) (*workloadsv1.JVMProbeOriginal, error) {
	return c.inner.Create(ctx, obj, metav1.CreateOptions{})
}

func (c *JVMClient) Update(ctx context.Context, _ string, obj *workloadsv1.JVMProbeOriginal) (*workloadsv1.JVMProbeOriginal, error) {
	return c.inner.Update(ctx, obj, metav1.UpdateOptions{})
}

func (c *JVMClient) UpdateStatus(ctx context.Context, _ string, obj *workloadsv1.JVMProbeOriginal) (*workloadsv1.JVMProbeOriginal, error) {
	return c.inner.UpdateStatus(ctx, obj, metav1.UpdateOptions{})
}

func (c *JVMClient) Delete(ctx context.Context, _, name string) error {
	return c.inner.Delete(ctx, name, metav1.DeleteOptions{})
}

func (c *JVMClient) List(ctx context.Context, _ string) ([]*workloadsv1.JVMProbeOriginal, error) {
	l, err := c.inner.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*workloadsv1.JVMProbeOriginal, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, &l.Items[i])
	}
	return out, nil
}

func (c *JVMClient) Patch(ctx context.Context, _, name string, pt types.PatchType, data []byte) (*workloadsv1.JVMProbeOriginal, error) {
	return c.inner.Patch(ctx, name, pt, data, metav1.PatchOptions{})
}
