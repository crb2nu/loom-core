// Recovery and resume paths for durable spawns interrupted by HUD restarts.
// This file keeps startup reconciliation separate from the active spawn flow.
package hud

import (
	"context"
	"errors"
	"fmt"

	"github.com/crb2nu/loom/internal/spawn"
)

func (o *SpawnOrchestrator) resumePreRuntimeSpawns() {
	if o == nil || o.ctrl == nil || len(o.backends) == 0 {
		return
	}
	for _, state := range o.ctrl.List() {
		if state == nil || state.StopRequestedAt != nil || state.PodName != "" || !isPreRuntimeSpawnStatus(state.Status) {
			continue
		}
		if state.Request.TaskDescription == "" || state.Request.Project == "" {
			continue
		}
		spawnID := state.SpawnID
		req := state.Request
		o.logger.Info("resuming pre-runtime spawn after HUD restart",
			"spawn_id", spawnID,
			"status", state.Status,
			"agent_type", req.AgentType,
			"project", req.Project,
		)
		go o.runSpawn(spawnID, req)
	}
}

// recoverStoppingSpawns completes durable stop intents whose owning HUD
// process exited before cleanup finished. A failed delete remains active with
// its pod handle + error so the next recovery/StopSpawn call retries it.
func (o *SpawnOrchestrator) recoverStoppingSpawns() {
	if o == nil || o.ctrl == nil || len(o.backends) == 0 {
		return
	}
	for _, state := range o.ctrl.List() {
		if state == nil || state.StopRequestedAt == nil || spawn.IsTerminal(state.Status) {
			continue
		}
		if err := o.cleanupStoppingSpawn(context.Background(), *state); err != nil && o.logger != nil {
			o.logger.Warn("failed to recover stopping spawn", "spawn_id", state.SpawnID, "error", err)
		}
	}
}

// reconcileStoppingSpawn is installed on the controller's 30-second loop so
// any durable stop intent without an in-process owner remains retryable.
func (o *SpawnOrchestrator) reconcileStoppingSpawn(ctx context.Context, state spawn.State) error {
	o.driversMu.Lock()
	activeDriver := o.drivers[state.SpawnID] != nil
	o.driversMu.Unlock()
	if activeDriver {
		return nil
	}
	return o.cleanupStoppingSpawn(ctx, state)
}

func (o *SpawnOrchestrator) cleanupStoppingSpawn(ctx context.Context, state spawn.State) error {
	// Callers pass a snapshot that may predate a late Start's authoritative
	// pod handle (recorded by cleanupLateSpawn before the driver is released,
	// and both callers check for driver absence first). Re-read so the retry
	// targets the retained handle instead of resurrecting the fallback name.
	if current, ok := o.ctrl.Get(state.SpawnID); ok && current != nil {
		state = *current
	}
	podName := state.PodName
	if podName == "" {
		podName = "spawn-" + state.SpawnID
	}
	if _, _, err := o.ctrl.RecordStoppingPod(context.Background(), state.SpawnID, podName); err != nil {
		return err
	}
	be := o.substrateBackend(state.Request.Substrate)
	if be == nil {
		err := errors.New("no substrate backend")
		_, _, persistErr := o.ctrl.RecordStopCleanupFailure(context.Background(), state.SpawnID, podName, err.Error())
		return errors.Join(err, persistErr)
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, spawnDriverStopTimeout)
	defer cancel()
	if err := o.stopSpawnRuntime(cleanupCtx, be, &state, podName); err != nil {
		wrapped := fmt.Errorf("cleanup stopping spawn pod %s: %w", podName, err)
		_, _, persistErr := o.ctrl.RecordStopCleanupFailure(context.Background(), state.SpawnID, podName, wrapped.Error())
		return errors.Join(wrapped, persistErr)
	}
	if stopped, ok, err := o.ctrl.CompleteStop(context.Background(), state.SpawnID); err != nil {
		return err
	} else if ok {
		o.finishStoppedSpawn(context.Background(), &stopped)
	}
	return nil
}

func isPreRuntimeSpawnStatus(status spawn.Status) bool {
	switch status {
	case spawn.StatusPending, spawn.StatusBuilding:
		return true
	default:
		return false
	}
}

// interruptedSpawnAction is the pure classification of what restart
// recovery does with one recovered non-terminal spawn whose agent-turn
// driver died with the previous process.
type interruptedSpawnAction int

const (
	// interruptedSkip: terminal, or pre-runtime with no pod — the latter is
	// owned by resumePreRuntimeSpawns.
	interruptedSkip interruptedSpawnAction = iota
	// interruptedRedrive: keyed spawn — re-run the full spawn flow. The
	// image build is cached, pod creation AlreadyExists-adopts the live pod
	// (startSpawnPod), and the agent turn re-executes in the same workspace.
	// Spawn id, pod name, and worktree stay stable across the crash; the agent
	// CLI turn itself is re-executed at-least-once.
	interruptedRedrive
	// interruptedFailFast: unkeyed spawn (or one missing the request fields
	// a re-drive needs) — fail immediately with an honest error instead of
	// holding a pool slot until the reconciler's ~65-minute deadline
	// backstop fires.
	interruptedFailFast
	// interruptedReattach: keyed + supervised spawn (S4) — do NOT re-drive.
	// The turn runs under a detached in-pod reaper that survived the crash;
	// recovery probes the reaper and RE-ATTACHES (or collects the recorded
	// outcome), preserving the original completion-wrapper/hold process pair.
	// recoverInterruptedSpawns refines this into reattach/collect/re-drive from
	// a live supervisorProbe (classifySupervisorProbe).
	interruptedReattach
)

// classifyInterruptedSpawn decides the restart-recovery action for one
// recovered spawn state. Pure; unit-tested directly.
//
// A supervised spawn (S4) short-circuits to interruptedReattach so the in-pod
// reaper's process pair is preserved. A non-supervised keyed spawn keeps the
// legacy interruptedRedrive path exactly, so pods launched by an older
// controller (State.Supervised false) still recover.
func classifyInterruptedSpawn(state *spawn.State) interruptedSpawnAction {
	if state == nil || state.StopRequestedAt != nil || spawn.IsTerminal(state.Status) {
		return interruptedSkip
	}
	if state.PodName == "" && isPreRuntimeSpawnStatus(state.Status) {
		return interruptedSkip // resumePreRuntimeSpawns owns this shape
	}
	req := state.Request
	keyed := req.IdempotencyKey != "" && req.TaskDescription != "" && req.Project != ""
	if !keyed {
		return interruptedFailFast
	}
	if state.Supervised {
		return interruptedReattach
	}
	return interruptedRedrive
}

// recoverInterruptedSpawns handles non-terminal spawns recovered from the
// store whose agent turn was being driven by the previous process. Exec
// sessions die with the driving process, so such a spawn can never progress
// on its own: the pod idles (PID 1 is sleep-infinity) while the durable
// record stays `running` — the S1c dual-crash kill-test wedge, loom-core#300.
// Before this, the only recovery was the reconciler's ~65-minute deadline
// backstop (spawnDeadlineExceeded), which fails the spawn without ever
// retrying the turn.
//
// Keyed spawns are re-driven (see interruptedRedrive); semantically this is
// the same turn-restart the Mills pipeline's auto-retry performs, but
// transparent to the caller — a workflow-runtime Resume polling the spawn id
// sees it complete instead of hanging. Unkeyed spawns cannot be re-attached
// (the AlreadyExists backstop deliberately refuses non-derived pod names),
// so they fail fast with an honest error.
func (o *SpawnOrchestrator) recoverInterruptedSpawns() {
	if o == nil || o.ctrl == nil || len(o.backends) == 0 {
		return
	}
	redrive := o.redriveSpawn
	if redrive == nil {
		redrive = func(spawnID string, req SpawnRequest) { go o.runSpawn(spawnID, req) }
	}
	reattach := o.reattachSpawn
	if reattach == nil {
		reattach = func(spawnID string, req SpawnRequest) { go o.runSpawnReattach(spawnID, req) }
	}
	ctx := context.Background()
	for _, state := range o.ctrl.List() {
		switch classifyInterruptedSpawn(state) {
		case interruptedRedrive:
			o.logger.Info("re-driving interrupted spawn after HUD restart",
				"spawn_id", state.SpawnID,
				"status", state.Status,
				"pod", state.PodName,
				"agent_type", state.Request.AgentType,
				"project", state.Request.Project,
			)
			redrive(state.SpawnID, state.Request)
		case interruptedReattach:
			o.recoverSupervisedSpawn(ctx, state, reattach, redrive)
		case interruptedFailFast:
			o.logger.Warn("failing interrupted spawn after HUD restart (unkeyed; cannot re-attach)",
				"spawn_id", state.SpawnID,
				"status", state.Status,
				"pod", state.PodName,
			)
			o.failSpawn(ctx, state,
				"agent turn driver lost across mobile-hud restart; unkeyed spawn cannot be re-driven")
		}
	}
}

// recoverSupervisedSpawn refines a supervised interrupted spawn (S4) by probing
// the in-pod reaper: an alive reaper or a recorded outcome is RE-ATTACHED
// (runSpawnReattach handles both — collect returns immediately, reattach
// tails+waits); a vanished supervisor (died mid-flight) falls back to the legacy
// re-drive for liveness. A probe error is treated as "unknown" and re-driven so
// the spawn never wedges — continuity is lost on that fallback, but liveness is
// kept, matching the runbook's contract.
func (o *SpawnOrchestrator) recoverSupervisedSpawn(
	ctx context.Context,
	state *spawn.State,
	reattach, redrive func(spawnID string, req SpawnRequest),
) {
	probeFn := o.probeSupervisorFn
	if probeFn == nil {
		probeFn = o.probeSupervisor
	}
	probe, err := probeFn(ctx, state.Request.Substrate, state.SpawnID)
	if err != nil {
		o.logger.Warn("supervised spawn probe failed on recovery; re-driving for liveness",
			"spawn_id", state.SpawnID, "pod", state.PodName, "error", err)
		redrive(state.SpawnID, state.Request)
		return
	}
	switch action := classifySupervisorProbe(probe); action {
	case supervisedRedrive:
		o.logger.Info("supervised spawn supervisor gone on recovery; re-driving for liveness",
			"spawn_id", state.SpawnID, "pod", state.PodName)
		redrive(state.SpawnID, state.Request)
	default: // supervisedReattach or supervisedCollect
		o.logger.Info("re-attaching to supervised spawn after HUD restart",
			"spawn_id", state.SpawnID,
			"status", state.Status,
			"pod", state.PodName,
			"reaper_alive", probe.ReaperAlive,
			"outcome_present", probe.OutcomePresent,
		)
		reattach(state.SpawnID, state.Request)
	}
}
