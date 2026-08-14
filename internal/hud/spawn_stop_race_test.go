package hud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

type stopRaceBackend struct {
	mu sync.Mutex

	buildEntered chan struct{}
	releaseBuild chan struct{}
	startEntered chan struct{}
	releaseStart chan struct{}
	execEntered  chan struct{}
	releaseExec  chan struct{}

	buildCalls int
	startCalls int
	stopCalls  []string
	startDone  bool
	startID    string
	stopErr    error
	// failStopAfterStart models a pod that did not exist for StopSpawn's
	// first delete but whose late Start result cannot then be cleaned up.
	failStopAfterStart bool
}

type failingStopStore struct {
	err  error
	fail bool
}

type cleanupCountingCaller struct {
	// toolCalls is incremented from concurrent monitor goroutines
	// (StartMonitors fans out refreshes), so it must be atomic — a plain int
	// tripped the race detector under `go test -race` independently of any
	// production code path.
	toolCalls atomic.Int64
}

type terminalCleanupCaller struct {
	mu    sync.Mutex
	calls map[string]int
	errs  map[string]error
}

func (c *cleanupCountingCaller) Call(string, any) (json.RawMessage, error) { return nil, nil }
func (c *cleanupCountingCaller) CallWithTimeout(string, any, time.Duration) (json.RawMessage, error) {
	return nil, nil
}
func (c *cleanupCountingCaller) CallTool(string, map[string]any) (json.RawMessage, error) {
	c.toolCalls.Add(1)
	return nil, nil
}
func (c *cleanupCountingCaller) CallToolWithTimeout(string, map[string]any, time.Duration) (json.RawMessage, error) {
	c.toolCalls.Add(1)
	return nil, nil
}
func (c *cleanupCountingCaller) CircuitOpen() bool { return false }
func (c *cleanupCountingCaller) Close() error      { return nil }

func (c *terminalCleanupCaller) Call(string, any) (json.RawMessage, error) {
	return nil, nil
}
func (c *terminalCleanupCaller) CallWithTimeout(string, any, time.Duration) (json.RawMessage, error) {
	return nil, nil
}
func (c *terminalCleanupCaller) CallTool(name string, _ map[string]any) (json.RawMessage, error) {
	return c.record(name)
}
func (c *terminalCleanupCaller) CallToolWithTimeout(name string, _ map[string]any, _ time.Duration) (json.RawMessage, error) {
	return c.record(name)
}
func (c *terminalCleanupCaller) CircuitOpen() bool { return false }
func (c *terminalCleanupCaller) Close() error      { return nil }

func (c *terminalCleanupCaller) record(name string) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = make(map[string]int)
	}
	c.calls[name]++
	if c.errs != nil {
		if err := c.errs[name]; err != nil {
			return nil, err
		}
	}
	return json.RawMessage(`{"content":[{"type":"text","text":"ok: true"}]}`), nil
}

func (c *terminalCleanupCaller) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[name]
}

func (s *failingStopStore) Save(context.Context, *spawn.State) error {
	if s.fail {
		return s.err
	}
	return nil
}
func (s *failingStopStore) Load(context.Context, string) (*spawn.State, error) {
	return nil, nil
}
func (s *failingStopStore) LoadAll(context.Context) ([]*spawn.State, error) { return nil, nil }
func (s *failingStopStore) Delete(context.Context, string) error            { return nil }

func (b *stopRaceBackend) Build(_ context.Context, _ backend.BuildOpts) (*backend.BuildResult, error) {
	b.mu.Lock()
	b.buildCalls++
	b.mu.Unlock()
	if b.buildEntered != nil {
		close(b.buildEntered)
		<-b.releaseBuild // Deliberately ignore cancellation to expose the late-return race.
	}
	return &backend.BuildResult{ImageTag: "spawn:test"}, nil
}

func (b *stopRaceBackend) Start(_ context.Context, opts backend.StartOpts) (*backend.StartResult, error) {
	b.mu.Lock()
	b.startCalls++
	b.mu.Unlock()
	if b.startEntered != nil {
		close(b.startEntered)
		<-b.releaseStart // Deliberately ignore cancellation to expose the late-create race.
	}
	b.mu.Lock()
	b.startDone = true
	b.mu.Unlock()
	containerID := opts.Name
	if b.startID != "" {
		containerID = b.startID
	}
	return &backend.StartResult{ContainerID: containerID}, nil
}

func (b *stopRaceBackend) Exec(_ context.Context, opts backend.ExecOpts) (*backend.ExecResult, error) {
	if b.execEntered != nil && strings.Contains(opts.Command, "codex exec") {
		close(b.execEntered)
		<-b.releaseExec // Deliberately ignore cancellation to verify Stop waits for driver exit.
	}
	return &backend.ExecResult{ExitCode: 0}, nil
}

func (b *stopRaceBackend) Stop(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopCalls = append(b.stopCalls, id)
	if b.failStopAfterStart && b.startDone {
		return errors.New("injected late cleanup failure")
	}
	return b.stopErr
}

func (b *stopRaceBackend) Status(_ context.Context, _ string) (*backend.StatusResult, error) {
	return &backend.StatusResult{Running: true}, nil
}

func (b *stopRaceBackend) Health(_ context.Context) error           { return nil }
func (b *stopRaceBackend) Pause(_ context.Context, _ string) error  { return backend.ErrNotSupported }
func (b *stopRaceBackend) Resume(_ context.Context, _ string) error { return backend.ErrNotSupported }
func (b *stopRaceBackend) ReadFile(_ context.Context, _, _ string) ([]byte, error) {
	return nil, backend.ErrNotSupported
}
func (b *stopRaceBackend) WriteFile(_ context.Context, _, _ string, _ []byte, _ string) error {
	return nil
}
func (b *stopRaceBackend) CleanupBuilds(_ context.Context, _ time.Duration) (int, error) {
	return 0, nil
}

func (b *stopRaceBackend) counts() (builds, starts int, stops []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buildCalls, b.startCalls, append([]string(nil), b.stopCalls...)
}

func (b *stopRaceBackend) allowStops() {
	b.mu.Lock()
	b.failStopAfterStart = false
	b.stopErr = nil
	b.mu.Unlock()
}

func newStopRaceOrchestrator(t *testing.T, be backend.Backend) *SpawnOrchestrator {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := spawn.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create spawn store: %v", err)
	}
	ctrl := spawn.NewK8sController(k8sfake.NewSimpleClientset(), "spawn-test", store, logger)
	return newStopRaceOrchestratorWithController(t, be, ctrl, logger)
}

func newStopRaceOrchestratorWithController(
	t *testing.T,
	be backend.Backend,
	ctrl *spawn.K8sController,
	logger *slog.Logger,
) *SpawnOrchestrator {
	t.Helper()
	o := &SpawnOrchestrator{
		backends:         map[string]backend.Backend{DefaultSubstrate: be},
		defaultSubstrate: DefaultSubstrate,
		tracer:           otel.Tracer("spawn-stop-race-test"),
		logger:           logger,
		ctrl:             ctrl,
		maxConcurrent:    1,
		buildSlots:       newBuildSlots(1),
		defaultTimeout:   time.Minute,
		defaultMemory:    128,
		defaultCPUs:      1,
		workspaceRoot:    t.TempDir(),
		syncMode:         "git-clone",
	}
	ctrl.SetStoppingHook(o.reconcileStoppingSpawn)
	return o
}

func stopRaceRequest() SpawnRequest {
	return SpawnRequest{
		AgentType:       "codex",
		TaskDescription: "exercise stop ownership",
		Project:         "services/loom-core",
		IdempotencyKey:  "stop-race-key",
	}
}

func waitForStop(t *testing.T, stopped <-chan error) {
	t.Helper()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("StopSpawn() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StopSpawn() did not observe driver exit")
	}
}

func waitForBackendStop(t *testing.T, be *stopRaceBackend, pod string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, _, stops := be.counts()
		for _, got := range stops {
			if got == pod {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("backend did not stop pod %q", pod)
}

func TestReapTerminalSpawn_RuntimeFailureBlocksAgentCleanup(t *testing.T) {
	wantErr := errors.Join(backend.ErrRuntimeIdentityConflict, errors.New("runtime generation changed"))
	be := &stopRaceBackend{stopErr: wantErr}
	o := newStopRaceOrchestrator(t, be)
	caller := &cleanupCountingCaller{}
	o.agentBridge = bridge.NewAgentBridge(caller)
	now := time.Now()
	state := &SpawnState{
		SpawnID:   "spawn-cleanup-fence",
		AgentID:   "spawn-codex-cleanup-fence",
		PodName:   "spawn-spawn-cleanup-fence",
		Status:    SpawnStatusCompleted,
		StartedAt: now,
	}
	o.ctrl.UpdateState(t.Context(), state)

	err := o.reapTerminalSpawn(t.Context(), *state)
	if !errors.Is(err, backend.ErrRuntimeIdentityConflict) {
		t.Fatalf("reapTerminalSpawn() error = %v, want identity conflict", err)
	}
	if got := caller.toolCalls.Load(); got != 0 {
		t.Fatalf("agent-context cleanup calls = %d, want 0 after runtime ownership loss", got)
	}
}

func TestReapTerminalSpawn_PresenceDeregisterFailureIsBounded(t *testing.T) {
	o := newStopRaceOrchestrator(t, &stopRaceBackend{})
	caller := &terminalCleanupCaller{
		errs: map[string]error{
			"agent_context__agent_presence_deregister": errors.New("presence unavailable"),
		},
	}
	o.agentBridge = bridge.NewAgentBridge(caller)
	now := time.Now()
	state := &SpawnState{
		SpawnID:   "spawn-presence-cleanup",
		AgentID:   "spawn-codex-presence-cleanup",
		SessionID: "session-presence-cleanup",
		Status:    SpawnStatusCompleted,
		StartedAt: now,
	}
	o.ctrl.UpdateState(t.Context(), state)

	if err := o.reapTerminalSpawn(t.Context(), *state); err != nil {
		t.Fatalf("reapTerminalSpawn() error = %v, want nil for bounded presence cleanup failure", err)
	}
	if got := caller.count("agent_context__agent_presence_deregister"); got != terminalPresenceDeregisterAttempts {
		t.Fatalf("presence deregister calls = %d, want %d", got, terminalPresenceDeregisterAttempts)
	}
	if got := caller.count("agent_context__agent_session_end"); got != 1 {
		t.Fatalf("session end calls = %d, want 1", got)
	}
}

func TestReapTerminalSpawn_SessionEndFailureDoesNotBlockCleanupAck(t *testing.T) {
	o := newStopRaceOrchestrator(t, &stopRaceBackend{})
	caller := &terminalCleanupCaller{
		errs: map[string]error{
			"agent_context__agent_session_end": errors.New("session store unavailable"),
		},
	}
	o.agentBridge = bridge.NewAgentBridge(caller)
	now := time.Now()
	state := &SpawnState{
		SpawnID:   "spawn-session-cleanup",
		AgentID:   "spawn-codex-session-cleanup",
		SessionID: "session-cleanup",
		Status:    SpawnStatusCompleted,
		StartedAt: now,
	}
	o.ctrl.UpdateState(t.Context(), state)

	if err := o.reapTerminalSpawn(t.Context(), *state); err != nil {
		t.Fatalf("reapTerminalSpawn() error = %v, want nil for session cleanup failure", err)
	}
	if got := caller.count("agent_context__agent_presence_deregister"); got != 1 {
		t.Fatalf("presence deregister calls = %d, want 1", got)
	}
	if got := caller.count("agent_context__agent_session_end"); got != 1 {
		t.Fatalf("session end calls = %d, want 1", got)
	}
}

func TestStopSpawnDuringBuildPreventsLatePodCreate(t *testing.T) {
	be := &stopRaceBackend{buildEntered: make(chan struct{}), releaseBuild: make(chan struct{})}
	o := newStopRaceOrchestrator(t, be)
	spawnID, err := o.Spawn(t.Context(), stopRaceRequest())
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	<-be.buildEntered

	stopped := make(chan error, 1)
	go func() { stopped <- o.StopSpawn(t.Context(), spawnID) }()
	select {
	case err := <-stopped:
		t.Fatalf("StopSpawn returned before the background driver exited: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	state, _ := o.ctrl.Get(spawnID)
	if state.Status == SpawnStatusStopped {
		t.Fatal("stopped became observable before the build driver exited")
	}
	close(be.releaseBuild)
	waitForStop(t, stopped)

	state, _ = o.ctrl.Get(spawnID)
	if state.Status != SpawnStatusStopped {
		t.Fatalf("status = %s, want stopped", state.Status)
	}
	_, starts, _ := be.counts()
	if starts != 0 {
		t.Fatalf("Start calls = %d, want 0 after stop during build", starts)
	}
}

func TestStopSpawnRefusesCleanupWhenIntentPersistenceFails(t *testing.T) {
	wantErr := errors.New("store unavailable")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &failingStopStore{err: wantErr}
	ctrl := spawn.NewK8sController(
		k8sfake.NewSimpleClientset(), "spawn-test", store, logger,
	)
	be := &stopRaceBackend{}
	o := newStopRaceOrchestratorWithController(t, be, ctrl, logger)
	spawnID, err := ctrl.Spawn(t.Context(), stopRaceRequest())
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	state, _ := ctrl.Get(spawnID)
	state.PodName = "spawn-" + spawnID
	ctrl.UpdateState(t.Context(), state)
	store.fail = true

	err = o.StopSpawn(t.Context(), spawnID)
	if err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("StopSpawn() error = %v, want persistence failure %v", err, wantErr)
	}
	state, _ = ctrl.Get(spawnID)
	if state.StopRequestedAt != nil || state.Status == SpawnStatusStopped {
		t.Fatalf("failed stop-intent persistence mutated state: status=%s intent=%v",
			state.Status, state.StopRequestedAt)
	}
	_, _, stops := be.counts()
	if len(stops) != 0 {
		t.Fatalf("cleanup ran without durable stop intent: %v", stops)
	}
}

func TestStopSpawnDuringPodStartCleansLatePodWithoutRevival(t *testing.T) {
	be := &stopRaceBackend{startEntered: make(chan struct{}), releaseStart: make(chan struct{})}
	o := newStopRaceOrchestrator(t, be)
	spawnID, err := o.Spawn(t.Context(), stopRaceRequest())
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	<-be.startEntered

	stopped := make(chan error, 1)
	go func() { stopped <- o.StopSpawn(t.Context(), spawnID) }()
	select {
	case err := <-stopped:
		t.Fatalf("StopSpawn returned before the pod-start driver exited: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	state, _ := o.ctrl.Get(spawnID)
	if state.Status == SpawnStatusStopped {
		t.Fatal("stopped became observable before late pod creation was cleaned up")
	}
	close(be.releaseStart)
	waitForStop(t, stopped)

	state, _ = o.ctrl.Get(spawnID)
	if state.Status != SpawnStatusStopped {
		t.Fatalf("status = %s, want stopped", state.Status)
	}
	_, starts, stops := be.counts()
	if starts != 1 {
		t.Fatalf("Start calls = %d, want the one in-flight call", starts)
	}
	wantPod := "spawn-" + spawnID
	found := false
	for _, pod := range stops {
		if pod == wantPod {
			found = true
		}
	}
	if !found {
		t.Fatalf("late-created pod %q was not cleaned up; Stop calls = %v", wantPod, stops)
	}
}

func TestStopSpawnLateStartCleanupFailureRetainsRetryablePod(t *testing.T) {
	be := &stopRaceBackend{
		startEntered:       make(chan struct{}),
		releaseStart:       make(chan struct{}),
		failStopAfterStart: true,
		startID:            "late-vm-container-123",
	}
	o := newStopRaceOrchestrator(t, be)
	spawnID, err := o.Spawn(t.Context(), stopRaceRequest())
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	<-be.startEntered

	stopped := make(chan error, 1)
	go func() { stopped <- o.StopSpawn(t.Context(), spawnID) }()
	stopIntentDeadline := time.Now().Add(time.Second)
	for {
		state, _ := o.ctrl.Get(spawnID)
		if state.StopRequestedAt != nil {
			break
		}
		if time.Now().After(stopIntentDeadline) {
			t.Fatal("StopSpawn did not persist its stop intent before late Start release")
		}
		time.Sleep(time.Millisecond)
	}
	close(be.releaseStart)
	select {
	case err = <-stopped:
		if err == nil || !strings.Contains(err.Error(), "late cleanup failure") {
			t.Fatalf("StopSpawn() error = %v, want late cleanup failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StopSpawn() did not report late cleanup failure")
	}

	wantPod := be.startID
	state, _ := o.ctrl.Get(spawnID)
	if state.Status == SpawnStatusStopped {
		t.Fatal("cleanup failure must not publish stopped")
	}
	if state.StopRequestedAt == nil || state.PodName != wantPod {
		t.Fatalf("cleanup failure lost retry handle: intent=%v pod=%q want=%q",
			state.StopRequestedAt, state.PodName, wantPod)
	}
	if !strings.Contains(state.Error, "late cleanup failure") {
		t.Fatalf("cleanup failure not persisted: %q", state.Error)
	}

	be.allowStops()
	if err := o.StopSpawn(t.Context(), spawnID); err != nil {
		t.Fatalf("retry StopSpawn() error = %v", err)
	}
	state, _ = o.ctrl.Get(spawnID)
	if state.Status != SpawnStatusStopped || state.Error != "" {
		t.Fatalf("successful cleanup retry = status %s error %q, want stopped with no error",
			state.Status, state.Error)
	}
}

func TestStopSpawnDuringExecWaitsForDriverAndCannotComplete(t *testing.T) {
	be := &stopRaceBackend{execEntered: make(chan struct{}), releaseExec: make(chan struct{})}
	o := newStopRaceOrchestrator(t, be)
	spawnID, err := o.Spawn(t.Context(), stopRaceRequest())
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	<-be.execEntered

	stopped := make(chan error, 1)
	go func() { stopped <- o.StopSpawn(t.Context(), spawnID) }()
	select {
	case err := <-stopped:
		t.Fatalf("StopSpawn returned before the exec driver exited: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	state, _ := o.ctrl.Get(spawnID)
	if state.Status == SpawnStatusStopped {
		t.Fatal("stopped became observable before the exec driver exited")
	}
	// Model the reconciler observing the just-deleted pod as failed before the
	// canceled exec returns. The explicit stop request must win at driver exit.
	wantPod := "spawn-" + spawnID
	waitForBackendStop(t, be, wantPod)
	if state.StopRequestedAt == nil {
		t.Fatal("StopSpawn did not persist stopping intent before cleanup")
	}
	if err := o.ctrl.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	state, _ = o.ctrl.Get(spawnID)
	if state.Status != SpawnStatusRunning {
		t.Fatalf("Reconcile changed stopping spawn to %s, want running until cleanup completes", state.Status)
	}
	close(be.releaseExec)
	waitForStop(t, stopped)

	state, _ = o.ctrl.Get(spawnID)
	if state.Status != SpawnStatusStopped {
		t.Fatalf("late exec completion changed status to %s, want stopped", state.Status)
	}
	_, _, stops := be.counts()
	found := false
	for _, pod := range stops {
		if pod == wantPod {
			found = true
		}
	}
	if !found {
		t.Fatalf("running pod %q was not stopped; Stop calls = %v", wantPod, stops)
	}
}

func TestStoppedSpawnCannotBeRedrivenOrCompleted(t *testing.T) {
	be := &stopRaceBackend{}
	o := newStopRaceOrchestrator(t, be)
	spawnID, err := o.ctrl.Spawn(t.Context(), stopRaceRequest())
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	if err := o.StopSpawn(t.Context(), spawnID); err != nil {
		t.Fatalf("StopSpawn() error = %v", err)
	}

	o.runSpawn(spawnID, stopRaceRequest())
	state, _ := o.ctrl.Get(spawnID)
	o.completeSpawn(t.Context(), state)
	state, _ = o.ctrl.Get(spawnID)
	if state.Status != SpawnStatusStopped {
		t.Fatalf("late driver transition revived stopped spawn as %s", state.Status)
	}
	builds, starts, _ := be.counts()
	if builds != 0 || starts != 0 {
		t.Fatalf("stopped spawn redrive reached backend: builds=%d starts=%d", builds, starts)
	}
}

func TestStopSpawnDoesNotRewriteTerminalWinner(t *testing.T) {
	for _, status := range []SpawnStatus{SpawnStatusCompleted, SpawnStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			be := &stopRaceBackend{}
			o := newStopRaceOrchestrator(t, be)
			spawnID, err := o.ctrl.Spawn(t.Context(), stopRaceRequest())
			if err != nil {
				t.Fatalf("seed spawn: %v", err)
			}
			state, _ := o.ctrl.Get(spawnID)
			state.Status = status
			state.PodName = "spawn-" + spawnID
			o.ctrl.UpdateState(t.Context(), state)

			if err := o.StopSpawn(t.Context(), spawnID); err != nil {
				t.Fatalf("StopSpawn() error = %v", err)
			}
			state, _ = o.ctrl.Get(spawnID)
			if state.Status != status || state.StopRequestedAt != nil {
				t.Fatalf("terminal winner changed to status=%s intent=%v, want %s with no intent",
					state.Status, state.StopRequestedAt, status)
			}
			_, _, stops := be.counts()
			wantPod := "spawn-" + spawnID
			if len(stops) != 1 || stops[0] != wantPod {
				t.Fatalf("terminal winner cleanup calls = %v, want exactly [%s]", stops, wantPod)
			}
		})
	}
}

func TestStopSpawnAfterDriverCompletionWinnerDoesNotCancelOwner(t *testing.T) {
	be := &stopRaceBackend{}
	o := newStopRaceOrchestrator(t, be)
	spawnID, err := o.ctrl.Spawn(t.Context(), stopRaceRequest())
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	state, _ := o.ctrl.Get(spawnID)
	state.PodName = "spawn-" + spawnID
	o.ctrl.UpdateState(t.Context(), state)
	owner, ok := o.acquireSpawnDriver(spawnID)
	if !ok {
		t.Fatal("acquireSpawnDriver() = false")
	}
	o.completeSpawn(t.Context(), state)

	if err := o.StopSpawn(t.Context(), spawnID); err != nil {
		t.Fatalf("StopSpawn() error = %v", err)
	}
	if owner.stopRequested {
		t.Fatal("stop canceled a driver whose completed terminal CAS already won")
	}
	state, _ = o.ctrl.Get(spawnID)
	if state.Status != SpawnStatusCompleted || state.StopRequestedAt != nil {
		t.Fatalf("completion winner changed: status=%s intent=%v", state.Status, state.StopRequestedAt)
	}
	o.releaseSpawnDriver(spawnID, owner)
}

func TestDriverTransitionSurfacesPersistenceFailure(t *testing.T) {
	storeErr := errors.New("injected driver transition persistence failure")
	store := &failingStopStore{err: storeErr}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctrl := spawn.NewK8sController(k8sfake.NewSimpleClientset(), "spawn-test", store, logger)
	o := newStopRaceOrchestratorWithController(t, &stopRaceBackend{}, ctrl, logger)
	spawnID, err := ctrl.Spawn(t.Context(), stopRaceRequest())
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	owner, ok := o.acquireSpawnDriver(spawnID)
	if !ok {
		t.Fatal("acquireSpawnDriver() = false")
	}
	defer o.releaseSpawnDriver(spawnID, owner)
	store.fail = true

	state, updated, err := o.updateSpawnFromDriver(t.Context(), spawnID, owner, func(current *SpawnState) {
		current.AuthMode = spawn.AuthModeClusterAPIKey
	})
	if updated || !errors.Is(err, storeErr) {
		t.Fatalf("driver transition = updated %v error %v, want surfaced persistence failure", updated, err)
	}
	if state == nil || state.Status != SpawnStatusCreating || state.AuthMode != "" {
		t.Fatalf("returned state was not rolled back: %#v", state)
	}
	loaded, _ := ctrl.Get(spawnID)
	if loaded.Status != SpawnStatusCreating || loaded.AuthMode != "" {
		t.Fatalf("controller state was not rolled back: %#v", loaded)
	}
}

func TestStopSpawnTerminalCleanupErrorPreservesWinner(t *testing.T) {
	wantErr := errors.New("terminal pod delete failed")
	be := &stopRaceBackend{stopErr: wantErr}
	o := newStopRaceOrchestrator(t, be)
	spawnID, err := o.ctrl.Spawn(t.Context(), stopRaceRequest())
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	state, _ := o.ctrl.Get(spawnID)
	state.Status = SpawnStatusCompleted
	state.PodName = "spawn-" + spawnID
	o.ctrl.UpdateState(t.Context(), state)

	err = o.StopSpawn(t.Context(), spawnID)
	if err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("StopSpawn() error = %v, want %v", err, wantErr)
	}
	state, _ = o.ctrl.Get(spawnID)
	if state.Status != SpawnStatusCompleted || state.StopRequestedAt != nil {
		t.Fatalf("terminal cleanup error rewrote winner: status=%s intent=%v",
			state.Status, state.StopRequestedAt)
	}
}

func TestRecoverStoppingSpawnRetriesCleanupWithoutRedrive(t *testing.T) {
	be := &stopRaceBackend{}
	o := newStopRaceOrchestrator(t, be)
	spawnID, err := o.ctrl.Spawn(t.Context(), stopRaceRequest())
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	if _, disposition, err := o.ctrl.BeginStop(t.Context(), spawnID); err != nil || disposition != spawn.StopBegan {
		t.Fatalf("BeginStop() disposition=%v error=%v", disposition, err)
	}

	o.recoverStoppingSpawns()
	state, _ := o.ctrl.Get(spawnID)
	if state.Status != SpawnStatusStopped {
		t.Fatalf("recovered stopping status = %s, want stopped", state.Status)
	}
	if action := classifyInterruptedSpawn(state); action != interruptedSkip {
		t.Fatalf("recovered stopped spawn action = %v, want skip", action)
	}
	_, starts, stops := be.counts()
	if starts != 0 || len(stops) != 1 {
		t.Fatalf("recovery redrove work or skipped cleanup: starts=%d stops=%v", starts, stops)
	}
}

func TestReconcilePeriodicallyRetriesFailedStoppingCleanup(t *testing.T) {
	be := &stopRaceBackend{stopErr: errors.New("temporary delete failure")}
	o := newStopRaceOrchestrator(t, be)
	spawnID, err := o.ctrl.Spawn(t.Context(), stopRaceRequest())
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	if _, disposition, err := o.ctrl.BeginStop(t.Context(), spawnID); err != nil || disposition != spawn.StopBegan {
		t.Fatalf("BeginStop() disposition=%v error=%v", disposition, err)
	}
	o.recoverStoppingSpawns()
	state, _ := o.ctrl.Get(spawnID)
	if state.Status == SpawnStatusStopped || state.Error == "" {
		t.Fatalf("failed recovery did not remain retryable: status=%s error=%q", state.Status, state.Error)
	}

	be.allowStops()
	if err := o.ctrl.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	state, _ = o.ctrl.Get(spawnID)
	if state.Status != SpawnStatusStopped || state.Error != "" {
		t.Fatalf("periodic retry = status %s error %q, want stopped", state.Status, state.Error)
	}
}
