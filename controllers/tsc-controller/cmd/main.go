package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

const (
	controllerName           = "castai-tsc-controller"
	configMapNamespace       = "castai-agent"
	configMapName            = "castai-tsc-controller-config"
	annotationSkipTSC        = "workloads.cast.ai/skip-tsc-injection"
	annotationTSCInjected    = "workloads.cast.ai/tsc-injected"
	leaderElectionLockName   = "castai-tsc-controller-leader-election"
)

// Default constraint configuration
type DefaultConstraint struct {
	MaxSkew           int    `json:"maxSkew"`
	TopologyKey       string `json:"topologyKey"`
	WhenUnsatisfiable string `json:"whenUnsatisfiable"`
}

// ConfigMap configuration
type Config struct {
	DefaultConstraints     []DefaultConstraint `json:"defaultConstraints"`
	SkipSingleReplica      bool                `json:"skipSingleReplica"`
	LogInterval            string              `json:"logInterval"`
	ReconcileInterval      string              `json:"reconcileInterval"`
	GarbageCollectInterval string              `json:"garbageCollectInterval"`
	Exclusions             []ExclusionRule     `json:"exclusions"`
}

// Exclusion rule for workloads
type ExclusionRule struct {
	NamespaceRegex string            `json:"namespaceRegex"`
	NameRegex      string            `json:"nameRegex"`
	Labels         map[string]string `json:"labels,omitempty"`
}

var (
	masterURL        string
	kubeconfig       string
	configNamespace  string
	configLock       sync.RWMutex
	currentConfig    Config
	lastInfoLogTimes = make(map[string]time.Time)
	lastWarnLogTimes = make(map[string]time.Time)
	logTimesLock     sync.Mutex
)

func init() {
	klog.InitFlags(nil)
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&configNamespace, "config-namespace", configMapNamespace, "The namespace where the ConfigMap is located")
}

func main() {
	flag.Parse()

	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		close(stopCh)
	}()

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		logError("Failed to build config: %v", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logError("Failed to create clientset: %v", err)
		os.Exit(1)
	}

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerName})

	// Load initial config
	loadConfig(clientset)

	// Start leader election for HA
	runLeaderElection(cfg, clientset, recorder, stopCh)
}

func runLeaderElection(cfg *rest.Config, clientset kubernetes.Interface, recorder record.EventRecorder, stopCh <-chan struct{}) {
	lock := &resourcelock.LeaseLock{
		LockMeta: metav1.ObjectMeta{
			Name:      leaderElectionLockName,
			Namespace: configNamespace,
		},
		Client: clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: fmt.Sprintf("tsc-controller-%d", os.Getpid()),
		},
	}

	leaderelection.RunOrDie(context.Background(), leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   30 * time.Second,
		RenewDeadline:   15 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderElectionCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				logInfo("Started leading, running controller")
				runController(ctx, clientset, recorder, stopCh)
			},
			OnStoppedLeading: func() {
				logWarn("Stopped leading, shutting down")
				os.Exit(0)
			},
			OnNewLeader: func(identity string) {
				logInfo("New leader elected: %s", identity)
			},
		},
	})
}

func runController(ctx context.Context, clientset kubernetes.Interface, recorder record.EventRecorder, stopCh <-chan struct{}) {
	informerFactory := informers.NewSharedInformerFactory(clientset, 30*time.Second)
	deployInformer := informerFactory.Apps().V1().Deployments().Informer()
	ssInformer := informerFactory.Apps().V1().StatefulSets().Informer()
	configMapInformer := informerFactory.Core().V1().ConfigMaps().Informer()

	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

	// Enqueue all items on configmap changes to pick up config updates
	configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cm := obj.(*corev1.ConfigMap)
			if cm.Name == configMapName && cm.Namespace == configNamespace {
				loadConfig(clientset)
				enqueueAllWorkloads(deployInformer, ssInformer)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			cm := newObj.(*corev1.ConfigMap)
			if cm.Name == configMapName && cm.Namespace == configNamespace {
				loadConfig(clientset)
				enqueueAllWorkloads(deployInformer, ssInformer)
			}
		},
	})

	deployInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			d := obj.(*appsv1.Deployment)
			handleWorkload(ctx, clientset, recorder, d.Namespace, d.Name, "Deployment")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			d := newObj.(*appsv1.Deployment)
			handleWorkload(ctx, clientset, recorder, d.Namespace, d.Name, "Deployment")
		},
		DeleteFunc: func(obj interface{}) {
			// No action needed for deletions
		},
	})

	ssInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ss := obj.(*appsv1.StatefulSet)
			handleWorkload(ctx, clientset, recorder, ss.Namespace, ss.Name, "StatefulSet")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			ss := newObj.(*appsv1.StatefulSet)
			handleWorkload(ctx, clientset, recorder, ss.Namespace, ss.Name, "StatefulSet")
		},
		DeleteFunc: func(obj interface{}) {
			// No action needed for deletions
		},
	})

	// Periodic reconciliation
	reconcileTicker := time.NewTicker(getReconcileInterval())
	defer reconcileTicker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-reconcileTicker.C:
			enqueueAllWorkloads(deployInformer, ssInformer)
		}
	}
}

func enqueueAllWorkloads(deployInformer cache.SharedIndexInformer, ssInformer cache.SharedIndexInformer) {
	objs, _ := deployInformer.GetIndexer().List()
	for _, obj := range objs {
		d := obj.(*appsv1.Deployment)
		handleWorkload(context.Background(), nil, nil, d.Namespace, d.Name, "Deployment")
	}

	objs, _ = ssInformer.GetIndexer().List()
	for _, obj := range objs {
		ss := obj.(*appsv1.StatefulSet)
		handleWorkload(context.Background(), nil, nil, ss.Namespace, ss.Name, "StatefulSet")
	}
}

func getReconcileInterval() time.Duration {
	configLock.RLock()
	defer configLock.RUnlock()

	interval := currentConfig.ReconcileInterval
	if interval == "" {
		interval = "2m"
	}

	d, err := time.ParseDuration(interval)
	if err != nil {
		return 2 * time.Minute
	}
	return d
}

func loadConfig(clientset kubernetes.Interface) {
	cm, err := clientset.CoreV1().ConfigMaps(configNamespace).Get(context.Background(), configMapName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			logError("Failed to get configmap: %v", err)
		}
		return
	}

	configLock.Lock()
	defer configLock.Unlock()

	// Parse default constraints
	constraintsJSON := cm.Data["defaultConstraints"]
	if constraintsJSON != "" {
		var constraints []DefaultConstraint
		if err := json.Unmarshal([]byte(constraintsJSON), &constraints); err != nil {
			logError("Failed to parse defaultConstraints: %v", err)
		} else {
			currentConfig.DefaultConstraints = constraints
		}
	}

	// Parse exclusions
	exclusionsJSON := cm.Data["exclusions"]
	if exclusionsJSON != "" && exclusionsJSON != "[]" {
		var exclusions []ExclusionRule
		if err := json.Unmarshal([]byte(exclusionsJSON), &exclusions); err != nil {
			logError("Failed to parse exclusions: %v", err)
		} else {
			currentConfig.Exclusions = exclusions
		}
	}

	// Parse boolean flags
	if val, ok := cm.Data["skipSingleReplica"]; ok {
		currentConfig.SkipSingleReplica = val == "true"
	}
	if val, ok := cm.Data["logInterval"]; ok {
		currentConfig.LogInterval = val
	}
	if val, ok := cm.Data["reconcileInterval"]; ok {
		currentConfig.ReconcileInterval = val
	}
	if val, ok := cm.Data["garbageCollectInterval"]; ok {
		currentConfig.GarbageCollectInterval = val
	}

	logInfo("Loaded config: %d constraints, skipSingleReplica=%v", 
		len(currentConfig.DefaultConstraints), currentConfig.SkipSingleReplica)
}

func handleWorkload(ctx context.Context, clientset kubernetes.Interface, recorder record.EventRecorder, namespace, name, kind string) {
	if clientset == nil {
		return
	}

	configLock.RLock()
	config := currentConfig
	configLock.RUnlock()

	// Check if workload should be skipped
	if isAnnotationSet(clientset, namespace, name, kind, annotationSkipTSC) {
		return
	}

	// Check exclusions
	if isWorkloadExcluded(namespace, name, config.Exclusions) {
		return
	}

	var workload interface{}
	var err error

	switch kind {
	case "Deployment":
		workload, err = clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	case "StatefulSet":
		workload, err = clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	}

	if err != nil {
		if !errors.IsNotFound(err) {
			logError("Failed to get %s %s/%s: %v", kind, namespace, name, err)
		}
		return
	}

	// Check if single replica and skipSingleReplica is enabled
	replicas := getReplicas(workload)
	if config.SkipSingleReplica && replicas <= 1 {
		removeTSCFromWorkload(ctx, clientset, namespace, name, kind, recorder)
		return
	}

	// Inject topology spread constraints
	injectTSC(ctx, clientset, namespace, name, kind, config.DefaultConstraints, recorder)
}

func isAnnotationSet(clientset kubernetes.Interface, namespace, name, kind string, annotation string) bool {
	ctx := context.Background()
	var val string
	var err error

	switch kind {
	case "Deployment":
		d, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		val = d.Annotations[annotation]
	case "StatefulSet":
		ss, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		val = ss.Annotations[annotation]
	}

	return val == "true" || val == "false"
}

func isWorkloadExcluded(namespace, name string, exclusions []ExclusionRule) bool {
	for _, rule := range exclusions {
		// Check namespace regex
		if rule.NamespaceRegex != "" {
			matched, err := regexp.MatchString(rule.NamespaceRegex, namespace)
			if err != nil {
				logError("Invalid namespace regex '%s': %v", rule.NamespaceRegex, err)
				continue
			}
			if !matched {
				continue
			}
		}

		// Check name regex
		if rule.NameRegex != "" {
			matched, err := regexp.MatchString(rule.NameRegex, name)
			if err != nil {
				logError("Invalid name regex '%s': %v", rule.NameRegex, err)
				continue
			}
			if !matched {
				continue
			}
		}

		return true
	}
	return false
}

func getReplicas(workload interface{}) int32 {
	switch w := workload.(type) {
	case *appsv1.Deployment:
		if w.Spec.Replicas != nil {
			return *w.Spec.Replicas
		}
		return 1
	case *appsv1.StatefulSet:
		if w.Spec.Replicas != nil {
			return *w.Spec.Replicas
		}
		return 1
	}
	return 1
}

func injectTSC(ctx context.Context, clientset kubernetes.Interface, namespace, name, kind string, constraints []DefaultConstraint, recorder record.EventRecorder) {
	if len(constraints) == 0 {
		return
	}

	var patchBytes []byte
	var err error

	switch kind {
	case "Deployment":
		patchBytes, err = patchDeploymentTSC(ctx, clientset, namespace, name, constraints)
	case "StatefulSet":
		patchBytes, err = patchStatefulSetTSC(ctx, clientset, namespace, name, constraints)
	}

	if err != nil {
		logError("Failed to patch %s %s/%s: %v", kind, namespace, name, err)
		return
	}

	if patchBytes == nil {
		return
	}

	var patched interface{}
	switch kind {
	case "Deployment":
		_, err = clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
		if err != nil {
			logError("Failed to patch deployment %s/%s: %v", namespace, name, err)
			return
		}
	case "StatefulSet":
		_, err = clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
		if err != nil {
			logError("Failed to patch statefulset %s/%s: %v", namespace, name, err)
			return
		}
	}

	// Update annotation to mark as injected
	markInjected(ctx, clientset, namespace, name, kind, recorder)

	logInfo("Injected topology spread constraints into %s %s/%s", kind, namespace, name)
}

func patchDeploymentTSC(ctx context.Context, clientset kubernetes.Interface, namespace, name string, constraints []DefaultConstraint) ([]byte, error) {
	d, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// Check if already has TSC
	if len(d.Spec.Template.Spec.TopologySpreadConstraints) > 0 {
		return nil, nil
	}

	// Build TSC from constraints
	tsc := buildTSC(constraints)

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"topologySpreadConstraints": tsc,
				},
			},
		},
	}

	return json.Marshal(patch)
}

func patchStatefulSetTSC(ctx context.Context, clientset kubernetes.Interface, namespace, name string, constraints []DefaultConstraint) ([]byte, error) {
	ss, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// Check if already has TSC
	if len(ss.Spec.Template.Spec.TopologySpreadConstraints) > 0 {
		return nil, nil
	}

	// Build TSC from constraints
	tsc := buildTSC(constraints)

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"topologySpreadConstraints": tsc,
				},
			},
		},
	}

	return json.Marshal(patch)
}

func buildTSC(constraints []DefaultConstraint) []corev1.TopologySpreadConstraint {
	tsc := make([]corev1.TopologySpreadConstraint, 0, len(constraints))

	for _, c := range constraints {
		constraint := corev1.TopologySpreadConstraint{
			MaxSkew:           int32(c.MaxSkew),
			TopologyKey:       c.TopologyKey,
			WhenUnsatisfiable: corev1.UnsatisfiableConstraintAction(c.WhenUnsatisfiable),
			LabelSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{},
			},
		}

		// For better distribution, add matchLabelKeys if not present
		constraint.MatchLabelKeys = []string{}

		tsc = append(tsc, constraint)
	}

	return tsc
}

func removeTSCFromWorkload(ctx context.Context, clientset kubernetes.Interface, namespace, name, kind string, recorder record.EventRecorder) {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"topologySpreadConstraints": []corev1.TopologySpreadConstraint{},
				},
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		logError("Failed to marshal patch for removing TSC: %v", err)
		return
	}

	switch kind {
	case "Deployment":
		_, err = clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	case "StatefulSet":
		_, err = clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	}

	if err != nil {
		logError("Failed to remove TSC from %s %s/%s: %v", kind, namespace, name, err)
		return
	}

	logInfo("Removed topology spread constraints from %s %s/%s (single replica)", kind, namespace, name)
}

func markInjected(ctx context.Context, clientset kubernetes.Interface, namespace, name, kind string, recorder record.EventRecorder) {
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				annotationTSCInjected: "true",
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return
	}

	switch kind {
	case "Deployment":
		clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	case "StatefulSet":
		clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	}
}