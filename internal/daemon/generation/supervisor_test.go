package generation

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testResource struct {
	closes atomic.Int64
	err    error
}

func (r *testResource) Close() error {
	r.closes.Add(1)
	return r.err
}

type contextAwareResource struct {
	ctx             context.Context
	closes          atomic.Int64
	canceledAtClose atomic.Bool
}

type blockingResource struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
	closes      atomic.Int64
}

func newBlockingResource() *blockingResource {
	return &blockingResource{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (r *blockingResource) Close() error {
	r.closes.Add(1)
	r.startedOnce.Do(func() { close(r.started) })
	<-r.release
	return nil
}

func (r *blockingResource) unblock() {
	r.releaseOnce.Do(func() { close(r.release) })
}

func (r *contextAwareResource) Close() error {
	r.closes.Add(1)
	if r.ctx.Err() != nil {
		r.canceledAtClose.Store(true)
	}
	return nil
}

func TestEnsureColdConcurrentSingleFactory(t *testing.T) {
	const callers = 64

	var factoryCalls atomic.Int64
	factoryStarted := make(chan struct{})
	allowFactory := make(chan struct{})
	resource := &testResource{}
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		if factoryCalls.Add(1) == 1 {
			close(factoryStarted)
		}
		<-allowFactory
		return resource, nil
	})

	type result struct {
		generation uint64
		resource   Resource
		err        error
	}
	start := make(chan struct{})
	results := make(chan result, callers)
	var callersDone sync.WaitGroup
	callersDone.Add(callers)
	for range callers {
		go func() {
			defer callersDone.Done()
			<-start
			generation, got, err := supervisor.Ensure(context.Background(), "hub")
			results <- result{generation: generation, resource: got, err: err}
		}()
	}
	close(start)
	<-factoryStarted
	close(allowFactory)
	callersDone.Wait()
	close(results)

	for got := range results {
		if got.err != nil {
			t.Fatalf("Ensure() error = %v", got.err)
		}
		if got.generation != 1 {
			t.Errorf("Ensure() generation = %d, want 1", got.generation)
		}
		if got.resource != resource {
			t.Errorf("Ensure() resource = %p, want %p", got.resource, resource)
		}
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("factory calls = %d, want 1", got)
	}

	snapshot, ok := supervisor.Snapshot("hub")
	if !ok {
		t.Fatal("Snapshot() did not find ready generation")
	}
	if snapshot.State != StateReady || snapshot.Generation != 1 || snapshot.Active != 0 || snapshot.LastActivity.IsZero() {
		t.Fatalf("Snapshot() = %+v, want ready generation 1 with no leases and activity", snapshot)
	}
}

func TestEnsureRejectsTypedNilResource(t *testing.T) {
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		var resource *testResource
		return resource, nil
	})

	generation, resource, err := supervisor.Ensure(context.Background(), "hub")
	if !errors.Is(err, ErrNilResource) || generation != 0 || resource != nil {
		t.Fatalf("Ensure(typed nil) = (%d, %v, %v), want (0, nil, ErrNilResource)", generation, resource, err)
	}
	snapshot, ok := supervisor.Snapshot("hub")
	if !ok || snapshot.Generation != 1 || snapshot.State != StateFailed {
		t.Fatalf("Snapshot(typed nil) = (%+v, %v), want failed generation 1", snapshot, ok)
	}
}

func TestDelayedOldFailureAfterReplacementIsNoOp(t *testing.T) {
	var resourcesMu sync.Mutex
	var resources []*testResource
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		resource := &testResource{}
		resourcesMu.Lock()
		resources = append(resources, resource)
		resourcesMu.Unlock()
		return resource, nil
	})

	oldGeneration, oldResourceView, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure(old) error = %v", err)
	}
	oldResource := oldResourceView.(*testResource)
	won, err := supervisor.FailIfCurrent("hub", oldGeneration, errors.New("connection reset"))
	if err != nil || !won {
		t.Fatalf("FailIfCurrent(old) = (%v, %v), want (true, nil)", won, err)
	}
	if got := oldResource.closes.Load(); got != 1 {
		t.Fatalf("old resource closes = %d, want 1", got)
	}

	newGeneration, newResourceView, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure(new) error = %v", err)
	}
	if newGeneration != oldGeneration+1 {
		t.Fatalf("new generation = %d, want %d", newGeneration, oldGeneration+1)
	}
	newResource := newResourceView.(*testResource)

	delayed := make(chan struct{})
	delayedResult := make(chan bool, 1)
	go func() {
		<-delayed
		won, delayedErr := supervisor.FailIfCurrent("hub", oldGeneration, errors.New("delayed old reader"))
		if delayedErr != nil {
			t.Errorf("delayed FailIfCurrent() error = %v", delayedErr)
		}
		delayedResult <- won
	}()
	close(delayed)
	if won := <-delayedResult; won {
		t.Fatal("delayed old failure won against replacement")
	}
	if got := oldResource.closes.Load(); got != 1 {
		t.Fatalf("old resource closes after stale failure = %d, want 1", got)
	}
	if got := newResource.closes.Load(); got != 0 {
		t.Fatalf("new resource closes after stale failure = %d, want 0", got)
	}

	lease, err := supervisor.AcquireLease("hub", newGeneration)
	if err != nil {
		t.Fatalf("AcquireLease(new) error = %v", err)
	}
	if lease.Resource() != newResource || lease.Generation() != newGeneration {
		t.Fatalf("lease = (%p, %d), want (%p, %d)", lease.Resource(), lease.Generation(), newResource, newGeneration)
	}
	if !lease.Release() || lease.Release() {
		t.Fatal("Release() is not idempotent")
	}
}

func TestFailIfCurrentHasSingleCloser(t *testing.T) {
	resource := &testResource{}
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		return resource, nil
	})
	generation, _, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	const contenders = 32
	var winners atomic.Int64
	var calls sync.WaitGroup
	calls.Add(contenders)
	for range contenders {
		go func() {
			defer calls.Done()
			won, failErr := supervisor.FailIfCurrent("hub", generation, errors.New("fault"))
			if failErr != nil {
				t.Errorf("FailIfCurrent() error = %v", failErr)
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
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("resource closes = %d, want 1", got)
	}
}

func TestLeaseReleaseDuringFailureDoesNotStartSecondClose(t *testing.T) {
	resource := newBlockingResource()
	defer resource.unblock()
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		return resource, nil
	})
	generation, _, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	lease, err := supervisor.AcquireLease("hub", generation)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}

	type failResult struct {
		won bool
		err error
	}
	failDone := make(chan failResult, 1)
	go func() {
		won, failErr := supervisor.FailIfCurrent("hub", generation, errors.New("fault"))
		failDone <- failResult{won: won, err: failErr}
	}()
	<-resource.started

	if !lease.Release() {
		t.Fatal("Release() did not release the active lease")
	}
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("resource closes while failure close is blocked = %d, want 1", got)
	}
	resource.unblock()
	result := <-failDone
	if result.err != nil || !result.won {
		t.Fatalf("FailIfCurrent() = (%v, %v), want (true, nil)", result.won, result.err)
	}
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("final resource closes = %d, want 1", got)
	}
}

func TestActiveLeaseBlocksIdleRetirement(t *testing.T) {
	resource := &testResource{}
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		return resource, nil
	})
	generation, _, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	lease, err := supervisor.AcquireLease("hub", generation)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}

	retired, err := supervisor.RetireIfIdle("hub", generation, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("RetireIfIdle(active) error = %v", err)
	}
	if retired {
		t.Fatal("RetireIfIdle(active) retired a leased generation")
	}
	if got := resource.closes.Load(); got != 0 {
		t.Fatalf("resource closes while leased = %d, want 0", got)
	}

	snapshot, ok := supervisor.Snapshot("hub")
	if !ok || snapshot.State != StateReady || snapshot.Active != 1 {
		t.Fatalf("Snapshot(active) = (%+v, %v), want ready with one lease", snapshot, ok)
	}
	if !lease.Release() || lease.Release() {
		t.Fatal("Release() is not idempotent")
	}

	retired, err = supervisor.RetireIfIdle("hub", generation, time.Now().Add(time.Hour))
	if err != nil || !retired {
		t.Fatalf("RetireIfIdle(released) = (%v, %v), want (true, nil)", retired, err)
	}
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("resource closes after retirement = %d, want 1", got)
	}
	if _, ok := supervisor.Snapshot("hub"); ok {
		t.Fatal("retired generation remains in snapshots")
	}
}

func TestRetireCurrentDefersCloseAndShutdownTracksLease(t *testing.T) {
	resource := &testResource{}
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		return resource, nil
	})
	generation, _, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	lease, err := supervisor.AcquireLease("hub", generation)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	retired, err := supervisor.RetireCurrent("hub")
	if err != nil || !retired {
		t.Fatalf("RetireCurrent() = (%v, %v), want (true, nil)", retired, err)
	}
	if got := resource.closes.Load(); got != 0 {
		t.Fatalf("resource closed before lease release: %d", got)
	}
	snapshot, ok := supervisor.Snapshot("hub")
	if !ok || snapshot.State != StateClosing || snapshot.Active != 1 {
		t.Fatalf("Snapshot() = (%+v, %v), want closing with one lease", snapshot, ok)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if shutdownErr := supervisor.Shutdown(ctx); !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("Shutdown(with active lease) error = %v, want deadline exceeded", shutdownErr)
	}
	if !lease.Release() {
		t.Fatal("Release() did not release active lease")
	}
	if shutdownErr := supervisor.Shutdown(context.Background()); shutdownErr != nil {
		t.Fatalf("Shutdown(after release) error = %v", shutdownErr)
	}
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("resource closes after lease release = %d, want 1", got)
	}
}

func TestRetireGenerationStaleNoOpAndMatchingLeaseDrain(t *testing.T) {
	resource := &testResource{}
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		return resource, nil
	})
	generation, _, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	retired, err := supervisor.RetireGeneration("hub", generation+1)
	if err != nil || retired {
		t.Fatalf("RetireGeneration(stale) = (%v, %v), want (false, nil)", retired, err)
	}
	snapshot, ok := supervisor.Snapshot("hub")
	if !ok || snapshot.Generation != generation || snapshot.State != StateReady || snapshot.Active != 0 {
		t.Fatalf("Snapshot(after stale retirement) = (%+v, %v), want unchanged Ready generation", snapshot, ok)
	}
	if got := resource.closes.Load(); got != 0 {
		t.Fatalf("resource closes after stale retirement = %d, want 0", got)
	}

	lease, err := supervisor.AcquireLease("hub", generation)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	retired, err = supervisor.RetireGeneration("hub", generation)
	if err != nil || !retired {
		t.Fatalf("RetireGeneration(current) = (%v, %v), want (true, nil)", retired, err)
	}
	snapshot, ok = supervisor.Snapshot("hub")
	if !ok || snapshot.Generation != generation || snapshot.State != StateClosing || snapshot.Active != 1 {
		t.Fatalf("Snapshot(during matching retirement) = (%+v, %v), want Closing with one lease", snapshot, ok)
	}
	if got := resource.closes.Load(); got != 0 {
		t.Fatalf("resource closes before final lease release = %d, want 0", got)
	}
	if _, err := supervisor.AcquireLease("hub", generation); !errors.Is(err, ErrNotReady) {
		t.Fatalf("AcquireLease(after matching retirement) error = %v, want ErrNotReady", err)
	}

	if !lease.Release() {
		t.Fatal("Release() did not release the matching generation lease")
	}
	if shutdownErr := supervisor.Shutdown(context.Background()); shutdownErr != nil {
		t.Fatalf("Shutdown() error = %v", shutdownErr)
	}
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("resource closes after final lease release = %d, want 1", got)
	}
}

func TestRetireGenerationAsyncReturnsBeforeBlockingCloseAndShutdownJoins(t *testing.T) {
	resource := newBlockingResource()
	defer resource.unblock()
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		return resource, nil
	})
	generation, _, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	retirement := make(chan error, 1)
	go func() {
		retired, retireErr := supervisor.RetireGenerationAsync("hub", generation)
		if retireErr == nil && !retired {
			retireErr = errors.New("generation was not retired")
		}
		retirement <- retireErr
	}()
	select {
	case retireErr := <-retirement:
		if retireErr != nil {
			t.Fatalf("RetireGenerationAsync() error = %v", retireErr)
		}
	case <-time.After(time.Second):
		t.Fatal("RetireGenerationAsync blocked in Resource.Close")
	}
	select {
	case <-resource.started:
	case <-time.After(time.Second):
		t.Fatal("asynchronous resource Close did not start")
	}
	if snapshot, ok := supervisor.Snapshot("hub"); !ok || snapshot.State != StateClosing {
		t.Fatalf("Snapshot() = (%+v, %v), want Closing", snapshot, ok)
	}

	deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelDeadline()
	if shutdownErr := supervisor.Shutdown(deadlineCtx); !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("Shutdown(blocked close) error = %v, want DeadlineExceeded", shutdownErr)
	}
	resource.unblock()
	if shutdownErr := supervisor.Shutdown(context.Background()); shutdownErr != nil {
		t.Fatalf("Shutdown(after release) error = %v", shutdownErr)
	}
}

func TestFinalLeaseReleaseHandsBlockingCloseToTrackedCloser(t *testing.T) {
	resource := newBlockingResource()
	defer resource.unblock()
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		return resource, nil
	})
	generation, _, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	lease, err := supervisor.AcquireLease("hub", generation)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	retired, err := supervisor.RetireCurrent("hub")
	if err != nil || !retired {
		t.Fatalf("RetireCurrent() = (%v, %v), want (true, nil)", retired, err)
	}

	releaseDone := make(chan bool, 1)
	go func() {
		releaseDone <- lease.Release()
	}()
	select {
	case released := <-releaseDone:
		if !released {
			t.Fatal("Release() = false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("Release blocked in Resource.Close")
	}
	select {
	case <-resource.started:
	case <-time.After(time.Second):
		t.Fatal("Release did not hand the resource to a closer")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if shutdownErr := supervisor.Shutdown(ctx); !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("Shutdown(blocked release close) error = %v, want deadline exceeded", shutdownErr)
	}
	snapshot, ok := supervisor.Snapshot("hub")
	if !ok || snapshot.Generation != generation || snapshot.State != StateClosing || snapshot.Active != 0 {
		t.Fatalf("Snapshot(blocked release close) = (%+v, %v), want tracked closing generation", snapshot, ok)
	}

	resource.unblock()
	if shutdownErr := supervisor.Shutdown(context.Background()); shutdownErr != nil {
		t.Fatalf("Shutdown(after close release) error = %v", shutdownErr)
	}
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("resource closes = %d, want 1", got)
	}
}

func TestShutdownDeadlineWhileDrainCloseRemainsTracked(t *testing.T) {
	resource := newBlockingResource()
	defer resource.unblock()
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		return resource, nil
	})
	generation, _, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- supervisor.Shutdown(ctx)
	}()

	select {
	case <-resource.started:
	case <-time.After(time.Second):
		t.Fatal("BeginDrain did not start the tracked resource close")
	}
	select {
	case shutdownErr := <-shutdownDone:
		if !errors.Is(shutdownErr, context.DeadlineExceeded) {
			t.Fatalf("Shutdown(blocked close) error = %v, want deadline exceeded", shutdownErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not honor its deadline while Resource.Close was blocked")
	}

	snapshot, ok := supervisor.Snapshot("hub")
	if !ok || snapshot.Generation != generation || snapshot.State != StateClosing || snapshot.Active != 0 {
		t.Fatalf("Snapshot(blocked close) = (%+v, %v), want tracked closing generation", snapshot, ok)
	}
	resource.unblock()
	if shutdownErr := supervisor.Shutdown(context.Background()); shutdownErr != nil {
		t.Fatalf("Shutdown(after close release) error = %v", shutdownErr)
	}
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("resource closes = %d, want 1", got)
	}
	if _, ok := supervisor.Snapshot("hub"); ok {
		t.Fatal("generation remains after tracked close completed")
	}
}

func TestConnectorReturningDuringShutdownIsRejectedAndClosed(t *testing.T) {
	factoryStarted := make(chan struct{})
	cancellationObserved := make(chan struct{})
	returnLateResource := make(chan struct{})
	lateResource := &testResource{}
	supervisor := New(func(ctx context.Context, _ string, _ uint64) (Resource, error) {
		close(factoryStarted)
		<-ctx.Done()
		close(cancellationObserved)
		<-returnLateResource
		return lateResource, ctx.Err()
	})

	type ensureResult struct {
		generation uint64
		resource   Resource
		err        error
	}
	ensureDone := make(chan ensureResult, 1)
	go func() {
		generation, resource, err := supervisor.Ensure(context.Background(), "hub")
		ensureDone <- ensureResult{generation: generation, resource: resource, err: err}
	}()
	<-factoryStarted

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- supervisor.Shutdown(context.Background())
	}()
	<-cancellationObserved

	snapshot, ok := supervisor.Snapshot("hub")
	if !ok || snapshot.State != StateClosing || snapshot.Generation != 1 {
		t.Fatalf("Snapshot(during shutdown) = (%+v, %v), want closing generation 1", snapshot, ok)
	}
	close(returnLateResource)

	result := <-ensureDone
	if !errors.Is(result.err, ErrDraining) || result.generation != 0 || result.resource != nil {
		t.Fatalf("Ensure(late) = (%d, %v, %v), want (0, nil, ErrDraining)", result.generation, result.resource, result.err)
	}
	if shutdownErr := <-shutdownDone; shutdownErr != nil {
		t.Fatalf("Shutdown() error = %v", shutdownErr)
	}
	if got := lateResource.closes.Load(); got != 1 {
		t.Fatalf("late resource closes = %d, want 1", got)
	}
	if _, _, err := supervisor.Ensure(context.Background(), "hub"); !errors.Is(err, ErrDraining) {
		t.Fatalf("Ensure(after shutdown) error = %v, want ErrDraining", err)
	}
}

func TestCanceledColdCallerDoesNotAbortSharedConnector(t *testing.T) {
	factoryStarted := make(chan struct{})
	connectorCanceled := make(chan struct{})
	allowFactory := make(chan struct{})
	resource := &testResource{}
	supervisor := New(func(ctx context.Context, _ string, _ uint64) (Resource, error) {
		close(factoryStarted)
		select {
		case <-ctx.Done():
			close(connectorCanceled)
			return nil, ctx.Err()
		case <-allowFactory:
			return resource, nil
		}
	})

	ctx, cancelColdCaller := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, _, err := supervisor.Ensure(ctx, "hub")
		firstResult <- err
	}()
	<-factoryStarted

	type ensureResult struct {
		generation uint64
		resource   Resource
		err        error
	}
	secondResult := make(chan ensureResult, 1)
	go func() {
		generation, got, err := supervisor.Ensure(context.Background(), "hub")
		secondResult <- ensureResult{generation: generation, resource: got, err: err}
	}()

	cancelColdCaller()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("cold Ensure(canceled) error = %v, want context.Canceled", err)
	}
	select {
	case <-connectorCanceled:
		t.Fatal("canceling the cold caller canceled the shared connector")
	default:
	}
	select {
	case result := <-secondResult:
		t.Fatalf("second waiter returned before connector publication: %+v", result)
	default:
	}

	close(allowFactory)
	result := <-secondResult
	if result.err != nil || result.generation != 1 || result.resource != resource {
		t.Fatalf("second Ensure() = (%d, %p, %v), want (1, %p, nil)", result.generation, result.resource, result.err, resource)
	}
	if got := resource.closes.Load(); got != 0 {
		t.Fatalf("published resource closes = %d, want 0", got)
	}
	if shutdownErr := supervisor.Shutdown(context.Background()); shutdownErr != nil {
		t.Fatalf("Shutdown() error = %v", shutdownErr)
	}
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("resource closes after Shutdown = %d, want 1", got)
	}
}

func TestFactoryContextLivesThroughReadyAndCancelsBeforeRetirementClose(t *testing.T) {
	connectorContext := make(chan context.Context, 1)
	var resource *contextAwareResource
	supervisor := New(func(ctx context.Context, _ string, _ uint64) (Resource, error) {
		connectorContext <- ctx
		resource = &contextAwareResource{ctx: ctx}
		return resource, nil
	})

	generation, _, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	captured := <-connectorContext
	select {
	case <-captured.Done():
		t.Fatalf("factory context canceled while generation %d is Ready: %v", generation, captured.Err())
	default:
	}

	retired, err := supervisor.RetireCurrent("hub")
	if err != nil || !retired {
		t.Fatalf("RetireCurrent() = (%v, %v), want (true, nil)", retired, err)
	}
	select {
	case <-captured.Done():
	default:
		t.Fatal("factory context remains live after retirement")
	}
	if !errors.Is(captured.Err(), context.Canceled) {
		t.Fatalf("factory context error = %v, want context.Canceled", captured.Err())
	}
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("resource closes = %d, want 1", got)
	}
	if !resource.canceledAtClose.Load() {
		t.Fatal("resource Close ran before the generation context was canceled")
	}
}

func TestRetirementKeepsContextLiveUntilFinalLeaseRelease(t *testing.T) {
	connectorContext := make(chan context.Context, 1)
	var resource *contextAwareResource
	supervisor := New(func(ctx context.Context, _ string, _ uint64) (Resource, error) {
		connectorContext <- ctx
		resource = &contextAwareResource{ctx: ctx}
		return resource, nil
	})

	generation, _, err := supervisor.Ensure(context.Background(), "hub")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	captured := <-connectorContext
	lease, err := supervisor.AcquireLease("hub", generation)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	retired, err := supervisor.RetireCurrent("hub")
	if err != nil || !retired {
		t.Fatalf("RetireCurrent() = (%v, %v), want (true, nil)", retired, err)
	}
	select {
	case <-captured.Done():
		t.Fatalf("generation context canceled while an active lease was draining: %v", captured.Err())
	default:
	}
	if got := resource.closes.Load(); got != 0 {
		t.Fatalf("resource closes before final lease release = %d, want 0", got)
	}

	if !lease.Release() {
		t.Fatal("Release() did not release the final lease")
	}
	select {
	case <-captured.Done():
	default:
		t.Fatal("generation context remains live after final lease release")
	}
	if shutdownErr := supervisor.Shutdown(context.Background()); shutdownErr != nil {
		t.Fatalf("Shutdown() error = %v", shutdownErr)
	}
	if !resource.canceledAtClose.Load() {
		t.Fatal("resource Close ran before context cancellation after final lease release")
	}
	if got := resource.closes.Load(); got != 1 {
		t.Fatalf("resource closes after final lease release = %d, want 1", got)
	}
}

func TestTenThousandFaultCyclesStayWithinLeakBudget(t *testing.T) {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	baselineGoroutines := runtime.NumGoroutine()

	var created atomic.Int64
	supervisor := New(func(context.Context, string, uint64) (Resource, error) {
		created.Add(1)
		return &testResource{}, nil
	})
	for i := 0; i < 10_000; i++ {
		generation, _, err := supervisor.Ensure(context.Background(), "hub")
		if err != nil {
			t.Fatalf("cycle %d Ensure() error = %v", i, err)
		}
		won, err := supervisor.FailIfCurrent("hub", generation, errors.New("injected fault"))
		if err != nil || !won {
			t.Fatalf("cycle %d FailIfCurrent() = (%v, %v), want (true, nil)", i, won, err)
		}
	}
	if got := created.Load(); got != 10_000 {
		t.Fatalf("factory resources = %d, want 10000", got)
	}
	if shutdownErr := supervisor.Shutdown(context.Background()); shutdownErr != nil {
		t.Fatalf("Shutdown() error = %v", shutdownErr)
	}

	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	goroutineGrowth := runtime.NumGoroutine() - baselineGoroutines
	heapGrowth := int64(after.HeapAlloc) - int64(before.HeapAlloc)

	if goroutineGrowth > 5 {
		t.Fatalf("post-GC goroutine growth = %+d, want <= +5", goroutineGrowth)
	}
	if heapGrowth > 5<<20 {
		t.Fatalf("post-GC heap growth = %d bytes, want <= 5 MiB", heapGrowth)
	}
}
