package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/retry"
)

// execMode returns the executor transport mode from the DEVBOX_EXEC_MODE env var.
// Default: "spdy". Set DEVBOX_EXEC_MODE=websocket to opt into the newer
// WebSocket transport once it is proven reliable in the target cluster.
func execMode() string {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("DEVBOX_EXEC_MODE")))
	if mode == "" {
		return "spdy"
	}
	return mode
}

// newExecForMode creates a remotecommand.Executor using the appropriate transport.
// Default is SPDY because the WebSocket executor can report empty exit-code-only
// failures for otherwise healthy in-cluster exec calls.
func newExecForMode(config *rest.Config, execURL *url.URL) (remotecommand.Executor, error) {
	if execMode() == "websocket" {
		return remotecommand.NewWebSocketExecutor(config, "GET", execURL.String())
	}
	return remotecommand.NewSPDYExecutor(config, "POST", execURL)
}

func (k *K8sBackend) Start(ctx context.Context, opts StartOpts) (*StartResult, error) {
	registryTag := k.registryTag(opts.ImageTag)
	expectedIdentity := expectedStartIdentityLabels(opts)

	// Check if a matching pod already exists and is running — reuse it.
	existing, err := k.clientset.CoreV1().Pods(k.namespace).Get(ctx, opts.Name, metav1.GetOptions{})
	if err == nil {
		existing, err = k.ensurePodStartIdentity(ctx, opts)
		if err != nil {
			return nil, err
		}
	}
	if err == nil && existing.Status.Phase == corev1.PodRunning && existing.DeletionTimestamp == nil {
		// Pod exists and is running — check if image matches.
		if len(existing.Spec.Containers) > 0 && existing.Spec.Containers[0].Image == registryTag {
			return &StartResult{ContainerID: existing.Name}, nil
		}
		// Image mismatch — stop and recreate.
		if err := k.StopIfIdentity(ctx, opts.Name, expectedIdentity); err != nil {
			return nil, fmt.Errorf("replace existing pod: %w", err)
		}
		if err := k.waitForPodGone(ctx, opts.Name, 30*time.Second); err != nil {
			return nil, fmt.Errorf("wait to replace existing pod: %w", err)
		}
	} else if err == nil {
		// Pod exists but is not reusable (including Terminating) — delete it
		// and wait for the API name to become free before Create.
		if err := k.StopIfIdentity(ctx, opts.Name, expectedIdentity); err != nil {
			return nil, fmt.Errorf("replace non-running pod: %w", err)
		}
		if err := k.waitForPodGone(ctx, opts.Name, 30*time.Second); err != nil {
			return nil, fmt.Errorf("wait to replace non-running pod: %w", err)
		}
	} else if !isNotFound(err) {
		return nil, fmt.Errorf("get existing pod: %w", err)
	}
	// If not found, proceed to create.

	pod := k.buildPodSpec(opts, registryTag)

	created, err := k.clientset.CoreV1().Pods(k.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		// A pod may have appeared between the initial GET and Create. Validate
		// its identity before surfacing AlreadyExists to the orchestrator's
		// stable-handle reattach path.
		if apierrors.IsAlreadyExists(err) {
			if _, ensureErr := k.ensurePodStartIdentity(ctx, opts); ensureErr != nil {
				return nil, ensureErr
			}
		}
		return nil, fmt.Errorf("create pod: %w", err)
	}

	// Wait for pod to be Running; cleanup dangling pod on failure
	if err := k.waitForPodRunning(ctx, opts.Name, 120*time.Second); err != nil {
		// Capture the git-clone init container's real `fatal: …` line BEFORE
		// StopIfIdentity deletes the pod — otherwise a repo-not-found / bad-ref
		// / auth clone failure surfaces only as the opaque "container git-clone
		// terminated exit_code=128 reason=Error" with the git message gone.
		detail := k.failedInitContainerDetail(ctx, opts.Name)
		_ = k.StopIfIdentity(context.Background(), opts.Name, expectedIdentity)
		return nil, fmt.Errorf("pod not ready: %w%s", err, detail)
	}

	return &StartResult{ContainerID: created.Name}, nil
}

// ProbeStartIdentity validates an existing deterministic pod without mutating
// it. A missing pod is not an error and returns exists=false.
func (k *K8sBackend) ProbeStartIdentity(ctx context.Context, opts StartOpts) (bool, error) {
	pod, err := k.clientset.CoreV1().Pods(k.namespace).Get(ctx, opts.Name, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("probe pod %s identity: %w", opts.Name, err)
	}
	if _, err := validateIdentityLabels(
		"pod "+pod.Name,
		pod.Labels,
		expectedStartIdentityLabels(opts),
		opts.AllowMissingIdentityLabels,
	); err != nil {
		return true, err
	}
	if pod.DeletionTimestamp != nil {
		return true, errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("pod %s is terminating", pod.Name),
		)
	}
	if pod.Status.Phase != corev1.PodRunning {
		return true, errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("pod %s phase %s is not attachable", pod.Name, pod.Status.Phase),
		)
	}
	if opts.ImageTag != "" {
		wantImage := k.registryTag(opts.ImageTag)
		if len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].Image != wantImage {
			return true, errors.Join(
				ErrRuntimeIdentityConflict,
				fmt.Errorf("pod %s image does not match %s", pod.Name, wantImage),
			)
		}
	}
	return true, nil
}

func (k *K8sBackend) ensurePodStartIdentity(ctx context.Context, opts StartOpts) (*corev1.Pod, error) {
	var result *corev1.Pod
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pod, err := k.clientset.CoreV1().Pods(k.namespace).Get(ctx, opts.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		missing, err := validateIdentityLabels(
			"pod "+pod.Name,
			pod.Labels,
			expectedStartIdentityLabels(opts),
			opts.AllowMissingIdentityLabels,
		)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			result = pod
			return nil
		}
		next := pod.DeepCopy()
		if next.Labels == nil {
			next.Labels = make(map[string]string)
		}
		for key, value := range missing {
			next.Labels[key] = value
		}
		result, err = k.clientset.CoreV1().Pods(k.namespace).Update(ctx, next, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("ensure pod %s identity: %w", opts.Name, err)
	}
	return result, nil
}

// StopIfIdentity deletes a pod only after validating its current identity.
// The UID precondition prevents a same-name replacement from winning the
// check/delete race.
func (k *K8sBackend) StopIfIdentity(ctx context.Context, id string, expectedLabels map[string]string) error {
	pod, err := k.clientset.CoreV1().Pods(k.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("get pod before conditional delete: %w", err)
	}
	if _, err := validateIdentityLabels("pod "+pod.Name, pod.Labels, expectedLabels, false); err != nil {
		return err
	}
	gracePeriod := int64(5)
	deleteOpts := metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod}
	if pod.UID != "" {
		uid := pod.UID
		deleteOpts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	if err := k.clientset.CoreV1().Pods(k.namespace).Delete(ctx, id, deleteOpts); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete pod with identity precondition: %w", err)
	}
	return nil
}

func (k *K8sBackend) Exec(_ context.Context, opts ExecOpts) (*ExecResult, error) {
	// Detach from the MCP request context so proxy timeouts don't kill
	// long-running test suites or builds. Use the exec's own timeout.
	timeout := 5 * time.Minute
	if opts.TimeoutSec > 0 {
		timeout = time.Duration(opts.TimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()

	// Build the command with workdir and env vars prepended
	shellCmd := opts.Command
	if len(opts.Env) > 0 {
		var envPrefix strings.Builder
		for k, v := range opts.Env {
			envPrefix.WriteString(fmt.Sprintf("export %s=%q; ", k, v))
		}
		shellCmd = envPrefix.String() + shellCmd
	}
	if opts.WorkDir != "" {
		shellCmd = fmt.Sprintf("cd %q && %s", opts.WorkDir, shellCmd)
	}

	// NFS cache flush: force the kernel to re-validate file attributes so
	// `make` sees correct mtimes after local edits synced via rsync.
	// This is lightweight (~1ms) and prepended to every exec.
	if k.nfsFlush && opts.WorkDir != "" {
		flushCmd := fmt.Sprintf("stat -f %q >/dev/null 2>&1; ", opts.WorkDir)
		shellCmd = flushCmd + shellCmd
	}

	req := k.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(opts.ContainerID).
		Namespace(k.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "devbox",
			Command:   []string{"sh", "-c", shellCmd},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := newExecForMode(k.restConfig, req.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	streamErr := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdoutBuf,
		Stderr: &stderrBuf,
	})

	durationMs := time.Since(start).Milliseconds()

	exitCode := 0
	oomKilled := false
	if streamErr != nil {
		if ctx.Err() != nil {
			return &ExecResult{
				ExitCode:   124,
				StdoutTail: "command timed out",
				DurationMs: durationMs,
			}, nil
		}
		// Extract exit code from error message if possible. parseExitCode
		// returns 1 as a default when it can't find an "exit code N" in
		// the error — the cases that produce that fallback (pod gone,
		// container terminating, exec channel rejected, etc.) are exactly
		// the ones an operator needs to see, so preserve the original
		// streamErr text in stderrBuf when neither buffer captured
		// anything from the command itself.
		exitCode = parseExitCode(streamErr)
		surfaceExecStreamError(&stdoutBuf, &stderrBuf, streamErr)

		// A runc "could not start the process" failure is what a memory kill
		// looks like from here: the OOM killer reaps runc init, the sandbox's
		// PID 1 survives, so nothing in the error or the container status
		// mentions memory. Say what it almost certainly is, and name the two
		// settings that fix it — otherwise the caller is left staring at
		// "procReady not received".
		if isRuncInitFailure(streamErr) {
			pod, podErr := k.clientset.CoreV1().Pods(k.namespace).Get(ctx, opts.ContainerID, metav1.GetOptions{})
			if podErr != nil {
				pod = nil
			}
			// Only the kubelet can confirm an OOM kill. Without that, the
			// hint hedges rather than asserting a cause it can't prove.
			if containerOOMKilled(pod) != "" {
				exitCode = 137
				oomKilled = true
			}
			stderrBuf.WriteString(execMemoryHint(containerMemoryLimit(pod, "devbox"), oomKilled))
			stderrBuf.WriteByte('\n')
		}
	}

	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = 20
	}

	stdoutTail, stdoutTotal, stdoutTrunc := TruncateOutput(stdoutBuf.String(), maxLines)
	stderrTail, stderrTotal, stderrTrunc := TruncateOutput(stderrBuf.String(), maxLines)

	return &ExecResult{
		ExitCode:    exitCode,
		StdoutLines: stdoutTotal,
		StderrLines: stderrTotal,
		StdoutTail:  stdoutTail,
		StderrTail:  stderrTail,
		DurationMs:  durationMs,
		Truncated:   stdoutTrunc || stderrTrunc,
		OOMKilled:   oomKilled || exitCode == 137,
	}, nil
}

func (k *K8sBackend) Stop(ctx context.Context, id string) error {
	gracePeriod := int64(5)
	err := k.clientset.CoreV1().Pods(k.namespace).Delete(ctx, id, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete pod: %w", err)
	}
	return nil
}

func (k *K8sBackend) Status(ctx context.Context, id string) (*StatusResult, error) {
	pod, err := k.clientset.CoreV1().Pods(k.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return &StatusResult{Running: false, Status: "not_found"}, nil
		}
		return nil, fmt.Errorf("get pod: %w", err)
	}

	status := strings.ToLower(string(pod.Status.Phase))
	return &StatusResult{
		Running: pod.Status.Phase == corev1.PodRunning,
		Status:  status,
	}, nil
}

func (k *K8sBackend) Pause(_ context.Context, _ string) error {
	return ErrNotSupported
}

func (k *K8sBackend) Resume(_ context.Context, _ string) error {
	return ErrNotSupported
}

func (k *K8sBackend) ReadFile(ctx context.Context, id, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req := k.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(id).
		Namespace(k.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "devbox",
			Command:   []string{"cat", path},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := newExecForMode(k.restConfig, req.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return nil, fmt.Errorf("read file %q: %w (%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (k *K8sBackend) WriteFile(ctx context.Context, id, path string, content []byte, mode string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if mode == "" {
		mode = "0644"
	}
	shellCmd := fmt.Sprintf("mkdir -p \"$(dirname %q)\" && cat > %q && chmod %s %q", path, path, mode, path)
	req := k.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(id).
		Namespace(k.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "devbox",
			Command:   []string{"sh", "-c", shellCmd},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := newExecForMode(k.restConfig, req.URL())
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	var stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  bytes.NewReader(content),
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("write file %q: %w (%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
