// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Mode values for JVMConfig.Mode.
const (
	ModeApply     = "apply"
	ModeRecommend = "recommend"
)

// JVMConfig holds the controller configuration loaded from the ConfigMap plus
// any environment-derived overrides. Mirrors the TSCConfig shape so the
// rollback/snapshot state machine can be shared.
type JVMConfig struct {
	// Existing fields
	Frameworks            map[string]FrameworkConfig `json:"frameworks"`
	LogInterval           string                     `json:"logInterval"`
	ReconcileInterval     string                     `json:"reconcileInterval"`
	RequireBothProbes     bool                       `json:"requireBothProbes"`
	SkipIfAnyProbeExists  bool                       `json:"skipIfAnyProbeExists"`
	Exclusions            string                     `json:"exclusions"`
	InjectLivenessProbe   bool                       `json:"injectLivenessProbe"`
	InjectReadinessProbe  bool                       `json:"injectReadinessProbe"`
	InjectStartupProbe    bool                       `json:"injectStartupProbe"`
	DryRun                bool                       `json:"dryRun"`
	LogIntendedChanges    bool                       `json:"logIntendedChanges"`
	EnableProbeManagement bool                       `json:"enableProbeManagement"`

	// New fields for PR3 (rollback + snapshot wiring)
	ManagementEnabled bool   `json:"managementEnabled"`
	RollbackOnDisable bool   `json:"rollbackOnDisable"`
	Mode              string `json:"mode"`
	SnapshotEnabled   bool   `json:"snapshotEnabled"`
	OperatorNamespace string `json:"operatorNamespace"`
	Version           string `json:"version"`
}

// RollbackState is the immutable view used to detect transitions.
type RollbackState struct {
	ManagementEnabled bool
	RollbackOnDisable bool
	Mode              string
	SnapshotEnabled   bool
	OperatorNamespace string
}

// StateOf returns the rollback-relevant subset of the config.
func (c *JVMConfig) StateOf() RollbackState {
	if c == nil {
		return RollbackState{}
	}
	return RollbackState{
		ManagementEnabled: c.ManagementEnabled,
		RollbackOnDisable: c.RollbackOnDisable,
		Mode:              c.Mode,
		SnapshotEnabled:   c.SnapshotEnabled,
		OperatorNamespace: c.OperatorNamespace,
	}
}

// ParseJVMConfig builds a JVMConfig from the ConfigMap data and the
// env-supplied version. Returns the config and any per-key parse errors.
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

	if v, ok := data["jvm-dryRun"]; ok {
		cfg.DryRun = v == "true"
	}
	if v, ok := data["jvm-logIntendedChanges"]; ok {
		cfg.LogIntendedChanges = v == "true"
	}

	// PR3 fields
	if v, ok := data["jvm-managementEnabled"]; ok && v != "" {
		cfg.ManagementEnabled = parseBool(v, true)
	}
	if v, ok := data["jvm-rollbackOnDisable"]; ok && v != "" {
		cfg.RollbackOnDisable = parseBool(v, false)
	}
	if v, ok := data["jvm-mode"]; ok && v != "" {
		switch v {
		case ModeApply, ModeRecommend:
			cfg.Mode = v
		default:
			errs = append(errs, &unknownModeError{value: v})
		}
	}
	if v, ok := data["jvm-snapshotEnabled"]; ok && v != "" {
		cfg.SnapshotEnabled = parseBool(v, true)
	}
	if v, ok := data["jvm-operatorNamespace"]; ok && v != "" {
		cfg.OperatorNamespace = v
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
