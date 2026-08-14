package mergequeue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// ExternalCandidate is a SHA-pinned merge intent submitted outside a Mills pipeline.
type ExternalCandidate struct {
	Producer, IdempotencyKey, Project, SourceBranch, TargetBranch, ObservedSHA string
	MRIID                                                                      int64
}

type ExternalResult struct {
	Outcome  string                 `json:"outcome"`
	State    store.MergeQueueState  `json:"state,omitempty"`
	Position int                    `json:"position,omitempty"`
	Entry    *store.MergeQueueEntry `json:"entry,omitempty"`
}

// ExternalEnqueuer durably adapts fleet producers to the canonical serial queue.
type ExternalEnqueuer struct {
	Store    *store.Store
	Enabled  func() bool
	MaxDepth func() int
}

func (e *ExternalEnqueuer) Enqueue(ctx context.Context, c ExternalCandidate) (ExternalResult, error) {
	if e == nil || e.Store == nil || e.Store.MergeQueue == nil || e.Store.Backlog == nil || e.Store.Pipeline == nil {
		return ExternalResult{}, errors.New("merge queue unavailable")
	}
	for name, value := range map[string]string{"producer": c.Producer, "idempotency_key": c.IdempotencyKey, "project": c.Project, "source_branch": c.SourceBranch, "target_branch": c.TargetBranch, "observed_sha": c.ObservedSHA} {
		if strings.TrimSpace(value) == "" {
			return ExternalResult{}, fmt.Errorf("%s is required", name)
		}
	}
	if c.MRIID <= 0 {
		return ExternalResult{}, errors.New("mr_iid must be positive")
	}
	if e.Enabled != nil && !e.Enabled() {
		return ExternalResult{Outcome: "disabled"}, nil
	}

	sum := sha256.Sum256([]byte(c.Producer + "\x00" + c.IdempotencyKey))
	id := "external-mq-" + hex.EncodeToString(sum[:16])
	backlogID, runID := id, id
	now := time.Now().UTC()
	// The compatibility rows satisfy the phase-1 queue's pipeline foreign key,
	// but are terminal so the reconciler never mistakes external provenance for
	// pipeline work it should execute.
	item := &store.BacklogItem{ID: backlogID, Title: "External merge candidate " + c.Project, State: store.BacklogRetired, Priority: store.P2, TargetProject: c.Project, CreatedBy: c.Producer, CreatedAt: now}
	if err := e.Store.Backlog.Put(ctx, item); err != nil && !errors.Is(err, store.ErrStaleWrite) {
		return ExternalResult{}, fmt.Errorf("record external provenance: %w", err)
	}
	iid := c.MRIID
	run := &store.PipelineRun{ID: runID, BacklogID: backlogID, Template: "external_merge", State: store.PipelineDone, CurrentStage: "merge_queue", MRIID: &iid, StartedAt: now}
	if err := e.Store.Pipeline.PutRun(ctx, run); err != nil && !errors.Is(err, store.ErrStaleWrite) {
		return ExternalResult{}, fmt.Errorf("record external run: %w", err)
	}
	depth := 0
	if e.MaxDepth != nil {
		depth = e.MaxDepth()
	}
	entry, created, err := e.Store.MergeQueue.Enqueue(ctx, &store.MergeQueueEntry{PipelineRunID: runID, BacklogID: backlogID, Project: c.Project, MRIID: c.MRIID, SourceBranch: c.SourceBranch, TargetBranch: c.TargetBranch, EnqueuedSHA: c.ObservedSHA, Detail: map[string]any{"producer": c.Producer, "idempotency_key": c.IdempotencyKey}}, depth)
	if errors.Is(err, store.ErrMergeQueueFull) {
		return ExternalResult{Outcome: "full"}, nil
	}
	if err != nil {
		return ExternalResult{}, err
	}
	if !created && (entry.Project != c.Project || entry.MRIID != c.MRIID || entry.SourceBranch != c.SourceBranch || entry.TargetBranch != c.TargetBranch || entry.EnqueuedSHA != c.ObservedSHA) {
		return ExternalResult{}, errors.New("idempotency key was already used for a different merge candidate")
	}
	position, _ := e.Store.MergeQueue.Position(ctx, runID)
	outcome := "duplicate"
	if created {
		outcome = "enqueued"
	}
	return ExternalResult{Outcome: outcome, State: entry.State, Position: position, Entry: entry}, nil
}
