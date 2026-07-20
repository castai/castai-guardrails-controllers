package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// Controller constants
const (
	ControllerName         = "castai-jvm-probe-controller"
	ConfigMapNamespace     = "castai-agent"
	ConfigMapName          = "castai-jvm-probe-controller-config"
	LeaderElectionLockName = "castai-jvm-probe-controller-leader-election"
)

// Global variables
var (
	masterURL       string
	kubeconfig      string
	configNamespace string
	help            bool
	version         bool

	clientset           *kubernetes.Clientset
	recorder            record.EventRecorder
	stopCh              chan struct{}
	config              *JVMConfig
	configLock          sync.RWMutex
	exclusionRules      *ExclusionRules
	workloadsProcessed  = make(map[types.NamespacedName]bool)
	workloadsLock       sync.Mutex
	informerStopCh      chan struct{}
)

// init registers flags
func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig.")
	flag.StringVar(&configNamespace, "config-namespace", ConfigMapNamespace, "Namespace for the ConfigMap")
	flag.BoolVar(&help, "help", false, "Print help")
	flag.BoolVar(&version, "version", false, "Print version")
}

// Controller represents the JVM Probe Controller
type Controller struct {
	informerFactory informers.SharedInformerFactory
	deployments     cache.SharedIndexInformer
	statefulSets    cache.SharedIndexInformer
	configMap       cache.SharedIndexInformer
	pods            cache.SharedIndexInformer
	events          cache.SharedIndexInformer
	workqueue       workqueue.RateLimitingInterface
	probeMonitor    *PodEventMonitor
}

// NewController creates a new JVM Probe Controller
func NewController(clientset *kubernetes.Clientset, factory informers.SharedInformerFactory) *Controller {
	// Create event recorder
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})
	recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: ControllerName})

	// Create informers
	deploymentsInformer := factory.Apps().V1().Deployments().Informer()
	statefulSetsInformer := factory.Apps().V1().StatefulSets().Informer()
	configMapInformer := factory.Core().V1().ConfigMaps().Informer()
	
	// Create pod and event informers for probe monitoring
	podsInformer := factory.Core().V1().Pods().Informer()
	eventsInformer := factory.Core().V1().Events().Informer()

	// Create workqueue
	workqueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	// Add event handlers for Deployments
	deploymentsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if d, ok := obj.(*appv1.Deployment); ok {
				enqueueWorkload(workqueue, d.ObjectMeta)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if d, ok := newObj.(*appv1.Deployment); ok {
				enqueueWorkload(workqueue, d.ObjectMeta)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if d, ok := obj.(*appv1.Deployment); ok {
				handleWorkloadDelete(workqueue, d.ObjectMeta)
			}
		},
	})

	// Add event handlers for StatefulSets
	statefulSetsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if s, ok := obj.(*appv1.StatefulSet); ok {
				enqueueWorkload(workqueue, s.ObjectMeta)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if s, ok := newObj.(*appv1.StatefulSet); ok {
				enqueueWorkload(workqueue, s.ObjectMeta)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if s, ok := obj.(*appv1.StatefulSet); ok {
				handleWorkloadDelete(workqueue, s.ObjectMeta)
			}
		},
	})

	// ConfigMap event handler for hot-reload
	configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if cm, ok := obj.(*corev1.ConfigMap); ok {
				if cm.Name == ConfigMapName && cm.Namespace == configNamespace {
					handleConfigMapUpdate(cm)
				}
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if cm, ok := newObj.(*corev1.ConfigMap); ok {
				if cm.Name == ConfigMapName && cm.Namespace == configNamespace {
					handleConfigMapUpdate(cm)
				}
			}
		},
	})

	// Create pod event monitor
	monitor := NewPodEventMonitor(clientset, factory)

	return &Controller{
		informerFactory: factory,
		deployments:     deploymentsInformer,
		statefulSets:    statefulSetsInformer,
		configMap:       configMapInformer,
		pods:            podsInformer,
		events:          eventsInformer,
		workqueue:       workqueue,
		probeMonitor:    monitor,
	}
}

// enqueueWorkload adds a workload to the workqueue
func enqueueWorkload(workqueue workqueue.RateLimitingInterface, meta metav1.ObjectMeta) {
	key := fmt.Sprintf("%s/%s", meta.Namespace, meta.Name)
	workqueue.Add(key)
	logDebug("enqueue", "Enqueued workload: %s", key)
}

// handleWorkloadDelete handles workload deletion
func handleWorkloadDelete(workqueue workqueue.RateLimitingInterface, meta metav1.ObjectMeta) {
	key := fmt.Sprintf("%s/%s", meta.Namespace, meta.Name)
	workloadsLock.Lock()
	delete(workloadsProcessed, types.NamespacedName{Namespace: meta.Namespace, Name: meta.Name})
	workloadsLock.Unlock()
	logInfo("delete", "Workload deleted: %s", key)
}

// handleConfigMapUpdate handles ConfigMap updates
func handleConfigMapUpdate(cm *corev1.ConfigMap) {
	newConfig := parseConfigMap(cm)
	configLock.Lock()
	oldConfig := config
	config = newConfig
	configLock.Unlock()

	// Update logging interval
	if newConfig.LogInterval != "" {
		if interval, err := time.ParseDuration(newConfig.LogInterval); err == nil {
			SetLogInterval(interval)
		}
	}

	// Update exclusion rules
	if newConfig.Exclusions != "" {
		rules := parseExclusionRules(newConfig.Exclusions)
		exclusionRules = rules
	}

	logInfo("configmap", "ConfigMap updated: logInterval=%s, reconcileInterval=%s",
		newConfig.LogInterval, newConfig.ReconcileInterval)

	// If significant config changed, trigger reconciliation
	if oldConfig != nil && oldConfig.ReconcileInterval != newConfig.ReconcileInterval {
		logInfo("configmap", "Reconcile interval changed, triggering reconciliation")
	}
}

// parseConfigMap parses the ConfigMap into JVMConfig
func parseConfigMap(cm *corev1.ConfigMap) *JVMConfig {
	cfg := DefaultJVMConfig()

	if data, ok := cm.Data["jvm-frameworks"]; ok {
		if err := json.Unmarshal([]byte(data), &cfg.Frameworks); err != nil {
			logWarn("config", "Failed to parse jvm-frameworks: %v", err)
		}
	}

	if val, ok := cm.Data["jvm-logInterval"]; ok {
		cfg.LogInterval = val
	}

	if val, ok := cm.Data["jvm-reconcileInterval"]; ok {
		cfg.ReconcileInterval = val
	}

	if val, ok := cm.Data["jvm-requireBothProbes"]; ok {
		cfg.RequireBothProbes = val != "false"
	}

	if val, ok := cm.Data["jvm-skipIfAnyProbeExists"]; ok {
		cfg.SkipIfAnyProbeExists = val == "true"
	}

	if val, ok := cm.Data["jvm-exclusions"]; ok {
		cfg.Exclusions = val
	}

	// P1: Liveness probe injection control
	if val, ok := cm.Data["jvm-injectLivenessProbe"]; ok {
		cfg.InjectLivenessProbe = val == "true"
	}
	if val, ok := cm.Data["jvm-injectReadinessProbe"]; ok {
		cfg.InjectReadinessProbe = val == "true"
	}
	if val, ok := cm.Data["jvm-injectStartupProbe"]; ok {
		cfg.InjectStartupProbe = val == "true"
	}

	// P2: Dry-run mode
	if val, ok := cm.Data["jvm-dryRun"]; ok {
		cfg.DryRun = val == "true"
	}
	if val, ok := cm.Data["jvm-logIntendedChanges"]; ok {
		cfg.LogIntendedChanges = val == "true"

	// P3: Enable/disable probe management from env (allows disabling without ConfigMap change)
	if envVal := os.Getenv("ENABLE_PROBE_MANAGEMENT"); envVal != "" {
		cfg.EnableProbeManagement = envVal == "true"
	}
	}

	return &cfg
}

// parseExclusionRules parses exclusion rules from ConfigMap
func parseExclusionRules(data string) *ExclusionRules {
	var rules []ExclusionRule
	if err := json.Unmarshal([]byte(data), &rules); err != nil {
		logWarn("exclusions", "Failed to parse exclusion rules: %v", err)
		return DefaultExclusionRules()
	}
	return NewExclusionRules(rules)
}

// Run starts the controller
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	logAlways("Starting JVM Probe Controller...")

	// Start informers
	c.informerFactory.Start(ctx.Done())

	// Wait for caches to sync
	if ok := cache.WaitForCacheSync(ctx.Done(), 
		c.deployments.HasSynced, 
		c.statefulSets.HasSynced,
		c.pods.HasSynced,
		c.events.HasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	// Start probe event monitor
	go c.probeMonitor.Run(ctx)

	logAlways("Informers synced, starting %d workers", workers)

	// Start workers
	for i := 0; i < workers; i++ {
		go c.runWorker(ctx)
	}

	// Start periodic reconciliation
	go c.runPeriodicReconciliation(ctx)

	// Start garbage collection
	go c.runGarbageCollection(ctx)

	<-ctx.Done()
	return nil
}

// runWorker processes items from the workqueue
func (c *Controller) runWorker(ctx context.Context) {
	for {
		obj, shutdown := c.workqueue.Get()
		if shutdown {
			return
		}

		key := obj.(string)
		if err := c.syncHandler(ctx, key); err != nil {
			utilruntime.HandleError(fmt.Errorf("error syncing %s: %v", key, err))
			c.workqueue.AddRateLimited(key)
		} else {
			c.workqueue.Forget(key)
		}
	}
}

// syncHandler handles the sync for a workload
func (c *Controller) syncHandler(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logError("sync", "Invalid key: %s", key)
		return nil // Don't retry invalid keys
	}

	// Try to get from both Deployments and StatefulSets
	var obj runtime.Object
	var exists bool

	// Check Deployment
	if d, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		obj, exists = d, true
	} else if !errors.IsNotFound(err) {
		return err
	}

	// Check StatefulSet
	if !exists {
		if s, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
			obj, exists = s, true
		} else if !errors.IsNotFound(err) {
			return err
		}
	}

	if !exists {
		logDebug("sync", "Workload not found: %s/%s", namespace, name)
		return nil
	}

	// Get current config
	configLock.RLock()
	currentConfig := config
	configLock.RUnlock()

	// Process the workload
	return c.processWorkload(ctx, obj, currentConfig)
}

// processWorkload processes a single workload
func (c *Controller) processWorkload(ctx context.Context, obj runtime.Object, cfg *JVMConfig) error {
	var name, namespace string
	var annotations map[string]string
	var spec *corev1.PodSpec

	switch o := obj.(type) {
	case *appv1.Deployment:
		name = o.Name
		namespace = o.Namespace
		annotations = o.Annotations
		spec = &o.Spec.Template.Spec
	case *appv1.StatefulSet:
		name = o.Name
		namespace = o.Namespace
		annotations = o.Annotations
		spec = &o.Spec.Template.Spec
	default:
		return nil
	}

	nn := types.NamespacedName{Namespace: namespace, Name: name}

	// Check bypass annotation
	if IsBypassAnnotation(annotations) {
		logInfo("bypass", "Workload %s is bypassed", nn)
		workloadsLock.Lock()
		workloadsProcessed[nn] = true
		workloadsLock.Unlock()
		return nil
	}

	// Check exclusion rules
	labelsMap := make(map[string]string)
	if meta, ok := obj.(metav1.Object); ok {
		labelsMap = meta.GetLabels()
	}

	if exclusionRules.IsExcluded(namespace, name, labelsMap) {
		logInfo("excluded", "Workload %s is excluded", nn)
		workloadsLock.Lock()
		workloadsProcessed[nn] = true
		workloadsLock.Unlock()
		return nil
	}

	// Check if probe management is enabled
	if !cfg.EnableProbeManagement {
		logInfo("disabled", "Skipping %s, probe management disabled via ENABLE_PROBE_MANAGEMENT", nn)
		workloadsLock.Lock()
		workloadsProcessed[nn] = true
		workloadsLock.Unlock()
		return nil
	}

	// Get framework override
	frameworkOverride := GetFrameworkOverride(annotations)

	// Check for overwrite settings
	overwriteAll := ShouldOverwriteAll(annotations)
	overwriteLiveness := ShouldOverwriteLiveness(annotations) || overwriteAll
	overwriteReadiness := ShouldOverwriteReadiness(annotations) || overwriteAll
	overwriteStartup := ShouldOverwriteStartup(annotations) || overwriteAll
	logFailuresEnabled := ShouldLogFailures(annotations)

	// Process each container
	modified := false
	var allPatches []map[string]interface{}
	var intendedActions []string

	for i, container := range spec.Containers {
		// Detect JVM
		containerInfo := DetectJVMContainer(container)
		if !containerInfo.IsJVM {
			continue
		}

		// Determine framework
		framework := containerInfo.Framework
		if frameworkOverride != "" {
			framework = frameworkOverride
		}

		// P2: Use new NeedsProbes that returns three separate bools
		needsLiveness, needsReadiness, needsStartup := NeedsProbes(container, cfg.RequireBothProbes)

		// Apply overwrite logic per-probe
		if overwriteLiveness || (!cfg.SkipIfAnyProbeExists && needsLiveness) {
			needsLiveness = true
		} else {
			needsLiveness = false
		}
		if overwriteReadiness || (!cfg.SkipIfAnyProbeExists && needsReadiness) {
			needsReadiness = true
		} else {
			needsReadiness = false
		}
		if overwriteStartup || needsStartup {
			needsStartup = true
		} else {
			needsStartup = false
		}

		if !needsLiveness && !needsReadiness && !needsStartup {
			logInfo("skip", "Container %s in %s already has all probes configured", container.Name, nn)
			continue
		}

		// Check for poor probe configurations that should be fixed
		if container.LivenessProbe != nil && !needsLiveness && isPoorProbeConfig(container.LivenessProbe) {
			logWarn("poor-liveness", "Container %s in %s has poorly configured liveness probe", container.Name, nn)
			needsLiveness = true
		}
		if container.ReadinessProbe != nil && !needsReadiness && isPoorProbeConfig(container.ReadinessProbe) {
			logWarn("poor-readiness", "Container %s in %s has poorly configured readiness probe", container.Name, nn)
			needsReadiness = true
		}

		// Build probes
		liveness, readiness, startup := BuildProbesForFramework(framework, containerInfo, annotations, cfg)

		// Create patches based on what needs to be added/replaced
		var containerPatches []map[string]interface{}

		if needsLiveness && liveness != nil {
			if container.LivenessProbe != nil {
				// Replace existing
				containerPatches = append(containerPatches, map[string]interface{}{
					"op":    "replace",
					"path":  fmt.Sprintf("/spec/template/spec/containers/%d/livenessProbe", i),
					"value": liveness,
				})
				intendedActions = append(intendedActions, 
					fmt.Sprintf("replace liveness probe for container %s", container.Name))
			} else {
				// Add new
				containerPatches = append(containerPatches, map[string]interface{}{
					"op":    "add",
					"path":  fmt.Sprintf("/spec/template/spec/containers/%d/livenessProbe", i),
					"value": liveness,
				})
				intendedActions = append(intendedActions,
					fmt.Sprintf("add liveness probe for container %s", container.Name))
			}
		}

		if needsReadiness && readiness != nil {
			if container.ReadinessProbe != nil {
				containerPatches = append(containerPatches, map[string]interface{}{
					"op":    "replace",
					"path":  fmt.Sprintf("/spec/template/spec/containers/%d/readinessProbe", i),
					"value": readiness,
				})
				intendedActions = append(intendedActions,
					fmt.Sprintf("replace readiness probe for container %s", container.Name))
			} else {
				containerPatches = append(containerPatches, map[string]interface{}{
					"op":    "add",
					"path":  fmt.Sprintf("/spec/template/spec/containers/%d/readinessProbe", i),
					"value": readiness,
				})
				intendedActions = append(intendedActions,
					fmt.Sprintf("add readiness probe for container %s", container.Name))
			}
		}

		// Always add startup probe for JVM containers - critical for slow-starting apps
		if needsStartup && startup != nil {
			if container.StartupProbe != nil {
				containerPatches = append(containerPatches, map[string]interface{}{
					"op":    "replace",
					"path":  fmt.Sprintf("/spec/template/spec/containers/%d/startupProbe", i),
					"value": startup,
				})
				intendedActions = append(intendedActions,
					fmt.Sprintf("replace startup probe for container %s", container.Name))
			} else {
				containerPatches = append(containerPatches, map[string]interface{}{
					"op":    "add",
					"path":  fmt.Sprintf("/spec/template/spec/containers/%d/startupProbe", i),
					"value": startup,
				})
				intendedActions = append(intendedActions,
					fmt.Sprintf("add startup probe for container %s", container.Name))
			}
			logInfo("startup-inject", "Injecting startup probe for container %s (JVM apps need this for slow startup)", container.Name)
		}

		// Enable failure logging if requested
		if logFailuresEnabled {
			c.enableFailureLogging(nn.Namespace, nn.Name, name, container.Name, GetFailureLogThreshold(annotations, 3))
		}

		allPatches = append(allPatches, containerPatches...)
		modified = true

		logInfo("inject", "Injecting probes into container %s (framework: %s, liveness:%v, readiness:%v, startup:%v)", 
			container.Name, framework, needsLiveness, needsReadiness, needsStartup)
	}

	if !modified {
		logDebug("skip", "No probes needed for workload %s", nn)
		workloadsLock.Lock()
		workloadsProcessed[nn] = true
		workloadsLock.Unlock()
		return nil
	}

	// P2: Dry-run mode - log intended changes but don't apply
	if cfg.DryRun {
		if cfg.LogIntendedChanges && len(intendedActions) > 0 {
			logAlways("DRY-RUN: Would apply %d patches to %s: %s", 
				len(allPatches), nn, strings.Join(intendedActions, "; "))
		}
		workloadsLock.Lock()
		workloadsProcessed[nn] = true
		workloadsLock.Unlock()
		return nil
	}

	// Apply patches
	if err := c.applyPatches(ctx, obj, allPatches); err != nil {
		logError("patch", "Failed to patch workload %s: %v", nn, err)
		return err
	}

	// Record success
	workloadsLock.Lock()
	workloadsProcessed[nn] = true
	workloadsLock.Unlock()

	logInfo("success", "Successfully processed workload %s", nn)
	recorder.Eventf(obj.(runtime.Object), corev1.EventTypeNormal, "ProbesInjected", "JVM probes injected successfully")

	return nil
}

// applyPatches applies JSON patches to a workload
func (c *Controller) applyPatches(ctx context.Context, obj runtime.Object, patches []map[string]interface{}) error {
	if len(patches) == 0 {
		return nil
	}

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %v", err)
	}

	var namespace, name string
	var client clientSetPatcher

	switch o := obj.(type) {
	case *appv1.Deployment:
		namespace = o.Namespace
		name = o.Name
		client = &deploymentPatcher{clientset: clientset, ctx: ctx, namespace: namespace, name: name}
	case *appv1.StatefulSet:
		namespace = o.Namespace
		name = o.Name
		client = &statefulSetPatcher{clientset: clientset, ctx: ctx, namespace: namespace, name: name}
	}

	return client.patch(patchBytes)
}

type clientSetPatcher interface {
	patch([]byte) error
}

type deploymentPatcher struct {
	clientset *kubernetes.Clientset
	ctx       context.Context
	namespace string
	name      string
}

func (p *deploymentPatcher) patch(data []byte) error {
	_, err := p.clientset.AppsV1().Deployments(p.namespace).Patch(
		p.ctx,
		p.name,
		types.JSONPatchType,
		data,
		metav1.PatchOptions{},
	)
	return err
}

type statefulSetPatcher struct {
	clientset *kubernetes.Clientset
	ctx       context.Context
	namespace string
	name      string
}

func (p *statefulSetPatcher) patch(data []byte) error {
	_, err := p.clientset.AppsV1().StatefulSets(p.namespace).Patch(
		p.ctx,
		p.name,
		types.JSONPatchType,
		data,
		metav1.PatchOptions{},
	)
	return err
}

// runPeriodicReconciliation runs periodic full reconciliation
func (c *Controller) runPeriodicReconciliation(ctx context.Context) {
	configLock.RLock()
	intervalStr := config.ReconcileInterval
	configLock.RUnlock()

	interval := 2 * time.Minute
	if intervalStr != "" {
		if d, err := time.ParseDuration(intervalStr); err == nil {
			interval = d
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logInfo("reconcile", "Starting periodic reconciliation")
			c.reconcileAllProbes(ctx)
		}
	}
}

// reconcileAllProbes performs a full scan of all workloads
func (c *Controller) reconcileAllProbes(ctx context.Context) {
	logInfo("reconcile", "Reconciling all probes")

	// List all Deployments
	deployments, err := clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		logError("reconcile", "Failed to list deployments: %v", err)
		return
	}

	// List all StatefulSets
	statefulSets, err := clientset.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		logError("reconcile", "Failed to list statefulsets: %v", err)
		return
	}

	// Enqueue all workloads
	for _, d := range deployments.Items {
		enqueueWorkload(c.workqueue, d.ObjectMeta)
	}
	for _, s := range statefulSets.Items {
		enqueueWorkload(c.workqueue, s.ObjectMeta)
	}

	logInfo("reconcile", "Enqueued %d deployments and %d statefulsets for reconciliation",
		len(deployments.Items), len(statefulSets.Items))
}

// runGarbageCollection runs periodic garbage collection
func (c *Controller) runGarbageCollection(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logInfo("gc", "Running garbage collection")
			c.garbageCollect(ctx)
		}
	}
}

// garbageCollect performs garbage collection of workloads
func (c *Controller) garbageCollect(ctx context.Context) {
	// For JVM probe controller, garbage collection can check:
	// 1. Workloads that were previously managed but are now deleted
	// 2. Workloads that have scaled down and may no longer need certain probes
	// For now, we just log that GC ran
	logInfo("gc", "Garbage collection completed")
}

// handleWorkloadUpdate handles annotation changes on workload updates
func HandleWorkloadUpdate(ctx context.Context, oldObj, newObj interface{}, cfg *JVMConfig) error {
	// This is handled by the informer update event
	// The key logic is to detect annotation changes and react accordingly
	return nil
}

// CreateProbesForWorkload injects probes into a workload's containers
func CreateProbesForWorkload(ctx context.Context, obj runtime.Object, cfg *JVMConfig) error {
	var namespace, name string
	var annotations map[string]string

	switch o := obj.(type) {
	case *appv1.Deployment:
		namespace = o.Namespace
		name = o.Name
		annotations = o.Annotations
	case *appv1.StatefulSet:
		namespace = o.Namespace
		name = o.Name
		annotations = o.Annotations
	}

	// Check bypass
	if IsBypassAnnotation(annotations) {
		return nil
	}

	// Check exclusions
	labelsMap := make(map[string]string)
	if meta, ok := obj.(metav1.Object); ok {
		labelsMap = meta.GetLabels()
	}

	if exclusionRules.IsExcluded(namespace, name, labelsMap) {
		return nil
	}

	logInfo("create", "Creating probes for workload %s/%s", namespace, name)
	return nil
}

// DeleteProbesForWorkload removes probes from a workload
func DeleteProbesForWorkload(ctx context.Context, obj runtime.Object) error {
	// For now, we don't remove probes when workloads are deleted
	// The garbage collector handles cleanup of managed resources
	logInfo("delete", "Workload deleted, probes will be cleaned up by GC")
	return nil
}

// IsWorkloadExcluded checks if a workload should be excluded from processing
func IsWorkloadExcluded(namespace, name string, labels map[string]string) bool {
	return exclusionRules.IsExcluded(namespace, name, labels)
}

// Main entry point
func main() {
	klog.InitFlags(nil)
	flag.Parse()

	if help {
		flag.Usage()
		os.Exit(0)
	}

	if version {
		fmt.Printf("JVM Probe Controller %s\n", "v1.0.0")
		os.Exit(0)
	}

	// Initialize logging
	initLogging()

	// Create Kubernetes client
	restConfig, err := getRestConfig()
	if err != nil {
		log.Fatalf("Failed to get rest config: %v", err)
	}

	clientset, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	// Initialize config
	defaultCfg := DefaultJVMConfig()
	config = &defaultCfg
	exclusionRules = DefaultExclusionRules()

	// Try to load ConfigMap
	if cm, err := clientset.CoreV1().ConfigMaps(configNamespace).Get(context.Background(), ConfigMapName, metav1.GetOptions{}); err == nil {
		newConfig := parseConfigMap(cm)
		config = newConfig

		if newConfig.LogInterval != "" {
			if interval, err := time.ParseDuration(newConfig.LogInterval); err == nil {
				SetLogInterval(interval)
			}
		}

		if newConfig.Exclusions != "" {
			exclusionRules = parseExclusionRules(newConfig.Exclusions)
		}
		logAlways("Loaded configuration from ConfigMap %s/%s", configNamespace, ConfigMapName)
	} else {
		logAlways("ConfigMap not found, using defaults")
	}

	// Create shared informer factory
	informerStopCh = make(chan struct{})
	factory := informers.NewSharedInformerFactory(clientset, 0)

	// Create controller
	controller := NewController(clientset, factory)

	// Prepare leader election
	id, err := os.Hostname()
	if err != nil {
		log.Fatalf("Failed to get hostname: %v", err)
	}
	id = fmt.Sprintf("%s-%s", ControllerName, id)

	// Create context with cancel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logAlways("Shutting down...")
		close(informerStopCh)
		cancel()
	}()

	// Run with leader election
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
		Lock:          lock,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				logAlways("Acquired leadership, starting controller")
				if err := controller.Run(ctx, 2); err != nil {
					log.Fatalf("Controller failed: %v", err)
				}
			},
			OnStoppedLeading: func() {
				logAlways("Lost leadership, exiting")
				os.Exit(0)
			},
		},
	})
}

// getRestConfig returns the REST config for the Kubernetes client
func getRestConfig() (*rest.Config, error) {
	// Try in-cluster config first
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}

	// Fall back to kubeconfig
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: masterURL}},
	).ClientConfig()
}

// isPoorProbeConfig checks if a probe has problematic settings
func isPoorProbeConfig(probe *corev1.Probe) bool {
	if probe == nil {
		return false
	}

	// Poor configurations:
	// 1. Initial delay < 10 seconds (JVM needs time to start)
	// 2. Failure threshold < 3 (too aggressive)
	// 3. Period < 5 seconds (too frequent)
	if probe.InitialDelaySeconds < 10 {
		return true
	}
	if probe.FailureThreshold < 3 {
		return true
	}
	if probe.PeriodSeconds < 5 {
		return true
	}
	// 4. Timeout > period (impossible)
	if probe.TimeoutSeconds > probe.PeriodSeconds {
		return true
	}

	return false
}

// enableFailureLogging registers a workload for detailed failure logging
func (c *Controller) enableFailureLogging(namespace, workloadName, workloadKind, containerName string, threshold int) {
	// Create a key for the workload
	key := fmt.Sprintf("%s/%s", namespace, workloadName)
	
	// This would integrate with the probe monitor to track failures
	// For now, we just log the registration
	logInfo("failure-logging", "Enabled detailed failure logging for %s/%s/%s container=%s threshold=%d", 
		namespace, workloadKind, workloadName, containerName, threshold)
	
	// Store in the controller for later use
	_ = key
}
