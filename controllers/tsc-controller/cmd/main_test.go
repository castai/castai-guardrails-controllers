// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"testing"
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
