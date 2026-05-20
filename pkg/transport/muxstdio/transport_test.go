package muxstdio_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/transport/muxstdio"
)

// testMetrics is an atomic-counter Metrics implementation used by tests that
// need to assert drop/dispatch counts. Methods are safe for concurrent use.
type testMetrics struct {
	dispatches     atomic.Int64
	dropsFullChan  atomic.Int64
	dropsNoPending atomic.Int64
	notifications  atomic.Int64
}

func (m *testMetrics) IncMuxDispatches()     { m.dispatches.Add(1) }
func (m *testMetrics) IncMuxDropsFullChan()  { m.dropsFullChan.Add(1) }
func (m *testMetrics) IncMuxDropsNoPending() { m.dropsNoPending.Add(1) }
func (m *testMetrics) IncMuxNotifications()  { m.notifications.Add(1) }

// newPair returns a wrapped client transport plus the raw server-side pipe.
// The client (wrapped) is what code under test exercises; the server is how
// the test simulates an MCP server.
func newPair(t *testing.T, opts ...muxstdio.Option) (client *muxstdio.Transport, server *mcp.PipeTransport) {
	t.Helper()
	a, b := mcp.NewPipeTransport()
	client = muxstdio.New(a, opts...)
	t.Cleanup(func() { _ = client.Close() })
	return client, b
}

// serverEcho reads one request from server and replies with a Result echoing
// the request id. Blocks until ctx cancels.
func serverEcho(t *testing.T, ctx context.Context, server *mcp.PipeTransport) {
	t.Helper()
	for {
		req, err := server.Recv(ctx)
		if err != nil {
			return
		}
		resp := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: req.ID, Result: []byte(`{"ok":true}`)}
		if err := server.Send(ctx, resp); err != nil {
			return
		}
	}
}

// newRequest is a tiny helper that fabricates a request Message; tests don't
// need full mcp.NewRequest validation.
func newRequest(id any, method string) *mcp.Message {
	return &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: id, Method: method}
}

func TestSend_RegistersPendingThenForwards(t *testing.T) {
	t.Parallel()
	client, server := newPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go serverEcho(t, ctx, server)

	if err := client.Send(ctx, newRequest(int64(1), "ping")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	resp, err := client.Recv(ctx, int64(1))
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if resp == nil {
		t.Fatal("Recv: nil message")
	}
	if got, want := idKeyForTest(resp.ID), idKeyForTest(int64(1)); got != want {
		t.Errorf("response id key = %q, want %q", got, want)
	}
}

func TestRecv_DemuxesByID_TwoConcurrentCalls(t *testing.T) {
	t.Parallel()
	client, server := newPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Custom server that responds to id=2 first, then id=1 — exercises the
	// cross-ordered-delivery code path.
	go func() {
		var firstReq, secondReq *mcp.Message
		for i := 0; i < 2; i++ {
			req, err := server.Recv(ctx)
			if err != nil {
				return
			}
			if firstReq == nil {
				firstReq = req
			} else {
				secondReq = req
			}
		}
		// Respond in reverse order.
		_ = server.Send(ctx, &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: secondReq.ID, Result: []byte(`{"second":true}`)})
		_ = server.Send(ctx, &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: firstReq.ID, Result: []byte(`{"first":true}`)})
	}()

	var wg sync.WaitGroup
	results := make(map[int64]any, 2)
	var resultsMu sync.Mutex

	for _, id := range []int64{1, 2} {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := client.Send(ctx, newRequest(id, "ping")); err != nil {
				t.Errorf("Send id=%d: %v", id, err)
				return
			}
			resp, err := client.Recv(ctx, id)
			if err != nil {
				t.Errorf("Recv id=%d: %v", id, err)
				return
			}
			resultsMu.Lock()
			results[id] = resp.ID
			resultsMu.Unlock()
		}()
	}
	wg.Wait()

	if got := idKeyForTest(results[1]); got != idKeyForTest(int64(1)) {
		t.Errorf("id=1 routed to wrong response: got id key %q", got)
	}
	if got := idKeyForTest(results[2]); got != idKeyForTest(int64(2)) {
		t.Errorf("id=2 routed to wrong response: got id key %q", got)
	}
}

func TestRecv_ContextCancel_RemovesPending(t *testing.T) {
	t.Parallel()
	client, server := newPair(t)

	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	// Drain server-side requests so Send doesn't block on the pipe buffer.
	go func() {
		for {
			if _, err := server.Recv(parent); err != nil {
				return
			}
		}
	}()

	// Send a request, then cancel the Recv context without the server
	// responding.
	if err := client.Send(parent, newRequest(int64(1), "slow")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	cancelCtx, cancelCall := context.WithCancel(parent)
	cancelCall()
	_, err := client.Recv(cancelCtx, int64(1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Recv after cancel: got err=%v, want context.Canceled", err)
	}

	// Pending entry should have been cleaned up: a second Send with the same
	// id must NOT return ErrDuplicateID.
	if err := client.Send(parent, newRequest(int64(1), "retry")); err != nil {
		t.Fatalf("Second Send (after cancel) should succeed, got: %v", err)
	}
}

func TestRecv_AfterClose_ReturnsClosedError(t *testing.T) {
	t.Parallel()
	client, server := newPair(t)
	_ = server

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Recv requires a pending entry; without it, Recv returns a "no pending"
	// error (programmer error path). To exercise the closed path, Send must
	// happen first — but Send after close fails with ErrClosed.
	err := client.Send(context.Background(), newRequest(int64(1), "ping"))
	if !errors.Is(err, muxstdio.ErrClosed) {
		t.Fatalf("Send after Close: got err=%v, want ErrClosed", err)
	}
}

func TestSlowDrainer_DoesNotBlockOtherCallers(t *testing.T) {
	t.Parallel()
	metrics := &testMetrics{}
	client, server := newPair(t, muxstdio.WithMetrics(metrics))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Server side: respond to every request immediately.
	go serverEcho(t, ctx, server)

	// Caller A: Send id=1 but DO NOT Recv. Its response will sit in the
	// pending chan (buffer 1).
	if err := client.Send(ctx, newRequest(int64(1), "stale")); err != nil {
		t.Fatalf("Caller A Send: %v", err)
	}

	// Caller B: full Send + Recv for id=2 must complete promptly.
	deadline := 200 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		if err := client.Send(ctx, newRequest(int64(2), "fast")); err != nil {
			done <- err
			return
		}
		_, err := client.Recv(ctx, int64(2))
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Caller B failed: %v", err)
		}
	case <-time.After(deadline):
		t.Fatalf("Caller B blocked > %v despite Caller A being a slow drainer", deadline)
	}
}

func TestNotificationFanOut_DropsOnFullChan(t *testing.T) {
	t.Parallel()
	metrics := &testMetrics{}
	client, server := newPair(t, muxstdio.WithMetrics(metrics))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Send notifyChanBufferOverflow notifications without reading NotificationCh.
	// Anything over the internal buffer must be dropped.
	const total = 32 // exceeds the package's internal buffer (16)
	for i := 0; i < total; i++ {
		note := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, Method: "notifications/test"}
		if err := server.Send(ctx, note); err != nil {
			t.Fatalf("server.Send notification #%d: %v", i, err)
		}
	}

	// Wait until the read loop has had a chance to process all messages.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if metrics.notifications.Load()+metrics.dropsFullChan.Load() >= int64(total) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	gotNotif := metrics.notifications.Load()
	gotDrops := metrics.dropsFullChan.Load()
	if gotNotif+gotDrops < int64(total) {
		t.Fatalf("after %d notifications, only saw notifications=%d drops=%d (sum=%d, want %d)",
			total, gotNotif, gotDrops, gotNotif+gotDrops, total)
	}
	if gotDrops == 0 {
		t.Errorf("expected at least one notification drop after exceeding buffer, got drops=%d notifications=%d",
			gotDrops, gotNotif)
	}
	_ = client
}

func TestClose_Idempotent(t *testing.T) {
	t.Parallel()
	client, _ := newPair(t)

	if err := client.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("third Close: %v", err)
	}
}

func TestClose_DeliversErrorToPendingWaiters(t *testing.T) {
	t.Parallel()
	client, server := newPair(t)

	parent := context.Background()

	// Drain server-side requests so Send doesn't block.
	go func() {
		for {
			if _, err := server.Recv(parent); err != nil {
				return
			}
		}
	}()

	if err := client.Send(parent, newRequest(int64(1), "never")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := client.Recv(parent, int64(1))
		done <- err
	}()

	// Give Recv a moment to block.
	time.Sleep(10 * time.Millisecond)

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, muxstdio.ErrClosed) {
			t.Fatalf("Recv after Close: got err=%v, want ErrClosed", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Recv did not return after Close")
	}
}

func TestSend_NilID_ReturnsErrNoID(t *testing.T) {
	t.Parallel()
	client, _ := newPair(t)

	err := client.Send(context.Background(), &mcp.Message{JSONRPC: mcp.JSONRPCVersion, Method: "notifications/x"})
	if !errors.Is(err, muxstdio.ErrNoID) {
		t.Fatalf("Send with nil id: got err=%v, want ErrNoID", err)
	}
}

func TestSend_DuplicateID_ReturnsErrDuplicateID(t *testing.T) {
	t.Parallel()
	client, server := newPair(t)

	parent := context.Background()
	go func() {
		for {
			if _, err := server.Recv(parent); err != nil {
				return
			}
		}
	}()

	if err := client.Send(parent, newRequest(int64(1), "a")); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	err := client.Send(parent, newRequest(int64(1), "b"))
	if !errors.Is(err, muxstdio.ErrDuplicateID) {
		t.Fatalf("duplicate Send: got err=%v, want ErrDuplicateID", err)
	}
}

func TestCall_RoundTrip(t *testing.T) {
	t.Parallel()
	client, server := newPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go serverEcho(t, ctx, server)

	resp, err := client.Call(ctx, newRequest(int64(42), "echo"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if idKeyForTest(resp.ID) != idKeyForTest(int64(42)) {
		t.Errorf("response id key = %q, want %q", idKeyForTest(resp.ID), idKeyForTest(int64(42)))
	}
}

// idKeyForTest mirrors the package-internal idKey so tests can compare
// JSON-unmarshaled float64 ids against the int64 ids callers send.
func idKeyForTest(id any) string {
	switch v := id.(type) {
	case nil:
		return ""
	case string:
		return "s:" + v
	case float64:
		if v == float64(int64(v)) {
			return "n:" + strconv.FormatInt(int64(v), 10)
		}
		return "n:" + strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return "n:" + strconv.Itoa(v)
	case int64:
		return "n:" + strconv.FormatInt(v, 10)
	default:
		return "o:?"
	}
}
