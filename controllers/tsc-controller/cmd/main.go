package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
		"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	appv1 "k8s.io/api/apps/v1"
)

const (
	ControllerName         = "castai-tsc-controller"
	ConfigMapNamespace     = "castai-agent"
	ConfigMapName          = "castai-tsc-controller-config"
	LeaderElectionLockName = "castai-tsc-controller-leader"
	AnnotationBypass       = "workloads.cast.ai/tsc-bypass"
	AnnotationMaxSkew      = "workloads.cast.ai/tsc-maxSkew"
	AnnotationTopologyKey  = "workloads.cast.ai/tsc-topologyKey"
	AnnotationWhenUnsat    = "workloads.cast.ai/tsc-whenUnsatisfiable"
	AnnotationConstraints  = "workloads.cast.ai/tsc-constraints"
	ManagedByLabel         = "cast.ai/managed-by"
	ManagedByValue         = "tsc-controller"
)

var (
	masterURL       string
	kubeconfig      string
	configNamespace string

	clientset        *kubernetes.Clientset
	recorder         record.EventRecorder
	config           *TSCConfig
	configLock       sync.RWMutex
	exclusionRules   []ExclusionRule
	rulesLock        sync.RWMutex
	processedWorkloads = make(map[string]bool)
	workloadsLock    sync.Mutex
)

type TSCConfig struct {
	DefaultConstraints     []corev1.TopologySpreadConstraint `json:"defaultConstraints"`
	LogInterval            time.Duration                     `json:"logInterval"`
	ReconcileInterval      time.Duration                     `json:"reconcileInterval"`
	GarbageCollectInterval time.Duration                     `json:"garbageCollectInterval"`
	DryRun                 bool                              `json:"dryRun"`
	EnableTSCManagement    bool                              `json:"enableTSCManagement"`
}

type ExclusionRule struct {
	NamespaceRegex string            `json:"namespaceRegex"`
	NameRegex      string            `json:"nameRegex"`
	Labels         map[string]string `json:"labels"`
}

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	flag.StringVar(&masterURL, "master", "", "Kubernetes API server address")
	flag.StringVar(&configNamespace, "config-namespace", ConfigMapNamespace, "ConfigMap namespace")
}

func main() {
	flag.Parse()

	initLogging()

	// Build config
	cfg, err := buildConfig()
	if err != nil {
		logAlways("Error building kubeconfig: %v", err)
		os.Exit(1)
	}

	// Create clientset
	clientset, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		logAlways("Error creating clientset: %v", err)
		os.Exit(1)
	}

	// Create event recorder
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: clientset.CoreV1().Events(""),
	})
	recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{
		Component: ControllerName,
	})

	// Load initial config
	loadConfig()

	// Setup signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-sigCh
		logAlways("Received shutdown signal")
		cancel()
	}()

	// Start leader election
	id := os.Getenv("HOSTNAME")
	if id == "" {
		id = fmt.Sprintf("%s-%d", ControllerName, time.Now().Unix())
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      LeaderElectionLockName,
			Namespace: configNamespace,
		},
		Client: clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				logAlways("Started leading")
				runController(ctx)
			},
			OnStoppedLeading: func() {
				logAlways("Stopped leading")
				os.Exit(0)
			},
			OnNewLeader: func(identity string) {
				if identity != id {
					logInfo("new-leader", "New leader elected: %s", identity)
				}
			},
		},
	})
}

func buildConfig() (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
			&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: masterURL}},
		).ClientConfig()
	}
	return rest.InClusterConfig()
}

func loadConfig() {
	// Load from ConfigMap or use defaults
	configLock.Lock()
	defer configLock.Unlock()

	// Read ENABLE_TSC_MANAGEMENT from env (allows disabling without ConfigMap change)
	enableTSCManagement := true
	if envVal := os.Getenv("ENABLE_TSC_MANAGEMENT"); envVal != "" {
		enableTSCManagement = envVal == "true"
	}

	config = &TSCConfig{
		DefaultConstraints: []corev1.TopologySpreadConstraint{
			{
				MaxSkew:           1,
				TopologyKey:       "topology.kubernetes.io/zone",
				WhenUnsatisfiable: corev1.DoNotSchedule,
			},
		},
		LogInterval:            15 * time.Minute,
		ReconcileInterval:      2 * time.Minute,
		GarbageCollectInterval: 5 * time.Minute,
		DryRun:                 true, // SAFETY: Default to dry-run mode
		EnableTSCManagement:    enableTSCManagement, // Default to enabled
	}

	// Try to load from ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps(configNamespace).Get(
		context.Background(), ConfigMapName, metav1.GetOptions{},
	)
	if err != nil {
		logWarn("config-load", "Failed to load ConfigMap, using defaults: %v", err)
		return
	}

	// Parse default constraints
	if constraintsJSON, ok := cm.Data["defaultConstraints"]; ok {
		var constraints []corev1.TopologySpreadConstraint
		if err := json.Unmarshal([]byte(constraintsJSON), &constraints); err == nil {
			config.DefaultConstraints = constraints
		}
	}

	// Parse intervals
	if logIntv, ok := cm.Data["logInterval"]; ok {
		if d, err := time.ParseDuration(logIntv); err == nil {
			config.LogInterval = d
			SetLogInterval(d)
		}
	}

	if recIntv, ok := cm.Data["reconcileInterval"]; ok {
		if d, err := time.ParseDuration(recIntv); err == nil {
			config.ReconcileInterval = d
		}
	}

	if gcIntv, ok := cm.Data["garbageCollectInterval"]; ok {
		if d, err := time.ParseDuration(gcIntv); err == nil {
			config.GarbageCollectInterval = d
		}
	}

	// Parse exclusion rules
	if exclusionsJSON, ok := cm.Data["exclusions"]; ok {
		var rules []ExclusionRule
		if err := json.Unmarshal([]byte(exclusionsJSON), &rules); err == nil {
			rulesLock.Lock()
			exclusionRules = rules
			rulesLock.Unlock()
		}
	}

	// SAFETY: Parse dry-run mode (default: true for safety)
	if dryRunStr, ok := cm.Data["dryRun"]; ok {
		config.DryRun = dryRunStr != "false"
	}

	logInfo("config-loaded", "Configuration loaded successfully")
}

func runController(ctx context.Context) {
	factory := informers.NewSharedInformerFactory(clientset, time.Minute*5)

	// Create informers
	deploymentInformer := factory.Apps().V1().Deployments().Informer()
	statefulSetInformer := factory.Apps().V1().StatefulSets().Informer()
	configMapInformer := factory.Core().V1().ConfigMaps().Informer()

	// Add handlers for Deployments
	deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if d, ok := obj.(*appv1.Deployment); ok {
				// SAFETY: Guard against nil Replicas pointer
				replicas := 1
				if d.Spec.Replicas != nil {
					replicas = int(*d.Spec.Replicas)
				}
				createTSCForWorkload(ctx, "Deployment", d.Namespace, d.Name, d.Annotations, d.Spec.Template.Labels, replicas)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if d, ok := newObj.(*appv1.Deployment); ok {
				oldD, _ := oldObj.(*appv1.Deployment)
				handleWorkloadUpdate(ctx, "Deployment", d, oldD)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if d, ok := obj.(*appv1.Deployment); ok {
				deleteTSCForWorkload(d.Namespace, d.Name)
			}
		},
	})

	// Add handlers for StatefulSets
	statefulSetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if s, ok := obj.(*appv1.StatefulSet); ok {
				// SAFETY: Guard against nil Replicas pointer
				replicas := 1
				if s.Spec.Replicas != nil {
					replicas = int(*s.Spec.Replicas)
				}
				createTSCForWorkload(ctx, "StatefulSet", s.Namespace, s.Name, s.Annotations, s.Spec.Template.Labels, replicas)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if s, ok := newObj.(*appv1.StatefulSet); ok {
				oldS, _ := oldObj.(*appv1.StatefulSet)
				handleWorkloadUpdate(ctx, "StatefulSet", s, oldS)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if s, ok := obj.(*appv1.StatefulSet); ok {
				deleteTSCForWorkload(s.Namespace, s.Name)
			}
		},
	})

	// Watch ConfigMap for changes
	configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == ConfigMapName {
				logInfo("configmap-add", "ConfigMap added, reloading config")
				loadConfig()
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if cm, ok := newObj.(*corev1.ConfigMap); ok && cm.Name == ConfigMapName {
				logInfo("configmap-update", "ConfigMap updated, reloading config")
				loadConfig()
			}
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	logAlways("Controller started. Watching Deployments and StatefulSets...")

	// Start reconciliation loop
	go runReconcileLoop(ctx)

	// Start garbage collection
	go runGarbageCollection(ctx)

	<-ctx.Done()
}

func createTSCForWorkload(ctx context.Context, kind, namespace, name string, annotations, labels map[string]string, replicas int) {
	key := fmt.Sprintf("%s/%s/%s", kind, namespace, name)

	// Check if already processed
	workloadsLock.Lock()
	if processedWorkloads[key] {
		workloadsLock.Unlock()
		return
	}
	workloadsLock.Unlock()

	// Check bypass annotation
	if annotations != nil {
		if val, ok := annotations[AnnotationBypass]; ok && val == "true" {
			logDebug("tsc-bypass", "Skipping %s, bypass annotation present", key)
			return
		}
	}

	// Check exclusion rules
	if isExcluded(namespace, name, labels) {
		logDebug("tsc-excluded", "Skipping %s, excluded by rule", key)
		return
	}

	// Skip if replicas < 2
	if replicas < 2 {
		logDebug("tsc-low-replicas", "Skipping %s, replicas < 2", key)
		return
	}

	// Read config under lock once to avoid data race
	configLock.RLock()
	enableTSCMgmt := config.EnableTSCManagement
	dryRun := config.DryRun
	configLock.RUnlock()

	// Check if TSC management is enabled
	if !enableTSCMgmt {
		logDebug("tsc-disabled", "Skipping %s, TSC management disabled via ENABLE_TSC_MANAGEMENT", key)
		return
	}

	// Build constraints
	constraints := buildConstraints(namespace, name, annotations, labels)
	if len(constraints) == 0 {
		return
	}

	if dryRun {
		logInfo("tsc-dry-run", "[DRY-RUN] Would add %d TSC(s) to %s", len(constraints), key)
		// Don't mark as processed in dry-run so it will be re-evaluated when dryRun is disabled
		return
	}

	// SAFETY: Check if TSC already exists and matches desired state
	needsPatch := false
	switch kind {
	case "Deployment":
		dep, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			logError("tsc-get-fail", "Failed to get Deployment %s: %v", key, err)
			return
		}
		needsPatch = !topologySpreadConstraintsMatch(dep.Spec.Template.Spec.TopologySpreadConstraints, constraints)
	case "StatefulSet":
		sts, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			logError("tsc-get-fail", "Failed to get StatefulSet %s: %v", key, err)
			return
		}
		needsPatch = !topologySpreadConstraintsMatch(sts.Spec.Template.Spec.TopologySpreadConstraints, constraints)
	}

	if !needsPatch {
		logDebug("tsc-no-change", "TSC already configured for %s, skipping", key)
		workloadsLock.Lock()
		processedWorkloads[key] = true
		workloadsLock.Unlock()
		return
	}

	// Apply patch based on kind
	var err error
	switch kind {
	case "Deployment":
		err = patchDeploymentTSC(ctx, namespace, name, constraints)
	case "StatefulSet":
		err = patchStatefulSetTSC(ctx, namespace, name, constraints)
	}

	if err != nil {
		logError("tsc-patch-fail", "Failed to patch %s: %v", key, err)
		recorder.Eventf(&corev1.ObjectReference{
			Kind:      kind,
			Namespace: namespace,
			Name:      name,
		}, corev1.EventTypeWarning, "TSCAdditionFailed", "Failed to add topology spread constraints: %v", err)
		return
	}

	workloadsLock.Lock()
	processedWorkloads[key] = true
	workloadsLock.Unlock()

	logInfo("tsc-added", "Added TSC to %s", key)
	recorder.Eventf(&corev1.ObjectReference{
		Kind:      kind,
		Namespace: namespace,
		Name:      name,
	}, corev1.EventTypeNormal, "TSCAdded", "Topology spread constraints added")
}

// topologySpreadConstraintsMatch checks if existing TSCs match the desired constraints
// Prevents unnecessary rolling restarts when TSC is already configured correctly
func topologySpreadConstraintsMatch(existing, desired []corev1.TopologySpreadConstraint) bool {
	if len(existing) != len(desired) {
		return false
	}
	for i := range existing {
		if existing[i].MaxSkew != desired[i].MaxSkew {
			return false
		}
		if existing[i].TopologyKey != desired[i].TopologyKey {
			return false
		}
		if existing[i].WhenUnsatisfiable != desired[i].WhenUnsatisfiable {
			return false
		}
		if len(existing[i].MatchLabelKeys) != len(desired[i].MatchLabelKeys) {
			return false
		}
		for j := range existing[i].MatchLabelKeys {
			if existing[i].MatchLabelKeys[j] != desired[i].MatchLabelKeys[j] {
				return false
			}
		}
	}
	return true
}

func buildConstraints(namespace, name string, annotations, labels map[string]string) []corev1.TopologySpreadConstraint {
	configLock.RLock()
	defer configLock.RUnlock()

	// Check for full constraints override
	if annotations != nil {
		if overrideJSON, ok := annotations[AnnotationConstraints]; ok {
			var override []corev1.TopologySpreadConstraint
			if err := json.Unmarshal([]byte(overrideJSON), &override); err == nil {
				return override
			}
		}

		// Individual overrides
		maxSkew := 1
		topologyKey := "topology.kubernetes.io/zone"
		whenUnsatisfiable := corev1.DoNotSchedule

		if val, ok := annotations[AnnotationMaxSkew]; ok {
			if v, err := strconv.Atoi(val); err == nil && v > 0 {
				maxSkew = v
			}
		}
		if val, ok := annotations[AnnotationTopologyKey]; ok && val != "" {
			topologyKey = val
		}
		if val, ok := annotations[AnnotationWhenUnsat]; ok {
			if val == "ScheduleAnyway" {
				whenUnsatisfiable = corev1.ScheduleAnyway
			}
		}

		return []corev1.TopologySpreadConstraint{
			{
				MaxSkew:           int32(maxSkew),
				TopologyKey:       topologyKey,
				WhenUnsatisfiable: whenUnsatisfiable,
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: labels,
				},
			},
		}
	}

	// Use defaults with workload-specific selector
	constraints := make([]corev1.TopologySpreadConstraint, len(config.DefaultConstraints))
	for i, c := range config.DefaultConstraints {
		constraints[i] = c
		if constraints[i].LabelSelector == nil {
			constraints[i].LabelSelector = &metav1.LabelSelector{
				MatchLabels: labels,
			}
		}
	}

	return constraints
}

func patchDeploymentTSC(ctx context.Context, namespace, name string, constraints []corev1.TopologySpreadConstraint) error {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"topologySpreadConstraints": constraints,
				},
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = clientset.AppsV1().Deployments(namespace).Patch(
		ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{},
	)
	return err
}

func patchStatefulSetTSC(ctx context.Context, namespace, name string, constraints []corev1.TopologySpreadConstraint) error {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"topologySpreadConstraints": constraints,
				},
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = clientset.AppsV1().StatefulSets(namespace).Patch(
		ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{},
	)
	return err
}

func handleWorkloadUpdate(ctx context.Context, kind string, newObj, oldObj metav1.Object) {
	namespace := newObj.GetNamespace()
	name := newObj.GetName()
	key := fmt.Sprintf("%s/%s/%s", kind, namespace, name)

	newAnnotations := newObj.GetAnnotations()
	oldAnnotations := oldObj.GetAnnotations()

	// Check if bypass was added
	if oldAnnotations == nil || oldAnnotations[AnnotationBypass] != "true" {
		if newAnnotations != nil && newAnnotations[AnnotationBypass] == "true" {
			// Remove TSC
			removeTSCFromWorkload(ctx, kind, namespace, name)
			workloadsLock.Lock()
			delete(processedWorkloads, key)
			workloadsLock.Unlock()
			return
		}
	}

	// Check if bypass was removed
	if oldAnnotations != nil && oldAnnotations[AnnotationBypass] == "true" {
		if newAnnotations == nil || newAnnotations[AnnotationBypass] != "true" {
			// Re-process
			workloadsLock.Lock()
			delete(processedWorkloads, key)
			workloadsLock.Unlock()
			return
		}
	}
}

func removeTSCFromWorkload(ctx context.Context, kind, namespace, name string) error {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"topologySpreadConstraints": nil,
				},
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	var patchErr error
	switch kind {
	case "Deployment":
		_, patchErr = clientset.AppsV1().Deployments(namespace).Patch(
			ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{},
		)
	case "StatefulSet":
		_, patchErr = clientset.AppsV1().StatefulSets(namespace).Patch(
			ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{},
		)
	}

	if patchErr == nil {
		logInfo("tsc-removed", "Removed TSC from %s/%s/%s", kind, namespace, name)
	}

	return patchErr
}

func deleteTSCForWorkload(namespace, name string) {
	key := fmt.Sprintf("*/%s/%s", namespace, name)
	workloadsLock.Lock()
	for k := range processedWorkloads {
		if strings.HasSuffix(k, key[1:]) {
			delete(processedWorkloads, k)
		}
	}
	workloadsLock.Unlock()
}

func isExcluded(namespace, name string, labels map[string]string) bool {
	rulesLock.RLock()
	defer rulesLock.RUnlock()

	for _, rule := range exclusionRules {
		// Check namespace regex
		if rule.NamespaceRegex != "" {
			matched, _ := regexp.MatchString(rule.NamespaceRegex, namespace)
			if !matched {
				continue
			}
		}

		// Check name regex
		if rule.NameRegex != "" {
			matched, _ := regexp.MatchString(rule.NameRegex, name)
			if !matched {
				continue
			}
		}

		// Check labels
		if len(rule.Labels) > 0 {
			allMatch := true
			for k, v := range rule.Labels {
				if labels[k] != v {
					allMatch = false
					break
				}
			}
			if !allMatch {
				continue
			}
		}

		return true
	}

	return false
}

func runReconcileLoop(ctx context.Context) {
	configLock.RLock()
	interval := config.ReconcileInterval
	configLock.RUnlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			reconcileAllWorkloads(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func reconcileAllWorkloads(ctx context.Context) {
	logInfo("reconcile-start", "Starting full reconciliation")

	// SAFETY: Don't clear processed cache - createTSCForWorkload now uses
	// idempotent checks to avoid unnecessary rolling restarts

	// List all deployments
	deployments, err := clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		logError("reconcile-deployments", "Failed to list deployments: %v", err)
	} else {
		for _, d := range deployments.Items {
			if d.Spec.Replicas != nil && *d.Spec.Replicas >= 2 {
				createTSCForWorkload(ctx, "Deployment", d.Namespace, d.Name,
					d.Annotations, d.Spec.Template.Labels, int(*d.Spec.Replicas))
			}
		}
	}

	// List all statefulsets
	statefulsets, err := clientset.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		logError("reconcile-statefulsets", "Failed to list statefulsets: %v", err)
	} else {
		for _, s := range statefulsets.Items {
			if s.Spec.Replicas != nil && *s.Spec.Replicas >= 2 {
				createTSCForWorkload(ctx, "StatefulSet", s.Namespace, s.Name,
					s.Annotations, s.Spec.Template.Labels, int(*s.Spec.Replicas))
			}
		}
	}

	logInfo("reconcile-complete", "Reconciliation complete")
}

func runGarbageCollection(ctx context.Context) {
	configLock.RLock()
	interval := config.GarbageCollectInterval
	configLock.RUnlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			garbageCollectOrphanedTSCs(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func garbageCollectOrphanedTSCs(ctx context.Context) {
	logDebug("gc-start", "Starting garbage collection")

	// Check deployments
	deployments, err := clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	for _, d := range deployments.Items {
		if d.Spec.Template.Spec.TopologySpreadConstraints == nil {
			continue
		}

		// Check if has managed TSC
		hasManaged := false
		for _, c := range d.Spec.Template.Spec.TopologySpreadConstraints {
			if c.LabelSelector != nil && c.LabelSelector.MatchLabels != nil {
				if val, ok := c.LabelSelector.MatchLabels[ManagedByLabel]; ok && val == ManagedByValue {
					hasManaged = true
					break
				}
			}
		}

		if hasManaged && (d.Spec.Replicas == nil || *d.Spec.Replicas < 2) {
			// Remove TSC
			removeTSCFromWorkload(ctx, "Deployment", d.Namespace, d.Name)
		}
	}

	// Check statefulsets
	statefulsets, err := clientset.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	for _, s := range statefulsets.Items {
		if s.Spec.Template.Spec.TopologySpreadConstraints == nil {
			continue
		}

		hasManaged := false
		for _, c := range s.Spec.Template.Spec.TopologySpreadConstraints {
			if c.LabelSelector != nil && c.LabelSelector.MatchLabels != nil {
				if val, ok := c.LabelSelector.MatchLabels[ManagedByLabel]; ok && val == ManagedByValue {
					hasManaged = true
					break
				}
			}
		}

		if hasManaged && (s.Spec.Replicas == nil || *s.Spec.Replicas < 2) {
			removeTSCFromWorkload(ctx, "StatefulSet", s.Namespace, s.Name)
		}
	}

	logDebug("gc-complete", "Garbage collection complete")
}

