// Package generation owns replaceable resource generations.
//
// A Supervisor serializes publication per key, lends the current generation to
// callers, and accounts for connection and close work during shutdown. It starts
// at most one tracked connector goroutine per connecting generation and one
// tracked closer goroutine per idle resource drained by BeginDrain or detached
// by a final lease release. Waiters do not require helper goroutines.
package generation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrDraining is returned after BeginDrain prevents new generations.
	ErrDraining = errors.New("generation supervisor is draining")
	// ErrNotReady is returned when a key has no current ready generation.
	ErrNotReady = errors.New("generation is not ready")
	// ErrGenerationMismatch is returned when a lease names a stale generation.
	ErrGenerationMismatch = errors.New("generation does not match current")
	// ErrRetired is returned when a connecting generation is explicitly retired.
	ErrRetired = errors.New("generation retired")
	// ErrNilResource is returned when a factory succeeds without a resource.
	ErrNilResource = errors.New("generation factory returned a nil resource")
	// ErrGenerationExhausted is returned rather than wrapping a generation ID.
	ErrGenerationExhausted = errors.New("generation counter exhausted")
	// ErrNilFactory is returned when a Supervisor was constructed without a factory.
	ErrNilFactory = errors.New("generation factory is nil")
)

// Resource is the lifetime owned by one published generation.
type Resource interface {
	Close() error
}

// Factory constructs a resource for key and generation. The supervisor owns ctx:
// caller cancellation does not affect it, and it remains live for the entire
// Ready generation. Failure, retirement, or drain cancels ctx before Close. A
// resource returned after retirement or drain is rejected and closed.
type Factory func(ctx context.Context, key string, generation uint64) (Resource, error)

// State is the externally visible lifecycle state of a generation.
type State string

const (
	StateConnecting State = "connecting"
	StateReady      State = "ready"
	StateClosing    State = "closing"
	StateFailed     State = "failed"
)

// Snapshot is a point-in-time view of a key's current generation.
type Snapshot struct {
	Key          string
	Generation   uint64
	State        State
	Active       uint64
	LastActivity time.Time
}

type closeDisposition uint8

const (
	closeAndRemove closeDisposition = iota
	closeAndFail
)

type entry struct {
	key          string
	generation   uint64
	state        State
	resource     Resource
	active       uint64
	lastActivity time.Time
	err          error
	cancel       context.CancelFunc
	wait         chan struct{}

	closeDisposition closeDisposition
	closeTracked     bool
	closeStarted     bool
}

// Supervisor owns the current resource generation for each key.
type Supervisor struct {
	mu sync.Mutex

	factory Factory
	entries map[string]*entry
	next    map[string]uint64
	now     func() time.Time

	draining  bool
	closeErrs []error
	// closeErrOverflow bounds retained failure history across long-running
	// replacement loops. Immediate operations still return their close error.
	closeErrOverflow uint64
	work             workTracker
}

const maxRecordedCloseErrors = 32

// New constructs a Supervisor. A nil factory is reported by Ensure as
// ErrNilFactory so construction can remain allocation-only.
func New(factory Factory) *Supervisor {
	return &Supervisor{
		factory: factory,
		entries: make(map[string]*entry),
		next:    make(map[string]uint64),
		now:     time.Now,
		work:    newWorkTracker(),
	}
}

// Ensure returns the current ready generation and its non-owning resource view.
// On a cold key, exactly one caller runs the factory and concurrent callers wait
// for that result. Callers must use AcquireLease to guard actual resource use.
func (s *Supervisor) Ensure(ctx context.Context, key string) (uint64, Resource, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	for {
		s.mu.Lock()
		if s.draining {
			s.mu.Unlock()
			return 0, nil, ErrDraining
		}
		if s.factory == nil {
			s.mu.Unlock()
			return 0, nil, ErrNilFactory
		}

		current := s.entries[key]
		if current != nil {
			switch current.state {
			case StateReady:
				generation, resource := current.generation, current.resource
				s.mu.Unlock()
				return generation, resource, nil
			case StateConnecting:
				wait := current.wait
				s.mu.Unlock()
				return s.awaitConnection(ctx, key, current, wait)
			case StateClosing:
				wait := current.wait
				s.mu.Unlock()
				if err := waitFor(ctx, wait); err != nil {
					return 0, nil, err
				}
				continue
			case StateFailed:
				// A later Ensure is the explicit retry boundary and publishes the
				// next monotonically increasing generation below.
			default:
				s.mu.Unlock()
				return 0, nil, fmt.Errorf("unknown generation state %q", current.state)
			}
		}

		generation, err := s.nextGenerationLocked(key)
		if err != nil {
			s.mu.Unlock()
			return 0, nil, err
		}
		factoryCtx, cancel := context.WithCancel(context.Background())
		candidate := &entry{
			key:          key,
			generation:   generation,
			state:        StateConnecting,
			lastActivity: s.now(),
			cancel:       cancel,
			wait:         make(chan struct{}),
		}
		s.entries[key] = candidate
		s.work.start()
		wait := candidate.wait
		s.mu.Unlock()

		go s.runFactory(candidate, factoryCtx, cancel)
		return s.awaitConnection(ctx, key, candidate, wait)
	}
}

func (s *Supervisor) awaitConnection(ctx context.Context, key string, candidate *entry, wait <-chan struct{}) (uint64, Resource, error) {
	if err := waitFor(ctx, wait); err != nil {
		return 0, nil, err
	}

	s.mu.Lock()
	isCurrent := s.entries[key] == candidate
	state := candidate.state
	resource := candidate.resource
	generation := candidate.generation
	connectErr := candidate.err
	draining := s.draining
	s.mu.Unlock()

	if draining {
		return 0, nil, ErrDraining
	}
	if isCurrent && state == StateReady {
		return generation, resource, nil
	}
	if connectErr != nil {
		return 0, nil, connectErr
	}
	return 0, nil, ErrNotReady
}

func (s *Supervisor) runFactory(candidate *entry, ctx context.Context, cancel context.CancelFunc) {
	defer s.work.done()
	resource, factoryErr := callFactory(s.factory, ctx, candidate.key, candidate.generation)
	if isNilResource(resource) && factoryErr == nil {
		factoryErr = ErrNilResource
	}
	s.publishFactoryResult(candidate, resource, factoryErr, cancel)
}

func (s *Supervisor) publishFactoryResult(candidate *entry, resource Resource, factoryErr error, cancel context.CancelFunc) {
	s.mu.Lock()
	isCurrent := s.entries[candidate.key] == candidate
	lifecycleErr := error(nil)
	if candidate.state == StateClosing {
		lifecycleErr = candidate.err
	}
	accepted := isCurrent && candidate.state == StateConnecting && !s.draining && factoryErr == nil
	if accepted {
		candidate.state = StateReady
		candidate.resource = resource
		candidate.lastActivity = s.now()
		signalLocked(candidate)
		s.mu.Unlock()
		return
	}

	if isCurrent && candidate.state == StateConnecting {
		candidate.err = factoryErr
		candidate.lastActivity = s.now()
	}
	// Every non-published factory result is terminal. Cancel before closing a
	// returned resource or waking failure waiters.
	cancel()
	candidate.cancel = nil

	rejectionErr := errors.Join(lifecycleErr, factoryErr)
	if rejectionErr == nil {
		if s.draining {
			rejectionErr = ErrDraining
		} else {
			rejectionErr = ErrRetired
		}
	}

	closeLate := resource != nil
	if closeLate {
		s.work.start()
	}
	s.mu.Unlock()

	var closeErr error
	if closeLate {
		closeErr = safeClose(resource)
		s.recordCloseResult(candidate, closeErr)
		s.work.done()
	}

	s.mu.Lock()
	if s.entries[candidate.key] == candidate {
		switch candidate.state {
		case StateConnecting:
			candidate.state = StateFailed
			candidate.err = errors.Join(rejectionErr, closeErr)
			signalLocked(candidate)
		case StateClosing:
			signalLocked(candidate)
			delete(s.entries, candidate.key)
		}
	}
	s.mu.Unlock()
}

// AcquireLease checks out the current ready generation. A generation token from
// Ensure prevents a delayed caller from acquiring a replacement accidentally.
func (s *Supervisor) AcquireLease(key string, generation uint64) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.draining {
		return nil, ErrDraining
	}
	current := s.entries[key]
	if current == nil {
		return nil, ErrNotReady
	}
	if current.generation != generation {
		return nil, ErrGenerationMismatch
	}
	if current.state != StateReady || current.resource == nil {
		return nil, ErrNotReady
	}

	current.active++
	current.lastActivity = s.now()
	return &Lease{
		supervisor: s,
		entry:      current,
		resource:   current.resource,
		generation: generation,
	}, nil
}

// Lease guards active use of a ready generation.
type Lease struct {
	supervisor *Supervisor
	entry      *entry
	resource   Resource
	generation uint64
	released   atomic.Bool
}

// Resource returns the leased resource.
func (l *Lease) Resource() Resource {
	if l == nil {
		return nil
	}
	return l.resource
}

// Generation returns the leased generation token.
func (l *Lease) Generation() uint64 {
	if l == nil {
		return 0
	}
	return l.generation
}

// Release returns a lease once. It reports whether this call performed the
// release; subsequent calls are harmless and return false. When this is the
// final lease of a Closing generation, Release detaches the resource and hands
// it to a tracked closer so callers never block in Resource.Close.
func (l *Lease) Release() bool {
	if l == nil || l.supervisor == nil || !l.released.CompareAndSwap(false, true) {
		return false
	}

	s := l.supervisor
	var closeNow *entry
	var closeResource Resource
	s.mu.Lock()
	if l.entry.active > 0 {
		l.entry.active--
		l.entry.lastActivity = s.now()
	}
	if s.entries[l.entry.key] == l.entry && l.entry.state == StateClosing && l.entry.active == 0 && l.entry.closeTracked && !l.entry.closeStarted {
		closeNow = l.entry
		closeResource = l.entry.resource
		l.entry.resource = nil
		l.entry.closeStarted = true
		s.cancelGenerationLocked(l.entry)
	}
	s.mu.Unlock()

	if closeNow != nil {
		go s.finishClose(closeNow, closeResource)
	}
	return true
}

// FailIfCurrent atomically fails generation only when it is still the current
// ready generation. Exactly one caller wins and closes the resource. Failures
// for stale or already-transitioned generations are no-ops.
func (s *Supervisor) FailIfCurrent(key string, generation uint64, cause error) (bool, error) {
	if cause == nil {
		cause = ErrNotReady
	}

	s.mu.Lock()
	current := s.entries[key]
	if current == nil || current.generation != generation || current.state != StateReady {
		s.mu.Unlock()
		return false, nil
	}
	resource := s.beginCloseLocked(current, closeAndFail, cause, true)
	s.mu.Unlock()

	return true, s.finishClose(current, resource)
}

// RetireIfIdle retires generation only when it is still current and ready, has
// no leases, and its last activity is strictly before cutoff.
func (s *Supervisor) RetireIfIdle(key string, generation uint64, cutoff time.Time) (bool, error) {
	s.mu.Lock()
	current := s.entries[key]
	if current == nil || current.generation != generation || current.state != StateReady || current.active != 0 || !current.lastActivity.Before(cutoff) {
		s.mu.Unlock()
		return false, nil
	}
	resource := s.beginCloseLocked(current, closeAndRemove, ErrRetired, true)
	s.mu.Unlock()

	return true, s.finishClose(current, resource)
}

// RetireCurrent prevents new leases for key and retires its current generation.
// A ready generation with active leases closes when its final lease is released.
func (s *Supervisor) RetireCurrent(key string) (bool, error) {
	return s.retire(key, 0, false, false)
}

// RetireGeneration gracefully retires generation only when it is still the
// current generation for key. A stale generation is a no-op. Like
// RetireCurrent, active leases defer context cancellation and Close until their
// final Release.
func (s *Supervisor) RetireGeneration(key string, generation uint64) (bool, error) {
	return s.retire(key, generation, true, false)
}

// RetireGenerationAsync starts a tracked close for the exact current
// generation and returns after the transition to Closing. Shutdown still joins
// the close. This is useful for configuration reloads, where one wedged
// resource must not delay publication for unrelated keys.
func (s *Supervisor) RetireGenerationAsync(key string, generation uint64) (bool, error) {
	return s.retire(key, generation, true, true)
}

func (s *Supervisor) retire(key string, generation uint64, requireGeneration, asyncClose bool) (bool, error) {
	s.mu.Lock()
	current := s.entries[key]
	if current == nil || requireGeneration && current.generation != generation {
		s.mu.Unlock()
		return false, nil
	}

	switch current.state {
	case StateConnecting:
		s.beginConnectingRetirementLocked(current, ErrRetired)
		s.mu.Unlock()
		return true, nil
	case StateReady:
		closeImmediately := current.active == 0
		resource := s.beginCloseLocked(current, closeAndRemove, ErrRetired, closeImmediately)
		s.mu.Unlock()
		if !closeImmediately {
			return true, nil
		}
		if asyncClose {
			go s.finishClose(current, resource)
			return true, nil
		}
		return true, s.finishClose(current, resource)
	case StateFailed:
		if current.cancel != nil {
			current.cancel()
			current.cancel = nil
		}
		delete(s.entries, key)
		s.mu.Unlock()
		return true, nil
	case StateClosing:
		s.mu.Unlock()
		return false, nil
	default:
		state := current.state
		s.mu.Unlock()
		return false, fmt.Errorf("unknown generation state %q", state)
	}
}

// BeginDrain rejects new generations and leases, cancels connectors, and starts
// retirement of every ready resource. It is safe to call more than once.
func (s *Supervisor) BeginDrain() {
	type pendingClose struct {
		entry    *entry
		resource Resource
	}
	var closeNow []pendingClose

	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return
	}
	s.draining = true
	for key, current := range s.entries {
		switch current.state {
		case StateConnecting:
			s.beginConnectingRetirementLocked(current, ErrDraining)
		case StateReady:
			closeImmediately := current.active == 0
			resource := s.beginCloseLocked(current, closeAndRemove, ErrDraining, closeImmediately)
			if closeImmediately {
				closeNow = append(closeNow, pendingClose{entry: current, resource: resource})
			}
		case StateFailed:
			if current.cancel != nil {
				current.cancel()
				current.cancel = nil
			}
			delete(s.entries, key)
		case StateClosing:
			// Already accounted by its connector or close transition.
		}
	}
	s.mu.Unlock()

	for _, pending := range closeNow {
		go s.finishClose(pending.entry, pending.resource)
	}
}

// Shutdown begins drain and waits for all tracked connectors, immediate closes,
// and lease-deferred closes. It does not create a waiter goroutine.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.BeginDrain()
	waitErr := s.work.wait(ctx)

	s.mu.Lock()
	closeErrs := append([]error(nil), s.closeErrs...)
	if s.closeErrOverflow > 0 {
		closeErrs = append(closeErrs, fmt.Errorf("%d additional resource close errors omitted", s.closeErrOverflow))
	}
	s.mu.Unlock()
	return errors.Join(append([]error{waitErr}, closeErrs...)...)
}

// Snapshot returns a point-in-time view of key.
func (s *Supervisor) Snapshot(key string) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.entries[key]
	if current == nil {
		return Snapshot{}, false
	}
	return snapshotOf(current), true
}

// Snapshots returns point-in-time views sorted by key.
func (s *Supervisor) Snapshots() []Snapshot {
	s.mu.Lock()
	result := make([]Snapshot, 0, len(s.entries))
	for _, current := range s.entries {
		result = append(result, snapshotOf(current))
	}
	s.mu.Unlock()

	sort.Slice(result, func(i, j int) bool {
		return result[i].Key < result[j].Key
	})
	return result
}

func snapshotOf(current *entry) Snapshot {
	return Snapshot{
		Key:          current.key,
		Generation:   current.generation,
		State:        current.state,
		Active:       current.active,
		LastActivity: current.lastActivity,
	}
}

func (s *Supervisor) nextGenerationLocked(key string) (uint64, error) {
	last := s.next[key]
	if last == math.MaxUint64 {
		return 0, ErrGenerationExhausted
	}
	next := last + 1
	s.next[key] = next
	return next, nil
}

func (s *Supervisor) beginConnectingRetirementLocked(current *entry, reason error) {
	signalLocked(current)
	current.wait = make(chan struct{})
	current.state = StateClosing
	current.err = reason
	current.closeDisposition = closeAndRemove
	if current.cancel != nil {
		current.cancel()
		current.cancel = nil
	}
}

// beginCloseLocked accounts the entire close lifecycle before making the
// transition visible. When takeResource is false, the final lease owns the call
// to finishClose.
func (s *Supervisor) beginCloseLocked(current *entry, disposition closeDisposition, reason error, takeResource bool) Resource {
	current.state = StateClosing
	current.err = reason
	current.closeDisposition = disposition
	current.wait = make(chan struct{})
	current.closeTracked = true
	current.closeStarted = takeResource
	s.work.start()

	if !takeResource {
		return nil
	}
	s.cancelGenerationLocked(current)
	resource := current.resource
	current.resource = nil
	return resource
}

func (s *Supervisor) cancelGenerationLocked(current *entry) {
	if current.cancel == nil {
		return
	}
	current.cancel()
	current.cancel = nil
}

func (s *Supervisor) finishClose(current *entry, resource Resource) error {
	closeErr := safeClose(resource)

	s.mu.Lock()
	if s.entries[current.key] == current && current.state == StateClosing {
		switch current.closeDisposition {
		case closeAndFail:
			current.state = StateFailed
			current.closeTracked = false
			signalLocked(current)
		case closeAndRemove:
			signalLocked(current)
			delete(s.entries, current.key)
		}
	}
	if closeErr != nil {
		s.recordCloseErrorLocked(contextualCloseError(current, closeErr))
	}
	s.mu.Unlock()
	s.work.done()
	return closeErr
}

func (s *Supervisor) recordCloseResult(current *entry, closeErr error) {
	if closeErr == nil {
		return
	}
	s.mu.Lock()
	s.recordCloseErrorLocked(contextualCloseError(current, closeErr))
	s.mu.Unlock()
}

func (s *Supervisor) recordCloseErrorLocked(err error) {
	if len(s.closeErrs) < maxRecordedCloseErrors {
		s.closeErrs = append(s.closeErrs, err)
		return
	}
	s.closeErrOverflow++
}

func contextualCloseError(current *entry, err error) error {
	return fmt.Errorf("close %q generation %d: %w", current.key, current.generation, err)
}

func signalLocked(current *entry) {
	if current.wait == nil {
		return
	}
	close(current.wait)
	current.wait = nil
}

func waitFor(ctx context.Context, wait <-chan struct{}) error {
	if wait == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wait:
		return nil
	}
}

func callFactory(factory Factory, ctx context.Context, key string, generation uint64) (resource Resource, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("generation factory panicked: %v", recovered)
		}
	}()
	return factory(ctx, key, generation)
}

func safeClose(resource Resource) (err error) {
	if isNilResource(resource) {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("resource close panicked: %v", recovered)
		}
	}()
	return resource.Close()
}

func isNilResource(resource Resource) bool {
	if resource == nil {
		return true
	}
	value := reflect.ValueOf(resource)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// workTracker is a reusable context-aware wait group that never starts a
// waiter goroutine. Its channel is replaced only at the zero-to-one edge.
type workTracker struct {
	mu    sync.Mutex
	count uint64
	zero  chan struct{}
}

func newWorkTracker() workTracker {
	zero := make(chan struct{})
	close(zero)
	return workTracker{zero: zero}
}

func (t *workTracker) start() {
	t.mu.Lock()
	if t.count == 0 {
		t.zero = make(chan struct{})
	}
	t.count++
	t.mu.Unlock()
}

func (t *workTracker) done() {
	t.mu.Lock()
	if t.count == 0 {
		t.mu.Unlock()
		panic("generation: work tracker underflow")
	}
	t.count--
	if t.count == 0 {
		close(t.zero)
	}
	t.mu.Unlock()
}

func (t *workTracker) wait(ctx context.Context) error {
	t.mu.Lock()
	zero := t.zero
	t.mu.Unlock()
	// Completed work wins over an already-canceled wait context.
	select {
	case <-zero:
		return nil
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-zero:
		return nil
	}
}
