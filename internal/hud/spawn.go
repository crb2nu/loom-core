package hud

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

// Pinned CLI versions for reproducible agent container builds.
const (
	claudeCodeVersion = "2.1.220"
	// codexVersion is the @openai/codex npm version baked into the k8s
	// spawn-runtime image (agentCLIInstallLines) and installed onto the
	// harvester-vm substrate (agentCLIInstallShell). It MUST be new enough for
	// every model the orchestrator may pin via resolveCodexModel. OpenAI gates
	// newer model ids behind a minimum Codex CLI version and the API rejects an
	// under-versioned CLI with HTTP 400 "The '<model>' model requires a newer
	// version of Codex. Please upgrade to the latest app or CLI." That 400 broke
	// EVERY per-stage-model spawn on 2026-07-18 (issues #347/#349/#350/#351):
	// Mills wired model=gpt-5.6-sol while the pod ran codex 0.130.0, so codex
	// authenticated, started the turn, then died on the version gate. The
	// operator only saw the misleading stdout tail "Reading additional input
	// from stdin..." because that line follows the failed turn; the true 400 was
	// captured in the spawn record's telemetry.errors.
	//
	// 0.143.0 does NOT clear the gpt-5.6 gate. The earlier "verified locally"
	// claim was run on a newer CLI; a pinned re-test on 2026-07-19 (npx
	// @openai/codex@0.143.0, and in-pod on the deployed
	// spawn-runtime-codex image) reproduces the same 400 for gpt-5.6-sol, so
	// the 0.143.0 pin left every stage_models codex spawn failing at $0 —
	// the plan_slice 64% error rate in /api/mills/telemetry/stages. 0.144.6
	// clears the gate: verified 2026-07-19 in-pod (cluster OAuth,
	// gpt-5.6-sol AND gpt-5.6-terra both complete turns). SPAWN_CODEX_VERSION
	// overrides this at image-build time so a future gate is an env flip, not
	// a rebuild (mirrors SPAWN_CODEX_MODEL) — see resolveCodexVersion.
	codexVersion  = "0.144.6"
	geminiVersion = "0.37.1"
	// goVersion is the Go toolchain baked into the k8s spawn-runtime image
	// (agentRuntimeDockerfile) and installed onto the harvester-vm substrate
	// (agentCLIInstallShell). It matches the repo's go.mod `go` directive and
	// .gitlab-ci.yml GO_VERSION so a spawned agent's `go build`/`go test` never
	// triggers a GOTOOLCHAIN auto-download — and, because the golang images pin
	// GOTOOLCHAIN=local, a runtime image OLDER than the go.mod directive makes
	// every in-spawn `go` command fail outright (the post-!979 1.25.11 image vs
	// the 1.26.4 directive did exactly that).
	goVersion = "1.26.6"
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

// streamExecCapable is satisfied by *backend.K8sBackend. It provides the
// low-level K8s client/config needed by backend.StreamExec.
type streamExecCapable interface {
	Clientset() kubernetes.Interface
	RestConfig() *rest.Config
	Namespace() string
	NFSFlush() bool
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

// NewSpawnOrchestrator creates a new spawn orchestrator. It initialises a
// spawn.K8sController backed by a FileStore for persistence and wires it
// into the HUD shuttle layer.
//
// backends must contain at least the entry for defaultSubstrate (which
// is also returned for empty / unknown substrate lookups). The
// single-backend production path passes
// {DefaultSubstrate: k8sBackend}, defaultSubstrate=DefaultSubstrate;
// the harvester-vm-enabled path adds an extra "harvester-vm" entry.
//
// NewSpawnOrchestratorSingleBackend is the legacy single-backend
// convenience wrapper for tests + callers that don't care about
// substrate routing.
func NewSpawnOrchestrator(
	backends map[string]backend.Backend,
	defaultSubstrate string,
	agentBridge *bridge.AgentBridge,
	sseHub *SSEHub,
	tracer trace.Tracer,
	metrics *HUDMetrics,
	logger *slog.Logger,
	cfg SpawnOrchestratorConfig,
) *SpawnOrchestrator {
	wsRoot := cfg.WorkspaceRoot
	if wsRoot == "" {
		wsRoot = "/workspace"
	}

	spawnLogger := logger.With("component", "spawn")

	if defaultSubstrate == "" {
		defaultSubstrate = DefaultSubstrate
	}
	// Defensive copy + ensure default is registered; otherwise lookups
	// would always fall back to a nil backend.
	bs := make(map[string]backend.Backend, len(backends))
	for k, v := range backends {
		bs[k] = v
	}
	defaultBackend := bs[defaultSubstrate]

	// Initialize persistent spawn store. In Kubernetes, use a ConfigMap
	// in the spawn namespace so HUD rollouts preserve accepted/in-flight
	// spawns. Local/dev backends keep the legacy FileStore. The K8s
	// ConfigMap lives in the default backend's namespace regardless of
	// which substrate a particular spawn runs on — state is HUD-side.
	var store spawn.Store
	if k8s, ok := defaultBackend.(streamExecCapable); ok && k8s.Clientset() != nil && k8s.Namespace() != "" {
		store = spawn.NewK8sConfigMapStore(k8s.Clientset(), k8s.Namespace(), "loom-spawn-state")
		spawnLogger.Info("using kubernetes spawn state store", "namespace", k8s.Namespace(), "configmap", "loom-spawn-state")
	} else {
		storeDir := spawn.DefaultStoreDir()
		if fs, err := spawn.NewFileStore(storeDir); err != nil {
			spawnLogger.Warn("failed to create spawn store, state will not be persisted",
				"dir", storeDir, "error", err)
		} else {
			store = fs
		}
	}

	// Create a K8sController. We pass a nil kubernetes.Interface because the
	// orchestrator uses the devbox backend (not raw K8s client) for pod
	// management. The controller still provides state tracking, reconciliation
	// hooks, and persistence. A future iteration can inject a real K8s client
	// when the spawn backend exposes it.
	controllerOpts := make([]spawn.ControllerOption, 0, 1)
	if _, shared := store.(*spawn.K8sConfigMapStore); shared {
		controllerID := strings.TrimSpace(cfg.ControllerID)
		if controllerID == "" {
			controllerID = defaultSpawnControllerID()
		}
		recoveryAuthority := cfg.RecoveryAuthority || spawnRecoveryAuthorityFromEnv()
		controllerOpts = append(controllerOpts, spawn.WithControllerOwnership(controllerID, recoveryAuthority))
		spawnLogger.Info("configured shared spawn-state ownership",
			"controller_id", controllerID, "recovery_authority", recoveryAuthority)
	}
	ctrl := spawn.NewK8sController(nil, "", store, spawnLogger, controllerOpts...)

	// Resolve the liveness stall timeout: explicit config wins, else the
	// 15-minute default, with an env escape hatch so the watchdog can be
	// retuned (or disabled with a large value) without a code redeploy on the
	// freshly-working autonomous-merge path.
	livenessStall := cfg.LivenessStallTimeout
	if livenessStall <= 0 {
		livenessStall = defaultLivenessStallTimeout
	}
	if env := strings.TrimSpace(os.Getenv("LOOM_SPAWN_LIVENESS_STALL_TIMEOUT")); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			livenessStall = d
			spawnLogger.Info("liveness stall timeout overridden via env",
				"timeout", livenessStall)
		} else {
			spawnLogger.Warn("ignoring invalid LOOM_SPAWN_LIVENESS_STALL_TIMEOUT",
				"value", env, "error", err)
		}
	}

	o := &SpawnOrchestrator{
		backends:             bs,
		defaultSubstrate:     defaultSubstrate,
		agentBridge:          agentBridge,
		sseHub:               sseHub,
		tracer:               tracer,
		metrics:              metrics,
		logger:               spawnLogger,
		ctrl:                 ctrl,
		maxConcurrent:        cfg.MaxConcurrent,
		buildSlots:           newBuildSlots(cfg.MaxConcurrentBuilds),
		defaultTimeout:       cfg.DefaultTimeout,
		defaultMemory:        cfg.DefaultMemory,
		defaultCPUs:          cfg.DefaultCPUs,
		workspaceRoot:        wsRoot,
		syncMode:             cfg.SyncMode,
		projects:             cfg.Projects,
		livenessStallTimeout: livenessStall,
		supervisedExecution:  cfg.SupervisedExecution,
	}
	o.probeSupervisorFn = o.probeSupervisor
	o.reattachExecFn = defaultReattachExec
	if cfg.SupervisedExecution {
		spawnLogger.Info("pod-owned execution supervisor enabled (S4): restart recovery re-attaches to in-pod reaper")
	}
	registered := make([]string, 0, len(bs))
	for k := range bs {
		registered = append(registered, k)
	}
	spawnLogger.Info("spawn substrate backends registered",
		"default", defaultSubstrate, "available", registered)
	// Wire the controller's terminal-cleanup hook so Reconcile reaps the
	// pod + presence + agent session for any spawn it observes in a
	// terminal state without CleanupAt set. This covers:
	//   - pods that exit naturally between failSpawn/completeSpawn ticks
	//   - pods abandoned across an operator restart
	//   - terminal-state spawns whose pod is still running (orphans that
	//     drain namespace quota — the failure mode that triggered this fix).
	ctrl.SetTerminalHook(o.reapTerminalSpawn)
	ctrl.SetStoppingHook(o.reconcileStoppingSpawn)
	return o
}

// NewSpawnOrchestratorSingleBackend is a legacy convenience wrapper for
// callers that don't care about per-spawn substrate routing (most tests
// + non-Mills HUD users). It registers `b` under DefaultSubstrate as
// the only backend.
func NewSpawnOrchestratorSingleBackend(
	b backend.Backend,
	agentBridge *bridge.AgentBridge,
	sseHub *SSEHub,
	tracer trace.Tracer,
	metrics *HUDMetrics,
	logger *slog.Logger,
	cfg SpawnOrchestratorConfig,
) *SpawnOrchestrator {
	return NewSpawnOrchestrator(
		map[string]backend.Backend{DefaultSubstrate: b},
		DefaultSubstrate,
		agentBridge, sseHub, tracer, metrics, logger, cfg,
	)
}

// substrateBackend picks the Backend impl for the named substrate.
//
// Empty substrate → default backend (matches current behavior pre-Slice 2c).
// Known substrate → its registered backend.
// Unknown substrate → default backend + warning log (so an operator who
// asked for harvester-vm without configuring it sees the misconfiguration
// in logs instead of silently running on k8s).
//
// Never returns nil so long as the orchestrator was constructed with the
// default substrate registered (NewSpawnOrchestrator's invariant).
func (o *SpawnOrchestrator) substrateBackend(substrate string) backend.Backend {
	if o == nil {
		return nil
	}
	key := substrate
	if key == "" {
		key = o.defaultSubstrate
	}
	if b, ok := o.backends[key]; ok {
		return b
	}
	if substrate != "" && o.logger != nil {
		o.logger.Warn("spawn substrate unknown; falling back to default",
			"requested", substrate, "default", o.defaultSubstrate)
	}
	return o.backends[o.defaultSubstrate]
}

func newBuildSlots(maxConcurrentBuilds int) chan struct{} {
	if maxConcurrentBuilds <= 0 {
		maxConcurrentBuilds = 1
	}
	return make(chan struct{}, maxConcurrentBuilds)
}

func (o *SpawnOrchestrator) acquireBuildSlot(ctx context.Context) (func(), error) {
	if o.buildSlots == nil {
		return func() {}, nil
	}
	select {
	case o.buildSlots <- struct{}{}:
		return func() { <-o.buildSlots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for spawn build slot: %w", ctx.Err())
	}
}

// Controller returns the underlying spawn.K8sController for callers that need
// direct access (e.g., to start a reconcile loop).
func (o *SpawnOrchestrator) Controller() *spawn.K8sController {
	return o.ctrl
}

// Spawn starts a new headless agent. Returns the spawn ID immediately (202).
// The actual spawn runs asynchronously in a goroutine.
func (o *SpawnOrchestrator) Spawn(ctx context.Context, req SpawnRequest) (string, error) {
	// While the backend is degraded (startup recovery pending because the
	// spawn-state store was unreachable at boot), refuse mutations: a fresh
	// record registered now could collide with a durable keyed row that
	// recovery has not loaded yet.
	if o.Degraded() {
		return "", errSpawnBackendDegraded
	}

	// Reject unsafe completion holds before defaults, deduplication, capacity
	// checks, or durable registration. An invalid request must have no side
	// effects and must not re-attach to an existing keyed spawn.
	if err := validateCompletionHoldSeconds(req.CompletionHoldSeconds); err != nil {
		return "", err
	}

	// Apply defaults.
	if req.MemoryMB <= 0 {
		req.MemoryMB = o.defaultMemory
	}
	if req.CPUs <= 0 {
		req.CPUs = o.defaultCPUs
	}
	if req.TimeoutMinutes <= 0 {
		req.TimeoutMinutes = int(o.defaultTimeout.Minutes())
	}
	if req.BaseBranch == "" {
		req.BaseBranch = "main"
	}
	if req.Namespace == "" {
		req.Namespace = req.Project + "/spawn"
	}
	if err := o.preflightKeyedRuntime(ctx, req); err != nil {
		return "", err
	}

	if existing := o.existingActiveSpawnForRequest(req); existing != "" {
		o.logger.Info("returning existing active spawn for idempotent request",
			"spawn_id", existing,
			"run_id", req.Metadata["LOOM_MILLS_RUN_ID"],
			"stage", req.Metadata["LOOM_MILLS_STAGE"],
		)
		return existing, nil
	}

	// Check concurrent limit.
	if o.ctrl.ActiveCount() >= o.maxConcurrent {
		return "", fmt.Errorf("max concurrent spawns reached (%d)", o.maxConcurrent)
	}

	// Delegate validation and ID generation to the controller.
	spawnID, dispatch, err := o.ctrl.Register(ctx, req)
	if err != nil {
		return "", err
	}
	if !dispatch {
		o.logger.Info("returning existing keyed spawn without duplicate dispatch",
			"spawn_id", spawnID, "idempotency_key", req.IdempotencyKey)
		return spawnID, nil
	}

	if o.metrics != nil {
		o.metrics.AgentSpawnTotal.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("agent_type", req.AgentType),
				attribute.String("outcome", "initiated"),
			),
		)
		o.metrics.SpawnedAgentActive.Add(ctx, 1)
		o.activeSpawnMetrics.Store(spawnID, struct{}{})
	}

	// Run spawn flow asynchronously.
	go o.runSpawn(spawnID, req)

	return spawnID, nil
}

// startSpawnPod creates the runtime pod via the substrate backend and applies
// the stable-handle-across-crash re-attach backstop on a k8s AlreadyExists.
//
// THE GAP it closes (.loom/134 §5): keyed spawns derive a deterministic pod
// name ("spawn-"+spawnID where spawnID == spawn.DeriveSpawnID(key)) and
// re-attach via the controller's IN-MEMORY map (spawnWithKey). But across an
// operator/HUD CRASH the in-memory map is empty, so a resume that re-derives
// the same key re-creates the SAME pod name. On the live k8s create path that
// surfaces as a real apierrors.AlreadyExists. Without this backstop the
// orchestrator failSpawns on it: the resume reports failure even though the pod
// the prior incarnation created is the correct, already-running handle.
//
// The backstop: when Start returns AlreadyExists AND the pod name is provably
// the deterministic name derived from a non-empty idempotency key
// (spawn.IsDerivedSpawnName), adopt the existing pod as a RE-ATTACH — return a
// success StartResult pointing at opts.Name (the deterministic, known pod
// name) and let the rest of runSpawn attach to that pod by name. The
// downstream steps (config inject, exec) are keyed off the pod name, which is
// stable across the crash, so adoption is safe.
//
// LEGACY PATH UNCHANGED: for a non-keyed spawn (empty IdempotencyKey, random
// NewSpawnID name) IsDerivedSpawnName returns false, so ANY Start error —
// including the near-impossible AlreadyExists on a random name — propagates
// verbatim and runSpawn failSpawns exactly as before. The backstop never
// changes behavior on the legacy/random-name path.
func (o *SpawnOrchestrator) startSpawnPod(
	ctx context.Context,
	be backend.Backend,
	req SpawnRequest,
	spawnID string,
	opts backend.StartOpts,
) (*backend.StartResult, error) {
	res, err := be.Start(ctx, opts)
	if err == nil {
		return res, nil
	}

	// Only treat AlreadyExists as a re-attach when the colliding name is the
	// deterministic name a keyed spawn derives. The empty-key (legacy) path
	// fails IsDerivedSpawnName, so its error semantics are preserved exactly.
	if apierrors.IsAlreadyExists(err) && spawn.IsDerivedSpawnName(opts.Name, req.IdempotencyKey) {
		// Only the K8s streaming backend can reattach by pod name. Harvester
		// also uses Kubernetes API objects, but its SSH key is process-local,
		// so adopting an AlreadyExists VM would produce an unusable handle.
		if _, ok := be.(streamExecCapable); !ok {
			return nil, err
		}
		prober, ok := be.(backend.StartIdentityProber)
		if !ok {
			return nil, fmt.Errorf("validate AlreadyExists runtime %s: backend has no identity probe: %w", opts.Name, err)
		}
		exists, probeErr := prober.ProbeStartIdentity(ctx, opts)
		if probeErr != nil {
			return nil, fmt.Errorf("validate AlreadyExists runtime %s: %w", opts.Name, probeErr)
		}
		if !exists {
			return nil, fmt.Errorf("validate AlreadyExists runtime %s: resource disappeared: %w", opts.Name, err)
		}
		o.logger.Info("k8s AlreadyExists on derived spawn name — re-attaching to existing pod (stable handle; agent turn re-executes at-least-once)",
			"spawn_id", spawnID,
			"pod", opts.Name,
		)
		// Adopt the existing pod. Its name is deterministic and equals
		// opts.Name, so the StartResult is reconstructable without another
		// API round-trip; downstream steps key off this name and Reconcile
		// will resolve the live phase from the cluster on its next tick.
		return &backend.StartResult{ContainerID: opts.Name}, nil
	}

	return nil, err
}

// runSpawn executes the full spawn lifecycle in a background goroutine.
func (o *SpawnOrchestrator) runSpawn(spawnID string, req SpawnRequest) {
	owner, ok := o.acquireSpawnDriver(spawnID)
	if !ok {
		return
	}
	defer o.releaseSpawnDriver(spawnID, owner)

	ctx, span := o.tracer.Start(owner.ctx, "agent.spawn",
		trace.WithAttributes(
			attribute.String("agent.type", req.AgentType),
			attribute.String("project", req.Project),
			attribute.String("namespace", req.Namespace),
			attribute.String("spawn_id", spawnID),
		),
	)
	defer span.End()

	state, _ := o.ctrl.Get(spawnID)
	// Spawn validates new requests before registration. Validate again for
	// recovered rows so corrupt or hand-authored persisted state cannot create
	// an unexpectedly long hold when the driver is re-attached after restart.
	if err := validateCompletionHoldSeconds(req.CompletionHoldSeconds); err != nil {
		o.failSpawn(ctx, state, err.Error())
		return
	}

	// Pick the substrate backend once per spawn so Build/Start/Exec/Stop
	// in this goroutine all hit the same impl. Slice 2d.
	be := o.substrateBackend(req.Substrate)
	if be == nil {
		o.failSpawn(ctx, state, fmt.Sprintf("no backend registered for substrate %q (default %q)", req.Substrate, o.defaultSubstrate))
		return
	}

	// Resolve which cluster auth path this spawn will use. Populated on
	// the state so HUD detail endpoints can surface "cluster_api_key" vs
	// "cluster_service_account" without introspecting the pod. In Slice 2a
	// the resolver returns a default based on agent type; Slice 2b layers
	// in cluster OAuth detection.
	state, ok, transitionErr := o.updateSpawnFromDriver(ctx, spawnID, owner, func(current *SpawnState) {
		current.AuthMode = resolveAuthMode(req.AgentType)
	})
	if transitionErr != nil {
		o.failSpawn(ctx, state, fmt.Sprintf("persist spawn auth mode: %v", transitionErr))
		return
	}
	if !ok {
		return
	}

	// Resolve the project name to an on-disk location plus the
	// workspace-relative path used inside the spawned pod. Bare names like
	// "loom-core" are searched under the standard buckets (services/, libs/,
	// ...) so monorepo repos resolve without explicit registration.
	projectDir, projectRel, resolveErr := resolveProjectPath(o.workspaceRoot, req.Project)
	if resolveErr != nil {
		// In git-clone mode the on-disk workspace copy is used ONLY to
		// fingerprint the repo for Dockerfile generation; the spawn pod's init
		// container clones the repo fresh as its source of truth. So a repo that
		// is not staged on the workspace PVC need not be a hard failure — fall
		// back to a lexical path and let generateDockerfile degrade to the
		// generic agent-runtime image. This lets Mills target additional
		// (services-group) repos without pre-staging them on the workspace
		// volume. For nfs/tar-pipe modes the local copy IS the source of truth,
		// so keep the hard failure there.
		lexRel, lexErr := lexicalProjectRel(req.Project)
		if !o.gitCloneMode() || lexErr != nil {
			msg := fmt.Sprintf("project resolution failed: %v", resolveErr)
			if lexErr != nil {
				msg = fmt.Sprintf("%s (lexical fallback: %v)", msg, lexErr)
			}
			o.failSpawn(ctx, state, msg)
			return
		}
		o.logger.Warn("spawn project not staged on workspace; using git-clone fallback with generic runtime image",
			"project", req.Project, "resolved_rel", lexRel, "resolve_error", resolveErr)
		projectDir = filepath.Join(o.workspaceRoot, lexRel)
		projectRel = lexRel
	}
	podProjectDir := "/workspace/" + projectRel

	// Step 1: Detect project environment and generate Dockerfile.
	state, ok, transitionErr = o.updateSpawnFromDriver(ctx, spawnID, owner, func(current *SpawnState) {
		current.Status = SpawnStatusBuilding
	})
	if transitionErr != nil {
		o.failSpawn(ctx, state, fmt.Sprintf("persist spawn building transition: %v", transitionErr))
		return
	}
	if !ok {
		return
	}
	o.broadcastSpawnEvent("agent.spawn.building", state)

	_, buildSpan := o.tracer.Start(ctx, "agent.spawn.image_build")

	df, dfErr := o.generateDockerfile(projectDir, req.AgentType)
	if dfErr != nil {
		buildSpan.End()
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, fmt.Sprintf("dockerfile generation failed: %v", dfErr))
		return
	}
	if !o.spawnDriverActive(spawnID, owner) {
		buildSpan.End()
		return
	}

	// ContextDir is used by the K8s backend for filepath.Rel (string-only,
	// no local filesystem access needed). In git-clone mode the backend
	// derives the project name from the path and clones the repo.
	buildTag := agentRuntimeBuildTag(req.AgentType, df)
	releaseBuildSlot, slotErr := o.acquireBuildSlot(ctx)
	if slotErr != nil {
		buildSpan.End()
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, fmt.Sprintf("image build queue failed: %v", slotErr))
		return
	}
	buildResult, err := be.Build(ctx, backend.BuildOpts{
		Tag:            buildTag,
		Dockerfile:     df,
		ContextDir:     projectDir,
		PreferExisting: true,
	})
	releaseBuildSlot()
	buildSpan.End()
	if err != nil {
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, fmt.Sprintf("image build failed: %v", err))
		return
	}
	if !o.spawnDriverActive(spawnID, owner) {
		return
	}

	// Step 2: Start K8s pod.
	o.logger.Info("build completed, starting pod", "spawn_id", spawnID, "image", buildResult.ImageTag)
	_, podSpan := o.tracer.Start(ctx, "agent.spawn.pod_create")
	env := buildSpawnPodEnv(req, state.AgentID, spawnID)
	startOpts := spawnRuntimeStartIdentityOpts(
		spawnID, state.AgentID, state.DriverOwnerID, state.StartedAt, false,
	)
	startOpts.ExtraLabels[spawn.AgentTypeLabel] = spawn.KubernetesLabelValue(req.AgentType)
	startOpts.ExtraLabels[spawn.ProjectLabel] = spawn.KubernetesLabelValue(req.Project)
	startOpts.ImageTag = buildResult.ImageTag
	startOpts.WorkDir = podProjectDir
	startOpts.Env = env
	startOpts.SecretEnv = agentSecretEnvVars(req.AgentType)
	startOpts.SecretMounts = agentSecretMounts(req.AgentType)
	startOpts.MemoryMB = req.MemoryMB
	startOpts.CPUs = req.CPUs
	startOpts.Network = true
	startOpts.Branch = req.Branch
	startOpts.BaseBranch = req.BaseBranch
	startOpts.AgentCLIInstallCmd = agentCLIInstallShell(req.AgentType)
	startOpts.CachePVCs = applySpawnGoCache(env, spawnGoCachePVC())
	startResult, err := o.startSpawnPod(ctx, be, req, spawnID, startOpts)
	podSpan.End()
	if err != nil {
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, fmt.Sprintf("pod creation failed: %v", err))
		return
	}

	state, ok, transitionErr = o.updateSpawnFromDriver(ctx, spawnID, owner, func(current *SpawnState) {
		current.PodName = startResult.ContainerID
	})
	if transitionErr != nil {
		o.cleanupLateSpawn(be, spawnID, startResult.ContainerID, owner)
		o.failSpawn(ctx, state, fmt.Sprintf("persist spawn pod handle: %v", transitionErr))
		return
	}
	if !ok {
		o.cleanupLateSpawn(be, spawnID, startResult.ContainerID, owner)
		return
	}

	// Step 3: Inject pre-authed configs (with a short timeout to avoid hanging on SPDY issues).
	_, cfgSpan := o.tracer.Start(ctx, "agent.spawn.config_inject")
	cfgCtx, cfgCancel := context.WithTimeout(ctx, 30*time.Second)
	o.logger.Info("injecting agent config", "spawn_id", spawnID, "pod", startResult.ContainerID, "agent_type", req.AgentType)
	if err := o.injectAgentConfig(cfgCtx, be, startResult.ContainerID, req.AgentType, podProjectDir); err != nil {
		cfgCancel()
		cfgSpan.End()
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, fmt.Sprintf("config injection failed: %v", err))
		return
	}
	cfgCancel()
	cfgSpan.End()

	// Step 4: Register agent session (before exec so the agent has session context).
	// Capture sessionID so per-tool-call events emitted by the accumulator below
	// can stamp the correct session and land in the Live Sessions panel row for
	// this spawn. StartSession is idempotent — repeat spawns under the same
	// namespace reuse the same session.
	_, sessSpan := o.tracer.Start(ctx, "agent.spawn.session_register")
	// Persist the session ID on the durable state so terminal transitions
	// (completeSpawn/failSpawn → persistTelemetrySummary) can write the
	// telemetry summary under a real, existing session. The state is flushed
	// to the controller below when we mark the spawn running. A failed
	// registration is logged inside registerSpawnSession (not swallowed): an
	// empty session id here makes persistTelemetrySummary skip the write,
	// silently dropping the spawn's turn-level telemetry — the exact blind
	// spot that hid the in-VM codex failure during the Mills A2 kill-test.
	if !o.spawnDriverActive(spawnID, owner) {
		sessSpan.End()
		return
	}
	sessionID := o.registerSpawnSession(req, state.AgentID)
	sessSpan.End()

	// Mark running and broadcast event.
	state, ok, transitionErr = o.updateSpawnFromDriver(ctx, spawnID, owner, func(current *SpawnState) {
		current.SessionID = sessionID
		current.Status = SpawnStatusRunning
	})
	if transitionErr != nil {
		o.cleanupLateSpawn(be, spawnID, startResult.ContainerID, owner)
		o.failSpawn(ctx, state, fmt.Sprintf("persist spawn running transition: %v", transitionErr))
		return
	}
	if !ok {
		o.cleanupLateSpawn(be, spawnID, startResult.ContainerID, owner)
		return
	}
	o.broadcastSpawnEvent("agent.spawn.running", state)

	// Step 5: Start heartbeat loop for spawn visibility.
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	go o.runHeartbeatLoop(heartbeatCtx, state)

	// Step 6: Execute agent CLI (or SDK driver) with real-time JSONL telemetry parsing.
	o.logger.Info("executing agent",
		"spawn_id", spawnID,
		"agent_type", req.AgentType,
		"pod", startResult.ContainerID,
		"use_sdk_driver", req.UseSDKDriver,
		"multi_turn", req.MultiTurn,
	)
	_, execSpan := o.tracer.Start(ctx, "agent.spawn.agent_exec")

	// Choose between the legacy CLI path and the embedded loom-spawn-driver
	// Node.js sidecar. When UseSDKDriver is set we inject the bundled driver
	// into the pod and invoke it instead of the raw agent CLI.
	var agentCmd string
	if req.UseSDKDriver {
		injectCtx, injectCancel := context.WithTimeout(ctx, 30*time.Second)
		if err := o.injectSDKDriver(injectCtx, be, startResult.ContainerID); err != nil {
			injectCancel()
			execSpan.End()
			heartbeatCancel()
			if !o.spawnDriverActive(spawnID, owner) {
				return
			}
			o.failSpawn(ctx, state, fmt.Sprintf("inject spawn driver: %v", err))
			return
		}

		// In multi-turn mode pre-create the control file so the driver's
		// fs.watch fires immediately on the first REST-driven append. The
		// REST handlers (slice 8c) call injectControlMessage to push
		// `{type:"message"|"interrupt"|"shutdown"}` lines into this file.
		var controlFilePath string
		if req.MultiTurn {
			if err := o.injectControlFile(injectCtx, be, startResult.ContainerID, spawnID); err != nil {
				injectCancel()
				execSpan.End()
				heartbeatCancel()
				if !o.spawnDriverActive(spawnID, owner) {
					return
				}
				o.failSpawn(ctx, state, fmt.Sprintf("inject control file: %v", err))
				return
			}
			controlFilePath = controlFilePathForSpawn(spawnID)
		}
		injectCancel()

		agentCmd = buildSDKDriverCommand(
			req.AgentType,
			req.TaskDescription,
			state.AgentID,
			spawnID,
			podProjectDir,
			controlFilePath,
			req.MaxTurns,
			req.MaxCostUSD,
		)
	} else {
		// Claude Code and Codex both accept a model override in their headless
		// modes. Gemini has no equivalent flag in this invocation.
		if m := strings.TrimSpace(req.Model); m != "" && req.AgentType != "codex" && req.AgentType != "claude-code" {
			o.logger.Info("spawn model override ignored: agent has no headless model knob",
				"spawn_id", spawnID, "agent_type", req.AgentType, "model", m)
		}
		agentCmd = buildAgentCommand(req.AgentType, req.TaskDescription, state.AgentID, req.Model, req.MaxTurns)
	}
	agentCmd = wrapAgentCommandWithCompletionHold(agentCmd, req.CompletionHoldSeconds)

	// S4 pod-owned execution supervisor. On the streaming (k8s) substrate, run
	// the wrapped agent+hold under a detached, PID-1-reparented in-pod reaper
	// that records the outcome durably, and have this exec only LAUNCH + TAIL it.
	// A controller crash then leaves the original completion-wrapper/hold process
	// pair intact and a restarted controller RE-ATTACHES (see runSpawnReattach)
	// instead of re-driving — the S1c process-continuity contract. Persist
	// State.Supervised BEFORE the exec so a crash mid-turn recovers via the
	// supervised path. Gated + streaming-only; the buffered harvester-vm path
	// keeps the direct exec (out of scope: k8s substrate only).
	supervised := false
	if _, streamCapable := be.(streamExecCapable); o.supervisedExecution && streamCapable {
		if err := o.injectSupervisorAssets(ctx, be, startResult.ContainerID, spawnID, agentCmd, req.TimeoutMinutes*60); err != nil {
			execSpan.End()
			heartbeatCancel()
			if !o.spawnDriverActive(spawnID, owner) {
				return
			}
			o.failSpawn(ctx, state, fmt.Sprintf("inject execution supervisor: %v", err))
			return
		}
		state, ok, transitionErr = o.updateSpawnFromDriver(ctx, spawnID, owner, func(current *SpawnState) {
			current.Supervised = true
		})
		if transitionErr != nil {
			execSpan.End()
			heartbeatCancel()
			o.failSpawn(ctx, state, fmt.Sprintf("persist supervised spawn flag: %v", transitionErr))
			return
		}
		if !ok {
			execSpan.End()
			heartbeatCancel()
			return
		}
		agentCmd = supervisorLaunchCommand(supervisorStateDir(spawnID), supervisorModeLaunch)
		supervised = true
		o.logger.Info("launched pod-owned execution supervisor",
			"spawn_id", spawnID, "pod", startResult.ContainerID)
	}

	// Create telemetry accumulator and JSONL parser for real-time parsing.
	// Wire it to the SSE hub via spawnTelemetryPublisher so per-tool-call
	// events (tool.call.start/end) reach /api/events. Without the publisher
	// the accumulator silently dropped them and the Live Sessions panel
	// only ever showed empty session rows for in-cluster spawn agents.
	acc := bridge.NewSpawnTelemetryAccumulatorWithPublisher(
		newSpawnTelemetryPublisher(o.sseHub),
		state.SessionID,
		state.AgentID,
	)
	o.telemetry.Store(spawnID, acc)

	broadcaster := SpawnEventBroadcaster(func(eventType string, agentID string, data any) {
		o.broadcastTelemetryEvent(eventType, agentID, data)
	})
	completionGuard := newCompletionGuardSink(acc)
	parser := newSpawnParser(req.AgentType, completionGuard, state.AgentID, spawnID, broadcaster, o.logger)

	var execResult *backend.ExecResult
	var execErr error
	usedStreaming := false

	// Cancellable exec context so the budget + liveness watchers can abort the
	// run. WithCancelCause lets the liveness watchdog attach errSpawnStalled so
	// the finalize path can distinguish a stalled-zombie cancellation from a
	// normal one (first cancel call wins, so the cleanup cancel(nil) below
	// never clobbers the watchdog's cause).
	execCtx, execCancel := context.WithCancelCause(ctx)

	// Budget watcher: polls the telemetry accumulator every 5s and cancels the
	// exec context when a configured budget is exceeded. The watcher terminates
	// when the exec returns via the done channel.
	watcherDone := make(chan struct{})
	if req.MaxCostUSD > 0 || req.MaxTurns > 0 {
		go o.runBudgetWatcher(execCtx, spawnID, req, acc, execCancel, watcherDone)
	} else {
		close(watcherDone)
	}

	// Liveness watchdog: streaming (K8s) path only. A zombie pod stuck in
	// Phase=Running with a dead agent process emits no further JSONL lines, so
	// acc.LastActivity() stops advancing. When it goes stale beyond the stall
	// timeout the watcher cancels exec with errSpawnStalled → failSpawn cleans
	// the pod → the Mills operator's poll sees a terminal failure and
	// auto-retries, instead of waiting out its 30-minute deadline pending a
	// manual operator restart. The buffered (harvester-vm) path is excluded:
	// it has no mid-flight telemetry, so freshness is not a valid signal there;
	// its Exec TimeoutSec bounds the run instead.
	_, streamCapable := be.(streamExecCapable)
	runLiveness := streamCapable && parser != nil && o.livenessStallTimeout > 0
	livenessDone := make(chan struct{})
	if runLiveness {
		go o.runLivenessWatcher(execCtx, spawnID, acc, o.livenessStallTimeout, execCancel, livenessDone)
	} else {
		close(livenessDone)
	}

	// On the supervised path the launcher must OUTLIVE the in-pod reaper (agent
	// timeout enforced in-pod by the reaper, plus the completion hold), so give
	// the launcher exec extra slack; otherwise it could time out and fail the
	// spawn while the reaper is still finishing the hold. Zero = unbounded.
	execTimeoutSec := req.TimeoutMinutes * 60
	if supervised && execTimeoutSec > 0 {
		execTimeoutSec += req.CompletionHoldSeconds + 60
	}

	// Use streaming exec if the substrate's backend supports it (K8s only)
	// and we have a parser; otherwise fall back to buffered Exec. The
	// harvester-vm backend doesn't implement streamExecCapable so its
	// spawns always take the buffered path.
	if sec, ok := be.(streamExecCapable); ok && parser != nil {
		usedStreaming = true
		execResult, execErr = backend.StreamExec(execCtx,
			sec.Clientset(), sec.RestConfig(), sec.Namespace(), sec.NFSFlush(),
			backend.StreamExecOpts{
				ContainerID: startResult.ContainerID,
				Command:     agentCmd,
				WorkDir:     podProjectDir,
				TimeoutSec:  execTimeoutSec,
				OnLine: func(line []byte) {
					// Stamp liveness on every streamed line: receiving output
					// proves the agent process is alive, which the parser's
					// telemetry mutations alone would not capture for lines that
					// carry no structured telemetry.
					acc.Touch()
					parser.HandleLine(line)
				},
			},
		)
	} else {
		// Fallback: buffered exec (no real-time telemetry). Pass the same
		// env that landed on the substrate at Start time. For K8s pods env
		// is already on the container so this is redundant; for harvester-vm
		// the SSH shell prefix is the delivery channel — without this the
		// agent CLI runs without spawn env (DEVBOX_BACKEND, LOOM_HUD_URL,
		// resolved SecretEnv API keys, etc.).
		execResult, execErr = be.Exec(execCtx, backend.ExecOpts{
			ContainerID: startResult.ContainerID,
			Command:     agentCmd,
			WorkDir:     podProjectDir,
			Env:         env,
			TimeoutSec:  req.TimeoutMinutes * 60,
		})
	}
	// Stop the watchers and release the exec context. cancel(nil) is a no-op
	// if a watcher already cancelled with a cause (first call wins), so the
	// liveness watchdog's errSpawnStalled survives for the finalize path.
	if req.MaxCostUSD > 0 || req.MaxTurns > 0 {
		close(watcherDone)
	}
	if runLiveness {
		close(livenessDone)
	}
	execCancel(nil)
	execSpan.End()
	heartbeatCancel()

	// Buffered (non-streaming) path — harvester-vm and any other backend that
	// is not streamExecCapable. The parser never saw the agent's output here,
	// so without this the spawn shows turn_count=0 regardless of what the agent
	// actually did, and a nonzero exit / stderr is invisible. This buffered
	// path silently swallowing the result is what hid the in-VM codex failure
	// during the Mills A2 first-autonomous-merge kill-test (empty diff,
	// turn_count=0, spawn marked "completed"). Make it observable:
	//   1. feed the captured stdout tail through the parser for best-effort
	//      telemetry (full real-time telemetry still needs a stream-capable
	//      backend — harvester-vm is not one yet),
	//   2. log a full exec summary (exit code, durations, stdout/stderr tails).
	if !usedStreaming && execResult != nil {
		if parser != nil && execResult.StdoutTail != "" {
			for _, line := range strings.Split(execResult.StdoutTail, "\n") {
				if strings.TrimSpace(line) != "" {
					parser.HandleLine([]byte(line))
				}
			}
		}
		o.logger.Info("buffered agent exec finished",
			"spawn_id", state.SpawnID,
			"agent_type", req.AgentType,
			"exit_code", execResult.ExitCode,
			"duration_ms", execResult.DurationMs,
			"stdout_lines", execResult.StdoutLines,
			"stderr_lines", execResult.StderrLines,
			"stderr_tail", execResult.StderrTail)
	}

	// Capture agent output as a context entry for session visibility. Include
	// the stderr tail when present — on a failing agent CLI that is where the
	// actionable diagnostic lives.
	if execResult != nil && (execResult.StdoutTail != "" || execResult.StderrTail != "") {
		go func() {
			truncated := execResult.StdoutTail
			if execResult.StderrTail != "" {
				truncated += "\n--- stderr ---\n" + execResult.StderrTail
			}
			if len(truncated) > 8000 {
				truncated = truncated[:8000] + "\n... (truncated)"
			}
			_ = o.agentBridge.ContextAdd("", []map[string]any{{
				"entry_type": "finding",
				"title":      fmt.Sprintf("Agent output (%s)", state.SpawnID),
				"content":    truncated,
			}})
		}()
	}

	// Step 7: Finalize based on exec result.
	if execErr != nil {
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		// Prefer the cancellation cause when the liveness watchdog tripped, so
		// the failure reads as a diagnosable stall rather than a generic
		// "context canceled" (the streaming exec surfaces watchdog cancellation
		// as a context error).
		reason := fmt.Sprintf("agent execution failed: %v", execErr)
		if cause := context.Cause(execCtx); cause != nil && errors.Is(cause, errSpawnStalled) {
			reason = cause.Error()
		}
		o.failSpawn(ctx, state, reason)
		return
	}
	// A nonzero exit from the agent CLI is a failure even though Backend.Exec
	// returns a nil Go error for command-level nonzero exits. Previously only
	// execErr was checked, so a failing agent on the buffered path fell through
	// to completeSpawn and the spawn showed as "completed" with no signal.
	if msg, failed := bufferedExecFailure(execResult); failed {
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.logger.Warn("agent CLI exited nonzero",
			"spawn_id", state.SpawnID,
			"agent_type", req.AgentType,
			"exit_code", execResult.ExitCode,
			"stderr_tail", execResult.StderrTail,
			"stdout_tail", execResult.StdoutTail)
		o.failSpawn(ctx, state, msg)
		return
	}
	// A clean process exit is not sufficient when the structured stream ended
	// mid-tool. In particular, Codex can report exit 0 after item.started while
	// omitting item.completed if its transport closes abruptly. Completing that
	// spawn would publish a false success and let Mills advance. The guard wraps
	// every parser through SpawnEventSink, so this is model-independent.
	if guardErr := spawnCompletionError(completionGuard, parser); guardErr != nil {
		if !o.spawnDriverActive(spawnID, owner) {
			return
		}
		o.failSpawn(ctx, state, guardErr.Error())
		return
	}
	if !o.spawnDriverActive(spawnID, owner) {
		return
	}
	o.completeSpawn(ctx, state)
}

// spawnCompletionError combines model-independent incomplete-tool detection
// with parser-specific terminal failures. Codex exec can exit 0 after emitting
// turn.failed, so its parser must get an opportunity to veto completion.
//
// The parser's terminal error is consulted FIRST, because the open-tool-call
// count is a SYMPTOM of an aborted turn, never its cause. A turn that fails
// mid-batch leaves every in-flight call open, and reporting "18 tool call(s)
// still open after agent process exit" as the verdict buried the actual reason
// (2026-07-26: plan_slice spawn-26e4557605de escalated with 18 open calls and
// no trace of why the turn died). Real exit reasons that arrive OUTSIDE the
// parser — a nonzero agent CLI exit, the exec deadline, the liveness watchdog —
// already win earlier in the finalizer, before this is reached.
//
// A clean turn end still sweeps as !1180 intended: turn.completed closes the
// stragglers, so the guard has nothing to report and this returns nil.
//
// A recorded stream abort is a VERDICT, not merely corroborating detail. It was
// previously consulted only when tool calls happened to be left open, so a
// stream that emitted a fatal error, completed no turn, and had nothing in
// flight reached completeSpawn as a success — the exact case the codex error
// handler already describes as "a spawn completed without producing work". The
// veto requires positive evidence: a parser reports a reason only when it
// recorded a fatal diagnostic AND nothing in the run recovered from it (codex:
// no turn completed; gemini: no terminal result). Absence of a terminal event
// on its own is NOT enough to fail here — that case keeps the older open-tool
// rule below, so this mints no failure for a quiet stream.
func spawnCompletionError(guard *completionGuardSink, parser SpawnLineParser) error {
	if reporter, ok := parser.(interface{ terminalError() error }); ok {
		if err := reporter.terminalError(); err != nil {
			return err
		}
	}
	open := guard.openToolCallCount()
	// The stream carried a fatal error and nothing recovered from it. Lead with
	// that cause, and keep the open-call count as corroborating detail when
	// there is one, instead of emitting the count alone (or nothing at all).
	if reporter, ok := parser.(interface{ streamAbortReason() string }); ok {
		if reason := strings.TrimSpace(reporter.streamAbortReason()); reason != "" {
			if open == 0 {
				return fmt.Errorf("agent stream aborted: %s", reason)
			}
			return fmt.Errorf(
				"agent stream aborted: %s (%d tool call(s) still open at stream end)",
				reason, open)
		}
	}
	if open == 0 {
		return nil
	}
	// Genuinely unexplained truncation: the process ended mid-turn with no
	// terminal event and no diagnostic. Still fails closed — completing here
	// would publish a false success and let Mills advance on partial work.
	return guard.completionError()
}

// bufferedExecFailure reports whether a buffered ExecResult represents an agent
// CLI failure (nonzero exit) and builds an operator-facing message that prefers
// the stderr tail (where the actionable diagnostic lives), falling back to the
// stdout tail. Backend.Exec returns a nil Go error for command-level nonzero
// exits, so the spawn finalizer must inspect ExitCode explicitly — not doing so
// is what let failing harvester-vm spawns report "completed" during the A2
// kill-test. Returns ("", false) for a nil result or a clean (exit 0) run.
func bufferedExecFailure(execResult *backend.ExecResult) (string, bool) {
	if execResult == nil || execResult.ExitCode == 0 {
		return "", false
	}
	msg := fmt.Sprintf("agent CLI exited %d", execResult.ExitCode)
	if s := strings.TrimSpace(execResult.StderrTail); s != "" {
		msg += ": " + s
	} else if s := strings.TrimSpace(execResult.StdoutTail); s != "" {
		msg += " (no stderr; stdout: " + s + ")"
	}
	return msg, true
}

// completionGuardSink tracks tool starts that have not received a matching
// completion while delegating the full telemetry interface to the canonical
// accumulator. Embedding keeps this guard model-independent: Claude, Codex,
// Gemini, and the SDK driver all pass through the same SpawnEventSink contract.
type completionGuardSink struct {
	SpawnEventSink

	mu       sync.Mutex
	openTool map[string]string // id → tool name, for actionable failure messages
}

func newCompletionGuardSink(delegate SpawnEventSink) *completionGuardSink {
	return &completionGuardSink{
		SpawnEventSink: delegate,
		openTool:       make(map[string]string),
	}
}

func (s *completionGuardSink) StartToolCall(id, name, serverName string) {
	s.mu.Lock()
	s.openTool[id] = name
	s.mu.Unlock()
	s.SpawnEventSink.StartToolCall(id, name, serverName)
}

func (s *completionGuardSink) CompleteToolCall(id string, durationMs int, exitCode *int, errMsg string) {
	s.SpawnEventSink.CompleteToolCall(id, durationMs, exitCode, errMsg)
	s.mu.Lock()
	delete(s.openTool, id)
	s.mu.Unlock()
}

func (s *completionGuardSink) openToolCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.openTool)
}

func (s *completionGuardSink) completionError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.openTool) == 0 {
		return nil
	}
	// Name the open calls so the escalation issue points at the actual
	// stragglers instead of forcing a Loki dig (spawn-d9889e05e2e9 took an
	// item-id diff across parser logs to identify). Sorted for determinism,
	// capped so a pathological stream cannot bloat the failure message.
	open := make([]string, 0, len(s.openTool))
	for id, name := range s.openTool {
		open = append(open, fmt.Sprintf("%s[%s]", name, id))
	}
	sort.Strings(open)
	if len(open) > 5 {
		open = append(open[:5], "…")
	}
	return fmt.Errorf(
		"agent execution incomplete: %d tool call(s) still open after agent process exit (open: %s)",
		len(s.openTool), strings.Join(open, ", "))
}

func validateCompletionHoldSeconds(seconds int) error {
	if seconds < 0 || seconds > spawn.MaxCompletionHoldSeconds {
		return fmt.Errorf(
			"completion_hold_seconds must be between 0 and %d", spawn.MaxCompletionHoldSeconds)
	}
	return nil
}

// wrapAgentCommandWithCompletionHold appends a bounded, foreground `sleep N`
// after a successful agent command. A nonzero agent status exits immediately,
// so a failed agent is never masked or held. The explicit final exit preserves
// the successful agent status after the hold; an interrupted/failed hold stays
// a failure rather than publishing a false completion.
func wrapAgentCommandWithCompletionHold(command string, seconds int) string {
	if seconds == 0 {
		return command
	}
	duration := strconv.Itoa(seconds)
	return command +
		`; loom_agent_exit=$?; ` +
		`if [ "$loom_agent_exit" -ne 0 ]; then exit "$loom_agent_exit"; fi; ` +
		`sleep ` + duration + `; loom_hold_exit=$?; ` +
		`if [ "$loom_hold_exit" -ne 0 ]; then exit "$loom_hold_exit"; fi; ` +
		`exit "$loom_agent_exit"`
}

// injectAgentConfig writes platform-specific config files into the pod.
// Uses Exec (stdout-only SPDY) instead of WriteFile (stdin SPDY) to avoid
// in-cluster SPDY stdin stream hangs observed on K3s.
//
// projectDir is the pod-internal absolute path to the project root (e.g.
// "/workspace/services/loom-core"), already resolved by the caller via
// resolveProjectPath so this function does not need to know about
// workspace bucket layouts.
//
// be is the substrate-specific backend chosen by the caller (Slice 2d):
// runSpawn picks via o.substrateBackend(req.Substrate) once per spawn,
// then threads it through to keep all backend calls on one impl.
func (o *SpawnOrchestrator) injectAgentConfig(ctx context.Context, be backend.Backend, containerID, agentType, projectDir string) error {
	writeCmd := func(dir, file, content string) error {
		encoded := base64.StdEncoding.EncodeToString([]byte(content))
		cmd := fmt.Sprintf("mkdir -p %s && echo '%s' | base64 -d > %s/%s", dir, encoded, dir, file)
		_, err := be.Exec(ctx, backend.ExecOpts{
			ContainerID: containerID,
			Command:     cmd,
			TimeoutSec:  30,
		})
		return err
	}

	// Plan Store MCP wiring: spawn pods have no loom daemon, but they can reach
	// the in-cluster agent-context WebSocket endpoint. The bundled loom binary
	// (loomBinaryCopyLines) runs `loom proxy --ws-backend <url>` as a stdio MCP
	// server so the spawned agent can call agent_plan_* (and the rest of
	// agent-context) directly against the Plan Store. Empty url ⇒ feature off.
	planStoreWSURL := resolvePlanStoreWSURL()

	// Authenticate direct git fetches of private modules so the agent can
	// `go build`/`go test` the single-repo clone without the ../../libs
	// siblings its go.work references. $GIT_TOKEN is the same token the
	// git-clone init container used (the k8s backend also exposes it on the
	// runtime container in git-clone mode); the url.insteadOf rule mirrors
	// services/loom-core/Dockerfile + .gitlab-ci.yml. Guarded on $GIT_TOKEN so
	// it is a clean no-op when git-clone mode / the token is absent (NFS-mode
	// devbox or the harvester-vm substrate). Paired with spawnGoModuleEnv,
	// which sets GOPRIVATE for the same host.
	if host := resolveSpawnGitPrivateHost(); host != "" {
		gitCred := fmt.Sprintf(
			`if [ -n "${GIT_TOKEN:-}" ]; then git config --global url."https://token:${GIT_TOKEN}@%s/".insteadOf "https://%s/"; fi`,
			host, host,
		)
		if _, err := be.Exec(ctx, backend.ExecOpts{
			ContainerID: containerID,
			Command:     gitCred,
			TimeoutSec:  30,
		}); err != nil {
			return fmt.Errorf("configure git private-module auth: %w", err)
		}
	}

	switch agentType {
	case "claude-code":
		// Claude Code reads project-level .claude/settings.json for permissions.
		// Modern Claude Code natively reads CLAUDE_CODE_OAUTH_TOKEN, which is
		// injected from the cluster setup-token secret. Do not configure an
		// apiKeyHelper here: the historical helper read a stale oauth.json mount
		// and silently fell back to API billing.
		// enableAllProjectMcpServers trusts the project .mcp.json below without
		// an interactive approval prompt (headless spawn).
		settings := `{"permissions":{"allow":["Bash","Read","Write","Edit","Glob","Grep"]},"enableAllProjectMcpServers":true}`
		if err := writeCmd(projectDir+"/.claude", "settings.json", settings); err != nil {
			return fmt.Errorf("write claude settings: %w", err)
		}
		// Project-root .mcp.json is auto-loaded by claude-code; the loom proxy
		// bridges to the in-cluster Plan Store over WebSocket.
		if planStoreWSURL != "" {
			mcpJSON := loomMCPServerJSON(planStoreWSURL)
			if err := writeCmd(projectDir, ".mcp.json", mcpJSON); err != nil {
				return fmt.Errorf("write claude .mcp.json: %w", err)
			}
		}
	case "codex":
		// Codex reads ~/.codex/config.toml for sandbox + multi-agent features
		// and ~/.codex/auth.json for OAuth (falling back to $OPENAI_API_KEY).
		// Because the auth.json is a read-only secret volume mount staged at
		// /home/agent/.codex.auth/, we symlink it into the writable
		// /home/agent/.codex/ directory so Codex CLI can read it at its
		// native path. The symlink transparently reflects kubelet-propagated
		// secret updates (e.g., refreshed OAuth tokens written by
		// mcp-auth-refresher).
		//
		// The loom binary is bundled into the spawn image (loomBinaryCopyLines),
		// so the [mcp_servers.loom] block below gives the codex agent a stdio
		// MCP server that bridges to the in-cluster Plan Store over WebSocket —
		// closing the long-standing TODO. `loom proxy --ws-backend` exposes
		// agent_plan_* (and the rest of agent-context) with un-namespaced names.
		config := `[agent]
approval = "auto-edit"

[sandbox]
mode = "workspace-write"
network_access = true

[features]
multi_agent = true
collaboration_modes = true
unified_exec = true
`
		if planStoreWSURL != "" {
			config += "\n" + loomMCPServerTOML(planStoreWSURL)
		}
		if err := writeCmd(AgentHomeDir+"/.codex", "config.toml", config); err != nil {
			return fmt.Errorf("write codex config: %w", err)
		}
		// Best-effort symlink; pipe "true" at the end so the exec doesn't
		// fail when the auth mount is absent (API-key-only operators).
		linkCmd := "ln -sf " + AgentHomeDir + "/.codex.auth/auth.json " + AgentHomeDir + "/.codex/auth.json 2>/dev/null || true"
		if _, err := be.Exec(ctx, backend.ExecOpts{
			ContainerID: containerID,
			Command:     linkCmd,
			TimeoutSec:  10,
		}); err != nil {
			return fmt.Errorf("link codex auth.json: %w", err)
		}
	case "gemini":
		// Gemini reads ~/.gemini/settings.json for permissions. The Google
		// Auth Library auto-detects GOOGLE_APPLICATION_CREDENTIALS; that env
		// var is set at pod-start time in runSpawn(), pointing at the
		// service-account JSON mounted from the cluster secret. If the SA
		// JSON key is absent, the file is missing and Gemini falls back to
		// GEMINI_API_KEY env.
		// Gemini reads ~/.gemini/settings.json mcpServers for MCP servers; the
		// loom proxy bridges to the in-cluster Plan Store over WebSocket.
		settings := `{"permissions":{"allow_all":true}}`
		if planStoreWSURL != "" {
			settings = `{"permissions":{"allow_all":true},` + loomMCPServerJSONInner(planStoreWSURL) + `}`
		}
		if err := writeCmd(AgentHomeDir+"/.gemini", "settings.json", settings); err != nil {
			return fmt.Errorf("write gemini settings: %w", err)
		}
	}
	return nil
}

// defaultPlanStoreWSURL is the in-cluster WebSocket MCP endpoint of the
// agent-context server (the Loom Plan Store backend). Spawn pods have no local
// loom daemon and no Unix socket, but they CAN reach this ClusterIP service.
// The agent-context server speaks plain MCP over WebSocket at /ws (the
// deployment sets MCP_TRANSPORT=websocket on :8080); a `loom proxy
// --ws-backend <url>` bridge in the pod exposes its tools (incl. agent_plan_*)
// to the spawned agent over stdio. Overridable via SPAWN_PLAN_STORE_WS_URL so
// a cluster/namespace move is an env flip, not a rebuild.
const defaultPlanStoreWSURL = "ws://mcp-agent-context.loom-hub.svc.cluster.local:8080/ws"

// defaultSpawnLoomImage is the image the generated spawn Dockerfile copies the
// `loom` binary from (loom-core ships it at /usr/local/bin/loom). It is a
// build-time COPY --from source for the ephemeral spawn-runtime image, NOT a
// running workload, so the `:latest` fallback here is acceptable; the operator
// deployment SHOULD set SPAWN_LOOM_IMAGE to its own Flux-pinned loom-core tag
// (e.g. registry.harbor.lan/mcp/loom-core:20260625-013914) so the bundled loom
// stays version-aligned with the daemon.
const defaultSpawnLoomImage = "registry.harbor.lan/mcp/loom-core:latest"

// resolvePlanStoreWSURL returns the agent-context WebSocket MCP URL injected
// into spawn agent configs. Empty SPAWN_PLAN_STORE_WS_URL falls back to the
// in-cluster default; an explicit value of "disabled" (case-insensitive)
// suppresses plan-store MCP wiring entirely (returns "").
func resolvePlanStoreWSURL() string {
	v := strings.TrimSpace(os.Getenv("SPAWN_PLAN_STORE_WS_URL"))
	if v == "" {
		return defaultPlanStoreWSURL
	}
	if strings.EqualFold(v, "disabled") || strings.EqualFold(v, "off") {
		return ""
	}
	return v
}

// resolveSpawnLoomImage returns the image to COPY the `loom` binary from when
// building the spawn-runtime image. SPAWN_LOOM_IMAGE overrides the default so
// the operator can pin its own loom-core tag without a code change.
func resolveSpawnLoomImage() string {
	if v := strings.TrimSpace(os.Getenv("SPAWN_LOOM_IMAGE")); v != "" {
		return v
	}
	return defaultSpawnLoomImage
}

// defaultCodexModel is the codex model used for spawn exec when
// SPAWN_CODEX_MODEL is unset. codex 0.120.0+ defaults to gpt-5.3-codex, which
// OpenAI DEPRECATED for ChatGPT-account (sign-in-with-ChatGPT) auth — so
// `codex exec` with no --model fails with HTTP 400 "The 'gpt-5.3-codex' model
// is not supported when using Codex with a ChatGPT account." (Mills A2
// kill-test, 2026-06-06: codex authenticated and started a turn, then the API
// rejected the default model). gpt-5.5 is the current model available with
// ChatGPT sign-in per https://developers.openai.com/codex/models (fetched
// 2026-06-06; supported there: gpt-5.5, gpt-5.4, gpt-5.4-mini; ChatGPT-Pro
// only: gpt-5.3-codex-spark; deprecated: gpt-5.2, gpt-5.3-codex).
const defaultCodexModel = "gpt-5.5"

// resolveCodexModel returns the codex model id to pin on `codex exec --model`.
// Precedence (highest first):
//
//  1. reqModel — the per-spawn model from spawn.Request.Model (Mills wires this
//     from policy stage_models / LOOM_MILLS_SPAWN_MODEL). This is how the
//     implement stage runs gpt-5.6-terra while plan_slice runs gpt-5.6-sol
//     without a global env flip.
//  2. SPAWN_CODEX_MODEL env — the orchestrator-wide override, so operators can
//     retune WITHOUT a rebuild the next time OpenAI shifts the ChatGPT-supported
//     model set (the failure class that broke the A2 kill-test).
//  3. defaultCodexModel — the compiled-in ChatGPT-account-safe fallback.
func resolveCodexModel(reqModel string) string {
	if m := strings.TrimSpace(reqModel); m != "" {
		return m
	}
	if m := strings.TrimSpace(os.Getenv("SPAWN_CODEX_MODEL")); m != "" {
		return m
	}
	return defaultCodexModel
}

// buildAgentCommand constructs the CLI command to run the agent headlessly.
//
// The returned string is executed via `sh -c` (see backend.StreamExec /
// K8sBackend.Exec), so the prompt MUST be shell-quoted, not Go-quoted. Go's
// %q yields a *Go* string literal, which leaves shell metacharacters live:
// backticks and $(...) inside the prompt are evaluated by the shell as
// command substitution before the agent CLI ever sees them. The Mills canary
// SpecDoc wraps the fixture path and backlog id in backticks
// (`testdata/mills-canary/heartbeat.md`, `MILLS-CANARY-...`), so a %q prompt
// produced `sh: testdata/mills-canary/heartbeat.md: Permission denied` /
// `sh: MILLS-CANARY-...: not found` and a prompt with those spans silently
// stripped — the plan_slice stage that blocked the first autonomous merge.
// shellQuote wraps the prompt in single quotes (with '\” escaping) so every
// metacharacter is passed through literally. The buildSDKDriverCommand path
// already uses shellQuote; this is the legacy CLI path catching up.
// defaultClaudeCodeMaxTurns caps a claude-code spawn when the request carries
// no turn budget. The previous hard-coded 50 was exhausted by review-stage
// agents that launch long test suites in the background and then poll with
// cheap one-command turns until the cap kills them mid-wait (2026-08-08:
// three pr_self_review spawns died at error_max_turns ~$1.5-1.9 apiece,
// the entire claude-code backend error count for that day).
const defaultClaudeCodeMaxTurns = 100

// buildAgentCommand builds the headless CLI command. model is the vendor-native
// LLM model id from spawn.Request.Model; empty means "use the vendor default".
// Codex and Claude Code consume it using their native headless --model flags;
// Gemini currently has no model flag in this invocation. maxTurns caps the
// claude-code agent loop; <=0 applies defaultClaudeCodeMaxTurns.
func buildAgentCommand(agentType, task, agentID, model string, maxTurns int) string {
	switch agentType {
	case "claude-code":
		// stream-json emits one JSONL event per line for real-time telemetry parsing.
		// --verbose is mandatory: claude-code 1.x rejects `-p` + `--output-format
		// stream-json` without it ("Error: When using --print, --output-format=
		// stream-json requires --verbose"). Without --verbose the CLI prints
		// that one line and exits 0 *without making any API call*, which is
		// why every Mills spawn showed turn_count=0 / cost=$0 / file_changes=0.
		modelArg := ""
		if m := strings.TrimSpace(model); m != "" {
			modelArg = " --model " + shellQuote(m)
		}
		if maxTurns <= 0 {
			maxTurns = defaultClaudeCodeMaxTurns
		}
		return fmt.Sprintf(`claude -p %s --dangerously-skip-permissions --output-format stream-json --verbose --max-turns %d%s`, shellQuote(task), maxTurns, modelArg)
	case "codex":
		// Wrap with EXIT trap so loom session-end fires even without a native hook.
		// The trap is best-effort: if the loom binary is not in the pod PATH,
		// stderr is suppressed via 2>/dev/null and the HUD-side completeSpawn /
		// failSpawn will still call EndSession as a fallback.
		//
		// --dangerously-bypass-approvals-and-sandbox is the codex equivalent of
		// claude-code's --dangerously-skip-permissions: it bypasses BOTH the
		// approval prompts (default approval="auto-edit" auto-approves file
		// edits but still asks for every shell command — git add/commit/push,
		// go test, etc.) AND the network-blocking workspace-write sandbox.
		// Without both bypasses, Mills' implement stage produced empty MRs:
		//   - --sandbox workspace-write alone: file edits land but `git push`
		//     hangs/fails because shell commands need approval AND network.
		//   - --sandbox danger-full-access alone: sandbox is open but codex
		//     still pauses for approval on every shell command in a headless
		//     pod where no human is around to type "yes".
		// The pod itself is already isolated by Kubernetes and runs with a
		// project-scoped GIT_TOKEN; the "EXTREMELY DANGEROUS" warning in the
		// flag's help text refers to running codex on a developer workstation,
		// not in an ephemeral spawn pod. See ab6f8446 / 5742ae07 / 46418c9f
		// for the earlier coupled fixes.
		// --skip-git-repo-check lets codex run in the /workspace clone where
		// the .git directory might be at the working dir rather than a parent.
		//
		// `< /dev/null` is REQUIRED. codex 0.120.0+ `exec` with a prompt arg
		// still inspects stdin: when stdin is a non-TTY pipe that is open but
		// never written/closed, codex reports "Reading additional input from
		// stdin..." and reads it as additional prompt input — then hangs (or,
		// under a session whose stdin EOFs oddly, exits 1) instead of running
		// the single turn. BOTH spawn exec paths leave stdin in exactly that
		// state — the K8s StreamExec sets PodExecOptions{Stdin:false} and the
		// harvester-vm SSH session leaves session.Stdin nil — so without this
		// redirect codex starts a thread + turn then dies with turn_count=1 and
		// no diff. This is the agent-execution gap the Mills A2 kill-test hit on
		// the VM path (and the latent cause of codex's empty implements on k8s).
		// Refs openai/codex#20919; the prompt is already passed as an arg, so no
		// real stdin is needed.
		//
		// `--model` is REQUIRED. codex 0.120.0+ `exec` with no --model uses its
		// CLI default (gpt-5.3-codex), which OpenAI deprecated for ChatGPT-account
		// auth: the Mills A2 kill-test (2026-06-06) saw codex authenticate and
		// start a turn, then fail with HTTP 400 "The 'gpt-5.3-codex' model is not
		// supported when using Codex with a ChatGPT account." resolveCodexModel
		// pins a ChatGPT-supported model (default gpt-5.5) with a SPAWN_CODEX_MODEL
		// env override so a future model-set shift is an env flip, not a rebuild.
		//
		// task is shell-quoted (not Go %q): the result runs via `sh -c`, so
		// backticks / $(...) / $VAR in the prompt would otherwise be evaluated by
		// the shell before codex sees them — the canary's backtick-wrapped
		// fixture path + backlog id died as `sh: ...: not found`. agentID stays
		// %q: it sits inside the trap's single-quoted body and is a controlled
		// identifier, never user text. The model is shell-quoted too (defensive;
		// it is a controlled identifier, never user text).
		//
		// codexAuthPreflight runs between the trap and the CLI: codex 0.144
		// with NO usable credential still starts a thread + turn and then spews
		// `401 Unauthorized: Missing bearer or basic authentication in header`
		// retries before exiting 1 at $0 — and because the always-printed
		// "Reading additional input from stdin..." line dominates the stdout
		// tail, the failure was binned as spawn-stdin-misconfig (escalation
		// #368, 2026-07-22: fresh spawn pods during the 01:54Z fleet-rollout
		// window ran codex against a dangling ~/.codex/auth.json while the
		// SAME secret+image+argv worked before and after the window). The
		// guard runs in-pod at turn time — on the direct exec path AND inside
		// the S4 supervisor's hold.sh — so every attempt re-checks the mount.
		return fmt.Sprintf(
			`trap 'loom agent session-end --agent-id %q --summarize --summary-async --quiet 2>/dev/null' EXIT; %s; codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --model %s --json %s < /dev/null`,
			agentID, codexAuthPreflight(AgentHomeDir), shellQuote(resolveCodexModel(model)), shellQuote(task),
		)
	case "gemini":
		return fmt.Sprintf(`gemini -p %s --yolo --output-format stream-json`, shellQuote(task))
	default:
		return fmt.Sprintf(`echo "Unsupported agent type: %s"`, agentType)
	}
}

// codexAuthPreflight returns the POSIX-sh guard that fails a codex spawn fast
// — BEFORE codex starts a turn — when the pod holds no usable credential.
//
// The codex CLI reads ~/.codex/auth.json (injectAgentConfig symlinks it to the
// optional cluster-agent-auth codex-auth-json secret mount) and falls back to
// $OPENAI_API_KEY. Both sources are k8s-Optional by design so a pod always
// starts — but that means an absent/unpopulated mount leaves a DANGLING
// symlink, and codex then runs a full unauthenticated turn: 401 "Missing
// bearer or basic authentication in header" spew, exit 1, $0.00, with the
// misleading "Reading additional input from stdin..." line as the visible
// stdout tail (escalation #368; the 2026-07-22 01:54Z rollout-window pods).
// `[ -s ]` follows the symlink, so a dangling link, a missing file, and an
// empty (unpopulated-key) file all trip the guard. Exit 78 (EX_CONFIG) is an
// honest nonzero for runSpawn's finalizer and does not collide with the S4
// launcher sentinels (231/232/233). The message is the classifier contract:
// pkg/mills/pipeline/spawn_class.go matches "codex auth preflight failed" to
// tag the failure spawn-auth-missing (retryable infra with a rollout-window
// backoff) instead of the stdin-misconfig catch-all.
func codexAuthPreflight(home string) string {
	authPath := home + "/.codex/auth.json"
	return `if [ ! -s ` + authPath + ` ] && [ -z "${OPENAI_API_KEY:-}" ]; then ` +
		`echo "codex auth preflight failed: ` + authPath + ` missing or empty and OPENAI_API_KEY unset ` +
		`(is the cluster-agent-auth secret codex-auth-json key populated and mounted?)" >&2; exit 78; fi`
}

// StopSpawn stops a running spawned agent.
func (o *SpawnOrchestrator) StopSpawn(ctx context.Context, spawnID string) error {
	if o == nil || o.ctrl == nil {
		return fmt.Errorf("spawn %s not found", spawnID)
	}

	var (
		owner          *spawnDriverOwner
		driverDone     <-chan struct{}
		cleanupDone    chan struct{}
		performCleanup bool
		terminalWinner bool
	)
	o.driversMu.Lock()
	state, disposition, err := o.ctrl.BeginStop(ctx, spawnID)
	if err != nil {
		o.driversMu.Unlock()
		return err
	}
	if disposition == spawn.StopTerminal {
		// Preserve the terminal winner, but still perform idempotent backend
		// cleanup for a retained pod handle before returning.
		terminalWinner = true
		performCleanup = true
	} else if owner = o.drivers[spawnID]; owner != nil {
		driverDone = owner.done
		if !owner.stopRequested {
			owner.stopRequested = true
			owner.stopCleanupDone = make(chan struct{})
			cleanupDone = owner.stopCleanupDone
			performCleanup = true
			owner.cancel(errSpawnStopped)
		}
	} else {
		// A durable intent recovered without an in-process driver owns its own
		// cleanup retry (including a prior failed attempt).
		performCleanup = true
	}
	podName := state.PodName
	if podName == "" {
		podName = "spawn-" + spawnID
	}
	substrate := state.Request.Substrate
	var cleanupErr error
	if performCleanup && !terminalWinner {
		// Retain the deterministic handle before delete, while still holding
		// driversMu: a late Start result is only observed after
		// updateSpawnFromDriver takes this lock, so the authoritative handle
		// recorded by cleanupLateSpawn always lands after this fallback record
		// and is never overwritten by it.
		_, _, cleanupErr = o.ctrl.RecordStoppingPod(ctx, spawnID, podName)
	}
	o.driversMu.Unlock()

	if performCleanup {
		be := o.substrateBackend(substrate)
		switch {
		case be == nil:
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stop spawn pod %s: no substrate backend", podName))
		case podName != "":
			if stopErr := o.stopSpawnRuntime(ctx, be, &state, podName); stopErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stop spawn pod %s: %w", podName, stopErr))
			}
		}
		if cleanupErr != nil {
			if !terminalWinner {
				// Record no pod handle here: the durable handle may already be
				// the authoritative late-Start container recorded by
				// cleanupLateSpawn, and re-recording the fallback would lose
				// that retry handle.
				_, _, persistFailureErr := o.ctrl.RecordStopCleanupFailure(ctx, spawnID, "", cleanupErr.Error())
				cleanupErr = errors.Join(cleanupErr, persistFailureErr)
				o.setStopCleanupError(spawnID, owner, cleanupErr)
			}
			if o.logger != nil {
				o.logger.Warn("failed to stop spawn pod", "spawn_id", spawnID, "pod", podName, "error", cleanupErr)
			}
		}
		if cleanupDone != nil {
			close(cleanupDone)
		}
	}
	if terminalWinner {
		return cleanupErr
	}

	if driverDone != nil {
		timer := time.NewTimer(spawnDriverStopTimeout)
		defer timer.Stop()
		select {
		case <-driverDone:
		case <-ctx.Done():
			return fmt.Errorf("wait for spawn %s driver exit: %w", spawnID, ctx.Err())
		case <-timer.C:
			return fmt.Errorf("wait for spawn %s driver exit: exceeded %s", spawnID, spawnDriverStopTimeout)
		}
		if owner.stopCleanupErr != nil {
			return owner.stopCleanupErr
		}
		return nil
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	if stopped, ok, err := o.ctrl.CompleteStop(ctx, spawnID); err != nil {
		return err
	} else if ok {
		o.finishStoppedSpawn(ctx, &stopped)
	}
	return nil
}

// SendControlMessage appends a control command to a running multi-turn
// spawn's JSONL control file. The spawn-driver's tail loop picks up the new
// line within ~200ms (fs.watch + poll fallback) and dispatches it to the
// active SDK Query or Codex Thread.
//
// Errors are returned as wrapped sentinels so REST handlers can distinguish
// 404 (not found), 409 (not running), and 400 (not multi-turn / invalid
// command) from 5xx backend failures:
//
//   - spawn.ErrSpawnNotFound         → 404
//   - spawn.ErrSpawnNotRunning       → 409
//   - spawn.ErrSpawnNotMultiTurn     → 400
//   - spawn.ErrInvalidControlCommand → 400
//
// Any other error is a backend/exec failure and should surface as 500.
func (o *SpawnOrchestrator) SendControlMessage(ctx context.Context, spawnID string, cmd spawn.ControlCommand) error {
	if err := validateControlCommand(cmd); err != nil {
		return err
	}

	state, ok := o.ctrl.Get(spawnID)
	if !ok {
		return fmt.Errorf("%w: %s", spawn.ErrSpawnNotFound, spawnID)
	}

	if state.Status != SpawnStatusRunning {
		return fmt.Errorf("%w: %s is %s", spawn.ErrSpawnNotRunning, spawnID, state.Status)
	}

	if !state.Request.MultiTurn || !state.Request.UseSDKDriver {
		return fmt.Errorf("%w: %s was not spawned with multi_turn=true", spawn.ErrSpawnNotMultiTurn, spawnID)
	}

	if state.PodName == "" {
		return fmt.Errorf("spawn %s has no pod name; cannot inject control command", spawnID)
	}

	if err := o.injectControlMessage(ctx, o.substrateBackend(state.Request.Substrate), state.PodName, spawnID, cmd); err != nil {
		return fmt.Errorf("inject control command for %s: %w", spawnID, err)
	}

	o.logger.Info("injected spawn control command",
		"spawn_id", spawnID,
		"agent_id", state.AgentID,
		"command_type", cmd.Type,
	)
	return nil
}

// validateControlCommand enforces the driver contract: Type must be one of
// the known discriminators and "message" requires non-empty Text.
func validateControlCommand(cmd spawn.ControlCommand) error {
	switch cmd.Type {
	case spawn.ControlCommandMessage:
		if strings.TrimSpace(cmd.Text) == "" {
			return fmt.Errorf("%w: message text is required", spawn.ErrInvalidControlCommand)
		}
		return nil
	case spawn.ControlCommandInterrupt, spawn.ControlCommandShutdown:
		return nil
	case "":
		return fmt.Errorf("%w: type is required", spawn.ErrInvalidControlCommand)
	default:
		return fmt.Errorf("%w: unknown type %q", spawn.ErrInvalidControlCommand, cmd.Type)
	}
}

// failSpawn marks a spawn as failed, cleans up the K8s pod, and broadcasts the event.
func (o *SpawnOrchestrator) failSpawn(ctx context.Context, state *SpawnState, reason string) {
	if o == nil || o.ctrl == nil || state == nil {
		return
	}
	var terminalTelemetry *bridge.SpawnTelemetry
	if accVal, exists := o.telemetry.Load(state.SpawnID); exists {
		acc := accVal.(*bridge.SpawnTelemetryAccumulator)
		snap := acc.Snapshot()
		terminalTelemetry = &snap
	}
	o.driversMu.Lock()
	updated, ok, persistErr := o.ctrl.UpdateUnlessStoppingOrTerminal(ctx, state.SpawnID, func(current *spawn.State) {
		// Attach partial telemetry snapshot (valuable for debugging failures).
		if terminalTelemetry != nil {
			current.Telemetry = terminalTelemetry
		}
		current.Status = SpawnStatusFailed
		current.Error = reason
		now := time.Now()
		current.EndedAt = &now
	})
	o.driversMu.Unlock()
	if persistErr != nil {
		o.logger.Error("failed to persist terminal spawn failure",
			"spawn_id", state.SpawnID, "reason", reason, "error", persistErr)
		return
	}
	if !ok {
		return
	}
	o.telemetry.Delete(state.SpawnID)
	state = &updated
	podName := state.PodName

	// Persist final telemetry summary to the agent-context session.
	o.persistTelemetrySummary(state, string(SpawnStatusFailed))

	// Clean up the pod/VM on the substrate it was created on.
	if podName != "" {
		if err := o.stopSpawnRuntime(ctx, o.substrateBackend(state.Request.Substrate), state, podName); err != nil {
			o.logger.Warn("failed to clean up pod on spawn failure",
				"spawn_id", state.SpawnID, "pod", podName, "error", err)
		}
	}

	if o.metrics != nil {
		if _, owned := o.activeSpawnMetrics.LoadAndDelete(state.SpawnID); owned {
			o.metrics.SpawnedAgentActive.Add(ctx, -1)
		}
		o.metrics.AgentSpawnTotal.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("agent_type", state.Request.AgentType),
				attribute.String("outcome", "failed"),
			),
		)
	}

	// Record partial spawn telemetry metrics (still valuable for debugging failures).
	if o.metrics != nil && state.Telemetry != nil {
		o.recordSpawnTelemetryMetrics(ctx, state)
	}

	o.logger.Error("spawn failed", "spawn_id", state.SpawnID, "reason", reason)
	o.broadcastSpawnEvent("agent.spawn.failed", state)

	// Record failure and end the agent session. Resolve the spawn's session
	// so agent_context_add lands the error entry instead of being rejected
	// for an empty session_id (skip the write when no session resolves).
	failSessionID := o.resolveSpawnSessionID(state)
	if o.agentBridge != nil {
		go func() {
			if failSessionID != "" {
				_ = o.agentBridge.ContextAdd(failSessionID, []map[string]any{{
					"entry_type": "error",
					"title":      "Spawn failed: " + state.SpawnID,
					"content":    reason,
				}})
			}
			summarize := false
			o.agentBridge.EndSession(bridge.SessionEndParams{AgentID: state.AgentID, Summarize: &summarize})
		}()
	}
}

// DeleteSpawn removes a terminal spawn from the controller and persistent store.
func (o *SpawnOrchestrator) DeleteSpawn(ctx context.Context, spawnID string) error {
	return o.ctrl.Delete(ctx, spawnID)
}

// ListSpawns returns all spawn states.
func (o *SpawnOrchestrator) ListSpawns() []*SpawnState {
	return o.ctrl.List()
}

// GetSpawn returns a specific spawn state.
func (o *SpawnOrchestrator) GetSpawn(spawnID string) (*SpawnState, bool) {
	return o.ctrl.Get(spawnID)
}

// Wait blocks until the given spawn reaches a terminal state
// (completed / failed / stopped) or ctx is canceled. Returns the terminal
// SpawnState on success; ctx.Err() on cancellation; an error if the spawn
// ID does not exist.
//
// Implemented via polling (spawn.IsTerminal) every waitPollInterval rather
// than subscribing to the SSE hub because the hub's fan-out shape doesn't
// let a single waiter filter by spawn_id without delivery contention with
// browser clients. Polling is cheap — spawn state lookups are O(1) map
// reads — and spawn lifecycles are measured in minutes, so 500ms poll
// granularity is invisible to callers.
func (o *SpawnOrchestrator) Wait(ctx context.Context, spawnID string) (*SpawnState, error) {
	if _, ok := o.ctrl.Get(spawnID); !ok {
		return nil, fmt.Errorf("spawn %s not found", spawnID)
	}

	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()
	for {
		state, ok := o.ctrl.Get(spawnID)
		if !ok {
			return nil, fmt.Errorf("spawn %s disappeared while waiting", spawnID)
		}
		if spawn.IsTerminal(state.Status) {
			return state, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// waitPollInterval is the cadence at which Wait() re-checks terminal
// state. Tuned for the minute-scale spawn lifecycle; low enough that
// Wait returns quickly after completion, high enough to keep polling
// overhead near zero.
const waitPollInterval = 500 * time.Millisecond

// Projects returns the configured project list for spawn pickers.
func (o *SpawnOrchestrator) Projects() []string { return o.projects }

// gitCloneMode reports whether the spawn backend clones the repo into the pod
// (as opposed to relying on an nfs/tar-pipe workspace mount). In git-clone mode
// the pod's init container is the repo's source of truth, so project resolution
// can fall back to a lexical path when the repo is absent from the workspace
// mount (see runSpawn). Empty syncMode is treated as non-git-clone so tests and
// legacy wirings keep the strict hard-fail behavior.
func (o *SpawnOrchestrator) gitCloneMode() bool { return o.syncMode == "git-clone" }

// NewSpawnOrchestratorForTest builds a minimal SpawnOrchestrator backed by
// the given controller. Intended for external-package tests that need
// ListSpawns / GetSpawn / Wait but can't construct a full orchestrator
// because backend/sseHub/etc. fields are unexported. Do not use in
// production code paths.
func NewSpawnOrchestratorForTest(ctrl *spawn.K8sController) *SpawnOrchestrator {
	return &SpawnOrchestrator{ctrl: ctrl, logger: slog.Default()}
}

// Cluster-scoped secret names. These hold credentials tied to the cluster
// identity, decoupled from any developer's Mac Keychain. See
// .loom/87-product-spec-session-spawning-weaver-2026-04-19.md §AUTH.
const (
	// ClusterAgentAPIKeysSecret holds vendor API keys (ANTHROPIC_API_KEY,
	// OPENAI_API_KEY, GEMINI_API_KEY) and the Gemini service-account JSON,
	// all scoped to the cluster's identity.
	ClusterAgentAPIKeysSecret = "cluster-agent-api-keys"

	// ClusterAgentAuthSecret holds cluster-owned OAuth tokens for agents
	// that support subscription auth (Claude, Codex). Populated by
	// `loom auth cluster-login`; refreshed in-cluster by mcp-auth-refresher.
	// Unused in Slice 2a (API-key only); Slice 2b adds the OAuth mounts.
	ClusterAgentAuthSecret = "cluster-agent-auth"

	// GeminiSAKeyName is the Secret key that holds the full Google service
	// account JSON for Gemini. When present it is mounted as a file at
	// GeminiSAMountPath/sa.json so Gemini CLI can pick it up via the
	// standard GOOGLE_APPLICATION_CREDENTIALS env var.
	GeminiSAKeyName   = "GOOGLE_APPLICATION_CREDENTIALS_JSON"
	GeminiSAMountPath = "/home/agent/.gcp"
	GeminiSAFilename  = "sa.json"

	// AgentHomeDir is the writable HOME for spawned agents. The runtime
	// image creates user `agent` (uid 1000) with this as its home; secret
	// mounts and CLI state files all live under here so the non-root
	// process can traverse the path. Stay in sync with
	// agentRuntimeDockerfile().
	AgentHomeDir = "/home/agent"
)
