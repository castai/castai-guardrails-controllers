// Package main provides annotations for the JVM Probe Controller
package main

import "strconv"

// ProbeOverwrite annotations for forcing probe updates
const (
	// AnnotationOverwriteAll forces overwrite of all probes
	AnnotationOverwriteAll = AnnotationPrefix + "overwrite-all"

	// AnnotationOverwriteLiveness forces overwrite of liveness probe
	AnnotationOverwriteLiveness = AnnotationPrefix + "overwrite-liveness"

	// AnnotationOverwriteReadiness forces overwrite of readiness probe
	AnnotationOverwriteReadiness = AnnotationPrefix + "overwrite-readiness"

	// AnnotationOverwriteStartup forces overwrite of startup probe
	AnnotationOverwriteStartup = AnnotationPrefix + "overwrite-startup"

	// AnnotationLogFailures enables detailed failure logging
	AnnotationLogFailures = AnnotationPrefix + "log-failures"

	// AnnotationFailureCountThreshold threshold before logging failures
	AnnotationFailureCountThreshold = AnnotationPrefix + "failure-log-threshold"

	// P1: Probe injection control annotations
	AnnotationJVMInjectLiveness  = AnnotationPrefix + "inject-liveness"
	AnnotationJVMInjectReadiness = AnnotationPrefix + "inject-readiness"
	AnnotationJVMInjectStartup   = AnnotationPrefix + "inject-startup"

	// Chunk 2: Pod-mutation-only annotations.
	// AnnotationJVMProbeStartupPeriod overrides periodSeconds for the
	// injected startup probe.
	AnnotationJVMProbeStartupPeriod = AnnotationPrefix + "startup-period"

	// AnnotationJVMProbeStartupFailureThreshold overrides failureThreshold
	// for the injected startup probe.
	AnnotationJVMProbeStartupFailureThreshold = AnnotationPrefix + "startup-failure-threshold"

	// AnnotationJVMProbeClearDelays removes initialDelaySeconds from the
	// existing liveness/readiness probes when set to a truthy value.
	AnnotationJVMProbeClearDelays = AnnotationPrefix + "clear-delays"

	// AnnotationJVMProbeAlign opts the Pod in to automatic probe alignment
	// for the lifetime of this Pod. It overrides the config-level
	// AlignProbes default on a per-Pod basis.
	AnnotationJVMProbeAlign = AnnotationPrefix + "align"
)

// ShouldOverwriteAll checks if all probes should be overwritten
func ShouldOverwriteAll(annotations map[string]string) bool {
	val, ok := annotations[AnnotationOverwriteAll]
	return ok && (val == "true" || val == "yes" || val == "1")
}

// ShouldOverwriteLiveness checks if liveness probe should be overwritten
func ShouldOverwriteLiveness(annotations map[string]string) bool {
	if ShouldOverwriteAll(annotations) {
		return true
	}
	val, ok := annotations[AnnotationOverwriteLiveness]
	return ok && (val == "true" || val == "yes" || val == "1")
}

// ShouldOverwriteReadiness checks if readiness probe should be overwritten
func ShouldOverwriteReadiness(annotations map[string]string) bool {
	if ShouldOverwriteAll(annotations) {
		return true
	}
	val, ok := annotations[AnnotationOverwriteReadiness]
	return ok && (val == "true" || val == "yes" || val == "1")
}

// ShouldOverwriteStartup checks if startup probe should be overwritten
func ShouldOverwriteStartup(annotations map[string]string) bool {
	if ShouldOverwriteAll(annotations) {
		return true
	}
	val, ok := annotations[AnnotationOverwriteStartup]
	return ok && (val == "true" || val == "yes" || val == "1")
}

// ShouldLogFailures checks if failure logging is enabled
func ShouldLogFailures(annotations map[string]string) bool {
	val, ok := annotations[AnnotationLogFailures]
	return ok && (val == "true" || val == "yes" || val == "1")
}

// GetFailureLogThreshold returns the failure threshold for logging
func GetFailureLogThreshold(annotations map[string]string, defaultVal int) int {
	val, ok := annotations[AnnotationFailureCountThreshold]
	if !ok {
		return defaultVal
	}

	intVal, err := strconv.Atoi(val)
	if err != nil {
		return defaultVal
	}
	return intVal
}

// ShouldInjectLiveness checks if liveness probe should be injected
func ShouldInjectLiveness(annotations map[string]string, configDefault bool) bool {
	val, ok := annotations[AnnotationJVMInjectLiveness]
	if !ok {
		return configDefault
	}
	return val == "true" || val == "yes" || val == "1"
}

// ShouldInjectReadiness checks if readiness probe should be injected
func ShouldInjectReadiness(annotations map[string]string, configDefault bool) bool {
	val, ok := annotations[AnnotationJVMInjectReadiness]
	if !ok {
		return configDefault
	}
	return val == "true" || val == "yes" || val == "1"
}

// ShouldInjectStartup checks if startup probe should be injected
func ShouldInjectStartup(annotations map[string]string, configDefault bool) bool {
	val, ok := annotations[AnnotationJVMInjectStartup]
	if !ok {
		return configDefault
	}
	return val == "true" || val == "yes" || val == "1"
}

// ShouldClearDelays reports whether the Pod annotation requests removal of
// initialDelaySeconds from existing liveness/readiness probes.
func ShouldClearDelays(annotations map[string]string) bool {
	val, ok := annotations[AnnotationJVMProbeClearDelays]
	return ok && (val == "true" || val == "yes" || val == "1")
}

// ShouldAlignProbes reports whether automatic probe alignment is enabled for
// this Pod. The Pod annotation overrides the config-level default. Any
// truthy value (true/yes/1) enables alignment; any other value disables it.
func ShouldAlignProbes(annotations map[string]string, configDefault bool) bool {
	val, ok := annotations[AnnotationJVMProbeAlign]
	if !ok {
		return configDefault
	}
	return val == "true" || val == "yes" || val == "1"
}
