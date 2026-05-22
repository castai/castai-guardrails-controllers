package main

import (
	"context"
	"fmt"
	"io"
	"log"
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
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"sigs.k8s.io/yaml"
)

const (
	configMapNamespace                   = "castai-agent"
	configMapName                        = "castai-pdb-controller-config"
	annotationMinAvailable               = "workloads.cast.ai/pdb-minAvailable"
	annotationMaxUnavailable             = "workloads.cast.ai/pdb-maxUnavailable"
	annotationUnhealthyPodEvictionPolicy = "workloads.cast.ai/pdb-unhealthyPodEvictionPolicy"
	annotationBypass                     = "workloads.cast.ai/bypass-default-pdb"
	defaultLogInterval                   = 15 * time.Minute
	defaultPDBScanInterval               = 2 * time.Minute
	defaultGarbageCollectInterval        = 2 * time.Minute
	defaultPDBDumpInterval               = 5 * time.Minute
	pdbDumpFile                          = "/tmp/castai-pdbs.yaml"
	pdbRaceRecheckAttempts               = 8
	pdbRaceRecheckInterval               = 250 * time.Millisecond
)

type ExclusionRule struct {
	NamespaceRegex string            `yaml:"namespaceRegex"`
	NameRegex      string            `yaml:"nameRegex"`
	Labels         map[string]string `yaml:"labels"`
}

type DefaultPDBConfig struct {
	MinAvailable               *intstr.IntOrString
	MaxUnavailable             *intstr.IntOrString
	UnhealthyPodEvictionPolicy *policyv1.UnhealthyPodEvictionPolicyType
	FixPoorPDBs                bool
	LogInterval                time.Duration
	PDBScanInterval            time.Duration
	GarbageCollectInterval     time.Duration
	PDBDumpInterval            time.Duration
	Exclusions                 []ExclusionRule
}

var (
	defaultPDBConfig     DefaultPDBConfig
	defaultPDBConfigLock sync.RWMutex

	skipLogTimes     = make(map[string]time.Time)
	skipLogTimesLock sync.Mutex
	warnLogTimes     = make(map[string]time.Time)
	warnLogTimesLock sync.Mutex
	fixLogTimes      = make(map[string]time.Time)
	fixLogTimesLock  sync.Mutex
)

func isWorkloadExcluded(namespace, name string, labels map[string]string) bool {
	defaultPDBConfigLock.RLock()
	exclusions := defaultPDBConfig.Exclusions
	defaultPDBConfigLock.RUnlock()

	logDebugf("isWorkloadExcluded called for %s/%s with %d exclusion rules", namespace, name, len(exclusions))

	for _, rule := range exclusions {
		if rule.NamespaceRegex != "" {
			matched, err := regexp.MatchString(rule.NamespaceRegex, namespace)
			if err != nil {
				logErrorf("ERROR: Invalid namespace regex '%s' in exclusion rule (workload: %s/%s): %v", rule.NamespaceRegex, namespace, name, err)
				continue
			}
			if !matched {
				continue
			}
		}

		if rule.NameRegex != "" {
			matched, err := regexp.MatchString(rule.NameRegex, name)
			if err != nil {
				logErrorf("ERROR: Invalid name regex '%s' in exclusion rule (workload: %s/%s): %v", rule.NameRegex, namespace, name, err)
				continue
			}
			if !matched {
				continue
			}
		}

		if len(rule.Labels) > 0 {
			allLabelsMatch := true
			for key, valuePattern := range rule.Labels {
				if labelValue, exists := labels[key]; exists {
					matched, err := regexp.MatchString(valuePattern, labelValue)
					if err != nil {
						logErrorf("ERROR: Invalid label value regex '%s' for key '%s' in exclusion rule (workload: %s/%s): %v", valuePattern, key, namespace, name, err)
						allLabelsMatch = false
						break
					}
					if !matched {
						allLabelsMatch = false
						break
					}
				} else {
					allLabelsMatch = false
					break
				}
			}
			if !allLabelsMatch {
				continue
			}
		}

		logInfof("EXCLUDED: Workload %s/%s matches exclusion rule (namespace='%s', name='%s', labels=%v)",
			namespace, name, rule.NamespaceRegex, rule.NameRegex, rule.Labels)
		return true
	}

	return false
}

func unhealthyPodEvictionStr(p *policyv1.UnhealthyPodEvictionPolicyType) string {
	if p == nil {
		return "<unset>"
	}
	return string(*p)
}

func parseUnhealthyPodEvictionPolicy(s string) *policyv1.UnhealthyPodEvictionPolicyType {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	switch strings.ToLower(s) {
	case strings.ToLower(string(policyv1.IfHealthyBudget)):
		p := policyv1.IfHealthyBudget
		return &p
	case strings.ToLower(string(policyv1.AlwaysAllow)):
		p := policyv1.AlwaysAllow
		return &p
	default:
		logWarnf("Invalid unhealthyPodEvictionPolicy %q (valid: IfHealthyBudget, AlwaysAllow); ignoring", s)
		return nil
	}
}

func resolveUnhealthyPodEvictionPolicy(workloadAnnotations map[string]string) *policyv1.UnhealthyPodEvictionPolicyType {
	if workloadAnnotations != nil {
		if v, ok := workloadAnnotations[annotationUnhealthyPodEvictionPolicy]; ok {
			if p := parseUnhealthyPodEvictionPolicy(v); p != nil {
				return p
			}
		}
	}
	defaultPDBConfigLock.RLock()
	def := defaultPDBConfig.UnhealthyPodEvictionPolicy
	defaultPDBConfigLock.RUnlock()
	if def == nil {
		return nil
	}
	c := *def
	return &c
}

func unhealthyPodEvictionPoliciesEqual(a, b *policyv1.UnhealthyPodEvictionPolicyType) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func updateExistingPDB(ctx context.Context, clientset *kubernetes.Clientset, existingPDB *policyv1.PodDisruptionBudget, workloadAnnotations map[string]string, replicas *int32, namespace, name string, obj interface{}) {
	var minAvailable, maxUnavailable *intstr.IntOrString
	var needsUpdate bool

	if workloadAnnotations != nil {
		if val, ok := workloadAnnotations[annotationMinAvailable]; ok {
			minAvailable = parsePDBValue(val)
		}
		if val, ok := workloadAnnotations[annotationMaxUnavailable]; ok {
			maxUnavailable = parsePDBValue(val)
		}
	}

	if minAvailable == nil && maxUnavailable == nil {
		defaultPDBConfigLock.RLock()
		minAvailable = defaultPDBConfig.MinAvailable
		maxUnavailable = defaultPDBConfig.MaxUnavailable
		defaultPDBConfigLock.RUnlock()
	}

	if minAvailable != nil && maxUnavailable != nil {
		logWarnf("Invalid PDB config for %s/%s: both minAvailable and maxUnavailable set, using minAvailable\n", namespace, name)
		maxUnavailable = nil
	}

	pdbSpec := policyv1.PodDisruptionBudgetSpec{
		Selector: existingPDB.Spec.Selector,
	}

	if minAvailable != nil {
		pdbSpec.MinAvailable = minAvailable
	} else if maxUnavailable != nil {
		pdbSpec.MaxUnavailable = maxUnavailable
	}

	pdbSpec.UnhealthyPodEvictionPolicy = resolveUnhealthyPodEvictionPolicy(workloadAnnotations)

	if existingPDB.Spec.MinAvailable == nil && pdbSpec.MinAvailable != nil {
		needsUpdate = true
	} else if existingPDB.Spec.MinAvailable != nil && pdbSpec.MinAvailable == nil {
		needsUpdate = true
	} else if existingPDB.Spec.MinAvailable != nil && pdbSpec.MinAvailable != nil {
		if existingPDB.Spec.MinAvailable.String() != pdbSpec.MinAvailable.String() {
			needsUpdate = true
		}
	}

	if existingPDB.Spec.MaxUnavailable == nil && pdbSpec.MaxUnavailable != nil {
		needsUpdate = true
	} else if existingPDB.Spec.MaxUnavailable != nil && pdbSpec.MaxUnavailable == nil {
		needsUpdate = true
	} else if existingPDB.Spec.MaxUnavailable != nil && pdbSpec.MaxUnavailable != nil {
		if existingPDB.Spec.MaxUnavailable.String() != pdbSpec.MaxUnavailable.String() {
			needsUpdate = true
		}
	}

	if !unhealthyPodEvictionPoliciesEqual(existingPDB.Spec.UnhealthyPodEvictionPolicy, pdbSpec.UnhealthyPodEvictionPolicy) {
		needsUpdate = true
	}

	if needsUpdate {
		existingPDB.Spec = pdbSpec
		_, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Update(ctx, existingPDB, metav1.UpdateOptions{})
		if err != nil {
			logErrorf("Failed to update PDB for %s/%s: %v\n", namespace, name, err)
		} else {
			logInfof("Updated PDB for %s/%s\n", namespace, name)
			if replicas != nil {
				logAndFixPoorPDBConfig(ctx, clientset, existingPDB, name, *replicas, namespace, obj)
			}
		}
	}
}

func parseDurationFromConfigMap(data map[string]string, key string, defaultDuration time.Duration) time.Duration {
	if val, ok := data[key]; ok {
		duration, err := time.ParseDuration(val)
		if err != nil {
			logWarnf("Invalid duration for %s in ConfigMap: %v, using default %v", key, err, defaultDuration)
			return defaultDuration
		}
		if duration <= 0 {
			logWarnf("Non-positive duration for %s in ConfigMap: %v, using default %v", key, duration, defaultDuration)
			return defaultDuration
		}
		return duration
	}
	return defaultDuration
}

func logFixPoorPDB(key, message string) {
	now := time.Now()
	defaultPDBConfigLock.RLock()
	interval := defaultPDBConfig.LogInterval
	defaultPDBConfigLock.RUnlock()
	fixLogTimesLock.Lock()
	last, ok := fixLogTimes[key]
	if !ok || now.Sub(last) > interval {
		if shouldLog(sevInfo) {
			log.Print(message)
		}
		fixLogTimes[key] = now
	}
	fixLogTimesLock.Unlock()
}

func logPoorPDBWarning(key, message string) {
	now := time.Now()
	defaultPDBConfigLock.RLock()
	interval := defaultPDBConfig.LogInterval
	defaultPDBConfigLock.RUnlock()
	warnLogTimesLock.Lock()
	last, ok := warnLogTimes[key]
	if !ok || now.Sub(last) > interval {
		if shouldLog(sevWarn) {
			log.Print(message)
		}
		warnLogTimes[key] = now
	}
	warnLogTimesLock.Unlock()
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			panic(err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	id, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	resetDefaultPDBConfig()
	loadDefaultPDBConfig(ctx, clientset)

	log.SetOutput(io.Discard)

	defaultPDBConfigLock.RLock()
	pdbScanInterval := defaultPDBConfig.PDBScanInterval
	garbageCollectInterval := defaultPDBConfig.GarbageCollectInterval
	pdbDumpInterval := defaultPDBConfig.PDBDumpInterval
	defaultPDBConfigLock.RUnlock()

	if pdbScanInterval <= 0 || garbageCollectInterval <= 0 || pdbDumpInterval <= 0 {
		logWarnf("WARNING: Config not properly initialized, resetting again")
		resetDefaultPDBConfig()
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "castai-pdb-controller-leader-election",
			Namespace: configMapNamespace,
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
				log.SetOutput(os.Stderr)
				logInfof("[%s] I am the leader now\n", id)
				runController(ctx, clientset)
			},
			OnStoppedLeading: func() {
				logInfof("[%s] Lost leadership, exiting\n", id)
				os.Exit(0)
			},
			OnNewLeader: func(identity string) {
				if identity == id {
					logInfof("[%s] I am the new leader\n", id)
				} else {
					logInfof("[%s] New leader elected: %s\n", id, identity)
				}
			},
		},
		ReleaseOnCancel: true,
		Name:            "castai-pdb-controller",
	})
}

type MinimalPDB struct {
	APIVersion string                           `yaml:"apiVersion"`
	Kind       string                           `yaml:"kind"`
	Metadata   MinimalMetadata                  `yaml:"metadata"`
	Spec       policyv1.PodDisruptionBudgetSpec `yaml:"spec"`
}

type MinimalMetadata struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

func dumpCastaiPDBsToFile(ctx context.Context, clientset *kubernetes.Clientset) {
	pdbs, err := clientset.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		logErrorf("Failed to list PDBs for dumping to file: %v", err)
		return
	}

	var yamlOutput []string
	for _, pdb := range pdbs.Items {
		if strings.HasPrefix(pdb.Name, "castai-") && strings.HasSuffix(pdb.Name, "-pdb") {
			minimalPDB := MinimalPDB{
				APIVersion: "policy/v1",
				Kind:       "PodDisruptionBudget",
				Metadata: MinimalMetadata{
					Name:      pdb.Name,
					Namespace: pdb.Namespace,
				},
				Spec: pdb.Spec,
			}
			pdbYaml, err := yaml.Marshal(&minimalPDB)
			if err != nil {
				logErrorf("Failed to marshal PDB %s/%s to YAML: %v", pdb.Namespace, pdb.Name, err)
				continue
			}
			yamlOutput = append(yamlOutput, string(pdbYaml))
		}
	}

	output := strings.Join(yamlOutput, "\n---\n")
	err = os.WriteFile(pdbDumpFile, []byte(output), 0644)
	if err != nil {
		logErrorf("Failed to write PDBs to %s: %v", pdbDumpFile, err)
		return
	}
	logInfof("Successfully wrote %d castai PDBs to %s", len(yamlOutput), pdbDumpFile)
}

func watchConfigMap(ctx context.Context, clientset *kubernetes.Clientset) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		0,
		informers.WithNamespace(configMapNamespace),
	)
	cmInformer := factory.Core().V1().ConfigMaps().Informer()

	cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == configMapName {
				updateDefaultPDBConfig(ctx, cm, clientset, true)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if cm, ok := newObj.(*corev1.ConfigMap); ok && cm.Name == configMapName {
				updateDefaultPDBConfig(ctx, cm, clientset, true)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == configMapName {
				resetDefaultPDBConfig()
			}
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	<-ctx.Done()
}

func updateDefaultPDBConfig(ctx context.Context, cm *corev1.ConfigMap, clientset *kubernetes.Clientset, reconcile bool) {
	syncLogLevelFromData(cm.Data)

	var minAvailable, maxUnavailable *intstr.IntOrString
	if val, ok := cm.Data["defaultMinAvailable"]; ok {
		minAvailable = parsePDBValue(val)
	}
	if val, ok := cm.Data["defaultMaxUnavailable"]; ok {
		maxUnavailable = parsePDBValue(val)
	}
	fixPoorPDBs := false
	if val, ok := cm.Data["FixPoorPDBs"]; ok && strings.ToLower(val) == "true" {
		fixPoorPDBs = true
	}
	if minAvailable != nil && maxUnavailable != nil {
		logWarnf("Invalid default PDB config: both defaultMinAvailable and defaultMaxUnavailable set in ConfigMap\n")
		minAvailable = nil
		maxUnavailable = nil
	}

	logInterval := parseDurationFromConfigMap(cm.Data, "logInterval", defaultLogInterval)
	pdbScanInterval := parseDurationFromConfigMap(cm.Data, "pdbScanInterval", defaultPDBScanInterval)
	garbageCollectInterval := parseDurationFromConfigMap(cm.Data, "garbageCollectInterval", defaultGarbageCollectInterval)
	pdbDumpInterval := parseDurationFromConfigMap(cm.Data, "pdbDumpInterval", defaultPDBDumpInterval)

	var exclusions []ExclusionRule
	if exclusionYAML, ok := cm.Data["exclusions"]; ok && exclusionYAML != "" {
		logDebugf("Found exclusions in ConfigMap: %s", exclusionYAML)
		err := yaml.Unmarshal([]byte(exclusionYAML), &exclusions)
		if err != nil {
			logWarnf("Warning: failed to parse exclusions from ConfigMap: %v\n", err)
			exclusions = []ExclusionRule{}
		} else {
			logDebugf("Successfully parsed %d exclusion rules", len(exclusions))
			validRules := []ExclusionRule{}
			for i, rule := range exclusions {
				logDebugf("Rule %d: namespaceRegex='%s', nameRegex='%s', labels=%v", i, rule.NamespaceRegex, rule.NameRegex, rule.Labels)

				if rule.NamespaceRegex != "" {
					if _, err := regexp.Compile(rule.NamespaceRegex); err != nil {
						logErrorf("ERROR: Invalid namespace regex '%s' in exclusion rule %d: %v", rule.NamespaceRegex, i, err)
						continue
					}
				}

				if rule.NameRegex != "" {
					if _, err := regexp.Compile(rule.NameRegex); err != nil {
						logErrorf("ERROR: Invalid name regex '%s' in exclusion rule %d: %v", rule.NameRegex, i, err)
						continue
					}
				}

				validRule := true
				for key, valuePattern := range rule.Labels {
					if _, err := regexp.Compile(valuePattern); err != nil {
						logErrorf("ERROR: Invalid label value regex '%s' for key '%s' in exclusion rule %d: %v", valuePattern, key, i, err)
						validRule = false
						break
					}
				}

				if validRule {
					validRules = append(validRules, rule)
					logDebugf("Rule %d is valid and will be used", i)
				} else {
					logErrorf("ERROR: Rule %d has invalid regex patterns and will be skipped", i)
				}
			}
			parsedRuleCount := len(exclusions)
			exclusions = validRules
			logDebugf("%d valid exclusion rules loaded (skipped %d invalid rules)", len(exclusions), parsedRuleCount-len(validRules))
		}
	} else {
		logDebugf("No exclusions found in ConfigMap")
	}

	var unhealthyPodEviction *policyv1.UnhealthyPodEvictionPolicyType
	if val, ok := cm.Data["defaultUnhealthyPodEvictionPolicy"]; ok {
		unhealthyPodEviction = parseUnhealthyPodEvictionPolicy(val)
	}

	defaultPDBConfigLock.Lock()
	defaultPDBConfig.MinAvailable = minAvailable
	defaultPDBConfig.MaxUnavailable = maxUnavailable
	defaultPDBConfig.UnhealthyPodEvictionPolicy = unhealthyPodEviction
	defaultPDBConfig.FixPoorPDBs = fixPoorPDBs
	defaultPDBConfig.LogInterval = logInterval
	defaultPDBConfig.PDBScanInterval = pdbScanInterval
	defaultPDBConfig.GarbageCollectInterval = garbageCollectInterval
	defaultPDBConfig.PDBDumpInterval = pdbDumpInterval
	defaultPDBConfig.Exclusions = exclusions
	defaultPDBConfigLock.Unlock()

	logInfof("Default PDB config updated from ConfigMap: defaultMinAvailable=%v, defaultMaxUnavailable=%v, defaultUnhealthyPodEvictionPolicy=%v, FixPoorPDBs=%v, "+
		"logInterval=%v, pdbScanInterval=%v, garbageCollectInterval=%v, pdbDumpInterval=%v, exclusions=%d rules\n",
		minAvailable, maxUnavailable, unhealthyPodEvictionStr(unhealthyPodEviction), fixPoorPDBs, logInterval, pdbScanInterval,
		garbageCollectInterval, pdbDumpInterval, len(exclusions))

	logDebugf("Final exclusion rules loaded: %d rules", len(exclusions))
	for i, rule := range exclusions {
		logDebugf("Final rule %d: namespaceRegex='%s', nameRegex='%s', labels=%v", i, rule.NamespaceRegex, rule.NameRegex, rule.Labels)
	}

	if reconcile {
		go reconcileAllDefaultPDBs(ctx, clientset)
		go scanAllPDBsForPoorConfig(ctx, clientset)
	}
}

func resetDefaultPDBConfig() {
	defaultPDBConfigLock.Lock()
	defaultPDBConfig.MinAvailable = nil
	defaultPDBConfig.MaxUnavailable = nil
	defaultPDBConfig.FixPoorPDBs = false
	defaultPDBConfig.LogInterval = defaultLogInterval
	defaultPDBConfig.PDBScanInterval = defaultPDBScanInterval
	defaultPDBConfig.GarbageCollectInterval = defaultGarbageCollectInterval
	defaultPDBConfig.PDBDumpInterval = defaultPDBDumpInterval
	defaultPDBConfig.Exclusions = nil
	defaultPDBConfig.UnhealthyPodEvictionPolicy = nil
	defaultPDBConfigLock.Unlock()
	syncLogLevelFromData(nil)
	logInfof("Default PDB config reset: using built-in fallback\n")
}

func loadDefaultPDBConfig(ctx context.Context, clientset *kubernetes.Clientset) {
	logDebugf("Loading config from ConfigMap %s/%s", configMapNamespace, configMapName)
	cm, err := clientset.CoreV1().ConfigMaps(configMapNamespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		logWarnf("Warning: could not load default PDB config, using built-in defaults: %v\n", err)
		resetDefaultPDBConfig()
		return
	}
	logDebugf("Successfully loaded ConfigMap, updating config")
	updateDefaultPDBConfig(ctx, cm, clientset, false)
}

func runController(ctx context.Context, clientset *kubernetes.Clientset) {
	go watchConfigMap(ctx, clientset)
	go scanAllPDBsForPoorConfig(ctx, clientset)

	factory := informers.NewSharedInformerFactory(clientset, 10*time.Minute)

	deployInformer := factory.Apps().V1().Deployments().Informer()
	deployInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { createPDBForWorkload(ctx, clientset, obj) },
		UpdateFunc: func(old, new interface{}) { handleWorkloadUpdate(ctx, clientset, old, new) },
		DeleteFunc: func(obj interface{}) { deletePDBForWorkload(ctx, clientset, obj) },
	})

	stsInformer := factory.Apps().V1().StatefulSets().Informer()
	stsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { createPDBForWorkload(ctx, clientset, obj) },
		UpdateFunc: func(old, new interface{}) { handleWorkloadUpdate(ctx, clientset, old, new) },
		DeleteFunc: func(obj interface{}) { deletePDBForWorkload(ctx, clientset, obj) },
	})

	go func() {
		defaultPDBConfigLock.RLock()
		interval := defaultPDBConfig.PDBScanInterval
		defaultPDBConfigLock.RUnlock()

		if interval <= 0 {
			logWarnf("WARNING: PDBScanInterval is %v, using default %v", interval, defaultPDBScanInterval)
			interval = defaultPDBScanInterval
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				defaultPDBConfigLock.RLock()
				newInterval := defaultPDBConfig.PDBScanInterval
				defaultPDBConfigLock.RUnlock()
				if newInterval != interval {
					ticker.Stop()
					ticker = time.NewTicker(newInterval)
					interval = newInterval
					logInfof("Updated PDB scan interval to %v", interval)
				}
				scanAllPDBsForPoorConfig(ctx, clientset)
				scanAllPDBsForMultiplePDBs(ctx, clientset)
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		defaultPDBConfigLock.RLock()
		interval := defaultPDBConfig.GarbageCollectInterval
		defaultPDBConfigLock.RUnlock()

		if interval <= 0 {
			interval = defaultGarbageCollectInterval
		}

		garbageCollectOrphanedPDBs(ctx, clientset)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				defaultPDBConfigLock.RLock()
				newInterval := defaultPDBConfig.GarbageCollectInterval
				defaultPDBConfigLock.RUnlock()
				if newInterval != interval {
					ticker.Stop()
					ticker = time.NewTicker(newInterval)
					interval = newInterval
					logInfof("Updated garbage collect interval to %v", interval)
				}
				garbageCollectOrphanedPDBs(ctx, clientset)
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		defaultPDBConfigLock.RLock()
		interval := defaultPDBConfig.PDBDumpInterval
		defaultPDBConfigLock.RUnlock()

		if interval <= 0 {
			interval = defaultPDBDumpInterval
		}

		dumpCastaiPDBsToFile(ctx, clientset)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				defaultPDBConfigLock.RLock()
				newInterval := defaultPDBConfig.PDBDumpInterval
				defaultPDBConfigLock.RUnlock()
				if newInterval != interval {
					ticker.Stop()
					ticker = time.NewTicker(newInterval)
					interval = newInterval
					logInfof("Updated PDB dump interval to %v", interval)
				}
				dumpCastaiPDBsToFile(ctx, clientset)
			case <-ctx.Done():
				return
			}
		}
	}()

	factory.Start(ctx.Done())
	for _, synced := range []cache.InformerSynced{deployInformer.HasSynced, stsInformer.HasSynced} {
		if !cache.WaitForCacheSync(ctx.Done(), synced) {
			panic("failed to sync informer cache")
		}
	}
	<-ctx.Done()
}

func handleWorkloadUpdate(ctx context.Context, clientset *kubernetes.Clientset, oldObj, newObj interface{}) {
	var oldAnnotations, newAnnotations map[string]string
	var namespace, name string

	switch oldWorkload := oldObj.(type) {
	case *appsv1.Deployment:
		oldAnnotations = oldWorkload.Annotations
		newWorkload := newObj.(*appsv1.Deployment)
		newAnnotations = newWorkload.Annotations
		namespace = newWorkload.Namespace
		name = newWorkload.Name
	case *appsv1.StatefulSet:
		oldAnnotations = oldWorkload.Annotations
		newWorkload := newObj.(*appsv1.StatefulSet)
		newAnnotations = newWorkload.Annotations
		namespace = newWorkload.Namespace
		name = newWorkload.Name
	default:
		return
	}

	oldBypass := false
	if oldAnnotations != nil {
		if val, ok := oldAnnotations[annotationBypass]; ok && val == "true" {
			oldBypass = true
		}
	}
	newBypass := false
	if newAnnotations != nil {
		if val, ok := newAnnotations[annotationBypass]; ok && val == "true" {
			newBypass = true
		}
	}

	if !oldBypass && newBypass {
		pdbName := fmt.Sprintf("castai-%s-pdb", name)
		err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, pdbName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			logErrorf("Failed to delete PDB %s/%s after bypass annotation added: %v\n", namespace, pdbName, err)
		} else {
			logInfof("Bypass annotation added to %s/%s, removed PDB\n", namespace, name)
		}
		return
	}

	if oldBypass && !newBypass {
		logInfof("Bypass annotation removed from %s/%s, creating PDB if needed\n", namespace, name)
		createPDBForWorkload(ctx, clientset, newObj)
		return
	}

	if newBypass {
		return
	}

	oldMin := ""
	newMin := ""
	oldMax := ""
	newMax := ""
	if oldAnnotations != nil {
		oldMin = oldAnnotations[annotationMinAvailable]
		oldMax = oldAnnotations[annotationMaxUnavailable]
	}
	if newAnnotations != nil {
		newMin = newAnnotations[annotationMinAvailable]
		newMax = newAnnotations[annotationMaxUnavailable]
	}
	if oldMin != newMin || oldMax != newMax {
		logInfof("PDB annotation changed for %s/%s, updating PDB\n", namespace, name)
		createPDBForWorkload(ctx, clientset, newObj)
		return
	}

	createPDBForWorkload(ctx, clientset, newObj)
}

func selectorMatchesLabelsFull(selector *metav1.LabelSelector, lbls map[string]string) (bool, error) {
	if selector == nil {
		return false, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false, err
	}
	return sel.Matches(labels.Set(lbls)), nil
}

func parsePDBValue(input string) *intstr.IntOrString {
	input = strings.TrimSpace(input)
	if strings.HasSuffix(input, "%") {
		return &intstr.IntOrString{Type: intstr.String, StrVal: input}
	}
	if val, err := strconv.Atoi(input); err == nil {
		return &intstr.IntOrString{Type: intstr.Int, IntVal: int32(val)}
	}
	return nil
}

func logAndFixPoorPDBConfig(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	pdb *policyv1.PodDisruptionBudget,
	workloadName string,
	replicas int32,
	namespace string,
	workloadObj interface{},
) {
	defaultPDBConfigLock.RLock()
	fixPoor := defaultPDBConfig.FixPoorPDBs
	defaultPDBConfigLock.RUnlock()

	poorConfig := false

	if pdb.Spec.MinAvailable != nil {
		min := pdb.Spec.MinAvailable
		if min.Type == intstr.Int && min.IntVal == replicas {
			key := fmt.Sprintf("%s/%s/minavailable-equals-replicas", namespace, pdb.Name)
			msg := fmt.Sprintf("WARNING: PDB %s/%s has minAvailable equal to replica count (%d). This is overly restrictive and may block disruptions.", namespace, pdb.Name, replicas)
			logPoorPDBWarning(key, msg)
			poorConfig = true
		}
		if min.Type == intstr.String && strings.TrimSpace(min.StrVal) == "100%" {
			key := fmt.Sprintf("%s/%s/minavailable-100-percent", namespace, pdb.Name)
			msg := fmt.Sprintf("WARNING: PDB %s/%s has minAvailable set to 100%%. This is overly restrictive and may block disruptions.", namespace, pdb.Name)
			logPoorPDBWarning(key, msg)
			poorConfig = true
		}
	}

	if pdb.Spec.MaxUnavailable != nil {
		max := pdb.Spec.MaxUnavailable
		if max.Type == intstr.Int && max.IntVal == 0 {
			key := fmt.Sprintf("%s/%s/maxunavailable-zero", namespace, pdb.Name)
			msg := fmt.Sprintf("WARNING: PDB %s/%s has maxUnavailable set to 0. This is overly restrictive and may block disruptions.", namespace, pdb.Name)
			logPoorPDBWarning(key, msg)
			poorConfig = true
		}
		if max.Type == intstr.String && strings.TrimSpace(max.StrVal) == "0%" {
			key := fmt.Sprintf("%s/%s/maxunavailable-zero-percent", namespace, pdb.Name)
			msg := fmt.Sprintf("WARNING: PDB %s/%s has maxUnavailable set to 0%%. This is overly restrictive and may block disruptions.", namespace, pdb.Name)
			logPoorPDBWarning(key, msg)
			poorConfig = true
		}
	}

	if poorConfig && fixPoor && workloadObj != nil {
		var annotations map[string]string
		switch w := workloadObj.(type) {
		case *appsv1.Deployment:
			annotations = w.Annotations
		case *appsv1.StatefulSet:
			annotations = w.Annotations
		}
		if annotations != nil {
			if val, ok := annotations[annotationBypass]; ok && val == "true" {
				return
			}
		}

		key := fmt.Sprintf("%s/%s", namespace, pdb.Name)
		msg := fmt.Sprintf("FixPoorPDBs enabled: Deleting poor PDB %s/%s and recreating with defaults.", namespace, pdb.Name)
		logFixPoorPDB(key, msg)
		err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, pdb.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			logErrorf("Failed to delete poor PDB %s/%s: %v\n", namespace, pdb.Name, err)
			return
		}
		createPDBForWorkload(ctx, clientset, workloadObj)
	}
}

func createPDBForWorkload(ctx context.Context, clientset *kubernetes.Clientset, obj interface{}) {
	logDebugf("createPDBForWorkload called for object type %T", obj)
	var (
		selector            *metav1.LabelSelector
		namespace, name     string
		workloadAnnotations map[string]string
		replicas            *int32
	)

	switch workload := obj.(type) {
	case *appsv1.Deployment:
		replicas = workload.Spec.Replicas
		if replicas == nil || *replicas < 2 {
			return
		}
		if workload.Annotations != nil {
			if val, ok := workload.Annotations[annotationBypass]; ok && val == "true" {
				return
			}
		}
		selector = workload.Spec.Selector
		workloadAnnotations = workload.Annotations
		namespace = workload.Namespace
		name = workload.Name

		logDebugf("About to check exclusions for %s/%s", namespace, name)
		if isWorkloadExcluded(namespace, name, workload.Labels) {
			logInfof("EXCLUDED: Workload %s/%s is excluded", namespace, name)
			return
		}
		logDebugf("Workload %s/%s is NOT excluded", namespace, name)
	case *appsv1.StatefulSet:
		replicas = workload.Spec.Replicas
		if replicas == nil || *replicas < 2 {
			return
		}
		if workload.Annotations != nil {
			if val, ok := workload.Annotations[annotationBypass]; ok && val == "true" {
				return
			}
		}
		selector = workload.Spec.Selector
		workloadAnnotations = workload.Annotations
		namespace = workload.Namespace
		name = workload.Name

		logDebugf("About to check exclusions for %s/%s", namespace, name)
		if isWorkloadExcluded(namespace, name, workload.Labels) {
			logInfof("EXCLUDED: Workload %s/%s is excluded", namespace, name)
			return
		}
		logDebugf("Workload %s/%s is NOT excluded", namespace, name)
	default:
		return
	}

	if selector == nil {
		return
	}

	workloadSel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		logWarnf("Invalid selector for %s/%s: %v\n", namespace, name, err)
		return
	}

	pdbList, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		logErrorf("Failed to list PDBs in namespace %s: %v\n", namespace, err)
		return
	}

	defaultPDBConfigLock.RLock()
	logInterval := defaultPDBConfig.LogInterval
	defaultPDBConfigLock.RUnlock()

	var existingCastaiPDB *policyv1.PodDisruptionBudget
	var existingNonCastaiPDB *policyv1.PodDisruptionBudget

	for _, pdb := range pdbList.Items {
		if pdb.Spec.Selector != nil {
			pdbSel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
			if err == nil && workloadSel.String() == pdbSel.String() {
				if strings.HasPrefix(pdb.Name, "castai-") {
					existingCastaiPDB = &pdb
				} else {
					existingNonCastaiPDB = &pdb
				}
			}
		}
	}

	if existingNonCastaiPDB != nil {
		key := fmt.Sprintf("%s/%s", namespace, name)
		now := time.Now()
		skipLogTimesLock.Lock()
		last, ok := skipLogTimes[key]
		if !ok || now.Sub(last) > logInterval {
			logInfof("Skipping PDB creation for %s/%s: existing non-castai PDB %s found", namespace, name, existingNonCastaiPDB.Name)
			skipLogTimes[key] = now
		}
		skipLogTimesLock.Unlock()
		return
	}

	if existingCastaiPDB != nil {
		updateExistingPDB(ctx, clientset, existingCastaiPDB, workloadAnnotations, replicas, namespace, name, obj)
		return
	}

	for attempt := 0; attempt < pdbRaceRecheckAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pdbRaceRecheckInterval):
			}
		}
		pdbListAfter, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			logWarnf("Race check: list PDBs in namespace %s failed (attempt %d/%d), will retry or proceed: %v\n",
				namespace, attempt+1, pdbRaceRecheckAttempts, err)
			continue
		}
		for _, pdb := range pdbListAfter.Items {
			if pdb.Spec.Selector != nil {
				pdbSel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
				if err == nil && workloadSel.String() == pdbSel.String() {
					logInfof("Skipping PDB creation for %s/%s: PDB %s was created after initial check", namespace, name, pdb.Name)
					return
				}
			}
		}
	}

	pdbName := fmt.Sprintf("castai-%s-pdb", name)
	existingPDB, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Get(ctx, pdbName, metav1.GetOptions{})
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		logErrorf("Error checking for existing PDB %s/%s: %v\n", namespace, pdbName, err)
		return
	}

	var minAvailable *intstr.IntOrString
	var maxUnavailable *intstr.IntOrString

	if workloadAnnotations != nil {
		if val, ok := workloadAnnotations[annotationMinAvailable]; ok {
			minAvailable = parsePDBValue(val)
		}
		if val, ok := workloadAnnotations[annotationMaxUnavailable]; ok {
			maxUnavailable = parsePDBValue(val)
		}
	}

	if minAvailable == nil && maxUnavailable == nil {
		defaultPDBConfigLock.RLock()
		minAvailable = defaultPDBConfig.MinAvailable
		maxUnavailable = defaultPDBConfig.MaxUnavailable
		defaultPDBConfigLock.RUnlock()
	}

	if minAvailable != nil && maxUnavailable != nil {
		logWarnf("Invalid PDB spec for %s/%s: both pdb-minAvailable and pdb-maxUnavailable set\n", namespace, name)
		return
	}

	if minAvailable == nil && maxUnavailable == nil {
		minAvailable = &intstr.IntOrString{Type: intstr.Int, IntVal: 1}
	}

	pdbSpec := policyv1.PodDisruptionBudgetSpec{
		Selector: selector.DeepCopy(),
	}
	if minAvailable != nil {
		pdbSpec.MinAvailable = minAvailable
	}
	if maxUnavailable != nil {
		pdbSpec.MaxUnavailable = maxUnavailable
	}

	pdbSpec.UnhealthyPodEvictionPolicy = resolveUnhealthyPodEvictionPolicy(workloadAnnotations)

	if exists {
		needsUpdate := false
		if existingPDB.Spec.MinAvailable == nil && pdbSpec.MinAvailable != nil {
			needsUpdate = true
		} else if existingPDB.Spec.MinAvailable != nil && pdbSpec.MinAvailable == nil {
			needsUpdate = true
		} else if existingPDB.Spec.MinAvailable != nil && pdbSpec.MinAvailable != nil &&
			existingPDB.Spec.MinAvailable.String() != pdbSpec.MinAvailable.String() {
			needsUpdate = true
		}
		if existingPDB.Spec.MaxUnavailable == nil && pdbSpec.MaxUnavailable != nil {
			needsUpdate = true
		} else if existingPDB.Spec.MaxUnavailable != nil && pdbSpec.MaxUnavailable == nil {
			needsUpdate = true
		} else if existingPDB.Spec.MaxUnavailable != nil && pdbSpec.MaxUnavailable != nil &&
			existingPDB.Spec.MaxUnavailable.String() != pdbSpec.MaxUnavailable.String() {
			needsUpdate = true
		}
		if !unhealthyPodEvictionPoliciesEqual(existingPDB.Spec.UnhealthyPodEvictionPolicy, pdbSpec.UnhealthyPodEvictionPolicy) {
			needsUpdate = true
		}
		if needsUpdate {
			existingPDB.Spec = pdbSpec
			_, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Update(ctx, existingPDB, metav1.UpdateOptions{})
			if err != nil {
				logErrorf("Failed to update PDB for %s/%s: %v\n", namespace, name, err)
			} else {
				logInfof("Updated PDB for %s/%s\n", namespace, name)
				if replicas != nil {
					logAndFixPoorPDBConfig(ctx, clientset, existingPDB, name, *replicas, namespace, obj)
				}
			}
		}
		return
	}

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pdbName,
			Namespace: namespace,
		},
		Spec: pdbSpec,
	}

	_, err = clientset.PolicyV1().PodDisruptionBudgets(namespace).Create(ctx, pdb, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return
		}
		logErrorf("Failed to create PDB for %s/%s: %v\n", namespace, name, err)
	} else {
		logInfof("No existing PDB for %s/%s, created PDB %s\n", namespace, name, pdbName)
		if replicas != nil {
			logAndFixPoorPDBConfig(ctx, clientset, pdb, name, *replicas, namespace, obj)
		}
	}
}

func deletePDBForWorkload(ctx context.Context, clientset *kubernetes.Clientset, obj interface{}) {
	var namespace, name string

	switch workload := obj.(type) {
	case *appsv1.Deployment:
		namespace = workload.Namespace
		name = workload.Name
	case *appsv1.StatefulSet:
		namespace = workload.Namespace
		name = workload.Name
	default:
		logWarnf("deletePDBForWorkload: unsupported workload type %T", obj)
		return
	}

	pdbName := fmt.Sprintf("castai-%s-pdb", name)
	err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, pdbName, metav1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			logInfof("PDB %s/%s not found, nothing to delete", namespace, pdbName)
		} else {
			logErrorf("Failed to delete PDB %s/%s: %v", namespace, pdbName, err)
		}
	} else {
		logInfof("Deleted PDB %s/%s", namespace, pdbName)
	}
}

func garbageCollectOrphanedPDBs(ctx context.Context, clientset *kubernetes.Clientset) {
	pdbs, err := clientset.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		logErrorf("Failed to list PDBs: %v", err)
		return
	}

	for _, pdb := range pdbs.Items {
		if !strings.HasPrefix(pdb.Name, "castai-") || !strings.HasSuffix(pdb.Name, "-pdb") {
			continue
		}
		workloadName := strings.TrimSuffix(strings.TrimPrefix(pdb.Name, "castai-"), "-pdb")

		_, errDep := clientset.AppsV1().Deployments(pdb.Namespace).Get(ctx, workloadName, metav1.GetOptions{})
		_, errSts := clientset.AppsV1().StatefulSets(pdb.Namespace).Get(ctx, workloadName, metav1.GetOptions{})

		if apierrors.IsNotFound(errDep) && apierrors.IsNotFound(errSts) {
			err := clientset.PolicyV1().PodDisruptionBudgets(pdb.Namespace).Delete(ctx, pdb.Name, metav1.DeleteOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					logInfof("Orphaned PDB %s/%s already deleted", pdb.Namespace, pdb.Name)
				} else {
					logErrorf("Failed to garbage collect orphaned PDB %s/%s: %v", pdb.Namespace, pdb.Name, err)
				}
			} else {
				logInfof("Garbage collected orphaned PDB %s/%s", pdb.Namespace, pdb.Name)
			}
		}
	}
}

func reconcileAllDefaultPDBs(ctx context.Context, clientset *kubernetes.Clientset) {
	logDebugf("reconcileAllDefaultPDBs called")
	namespaces, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		logErrorf("Failed to list namespaces: %v", err)
		return
	}

	for _, ns := range namespaces.Items {
		deployments, err := clientset.AppsV1().Deployments(ns.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			logErrorf("Failed to list Deployments in namespace %s: %v", ns.Name, err)
		} else {
			for _, d := range deployments.Items {
				logDebugf("Reconciliation: Checking deployment %s/%s", d.Namespace, d.Name)
				if !hasCustomPDBAnnotations(d.Annotations) &&
					!hasBypassAnnotation(d.Annotations) &&
					d.Spec.Replicas != nil && *d.Spec.Replicas >= 2 {
					logDebugf("Reconciliation: Deployment %s/%s meets criteria, checking for existing PDB", d.Namespace, d.Name)
					if !workloadHasExistingPDB(ctx, clientset, &d) {
						logDebugf("Reconciliation: No existing PDB for %s/%s, creating PDB", d.Namespace, d.Name)
						createPDBForWorkload(ctx, clientset, &d)
					} else {
						logDebugf("Reconciliation: Existing PDB found for %s/%s, skipping creation", d.Namespace, d.Name)
					}
				} else {
					logDebugf("Reconciliation: Deployment %s/%s does not meet criteria, skipping", d.Namespace, d.Name)
				}
			}
		}

		statefulsets, err := clientset.AppsV1().StatefulSets(ns.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			logErrorf("Failed to list StatefulSets in namespace %s: %v", ns.Name, err)
		} else {
			for _, s := range statefulsets.Items {
				if !hasCustomPDBAnnotations(s.Annotations) &&
					!hasBypassAnnotation(s.Annotations) &&
					s.Spec.Replicas != nil && *s.Spec.Replicas >= 2 {
					if !workloadHasExistingPDB(ctx, clientset, &s) {
						createPDBForWorkload(ctx, clientset, &s)
					}
				}
			}
		}
	}
}

func workloadHasExistingPDB(ctx context.Context, clientset *kubernetes.Clientset, obj interface{}) bool {
	var selector *metav1.LabelSelector
	var namespace, name string
	var labels map[string]string

	switch workload := obj.(type) {
	case *appsv1.Deployment:
		selector = workload.Spec.Selector
		namespace = workload.Namespace
		name = workload.Name
		labels = workload.Labels
	case *appsv1.StatefulSet:
		selector = workload.Spec.Selector
		namespace = workload.Namespace
		name = workload.Name
		labels = workload.Labels
	default:
		return false
	}

	logDebugf("Reconciliation: Checking exclusions for %s/%s", namespace, name)
	if isWorkloadExcluded(namespace, name, labels) {
		logInfof("Reconciliation: Workload %s/%s is excluded, skipping PDB creation", namespace, name)
		return true
	}
	logDebugf("Reconciliation: Workload %s/%s is NOT excluded", namespace, name)

	if selector == nil {
		return false
	}

	workloadSel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false
	}

	pdbList, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false
	}

	for _, pdb := range pdbList.Items {
		if pdb.Spec.Selector != nil {
			pdbSel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
			if err == nil && workloadSel.String() == pdbSel.String() {
				logInfof("Reconciliation: Found existing PDB %s for workload %s/%s, skipping creation", pdb.Name, namespace, name)
				return true
			}
		}
	}

	return false
}

func hasCustomPDBAnnotations(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}
	if _, ok := annotations[annotationMinAvailable]; ok {
		return true
	}
	if _, ok := annotations[annotationMaxUnavailable]; ok {
		return true
	}
	if _, ok := annotations[annotationUnhealthyPodEvictionPolicy]; ok {
		return true
	}
	return false
}

func hasBypassAnnotation(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}
	if val, ok := annotations[annotationBypass]; ok && val == "true" {
		return true
	}
	return false
}

func scanAllPDBsForPoorConfig(ctx context.Context, clientset *kubernetes.Clientset) {
	pdbs, err := clientset.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		logErrorf("Failed to list PDBs for audit: %v", err)
		return
	}
	for _, pdb := range pdbs.Items {
		if pdb.Spec.Selector == nil {
			continue
		}
		pdbSelector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			logWarnf("Invalid label selector for PDB %s/%s: %v", pdb.Namespace, pdb.Name, err)
			continue
		}

		deployments, err := clientset.AppsV1().Deployments(pdb.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			logErrorf("Failed to list Deployments in namespace %s: %v", pdb.Namespace, err)
		} else {
			for _, deploy := range deployments.Items {
				if deploy.Annotations != nil && deploy.Annotations[annotationBypass] == "true" {
					continue
				}
				if deploy.Spec.Selector == nil {
					continue
				}
				workloadSelector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
				if err != nil {
					logWarnf("Invalid selector for Deployment %s/%s: %v", deploy.Namespace, deploy.Name, err)
					continue
				}
				if pdbSelector.String() == workloadSelector.String() {
					replicas := int32(1)
					if deploy.Spec.Replicas != nil {
						replicas = *deploy.Spec.Replicas
					}
					logAndFixPoorPDBConfig(ctx, clientset, &pdb, deploy.Name, replicas, pdb.Namespace, &deploy)
				}
			}
		}

		statefulsets, err := clientset.AppsV1().StatefulSets(pdb.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			logErrorf("Failed to list StatefulSets in namespace %s: %v", pdb.Namespace, err)
		} else {
			for _, sts := range statefulsets.Items {
				if sts.Annotations != nil && sts.Annotations[annotationBypass] == "true" {
					continue
				}
				if sts.Spec.Selector == nil {
					continue
				}
				workloadSelector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
				if err != nil {
					logWarnf("Invalid selector for StatefulSet %s/%s: %v", sts.Namespace, sts.Name, err)
					continue
				}
				if pdbSelector.String() == workloadSelector.String() {
					replicas := int32(1)
					if sts.Spec.Replicas != nil {
						replicas = *sts.Spec.Replicas
					}
					logAndFixPoorPDBConfig(ctx, clientset, &pdb, sts.Name, replicas, pdb.Namespace, &sts)
				}
			}
		}
	}
}

func isPoorPDBConfig(pdb *policyv1.PodDisruptionBudget, replicas int32) bool {
	if pdb.Spec.MinAvailable != nil {
		min := pdb.Spec.MinAvailable
		if (min.Type == intstr.Int && min.IntVal == replicas) ||
			(min.Type == intstr.String && strings.TrimSpace(min.StrVal) == "100%") {
			return true
		}
	}
	if pdb.Spec.MaxUnavailable != nil {
		max := pdb.Spec.MaxUnavailable
		if (max.Type == intstr.Int && max.IntVal == 0) ||
			(max.Type == intstr.String && strings.TrimSpace(max.StrVal) == "0%") {
			return true
		}
	}
	return false
}

func scanAllPDBsForMultiplePDBs(ctx context.Context, clientset *kubernetes.Clientset) {
	namespaces, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		logErrorf("Failed to list namespaces: %v", err)
		return
	}

	for _, ns := range namespaces.Items {
		namespace := ns.Name

		pdbList, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			logErrorf("Failed to list PDBs in namespace %s: %v", namespace, err)
			continue
		}

		deployments, err := clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			logErrorf("Failed to list Deployments in namespace %s: %v", namespace, err)
		} else {
			for _, deploy := range deployments.Items {
				if deploy.Annotations != nil && deploy.Annotations[annotationBypass] == "true" {
					continue
				}
				if deploy.Spec.Selector == nil {
					continue
				}
				workloadSelector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
				if err != nil {
					logWarnf("Invalid selector for Deployment %s/%s: %v", deploy.Namespace, deploy.Name, err)
					continue
				}
				matchingPDBs := []*policyv1.PodDisruptionBudget{}
				for i, pdb := range pdbList.Items {
					if pdb.Spec.Selector == nil {
						continue
					}
					pdbSelector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
					if err != nil {
						continue
					}
					if pdbSelector.String() == workloadSelector.String() {
						matchingPDBs = append(matchingPDBs, &pdbList.Items[i])
					}
				}
				if len(matchingPDBs) > 1 {
					nonCastaiCount := 0
					for _, pdb := range matchingPDBs {
						if !(strings.HasPrefix(pdb.Name, "castai-") && strings.HasSuffix(pdb.Name, "-pdb")) {
							nonCastaiCount++
						}
					}
					if nonCastaiCount > 0 {
						for _, pdb := range matchingPDBs {
							if strings.HasPrefix(pdb.Name, "castai-") && strings.HasSuffix(pdb.Name, "-pdb") {
								err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, pdb.Name, metav1.DeleteOptions{})
								if err != nil && !apierrors.IsNotFound(err) {
									logErrorf("Failed to delete castai PDB %s/%s: %v", namespace, pdb.Name, err)
								} else {
									logInfof("Deleted castai PDB %s/%s due to multiple PDBs targeting deployment %s", namespace, pdb.Name, deploy.Name)
								}
							}
						}
					}
					if nonCastaiCount > 1 {
						logWarnf("WARNING: Multiple non-castai PDBs target deployment %s/%s", namespace, deploy.Name)
					}
				}
			}
		}

		statefulsets, err := clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			logErrorf("Failed to list StatefulSets in namespace %s: %v", namespace, err)
		} else {
			for _, sts := range statefulsets.Items {
				if sts.Annotations != nil && sts.Annotations[annotationBypass] == "true" {
					continue
				}
				if sts.Spec.Selector == nil {
					continue
				}
				workloadSelector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
				if err != nil {
					logWarnf("Invalid selector for StatefulSet %s/%s: %v", sts.Namespace, sts.Name, err)
					continue
				}
				matchingPDBs := []*policyv1.PodDisruptionBudget{}
				for i, pdb := range pdbList.Items {
					if pdb.Spec.Selector == nil {
						continue
					}
					pdbSelector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
					if err != nil {
						continue
					}
					if pdbSelector.String() == workloadSelector.String() {
						matchingPDBs = append(matchingPDBs, &pdbList.Items[i])
					}
				}
				if len(matchingPDBs) > 1 {
					nonCastaiCount := 0
					for _, pdb := range matchingPDBs {
						if !(strings.HasPrefix(pdb.Name, "castai-") && strings.HasSuffix(pdb.Name, "-pdb")) {
							nonCastaiCount++
						}
					}
					if nonCastaiCount > 0 {
						for _, pdb := range matchingPDBs {
							if strings.HasPrefix(pdb.Name, "castai-") && strings.HasSuffix(pdb.Name, "-pdb") {
								err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, pdb.Name, metav1.DeleteOptions{})
								if err != nil && !apierrors.IsNotFound(err) {
									logErrorf("Failed to delete castai PDB %s/%s: %v", namespace, pdb.Name, err)
								} else {
									logInfof("Deleted castai PDB %s/%s due to multiple PDBs targeting statefulset %s", namespace, pdb.Name, sts.Name)
								}
							}
						}
					}
					if nonCastaiCount > 1 {
						logWarnf("WARNING: Multiple non-castai PDBs target statefulset %s/%s", namespace, sts.Name)
					}
				}
			}
		}
	}
}
