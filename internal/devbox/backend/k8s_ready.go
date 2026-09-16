package backend

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/crb2nu/loom/pkg/env"
)

const (
	spawnAttachWait          = 5 * time.Minute
	spawnImageWait           = 2 * time.Minute
	spawnStartWait           = 2 * time.Minute
	spawnPollInterval        = 2 * time.Second
	defaultSpawnStartTimeout = 5 * time.Minute
)

type podReadyPhase string

const (
	podReadyAttach podReadyPhase = "attach"
	podReadyImage  podReadyPhase = "image"
	// podReadyInit is the window in which an init container is doing real
	// work — for git-clone pods, cloning the repository. It is bounded only
	// by the overall start timeout: a clone that takes three minutes under
	// load is healthy, not stuck, and was exactly the failure the fixed
	// 2m0s wait produced (2026-09-05, 8 consecutive plan_slice attempts on
	// bl-custom-server-shared-child, pod Pending in PodInitializing).
	podReadyInit  podReadyPhase = "init"
	podReadyStart podReadyPhase = "start"
)

type podReadyBudgets struct {
	attach, image, init, start time.Duration
}

type podReadyTracker struct {
	phase       podReadyPhase
	phaseStart  time.Time
	lastEvent   string
	startedMain map[string]bool
}

func newPodReadyTracker(now time.Time) *podReadyTracker {
	return &podReadyTracker{phase: podReadyAttach, phaseStart: now, startedMain: make(map[string]bool)}
}

func (t *podReadyTracker) observe(pod *corev1.Pod, events []corev1.Event, now time.Time, budgets podReadyBudgets) (bool, error) {
	for _, event := range events {
		if event.Reason != "" {
			t.lastEvent = event.Reason
		}
	}

	if pod.Status.Phase == corev1.PodRunning {
		return true, nil
	}
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return true, fmt.Errorf("container_crash (last event %s): pod entered terminal phase: %s", t.eventReason(), podFailureReason(pod))
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
			return true, fmt.Errorf("unschedulable (last event %s): %s", t.eventReason(), condition.Message)
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if term := cs.State.Terminated; term != nil && term.ExitCode != 0 {
			return true, podEarlyContainerError(pod)
		}
		if err := terminalImagePullError(cs, t.eventReason()); err != nil {
			return true, err
		}
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running != nil {
			t.startedMain[cs.Name] = true
		}
		if term := cs.State.Terminated; term != nil && (t.startedMain[cs.Name] || !term.StartedAt.IsZero()) {
			return true, fmt.Errorf("container_crash (last event %s): container %s terminated exit_code=%d reason=%s", t.eventReason(), cs.Name, term.ExitCode, term.Reason)
		}
		if err := terminalImagePullError(cs, t.eventReason()); err != nil {
			return true, err
		}
	}

	// Image activity proves attachment is no longer the only gating operation.
	// FailedAttachVolume (including CSI Aborted while a prior RWO user detaches)
	// deliberately remains in the five-minute attach phase until then.
	next := podReadyAttach
	switch {
	case initInProgress(pod):
		// An init container is running (git-clone hydrating the workspace).
		// allImagesAvailable is typically true here too, but the main
		// container cannot start until init finishes, so the start budget
		// must not tick yet.
		next = podReadyInit
	case allImagesAvailable(pod):
		next = podReadyStart
	case hasEvent(events, "Pulled", "Pulling", "SuccessfulAttachVolume") ||
		(containersReported(pod) && !hasEvent(events, "FailedAttachVolume", "FailedMount")):
		next = podReadyImage
	}
	if phaseRank(next) > phaseRank(t.phase) {
		t.phase, t.phaseStart = next, now
	}

	budget, reason := t.phaseBudget(budgets)
	if budget > 0 && now.Sub(t.phaseStart) >= budget {
		return true, fmt.Errorf("%s (last event %s): timed out after %s waiting for pod %s phase", reason, t.eventReason(), budget, t.phase)
	}
	return false, nil
}

func terminalImagePullError(status corev1.ContainerStatus, eventReason string) error {
	waiting := status.State.Waiting
	if waiting == nil || (waiting.Reason != "ErrImagePull" && waiting.Reason != "ImagePullBackOff") {
		return nil
	}
	return fmt.Errorf("image_pull_backoff (last event %s): image pull error in %s: %s — %s", eventReason, status.Name, waiting.Reason, waiting.Message)
}

// phaseBudget returns the wait budget and the stable failure reason for the
// tracker's current phase. A zero budget means the phase is bounded only by
// the caller's overall timeout.
func (t *podReadyTracker) phaseBudget(budgets podReadyBudgets) (time.Duration, string) {
	switch t.phase {
	case podReadyImage:
		return budgets.image, "image_pull_backoff"
	case podReadyInit:
		return budgets.init, "init_wait_exceeded"
	case podReadyStart:
		return budgets.start, "container_crash"
	default:
		return budgets.attach, "attach_wait_exceeded"
	}
}

func (t *podReadyTracker) timeoutError(timeout time.Duration) error {
	_, reason := t.phaseBudget(podReadyBudgets{})
	return fmt.Errorf("%s (last event %s): timed out after %s waiting in %s phase", reason, t.eventReason(), timeout, t.phase)
}

func phaseRank(phase podReadyPhase) int {
	switch phase {
	case podReadyImage:
		return 1
	case podReadyInit:
		return 2
	case podReadyStart:
		return 3
	default:
		return 0
	}
}

// initInProgress reports whether an init container is currently running —
// the pod has attached its volumes and pulled the init image, and is now
// doing the init work itself (for git-clone pods, the clone).
func initInProgress(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Running != nil {
			return true
		}
	}
	return false
}

func (t *podReadyTracker) eventReason() string {
	if t.lastEvent == "" {
		return "unknown"
	}
	return t.lastEvent
}

func containersReported(pod *corev1.Pod) bool {
	return len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses) > 0
}

func allImagesAvailable(pod *corev1.Pod) bool {
	if len(pod.Spec.InitContainers)+len(pod.Spec.Containers) == 0 {
		return false
	}
	available := make(map[string]bool)
	for _, cs := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		available[cs.Name] = cs.ImageID != "" || cs.State.Running != nil || cs.State.Terminated != nil
	}
	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		if !available[container.Name] {
			return false
		}
	}
	return true
}

func hasEvent(events []corev1.Event, reasons ...string) bool {
	for _, event := range events {
		for _, reason := range reasons {
			if strings.EqualFold(event.Reason, reason) {
				return true
			}
		}
	}
	return false
}

// waitForSpawnPodRunning uses independent budgets so a slow RWO detach cannot
// consume the image-pull and container-start windows. Events enrich errors but
// are optional: clusters that deny event reads still get status-based waiting.
func (k *K8sBackend) waitForSpawnPodRunning(ctx context.Context, name string) error {
	timeout := env.Duration("DEVBOX_K8S_START_TIMEOUT", defaultSpawnStartTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tracker := newPodReadyTracker(time.Now())
	// init has no budget of its own: a running init container (the clone)
	// is bounded only by the overall timeout.
	budgets := podReadyBudgets{attach: spawnAttachWait, image: spawnImageWait, init: 0, start: spawnStartWait}
	ticker := time.NewTicker(spawnPollInterval)
	defer ticker.Stop()

	seen := false
	for {
		pod, err := k.clientset.CoreV1().Pods(k.namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			seen = true
			events, listErr := k.clientset.CoreV1().Events(k.namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + name})
			var items []corev1.Event
			if listErr == nil {
				items = events.Items
			}
			done, waitErr := tracker.observe(pod, items, time.Now(), budgets)
			if done || waitErr != nil {
				return waitErr
			}
		} else if isNotFound(err) {
			if seen {
				return fmt.Errorf("pod %s was deleted before reaching Running", name)
			}
		} else {
			return fmt.Errorf("get pod while waiting for readiness: %w", err)
		}

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return tracker.timeoutError(timeout)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
