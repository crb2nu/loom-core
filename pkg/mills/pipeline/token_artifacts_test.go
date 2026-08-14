package pipeline

import (
	"context"
	"testing"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type usageWeaverClient struct {
	resp WeaverResponse
}

func (c *usageWeaverClient) Research(context.Context, WeaverRequest) (WeaverResponse, error) {
	return c.resp, nil
}

func TestSpawnResponseToStageOutput_WritesTokenArtifacts(t *testing.T) {
	out := spawnResponseToStageOutput(SpawnResponse{
		SpawnID:   "spawn-1",
		CostUSD:   1.5,
		Artifacts: map[string]any{"agent_id": "claude-1", "status": "completed"},
		TokenUsage: SpawnTokenUsage{
			InputTokens:         8192,
			OutputTokens:        1024,
			CacheCreationTokens: 4096,
			CacheReadTokens:     65536,
		},
	})
	for key, want := range map[string]int{
		spawnInputTokensArtifactKey:         8192,
		spawnOutputTokensArtifactKey:        1024,
		spawnCacheCreationTokensArtifactKey: 4096,
		spawnCacheReadTokensArtifactKey:     65536,
	} {
		if out.Artifacts[key] != want {
			t.Errorf("artifact %s = %v, want %d", key, out.Artifacts[key], want)
		}
	}
	// Pre-existing artifact slots must survive the merge untouched.
	if out.Artifacts["agent_id"] != "claude-1" || out.Artifacts["status"] != "completed" {
		t.Errorf("existing artifacts clobbered: %+v", out.Artifacts)
	}
}

// TestSpawnResponseToStageOutput_UnreportedUsageLeavesArtifacts asserts the
// additive guarantee: a harness that reports no tokens must produce a
// byte-identical artifact map, not a row of zeros that an aggregation would
// read as a measured 0% cache hit rate.
func TestSpawnResponseToStageOutput_UnreportedUsageLeavesArtifacts(t *testing.T) {
	out := spawnResponseToStageOutput(SpawnResponse{
		SpawnID:   "spawn-quiet",
		Artifacts: map[string]any{"status": "completed"},
	})
	if len(out.Artifacts) != 1 {
		t.Errorf("artifacts = %+v, want only the pre-existing status slot", out.Artifacts)
	}
	for _, key := range []string{
		spawnInputTokensArtifactKey, spawnOutputTokensArtifactKey,
		spawnCacheReadTokensArtifactKey, spawnCacheCreationTokensArtifactKey,
	} {
		if _, ok := out.Artifacts[key]; ok {
			t.Errorf("artifact %s written for an unreported usage block", key)
		}
	}
}

// TestSpawnResponseToStageOutput_PartialUsageOmitsZeros covers Codex and
// Gemini, which report input/output/cache-read but no cache-creation count.
// Writing an explicit 0 there would be indistinguishable from a Claude spawn
// that genuinely wrote nothing into the cache.
func TestSpawnResponseToStageOutput_PartialUsageOmitsZeros(t *testing.T) {
	out := spawnResponseToStageOutput(SpawnResponse{
		TokenUsage: SpawnTokenUsage{InputTokens: 500, OutputTokens: 100, CacheReadTokens: 2000},
	})
	if _, ok := out.Artifacts[spawnCacheCreationTokensArtifactKey]; ok {
		t.Errorf("cache_creation_tokens written despite a zero count: %+v", out.Artifacts)
	}
	if out.Artifacts[spawnCacheReadTokensArtifactKey] != 2000 {
		t.Errorf("cache_read_tokens = %v, want 2000", out.Artifacts[spawnCacheReadTokensArtifactKey])
	}
}

// TestTokenArtifactsPersistThroughMergeArtifacts is the assertion that
// matters operationally: mergeArtifacts is what actually lands in
// stage_results.artifacts_json, so the keys are only queryable if they
// survive that flattening.
func TestTokenArtifactsPersistThroughMergeArtifacts(t *testing.T) {
	out := spawnResponseToStageOutput(SpawnResponse{
		TokenUsage: SpawnTokenUsage{
			InputTokens:         10,
			OutputTokens:        20,
			CacheCreationTokens: 30,
			CacheReadTokens:     40,
		},
	})
	merged := mergeArtifacts("implement", out)
	for key, want := range map[string]int{
		spawnInputTokensArtifactKey:         10,
		spawnOutputTokensArtifactKey:        20,
		spawnCacheCreationTokensArtifactKey: 30,
		spawnCacheReadTokensArtifactKey:     40,
	} {
		if merged[key] != want {
			t.Errorf("merged artifact %s = %v, want %d", key, merged[key], want)
		}
	}
	if merged["stage_id"] != "implement" {
		t.Errorf("stage_id = %v, want implement", merged["stage_id"])
	}
}

func TestWeaverWorker_WritesResearchTokenArtifacts(t *testing.T) {
	w := &WeaverWorker{
		Client: &usageWeaverClient{resp: WeaverResponse{
			Notes: "grounded notes",
			Usage: llmusage.Usage{
				PromptTokens:       4096,
				CachedPromptTokens: 3968,
				CompletionTokens:   256,
			},
		}},
		PromptFor: func(JobContext) string { return "research X" },
	}
	out, err := w.Run(context.Background(), JobContext{Item: &store.BacklogItem{ID: "BL-1"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for key, want := range map[string]int{
		researchPromptTokensArtifactKey:     4096,
		researchCachedTokensArtifactKey:     3968,
		researchCompletionTokensArtifactKey: 256,
	} {
		if out.Artifacts[key] != want {
			t.Errorf("artifact %s = %v, want %d", key, out.Artifacts[key], want)
		}
	}
	if out.Artifacts["research_notes"] != "grounded notes" {
		t.Errorf("research_notes clobbered: %+v", out.Artifacts)
	}
	// And they must survive the flattening into stage_results.
	if merged := mergeArtifacts("research", out); merged[researchCachedTokensArtifactKey] != 3968 {
		t.Errorf("cached_prompt_tokens lost in mergeArtifacts: %+v", merged)
	}
}

// TestWeaverWorker_DelegatedResearchReportsNoUsage covers the weaver-RPC
// path, which has no usage block to report. The row must keep its previous
// shape rather than gaining zeros.
func TestWeaverWorker_DelegatedResearchReportsNoUsage(t *testing.T) {
	w := &WeaverWorker{
		Client:    &usageWeaverClient{resp: WeaverResponse{Notes: "delegated notes"}},
		PromptFor: func(JobContext) string { return "research X" },
	}
	out, err := w.Run(context.Background(), JobContext{Item: &store.BacklogItem{ID: "BL-2"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, key := range []string{
		researchPromptTokensArtifactKey,
		researchCompletionTokensArtifactKey,
		researchCachedTokensArtifactKey,
	} {
		if _, ok := out.Artifacts[key]; ok {
			t.Errorf("artifact %s written for a lane that reported no usage", key)
		}
	}
}
