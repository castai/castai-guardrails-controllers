// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package scheme

import (
	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// Codecs provides access to encoding and decoding for the scheme.
var Codecs = serializer.NewCodecFactory(Scheme)

// ParameterCodec is used by the typed REST clients for query-string encoding.
var ParameterCodec = runtime.NewParameterCodec(Scheme)

// Scheme is the runtime scheme shared by all clients.
var Scheme = runtime.NewScheme()

func init() {
	if err := workloadsv1.AddToScheme(Scheme); err != nil {
		panic(err)
	}
}
