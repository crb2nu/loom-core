package store

import (
	"context"
	"errors"
	"fmt"
)

// QuiescenceCounts is a point-in-time view of durable Mills work that must be
// idle before an operator restart is safe to inject. The counts come from one
// SQLite statement, so they share one read snapshot instead of racing across
// independent list requests.
type QuiescenceCounts struct {
	QueuedBacklog          int `json:"queued_backlog"`
	ActivePipelineRuns     int `json:"active_pipeline_runs"`
	ActiveWorkflowRuns     int `json:"active_workflow_runs"`
	ActiveSpinningRoomRuns int `json:"active_spinning_room_runs"`
	ActiveCouncilRuns      int `json:"active_council_runs"`
	ActiveCrossRepoRuns    int `json:"active_cross_repo_runs"`
	PendingDispatches      int `json:"pending_dispatches"`
}

// Quiescent reports whether every durable work class in the snapshot is idle.
func (c QuiescenceCounts) Quiescent() bool {
	return c.QueuedBacklog == 0 &&
		c.ActivePipelineRuns == 0 &&
		c.ActiveWorkflowRuns == 0 &&
		c.ActiveSpinningRoomRuns == 0 &&
		c.ActiveCouncilRuns == 0 &&
		c.ActiveCrossRepoRuns == 0 &&
		c.PendingDispatches == 0
}

// ReadQuiescence returns exact active-work counts from a single database read
// snapshot. It deliberately fails closed: callers never receive a partial set
// of zero values when any table cannot be read.
//
// Pipeline activity matches PipelineDAO.CountActive. Workflow activity also
// includes paused runs because a paused imperative run is resumable durable
// work. Pending dispatches are reported separately even though their pipeline
// run is normally active: the outbox is restart-recoverable but should be empty
// before deliberate fault injection. State-bearing tables use terminal
// deny-lists rather than active allowlists so a future or corrupt state fails
// closed as active until the safety contract explicitly classifies it.
func (s *Store) ReadQuiescence(ctx context.Context) (QuiescenceCounts, error) {
	var counts QuiescenceCounts
	if s == nil || s.db == nil {
		return counts, errors.New("mills quiescence: store unavailable")
	}

	err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM backlog_items WHERE state = 'queued'),
			(SELECT COUNT(*) FROM pipeline_runs
				WHERE state NOT IN ('done', 'escalated', 'paused')),
			(SELECT COUNT(*) FROM workflow_runs
				WHERE state NOT IN ('done', 'escalated', 'error', 'quarantined')),
			(SELECT COUNT(*) FROM spin_runs
				WHERE status NOT IN ('succeeded', 'failed', 'timeout')),
			(SELECT COUNT(*) FROM council_runs WHERE ended_at IS NULL),
			(SELECT COUNT(*) FROM cross_repo_runs
				WHERE state NOT IN ('merged', 'reverted', 'failed')),
			(SELECT COUNT(*) FROM pending_dispatches
				WHERE status NOT IN ('delivered', 'dead_letter'))
	`).Scan(
		&counts.QueuedBacklog,
		&counts.ActivePipelineRuns,
		&counts.ActiveWorkflowRuns,
		&counts.ActiveSpinningRoomRuns,
		&counts.ActiveCouncilRuns,
		&counts.ActiveCrossRepoRuns,
		&counts.PendingDispatches,
	)
	if err != nil {
		return QuiescenceCounts{}, fmt.Errorf("mills quiescence: read counts: %w", err)
	}
	return counts, nil
}
