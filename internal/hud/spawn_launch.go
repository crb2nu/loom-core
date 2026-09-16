package hud

import (
	"context"
	"fmt"
)

type productionSpawnLaunchSpecBuilder struct{ orchestrator *SpawnOrchestrator }

func (b productionSpawnLaunchSpecBuilder) Build(ctx context.Context, req SpawnRequest) (SpawnRequest, error) {
	o := b.orchestrator
	if err := validateCompletionHoldSeconds(req.CompletionHoldSeconds); err != nil {
		return SpawnRequest{}, err
	}
	if len(o.projects) > 0 && !projectConfigured(req.Project, o.projects) {
		return SpawnRequest{}, fmt.Errorf("project %q is not configured for spawning", req.Project)
	}
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
		return SpawnRequest{}, err
	}
	return req, nil
}

type productionSpawnLauncher struct{ orchestrator *SpawnOrchestrator }

func (l productionSpawnLauncher) Launch(spawnID string, req SpawnRequest) {
	go l.orchestrator.runSpawn(spawnID, req)
}
