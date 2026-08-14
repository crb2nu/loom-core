package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

type fakeLegacyMRProjectBackfillStore struct {
	runs         []*store.PipelineRun
	stages       map[string][]*store.StageResult
	listErr      error
	advanceErr   error
	patchErr     error
	patchApplied bool
	patchCalls   []legacyMRProjectPatchCall
	advanceCalls []string
}

type legacyMRProjectPatchCall struct {
	stageResultID int64
	expectedRunID string
	expectedMRURL string
	expectedMRIID int64
	project       string
}

func (f *fakeLegacyMRProjectBackfillStore) ListLegacyMRProjectBackfillCandidates(_ context.Context, limit int) ([]*store.PipelineRun, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.runs) > limit {
		return f.runs[:limit], nil
	}
	return f.runs, nil
}

func (f *fakeLegacyMRProjectBackfillStore) AdvanceLegacyMRProjectBackfillCursor(_ context.Context, run *store.PipelineRun) error {
	if run != nil {
		f.advanceCalls = append(f.advanceCalls, run.ID)
	}
	return f.advanceErr
}

func (f *fakeLegacyMRProjectBackfillStore) ListStages(_ context.Context, runID string) ([]*store.StageResult, error) {
	return f.stages[runID], nil
}

func (f *fakeLegacyMRProjectBackfillStore) PatchMRProjectArtifact(_ context.Context, stageResultID int64, expectedRunID, expectedMRURL string, expectedMRIID int64, project string) (bool, error) {
	f.patchCalls = append(f.patchCalls, legacyMRProjectPatchCall{
		stageResultID: stageResultID,
		expectedRunID: expectedRunID,
		expectedMRURL: expectedMRURL,
		expectedMRIID: expectedMRIID,
		project:       project,
	})
	return f.patchApplied, f.patchErr
}

type fakeLegacyMRProjectVerifier struct {
	err   error
	calls []legacyMRProjectVerifyCall
}

type legacyMRProjectVerifyCall struct {
	project string
	mrIID   int64
}

func (f *fakeLegacyMRProjectVerifier) VerifyMR(_ context.Context, project string, mrIID int64) error {
	f.calls = append(f.calls, legacyMRProjectVerifyCall{project: project, mrIID: mrIID})
	return f.err
}

func legacyBackfillFixture(mrURL string) (*store.PipelineRun, *store.StageResult) {
	iid := int64(847)
	outcome := store.StageOutcomeSuccess
	started := time.Date(2026, 6, 30, 0, 4, 18, 0, time.UTC)
	return &store.PipelineRun{ID: "PIPE-LEGACY", MRIID: &iid, State: store.PipelineEscalated}, &store.StageResult{
		ID: 71, PipelineRunID: "PIPE-LEGACY", Stage: "mr", Attempt: 1,
		StartedAt: started, Outcome: &outcome,
		Artifacts: map[string]any{"mr_iid": float64(iid), "mr_url": mrURL, "branch": "feat/legacy"},
	}
}

func TestLegacyMRProjectBackfillVerifiesThenPatches(t *testing.T) {
	run, stage := legacyBackfillFixture("https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/847")
	db := &fakeLegacyMRProjectBackfillStore{
		runs: []*store.PipelineRun{run}, stages: map[string][]*store.StageResult{run.ID: {stage}}, patchApplied: true,
	}
	verifier := &fakeLegacyMRProjectVerifier{}
	got, err := backfillLegacyMRProjects(context.Background(), db, verifier, "https://gitlab.flexinfer.ai/api/v4", nil)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got.Scanned != 1 || got.Patched != 1 || got.Rejected != 0 || got.VerificationError != 0 || got.PatchError != 0 {
		t.Fatalf("result = %+v", got)
	}
	if len(verifier.calls) != 1 || verifier.calls[0] != (legacyMRProjectVerifyCall{project: "services/loom-core", mrIID: 847}) {
		t.Fatalf("verification calls = %+v", verifier.calls)
	}
	if len(db.advanceCalls) != 1 || db.advanceCalls[0] != run.ID {
		t.Fatalf("cursor advance calls = %v, want [%s]", db.advanceCalls, run.ID)
	}
	if len(db.patchCalls) != 1 || db.patchCalls[0] != (legacyMRProjectPatchCall{
		stageResultID: 71,
		expectedRunID: "PIPE-LEGACY",
		expectedMRURL: "https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/847",
		expectedMRIID: 847,
		project:       "services/loom-core",
	}) {
		t.Fatalf("patch calls = %+v", db.patchCalls)
	}
}

func TestLegacyMRProjectBackfillRejectsAmbiguousProvenanceBeforeGET(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store.PipelineRun, *store.StageResult) []*store.StageResult
	}{
		{
			name: "foreign host",
			mutate: func(_ *store.PipelineRun, stage *store.StageResult) []*store.StageResult {
				stage.Artifacts["mr_url"] = "https://other.example/services/loom-core/-/merge_requests/847"
				return []*store.StageResult{stage}
			},
		},
		{
			name: "IID mismatch",
			mutate: func(_ *store.PipelineRun, stage *store.StageResult) []*store.StageResult {
				stage.Artifacts["mr_url"] = "https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/848"
				return []*store.StageResult{stage}
			},
		},
		{
			name: "stage artifact IID mismatch",
			mutate: func(_ *store.PipelineRun, stage *store.StageResult) []*store.StageResult {
				stage.Artifacts["mr_iid"] = float64(848)
				return []*store.StageResult{stage}
			},
		},
		{
			name: "credentials",
			mutate: func(_ *store.PipelineRun, stage *store.StageResult) []*store.StageResult {
				stage.Artifacts["mr_url"] = "https://user@gitlab.flexinfer.ai/services/loom-core/-/merge_requests/847"
				return []*store.StageResult{stage}
			},
		},
		{
			name: "conflicting projects",
			mutate: func(_ *store.PipelineRun, stage *store.StageResult) []*store.StageResult {
				other := *stage
				other.ID = 72
				other.Attempt = 2
				other.StartedAt = stage.StartedAt.Add(time.Minute)
				other.Artifacts = map[string]any{"mr_iid": float64(847), "mr_url": "https://gitlab.flexinfer.ai/services/flexdeck/-/merge_requests/847"}
				return []*store.StageResult{stage, &other}
			},
		},
		{
			name: "project case mismatch",
			mutate: func(_ *store.PipelineRun, stage *store.StageResult) []*store.StageResult {
				other := *stage
				other.ID = 72
				other.Attempt = 2
				other.StartedAt = stage.StartedAt.Add(time.Minute)
				other.Artifacts = map[string]any{"mr_iid": float64(847), "mr_url": "https://gitlab.flexinfer.ai/Services/loom-core/-/merge_requests/847"}
				return []*store.StageResult{stage, &other}
			},
		},
		{
			name: "project git suffix mismatch",
			mutate: func(_ *store.PipelineRun, stage *store.StageResult) []*store.StageResult {
				other := *stage
				other.ID = 72
				other.Attempt = 2
				other.StartedAt = stage.StartedAt.Add(time.Minute)
				other.Artifacts = map[string]any{"mr_iid": float64(847), "mr_url": "https://gitlab.flexinfer.ai/services/loom-core.git/-/merge_requests/847"}
				return []*store.StageResult{stage, &other}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, stage := legacyBackfillFixture("https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/847")
			db := &fakeLegacyMRProjectBackfillStore{
				runs: []*store.PipelineRun{run}, stages: map[string][]*store.StageResult{run.ID: tt.mutate(run, stage)}, patchApplied: true,
			}
			verifier := &fakeLegacyMRProjectVerifier{}
			got, err := backfillLegacyMRProjects(context.Background(), db, verifier, "https://gitlab.flexinfer.ai/api/v4", nil)
			if err != nil {
				t.Fatalf("backfill: %v", err)
			}
			if got.Rejected != 1 || len(verifier.calls) != 0 || len(db.patchCalls) != 0 {
				t.Fatalf("result=%+v verification=%+v patches=%+v", got, verifier.calls, db.patchCalls)
			}
		})
	}
}

func TestLegacyMRProjectBackfillRequiresSuccessfulScopedGET(t *testing.T) {
	run, stage := legacyBackfillFixture("https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/847")
	db := &fakeLegacyMRProjectBackfillStore{
		runs: []*store.PipelineRun{run}, stages: map[string][]*store.StageResult{run.ID: {stage}}, patchApplied: true,
	}
	verifier := &fakeLegacyMRProjectVerifier{err: errors.New("gitlab: status 404")}
	got, err := backfillLegacyMRProjects(context.Background(), db, verifier, "https://gitlab.flexinfer.ai/api/v4", nil)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got.VerificationError != 1 || len(db.patchCalls) != 0 {
		t.Fatalf("result=%+v patches=%+v", got, db.patchCalls)
	}
}

func TestLegacyMRProjectBackfillNeverLogsRejectedURLCredentials(t *testing.T) {
	const secret = "startup-log-secret"
	run, stage := legacyBackfillFixture("https://user:" + secret + "@gitlab.flexinfer.ai/services/loom-core/-/merge_requests/847?private_token=" + secret)
	db := &fakeLegacyMRProjectBackfillStore{
		runs: []*store.PipelineRun{run}, stages: map[string][]*store.StageResult{run.ID: {stage}}, patchApplied: true,
	}
	verifier := &fakeLegacyMRProjectVerifier{}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	got, err := backfillLegacyMRProjects(context.Background(), db, verifier, "https://gitlab.flexinfer.ai/api/v4", logger)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got.Rejected != 1 || len(verifier.calls) != 0 || len(db.patchCalls) != 0 {
		t.Fatalf("result=%+v verification=%+v patches=%+v", got, verifier.calls, db.patchCalls)
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "private_token") {
		t.Fatalf("rejected URL credentials reached logs: %s", logs.String())
	}
}

func TestLegacyMRProjectBackfillPatchIsIdempotent(t *testing.T) {
	run, stage := legacyBackfillFixture("https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/847")
	db := &fakeLegacyMRProjectBackfillStore{
		runs: []*store.PipelineRun{run}, stages: map[string][]*store.StageResult{run.ID: {stage}}, patchApplied: false,
	}
	got, err := backfillLegacyMRProjects(context.Background(), db, &fakeLegacyMRProjectVerifier{}, "https://gitlab.flexinfer.ai/api/v4", nil)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got.AlreadyPresent != 1 || got.Patched != 0 {
		t.Fatalf("result = %+v", got)
	}
}

func TestLegacyMRProjectBackfillHonorsCandidateFailureAndContext(t *testing.T) {
	_, err := backfillLegacyMRProjects(context.Background(), &fakeLegacyMRProjectBackfillStore{listErr: errors.New("db unavailable")}, &fakeLegacyMRProjectVerifier{}, "https://gitlab.flexinfer.ai/api/v4", nil)
	if err == nil {
		t.Fatal("candidate query failure returned nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = backfillLegacyMRProjects(ctx, &fakeLegacyMRProjectBackfillStore{}, &fakeLegacyMRProjectVerifier{}, "https://gitlab.flexinfer.ai/api/v4", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled backfill error = %v", err)
	}

	run, stage := legacyBackfillFixture("https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/847")
	db := &fakeLegacyMRProjectBackfillStore{
		runs: []*store.PipelineRun{run}, stages: map[string][]*store.StageResult{run.ID: {stage}},
		advanceErr: errors.New("cursor unavailable"),
	}
	got, err := backfillLegacyMRProjects(context.Background(), db, &fakeLegacyMRProjectVerifier{}, "https://gitlab.flexinfer.ai/api/v4", nil)
	if err == nil || !strings.Contains(err.Error(), "advance cursor") {
		t.Fatalf("cursor failure error = %v", err)
	}
	if got.Scanned != 1 || len(db.advanceCalls) != 1 {
		t.Fatalf("cursor failure result=%+v calls=%v", got, db.advanceCalls)
	}
}
