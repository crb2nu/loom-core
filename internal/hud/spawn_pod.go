package hud

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/spawn"
)

// startSpawnPod creates the runtime pod via the substrate backend and applies
// the stable-handle-across-crash re-attach backstop on a k8s AlreadyExists.
//
// Keyed spawns derive a deterministic pod name. Across a HUD crash the
// in-memory key map is empty, so Start may find the pod created by the prior
// process. Adopt it only after its runtime identity has been verified.
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

	// The empty-key path fails IsDerivedSpawnName, preserving legacy errors.
	if apierrors.IsAlreadyExists(err) && spawn.IsDerivedSpawnName(opts.Name, req.IdempotencyKey) {
		// Only the K8s streaming backend can reattach by pod name. A
		// Harvester VM's SSH key is process-local, so its handle is not safe
		// to adopt after a restart.
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
		return &backend.StartResult{ContainerID: opts.Name}, nil
	}

	return nil, err
}

// StopSpawn persists stop intent, cleans the current runtime, and waits for
// the lifecycle owner to reap any pod returned after cancellation.
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
	if o.stopIntentPersisted != nil {
		o.stopIntentPersisted(spawnID)
	}
	if disposition == spawn.StopTerminal {
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
		performCleanup = true
	}
	podName := state.PodName
	if podName == "" {
		podName = "spawn-" + spawnID
	}
	substrate := state.Request.Substrate
	var cleanupErr error
	if performCleanup && !terminalWinner {
		// Keep a retry handle while holding driversMu. A late Start records its
		// authoritative handle only after updateSpawnFromDriver takes this lock.
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

func (o *SpawnOrchestrator) cleanupLateSpawn(
	be backend.Backend,
	spawnID, containerID string,
	owner *spawnDriverOwner,
) {
	if be == nil || containerID == "" {
		return
	}
	_, _, persistPodErr := o.ctrl.RecordStoppingPod(context.Background(), spawnID, containerID)
	if persistPodErr != nil {
		o.setStopCleanupError(spawnID, owner, persistPodErr)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state, ok := o.ctrl.Get(spawnID)
	if !ok {
		o.setStopCleanupError(spawnID, owner, fmt.Errorf("clean up spawn pod %s: state not found", containerID))
		return
	}
	if err := o.stopSpawnRuntime(cleanupCtx, be, state, containerID); err != nil {
		wrapped := fmt.Errorf("clean up spawn pod returned after cancellation: %w", err)
		_, _, persistFailureErr := o.ctrl.RecordStopCleanupFailure(context.Background(), spawnID, containerID, wrapped.Error())
		o.setStopCleanupError(spawnID, owner, errors.Join(wrapped, persistFailureErr))
		if o.logger != nil {
			o.logger.Warn("failed to clean up spawn pod returned after cancellation",
				"spawn_id", spawnID, "pod", containerID, "error", err)
		}
	}
}

func (o *SpawnOrchestrator) stopSpawnRuntime(
	ctx context.Context,
	be backend.Backend,
	state *spawn.State,
	runtimeName string,
) error {
	if be == nil {
		return errors.New("no substrate backend")
	}
	if state == nil || state.DriverOwnerID == "" {
		return be.Stop(ctx, runtimeName)
	}
	stopper, ok := be.(backend.IdentityStopper)
	if !ok {
		return fmt.Errorf(
			"shared spawn runtime cleanup: backend cannot conditionally stop %s",
			runtimeName,
		)
	}
	return stopper.StopIfIdentity(ctx, runtimeName, spawn.RuntimeIdentityLabelsForState(state))
}
