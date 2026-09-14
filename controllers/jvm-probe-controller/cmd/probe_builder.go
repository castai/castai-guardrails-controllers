package main

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// FrameworkConfig holds the probe configuration for a specific framework
type FrameworkConfig struct {
	LivenessPath         string `yaml:"livenessPath"`
	ReadinessPath        string `yaml:"readinessPath"`
	StartupPath          string `yaml:"startupPath"`
	DefaultPort          int32  `yaml:"defaultPort"`
	InitialDelaySeconds int32  `yaml:"initialDelaySeconds"`
	PeriodSeconds        int32  `yaml:"periodSeconds"`
	TimeoutSeconds       int32  `yaml:"timeoutSeconds"`
	FailureThreshold     int32  `yaml:"failureThreshold"`
	SuccessThreshold     int32  `yaml:"successThreshold"`
	UseTCPSocket         bool   `yaml:"useTCPSocket"`
}

// DefaultFrameworkConfigs returns default framework configurations
func DefaultFrameworkConfigs() map[string]FrameworkConfig {
	return map[string]FrameworkConfig{
		FrameworkSpringBoot: {
			LivenessPath:         "/actuator/health/liveness",
			ReadinessPath:        "/actuator/health/readiness",
			StartupPath:          "/actuator/health",
			DefaultPort:          8080,
			InitialDelaySeconds:  60,
			PeriodSeconds:        10,
			TimeoutSeconds:       5,
			FailureThreshold:     3,
			SuccessThreshold:     1,
			UseTCPSocket:         false,
		},
		FrameworkQuarkus: {
			LivenessPath:         "/q/health/live",
			ReadinessPath:        "/q/health/ready",
			StartupPath:          "/q/health/started",
			DefaultPort:          8080,
			InitialDelaySeconds:  30,
			PeriodSeconds:        10,
			TimeoutSeconds:       5,
			FailureThreshold:     3,
			SuccessThreshold:     1,
			UseTCPSocket:         false,
		},
		FrameworkMicronaut: {
			LivenessPath:         "/health/liveness",
			ReadinessPath:        "/health/readiness",
			StartupPath:          "/health",
			DefaultPort:          8080,
			InitialDelaySeconds:  30,
			PeriodSeconds:        10,
			TimeoutSeconds:       5,
			FailureThreshold:     3,
			SuccessThreshold:     1,
			UseTCPSocket:         false,
		},
		FrameworkGeneric: {
			LivenessPath:         "",
			ReadinessPath:        "",
			StartupPath:          "",
			DefaultPort:          8080,
			InitialDelaySeconds:  30,
			PeriodSeconds:        10,
			TimeoutSeconds:       5,
			FailureThreshold:     3,
			SuccessThreshold:     1,
			UseTCPSocket:         true,
		},
	}
}

// DefaultJVMConfig returns the default JVM configuration
func DefaultJVMConfig() JVMConfig {
	return JVMConfig{
		Frameworks:             DefaultFrameworkConfigs(),
		LogInterval:            "15m",
		ReconcileInterval:      "2m",
		RequireBothProbes:      true,
		SkipIfAnyProbeExists:   true,
		// P1: Liveness opt-in (safer default)
		InjectLivenessProbe:    false,
		InjectReadinessProbe:   true,
		InjectStartupProbe:     true,
		LogIntendedChanges:     true,
		// PR3 defaults
		ManagementEnabled:      true,
		RollbackOnDisable:      false,
		Mode:                   ModeApply,
		SnapshotEnabled:        true,
		OperatorNamespace:      "castai-agent",
		Version:                "dev",
	}
}

// BuildProbesForFramework generates probes based on framework and container info
func BuildProbesForFramework(framework string, containerInfo ContainerInfo, annotations map[string]string, config *JVMConfig) (liveness *corev1.Probe, readiness *corev1.Probe, startup *corev1.Probe) {
	// Get framework config
	frameworkConfig, ok := config.Frameworks[framework]
	if !ok {
		frameworkConfig = config.Frameworks[FrameworkGeneric]
	}

	// Allow annotation overrides
	port := getAnnotationInt(annotations, AnnotationJVMProbePort, containerInfo.Port)
	initialDelay := getAnnotationInt(annotations, AnnotationJVMProbeInitialDelay, frameworkConfig.InitialDelaySeconds)
	period := getAnnotationInt(annotations, AnnotationJVMProbePeriod, frameworkConfig.PeriodSeconds)
	timeout := getAnnotationInt(annotations, AnnotationJVMProbeTimeout, frameworkConfig.TimeoutSeconds)
	failureThreshold := getAnnotationInt(annotations, AnnotationJVMProbeFailureThreshold, frameworkConfig.FailureThreshold)
	successThreshold := getAnnotationInt(annotations, AnnotationJVMProbeSuccessThreshold, frameworkConfig.SuccessThreshold)

	// Check if TCP socket should be used (generic JVM fallback)
	useTCP := frameworkConfig.UseTCPSocket
	if framework == FrameworkGeneric {
		useTCP = true
	}

	// Check for custom paths in annotations
	livenessPath := getAnnotation(annotations, AnnotationJVMProbeLivenessPath, frameworkConfig.LivenessPath)
	readinessPath := getAnnotation(annotations, AnnotationJVMProbeReadinessPath, frameworkConfig.ReadinessPath)
	startupPath := getAnnotation(annotations, AnnotationJVMProbeStartupPath, frameworkConfig.StartupPath)

	// P1: Check config flags for probe injection (opt-in liveness, opt-out readiness/startup)
	injectLiveness := getAnnotationBool(annotations, AnnotationJVMInjectLiveness, config.InjectLivenessProbe)
	injectReadiness := getAnnotationBool(annotations, AnnotationJVMInjectReadiness, config.InjectReadinessProbe)
	injectStartup := getAnnotationBool(annotations, AnnotationJVMInjectStartup, config.InjectStartupProbe)

	// Build liveness probe (only if enabled)
	if injectLiveness {
		if useTCP {
			liveness = buildTCPProbe(port, initialDelay, period, timeout, failureThreshold)
		} else if livenessPath != "" {
			liveness = buildHTTPGetProbe(port, livenessPath, initialDelay, period, timeout, failureThreshold, successThreshold)
		}
	}

	// Build readiness probe (only if enabled)
	if injectReadiness {
		if useTCP {
			readiness = buildTCPProbe(port, initialDelay, period, timeout, failureThreshold)
		} else if readinessPath != "" {
			readiness = buildHTTPGetProbe(port, readinessPath, initialDelay, period, timeout, failureThreshold, successThreshold)
		}
	}

	// Build startup probe (only if enabled)
	if injectStartup {
		if useTCP {
			startup = buildTCPProbe(port, initialDelay*3, period, timeout, failureThreshold*2)
		} else if startupPath != "" {
			startup = buildHTTPGetProbe(port, startupPath, initialDelay, period, timeout, failureThreshold*2, successThreshold)
		}
	}

	return liveness, readiness, startup
}

// buildHTTPGetProbe creates an HTTP GET probe
func buildHTTPGetProbe(port int32, path string, initialDelay, period, timeout, failureThreshold, successThreshold int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   path,
				Port:   intstr.FromInt(int(port)),
				Scheme: corev1.URISchemeHTTP,
			},
		},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
		TimeoutSeconds:      timeout,
		FailureThreshold:    failureThreshold,
		SuccessThreshold:    successThreshold,
	}
}

// buildTCPProbe creates a TCP socket probe
func buildTCPProbe(port int32, initialDelay, period, timeout, failureThreshold int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt(int(port)),
			},
		},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
		TimeoutSeconds:      timeout,
		FailureThreshold:    failureThreshold,
		SuccessThreshold:    1,
	}
}

// getAnnotation retrieves an annotation value with a fallback
func getAnnotation(annotations map[string]string, key, fallback string) string {
	if val, ok := annotations[key]; ok && val != "" {
		return val
	}
	return fallback
}

// getAnnotationInt retrieves an annotation value as int with a fallback
func getAnnotationInt(annotations map[string]string, key string, fallback int32) int32 {
	if val, ok := annotations[key]; ok && val != "" {
		if intVal, err := strconv.Atoi(val); err == nil {
			return int32(intVal)
		}
	}
	return fallback
}

// getAnnotationBool retrieves an annotation value as bool with a fallback
func getAnnotationBool(annotations map[string]string, key string, fallback bool) bool {
	if val, ok := annotations[key]; ok && val != "" {
		return val == "true" || val == "yes" || val == "1"
	}
	return fallback
}

// CloneProbe returns a deep copy of the given probe, or nil if the input is nil.
// The webhook mutation function uses this to copy an existing liveness/readiness
// probe into a startup probe without aliasing the source struct.
func CloneProbe(probe *corev1.Probe) *corev1.Probe {
	if probe == nil {
		return nil
	}
	return probe.DeepCopy()
}

// Annotations for probe injection tracking
const (
	AnnotationPrefix                      = "workloads.cast.ai/jvm-probe-"
	AnnotationJVMBypass                   = AnnotationPrefix + "bypass"
	AnnotationJVMFramework                = AnnotationPrefix + "framework"
	AnnotationJVMProbePort                = AnnotationPrefix + "port"
	AnnotationJVMProbeLivenessPath        = AnnotationPrefix + "liveness-path"
	AnnotationJVMProbeReadinessPath       = AnnotationPrefix + "readiness-path"
	AnnotationJVMProbeStartupPath         = AnnotationPrefix + "startup-path"
	AnnotationJVMProbeInitialDelay        = AnnotationPrefix + "initial-delay"
	AnnotationJVMProbePeriod              = AnnotationPrefix + "period"
	AnnotationJVMProbeTimeout             = AnnotationPrefix + "timeout"
	AnnotationJVMProbeFailureThreshold    = AnnotationPrefix + "failure-threshold"
	AnnotationJVMProbeSuccessThreshold    = AnnotationPrefix + "success-threshold"
	AnnotationJVMProbeManaged             = AnnotationPrefix + "managed"
)

// GetProbeAnnotations returns annotations to mark probes as managed
func GetProbeAnnotations() map[string]string {
	return map[string]string{
		AnnotationJVMProbeManaged: "true",
	}
}

// IsBypassAnnotation returns true if the workload has bypass annotation
func IsBypassAnnotation(annotations map[string]string) bool {
	val, ok := annotations[AnnotationJVMBypass]
	return ok && strings.ToLower(val) == "true"
}

// GetFrameworkOverride returns the framework override from annotations
func GetFrameworkOverride(annotations map[string]string) string {
	if val, ok := annotations[AnnotationJVMFramework]; ok && val != "" {
		return val
	}
	return ""
}