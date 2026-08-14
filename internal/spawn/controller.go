package spawn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// Controller defines the interface for managing spawn lifecycles.
type Controller interface {
	Spawn(ctx context.Context, req Request) (string, error)
	Stop(ctx context.Context, spawnID string) error
	Get(spawnID string) (*State, bool)
	List() []*State
	Reconcile(ctx context.Context) error
}

// ErrSpawnCleanupPending means a terminal spawn still owns cleanup work. Its
// durable row is the generation lock that prevents a same-key replacement from
// reusing the deterministic AgentID while the old cleanup hook is in flight.
var ErrSpawnCleanupPending = errors.New("spawn terminal cleanup is pending")

// TerminalHook is fired by Reconcile for a spawn that has reached a terminal
// Status. Implementations release K8s, HUD, and agent-context resources. A
// non-nil error keeps CleanupAt unset so cleanup is retried on the next tick.
type TerminalHook func(ctx context.Context, state State) error

// StoppingHook retries cleanup for a durable non-terminal stop intent. An
// error leaves the record active and causes another attempt on the next
// Reconcile tick.
type StoppingHook func(ctx context.Context, state State) error

// K8sController implements Controller using Kubernetes pods as the source of
// truth. On each Reconcile cycle it lists pods by the managed-by label and
// updates the in-memory state map accordingly, eliminating the "stale after
// restart" bug where local JSON files diverged from actual pod status.
type K8sController struct {
	client    kubernetes.Interface
	namespace string
	store     Store
	logger    *slog.Logger

	mu     sync.RWMutex
	spawns map[string]*State
	// keyedRegistration serializes one deterministic ID without holding the
	// global lifecycle map lock across durable Kubernetes I/O. Independent
	// keys use independent stripes, so a slow ConfigMap request for one spawn
	// does not head-of-line block admission for the whole controller.
	keyedRegistration [256]sync.Mutex
	// driverOwnerID is a stable logical recovery domain. Only rows bearing
	// this ID enter the mutable lifecycle map. recoveryAuthority is reserved
	// for the one controller allowed to claim pre-ownership rows and unlabeled
	// rowless pods during migration.
	driverOwnerID     string
	recoveryAuthority bool
	// peerSpawns caches live pod IDs whose durable rows belong to another
	// controller. It avoids re-reading the full shared ConfigMap every reconcile
	// tick while deliberately keeping those spawns out of the lifecycle map.
	peerSpawns map[string]struct{}

	// terminalHook, when non-nil, is invoked by Reconcile for every
	// spawn whose Status is terminal and whose CleanupAt has not yet
	// been stamped. The hook runs outside the controller's lock; it
	// must be safe to invoke concurrently with other controller calls.
	terminalHook TerminalHook
	stoppingHook StoppingHook
}

// ControllerOption configures a K8sController without breaking legacy
// in-memory/FileStore callers that do not share state with peer controllers.
type ControllerOption func(*K8sController)

// WithControllerOwnership fences shared-store lifecycle ownership. ownerID
// must remain stable across replacement restarts and unique among concurrently
// running controllers. Exactly one shared-store controller may be the recovery
// authority for legacy ownerless rows and genuinely rowless orphan pods.
func WithControllerOwnership(ownerID string, recoveryAuthority bool) ControllerOption {
	return func(c *K8sController) {
		c.driverOwnerID = strings.TrimSpace(ownerID)
		c.recoveryAuthority = recoveryAuthority
	}
}

// NewK8sController creates a new K8s-native spawn controller.
func NewK8sController(client kubernetes.Interface, namespace string, store Store, logger *slog.Logger, opts ...ControllerOption) *K8sController {
	if logger == nil {
		logger = slog.Default()
	}
	c := &K8sController{
		client:     client,
		namespace:  namespace,
		store:      store,
		logger:     logger.With("component", "spawn-controller"),
		spawns:     make(map[string]*State),
		peerSpawns: make(map[string]struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// SetK8sClient injects a Kubernetes client and namespace after construction.
// This is used when the controller is created before the K8s backend is fully
// initialised (e.g., in the HUD startup sequence where the backend clientset
// is only available after NewK8sBackend succeeds).
func (c *K8sController) SetK8sClient(client kubernetes.Interface, namespace string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = client
	c.namespace = namespace
}

// Ownership reports the logical shared-store owner and whether this process
// is allowed to adopt missing legacy identity labels.
func (c *K8sController) Ownership() (ownerID string, recoveryAuthority bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.driverOwnerID, c.recoveryAuthority
}

// LoadDurable returns the current store row without installing it into the
// mutable lifecycle map. It is used by runtime preflight before registration.
func (c *K8sController) LoadDurable(ctx context.Context, spawnID string) (*State, error) {
	if c.store == nil {
		return nil, nil
	}
	state, err := c.store.Load(ctx, spawnID)
	if err != nil || state == nil {
		return state, err
	}
	return cloneStateForRead(state), nil
}

// SetTerminalHook installs the cleanup hook fired by Reconcile when a
// spawn reaches a terminal Status. Passing nil clears the hook.
func (c *K8sController) SetTerminalHook(h TerminalHook) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.terminalHook = h
}

// SetStoppingHook installs the retry hook for durable stop intents.
func (c *K8sController) SetStoppingHook(h StoppingHook) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stoppingHook = h
}

// Spawn records a new spawn in the in-memory map and persistent store. The
// actual pod creation is left to the caller (the HUD orchestrator or an
// external workflow) because it requires the devbox backend, Dockerfile
// generation, config injection, and session registration -- concerns that
// belong to the orchestration layer, not the controller.
//
// Returns the generated spawn ID.
func (c *K8sController) Spawn(ctx context.Context, req Request) (string, error) {
	spawnID, _, err := c.Register(ctx, req)
	return spawnID, err
}

// Register records a spawn and reports whether this caller owns the first
// dispatch. Keyed registration consults durable state before installing a
// Pending row, extending idempotency across controller processes. A false
// dispatch means an existing local registration was reattached and must not
// launch another lifecycle driver.
func (c *K8sController) Register(ctx context.Context, req Request) (spawnID string, dispatch bool, err error) {
	if req.AgentType == "" {
		req.AgentType = "claude-code"
	}
	switch req.AgentType {
	case "claude-code", "codex", "gemini":
		// ok
	default:
		return "", false, fmt.Errorf("unsupported agent type: %s", req.AgentType)
	}
	if req.TaskDescription == "" {
		return "", false, fmt.Errorf("task_description is required")
	}
	if req.Project == "" {
		return "", false, fmt.Errorf("project is required")
	}

	// ADDITIVE / OPT-IN: when the caller supplies a non-empty
	// IdempotencyKey, derive a stable spawn id from it and make
	// registration idempotent — a duplicate create re-attaches to the
	// existing spawn (AlreadyExists no-op) instead of minting a second
	// pod. When the key is empty (the only case any current caller hits),
	// the id is server-minted via NewSpawnID() exactly as before and the
	// flow is byte-identical to legacy behavior.
	if key := req.IdempotencyKey; key != "" {
		return c.spawnWithKey(ctx, req, key)
	}

	spawnID = NewSpawnID()
	agentID := fmt.Sprintf("spawn-%s-%s", req.AgentType, spawnID[6:])

	state := &State{
		SpawnID:       spawnID,
		AgentID:       agentID,
		DriverOwnerID: c.driverOwnerID,
		Status:        StatusPending,
		Request:       req,
		StartedAt:     time.Now(),
	}
	// An owned controller participates in the shared-store fencing protocol.
	// It must establish the durable owner row before returning dispatch=true;
	// otherwise a transient write failure can launch a rowless runtime whose
	// request/session identity is unrecoverable after restart. Preserve the
	// historical best-effort order only for ownerless legacy controllers.
	if c.store != nil && c.driverOwnerID != "" {
		if err := c.store.Save(ctx, state); err != nil {
			return "", false, fmt.Errorf("persist owned spawn %s before dispatch: %w", spawnID, err)
		}
		c.mu.Lock()
		c.spawns[spawnID] = state
		delete(c.peerSpawns, spawnID)
		c.mu.Unlock()
		return spawnID, true, nil
	}

	c.mu.Lock()
	c.spawns[spawnID] = state
	delete(c.peerSpawns, spawnID)
	c.mu.Unlock()

	if c.store != nil {
		if err := c.store.Save(ctx, state); err != nil {
			c.logger.Warn("failed to persist spawn state",
				"spawn_id", spawnID, "error", err)
		}
	}

	return spawnID, true, nil
}

// spawnWithKey is the deterministic, idempotent registration path. It is
// only reached when the caller supplied a non-empty IdempotencyKey.
//
// The spawn id is derived deterministically from the key (deriveSpawnID),
// so a re-driven create with the same key targets the same id. If a spawn
// with that id is already registered, the existing state is returned
// unchanged — modeling an AlreadyExists no-op / re-attach so no second pod
// is created. The durable runtime supplies a stable key per logical step, so
// a retry after a crash lands on one stable spawn/pod handle. The agent CLI
// turn may be re-executed and therefore remains at-least-once.
//
// RECORD-BEFORE-DISPATCH: the id and its Pending state are committed to the
// in-memory map and persistent store BEFORE this returns — and the caller
// (the HUD orchestrator) only dispatches the pod-create AFTER Spawn
// returns. So a crash in the window between record and dispatch leaves a
// recoverable handle keyed by the deterministic id; resuming with the same
// key re-attaches to that handle rather than creating a second pod.
func (c *K8sController) spawnWithKey(ctx context.Context, req Request, key string) (string, bool, error) {
	if c.store == nil {
		return "", false, errors.New("keyed spawn requires a persistent store before registration")
	}
	spawnID := DeriveSpawnID(key)
	stripe := &c.keyedRegistration[sha256.Sum256([]byte(spawnID))[0]]
	stripe.Lock()
	defer stripe.Unlock()

	// Process-local lookup is insufficient when multiple HUDs share one
	// ConfigMap. Resolve the deterministic ID durably before installing a new
	// Pending row so a peer retry cannot regress Running back to Pending and
	// re-execute the same agent turn.
	durable, loadErr := c.store.Load(ctx, spawnID)
	if loadErr != nil {
		return "", false, fmt.Errorf("load keyed spawn %s before registration: %w", spawnID, loadErr)
	}
	if durable != nil {
		if durable.Request.IdempotencyKey != key {
			return "", false, fmt.Errorf(
				"%w for %s: durable idempotency key %q does not match %q",
				ErrSpawnStateConflict, spawnID, durable.Request.IdempotencyKey, key,
			)
		}
		if durable.DriverOwnerID == "" && c.driverOwnerID != "" {
			if !c.recoveryAuthority {
				c.mu.Lock()
				c.peerSpawns[spawnID] = struct{}{}
				c.mu.Unlock()
				return "", false, fmt.Errorf(
					"%w for %s: ownerless keyed spawn requires the recovery authority",
					ErrSpawnStateConflict, spawnID,
				)
			}
			claimed := cloneStateForRead(durable)
			claimed.DriverOwnerID = c.driverOwnerID
			if err := c.store.Save(ctx, claimed); err != nil {
				return "", false, fmt.Errorf("claim legacy keyed spawn %s: %w", spawnID, err)
			}
			durable, loadErr = c.store.Load(ctx, spawnID)
			if loadErr != nil {
				return "", false, fmt.Errorf("reload claimed keyed spawn %s: %w", spawnID, loadErr)
			}
			if durable == nil {
				return "", false, fmt.Errorf("reload claimed keyed spawn %s: durable row disappeared", spawnID)
			}
		}
		if !c.ownsDurableState(durable) {
			c.mu.Lock()
			c.peerSpawns[spawnID] = struct{}{}
			c.mu.Unlock()
			return "", false, fmt.Errorf(
				"%w for %s: keyed spawn is owned by controller %q",
				ErrSpawnStateConflict, spawnID, durable.DriverOwnerID,
			)
		}
		copy := cloneStateForRead(durable)
		c.mu.Lock()
		c.spawns[spawnID] = copy
		delete(c.peerSpawns, spawnID)
		c.mu.Unlock()
		c.logger.Info("idempotent spawn re-attach from durable state",
			"spawn_id", spawnID, "status", copy.Status)
		return spawnID, false, nil
	}
	c.mu.RLock()
	local := c.spawns[spawnID]
	c.mu.RUnlock()
	if local != nil {
		if c.driverOwnerID == "" {
			c.logger.Info("idempotent spawn re-attach (already exists)",
				"spawn_id", spawnID, "status", local.Status)
			return spawnID, false, nil
		}
		// Durable state is authoritative for keyed work. A process-local row
		// whose durable generation disappeared must not be treated as a valid
		// reattach or silently recreated; runtime recovery must resolve it.
		return "", false, fmt.Errorf(
			"%w for %s: local keyed spawn %q has no durable row",
			ErrSpawnStateConflict, spawnID, local.Status,
		)
	}

	agentID := fmt.Sprintf("spawn-%s-%s", req.AgentType, spawnID[6:])
	state := &State{
		SpawnID:       spawnID,
		AgentID:       agentID,
		DriverOwnerID: c.driverOwnerID,
		Status:        StatusPending,
		Request:       req,
		StartedAt:     time.Now(),
	}
	// Persist BEFORE returning (and therefore before the caller dispatches
	// the pod) so the handle survives a crash in the record→dispatch window.
	// The per-key stripe, rather than c.mu, remains held through Save: a
	// same-key caller cannot observe a provisional successful reattach, while
	// other keys and read-only lifecycle APIs remain available.
	if err := c.store.Save(ctx, state); err != nil {
		return "", false, fmt.Errorf("persist keyed spawn %s: %w", spawnID, err)
	}
	c.mu.Lock()
	if current := c.spawns[spawnID]; current != nil && !sameDurableGeneration(current, state) {
		c.mu.Unlock()
		return "", false, fmt.Errorf(
			"%w for %s: local generation changed after durable registration",
			ErrSpawnStateConflict, spawnID,
		)
	}
	c.spawns[spawnID] = state
	delete(c.peerSpawns, spawnID)
	c.mu.Unlock()

	return spawnID, true, nil
}

// UpdateState updates the in-memory state and persists it. This is called by
// the orchestration layer as the spawn progresses through lifecycle stages.
func (c *K8sController) UpdateState(ctx context.Context, state *State) {
	if state == nil {
		return
	}
	c.mu.Lock()
	if c.driverOwnerID != "" {
		existing, owned := c.spawns[state.SpawnID]
		if !owned || existing == nil || existing.DriverOwnerID != c.driverOwnerID {
			c.mu.Unlock()
			c.logger.Warn("refusing state update for unowned spawn", "spawn_id", state.SpawnID)
			return
		}
		state.DriverOwnerID = c.driverOwnerID
	}
	c.spawns[state.SpawnID] = state
	delete(c.peerSpawns, state.SpawnID)
	c.mu.Unlock()

	if c.store != nil {
		if err := c.store.Save(ctx, state); err != nil {
			c.logger.Warn("failed to persist spawn state",
				"spawn_id", state.SpawnID, "error", err)
		}
	}
}

// ClearTerminalError repairs legacy poisoned completed/stopped rows without
// exposing a mutable state pointer or bypassing the durable winner. It is a
// no-op when the record is not a clean terminal outcome or is already clean.
func (c *K8sController) ClearTerminalError(ctx context.Context, spawnID string) (State, bool, error) {
	expected, ok := c.Get(spawnID)
	if !ok || expected == nil {
		return State{}, false, nil
	}
	return c.ClearTerminalErrorForGeneration(ctx, *expected)
}

// ClearTerminalErrorForGeneration repairs a poisoned clean terminal result
// only when the hook snapshot still names the exact local and durable spawn
// generation. A stale hook must never rewrite a same-ID replacement.
func (c *K8sController) ClearTerminalErrorForGeneration(
	ctx context.Context,
	expected State,
) (State, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.spawns[expected.SpawnID]
	if !ok || state == nil {
		return State{}, false, nil
	}
	if !sameDurableGeneration(state, &expected) {
		return *state, false, fmt.Errorf(
			"%w for %s: terminal error repair generation changed",
			ErrSpawnStateConflict, expected.SpawnID,
		)
	}
	if err := c.requireDurableGenerationLocked(ctx, &expected); err != nil {
		return *state, false, err
	}
	if (state.Status != StatusCompleted && state.Status != StatusStopped) || state.Error == "" {
		return *state, false, nil
	}
	previous := *state
	state.Error = ""
	if err := c.persistLocked(ctx, state); err != nil {
		winner, reloaded := c.rollbackRejectedTransitionLocked(ctx, state, previous, err)
		if reloaded && errors.Is(err, ErrSpawnStateConflict) &&
			(winner.Status == StatusCompleted || winner.Status == StatusStopped) && winner.Error == "" {
			return winner, false, nil
		}
		return winner, false, err
	}
	return *state, true, nil
}

// StopDisposition reports the result of atomically installing a durable stop
// intent. A terminal winner is never rewritten by a later stop request.
type StopDisposition uint8

const (
	StopBegan StopDisposition = iota
	StopAlreadyRequested
	StopTerminal
)

// BeginStop compare-and-sets a non-terminal spawn into durable stopping
// ownership without changing its active Status. Reconcile recognizes the
// intent and leaves the record untouched until cleanup calls CompleteStop.
func (c *K8sController) BeginStop(ctx context.Context, spawnID string) (State, StopDisposition, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.spawns[spawnID]
	if !ok || state == nil {
		return State{}, StopTerminal, fmt.Errorf("spawn %s not found", spawnID)
	}
	if IsTerminal(state.Status) {
		return *state, StopTerminal, nil
	}
	if state.StopRequestedAt != nil {
		return *state, StopAlreadyRequested, nil
	}
	previous := *state
	now := time.Now()
	state.StopRequestedAt = &now
	if err := c.persistLocked(ctx, state); err != nil {
		winner, reloaded := c.rollbackRejectedTransitionLocked(ctx, state, previous, err)
		if reloaded && errors.Is(err, ErrSpawnStateConflict) {
			switch {
			case IsTerminal(winner.Status):
				return winner, StopTerminal, nil
			case winner.StopRequestedAt != nil:
				return winner, StopAlreadyRequested, nil
			}
		}
		return winner, StopTerminal, fmt.Errorf("persist stop intent for %s: %w", spawnID, err)
	}
	return *state, StopBegan, nil
}

// UpdateUnlessStoppingOrTerminal atomically applies one lifecycle transition
// only while no stop intent or terminal result has won. It is the controller
// CAS used by HUD drivers and prevents Reconcile/Stop races from reviving work.
func (c *K8sController) UpdateUnlessStoppingOrTerminal(
	ctx context.Context,
	spawnID string,
	update func(*State),
) (State, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.spawns[spawnID]
	if !ok || state == nil || IsTerminal(state.Status) || state.StopRequestedAt != nil {
		return State{}, false, nil
	}
	previous := *state
	update(state)
	if err := c.persistLocked(ctx, state); err != nil {
		winner, _ := c.rollbackRejectedTransitionLocked(ctx, state, previous, err)
		return winner, false, err
	}
	return *state, true, nil
}

// RecordStoppingPod durably retains a pod/container handle returned after a
// stop request. Cleanup can then be retried after a failed delete or restart.
func (c *K8sController) RecordStoppingPod(ctx context.Context, spawnID, podName string) (State, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.spawns[spawnID]
	if !ok || state == nil || IsTerminal(state.Status) || state.StopRequestedAt == nil {
		return State{}, false, nil
	}
	previous := *state
	if podName != "" {
		state.PodName = podName
	}
	if err := c.persistLocked(ctx, state); err != nil {
		winner, reloaded := c.rollbackRejectedTransitionLocked(ctx, state, previous, err)
		if reloaded && errors.Is(err, ErrSpawnStateConflict) {
			return winner, false, nil
		}
		return winner, false, fmt.Errorf("persist stopping pod for %s: %w", spawnID, err)
	}
	return *state, true, nil
}

// RecordStopCleanupFailure keeps a stopping record active and attributable;
// callers can retry using the retained PodName. It deliberately does not
// publish a terminal status.
func (c *K8sController) RecordStopCleanupFailure(ctx context.Context, spawnID, podName, reason string) (State, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.spawns[spawnID]
	if !ok || state == nil || IsTerminal(state.Status) || state.StopRequestedAt == nil {
		return State{}, false, nil
	}
	previous := *state
	if podName != "" {
		state.PodName = podName
	}
	state.Error = reason
	if err := c.persistLocked(ctx, state); err != nil {
		winner, reloaded := c.rollbackRejectedTransitionLocked(ctx, state, previous, err)
		if reloaded && errors.Is(err, ErrSpawnStateConflict) {
			return winner, false, nil
		}
		return winner, false, fmt.Errorf("persist stop cleanup failure for %s: %w", spawnID, err)
	}
	return *state, true, nil
}

// CompleteStop compare-and-sets a stopping record to stopped only after all
// driver and backend cleanup has succeeded. A genuine terminal winner remains
// untouched, preventing double terminal metrics/events.
func (c *K8sController) CompleteStop(ctx context.Context, spawnID string) (State, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.spawns[spawnID]
	if !ok || state == nil || IsTerminal(state.Status) || state.StopRequestedAt == nil {
		return State{}, false, nil
	}
	previous := *state
	state.Status = StatusStopped
	state.Error = ""
	now := time.Now()
	state.EndedAt = &now
	if err := c.persistLocked(ctx, state); err != nil {
		winner, reloaded := c.rollbackRejectedTransitionLocked(ctx, state, previous, err)
		if reloaded && errors.Is(err, ErrSpawnStateConflict) {
			return winner, false, nil
		}
		return winner, false, fmt.Errorf("persist completed stop for %s: %w", spawnID, err)
	}
	return *state, true, nil
}

// Stop marks a spawn as stopped and deletes the associated pod.
func (c *K8sController) Stop(ctx context.Context, spawnID string) error {
	state, disposition, err := c.BeginStop(ctx, spawnID)
	if err != nil {
		return err
	}
	if disposition == StopTerminal {
		return nil
	}

	// Delete the pod if one exists and a K8s client is available.
	if state.PodName != "" && c.client != nil {
		pod, getErr := c.client.CoreV1().Pods(c.namespace).Get(ctx, state.PodName, metav1.GetOptions{})
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			return fmt.Errorf("get spawn pod %s before stop: %w", state.PodName, getErr)
		}
		if getErr == nil {
			if err := validateRuntimePodIdentity(&state, pod); err != nil {
				return err
			}
			deleteOpts := metav1.DeleteOptions{}
			if pod.UID != "" {
				uid := pod.UID
				deleteOpts.Preconditions = &metav1.Preconditions{UID: &uid}
			}
			getErr = c.client.CoreV1().Pods(c.namespace).Delete(ctx, state.PodName, deleteOpts)
		}
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			reason := fmt.Sprintf("delete spawn pod %s: %v", state.PodName, getErr)
			_, _, persistErr := c.RecordStopCleanupFailure(ctx, spawnID, state.PodName, reason)
			return errors.Join(fmt.Errorf("%s", reason), persistErr)
		}
	}
	if winner, ok, err := c.CompleteStop(ctx, spawnID); err != nil {
		return err
	} else if !ok {
		if IsTerminal(winner.Status) {
			return nil
		}
		if current, exists := c.Get(spawnID); exists && current != nil && IsTerminal(current.Status) {
			return nil
		}
		return fmt.Errorf("spawn %s stop completion lost compare-and-set", spawnID)
	}
	return nil
}

// Get returns a specific spawn state.
func (c *K8sController) Get(spawnID string) (*State, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.spawns[spawnID]
	if !ok || s == nil {
		return nil, ok
	}
	return cloneStateForRead(s), true
}

// Delete removes a terminal spawn from the in-memory map and persistent store.
// Non-terminal spawns cannot be deleted (stop them first).
func (c *K8sController) Delete(ctx context.Context, spawnID string) error {
	c.mu.RLock()
	state, ok := c.spawns[spawnID]
	if !ok {
		c.mu.RUnlock()
		return fmt.Errorf("spawn %s not found", spawnID)
	}
	if !IsTerminal(state.Status) {
		c.mu.RUnlock()
		return fmt.Errorf("spawn %s is still %s — stop it first", spawnID, state.Status)
	}
	if state.CleanupAt == nil {
		c.mu.RUnlock()
		return fmt.Errorf("%w for %s", ErrSpawnCleanupPending, spawnID)
	}
	candidate := cloneStateForRead(state)
	c.mu.RUnlock()

	if err := c.requireCleanupAcknowledged(ctx, candidate); err != nil {
		if errors.Is(err, ErrSpawnStateConflict) {
			c.evictStaleGeneration(candidate, true)
		}
		return fmt.Errorf("delete spawn %s: %w", spawnID, err)
	}
	if err := c.deleteDurableGeneration(ctx, candidate); err != nil {
		if errors.Is(err, ErrSpawnStateConflict) {
			c.evictStaleGeneration(candidate, true)
		}
		return fmt.Errorf("delete spawn %s: %w", spawnID, err)
	}
	c.evictStaleGeneration(candidate, false)
	return nil
}

// List returns all spawn states.
func (c *K8sController) List() []*State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]*State, 0, len(c.spawns))
	for _, s := range c.spawns {
		result = append(result, cloneStateForRead(s))
	}
	return result
}

func cloneStateForRead(state *State) *State {
	copy := *state
	if state.Request.Metadata != nil {
		copy.Request.Metadata = make(map[string]string, len(state.Request.Metadata))
		for key, value := range state.Request.Metadata {
			copy.Request.Metadata[key] = value
		}
	}
	return &copy
}

// Prune removes cleanup-acknowledged terminal spawns whose EndedAt (or
// CleanupAt as a fallback) is older than maxAge from both the in-memory map and
// the persistent store. It also asks capacity-aware stores to shed oldest
// terminal history when their serialized state crosses the soft size limit.
// Cleanup-pending rows are retained by age pruning as generation locks; the
// pressure pass may remove any terminal row to restore dispatch capacity.
//
// Returns the number of pruned entries. The HUD's `/api/spawns` list grows
// unboundedly without this — Reconcile keeps every record forever, and the
// disk store's `LoadAll` pulls them all back into memory on each restart.
// Without periodic pruning, the operator inbox surfaces "old orphan spawns"
// from days ago even though the underlying pods have been reaped.
func (c *K8sController) Prune(ctx context.Context, maxAge time.Duration) int {
	pruned := 0
	if maxAge > 0 {
		cutoff := time.Now().Add(-maxAge)

		c.mu.RLock()
		var candidates []*State
		for id, state := range c.spawns {
			if state == nil || !IsTerminal(state.Status) || state.CleanupAt == nil {
				continue
			}
			// Prefer EndedAt for the cutoff comparison (set by failSpawn /
			// completeSpawn / Reconcile when a pod transitions to terminal).
			// Fall back to CleanupAt for spawns that were reaped via the
			// TerminalHook path without an EndedAt stamp (rare but possible
			// across pre-hook restarts).
			var ts time.Time
			switch {
			case state.EndedAt != nil && !state.EndedAt.IsZero():
				ts = *state.EndedAt
			case state.CleanupAt != nil && !state.CleanupAt.IsZero():
				ts = *state.CleanupAt
			default:
				continue
			}
			if ts.Before(cutoff) {
				candidate := cloneStateForRead(state)
				candidate.SpawnID = id
				candidates = append(candidates, candidate)
			}
		}
		c.mu.RUnlock()

		for _, candidate := range candidates {
			if err := c.requireCleanupAcknowledged(ctx, candidate); err != nil {
				if errors.Is(err, ErrSpawnStateConflict) {
					c.evictStaleGeneration(candidate, true)
				}
				c.logger.Warn("prune: cleanup acknowledgement changed before delete",
					"spawn_id", candidate.SpawnID, "error", err)
				continue
			}
			if err := c.deleteDurableGeneration(ctx, candidate); err != nil {
				if errors.Is(err, ErrSpawnStateConflict) {
					c.evictStaleGeneration(candidate, true)
				}
				c.logger.Warn("prune: failed to delete spawn generation",
					"spawn_id", candidate.SpawnID, "error", err)
				continue
			}
			c.evictStaleGeneration(candidate, false)
			pruned++
		}
	}

	if store, ok := c.store.(PressurePrunableStore); ok {
		pressurePruned, err := store.PruneTerminalToSoftLimit(ctx)
		if err != nil {
			c.logger.Warn("prune: pressure prune failed", "error", err)
		} else {
			for _, candidate := range pressurePruned {
				c.evictStaleGeneration(candidate, false)
			}
			pruned += len(pressurePruned)
			if len(pressurePruned) > 0 {
				c.logger.Warn("pressure-pruned terminal spawns to restore ConfigMap capacity", "count", len(pressurePruned))
			}
		}
	}
	if pruned > 0 {
		c.logger.Info("pruned terminal spawns",
			"count", pruned,
			"max_age", maxAge.String(),
		)
	}
	return pruned
}

func deleteConditionForState(state *State) DeleteCondition {
	if state == nil {
		return DeleteCondition{}
	}
	return DeleteCondition{
		DriverOwnerID:  state.DriverOwnerID,
		IdempotencyKey: state.Request.IdempotencyKey,
		StartedAt:      state.StartedAt,
	}
}

func sameDurableGeneration(left, right *State) bool {
	if left == nil || right == nil {
		return false
	}
	return left.SpawnID == right.SpawnID &&
		left.DriverOwnerID == right.DriverOwnerID &&
		left.Request.IdempotencyKey == right.Request.IdempotencyKey &&
		left.StartedAt.Equal(right.StartedAt)
}

// requireDurableGenerationLocked verifies that expected is still the exact
// durable generation. The caller must hold c.mu so a local replacement cannot
// cross the check and subsequent mutation.
func (c *K8sController) requireDurableGenerationLocked(ctx context.Context, expected *State) error {
	if c.store == nil || expected == nil {
		return nil
	}
	durable, err := c.store.Load(ctx, expected.SpawnID)
	if err != nil {
		return fmt.Errorf("load spawn %s before generation mutation: %w", expected.SpawnID, err)
	}
	if durable == nil || !sameDurableGeneration(durable, expected) {
		return fmt.Errorf(
			"%w for %s: durable generation changed before mutation",
			ErrSpawnStateConflict, expected.SpawnID,
		)
	}
	return nil
}

func (c *K8sController) requireCleanupAcknowledged(ctx context.Context, candidate *State) error {
	if candidate == nil || candidate.CleanupAt == nil {
		spawnID := ""
		if candidate != nil {
			spawnID = candidate.SpawnID
		}
		return fmt.Errorf("%w for %s", ErrSpawnCleanupPending, spawnID)
	}
	if c.store == nil {
		return nil
	}
	durable, err := c.store.Load(ctx, candidate.SpawnID)
	if err != nil {
		return fmt.Errorf("load spawn %s before cleanup-aware delete: %w", candidate.SpawnID, err)
	}
	if durable == nil {
		return nil
	}
	if !sameDurableGeneration(durable, candidate) {
		return fmt.Errorf(
			"%w for %s: durable generation changed before cleanup-aware delete",
			ErrSpawnStateConflict, candidate.SpawnID,
		)
	}
	if durable.CleanupAt == nil {
		return fmt.Errorf("%w for %s", ErrSpawnCleanupPending, candidate.SpawnID)
	}
	return nil
}

func (c *K8sController) acknowledgeTerminalCleanup(ctx context.Context, expected *State) error {
	if expected == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	live, ok := c.spawns[expected.SpawnID]
	if !ok || live == nil {
		return fmt.Errorf(
			"%w for %s: terminal cleanup generation disappeared",
			ErrSpawnStateConflict, expected.SpawnID,
		)
	}
	if !sameDurableGeneration(live, expected) {
		return fmt.Errorf(
			"%w for %s: terminal cleanup generation changed",
			ErrSpawnStateConflict, expected.SpawnID,
		)
	}
	if live.CleanupAt != nil {
		return nil
	}
	if err := c.requireDurableGenerationLocked(ctx, expected); err != nil {
		return err
	}
	previous := *live
	now := time.Now()
	live.CleanupAt = &now
	if err := c.persistLocked(ctx, live); err != nil {
		_, _ = c.rollbackRejectedTransitionLocked(ctx, live, previous, err)
		return fmt.Errorf("persist terminal cleanup acknowledgement for %s: %w", expected.SpawnID, err)
	}
	return nil
}

func (c *K8sController) deleteDurableGeneration(ctx context.Context, candidate *State) error {
	if c.store == nil || candidate == nil {
		return nil
	}
	if conditional, ok := c.store.(ConditionalDeleteStore); ok {
		_, err := conditional.DeleteIfMatch(ctx, candidate.SpawnID, deleteConditionForState(candidate))
		return err
	}
	return c.store.Delete(ctx, candidate.SpawnID)
}

// evictStaleGeneration removes only the candidate the caller inspected. On a
// durable conflict, the ID is cached as peer-owned so reconcile cannot turn a
// replacement into a lossy orphan row.
func (c *K8sController) evictStaleGeneration(candidate *State, peer bool) {
	if candidate == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.spawns[candidate.SpawnID]; ok && sameDurableGeneration(current, candidate) {
		delete(c.spawns, candidate.SpawnID)
		if peer {
			c.peerSpawns[candidate.SpawnID] = struct{}{}
		}
	}
}

// Reconcile synchronises the in-memory state map with actual Kubernetes pod
// status. This is the key fix for the stale-after-restart bug: instead of
// trusting local JSON, we query the cluster for pods labeled
// app.kubernetes.io/managed-by=loom-spawn and derive state from their phase.
func (c *K8sController) Reconcile(ctx context.Context) error {
	if c.client == nil {
		// No K8s client configured — skip reconciliation silently.
		return nil
	}

	selector := fmt.Sprintf("%s=%s", ManagedByLabel, ManagedByValue)
	pods, err := c.client.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return fmt.Errorf("list spawn pods: %w", err)
	}

	// Build a set of spawn IDs from live pods.
	livePods := make(map[string]*corev1.Pod, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		spawnID := pod.Labels[SpawnIDLabel]
		if spawnID == "" {
			continue
		}
		livePods[spawnID] = pod
	}

	// A live pod absent from this controller's in-memory map may still be
	// owned by another controller using the same durable store. Resolve that
	// distinction before taking the controller lock: record-before-dispatch
	// guarantees a legitimate owner saved the complete state before its pod
	// could become visible. Treat an existing row as peer-owned and leave it
	// untouched; reconstruct from labels only after a positive durable miss.
	// A read failure is ownership-unknown, so fail closed instead of replacing
	// potentially complete request/idempotency metadata with a lossy record.
	discoveryCandidates := make(map[string]struct{})
	durablyTrackedByPeer := make(map[string]struct{})
	if c.store != nil {
		c.mu.Lock()
		for spawnID := range c.peerSpawns {
			if _, stillLive := livePods[spawnID]; !stillLive {
				delete(c.peerSpawns, spawnID)
			}
		}
		for spawnID := range livePods {
			_, tracked := c.spawns[spawnID]
			_, knownPeer := c.peerSpawns[spawnID]
			if !tracked && !knownPeer {
				discoveryCandidates[spawnID] = struct{}{}
			}
		}
		c.mu.Unlock()

		if len(discoveryCandidates) > 0 {
			durableStates, loadErr := c.store.LoadAll(ctx)
			if loadErr != nil {
				return fmt.Errorf("load durable spawn snapshot before discovery: %w", loadErr)
			}
			for _, durable := range durableStates {
				if durable == nil {
					continue
				}
				if _, candidate := discoveryCandidates[durable.SpawnID]; candidate {
					durablyTrackedByPeer[durable.SpawnID] = struct{}{}
				}
			}
		}
	}

	c.mu.Lock()

	// Collect terminal spawns needing cleanup; fire the hook after we
	// release the lock so handlers can call back into the controller
	// without deadlocking.
	var terminal []State
	var stopping []State

	// Update existing entries from pod status.
	for spawnID, state := range c.spawns {
		if state == nil {
			continue
		}
		// Harvester and future non-K8s substrates are not represented by Pods in
		// this controller's devbox namespace. Treating their absent Pod as a
		// failure immediately reaps a healthy VM. Their lifecycle driver owns
		// active status; durable stop and terminal hooks still need dispatch.
		if substrate := strings.TrimSpace(state.Request.Substrate); substrate != "" && substrate != "k8s" {
			switch {
			case state.StopRequestedAt != nil && !IsTerminal(state.Status):
				stopping = append(stopping, *state)
			case IsTerminal(state.Status) && state.CleanupAt == nil:
				terminal = append(terminal, *state)
			}
			continue
		}
		pod, exists := livePods[spawnID]
		if exists && c.driverOwnerID != "" {
			if c.recoveryAuthority && runtimePodIdentityHasMissingLabels(state, pod) {
				claimedPod, err := c.ensureOrphanPodIdentity(ctx, pod, state)
				if err != nil {
					c.logger.Warn("deferring spawn reconciliation until legacy runtime identity is stamped",
						"spawn_id", spawnID, "pod", pod.Name, "error", err)
					continue
				}
				pod = claimedPod
				livePods[spawnID] = claimedPod
			}
			if err := validateRuntimePodIdentity(state, pod); err != nil {
				delete(c.spawns, spawnID)
				c.peerSpawns[spawnID] = struct{}{}
				c.logger.Error("evicting spawn whose runtime identity changed",
					"spawn_id", spawnID, "pod", pod.Name, "error", err)
				continue
			}
		}
		// A durable stop intent owns this non-terminal record. Preserve its
		// active status until HUD confirms driver + backend cleanup and CASes it
		// to stopped; pod phase/not-found observations must not manufacture a
		// competing failed/completed winner. If a late pod appeared, retain its
		// exact name so cleanup remains retryable after restart.
		if state.StopRequestedAt != nil && !IsTerminal(state.Status) {
			if exists && state.PodName == "" {
				previous := *state
				state.PodName = pod.Name
				if err := c.persistLocked(ctx, state); err != nil {
					_, _ = c.rollbackRejectedTransitionLocked(ctx, state, previous, err)
					continue
				}
			}
			stopping = append(stopping, *state)
			continue
		}
		if !exists {
			if state.PodName == "" && isPreRuntimeStatus(state.Status) {
				continue
			}

			// Pod gone -- if a runtime pod existed for a non-terminal spawn,
			// mark it as failed.
			if !IsTerminal(state.Status) {
				previous := *state
				state.Status = StatusFailed
				state.Error = "pod not found during reconciliation"
				now := time.Now()
				state.EndedAt = &now
				if err := c.persistLocked(ctx, state); err != nil {
					_, _ = c.rollbackRejectedTransitionLocked(ctx, state, previous, err)
					continue
				}
			}
			// Pod is gone — if cleanup never ran (e.g., the spawn went
			// terminal across a controller restart), queue the hook so we
			// still deregister presence and end the session.
			if IsTerminal(state.Status) && state.CleanupAt == nil {
				terminal = append(terminal, *state)
			}
			continue
		}
		// Update state from pod phase.
		newStatus := podPhaseToStatus(pod.Status.Phase)
		if state.Status != newStatus && !IsTerminal(state.Status) {
			previous := *state
			state.Status = newStatus
			state.PodName = pod.Name
			if IsTerminal(newStatus) {
				now := time.Now()
				state.EndedAt = &now
			}
			if err := c.persistLocked(ctx, state); err != nil {
				_, _ = c.rollbackRejectedTransitionLocked(ctx, state, previous, err)
				continue
			}
		}
		// Restart-durable deadline backstop. The in-process liveness watchdog
		// (runLivenessWatcher) and the exec TimeoutSec both live in the
		// mobile-hud process, so a spawn that was mid-flight when mobile-hud
		// restarted is recovered as "running" with a live pod and NOTHING
		// re-enforces its deadline — the phase update above keeps it running
		// for as long as the pod exists, an immortal slot-holder that counts
		// against ActiveCount forever (observed 2026-06-30: a 60-min spawn ran
		// 79 min past a mobile-hud restart, holding a spawn-pool slot and 4 GiB
		// with no live watcher). Enforce the request deadline here so the
		// reconciler reaps the orphan regardless of the watchdog. The bound is
		// max(TimeoutMinutes, spawnAbsoluteMaxAge)+grace, so a spawn with empty
		// request metadata (TimeoutMinutes == 0 — e.g. a pod discovered only
		// from labels, whose Request the discovery path can't fully rebuild) is
		// still reaped by the absolute-age floor instead of holding a pool slot
		// forever; only a spawn with no age anchor at all (zero StartedAt) is
		// skipped, so normal in-flight work within its deadline is never touched.
		if !IsTerminal(state.Status) && spawnDeadlineExceeded(state, time.Now()) {
			previous := *state
			state.Status = StatusFailed
			state.Error = "spawn deadline exceeded during reconciliation"
			now := time.Now()
			state.EndedAt = &now
			if state.PodName == "" {
				state.PodName = pod.Name
			}
			if err := c.persistLocked(ctx, state); err != nil {
				_, _ = c.rollbackRejectedTransitionLocked(ctx, state, previous, err)
				continue
			}
			terminal = append(terminal, *state)
			continue
		}
		// Spawn-is-terminal-but-pod-is-still-alive — the orphan path that
		// drains namespace quota. Queue cleanup so the hook can delete
		// the pod and deregister presence.
		if IsTerminal(state.Status) && state.CleanupAt == nil {
			if state.PodName == "" {
				state.PodName = pod.Name
			}
			terminal = append(terminal, *state)
		}
	}

	// Discover pods that are not tracked (e.g., after a full restart with no
	// persisted state). Create new entries from pod labels.
	//
	// EMPTY-METADATA SMELL: this reconstruction is lossy. Pod labels carry only
	// AgentType and Project (and only when the orchestrator stamped them); the
	// request's TaskDescription, TimeoutMinutes, and everything else are gone.
	// The rebuilt Request therefore has TimeoutMinutes == 0, which is precisely
	// the shape the request-timeout deadline can't bound — see
	// spawnDeadlineExceeded, whose absolute-age floor exists so these entries
	// are still reaped rather than pinning a pool slot forever. StartedAt is set
	// from pod.CreationTimestamp so the age floor has a real anchor. Stamping a
	// loom.dev/timeout-minutes label at pod-create time (so this path could
	// recover the true deadline) is a worthwhile follow-up but out of scope
	// here; the absolute-age backstop closes the safety gap either way.
	for spawnID, pod := range livePods {
		if _, tracked := c.spawns[spawnID]; tracked {
			continue
		}
		if c.store != nil {
			if _, knownPeer := c.peerSpawns[spawnID]; knownPeer {
				continue
			}
			if _, peerOwned := durablyTrackedByPeer[spawnID]; peerOwned {
				c.peerSpawns[spawnID] = struct{}{}
				c.logger.Debug("leaving durably tracked peer spawn unclaimed",
					"spawn_id", spawnID, "pod", pod.Name)
				continue
			}
			// The spawn was tracked when candidates were snapshotted but was
			// removed before the write lock was acquired. Its ownership is now
			// unknown; defer to the next tick rather than reconstructing it.
			if _, checked := discoveryCandidates[spawnID]; !checked {
				continue
			}
		}
		if c.driverOwnerID != "" {
			podOwner := pod.Labels[DriverOwnerLabel]
			localOwner := DriverOwnerLabelValue(c.driverOwnerID)
			switch {
			case podOwner != "" && podOwner != localOwner:
				c.peerSpawns[spawnID] = struct{}{}
				c.logger.Debug("leaving peer-owned orphan pod unclaimed",
					"spawn_id", spawnID, "pod", pod.Name)
				continue
			case podOwner == "" && !c.recoveryAuthority:
				c.peerSpawns[spawnID] = struct{}{}
				c.logger.Debug("leaving ownerless orphan pod to recovery authority",
					"spawn_id", spawnID, "pod", pod.Name)
				continue
			}
		}
		startedAt := pod.CreationTimestamp.Time
		if generation := strings.TrimSpace(pod.Labels[RuntimeGenerationLabel]); generation != "" {
			parsed, parseErr := ParseRuntimeGenerationLabelValue(generation)
			if parseErr != nil {
				c.logger.Warn("leaving rowless spawn pod unclaimed because its generation label is invalid",
					"spawn_id", spawnID, "pod", pod.Name, "generation", generation, "error", parseErr)
				continue
			}
			startedAt = parsed
		} else if c.driverOwnerID != "" {
			// An owned controller may only claim a rowless runtime when the pod
			// already proves its immutable generation. CreationTimestamp is later
			// than record-before-dispatch StartedAt and cannot distinguish a legacy
			// pod from an unlabeled same-name replacement.
			c.logger.Warn("leaving rowless spawn pod unclaimed because it has no generation proof",
				"spawn_id", spawnID, "pod", pod.Name)
			continue
		}
		state := &State{
			SpawnID:       spawnID,
			AgentID:       pod.Labels[AgentIDLabel],
			DriverOwnerID: c.driverOwnerID,
			PodName:       pod.Name,
			Status:        podPhaseToStatus(pod.Status.Phase),
			StartedAt:     startedAt,
			Request: Request{
				AgentType: pod.Labels[AgentTypeLabel],
				Project:   pod.Labels[ProjectLabel],
			},
		}
		c.spawns[spawnID] = state
		if err := c.persistLocked(ctx, state); err != nil {
			// Discovery does not own a durable conflict. Remove the provisional
			// local claim so the next tick re-evaluates ownership from the store.
			if current, ok := c.spawns[spawnID]; ok && current == state {
				delete(c.spawns, spawnID)
			}
			c.logger.Warn("failed to persist discovered orphan; leaving it unclaimed",
				"spawn_id", spawnID, "pod", pod.Name, "error", err)
			continue
		}
		// Save may have merged this lossy discovery claim into a complete row
		// that appeared after LoadAll. Reload the durable winner before runtime
		// labeling or local installation so request/key/generation metadata can
		// never be replaced by the provisional label reconstruction.
		if c.store != nil {
			authoritative, err := c.store.Load(ctx, spawnID)
			if err != nil || authoritative == nil || !c.ownsDurableState(authoritative) {
				delete(c.spawns, spawnID)
				if authoritative != nil && !c.ownsDurableState(authoritative) {
					c.peerSpawns[spawnID] = struct{}{}
				}
				c.logger.Warn("failed to reload authoritative orphan claim; leaving it unclaimed",
					"spawn_id", spawnID, "pod", pod.Name, "state", authoritative, "error", err)
				continue
			}
			state = cloneStateForRead(authoritative)
			c.spawns[spawnID] = state
		}
		if c.driverOwnerID != "" {
			claimedPod, err := c.ensureOrphanPodIdentity(ctx, pod, state)
			if err != nil {
				// Retain the authoritative local row without running any lifecycle
				// side effect. A later reconcile retries only the missing-label
				// migration; non-empty identity conflicts remain fail-closed.
				c.logger.Warn("deferring orphan lifecycle until runtime identity is stamped",
					"spawn_id", spawnID, "pod", pod.Name, "error", err)
				continue
			}
			pod = claimedPod
			state.PodName = pod.Name
		}
		c.logger.Info("discovered untracked spawn pod",
			"spawn_id", spawnID, "pod", pod.Name, "status", state.Status)
		// Discovered-already-terminal pod (e.g., previous operator died
		// mid-run): fire cleanup so the orphan does not linger.
		if IsTerminal(state.Status) {
			terminal = append(terminal, *state)
		}
	}

	hook := c.terminalHook
	stopHook := c.stoppingHook
	c.mu.Unlock()

	if stopHook != nil {
		for i := range stopping {
			if err := stopHook(ctx, stopping[i]); err != nil {
				c.logger.Warn("stopping spawn cleanup retry failed",
					"spawn_id", stopping[i].SpawnID, "error", err)
			}
		}
	}

	// Fire cleanup hooks outside the lock so handlers can call back
	// into Get/List/ActiveCount without deadlocking. A successful hook only
	// stamps CleanupAt when that acknowledgement is durably saved; otherwise
	// the in-memory mutation is rolled back so the next tick retries it.
	if hook != nil {
		for i := range terminal {
			st := terminal[i]
			if err := hook(ctx, st); err != nil {
				c.logger.Warn("terminal spawn cleanup failed; will retry",
					"spawn_id", st.SpawnID, "error", err)
				continue
			}
			if err := c.acknowledgeTerminalCleanup(ctx, &st); err != nil {
				c.logger.Warn("terminal spawn cleanup completed but acknowledgement was rejected",
					"spawn_id", st.SpawnID, "error", err)
			}
		}
	}

	return nil
}

func validateRuntimePodIdentity(state *State, pod *corev1.Pod) error {
	if state == nil || pod == nil {
		return fmt.Errorf("%w: missing spawn state or pod", ErrSpawnStateConflict)
	}
	for key, want := range RuntimeIdentityLabelsForState(state) {
		if got := pod.Labels[key]; got != want {
			return fmt.Errorf(
				"%w for %s: pod %s label %s=%q, want %q",
				ErrSpawnStateConflict, state.SpawnID, pod.Name, key, got, want,
			)
		}
	}
	return nil
}

func runtimePodIdentityHasMissingLabels(state *State, pod *corev1.Pod) bool {
	if state == nil || pod == nil {
		return false
	}
	for key, want := range RuntimeIdentityLabelsForState(state) {
		if want != "" && pod.Labels[key] == "" {
			return true
		}
	}
	return false
}

func runtimePodHasGenerationProof(state *State, pod *corev1.Pod) bool {
	if state == nil || pod == nil {
		return false
	}
	want := RuntimeGenerationLabelValue(state.StartedAt)
	return want != "" && pod.Labels[RuntimeGenerationLabel] == want
}

func (c *K8sController) ensureOrphanPodIdentity(
	ctx context.Context,
	discovered *corev1.Pod,
	state *State,
) (*corev1.Pod, error) {
	var result *corev1.Pod
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := c.client.CoreV1().Pods(c.namespace).Get(ctx, discovered.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if discovered.UID != "" && latest.UID != discovered.UID {
			return fmt.Errorf("%w for %s: pod UID changed during orphan claim", ErrSpawnStateConflict, state.SpawnID)
		}
		if runtimePodIdentityHasMissingLabels(state, latest) && !runtimePodHasGenerationProof(state, latest) {
			return fmt.Errorf(
				"%w for %s: pod %s has missing identity labels without immutable generation proof",
				ErrSpawnStateConflict, state.SpawnID, latest.Name,
			)
		}
		missing := make(map[string]string)
		for key, want := range RuntimeIdentityLabelsForState(state) {
			got := latest.Labels[key]
			switch {
			case got == want:
			case got == "" && c.recoveryAuthority:
				missing[key] = want
			default:
				return fmt.Errorf(
					"%w for %s: orphan pod %s label %s=%q, want %q",
					ErrSpawnStateConflict, state.SpawnID, latest.Name, key, got, want,
				)
			}
		}
		if len(missing) == 0 {
			result = latest
			return nil
		}
		next := latest.DeepCopy()
		if next.Labels == nil {
			next.Labels = make(map[string]string)
		}
		for key, value := range missing {
			next.Labels[key] = value
		}
		result, err = c.client.CoreV1().Pods(c.namespace).Update(ctx, next, metav1.UpdateOptions{})
		return err
	})
	return result, err
}

func isPreRuntimeStatus(status Status) bool {
	switch status {
	case StatusPending, StatusBuilding:
		return true
	default:
		return false
	}
}

// reconcileDeadlineGrace is added to a spawn's request timeout before the
// reconciler treats it as expired. The in-process liveness watchdog + exec
// TimeoutSec enforce the deadline under steady state (the Exec returns at
// ~TimeoutMinutes), so the reconciler is only the restart-durable backstop;
// the grace keeps it from racing a normally-finishing spawn while still
// reaping a watcher-less orphan promptly.
const reconcileDeadlineGrace = 5 * time.Minute

// spawnAbsoluteMaxAge is the hard ceiling on how long ANY non-terminal spawn
// may run before the reconciler reaps it, independent of request metadata. It
// backstops spawns the request-timeout path cannot otherwise bound — those
// with Request.TimeoutMinutes <= 0. Two ways a live spawn reaches that state:
// a caller (e.g. a Mills pipeline stage) that left TimeoutMinutes unset, or —
// more insidiously — a State the reconciler rebuilt from pod labels alone via
// the discovered-untracked-pod path in Reconcile, which drops TaskDescription
// and TimeoutMinutes entirely. Before this floor existed, spawnDeadlineExceeded
// short-circuited to false whenever TimeoutMinutes <= 0, so such a spawn was
// NEVER reaped by the deadline path: it held a spawn-pool slot (and its pod's
// memory) until manual intervention. On 2026-07-01 six codex spawns with empty
// request metadata survived 22–30h, pinning the pool at its cap of 6 and
// forcing every pipeline stage to escalate with "400 max concurrent spawns
// reached (6)". 60m matches the effective single-spawn budget; a legitimately
// longer spawn is unaffected because the effective bound is the max of the two.
const spawnAbsoluteMaxAge = 60 * time.Minute

// spawnDeadlineExceeded reports whether a non-terminal spawn has run past its
// effective deadline: StartedAt + max(TimeoutMinutes, spawnAbsoluteMaxAge) +
// reconcileDeadlineGrace. Taking the max of the request timeout and an absolute
// floor means the reconciler ALWAYS reaps a watcher-less orphan within ~65 min
// even when the request carries no usable timeout (0 or negative — empty
// metadata / a label-reconstructed state), while never reaping a spawn earlier
// than its own explicit, longer deadline. Returns false only when the spawn has
// no age anchor at all (zero StartedAt); a real running pod always has one
// (spawn time, or pod CreationTimestamp on the discovery path), so this guard
// only skips genuinely unbounded entries, never a live slot-holding zombie.
func spawnDeadlineExceeded(state *State, now time.Time) bool {
	if state == nil || state.StartedAt.IsZero() {
		return false
	}
	timeout := time.Duration(state.Request.TimeoutMinutes) * time.Minute
	if timeout < spawnAbsoluteMaxAge {
		timeout = spawnAbsoluteMaxAge
	}
	deadline := state.StartedAt.Add(timeout + reconcileDeadlineGrace)
	return now.After(deadline)
}

// RecoverFromStore loads only states owned by this controller's stable recovery
// domain. Foreign rows stay outside the mutable lifecycle map, so startup
// resume/redrive/stop/cleanup paths cannot adopt peer work. A controller with
// no configured owner preserves the legacy single-store behavior.
func (c *K8sController) RecoverFromStore(ctx context.Context) error {
	if c.store == nil {
		return nil
	}
	states, err := c.store.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("load persisted spawns: %w", err)
	}

	owned := make([]*State, 0, len(states))
	peers := make([]string, 0, len(states))
	for _, st := range states {
		if st == nil || st.SpawnID == "" {
			continue
		}
		if c.driverOwnerID == "" || st.DriverOwnerID == c.driverOwnerID {
			owned = append(owned, cloneStateForRead(st))
			continue
		}
		if st.DriverOwnerID != "" || !c.recoveryAuthority {
			peers = append(peers, st.SpawnID)
			continue
		}

		// This is the one migration authority for ownerless shared rows. Claim
		// durably before exposing the state to any recovery side effect. A
		// concurrent claimant wins through the store's per-entry owner fence.
		claimed := cloneStateForRead(st)
		claimed.DriverOwnerID = c.driverOwnerID
		if err := c.store.Save(ctx, claimed); err != nil {
			if errors.Is(err, ErrSpawnStateConflict) {
				winner, loadErr := c.store.Load(ctx, st.SpawnID)
				if loadErr != nil {
					return fmt.Errorf("reload legacy spawn %s after ownership conflict: %w", st.SpawnID, loadErr)
				}
				if c.ownsDurableState(winner) {
					owned = append(owned, cloneStateForRead(winner))
				} else {
					peers = append(peers, st.SpawnID)
				}
				continue
			}
			return fmt.Errorf("claim legacy spawn %s for controller %q: %w", st.SpawnID, c.driverOwnerID, err)
		}
		winner, loadErr := c.store.Load(ctx, st.SpawnID)
		if loadErr != nil {
			return fmt.Errorf("reload claimed legacy spawn %s: %w", st.SpawnID, loadErr)
		}
		if !c.ownsDurableState(winner) {
			return fmt.Errorf("%w for %s: legacy ownership claim was not retained", ErrSpawnStateConflict, st.SpawnID)
		}
		owned = append(owned, cloneStateForRead(winner))
	}
	for _, state := range owned {
		if err := c.ensureRecoveredRuntimeIdentity(ctx, state); err != nil {
			return fmt.Errorf("prepare recovered spawn %s runtime identity: %w", state.SpawnID, err)
		}
	}

	c.mu.Lock()
	for _, st := range owned {
		c.spawns[st.SpawnID] = st
		delete(c.peerSpawns, st.SpawnID)
	}
	for _, spawnID := range peers {
		if _, local := c.spawns[spawnID]; !local {
			c.peerSpawns[spawnID] = struct{}{}
		}
	}
	c.mu.Unlock()

	c.logger.Info("recovered owned spawn state from store",
		"owned", len(owned), "peer_skipped", len(peers), "controller_id", c.driverOwnerID)
	return nil
}

func (c *K8sController) ensureRecoveredRuntimeIdentity(ctx context.Context, state *State) error {
	if !c.recoveryAuthority || c.client == nil || state == nil || state.PodName == "" {
		return nil
	}
	if substrate := strings.TrimSpace(state.Request.Substrate); substrate != "" && substrate != "k8s" {
		return nil
	}
	pod, err := c.client.CoreV1().Pods(c.namespace).Get(ctx, state.PodName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get recovered pod %s: %w", state.PodName, err)
	}
	if !runtimePodIdentityHasMissingLabels(state, pod) {
		return validateRuntimePodIdentity(state, pod)
	}
	if _, err := c.ensureOrphanPodIdentity(ctx, pod, state); err != nil {
		return fmt.Errorf("stamp recovered pod %s identity: %w", state.PodName, err)
	}
	return nil
}

func (c *K8sController) ownsDurableState(state *State) bool {
	if state == nil {
		return false
	}
	if c.driverOwnerID == "" {
		return true
	}
	return state.DriverOwnerID == c.driverOwnerID
}

// ActiveCount returns the number of spawns in a non-terminal state.
func (c *K8sController) ActiveCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	count := 0
	for _, s := range c.spawns {
		if !IsTerminal(s.Status) {
			count++
		}
	}
	return count
}

// persistLocked saves to the store. Caller must hold c.mu. A nil store is an
// in-memory controller and therefore has no persistence failure.
func (c *K8sController) persistLocked(ctx context.Context, state *State) error {
	if c.store == nil {
		return nil
	}
	return c.store.Save(ctx, state)
}

// rollbackRejectedTransitionLocked prevents an in-memory lifecycle winner from
// escaping after the durable store rejected it. Prefer the current durable row
// (which may be a peer's terminal/stop winner); if it cannot be reloaded, restore
// the exact pre-transition value. Caller must hold c.mu.
func (c *K8sController) rollbackRejectedTransitionLocked(
	ctx context.Context,
	state *State,
	previous State,
	persistErr error,
) (State, bool) {
	if c.store != nil {
		durable, loadErr := c.store.Load(ctx, state.SpawnID)
		if loadErr == nil && durable != nil {
			if !c.ownsDurableState(durable) {
				delete(c.spawns, state.SpawnID)
				c.peerSpawns[state.SpawnID] = struct{}{}
				c.logger.Warn("spawn transition rejected; durable winner belongs to peer",
					"spawn_id", state.SpawnID,
					"durable_status", durable.Status,
					"driver_owner_id", durable.DriverOwnerID,
					"error", persistErr)
				return *durable, true
			}
			c.spawns[state.SpawnID] = durable
			c.logger.Warn("spawn transition rejected; reloaded durable winner",
				"spawn_id", state.SpawnID,
				"durable_status", durable.Status,
				"error", persistErr)
			return *durable, true
		}
		if loadErr != nil {
			c.logger.Warn("spawn transition rejected and durable winner reload failed",
				"spawn_id", state.SpawnID,
				"error", persistErr,
				"load_error", loadErr)
		}
	}
	*state = previous
	c.logger.Warn("spawn transition rejected; restored previous in-memory state",
		"spawn_id", state.SpawnID, "status", state.Status, "error", persistErr)
	return previous, false
}

// podPhaseToStatus maps a Kubernetes pod phase to a spawn Status.
func podPhaseToStatus(phase corev1.PodPhase) Status {
	switch phase {
	case corev1.PodPending:
		return StatusPending
	case corev1.PodRunning:
		return StatusRunning
	case corev1.PodSucceeded:
		return StatusCompleted
	case corev1.PodFailed:
		return StatusFailed
	default:
		return StatusUnknown
	}
}

// NewSpawnID generates a unique spawn ID using crypto/rand.
func NewSpawnID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("spawn-%d", time.Now().UnixNano())
	}
	return "spawn-" + hex.EncodeToString(buf[:])
}

// derivedSpawnIDHexLen is the number of hex characters of the key digest
// used in a derived spawn id. It matches the 12 hex chars NewSpawnID emits
// (6 random bytes) so derived ids are shape-compatible with random ones —
// same length, same "spawn-"+hex form, same valid-k8s-name guarantees, and
// the same agentID slice math (spawnID[6:]).
const derivedSpawnIDHexLen = 12

// DeriveSpawnID returns a deterministic spawn id for a non-empty
// idempotency key. The same key always yields the same id, which is what
// makes a duplicate create an AlreadyExists no-op in spawnWithKey. The id
// is "spawn-" + the first derivedSpawnIDHexLen hex chars of SHA-256(key),
// keeping it shape-identical to NewSpawnID() output (lowercase hex, safe
// as a k8s pod-name fragment, and sliceable at [6:] for the agent id).
//
// This is the AUTHORITATIVE derivation. The worker package mirrors it as
// worker.DeriveSpawnID (it cannot import this package without dragging in
// k8s client-go); a parity test locks the two to identical output.
func DeriveSpawnID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "spawn-" + hex.EncodeToString(sum[:])[:derivedSpawnIDHexLen]
}

// IsDerivedSpawnName reports whether podName is the deterministic pod name a
// keyed spawn produces for the given idempotency key — i.e. it equals
// "spawn-" + DeriveSpawnID(key). The pod the orchestrator creates is named
// "spawn-"+spawnID (internal/hud/spawn.go), and for a keyed spawn
// spawnID == DeriveSpawnID(key), so the derived pod name is
// "spawn-spawn-"+hex.
//
// This is the predicate the live-create AlreadyExists backstop uses to decide
// whether a k8s AlreadyExists is safe to treat as a RE-ATTACH: it only adopts
// the existing pod when the colliding name is provably the deterministic name
// derived from a non-empty idempotency key. An empty key (the legacy /
// random-name path) always returns false, so the legacy AlreadyExists
// semantics are preserved untouched. See .loom/134 §5.
func IsDerivedSpawnName(podName, key string) bool {
	if key == "" || podName == "" {
		return false
	}
	return podName == "spawn-"+DeriveSpawnID(key)
}
