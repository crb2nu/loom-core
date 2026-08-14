package daemon

import (
	"context"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/daemon/generation"
	"github.com/crb2nu/loom/internal/pool"
	"github.com/crb2nu/loom/internal/router"
)

func TestCallPipelineMuxCancellationPreservesSharedGeneration(t *testing.T) {
	tests := []struct {
		name       string
		stage      string
		context    func() (context.Context, context.CancelFunc)
		failure    error
		checkRetry bool
	}{
		{
			name:  "explicit cancellation during recv",
			stage: "recv",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			failure: context.Canceled,
		},
		{
			name:  "parent deadline during recv",
			stage: "recv",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			failure: daemonRPCPhaseError("tools/call", "recv", time.Second, context.DeadlineExceeded),
		},
		{
			name:  "parent deadline during send skips reconnect",
			stage: "send",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			failure:    daemonRPCPhaseError("tools/call", "send", time.Second, context.DeadlineExceeded),
			checkRetry: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resource := &localCheckoutTestResource{}
			core := generation.New(func(context.Context, string, uint64) (generation.Resource, error) {
				return resource, nil
			})
			generationID, _, err := core.Ensure(context.Background(), "shared-cancel")
			if err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			callerLease, err := core.AcquireLease("shared-cancel", generationID)
			if err != nil {
				t.Fatalf("AcquireLease(caller) error = %v", err)
			}
			survivorLease, err := core.AcquireLease("shared-cancel", generationID)
			if err != nil {
				t.Fatalf("AcquireLease(survivor) error = %v", err)
			}
			defer func() {
				callerLease.Release()
				survivorLease.Release()
				_ = core.Shutdown(context.Background())
			}()

			d := newCallPipelineTestDaemon()
			d.muxStdio = true
			d.serverSupervisor = &serverSupervisor{core: core}
			d.pool = newTestPool(t)
			defer func() { _ = d.pool.Close() }()
			ctx, cancel := tc.context()
			defer cancel()
			p := newCallPipeline(d, ctx, &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: "cancel"})
			p.serverName = "shared-cancel"
			p.method = "tools/call"
			p.target = router.TargetLocal
			p.targetStr = router.TargetLocal.String()
			p.localGeneration = generationID
			p.conn = &pool.Conn{Healthy: true, Transport: &fakeTransport{}}

			if tc.checkRetry {
				if _, retried := p.retryLocalAfterLocalSendFailure(tc.failure, &mcp.Message{ID: "retry"}, time.Now()); retried {
					t.Fatal("caller deadline triggered a shared-generation reconnect")
				}
				if p.localTransportRetryUsed {
					t.Fatal("caller deadline consumed the shared transport retry budget")
				}
			}
			_ = p.transportFailure(tc.stage, tc.failure, time.Now())
			snapshot, ok := core.Snapshot("shared-cancel")
			if !ok || snapshot.Generation != generationID || snapshot.State != generation.StateReady || snapshot.Active != 2 {
				t.Fatalf("generation after caller context end = (%+v, %v), want Ready generation %d with two leases", snapshot, ok, generationID)
			}
			if got := resource.closes.Load(); got != 0 {
				t.Fatalf("shared resource closes after caller context end = %d, want 0", got)
			}
			if lease, leaseErr := core.AcquireLease("shared-cancel", generationID); leaseErr != nil {
				t.Fatalf("replacement caller could not lease surviving generation: %v", leaseErr)
			} else {
				lease.Release()
			}
		})
	}
}

func TestCallPipelineNonMuxRecvTimeoutRetiresObservedGeneration(t *testing.T) {
	resource := &localCheckoutTestResource{}
	core := generation.New(func(context.Context, string, uint64) (generation.Resource, error) {
		return resource, nil
	})
	generationID, _, err := core.Ensure(context.Background(), "nonmux-timeout")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	lease, err := core.AcquireLease("nonmux-timeout", generationID)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	defer func() { _ = core.Shutdown(context.Background()) }()

	d := newCallPipelineTestDaemon()
	d.muxStdio = false
	d.serverSupervisor = &serverSupervisor{core: core}
	p := newCallPipeline(d, context.Background(), &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: "timeout"})
	p.serverName = "nonmux-timeout"
	p.method = "tools/call"
	p.target = router.TargetLocal
	p.targetStr = router.TargetLocal.String()
	p.localGeneration = generationID
	p.conn = &pool.Conn{Healthy: true, Transport: &fakeTransport{}}

	timeoutErr := daemonRPCPhaseError("tools/call", "recv", time.Second, context.DeadlineExceeded)
	_ = p.transportFailure("recv", timeoutErr, time.Now())
	snapshot, ok := core.Snapshot("nonmux-timeout")
	if !ok || snapshot.Generation != generationID || snapshot.State != generation.StateFailed || snapshot.Active != 1 {
		t.Fatalf("generation after non-mux recv timeout = (%+v, %v), want Failed generation %d with one outstanding lease", snapshot, ok, generationID)
	}
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("physical resource closes after non-mux timeout = %d, want 1", got)
	}
	lease.Release()
	if snapshot, ok = core.Snapshot("nonmux-timeout"); !ok || snapshot.State != generation.StateFailed || snapshot.Active != 0 {
		t.Fatalf("generation after timeout lease release = (%+v, %v), want Failed with no leases", snapshot, ok)
	}
}
