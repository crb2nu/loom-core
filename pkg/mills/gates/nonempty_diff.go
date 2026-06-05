package gates

import "context"

// NonEmptyDiff fails when the implement stage produced no observable change
// at all — no files touched and an empty diff. It is the deterministic guard
// against the "empty MR" false-positive that blocked every historical Mills
// canary: when the spawned agent does zero work (e.g. codex exiting with
// turn_count=0 because its CLI is absent on the substrate, an auth failure, or
// an undelivered prompt), the implement StageOutput is empty, yet the other
// post-implement gates wave it through — scope and path_policy early-return
// pass on len(FilesChanged)==0, diff_size/secret_scan pass on an empty diff,
// and the LLM rubric scores an empty diff 1.0. The run then reaches the mr
// stage and opens a 0-commit MR (head_sha=null), which is exactly what killed
// the .loom/44 autonomy round (!518/!520/!522) and the .loom/126 Slice A2
// kill-test (!598).
//
// Placed first in post_implement_gate so it short-circuits before the size and
// scope checks. With the stage's RetryFrom: "implement", a fail retries the
// implement stage and, on repeated empty output, escalates to a human instead
// of producing a junk MR.
//
// The gate keys on "no change at all" (both FilesChanged and DiffPatch empty)
// rather than requiring FilesChanged>=1 so it never fires on a stage that
// legitimately reports a diff without parsed file paths, and so the NoOp smoke
// dispatcher can satisfy it with a placeholder diff while keeping FilesChanged
// empty (which the scope gate needs for no-slice smoke items).
type NonEmptyDiff struct{}

// Name identifies the gate in persistence + logs.
func (g *NonEmptyDiff) Name() string { return "nonempty_diff" }

// Evaluate passes when the implement stage produced any observable change and
// fails when it produced none.
func (g *NonEmptyDiff) Evaluate(_ context.Context, in StageInput) (Outcome, error) {
	if len(in.FilesChanged) == 0 && len(in.DiffPatch) == 0 {
		return fail("implement stage produced no changes (0 files changed, empty diff); " +
			"the agent did no work — escalating instead of opening an empty MR"), nil
	}
	return pass(), nil
}
