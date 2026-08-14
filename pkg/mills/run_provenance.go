package mills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// RunProvenanceEventKind is the run-start stamp recording the exact
// configuration a run was dispatched under. It is the join key every
// per-version analysis depends on: win rate and cost per policy revision, per
// stage-model pin, per prompt template. Without it a merged run is an
// unattributable data point — the models and prompts that produced it are only
// recoverable by guessing from deploy timestamps.
const RunProvenanceEventKind = "run.provenance"

// RunProvenanceActor is the actor on the stamp. It is distinct from
// "reconciler" so provenance rows are trivially separable from the
// reconciler's operational event stream.
const RunProvenanceActor = "reconciler.provenance"

// ProvenanceDigest is the one digest format every provenance field uses:
// sha256 over exact bytes, 64 lowercase hex characters. Policy checksums and
// prompt-template hashes share it so a stamped value can be compared against
// the deployment's loom.flexinfer.ai/policy-checksum annotation without
// per-field normalization rules.
func ProvenanceDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// stampRunProvenance records the configuration a run starts under, keyed on
// (subject_kind, subject_id=runID) so both lanes stamp identically —
// pipeline_run for the DAG lane, workflow_run for imperative starts.
//
// It sits at the same post-commit boundary as squad routing and copies its
// exactly-once shape: read an existing stamp first, and only then resolve and
// append. The read-before-resolve order is load-bearing, not an optimization —
// crash recovery re-drives dispatchCommittedStart, and policy may have
// hot-reloaded in between. Re-resolving would overwrite the truth of what the
// run actually started under with whatever is configured now.
//
// Recursion subruns are deliberately not stamped: they carry parent_run_id, so
// they join to their parent's stamp, and re-resolving configuration mid-tree
// would record a second, later truth for the same logical run.
//
// Best-effort throughout: resolution and append failures are logged and
// counted, never returned. A run that cannot be stamped still runs.
func (r *Reconciler) stampRunProvenance(
	ctx context.Context, subjectKind, runID, lane string, item *store.BacklogItem,
) {
	if r == nil || r.Store == nil || r.Store.Events == nil || runID == "" {
		return
	}
	if _, err := r.Store.Events.FirstBySubjectKind(
		ctx, subjectKind, runID, RunProvenanceEventKind,
	); err == nil {
		RunProvenanceStampsTotal.WithLabelValues(lane, "duplicate").Inc()
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		RunProvenanceStampsTotal.WithLabelValues(lane, "error").Inc()
		if r.Logger != nil {
			r.Logger.Warn("reconciler: read run provenance stamp failed",
				"run", runID, "error", err)
		}
		return
	}
	backlogID := ""
	if item != nil {
		backlogID = item.ID
	}
	payload := map[string]any{
		"run_id":     runID,
		"backlog_id": backlogID,
		"lane":       lane,
		// policy_checksum is the sha256 of the exact policy bytes the active
		// policy was parsed from (PolicyChecksum format), matching the
		// deployment's loom.flexinfer.ai/policy-checksum annotation. Empty
		// when no PolicyManager is wired — recorded as empty rather than
		// omitted so a consumer can tell "unknown" from "never stamped".
		"policy_checksum": r.Policy.CurrentChecksum(),
		// stage_models and prompt_hashes record START-TIME INTENT. A mid-run
		// swap (policy hot-reload, council fallback editor substituting for an
		// unreachable remote backend) is deliberately out of scope here and is
		// attributable from the per-dispatch pipeline.stage.agent_routed
		// events instead.
		"stage_models":  r.provenanceStageModels(item),
		"prompt_hashes": r.provenancePromptHashes(),
		"outcome":       "ok",
	}
	inserted, err := r.Store.Events.AppendOnceBySubjectKind(ctx, &store.Event{
		Actor:       RunProvenanceActor,
		Kind:        RunProvenanceEventKind,
		SubjectKind: subjectKind,
		SubjectID:   runID,
		Payload:     payload,
	})
	switch {
	case err != nil:
		RunProvenanceStampsTotal.WithLabelValues(lane, "error").Inc()
		if r.Logger != nil {
			r.Logger.Warn("reconciler: append run provenance stamp failed",
				"error", err, "run", runID)
		}
	case inserted:
		RunProvenanceStampsTotal.WithLabelValues(lane, "stamped").Inc()
	default:
		RunProvenanceStampsTotal.WithLabelValues(lane, "duplicate").Inc()
	}
}

// provenanceStageModels resolves the stage→model map for the stamp. The
// injected ProvenanceStageModels wins because the effective chain includes
// rungs policy alone cannot see (the LOOM_MILLS_SPAWN_AGENT /
// LOOM_MILLS_SPAWN_MODEL break-glass lives in the operator's spawn closure).
// The policy fallback covers deployments that wire no resolver; stages with no
// pin are omitted rather than recorded as empty, so the map is exactly the set
// of models the run was pinned to.
func (r *Reconciler) provenanceStageModels(item *store.BacklogItem) map[string]string {
	if r.ProvenanceStageModels != nil {
		return nonEmptyStrings(r.ProvenanceStageModels(item))
	}
	policy := r.Policy.Current()
	if policy == nil {
		return map[string]string{}
	}
	models := make(map[string]string, len(StageModelKeysValid))
	for stage := range StageModelKeysValid {
		if model := policy.ResolveAgentRoute(stage, item).Model; model != "" {
			models[stage] = model
		}
	}
	return models
}

// provenancePromptHashes returns the prompt-template digests for the stamp.
// The prompt builders live in the operator wiring (pkg/mills cannot reach
// them), so without an injected resolver the honest answer is an empty map:
// recording a guessed or partial hash set would be worse than recording none,
// because a wrong join key silently corrupts every per-prompt-version rollup.
func (r *Reconciler) provenancePromptHashes() map[string]string {
	if r.ProvenancePromptHashes == nil {
		return map[string]string{}
	}
	return nonEmptyStrings(r.ProvenancePromptHashes())
}

func nonEmptyStrings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	return out
}
