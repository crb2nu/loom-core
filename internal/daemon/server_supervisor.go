package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/daemon/generation"
	"github.com/crb2nu/loom/pkg/transport/muxstdio"
)

// localProcessController is the name-keyed process surface owned by the
// generation supervisor. fi-mcp-kit's Manager satisfies this interface; the
// narrow boundary keeps lifecycle interleavings deterministic in tests.
type localProcessController interface {
	Dial(context.Context, string) (mcp.Transport, error)
	Stop(string) error
}

type generationEpochKey struct {
	serverName string
	generation uint64
}

// serverSupervisor adapts the dependency-light generation kernel to loomd's
// local process and stdio topology. The kernel is the lifecycle authority;
// pooled transports are non-owning logical views tagged with the generation
// they observed.
type serverSupervisor struct {
	core      *generation.Supervisor
	logger    *slog.Logger
	proc      localProcessController
	muxStdio  bool
	onClosed  func(string, uint64)
	initMCP   func(context.Context, mcp.Transport) error
	initLimit time.Duration

	registryEpoch    func() uint64
	epochMu          sync.Mutex
	generationEpochs map[generationEpochKey]uint64
	// beforePublish is a deterministic lifecycle seam used only by focused
	// package tests; production leaves it nil.
	beforePublish func(string, uint64)
}

func newServerSupervisor(
	proc localProcessController,
	muxStdio bool,
	logger *slog.Logger,
	onClosed func(string, uint64),
) *serverSupervisor {
	if logger == nil {
		logger = slog.Default()
	}
	s := &serverSupervisor{
		logger:    logger,
		proc:      proc,
		muxStdio:  muxStdio,
		onClosed:  onClosed,
		initMCP:   initializeMCPTransport,
		initLimit: 30 * time.Second,
		registryEpoch: func() uint64 {
			return 1
		},
		generationEpochs: make(map[generationEpochKey]uint64),
	}
	s.core = generation.New(s.build)
	return s
}

func (s *serverSupervisor) build(ctx context.Context, serverName string, generationID uint64) (generation.Resource, error) {
	if s == nil || s.proc == nil {
		return nil, fmt.Errorf("start server %s generation %d: process manager unavailable", serverName, generationID)
	}
	epoch := uint64(1)
	if s.registryEpoch != nil {
		epoch = s.registryEpoch()
	}
	s.rememberGenerationEpoch(serverName, generationID, epoch)
	committed := false
	defer func() {
		if !committed {
			s.forgetGenerationEpoch(serverName, generationID)
		}
	}()

	// Let drain cancellation interrupt a blocked local/SSH dial, then detach the
	// successfully started process from the connector context. fi-mcp-kit uses
	// exec.CommandContext for local children; leaving it attached would SIGKILL a
	// published process before localServerResource.Close can perform its graceful
	// EOF -> TERM -> KILL sequence. The resource owns processCancel after detach.
	processCtx, processCancel := context.WithCancel(context.Background())
	stopConnectorCancel := context.AfterFunc(ctx, processCancel)
	raw, err := s.proc.Dial(processCtx, serverName)
	if err != nil {
		stopConnectorCancel()
		processCancel()
		return nil, err
	}
	stopConnectorCancel()
	resource := &localServerResource{
		serverName: serverName,
		generation: generationID,
		proc:       s.proc,
		raw:        raw,
		onClosed: func(name string, closedGeneration uint64) {
			s.forgetGenerationEpoch(name, closedGeneration)
			if s.onClosed != nil {
				s.onClosed(name, closedGeneration)
			}
		},
		processCancel: processCancel,
	}
	defer func() {
		if !committed {
			_ = resource.Close()
		}
	}()

	initCtx, cancel := context.WithTimeout(ctx, s.initLimit)
	defer cancel()
	if err := s.initMCP(initCtx, raw); err != nil {
		return nil, fmt.Errorf("initialize transport: %w", err)
	}
	if s.muxStdio {
		resource.mux = muxstdio.New(raw)
	}
	committed = true
	return resource, nil
}

func (s *serverSupervisor) rememberGenerationEpoch(serverName string, generationID, epoch uint64) {
	s.epochMu.Lock()
	s.generationEpochs[generationEpochKey{serverName: serverName, generation: generationID}] = epoch
	s.epochMu.Unlock()
}

func (s *serverSupervisor) forgetGenerationEpoch(serverName string, generationID uint64) {
	s.epochMu.Lock()
	delete(s.generationEpochs, generationEpochKey{serverName: serverName, generation: generationID})
	s.epochMu.Unlock()
}

func (s *serverSupervisor) generationsAtOrBefore(epoch uint64) map[string]uint64 {
	result := make(map[string]uint64)
	s.epochMu.Lock()
	for key, observedEpoch := range s.generationEpochs {
		if observedEpoch <= epoch && key.generation > result[key.serverName] {
			result[key.serverName] = key.generation
		}
	}
	s.epochMu.Unlock()
	return result
}

// connection returns one non-owning logical pool view and whether this is the
// first view published for the generation (used for process.start events).
func (s *serverSupervisor) connection(ctx context.Context, serverName string) (mcp.Transport, uint64, bool, error) {
	return s.connectionRegistered(ctx, serverName, nil)
}

// connectionRegistered fences first-publication bookkeeping with the physical
// resource lifecycle. onFirst runs only while this exact generation is still
// Ready and before Close can begin, so an old delayed dial cannot overwrite a
// newer running-generation marker or emit process.start after process.stop.
func (s *serverSupervisor) connectionRegistered(
	ctx context.Context,
	serverName string,
	onFirst func(uint64),
) (mcp.Transport, uint64, bool, error) {
	if s == nil || s.core == nil {
		return nil, 0, false, fmt.Errorf("server supervisor unavailable")
	}
	const maxPublicationAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxPublicationAttempts; attempt++ {
		generationID, resource, err := s.core.Ensure(ctx, serverName)
		if err != nil {
			if !errors.Is(err, generation.ErrRetired) && !errors.Is(err, generation.ErrNotReady) {
				return nil, 0, false, err
			}
			lastErr = err
			if ctx != nil && ctx.Err() != nil {
				return nil, 0, false, ctx.Err()
			}
			continue
		}
		local, ok := resource.(*localServerResource)
		if !ok || local == nil {
			return nil, 0, false, fmt.Errorf("server %s generation %d published unexpected resource %T", serverName, generationID, resource)
		}
		if s.beforePublish != nil {
			s.beforePublish(serverName, generationID)
		}
		transport, publishedGeneration, started, publishErr := s.publishConnection(serverName, generationID, local, onFirst)
		if publishErr == nil {
			return transport, publishedGeneration, started, nil
		}
		if !errors.Is(publishErr, generation.ErrRetired) && !errors.Is(publishErr, generation.ErrNotReady) {
			return nil, 0, false, publishErr
		}
		lastErr = publishErr
		if ctx != nil && ctx.Err() != nil {
			return nil, 0, false, ctx.Err()
		}
	}
	return nil, 0, false, fmt.Errorf("publish current generation for %s after %d attempts: %w", serverName, maxPublicationAttempts, lastErr)
}

func (s *serverSupervisor) publishConnection(
	serverName string,
	generationID uint64,
	local *localServerResource,
	onFirst func(uint64),
) (mcp.Transport, uint64, bool, error) {
	local.lifecycleMu.Lock()
	defer local.lifecycleMu.Unlock()
	if local.closed || !s.currentReady(serverName, generationID) {
		return nil, 0, false, generation.ErrRetired
	}
	started := !local.announced
	if started {
		local.announced = true
		if onFirst != nil {
			onFirst(generationID)
		}
	}
	return local.connection(), generationID, started, nil
}

func (s *serverSupervisor) acquireLease(serverName string, generationID uint64) (*generation.Lease, error) {
	if s == nil || s.core == nil {
		return nil, generation.ErrNotReady
	}
	return s.core.AcquireLease(serverName, generationID)
}

func (s *serverSupervisor) failIfCurrent(serverName string, generationID uint64, cause error) (bool, error) {
	if s == nil || s.core == nil || generationID == 0 {
		return false, nil
	}
	return s.core.FailIfCurrent(serverName, generationID, cause)
}

func (s *serverSupervisor) retireIfIdle(serverName string, generationID uint64, cutoff time.Time) (bool, error) {
	if s == nil || s.core == nil || generationID == 0 {
		return false, nil
	}
	return s.core.RetireIfIdle(serverName, generationID, cutoff)
}

func (s *serverSupervisor) retireCurrent(serverName string) (bool, error) {
	if s == nil || s.core == nil {
		return false, nil
	}
	return s.core.RetireCurrent(serverName)
}

func (s *serverSupervisor) retireGeneration(serverName string, generationID uint64) (bool, error) {
	if s == nil || s.core == nil || generationID == 0 {
		return false, nil
	}
	return s.core.RetireGeneration(serverName, generationID)
}

func (s *serverSupervisor) retireGenerationAsync(serverName string, generationID uint64) (bool, error) {
	if s == nil || s.core == nil || generationID == 0 {
		return false, nil
	}
	return s.core.RetireGenerationAsync(serverName, generationID)
}

func (s *serverSupervisor) current(serverName string) (generation.Snapshot, bool) {
	if s == nil || s.core == nil {
		return generation.Snapshot{}, false
	}
	return s.core.Snapshot(serverName)
}

func (s *serverSupervisor) currentReady(serverName string, generationID uint64) bool {
	snapshot, ok := s.current(serverName)
	return ok && snapshot.Generation == generationID && snapshot.State == generation.StateReady
}

func (s *serverSupervisor) snapshots() []generation.Snapshot {
	if s == nil || s.core == nil {
		return nil
	}
	return s.core.Snapshots()
}

func (s *serverSupervisor) beginDrain() {
	if s != nil && s.core != nil {
		s.core.BeginDrain()
	}
}

func (s *serverSupervisor) shutdown(ctx context.Context) error {
	if s == nil || s.core == nil {
		return nil
	}
	return s.core.Shutdown(ctx)
}

// localServerResource owns the physical local generation. Closing logical
// pool views never closes this resource; only a generation transition does.
type localServerResource struct {
	serverName    string
	generation    uint64
	proc          localProcessController
	raw           mcp.Transport
	mux           *muxstdio.Transport
	onClosed      func(string, uint64)
	processCancel context.CancelFunc

	lifecycleMu sync.Mutex
	announced   bool
	closed      bool
	closeOnce   sync.Once
	closeErr    error
}

func (r *localServerResource) connection() mcp.Transport {
	if r.mux != nil {
		return newPerConnTransport(r.mux, r.generation)
	}
	return &ownedLocalTransport{inner: r.raw, generation: r.generation}
}

func (r *localServerResource) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.lifecycleMu.Lock()
		r.closed = true
		r.lifecycleMu.Unlock()

		var closeErrs []error
		// Cancel the detached exec.CommandContext before any transport or Manager
		// cleanup that can block. Shutdown may time out while a dependency Stop is
		// stuck, but the local child is no longer orphaned; Manager.Stop still gets
		// to perform its EOF/session cleanup for cancellation-insensitive remotes.
		if r.processCancel != nil {
			r.processCancel()
		}
		if r.mux != nil {
			if err := r.mux.Close(); err != nil {
				closeErrs = append(closeErrs, err)
			}
		}
		if r.proc != nil {
			if err := r.proc.Stop(r.serverName); err != nil {
				closeErrs = append(closeErrs, err)
			}
		} else if r.raw != nil {
			if err := r.raw.Close(); err != nil {
				closeErrs = append(closeErrs, err)
			}
		}
		if r.onClosed != nil {
			r.onClosed(r.serverName, r.generation)
		}
		r.closeErr = errors.Join(closeErrs...)
	})
	return r.closeErr
}

// ownedLocalTransport is a non-owning logical view used when stdio muxing is
// disabled. The supervisor still owns the shared physical process transport.
type ownedLocalTransport struct {
	inner      mcp.Transport
	generation uint64
}

func (t *ownedLocalTransport) Send(ctx context.Context, message *mcp.Message) error {
	return t.inner.Send(ctx, message)
}

func (t *ownedLocalTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	return t.inner.Recv(ctx)
}

func (t *ownedLocalTransport) Close() error { return nil }

func (t *ownedLocalTransport) serverGeneration() uint64 {
	if t == nil {
		return 0
	}
	return t.generation
}

type generationTransport interface {
	serverGeneration() uint64
}

func transportGeneration(transport mcp.Transport) (uint64, bool) {
	bound, ok := transport.(generationTransport)
	if !ok || bound == nil {
		return 0, false
	}
	generationID := bound.serverGeneration()
	return generationID, generationID != 0
}

func (d *Daemon) runningLocalServerNames() []string {
	if d == nil {
		return nil
	}
	names := make(map[string]struct{})
	d.runningServers.Range(func(key, _ any) bool {
		if name, ok := key.(string); ok {
			names[name] = struct{}{}
		}
		return true
	})
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func (d *Daemon) readyLocalServerNames() []string {
	if d == nil {
		return nil
	}
	if d.serverSupervisor == nil {
		return d.runningLocalServerNames()
	}
	names := make(map[string]struct{})
	if d.serverSupervisor != nil {
		for _, snapshot := range d.serverSupervisor.snapshots() {
			if snapshot.State != generation.StateReady {
				continue
			}
			names[snapshot.Key] = struct{}{}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func (d *Daemon) currentLocalGenerations() map[string]uint64 {
	result := make(map[string]uint64)
	if d == nil {
		return result
	}
	if d.serverSupervisor != nil {
		for _, snapshot := range d.serverSupervisor.snapshots() {
			if snapshot.State != generation.StateFailed {
				result[snapshot.Key] = snapshot.Generation
			}
		}
		return result
	}
	d.runningServers.Range(func(key, value any) bool {
		name, ok := key.(string)
		if !ok {
			return true
		}
		generationID, _ := value.(uint64)
		result[name] = generationID
		return true
	})
	return result
}
