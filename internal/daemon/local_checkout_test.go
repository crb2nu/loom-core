package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/daemon/generation"
	"github.com/crb2nu/loom/internal/pool"
)

type localCheckoutTestResource struct {
	closes atomic.Int64
}

func (r *localCheckoutTestResource) Close() error {
	r.closes.Add(1)
	return nil
}

type localCheckoutTestTransport struct {
	generation uint64
	result     json.RawMessage
	sendErr    error
	recvErr    error

	recvEntered chan struct{}
	releaseRecv chan struct{}
	recvOnce    sync.Once
	sends       atomic.Int64
	closes      atomic.Int64
}

func (t *localCheckoutTestTransport) Send(context.Context, *mcp.Message) error {
	t.sends.Add(1)
	return t.sendErr
}

func (t *localCheckoutTestTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	if t.recvEntered != nil {
		t.recvOnce.Do(func() { close(t.recvEntered) })
	}
	if t.releaseRecv != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.releaseRecv:
		}
	}
	if t.recvErr != nil {
		return nil, t.recvErr
	}
	return &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      float64(1),
		Result:  t.result,
	}, nil
}

func TestPooledProbesRetireObservedGenerationOnTransportFailure(t *testing.T) {
	tests := []struct {
		name      string
		transport *localCheckoutTestTransport
		fetch     func(*Daemon, context.Context, string) error
	}{
		{
			name:      "tools recv EOF",
			transport: &localCheckoutTestTransport{recvErr: io.EOF},
			fetch: func(d *Daemon, ctx context.Context, serverName string) error {
				_, err := d.fetchServerToolsViaPool(ctx, serverName)
				return err
			},
		},
		{
			name:      "resources send failure",
			transport: &localCheckoutTestTransport{sendErr: errors.New("broken pipe")},
			fetch: func(d *Daemon, ctx context.Context, serverName string) error {
				_, err := d.fetchServerResources(ctx, serverName)
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resource := &localCheckoutTestResource{}
			core := generation.New(func(context.Context, string, uint64) (generation.Resource, error) {
				return resource, nil
			})
			generationID, _, err := core.Ensure(context.Background(), "probe-failure")
			if err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			tc.transport.generation = generationID

			d := newCallPipelineTestDaemon()
			d.muxStdio = true
			d.serverSupervisor = &serverSupervisor{core: core}
			d.pool = pool.New(pool.Config{
				MaxIdle:     1,
				MaxOpen:     1,
				IdleTimeout: time.Minute,
				DialFunc: func(context.Context, string) (mcp.Transport, error) {
					return tc.transport, nil
				},
			})
			defer func() {
				_ = d.pool.Close()
				_ = core.Shutdown(context.Background())
			}()

			if fetchErr := tc.fetch(d, context.Background(), "probe-failure"); fetchErr == nil {
				t.Fatal("probe transport failure returned nil error")
			}
			deadline := time.Now().Add(time.Second)
			for resource.closes.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := resource.closes.Load(); got != 1 {
				t.Fatalf("physical resource closes after probe failure = %d, want 1", got)
			}
			snapshot, ok := core.Snapshot("probe-failure")
			if !ok || snapshot.Generation != generationID || snapshot.State != generation.StateFailed || snapshot.Active != 0 {
				t.Fatalf("generation after probe failure = (%+v, %v), want Failed generation %d with no leases", snapshot, ok, generationID)
			}
		})
	}
}

func (t *localCheckoutTestTransport) Close() error {
	t.closes.Add(1)
	return nil
}

func (t *localCheckoutTestTransport) serverGeneration() uint64 {
	return t.generation
}

func TestFetchServerToolsViaPool_ActiveGenerationLeaseBlocksIdleRetirement(t *testing.T) {
	resource := &localCheckoutTestResource{}
	core := generation.New(func(context.Context, string, uint64) (generation.Resource, error) {
		return resource, nil
	})
	generationID, _, err := core.Ensure(context.Background(), "lease_fetch")
	if err != nil {
		t.Fatalf("ensure generation: %v", err)
	}

	transport := &localCheckoutTestTransport{
		generation:  generationID,
		result:      json.RawMessage(`{"tools":[]}`),
		recvEntered: make(chan struct{}),
		releaseRecv: make(chan struct{}),
	}
	d := newCallPipelineTestDaemon()
	d.serverSupervisor = &serverSupervisor{core: core}
	d.pool = pool.New(pool.Config{
		MaxIdle:     1,
		MaxOpen:     1,
		IdleTimeout: time.Minute,
		DialFunc: func(context.Context, string) (mcp.Transport, error) {
			return transport, nil
		},
	})
	t.Cleanup(func() {
		_ = d.pool.Close()
		_ = core.Shutdown(context.Background())
	})

	type fetchResult struct {
		tools []mcp.Tool
		err   error
	}
	done := make(chan fetchResult, 1)
	go func() {
		tools, fetchErr := d.fetchServerToolsViaPool(context.Background(), "lease_fetch")
		done <- fetchResult{tools: tools, err: fetchErr}
	}()

	select {
	case <-transport.recvEntered:
	case <-time.After(time.Second):
		t.Fatal("tools/list fetch did not enter Recv")
	}

	snapshot, ok := core.Snapshot("lease_fetch")
	if !ok || snapshot.Active != 1 {
		t.Fatalf("active fetch snapshot = (%+v, %v), want one active lease", snapshot, ok)
	}
	retired, err := core.RetireIfIdle("lease_fetch", generationID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("retire active generation: %v", err)
	}
	if retired {
		t.Fatal("idle retirement closed a generation with an active tools/list checkout")
	}
	if got := resource.closes.Load(); got != 0 {
		t.Fatalf("resource closes while fetch active = %d, want 0", got)
	}

	close(transport.releaseRecv)
	var result fetchResult
	select {
	case result = <-done:
	case <-time.After(time.Second):
		t.Fatal("tools/list fetch did not finish after Recv was released")
	}
	if result.err != nil {
		t.Fatalf("fetchServerToolsViaPool: %v", result.err)
	}
	if len(result.tools) != 0 {
		t.Fatalf("tools = %v, want empty list", result.tools)
	}
	snapshot, ok = core.Snapshot("lease_fetch")
	if !ok || snapshot.Active != 0 {
		t.Fatalf("completed fetch snapshot = (%+v, %v), want zero active leases", snapshot, ok)
	}
}

func TestFetchServerResources_RejectsStaleGenerationView(t *testing.T) {
	core := generation.New(func(context.Context, string, uint64) (generation.Resource, error) {
		return &localCheckoutTestResource{}, nil
	})
	oldGeneration, _, err := core.Ensure(context.Background(), "stale_fetch")
	if err != nil {
		t.Fatalf("ensure old generation: %v", err)
	}

	stale := &localCheckoutTestTransport{
		generation: oldGeneration,
		result:     json.RawMessage(`{"resources":[]}`),
	}
	fresh := &localCheckoutTestTransport{
		result: json.RawMessage(`{"resources":[{"uri":"test://fresh","name":"fresh"}]}`),
	}
	var dials atomic.Int64
	d := newCallPipelineTestDaemon()
	d.serverSupervisor = &serverSupervisor{core: core}
	d.pool = pool.New(pool.Config{
		MaxIdle:     1,
		MaxOpen:     1,
		IdleTimeout: time.Minute,
		DialFunc: func(context.Context, string) (mcp.Transport, error) {
			if dials.Add(1) == 1 {
				return stale, nil
			}
			return fresh, nil
		},
	})
	t.Cleanup(func() {
		_ = d.pool.Close()
		_ = core.Shutdown(context.Background())
	})

	// Seed the pool with a logical view of generation 1, then replace the
	// physical generation while that non-owning view remains idle.
	seed, err := d.pool.Get(context.Background(), "stale_fetch")
	if err != nil {
		t.Fatalf("seed stale pool view: %v", err)
	}
	d.pool.Put(seed)
	retired, err := core.FailIfCurrent("stale_fetch", oldGeneration, context.Canceled)
	if err != nil {
		t.Fatalf("fail old generation: %v", err)
	}
	if !retired {
		t.Fatal("old generation was not retired")
	}
	newGeneration, _, err := core.Ensure(context.Background(), "stale_fetch")
	if err != nil {
		t.Fatalf("ensure replacement generation: %v", err)
	}
	fresh.generation = newGeneration

	fetchCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resources, err := d.fetchServerResources(fetchCtx, "stale_fetch")
	if err != nil {
		t.Fatalf("fetchServerResources: %v", err)
	}
	if len(resources) != 1 || resources[0].URI != "test://fresh" {
		t.Fatalf("resources = %+v, want fresh generation response", resources)
	}
	if got := stale.sends.Load(); got != 0 {
		t.Fatalf("stale transport sends = %d, want 0", got)
	}
	if got := stale.closes.Load(); got != 1 {
		t.Fatalf("stale transport closes = %d, want 1", got)
	}
	if got := fresh.sends.Load(); got != 1 {
		t.Fatalf("fresh transport sends = %d, want 1", got)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("pool dials = %d, want stale seed plus one replacement", got)
	}
	snapshot, ok := core.Snapshot("stale_fetch")
	if !ok || snapshot.Generation != newGeneration || snapshot.Active != 0 || snapshot.State != generation.StateReady {
		t.Fatalf("replacement snapshot = (%+v, %v), want ready generation %d with no leases", snapshot, ok, newGeneration)
	}
}
