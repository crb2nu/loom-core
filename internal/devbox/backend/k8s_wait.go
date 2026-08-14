package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// waitForPodRunning watches until the pod reaches Running phase or timeout.
// Uses the Watch API for sub-second latency instead of polling.
func (k *K8sBackend) waitForPodRunning(ctx context.Context, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if pod, err := k.clientset.CoreV1().Pods(k.namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		done, waitErr := podRunningState(pod)
		if done || waitErr != nil {
			return waitErr
		}
	} else if !isNotFound(err) {
		return fmt.Errorf("get pod before watch: %w", err)
	}

	watcher, err := k.clientset.CoreV1().Pods(k.namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	})
	if err != nil {
		return fmt.Errorf("watch pod: %w", err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		if event.Type == watch.Deleted {
			return fmt.Errorf("pod %s was deleted before reaching Running", name)
		}
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}
		done, waitErr := podRunningState(pod)
		if done || waitErr != nil {
			return waitErr
		}
	}
	return fmt.Errorf("watch closed for pod %s", name)
}

// podFailureReason extracts a diagnostic string from a failed pod's container statuses.
func podFailureReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			parts := []string{fmt.Sprintf("exit_code=%d", t.ExitCode)}
			if t.Reason != "" {
				parts = append(parts, "reason="+t.Reason)
			}
			if t.Message != "" {
				parts = append(parts, "message="+t.Message)
			}
			return strings.Join(parts, " ")
		}
	}
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	return string(pod.Status.Phase)
}

// waitForPodDone watches until the pod reaches Succeeded or Failed, or timeout.
// Uses the Watch API for sub-second latency instead of polling.
// Returns early on image pull errors to avoid waiting the full timeout.
func (k *K8sBackend) waitForPodDone(ctx context.Context, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if pod, err := k.clientset.CoreV1().Pods(k.namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		done, waitErr := podDoneState(pod)
		if done || waitErr != nil {
			return waitErr
		}
	} else if !isNotFound(err) {
		return fmt.Errorf("get pod before watch: %w", err)
	}

	watcher, err := k.clientset.CoreV1().Pods(k.namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	})
	if err != nil {
		return fmt.Errorf("watch pod: %w", err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		if event.Type == watch.Deleted {
			return fmt.Errorf("pod %s was deleted before completion", name)
		}
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}
		done, waitErr := podDoneState(pod)
		if done || waitErr != nil {
			return waitErr
		}
	}
	return fmt.Errorf("watch closed for pod %s", name)
}

func podRunningState(pod *corev1.Pod) (bool, error) {
	switch pod.Status.Phase {
	case corev1.PodRunning:
		return true, nil
	case corev1.PodFailed, corev1.PodSucceeded:
		return true, fmt.Errorf("pod entered terminal phase: %s", podFailureReason(pod))
	}
	if err := podEarlyContainerError(pod); err != nil {
		return true, err
	}
	return false, nil
}

func podDoneState(pod *corev1.Pod) (bool, error) {
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return true, nil
	case corev1.PodFailed:
		return true, fmt.Errorf("build pod failed: %s", podFailureReason(pod))
	}
	if err := podEarlyContainerError(pod); err != nil {
		return true, err
	}
	return false, nil
}

func podEarlyContainerError(pod *corev1.Pod) error {
	for _, cs := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		if w := cs.State.Waiting; w != nil {
			if w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" {
				return fmt.Errorf("image pull error in %s: %s — %s", cs.Name, w.Reason, w.Message)
			}
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			parts := []string{fmt.Sprintf("container %s terminated exit_code=%d", cs.Name, t.ExitCode)}
			if t.Reason != "" {
				parts = append(parts, "reason="+t.Reason)
			}
			if t.Message != "" {
				parts = append(parts, "message="+t.Message)
			}
			return fmt.Errorf("%s", strings.Join(parts, " "))
		}
	}
	return nil
}

// getPodLogs reads the last 100 lines from the buildah container.
func (k *K8sBackend) getPodLogs(ctx context.Context, podName string) (string, error) {
	return k.getContainerLogs(ctx, podName, "buildah")
}

// getContainerLogs reads the last 100 lines from a named container of a pod.
// Generalises getPodLogs so a FAILED init container (e.g. git-clone) can be
// read directly — its logs, not the buildah container's, hold the real error.
func (k *K8sBackend) getContainerLogs(ctx context.Context, podName, container string) (string, error) {
	tailLines := int64(100)
	req := k.clientset.CoreV1().Pods(k.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tailLines,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("get logs: %w", err)
	}
	defer stream.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, stream); err != nil {
		return "", fmt.Errorf("read logs: %w", err)
	}
	return buf.String(), nil
}

// cloneLogTailLines caps how many trailing non-empty log lines the failure
// detail carries. git's fatal output is 1-3 lines; a small tail keeps the
// escalation reason readable and bounds the size of the string that reaches a
// GitLab issue body.
const cloneLogTailLines = 6

// gitCredentialsRE matches the `user:pass@` / `token:xxx@` userinfo of a clone
// URL so the spawn git token — which git echoes verbatim in
// `fatal: repository 'https://token:…@host/…' not found` — never leaks into a
// spawn error string or the escalation issue it becomes.
var gitCredentialsRE = regexp.MustCompile(`(?i)(https?://)[^/@\s'"]*@`)

// failedInitContainerName returns the name of the first INIT container that
// terminated non-zero, or "" if none did. The git-clone container runs as an
// init container in both the buildah build pod and the spawn runtime pod, so a
// repo-not-found / bad-ref / auth clone failure exits it non-zero before the
// main container ever starts.
func failedInitContainerName(pod *corev1.Pod) string {
	for _, cs := range pod.Status.InitContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return cs.Name
		}
	}
	return ""
}

// failedInitContainerDetail returns a credential-redacted, single-line tail of
// the logs from a failed init container, formatted for appending to a spawn or
// build error (" — git-clone log: …"). k8s reports a failed git-clone init
// container only as `exit_code=128 reason=Error` with an empty terminated
// message (no terminationMessagePolicy is set), so the actionable `fatal: …`
// git line lives ONLY in this container's logs — and the buildah container's
// logs (what getPodLogs reads) are empty because buildah never ran. Returns ""
// when no init container failed, the pod is gone, or the logs are unavailable,
// leaving the caller's existing behaviour intact.
func (k *K8sBackend) failedInitContainerDetail(ctx context.Context, podName string) string {
	pod, err := k.clientset.CoreV1().Pods(k.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	name := failedInitContainerName(pod)
	if name == "" {
		return ""
	}
	logs, err := k.getContainerLogs(ctx, podName, name)
	if err != nil {
		return ""
	}
	tail := gitCredentialsRE.ReplaceAllString(lastNonEmptyLines(logs, cloneLogTailLines), "$1***@")
	if tail == "" {
		return ""
	}
	return fmt.Sprintf(" — %s log: %s", name, tail)
}

// lastNonEmptyLines joins the last n non-empty, trimmed lines of s with " | "
// so a multi-line git-clone failure collapses into one readable error segment.
func lastNonEmptyLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	raw := strings.Split(s, "\n")
	lines := make([]string, 0, len(raw))
	for _, ln := range raw {
		if t := strings.TrimSpace(ln); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// deletePod deletes a pod with zero grace period (immediate). The
// Delete API returns as soon as the request is accepted — the pod
// may still be in "Terminating" state for some seconds afterward,
// which is why a subsequent Create with the same name can fail with
// 409 AlreadyExists. Use evictPod when the caller intends to recreate
// under the same name.
func (k *K8sBackend) deletePod(ctx context.Context, name string) error {
	gracePeriod := int64(0)
	err := k.clientset.CoreV1().Pods(k.namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete pod: %w", err)
	}
	return nil
}

// waitForPodGone polls until the named pod is no longer present, or
// returns an error on timeout / ctx cancellation. Caller uses this
// before recreating under the same name to avoid the 409 AlreadyExists
// race that produced ~20% of buildah build failures in the 2026-05-24
// kill-test (.loom/local/handoffs/mills-autonomy-killtest-2026-05-24.md).
//
// Watch isn't ideal for "pod is gone" because the watcher closes when
// the pod is deleted, so we poll Get for NotFound. Polling cadence is
// 200ms — fast enough that a typical 1-2s termination window resolves
// in <10 polls.
func (k *K8sBackend) waitForPodGone(ctx context.Context, name string, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		_, err := k.clientset.CoreV1().Pods(k.namespace).Get(deadline, name, metav1.GetOptions{})
		if isNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wait pod gone: get %s: %w", name, err)
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("wait pod gone: %s still present after %s", name, timeout)
		case <-tick.C:
			// continue
		}
	}
}

// evictPod deletes the pod and waits for it to actually disappear from
// the cluster. Cheap when the pod isn't there (Delete returns NotFound,
// waitForPodGone returns immediately on the first poll). Used before a
// recreate under the same name to dodge the "pod already exists" 409.
func (k *K8sBackend) evictPod(ctx context.Context, name string, timeout time.Duration) error {
	if err := k.deletePod(ctx, name); err != nil {
		return err
	}
	return k.waitForPodGone(ctx, name, timeout)
}

// parseExitCode extracts the exit code from a K8s exec error.
// Returns 1 as default for non-zero exits when code can't be parsed.
func parseExitCode(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	// K8s exec errors look like: "command terminated with exit code 2"
	if strings.Contains(msg, "exit code") {
		var code int
		if _, scanErr := fmt.Sscanf(msg[strings.LastIndex(msg, "exit code")+len("exit code "):], "%d", &code); scanErr == nil {
			return code
		}
	}
	return 1
}

// surfaceExecStreamError writes the exec stream error text into stderrBuf
// when neither buffer captured any output from the command itself. This
// turns silent failures (pod gone, container terminating, exec channel
// rejected) into something actionable. Skipped when the buffers already
// have content — the command's own output takes priority.
//
// stdoutBuf may be nil for streaming-exec callers that maintain a tail
// ring buffer outside; pass nil and gate on your own totalLines counter.
func surfaceExecStreamError(stdoutBuf, stderrBuf *bytes.Buffer, streamErr error) {
	if streamErr == nil || stderrBuf == nil {
		return
	}
	if stderrBuf.Len() > 0 {
		return
	}
	if stdoutBuf != nil && stdoutBuf.Len() > 0 {
		return
	}
	stderrBuf.WriteString("exec error: ")
	stderrBuf.WriteString(streamErr.Error())
	stderrBuf.WriteByte('\n')
}

// runcInitFailureMarkers are the substrings a K8s exec stream error carries
// when runc could not get the exec'd process off the ground at all. In a
// devbox sandbox the usual cause is the cgroup OOM killer reaping runc's init
// helper: the sandbox's PID 1 is untouched, so the container is never marked
// OOMKilled and the caller is left with an error that says nothing about
// memory. Recognising the signature lets us say so.
var runcInitFailureMarkers = []string{
	"procReady not received",
	"error during container init",
	"unable to start container process",
}

// isRuncInitFailure reports whether an exec stream error is runc failing to
// start the process, as opposed to the command itself exiting non-zero.
func isRuncInitFailure(err error) bool {
	if err == nil {
		return false
	}
	return hasRuncInitFailureMarker(err.Error())
}

// hasRuncInitFailureMarker is the string form of isRuncInitFailure, for the
// docker backend, where the signature arrives on the container's stderr
// rather than as an error value.
func hasRuncInitFailureMarker(msg string) bool {
	for _, marker := range runcInitFailureMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// containerOOMKilled returns the name of the first container the kubelet
// reports as OOMKilled, in either its current or previous state, or "" when
// none is. The previous state matters: a container that was OOM-killed and
// restarted is Running again by the time an exec fails against it.
func containerOOMKilled(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.Reason == "OOMKilled" {
			return cs.Name
		}
		if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
			return cs.Name
		}
	}
	return ""
}

// containerMemoryLimit returns the named container's memory limit as it was
// requested in the pod spec ("4Gi"), or "" when the container has no limit.
func containerMemoryLimit(pod *corev1.Pod, container string) string {
	if pod == nil {
		return ""
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != container {
			continue
		}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			return q.String()
		}
	}
	return ""
}

// execMemoryHint renders the guidance appended to an exec failure that looks
// like a memory kill. confirmed distinguishes "the kubelet told us this was an
// OOM kill" from "runc's init died and this is the overwhelmingly common
// reason", so the text never claims more than it knows. memLimit is the
// sandbox's configured limit, or "" when it could not be read.
func execMemoryHint(memLimit string, confirmed bool) string {
	limit := memLimit
	if limit == "" {
		limit = "unset"
	}
	lead := fmt.Sprintf("devbox: the sandbox could not start the exec process (container memory limit %s). "+
		"The usual cause is the cgroup OOM killer reaping it — a compile or link step needed more memory "+
		"than the limit allows. A node under memory pressure produces the same error.", limit)
	if confirmed {
		lead = fmt.Sprintf("devbox: the sandbox container was OOM-killed (container memory limit %s).", limit)
	}
	return lead + " Raise it for one project with limits.memory_mb in a .devbox.yaml at the repo root, " +
		"or for every project with DEVBOX_DEFAULT_MEMORY_MB."
}

// isNotFound returns true if the error is a K8s "not found" error.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}
