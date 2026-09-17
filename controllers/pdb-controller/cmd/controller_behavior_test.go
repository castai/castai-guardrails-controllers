package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
)

func intstrPtr(i intstr.IntOrString) *intstr.IntOrString { return &i }

// Regression tests for core PDB controller behavior (values, annotations, exclusions, unhealthy policy resolution).

func TestParsePDBValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want *intstr.IntOrString
	}{
		{"", nil},
		{"  ", nil},
		{"1", &intstr.IntOrString{Type: intstr.Int, IntVal: 1}},
		{" 2 ", &intstr.IntOrString{Type: intstr.Int, IntVal: 2}},
		{"50%", &intstr.IntOrString{Type: intstr.String, StrVal: "50%"}},
		{"100%", &intstr.IntOrString{Type: intstr.String, StrVal: "100%"}},
	}
	for _, tc := range cases {
		got := parsePDBValue(tc.in)
		if (got == nil) != (tc.want == nil) {
			t.Fatalf("parsePDBValue(%q) nil mismatch: got %v want %v", tc.in, got, tc.want)
		}
		if got != nil && tc.want != nil {
			if got.Type != tc.want.Type || got.IntVal != tc.want.IntVal || got.StrVal != tc.want.StrVal {
				t.Fatalf("parsePDBValue(%q) = %#v want %#v", tc.in, got, tc.want)
			}
		}
	}
	if parsePDBValue("not-a-number") != nil {
		t.Fatal("invalid int should return nil")
	}
	got := parsePDBValue("  25%  ")
	if got == nil || got.Type != intstr.String || got.StrVal != "25%" {
		t.Fatalf("trim percent: got %#v", got)
	}
}

func TestIsWorkloadExcluded_noRulesNeverExcludes(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	if isWorkloadExcluded("any", "thing", nil) {
		t.Fatal("with no exclusion rules, nothing should be excluded")
	}
}

func TestIsWorkloadExcluded_nameAndNamespaceBothRequired(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)
	defaultPDBConfigLock.Lock()
	defaultPDBConfig.Exclusions = []ExclusionRule{
		{NamespaceRegex: "^prod$", NameRegex: "^api$", Labels: map[string]string{}},
	}
	defaultPDBConfigLock.Unlock()
	if !isWorkloadExcluded("prod", "api", nil) {
		t.Fatal("prod/api should match")
	}
	if isWorkloadExcluded("prod", "api-v2", nil) {
		t.Fatal("prod/api-v2 should not match name regex")
	}
	if isWorkloadExcluded("staging", "api", nil) {
		t.Fatal("staging/api should not match namespace regex")
	}
}

func TestParseDurationFromConfigMap(t *testing.T) {
	t.Parallel()
	def := 5 * time.Minute
	data := map[string]string{
		"ok":    "2m",
		"bad":   "nope",
		"zero":  "0s",
		"empty": "",
	}
	if d := parseDurationFromConfigMap(data, "ok", def); d != 2*time.Minute {
		t.Fatalf("ok: got %v", d)
	}
	if d := parseDurationFromConfigMap(data, "missing", def); d != def {
		t.Fatalf("missing: got %v want %v", d, def)
	}
	if d := parseDurationFromConfigMap(data, "bad", def); d != def {
		t.Fatalf("bad: got %v want %v", d, def)
	}
	if d := parseDurationFromConfigMap(data, "zero", def); d != def {
		t.Fatalf("zero: got %v want %v", d, def)
	}
	if d := parseDurationFromConfigMap(data, "empty", def); d != def {
		t.Fatalf("empty: got %v want %v", d, def)
	}
}

func TestHasCustomPDBAnnotations_unhealthyKeyPresentCountsAsCustom(t *testing.T) {
	t.Parallel()
	// Key presence drives reconcile eligibility even if value is empty (caller may fix later).
	if !hasCustomPDBAnnotations(map[string]string{annotationUnhealthyPodEvictionPolicy: ""}) {
		t.Fatal("empty unhealthy annotation value should still count as custom PDB annotations")
	}
}

func TestHasCustomPDBAnnotations(t *testing.T) {
	t.Parallel()
	if hasCustomPDBAnnotations(nil) {
		t.Fatal("nil map")
	}
	if hasCustomPDBAnnotations(map[string]string{}) {
		t.Fatal("empty map")
	}
	if !hasCustomPDBAnnotations(map[string]string{annotationMinAvailable: "1"}) {
		t.Fatal("minAvailable")
	}
	if !hasCustomPDBAnnotations(map[string]string{annotationMaxUnavailable: "1"}) {
		t.Fatal("maxUnavailable")
	}
	if !hasCustomPDBAnnotations(map[string]string{annotationUnhealthyPodEvictionPolicy: "AlwaysAllow"}) {
		t.Fatal("unhealthy policy")
	}
	if hasCustomPDBAnnotations(map[string]string{"other": "x"}) {
		t.Fatal("unrelated key")
	}
}

func TestHasBypassAnnotation(t *testing.T) {
	t.Parallel()
	if hasBypassAnnotation(nil) || hasBypassAnnotation(map[string]string{}) {
		t.Fatal("nil/empty")
	}
	if hasBypassAnnotation(map[string]string{annotationBypass: "false"}) {
		t.Fatal("false should not bypass")
	}
	if !hasBypassAnnotation(map[string]string{annotationBypass: "true"}) {
		t.Fatal("true should bypass")
	}
	if hasBypassAnnotation(map[string]string{annotationBypass: "TRUE"}) {
		t.Fatal("only exact lowercase true bypasses")
	}
}

func TestIsWorkloadExcluded_namespaceAndNameRegex(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	defaultPDBConfigLock.Lock()
	defaultPDBConfig.Exclusions = []ExclusionRule{
		{NamespaceRegex: "^(istio-system|castai-agent)$", NameRegex: "", Labels: map[string]string{}},
		{NamespaceRegex: "", NameRegex: `.*-temp$`, Labels: map[string]string{}},
	}
	defaultPDBConfigLock.Unlock()

	if !isWorkloadExcluded("istio-system", "istiod", map[string]string{"app": "x"}) {
		t.Fatal("istio-system should match namespace rule")
	}
	if !isWorkloadExcluded("castai-agent", "agent", nil) {
		t.Fatal("castai-agent should match namespace rule")
	}
	if isWorkloadExcluded("app-ns", "api", nil) {
		t.Fatal("app-ns/api should not be excluded")
	}
	if !isWorkloadExcluded("app-ns", "api-temp", nil) {
		t.Fatal("api-temp should match name suffix rule")
	}
	if isWorkloadExcluded("app-ns", "api", map[string]string{"env": "prod"}) {
		t.Fatal("api without -temp should not match name-only rule")
	}
}

func TestIsWorkloadExcluded_labelRule(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	defaultPDBConfigLock.Lock()
	defaultPDBConfig.Exclusions = []ExclusionRule{
		{
			NamespaceRegex: "^app$",
			NameRegex:      "",
			Labels:         map[string]string{"skip-pdb": "^true$"},
		},
	}
	defaultPDBConfigLock.Unlock()

	if !isWorkloadExcluded("app", "svc", map[string]string{"skip-pdb": "true"}) {
		t.Fatal("label skip-pdb=true should exclude")
	}
	if isWorkloadExcluded("app", "svc", map[string]string{"skip-pdb": "false"}) {
		t.Fatal("skip-pdb=false should not exclude")
	}
	if isWorkloadExcluded("other", "svc", map[string]string{"skip-pdb": "true"}) {
		t.Fatal("wrong namespace should not match")
	}
}

func TestResolveUnhealthyPodEvictionPolicy_defaultsAndAnnotation(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	if p := resolveUnhealthyPodEvictionPolicy(nil); p != nil {
		t.Fatalf("no default: want nil, got %v", p)
	}

	aa := policyv1.AlwaysAllow
	defaultPDBConfigLock.Lock()
	defaultPDBConfig.UnhealthyPodEvictionPolicy = &aa
	defaultPDBConfigLock.Unlock()

	if p := resolveUnhealthyPodEvictionPolicy(nil); p == nil || *p != policyv1.AlwaysAllow {
		t.Fatalf("default AlwaysAllow: got %v", p)
	}

	ann := map[string]string{annotationUnhealthyPodEvictionPolicy: "IfHealthyBudget"}
	if p := resolveUnhealthyPodEvictionPolicy(ann); p == nil || *p != policyv1.IfHealthyBudget {
		t.Fatalf("annotation override: got %v", p)
	}

	annInvalid := map[string]string{annotationUnhealthyPodEvictionPolicy: "bogus"}
	if p := resolveUnhealthyPodEvictionPolicy(annInvalid); p == nil || *p != policyv1.AlwaysAllow {
		t.Fatalf("invalid annotation should fall back to default, got %v", p)
	}

	// Empty annotation value → invalid parse → fall back to default AlwaysAllow
	annEmpty := map[string]string{annotationUnhealthyPodEvictionPolicy: ""}
	if p := resolveUnhealthyPodEvictionPolicy(annEmpty); p == nil || *p != policyv1.AlwaysAllow {
		t.Fatalf("empty annotation should fall back to default, got %v", p)
	}

	// Whitespace in annotation value is trimmed by parser
	annSpaced := map[string]string{annotationUnhealthyPodEvictionPolicy: "  IfHealthyBudget  "}
	if p := resolveUnhealthyPodEvictionPolicy(annSpaced); p == nil || *p != policyv1.IfHealthyBudget {
		t.Fatalf("trimmed annotation: got %v", p)
	}
}

// --- PR #25 cleanup behavior tests ---

func TestCreatePDBForWorkload_deletesLeftoverControllerPDBWhenCastaiHelmCovers(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	helmPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-cluster-controller", Namespace: "castai-agent"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "castai-cluster-controller"}},
			MaxUnavailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	leftover := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-cluster-controller-pdb", Namespace: "castai-agent"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "castai-cluster-controller"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	two := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-cluster-controller", Namespace: "castai-agent"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "castai-cluster-controller"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app.kubernetes.io/name":     "castai-cluster-controller",
					"app.kubernetes.io/instance": "cluster-controller",
				}},
			},
		},
	}
	clientset := fake.NewSimpleClientset(helmPDB, leftover, deploy)

	createPDBForWorkload(context.Background(), clientset, deploy)

	if _, err := clientset.PolicyV1().PodDisruptionBudgets("castai-agent").Get(context.Background(), "castai-cluster-controller-pdb", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected leftover controller PDB to be deleted, got err=%v", err)
	}
	got, err := clientset.PolicyV1().PodDisruptionBudgets("castai-agent").Get(context.Background(), "castai-cluster-controller", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected CAST Helm PDB to remain: %v", err)
	}
	if got.Spec.MaxUnavailable == nil || got.Spec.MinAvailable != nil {
		t.Fatalf("expected Helm PDB untouched, got MinAvailable=%v MaxUnavailable=%v", got.Spec.MinAvailable, got.Spec.MaxUnavailable)
	}
}

func TestCreatePDBForWorkload_keepsCustomerControllerPDBWhenUnrelatedHelmCovers(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	helmPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "my-helm-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	controllerPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	two := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
			},
		},
	}
	clientset := fake.NewSimpleClientset(helmPDB, controllerPDB, deploy)

	createPDBForWorkload(context.Background(), clientset, deploy)

	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected customer controller PDB to be preserved, got err=%v", err)
	}
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "my-helm-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected Helm PDB to remain: %v", err)
	}
}

func TestScanAllPDBsForMultiplePDBs_deletesLeftoverWhenCastaiHelmCovers(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	helmPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-agent", Namespace: "castai-agent"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "castai-agent"}},
			MaxUnavailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	leftover := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-agent-pdb", Namespace: "castai-agent"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "castai-agent"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	two := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-agent", Namespace: "castai-agent"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "castai-agent"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app.kubernetes.io/name":     "castai-agent",
					"app.kubernetes.io/instance": "agent",
				}},
			},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "castai-agent"}}
	clientset := fake.NewSimpleClientset(ns, helmPDB, leftover, deploy)

	scanAllPDBsForMultiplePDBs(context.Background(), clientset)

	if _, err := clientset.PolicyV1().PodDisruptionBudgets("castai-agent").Get(context.Background(), "castai-agent-pdb", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected leftover controller PDB deleted by scan, got err=%v", err)
	}
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("castai-agent").Get(context.Background(), "castai-agent", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected CAST Helm PDB to remain: %v", err)
	}
}

func TestScanAllPDBsForMultiplePDBs_keepsCustomerControllerPDB(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	helmPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "my-helm-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp", "shard": "0"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	controllerPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	two := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp", "shard": "0"}},
			},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	clientset := fake.NewSimpleClientset(ns, helmPDB, controllerPDB, deploy)

	scanAllPDBsForMultiplePDBs(context.Background(), clientset)

	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected customer controller PDB preserved by scan, got err=%v", err)
	}
}

func TestCreatePDBForWorkload_warnsOnInvalidPDBSelector(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// A DoesNotExist match expression with non-empty Values is rejected by
	// metav1.LabelSelectorAsSelector, which causes pdbCoversPodTemplate to emit the
	// "invalid selector" warning and skip coverage for this PDB.
	invalidSelectorPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "broken-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpDoesNotExist, Values: []string{"bogus"}},
				},
			},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	two := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2, AvailableReplicas: 2},
	}
	clientset := fake.NewSimpleClientset(invalidSelectorPDB, deploy)

	createPDBForWorkload(context.Background(), clientset, deploy)

	logged := buf.String()
	if !strings.Contains(logged, "has an invalid selector and will be ignored") {
		t.Fatalf("expected invalid-selector warning in logs, got: %q", logged)
	}
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected controller PDB to be created despite invalid covering PDB selector, got err=%v", err)
	}
}

func TestCreatePDBForWorkload_logsAllCoveringNonControllerPDBs(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	// resetDefaultPDBConfig does not clear the global skipLogTimes map, so the
	// skip log would be suppressed if an earlier test logged against the same
	// (namespace, name) within LogInterval. Reset it to guarantee the log fires.
	skipLogTimesLock.Lock()
	skipLogTimes = make(map[string]time.Time)
	skipLogTimesLock.Unlock()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	helmPDBOne := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "helm-pdb-one", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	helmPDBTwo := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "helm-pdb-two", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	two := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
			},
		},
	}
	clientset := fake.NewSimpleClientset(helmPDBOne, helmPDBTwo, deploy)

	createPDBForWorkload(context.Background(), clientset, deploy)

	logged := buf.String()
	if !strings.Contains(logged, "helm-pdb-one") {
		t.Fatalf("expected log to mention helm-pdb-one, got: %q", logged)
	}
	if !strings.Contains(logged, "helm-pdb-two") {
		t.Fatalf("expected log to mention helm-pdb-two, got: %q", logged)
	}
	if !strings.Contains(logged, "helm-pdb-one, helm-pdb-two") && !strings.Contains(logged, "helm-pdb-two, helm-pdb-one") {
		t.Fatalf("expected log to list both covering non-controller PDBs joined by ', ', got: %q", logged)
	}
}

// --- PR #26 tests: readiness gating + stuck-disruption warnings ---

func TestWorkloadReadyForNewPDB(t *testing.T) {
	if !workloadReadyForNewPDB(2, 2, 2) {
		t.Fatal("expected ready when ready/available match desired")
	}
	if workloadReadyForNewPDB(2, 0, 0) {
		t.Fatal("expected not ready when zero ready")
	}
	if workloadReadyForNewPDB(2, 1, 1) {
		t.Fatal("expected not ready when only partially ready")
	}
	if workloadReadyForNewPDB(2, 2, 1) {
		t.Fatal("expected not ready when available lags ready")
	}
	if workloadReadyForNewPDB(1, 1, 1) {
		t.Fatal("expected not ready for single-replica desired count")
	}
}

func TestCreatePDBForWorkload_skipsWhenWorkloadNotReady(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	// Reset the skip-log dedupe map so this test's skip message always logs.
	skipLogTimesLock.Lock()
	skipLogTimes = make(map[string]time.Time)
	skipLogTimesLock.Unlock()

	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "crashloop", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "crashloop"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "crashloop"}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 0, AvailableReplicas: 0},
	}
	clientset := fake.NewSimpleClientset()

	createPDBForWorkload(context.Background(), clientset, dep)

	pdbs, err := clientset.PolicyV1().PodDisruptionBudgets("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list PDBs: %v", err)
	}
	if len(pdbs.Items) != 0 {
		t.Fatalf("expected no PDB when workload is not ready, got %#v", pdbs.Items)
	}
}

func TestCreatePDBForWorkload_skipsWhenPartiallyReady(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	skipLogTimesLock.Lock()
	skipLogTimes = make(map[string]time.Time)
	skipLogTimesLock.Unlock()

	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rolling", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "rolling"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "rolling"}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1},
	}
	clientset := fake.NewSimpleClientset()

	createPDBForWorkload(context.Background(), clientset, dep)

	pdbs, err := clientset.PolicyV1().PodDisruptionBudgets("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list PDBs: %v", err)
	}
	if len(pdbs.Items) != 0 {
		t.Fatalf("expected no PDB while only partially ready, got %#v", pdbs.Items)
	}
}

func TestCreatePDBForWorkload_createsWhenFullyReady(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	skipLogTimesLock.Lock()
	skipLogTimes = make(map[string]time.Time)
	skipLogTimesLock.Unlock()

	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "healthy", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "healthy"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "healthy"}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2, AvailableReplicas: 2},
	}
	clientset := fake.NewSimpleClientset()

	createPDBForWorkload(context.Background(), clientset, dep)

	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-healthy-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected PDB when fully ready, got err=%v", err)
	}
}

func TestCreatePDBForWorkload_preservesExistingWhenReadinessDrops(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	skipLogTimesLock.Lock()
	skipLogTimes = make(map[string]time.Time)
	skipLogTimesLock.Unlock()

	two := int32(2)
	existing := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 0, AvailableReplicas: 0},
	}
	clientset := fake.NewSimpleClientset(existing)

	createPDBForWorkload(context.Background(), clientset, dep)

	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected existing castai PDB to be preserved when readiness drops, got err=%v", err)
	}
}

func TestScanAllPDBsForDisruptionBlocks_warnsButDoesNotDelete(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	defaultPDBConfigLock.Lock()
	defaultPDBConfig.LogInterval = time.Millisecond
	defaultPDBConfigLock.Unlock()

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-stuck-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "stuck"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			DisruptionsAllowed: 0,
			CurrentHealthy:     0,
			DesiredHealthy:     1,
			ExpectedPods:       2,
		},
	}
	foreign := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "helm-managed", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			DisruptionsAllowed: 0,
			CurrentHealthy:     0,
			DesiredHealthy:     1,
			ExpectedPods:       2,
		},
	}
	clientset := fake.NewSimpleClientset(pdb, foreign)

	scanAllPDBsForDisruptionBlocks(context.Background(), clientset)

	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-stuck-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("scan must not delete controller PDB, got err=%v", err)
	}
	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "helm-managed", metav1.GetOptions{}); err != nil {
		t.Fatalf("scan must not delete non-controller PDB, got err=%v", err)
	}

	disruptionBlockedSinceLock.Lock()
	_, tracked := disruptionBlockedSince["default/castai-stuck-pdb"]
	_, trackedForeign := disruptionBlockedSince["default/helm-managed"]
	disruptionBlockedSinceLock.Unlock()
	if !tracked {
		t.Fatal("expected stuck controller PDB to be tracked for disruption duration")
	}
	if trackedForeign {
		t.Fatal("expected non-controller PDB not to be tracked")
	}
}

// --- PR #15 tests: additional selector labels, scale-down cleanup, selector recreation, nil selector guard ---

func TestEnrichSelectorWithAdditionalLabels_mergesPodTemplateLabels(t *testing.T) {
	t.Parallel()
	base := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}}
	podLabels := map[string]string{
		"app":               "myapp",
		"tier":              "backend",
		"pod-template-hash": "abc123",
	}
	got := enrichSelectorWithAdditionalLabels(base, podLabels, []string{"tier", "missing-key", "app"})
	if got == nil {
		t.Fatal("expected enriched selector")
	}
	if got.MatchLabels["app"] != "myapp" {
		t.Fatalf("existing matchLabels key overwritten: got %v", got.MatchLabels)
	}
	if got.MatchLabels["tier"] != "backend" {
		t.Fatalf("expected tier label to be added from pod template, got %v", got.MatchLabels)
	}
	if _, has := got.MatchLabels["missing-key"]; has {
		t.Fatalf("missing pod-template label should not be added: got %v", got.MatchLabels)
	}
}

func TestEnrichSelectorWithAdditionalLabels_noKeysReturnsBase(t *testing.T) {
	t.Parallel()
	base := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}}
	podLabels := map[string]string{"tier": "backend"}
	got := enrichSelectorWithAdditionalLabels(base, podLabels, nil)
	if got != base {
		t.Fatalf("expected the same selector pointer when no additional keys are configured")
	}
}

func TestCreatePDBForWorkload_includesAdditionalSelectorLabels(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	defaultPDBConfigLock.Lock()
	defaultPDBConfig.AdditionalSelectorLabels = []string{"tier"}
	defaultPDBConfigLock.Unlock()

	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp", "tier": "backend"}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2, AvailableReplicas: 2},
	}
	clientset := fake.NewSimpleClientset()

	createPDBForWorkload(context.Background(), clientset, dep)

	got, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected PDB to be created, got err=%v", err)
	}
	if got.Spec.Selector == nil || got.Spec.Selector.MatchLabels["tier"] != "backend" {
		t.Fatalf("expected selector to include tier=backend, got %#v", got.Spec.Selector)
	}
	if got.Spec.Selector.MatchLabels["app"] != "myapp" {
		t.Fatalf("expected selector to include app=myapp, got %#v", got.Spec.Selector)
	}
}

func TestCreatePDBForWorkload_deletesCastaiPDBWhenScaledBelowTwo(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	one := int32(1)
	existing := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
			},
		},
	}
	clientset := fake.NewSimpleClientset(existing)

	createPDBForWorkload(context.Background(), clientset, dep)

	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected existing castai PDB to be deleted when replicas<2, got err=%v", err)
	}
}

func TestUpdateExistingPDB_deletesAndRecreatesWhenSelectorChanges(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	defaultPDBConfigLock.Lock()
	defaultPDBConfig.AdditionalSelectorLabels = []string{"tier"}
	defaultPDBConfigLock.Unlock()

	existing := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp", "tier": "backend"}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2, AvailableReplicas: 2},
	}
	clientset := fake.NewSimpleClientset(existing)

	updateExistingPDB(context.Background(), clientset, existing, nil, &two, "default", "myapp", dep)

	got, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected PDB to be recreated with new selector, got err=%v", err)
	}
	if got.Spec.Selector == nil || got.Spec.Selector.MatchLabels["tier"] != "backend" {
		t.Fatalf("expected recreated PDB selector to include tier=backend, got %#v", got.Spec.Selector)
	}
}

func TestUpdateExistingPDB_deletesAndRecreatesWhenExistingSelectorIsNil(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	// A pre-existing PDB whose Spec.Selector is nil would otherwise be mutated
	// in place by an Update — which the API server rejects because selectors
	// are immutable. The controller must delete and recreate instead.
	existing := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     nil,
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2, AvailableReplicas: 2},
	}
	clientset := fake.NewSimpleClientset(existing)

	updateExistingPDB(context.Background(), clientset, existing, nil, &two, "default", "myapp", dep)

	got, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected PDB to be recreated when existing selector is nil, got err=%v", err)
	}
	if got.Spec.Selector == nil {
		t.Fatal("expected recreated PDB to have a non-nil selector")
	}
	if got.Spec.Selector.MatchLabels["app"] != "myapp" {
		t.Fatalf("expected recreated selector to include app=myapp, got %#v", got.Spec.Selector)
	}
}

// --- PR #22 tests: fail-closed on PDB list errors, transient GC errors keep PDBs ---

func TestCreatePDBForWorkload_failsClosedOnPDBListError(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2, AvailableReplicas: 2},
	}
	clientset := fake.NewSimpleClientset()
	// Inject a transient list error for every PDB list attempt (initial + race rechecks).
	clientset.PrependReactor("list", "poddisruptionbudgets", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated transient API error")
	})

	createPDBForWorkload(context.Background(), clientset, dep)

	// Drop the transient-error reactor (PrependReactor inserts at index 0) so the
	// verification list below can succeed. Production code logs and returns early
	// on list error rather than returning it.
	clientset.ReactionChain = clientset.ReactionChain[1:]

	pdbs, err := clientset.PolicyV1().PodDisruptionBudgets("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list PDBs: %v", err)
	}
	if len(pdbs.Items) != 0 {
		t.Fatalf("expected no PDB to be created when list fails, got %#v", pdbs.Items)
	}
}

func TestGarbageCollectOrphanedPDBs_keepsPDBOnTransientDeploymentGetError(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	clientset := fake.NewSimpleClientset(pdb)
	// Simulate a transient API error (not NotFound) on Deployment Get.
	clientset.PrependReactor("get", "deployments", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("simulated transient API error")
	})

	garbageCollectOrphanedPDBs(context.Background(), clientset)

	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected PDB to be kept on transient Get error, got err=%v", err)
	}
}

func TestGarbageCollectOrphanedPDBs_keepsPDBOnTransientStatefulSetGetError(t *testing.T) {
	resetDefaultPDBConfig()
	t.Cleanup(resetDefaultPDBConfig)

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "castai-myapp-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
			MinAvailable: intstrPtr(intstr.FromInt32(1)),
		},
	}
	clientset := fake.NewSimpleClientset(pdb)
	clientset.PrependReactor("get", "statefulsets", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("simulated transient API error")
	})

	garbageCollectOrphanedPDBs(context.Background(), clientset)

	if _, err := clientset.PolicyV1().PodDisruptionBudgets("default").Get(context.Background(), "castai-myapp-pdb", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected PDB to be kept on transient StatefulSet Get error, got err=%v", err)
	}
}

// --- PR #23 tests: suffix matching for isControllerOwnedPDB and isCastaiHelmStylePDB ---

func TestIsControllerOwnedPDB_suffixMatching(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		{"castai-myapp-pdb", true},
		{"castai-cluster-controller-pdb", true},
		{"castai-foo", false},                  // CAST Helm-style (no -pdb suffix)
		{"castai-agent", false},                // CAST Helm-style
		{"my-pdb", false},                       // missing prefix
		{"castai-foo-bar-pdb", true},           // internal dashes are fine
		{"othercastai-myapp-pdb", false},       // requires castai- prefix
		{"castai-myapp", false},                // missing -pdb suffix
		{"", false},
	}
	for _, tc := range cases {
		if got := isControllerOwnedPDB(tc.name); got != tc.want {
			t.Errorf("isControllerOwnedPDB(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsCastaiHelmStylePDB_suffixMatching(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		{"castai-foo", true},
		{"castai-agent", true},
		{"castai-cluster-controller", true},
		{"castai-myapp-pdb", false},            // controller-owned, not Helm-style
		{"castai-cluster-controller-pdb", false},
		{"myapp", false},
		{"", false},
		{"castai-", true},                       // prefix match with empty remainder is still Helm-style
	}
	for _, tc := range cases {
		if got := isCastaiHelmStylePDB(tc.name); got != tc.want {
			t.Errorf("isCastaiHelmStylePDB(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
