package hud

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

// streamExecCapable is satisfied by *backend.K8sBackend. It provides the
// low-level K8s client/config needed by backend.StreamExec.
type streamExecCapable interface {
	Clientset() kubernetes.Interface
	RestConfig() *rest.Config
	Namespace() string
	NFSFlush() bool
}

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
