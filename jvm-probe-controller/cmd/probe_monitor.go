package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// PodEventMonitor tracks probe failures and schedules fixes
type PodEventMonitor struct {
	clientset  *kubernetes.Clientset
	factory    informers.SharedInformerFactory
	pods       cache.SharedIndexInformer
	events     cache.SharedIndexInformer

	// Track failures: workload key -> container -> failure info
	failures     map[string]*ContainerFailureInfo
	failuresLock sync.RWMutex

	// Track permanently failed probes to avoid spam
	permanentFailures map[string]*PermanentFailureRecord
	pfLock            sync.RWMutex

	// Work queue for fixes
	fixQueue chan FixRequest
}

// ContainerFailureInfo tracks failure details per container
type ContainerFailureInfo struct {
	ContainerName       string
	WorkloadName        string
	WorkloadNamespace   string
	WorkloadKind        string
	ProbeType           string // liveness, readiness, startup
	FailureCount        int
	FirstFailure        time.Time
	LastFailure         time.Time
	FailureMessages     []string
	RestartCount        int32
	OriginalDelay       int32
	OriginalThreshold   int32
}

// PermanentFailureRecord tracks probes that were fixed
type PermanentFailureRecord struct {
	FixedAt     time.Time
	FixApplied  ProbeFix
	FailureRate float64
}

// FixRequest represents a request to fix probe settings
type FixRequest struct {
	WorkloadKey       string
	WorkloadNamespace string
	WorkloadName      string
	WorkloadKind      string
	ContainerName     string
	ProbeType         string
	CurrentDelay      int32
	CurrentThreshold  int32
	FailureInfo       *ContainerFailureInfo
}

// ProbeFix represents the changes made to fix a probe
type ProbeFix struct {
	NewInitialDelaySeconds  int32
	NewFailureThreshold     int32
	NewTimeoutSeconds       int32
	NewPeriodSeconds        int32
	Reason                  string
}

// NewPodEventMonitor creates a new pod event monitor
func NewPodEventMonitor(clientset *kubernetes.Clientset, factory informers.SharedInformerFactory) *PodEventMonitor {
	return &PodEventMonitor{
		clientset:         clientset,
		factory:           factory,
		failures:          make(map[string]*ContainerFailureInfo),
		permanentFailures: make(map[string]*PermanentFailureRecord),
		fixQueue:          make(chan FixRequest, 100),
	}
}

// Run starts the pod event monitor
func (p *PodEventMonitor) Run(ctx context.Context) {
	// Create informers
	p.pods = p.factory.Core().V1().Pods().Informer()
	p.events = p.factory.Core().V1().Events().Informer()

	// Add handlers for pods - track restart counts
	p.pods.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: p.handlePodUpdate,
	})

	// Add handlers for events - track probe failures
	p.events.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    p.handleEventAdd,
		UpdateFunc: p.handleEventUpdate,
	})

	logAlways("PodEventMonitor: Started watching for probe failures")

	// Start fix processor
	go p.processFixQueue(ctx)

	// Start cleanup routine
	go p.cleanupRoutine(ctx)

	<-ctx.Done()
}

// handlePodUpdate tracks container restarts due to probe failures
func (p *PodEventMonitor) handlePodUpdate(oldObj, newObj interface{}) {
	oldPod, ok1 := oldObj.(*corev1.Pod)
	newPod, ok2 := newObj.(*corev1.Pod)
	if !ok1 || !ok2 || newPod == nil {
		return
	}

	// Check owner reference to find the workload
	workloadKey, workloadKind := p.getWorkloadFromPod(newPod)
	if workloadKey == "" {
		return
	}

	// Check for restart count increases
	for _, newStatus := range newPod.Status.ContainerStatuses {
		for _, oldStatus := range oldPod.Status.ContainerStatuses {
			if newStatus.Name == oldStatus.Name && newStatus.RestartCount > oldStatus.RestartCount {
				// Container restarted - likely due to probe failure
				p.recordRestart(workloadKey, workloadKind, newPod.Namespace, newPod.Name, newStatus.Name, newStatus.RestartCount)
			}
		}
	}
}

// handleEventAdd processes new events
func (p *PodEventMonitor) handleEventAdd(obj interface{}) {
	if event, ok := obj.(*corev1.Event); ok {
		p.processProbeEvent(event)
	}
}

// handleEventUpdate processes updated events
func (p *PodEventMonitor) handleEventUpdate(oldObj, newObj interface{}) {
	if event, ok := newObj.(*corev1.Event); ok {
		p.processProbeEvent(event)
	}
}

// processProbeEvent analyzes events for probe failures
func (p *PodEventMonitor) processProbeEvent(event *corev1.Event) {
	// Only interested in events related to pods
	if event.InvolvedObject.Kind != "Pod" {
		return
	}

	// Check for probe-related events
	reason := event.Reason
	message := event.Message

	probeEventTypes := map[string]string{
		"Unhealthy":            "liveness",
		"ProbeFailed":          "unknown",
		"ReadinessProbeFailed": "readiness",
		"LivenessProbeFailed":  "liveness",
		"StartupProbeFailed":   "startup",
	}

	probeType, isProbe := probeEventTypes[reason]
	if !isProbe {
		return
	}

	// Try to determine probe type from message
	if probeType == "unknown" {
		probeType = determineProbeTypeFromMessage(message)
	}

	// Get pod to find workload
	pod, err := p.clientset.CoreV1().Pods(event.Namespace).Get(
		context.Background(), event.InvolvedObject.Name, metav1.GetOptions{},
	)
	if err != nil {
		return
	}

	workloadKey, workloadKind := p.getWorkloadFromPod(pod)
	if workloadKey == "" {
		return
	}

	// Find container name from message
	containerName := extractContainerName(event.Message)
	if containerName == "" {
		// Try to get from pod spec
		for _, c := range pod.Spec.Containers {
			containerName = c.Name
			break
		}
	}

	// Record the failure
	p.recordProbeFailure(workloadKey, workloadKind, event.Namespace, event.Name, containerName, probeType, message)
}

// recordProbeFailure records a probe failure event
func (p *PodEventMonitor) recordProbeFailure(workloadKey, workloadKind, namespace, podName, containerName, probeType, message string) {
	key := fmt.Sprintf("%s/%s/%s", workloadKey, containerName, probeType)

	p.failuresLock.Lock()
	defer p.failuresLock.Unlock()

	info, exists := p.failures[key]
	if !exists {
		info = &ContainerFailureInfo{
			ContainerName:     containerName,
			WorkloadName:      extractWorkloadName(workloadKey),
			WorkloadNamespace: namespace,
			WorkloadKind:      workloadKind,
			ProbeType:         probeType,
			FailureCount:      0,
			FirstFailure:      time.Now(),
			FailureMessages:   make([]string, 0),
		}
		p.failures[key] = info
	}

	info.FailureCount++
	info.LastFailure = time.Now()
	info.FailureMessages = append(info.FailureMessages, message)

	// Keep only last 10 messages
	if len(info.FailureMessages) > 10 {
		info.FailureMessages = info.FailureMessages[1:]
	}

	// Log failure with special format for visibility
	logWarn("probe-failure",
		"[PROBE FAILURE] %s container=%s probe=%s count=%d message=%s",
		workloadKey, containerName, probeType, info.FailureCount, message)

	// Check if we should trigger a fix
	if p.shouldTriggerFix(info) {
		p.triggerFix(info)
	}
}

// recordRestart records a container restart
func (p *PodEventMonitor) recordRestart(workloadKey, workloadKind, namespace, podName, containerName string, restartCount int32) {
	key := fmt.Sprintf("%s/%s/restart", workloadKey, containerName)

	p.failuresLock.Lock()
	defer p.failuresLock.Unlock()

	info, exists := p.failures[key]
	if !exists {
		info = &ContainerFailureInfo{
			ContainerName:     containerName,
			WorkloadName:      extractWorkloadName(workloadKey),
			WorkloadNamespace: namespace,
			WorkloadKind:      workloadKind,
			ProbeType:         "restart",
			FailureCount:      0,
			FirstFailure:      time.Now(),
		}
		p.failures[key] = info
	}

	info.RestartCount = restartCount
	info.LastFailure = time.Now()
	info.FailureCount++

	// Log restart event
	logError("container-restart",
		"[CONTAINER RESTART] %s/%s container=%s restarts=%d",
		namespace, podName, containerName, restartCount)
}

// shouldTriggerFix determines if a fix should be triggered
func (p *PodEventMonitor) shouldTriggerFix(info *ContainerFailureInfo) bool {
	// Check if already fixed
	p.pfLock.RLock()
	key := fmt.Sprintf("%s/%s/%s/%s", info.WorkloadNamespace, info.WorkloadName, info.ContainerName, info.ProbeType)
	if record, exists := p.permanentFailures[key]; exists {
		// Don't fix again within 30 minutes
		if time.Since(record.FixedAt) < 30*time.Minute {
			p.pfLock.RUnlock()
			return false
		}
	}
	p.pfLock.RUnlock()

	// Trigger conditions:
	// 1. More than 5 failures in 5 minutes
	// 2. More than 3 container restarts
	// 3. Failure rate > 50% over 10 minutes

	timeWindow := time.Since(info.FirstFailure)
	if timeWindow > 10*time.Minute {
		timeWindow = 10 * time.Minute
	}

	if info.FailureCount >= 5 && timeWindow < 5*time.Minute {
		return true
	}

	if info.RestartCount >= 3 {
		return true
	}

	return false
}

// triggerFix creates a fix request
func (p *PodEventMonitor) triggerFix(info *ContainerFailureInfo) {
	workloadKey := fmt.Sprintf("%s/%s", info.WorkloadNamespace, info.WorkloadName)

	// Get current probe settings
	currentDelay, currentThreshold := p.getCurrentProbeSettings(
		info.WorkloadNamespace, info.WorkloadName, info.WorkloadKind, info.ContainerName, info.ProbeType,
	)

	req := FixRequest{
		WorkloadKey:       workloadKey,
		WorkloadNamespace: info.WorkloadNamespace,
		WorkloadName:      info.WorkloadName,
		WorkloadKind:      info.WorkloadKind,
		ContainerName:     info.ContainerName,
		ProbeType:         info.ProbeType,
		CurrentDelay:      currentDelay,
		CurrentThreshold:  currentThreshold,
		FailureInfo:       info,
	}

	select {
	case p.fixQueue <- req:
		logInfo("fix-queued", "Queued probe fix for %s/%s container=%s probe=%s",
			info.WorkloadNamespace, info.WorkloadName, info.ContainerName, info.ProbeType)
	default:
		logWarn("fix-queue-full", "Fix queue full, dropping fix request for %s", workloadKey)
	}
}

// processFixQueue processes fix requests
func (p *PodEventMonitor) processFixQueue(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-p.fixQueue:
			p.applyFix(ctx, req)
		}
	}
}

// applyFix applies a probe fix to the workload
func (p *PodEventMonitor) applyFix(ctx context.Context, req FixRequest) {
	// Calculate new probe settings based on failure pattern
	fix := p.calculateProbeFix(req)

	// Apply the patch
	err := p.patchWorkloadProbes(ctx, req, fix)
	if err != nil {
		logError("fix-failed", "Failed to apply probe fix for %s: %v", req.WorkloadKey, err)
		return
	}

	// Record the fix
	p.pfLock.Lock()
	key := fmt.Sprintf("%s/%s/%s/%s", req.WorkloadNamespace, req.WorkloadName, req.ContainerName, req.ProbeType)
	p.permanentFailures[key] = &PermanentFailureRecord{
		FixedAt:     time.Now(),
		FixApplied:  fix,
		FailureRate: float64(req.FailureInfo.FailureCount) / time.Since(req.FailureInfo.FirstFailure).Minutes(),
	}
	p.pfLock.Unlock()

	// Log the fix with prominent formatting
	logAlways("╔════════════════════════════════════════════════════════════════╗")
	logAlways("║              PROBE FIX APPLIED                                 ║")
	logAlways("╠════════════════════════════════════════════════════════════════╣")
	logAlways("║ Workload: %s/%s", req.WorkloadNamespace, req.WorkloadName)
	logAlways("║ Container: %s", req.ContainerName)
	logAlways("║ Probe Type: %s", req.ProbeType)
	logAlways("║ Reason: %s", fix.Reason)
	logAlways("║ Changes:")
	logAlways("║   InitialDelay: %d → %d seconds", req.CurrentDelay, fix.NewInitialDelaySeconds)
	logAlways("║   FailureThreshold: %d → %d", req.CurrentThreshold, fix.NewFailureThreshold)
	logAlways("╚════════════════════════════════════════════════════════════════╝")

	// Emit Kubernetes event
	p.emitFixEvent(ctx, req, fix)
}

// calculateProbeFix calculates the new probe settings based on failure pattern
func (p *PodEventMonitor) calculateProbeFix(req FixRequest) ProbeFix {
	fix := ProbeFix{
		NewInitialDelaySeconds: req.CurrentDelay,
		NewFailureThreshold:    req.CurrentThreshold,
		NewTimeoutSeconds:      5,
		NewPeriodSeconds:       10,
	}

	// Analyze failure pattern
	failureRate := float64(req.FailureInfo.FailureCount)
	if time.Since(req.FailureInfo.FirstFailure) > 0 {
		failureRate = failureRate / time.Since(req.FailureInfo.FirstFailure).Minutes()
	}

	// High failure rate - increase initial delay significantly
	if failureRate > 10 {
		fix.NewInitialDelaySeconds = req.CurrentDelay * 3
		if fix.NewInitialDelaySeconds < 120 {
			fix.NewInitialDelaySeconds = 120
		}
		fix.Reason = "High failure rate - container needs more startup time"
	} else if failureRate > 5 {
		fix.NewInitialDelaySeconds = req.CurrentDelay * 2
		if fix.NewInitialDelaySeconds < 60 {
			fix.NewInitialDelaySeconds = 60
		}
		fix.Reason = "Moderate failure rate - increasing startup delay"
	} else {
		fix.NewInitialDelaySeconds = req.CurrentDelay + 30
		fix.Reason = "Low failure rate - slight adjustment needed"
	}

	// Cap at reasonable limits
	if fix.NewInitialDelaySeconds > 300 {
		fix.NewInitialDelaySeconds = 300
	}

	// Increase failure threshold if many restarts
	if req.FailureInfo.RestartCount > 0 {
		fix.NewFailureThreshold = req.CurrentThreshold + 3
		if fix.NewFailureThreshold > 10 {
			fix.NewFailureThreshold = 10
		}
	}

	// Slightly increase timeout for slow-responding services
	fix.NewTimeoutSeconds = req.CurrentDelay/10 + 5
	if fix.NewTimeoutSeconds > 30 {
		fix.NewTimeoutSeconds = 30
	}

	return fix
}

// patchWorkloadProbes applies a JSON patch to fix probe settings
func (p *PodEventMonitor) patchWorkloadProbes(ctx context.Context, req FixRequest, fix ProbeFix) error {
	// Create patch operations
	patches := make([]map[string]interface{}, 0)

	probePath := fmt.Sprintf("/spec/template/spec/containers/")
	if req.WorkloadKind == "Deployment" || req.WorkloadKind == "StatefulSet" {
		// Need to find container index
		var containerIndex int
		var err error
		containerIndex, err = p.findContainerIndex(ctx, req.WorkloadNamespace, req.WorkloadName, req.WorkloadKind, req.ContainerName)
		if err != nil {
			return err
		}
		probePath = fmt.Sprintf("%s%d/", probePath, containerIndex)
	}

	// Build probe patch based on probe type
	probeJSON := map[string]interface{}{
		"initialDelaySeconds": fix.NewInitialDelaySeconds,
		"periodSeconds":       fix.NewPeriodSeconds,
		"timeoutSeconds":      fix.NewTimeoutSeconds,
		"failureThreshold":    fix.NewFailureThreshold,
		"successThreshold":    1,
	}

	switch req.ProbeType {
	case "liveness":
		probePath += "livenessProbe"
		if req.CurrentDelay == 0 {
			// Add complete probe if doesn't exist
			probeJSON["httpGet"] = map[string]interface{}{
				"path": "/actuator/health/liveness",
				"port": 8080,
			}
			patches = append(patches, map[string]interface{}{
				"op":    "add",
				"path":  probePath,
				"value": probeJSON,
			})
		} else {
			// Replace existing probe
			patches = append(patches, map[string]interface{}{
				"op":    "replace",
				"path":  probePath,
				"value": probeJSON,
			})
		}
	case "readiness":
		probePath += "readinessProbe"
		if req.CurrentDelay == 0 {
			probeJSON["httpGet"] = map[string]interface{}{
				"path": "/actuator/health/readiness",
				"port": 8080,
			}
			patches = append(patches, map[string]interface{}{
				"op":    "add",
				"path":  probePath,
				"value": probeJSON,
			})
		} else {
			patches = append(patches, map[string]interface{}{
				"op":    "replace",
				"path":  probePath,
				"value": probeJSON,
			})
		}
	case "startup":
		probePath += "startupProbe"
		// Always add startup probe if fixing
		if req.CurrentDelay == 0 {
			probeJSON["httpGet"] = map[string]interface{}{
				"path": "/actuator/health",
				"port": 8080,
			}
			patches = append(patches, map[string]interface{}{
				"op":    "add",
				"path":  probePath,
				"value": probeJSON,
			})
		} else {
			patches = append(patches, map[string]interface{}{
				"op":    "replace",
				"path":  probePath,
				"value": probeJSON,
			})
		}
	}

	if len(patches) == 0 {
		return fmt.Errorf("no patches to apply")
	}

	// Apply patches
	patchBytes, err := json.Marshal(patches)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %v", err)
	}

	var patchErr error
	switch req.WorkloadKind {
	case "Deployment":
		_, patchErr = p.clientset.AppsV1().Deployments(req.WorkloadNamespace).Patch(
			ctx, req.WorkloadName, types.JSONPatchType, patchBytes, metav1.PatchOptions{},
		)
	case "StatefulSet":
		_, patchErr = p.clientset.AppsV1().StatefulSets(req.WorkloadNamespace).Patch(
			ctx, req.WorkloadName, types.JSONPatchType, patchBytes, metav1.PatchOptions{},
		)
	default:
		return fmt.Errorf("unsupported workload kind: %s", req.WorkloadKind)
	}

	return patchErr
}

// findContainerIndex finds the index of a container in the workload
func (p *PodEventMonitor) findContainerIndex(ctx context.Context, namespace, name, kind, containerName string) (int, error) {
	switch kind {
	case "Deployment":
		dep, err := p.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		for i, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == containerName {
				return i, nil
			}
		}
	case "StatefulSet":
		sts, err := p.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		for i, c := range sts.Spec.Template.Spec.Containers {
			if c.Name == containerName {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("container %s not found", containerName)
}

// getCurrentProbeSettings retrieves current probe settings from workload
func (p *PodEventMonitor) getCurrentProbeSettings(namespace, name, kind, containerName, probeType string) (delay, threshold int32) {
	ctx := context.Background()

	switch kind {
	case "Deployment":
		dep, err := p.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 30, 3 // defaults
		}
		for _, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == containerName {
				switch probeType {
				case "liveness":
					if c.LivenessProbe != nil {
						return c.LivenessProbe.InitialDelaySeconds, c.LivenessProbe.FailureThreshold
					}
				case "readiness":
					if c.ReadinessProbe != nil {
						return c.ReadinessProbe.InitialDelaySeconds, c.ReadinessProbe.FailureThreshold
					}
				case "startup":
					if c.StartupProbe != nil {
						return c.StartupProbe.InitialDelaySeconds, c.StartupProbe.FailureThreshold
					}
				}
				break
			}
		}
	case "StatefulSet":
		sts, err := p.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 30, 3
		}
		for _, c := range sts.Spec.Template.Spec.Containers {
			if c.Name == containerName {
				switch probeType {
				case "liveness":
					if c.LivenessProbe != nil {
						return c.LivenessProbe.InitialDelaySeconds, c.LivenessProbe.FailureThreshold
					}
				case "readiness":
					if c.ReadinessProbe != nil {
						return c.ReadinessProbe.InitialDelaySeconds, c.ReadinessProbe.FailureThreshold
					}
				case "startup":
					if c.StartupProbe != nil {
						return c.StartupProbe.InitialDelaySeconds, c.StartupProbe.FailureThreshold
					}
				}
				break
			}
		}
	}

	return 30, 3 // defaults
}

// emitFixEvent emits a Kubernetes event for the fix
func (p *PodEventMonitor) emitFixEvent(ctx context.Context, req FixRequest, fix ProbeFix) {
	ref := &corev1.ObjectReference{
		Kind:      req.WorkloadKind,
		Namespace: req.WorkloadNamespace,
		Name:      req.WorkloadName,
	}

	reason := "ProbeAutoFixed"
	message := fmt.Sprintf(
		"Probe %s for container %s auto-fixed. InitialDelay: %d→%d, FailureThreshold: %d→%d. Reason: %s",
		req.ProbeType, req.ContainerName,
		req.CurrentDelay, fix.NewInitialDelaySeconds,
		req.CurrentThreshold, fix.NewFailureThreshold,
		fix.Reason,
	)

	// This would need the event recorder from main
	// recorder.Event(ref, corev1.EventTypeWarning, reason, message)
	_ = ref
	_ = reason
	_ = message
	_ = ctx
}

// cleanupRoutine cleans up old failure records
func (p *PodEventMonitor) cleanupRoutine(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.cleanup()
		}
	}
}

// cleanup removes old failure records
func (p *PodEventMonitor) cleanup() {
	p.failuresLock.Lock()
	defer p.failuresLock.Unlock()

	now := time.Now()
	for key, info := range p.failures {
		// Remove records older than 1 hour
		if now.Sub(info.LastFailure) > time.Hour {
			delete(p.failures, key)
		}
	}
}

// getWorkloadFromPod extracts the parent workload from pod owner references
func (p *PodEventMonitor) getWorkloadFromPod(pod *corev1.Pod) (string, string) {
	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {
		case "ReplicaSet":
			// Get the ReplicaSet to find the Deployment
			rs, err := p.clientset.AppsV1().ReplicaSets(pod.Namespace).Get(
				context.Background(), ref.Name, metav1.GetOptions{},
			)
			if err == nil && len(rs.OwnerReferences) > 0 {
				for _, rsRef := range rs.OwnerReferences {
					if rsRef.Kind == "Deployment" {
						return fmt.Sprintf("%s/%s", pod.Namespace, rsRef.Name), "Deployment"
					}
				}
			}
			return fmt.Sprintf("%s/%s", pod.Namespace, ref.Name), "ReplicaSet"
		case "StatefulSet":
			return fmt.Sprintf("%s/%s", pod.Namespace, ref.Name), "StatefulSet"
		case "DaemonSet":
			return fmt.Sprintf("%s/%s", pod.Namespace, ref.Name), "DaemonSet"
		case "Job":
			return fmt.Sprintf("%s/%s", pod.Namespace, ref.Name), "Job"
		}
	}
	return "", ""
}

// Helper functions
func extractContainerName(message string) string {
	// Try to extract container name from common message patterns
	patterns := []string{
		`container "([^"]+)"`,
		`Liveness probe failed:.*container ([^ ]+)`,
		`Readiness probe failed:.*container ([^ ]+)`,
		`Startup probe failed:.*container ([^ ]+)`,
		`Container ([^ ]+) (failed|unhealthy)`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(message)
		if len(matches) > 1 {
			return matches[1]
		}
	}

	return ""
}

func extractWorkloadName(workloadKey string) string {
	parts := strings.Split(workloadKey, "/")
	if len(parts) >= 2 {
		return parts[1]
	}
	return workloadKey
}

func determineProbeTypeFromMessage(message string) string {
	msgLower := strings.ToLower(message)
	if strings.Contains(msgLower, "liveness") {
		return "liveness"
	}
	if strings.Contains(msgLower, "readiness") {
		return "readiness"
	}
	if strings.Contains(msgLower, "startup") {
		return "startup"
	}
	return "unknown"
}

// ForceOverwriteProbe marks a probe for forced overwrite
func (p *PodEventMonitor) ForceOverwriteProbe(workloadKey, containerName, probeType string) {
	p.pfLock.Lock()
	defer p.pfLock.Unlock()

	key := fmt.Sprintf("force/%s/%s/%s", workloadKey, containerName, probeType)
	p.permanentFailures[key] = &PermanentFailureRecord{
		FixedAt:    time.Now(),
		FixApplied: ProbeFix{Reason: "Forced overwrite via annotation"},
	}
}

// IsForceOverwriteEnabled checks if force overwrite is enabled
func (p *PodEventMonitor) IsForceOverwriteEnabled(workloadKey, containerName, probeType string) bool {
	p.pfLock.RLock()
	defer p.pfLock.RUnlock()

	key := fmt.Sprintf("force/%s/%s/%s", workloadKey, containerName, probeType)
	_, exists := p.permanentFailures[key]
	return exists
}
