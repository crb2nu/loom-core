package mergequeue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
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

var (
	ErrMergePermissionDenied      = errors.New("operator GitLab identity cannot merge this merge request")
	ErrMergePermissionUnavailable = errors.New("operator GitLab merge permission could not be verified")
)

// ExternalEnqueuer durably adapts fleet producers to the canonical serial queue.
type ExternalEnqueuer struct {
	Store    *store.Store
	Enabled  func() bool
	MaxDepth func() int
	// CheckPermission must use the same identity as the processor. Nil or an
	// uncertain lookup rejects admission before any durable provenance rows.
	CheckPermission func(context.Context, string, int64) (bool, error)
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
	if e.CheckPermission == nil {
		return ExternalResult{}, ErrMergePermissionUnavailable
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	allowed, err := e.CheckPermission(checkCtx, c.Project, c.MRIID)
	cancel()
	if err != nil {
		return ExternalResult{}, fmt.Errorf("%w: %w", ErrMergePermissionUnavailable, err)
	}
	if !allowed {
		return ExternalResult{}, ErrMergePermissionDenied
	}
	if prior := e.conflictedAtHead(ctx, c); prior != nil {
		// The queue already rebased this exact head and GitLab reported a
		// conflict; the verdict cannot change until the producer pushes a new
		// head. Refusing here (instead of enqueue → rebase → evict again) stops
		// the loop where a producer re-arms a conflicted MR after every target
		// move — live 2026-09-08 the stale canary MR !1789 was re-driven this
		// way after each merge to main.
		return ExternalResult{Outcome: "conflicted", State: prior.State, Entry: prior}, nil
	}

	sum := sha256.Sum256([]byte(c.Producer + "\x00" + c.IdempotencyKey))
	id := "external-mq-" + hex.EncodeToString(sum[:16])
	backlogID, runID := id, id
	if prior, _ := e.Store.MergeQueue.Get(ctx, runID); prior == nil {
		if active := e.activeForMR(ctx, c); active != nil {
			// The queue already holds this MR (its own pipeline run, or an
			// earlier external candidate under another key). A second row would
			// be driven as a separate candidate: it doubled recovery pipelines
			// before !1906 and, with speculation on, tried to stack the MR on
			// top of itself. Report the live entry instead of creating another.
			// (A replay of the SAME key still lands on the idempotent path
			// below and reports "duplicate".)
			return ExternalResult{Outcome: "active", State: active.State, Entry: active}, nil
		}
	}
	now := time.Now().UTC()
	// The compatibility rows satisfy the phase-1 queue's pipeline foreign key,
	// but are terminal so the reconciler never mistakes external provenance for
	// pipeline work it should execute.
	item := &store.BacklogItem{ID: backlogID, Title: "External merge candidate " + c.Project, State: store.BacklogRetired, Priority: store.P2, TargetProject: c.Project, CreatedBy: c.Producer, CreatedAt: now}
	if err := e.Store.Backlog.Put(ctx, item); err != nil && !errors.Is(err, store.ErrStaleWrite) {
		return ExternalResult{}, fmt.Errorf("record external provenance: %w", err)
	}
	iid := c.MRIID
	run := &store.PipelineRun{ID: runID, BacklogID: backlogID, Template: store.PipelineTemplateExternalMerge, State: store.PipelineDone, CurrentStage: "merge_queue", MRIID: &iid, StartedAt: now}
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

// conflictedAtHead returns the MR's latest settled queue row when that row is
// a rebase_conflict eviction of the same head the producer is offering now
// (matched against the SHA the queue enqueued and the one it last observed —
// a failed rebase leaves both at the conflicted head). Any other history —
// merged, evicted for another reason, evicted at a different head, or a store
// read failure — admits the candidate; the fence is deliberately narrow so a
// flaky read never blocks a legitimate enqueue.
func (e *ExternalEnqueuer) conflictedAtHead(ctx context.Context, c ExternalCandidate) *store.MergeQueueEntry {
	prior, err := e.Store.MergeQueue.LatestSettledByMR(ctx, c.Project, c.MRIID)
	if err != nil || prior == nil {
		return nil
	}
	if prior.State != store.MergeQueueEvicted || prior.EvictionReason != store.MergeQueueEvictRebaseConflict {
		return nil
	}
	if prior.EnqueuedSHA != c.ObservedSHA && prior.CurrentSHA != c.ObservedSHA {
		return nil
	}
	return prior
}

// activeForMR returns the queue's live (unsettled) entry for the candidate's
// MR, or nil. A store read failure admits the candidate: the idempotency key
// and the processor's own dedup still bound the damage, and a flaky read must
// never block a legitimate enqueue.
func (e *ExternalEnqueuer) activeForMR(ctx context.Context, c ExternalCandidate) *store.MergeQueueEntry {
	active, err := e.Store.MergeQueue.ListActive(ctx)
	if err != nil {
		return nil
	}
	for _, entry := range active {
		if entry != nil && entry.Project == c.Project && entry.MRIID == c.MRIID {
			return entry
		}
	}
	return nil
}

// externalBranchOwners finds escalated owners even when no run recorded the MR
// IID. The caller still applies AuthorizedProject to every returned run.
func (p *Processor) externalBranchOwners(ctx context.Context, e *store.MergeQueueEntry) ([]*store.PipelineRun, error) {
	if e.SourceBranch == "" {
		return nil, nil
	}
	items, err := p.Store.Backlog.ListByState(ctx, store.BacklogEscalated)
	if err != nil {
		return nil, err
	}
	var owners []*store.PipelineRun
	for _, item := range items {
		if item.State != store.BacklogEscalated {
			continue
		}
		matched := false
		branches := []string{pipeline.BranchContractFor(nil, item, pipeline.Stage{}, "").SourceBranch}
		for _, slice := range item.Slices {
			branches = append(branches, pipeline.BranchContractFor(nil, item, pipeline.Stage{}, slice.Name).SliceBranch)
		}
		for _, branch := range branches {
			if branch == e.SourceBranch {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		runs, err := p.Store.Pipeline.ListByBacklog(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		// Only the latest attempt can own a branch reused across retries.
		var latest *store.PipelineRun
		for _, run := range runs {
			if latest == nil || run.StartedAt.After(latest.StartedAt) {
				latest = run
			}
		}
		if latest == nil || latest.State != store.PipelineEscalated {
			continue
		}
		if latest.MRIID != nil && *latest.MRIID != e.MRIID {
			continue
		}
		if e.SettledAt != nil && e.SettledAt.Before(latest.StartedAt) {
			continue
		}
		owners = append(owners, latest)
	}
	return owners, nil
}
