package muxstdio_test

import (
	"context"
	"errors"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/transport/muxstdio"
)

// waitCounter polls an atomic-backed getter until it reaches want or the
// deadline passes. The reader goroutine dispatches asynchronously, so the
// assertions below cannot read the counters synchronously after Send.
func waitCounter(t *testing.T, name string, get func() int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if get() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s = %d, want %d", name, get(), want)
}

// A response that lands after the caller's context expired is the server
// finishing work the caller stopped waiting for. It must be metered as a late
// response and must not be reported as an unsolicited drop: the 2026-09-13
// daemon log carried 2,290 "dropped response (no pending waiter)" warnings in
// a day, every one of them a session-list reply arriving inside the same
// second the 3s budget fired.
func TestRecv_LateResponseAfterCallerTimeoutIsMeteredNotDropped(t *testing.T) {
	t.Parallel()
	metrics := &testMetrics{}
	client, server := newPair(t, muxstdio.WithMetrics(metrics))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// The server reads the request but only answers once told to.
	release := make(chan struct{})
	go func() {
		req, err := server.Recv(ctx)
		if err != nil {
			return
		}
		<-release
		_ = server.Send(ctx, &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: req.ID, Result: []byte(`{"late":true}`)})
	}()

	if err := client.Send(ctx, newRequest(int64(7), "slow")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	callCtx, callCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer callCancel()
	if _, err := client.Recv(callCtx, int64(7)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Recv err = %v, want context.DeadlineExceeded", err)
	}

	close(release)

	waitCounter(t, "lateResponses", metrics.lateResponses.Load, 1)
	if got := metrics.dropsNoPending.Load(); got != 0 {
		t.Errorf("dropsNoPending = %d, want 0 (late reply must not count as unsolicited)", got)
	}
	if got := metrics.dispatches.Load(); got != 0 {
		t.Errorf("dispatches = %d, want 0", got)
	}
}

// A response for an id nobody ever sent remains an unsolicited drop.
func TestReadLoop_UnsolicitedResponseStillCountsAsDrop(t *testing.T) {
	t.Parallel()
	metrics := &testMetrics{}
	_, server := newPair(t, muxstdio.WithMetrics(metrics))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := server.Send(ctx, &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: int64(99), Result: []byte(`{}`)}); err != nil {
		t.Fatalf("server Send: %v", err)
	}

	waitCounter(t, "dropsNoPending", metrics.dropsNoPending.Load, 1)
	if got := metrics.lateResponses.Load(); got != 0 {
		t.Errorf("lateResponses = %d, want 0", got)
	}
}

// An abandoned id is consumed by its late response: a second response with the
// same id is unsolicited again, so the set cannot be used to launder repeats.
func TestReadLoop_AbandonedIDIsConsumedByFirstLateResponse(t *testing.T) {
	t.Parallel()
	metrics := &testMetrics{}
	client, server := newPair(t, muxstdio.WithMetrics(metrics))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		req, err := server.Recv(ctx)
		if err != nil {
			return
		}
		// Wait for the caller to give up, then answer twice.
		time.Sleep(60 * time.Millisecond)
		for i := 0; i < 2; i++ {
			_ = server.Send(ctx, &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: req.ID, Result: []byte(`{}`)})
		}
	}()

	if err := client.Send(ctx, newRequest(int64(3), "slow")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	callCtx, callCancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer callCancel()
	if _, err := client.Recv(callCtx, int64(3)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Recv err = %v, want context.DeadlineExceeded", err)
	}

	waitCounter(t, "lateResponses", metrics.lateResponses.Load, 1)
	waitCounter(t, "dropsNoPending", metrics.dropsNoPending.Load, 1)
}
