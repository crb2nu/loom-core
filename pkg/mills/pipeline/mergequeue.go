package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ----- Serial merge queue (phase 1) -----
//
// When the operator wires a MergeQueue and the policy enables it, the merge
// stage hands its CI-authorized candidate to the queue instead of merging
// directly, then waits for the queue's verdict. The queue serializes merges
// per (project, target_branch): the head candidate is rebased onto the exact
// target tip when behind, its rebased head is re-proven by a fresh branch
// pipeline, and only then merged — so no queue-merged MR can die with
// has_conflicts, and merges stop landing in bursts that stack uncancelable
// main pipelines.
//
// The stage validates the run's full ci_watch authorization (including the
// #374 head-movement fence) ONCE, at enqueue. After that the queue owns the
// authorization chain: its own rebase advances the run's head-transition
// ledger, which would trip the enqueue-time fence on re-validation — that is
// why a resumed stage re-finds its entry FIRST and only validates when no
// entry exists yet.

// MergeQueueCandidate is the merge stage's enqueue payload: the exact
// authorization tuple ci_watch proved, plus run/backlog identity for
// durability and audit.
type MergeQueueCandidate struct {
	RunID        string
	BacklogID    string
	Project      string
	MRIID        int64
	SourceBranch string
	TargetBranch string
	// SHA is the ci_watch-authorized head (the ci_sha artifact).
	SHA string
}

// MergeQueueStatus is the stage's view of its candidate.
type MergeQueueStatus struct {
	State          string
	Terminal       bool
	Merged         bool
	MergedSHA      string
	EvictionReason string
	Detail         string
	// Position is the candidate's 1-based place in its lane (1 = head);
	// 0 when terminal or unknown.
	Position int
}

// MergeQueue is the serial-queue contract the merge stage drives. The
// mergequeue.StageGateway implements it over the canonical store.
type MergeQueue interface {
	// Enqueue registers the candidate (idempotent per run). ErrMergeQueueFull
	// when the lane is at the policy's max depth.
	Enqueue(ctx context.Context, c MergeQueueCandidate) error
	// Status reports the candidate's current queue state.
	// ErrMergeQueueUnknownRun when the run has no entry.
	Status(ctx context.Context, runID string) (MergeQueueStatus, error)
}

// ErrMergeQueueFull is the enqueue-time refusal for a lane at max depth. The
// stage escalates immediately (reason queue_full) instead of waiting.
var ErrMergeQueueFull = errors.New("pipeline: merge queue lane is full")

// ErrMergeQueueUnknownRun distinguishes "never enqueued" from a status read
// failure so a resumed stage knows whether to validate + enqueue.
var ErrMergeQueueUnknownRun = errors.New("pipeline: run has no merge queue entry")

// mergeQueueDefaultPollInterval is how often the waiting stage re-reads its
// candidate's status.
const mergeQueueDefaultPollInterval = 10 * time.Second

// mergeQueueDefaultWaitMinutes bounds the stage's wait for a queue verdict.
// The bound exists so a dead processor cannot hold a run open forever; it is
// sized for several 17–28 minute pipelines queued ahead. Override with
// LOOM_MILLS_MERGE_QUEUE_WAIT_MINUTES (stage env first, then process env).
const mergeQueueDefaultWaitMinutes = 180

func mergeQueueWaitBound(env map[string]string) time.Duration {
	raw := ""
	if env != nil {
		raw = strings.TrimSpace(env["LOOM_MILLS_MERGE_QUEUE_WAIT_MINUTES"])
	}
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("LOOM_MILLS_MERGE_QUEUE_WAIT_MINUTES"))
	}
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return mergeQueueDefaultWaitMinutes * time.Minute
}

// mergeQueueActive reports whether this stage attempt should route through
// the queue: a queue is wired AND the hot-reloaded policy enables it.
func (w *GitLabWorker) mergeQueueActive() bool {
	return w.MergeQueue != nil && w.MergeQueueEnabled != nil && w.MergeQueueEnabled()
}

// runMergeViaQueue is the merge stage's queue-mode body.
func (w *GitLabWorker) runMergeViaQueue(ctx context.Context, jc JobContext, mrIID int64) (StageOutput, error) {
	runID := ""
	if jc.Run != nil {
		runID = jc.Run.ID
	}
	if runID == "" {
		return StageOutput{}, fmt.Errorf("merge: queue mode requires a run id for mr %d", mrIID)
	}

	// Resume path: an existing entry (any state) means this run already
	// validated + enqueued. Re-validating here would trip the #374 fence once
	// the queue's own rebase advances the ledger — so re-find first.
	_, err := w.MergeQueue.Status(ctx, runID)
	switch {
	case errors.Is(err, ErrMergeQueueUnknownRun):
		mergeReq, aerr := ciMergeRequestFrom(jc, mrIID)
		if aerr != nil {
			return StageOutput{}, aerr
		}
		backlogID := ""
		if jc.Item != nil {
			backlogID = jc.Item.ID
		}
		if qerr := w.MergeQueue.Enqueue(ctx, MergeQueueCandidate{
			RunID:        runID,
			BacklogID:    backlogID,
			Project:      mergeReq.Project,
			MRIID:        mrIID,
			SourceBranch: mergeReq.SourceBranch,
			TargetBranch: mergeReq.TargetBranch,
			SHA:          mergeReq.ExpectedSHA,
		}); qerr != nil {
			if errors.Is(qerr, ErrMergeQueueFull) {
				return StageOutput{}, fmt.Errorf("merge: queue refused mr %d (queue_full): %w", mrIID, qerr)
			}
			return StageOutput{}, fmt.Errorf("merge: enqueue mr %d: %w", mrIID, qerr)
		}
	case err != nil:
		return StageOutput{}, fmt.Errorf("merge: queue status for mr %d: %w", mrIID, err)
	}

	return w.awaitMergeQueue(ctx, jc, mrIID, runID)
}

// awaitMergeQueue polls the candidate to a terminal verdict.
func (w *GitLabWorker) awaitMergeQueue(ctx context.Context, jc JobContext, mrIID int64, runID string) (StageOutput, error) {
	interval := w.MergeQueuePollInterval
	if interval <= 0 {
		interval = mergeQueueDefaultPollInterval
	}
	deadline := time.Now().Add(mergeQueueWaitBound(jc.Env))
	logPosition := -1

	for {
		st, err := w.MergeQueue.Status(ctx, runID)
		if err != nil && !errors.Is(err, ErrMergeQueueUnknownRun) {
			return StageOutput{}, fmt.Errorf("merge: queue status for mr %d: %w", mrIID, err)
		}
		if err == nil && st.Terminal {
			if st.Merged {
				return StageOutput{
					MergedSHA: st.MergedSHA,
					LogTail:   fmt.Sprintf("merge: landed via merge queue (sha %s)", st.MergedSHA),
					Artifacts: map[string]any{
						"merged_sha":     st.MergedSHA,
						"merged_project": jcProjectForLog(jc),
						"merged_via":     "merge_queue",
					},
				}, nil
			}
			return StageOutput{}, fmt.Errorf("merge: queue evicted mr %d (%s): %s", mrIID, st.EvictionReason, st.Detail)
		}
		if err == nil && st.Position != logPosition {
			logPosition = st.Position
			w.logger().Info("merge queue: waiting", "mr", mrIID, "run", runID, "state", st.State, "position", st.Position)
		}

		// A policy flip mid-wait halts the processor; fall back to the direct
		// merge so the run does not hang. The #374 fence re-validates — if the
		// queue already rebased, the stale authorization fails closed and the
		// run escalates for a clean re-gate.
		if !w.mergeQueueActive() {
			w.logger().Warn("merge queue: disabled mid-wait; falling back to direct merge", "mr", mrIID, "run", runID)
			return w.runMergeDirect(ctx, jc, mrIID)
		}
		if time.Now().After(deadline) {
			return StageOutput{}, fmt.Errorf("merge: queue verdict for mr %d not reached within %s (state %s); processor stalled or queue saturated", mrIID, mergeQueueWaitBound(jc.Env), st.State)
		}

		select {
		case <-ctx.Done():
			return StageOutput{}, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// jcProjectForLog best-efforts the project for the merged_project artifact on
// the queue path (the queue entry itself is authoritative).
func jcProjectForLog(jc JobContext) string {
	if ci, ok := jc.Prior["ci_watch"]; ok && ci.Artifacts != nil {
		if v, ok := ci.Artifacts["ci_project"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
