// This file owns keyed-spawn admission and runtime preflight.
// It also keeps spawn identity and active-request deduplication together.

package hud

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/spawn"
)

// preflightKeyedRuntime prevents a missing durable row from being interpreted
// as permission to claim a same-name pod or VM. Existing durable rows remain
// the controller's responsibility, but their runtime (when present) must also
// match the durable owner/generation before an idempotent reattach succeeds.
func (o *SpawnOrchestrator) preflightKeyedRuntime(ctx context.Context, req SpawnRequest) error {
	if o == nil || o.ctrl == nil || req.IdempotencyKey == "" {
		return nil
	}
	ownerID, recoveryAuthority := o.ctrl.Ownership()
	if ownerID == "" {
		return nil
	}
	spawnID := spawn.DeriveSpawnID(req.IdempotencyKey)
	durable, err := o.ctrl.LoadDurable(ctx, spawnID)
	if err != nil {
		return fmt.Errorf("load durable spawn %s before runtime preflight: %w", spawnID, err)
	}
	if durable == nil {
		if local, ok := o.ctrl.Get(spawnID); ok && local != nil {
			return fmt.Errorf(
				"%w for %s: local keyed spawn has no durable row",
				spawn.ErrSpawnStateConflict, spawnID,
			)
		}
	}
	if durable != nil {
		if durable.Request.IdempotencyKey != req.IdempotencyKey {
			return fmt.Errorf(
				"%w for %s: durable idempotency key %q does not match %q",
				spawn.ErrSpawnStateConflict, spawnID, durable.Request.IdempotencyKey, req.IdempotencyKey,
			)
		}
		if durable.DriverOwnerID != "" && durable.DriverOwnerID != ownerID {
			return fmt.Errorf(
				"%w for %s: durable spawn is owned by controller %q",
				spawn.ErrSpawnStateConflict, spawnID, durable.DriverOwnerID,
			)
		}
	}
	agentType := req.AgentType
	if agentType == "" {
		agentType = "claude-code"
	}
	agentID := fmt.Sprintf("spawn-%s-%s", agentType, spawnID[6:])
	var startedAt time.Time
	if durable != nil {
		startedAt = durable.StartedAt
		if durable.AgentID != "" {
			agentID = durable.AgentID
		}
	}
	opts := spawnRuntimeStartIdentityOpts(spawnID, agentID, ownerID, startedAt, recoveryAuthority)
	be := o.substrateBackend(req.Substrate)
	prober, ok := be.(backend.StartIdentityProber)
	if !ok {
		return fmt.Errorf("shared spawn runtime preflight: backend %q does not validate runtime identity", req.Substrate)
	}
	exists, err := prober.ProbeStartIdentity(ctx, opts)
	if err != nil {
		return fmt.Errorf("preflight keyed spawn %s runtime: %w", spawnID, err)
	}
	if exists && durable == nil {
		return fmt.Errorf(
			"%w: runtime %s exists without its durable spawn row; recovery must reconstruct it before registration",
			backend.ErrRuntimeIdentityConflict, opts.Name,
		)
	}
	return nil
}

func spawnRuntimeStartIdentityOpts(
	spawnID, agentID, ownerID string,
	startedAt time.Time,
	allowMissing bool,
) backend.StartOpts {
	return backend.StartOpts{
		Name:                       "spawn-" + spawnID,
		AgentID:                    agentID,
		ManagedByOverride:          spawn.ManagedByValue,
		ExtraLabels:                spawn.RuntimeIdentityLabels(spawnID, agentID, ownerID, startedAt),
		AllowMissingIdentityLabels: allowMissing,
	}
}

func (o *SpawnOrchestrator) existingActiveSpawnForRequest(req SpawnRequest) string {
	if o == nil || o.ctrl == nil {
		return ""
	}
	runID := firstNonEmptySpawnTag(req.Metadata["LOOM_MILLS_RUN_ID"], req.Metadata["loom_mills_run_id"])
	stage := firstNonEmptySpawnTag(req.Metadata["LOOM_MILLS_STAGE"], req.Metadata["loom_mills_stage"])
	if runID == "" || stage == "" {
		return ""
	}
	for _, state := range o.ctrl.List() {
		if state == nil || spawn.IsTerminal(state.Status) {
			continue
		}
		meta := state.Request.Metadata
		if firstNonEmptySpawnTag(meta["LOOM_MILLS_RUN_ID"], meta["loom_mills_run_id"]) != runID {
			continue
		}
		if firstNonEmptySpawnTag(meta["LOOM_MILLS_STAGE"], meta["loom_mills_stage"]) != stage {
			continue
		}
		requestAttempt := firstNonEmptySpawnTag(req.Metadata["LOOM_MILLS_ATTEMPT"], req.Metadata["loom_mills_attempt"])
		candidateAttempt := firstNonEmptySpawnTag(meta["LOOM_MILLS_ATTEMPT"], meta["loom_mills_attempt"])
		if requestAttempt != "" && candidateAttempt != "" && requestAttempt != candidateAttempt {
			continue
		}
		if req.Project != "" && state.Request.Project != "" && req.Project != state.Request.Project {
			continue
		}
		if req.Branch != "" && state.Request.Branch != "" && req.Branch != state.Request.Branch {
			continue
		}
		return state.SpawnID
	}
	return ""
}

func firstNonEmptySpawnTag(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// newSpawnParser creates the appropriate JSONL parser for the given agent type.
// Returns nil for agent types that don't support structured telemetry.
func newSpawnParser(agentType string, sink SpawnEventSink, agentID, spawnID string, broadcast SpawnEventBroadcaster, logger *slog.Logger) SpawnLineParser {
	switch agentType {
	case "claude-code":
		return NewClaudeJSONLParser(sink, agentID, spawnID, broadcast, logger)
	case "codex":
		return NewCodexJSONLParser(sink, agentID, spawnID, broadcast, logger)
	case "gemini":
		return NewGeminiJSONLParser(sink, agentID, spawnID, broadcast, logger)
	default:
		return nil
	}
}
