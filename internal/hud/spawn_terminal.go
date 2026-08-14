package hud

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

func (o *SpawnOrchestrator) finishStoppedSpawn(ctx context.Context, state *SpawnState) {
	if state == nil {
		return
	}
	if o.metrics != nil {
		if _, owned := o.activeSpawnMetrics.LoadAndDelete(state.SpawnID); owned {
			o.metrics.SpawnedAgentActive.Add(ctx, -1)
		}
	}
	o.broadcastSpawnEvent("agent.spawn.stopped", state)
	if o.agentBridge != nil {
		go func(agentID string) {
			summarize := false
			o.agentBridge.EndSession(bridge.SessionEndParams{AgentID: agentID, Summarize: &summarize})
		}(state.AgentID)
	}
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

// completeSpawn marks a spawn as completed.
func (o *SpawnOrchestrator) completeSpawn(ctx context.Context, state *SpawnState) {
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
		// Attach final telemetry snapshot if available.
		if terminalTelemetry != nil {
			current.Telemetry = terminalTelemetry
		}

		current.Status = SpawnStatusCompleted
		// Clear any stale Error left over from a transient reconcile tick that
		// poisoned the persisted state before the success path landed.
		current.Error = ""
		now := time.Now()
		current.EndedAt = &now
	})
	o.driversMu.Unlock()
	if persistErr != nil {
		o.logger.Error("failed to persist terminal spawn completion",
			"spawn_id", state.SpawnID, "error", persistErr)
		return
	}
	if !ok {
		return
	}
	o.telemetry.Delete(state.SpawnID)
	state = &updated

	// Persist final telemetry summary to the agent-context session.
	o.persistTelemetrySummary(state, string(SpawnStatusCompleted))

	if o.metrics != nil {
		if _, owned := o.activeSpawnMetrics.LoadAndDelete(state.SpawnID); owned {
			o.metrics.SpawnedAgentActive.Add(ctx, -1)
		}
		o.metrics.AgentSpawnTotal.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("agent_type", state.Request.AgentType),
				attribute.String("outcome", "completed"),
			),
		)
	}

	// Record spawn telemetry metrics.
	if o.metrics != nil && state.Telemetry != nil {
		o.recordSpawnTelemetryMetrics(ctx, state)
	}

	o.logger.Info("spawn completed", "spawn_id", state.SpawnID)
	o.broadcastSpawnEvent("agent.spawn.completed", state)

	// End the agent session with summarization. Nil-safe to support tests
	// that construct a minimal orchestrator without a real agent bridge.
	if o.agentBridge != nil {
		go func() {
			summarize := true
			o.agentBridge.EndSession(bridge.SessionEndParams{
				AgentID:   state.AgentID,
				Summarize: &summarize,
			})
		}()
	}
}

// reapTerminalSpawn is the K8sController.TerminalHook invocation: it
// releases the cluster + agent-context resources for a terminal spawn
// so neither stale pods nor stale presence rows accumulate.
//
// Runtime cleanup is the ownership fence for every later agent-context side
// effect. If it fails, the hook returns immediately: deregistering a recycled
// deterministic AgentID after losing the runtime generation would corrupt the
// replacement spawn. Reconcile stamps CleanupAt only after the exact durable
// generation succeeds and retries failures on its next tick.
func (o *SpawnOrchestrator) reapTerminalSpawn(ctx context.Context, state spawn.State) error {
	o.logger.Info("reaping terminal spawn",
		"spawn_id", state.SpawnID,
		"agent_id", state.AgentID,
		"pod", state.PodName,
		"status", state.Status,
	)
	if err := o.requireCurrentTerminalGeneration(&state); err != nil {
		return err
	}

	if state.PodName != "" {
		be := o.substrateBackend(state.Request.Substrate)
		if be == nil {
			return fmt.Errorf("stop pod %s: no substrate backend", state.PodName)
		}
		if err := o.stopSpawnRuntime(ctx, be, &state, state.PodName); err != nil {
			o.logger.Warn("reap: failed to stop pod",
				"spawn_id", state.SpawnID, "pod", state.PodName, "error", err)
			return fmt.Errorf("stop pod %s: %w", state.PodName, err)
		}
	}

	// The runtime call can block long enough for an external store repair or a
	// peer takeover to replace this deterministic spawn ID. Revalidate before
	// touching the likewise deterministic agent identity.
	if err := o.requireCurrentTerminalGeneration(&state); err != nil {
		return err
	}

	// Hygiene: if a spawn reached a clean terminal status (completed /
	// stopped) but still carries a stale Error from an earlier reconcile
	// tick (e.g., "pod not found during reconciliation"), clear it and
	// re-persist so the ConfigMap row matches the actual outcome. Failed
	// spawns intentionally keep their Error.
	if (state.Status == spawn.StatusCompleted || state.Status == spawn.StatusStopped) && state.Error != "" && o.ctrl != nil {
		if _, _, err := o.ctrl.ClearTerminalErrorForGeneration(ctx, state); err != nil {
			return fmt.Errorf("repair terminal spawn error: %w", err)
		}
	}

	if state.AgentID != "" && o.agentBridge != nil {
		if err := o.requireCurrentTerminalGeneration(&state); err != nil {
			return err
		}
		presenceDeregistered, presenceErr := o.deregisterTerminalSpawnPresence(state)
		// EndSession is idempotent and safe to call even when the
		// session already ended in failSpawn/completeSpawn. It guards
		// against the operator restart path where the prior process
		// died before reaching its EndSession call.
		summarize := false
		if _, err := o.agentBridge.EndSession(bridge.SessionEndParams{
			SessionID:              state.SessionID,
			AgentID:                state.AgentID,
			Summarize:              &summarize,
			SkipPresenceDeregister: true,
		}); err != nil {
			o.logger.Warn("reap: failed to end session",
				"spawn_id", state.SpawnID,
				"agent_id", state.AgentID,
				"cleanup_state", "local_terminal_acknowledged",
				"retryable", false,
				"error", err,
			)
		}
		if !presenceDeregistered && presenceErr != nil {
			o.logger.Warn("reap: presence deregistration exhausted; acknowledging local terminal cleanup",
				"spawn_id", state.SpawnID,
				"agent_id", state.AgentID,
				"cleanup_state", "local_terminal_acknowledged",
				"attempts", terminalPresenceDeregisterAttempts,
				"retryable", false,
				"error", presenceErr,
			)
		}
	}
	return nil
}

func (o *SpawnOrchestrator) deregisterTerminalSpawnPresence(state spawn.State) (bool, error) {
	if o == nil || o.agentBridge == nil || strings.TrimSpace(state.AgentID) == "" {
		return true, nil
	}
	var lastErr error
	for attempt := 1; attempt <= terminalPresenceDeregisterAttempts; attempt++ {
		if err := o.agentBridge.PresenceDeregisterWithTimeout(state.AgentID, terminalPresenceDeregisterTimeout); err != nil {
			lastErr = err
			o.logger.Warn("reap: failed to deregister presence",
				"spawn_id", state.SpawnID,
				"agent_id", state.AgentID,
				"attempt", attempt,
				"max_attempts", terminalPresenceDeregisterAttempts,
				"timeout", terminalPresenceDeregisterTimeout.String(),
				"cleanup_state", "presence_deregister_retrying",
				"retryable", attempt < terminalPresenceDeregisterAttempts,
				"error", err,
			)
			continue
		}
		if attempt > 1 {
			o.logger.Info("reap: deregistered presence after retry",
				"spawn_id", state.SpawnID,
				"agent_id", state.AgentID,
				"attempt", attempt,
				"cleanup_state", "presence_deregistered",
			)
		}
		return true, nil
	}
	return false, lastErr
}

func (o *SpawnOrchestrator) requireCurrentTerminalGeneration(expected *spawn.State) error {
	if o == nil || o.ctrl == nil || expected == nil {
		return nil
	}
	current, ok := o.ctrl.Get(expected.SpawnID)
	if !ok || current == nil || current.CleanupAt != nil || !spawnGenerationMatches(current, expected) ||
		current.Status != expected.Status || current.AgentID != expected.AgentID || current.PodName != expected.PodName {
		return fmt.Errorf(
			"%w for %s: terminal cleanup snapshot is no longer current",
			spawn.ErrSpawnStateConflict, expected.SpawnID,
		)
	}
	return nil
}

func spawnGenerationMatches(left, right *spawn.State) bool {
	if left == nil || right == nil {
		return false
	}
	return left.SpawnID == right.SpawnID &&
		left.DriverOwnerID == right.DriverOwnerID &&
		left.Request.IdempotencyKey == right.Request.IdempotencyKey &&
		left.StartedAt.Equal(right.StartedAt)
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
