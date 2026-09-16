package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// StreamExecOpts configures a streaming command execution that delivers stdout
// lines incrementally via a callback.
type StreamExecOpts struct {
	ContainerID string
	Command     string
	WorkDir     string
	Env         map[string]string
	TimeoutSec  int
	OnLine      func(line []byte) // called for each complete stdout line
	OnStderr    func(line []byte) // optional: called for each stderr line
}

// tailRing keeps the last N lines of a stream in a fixed-size ring, so a
// command emitting megabytes of stdout costs a bounded amount of memory while
// its tail — where the failure diagnostic lives — still reaches ExecResult.
//
// Extracted from an inline closure because the index arithmetic was wrong and
// untestable in place: the write index was taken AFTER incrementing the counter,
// so line N landed at N%size instead of (N-1)%size. The first wrap therefore
// evicted line 2 while line 1 survived, and the reader (which assumed the
// correct layout) replayed that stale line 1 in the middle of the tail. On a
// 20-line ring every buffered spawn tail past line 20 carried one line of a
// different, much older part of the run — read as adjacent context by anything
// diagnosing the failure, including the Mills spawn finalizer and the codex
// parser replay that only ever sees this tail.
type tailRing struct {
	buf   []string
	size  int
	total int
}

func newTailRing(size int) *tailRing {
	return &tailRing{buf: make([]string, 0, size), size: size}
}

func (r *tailRing) add(line string) {
	r.total++
	if len(r.buf) < r.size {
		r.buf = append(r.buf, line)
		return
	}
	// Line N lives at (N-1)%size: line 1 at 0 … line size at size-1, and line
	// size+1 wraps back onto index 0, evicting the OLDEST line rather than the
	// second-oldest.
	r.buf[(r.total-1)%r.size] = line
}

// lines returns the retained tail in emission order.
func (r *tailRing) lines() []string {
	if r.total <= r.size {
		return r.buf
	}
	// The oldest retained line is total-size+1, which sits at
	// (total-size)%size == total%size.
	start := r.total % r.size
	ordered := make([]string, r.size)
	for i := 0; i < r.size; i++ {
		ordered[i] = r.buf[(start+i)%r.size]
	}
	return ordered
}

// lineCallbackWriter is an io.Writer that calls a callback for each
// newline-terminated line. Partial lines are buffered until the next newline.
type lineCallbackWriter struct {
	buf    bytes.Buffer
	onLine func(line []byte)
}

// Write implements io.Writer. It scans incoming data for newlines and calls
// onLine for each complete line.
func (w *lineCallbackWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			// No newline found -- buffer the remainder.
			w.buf.Write(p)
			break
		}
		// Write everything up to (but not including) the newline to the buffer,
		// then deliver the complete line.
		w.buf.Write(p[:idx])
		if w.onLine != nil {
			// Deliver a copy so the callback can retain it safely.
			line := make([]byte, w.buf.Len())
			copy(line, w.buf.Bytes())
			w.onLine(line)
		}
		w.buf.Reset()
		p = p[idx+1:]
	}
	return total, nil
}

// Flush delivers any remaining buffered content as a final line.
func (w *lineCallbackWriter) Flush() {
	if w.buf.Len() > 0 && w.onLine != nil {
		line := make([]byte, w.buf.Len())
		copy(line, w.buf.Bytes())
		w.onLine(line)
		w.buf.Reset()
	}
}

// streamExecContext keeps StreamExec's own timeout independent from request
// deadlines while still relaying explicit cancellation from its caller. Spawn
// uses that cancellation for its budget and liveness watchdogs: the relay is
// what closes a wedged SPDY/WebSocket exec as soon as the watchdog fires.
func streamExecContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	commandCtx, cancelCommand := context.WithTimeout(context.Background(), timeout)
	ctx, cancelCause := context.WithCancelCause(commandCtx)
	cancel := func() {
		cancelCommand()
		cancelCause(context.Canceled)
	}
	if parent == nil {
		return ctx, cancel
	}
	go func() {
		select {
		case <-parent.Done():
			// Request/proxy deadlines must not shorten a spawn's configured
			// command timeout. Explicit cancellation is the control signal used
			// by stop, budget, and liveness watchdog paths.
			if errors.Is(parent.Err(), context.Canceled) {
				cancelCause(context.Cause(parent))
			}
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// StreamExec runs a command in a K8s pod and streams stdout line-by-line
// to opts.OnLine. It returns an ExecResult with aggregate stats after the
// command completes.
//
// This function takes K8s client/config/namespace directly rather than
// depending on the K8sBackend struct, so it can be called from the spawn
// orchestrator without modifying the Backend interface.
func StreamExec(parent context.Context, clientset kubernetes.Interface, restConfig *rest.Config, namespace string, nfsFlush bool, opts StreamExecOpts) (*ExecResult, error) {
	// Keep the command deadline independent from proxy/request deadlines, but
	// preserve explicit caller cancellation. In particular, the spawn liveness
	// watchdog must terminate the active remotecommand stream immediately.
	timeout := 5 * time.Minute
	if opts.TimeoutSec > 0 {
		timeout = time.Duration(opts.TimeoutSec) * time.Second
	}
	ctx, cancel := streamExecContext(parent, timeout)
	defer cancel()

	start := time.Now()

	// Build the shell command (same pattern as K8sBackend.Exec).
	shellCmd := opts.Command
	if len(opts.Env) > 0 {
		var envPrefix strings.Builder
		for k, v := range opts.Env {
			envPrefix.WriteString(fmt.Sprintf("export %s=%s; ", k, shellQuote(v)))
		}
		shellCmd = envPrefix.String() + shellCmd
	}
	if opts.WorkDir != "" {
		shellCmd = fmt.Sprintf("cd %s && %s", shellQuote(opts.WorkDir), shellCmd)
	}

	// NFS cache flush: force kernel to re-validate file attributes.
	if nfsFlush && opts.WorkDir != "" {
		flushCmd := fmt.Sprintf("stat -f %q >/dev/null 2>&1; ", opts.WorkDir)
		shellCmd = flushCmd + shellCmd
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(opts.ContainerID).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "devbox",
			Command:   []string{"sh", "-c", shellCmd},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := newExecForMode(restConfig, req.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	// Set up streaming stdout writer with tail buffer.
	const tailSize = 20
	tail := newTailRing(tailSize)

	stdoutWriter := &lineCallbackWriter{
		onLine: func(line []byte) {
			tail.add(string(line))
			// Forward to caller callback.
			if opts.OnLine != nil {
				opts.OnLine(line)
			}
		},
	}

	// Capture the head and complete tail without retaining the middle.
	stderrCapture := &stderrCapture{onLine: opts.OnStderr}
	defer stderrCapture.Close()

	streamErr := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: stdoutWriter,
		Stderr: stderrCapture,
	})

	// Flush any remaining buffered partial lines.
	stdoutWriter.Flush()
	stderrCapture.Flush()
	if stderrCapture.err != nil {
		return nil, fmt.Errorf("capture stderr: %w", stderrCapture.err)
	}

	durationMs := time.Since(start).Milliseconds()

	exitCode := 0
	if streamErr != nil {
		// A caller cancellation (for example the spawn liveness watchdog) is
		// distinct from the command deadline. Return its original cause so the
		// orchestrator records the stall diagnostic, then runs its normal output
		// capture and pod cleanup path instead of waiting for TimeoutSec.
		if parent != nil && parent.Err() != nil {
			return &ExecResult{
				ExitCode:   124,
				StdoutTail: "command canceled",
				DurationMs: durationMs,
			}, context.Cause(parent)
		}
		if ctx.Err() != nil {
			return &ExecResult{
				ExitCode:   124,
				StdoutTail: "command timed out",
				DurationMs: durationMs,
			}, nil
		}
		exitCode = parseExitCode(streamErr)
		// See K8sBackend.Exec — same rationale: when no stdout/stderr
		// got captured but the stream errored out, surface streamErr in
		// stderr so the failure mode (pod gone, container terminating,
		// exec rejected) is visible instead of an empty buffer. The
		// stdout side here is a line-streaming ring buffer so we gate
		// on totalLines instead of a buffer length.
		stderrCapture.surfaceError(tail.total, streamErr)
	}

	stdoutTail := strings.Join(tail.lines(), "\n")
	stderrTail, captureErr := stderrCapture.Tail()
	if captureErr != nil {
		return nil, fmt.Errorf("read stderr: %w", captureErr)
	}
	stderrTotal, stderrTrunc := stderrCapture.total, stderrCapture.total > tailSize

	return &ExecResult{
		ExitCode:    exitCode,
		StdoutLines: tail.total,
		StderrLines: stderrTotal,
		StdoutTail:  stdoutTail,
		StderrTail:  stderrTail,
		StderrHead:  stderrHead(string(stderrCapture.head[:stderrCapture.headLen])),
		DurationMs:  durationMs,
		Truncated:   tail.total > tailSize || stderrTrunc,
		OOMKilled:   exitCode == 137,
	}, nil
}

// stderrCapture retains an 8 KiB head and 21 fixed line slots (20 completed
// lines plus one in progress), using less than 64 KiB for tail buffers. A fixed
// byte window cannot preserve arbitrary-length lines: oversized lines spill to
// private temporary files, removed on eviction or Close. Only the retained
// lines occupy disk. Tail and whole-line callbacks necessarily allocate their
// complete output; those allocations are not retained by the capture.
// Truncation remains line-based, matching TruncateOutput for gate consumers.
type stderrCapture struct {
	head               [8 * 1024]byte
	headLen, headLines int
	lines              [21]stderrLine
	total              int
	onLine             func([]byte)
	err                error
}

type stderrLine struct {
	buf  [3 * 1024]byte
	n    int
	file *os.File
}

func (l *stderrLine) close() {
	if l.file != nil {
		name := l.file.Name()
		_ = l.file.Close()
		_ = os.Remove(name)
	}
	l.file = nil
	l.n = 0
}

func (l *stderrLine) write(p []byte) error {
	if l.file == nil && len(p) <= len(l.buf)-l.n {
		l.n += copy(l.buf[l.n:], p)
		return nil
	}
	if l.file == nil {
		f, err := os.CreateTemp("", "loom-stderr-*")
		if err != nil {
			return err
		}
		l.file = f
		if _, err := f.Write(l.buf[:l.n]); err != nil {
			return err
		}
	}
	_, err := l.file.Write(p)
	// n is also the nonempty marker for a spilled partial line.
	l.n = 1
	return err
}

func (l *stderrLine) text() (string, error) {
	if l.file == nil {
		return string(l.buf[:l.n]), nil
	}
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	var b strings.Builder
	_, err := io.Copy(&b, l.file)
	return b.String(), err
}

func (c *stderrCapture) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	original := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		n := len(p)
		if i >= 0 {
			n = i + 1
		}
		if c.headLines < 20 && c.headLen < len(c.head) {
			c.headLen += copy(c.head[c.headLen:], p[:n])
			if i >= 0 {
				c.headLines++
			}
		}
		content := n
		if i >= 0 {
			content--
		}
		if err := c.lines[c.total%21].write(p[:content]); err != nil {
			c.err = err
			return original - len(p), err
		}
		p = p[n:]
		if i >= 0 {
			c.finishLine()
			if c.err != nil {
				return original - len(p), c.err
			}
		}
	}
	return original, nil
}

func (c *stderrCapture) finishLine() {
	if c.onLine != nil {
		line, err := c.lines[c.total%21].text()
		if err != nil {
			c.err = err
			return
		}
		c.onLine([]byte(line))
	}
	c.total++
	c.lines[c.total%21].close()
}

func (c *stderrCapture) Flush() {
	if c.err == nil && c.lines[c.total%21].n > 0 {
		c.finishLine()
	}
}

// Tail is called after Flush, when there is no partial line left.
func (c *stderrCapture) Tail() (string, error) {
	if c.err != nil {
		return "", c.err
	}
	var b strings.Builder
	start := max(0, c.total-20)
	for i := start; i < c.total; i++ {
		line, err := c.lines[i%21].text()
		if err != nil {
			return "", err
		}
		if i > start {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String(), nil
}

func (c *stderrCapture) Close() {
	for i := range c.lines {
		c.lines[i].close()
	}
}

func (c *stderrCapture) surfaceError(stdoutLines int, err error) {
	if err == nil || c.total != 0 || stdoutLines != 0 {
		return
	}
	// Match surfaceExecStreamError; synthetic diagnostics are not callbacks.
	c.onLine = nil
	_, _ = c.Write([]byte("exec error: " + err.Error() + "\n"))
	c.Flush()
}
