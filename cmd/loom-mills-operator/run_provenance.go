package main

import (
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// provenancePromptHashes digests every prompt template a stage of this binary
// can run under, keyed by surface: `stage_prompt:<stage>` for the per-stage
// bodies stagePromptWithPreamble renders the item into, `judge_rubric:<name>`
// for the LLM-judge rubrics.
//
// Only compile-time-stable bytes are hashed. The rendered per-item prompt
// (journal preamble, item context, retry context) is assembled inside the
// worker at dispatch and is not a template — hashing it here would produce a
// digest that changes per item and joins nothing.
//
// The templates are constants, so the operator computes this once and reuses
// the map for every run.
func provenancePromptHashes() map[string]string {
	hashes := make(map[string]string, len(stagePromptTemplates)+2)
	for stage, tmpl := range stagePromptTemplates {
		hashes["stage_prompt:"+stage] = mills.ProvenanceDigest([]byte(tmpl))
	}
	hashes["judge_rubric:"+gates.SpecConformanceRubricName] =
		mills.ProvenanceDigest([]byte(gates.SpecConformanceRubric))
	hashes["judge_rubric:"+gates.PRSelfReviewRubricName] =
		mills.ProvenanceDigest([]byte(gates.PRSelfReviewRubric))
	return hashes
}

// provenanceStageModels resolves the model each spawn-driven stage would
// dispatch under for one item, through the same precedence chain the
// SpawnWorker consults. Reading policy directly would miss the
// LOOM_MILLS_SPAWN_MODEL / LOOM_MILLS_SPAWN_AGENT break-glass, which is exactly
// the case where knowing what a run actually ran on matters most.
//
// Stages with no pin are omitted: the empty model means "vendor CLI default",
// which names no version to join on.
func provenanceStageModels(
	resolve func(string, *store.BacklogItem) mills.AgentDecision,
) func(*store.BacklogItem) map[string]string {
	return func(item *store.BacklogItem) map[string]string {
		models := make(map[string]string, len(mills.StageModelKeysValid))
		for stage := range mills.StageModelKeysValid {
			if model := resolve(stage, item).Model; model != "" {
				models[stage] = model
			}
		}
		return models
	}
}
