//go:build killtest

// Package muxstdio's kill-test prototype.
//
// Gating slice 1 of .loom/implementation-plan-stdio-mux-2026-05-20.md.
// Proves the load-bearing assumption from
// .loom/product-spec-stdio-mux-2026-05-20.md: wrapping mcp.StdioTransport in a
// demuxer keyed by JSON-RPC id is sufficient to safely run N concurrent
// Send+Recv pairs against a real local-stdio MCP server.
//
// Deliberately throwaway. The production package design budget belongs to
// slice 2 and is not constrained by anything in this file.
//
// Run:
//
//	go build -o /tmp/mcp-agent-context ./cmd/mcp-agent-context
//	go test ./pkg/transport/muxstdio/ -tags=killtest -run TestKill -v -count=3
package muxstdio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"
)

const (
	binPath        = "/tmp/mcp-agent-context"
	burstSize      = 10
	burstDeadline  = 300 * time.Millisecond
	followDeadline = 150 * time.Millisecond
)

// requireBin fails the test fast with a clear message if the binary isn't
// where slice 1 expects it.
func requireBin(t *testing.T) {
	t.Helper()
	info, err := os.Stat(binPath)
	if err != nil || info.IsDir() {
		t.Fatalf("kill-test prerequisite missing: %s\n"+
			"build it first:\n  go build -o %s ./cmd/mcp-agent-context",
			binPath, binPath)
	}
}

// idKey normalizes a JSON-RPC id (any) to a canonical string for map lookup.
// JSON unmarshal of `ID any` yields float64 for numbers, so a request sent
// with int64(42) comes back as float64(42) — both must hash to "42".
func idKey(id any) string {
	if id == nil {
		return ""
	}
	switch v := id.(type) {
	case string:
		return "s:" + v
	case float64:
		// Whole-number floats hash to the same key as their int form.
		if v == float64(int64(v)) {
			return "n:" + strconv.FormatInt(int64(v), 10)
		}
		return "n:" + strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return "n:" + strconv.Itoa(v)
	case int64:
		return "n:" + strconv.FormatInt(v, 10)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return "n:" + strconv.FormatInt(i, 10)
		}
		return "n:" + v.String()
	default:
		return fmt.Sprintf("o:%v", v)
	}
}

// demux is the prototype id-routing wrapper for the kill test. Production
// code lives in slice 2 and is intentionally not derived from this struct.
type demux struct {
	inner mcp.Transport

	mu      sync.Mutex
	pending map[string]chan *mcp.Message

	notifCh chan *mcp.Message

	done     chan struct{}
	closeOne sync.Once
	readerWG sync.WaitGroup

	// Counters for diagnostics on failure.
	delivered uint64
	dropped   uint64
}

func newDemux(inner mcp.Transport) *demux {
	d := &demux{
		inner:   inner,
		pending: make(map[string]chan *mcp.Message),
		notifCh: make(chan *mcp.Message, 16),
		done:    make(chan struct{}),
	}
	d.readerWG.Add(1)
	go d.readLoop()
	return d
}

func (d *demux) readLoop() {
	defer d.readerWG.Done()
	bg := context.Background()
	for {
		msg, err := d.inner.Recv(bg)
		if err != nil {
			// Drain any waiters with the error so they don't hang.
			d.mu.Lock()
			for k, ch := range d.pending {
				select {
				case ch <- nil:
				default:
				}
				delete(d.pending, k)
			}
			d.mu.Unlock()
			return
		}
		if msg == nil {
			continue
		}

		key := idKey(msg.ID)
		if key == "" {
			// Notification or id-less message.
			select {
			case d.notifCh <- msg:
			default:
				atomic.AddUint64(&d.dropped, 1)
			}
			continue
		}

		d.mu.Lock()
		ch, ok := d.pending[key]
		d.mu.Unlock()
		if !ok {
			atomic.AddUint64(&d.dropped, 1)
			continue
		}
		select {
		case ch <- msg:
			atomic.AddUint64(&d.delivered, 1)
		default:
			atomic.AddUint64(&d.dropped, 1)
		}
	}
}

// Send registers a pending chan for msg.ID, then forwards to inner.Send.
// Returns the chan for the caller to wait on with Recv.
func (d *demux) Send(ctx context.Context, msg *mcp.Message) (<-chan *mcp.Message, error) {
	key := idKey(msg.ID)
	if key == "" {
		return nil, errors.New("muxstdio: cannot Send message with no id (notifications go through a separate path)")
	}
	ch := make(chan *mcp.Message, 1)

	d.mu.Lock()
	if _, dup := d.pending[key]; dup {
		d.mu.Unlock()
		return nil, fmt.Errorf("muxstdio: duplicate pending id %q", key)
	}
	d.pending[key] = ch
	d.mu.Unlock()

	if err := d.inner.Send(ctx, msg); err != nil {
		d.mu.Lock()
		delete(d.pending, key)
		d.mu.Unlock()
		return nil, err
	}
	return ch, nil
}

// Recv waits for the response on the chan returned by Send, with context
// cancellation. Deregisters the pending entry on return.
func (d *demux) Recv(ctx context.Context, id any, ch <-chan *mcp.Message) (*mcp.Message, error) {
	key := idKey(id)
	defer func() {
		d.mu.Lock()
		delete(d.pending, key)
		d.mu.Unlock()
	}()
	select {
	case msg := <-ch:
		if msg == nil {
			return nil, errors.New("muxstdio: transport closed before response")
		}
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.done:
		return nil, errors.New("muxstdio: closed")
	}
}

func (d *demux) Close() error {
	d.closeOne.Do(func() { close(d.done) })
	return d.inner.Close()
}

// Call is a convenience that wraps Send+Recv.
func (d *demux) Call(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	ch, err := d.Send(ctx, msg)
	if err != nil {
		return nil, err
	}
	return d.Recv(ctx, msg.ID, ch)
}

// spawnedServer holds a running mcp-agent-context process and a demux over its
// stdio pipes.
type spawnedServer struct {
	cmd      *exec.Cmd
	stdin    *os.File
	stdout   *os.File
	transp   *mcp.StdioTransport
	mux      *demux
	stderrWG sync.WaitGroup
	stderr   []byte
	stderrMu sync.Mutex
}

func spawnServer(t *testing.T) *spawnedServer {
	t.Helper()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}

	cmd := exec.Command(binPath)
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	// Strip any qdrant / daemon env so the process boots in pure stdio mode.
	cmd.Env = append(os.Environ(),
		"LOOM_SOCKET=",
		"LOOM_DAEMON_HTTP_URL=",
		"AGENT_CONTEXT_DISABLE_BACKGROUND=1",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mcp-agent-context: %v", err)
	}
	// Close the child's ends in the parent; the child holds its own copies.
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()

	tx := mcp.NewStdioTransport(stdoutR, stdinW)
	srv := &spawnedServer{
		cmd:    cmd,
		stdin:  stdinW,
		stdout: stdoutR,
		transp: tx,
		mux:    newDemux(tx),
	}

	srv.stderrWG.Add(1)
	go func() {
		defer srv.stderrWG.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderrR.Read(buf)
			if n > 0 {
				srv.stderrMu.Lock()
				srv.stderr = append(srv.stderr, buf[:n]...)
				srv.stderrMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return srv
}

func (s *spawnedServer) capturedStderr() string {
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()
	return string(s.stderr)
}

func (s *spawnedServer) shutdown(t *testing.T) {
	t.Helper()
	_ = s.mux.Close()
	_ = s.stdin.Close()
	_ = s.stdout.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
	s.stderrWG.Wait()
}

func (s *spawnedServer) initialize(t *testing.T, ctx context.Context) {
	t.Helper()
	req, err := mcp.NewRequest(int64(1), "initialize", mcp.InitializeParams{
		ProtocolVersion: mcp.ProtocolVersion,
		Capabilities:    mcp.Capabilities{},
		ClientInfo:      mcp.ClientInfo{Name: "muxstdio-killtest", Version: "0.0.0"},
	})
	if err != nil {
		t.Fatalf("build initialize request: %v", err)
	}
	resp, err := s.mux.Call(ctx, req)
	if err != nil {
		t.Fatalf("initialize call: %v\nstderr:\n%s", err, s.capturedStderr())
	}
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
	// Send notifications/initialized — no id, no response.
	note := &mcp.Message{JSONRPC: "2.0", Method: "notifications/initialized"}
	if err := s.transp.Send(ctx, note); err != nil {
		t.Fatalf("send notifications/initialized: %v", err)
	}
}

// callToolsList issues tools/list via the demuxer with the given id. This is
// the wire-level burst payload: no handler IO (the server answers from
// in-memory tool registry), so latency is dominated by JSON marshal/unmarshal
// and a stdio round-trip. Picked over agent_context_recall (which calls an
// external embedder + Qdrant and adds ~300ms of network RTT, swamping the
// wire-level signal the spec's assumption is actually about).
func (s *spawnedServer) callToolsList(ctx context.Context, id int64) (*mcp.Message, time.Duration, error) {
	req, err := mcp.NewRequest(id, "tools/list", nil)
	if err != nil {
		return nil, 0, err
	}
	start := time.Now()
	resp, err := s.mux.Call(ctx, req)
	return resp, time.Since(start), err
}

// callRecall issues a single agent_context_recall via the demuxer. Kept around
// because the spec's original payload was this tool; useful as a single
// warmup call to flush per-process embedder/Qdrant state, and as a probe for
// follow-up slices that measure end-to-end concurrency with real handlers.
func (s *spawnedServer) callRecall(ctx context.Context, id int64) (*mcp.Message, time.Duration, error) {
	params := mcp.CallToolParams{
		Name:      "agent_context_recall",
		Arguments: map[string]any{"query": fmt.Sprintf("kill-test query #%d", id)},
	}
	req, err := mcp.NewRequest(id, "tools/call", params)
	if err != nil {
		return nil, 0, err
	}
	start := time.Now()
	resp, err := s.mux.Call(ctx, req)
	return resp, time.Since(start), err
}

// TestKill_TenConcurrentAgentContextRecallCalls is the gating kill-test for
// slice 1 of the stdio-mux plan. It spawns the real mcp-agent-context binary,
// performs MCP handshake + one warmup call, then bursts 10 concurrent
// agent_context_recall calls through the prototype demuxer and asserts:
//
//	(a) all 10 responses arrive within burstDeadline,
//	(b) every response's id was routed to the goroutine that owned that id,
//	(c) an 11th call after the burst succeeds within followDeadline,
//	(d) the spawned process is still alive.
//
// Run with: -tags=killtest -count=3. See file-level docs for the build cmd.
func TestKill_TenConcurrentAgentContextRecallCalls(t *testing.T) {
	requireBin(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := spawnServer(t)
	defer srv.shutdown(t)

	// 1) Handshake.
	srv.initialize(t, ctx)

	// 2) Warmup with a single tools/list — same payload as the burst, primes
	//    the server's read loop and any per-process JIT/caches. Cheap and
	//    deterministic; does not depend on external services.
	if _, _, err := srv.callToolsList(ctx, 2); err != nil {
		t.Fatalf("warmup tools/list: %v\nstderr:\n%s", err, srv.capturedStderr())
	}

	// 3) The burst. Ids 100..109 — distinct, easy to spot in failure logs.
	//    Burst payload is tools/list (no handler IO), per the doc-string on
	//    callToolsList. The spec's load-bearing assumption is about the WIRE
	//    (Send/Recv pair safety + id routing); handler-side IO/concurrency is
	//    a separate, downstream concern.
	type result struct {
		ownerID  int64
		respID   any
		dur      time.Duration
		err      error
		isErrRes bool
	}
	results := make([]result, burstSize)
	var wg sync.WaitGroup
	burstStart := time.Now()
	for i := 0; i < burstSize; i++ {
		i := i
		ownerID := int64(100 + i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, callCancel := context.WithTimeout(ctx, burstDeadline+200*time.Millisecond)
			defer callCancel()
			resp, dur, err := srv.callToolsList(callCtx, ownerID)
			r := result{ownerID: ownerID, dur: dur, err: err}
			if resp != nil {
				r.respID = resp.ID
				r.isErrRes = resp.Error != nil
			}
			results[i] = r
		}()
	}
	wg.Wait()
	burstWall := time.Since(burstStart)

	// (a) wall-clock budget.
	if burstWall > burstDeadline {
		t.Errorf("(a) burst of %d concurrent calls took %v, want < %v\nresults: %+v",
			burstSize, burstWall, burstDeadline, results)
	}

	// (b) id routing: each goroutine must have received exactly its own id back.
	for _, r := range results {
		if r.err != nil {
			t.Errorf("(b) call id=%d failed: %v", r.ownerID, r.err)
			continue
		}
		if idKey(r.respID) != idKey(r.ownerID) {
			t.Errorf("(b) misrouted: goroutine for id=%d received response with id=%v", r.ownerID, r.respID)
		}
		if r.isErrRes {
			t.Errorf("(b) call id=%d returned an error response (tools/list should never fail)", r.ownerID)
		}
	}

	// (c) follow-up 11th call after the burst.
	followCtx, followCancel := context.WithTimeout(ctx, followDeadline+200*time.Millisecond)
	defer followCancel()
	_, followDur, err := srv.callToolsList(followCtx, 999)
	if err != nil {
		t.Errorf("(c) follow-up call failed: %v\nstderr:\n%s", err, srv.capturedStderr())
	} else if followDur > followDeadline {
		t.Errorf("(c) follow-up call took %v, want < %v", followDur, followDeadline)
	}

	// (d) process still alive. syscall.Signal(0) is the POSIX idiom for an
	//    existence check: doesn't deliver a signal, but returns an error if
	//    the process is gone or unreachable.
	if srv.cmd.Process == nil {
		t.Errorf("(d) cmd.Process is nil — server didn't start cleanly")
	} else if err := srv.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("(d) process not alive: %v", err)
	}

	// Stderr should not show transport-level corruption.
	stderr := srv.capturedStderr()
	for _, bad := range []string{"transport closed", "broken pipe", "EOF"} {
		if contains(stderr, bad) {
			t.Errorf("server stderr contains %q (transport-level corruption suspected):\n%s", bad, stderr)
		}
	}

	t.Logf("burst wall=%v follow=%v delivered=%d dropped=%d",
		burstWall, followDur,
		atomic.LoadUint64(&srv.mux.delivered),
		atomic.LoadUint64(&srv.mux.dropped),
	)
}

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
