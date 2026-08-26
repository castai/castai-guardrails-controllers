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
// All methods pass through the namespace argument to the underlying clientset.
type TSCClient struct {
	client workloadsclient.WorkloadsV1Interface
}

// NewTSCClient builds a TSCClient from a REST config.
func NewTSCClient(restConfig *rest.Config) (*TSCClient, error) {
	c, err := workloadsclient.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	return &TSCClient{client: c}, nil
}

// NewTSCClientFromInterface is a test-friendly constructor.
func NewTSCClientFromInterface(i workloadsclient.WorkloadsV1Interface) *TSCClient {
	return &TSCClient{client: i}
}

func (c *TSCClient) Get(ctx context.Context, namespace, name string) (*workloadsv1.TSCOriginal, error) {
	return c.client.TSCOriginals(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *TSCClient) Create(ctx context.Context, namespace string, obj *workloadsv1.TSCOriginal) (*workloadsv1.TSCOriginal, error) {
	return c.client.TSCOriginals(namespace).Create(ctx, obj, metav1.CreateOptions{})
}

func (c *TSCClient) Update(ctx context.Context, namespace string, obj *workloadsv1.TSCOriginal) (*workloadsv1.TSCOriginal, error) {
	return c.client.TSCOriginals(namespace).Update(ctx, obj, metav1.UpdateOptions{})
}

func (c *TSCClient) UpdateStatus(ctx context.Context, namespace string, obj *workloadsv1.TSCOriginal) (*workloadsv1.TSCOriginal, error) {
	return c.client.TSCOriginals(namespace).UpdateStatus(ctx, obj, metav1.UpdateOptions{})
}

func (c *TSCClient) Delete(ctx context.Context, namespace, name string) error {
	return c.client.TSCOriginals(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (c *TSCClient) List(ctx context.Context, namespace string) ([]*workloadsv1.TSCOriginal, error) {
	l, err := c.client.TSCOriginals(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*workloadsv1.TSCOriginal, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, &l.Items[i])
	}
	return out, nil
}

func (c *TSCClient) Patch(ctx context.Context, namespace, name string, pt types.PatchType, data []byte) (*workloadsv1.TSCOriginal, error) {
	return c.client.TSCOriginals(namespace).Patch(ctx, name, pt, data, metav1.PatchOptions{})
}
