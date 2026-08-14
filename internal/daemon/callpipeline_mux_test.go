package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	kitregistry "gitlab.flexinfer.ai/libs/fi-mcp-kit/pkg/registry"
	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/pool"
	"github.com/crb2nu/loom/internal/router"
	"github.com/crb2nu/loom/pkg/transport/muxstdio"
)

// asyncEchoTransport models a stdio server that accepts many in-flight
// Sends and responds asynchronously after a per-request delay. Each
// response carries the originating request's id. Unlike slowEchoTransport
// (which serializes on lastID), this transport supports multiple
// concurrent in-flight requests — the property the per-id mux is meant
// to exploit.
type asyncEchoTransport struct {
	delay     time.Duration
	ch        chan *mcp.Message
	closeCh   chan struct{}
	closeOnce sync.Once
}

func newAsyncEchoTransport(delay time.Duration) *asyncEchoTransport {
	return &asyncEchoTransport{
		delay:   delay,
		ch:      make(chan *mcp.Message, 256),
		closeCh: make(chan struct{}),
	}
}

func (t *asyncEchoTransport) Send(_ context.Context, msg *mcp.Message) error {
	id := msg.ID
	go func() {
		select {
		case <-time.After(t.delay):
		case <-t.closeCh:
			return
		}
		resp := &mcp.Message{
			JSONRPC: mcp.JSONRPCVersion,
			ID:      id,
			Result:  json.RawMessage(`{"ok":true}`),
		}
		select {
		case t.ch <- resp:
		case <-t.closeCh:
		}
	}()
	return nil
}

func (t *asyncEchoTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	select {
	case msg, ok := <-t.ch:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.closeCh:
		return nil, io.EOF
	}
}

func (t *asyncEchoTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closeCh) })
	return nil
}

// TestHandleCall_LocalConcurrentCallsRunInParallel_WithMux is the S3
// regression for "the per-id mux unblocks parallel calls on a single
// shared stdio pipe". Mirrors TestHandleCall_HubConcurrentCallsRunInParallel
// but on the TargetLocal path with LOOM_MUX_STDIO=on.
//
// Setup: a single shared asyncEchoTransport stands in for one stdio
// process. Every pool dial returns a perConnTransport over the SAME
// muxstdio.Transport for that server name — modelling the production
// dial path with the mux cache. 10 concurrent callers, each with a
// distinct request id, must complete in parallel. If the mux is
// wired correctly, wall-clock is ~perCallLatency. If it accidentally
// serialized (e.g. callLock left in place), wall-clock would be
// ~callers*perCallLatency.
func TestHandleCall_LocalConcurrentCallsRunInParallel_WithMux(t *testing.T) {
	d := newCallPipelineTestDaemon()
	d.muxStdio = true
	d.muxCache = newMuxCache(d.logger)
	t.Cleanup(d.muxCache.CloseAll)

	d.router = router.New(router.Config{
		HubEnabled: false,
		Registry: &kitregistry.Registry{
			Servers: []*kitregistry.Server{
				{Name: "local_mux_parallel", Categories: []string{"local-only"}},
			},
		},
	})

	const perCallLatency = 100 * time.Millisecond
	const callers = 10

	shared := newAsyncEchoTransport(perCallLatency)
	t.Cleanup(func() { _ = shared.Close() })

	d.pool = pool.New(pool.Config{
		MaxIdle:     callers,
		MaxOpen:     callers,
		IdleTimeout: time.Minute,
		DialFunc: func(_ context.Context, serverName string) (mcp.Transport, error) {
			mux := d.muxCache.GetOrCreate(serverName, shared)
			return newPerConnTransport(mux), nil
		},
	})
	t.Cleanup(func() { _ = d.pool.Close() })

	makeMsg := func(id string) *mcp.Message {
		msg := newCallMessage(t, map[string]any{
			"server": "local_mux_parallel",
			"tool":   "echo",
		})
		msg.ID = id
		return msg
	}

	// If the local path were still serialized (callLock kept around, or
	// mux mis-wired), wall-clock would be ~callers*perCallLatency = 1000ms
	// on EVERY attempt. The parallel path is ~perCallLatency plus scheduling
	// jitter, which on loaded CI runners has been observed to exceed 500ms
	// (job 202383). Budget at half the serialized baseline and allow retries:
	// jitter is random per attempt, a real serialization bug is deterministic
	// and blows the budget every time.
	assertParallelWallClock(t, callers, perCallLatency, func(attempt int) time.Duration {
		return runMuxFanout(t, d, callers, func(idx int) *mcp.Message {
			return makeMsg(fmt.Sprintf("req-%d-%d", attempt, idx))
		})
	})
}

// runMuxFanout fires callers concurrent handleCall invocations, asserts every
// call succeeds, and returns the wall-clock time for the whole fan-out.
func runMuxFanout(t *testing.T, d *Daemon, callers int, makeMsg func(idx int) *mcp.Message) time.Duration {
	t.Helper()
	type result struct {
		resp *mcp.Message
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			resp, err := d.handleCall(context.Background(), makeMsg(idx))
			results <- result{resp: resp, err: err}
		}(i)
	}

	beginAt := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(beginAt)
	close(results)

	for res := range results {
		if res.err != nil {
			t.Fatalf("unexpected call error: %v", res.err)
		}
		if res.resp == nil || res.resp.Error != nil {
			t.Fatalf("expected success, got %+v", res.resp)
		}
	}
	return elapsed
}

// assertParallelWallClock distinguishes the parallel regime (~perCallLatency)
// from the serialized regime (~callers*perCallLatency) without flaking on
// loaded runners. The budget is half the serialized baseline — far above any
// observed scheduling jitter, far below a serialized run — and the fan-out is
// retried a couple of times because jitter is random per attempt while a real
// serialization regression exceeds the budget on every attempt.
func assertParallelWallClock(t *testing.T, callers int, perCallLatency time.Duration, run func(attempt int) time.Duration) {
	t.Helper()
	serializedBaseline := time.Duration(callers) * perCallLatency
	parallelBudget := serializedBaseline / 2
	var elapsed time.Duration
	for attempt := 0; attempt < 3; attempt++ {
		elapsed = run(attempt)
		if elapsed <= parallelBudget {
			return
		}
		t.Logf("attempt %d: %d concurrent callers took %v (budget %v); retrying in case of runner load",
			attempt, callers, elapsed, parallelBudget)
	}
	t.Fatalf("local mux'd calls appear serialized: %d concurrent callers took %v on every attempt, want < %v (serialized baseline %v)",
		callers, elapsed, parallelBudget, serializedBaseline)
}

// TestHandleCall_LocalConcurrentCallsHighFanout exercises the mux under
// heavier load: 50 concurrent callers × 50 ms latency must stay in the
// parallel regime (~perCallLatency), not the serialized one (2.5 s).
func TestHandleCall_LocalConcurrentCallsHighFanout_WithMux(t *testing.T) {
	d := newCallPipelineTestDaemon()
	d.muxStdio = true
	d.muxCache = newMuxCache(d.logger)
	t.Cleanup(d.muxCache.CloseAll)

	d.router = router.New(router.Config{
		HubEnabled: false,
		Registry: &kitregistry.Registry{
			Servers: []*kitregistry.Server{
				{Name: "local_mux_fanout", Categories: []string{"local-only"}},
			},
		},
	})

	const perCallLatency = 50 * time.Millisecond
	const callers = 50

	shared := newAsyncEchoTransport(perCallLatency)
	t.Cleanup(func() { _ = shared.Close() })

	d.pool = pool.New(pool.Config{
		MaxIdle:     callers,
		MaxOpen:     callers,
		IdleTimeout: time.Minute,
		DialFunc: func(_ context.Context, serverName string) (mcp.Transport, error) {
			mux := d.muxCache.GetOrCreate(serverName, shared)
			return newPerConnTransport(mux), nil
		},
	})
	t.Cleanup(func() { _ = d.pool.Close() })

	makeMsg := func(id string) *mcp.Message {
		msg := newCallMessage(t, map[string]any{
			"server": "local_mux_fanout",
			"tool":   "echo",
		})
		msg.ID = id
		return msg
	}

	// Serialized baseline would be 50*50ms = 2.5s; the parallel path is
	// ~50ms plus scheduling jitter. The old 250ms absolute budget flaked on
	// loaded CI runners (job 202383: 602ms with the mux healthy), so the
	// budget is now half the serialized baseline with retries — see
	// assertParallelWallClock.
	assertParallelWallClock(t, callers, perCallLatency, func(attempt int) time.Duration {
		return runMuxFanout(t, d, callers, func(idx int) *mcp.Message {
			return makeMsg(fmt.Sprintf("req-%d-%d", attempt, idx))
		})
	})
}

// TestMuxCache_EvictClosesMuxAndDrainsPending verifies that stopServerProc
// (via muxCache.Evict) propagates ErrClosed to any in-flight callers
// blocked on the mux. This is the property that makes the lock-free
// stop path safe — without it, callers would hang on Recv after the
// process exited.
func TestMuxCache_EvictClosesMuxAndDrainsPending(t *testing.T) {
	d := newCallPipelineTestDaemon()
	d.muxStdio = true
	d.muxCache = newMuxCache(d.logger)
	t.Cleanup(d.muxCache.CloseAll)

	// Inner transport that never responds — every Send is silently
	// accepted; Recv blocks until Close. Forces the test to depend on
	// Evict for unblocking.
	shared := newAsyncEchoTransport(10 * time.Second)
	t.Cleanup(func() { _ = shared.Close() })

	mux := d.muxCache.GetOrCreate("evict_test", shared)
	conn := newPerConnTransport(mux)

	// Issue a Send and start a Recv in another goroutine.
	req := &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      "evict-1",
		Method:  "tools/list",
	}
	if err := conn.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.Recv(context.Background())
		done <- err
	}()

	// Let the Recv park before we Evict.
	time.Sleep(20 * time.Millisecond)
	d.muxCache.Evict("evict_test")

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from Recv after Evict, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv did not return after Evict; mux teardown leaked")
	}
}

// TestPerConnTransport_ConcurrentCallersWithCollidingCallerIDs is the
// regression for the agent_context restart cascade where multiple
// concurrent callers (e.g. embedded-hud heartbeats) all issued
// requests with the same JSON-RPC id (id=1 from a fresh initialize)
// against the same shared mux, causing muxstdio.ErrDuplicateID,
// which the daemon misinterpreted as subprocess death and
// triggered a restart cascade.
//
// The fix: perConnTransport.Send rewrites the id to a unique
// internal value before forwarding to the shared mux, and restores
// the caller's original id on the response. With that, colliding
// caller ids are routed correctly to their originating conn.
func TestPerConnTransport_ConcurrentCallersWithCollidingCallerIDs(t *testing.T) {
	d := newCallPipelineTestDaemon()
	d.muxStdio = true
	d.muxCache = newMuxCache(d.logger)
	t.Cleanup(d.muxCache.CloseAll)

	const callers = 16
	const delay = 25 * time.Millisecond

	shared := newAsyncEchoTransport(delay)
	t.Cleanup(func() { _ = shared.Close() })

	mux := d.muxCache.GetOrCreate("collide_test", shared)

	type result struct {
		respID any
		err    error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := newPerConnTransport(mux)
			<-start
			req := &mcp.Message{
				JSONRPC: mcp.JSONRPCVersion,
				// Every caller uses the SAME id, mirroring concurrent
				// embedded-hud heartbeat retries after a subprocess restart.
				ID:     int64(1),
				Method: "tools/list",
			}
			if err := conn.Send(context.Background(), req); err != nil {
				results <- result{err: err}
				return
			}
			resp, err := conn.Recv(context.Background())
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{respID: resp.ID, err: nil}
		}()
	}

	close(start)
	wg.Wait()
	close(results)

	successes := 0
	for r := range results {
		if r.err != nil {
			t.Fatalf("concurrent caller with id=1 failed (expected fix #3 to dedup ids): %v", r.err)
		}
		// Caller's original id must be restored on the response so the
		// upstream callpipeline ID-match check (callpipeline_stages.go:433)
		// passes.
		if fmt.Sprint(r.respID) != "1" {
			t.Fatalf("response id = %v (%T), want 1 — perConnTransport did not restore caller id",
				r.respID, r.respID)
		}
		successes++
	}
	if successes != callers {
		t.Fatalf("got %d successful responses, want %d", successes, callers)
	}
}

// TestCallPipeline_MuxstdioDuplicateID_DoesNotRestartSubprocess is the
// classifier regression for fix #2. Even if a duplicate-id error
// reaches transportFailure (e.g. a future caller that bypasses
// perConnTransport's id rewriter), the daemon must NOT tear down the
// subprocess — duplicate-id is a muxer-side state error, not
// subprocess death. Tearing down the subprocess on duplicate-id is
// what amplified the 2026-05-25 agent_context cascade.
func TestCallPipeline_MuxstdioDuplicateID_DoesNotRestartSubprocess(t *testing.T) {
	d := newCallPipelineTestDaemon()

	d.router = router.New(router.Config{
		HubEnabled:       false,
		FailureThreshold: 10,
		Registry: &kitregistry.Registry{
			Servers: []*kitregistry.Server{
				{Name: "dup_id_srv", Categories: []string{"local-only"}},
			},
		},
	})

	// Mark the server as already running so we can detect whether
	// the daemon delete it (which would mean it tore down the
	// subprocess).
	d.runningServers.Store("dup_id_srv", true)

	dials := 0
	d.pool = pool.New(pool.Config{
		MaxIdle:     2,
		MaxOpen:     2,
		IdleTimeout: time.Minute,
		DialFunc: func(_ context.Context, _ string) (mcp.Transport, error) {
			dials++
			// Fake transport that fails Send with ErrDuplicateID.
			return &fakeTransport{
				sendErr: fmt.Errorf("tools/call failed during send: %w", muxstdio.ErrDuplicateID),
			}, nil
		},
	})
	defer func() { _ = d.pool.Close() }()

	msg := newCallMessage(t, map[string]any{
		"server": "dup_id_srv",
		"tool":   "check",
	})

	resp, err := d.handleCall(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.Error == nil {
		t.Fatalf("expected error response, got %+v", resp)
	}

	// Critical assertion: subprocess must NOT have been torn down.
	// stopServerProc + runningServers.Delete would remove the entry.
	if _, alive := d.runningServers.Load("dup_id_srv"); !alive {
		t.Fatal("daemon deleted running server entry on muxstdio.ErrDuplicateID — subprocess was killed when it should have been preserved")
	}
}
