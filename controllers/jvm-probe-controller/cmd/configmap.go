// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Mode values for JVMConfig.Mode.
const (
	ModeApply = "apply"
)

// JVMConfig holds the controller configuration loaded from the ConfigMap plus
// any environment-derived overrides.
//
// Canonical model:
//   - ManagementEnabled: master switch. false = stop patching.
//   - Mode:              "apply" (patch). The previous "recommend" mode is
//     removed in the webhook migration: per-Pod admission
//     has no snapshot-only mode. Only "apply" is valid
//     for the webhook. Legacy ConfigMaps that set
//     mode=recommend are migrated to mode=apply at load
//     time with an info-level log line.
//
// Deprecated keys (jvm-enableProbeManagement, jvm-dryRun) are still parsed
// for backward compatibility. jvm-enableProbeManagement maps onto
// ManagementEnabled; jvm-dryRun=true previously mapped to Mode=recommend
// (snapshot-only) and is now logged at error level and ignored because
// the controller runs as a live mutating webhook.
type JVMConfig struct {
	// Existing fields
	Frameworks           map[string]FrameworkConfig `json:"frameworks"`
	LogInterval          string                     `json:"logInterval"`
	ReconcileInterval    string                     `json:"reconcileInterval"`
	RequireBothProbes    bool                       `json:"requireBothProbes"`
	SkipIfAnyProbeExists bool                       `json:"skipIfAnyProbeExists"`
	Exclusions           string                     `json:"exclusions"`
	InjectLivenessProbe  bool                       `json:"injectLivenessProbe"`
	InjectReadinessProbe bool                       `json:"injectReadinessProbe"`
	InjectStartupProbe   bool                       `json:"injectStartupProbe"`
	LogIntendedChanges   bool                       `json:"logIntendedChanges"`

	// Chunk 2: probe alignment. When AlignProbes is true and a startup
	// probe is being injected for a container, the controller adjusts the
	// liveness/readiness probes so they cover at least MinProbeWindowSeconds
	// of total observation time (periodSeconds*failureThreshold). It also
	// strips initialDelaySeconds so the startup probe gates startup
	// correctly.
	AlignProbes           bool  `json:"alignProbes"`
	MinProbeWindowSeconds int32 `json:"minProbeWindowSeconds"`
	MaxFailureThreshold   int32 `json:"maxFailureThreshold"`

	// Canonical state fields
	ManagementEnabled bool   `json:"managementEnabled"`
	Mode              string `json:"mode"`
	OperatorNamespace string `json:"operatorNamespace"`
	Version           string `json:"version"`
}

// RollbackState captures the previous ManagementEnabled, Mode, and
// OperatorNamespace values so handleConfigMapUpdate can detect
// transitions if needed in the future.
type RollbackState struct {
	ManagementEnabled bool
	Mode              string
	OperatorNamespace string
}

// StateOf returns a snapshot of the transition-relevant fields.
func (c *JVMConfig) StateOf() RollbackState {
	if c == nil {
		return RollbackState{}
	}
	return RollbackState{
		ManagementEnabled: c.ManagementEnabled,
		Mode:              c.Mode,
		OperatorNamespace: c.OperatorNamespace,
	}
}

// ParseJVMConfig builds a JVMConfig from the ConfigMap data and the
// env-supplied version. Returns the config and any per-key parse errors.
//
// Deprecated keys (jvm-enableProbeManagement, jvm-dryRun) are accepted for
// backward compatibility: a deprecation warning is logged. An explicit
// canonical key always wins.
func ParseJVMConfig(cm *corev1.ConfigMap, envVersion string) (*JVMConfig, []error) {
	def := DefaultJVMConfig()
	cfg := &def

	if envVersion == "" {
		cfg.Version = "dev"
	} else {
		cfg.Version = envVersion
	}

	var errs []error
	if cm == nil {
		return cfg, errs
	}

	data := cm.Data

	if v, ok := data["jvm-frameworks"]; ok && v != "" {
		var fws map[string]FrameworkConfig
		if err := json.Unmarshal([]byte(v), &fws); err != nil {
			errs = append(errs, err)
		} else {
			cfg.Frameworks = fws
		}
	}

	if v, ok := data["jvm-logInterval"]; ok && v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			errs = append(errs, err)
		} else {
			cfg.LogInterval = v
			SetLogInterval(d)
		}
	}
	if v, ok := data["jvm-reconcileInterval"]; ok {
		cfg.ReconcileInterval = v
	}

	if v, ok := data["jvm-requireBothProbes"]; ok {
		cfg.RequireBothProbes = v != "false"
	}
	if v, ok := data["jvm-skipIfAnyProbeExists"]; ok {
		cfg.SkipIfAnyProbeExists = v == "true"
	}
	if v, ok := data["jvm-exclusions"]; ok {
		cfg.Exclusions = v
	}

	if v, ok := data["jvm-injectLivenessProbe"]; ok {
		cfg.InjectLivenessProbe = v == "true"
	}
	if v, ok := data["jvm-injectReadinessProbe"]; ok {
		cfg.InjectReadinessProbe = v == "true"
	}
	if v, ok := data["jvm-injectStartupProbe"]; ok {
		cfg.InjectStartupProbe = v == "true"
	}

	if v, ok := data["jvm-logIntendedChanges"]; ok {
		cfg.LogIntendedChanges = v == "true"
	}

	// Chunk 2: probe alignment fields.
	if v, ok := data["jvm-alignProbes"]; ok {
		cfg.AlignProbes = v == "true"
	}
	if v, ok := data["jvm-minProbeWindowSeconds"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MinProbeWindowSeconds = int32(n)
		} else {
			errs = append(errs, fmt.Errorf("invalid jvm-minProbeWindowSeconds %q: %w", v, err))
		}
	}
	if v, ok := data["jvm-maxFailureThreshold"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxFailureThreshold = int32(n)
		} else {
			errs = append(errs, fmt.Errorf("invalid jvm-maxFailureThreshold %q: %w", v, err))
		}
	}

	// Canonical state fields (no jvm- prefix — matches the rendered ConfigMap).
	if v, ok := data["managementEnabled"]; ok && v != "" {
		cfg.ManagementEnabled = parseBool(v, true)
	}
	if v, ok := data["mode"]; ok && v != "" {
		switch v {
		case ModeApply:
			cfg.Mode = v
		case "recommend":
			// Legacy migration: ConfigMaps that set mode=recommend used to
			// select a snapshot-only mode. That mode is removed by the
			// webhook migration — per-Pod admission has no snapshot-only
			// mode. Normalize to ModeApply and warn so operators can find
			// the legacy setting.
			log.Printf("[WARN] config-migrate: mode=recommend is no longer supported for JVM webhook; using mode=apply")
			cfg.Mode = ModeApply
		default:
			errs = append(errs, &unknownModeError{value: v})
		}
	}
	if v, ok := data["operatorNamespace"]; ok && v != "" {
		cfg.OperatorNamespace = v
	}

	// Deprecated keys — backward compatibility mapping.
	// jvm-enableProbeManagement=false → ManagementEnabled=false.
	// jvm-dryRun=true → previously mapped to Mode=recommend; the
	// recommend mode is removed by the webhook migration, so the key is
	// now logged at error level and ignored. The controller now runs as a
	// live mutating webhook.
	// Canonical keys (set above) win.
	if v, ok := data["jvm-enableProbeManagement"]; ok {
		log.Printf("[WARN] config-deprecated: jvm-enableProbeManagement is deprecated, use managementEnabled")
		enable := v != "false"
		if _, explicit := data["managementEnabled"]; !explicit {
			cfg.ManagementEnabled = enable
		}
	}
	if v, ok := data["jvm-dryRun"]; ok {
		log.Printf("[ERROR] config-deprecated: jvm-dryRun=%s is no longer supported; the JVM controller now runs as a live mutating webhook (mode=apply). The controller will mutate Pods on admission.", v)
	}

	return cfg, errs
}

// unknownModeError reports an invalid mode value. Implements error.
type unknownModeError struct{ value string }

func (e *unknownModeError) Error() string {
	return fmt.Sprintf("unknown mode value: %s", e.value)
}

func parseBool(s string, def bool) bool {
	switch s {
	case "true":
		return true
	case "false":
		return false
	default:
		return def
	}
}
