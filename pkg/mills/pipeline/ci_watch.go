package pipeline

import (
	"context"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/policy"
)

// DefaultBranchCIRecorder is the persistence edge used by CI watchers after a
// terminal default-branch pipeline has been classified.
type DefaultBranchCIRecorder struct {
	Store  *store.Store
	Policy policy.MainRedExternalHoldPolicy
	// PolicyFn resolves the live Mills policy when configured.
	PolicyFn func() *mills.Policy
}

func (r DefaultBranchCIRecorder) Record(ctx context.Context, project, branch, pipelineID, classification string, at time.Time) (*store.MainRedExternalHold, error) {
	p := r.Policy
	if r.PolicyFn != nil {
		p = r.PolicyFn().MergeQueueMainRedExternalHold()
	}
	return r.Store.RecordDefaultBranchPipeline(ctx, project, branch, pipelineID, classification, at, p.Duration())
}
