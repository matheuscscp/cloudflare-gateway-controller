// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

var gatewayGVR = schema.GroupVersionResource{
	Group:    "gateway.networking.k8s.io",
	Version:  "v1",
	Resource: "gateways",
}

// watchSuspendedError is returned when the Gateway is suspended while
// there are still Deployments that haven't been updated.
type watchSuspendedError struct {
	pending int
	total   int
}

func (e *watchSuspendedError) Error() string {
	return fmt.Sprintf("Gateway was suspended while %d/%d deployment(s) are still pending update", e.pending, e.total)
}

// rotationEvent records a state change observed during the rolling update.
type rotationEvent struct {
	Time   time.Time
	Source string // "Gateway/<name>", "Deployment/<name>", "Pod/<name>"
	Kind   string // event type
	Detail string // additional info
}

// informerEvent wraps a change observed by an informer.
type informerEvent struct {
	resourceKind string // "Deployment", "Pod", "Gateway"
	changeType   string // "update", "add", "delete"
	obj          any
}

// watchRotation watches the rolling update of a token rotation until completion.
//
// Parameters:
//   - numDeployments: number of tunnel Deployments to watch
//   - currentTokenHash: the token hash from CGS (Deployments matching this are "up to date")
//   - patchFn: if non-nil, called after informer sync to start a new rotation.
//     If nil, attaches to an ongoing rotation.
func watchRotation(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, namespace, gatewayName string, numDeployments int, currentTokenHash string, patchFn func() error) error {
	labelSelector := fmt.Sprintf("app.kubernetes.io/instance=%s", gatewayName)

	// Create informer factories with namespace and label/field filtering.
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = labelSelector
		}),
	)
	dynFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynClient, 0, namespace,
		func(opts *metav1.ListOptions) {
			opts.FieldSelector = fmt.Sprintf("metadata.name=%s", gatewayName)
		},
	)

	eventCh := make(chan informerEvent, 256)

	// Register event handlers before starting informers.
	if _, err := factory.Apps().V1().Deployments().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, newObj any) {
			eventCh <- informerEvent{"Deployment", "update", newObj}
		},
	}); err != nil {
		return fmt.Errorf("adding Deployment event handler: %w", err)
	}
	if _, err := factory.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			eventCh <- informerEvent{"Pod", "add", obj}
		},
		UpdateFunc: func(_, newObj any) {
			eventCh <- informerEvent{"Pod", "update", newObj}
		},
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			eventCh <- informerEvent{"Pod", "delete", obj}
		},
	}); err != nil {
		return fmt.Errorf("adding Pod event handler: %w", err)
	}
	if _, err := dynFactory.ForResource(gatewayGVR).Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, newObj any) {
			eventCh <- informerEvent{"Gateway", "update", newObj}
		},
	}); err != nil {
		return fmt.Errorf("adding Gateway event handler: %w", err)
	}

	// Start informers and wait for initial cache sync.
	stopCh := make(chan struct{})
	defer close(stopCh)
	factory.Start(stopCh)
	dynFactory.Start(stopCh)

	if !cache.WaitForCacheSync(ctx.Done(),
		factory.Apps().V1().Deployments().Informer().HasSynced,
		factory.Core().V1().Pods().Informer().HasSynced,
		dynFactory.ForResource(gatewayGVR).Informer().HasSynced,
	) {
		return fmt.Errorf("timed out waiting for informer cache sync")
	}

	// Drain initial sync events (Add/Update events from initial list).
drainLoop:
	for {
		select {
		case <-eventCh:
		default:
			break drainLoop
		}
	}

	// Snapshot Gateway state BEFORE patching so we can detect the change.
	gwRuntimeObj, err := dynFactory.ForResource(gatewayGVR).Lister().ByNamespace(namespace).Get(gatewayName)
	if err != nil {
		return fmt.Errorf("getting Gateway from cache: %w", err)
	}
	gwObj := gwRuntimeObj.(*unstructured.Unstructured)
	lastGWProgrammed := getUnstructuredCondition(gwObj, "Programmed")
	lastGWReady := getUnstructuredCondition(gwObj, "Ready")
	lastRotateAt := getUnstructuredAnnotation(gwObj, apiv1.AnnotationRotateTokenRequestedAt)

	// Trigger the rotation if starting a new one.
	if patchFn != nil {
		if err := patchFn(); err != nil {
			return err
		}
	} else {
		fmt.Printf("Attaching to ongoing token rotation for Gateway %s/%s\n", namespace, gatewayName)
	}

	// Build initial state from informer caches.
	n := numDeployments
	deployList, err := factory.Apps().V1().Deployments().Lister().Deployments(namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("listing Deployments from cache: %w", err)
	}
	if len(deployList) != n {
		return fmt.Errorf("expected %d Deployments, found %d", n, len(deployList))
	}

	initialHash := make(map[string]string, n)
	currentHash := make(map[string]string, n)
	tokenUpdated := make(map[string]bool, n)
	rolledOut := make(map[string]bool, n)
	for _, d := range deployList {
		h := d.Spec.Template.Annotations[apiv1.AnnotationTokenHash]
		initialHash[d.Name] = h
		currentHash[d.Name] = h

		// When attaching to an ongoing rotation, some deployments may
		// already have the new token hash (different from currentTokenHash).
		if patchFn == nil && h != currentTokenHash {
			tokenUpdated[d.Name] = true
			if isWatchDeploymentRolledOut(d) {
				rolledOut[d.Name] = true
			}
		}
	}

	podList, err := factory.Core().V1().Pods().Lister().Pods(namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("listing Pods from cache: %w", err)
	}
	podState := make(map[string]string, len(podList))
	for _, p := range podList {
		state := string(p.Status.Phase)
		if isPodReady(p) {
			state = "Ready"
		}
		podState[p.Name] = state
	}

	w := &rotationWatcher{
		currentTokenHash: currentTokenHash,
		initialHash:      initialHash,
		currentHash:      currentHash,
		podState:         podState,
		lastGWProgrammed: lastGWProgrammed,
		lastGWReady:      lastGWReady,
		lastRotateAt:     lastRotateAt,
		tokenUpdated:     tokenUpdated,
		rolledOut:        rolledOut,
		numDeployments:   n,
		allRolledOut:     len(rolledOut) == n,
	}

	// When attaching to an ongoing rotation, emit synthetic events for
	// deployments that already have the new hash (their token-updated
	// annotation change happened before the informer was set up).
	if patchFn == nil {
		for _, d := range deployList {
			if tokenUpdated[d.Name] {
				h := d.Spec.Template.Annotations[apiv1.AnnotationTokenHash]
				w.addEvent(fmt.Sprintf("Deployment/%s", d.Name), "token-updated",
					fmt.Sprintf("hash=%s", h))
				if rolledOut[d.Name] {
					w.addEvent(fmt.Sprintf("Deployment/%s", d.Name), "rolled-out",
						fmt.Sprintf("updated=%d ready=%d",
							d.Status.UpdatedReplicas, d.Status.ReadyReplicas))
				}
			}
		}
	}

	// If already all rolled out (attaching to a completed rotation), check immediately.
	if w.allRolledOut && w.lastGWReady == "True" {
		return printReport(w.events, initialHash, n)
	}

	// Process events from informers.
	for {
		select {
		case <-ctx.Done():
			_ = printReport(w.events, initialHash, n)
			return fmt.Errorf("timed out waiting for rotation to complete (%d/%d rolled out)", len(w.rolledOut), n)

		case ev := <-eventCh:
			result := w.processEvent(ev)
			if result.suspended {
				_ = printReport(w.events, initialHash, n)
				return &watchSuspendedError{
					pending: n - len(w.rolledOut),
					total:   n,
				}
			}
			if result.done {
				// Drain any remaining events that arrived while we were processing
				// (e.g. old pod termination for the last Deployment).
				drainTimer := time.NewTimer(2 * time.Second)
			drain:
				for {
					select {
					case ev := <-eventCh:
						w.processEvent(ev)
					case <-drainTimer.C:
						break drain
					}
				}
				return printReport(w.events, initialHash, n)
			}
		}
	}
}

// watchResult signals completion or suspension from event processing.
type watchResult struct {
	done      bool
	suspended bool // Gateway was suspended with pending deployments
}

// rotationWatcher holds mutable state for the watch loop.
type rotationWatcher struct {
	currentTokenHash string
	initialHash      map[string]string
	currentHash      map[string]string
	podState         map[string]string
	lastGWProgrammed string
	lastGWReady      string
	lastGWSuspended  bool
	lastRotateAt     string
	tokenUpdated     map[string]bool
	rolledOut        map[string]bool
	numDeployments   int
	events           []rotationEvent
	allRolledOut     bool
}

func (w *rotationWatcher) addEvent(source, kind, detail string) {
	now := time.Now()
	w.events = append(w.events, rotationEvent{Time: now, Source: source, Kind: kind, Detail: detail})

	// Print the event in real time with truncated hashes for readability.
	elapsed := ""
	if len(w.events) > 1 {
		elapsed = fmt.Sprintf("+%s", formatDuration(now.Sub(w.events[0].Time)))
	}
	displayDetail := detail
	if strings.HasPrefix(detail, "hash=") {
		displayDetail = "hash=" + truncateHash(detail[len("hash="):])
	}
	if displayDetail != "" {
		fmt.Printf("[%s] %-8s %-42s %s %s\n", now.Format("15:04:05"), elapsed, source, kind, displayDetail)
	} else {
		fmt.Printf("[%s] %-8s %-42s %s\n", now.Format("15:04:05"), elapsed, source, kind)
	}
}

func (w *rotationWatcher) processEvent(ev informerEvent) watchResult {
	switch ev.resourceKind {
	case "Deployment":
		return w.handleDeployUpdate(ev.obj.(*appsv1.Deployment))
	case "Pod":
		pod, ok := ev.obj.(*corev1.Pod)
		if !ok {
			return watchResult{}
		}
		switch ev.changeType {
		case "add":
			w.handlePodAdd(pod)
		case "update":
			w.handlePodUpdate(pod)
		case "delete":
			w.handlePodDelete(pod)
		}
	case "Gateway":
		obj, ok := ev.obj.(*unstructured.Unstructured)
		if !ok {
			return watchResult{}
		}
		return w.handleGatewayUpdate(obj)
	}
	return watchResult{}
}

// handleDeployUpdate processes a Deployment update.
func (w *rotationWatcher) handleDeployUpdate(deploy *appsv1.Deployment) watchResult {
	source := fmt.Sprintf("Deployment/%s", deploy.Name)
	newHash := deploy.Spec.Template.Annotations[apiv1.AnnotationTokenHash]

	if newHash != w.currentHash[deploy.Name] && newHash != w.currentTokenHash {
		w.currentHash[deploy.Name] = newHash
		if !w.tokenUpdated[deploy.Name] {
			w.tokenUpdated[deploy.Name] = true
			w.addEvent(source, "token-updated", fmt.Sprintf("hash=%s", newHash))
		}
	}

	if w.tokenUpdated[deploy.Name] && !w.rolledOut[deploy.Name] && isWatchDeploymentRolledOut(deploy) {
		w.rolledOut[deploy.Name] = true
		w.addEvent(source, "rolled-out",
			fmt.Sprintf("updated=%d ready=%d",
				deploy.Status.UpdatedReplicas, deploy.Status.ReadyReplicas))
		if len(w.rolledOut) == w.numDeployments {
			w.allRolledOut = true
		}
	}

	// Only finish when all deployments rolled out AND Gateway is Ready.
	return watchResult{done: w.allRolledOut && w.lastGWReady == "True"}
}

func (w *rotationWatcher) handlePodAdd(pod *corev1.Pod) {
	source := fmt.Sprintf("Pod/%s", pod.Name)
	w.addEvent(source, "created", fmt.Sprintf("phase=%s", pod.Status.Phase))
	w.podState[pod.Name] = string(pod.Status.Phase)
}

func (w *rotationWatcher) handlePodUpdate(pod *corev1.Pod) {
	source := fmt.Sprintf("Pod/%s", pod.Name)
	newState := string(pod.Status.Phase)
	if isPodReady(pod) {
		newState = "Ready"
	}
	oldState := w.podState[pod.Name]
	if newState != oldState {
		w.addEvent(source, "state-changed", fmt.Sprintf("%s -> %s", oldState, newState))
		w.podState[pod.Name] = newState
	}
}

func (w *rotationWatcher) handlePodDelete(pod *corev1.Pod) {
	source := fmt.Sprintf("Pod/%s", pod.Name)
	w.addEvent(source, "deleted", "")
	delete(w.podState, pod.Name)
}

func (w *rotationWatcher) handleGatewayUpdate(obj *unstructured.Unstructured) watchResult {
	source := fmt.Sprintf("Gateway/%s", obj.GetName())

	rotateAt := getUnstructuredAnnotation(obj, apiv1.AnnotationRotateTokenRequestedAt)
	if rotateAt != w.lastRotateAt {
		w.addEvent(source, "rotation-requested", "")
		w.lastRotateAt = rotateAt
	}

	programmed := getUnstructuredCondition(obj, "Programmed")
	ready := getUnstructuredCondition(obj, "Ready")
	if programmed != w.lastGWProgrammed || ready != w.lastGWReady {
		w.addEvent(source, "conditions-changed",
			fmt.Sprintf("Programmed=%s Ready=%s", programmed, ready))
		w.lastGWProgrammed = programmed
		w.lastGWReady = ready
	}

	// Detect suspension: if the reconcile annotation changed to disabled
	// while there are still pending deployments, signal suspension.
	suspended := getUnstructuredAnnotation(obj, apiv1.AnnotationReconcile) == apiv1.AnnotationReconcileDisabled
	if suspended != w.lastGWSuspended {
		w.lastGWSuspended = suspended
		if suspended && !w.allRolledOut {
			w.addEvent(source, "suspended", "")
			return watchResult{suspended: true}
		}
	}

	return watchResult{done: w.allRolledOut && w.lastGWReady == "True"}
}

// getUnstructuredAnnotation extracts an annotation value from an unstructured object.
func getUnstructuredAnnotation(obj *unstructured.Unstructured, key string) string {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return ""
	}
	return annotations[key]
}

// getUnstructuredCondition extracts a condition status from an unstructured object.
func getUnstructuredCondition(obj *unstructured.Unstructured, condType string) string {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return "Unknown"
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t == condType {
			if s, _ := cond["status"].(string); s != "" {
				return s
			}
		}
	}
	return "Unknown"
}

// isWatchDeploymentRolledOut checks that the Deployment has completed its rollout.
func isWatchDeploymentRolledOut(deploy *appsv1.Deployment) bool {
	if deploy.Status.ObservedGeneration < deploy.Generation {
		return false
	}
	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}
	return deploy.Status.UpdatedReplicas >= desired &&
		deploy.Status.Replicas <= deploy.Status.UpdatedReplicas &&
		deploy.Status.ReadyReplicas >= deploy.Status.UpdatedReplicas
}

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// printReport prints a comprehensive timeline report and validates the
// rolling update order. Returns an error if validation fails.
func printReport(events []rotationEvent, initialHash map[string]string, numDeployments int) error {
	// Determine the new hash from events.
	var newHash string
	for _, ev := range events {
		if ev.Kind == "token-updated" {
			newHash = ev.Detail[len("hash="):]
			break
		}
	}

	// Collect unique old hashes (hashes that differ from the new one).
	// When attaching to an ongoing rotation triggered mid-rotation,
	// deployments may have different old hashes.
	oldHashes := make(map[string]struct{})
	for _, h := range initialHash {
		if h != newHash {
			oldHashes[h] = struct{}{}
		}
	}
	var oldHashDisplay string
	switch len(oldHashes) {
	case 0:
		oldHashDisplay = truncateHash(newHash)
	case 1:
		for h := range oldHashes {
			oldHashDisplay = truncateHash(h)
		}
	default:
		parts := make([]string, 0, len(oldHashes))
		for h := range oldHashes {
			parts = append(parts, truncateHash(h))
		}
		sort.Strings(parts)
		oldHashDisplay = strings.Join(parts, ", ")
	}

	// Print header.
	fmt.Println()
	fmt.Println("Token Rotation — Rolling Update Report")
	fmt.Println("──────────────────────────────────────")
	fmt.Printf("  Deployments:  %d\n", numDeployments)
	fmt.Printf("  Old hash:     %s\n", oldHashDisplay)
	fmt.Printf("  New hash:     %s\n", truncateHash(newHash))

	// Build per-deployment rolling update timeline.
	type deployTimeline struct {
		name         string
		tokenUpdated time.Time
		rolledOut    time.Time
		podEvents    []rotationEvent
	}
	m := make(map[string]*deployTimeline)
	getDT := func(name string) *deployTimeline {
		dt, ok := m[name]
		if !ok {
			dt = &deployTimeline{name: name}
			m[name] = dt
		}
		return dt
	}
	for _, ev := range events {
		if ev.Kind == "token-updated" {
			name := ev.Source[len("Deployment/"):]
			dt := getDT(name)
			dt.tokenUpdated = ev.Time
			if newHash != "" {
				hash := ev.Detail[len("hash="):]
				if hash != newHash {
					return fmt.Errorf("inconsistent new tokenHash: %s vs %s", truncateHash(newHash), truncateHash(hash))
				}
			}
		}
		if ev.Kind == "rolled-out" {
			name := ev.Source[len("Deployment/"):]
			getDT(name).rolledOut = ev.Time
		}
		// Associate pod events with their Deployment by naming convention.
		if strings.HasPrefix(ev.Source, "Pod/") {
			podName := ev.Source[len("Pod/"):]
			for dName := range initialHash {
				if strings.HasPrefix(podName, dName+"-") {
					getDT(dName).podEvents = append(getDT(dName).podEvents, ev)
					break
				}
			}
		}
	}

	if len(m) != numDeployments {
		return fmt.Errorf("expected %d Deployments with rotation events, got %d", numDeployments, len(m))
	}

	// Sort by token-updated time.
	timelines := make([]*deployTimeline, 0, len(m))
	for _, dt := range m {
		timelines = append(timelines, dt)
	}
	sort.Slice(timelines, func(i, j int) bool {
		return timelines[i].tokenUpdated.Before(timelines[j].tokenUpdated)
	})

	// Print per-deployment rolling update details.
	fmt.Println()
	fmt.Println("Per-Deployment Rollout Details")
	fmt.Println("──────────────────────────────")

	totalStart := timelines[0].tokenUpdated
	var validationErr error
	for i, dt := range timelines {
		if dt.tokenUpdated.IsZero() {
			return fmt.Errorf("deployment %s: missing token-updated event", dt.name)
		}
		if dt.rolledOut.IsZero() {
			return fmt.Errorf("deployment %s: missing rolled-out event", dt.name)
		}
		rolloutDuration := dt.rolledOut.Sub(dt.tokenUpdated)

		fmt.Println()
		fmt.Printf("  %d. %s\n", i+1, dt.name)
		fmt.Printf("     Token updated:   %s  (+%s)\n",
			dt.tokenUpdated.Format("15:04:05.000"), formatDuration(dt.tokenUpdated.Sub(totalStart)))
		fmt.Printf("     Rollout done:    %s  (+%s)\n",
			dt.rolledOut.Format("15:04:05.000"), formatDuration(dt.rolledOut.Sub(totalStart)))
		fmt.Printf("     Rollout took:    %s\n", formatDuration(rolloutDuration))

		if i > 0 {
			prev := timelines[i-1]
			waitTime := dt.tokenUpdated.Sub(prev.rolledOut)
			fmt.Printf("     Waited after #%d: %s\n", i, formatDuration(waitTime))
			if dt.tokenUpdated.Before(prev.rolledOut) {
				validationErr = fmt.Errorf(
					"rolling update order violated: %s token updated at %s, but %s rollout completed at %s",
					dt.name, dt.tokenUpdated.Format("15:04:05.000"),
					prev.name, prev.rolledOut.Format("15:04:05.000"))
			}
		}

		// Print pod events for this Deployment.
		if len(dt.podEvents) > 0 {
			fmt.Println("     Pod events:")
			for _, pev := range dt.podEvents {
				detail := ""
				if pev.Detail != "" {
					detail = " " + pev.Detail
				}
				fmt.Printf("       +%-8s  %s %s%s\n",
					formatDuration(pev.Time.Sub(dt.tokenUpdated)),
					pev.Source[len("Pod/"):],
					pev.Kind, detail)
			}
		}
	}

	totalDuration := timelines[len(timelines)-1].rolledOut.Sub(totalStart)
	fmt.Println()
	fmt.Println("─────────────────────────────────────────────────────────────────")
	fmt.Printf("  Total rolling update time: %s\n", formatDuration(totalDuration))
	fmt.Println("─────────────────────────────────────────────────────────────────")

	if validationErr != nil {
		fmt.Println()
		fmt.Println("  FAILED: " + validationErr.Error())
		return validationErr
	}

	fmt.Println()
	fmt.Println("  PASSED: All Deployments updated one at a time in order.")
	return nil
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func truncateHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
