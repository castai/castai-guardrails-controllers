// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

// Hand-written top-level WorkloadsV1 typed client. See hack/update-codegen.sh.

package v1

import (
	"k8s.io/client-go/rest"

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	"github.com/castai/castai-guardrails-controllers/clientset/versioned/scheme"
)

// workloadsv1SchemeGroupVersion aliases the registered scheme group/version.
var workloadsv1SchemeGroupVersion = workloadsv1.SchemeGroupVersion

// WorkloadsV1Interface groups the per-resource typed clients under workloads.cast.ai/v1.
type WorkloadsV1Interface interface {
	TSCOriginals(namespace string) TSCOriginalInterface
	JVMProbeOriginals(namespace string) JVMProbeOriginalInterface
}

// WorkloadsV1Client is the concrete implementation of WorkloadsV1Interface.
type WorkloadsV1Client struct {
	restClient rest.Interface
}

// NewForConfig creates a new WorkloadsV1Client backed by the given REST config.
func NewForConfig(c *rest.Config) (*WorkloadsV1Client, error) {
	cfg := *c
	cfg.ContentConfig.GroupVersion = &groupVersion
	cfg.APIPath = "/apis"
	cfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	cfg.UserAgent = rest.DefaultKubernetesUserAgent() + " castai-guardrails-controllers/clientset/v0.0.0"
	client, err := rest.RESTClientForConfigAndClient(&cfg, nil)
	if err != nil {
		return nil, err
	}
	return &WorkloadsV1Client{restClient: client}, nil
}

// NewForConfigOrDie is like NewForConfig but panics on error.
func NewForConfigOrDie(c *rest.Config) *WorkloadsV1Client {
	client, err := NewForConfig(c)
	if err != nil {
		panic(err)
	}
	return client
}

// New creates a new WorkloadsV1Client for the given REST client.
func New(c rest.Interface) *WorkloadsV1Client {
	return &WorkloadsV1Client{restClient: c}
}

// groupVersion is the group/version this clientset targets.
var groupVersion = workloadsv1SchemeGroupVersion

func (c *WorkloadsV1Client) TSCOriginals(namespace string) TSCOriginalInterface {
	return newTSCOriginalClient(c.restClient, namespace)
}

func (c *WorkloadsV1Client) JVMProbeOriginals(namespace string) JVMProbeOriginalInterface {
	return newJVMProbeOriginalClient(c.restClient, namespace)
}
