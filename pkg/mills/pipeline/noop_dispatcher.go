package pipeline

import (
	"context"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// NoOpDispatcher is the placeholder dispatcher the operator wires until
// the spawn / weaver / devbox / gitlab clients are connected. It returns
// deterministic StageOutputs that satisfy the deterministic gates and
// keep the pipeline state machine moving.
//
// It is intended for two scenarios:
//  1. End-to-end smoke tests of the reconciler + runner + attributor chain
//     where we don't want the test to require real network clients.
//  2. Operator boot during the bring-up window before slice X.Y wires
//     real worker clients — the pipeline will run to merged but produce
//     no actual code or MR. The operator should log a clear warning when
//     this dispatcher is in use.
//
// NoOpDispatcher is NOT a production-shippable mode for the autonomous
// loop. Wire real clients before flipping policy.enabled to true.
type NoOpDispatcher struct {
	// MRIID is the merge-request iid the mr stage returns. Defaults to
	// a sentinel (1) so tests can assert it bubbled up; override per
	// test if multiple runs need distinct values.
	MRIID int64
	// Cost is added per stage so total cost > 0 in event logs / HUD.
	Cost float64
}

// NoOpWorker is the Worker-compatible form of NoOpDispatcher. It is useful as
// Dispatcher fallback while preserving the deterministic placeholder outputs
// used during operator bring-up and reconciler smoke tests.
type NoOpWorker struct {
	MRIID int64
	Cost  float64
}

// Run satisfies Worker.
func (n *NoOpWorker) Run(_ context.Context, jc JobContext) (StageOutput, error) {
	return noOpStageOutput(n.MRIID, n.Cost, jc.Stage), nil
}

// Dispatch satisfies WorkerDispatcher.
func (n *NoOpDispatcher) Dispatch(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, _ map[string]StageOutput) (StageOutput, error) {
	return noOpStageOutput(n.MRIID, n.Cost, stage), nil
}

func noOpStageOutput(mrIID int64, configuredCost float64, stage Stage) StageOutput {
	cost := configuredCost
	if cost == 0 {
		cost = 0.001
	}
	out := StageOutput{CostUSD: cost}
	switch stage.ID {
	case "implement":
		// Emit a non-empty placeholder diff so the deterministic gates this
		// dispatcher is documented to satisfy include the nonempty_diff guard.
		// FilesChanged is intentionally left empty so the scope/path_policy
		// gates still short-circuit to pass on no-slice smoke items; the
		// non-empty DiffPatch alone is what satisfies nonempty_diff.
		out.FilesChanged = []string{}
		out.LinesAdded = 1
		out.LinesRemoved = 0
		out.DiffPatch = []byte("diff --git a/NOOP_STUB b/NOOP_STUB\n" +
			"--- a/NOOP_STUB\n+++ b/NOOP_STUB\n@@ -0,0 +1 @@\n" +
			"+noop dispatcher placeholder change\n")
		out.CommitMessages = []string{"feat(stub): noop dispatcher placeholder"}
		out.Artifacts = map[string]any{"stub": true}
	case "mr":
		mr := mrIID
		if mr == 0 {
			mr = 1
		}
		out.MRIID = mr
		out.Artifacts = map[string]any{"mr_iid": mr}
	case "merge":
		out.MergedSHA = "noopdeadbeef"
		out.Artifacts = map[string]any{"merged_sha": "noopdeadbeef"}
	}
	return out
}
