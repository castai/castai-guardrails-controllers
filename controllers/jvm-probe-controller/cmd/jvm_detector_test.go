// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestDetectContainerPort_WellKnownNamedPort verifies that a port with a
// well-known name (http/web/https/http-web) is selected and the returned
// port name matches the declared one. Resolution must look at name first.
func TestDetectContainerPort_WellKnownNamedPort(t *testing.T) {
	cases := []struct {
		name     string
		portName string
		wantName string
	}{
		{"http", "http", "http"},
		{"web", "web", "web"},
		{"https", "https", "https"},
		{"http-web", "http-web", "http-web"},
		// Mixed case: detector lowercases for comparison but the
		// returned name must be the original, not normalized.
		{"HTTP-mixed-case", "HTTP", "HTTP"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			container := corev1.Container{
				Ports: []corev1.ContainerPort{
					// Put an unrelated numeric port first to confirm
					// named-port matching doesn't depend on order.
					{Name: "metrics", ContainerPort: 9000},
					{Name: tc.portName, ContainerPort: 8080},
				},
			}

			port, name := detectContainerPort(container)
			if port != 8080 {
				t.Errorf("port = %d, want 8080", port)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
		})
	}
}

// TestDetectContainerPort_CommonJVMNumericPort verifies that a port on
// one of the common JVM numeric values returns the numeric value with an
// empty name, even when the port has been declared with a non-well-known
// name. This preserves the existing "common port" detection semantics.
func TestDetectContainerPort_CommonJVMNumericPort(t *testing.T) {
	cases := []struct {
		name     string
		port     int32
		declName string
	}{
		{"8080-named-app", 8080, "app"},
		{"8443-named-app", 8443, "app"},
		{"9090-named-app", 9090, "app"},
		{"8888-named-app", 8888, "app"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			container := corev1.Container{
				Ports: []corev1.ContainerPort{
					{Name: tc.declName, ContainerPort: tc.port},
				},
			}

			port, name := detectContainerPort(container)
			if port != tc.port {
				t.Errorf("port = %d, want %d", port, tc.port)
			}
			if name != "" {
				t.Errorf("name = %q, want \"\" (common port should not surface arbitrary names)", name)
			}
		})
	}
}

// TestDetectContainerPort_FallbackToFirstPort verifies that when no
// well-known name and no common numeric port is found, the first
// declared port is used and its name is surfaced.
func TestDetectContainerPort_FallbackToFirstPort(t *testing.T) {
	t.Run("first-port-with-name", func(t *testing.T) {
		container := corev1.Container{
			Ports: []corev1.ContainerPort{
				{Name: "grpc", ContainerPort: 9001},
				{Name: "metrics", ContainerPort: 9000},
			},
		}

		port, name := detectContainerPort(container)
		if port != 9001 {
			t.Errorf("port = %d, want 9001", port)
		}
		if name != "grpc" {
			t.Errorf("name = %q, want \"grpc\"", name)
		}
	})

	t.Run("first-port-without-name", func(t *testing.T) {
		container := corev1.Container{
			Ports: []corev1.ContainerPort{
				{ContainerPort: 9001},
				{ContainerPort: 9000},
			},
		}

		port, name := detectContainerPort(container)
		if port != 9001 {
			t.Errorf("port = %d, want 9001", port)
		}
		if name != "" {
			t.Errorf("name = %q, want \"\"", name)
		}
	})
}

// TestDetectContainerPort_NoPorts verifies the ultimate fallback to 8080.
func TestDetectContainerPort_NoPorts(t *testing.T) {
	container := corev1.Container{}

	port, name := detectContainerPort(container)
	if port != 8080 {
		t.Errorf("port = %d, want 8080", port)
	}
	if name != "" {
		t.Errorf("name = %q, want \"\"", name)
	}
}

// TestDetectJVMContainer_PopulatesPortName verifies that DetectJVMContainer
// forwards the portName discovered by detectContainerPort into the
// returned ContainerInfo struct.
func TestDetectJVMContainer_PopulatesPortName(t *testing.T) {
	container := corev1.Container{
		Name:  "app",
		Image: "eclipse-temurin:17",
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: 8080},
		},
	}

	info := DetectJVMContainer(container)
	if !info.IsJVM {
		t.Fatalf("expected IsJVM=true for temurin image")
	}
	if info.Port != 8080 {
		t.Errorf("info.Port = %d, want 8080", info.Port)
	}
	if info.PortName != "http" {
		t.Errorf("info.PortName = %q, want \"http\"", info.PortName)
	}
}

// TestBuildProbesForFramework_NamedPortProducesStringPort verifies that a
// container with a well-known named port produces probes that reference
// the port by name (intstr.String).
func TestBuildProbesForFramework_NamedPortProducesStringPort(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.InjectLivenessProbe = true
	ci := ContainerInfo{
		Port:     8080,
		PortName: "http",
		Ports:    []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
	}
	annotations := map[string]string{}

	liveness, readiness, startup := BuildProbesForFramework(FrameworkSpringBoot, ci, annotations, &cfg)

	if liveness == nil || liveness.HTTPGet == nil {
		t.Fatalf("expected liveness HTTPGet probe, got nil")
	}
	assertNamedPort(t, liveness.HTTPGet.Port, "http", "liveness")

	if readiness == nil || readiness.HTTPGet == nil {
		t.Fatalf("expected readiness HTTPGet probe, got nil")
	}
	assertNamedPort(t, readiness.HTTPGet.Port, "http", "readiness")

	if startup == nil || startup.HTTPGet == nil {
		t.Fatalf("expected startup HTTPGet probe, got nil")
	}
	assertNamedPort(t, startup.HTTPGet.Port, "http", "startup")
}

// TestBuildProbesForFramework_CommonPortProducesNumericPort verifies that
// when ContainerInfo.PortName is empty (e.g. a common JVM port with a
// non-well-known name) probes fall back to the numeric port.
func TestBuildProbesForFramework_CommonPortProducesNumericPort(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.InjectLivenessProbe = true
	ci := ContainerInfo{
		Port:     8080,
		PortName: "",
		Ports:    []corev1.ContainerPort{{Name: "app", ContainerPort: 8080}},
	}
	annotations := map[string]string{}

	liveness, readiness, startup := BuildProbesForFramework(FrameworkSpringBoot, ci, annotations, &cfg)

	if liveness == nil || liveness.HTTPGet == nil {
		t.Fatalf("expected liveness HTTPGet probe, got nil")
	}
	assertNumericPort(t, liveness.HTTPGet.Port, 8080, "liveness")

	if readiness == nil || readiness.HTTPGet == nil {
		t.Fatalf("expected readiness HTTPGet probe, got nil")
	}
	assertNumericPort(t, readiness.HTTPGet.Port, 8080, "readiness")

	if startup == nil || startup.HTTPGet == nil {
		t.Fatalf("expected startup HTTPGet probe, got nil")
	}
	assertNumericPort(t, startup.HTTPGet.Port, 8080, "startup")
}

// TestBuildProbesForFramework_AnnotationOverrideWinsOverNamedPort
// verifies that AnnotationJVMProbePort forces a numeric port even when a
// named port (e.g. "http") was detected on the container.
func TestBuildProbesForFramework_AnnotationOverrideWinsOverNamedPort(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.InjectLivenessProbe = true
	ci := ContainerInfo{
		Port:     8080,
		PortName: "http",
		Ports:    []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
	}
	annotations := map[string]string{
		AnnotationJVMProbePort: "9090",
	}

	liveness, readiness, startup := BuildProbesForFramework(FrameworkSpringBoot, ci, annotations, &cfg)

	if liveness == nil || liveness.HTTPGet == nil {
		t.Fatalf("expected liveness HTTPGet probe, got nil")
	}
	assertNumericPort(t, liveness.HTTPGet.Port, 9090, "liveness")

	if readiness == nil || readiness.HTTPGet == nil {
		t.Fatalf("expected readiness HTTPGet probe, got nil")
	}
	assertNumericPort(t, readiness.HTTPGet.Port, 9090, "readiness")

	if startup == nil || startup.HTTPGet == nil {
		t.Fatalf("expected startup HTTPGet probe, got nil")
	}
	assertNumericPort(t, startup.HTTPGet.Port, 9090, "startup")
}

// TestBuildProbesForFramework_NoNamedPortProducesNumericPort verifies that
// without any named port, probes fall back to the numeric port.
func TestBuildProbesForFramework_NoNamedPortProducesNumericPort(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.InjectLivenessProbe = true
	ci := ContainerInfo{
		Port:     9000,
		PortName: "",
		Ports:    []corev1.ContainerPort{{ContainerPort: 9000}},
	}
	annotations := map[string]string{}

	liveness, _, _ := BuildProbesForFramework(FrameworkSpringBoot, ci, annotations, &cfg)
	if liveness == nil || liveness.HTTPGet == nil {
		t.Fatalf("expected liveness HTTPGet probe, got nil")
	}
	assertNumericPort(t, liveness.HTTPGet.Port, 9000, "liveness")
}

// TestBuildProbesForFramework_TCPProbeAlsoUsesNamedPort verifies the
// named-port behaviour is wired into the TCP probe variant (used for the
// generic framework).
func TestBuildProbesForFramework_TCPProbeAlsoUsesNamedPort(t *testing.T) {
	cfg := DefaultJVMConfig()
	ci := ContainerInfo{
		Port:     8080,
		PortName: "http",
	}
	annotations := map[string]string{}

	_, readiness, _ := BuildProbesForFramework(FrameworkGeneric, ci, annotations, &cfg)
	if readiness == nil || readiness.TCPSocket == nil {
		t.Fatalf("expected readiness TCPSocket probe, got nil")
	}
	assertNamedPort(t, readiness.TCPSocket.Port, "http", "readiness")
}

// TestBuildProbesForFramework_TCPProbeHonoursAnnotationOverride verifies
// the annotation override beats named-port selection for TCP probes too.
func TestBuildProbesForFramework_TCPProbeHonoursAnnotationOverride(t *testing.T) {
	cfg := DefaultJVMConfig()
	ci := ContainerInfo{
		Port:     8080,
		PortName: "http",
	}
	annotations := map[string]string{
		AnnotationJVMProbePort: "9090",
	}

	_, readiness, _ := BuildProbesForFramework(FrameworkGeneric, ci, annotations, &cfg)
	if readiness == nil || readiness.TCPSocket == nil {
		t.Fatalf("expected readiness TCPSocket probe, got nil")
	}
	assertNumericPort(t, readiness.TCPSocket.Port, 9090, "readiness")
}

// assertNamedPort fails the test if port is not the intstr.String form
// expected. probeLabel identifies the probe under test for clearer errors.
func assertNamedPort(t *testing.T, port intstr.IntOrString, want, probeLabel string) {
	t.Helper()
	if port.Type != intstr.String {
		t.Fatalf("%s port Type = %d, want intstr.String (%d)", probeLabel, port.Type, intstr.String)
	}
	if port.StrVal != want {
		t.Errorf("%s port StrVal = %q, want %q", probeLabel, port.StrVal, want)
	}
	if port.IntVal != 0 {
		t.Errorf("%s port IntVal = %d, want 0 for a named port", probeLabel, port.IntVal)
	}
}

// assertNumericPort fails the test if port is not the intstr.Int form
// expected. probeLabel identifies the probe under test for clearer errors.
func assertNumericPort(t *testing.T, port intstr.IntOrString, want int32, probeLabel string) {
	t.Helper()
	if port.Type != intstr.Int {
		t.Fatalf("%s port Type = %d, want intstr.Int (%d)", probeLabel, port.Type, intstr.Int)
	}
	if port.IntVal != want {
		t.Errorf("%s port IntVal = %d, want %d", probeLabel, port.IntVal, want)
	}
	if port.StrVal != "" {
		t.Errorf("%s port StrVal = %q, want \"\" for a numeric port", probeLabel, port.StrVal)
	}
}

// TestBuildProbesForFramework_NoDeclaredPortsUsesTCP verifies that a JVM
// container with no declared ports receives tcpSocket probes, even for a
// framework whose default config (spring-boot) would otherwise emit
// httpGet probes on /actuator/health. Without this fallback the kubelet
// would call an HTTP endpoint that has no listener and the pod would
// restart forever.
func TestBuildProbesForFramework_NoDeclaredPortsUsesTCP(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.InjectLivenessProbe = true
	cfg.InjectReadinessProbe = true
	cfg.InjectStartupProbe = true
	// Spring Boot defaults use httpGet, so we can demonstrate that the
	// no-ports fallback overrides the framework preference.
	ci := ContainerInfo{
		Port:     8080,
		PortName: "",
		// Ports intentionally empty: BuildProbesForFramework checks
		// len(containerInfo.Ports) directly.
	}
	annotations := map[string]string{}

	liveness, readiness, startup := BuildProbesForFramework(FrameworkSpringBoot, ci, annotations, &cfg)

	if liveness == nil || liveness.TCPSocket == nil {
		t.Fatalf("expected liveness TCPSocket probe, got %+v", liveness)
	}
	if liveness.HTTPGet != nil {
		t.Errorf("did not expect HTTPGet on liveness when no ports declared, got %+v", liveness.HTTPGet)
	}

	if readiness == nil || readiness.TCPSocket == nil {
		t.Fatalf("expected readiness TCPSocket probe, got %+v", readiness)
	}
	if readiness.HTTPGet != nil {
		t.Errorf("did not expect HTTPGet on readiness when no ports declared, got %+v", readiness.HTTPGet)
	}

	if startup == nil || startup.TCPSocket == nil {
		t.Fatalf("expected startup TCPSocket probe, got %+v", startup)
	}
	if startup.HTTPGet != nil {
		t.Errorf("did not expect HTTPGet on startup when no ports declared, got %+v", startup.HTTPGet)
	}
}

// TestBuildProbesForFramework_DeclaredPortKeepsHTTPPath verifies that a
// JVM container that declares a port (e.g. spring-boot on 8080) keeps
// the framework's HTTP probes. The no-ports fallback must not suppress
// HTTP probes for containers that explicitly declared ports.
func TestBuildProbesForFramework_DeclaredPortKeepsHTTPPath(t *testing.T) {
	cfg := DefaultJVMConfig()
	cfg.InjectLivenessProbe = true
	cfg.InjectReadinessProbe = true
	cfg.InjectStartupProbe = true
	ci := ContainerInfo{
		Port:     8080,
		PortName: "",
		Ports:    []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
	}
	annotations := map[string]string{}

	liveness, readiness, startup := BuildProbesForFramework(FrameworkSpringBoot, ci, annotations, &cfg)

	if liveness == nil || liveness.HTTPGet == nil {
		t.Fatalf("expected liveness HTTPGet probe, got %+v", liveness)
	}
	if liveness.TCPSocket != nil {
		t.Errorf("did not expect TCPSocket on liveness when ports declared, got %+v", liveness.TCPSocket)
	}
	if liveness.HTTPGet.Path != "/actuator/health/liveness" {
		t.Errorf("liveness path = %q, want /actuator/health/liveness", liveness.HTTPGet.Path)
	}

	if readiness == nil || readiness.HTTPGet == nil {
		t.Fatalf("expected readiness HTTPGet probe, got %+v", readiness)
	}
	if readiness.HTTPGet.Path != "/actuator/health/readiness" {
		t.Errorf("readiness path = %q, want /actuator/health/readiness", readiness.HTTPGet.Path)
	}

	if startup == nil || startup.HTTPGet == nil {
		t.Fatalf("expected startup HTTPGet probe, got %+v", startup)
	}
	if startup.HTTPGet.Path != "/actuator/health" {
		t.Errorf("startup path = %q, want /actuator/health", startup.HTTPGet.Path)
	}
}
