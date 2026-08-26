// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"encoding/json"
	"log"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Mode values for TSCConfig.Mode.
const (
	ModeApply     = "apply"
	ModeRecommend = "recommend"
)

// TSCConfig holds the controller configuration loaded from the ConfigMap plus
// any environment-derived overrides. The canonical model is:
//
//   - ManagementEnabled: master switch. false = stop patching.
//   - Mode:              "apply" (patch) or "recommend" (snapshot-only).
//   - RollbackOnDisable: when true and ManagementEnabled flips true→false,
//                        run rollback.
//   - SnapshotEnabled:   capture snapshots.
//
// Deprecated keys (enableTSCManagement, dryRun) are still parsed for backward
// compatibility and mapped onto the canonical fields with a deprecation
// warning logged.
type TSCConfig struct {
	DefaultConstraints     []corev1.TopologySpreadConstraint `json:"defaultConstraints"`
	LogInterval            time.Duration                     `json:"logInterval"`
	ReconcileInterval      time.Duration                     `json:"reconcileInterval"`
	GarbageCollectInterval time.Duration                     `json:"garbageCollectInterval"`

	ManagementEnabled bool   `json:"managementEnabled"` // freeze toggle, default true
	RollbackOnDisable bool   `json:"rollbackOnDisable"` // when true and managementEnabled flips true→false, run rollback
	Mode              string `json:"mode"`              // "apply" or "recommend"
	SnapshotEnabled   bool   `json:"snapshotEnabled"`   // capture before patching
	OperatorNamespace string `json:"operatorNamespace"` // namespace where TSCOriginal CRs live
	Version           string `json:"version"`           // build/operator version (informational)
}

// RollbackState is the immutable view of TSCConfig used to detect transitions.
type RollbackState struct {
	ManagementEnabled bool
	RollbackOnDisable bool
	Mode              string
	SnapshotEnabled   bool
	OperatorNamespace string
}

// StateOf returns the rollback-relevant subset of the config.
func (c *TSCConfig) StateOf() RollbackState {
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

// ParseTSCConfig builds a TSCConfig from the ConfigMap data and the
// env-supplied version. Environment variables (MANAGEMENT_ENABLED,
// OPERATOR_NAMESPACE, MODE) override ConfigMap values so operators can flip
// the freeze switch via env without editing the ConfigMap.
//
// Returns the config and any per-key parse errors. A nil error slice means
// every field parsed cleanly. Defaults are applied for missing keys.
//
// Deprecated keys (enableTSCManagement, dryRun) are accepted only for
// backward compatibility: if either is present, a deprecation warning is
// logged and the canonical fields are updated. An explicit canonical key
// always wins over a deprecated key.
func ParseTSCConfig(cm *corev1.ConfigMap, envVersion string) (*TSCConfig, []error) {
	cfg := &TSCConfig{
		DefaultConstraints: defaultConstraints(),
		LogInterval:        15 * time.Minute,
		ReconcileInterval:  2 * time.Minute,
		// PR2 defaults
		ManagementEnabled: true,
		RollbackOnDisable: false,
		Mode:              ModeApply,
		SnapshotEnabled:   true,
		OperatorNamespace: "castai-agent",
		Version:           envVersion,
	}
	if envVersion == "" {
		cfg.Version = "dev"
	}

	var errs []error
	if cm == nil {
		return cfg, errs
	}

	data := cm.Data

	// defaultConstraints (JSON)
	if v, ok := data["defaultConstraints"]; ok && v != "" {
		var cs []corev1.TopologySpreadConstraint
		if err := json.Unmarshal([]byte(v), &cs); err != nil {
			errs = append(errs, err)
		} else {
			cfg.DefaultConstraints = cs
		}
	}

	// durations
	if v, ok := data["logInterval"]; ok && v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			errs = append(errs, err)
		} else {
			cfg.LogInterval = d
			SetLogInterval(d)
		}
	}
	if v, ok := data["reconcileInterval"]; ok && v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			errs = append(errs, err)
		} else {
			cfg.ReconcileInterval = d
		}
	}
	if v, ok := data["garbageCollectInterval"]; ok && v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			errs = append(errs, err)
		} else {
			cfg.GarbageCollectInterval = d
		}
	}

	// exclusion rules (handled separately by the caller — ParseTSCConfig only
	// reports that they exist; the exclusion list itself is owned by main.go
	// because it is read under a separate lock).

	// Canonical fields
	if v, ok := data["managementEnabled"]; ok && v != "" {
		cfg.ManagementEnabled = parseBool(v, true)
	}
	if v, ok := data["rollbackOnDisable"]; ok && v != "" {
		cfg.RollbackOnDisable = parseBool(v, false)
	}
	if v, ok := data["mode"]; ok && v != "" {
		switch v {
		case ModeApply, ModeRecommend:
			cfg.Mode = v
		default:
			errs = append(errs, &unknownModeError{value: v})
		}
	}
	if v, ok := data["snapshotEnabled"]; ok && v != "" {
		cfg.SnapshotEnabled = parseBool(v, true)
	}
	if v, ok := data["operatorNamespace"]; ok && v != "" {
		cfg.OperatorNamespace = v
	}

	// Deprecated keys — backward compatibility mapping.
	// enableTSCManagement=false → ManagementEnabled=false.
	// dryRun=true → Mode=recommend.
	// Canonical keys (set above) win; deprecated keys only fill in fields
	// that are still at their defaults.
	if v, ok := data["enableTSCManagement"]; ok {
		log.Printf("[WARN] config-deprecated: enableTSCManagement is deprecated, use managementEnabled")
		enable := v != "false"
		if _, explicit := data["managementEnabled"]; !explicit {
			cfg.ManagementEnabled = enable
		}
	}
	if v, ok := data["dryRun"]; ok {
		log.Printf("[WARN] config-deprecated: dryRun is deprecated, use mode=recommend")
		if v != "false" {
			if _, explicit := data["mode"]; !explicit {
				cfg.Mode = ModeRecommend
			}
		}
	}

	// Env overrides — final word on management + namespace
	if v := os.Getenv("MANAGEMENT_ENABLED"); v != "" {
		cfg.ManagementEnabled = parseBool(v, cfg.ManagementEnabled)
	}
	if v := os.Getenv("OPERATOR_NAMESPACE"); v != "" {
		cfg.OperatorNamespace = v
	}
	if v := os.Getenv("MODE"); v != "" {
		switch v {
		case ModeApply, ModeRecommend:
			cfg.Mode = v
		default:
			errs = append(errs, &unknownModeError{value: v})
		}
	}

	return cfg, errs
}

// unknownModeError reports an invalid mode value. Implements error.
type unknownModeError struct{ value string }

func (e *unknownModeError) Error() string {
	return "unknown mode value: " + e.value
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

func defaultConstraints() []corev1.TopologySpreadConstraint {
	return []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.DoNotSchedule,
		},
	}
}
