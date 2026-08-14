package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/store"
)

const (
	legacyMRProjectBackfillBatchSize = 128
	legacyMRProjectBackfillTimeout   = 30 * time.Second
)

type legacyMRProjectBackfillStore interface {
	ListLegacyMRProjectBackfillCandidates(ctx context.Context, limit int) ([]*store.PipelineRun, error)
	AdvanceLegacyMRProjectBackfillCursor(ctx context.Context, run *store.PipelineRun) error
	ListStages(ctx context.Context, pipelineRunID string) ([]*store.StageResult, error)
	PatchMRProjectArtifact(ctx context.Context, stageResultID int64, expectedRunID, expectedMRURL string, expectedMRIID int64, project string) (bool, error)
}

type legacyMRProjectVerifier interface {
	VerifyMR(ctx context.Context, project string, mrIID int64) error
}

type gitLabLegacyMRProjectVerifier struct {
	client *clients.GitLabClient
}

func (v gitLabLegacyMRProjectVerifier) VerifyMR(ctx context.Context, project string, mrIID int64) error {
	if v.client == nil {
		return errors.New("legacy MR project verifier: GitLab client is nil")
	}
	return v.client.ForProject(project).VerifyMR(ctx, mrIID)
}

type legacyMRProjectBackfillResult struct {
	Scanned           int
	Patched           int
	AlreadyPresent    int
	Rejected          int
	VerificationError int
	PatchError        int
}

// backfillLegacyMRProjects upgrades only terminal legacy MR provenance. The
// candidate DAO restricts this to the latest escalated run of an escalated
// backlog item with no durable project key. Each persisted URL is then bound to
// the configured GitLab authority and run IID, confirmed with a project-scoped
// GET, and patched through an artifact-only transactional CAS.
func backfillLegacyMRProjects(
	ctx context.Context,
	db legacyMRProjectBackfillStore,
	verifier legacyMRProjectVerifier,
	gitLabBaseURL string,
	logger *slog.Logger,
) (legacyMRProjectBackfillResult, error) {
	result := legacyMRProjectBackfillResult{}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if db == nil || verifier == nil {
		return result, errors.New("legacy MR project backfill: store and verifier are required")
	}
	expectedAuthority := clients.CanonicalGitLabAuthority(gitLabBaseURL)
	if expectedAuthority == "" {
		return result, errors.New("legacy MR project backfill: valid GitLab base URL required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	runs, err := db.ListLegacyMRProjectBackfillCandidates(ctx, legacyMRProjectBackfillBatchSize)
	if err != nil {
		return result, fmt.Errorf("legacy MR project backfill candidates: %w", err)
	}
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if run == nil {
			continue
		}
		result.Scanned++
		// Advance before any external verification. The durable round-robin
		// cursor guarantees a permanently rejected or slow legacy row cannot
		// monopolize the bounded 128-row startup page across restarts.
		if err := db.AdvanceLegacyMRProjectBackfillCursor(ctx, run); err != nil {
			return result, fmt.Errorf("legacy MR project backfill advance cursor after run %s: %w", run.ID, err)
		}
		stages, err := db.ListStages(ctx, run.ID)
		if err != nil {
			result.Rejected++
			logger.Warn("legacy MR project backfill stage read rejected", "run", run.ID, "error", err)
			continue
		}
		stage, project, mrURL, err := legacyMRProjectPatchTarget(run, stages, expectedAuthority)
		if err != nil {
			result.Rejected++
			logger.Warn("legacy MR project backfill provenance rejected", "run", run.ID, "error", err)
			continue
		}
		if err := verifier.VerifyMR(ctx, project, *run.MRIID); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			result.VerificationError++
			logger.Warn("legacy MR project backfill GitLab verification failed",
				"run", run.ID, "project", project, "mr_iid", *run.MRIID, "error", err)
			continue
		}
		applied, err := db.PatchMRProjectArtifact(ctx, stage.ID, run.ID, mrURL, *run.MRIID, project)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			result.PatchError++
			logger.Warn("legacy MR project backfill artifact patch failed",
				"run", run.ID, "stage_result_id", stage.ID, "error", err)
			continue
		}
		if applied {
			result.Patched++
		} else {
			result.AlreadyPresent++
		}
	}
	return result, nil
}

func legacyMRProjectPatchTarget(run *store.PipelineRun, stages []*store.StageResult, expectedAuthority string) (*store.StageResult, string, string, error) {
	if run == nil || run.MRIID == nil || *run.MRIID <= 0 {
		return nil, "", "", errors.New("legacy MR project backfill: run has no positive MR IID")
	}
	var target *store.StageResult
	targetURL := ""
	targetProject := ""
	project := ""
	for _, stage := range stages {
		if stage == nil || stage.Stage != "mr" || stage.Outcome == nil || *stage.Outcome != store.StageOutcomeSuccess {
			continue
		}
		raw, exists := stage.Artifacts["mr_url"]
		if !exists {
			continue
		}
		mrURL, ok := raw.(string)
		mrURL = strings.TrimSpace(mrURL)
		if !ok || mrURL == "" {
			return nil, "", "", fmt.Errorf("run %s successful MR artifact has invalid mr_url", run.ID)
		}
		ref, ok := clients.ParseGitLabMRReference(mrURL)
		if !ok || !ref.ProjectBound || ref.Authority == "" {
			// Never reflect the untrusted URL: rejected legacy artifacts can carry
			// userinfo or query credentials, and this error is written to startup
			// logs on every backfill pass.
			return nil, "", "", fmt.Errorf("run %s stage result %d has an ambiguous MR URL", run.ID, stage.ID)
		}
		if ref.Authority != expectedAuthority {
			return nil, "", "", fmt.Errorf("run %s MR authority %q does not match configured %q", run.ID, ref.Authority, expectedAuthority)
		}
		if ref.IID != *run.MRIID {
			return nil, "", "", fmt.Errorf("run %s MR URL IID %d does not match run IID %d", run.ID, ref.IID, *run.MRIID)
		}
		stageIID, ok := legacyMRArtifactIID(stage.Artifacts["mr_iid"])
		if !ok || stageIID != ref.IID {
			return nil, "", "", fmt.Errorf("run %s MR artifact IID does not match URL/run IID %d", run.ID, ref.IID)
		}
		if project != "" && !clients.SameGitLabProject(project, ref.Project) {
			return nil, "", "", fmt.Errorf("run %s has conflicting MR URL projects %q and %q", run.ID, project, ref.Project)
		}
		if project == "" {
			project = ref.Project
		}
		if target == nil || stage.StartedAt.After(target.StartedAt) ||
			(stage.StartedAt.Equal(target.StartedAt) && stage.Attempt > target.Attempt) {
			target = stage
			targetURL = mrURL
			targetProject = ref.Project
		}
	}
	if target == nil || project == "" {
		return nil, "", "", fmt.Errorf("run %s has no successful canonical MR URL", run.ID)
	}
	return target, targetProject, targetURL, nil
}

func legacyMRArtifactIID(value any) (int64, bool) {
	var iid int64
	switch value := value.(type) {
	case int:
		iid = int64(value)
	case int32:
		iid = int64(value)
	case int64:
		iid = value
	case uint:
		if uint64(value) > math.MaxInt64 {
			return 0, false
		}
		iid = int64(value)
	case uint32:
		iid = int64(value)
	case uint64:
		if value > math.MaxInt64 {
			return 0, false
		}
		iid = int64(value)
	case float64:
		if value >= float64(uint64(1)<<63) || value != math.Trunc(value) {
			return 0, false
		}
		iid = int64(value)
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, false
		}
		iid = parsed
	default:
		return 0, false
	}
	return iid, iid > 0
}
