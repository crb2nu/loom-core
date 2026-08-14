package hud

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/spawn"
)

// flakySpawnStore is a spawn.Store whose every operation fails with a
// no-route-to-host style error until setHealthy(true) — simulating the k3s
// API being unreachable at daemon boot (the 2026-07-14 incident: transient
// ARP flap failed the loom-spawn-state ConfigMap read and permanently 404'd
// the embedded HUD).
type flakySpawnStore struct {
	mu      sync.Mutex
	healthy bool
	states  map[string]*spawn.State
}

var errStoreUnreachable = errors.New(
	"get configmap loom-spawn-state: dial tcp 192.168.50.200:6443: connect: no route to host")

func (s *flakySpawnStore) setHealthy(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthy = v
}

func (s *flakySpawnStore) Save(_ context.Context, st *spawn.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.healthy {
		return errStoreUnreachable
	}
	s.states[st.SpawnID] = st
	return nil
}

func (s *flakySpawnStore) Load(_ context.Context, id string) (*spawn.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.healthy {
		return nil, errStoreUnreachable
	}
	return s.states[id], nil
}

func (s *flakySpawnStore) LoadAll(_ context.Context) ([]*spawn.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.healthy {
		return nil, errStoreUnreachable
	}
	out := make([]*spawn.State, 0, len(s.states))
	for _, st := range s.states {
		out = append(out, st)
	}
	return out, nil
}

func (s *flakySpawnStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.healthy {
		return errStoreUnreachable
	}
	delete(s.states, id)
	return nil
}

// TestSpawnRecoveryStoreUnreachableDegradesAndRecovers is the regression test
// for the embedded-HUD hard-fail on k3s outage: an unreachable spawn-state
// store at init must not abort spawn init (which previously aborted HUD init
// entirely). Instead the backend goes degraded — Spawn refuses new work —
// and the background retry recovers the persisted state once the store is
// reachable again.
func TestSpawnRecoveryStoreUnreachableDegradesAndRecovers(t *testing.T) {
	// Compress the production retry pacing so the test runs in milliseconds.
	oldSyncAttempts := spawnRecoverySyncAttempts
	oldSyncInitial, oldSyncMax := spawnRecoverySyncInitial, spawnRecoverySyncMax
	oldRetryInitial, oldRetryMax := spawnRecoveryRetryInitial, spawnRecoveryRetryMax
	spawnRecoverySyncAttempts = 2
	spawnRecoverySyncInitial = time.Millisecond
	spawnRecoverySyncMax = 2 * time.Millisecond
	spawnRecoveryRetryInitial = 2 * time.Millisecond
	spawnRecoveryRetryMax = 10 * time.Millisecond
	t.Cleanup(func() {
		spawnRecoverySyncAttempts = oldSyncAttempts
		spawnRecoverySyncInitial, spawnRecoverySyncMax = oldSyncInitial, oldSyncMax
		spawnRecoveryRetryInitial, spawnRecoveryRetryMax = oldRetryInitial, oldRetryMax
	})

	ended := time.Now()
	seeded := &spawn.State{
		SpawnID:   "spawn-durable-1",
		AgentID:   "agent-1",
		Status:    spawn.StatusCompleted,
		StartedAt: ended.Add(-time.Minute),
		EndedAt:   &ended,
		CleanupAt: &ended,
	}
	store := &flakySpawnStore{states: map[string]*spawn.State{seeded.SpawnID: seeded}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctrl := spawn.NewK8sController(nil, "", store, logger)
	orch := NewSpawnOrchestratorForTest(ctrl)
	app := &App{logger: logger}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Store unreachable at init: finishSpawnInit must return (HUD init
	// proceeds) with the backend degraded instead of failing startup.
	app.finishSpawnInit(ctx, orch)

	if !orch.Degraded() {
		t.Fatal("expected spawn backend degraded while store is unreachable")
	}
	if _, err := orch.Spawn(ctx, SpawnRequest{Project: "demo", TaskDescription: "task"}); !errors.Is(err, errSpawnBackendDegraded) {
		t.Fatalf("expected degraded-backend error from Spawn, got %v", err)
	}

	// Store comes back: the background retry must recover the persisted
	// state and clear the degraded flag.
	store.setHealthy(true)

	deadline := time.Now().Add(5 * time.Second)
	for orch.Degraded() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if orch.Degraded() {
		t.Fatal("spawn backend still degraded after store became reachable")
	}

	found := false
	for _, st := range orch.ListSpawns() {
		if st != nil && st.SpawnID == seeded.SpawnID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("recovered spawn state missing %q after store came back", seeded.SpawnID)
	}
}

// TestStartMonitorsSpawnInitFailureKeepsHUDServing asserts the daemon-facing
// contract: a spawn backend that cannot initialize must not fail
// StartMonitors, because the daemon aborts embedded-HUD route registration
// on that error and every /api/* route then 404s for the process lifetime.
func TestStartMonitorsSpawnInitFailureKeepsHUDServing(t *testing.T) {
	// Explicit controller identity skips the host-local owner flock.
	t.Setenv("SPAWN_CONTROLLER_ID", "hud-degraded-test")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app, err := NewApp(Config{
		SpawnEnabled: true,
		// Deterministic construction failure with no kubeconfig or network
		// dependency: the backend rejects the quantity before any dial.
		SpawnBuildCPURequest: "not-a-quantity",
	}, &cleanupCountingCaller{}, logger)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := app.StartMonitors(ctx); err != nil {
		t.Fatalf("StartMonitors must not fail when spawn init fails: %v", err)
	}
	defer app.StopMonitors()

	if app.SpawnOrchestrator() != nil {
		t.Fatal("expected no spawn orchestrator after spawn init failure")
	}

	mux := http.NewServeMux()
	app.RegisterRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d, want 200", rr.Code)
	}
}
