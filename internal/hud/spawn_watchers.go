// This file owns spawn budget, liveness, handoff, and heartbeat watchers.
// It keeps supervision loops separate from spawn orchestration.

package hud

import (
	"context"
	"fmt"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// runBudgetWatcher polls the spawn telemetry accumulator at a fixed interval
// and cancels the exec context when the configured cost or turn budget is
// exceeded. It records a structured error on the accumulator ("max_budget" or
// "max_turns") so the persisted telemetry captures the reason. The watcher
// exits when its own ctx is canceled (exec returned / parent canceled) or when
// done is closed by the caller after the exec returns.
func (o *SpawnOrchestrator) runBudgetWatcher(
	ctx context.Context,
	spawnID string,
	req SpawnRequest,
	acc *bridge.SpawnTelemetryAccumulator,
	cancelExec context.CancelCauseFunc,
	done <-chan struct{},
) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			snap := acc.Snapshot()
			if req.MaxCostUSD > 0 && snap.TotalCostUSD >= req.MaxCostUSD {
				msg := fmt.Sprintf("spawn %s cost budget exceeded: $%.4f >= $%.4f",
					spawnID, snap.TotalCostUSD, req.MaxCostUSD)
				acc.AddError("max_budget", msg)
				o.logger.Warn("spawn budget exceeded, canceling exec",
					"spawn_id", spawnID, "cost_usd", snap.TotalCostUSD, "max_cost_usd", req.MaxCostUSD)
				cancelExec(context.Canceled)
				return
			}
			if req.MaxTurns > 0 && snap.TurnCount >= req.MaxTurns {
				msg := fmt.Sprintf("spawn %s turn budget exceeded: %d >= %d",
					spawnID, snap.TurnCount, req.MaxTurns)
				acc.AddError("max_turns", msg)
				o.logger.Warn("spawn turn budget exceeded, canceling exec",
					"spawn_id", spawnID, "turns", snap.TurnCount, "max_turns", req.MaxTurns)
				cancelExec(context.Canceled)
				return
			}
			// F5 / Slice C1: auto-handoff triggers. Nil-safe — skipped when the
			// hook is not installed or not enabled.
			o.evalAutoHandoff(ctx, spawnID, req, snap)
		}
	}
}

// runLivenessWatcher cancels the exec context when a streaming spawn produces
// no agent output for longer than stallTimeout — the zombie-pod guard. A
// healthy agent continuously emits JSONL lines (each Touch()es the
// accumulator), so a stalled lastActivity means the container is Running but
// the process inside is dead. On trip it records a structured "stalled" error
// on the accumulator for persisted telemetry and cancels exec with
// errSpawnStalled as the cause, which the finalize path reads to fail the
// spawn with a precise reason (→ pod cleanup → Mills operator auto-retry).
//
// The poll cadence is derived from stallTimeout (~1/8th, clamped to
// [100ms, 30s]) so production's 15-minute timeout polls at the cheap 30s
// ceiling while short override/test timeouts still trip promptly.
// The watcher exits when its ctx is canceled (exec returned / parent canceled)
// or when done is closed by the caller after the exec returns.
func (o *SpawnOrchestrator) runLivenessWatcher(
	ctx context.Context,
	spawnID string,
	acc *bridge.SpawnTelemetryAccumulator,
	stallTimeout time.Duration,
	cancelExec context.CancelCauseFunc,
	done <-chan struct{},
) {
	if stallTimeout <= 0 {
		return
	}
	poll := stallTimeout / 8
	if poll > 30*time.Second {
		poll = 30 * time.Second
	}
	if poll < 100*time.Millisecond {
		poll = 100 * time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			idle := time.Since(acc.LastActivity())
			if idle < stallTimeout {
				continue
			}
			msg := fmt.Sprintf("spawn %s stalled: no agent output for %s (>= %s stall timeout)",
				spawnID, idle.Round(time.Second), stallTimeout)
			acc.AddError("stalled", msg)
			o.logger.Warn("spawn liveness watchdog tripped, canceling exec",
				"spawn_id", spawnID, "idle", idle.Round(time.Second), "stall_timeout", stallTimeout)
			cancelExec(fmt.Errorf("%w: %s", errSpawnStalled, msg))
			return
		}
	}
}

// evalAutoHandoff checks the current telemetry snapshot against the
// configured auto-handoff thresholds and, on a gate fire, creates a
// draft handoff. Additive, ≤25 lines.
func (o *SpawnOrchestrator) evalAutoHandoff(ctx context.Context, spawnID string, req SpawnRequest, snap bridge.SpawnTelemetry) {
	hook := o.autoHandoff
	if hook == nil {
		return
	}
	cfg := hook.Config()
	if !cfg.Enabled {
		return
	}
	var reason string
	switch {
	case cfg.InputTokenHigh > 0 && snap.TokenUsage.InputTokens >= cfg.InputTokenHigh:
		reason = "input_tokens"
	case cfg.CostUSDHigh > 0 && snap.TotalCostUSD >= cfg.CostUSDHigh:
		reason = "cost"
	}
	if !hook.Observe(spawnID, reason, time.Now()) {
		return
	}
	details := map[string]any{"input_tokens": snap.TokenUsage.InputTokens, "cost_usd": snap.TotalCostUSD, "turns": snap.TurnCount}
	if err := hook.Create(ctx, spawnID, req.AgentType, req.AgentType, reason, details); err != nil {
		o.logger.Warn("auto-handoff create failed", "spawn_id", spawnID, "reason", reason, "error", err)
	}
}

// runHeartbeatLoop sends periodic heartbeats for a spawned agent while it's running.
func (o *SpawnOrchestrator) runHeartbeatLoop(ctx context.Context, state *SpawnState) {
	spawnID := state.SpawnID
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.driversMu.Lock()
			live, ok := o.ctrl.Get(spawnID)
			if !ok || live == nil || live.Status != SpawnStatusRunning {
				o.driversMu.Unlock()
				return
			}
			agentID := live.AgentID
			task := live.Request.TaskDescription
			branch := live.Request.Branch
			o.driversMu.Unlock()
			if o.agentBridge == nil {
				return
			}
			_, _ = o.agentBridge.PresenceHeartbeat(agentID, bridge.PresenceHeartbeatParams{
				Status:      "active",
				CurrentTask: task,
				Branch:      branch,
			})
		}
	}
}
