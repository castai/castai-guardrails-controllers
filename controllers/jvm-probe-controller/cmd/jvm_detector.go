package main

import (
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// P1: Word-boundary regex patterns for JVM image detection
var jvmImagePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bjava\b`),
	regexp.MustCompile(`\bjdk\b`),
	regexp.MustCompile(`\bjre\b`),
	regexp.MustCompile(`\bopenjdk\b`),
	regexp.MustCompile(`\btemurin\b`),
	regexp.MustCompile(`\bcorretto\b`),
	regexp.MustCompile(`\bzulu\b`),
	regexp.MustCompile(`\beclipse-temurin\b`),
	regexp.MustCompile(`\bamazoncorretto\b`),
	regexp.MustCompile(`\bazul\b`),
	regexp.MustCompile(`\bliberica\b`),
	regexp.MustCompile(`\bbellsoft\b`),
}

// P1: False-positive patterns to exclude (javascript, bootstrap, etc.)
var nonJVMImagePatterns = []*regexp.Regexp{
	regexp.MustCompile(`javascript`),
	regexp.MustCompile(`javanese`),
	regexp.MustCompile(`bootstrap`),
	regexp.MustCompile(`reboot`),
}

// ContainerInfo holds detected JVM information
type ContainerInfo struct {
	Name      string
	Image     string
	IsJVM     bool
	Framework string // "spring-boot", "quarkus", "micronaut", "generic"
	Port      int32
	// PortName is the preferred named container port (e.g. "http"), empty
	// when the resolved port should be referenced numerically. Probe
	// builders prefer this over the numeric Port whenever it is non-empty
	// and no workloads.cast.ai/jvm-probe-port annotation override is set.
	PortName string
	Ports []corev1.ContainerPort
	Env   []corev1.EnvVar
}

// Framework constants
const (
	FrameworkSpringBoot = "spring-boot"
	FrameworkQuarkus    = "quarkus"
	FrameworkMicronaut  = "micronaut"
	FrameworkGeneric    = "generic"
	FrameworkNone       = ""
)

// Framework-specific image patterns (word-boundary regex)
var springBootImagePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bspring\b`),
	regexp.MustCompile(`\bboot\b`),
}

var quarkusImagePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bquarkus\b`),
}

var micronautImagePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bmicronaut\b`),
	regexp.MustCompile(`\bgraalvm\b`),
}

// frameworkImagePatterns is the concatenation of all framework image
// patterns, built once at package init so DetectJVMContainer does not
// allocate a fresh backing slice on every call. Appended in a fixed
// order so iteration results are deterministic.
var frameworkImagePatterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0,
		len(springBootImagePatterns)+len(quarkusImagePatterns)+len(micronautImagePatterns))
	out = append(out, springBootImagePatterns...)
	out = append(out, quarkusImagePatterns...)
	out = append(out, micronautImagePatterns...)
	return out
}()

// envVarsIndicateJVM checks if environment variables strongly indicate a JVM container
func envVarsIndicateJVM(env []corev1.EnvVar) bool {
	for _, e := range env {
		envNameLower := strings.ToLower(e.Name)
		// Strong JVM indicators
		if strings.HasPrefix(envNameLower, "java_") ||
			envNameLower == "java_version" ||
			envNameLower == "java_home" ||
			envNameLower == "jvm_options" ||
			envNameLower == "java_tool_options" ||
			envNameLower == "_java_options" {
			return true
		}
	}
	return false
}

// DetectJVMContainer analyzes a container spec to determine if it's a JVM container
// P1: Env vars checked FIRST (strongest signal), then image patterns with word boundaries
func DetectJVMContainer(container corev1.Container) ContainerInfo {
	info := ContainerInfo{
		Name:             container.Name,
		Image:            container.Image,
		IsJVM:            false,
		Framework:        FrameworkNone,
		Port:  8080, // default
		Ports: container.Ports,
		Env:   container.Env,
	}

	// PHASE 1: Check environment variables FIRST (strongest signal)
	for _, env := range container.Env {
		envNameLower := strings.ToLower(env.Name)
		_ = strings.ToLower(env.Value) // envValueLower reserved for future use

		// Check for Java-specific environment variables (strongest JVM signal)
		if strings.HasPrefix(envNameLower, "java_") ||
			envNameLower == "java_version" ||
			envNameLower == "java_home" ||
			envNameLower == "jvm_options" ||
			envNameLower == "java_tool_options" ||
			envNameLower == "_java_options" {
			info.IsJVM = true
		}

		// Detect framework from environment variables
		// Spring Boot
		if strings.HasPrefix(envNameLower, "spring_") && !strings.HasSuffix(envNameLower, "_env") {
			info.Framework = FrameworkSpringBoot
		}
		if strings.Contains(envNameLower, "spring_profiles") {
			info.Framework = FrameworkSpringBoot
		}

		// Quarkus
		if envNameLower == "quarkus_http_port" ||
			strings.HasPrefix(envNameLower, "quarkus_") {
			info.Framework = FrameworkQuarkus
		}

		// Micronaut
		if envNameLower == "micronaut_server_port" ||
			strings.HasPrefix(envNameLower, "micronaut_") {
			info.Framework = FrameworkMicronaut
		}
	}

	// PHASE 2: Image patterns with word boundaries (only if not already detected via env)
	if !info.IsJVM {
		imageLower := strings.ToLower(container.Image)

		// Step 1: JVM runtime patterns (openjdk, java, jre, jdk, ...)
		for _, re := range jvmImagePatterns {
			if re.MatchString(imageLower) {
				info.IsJVM = true
				break
			}
		}

		// Step 2: Apply false-positive exclusion. Only clear the runtime
		// detection here. Skipped when env vars strongly indicate JVM.
		if info.IsJVM && !envVarsIndicateJVM(container.Env) {
			for _, re := range nonJVMImagePatterns {
				if re.MatchString(imageLower) {
					info.IsJVM = false
					break
				}
			}
		}

		// Step 3: Framework-specific images (spring-boot, quarkus,
		// micronaut) are also JVM workloads even when they do not contain
		// a runtime keyword. Runs AFTER the non-JVM exclusion so a keyword
		// appearing in both lists cannot silently clear a framework match.
		// This is the final positive signal that is not overridden by
		// nonJVMImagePatterns. Iterates the package-level
		// frameworkImagePatterns slice (built once at init) to avoid
		// per-call allocation.
		if !info.IsJVM {
			for _, re := range frameworkImagePatterns {
				if re.MatchString(imageLower) {
					info.IsJVM = true
					break
				}
			}
		}
	}

	// PHASE 3: Framework from image name (only if JVM detected and no framework yet)
	if info.IsJVM && info.Framework == FrameworkNone {
		imageLower := strings.ToLower(container.Image)
		for _, re := range springBootImagePatterns {
			if re.MatchString(imageLower) {
				info.Framework = FrameworkSpringBoot
				break
			}
		}
	}

	if info.IsJVM && info.Framework == FrameworkNone {
		imageLower := strings.ToLower(container.Image)
		for _, re := range quarkusImagePatterns {
			if re.MatchString(imageLower) {
				info.Framework = FrameworkQuarkus
				break
			}
		}
	}

	if info.IsJVM && info.Framework == FrameworkNone {
		imageLower := strings.ToLower(container.Image)
		for _, re := range micronautImagePatterns {
			if re.MatchString(imageLower) {
				info.Framework = FrameworkMicronaut
				break
			}
		}
	}

	// If JVM detected but no framework, assume generic
	if info.IsJVM && info.Framework == FrameworkNone {
		info.Framework = FrameworkGeneric
	}

	// Detect port from container ports
	info.Port, info.PortName = detectContainerPort(container)

	return info
}

// detectContainerPort attempts to find the HTTP port from container
// definitions. It returns the numeric port to use alongside the port's
// name when the selected entry has one.
//
// Resolution order:
//  1. Well-known named ports (http, web, https, http-web): return the
//     port's numeric value together with its Name so callers can emit
//     an intstr.String probe reference.
//  2. Common JVM numeric ports (8080, 8443, 9090, 8888): return the
//     numeric value with an empty name. Only well-known names trigger
//     the string form; an arbitrary name on a common port does not.
//  3. The first declared port: return its numeric value and Name (which
//     may be empty).
//  4. Ultimate fallback: (8080, "").
func detectContainerPort(container corev1.Container) (int32, string) {
	// First try named ports
	for _, port := range container.Ports {
		portNameLower := strings.ToLower(port.Name)
		if portNameLower == "http" ||
			portNameLower == "web" ||
			portNameLower == "http-web" ||
			portNameLower == "https" {
			return port.ContainerPort, port.Name
		}
	}

	// Try common HTTP ports
	for _, port := range container.Ports {
		if port.ContainerPort == 8080 ||
			port.ContainerPort == 8443 ||
			port.ContainerPort == 9090 ||
			port.ContainerPort == 8888 {
			return port.ContainerPort, ""
		}
	}

	// Default to the first declared port if no port matched above.
	if len(container.Ports) > 0 {
		p := container.Ports[0]
		return p.ContainerPort, p.Name
	}

	return 8080, ""
}

// DetectFramework determines the framework type from container info
func DetectFramework(container corev1.Container) string {
	info := DetectJVMContainer(container)
	return info.Framework
}

// IsJVMContainer checks if a container is likely running a JVM
func IsJVMContainer(container corev1.Container) bool {
	info := DetectJVMContainer(container)
	return info.IsJVM
}

// ExclusionRule represents a rule for excluding workloads from processing
type ExclusionRule struct {
	NamespaceRegex string            `yaml:"namespaceRegex"`
	NameRegex      string            `yaml:"nameRegex"`
	Labels         map[string]string `yaml:"labels"`
}

// ExclusionRules holds compiled exclusion rules
type ExclusionRules struct {
	rules             []ExclusionRule
	compiledNamespace []*regexp.Regexp
	compiledName      []*regexp.Regexp
}

// NewExclusionRules creates a new ExclusionRules from raw rule data
func NewExclusionRules(rules []ExclusionRule) *ExclusionRules {
	er := &ExclusionRules{
		rules: rules,
	}

	for _, rule := range rules {
		if rule.NamespaceRegex != "" {
			if re, err := regexp.Compile(rule.NamespaceRegex); err == nil {
				er.compiledNamespace = append(er.compiledNamespace, re)
			}
		}
		if rule.NameRegex != "" {
			if re, err := regexp.Compile(rule.NameRegex); err == nil {
				er.compiledName = append(er.compiledName, re)
			}
		}
	}

	return er
}

// IsExcluded checks if a workload should be excluded based on namespace, name, and labels
func (er *ExclusionRules) IsExcluded(namespace, name string, labels map[string]string) bool {
	// Check namespace regex
	for _, re := range er.compiledNamespace {
		if re.MatchString(namespace) {
			return true
		}
	}

	// Check name regex
	for _, re := range er.compiledName {
		if re.MatchString(name) {
			return true
		}
	}

	// Check label selectors
	for _, rule := range er.rules {
		if len(rule.Labels) > 0 {
			match := true
			for k, v := range rule.Labels {
				if labels[k] != v {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}

	return false
}

// DefaultExclusionRules returns the default exclusion rules
func DefaultExclusionRules() *ExclusionRules {
	return NewExclusionRules([]ExclusionRule{
		{NamespaceRegex: "kube-.*"},
		{NamespaceRegex: "kube-system"},
		{NamespaceRegex: "kube-public"},
	})
}
