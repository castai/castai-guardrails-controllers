// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// setOperatorNamespace temporarily installs a TSCConfig with the given
// OperatorNamespace and returns a cleanup function that restores the prior
// global config. Mutates the package-level state guarded by configLock.
func setOperatorNamespace(t *testing.T, ns string) {
	t.Helper()
	configLock.Lock()
	prev := config
	config = &TSCConfig{OperatorNamespace: ns}
	configLock.Unlock()
	t.Cleanup(func() {
		configLock.Lock()
		config = prev
		configLock.Unlock()
	})
}

// setExclusionRules temporarily installs exclusion rules and returns a
// cleanup that restores the prior slice. Mutates state guarded by rulesLock.
func setExclusionRules(t *testing.T, rules []ExclusionRule) {
	t.Helper()
	rulesLock.Lock()
	prev := exclusionRules
	exclusionRules = rules
	rulesLock.Unlock()
	t.Cleanup(func() {
		rulesLock.Lock()
		exclusionRules = prev
		rulesLock.Unlock()
	})
}

func TestIsControllerSelf_Match(t *testing.T) {
	setOperatorNamespace(t, "castai-agent")
	if !isControllerSelf("castai-agent", map[string]string{ControllerSelfLabel: "true"}) {
		t.Fatalf("expected self match for namespace %q with label %s=true", "castai-agent", ControllerSelfLabel)
	}
}

func TestIsControllerSelf_NamespaceMismatch(t *testing.T) {
	setOperatorNamespace(t, "castai-agent")
	if isControllerSelf("other-ns", map[string]string{ControllerSelfLabel: "true"}) {
		t.Fatalf("expected no self match when namespace %q != operator", "other-ns")
	}
}

func TestIsControllerSelf_LabelMissing(t *testing.T) {
	setOperatorNamespace(t, "castai-agent")
	if isControllerSelf("castai-agent", map[string]string{"app": "other"}) {
		t.Fatalf("expected no self match without %s label", ControllerSelfLabel)
	}
}

func TestIsControllerSelf_LabelWrongValue(t *testing.T) {
	setOperatorNamespace(t, "castai-agent")
	if isControllerSelf("castai-agent", map[string]string{ControllerSelfLabel: "false"}) {
		t.Fatalf("expected no self match when %s label value is not %q", ControllerSelfLabel, ControllerSelfLabelTrue)
	}
}

func TestIsControllerSelf_NilLabels(t *testing.T) {
	setOperatorNamespace(t, "castai-agent")
	if isControllerSelf("castai-agent", nil) {
		t.Fatalf("expected no self match with nil labels")
	}
}

func TestIsExcluded_SelfLabelInOperatorNamespace(t *testing.T) {
	setOperatorNamespace(t, "castai-agent")
	setExclusionRules(t, nil)

	labels := map[string]string{ControllerSelfLabel: "true"}
	if !isExcluded("castai-agent", "castai-tsc-controller", labels) {
		t.Fatalf("expected workload with %s label in operator namespace to be excluded by self-check", ControllerSelfLabel)
	}
}

func TestIsExcluded_NoSelfLabelInOperatorNamespace(t *testing.T) {
	setOperatorNamespace(t, "castai-agent")
	setExclusionRules(t, nil)

	// No self-label, no exclusion rules → must not be excluded.
	if isExcluded("castai-agent", "other-workload", map[string]string{"app": "other"}) {
		t.Fatalf("expected workload without self-label and no rules to NOT be excluded")
	}
}

func TestIsExcluded_SelfLabelInDifferentNamespace(t *testing.T) {
	setOperatorNamespace(t, "castai-agent")
	setExclusionRules(t, nil)

	labels := map[string]string{ControllerSelfLabel: "true"}
	// Self-label in a non-operator namespace must NOT be excluded by
	// self-check (the user has applied the label elsewhere on purpose).
	if isExcluded("other-ns", "other-workload", labels) {
		t.Fatalf("expected self-label outside operator namespace to NOT be excluded")
	}
}

func TestIsExcluded_SelfCheckShortCircuitsUserRules(t *testing.T) {
	setOperatorNamespace(t, "castai-agent")
	// Even if a user adds a permissive rule that would match any labels map,
	// the self-check must short-circuit first and exclude the controller.
	setExclusionRules(t, []ExclusionRule{
		{Labels: map[string]string{"app": "would-match-anything-but-empty"}},
	})

	if !isExcluded("castai-agent", "castai-tsc-controller", map[string]string{ControllerSelfLabel: "true"}) {
		t.Fatalf("expected self-check to exclude controller regardless of user rules")
	}
}

func TestIsExcluded_UserRulesStillApplyForNonSelf(t *testing.T) {
	setOperatorNamespace(t, "castai-agent")
	setExclusionRules(t, []ExclusionRule{
		{Labels: map[string]string{"app": "skip-me"}},
	})

	labels := map[string]string{"app": "skip-me"}
	if !isExcluded("any-ns", "any-name", labels) {
		t.Fatalf("expected user exclusion rule to still exclude matching workload")
	}

	noMatch := map[string]string{"app": "keep-me"}
	if isExcluded("any-ns", "any-name", noMatch) {
		t.Fatalf("expected non-matching workload to NOT be excluded")
	}
}

func TestConstraintsChanged(t *testing.T) {
	base := []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
		},
	}

	cases := []struct {
		name string
		old  []corev1.TopologySpreadConstraint
		new  []corev1.TopologySpreadConstraint
		want bool
	}{
		{
			name: "equal",
			old:  base,
			new:  append([]corev1.TopologySpreadConstraint(nil), base...),
			want: false,
		},
		{
			name: "different length",
			old:  base,
			new:  append(base, corev1.TopologySpreadConstraint{MaxSkew: 2, TopologyKey: "kubernetes.io/hostname"}),
			want: true,
		},
		{
			name: "changed MaxSkew",
			old:  base,
			new: []corev1.TopologySpreadConstraint{
				{
					MaxSkew:           2, // changed
					TopologyKey:       base[0].TopologyKey,
					WhenUnsatisfiable: base[0].WhenUnsatisfiable,
					LabelSelector:     base[0].LabelSelector,
				},
			},
			want: true,
		},
		{
			name: "changed TopologyKey",
			old:  base,
			new: []corev1.TopologySpreadConstraint{
				{
					MaxSkew:           base[0].MaxSkew,
					TopologyKey:       "kubernetes.io/hostname", // changed
					WhenUnsatisfiable: base[0].WhenUnsatisfiable,
					LabelSelector:     base[0].LabelSelector,
				},
			},
			want: true,
		},
		{
			name: "changed WhenUnsatisfiable",
			old:  base,
			new: []corev1.TopologySpreadConstraint{
				{
					MaxSkew:           base[0].MaxSkew,
					TopologyKey:       base[0].TopologyKey,
					WhenUnsatisfiable: corev1.DoNotSchedule, // changed
					LabelSelector:     base[0].LabelSelector,
				},
			},
			want: true,
		},
		{
			name: "changed LabelSelector",
			old:  base,
			new: []corev1.TopologySpreadConstraint{
				{
					MaxSkew:           base[0].MaxSkew,
					TopologyKey:       base[0].TopologyKey,
					WhenUnsatisfiable: base[0].WhenUnsatisfiable,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "api"}, // changed
					},
				},
			},
			want: true,
		},
		{
			name: "nil vs empty",
			old:  nil,
			new:  []corev1.TopologySpreadConstraint{},
			want: true,
		},
		{
			name: "nil vs nil",
			old:  nil,
			new:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := constraintsChanged(tc.old, tc.new); got != tc.want {
				t.Errorf("constraintsChanged = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExclusionsChanged(t *testing.T) {
	base := []ExclusionRule{
		{
			NamespaceRegex: "^kube-",
			NameRegex:      ".*-system$",
			Labels:         map[string]string{"app": "kube"},
		},
	}

	cases := []struct {
		name string
		old  []ExclusionRule
		new  []ExclusionRule
		want bool
	}{
		{
			name: "equal",
			old:  base,
			new:  append([]ExclusionRule(nil), base...),
			want: false,
		},
		{
			name: "different length",
			old:  base,
			new: append(base, ExclusionRule{
				NamespaceRegex: "^other-",
			}),
			want: true,
		},
		{
			name: "changed regex",
			old:  base,
			new: []ExclusionRule{
				{
					NamespaceRegex: "^kube-system$", // changed
					NameRegex:      base[0].NameRegex,
					Labels:         base[0].Labels,
				},
			},
			want: true,
		},
		{
			name: "changed labels map",
			old:  base,
			new: []ExclusionRule{
				{
					NamespaceRegex: base[0].NamespaceRegex,
					NameRegex:      base[0].NameRegex,
					Labels:         map[string]string{"app": "kube", "tier": "control"}, // changed
				},
			},
			want: true,
		},
		{
			name: "nil vs nil",
			old:  nil,
			new:  nil,
			want: false,
		},
		{
			name: "nil vs empty",
			old:  nil,
			new:  []ExclusionRule{},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exclusionsChanged(tc.old, tc.new); got != tc.want {
				t.Errorf("exclusionsChanged = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTriggerWorkloadReconcile exercises the goroutine-spawning helper.
// We replace reconcileAllWorkloadsFn with a synchronous spy that records
// invocation and signals via a WaitGroup, so the test never depends on
// timing. The package-level fn is restored at the end so subsequent tests
// see the production value.
func TestTriggerWorkloadReconcile(t *testing.T) {
	// Seed processedWorkloads so we can verify it gets cleared.
	workloadsLock.Lock()
	processedWorkloads["Deployment/ns/a"] = true
	processedWorkloads["StatefulSet/ns/b"] = true
	processedWorkloads["Deployment/ns/c"] = true
	workloadsLock.Unlock()

	// Install a synchronous spy.
	originalFn := reconcileAllWorkloadsFn
	t.Cleanup(func() {
		reconcileAllWorkloadsFn = originalFn
	})

	var (
		spyMu      sync.Mutex
		spyCalls   int
		spySeenCtx context.Context
	)
	done := make(chan struct{})
	reconcileAllWorkloadsFn = func(ctx context.Context) {
		spyMu.Lock()
		spyCalls++
		spySeenCtx = ctx
		spyMu.Unlock()
		close(done)
	}

	triggerWorkloadReconcile(context.Background())

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for reconcileAllWorkloadsFn to be invoked")
	}

	// Verify the map was cleared.
	workloadsLock.Lock()
	remaining := len(processedWorkloads)
	workloadsLock.Unlock()
	if remaining != 0 {
		t.Errorf("processedWorkloads not cleared: %d entries remain", remaining)
	}

	// Verify the spy was invoked exactly once.
	spyMu.Lock()
	calls := spyCalls
	gotCtx := spySeenCtx
	spyMu.Unlock()
	if calls != 1 {
		t.Errorf("reconcileAllWorkloadsFn calls = %d, want 1", calls)
	}
	if gotCtx == nil {
		t.Errorf("spy did not receive context")
	}
}

// TestShouldReconcileOnConfigChange covers the decision logic for the
// extracted ConfigMap-event handler.
func TestShouldReconcileOnConfigChange(t *testing.T) {
	baseCfg := func() *TSCConfig {
		return &TSCConfig{
			ManagementEnabled: true,
			Mode:              ModeApply,
			DefaultConstraints: []corev1.TopologySpreadConstraint{
				{MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: corev1.ScheduleAnyway},
			},
		}
	}

	baseRules := []ExclusionRule{{NamespaceRegex: "^kube-"}}

	cases := []struct {
		name      string
		oldCfg    *TSCConfig
		newCfg    *TSCConfig
		oldRules  []ExclusionRule
		newRules  []ExclusionRule
		wantRecon bool
	}{
		{
			name:      "constraints changed while enabled/apply",
			oldCfg:    baseCfg(),
			newCfg:    withChangedConstraints(baseCfg()),
			oldRules:  baseRules,
			newRules:  baseRules,
			wantRecon: true,
		},
		{
			name:      "exclusions changed while enabled/apply",
			oldCfg:    baseCfg(),
			newCfg:    baseCfg(),
			oldRules:  baseRules,
			newRules:  []ExclusionRule{{NamespaceRegex: "^other-"}},
			wantRecon: true,
		},
		{
			name: "management false→true while apply",
			oldCfg: &TSCConfig{
				ManagementEnabled: false,
				Mode:              ModeApply,
			},
			newCfg: &TSCConfig{
				ManagementEnabled: true,
				Mode:              ModeApply,
			},
			oldRules:  baseRules,
			newRules:  baseRules,
			wantRecon: true,
		},
		{
			name: "mode recommend→apply while enabled",
			oldCfg: &TSCConfig{
				ManagementEnabled: true,
				Mode:              ModeRecommend,
			},
			newCfg: &TSCConfig{
				ManagementEnabled: true,
				Mode:              ModeApply,
			},
			oldRules:  baseRules,
			newRules:  baseRules,
			wantRecon: true,
		},
		{
			name:      "no change while enabled/apply",
			oldCfg:    baseCfg(),
			newCfg:    baseCfg(),
			oldRules:  baseRules,
			newRules:  baseRules,
			wantRecon: false,
		},
		{
			name: "management true→false while apply (rollback path)",
			oldCfg: &TSCConfig{
				ManagementEnabled: true,
				Mode:              ModeApply,
			},
			newCfg: &TSCConfig{
				ManagementEnabled: false,
				Mode:              ModeApply,
			},
			oldRules:  baseRules,
			newRules:  baseRules,
			wantRecon: false,
		},
		{
			name:      "enabled/apply but config unchanged",
			oldCfg:    baseCfg(),
			newCfg:    baseCfg(),
			oldRules:  baseRules,
			newRules:  baseRules,
			wantRecon: false,
		},
		{
			name: "enabled/recommend",
			oldCfg: &TSCConfig{
				ManagementEnabled: true,
				Mode:              ModeRecommend,
			},
			newCfg: &TSCConfig{
				ManagementEnabled: true,
				Mode:              ModeRecommend,
			},
			oldRules:  baseRules,
			newRules:  baseRules,
			wantRecon: false,
		},
		{
			name:      "nil newCfg",
			oldCfg:    baseCfg(),
			newCfg:    nil,
			oldRules:  baseRules,
			newRules:  baseRules,
			wantRecon: false,
		},
		{
			name:   "nil oldCfg (first load after cache sync)",
			oldCfg: nil,
			newCfg: &TSCConfig{
				ManagementEnabled: true,
				Mode:              ModeApply,
			},
			oldRules:  nil,
			newRules:  nil,
			wantRecon: true,
		},
		{
			name: "management disabled, exclusions changed (should NOT reconcile)",
			oldCfg: &TSCConfig{
				ManagementEnabled: false,
				Mode:              ModeApply,
			},
			newCfg: &TSCConfig{
				ManagementEnabled: false,
				Mode:              ModeApply,
			},
			oldRules:  baseRules,
			newRules:  []ExclusionRule{{NamespaceRegex: "^other-"}},
			wantRecon: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldReconcileOnConfigChange(tc.oldCfg, tc.newCfg, tc.oldRules, tc.newRules)
			if got != tc.wantRecon {
				t.Errorf("shouldReconcileOnConfigChange = %v, want %v", got, tc.wantRecon)
			}
		})
	}
}

// withChangedConstraints returns a copy of cfg with the DefaultConstraints
// MaxSkew field bumped, for "constraint changed" fixtures.
func withChangedConstraints(cfg *TSCConfig) *TSCConfig {
	clone := *cfg
	if len(clone.DefaultConstraints) > 0 {
		cs := append([]corev1.TopologySpreadConstraint(nil), clone.DefaultConstraints...)
		cs[0].MaxSkew = 5
		clone.DefaultConstraints = cs
	}
	return &clone
}

// TestConfigMapChangeTriggersReconcile exercises the onConfigMapChange
// pure helper for the full matrix of transitions the reviewer called out:
// constraint change, exclusion change, management false→true, no
// effective change, management true→false (rollback path), enabled but
// recommend mode, and a nil oldCfg. We verify both flags (rollback and
// reconcile) for each scenario.
func TestConfigMapChangeTriggersReconcile(t *testing.T) {
	enabledApply := func() *TSCConfig {
		return &TSCConfig{
			ManagementEnabled: true,
			RollbackOnDisable: true,
			Mode:              ModeApply,
			OperatorNamespace: "castai-agent",
			DefaultConstraints: []corev1.TopologySpreadConstraint{
				{MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: corev1.ScheduleAnyway},
			},
		}
	}
	baseRules := []ExclusionRule{{NamespaceRegex: "^kube-"}}

	cases := []struct {
		name          string
		oldCfg        *TSCConfig
		newCfg        *TSCConfig
		oldRules      []ExclusionRule
		newRules      []ExclusionRule
		wantRollback  bool
		wantReconcile bool
	}{
		{
			name:          "constraint change while enabled/apply",
			oldCfg:        enabledApply(),
			newCfg:        withChangedConstraints(enabledApply()),
			oldRules:      baseRules,
			newRules:      baseRules,
			wantRollback:  false,
			wantReconcile: true,
		},
		{
			name:          "exclusion change while enabled/apply",
			oldCfg:        enabledApply(),
			newCfg:        enabledApply(),
			oldRules:      baseRules,
			newRules:      []ExclusionRule{{NamespaceRegex: "^other-"}},
			wantRollback:  false,
			wantReconcile: true,
		},
		{
			name: "management false→true while apply",
			oldCfg: &TSCConfig{
				ManagementEnabled: false,
				Mode:              ModeApply,
				OperatorNamespace: "castai-agent",
			},
			newCfg:        enabledApply(),
			oldRules:      baseRules,
			newRules:      baseRules,
			wantRollback:  false,
			wantReconcile: true,
		},
		{
			name:          "no effective change while enabled/apply",
			oldCfg:        enabledApply(),
			newCfg:        enabledApply(),
			oldRules:      baseRules,
			newRules:      baseRules,
			wantRollback:  false,
			wantReconcile: false,
		},
		{
			name: "management true→false with rollbackOnDisable=true",
			oldCfg: &TSCConfig{
				ManagementEnabled: true,
				RollbackOnDisable: true,
				Mode:              ModeApply,
				OperatorNamespace: "castai-agent",
			},
			newCfg: &TSCConfig{
				ManagementEnabled: false,
				RollbackOnDisable: true,
				Mode:              ModeApply,
				OperatorNamespace: "castai-agent",
			},
			oldRules:      baseRules,
			newRules:      baseRules,
			wantRollback:  true,
			wantReconcile: false,
		},
		{
			name: "enabled/recommend",
			oldCfg: &TSCConfig{
				ManagementEnabled: true,
				RollbackOnDisable: true,
				Mode:              ModeRecommend,
				OperatorNamespace: "castai-agent",
			},
			newCfg: &TSCConfig{
				ManagementEnabled: true,
				RollbackOnDisable: true,
				Mode:              ModeRecommend,
				OperatorNamespace: "castai-agent",
			},
			oldRules:      baseRules,
			newRules:      baseRules,
			wantRollback:  false,
			wantReconcile: false,
		},
		{
			name:          "nil oldCfg while enabled/apply",
			oldCfg:        nil,
			newCfg:        enabledApply(),
			oldRules:      nil,
			newRules:      nil,
			wantRollback:  false,
			wantReconcile: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRollback, gotReconcile := onConfigMapChange(tc.oldCfg, tc.newCfg, tc.oldRules, tc.newRules)
			if gotRollback != tc.wantRollback {
				t.Errorf("triggerRollback = %v, want %v", gotRollback, tc.wantRollback)
			}
			if gotReconcile != tc.wantReconcile {
				t.Errorf("triggerReconcile = %v, want %v", gotReconcile, tc.wantReconcile)
			}
		})
	}
}
