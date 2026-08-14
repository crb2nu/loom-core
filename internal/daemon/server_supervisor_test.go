package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/daemon/generation"
)

type supervisorTestProcessController struct {
	mu         sync.Mutex
	dials      int
	stops      int
	closed     int
	transports map[string][]*supervisorTestTransport
	dialCtxs   map[string][]context.Context
}

func newSupervisorTestProcessController() *supervisorTestProcessController {
	return &supervisorTestProcessController{
		transports: make(map[string][]*supervisorTestTransport),
		dialCtxs:   make(map[string][]context.Context),
	}
}

func (p *supervisorTestProcessController) Dial(ctx context.Context, serverName string) (mcp.Transport, error) {
	transport := &supervisorTestTransport{
		owner: p,
		done:  make(chan struct{}),
	}
	p.mu.Lock()
	p.dials++
	p.transports[serverName] = append(p.transports[serverName], transport)
	p.dialCtxs[serverName] = append(p.dialCtxs[serverName], ctx)
	p.mu.Unlock()
	return transport, nil
}

func (p *supervisorTestProcessController) latestDialContext(serverName string) context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	contexts := p.dialCtxs[serverName]
	if len(contexts) == 0 {
		return nil
	}
	return contexts[len(contexts)-1]
}

func (p *supervisorTestProcessController) Stop(serverName string) error {
	p.mu.Lock()
	p.stops++
	var current *supervisorTestTransport
	for _, transport := range p.transports[serverName] {
		if !transport.isClosed() {
			current = transport
			break
		}
	}
	p.mu.Unlock()
	if current == nil {
		return nil
	}
	return current.Close()
}

func (p *supervisorTestProcessController) noteClosed() {
	p.mu.Lock()
	p.closed++
	p.mu.Unlock()
}

func (p *supervisorTestProcessController) stats() (dials, stops, closed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dials, p.stops, p.closed
}

type supervisorTestTransport struct {
	owner     *supervisorTestProcessController
	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
}

func (t *supervisorTestTransport) Send(ctx context.Context, _ *mcp.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return errors.New("test transport closed")
	default:
		return nil
	}
}

func (t *supervisorTestTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, errors.New("test transport closed")
	}
}

func (t *supervisorTestTransport) Close() error {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		close(t.done)
		t.owner.noteClosed()
	})
	return nil
}

func (t *supervisorTestTransport) isClosed() bool {
	return t.closed.Load()
}

func newTestServerSupervisor(
	processes *supervisorTestProcessController,
	muxStdio bool,
	initialize func(context.Context, mcp.Transport) error,
	onClosed func(string, uint64),
) *serverSupervisor {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	supervisor := newServerSupervisor(processes, muxStdio, logger, onClosed)
	supervisor.initMCP = initialize
	supervisor.initLimit = time.Second
	return supervisor
}

func TestDaemonRunningNamesExcludeConnectingGeneration(t *testing.T) {
	processes := newSupervisorTestProcessController()
	initEntered := make(chan struct{})
	allowInit := make(chan struct{})
	supervisor := newTestServerSupervisor(
		processes,
		false,
		func(context.Context, mcp.Transport) error {
			close(initEntered)
			<-allowInit
			return nil
		},
		nil,
	)
	d := &Daemon{serverSupervisor: supervisor}

	type result struct {
		transport mcp.Transport
		err       error
	}
	connected := make(chan result, 1)
	go func() {
		transport, _, _, err := supervisor.connectionRegistered(context.Background(), "alpha", func(generationID uint64) {
			d.runningServers.Store("alpha", generationID)
		})
		connected <- result{transport: transport, err: err}
	}()

	<-initEntered
	if names := d.runningLocalServerNames(); len(names) != 0 {
		t.Fatalf("running names while Connecting = %v, want none", names)
	}
	if names := d.readyLocalServerNames(); len(names) != 0 {
		t.Fatalf("ready names while Connecting = %v, want none", names)
	}

	close(allowInit)
	connection := <-connected
	if connection.err != nil {
		t.Fatalf("connectionRegistered() error = %v", connection.err)
	}
	if connection.transport == nil {
		t.Fatal("connectionRegistered() returned nil transport")
	}
	if names := d.runningLocalServerNames(); len(names) != 1 || names[0] != "alpha" {
		t.Fatalf("running names after Ready = %v, want [alpha]", names)
	}
	if names := d.readyLocalServerNames(); len(names) != 1 || names[0] != "alpha" {
		t.Fatalf("ready names after Ready = %v, want [alpha]", names)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestServerSupervisorConcurrentColdConnectionUsesSinglePhysicalGeneration(t *testing.T) {
	const callers = 64

	processes := newSupervisorTestProcessController()
	initEntered := make(chan struct{})
	allowInit := make(chan struct{})
	var initEnteredOnce sync.Once
	var initCalls atomic.Int64
	var closedCalls atomic.Int64
	var closedGeneration atomic.Uint64
	supervisor := newTestServerSupervisor(
		processes,
		false,
		func(context.Context, mcp.Transport) error {
			initCalls.Add(1)
			initEnteredOnce.Do(func() { close(initEntered) })
			<-allowInit
			return nil
		},
		func(serverName string, generationID uint64) {
			if serverName != "alpha" {
				t.Errorf("onClosed server = %q, want alpha", serverName)
			}
			closedCalls.Add(1)
			closedGeneration.Store(generationID)
		},
	)

	type connectionResult struct {
		transport  mcp.Transport
		generation uint64
		started    bool
		err        error
	}
	start := make(chan struct{})
	results := make(chan connectionResult, callers)
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(callers)
	done.Add(callers)
	for range callers {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			transport, generationID, started, err := supervisor.connection(context.Background(), "alpha")
			results <- connectionResult{transport: transport, generation: generationID, started: started, err: err}
		}()
	}
	ready.Wait()
	close(start)
	<-initEntered
	close(allowInit)
	done.Wait()
	close(results)

	var physical mcp.Transport
	started := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("connection() error = %v", result.err)
		}
		if result.generation != 1 {
			t.Errorf("connection() generation = %d, want 1", result.generation)
		}
		if result.started {
			started++
		}
		owned, ok := result.transport.(*ownedLocalTransport)
		if !ok {
			t.Fatalf("connection transport = %T, want *ownedLocalTransport", result.transport)
		}
		if observed, ok := transportGeneration(owned); !ok || observed != 1 {
			t.Errorf("transportGeneration() = (%d, %v), want (1, true)", observed, ok)
		}
		if physical == nil {
			physical = owned.inner
		} else if owned.inner != physical {
			t.Errorf("connection() published physical transport %p, want %p", owned.inner, physical)
		}
		if err := owned.Close(); err != nil {
			t.Errorf("logical Close() error = %v", err)
		}
	}

	if started != 1 {
		t.Fatalf("started announcements = %d, want 1", started)
	}
	if got := initCalls.Load(); got != 1 {
		t.Fatalf("initialize calls = %d, want 1", got)
	}
	if dials, stops, closed := processes.stats(); dials != 1 || stops != 0 || closed != 0 {
		t.Fatalf("before shutdown process stats = (%d dials, %d stops, %d closed), want (1, 0, 0)", dials, stops, closed)
	}

	if err := supervisor.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if dials, stops, closed := processes.stats(); dials != 1 || stops != 1 || closed != 1 {
		t.Fatalf("after shutdown process stats = (%d dials, %d stops, %d closed), want (1, 1, 1)", dials, stops, closed)
	}
	if got := closedCalls.Load(); got != 1 {
		t.Fatalf("onClosed calls = %d, want 1", got)
	}
	if got := closedGeneration.Load(); got != 1 {
		t.Fatalf("onClosed generation = %d, want 1", got)
	}
}

func TestServerSupervisorReplacementHasSingleWinnerAndRejectsStaleFailure(t *testing.T) {
	processes := newSupervisorTestProcessController()
	var initCalls atomic.Int64
	supervisor := newTestServerSupervisor(
		processes,
		false,
		func(context.Context, mcp.Transport) error {
			initCalls.Add(1)
			return nil
		},
		nil,
	)

	_, oldGeneration, started, err := supervisor.connection(context.Background(), "beta")
	if err != nil {
		t.Fatalf("connection(old) error = %v", err)
	}
	if oldGeneration != 1 || !started {
		t.Fatalf("connection(old) = generation %d, started %v; want 1, true", oldGeneration, started)
	}

	const contenders = 32
	var winners atomic.Int64
	var calls sync.WaitGroup
	calls.Add(contenders)
	for range contenders {
		go func() {
			defer calls.Done()
			won, failErr := supervisor.failIfCurrent("beta", oldGeneration, errors.New("connection reset"))
			if failErr != nil {
				t.Errorf("failIfCurrent() error = %v", failErr)
			}
			if won {
				winners.Add(1)
			}
		}()
	}
	calls.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("failure winners = %d, want 1", got)
	}
	if dials, stops, closed := processes.stats(); dials != 1 || stops != 1 || closed != 1 {
		t.Fatalf("after failure process stats = (%d dials, %d stops, %d closed), want (1, 1, 1)", dials, stops, closed)
	}

	_, replacementGeneration, replacementStarted, err := supervisor.connection(context.Background(), "beta")
	if err != nil {
		t.Fatalf("connection(replacement) error = %v", err)
	}
	if replacementGeneration != oldGeneration+1 || !replacementStarted {
		t.Fatalf(
			"connection(replacement) = generation %d, started %v; want %d, true",
			replacementGeneration,
			replacementStarted,
			oldGeneration+1,
		)
	}

	won, err := supervisor.failIfCurrent("beta", oldGeneration, errors.New("delayed old reader"))
	if err != nil {
		t.Fatalf("stale failIfCurrent() error = %v", err)
	}
	if won {
		t.Fatal("stale failIfCurrent() retired the replacement")
	}
	if !supervisor.currentReady("beta", replacementGeneration) {
		t.Fatalf("replacement generation %d is not current and ready", replacementGeneration)
	}

	for range 1000 {
		lease, leaseErr := supervisor.acquireLease("beta", replacementGeneration)
		if leaseErr != nil {
			t.Fatalf("acquireLease(replacement) error = %v", leaseErr)
		}
		if lease.Generation() != replacementGeneration {
			t.Fatalf("replacement lease generation = %d, want %d", lease.Generation(), replacementGeneration)
		}
		if !lease.Release() {
			t.Fatal("replacement lease Release() = false, want true")
		}
	}
	if got := initCalls.Load(); got != 2 {
		t.Fatalf("initialize calls = %d, want exactly 2", got)
	}
	if dials, stops, closed := processes.stats(); dials != 2 || stops != 1 || closed != 1 {
		t.Fatalf("before shutdown process stats = (%d dials, %d stops, %d closed), want (2, 1, 1)", dials, stops, closed)
	}

	if err := supervisor.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if dials, stops, closed := processes.stats(); dials != 2 || stops != 2 || closed != 2 {
		t.Fatalf("after shutdown process stats = (%d dials, %d stops, %d closed), want (2, 2, 2)", dials, stops, closed)
	}
}

func TestServerSupervisorDelayedOldPublicationCannotOverwriteReplacement(t *testing.T) {
	processes := newSupervisorTestProcessController()
	supervisor := newTestServerSupervisor(
		processes,
		false,
		func(context.Context, mcp.Transport) error { return nil },
		nil,
	)

	oldGeneration, oldResource, err := supervisor.core.Ensure(context.Background(), "publish-race")
	if err != nil {
		t.Fatalf("Ensure(old) error = %v", err)
	}
	retired, err := supervisor.failIfCurrent("publish-race", oldGeneration, errors.New("replace"))
	if err != nil || !retired {
		t.Fatalf("failIfCurrent(old) = (%v, %v), want (true, nil)", retired, err)
	}

	newGeneration, newResource, err := supervisor.core.Ensure(context.Background(), "publish-race")
	if err != nil {
		t.Fatalf("Ensure(new) error = %v", err)
	}
	var marker atomic.Uint64
	_, _, started, err := supervisor.publishConnection(
		"publish-race",
		newGeneration,
		newResource.(*localServerResource),
		marker.Store,
	)
	if err != nil || !started {
		t.Fatalf("publish replacement = (started %v, err %v), want (true, nil)", started, err)
	}
	if got := marker.Load(); got != newGeneration {
		t.Fatalf("running marker = %d, want replacement %d", got, newGeneration)
	}

	oldCallbackCalled := atomic.Bool{}
	if _, _, _, err := supervisor.publishConnection(
		"publish-race",
		oldGeneration,
		oldResource.(*localServerResource),
		func(generationID uint64) {
			oldCallbackCalled.Store(true)
			marker.Store(generationID)
		},
	); !errors.Is(err, generation.ErrRetired) {
		t.Fatalf("delayed old publish error = %v, want ErrRetired", err)
	}
	if oldCallbackCalled.Load() {
		t.Fatal("delayed old generation executed first-publication callback")
	}
	if got := marker.Load(); got != newGeneration {
		t.Fatalf("delayed old generation overwrote marker with %d; want %d", got, newGeneration)
	}
	if err := supervisor.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestServerSupervisorRetriesRetireBetweenEnsureAndPublication(t *testing.T) {
	processes := newSupervisorTestProcessController()
	supervisor := newTestServerSupervisor(
		processes,
		false,
		func(context.Context, mcp.Transport) error { return nil },
		nil,
	)
	var retireOnce sync.Once
	supervisor.beforePublish = func(serverName string, generationID uint64) {
		retireOnce.Do(func() {
			retired, err := supervisor.failIfCurrent(serverName, generationID, errors.New("publish race"))
			if err != nil || !retired {
				t.Errorf("failIfCurrent during publish = (%v, %v), want (true, nil)", retired, err)
			}
		})
	}

	transport, generationID, started, err := supervisor.connection(context.Background(), "publish-retry")
	if err != nil {
		t.Fatalf("connection() error = %v", err)
	}
	if transport == nil || generationID != 2 || !started {
		t.Fatalf("connection() = (%T, generation %d, started %v), want generation 2 publication", transport, generationID, started)
	}
	if dials, stops, closed := processes.stats(); dials != 2 || stops != 1 || closed != 1 {
		t.Fatalf("replacement stats = (%d dials, %d stops, %d closed), want (2, 1, 1)", dials, stops, closed)
	}
	if err := supervisor.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestServerSupervisorWaitersRetryConnectingGenerationRetirement(t *testing.T) {
	const callers = 16
	processes := newSupervisorTestProcessController()
	firstInitStarted := make(chan struct{})
	var initCalls atomic.Int64
	supervisor := newTestServerSupervisor(
		processes,
		false,
		func(ctx context.Context, _ mcp.Transport) error {
			if initCalls.Add(1) == 1 {
				close(firstInitStarted)
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		},
		nil,
	)

	type result struct {
		generation uint64
		started    bool
		err        error
	}
	results := make(chan result, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			_, generationID, started, err := supervisor.connection(context.Background(), "connecting-retry")
			results <- result{generation: generationID, started: started, err: err}
		}()
	}
	awaitSignal(t, firstInitStarted, "first connecting generation initialization")
	retired, err := supervisor.retireGeneration("connecting-retry", 1)
	if err != nil || !retired {
		t.Fatalf("retireGeneration(connecting) = (%v, %v), want (true, nil)", retired, err)
	}
	wg.Wait()
	close(results)
	startedCount := 0
	for got := range results {
		if got.err != nil {
			t.Fatalf("connection waiter error = %v", got.err)
		}
		if got.generation != 2 {
			t.Fatalf("connection waiter generation = %d, want 2", got.generation)
		}
		if got.started {
			startedCount++
		}
	}
	if startedCount != 1 || initCalls.Load() != 2 {
		t.Fatalf("replacement publication = %d starts, %d init calls; want 1/2", startedCount, initCalls.Load())
	}
	if err := supervisor.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestServerSupervisorRegistryEpochFencePreservesPostReloadReplacement(t *testing.T) {
	processes := newSupervisorTestProcessController()
	supervisor := newTestServerSupervisor(
		processes,
		false,
		func(context.Context, mcp.Transport) error { return nil },
		nil,
	)
	var epoch atomic.Uint64
	epoch.Store(1)
	supervisor.registryEpoch = epoch.Load
	_, oldGeneration, _, err := supervisor.connection(context.Background(), "reload-epoch")
	if err != nil {
		t.Fatalf("connection(old) error = %v", err)
	}

	epoch.Store(2) // registry publication boundary
	observed := supervisor.generationsAtOrBefore(1)
	if observed["reload-epoch"] != oldGeneration {
		t.Fatalf("pre-reload observed generation = %d, want %d", observed["reload-epoch"], oldGeneration)
	}
	failed, err := supervisor.failIfCurrent("reload-epoch", oldGeneration, errors.New("old generation failed during reload"))
	if err != nil || !failed {
		t.Fatalf("fail old generation = (%v, %v), want (true, nil)", failed, err)
	}
	_, replacementGeneration, _, err := supervisor.connection(context.Background(), "reload-epoch")
	if err != nil {
		t.Fatalf("connection(replacement) error = %v", err)
	}
	if replacementGeneration != oldGeneration+1 {
		t.Fatalf("replacement generation = %d, want %d", replacementGeneration, oldGeneration+1)
	}

	retired, err := supervisor.retireGeneration("reload-epoch", observed["reload-epoch"])
	if err != nil {
		t.Fatalf("retire observed pre-reload generation: %v", err)
	}
	if retired {
		t.Fatal("pre-reload generation fence retired the post-reload replacement")
	}
	if !supervisor.currentReady("reload-epoch", replacementGeneration) {
		t.Fatal("post-reload replacement is not current and Ready")
	}
	if err := supervisor.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestServerSupervisorInitPanicClosesUnpublishedProcess(t *testing.T) {
	processes := newSupervisorTestProcessController()
	supervisor := newTestServerSupervisor(
		processes,
		false,
		func(context.Context, mcp.Transport) error {
			panic("init panic")
		},
		nil,
	)

	if _, _, _, err := supervisor.connection(context.Background(), "panic-init"); err == nil || !strings.Contains(err.Error(), "factory panicked") {
		t.Fatalf("connection() error = %v, want recovered factory panic", err)
	}
	if dials, stops, closed := processes.stats(); dials != 1 || stops != 1 || closed != 1 {
		t.Fatalf("panic cleanup stats = (%d dials, %d stops, %d closed), want (1, 1, 1)", dials, stops, closed)
	}
	if err := supervisor.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestServerSupervisorShutdownCancelsConnectingInitialization(t *testing.T) {
	processes := newSupervisorTestProcessController()
	initStarted := make(chan struct{})
	initCanceled := make(chan struct{})
	supervisor := newTestServerSupervisor(
		processes,
		false,
		func(ctx context.Context, _ mcp.Transport) error {
			close(initStarted)
			<-ctx.Done()
			close(initCanceled)
			return ctx.Err()
		},
		nil,
	)

	connectionDone := make(chan error, 1)
	go func() {
		_, _, _, err := supervisor.connection(context.Background(), "connecting")
		connectionDone <- err
	}()
	<-initStarted

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	select {
	case <-initCanceled:
	default:
		t.Fatal("shutdown returned before connector initialization observed cancellation")
	}
	if err := <-connectionDone; !errors.Is(err, generation.ErrDraining) {
		t.Fatalf("connection() error = %v, want ErrDraining", err)
	}
	if dials, stops, closed := processes.stats(); dials != 1 || stops != 1 || closed != 1 {
		t.Fatalf("final process stats = (%d dials, %d stops, %d closed), want (1, 1, 1)", dials, stops, closed)
	}
}

func TestServerSupervisorActiveMuxLeaseBlocksIdleRetirementAndShutdownJoins(t *testing.T) {
	processes := newSupervisorTestProcessController()
	supervisor := newTestServerSupervisor(
		processes,
		true,
		func(context.Context, mcp.Transport) error { return nil },
		nil,
	)

	transport, generationID, _, err := supervisor.connection(context.Background(), "gamma")
	if err != nil {
		t.Fatalf("connection() error = %v", err)
	}
	if _, ok := transport.(*perConnTransport); !ok {
		t.Fatalf("mux connection transport = %T, want *perConnTransport", transport)
	}
	if observed, ok := transportGeneration(transport); !ok || observed != generationID {
		t.Fatalf("transportGeneration() = (%d, %v), want (%d, true)", observed, ok, generationID)
	}
	lease, err := supervisor.acquireLease("gamma", generationID)
	if err != nil {
		t.Fatalf("acquireLease() error = %v", err)
	}
	processCtx := processes.latestDialContext("gamma")
	if processCtx == nil {
		t.Fatal("process Dial context was not recorded")
	}
	select {
	case <-processCtx.Done():
		t.Fatal("published process context was canceled before generation retirement")
	default:
	}

	retired, err := supervisor.retireIfIdle("gamma", generationID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("retireIfIdle(active) error = %v", err)
	}
	if retired {
		t.Fatal("retireIfIdle(active) retired a mux generation with an active lease")
	}
	if dials, stops, closed := processes.stats(); dials != 1 || stops != 0 || closed != 0 {
		t.Fatalf("while leased process stats = (%d dials, %d stops, %d closed), want (1, 0, 0)", dials, stops, closed)
	}
	if !lease.Release() {
		t.Fatal("lease Release() = false, want true")
	}

	retired, err = supervisor.retireIfIdle("gamma", generationID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("retireIfIdle(released) error = %v", err)
	}
	if !retired {
		t.Fatal("retireIfIdle(released) = false, want true")
	}
	select {
	case <-processCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("retired resource did not cancel its detached process context after Close")
	}
	if dials, stops, closed := processes.stats(); dials != 1 || stops != 1 || closed != 1 {
		t.Fatalf("after idle retirement process stats = (%d dials, %d stops, %d closed), want (1, 1, 1)", dials, stops, closed)
	}

	_, replacementGeneration, _, err := supervisor.connection(context.Background(), "gamma")
	if err != nil {
		t.Fatalf("connection(replacement) error = %v", err)
	}
	if replacementGeneration != generationID+1 {
		t.Fatalf("replacement generation = %d, want %d", replacementGeneration, generationID+1)
	}
	replacementLease, err := supervisor.acquireLease("gamma", replacementGeneration)
	if err != nil {
		t.Fatalf("acquireLease(replacement) error = %v", err)
	}
	replacementProcessCtx := processes.latestDialContext("gamma")
	if replacementProcessCtx == nil || replacementProcessCtx == processCtx {
		t.Fatal("replacement process Dial context was not independently recorded")
	}

	shutdownDone := make(chan error, 1)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	go func() {
		shutdownDone <- supervisor.shutdown(shutdownCtx)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		snapshot, ok := supervisor.current("gamma")
		if ok && snapshot.Generation == replacementGeneration && snapshot.State == generation.StateClosing && snapshot.Active == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replacement did not enter closing state with active lease; snapshot = %+v, found = %v", snapshot, ok)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case shutdownErr := <-shutdownDone:
		t.Fatalf("shutdown returned before active lease release: %v", shutdownErr)
	default:
	}
	select {
	case <-replacementProcessCtx.Done():
		t.Fatal("active closing generation canceled its process before final lease release")
	default:
	}
	if !replacementLease.Release() {
		t.Fatal("replacement lease Release() = false, want true")
	}
	select {
	case shutdownErr := <-shutdownDone:
		if shutdownErr != nil {
			t.Fatalf("shutdown() error = %v", shutdownErr)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join deferred generation close")
	}
	select {
	case <-replacementProcessCtx.Done():
	default:
		t.Fatal("shutdown closed the replacement without canceling its process context")
	}

	if dials, stops, closed := processes.stats(); dials != 2 || stops != 2 || closed != 2 {
		t.Fatalf("final process stats = (%d dials, %d stops, %d closed), want (2, 2, 2)", dials, stops, closed)
	}
	if _, _, _, err := supervisor.connection(context.Background(), "gamma"); !errors.Is(err, generation.ErrDraining) {
		t.Fatalf("connection(after shutdown) error = %v, want ErrDraining", err)
	}
}
