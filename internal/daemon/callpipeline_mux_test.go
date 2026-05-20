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
			resp, err := d.handleCall(context.Background(), makeMsg(fmt.Sprintf("req-%d", idx)))
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

	// If the local path were still serialized (callLock kept around, or
	// mux mis-wired), wall-clock would be ~callers*perCallLatency = 1000ms.
	// Allow generous slack (3×) for CI scheduling jitter.
	parallelBudget := perCallLatency * 3
	if elapsed > parallelBudget {
		t.Fatalf("local mux'd calls appear serialized: %d concurrent callers took %v, want < %v",
			callers, elapsed, parallelBudget)
	}
}

// TestHandleCall_LocalConcurrentCallsHighFanout exercises the mux under
// heavier load to catch super-linear degradation. 50 concurrent callers
// × 50 ms latency must complete in <250 ms.
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
			resp, err := d.handleCall(context.Background(), makeMsg(fmt.Sprintf("req-%d", idx)))
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

	// Serialized baseline would be 50*50ms = 2.5s. Allow 5× single-call
	// latency to catch super-linear degradation without flaking on jitter.
	parallelBudget := perCallLatency * 5
	if elapsed > parallelBudget {
		t.Fatalf("local mux'd high-fanout calls degraded: %d concurrent callers took %v, want < %v",
			callers, elapsed, parallelBudget)
	}
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
