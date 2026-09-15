// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

// admission_test.go is a small, visible end-to-end integration test for
// the JVM Probe Controller webhook. It complements webhook_test.go and
// pod_mutation_test.go — both of which can be truncated out of a GitHub
// diff — by exercising the full HTTP admission cycle through the real
// WebhookServer (TLS + mux + instrument wrapper) and the /metrics endpoint.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// admissionJVMpod is the canonical JVM workload: Spring Boot image with a
// liveness probe, so the mutation algorithm must inject a startup probe.
func admissionJVMpod() *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "spring-boot-app:1.0",
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{Path: "/actuator/health/liveness", Port: intstr.FromInt(8080)},
					},
				},
			}},
		},
	}
}

func admissionReview(uid string, pod *corev1.Pod) string {
	raw, _ := json.Marshal(pod)
	b, _ := json.Marshal(&admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:    types.UID(uid),
			Object: runtime.RawExtension{Raw: raw},
		},
	})
	return string(b)
}

// startAdmissionServer boots the production WebhookServer on an
// ephemeral TLS port using the real handlePodAdmission. It registers
// t.Cleanup to restore the global config and shut the server down.
func startAdmissionServer(t *testing.T) (*http.Client, string) {
	t.Helper()
	certFile, keyFile, pool := writeSelfSignedCert(t)

	saved := config
	cfg := DefaultJVMConfig()
	config = &cfg
	t.Cleanup(func() { config = saved })

	srv := NewWebhookServer("127.0.0.1:0", certFile, keyFile, handlePodAdmission)
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-serveErr })

	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}, //nolint:gosec
		"https://" + srv.Addr()
}

func postAdmission(t *testing.T, client *http.Client, url, body string) admissionv1.AdmissionReview {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s status = %d, body = %s", url, resp.StatusCode, raw)
	}
	var got admissionv1.AdmissionReview
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return got
}

// TestAdmission_JVMPodReturnsPatch: a JVM Pod with a liveness probe is
// allowed and receives a non-empty JSON Patch.
func TestAdmission_JVMPodReturnsPatch(t *testing.T) {
	client, base := startAdmissionServer(t)
	got := postAdmission(t, client, base+"/mutate/pods", admissionReview("jvm", admissionJVMpod()))
	if got.Response == nil || !got.Response.Allowed {
		t.Fatalf("Allowed = false: %+v", got.Response)
	}
	if len(got.Response.Patch) == 0 {
		t.Errorf("expected non-empty patch for JVM Pod with liveness probe")
	}
}

// TestAdmission_NonJVMPodNoPatch: a non-JVM workload is admitted without a patch.
func TestAdmission_NonJVMPodNoPatch(t *testing.T) {
	client, base := startAdmissionServer(t)
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "nginx", Image: "nginx:1.25"}}}}
	got := postAdmission(t, client, base+"/mutate/pods", admissionReview("non-jvm", pod))
	if got.Response == nil || !got.Response.Allowed {
		t.Fatalf("Allowed = false: %+v", got.Response)
	}
	if len(got.Response.Patch) != 0 {
		t.Errorf("expected no patch for non-JVM Pod, got %d bytes", len(got.Response.Patch))
	}
}

// TestAdmission_BypassAnnotationNoPatch: a JVM Pod carrying the bypass
// annotation is admitted without a patch.
func TestAdmission_BypassAnnotationNoPatch(t *testing.T) {
	client, base := startAdmissionServer(t)
	pod := admissionJVMpod()
	pod.Annotations = map[string]string{AnnotationJVMBypass: "true"}
	got := postAdmission(t, client, base+"/mutate/pods", admissionReview("bypass", pod))
	if got.Response == nil || !got.Response.Allowed {
		t.Fatalf("Allowed = false: %+v", got.Response)
	}
	if len(got.Response.Patch) != 0 {
		t.Errorf("expected no patch with bypass annotation, got %d bytes", len(got.Response.Patch))
	}
}

// TestAdmission_MalformedBodyIsAllowed: failurePolicy=Ignore — a
// malformed body still returns 200 and Allowed=true.
func TestAdmission_MalformedBodyIsAllowed(t *testing.T) {
	client, base := startAdmissionServer(t)
	got := postAdmission(t, client, base+"/mutate/pods", "{not valid json")
	if got.Response == nil || !got.Response.Allowed {
		t.Errorf("Allowed = false on malformed body, want true (failurePolicy=Ignore): %+v", got.Response)
	}
}

// TestMetrics_ExposesCountersAfterTraffic sends one request of each
// kind and verifies /metrics surfaces non-zero values for all four
// counters (requests, mutations, bypasses, errors).
func TestMetrics_ExposesCountersAfterTraffic(t *testing.T) {
	client, base := startAdmissionServer(t)

	nonJVM := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "nginx", Image: "nginx:1.25"}}}}
	bypass := admissionJVMpod()
	bypass.Annotations = map[string]string{AnnotationJVMBypass: "true"}

	for _, body := range []string{
		admissionReview("jvm", admissionJVMpod()),
		admissionReview("non-jvm", nonJVM),
		admissionReview("bypass", bypass),
		"{not valid json",
	} {
		postAdmission(t, client, base+"/mutate/pods", body)
	}

	resp, err := client.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}
	text, _ := io.ReadAll(resp.Body)

	for _, want := range []string{
		"jvm_probe_admission_requests_total 4",
		"jvm_probe_mutations_total 1",
		"jvm_probe_bypasses_total 1",
		"jvm_probe_errors_total 1",
	} {
		if !strings.Contains(string(text), want) {
			t.Errorf("/metrics missing %q\n--- body ---\n%s", want, text)
		}
	}
}
