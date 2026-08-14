package main

// workflow_merger.go adapts the operator's GitLab client to the S6-full
// merging canary's single merge effect (workflow.CanaryMergeExecutor). The
// whole path is lookup-first idempotent so a workflow replay after any crash
// converges on the same single merge:
//
//	MergedMRForBranch  — replay fast-path: the merge already landed
//	EnsureBranch       — adopt an existing canary branch
//	EnsureFileOnBranch — adopt an existing canary commit
//	CreateMR           — adopt-first (open-MR lookup + 409 adoption)
//	Merge              — merged-state reconciliation makes the PUT idempotent
//
// PASS-3 (no double-merge) rests on the composition: every step either finds
// the prior attempt's result or creates it exactly once under GitLab's own
// uniqueness (branch name, file path, open-MR-per-branch, merged-state).

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/workflow"
)

const canaryMergeTargetBranch = "main"

type canaryMerger struct {
	gl      *clients.GitLabClient
	project string
	logger  *slog.Logger
}

func newCanaryMerger(gl *clients.GitLabClient, project string, logger *slog.Logger) *canaryMerger {
	if logger == nil {
		logger = slog.Default()
	}
	return &canaryMerger{gl: gl, project: project, logger: logger}
}

// CanaryMergeBranch derives the deterministic merge branch for a run. The
// merge branch is distinct from the agent worktree branch (mills-wf/<id>):
// the canary's merge content is operator-authored, so the proof does not
// depend on what the crashed-and-resumed agent turn pushed.
func CanaryMergeBranch(runID string) string { return "mills-wf-merge/" + runID }

func (m *canaryMerger) MergeCanary(ctx context.Context, runID string) (workflow.CanaryMergeOutcome, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return workflow.CanaryMergeOutcome{}, fmt.Errorf("canary merge: run id required")
	}
	branch := CanaryMergeBranch(runID)

	// Replay fast-path: a prior interrupted attempt already merged.
	if iid, mergedAt, ok, err := m.gl.MergedMRForBranch(ctx, branch); err != nil {
		return workflow.CanaryMergeOutcome{}, fmt.Errorf("canary merge: merged-MR lookup for %q: %w", branch, err)
	} else if ok {
		m.logger.Info("canary merge: adopting already-merged MR",
			"run_id", runID, "mr_iid", iid, "merged_at", mergedAt)
		return workflow.CanaryMergeOutcome{MRIID: iid, SourceBranch: branch, AlreadyMerged: true}, nil
	}

	if _, err := m.gl.EnsureBranch(ctx, branch, canaryMergeTargetBranch); err != nil {
		return workflow.CanaryMergeOutcome{}, fmt.Errorf("canary merge: ensure branch: %w", err)
	}
	filePath := ".mills-canary/" + runID + ".md"
	content := fmt.Sprintf("Mills S6-full merging canary proof artifact for workflow run %s.\n"+
		"This file exists to give the canary's exactly-once merge a real, inert diff.\n", runID)
	message := fmt.Sprintf("chore(mills): S6-full merging canary %s\n\n[skip-docs-check]", runID)
	head, err := m.gl.EnsureFileOnBranch(ctx, branch, filePath, content, message)
	if err != nil {
		return workflow.CanaryMergeOutcome{}, fmt.Errorf("canary merge: ensure commit: %w", err)
	}

	mr, err := m.gl.CreateMR(ctx, pipeline.CreateMRRequest{
		SourceBranch: branch,
		TargetBranch: canaryMergeTargetBranch,
		Title:        fmt.Sprintf("chore(mills): S6-full merging canary %s", runID),
		Description: fmt.Sprintf("Autonomous S6-full merging canary for workflow run `%s`. "+
			"Asserts PASS-3 (no double-merge across crashes) with a single inert proof file. "+
			"Merged exactly once by the operator's idempotent merge effect.\n\n[skip-docs-check]", runID),
	})
	if err != nil {
		return workflow.CanaryMergeOutcome{}, fmt.Errorf("canary merge: ensure MR: %w", err)
	}

	resp, err := m.gl.Merge(ctx, pipeline.MergeRequestArgs{
		MRIID:        mr.MRIID,
		Project:      m.project,
		SourceBranch: branch,
		TargetBranch: canaryMergeTargetBranch,
		ExpectedSHA:  head,
	})
	if err != nil {
		return workflow.CanaryMergeOutcome{}, fmt.Errorf("canary merge: merge MR %d: %w", mr.MRIID, err)
	}
	m.logger.Info("canary merge: merged",
		"run_id", runID, "mr_iid", mr.MRIID, "merged_sha", resp.MergedSHA)
	return workflow.CanaryMergeOutcome{
		MRIID:          mr.MRIID,
		MergeCommitSHA: resp.MergedSHA,
		SourceBranch:   branch,
	}, nil
}
