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
