// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseJVMConfig_Defaults(t *testing.T) {
	cfg, errs := ParseJVMConfig(nil, "")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !cfg.ManagementEnabled {
		t.Errorf("ManagementEnabled default = false, want true")
	}
	if cfg.RollbackOnDisable {
		t.Errorf("RollbackOnDisable default = true, want false")
	}
	if cfg.Mode != ModeApply {
		t.Errorf("Mode default = %q, want %q", cfg.Mode, ModeApply)
	}
	if !cfg.SnapshotEnabled {
		t.Errorf("SnapshotEnabled default = false, want true")
	}
	if cfg.OperatorNamespace != "castai-agent" {
		t.Errorf("OperatorNamespace default = %q, want castai-agent", cfg.OperatorNamespace)
	}
	if cfg.Version != "dev" {
		t.Errorf("Version default = %q, want dev", cfg.Version)
	}
	if cfg.LogInterval != "15m" {
		t.Errorf("LogInterval default = %q, want 15m", cfg.LogInterval)
	}
	if cfg.ReconcileInterval != "2m" {
		t.Errorf("ReconcileInterval default = %q, want 2m", cfg.ReconcileInterval)
	}
	if !cfg.DryRun {
		t.Errorf("DryRun default = false, want true")
	}
}

func TestParseJVMConfig_OverrideManagement(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"jvm-managementEnabled": "false",
		},
	}
	cfg, errs := ParseJVMConfig(cm, "v1.2.3")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if cfg.ManagementEnabled {
		t.Errorf("ManagementEnabled = true, want false")
	}
	if cfg.Version != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", cfg.Version)
	}
}

func TestParseJVMConfig_OverrideRollbackOnDisable(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"jvm-rollbackOnDisable": "true",
		},
	}
	cfg, errs := ParseJVMConfig(cm, "")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !cfg.RollbackOnDisable {
		t.Errorf("RollbackOnDisable = false, want true")
	}
}

func TestParseJVMConfig_OverrideModeRecommend(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"jvm-mode": ModeRecommend,
		},
	}
	cfg, errs := ParseJVMConfig(cm, "")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if cfg.Mode != ModeRecommend {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeRecommend)
	}
}

func TestParseJVMConfig_OverrideSnapshotDisabled(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"jvm-snapshotEnabled": "false",
		},
	}
	cfg, errs := ParseJVMConfig(cm, "")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if cfg.SnapshotEnabled {
		t.Errorf("SnapshotEnabled = true, want false")
	}
}

func TestParseJVMConfig_InvalidMode(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"jvm-mode": "yolo",
		},
	}
	_, errs := ParseJVMConfig(cm, "")
	if len(errs) == 0 {
		t.Fatalf("expected error for invalid mode, got none")
	}
	if !strings.Contains(errs[0].Error(), "yolo") {
		t.Errorf("error %q does not mention the invalid value", errs[0].Error())
	}
}

func TestParseJVMConfig_OperatorNamespaceOverride(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"jvm-operatorNamespace": "custom-ns",
		},
	}
	cfg, errs := ParseJVMConfig(cm, "")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if cfg.OperatorNamespace != "custom-ns" {
		t.Errorf("OperatorNamespace = %q, want custom-ns", cfg.OperatorNamespace)
	}
}

func TestParseJVMConfig_ExistingFieldsPreserved(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"jvm-requireBothProbes":    "false",
			"jvm-skipIfAnyProbeExists":  "true",
			"jvm-injectLivenessProbe":   "true",
			"jvm-injectReadinessProbe":  "true",
			"jvm-injectStartupProbe":    "false",
			"jvm-dryRun":                "false",
			"jvm-logIntendedChanges":    "true",
			"jvm-logInterval":           "30s",
			"jvm-reconcileInterval":     "5m",
		},
	}
	cfg, errs := ParseJVMConfig(cm, "")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if cfg.RequireBothProbes {
		t.Errorf("RequireBothProbes = true, want false")
	}
	if !cfg.SkipIfAnyProbeExists {
		t.Errorf("SkipIfAnyProbeExists = false, want true")
	}
	if !cfg.InjectLivenessProbe {
		t.Errorf("InjectLivenessProbe = false, want true")
	}
	if !cfg.InjectReadinessProbe {
		t.Errorf("InjectReadinessProbe = false, want true")
	}
	if cfg.InjectStartupProbe {
		t.Errorf("InjectStartupProbe = true, want false")
	}
	if cfg.DryRun {
		t.Errorf("DryRun = true, want false")
	}
	if !cfg.LogIntendedChanges {
		t.Errorf("LogIntendedChanges = false, want true")
	}
	if cfg.LogInterval != "30s" {
		t.Errorf("LogInterval = %q, want 30s", cfg.LogInterval)
	}
	if cfg.ReconcileInterval != "5m" {
		t.Errorf("ReconcileInterval = %q, want 5m", cfg.ReconcileInterval)
	}
}

func TestJVMConfig_StateOf(t *testing.T) {
	cfg := &JVMConfig{
		ManagementEnabled: true,
		RollbackOnDisable: true,
		Mode:              ModeRecommend,
		SnapshotEnabled:   false,
		OperatorNamespace: "ns1",
	}
	st := cfg.StateOf()
	if st.ManagementEnabled != true || st.RollbackOnDisable != true ||
		st.Mode != ModeRecommend || st.SnapshotEnabled != false ||
		st.OperatorNamespace != "ns1" {
		t.Errorf("StateOf = %+v, want all fields mirrored", st)
	}

	var nilCfg *JVMConfig
	if st := nilCfg.StateOf(); st.ManagementEnabled || st.RollbackOnDisable ||
		st.Mode != "" || st.SnapshotEnabled || st.OperatorNamespace != "" {
		t.Errorf("nil StateOf should be zero-value, got %+v", st)
	}
}
