// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"
)

// Controller constants
const (
	ControllerName     = "castai-jvm-probe-controller"
	ConfigMapNamespace = "castai-agent"
	ConfigMapName      = "castai-jvm-probe-controller-config"
)

// Global variables
var (
	masterURL       string
	kubeconfig      string
	configNamespace string
	help            bool
	version         bool

	webhookAddr string
	tlsCertFile string
	tlsKeyFile  string
	healthAddr  string

	clientset      *kubernetes.Clientset
	config         *JVMConfig
	configLock     sync.RWMutex
	exclusionRules *ExclusionRules
)

// init registers flags
func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig.")
	flag.StringVar(&configNamespace, "config-namespace", ConfigMapNamespace, "Namespace for the ConfigMap")
	flag.BoolVar(&help, "help", false, "Print help")
	flag.BoolVar(&version, "version", false, "Print version")

	flag.StringVar(&webhookAddr, "webhook-addr", ":8443", "Address the admission webhook server listens on (host:port). Use :0 for an ephemeral port in tests.")
	flag.StringVar(&tlsCertFile, "tls-cert-file", "/etc/webhook/certs/tls.crt", "Path to the TLS certificate (PEM) for the webhook server.")
	flag.StringVar(&tlsKeyFile, "tls-key-file", "/etc/webhook/certs/tls.key", "Path to the TLS private key (PEM) for the webhook server.")
	flag.StringVar(&healthAddr, "health-addr", ":8080", "Address the plain-HTTP /healthz server listens on (host:port). Used by kubelet probes. Use :0 for an ephemeral port in tests.")
}

// Controller represents the JVM Probe Controller. The Deployment and
// StatefulSet reconcile loop has been retired in favor of the admission
// webhook (pod_mutation.go + webhook.go); only the ConfigMap informer
// remains so that JVMConfig updates are picked up at runtime via
// handleConfigMapUpdate.
type Controller struct {
	informerFactory informers.SharedInformerFactory
	configMap       cache.SharedIndexInformer
}

// NewController creates a new JVM Probe Controller. The only informer
// that remains is the ConfigMap informer used for hot-reload of
// JVMConfig; the Deployment/StatefulSet reconcile loop (and its
// associated event recorder) was retired during the webhook migration.
func NewController(clientset *kubernetes.Clientset, factory informers.SharedInformerFactory) *Controller {
	configMapInformer := factory.Core().V1().ConfigMaps().Informer()

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

	return &Controller{
		informerFactory: factory,
		configMap:       configMapInformer,
	}
}

// handleConfigMapUpdate handles ConfigMap updates for hot-reload.
func handleConfigMapUpdate(cm *corev1.ConfigMap) {
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

	logInfo("configmap", "ConfigMap updated: logInterval=%s, reconcileInterval=%s mgmt=%v mode=%s",
		newConfig.LogInterval, newConfig.ReconcileInterval,
		newConfig.ManagementEnabled, newConfig.Mode)
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

// Run starts the controller. It boots the ConfigMap informer, waits for
// the cache to sync, then starts the admission webhook server and the
// plain-HTTP health server. Both servers are shut down cleanly when ctx
// is canceled (or when either reports a fatal error, which also cancels
// ctx so the other server tears down).
//
// The webhook server is the runtime path for probe injection; the health
// server is the runtime path for kubelet probes. The Deployment/
// StatefulSet reconcile loop is gone.
func (c *Controller) Run(ctx context.Context, webhookServer *WebhookServer, healthServer *HealthServer) error {
	defer utilruntime.HandleCrash()

	logAlways("Starting JVM Probe Controller...")

	// Start informers
	c.informerFactory.Start(ctx.Done())

	// Wait for caches to sync
	if ok := cache.WaitForCacheSync(ctx.Done(), c.configMap.HasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logAlways("ConfigMap informer synced; admission webhook is the runtime path")

	// If either server returns a fatal error we want the other to shut
	// down too. Drive that with a child context we can cancel locally.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	expected := 1
	serveErr := make(chan error, 2)
	go func() {
		serveErr <- webhookServer.Start(runCtx)
	}()
	if healthServer != nil {
		expected = 2
		go func() {
			serveErr <- healthServer.Start(runCtx)
		}()
	}

	select {
	case <-ctx.Done():
		// Parent cancellation: wait for all servers to drain so their
		// goroutines exit cleanly before Run returns.
		var firstErr error
		for i := 0; i < expected; i++ {
			if err := <-serveErr; err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	case err := <-serveErr:
		// One of the servers failed; cancel the other so it tears down.
		cancel()
		firstErr := err
		for i := 1; i < expected; i++ {
			if e := <-serveErr; e != nil && firstErr == nil {
				firstErr = e
			}
		}
		return firstErr
	}
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
		logAlways("Loaded configuration from ConfigMap %s/%s (mgmt=%v mode=%s ns=%s)",
			configNamespace, ConfigMapName,
			newConfig.ManagementEnabled, newConfig.Mode,
			newConfig.OperatorNamespace)
	} else {
		logAlways("ConfigMap not found, using defaults")
	}

	// Create shared informer factory
	factory := informers.NewSharedInformerFactory(clientset, 0)

	// Create controller. The ConfigMap informer is the only watcher that
	// remains; the webhook server is the runtime path for probe injection.
	controller := NewController(clientset, factory)

	// Build the webhook server. The mutation handler reads the current
	// JVMConfig under configLock (hot-reload safe) and produces JSON Patch
	// mutations for the admitted Pod.
	mutateHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlePodAdmission(w, r)
	})
	webhookServer := NewWebhookServer(webhookAddr, tlsCertFile, tlsKeyFile, mutateHandler)

	// Build the plain-HTTP health server for kubelet probes. It only
	// serves /healthz; /metrics and /mutate/pods stay on the HTTPS
	// webhook port.
	healthServer := NewHealthServer(healthAddr)

	// Create context with cancel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logAlways("Shutting down...")
		cancel()
	}()

	logAlways("Starting JVM Probe admission webhook on %s (tls=%s,%s) and health server on %s",
		webhookAddr, tlsCertFile, tlsKeyFile, healthAddr)
	if err := controller.Run(ctx, webhookServer, healthServer); err != nil {
		log.Fatalf("Controller run failed: %v", err)
	}
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
