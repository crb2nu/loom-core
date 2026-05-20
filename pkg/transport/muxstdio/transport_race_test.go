package muxstdio_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/transport/muxstdio"
)

// TestRace_HighFanoutSendRecv exercises the demuxer under heavy concurrent
// load. Intended to be run with `-race` (the build tag `race` is implicit
// when `go test -race` is used; we don't gate the test on it explicitly so
// that even non-race CI runs catch deadlocks and panics).
//
// 64 goroutines each issue 100 Send+Recv pairs through a shared Transport,
// with randomly chosen ids. The asserts are: no deadlock, no panic, every
// goroutine receives a response with the id it sent, and the dispatch
// counter equals the total request count.
func TestRace_HighFanoutSendRecv(t *testing.T) {
	t.Parallel()

	const (
		goroutines  = 64
		perRoutine  = 100
		totalCalls  = goroutines * perRoutine
		testTimeout = 30 * time.Second
	)

	metrics := &testMetrics{}
	a, b := mcp.NewPipeTransport()
	client := muxstdio.New(a, muxstdio.WithMetrics(metrics))
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Server: echo every request with a Result that includes the request id.
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			req, err := b.Recv(ctx)
			if err != nil {
				return
			}
			resp := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: req.ID, Result: []byte(`{}`)}
			if err := b.Send(ctx, resp); err != nil {
				return
			}
		}
	}()

	var nextID int64
	var failures atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perRoutine; i++ {
				id := atomic.AddInt64(&nextID, 1)
				req := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: id, Method: "ping"}
				if err := client.Send(ctx, req); err != nil {
					t.Errorf("g=%d i=%d Send id=%d: %v", g, i, id, err)
					failures.Add(1)
					return
				}
				resp, err := client.Recv(ctx, id)
				if err != nil {
					t.Errorf("g=%d i=%d Recv id=%d: %v", g, i, id, err)
					failures.Add(1)
					return
				}
				if idKeyForTest(resp.ID) != idKeyForTest(id) {
					t.Errorf("g=%d i=%d misroute: sent id=%d got id=%v", g, i, id, resp.ID)
					failures.Add(1)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	if failures.Load() > 0 {
		t.Fatalf("%d goroutine failures", failures.Load())
	}

	if got, want := metrics.dispatches.Load(), int64(totalCalls); got != want {
		t.Errorf("dispatches=%d, want %d", got, want)
	}
	if drops := metrics.dropsFullChan.Load() + metrics.dropsNoPending.Load(); drops != 0 {
		t.Errorf("unexpected drops: full=%d no-pending=%d", metrics.dropsFullChan.Load(), metrics.dropsNoPending.Load())
	}
}

// stdioLikeTransport is a mock mcp.Transport that mirrors StdioTransport's
// concurrent-close contract: Send and Recv return a "closed" error after
// Close, never panic. Used by TestRace_ConcurrentSendAndClose because
// PipeTransport's own Close races with concurrent Send and panics on
// send-after-close — quirks specific to that test fixture, not present in
// the production StdioTransport this wrapper targets.
type stdioLikeTransport struct {
	mu        sync.Mutex
	msgCh     chan *mcp.Message // server -> client; what Recv reads
	echo      bool              // if true, every Send is echoed to msgCh
	closed    bool
	done      chan struct{}
	closeOnce sync.Once
}

func newStdioLikeTransport(echo bool) *stdioLikeTransport {
	return &stdioLikeTransport{
		msgCh: make(chan *mcp.Message, 64),
		echo:  echo,
		done:  make(chan struct{}),
	}
}

func (s *stdioLikeTransport) Send(ctx context.Context, msg *mcp.Message) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return muxstdio.ErrClosed
	}
	s.mu.Unlock()
	if !s.echo {
		return nil
	}
	resp := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: msg.ID, Result: []byte(`{}`)}
	select {
	case s.msgCh <- resp:
		return nil
	case <-s.done:
		return muxstdio.ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *stdioLikeTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	select {
	case msg, ok := <-s.msgCh:
		if !ok {
			return nil, muxstdio.ErrClosed
		}
		return msg, nil
	case <-s.done:
		return nil, muxstdio.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *stdioLikeTransport) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
	})
	return nil
}

// TestRace_ConcurrentSendAndClose ensures Close is safe to call while other
// goroutines are mid-Send. Uses stdioLikeTransport (matches production
// StdioTransport's close semantics) so the race detector flags only races in
// MY code, not in the test fixture.
func TestRace_ConcurrentSendAndClose(t *testing.T) {
	t.Parallel()

	inner := newStdioLikeTransport(true)
	client := muxstdio.New(inner)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const senders = 16
	var wg sync.WaitGroup
	wg.Add(senders)
	for g := 0; g < senders; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := int64(g*1000 + i)
				if err := client.Send(ctx, &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: id, Method: "ping"}); err != nil {
					return // accept ErrClosed mid-loop
				}
				if _, err := client.Recv(ctx, id); err != nil {
					return
				}
			}
		}(g)
	}

	// Let some Send/Recv pairs land before slamming Close.
	time.Sleep(10 * time.Millisecond)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Senders should observe ErrClosed and exit quickly.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("senders did not exit within 5s after Close")
	}
}
