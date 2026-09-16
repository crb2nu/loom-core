package hud

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

// errSpawnBackendDegraded is returned by Spawn while the startup spawn-state
// recovery pass has not completed (store unreachable at boot). The background
// retry loop in embed.go clears the condition once the store is reachable.
var errSpawnBackendDegraded = errors.New(
	"spawn backend degraded: spawn-state recovery pending (store unreachable at startup); retry once recovery completes")

// SpawnStatus is a type alias for spawn.Status, preserving the existing HUD API.
type SpawnStatus = spawn.Status

// SpawnStatus constants — aliases to spawn package constants.
const (
	SpawnStatusCreating  = spawn.StatusPending
	SpawnStatusBuilding  = spawn.StatusBuilding
	SpawnStatusRunning   = spawn.StatusRunning
	SpawnStatusCompleted = spawn.StatusCompleted
	SpawnStatusFailed    = spawn.StatusFailed
	SpawnStatusStopped   = spawn.StatusStopped
)

// SpawnRequest is a type alias for spawn.Request.
type SpawnRequest = spawn.Request

// SpawnState is a type alias for spawn.State.
type SpawnState = spawn.State

// spawnLaunchSpecBuilder prepares the request handed to the controller and
// launcher. Keeping this boundary narrow lets the launch path be extracted
// without changing Spawn's public surface.
type spawnLaunchSpecBuilder interface {
	Build(context.Context, SpawnRequest) (SpawnRequest, error)
}

// spawnLauncher dispatches an accepted spawn. The production implementation
// retains the existing asynchronous handoff to runSpawn.
type spawnLauncher interface {
	Launch(string, SpawnRequest)
}

// DefaultSubstrate is the substrate name used when a SpawnRequest leaves
// req.Substrate empty. Mirrors policy.SubstrateDefault in pkg/mills so
// callers can opt OUT of routing without juggling literals.
const DefaultSubstrate = "k8s"

const (
	terminalPresenceDeregisterAttempts = 3
	terminalPresenceDeregisterTimeout  = 2 * time.Second
)

// SpawnOrchestrator manages the full lifecycle of headless agent spawns.
// It delegates state management to a spawn.Controller, keeping the HUD layer
// focused on shuttle concerns (build, deploy, exec, SSE, metrics).
type SpawnOrchestrator struct {
	launchSpecBuilder spawnLaunchSpecBuilder
	launcher          spawnLauncher
	// backends maps substrate name → Backend impl. At least one entry
	// (the default substrate, typically "k8s") must be present; harvester-vm
	// is registered only when the operator configures it. Lookup goes
	// through substrateBackend(substrate) which handles fallback +
	// warn-on-unknown so a misconfigured Mills policy surfaces in logs
	// rather than silently wedging. Slice 2d: spec
	// .loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md
	backends         map[string]backend.Backend
	defaultSubstrate string

	agentBridge *bridge.AgentBridge
	sseHub      *SSEHub
	tracer      trace.Tracer
	metrics     *HUDMetrics
	logger      *slog.Logger
	ctrl        *spawn.K8sController

	// Limits.
	maxConcurrent  int
	buildSlots     chan struct{}
	defaultTimeout time.Duration
	defaultMemory  int
	defaultCPUs    float64
	// livenessStallTimeout bounds how long a streaming spawn may run without
	// any agent output before the liveness watchdog fails it. See
	// SpawnOrchestratorConfig.LivenessStallTimeout.
	livenessStallTimeout time.Duration

	// redriveSpawn, when non-nil, replaces the `go runSpawn(...)` launch
	// recoverInterruptedSpawns performs for a keyed interrupted spawn.
	// Test seam only; production leaves it nil.
	redriveSpawn func(spawnID string, req SpawnRequest)

	// stopIntentPersisted, when non-nil, is called after BeginStop has durably
	// recorded a stop request. Test seam only; production leaves it nil.
	stopIntentPersisted func(spawnID string)

	// supervisedExecution enables the S4 pod-owned execution supervisor: new
	// spawns launch their agent turn under a detached in-pod reaper and record
	// State.Supervised, so a controller restart RE-ATTACHES instead of
	// re-driving (preserving the completion-wrapper/hold process pair — the S1c
	// continuity contract). Off preserves the exact legacy exec+re-drive path.
	// Configured from LOOM_SPAWN_SUPERVISED_EXECUTION.
	supervisedExecution bool

	// reattachSpawn, when non-nil, replaces the `go runSpawnReattach(...)` launch
	// recoverInterruptedSpawns performs for a supervised interrupted spawn.
	// Test seam only; production leaves it nil.
	reattachSpawn func(spawnID string, req SpawnRequest)

	// probeSupervisorFn resolves a supervised spawn's live in-pod state during
	// recovery (reattach vs collect vs re-drive). Defaults to o.probeSupervisor;
	// overridden by tests to drive the decision table without a real pod.
	probeSupervisorFn func(ctx context.Context, substrate, spawnID string) (supervisorProbe, error)

	// reattachExecFn runs the reattach launcher in the pod and returns the
	// reaper's recorded outcome as an ExecResult. Defaults to
	// defaultReattachExec (backend.StreamExec); tests override it to return a
	// chosen outcome without a live pod.
	reattachExecFn func(owner *spawnDriverOwner, sec streamExecCapable, podName, attachCmd string, timeoutSec int, onLine func([]byte)) (*backend.ExecResult, error)

	// workspaceRoot is the local path to the workspace mount (for project detection).
	workspaceRoot string
	// syncMode is the backend workspace sync mode ("git-clone", "nfs",
	// "tar-pipe"). In git-clone mode the spawn pod clones the repo fresh, so a
	// repo missing from the workspace mount can fall back to a lexical path +
	// generic runtime image instead of hard-failing project resolution.
	syncMode string
	// projects lists available project names for the spawn picker.
	projects []string

	// telemetry holds live SpawnTelemetryAccumulators for running spawns.
	// map[spawnID]*bridge.SpawnTelemetryAccumulator
	telemetry sync.Map
	// activeSpawnMetrics records only spawns whose active gauge was incremented
	// by this process, preventing recovered/duplicate terminal paths from
	// decrementing it twice or below zero.
	activeSpawnMetrics sync.Map

	// drivers owns the one background lifecycle goroutine permitted to mutate
	// each spawn. StopSpawn cancels that owner and waits for its exit before the
	// durable state becomes stopped, so a late Build/Start/Exec return cannot
	// recreate a pod or revive a terminal record.
	driversMu sync.Mutex
	drivers   map[string]*spawnDriverOwner

	// recoveryMu makes startup recovery one-shot. Loading/redriving the same
	// durable rows twice can launch two agent-turn drivers even in one process.
	recoveryMu sync.Mutex
	recovered  bool

	// degraded marks the spawn backend as serving without a completed startup
	// recovery pass (spawn-state store unreachable at boot). While set, Spawn
	// refuses new work — registering fresh records before the durable rows are
	// loaded could collide with keyed spawns that recovery would re-adopt.
	// Read paths keep serving. Set/cleared by the App's spawn init + the
	// background recovery retry loop (embed.go).
	degraded atomic.Bool

	// autoHandoff is the F5/Slice C1 trigger hook. Nil-safe: if unset,
	// the budget watcher skips auto-handoff evaluation. Set via
	// SetAutoHandoffHook from the orchestrator's wiring layer so this
	// file does not need to import pkg/agentcontext.
	autoHandoff AutoHandoffHook
}

type spawnDriverOwner struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}

	stopRequested   bool
	stopCleanupDone chan struct{}
	stopCleanupErr  error
}

// AutoHandoffHook is the minimal surface the budget watcher needs to
// evaluate + create an auto-handoff draft. Implemented by the
// agentcontext wiring layer so internal/hud/spawn.go stays
// dependency-light. All methods must be nil-safe at the call site.
type AutoHandoffHook interface {
	// Observe returns true if the trigger gate fires for this
	// (sessionKey, reason) pair at `now`.
	Observe(sessionKey, reason string, now time.Time) bool
	// Create drafts a handoff tagged source="auto". Errors are
	// logged by callers; Create must not panic on missing context.
	Create(ctx context.Context, sessionKey, sourceAgent, targetAgent, reason string, details map[string]any) error
	// Config exposes the live thresholds for inline breach evaluation.
	Config() AutoHandoffThresholds
}

// AutoHandoffThresholds is the subset of AutoHandoffConfig the watcher
// needs. Mirroring it here keeps this file independent of the
// agentcontext package.
type AutoHandoffThresholds struct {
	Enabled         bool
	InputTokenHigh  int
	CostUSDHigh     float64
	StalledDuration time.Duration
}

// SetAutoHandoffHook installs the auto-handoff trigger. Calling with a
// nil hook disables the feature without restructuring the orchestrator.
func (o *SpawnOrchestrator) SetAutoHandoffHook(h AutoHandoffHook) {
	o.autoHandoff = h
}

// errSpawnStalled is the cancellation cause attached to a spawn's exec context
// when the liveness watchdog trips. The finalize path reads it via
// context.Cause to fail the spawn with a precise, diagnosable reason instead
// of a generic "context canceled".
var errSpawnStalled = errors.New("liveness watchdog: agent produced no output within stall timeout")

// errSpawnStopped cancels the root lifecycle context when a caller manually
// stops a spawn. It is distinct from budget/liveness cancellation, which only
// cancels the child exec context and must still finalize as failed.
var errSpawnStopped = errors.New("spawn stopped by request")

const spawnDriverStopTimeout = 30 * time.Second

// Controller returns the underlying spawn.K8sController for callers that need
// direct access (e.g., to start a reconcile loop).
func (o *SpawnOrchestrator) Controller() *spawn.K8sController {
	return o.ctrl
}

// Spawn starts a new headless agent. Returns the spawn ID immediately (202).
// The actual spawn runs asynchronously in a goroutine.
