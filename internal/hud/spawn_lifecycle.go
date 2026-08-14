package hud

import (
	"context"
	"errors"

	"github.com/crb2nu/loom/internal/spawn"
)

// RecoverSpawns delegates recovery to the spawn controller. Previously this
// blindly marked non-terminal spawns as failed ("stale after HUD restart").
// Now the controller recovers from the store and a subsequent Reconcile call
// will check actual pod status — fixing the stale-after-restart bug.
func (o *SpawnOrchestrator) RecoverSpawns() error {
	return o.RecoverSpawnsContext(context.Background())
}

// RecoverSpawnsContext performs the one startup recovery pass. Failures remain
// retryable; callers must not start reconciliation or serve spawn mutations
// until this returns nil, otherwise owned durable rows can be cached as peers.
func (o *SpawnOrchestrator) RecoverSpawnsContext(ctx context.Context) error {
	if o == nil || o.ctrl == nil {
		return nil
	}
	o.recoveryMu.Lock()
	if o.recovered {
		o.recoveryMu.Unlock()
		return nil
	}
	if err := o.ctrl.RecoverFromStore(ctx); err != nil {
		o.logger.Warn("failed to recover spawns", "error", err)
		o.recoveryMu.Unlock()
		return err
	}
	o.recovered = true
	o.recoveryMu.Unlock()
	o.recoverStoppingSpawns()
	o.resumePreRuntimeSpawns()
	o.recoverInterruptedSpawns()
	return nil
}

// SetDegraded marks the spawn backend degraded (true) or healthy (false).
// Degraded means the HUD is serving but the startup spawn-state recovery
// pass has not completed — see the degraded field doc for what is gated.
func (o *SpawnOrchestrator) SetDegraded(v bool) {
	if o == nil {
		return
	}
	o.degraded.Store(v)
}

// Degraded reports whether the spawn backend is degraded because startup
// spawn-state recovery has not completed yet.
func (o *SpawnOrchestrator) Degraded() bool {
	return o != nil && o.degraded.Load()
}

func (o *SpawnOrchestrator) acquireSpawnDriver(spawnID string) (*spawnDriverOwner, bool) {
	if o == nil || o.ctrl == nil {
		return nil, false
	}
	o.driversMu.Lock()
	defer o.driversMu.Unlock()
	if o.drivers == nil {
		o.drivers = make(map[string]*spawnDriverOwner)
	}
	if _, exists := o.drivers[spawnID]; exists {
		if o.logger != nil {
			o.logger.Warn("spawn lifecycle already has an active driver", "spawn_id", spawnID)
		}
		return nil, false
	}
	state, ok := o.ctrl.Get(spawnID)
	if !ok || state == nil || spawn.IsTerminal(state.Status) || state.StopRequestedAt != nil {
		return nil, false
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	owner := &spawnDriverOwner{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	o.drivers[spawnID] = owner
	return owner, true
}

func (o *SpawnOrchestrator) spawnDriverActive(spawnID string, owner *spawnDriverOwner) bool {
	if o == nil || owner == nil || o.ctrl == nil {
		return false
	}
	o.driversMu.Lock()
	defer o.driversMu.Unlock()
	if o.drivers[spawnID] != owner || owner.stopRequested || owner.ctx.Err() != nil {
		return false
	}
	state, ok := o.ctrl.Get(spawnID)
	return ok && state != nil && !spawn.IsTerminal(state.Status) && state.StopRequestedAt == nil
}

// updateSpawnFromDriver is the only non-terminal controller transition used
// by runSpawn. StopSpawn takes the same lock while requesting cancellation,
// making the terminal fence atomic with respect to late driver writes.
func (o *SpawnOrchestrator) updateSpawnFromDriver(
	ctx context.Context,
	spawnID string,
	owner *spawnDriverOwner,
	update func(*SpawnState),
) (*SpawnState, bool, error) {
	if o == nil || owner == nil || o.ctrl == nil {
		return nil, false, nil
	}
	o.driversMu.Lock()
	defer o.driversMu.Unlock()
	if o.drivers[spawnID] != owner || owner.stopRequested || owner.ctx.Err() != nil {
		return nil, false, nil
	}
	updated, ok, err := o.ctrl.UpdateUnlessStoppingOrTerminal(ctx, spawnID, update)
	if err != nil {
		return &updated, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return &updated, true, nil
}

func (o *SpawnOrchestrator) setStopCleanupError(spawnID string, owner *spawnDriverOwner, err error) {
	if err == nil || owner == nil {
		return
	}
	o.driversMu.Lock()
	if o.drivers[spawnID] == owner {
		owner.stopCleanupErr = errors.Join(owner.stopCleanupErr, err)
	}
	o.driversMu.Unlock()
}

func (o *SpawnOrchestrator) releaseSpawnDriver(spawnID string, owner *spawnDriverOwner) {
	if owner == nil {
		return
	}
	defer owner.cancel(nil)
	if o == nil {
		close(owner.done)
		return
	}
	o.driversMu.Lock()
	if o.drivers[spawnID] != owner {
		o.driversMu.Unlock()
		close(owner.done)
		return
	}
	if !owner.stopRequested {
		delete(o.drivers, spawnID)
		o.driversMu.Unlock()
		close(owner.done)
		return
	}
	cleanupDone := owner.stopCleanupDone
	o.driversMu.Unlock()

	// The stop caller owns cleanup of the pod known at cancellation time.
	// A pod returned late from Start is cleaned by runSpawn before it exits.
	// Waiting for both makes the stopped state a truthful cleanup barrier.
	if cleanupDone != nil {
		<-cleanupDone
	}

	var cleanupErr error
	o.driversMu.Lock()
	if o.drivers[spawnID] == owner {
		delete(o.drivers, spawnID)
		cleanupErr = owner.stopCleanupErr
	}
	o.driversMu.Unlock()
	if cleanupErr == nil {
		if stopped, ok, err := o.ctrl.CompleteStop(context.Background(), spawnID); err != nil {
			owner.stopCleanupErr = err
		} else if ok {
			o.finishStoppedSpawn(context.Background(), &stopped)
		}
	}
	close(owner.done)
}

// StopSpawn stops a running spawned agent.
