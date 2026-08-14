package main

import (
	"context"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/clients"
)

// gitlabRegressionSource adapts the GitLab client's read-only list endpoints to
// the reconciler's regression-attribution interfaces. pkg/mills cannot import
// pkg/mills/clients (the dependency runs the other way), so the projection from
// the client's wire views into the mills records lives here, where both are
// visible — the same seam GhostSparkBranchesFor uses.
type gitlabRegressionSource struct {
	client *clients.GitLabClient
}

func (s gitlabRegressionSource) ListMergedMRs(ctx context.Context, since time.Time, limit int) ([]mills.MergedMRRecord, error) {
	items, err := s.client.ListMergedMergeRequests(ctx, limit, since)
	if err != nil {
		return nil, err
	}
	out := make([]mills.MergedMRRecord, 0, len(items))
	for _, item := range items {
		out = append(out, mills.MergedMRRecord{
			IID:       item.IID,
			Title:     item.Title,
			LandedSHA: item.LandedSHA(),
			MergedAt:  item.MergedAt,
		})
	}
	return out, nil
}

func (s gitlabRegressionSource) ListBranchCommits(ctx context.Context, ref string, since time.Time, limit int) ([]mills.BranchCommitRecord, error) {
	commits, err := s.client.ListBranchCommits(ctx, ref, since, limit)
	if err != nil {
		return nil, err
	}
	out := make([]mills.BranchCommitRecord, 0, len(commits))
	for _, commit := range commits {
		out = append(out, mills.BranchCommitRecord{
			SHA:       commit.ID,
			Title:     commit.Title,
			Message:   commit.Message,
			CreatedAt: commit.CreatedAt,
		})
	}
	return out, nil
}
