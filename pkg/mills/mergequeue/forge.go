// Package mergequeue is the serial merge queue for
// the Mills pipeline. GitLab CE has no merge trains, so parallel Mills
// branches go stale while long pipelines run; main moves and MRs die with
// has_conflicts at the merge PUT. The queue guarantees every MR is CI-tested
// on the exact target-branch tree it lands on: one candidate per
// (project, target_branch) lane is driven through rebase-if-behind →
// await-pipeline-on-rebased-tree → SHA-preconditioned merge, then the next
// head is promoted. On red CI or a rebase conflict the candidate is EVICTED
// with a distinct reason and the owning run falls through to the normal
// escalation path — the queue never retries internally.
//
// Durability: queue state lives in the canonical SQLite store
// (store.MergeQueueDAO, migration 024); every rebase the queue requests is
// also a durable mr_head_transitions ledger row (#374), so a restart resumes
// the head candidate exactly where the previous process died — re-observing,
// never re-mutating.
//
// Activation: the processor self-gates on a policy fence every tick
// (merge_queue.enabled, default OFF), mirroring the CanaryScheduler pattern —
// wiring it into the operator's errgroup activates nothing until the policy
// flips.
package mergequeue

import (
	"context"
	"errors"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// ErrRebaseInProgress is returned by Forge.RequestRebase when GitLab refuses
// because a rebase is already in flight (HTTP 409). The processor proceeds to
// observation — the in-flight rebase is exactly what it wants to settle.
var ErrRebaseInProgress = errors.New("mergequeue: rebase already in progress")

// MRSnapshot is the queue's view of a merge request's live state.
type MRSnapshot struct {
	// SHA is the current head of the source branch as the MR reports it.
	SHA string
	// State is GitLab's MR state: "opened" | "merged" | "closed" | "locked".
	State string
	// BaseSHA is diff_refs.base_sha — the target-branch commit the MR's diff
	// is currently computed against. BaseSHA == the target tip means the MR is
	// up to date and needs no rebase.
	BaseSHA string
	// MergedSHA is the merge/squash commit when State == "merged".
	MergedSHA string
	// RebaseInProgress reports GitLab's async rebase flag.
	RebaseInProgress bool
	// HasConflicts mirrors the MR's has_conflicts flag.
	HasConflicts bool
	// MergeError carries GitLab's merge_error text (rebase/merge failures).
	MergeError string
}

// PipelineStatus is one branch pipeline's identity + status.
type PipelineStatus struct {
	ID     int64
	SHA    string
	Ref    string
	Source string
	Status string
	WebURL string
	Found  bool
}

// PipelineTiming is GitLab's terminal timing data for one pipeline.
type PipelineTiming struct {
	Duration       *float64
	QueuedDuration *float64
}

// PipelineProof is a pipeline whose commit proves the candidate tree.
// Source is one of sha, tree, or speculative.
type PipelineProof struct {
	PipelineStatus
	Tree   string
	Source string
}

type SpeculativeHead struct {
	Ref      string
	SHA      string
	Pipeline PipelineStatus
	Adopted  bool
}

// SpeculativeForge is the optional mutation surface used when speculation is
// enabled. Keeping it separate preserves compatibility for non-GitLab forges.
type SpeculativeForge interface {
	PrepareSpeculativeHead(ctx context.Context, mrIID int64, ontoSHA, ref string) (SpeculativeHead, error)
	CancelQueuePipeline(ctx context.Context, pipelineID int64) error
	DeleteQueueRef(ctx context.Context, ref string) error
}

// Forge is the GitLab surface the processor drives. *clients.GitLabClient
// implements it directly (pkg/mills/clients/gitlab_mergequeue.go); tests
// inject a fake.
type Forge interface {
	// MRSnapshot reads the MR's live head, state, and diff base.
	MRSnapshot(ctx context.Context, mrIID int64) (MRSnapshot, error)
	// BranchTip returns the current tip SHA of a branch.
	BranchTip(ctx context.Context, branch string) (string, error)
	// RequestRebase asks GitLab to rebase the MR onto its target branch. The
	// call is async on GitLab's side; ObserveHead settles the outcome.
	RequestRebase(ctx context.Context, mrIID int64) error
	// ReadHeadCursors snapshots observation cursors BEFORE a rebase request.
	ReadHeadCursors(ctx context.Context, req pipeline.HeadCursorRequest) (pipeline.HeadCursors, error)
	// ObserveHead settles and classifies one head movement (#374).
	ObserveHead(ctx context.Context, req pipeline.HeadObservationRequest) (pipeline.HeadObservation, error)
	// BranchPipelineStatus resolves the newest branch pipeline for (sha, ref),
	// preferring push pipelines and falling back to api-created ones.
	BranchPipelineStatus(ctx context.Context, sha, ref string) (PipelineStatus, error)
	// MRPipelineStatus resolves the newest merge-request pipeline (source
	// merge_request_event) attached to the MR whose built SHA matches sha.
	// Repos that gate MRs on detached MR pipelines instead of branch
	// pipelines (flexinfer) prove external candidates here. Not-found is not
	// an error.
	MRPipelineStatus(ctx context.Context, mrIID int64, sha string) (PipelineStatus, error)
	FindActivePipeline(ctx context.Context, ref, sha string) (PipelineStatus, error)
	// PipelineProof resolves a project pipeline for headSHA. Implementations
	// must prefer an exact SHA and may otherwise return only a pipeline whose
	// commit tree equals headSHA's tree. Missing tree data fails closed.
	PipelineProof(ctx context.Context, headSHA string) (PipelineProof, error)
	// CreateQueuePipeline creates a fresh branch pipeline on ref (the queue's
	// bounded recovery when the rebase push minted none).
	CreateQueuePipeline(ctx context.Context, ref string, mrIID int64) (PipelineStatus, error)
	// PipelineTiming retrieves terminal wall and runner-queue durations.
	PipelineTiming(ctx context.Context, pipelineID int64) (PipelineTiming, error)
	// Merge performs the SHA-preconditioned merge with the full bounded
	// 405/409/422 recovery machinery.
	Merge(ctx context.Context, req pipeline.MergeRequestArgs) (pipeline.MergeResponse, error)
}
