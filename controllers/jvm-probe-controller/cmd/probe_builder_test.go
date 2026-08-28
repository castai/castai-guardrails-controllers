// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestStartupProbeInjectedWhenMissing verifies that JVM workloads missing a
// startup probe have one injected by the controller for every supported
// framework.
func TestStartupProbeInjectedWhenMissing(t *testing.T) {
	// Container with liveness + readiness probes but NO startup probe.
	container := corev1.Container{
		Name: "app",
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt(8080),
				},
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/ready",
					Port: intstr.FromInt(8080),
				},
			},
		},
		StartupProbe: nil,
	}

	// NeedsProbes must report needsStartup=true regardless of requireBoth
	// (startup probe injection is independent of the other probes).
	for _, requireBoth := range []bool{true, false} {
		_, _, needsStartup := NeedsProbes(container, requireBoth)
		if !needsStartup {
			t.Fatalf("NeedsProbes(requireBoth=%v) needsStartup = false, want true", requireBoth)
		}
	}

	cfg := DefaultJVMConfig()
	info := ContainerInfo{
		Name:      container.Name,
		Image:     "eclipse-temurin:17",
		IsJVM:     true,
		Framework: FrameworkSpringBoot,
		Port:      8080,
	}

	frameworks := []string{
		FrameworkSpringBoot,
		FrameworkQuarkus,
		FrameworkMicronaut,
		FrameworkGeneric,
	}

	for _, fw := range frameworks {
		_, _, startup := BuildProbesForFramework(fw, info, nil, &cfg)
		if startup == nil {
			t.Errorf("BuildProbesForFramework(%q) startup = nil, want non-nil", fw)
		}
	}

	// CreateProbePatch must emit an "add" op for the startup probe path.
	_, _, startup := BuildProbesForFramework(FrameworkSpringBoot, info, nil, &cfg)
	patch := CreateProbePatch(0, nil, nil, startup)
	if len(patch) != 1 {
		t.Fatalf("CreateProbePatch returned %d ops, want 1 (startup only)", len(patch))
	}
	op, ok := patch[0]["op"].(string)
	if !ok || op != "add" {
		t.Errorf("patch op = %v, want \"add\"", patch[0]["op"])
	}
	if path, _ := patch[0]["path"].(string); path != "/spec/template/spec/containers/0/startupProbe" {
		t.Errorf("patch path = %q, want %q", path, "/spec/template/spec/containers/0/startupProbe")
	}
}
