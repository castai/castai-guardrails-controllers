// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

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
	"k8s.io/client-go/tools/record"

	workloadsv1 "github.com/castai/castai-guardrails-controllers/apis/workloads/v1"
	workloadsclient "github.com/castai/castai-guardrails-controllers/clientset/versioned/typed/workloads/v1"
	"github.com/castai/castai-guardrails-controllers/snapshot"
)

const (
	ControllerName         = "castai-tsc-controller"
	ConfigMapNamespace     = "castai-agent"
	ConfigMapName          = "castai-tsc-controller-config"
	AnnotationBypass       = "workloads.cast.ai/tsc-bypass"
	AnnotationMaxSkew      = "workloads.cast.ai/tsc-maxSkew"
	AnnotationTopologyKey  = "workloads.cast.ai/tsc-topologyKey"
	AnnotationWhenUnsat    = "workloads.cast.ai/tsc-whenUnsatisfiable"
	AnnotationConstraints  = "workloads.cast.ai/tsc-constraints"
	ManagedByLabel         = "cast.ai/managed-by"
	ManagedByValue         = "tsc-controller"
	AnnotationTSCManaged   = "workloads.cast.ai/tsc-managed"
	TSCControllerFinalizer = "workloads.cast.ai/castai-tsc-controller-finalizer"
	// ControllerSelfLabel marks the controller's own pod template so the
	// controller can recognise and exclude its own workload without relying
	// on the Helm release name.
	ControllerSelfLabel     = "workloads.cast.ai/tsc-controller"
	ControllerSelfLabelTrue = "true"
)

var (
	masterURL       string
	kubeconfig      string
	configNamespace string
	webhookAddr     string
	healthAddr      string
	tlsCertFile     string
	tlsKeyFile      string

	clientset       kubernetes.Interface
	clientsetLock   sync.RWMutex
	workloadsClient workloadsclient.WorkloadsV1Interface
	recorder        record.EventRecorder
	config          *TSCConfig
	configLock      sync.RWMutex
	exclusionRules  []ExclusionRule
	rulesLock       sync.RWMutex

	// runRollbackFn is a function-typed indirection over runRollback so
	// tests can inject a synchronous spy for the rollback path. The
	// production code uses the default value; tests reassign it under
	// their own mutex and restore it on cleanup. The namespace argument
	// is passed explicitly so the rollback goroutine never has to read
	// the mutable global `config` pointer.
	runRollbackFn = runRollback

	tscClient   *snapshot.TSCClient
	tscAccessor *snapshot.Accessor[*workloadsv1.TSCOriginal]
)

// ExclusionRule mirrors the JSON document stored in the ConfigMap under
// "exclusions". An empty field matches anything; all set fields must
// match (logical AND).
type ExclusionRule struct {
	NamespaceRegex string            `json:"namespaceRegex"`
	NameRegex      string            `json:"nameRegex"`
	Labels         map[string]string `json:"labels"`
}

// getClientset returns the package-level clientset under clientsetLock.
// The webhook admission path and tests use this helper to read the global
// pointer safely; in production the variable is assigned once at startup,
// but the lock keeps the pattern correct under -race if a future change
// ever swaps the pointer (e.g., for credential rotation).
func getClientset() kubernetes.Interface {
	clientsetLock.RLock()
	defer clientsetLock.RUnlock()
	return clientset
}

// setClientset assigns the package-level clientset under clientsetLock.
// Production code calls this exactly once during startup; tests use it to
// swap in a fake clientset for the duration of a single test case.
func setClientset(cs kubernetes.Interface) {
	clientsetLock.Lock()
	defer clientsetLock.Unlock()
	clientset = cs
}

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	flag.StringVar(&masterURL, "master", "", "Kubernetes API server address")
	flag.StringVar(&configNamespace, "config-namespace", ConfigMapNamespace, "ConfigMap namespace")

	flag.StringVar(&webhookAddr, "webhook-addr", ":8443", "Address the admission webhook server listens on (host:port). Use :0 for an ephemeral port in tests.")
	flag.StringVar(&healthAddr, "health-addr", ":8080", "Address the plain-HTTP health server listens on (host:port). Used for kubelet readiness/liveness probes. Use :0 for an ephemeral port in tests.")
	flag.StringVar(&tlsCertFile, "tls-cert-file", "/etc/webhook/certs/tls.crt", "Path to the TLS certificate (PEM) for the webhook server.")
	flag.StringVar(&tlsKeyFile, "tls-key-file", "/etc/webhook/certs/tls.key", "Path to the TLS private key (PEM) for the webhook server.")
}

func main() {
	flag.Parse()

	initLogging()

	// Build config
	restConfig, err := buildConfig()
	if err != nil {
		logAlways("Error building kubeconfig: %v", err)
		os.Exit(1)
	}

	// Create clientset
	typedClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		logAlways("Error creating clientset: %v", err)
		os.Exit(1)
	}
	clientset = typedClient

	// Create workloads typed client (for TSCOriginal snapshots).
	workloadsClient = workloadsclient.NewForConfigOrDie(restConfig)

	// Create event recorder (kept for downstream tooling; webhook itself
	// does not emit events today).
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: typedClient.CoreV1().Events(""),
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

	// One-shot startup cleanup so the rollout from the legacy informer-based
	// controller to the webhook-based controller leaves no stale state.
	// reconcileOrphanedSnapshots drops the finalizer on snapshots whose
	// target workload is gone. reconcileManagedAnnotations strips the
	// workloads.cast.ai/tsc-managed annotation from workloads whose
	// TSCOriginal CRD is missing — this lets the new controller recapture
	// snapshots after a fresh install.
	if err := reconcileOrphanedSnapshots(ctx); err != nil {
		logError("orphan-reconcile", "Startup orphan reconcile had errors: %v", err)
	}
	if err := reconcileManagedAnnotations(ctx, typedClient, config.OperatorNamespace, tscClient); err != nil {
		logWarn("managed-annot-reconcile", "Startup managed-annotation reconcile had errors: %v", err)
	}

	// Build the ConfigMap-only informer factory and the webhook server.
	// The Deployment/StatefulSet informer loops were retired in the
	// webhook migration; per-Pod admission is the runtime path now.
	factory := informers.NewSharedInformerFactory(clientset, time.Minute*5)
	controller := NewController(clientset, factory)

	mutateHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlePodAdmission(w, r)
	})
	webhookServer := NewWebhookServer(webhookAddr, tlsCertFile, tlsKeyFile, mutateHandler)
	healthServer := NewHealthServer(healthAddr)

	logAlways("Starting TSC admission webhook on %s (tls=%s,%s) and health server on %s", webhookAddr, tlsCertFile, tlsKeyFile, healthAddr)
	if err := controller.Run(ctx, webhookServer, healthServer); err != nil {
		logAlways("Controller run failed: %v", err)
		os.Exit(1)
	}
}

func buildConfig() (*rest.Config, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
			&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: masterURL}},
		).ClientConfig()
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	// The admission webhook resolves workload owners (ReplicaSet -> Deployment)
	// on every Pod CREATE. With many concurrent pod creations the default
	// client-go rate limit (QPS=5, Burst=10) causes throttling and webhook
	// timeouts, which silently skips mutations because failurePolicy=Ignore.
	// Defaults here are high enough for burst admission traffic without being
	// unbounded.
	cfg.QPS = 50
	cfg.Burst = 100
	return cfg, nil
}

// Controller owns the ConfigMap informer that hot-reloads TSCConfig. The
// Deployment/StatefulSet informer loops have been retired; the runtime path
// is the admission webhook (pod_mutation.go + webhook.go).
type Controller struct {
	informerFactory informers.SharedInformerFactory
	configMap       cache.SharedIndexInformer
	// ctx is captured at Run time so the ConfigMap event handler can
	// trigger asynchronous rollback. The handler is registered before
	// Run captures ctx, so a brief window exists where the handler runs
	// without ctx — the handler must tolerate that (see below).
	ctx context.Context
}

// NewController creates a Controller and registers the ConfigMap informer
// event handler. Only the ConfigMap is watched; Deployment/StatefulSet
// informers were retired in the webhook migration.
func NewController(clientset kubernetes.Interface, factory informers.SharedInformerFactory) *Controller {
	configMapInformer := factory.Core().V1().ConfigMaps().Informer()
	configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok || cm.Name != ConfigMapName || cm.Namespace != configNamespace {
				return
			}
			logInfo("configmap-add", "ConfigMap added, reloading config")
			onConfigMapEvent()
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			cm, ok := newObj.(*corev1.ConfigMap)
			if !ok || cm.Name != ConfigMapName || cm.Namespace != configNamespace {
				return
			}
			logInfo("configmap-update", "ConfigMap updated, reloading config")
			onConfigMapEvent()
		},
	})
	return &Controller{
		informerFactory: factory,
		configMap:       configMapInformer,
	}
}

// Run starts the controller: it boots the informer factory, waits for the
// ConfigMap cache to sync, then runs the admission webhook server and the
// plain-HTTP health server and blocks until ctx is canceled or either
// server reports a fatal error.
//
// Both servers share the same derived context, so a single ctx cancel
// shuts down both. If one server returns an error, Run cancels the
// derived context so the other server also shuts down cleanly. They run
// in independent goroutines so a panic or slow shutdown in one does not
// block the other.
//
// Unlike the legacy informer-based loop, this Run does NOT participate in
// leader election. Admission webhooks must accept traffic on every replica
// so Pod creation is never dropped during a leader transition.
func (c *Controller) Run(ctx context.Context, webhookServer *WebhookServer, healthServer *HealthServer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.ctx = ctx

	// Start informers
	c.informerFactory.Start(ctx.Done())

	// Wait for caches to sync. We only need the ConfigMap cache; the
	// factory does not have any other informer registered.
	if ok := cache.WaitForCacheSync(ctx.Done(), c.configMap.HasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logAlways("ConfigMap informer synced; admission webhook is the runtime path")

	webhookErr := make(chan error, 1)
	go func() { webhookErr <- webhookServer.Start(ctx) }()
	healthErr := make(chan error, 1)
	go func() { healthErr <- healthServer.Start(ctx) }()

	select {
	case <-ctx.Done():
		// Both servers observe ctx.Done() internally and perform graceful
		// shutdown. Wait for them so the function does not return until
		// the listeners are fully released.
		<-webhookErr
		<-healthErr
		return nil
	case err := <-webhookErr:
		// Cancel so the health server also shuts down. We then wait for
		// its graceful shutdown before returning so listeners are
		// released.
		cancel()
		<-healthErr
		return err
	case err := <-healthErr:
		// Symmetric: cancel so the webhook server also shuts down.
		cancel()
		<-webhookErr
		return err
	}
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
	//
	// Always reassign exclusionRules under rulesLock when the ConfigMap
	// exists, including the case where the key is absent or empty: in that
	// case we set rules to nil so workloads that were previously excluded
	// stop being skipped on the next admission cycle. Silently keeping the
	// stale slice would contradict the contract that exclusion-rule edits
	// take effect on the next Pod admission.
	if cm != nil {
		var rules []ExclusionRule
		if exclusionsJSON, ok := cm.Data["exclusions"]; ok && exclusionsJSON != "" {
			if err := json.Unmarshal([]byte(exclusionsJSON), &rules); err != nil {
				logWarn("config-parse", "Failed to parse exclusions: %v", err)
			}
		}
		rulesLock.Lock()
		exclusionRules = rules
		rulesLock.Unlock()
	}

	config = newCfg

	// Re-wire the snapshot client if the operator namespace changed.
	if tscClient != nil {
		tscClient = snapshot.NewTSCClientFromClient(workloadsClient, config.OperatorNamespace)
	}

	logInfo("config-loaded", "Configuration loaded successfully (mgmt=%v rollback=%v mode=%s snapshot=%v skipSingleReplica=%v ns=%s)",
		config.ManagementEnabled, config.RollbackOnDisable, config.Mode, config.SnapshotEnabled, config.SkipSingleReplica, config.OperatorNamespace)
}

// onConfigMapEvent is the shared body of the ConfigMap AddFunc/UpdateFunc
// handlers. It captures the in-memory config + exclusion rules before
// reloading, then computes transitions for the rollback path
// (true→false management flip with rollbackOnDisable=true).
//
// The reconcile pass is gone — there is no live patcher anymore — so
// transitions other than the rollback trigger are no-ops.
func onConfigMapEvent() {
	configLock.RLock()
	oldCfg := config
	configLock.RUnlock()

	loadConfig()

	configLock.RLock()
	newCfg := config
	configLock.RUnlock()

	if oldCfg == nil || newCfg == nil {
		return
	}
	oldState := oldCfg.StateOf()
	newState := newCfg.StateOf()

	if oldState.ManagementEnabled && !newState.ManagementEnabled && newState.RollbackOnDisable {
		logInfo("rollback-trigger", "managementEnabled went true→false; triggering rollback asynchronously")
		go runRollbackFn(context.Background(), newState.OperatorNamespace)
	}
}

// currentRollbackState is a convenience accessor that returns the
// rollback-relevant subset of the live config. Kept for callers (tests,
// future tooling) that need to inspect the current state without holding
// configLock directly.
func currentRollbackState() RollbackState {
	configLock.RLock()
	defer configLock.RUnlock()
	if config == nil {
		return RollbackState{}
	}
	return config.StateOf()
}

func runRollback(ctx context.Context, ns string) {
	logger := snapshot.SimpleLogger{Info: logInfoSimple, Warn: logWarnSimple, Error: logErrorSimple}
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
//
// Although the live patching path has been retired, captureTSCOriginal is
// retained so future rollouts (e.g., a "snapshot on admission" mode) can
// reuse it without re-deriving the WorkloadIdentity mapping.
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
// TSCOriginal snapshot owned by this workload, so the workload can be
// deleted. Retained alongside the snapshot helpers for compatibility with
// any callers that still trigger it.
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

// removeTSCFromWorkload removes the topologySpreadConstraints field from a
// Deployment or StatefulSet's pod template. Retained for rollback of old
// snapshots: the legacy controller used this to undo its patches on
// management disable, and the path is exercised by the rollback tests.
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

// isControllerSelf reports whether the given workload is the controller's
// own pod. The controller marks its Deployment's pod template with
// ControllerSelfLabel; combined with the operator namespace this identifies
// the controller regardless of Helm release name or per-instance naming
// overrides. config.OperatorNamespace is read under configLock to match the
// concurrency pattern used elsewhere in this file.
func isControllerSelf(namespace string, labels map[string]string) bool {
	configLock.RLock()
	operatorNS := ""
	if config != nil {
		operatorNS = config.OperatorNamespace
	}
	configLock.RUnlock()

	if namespace != operatorNS {
		return false
	}
	if labels == nil {
		return false
	}
	return labels[ControllerSelfLabel] == ControllerSelfLabelTrue
}

func isExcluded(namespace, name string, labels map[string]string) bool {
	if isControllerSelf(namespace, labels) {
		return true
	}

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

// Adapter wrappers so snapshot.SimpleLogger can consume this controller's
// rate-limited log helpers (which take a key as the first argument).
func logInfoSimple(format string, args ...interface{})  { logInfo("tsc-snapshot", format, args...) }
func logWarnSimple(format string, args ...interface{})  { logWarn("tsc-snapshot", format, args...) }
func logErrorSimple(format string, args ...interface{}) { logError("tsc-snapshot", format, args...) }
