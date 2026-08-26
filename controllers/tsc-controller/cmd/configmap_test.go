// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestParseTSCConfig_Defaults(t *testing.T) {
	cfg, errs := ParseTSCConfig(nil, "")
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
	if cfg.LogInterval != 15*time.Minute {
		t.Errorf("LogInterval default = %v, want 15m", cfg.LogInterval)
	}
	if cfg.ReconcileInterval != 2*time.Minute {
		t.Errorf("ReconcileInterval default = %v, want 2m", cfg.ReconcileInterval)
	}
	if !cfg.DryRun {
		t.Errorf("DryRun default = false, want true")
	}
	if cfg.Version != "dev" {
		t.Errorf("Version default = %q, want dev", cfg.Version)
	}
	if len(cfg.DefaultConstraints) == 0 {
		t.Errorf("DefaultConstraints empty, want non-empty")
	}
}

func TestParseTSCConfig_OverrideManagement(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"managementEnabled": "false",
		},
	}
	cfg, errs := ParseTSCConfig(cm, "v1.2.3")
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

func TestParseTSCConfig_OverrideRollbackOnDisable(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"rollbackOnDisable": "true",
		},
	}
	cfg, errs := ParseTSCConfig(cm, "")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !cfg.RollbackOnDisable {
		t.Errorf("RollbackOnDisable = false, want true")
	}
}

func TestParseTSCConfig_OverrideModeRecommend(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"mode": ModeRecommend,
		},
	}
	cfg, errs := ParseTSCConfig(cm, "")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if cfg.Mode != ModeRecommend {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeRecommend)
	}
}

func TestParseTSCConfig_OverrideSnapshotDisabled(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"snapshotEnabled": "false",
		},
	}
	cfg, errs := ParseTSCConfig(cm, "")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if cfg.SnapshotEnabled {
		t.Errorf("SnapshotEnabled = true, want false")
	}
}

func TestParseTSCConfig_InvalidMode(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"mode": "yolo",
		},
	}
	_, errs := ParseTSCConfig(cm, "")
	if len(errs) == 0 {
		t.Fatalf("expected error for invalid mode, got none")
	}
	if !strings.Contains(errs[0].Error(), "yolo") {
		t.Errorf("error %q does not mention the invalid value", errs[0].Error())
	}
}

func TestParseTSCConfig_OperatorNamespaceOverride(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"operatorNamespace": "custom-ns",
		},
	}
	cfg, errs := ParseTSCConfig(cm, "")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if cfg.OperatorNamespace != "custom-ns" {
		t.Errorf("OperatorNamespace = %q, want custom-ns", cfg.OperatorNamespace)
	}
}

func TestTSCConfig_StateOf(t *testing.T) {
	cfg := &TSCConfig{
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

	var nilCfg *TSCConfig
	if st := nilCfg.StateOf(); st.ManagementEnabled || st.RollbackOnDisable ||
		st.Mode != "" || st.SnapshotEnabled || st.OperatorNamespace != "" {
		t.Errorf("nil StateOf should be zero-value, got %+v", st)
	}
}
