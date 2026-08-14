package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

// Regression: a hub dial whose WebSocket upgrade succeeds but whose init
// response never arrives (wedged tunnel stream) must fail within the
// handshake bound even when the caller context carries no deadline.
// Observed live 2026-07-11: gitlab tools/call hung 1800s+ inside the hub
// DialFunc because initializeMCPTransport inherited an unbounded request ctx.
func TestInitializeMCPTransportWithTimeout_BlockedRecvReturns(t *testing.T) {
	blocked := &fakeTransport{
		recvFn: func(ctx context.Context) (*mcp.Message, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- initializeMCPTransportWithTimeout(context.Background(), blocked, 50*time.Millisecond)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected handshake error, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initializeMCPTransportWithTimeout did not return; unbounded handshake regression")
	}
}

// The handshake bound must still defer to an earlier caller deadline.
func TestInitializeMCPTransportWithTimeout_CallerCancelWins(t *testing.T) {
	blocked := &fakeTransport{
		recvFn: func(ctx context.Context) (*mcp.Message, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- initializeMCPTransportWithTimeout(ctx, blocked, time.Hour)
	}()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected handshake error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake ignored caller cancellation")
	}
}
