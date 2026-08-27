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

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	workloadsclient "github.com/castai/castai-guardrails-controllers/clientset/versioned/typed/workloads/v1"
	"github.com/castai/castai-guardrails-controllers/snapshot"
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
	workloadsClient     workloadsclient.WorkloadsV1Interface
	recorder            record.EventRecorder
	config              *JVMConfig
	configLock          sync.RWMutex
	exclusionRules      *ExclusionRules
	workloadsProcessed  = make(map[types.NamespacedName]bool)
	workloadsLock       sync.Mutex
	informerStopCh      chan struct{}

	jvmClient   *snapshot.JVMClient
	jvmAccessor *snapshot.Accessor[*workloadsv1.JVMProbeOriginal]
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
	// PR3: remove snapshot finalizer so the JVMProbeOriginal CRD can be deleted.
	removeJVMFinalizer(context.Background(), meta.Namespace, meta.Name, meta.UID)
	logInfo("delete", "Workload deleted: %s", key)
}

// handleConfigMapUpdate handles ConfigMap updates
func handleConfigMapUpdate(cm *corev1.ConfigMap) {
	configLock.Lock()
	var oldState RollbackState
	if config != nil {
		oldState = config.StateOf()
	}
	configLock.Unlock()

	envVersion := os.Getenv("OPERATOR_VERSION")
	newConfig, parseErrs := ParseJVMConfig(cm, envVersion)
	for _, e := range parseErrs {
		logWarn("config-parse", "ConfigMap field error: %v", e)
	}

	configLock.Lock()
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

	// Re-wire snapshot client if operator namespace changed.
	if jvmClient != nil {
		jvmClient = snapshot.NewJVMClientFromClient(workloadsClient, newConfig.OperatorNamespace)
	}

	logInfo("configmap", "ConfigMap updated: logInterval=%s, reconcileInterval=%s mgmt=%v rollback=%v mode=%s",
		newConfig.LogInterval, newConfig.ReconcileInterval,
		newConfig.ManagementEnabled, newConfig.RollbackOnDisable, newConfig.Mode)

	newState := newConfig.StateOf()
	if oldState.ManagementEnabled && !newState.ManagementEnabled && newState.RollbackOnDisable {
		logInfo("rollback-trigger", "managementEnabled went true→false; triggering rollback asynchronously")
		go runJVMRollback()
	}
}

// runJVMRollback executes the rollback loop over all JVMProbeOriginal snapshots.
func runJVMRollback() {
	if jvmClient == nil || jvmAccessor == nil {
		return
	}
	configLock.RLock()
	ns := config.OperatorNamespace
	configLock.RUnlock()

	logger := snapshot.SimpleLogger{
		Info:  func(format string, args ...interface{}) { logInfo("jvm-snapshot", format, args...) },
		Warn:  func(format string, args ...interface{}) { logWarn("jvm-snapshot", format, args...) },
		Error: func(format string, args ...interface{}) { logError("jvm-snapshot", format, args...) },
	}
	if err := snapshot.Rollback(context.Background(),
		jvmClient,
		*jvmAccessor,
		logger,
		ns,
		snapshot.FinalizerName(ControllerName),
		jvmInverseFn(clientset),
		func(ctx context.Context, snap *workloadsv1.JVMProbeOriginal) error {
			return applyInverseJVMPatch(ctx, clientset, snap)
		},
	); err != nil {
		logError("rollback", "JVM rollback loop had errors: %v", err)
	}
}

// captureJVMOriginal captures the per-container probes on a workload before
// patch, if snapshotting is enabled. Skips when the workload is already
// annotated as managed (idempotent re-runs). applied/appliedPresent are
// recorded when capture happens after the controller's patch succeeded;
// pre-patch callers pass nil/false and update the existing snapshot via
// recordAppliedJVMContainers after the patch instead.
func captureJVMOriginal(ctx context.Context, kind, namespace, name string, obj metav1.Object, appliedContainers map[string]workloadsv1.ContainerProbes, appliedPresent bool) {
	if jvmClient == nil || jvmAccessor == nil {
		return
	}
	configLock.RLock()
	enabled := config.SnapshotEnabled
	controllerVersion := config.Version
	operatorNS := config.OperatorNamespace
	configLock.RUnlock()
	if !enabled {
		return
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if snapshot.IsManaged(annotations, ControllerName) {
		return
	}

	identity := snapshot.WorkloadIdentity{
		APIVersion:  "apps/v1",
		Kind:        kind,
		Namespace:   namespace,
		Name:        name,
		UID:         obj.GetUID(),
		Generation:  obj.GetGeneration(),
		Annotations: annotations,
	}

	newFn := func(id snapshot.WorkloadIdentity) (*workloadsv1.JVMProbeOriginal, error) {
		crdName := snapshot.CollisionSafeName(id.Kind, id.Namespace, id.Name, id.UID)
		var containers map[string]workloadsv1.ContainerProbes
		switch id.Kind {
		case "Deployment":
			d, err := clientset.AppsV1().Deployments(id.Namespace).Get(ctx, id.Name, metav1.GetOptions{})
			if err == nil {
				containers = buildJVMSnapshotContainers(d.Spec.Template.Spec.Containers)
			}
		case "StatefulSet":
			s, err := clientset.AppsV1().StatefulSets(id.Namespace).Get(ctx, id.Name, metav1.GetOptions{})
			if err == nil {
				containers = buildJVMSnapshotContainers(s.Spec.Template.Spec.Containers)
			}
		}
		return &workloadsv1.JVMProbeOriginal{
			ObjectMeta: metav1.ObjectMeta{
				Name:      crdName,
				Namespace: id.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": ControllerName,
				},
			},
			Spec: workloadsv1.JVMProbeOriginalSpec{
				TargetRef: workloadsv1.TargetRef{
					APIVersion: id.APIVersion,
					Kind:       id.Kind,
					Namespace:  id.Namespace,
					Name:       id.Name,
					UID:        id.UID,
				},
				OriginalContainers:      containers,
				AppliedContainers:       appliedContainers,
				AppliedContainersPresent: appliedPresent,
				CapturedAt:              metav1.Now(),
				ControllerVersion:       controllerVersion,
			},
		}, nil
	}

	logger := snapshot.SimpleLogger{
		Info:  func(format string, args ...interface{}) { logInfo("jvm-snapshot", format, args...) },
		Warn:  func(format string, args ...interface{}) { logWarn("jvm-snapshot", format, args...) },
		Error: func(format string, args ...interface{}) { logError("jvm-snapshot", format, args...) },
	}
	if err := snapshot.CaptureIfAbsent(ctx,
		jvmClient, *jvmAccessor, logger,
		operatorNS,
		snapshot.FinalizerName(ControllerName),
		ControllerName,
		identity, newFn,
	); err != nil {
		logError("capture", "Capture failed for %s/%s/%s: %v", kind, namespace, name, err)
	}
}

// recordAppliedJVMContainers updates Spec.AppliedContainers and
// AppliedContainersPresent on the existing snapshot in-place. CaptureIfAbsent
// is a no-op when a Ready snapshot already exists, so post-patch applied-state
// writes must go through Get+Update directly.
func recordAppliedJVMContainers(ctx context.Context, kind, namespace, name string, uid types.UID, applied map[string]workloadsv1.ContainerProbes, appliedPresent bool) {
	if jvmClient == nil || jvmAccessor == nil {
		return
	}
	configLock.RLock()
	operatorNS := config.OperatorNamespace
	configLock.RUnlock()

	crdName := snapshot.CollisionSafeName(kind, namespace, name, uid)
	existing, err := jvmClient.Get(ctx, operatorNS, crdName)
	if err != nil {
		logWarn("applied-jvm", "Failed to load snapshot %s/%s for applied containers update: %v", operatorNS, crdName, err)
		return
	}
	existing.Spec.AppliedContainers = applied
	existing.Spec.AppliedContainersPresent = appliedPresent
	if _, err := jvmClient.Update(ctx, operatorNS, existing); err != nil {
		logWarn("applied-jvm", "Failed to update snapshot %s/%s with applied containers: %v", operatorNS, crdName, err)
		return
	}
	logInfo("applied-jvm", "Recorded applied containers on snapshot %s/%s", operatorNS, crdName)
}

// buildJVMSnapshotContainers extracts per-container probes for snapshot.
func buildJVMSnapshotContainers(cs []corev1.Container) map[string]workloadsv1.ContainerProbes {
	containers := make(map[string]workloadsv1.ContainerProbes, len(cs))
	for _, c := range cs {
		containers[c.Name] = workloadsv1.ContainerProbes{
			LivenessProbe:    c.LivenessProbe,
			ReadinessProbe:   c.ReadinessProbe,
			StartupProbe:     c.StartupProbe,
			LivenessPresent:  c.LivenessProbe != nil,
			ReadinessPresent: c.ReadinessProbe != nil,
			StartupPresent:   c.StartupProbe != nil,
		}
	}
	return containers
}

// removeJVMFinalizer removes the controller finalizer from snapshots owned
// by the given workload UID, so the workload can be deleted.
func removeJVMFinalizer(ctx context.Context, namespace, name string, uid types.UID) {
	if jvmClient == nil || workloadsClient == nil {
		return
	}
	configLock.RLock()
	operatorNS := config.OperatorNamespace
	configLock.RUnlock()
	list, err := workloadsClient.JVMProbeOriginals(operatorNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		logWarn("snapshot-list", "Failed to list JVM snapshots: %v", err)
		return
	}
	for i := range list.Items {
		snap := &list.Items[i]
		if snap.Spec.TargetRef.Namespace != namespace ||
			snap.Spec.TargetRef.Name != name ||
			snap.Spec.TargetRef.UID != uid {
			continue
		}
		if err := snapshot.RemoveFinalizer(ctx, jvmClient, *jvmAccessor,
			operatorNS, snap.Name, snapshot.FinalizerName(ControllerName)); err != nil {
			logWarn("snapshot-finalizer", "Failed to remove finalizer from %s: %v", snap.Name, err)
		}
	}
}

// markJVMWorkloadManaged sets the workloads.cast.ai/jvm-probe-controller-managed=true
// annotation on the workload via Strategic Merge Patch.
func markJVMWorkloadManaged(ctx context.Context, kind, namespace, name string) error {
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				snapshot.ManagedAnnotationName(ControllerName): "true",
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	switch kind {
	case "Deployment":
		_, err = clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	case "StatefulSet":
		_, err = clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	}
	return err
}

// reconcileJVMSnapshots removes finalizers from orphaned snapshots at startup.
func reconcileJVMSnapshots(ctx context.Context) error {
	if jvmClient == nil {
		return nil
	}
	configLock.RLock()
	operatorNS := config.OperatorNamespace
	configLock.RUnlock()
	list, err := jvmClient.List(ctx, operatorNS)
	if err != nil {
		return fmt.Errorf("list jvm snapshots: %w", err)
	}
	for _, snap := range list {
		conds := jvmAccessor.GetConditions(snap)
		if snapshot.IsRolledBack(conds) {
			continue
		}
		ref := snap.Spec.TargetRef
		var err error
		switch ref.Kind {
		case "Deployment":
			_, err = clientset.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		case "StatefulSet":
			_, err = clientset.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		default:
			continue
		}
		if errors.IsNotFound(err) {
			if err := snapshot.RemoveFinalizer(ctx, jvmClient, *jvmAccessor, operatorNS, snap.Name, snapshot.FinalizerName(ControllerName)); err != nil {
				logWarn("orphan-finalizer", "Failed to remove JVM finalizer from %s: %v", snap.Name, err)
			}
		}
	}
	return nil
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
	var metaObj metav1.Object
	var kind string

	switch o := obj.(type) {
	case *appv1.Deployment:
		name = o.Name
		namespace = o.Namespace
		annotations = o.Annotations
		spec = &o.Spec.Template.Spec
		metaObj = o
		kind = "Deployment"
	case *appv1.StatefulSet:
		name = o.Name
		namespace = o.Namespace
		annotations = o.Annotations
		spec = &o.Spec.Template.Spec
		metaObj = o
		kind = "StatefulSet"
	default:
		return nil
	}

	nn := types.NamespacedName{Namespace: namespace, Name: name}

	// PR3: managementEnabled is the freeze toggle. If disabled, return early.
	if cfg != nil && !cfg.ManagementEnabled {
		logInfo("disabled", "Skipping %s, management disabled", nn)
		return nil
	}

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
	if metaObj != nil {
		labelsMap = metaObj.GetLabels()
	}

	if exclusionRules.IsExcluded(namespace, name, labelsMap) {
		logInfo("excluded", "Workload %s is excluded", nn)
		workloadsLock.Lock()
		workloadsProcessed[nn] = true
		workloadsLock.Unlock()
		return nil
	}

	// Check if probe management is enabled
	if !cfg.ManagementEnabled {
		logInfo("disabled", "Skipping %s, probe management disabled via MANAGEMENT_ENABLED", nn)
		// Don't mark as processed so it will be reprocessed when re-enabled
		return nil
	}

	// PR3: recommend mode = capture-only, do not patch.
	if cfg.Mode == ModeRecommend {
		captureJVMOriginal(ctx, kind, namespace, name, metaObj, nil, false)
		logInfo("recommend", "[RECOMMEND] Would inject probes into %s", nn)
		return nil
	}

	// PR3: capture snapshot before first patch (no-op if already captured).
	captureJVMOriginal(ctx, kind, namespace, name, metaObj, nil, false)

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

	// PR3: recommend mode - log intended changes but don't apply (snapshots
	// already captured above).
	if cfg.Mode == ModeRecommend {
		if cfg.LogIntendedChanges && len(intendedActions) > 0 {
			logAlways("RECOMMEND: Would apply %d patches to %s: %s",
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

	// Re-fetch the live workload and record the applied container probes on
	// the existing snapshot. The pre-patch capture was a no-op when a Ready
	// snapshot already existed; we update applied state in-place via
	// Get+Update so CaptureIfAbsent's "skip when Ready" rule doesn't drop
	// the post-patch write.
	if live, lerr := c.fetchLive(ctx, kind, namespace, name); lerr == nil {
		if liveSpec, ok := liveSpecOf(live); ok {
			applied := buildJVMSnapshotContainers(liveSpec.Containers)
			appliedPresent := liveSpec.Containers != nil || applied != nil
			recordAppliedJVMContainers(ctx, kind, namespace, name, metaObj.GetUID(), applied, appliedPresent)
		}
	}

	// PR3: mark workload as managed so subsequent reconciles skip capture.
	if err := markJVMWorkloadManaged(ctx, kind, namespace, name); err != nil {
		logWarn("managed-annot", "Failed to set managed annotation on %s: %v", nn, err)
	}

	// Record success
	workloadsLock.Lock()
	workloadsProcessed[nn] = true
	workloadsLock.Unlock()

	logInfo("success", "Successfully processed workload %s", nn)
	recorder.Eventf(obj, corev1.EventTypeNormal, "ProbesInjected", "JVM probes injected successfully")

	return nil
}

// fetchLive returns the current Deployments/StatefulSets object for the
// given kind/name. Used to read post-patch state for applied-snapshot
// recording.
func (c *Controller) fetchLive(ctx context.Context, kind, namespace, name string) (runtime.Object, error) {
	switch kind {
	case "Deployment":
		return clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	case "StatefulSet":
		return clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	default:
		return nil, fmt.Errorf("unsupported kind: %s", kind)
	}
}

// liveSpecOf returns the pod template spec from a Deployment or StatefulSet.
func liveSpecOf(obj runtime.Object) (*corev1.PodSpec, bool) {
	switch o := obj.(type) {
	case *appv1.Deployment:
		return &o.Spec.Template.Spec, true
	case *appv1.StatefulSet:
		return &o.Spec.Template.Spec, true
	}
	return nil, false
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
		newConfig, parseErrs := ParseJVMConfig(cm, os.Getenv("OPERATOR_VERSION"))
		for _, e := range parseErrs {
			logWarn("config-parse", "ConfigMap field error: %v", e)
		}
		config = newConfig

		if newConfig.LogInterval != "" {
			if interval, err := time.ParseDuration(newConfig.LogInterval); err == nil {
				SetLogInterval(interval)
			}
		}

		if newConfig.Exclusions != "" {
			exclusionRules = parseExclusionRules(newConfig.Exclusions)
		}
		logAlways("Loaded configuration from ConfigMap %s/%s (mgmt=%v rollback=%v mode=%s snapshot=%v ns=%s)",
			configNamespace, ConfigMapName,
			newConfig.ManagementEnabled, newConfig.RollbackOnDisable, newConfig.Mode,
			newConfig.SnapshotEnabled, newConfig.OperatorNamespace)
	} else {
		logAlways("ConfigMap not found, using defaults")
	}

	// Create workloads typed client and wire snapshot clients.
	workloadsClient = workloadsclient.NewForConfigOrDie(restConfig)
	jvmClient = snapshot.NewJVMClientFromClient(workloadsClient, config.OperatorNamespace)
	jvmAccessor = snapshot.NewJVMProbeAccessor()
	logInfo("snapshot-client", "Snapshot client wired for namespace %s", config.OperatorNamespace)

	// Reconcile orphaned JVM snapshots at startup.
	if err := reconcileJVMSnapshots(context.Background()); err != nil {
		logWarn("orphan-reconcile", "Startup JVM orphan reconcile had errors: %v", err)
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
