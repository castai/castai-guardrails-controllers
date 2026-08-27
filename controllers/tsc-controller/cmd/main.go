// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	workloadsclient "github.com/castai/castai-guardrails-controllers/clientset/versioned/typed/workloads/v1"
	"github.com/castai/castai-guardrails-controllers/snapshot"
)

const (
	ControllerName          = "castai-tsc-controller"
	ConfigMapNamespace      = "castai-agent"
	ConfigMapName           = "castai-tsc-controller-config"
	LeaderElectionLockName  = "castai-tsc-controller-leader"
	AnnotationBypass        = "workloads.cast.ai/tsc-bypass"
	AnnotationMaxSkew       = "workloads.cast.ai/tsc-maxSkew"
	AnnotationTopologyKey   = "workloads.cast.ai/tsc-topologyKey"
	AnnotationWhenUnsat     = "workloads.cast.ai/tsc-whenUnsatisfiable"
	AnnotationConstraints   = "workloads.cast.ai/tsc-constraints"
	ManagedByLabel          = "cast.ai/managed-by"
	ManagedByValue          = "tsc-controller"
	AnnotationTSCManaged    = "workloads.cast.ai/tsc-managed"
	TSCControllerFinalizer  = "workloads.cast.ai/castai-tsc-controller-finalizer"
)

var (
	masterURL       string
	kubeconfig      string
	configNamespace string

	clientset        *kubernetes.Clientset
	workloadsClient  workloadsclient.WorkloadsV1Interface
	recorder         record.EventRecorder
	config           *TSCConfig
	configLock       sync.RWMutex
	exclusionRules   []ExclusionRule
	rulesLock        sync.RWMutex
	processedWorkloads = make(map[string]bool)
	workloadsLock    sync.Mutex

	tscClient   *snapshot.TSCClient
	tscAccessor *snapshot.Accessor[*workloadsv1.TSCOriginal]
)

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

	// Create workloads typed client (for TSCOriginal snapshots).
	workloadsClient = workloadsclient.NewForConfigOrDie(cfg)

	// Create event recorder
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: clientset.CoreV1().Events(""),
	})
	recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{
		Component: ControllerName,
	})

	// Load initial config (so we know the operator namespace before wiring snapshot client).
	loadConfig()

	// Wire snapshot clients now that we know the operator namespace.
	tscClient = snapshot.NewTSCClientFromClient(workloadsClient, config.OperatorNamespace)
	tscAccessor = snapshot.NewTSCAccessor()
	logInfo("snapshot-client", "Snapshot client wired for namespace %s", config.OperatorNamespace)

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
				// Reconcile orphaned snapshots before the controller loop kicks in.
				if err := reconcileOrphanedSnapshots(ctx); err != nil {
					logError("orphan-reconcile", "Startup orphan reconcile had errors: %v", err)
				}
				// Strip stale managed annotations from workloads whose snapshot
				// CRD is missing (e.g. after an uninstall on a fresh install).
				if err := reconcileManagedAnnotations(ctx, clientset, config.OperatorNamespace, tscClient); err != nil {
					logWarn("managed-annot-reconcile", "Startup managed-annotation reconcile had errors: %v", err)
				}
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

// loadConfig reads the controller ConfigMap and produces a *TSCConfig via
// ParseTSCConfig. The previous version of this function hand-rolled the
// defaults and parsing; that responsibility now lives in configmap.go so the
// parser can be unit-tested in isolation.
func loadConfig() {
	configLock.Lock()
	defer configLock.Unlock()

	cm, err := clientset.CoreV1().ConfigMaps(configNamespace).Get(
		context.Background(), ConfigMapName, metav1.GetOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		logWarn("config-load", "Failed to load ConfigMap, using defaults: %v", err)
	}

	envVersion := os.Getenv("OPERATOR_VERSION")
	newCfg, parseErrs := ParseTSCConfig(cm, envVersion)
	for _, e := range parseErrs {
		logWarn("config-parse", "ConfigMap field error: %v", e)
	}

	// Exclusion rules are not part of TSCConfig; parse them here so they keep
	// their own lock and stay in sync with the previous behaviour.
	if cm != nil {
		if exclusionsJSON, ok := cm.Data["exclusions"]; ok {
			var rules []ExclusionRule
			if err := json.Unmarshal([]byte(exclusionsJSON), &rules); err == nil {
				rulesLock.Lock()
				exclusionRules = rules
				rulesLock.Unlock()
			}
		}
	}

	config = newCfg

	// Re-wire the snapshot client if the operator namespace changed.
	if tscClient != nil {
		tscClient = snapshot.NewTSCClientFromClient(workloadsClient, config.OperatorNamespace)
	}

	logInfo("config-loaded", "Configuration loaded successfully (mgmt=%v rollback=%v mode=%s snapshot=%v ns=%s)",
		config.ManagementEnabled, config.RollbackOnDisable, config.Mode, config.SnapshotEnabled, config.OperatorNamespace)
}

func runController(ctx context.Context) {
	factory := informers.NewSharedInformerFactory(clientset, time.Minute*5)

	// Create informers
	deploymentInformer := factory.Apps().V1().Deployments().Informer()
	statefulSetInformer := factory.Apps().V1().StatefulSets().Informer()
	configMapInformer := factory.Core().V1().ConfigMaps().Informer()

	// Track previous RollbackState to detect true→false transitions.
	var prevStateMu sync.Mutex
	var prevState RollbackState

	// Add handlers for Deployments
	deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if d, ok := obj.(*appsv1.Deployment); ok {
				replicas := 1
				if d.Spec.Replicas != nil {
					replicas = int(*d.Spec.Replicas)
				}
				createTSCForWorkload(ctx, "Deployment", d.Namespace, d.Name, d.Annotations, d.Spec.Template.Labels, replicas, d.UID)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if d, ok := newObj.(*appsv1.Deployment); ok {
				oldD, _ := oldObj.(*appsv1.Deployment)
				handleWorkloadUpdate(ctx, "Deployment", d, oldD)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if d, ok := obj.(*appsv1.Deployment); ok {
				deleteTSCForWorkload(d.Namespace, d.Name)
			}
		},
	})

	// Add handlers for StatefulSets
	statefulSetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if s, ok := obj.(*appsv1.StatefulSet); ok {
				replicas := 1
				if s.Spec.Replicas != nil {
					replicas = int(*s.Spec.Replicas)
				}
				createTSCForWorkload(ctx, "StatefulSet", s.Namespace, s.Name, s.Annotations, s.Spec.Template.Labels, replicas, s.UID)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if s, ok := newObj.(*appsv1.StatefulSet); ok {
				oldS, _ := oldObj.(*appsv1.StatefulSet)
				handleWorkloadUpdate(ctx, "StatefulSet", s, oldS)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if s, ok := obj.(*appsv1.StatefulSet); ok {
				deleteTSCForWorkload(s.Namespace, s.Name)
			}
		},
	})

	// Watch ConfigMap for changes
	configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok || cm.Name != ConfigMapName {
				return
			}
			logInfo("configmap-add", "ConfigMap added, reloading config")
			prevStateMu.Lock()
			oldState := prevState
			prevStateMu.Unlock()
			loadConfig()
			newState := currentRollbackState()
			prevStateMu.Lock()
			prevState = newState
			prevStateMu.Unlock()
			maybeTriggerRollback(ctx, oldState, newState)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			cm, ok := newObj.(*corev1.ConfigMap)
			if !ok || cm.Name != ConfigMapName {
				return
			}
			// Capture pre-update state from in-memory config (since loadConfig
			// hasn't run yet).
			prevStateMu.Lock()
			oldState := prevState
			prevStateMu.Unlock()
			logInfo("configmap-update", "ConfigMap updated, reloading config")
			loadConfig()
			newState := currentRollbackState()
			prevStateMu.Lock()
			prevState = newState
			prevStateMu.Unlock()
			maybeTriggerRollback(ctx, oldState, newState)
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// Seed prevState after cache sync so we can detect future transitions.
	prevStateMu.Lock()
	prevState = currentRollbackState()
	prevStateMu.Unlock()

	logAlways("Controller started. Watching Deployments and StatefulSets...")

	go runReconcileLoop(ctx)
	go runGarbageCollection(ctx)

	<-ctx.Done()
}

func currentRollbackState() RollbackState {
	configLock.RLock()
	defer configLock.RUnlock()
	if config == nil {
		return RollbackState{}
	}
	return config.StateOf()
}

func maybeTriggerRollback(ctx context.Context, oldState, newState RollbackState) {
	if oldState.ManagementEnabled && !newState.ManagementEnabled && newState.RollbackOnDisable {
		logInfo("rollback-trigger", "managementEnabled went true→false; triggering rollback asynchronously")
		go runRollback(ctx)
	}
}

func runRollback(ctx context.Context) {
	logger := snapshot.SimpleLogger{Info: logInfoSimple, Warn: logWarnSimple, Error: logErrorSimple}
	ns := config.OperatorNamespace
	if err := snapshot.Rollback(ctx,
		tscClient,
		*tscAccessor,
		logger,
		ns,
		snapshot.FinalizerName(ControllerName),
		tscInverseFn(clientset),
		func(ctx context.Context, snap *workloadsv1.TSCOriginal) error {
			return applyInversePatch(ctx, clientset, snap)
		},
	); err != nil {
		logError("rollback", "Rollback loop had errors: %v", err)
	}
}

// captureTSCOriginal captures the pre-castai topology spread constraints for a
// workload, if snapshotting is enabled and the workload is not yet managed.
// Skips capture when the CRD is missing from the cluster.
func captureTSCOriginal(ctx context.Context, kind, namespace, name string, obj metav1.Object, currentTSCs []corev1.TopologySpreadConstraint) {
	configLock.RLock()
	enabled := config.SnapshotEnabled
	controllerVersion := config.Version
	configLock.RUnlock()

	if !enabled {
		return
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	// Honor existing managed annotation (idempotent re-runs).
	if annotations[AnnotationTSCManaged] == "true" {
		return
	}

	// Honor user-set bypass.
	if annotations[AnnotationBypass] == "true" {
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

	originalPresent := currentTSCs != nil
	original := currentTSCs
	if !originalPresent {
		original = []corev1.TopologySpreadConstraint{}
	}

	newFn := func(id snapshot.WorkloadIdentity) (*workloadsv1.TSCOriginal, error) {
		crdName := snapshot.CollisionSafeName(id.Kind, id.Namespace, id.Name, id.UID)
		return &workloadsv1.TSCOriginal{
			ObjectMeta: metav1.ObjectMeta{
				Name:      crdName,
				Namespace: id.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": ControllerName,
				},
			},
			Spec: workloadsv1.TSCOriginalSpec{
				TargetRef: workloadsv1.TargetRef{
					APIVersion: id.APIVersion,
					Kind:       id.Kind,
					Namespace:  id.Namespace,
					Name:       id.Name,
					UID:        id.UID,
				},
				OriginalTSCs:        original,
				OriginalTSCsPresent: originalPresent,
				CapturedAt:          metav1.Now(),
				ControllerVersion:   controllerVersion,
			},
		}, nil
	}

	logger := snapshot.SimpleLogger{Info: logInfoSimple, Warn: logWarnSimple, Error: logErrorSimple}
	if err := snapshot.CaptureIfAbsent(ctx,
		tscClient, *tscAccessor, logger,
		config.OperatorNamespace,
		snapshot.FinalizerName(ControllerName),
		ControllerName,
		identity, newFn,
	); err != nil {
		logError("capture", "Capture failed for %s/%s/%s: %v", kind, namespace, name, err)
	}
}

// removeSnapshotFinalizer removes the tsc-controller finalizer from any
// TSCOriginal snapshot owned by this workload, so the workload can be deleted.
func removeSnapshotFinalizer(ctx context.Context, namespace, name string, uid types.UID) {
	if tscClient == nil || workloadsClient == nil {
		return
	}
	list, err := workloadsClient.TSCOriginals(config.OperatorNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		logWarn("snapshot-list", "Failed to list snapshots: %v", err)
		return
	}
	for i := range list.Items {
		snap := &list.Items[i]
		if snap.Spec.TargetRef.Namespace != namespace ||
			snap.Spec.TargetRef.Name != name ||
			snap.Spec.TargetRef.UID != uid {
			continue
		}
		if err := snapshot.RemoveFinalizer(ctx, tscClient, *tscAccessor,
			config.OperatorNamespace, snap.Name, snapshot.FinalizerName(ControllerName)); err != nil {
			logWarn("snapshot-finalizer", "Failed to remove finalizer from %s: %v", snap.Name, err)
		}
	}
}

// reconcileOrphanedSnapshots removes the finalizer (and therefore allows
// deletion) of any TSCOriginal whose target workload is missing or has a
// different UID. Called at controller startup.
func reconcileOrphanedSnapshots(ctx context.Context) error {
	if tscClient == nil {
		return nil
	}
	ns := config.OperatorNamespace
	list, err := tscClient.List(ctx, ns)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	for _, snap := range list {
		ref := snap.Spec.TargetRef
		// Already rolled back, leave alone.
		conds := tscAccessor.GetConditions(snap)
		if snapshot.IsRolledBack(conds) {
			continue
		}
		gone, err := targetGone(ctx, ref)
		if err != nil {
			logWarn("orphan-lookup", "Failed to lookup %s/%s/%s: %v", ref.Kind, ref.Namespace, ref.Name, err)
			continue
		}
		if gone {
			if err := snapshot.RemoveFinalizer(ctx, tscClient, *tscAccessor, ns, snap.Name, snapshot.FinalizerName(ControllerName)); err != nil {
				logWarn("orphan-finalizer", "Failed to remove finalizer from %s: %v", snap.Name, err)
			}
		}
	}
	return nil
}

func targetGone(ctx context.Context, ref workloadsv1.TargetRef) (bool, error) {
	var err error
	switch ref.Kind {
	case "Deployment":
		_, err = clientset.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	case "StatefulSet":
		_, err = clientset.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	default:
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

// tscSnapshotLookup is the minimal snapshot-client surface used by
// reconcileManagedAnnotations. Using an interface keeps the function mockable
// in tests without dragging in a fake typed clientset.
type tscSnapshotLookup interface {
	Get(ctx context.Context, namespace, name string) (*workloadsv1.TSCOriginal, error)
}

// reconcileManagedAnnotations walks Deployments and StatefulSets across all
// namespaces and removes the workloads.cast.ai/tsc-managed annotation from
// any workload whose TSCOriginal snapshot CRD is missing.
//
// Why this exists: when the controller is uninstalled, the managed annotation
// is left on existing workloads. After a fresh install, CaptureIfAbsent
// short-circuits on the annotation (lost-snapshot guard) and never captures,
// leaving those workloads un-rollable-back. This reconciler repairs the gap
// at startup so the next reconcile can capture a fresh snapshot.
//
// Errors are logged but not fatal — one bad workload must not block the rest.
func reconcileManagedAnnotations(
	ctx context.Context,
	cs kubernetes.Interface,
	operatorNS string,
	snapClient tscSnapshotLookup,
) error {
	if cs == nil || snapClient == nil || operatorNS == "" {
		return nil
	}
	deps, err := cs.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list deployments: %w", err)
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		if d.Annotations[AnnotationTSCManaged] != "true" {
			continue
		}
		if err := reconcileOneManagedWorkload(ctx, cs, snapClient, operatorNS, "Deployment", d.Namespace, d.Name, d.UID); err != nil {
			logWarn("managed-annot", "Failed to reconcile Deployment %s/%s: %v", d.Namespace, d.Name, err)
		}
	}
	ssets, err := cs.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list statefulsets: %w", err)
	}
	for i := range ssets.Items {
		s := &ssets.Items[i]
		if s.Annotations[AnnotationTSCManaged] != "true" {
			continue
		}
		if err := reconcileOneManagedWorkload(ctx, cs, snapClient, operatorNS, "StatefulSet", s.Namespace, s.Name, s.UID); err != nil {
			logWarn("managed-annot", "Failed to reconcile StatefulSet %s/%s: %v", s.Namespace, s.Name, err)
		}
	}
	return nil
}

// reconcileOneManagedWorkload checks whether the snapshot CRD for a single
// managed workload still exists and, if not, removes the managed annotation.
func reconcileOneManagedWorkload(
	ctx context.Context,
	cs kubernetes.Interface,
	snapClient tscSnapshotLookup,
	operatorNS, kind, namespace, name string,
	uid types.UID,
) error {
	crdName := snapshot.CollisionSafeName(kind, namespace, name, uid)
	if _, err := snapClient.Get(ctx, operatorNS, crdName); err == nil {
		// Snapshot exists, managed annotation is correct.
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get snapshot %s/%s: %w", operatorNS, crdName, err)
	}

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				AnnotationTSCManaged: nil,
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	switch kind {
	case "Deployment":
		_, err = cs.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	case "StatefulSet":
		_, err = cs.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	}
	if err != nil {
		return fmt.Errorf("patch %s %s/%s: %w", kind, namespace, name, err)
	}
	logWarn("managed-annot-stripped", "Removed stale %s annotation from %s %s/%s (snapshot %s/%s missing)", AnnotationTSCManaged, kind, namespace, name, operatorNS, crdName)
	return nil
}

func createTSCForWorkload(ctx context.Context, kind, namespace, name string, annotations, labels map[string]string, replicas int, uid types.UID) {
	key := fmt.Sprintf("%s/%s/%s", kind, namespace, name)

	workloadsLock.Lock()
	if processedWorkloads[key] {
		workloadsLock.Unlock()
		return
	}
	workloadsLock.Unlock()

	if annotations != nil {
		if val, ok := annotations[AnnotationBypass]; ok && val == "true" {
			logDebug("tsc-bypass", "Skipping %s, bypass annotation present", key)
			return
		}
	}

	if isExcluded(namespace, name, labels) {
		logDebug("tsc-excluded", "Skipping %s, excluded by rule", key)
		return
	}

	if replicas < 2 {
		logDebug("tsc-low-replicas", "Skipping %s, replicas < 2", key)
		return
	}

	configLock.RLock()
	managementEnabled := config.ManagementEnabled
	mode := config.Mode
	configLock.RUnlock()

	if !managementEnabled {
		logDebug("tsc-disabled", "Skipping %s, management disabled", key)
		return
	}

	constraints := buildConstraints(namespace, name, annotations, labels)
	if len(constraints) == 0 {
		return
	}

	// Capture snapshot before patching. The capture helper is a no-op when
	// SnapshotEnabled=false or when the workload is already marked managed.
	// In recommend mode we still capture so dry-runs build the rollback
	// history, but we never patch.
	var current []corev1.TopologySpreadConstraint
	var currentPresent bool
	switch kind {
	case "Deployment":
		d, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			current = d.Spec.Template.Spec.TopologySpreadConstraints
			currentPresent = d.Spec.Template.Spec.TopologySpreadConstraints != nil
			if current == nil {
				current = nil
			}
		}
	case "StatefulSet":
		s, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			current = s.Spec.Template.Spec.TopologySpreadConstraints
			currentPresent = s.Spec.Template.Spec.TopologySpreadConstraints != nil
			if current == nil {
				current = nil
			}
		}
	}
	captureTSCOriginalWithCurrent(ctx, kind, namespace, name, annotations, uid, current, currentPresent, nil, false)

	if mode == ModeRecommend {
		logInfo("tsc-recommend", "[RECOMMEND] Would add %d TSC(s) to %s", len(constraints), key)
		return
	}

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

	// Mark workload as managed so subsequent reconciles skip capture.
	if err := markWorkloadManaged(ctx, kind, namespace, name); err != nil {
		logWarn("tsc-managed-annot", "Failed to set managed annotation on %s: %v", key, err)
	}

	// Re-fetch the live workload and record the applied TSCs on the
	// existing snapshot. The pre-patch capture set OriginalTSCs and was a
	// no-op when a Ready snapshot already existed; we update applied state
	// in-place via Get+Update so CaptureIfAbsent's "skip when Ready" rule
	// doesn't drop the post-patch write.
	switch kind {
	case "Deployment":
		if d, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
			applied := d.Spec.Template.Spec.TopologySpreadConstraints
			appliedPresent := d.Spec.Template.Spec.TopologySpreadConstraints != nil
			recordAppliedTSCs(ctx, kind, namespace, name, uid, applied, appliedPresent)
		}
	case "StatefulSet":
		if s, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
			applied := s.Spec.Template.Spec.TopologySpreadConstraints
			appliedPresent := s.Spec.Template.Spec.TopologySpreadConstraints != nil
			recordAppliedTSCs(ctx, kind, namespace, name, uid, applied, appliedPresent)
		}
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

// captureTSCOriginalWithCurrent is the inner capture helper that takes an
// already-fetched "current TSCs" list and Present flag. applied/appliedPresent
// are recorded when capture happens after the controller's patch succeeded;
// pre-patch callers pass nil/false and update the existing snapshot via
// recordAppliedTSCs after the patch instead.
func captureTSCOriginalWithCurrent(ctx context.Context, kind, namespace, name string, annotations map[string]string, uid types.UID, current []corev1.TopologySpreadConstraint, present bool, applied []corev1.TopologySpreadConstraint, appliedPresent bool) {
	configLock.RLock()
	enabled := config.SnapshotEnabled
	controllerVersion := config.Version
	configLock.RUnlock()

	if !enabled {
		return
	}
	if annotations[AnnotationTSCManaged] == "true" {
		return
	}

	identity := snapshot.WorkloadIdentity{
		APIVersion:  "apps/v1",
		Kind:        kind,
		Namespace:   namespace,
		Name:        name,
		UID:         uid,
		Annotations: annotations,
	}

	newFn := func(id snapshot.WorkloadIdentity) (*workloadsv1.TSCOriginal, error) {
		crdName := snapshot.CollisionSafeName(id.Kind, id.Namespace, id.Name, id.UID)
		return &workloadsv1.TSCOriginal{
			ObjectMeta: metav1.ObjectMeta{
				Name:      crdName,
				Namespace: config.OperatorNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": ControllerName,
				},
			},
			Spec: workloadsv1.TSCOriginalSpec{
				TargetRef: workloadsv1.TargetRef{
					APIVersion: id.APIVersion,
					Kind:       id.Kind,
					Namespace:  id.Namespace,
					Name:       id.Name,
					UID:        id.UID,
				},
				OriginalTSCs:        current,
				OriginalTSCsPresent: present,
				AppliedTSCs:         applied,
				AppliedTSCsPresent:  appliedPresent,
				CapturedAt:          metav1.Now(),
				ControllerVersion:   controllerVersion,
			},
		}, nil
	}

	logger := snapshot.SimpleLogger{Info: logInfoSimple, Warn: logWarnSimple, Error: logErrorSimple}
	if err := snapshot.CaptureIfAbsent(ctx,
		tscClient, *tscAccessor, logger,
		config.OperatorNamespace,
		snapshot.FinalizerName(ControllerName),
		ControllerName,
		identity, newFn,
	); err != nil {
		logError("capture", "Capture failed for %s/%s/%s: %v", kind, namespace, name, err)
	}
}

// recordAppliedTSCs updates the Spec.AppliedTSCs/AppliedTSCsPresent fields of
// the existing snapshot in-place. CaptureIfAbsent is a no-op when a Ready
// snapshot already exists, so post-patch applied-state writes must go through
// Get+Update directly.
func recordAppliedTSCs(ctx context.Context, kind, namespace, name string, uid types.UID, applied []corev1.TopologySpreadConstraint, appliedPresent bool) {
	if tscClient == nil || tscAccessor == nil {
		return
	}
	configLock.RLock()
	operatorNS := config.OperatorNamespace
	configLock.RUnlock()

	crdName := snapshot.CollisionSafeName(kind, namespace, name, uid)
	existing, err := tscClient.Get(ctx, operatorNS, crdName)
	if err != nil {
		logWarn("applied-tsc", "Failed to load snapshot %s/%s for applied TSC update: %v", operatorNS, crdName, err)
		return
	}
	existing.Spec.AppliedTSCs = applied
	existing.Spec.AppliedTSCsPresent = appliedPresent
	if _, err := tscClient.Update(ctx, operatorNS, existing); err != nil {
		logWarn("applied-tsc", "Failed to update snapshot %s/%s with applied TSCs: %v", operatorNS, crdName, err)
		return
	}
	logInfo("applied-tsc", "Recorded applied TSCs on snapshot %s/%s", operatorNS, crdName)
}

// markWorkloadManaged sets the workloads.cast.ai/tsc-managed=true annotation
// on the workload using a Strategic Merge Patch that merges annotations.
func markWorkloadManaged(ctx context.Context, kind, namespace, name string) error {
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				AnnotationTSCManaged: "true",
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

// topologySpreadConstraintsMatch checks if existing TSCs match the desired constraints
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

	if annotations != nil {
		if overrideJSON, ok := annotations[AnnotationConstraints]; ok {
			var override []corev1.TopologySpreadConstraint
			if err := json.Unmarshal([]byte(overrideJSON), &override); err == nil {
				return override
			}
		}

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
	uid := newObj.GetUID()
	key := fmt.Sprintf("%s/%s/%s", kind, namespace, name)

	newAnnotations := newObj.GetAnnotations()
	oldAnnotations := oldObj.GetAnnotations()

	if oldAnnotations == nil || oldAnnotations[AnnotationBypass] != "true" {
		if newAnnotations != nil && newAnnotations[AnnotationBypass] == "true" {
			removeTSCFromWorkload(ctx, kind, namespace, name)
			removeSnapshotFinalizer(ctx, namespace, name, uid)
			workloadsLock.Lock()
			delete(processedWorkloads, key)
			workloadsLock.Unlock()
			return
		}
	}

	if oldAnnotations != nil && oldAnnotations[AnnotationBypass] == "true" {
		if newAnnotations == nil || newAnnotations[AnnotationBypass] != "true" {
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
		if rule.NamespaceRegex != "" {
			matched, _ := regexp.MatchString(rule.NamespaceRegex, namespace)
			if !matched {
				continue
			}
		}
		if rule.NameRegex != "" {
			matched, _ := regexp.MatchString(rule.NameRegex, name)
			if !matched {
				continue
			}
		}
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

	deployments, err := clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		logError("reconcile-deployments", "Failed to list deployments: %v", err)
	} else {
		for _, d := range deployments.Items {
			if d.Spec.Replicas != nil && *d.Spec.Replicas >= 2 {
				createTSCForWorkload(ctx, "Deployment", d.Namespace, d.Name,
					d.Annotations, d.Spec.Template.Labels, int(*d.Spec.Replicas), d.UID)
			}
		}
	}

	statefulsets, err := clientset.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		logError("reconcile-statefulsets", "Failed to list statefulsets: %v", err)
	} else {
		for _, s := range statefulsets.Items {
			if s.Spec.Replicas != nil && *s.Spec.Replicas >= 2 {
				createTSCForWorkload(ctx, "StatefulSet", s.Namespace, s.Name,
					s.Annotations, s.Spec.Template.Labels, int(*s.Spec.Replicas), s.UID)
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

	deployments, err := clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	for _, d := range deployments.Items {
		if d.Spec.Template.Spec.TopologySpreadConstraints == nil {
			continue
		}
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
			removeTSCFromWorkload(ctx, "Deployment", d.Namespace, d.Name)
		}
	}

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

// Adapter wrappers so snapshot.SimpleLogger can consume this controller's
// rate-limited log helpers (which take a key as the first argument).
func logInfoSimple(format string, args ...interface{})  { logInfo("tsc-snapshot", format, args...) }
func logWarnSimple(format string, args ...interface{})  { logWarn("tsc-snapshot", format, args...) }
func logErrorSimple(format string, args ...interface{}) { logError("tsc-snapshot", format, args...) }
