package mergequeue

import (
	"context"
	"errors"
	"fmt"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// StageGateway adapts the store DAO to the merge stage's pipeline.MergeQueue
// contract: idempotent enqueue with the policy's depth bound, and a status
// read the stage polls while it waits for the processor to land (or evict)
// its candidate.
type StageGateway struct {
	DAO *store.MergeQueueDAO
	// MaxDepth resolves the policy's lane depth bound per call so a policy
	// hot-reload applies to the next enqueue. Nil → unbounded.
	MaxDepth func() int
}

var _ pipeline.MergeQueue = (*StageGateway)(nil)

// Enqueue inserts (or re-finds) the run's candidate. A full lane maps to
// pipeline.ErrMergeQueueFull so the stage escalates with reason queue_full.
func (g *StageGateway) Enqueue(ctx context.Context, c pipeline.MergeQueueCandidate) error {
	if g == nil || g.DAO == nil {
		return errors.New("merge queue gateway: not configured")
	}
	maxDepth := 0
	if g.MaxDepth != nil {
		maxDepth = g.MaxDepth()
	}
	_, _, err := g.DAO.Enqueue(ctx, &store.MergeQueueEntry{
		PipelineRunID: c.RunID,
		BacklogID:     c.BacklogID,
		Project:       c.Project,
		MRIID:         c.MRIID,
		SourceBranch:  c.SourceBranch,
		TargetBranch:  c.TargetBranch,
		EnqueuedSHA:   c.SHA,
	}, maxDepth)
	if errors.Is(err, store.ErrMergeQueueFull) {
		return fmt.Errorf("%w: %v", pipeline.ErrMergeQueueFull, err)
	}
	return err
}

// Status projects the run's entry into the stage's wait contract.
func (g *StageGateway) Status(ctx context.Context, runID string) (pipeline.MergeQueueStatus, error) {
	if g == nil || g.DAO == nil {
		return pipeline.MergeQueueStatus{}, errors.New("merge queue gateway: not configured")
	}
	e, err := g.DAO.Get(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return pipeline.MergeQueueStatus{}, pipeline.ErrMergeQueueUnknownRun
	}
	if err != nil {
		return pipeline.MergeQueueStatus{}, err
	}
	st := pipeline.MergeQueueStatus{
		State:          string(e.State),
		EvictionReason: e.EvictionReason,
		MergedSHA:      e.MergedSHA,
	}
	if detail, ok := e.Detail["detail"].(string); ok {
		st.Detail = detail
	}
	switch e.State {
	case store.MergeQueueMerged:
		st.Terminal, st.Merged = true, true
	case store.MergeQueueEvicted:
		st.Terminal = true
	default:
		pos, perr := g.DAO.Position(ctx, runID)
		if perr == nil {
			st.Position = pos
		}
	}
	return st, nil
}
