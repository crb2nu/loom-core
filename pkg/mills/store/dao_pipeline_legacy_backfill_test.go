package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func seedLegacyBackfillItem(t *testing.T, st *Store, id string, state BacklogState) {
	t.Helper()
	if err := st.Backlog.Put(context.Background(), &BacklogItem{
		ID: id, Title: id, State: state, Priority: P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("put backlog %s: %v", id, err)
	}
}

func seedLegacyBackfillRun(t *testing.T, st *Store, id, backlogID string, state PipelineState, attempt int, startedAt time.Time, mrIID *int64) {
	t.Helper()
	if err := st.Pipeline.PutRun(context.Background(), &PipelineRun{
		ID: id, BacklogID: backlogID, Template: "mills-default-pipeline",
		State: state, Attempts: attempt, StartedAt: startedAt, MRIID: mrIID,
	}); err != nil {
		t.Fatalf("put run %s: %v", id, err)
	}
}

func seedLegacyBackfillStage(t *testing.T, st *Store, runID, stage string, attempt int, outcome *StageOutcome, artifacts map[string]any) *StageResult {
	t.Helper()
	endedAt := time.Now().UTC()
	result := &StageResult{
		PipelineRunID: runID,
		Stage:         stage,
		Attempt:       attempt,
		StartedAt:     endedAt.Add(-time.Second),
		EndedAt:       &endedAt,
		Outcome:       outcome,
		Artifacts:     artifacts,
	}
	if err := st.Pipeline.PutStage(context.Background(), result); err != nil {
		t.Fatalf("put stage %s/%s/%d: %v", runID, stage, attempt, err)
	}
	return result
}

func TestPipelineListLegacyMRProjectBackfillCandidatesFilters(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	success := StageOutcomeSuccess
	failure := StageOutcomeError
	next := 0

	add := func(name string, backlogState BacklogState, runState PipelineState, mrIID *int64, artifacts map[string]any) (string, *StageResult) {
		t.Helper()
		next++
		backlogID := "BL-BACKFILL-" + name
		runID := "RUN-BACKFILL-" + name
		seedLegacyBackfillItem(t, st, backlogID, backlogState)
		seedLegacyBackfillRun(t, st, runID, backlogID, runState, 1, base.Add(time.Duration(next)*time.Minute), mrIID)
		return runID, seedLegacyBackfillStage(t, st, runID, "mr", 1, &success, artifacts)
	}
	iid := func(value int64) *int64 { return &value }

	want := make(map[string]bool)
	eligible, _ := add("eligible", BacklogEscalated, PipelineEscalated, iid(101), map[string]any{
		"mr_url": " https://gitlab.example/services/loom-core/-/merge_requests/101 ",
		"mr_iid": 101,
		"other":  "keep",
	})
	want[eligible] = true
	seedLegacyBackfillStage(t, st, eligible, "ci_watch", 1, &failure, map[string]any{"ci_project": "services/wrong"})
	seedLegacyBackfillStage(t, st, eligible, "research", 1, &success, map[string]any{"mr_project": "services/ignored"})
	seedLegacyBackfillStage(t, st, eligible, "mr", 2, &failure, map[string]any{
		"mr_url": "https://gitlab.example/services/wrong/-/merge_requests/999", "mr_iid": 999,
	})

	add("nil-iid", BacklogEscalated, PipelineEscalated, nil, map[string]any{"mr_url": "https://gitlab.example/mr/1", "mr_iid": 1})
	add("zero-iid", BacklogEscalated, PipelineEscalated, iid(0), map[string]any{"mr_url": "https://gitlab.example/mr/1", "mr_iid": 0})
	add("negative-iid", BacklogEscalated, PipelineEscalated, iid(-1), map[string]any{"mr_url": "https://gitlab.example/mr/1", "mr_iid": -1})
	add("backlog-running", BacklogRunning, PipelineEscalated, iid(102), map[string]any{"mr_url": "https://gitlab.example/mr/102", "mr_iid": 102})
	add("run-done", BacklogEscalated, PipelineDone, iid(103), map[string]any{"mr_url": "https://gitlab.example/mr/103", "mr_iid": 103})
	add("missing-url", BacklogEscalated, PipelineEscalated, iid(104), map[string]any{"mr_iid": 104, "other": "value"})
	add("blank-url", BacklogEscalated, PipelineEscalated, iid(105), map[string]any{"mr_url": " \t ", "mr_iid": 105})
	add("number-url", BacklogEscalated, PipelineEscalated, iid(106), map[string]any{"mr_url": 106, "mr_iid": 106})
	add("missing-artifact-iid", BacklogEscalated, PipelineEscalated, iid(112), map[string]any{"mr_url": "https://gitlab.example/mr/112"})
	add("zero-artifact-iid", BacklogEscalated, PipelineEscalated, iid(113), map[string]any{"mr_url": "https://gitlab.example/mr/113", "mr_iid": 0})
	add("non-integer-artifact-iid", BacklogEscalated, PipelineEscalated, iid(114), map[string]any{"mr_url": "https://gitlab.example/mr/114", "mr_iid": 114.5})
	add("mismatched-artifact-iid", BacklogEscalated, PipelineEscalated, iid(115), map[string]any{"mr_url": "https://gitlab.example/mr/115", "mr_iid": 999})
	multipleMR, _ := add("multiple-mr-invalid", BacklogEscalated, PipelineEscalated, iid(116), map[string]any{
		"mr_url": "https://gitlab.example/mr/116", "mr_iid": 116,
	})
	seedLegacyBackfillStage(t, st, multipleMR, "mr", 2, &success, map[string]any{
		"mr_url": "https://gitlab.example/mr/116",
	})

	_, malformed := add("malformed", BacklogEscalated, PipelineEscalated, iid(107), map[string]any{"mr_url": "https://gitlab.example/mr/107", "mr_iid": 107})
	if _, err := st.DB().ExecContext(ctx, `UPDATE stage_results SET artifacts_json = '{' WHERE id = ?`, malformed.ID); err != nil {
		t.Fatalf("malform MR artifacts: %v", err)
	}
	_, nonObject := add("non-object", BacklogEscalated, PipelineEscalated, iid(108), map[string]any{"mr_url": "https://gitlab.example/mr/108", "mr_iid": 108})
	if _, err := st.DB().ExecContext(ctx, `UPDATE stage_results SET artifacts_json = '[]' WHERE id = ?`, nonObject.ID); err != nil {
		t.Fatalf("make MR artifacts non-object: %v", err)
	}
	malformedExtra, _ := add("malformed-extra", BacklogEscalated, PipelineEscalated, iid(109), map[string]any{"mr_url": "https://gitlab.example/mr/109", "mr_iid": 109})
	extra := seedLegacyBackfillStage(t, st, malformedExtra, "implement", 1, &success, map[string]any{"ok": true})
	if _, err := st.DB().ExecContext(ctx, `UPDATE stage_results SET artifacts_json = 'null' WHERE id = ?`, extra.ID); err != nil {
		t.Fatalf("make extra artifacts non-object: %v", err)
	}

	add("mr-project", BacklogEscalated, PipelineEscalated, iid(110), map[string]any{
		"mr_url": "https://gitlab.example/mr/110", "mr_iid": 110, "mr_project": "services/loom-core",
	})
	add("blank-mr-project", BacklogEscalated, PipelineEscalated, iid(111), map[string]any{
		"mr_url": "https://gitlab.example/mr/111", "mr_iid": 111, "mr_project": "",
	})
	for i, projectStage := range []struct {
		stage string
		key   string
	}{
		{stage: "ci_watch", key: "ci_project"},
		{stage: "merge", key: "merged_project"},
		{stage: "cleanup", key: "cleanup_project"},
	} {
		runID, _ := add(fmt.Sprintf("durable-%d", i), BacklogEscalated, PipelineEscalated, iid(int64(120+i)), map[string]any{
			"mr_url": fmt.Sprintf("https://gitlab.example/mr/%d", 120+i),
			"mr_iid": 120 + i,
		})
		seedLegacyBackfillStage(t, st, runID, projectStage.stage, 1, &success, map[string]any{projectStage.key: "services/loom-core"})
	}

	got, err := st.Pipeline.ListLegacyMRProjectBackfillCandidates(ctx, 128)
	if err != nil {
		t.Fatalf("ListLegacyMRProjectBackfillCandidates: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("candidate IDs = %v, want %v", legacyBackfillRunIDs(got), want)
	}
	for _, run := range got {
		if !want[run.ID] {
			t.Errorf("unexpected candidate %s", run.ID)
		}
	}
}

func TestPipelineListLegacyMRProjectBackfillCandidatesUsesExactLatestRun(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	success := StageOutcomeSuccess
	base := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
	iid := int64(201)

	seedLegacyBackfillItem(t, st, "BL-LATEST-BLOCKED", BacklogEscalated)
	seedLegacyBackfillRun(t, st, "RUN-LATEST-OLDER", "BL-LATEST-BLOCKED", PipelineEscalated, 2, base, &iid)
	seedLegacyBackfillStage(t, st, "RUN-LATEST-OLDER", "mr", 1, &success, map[string]any{"mr_url": "https://gitlab.example/mr/201", "mr_iid": 201})
	seedLegacyBackfillRun(t, st, "RUN-LATEST-NEWER", "BL-LATEST-BLOCKED", PipelineEscalated, 1, base.Add(time.Minute), &iid)
	seedLegacyBackfillStage(t, st, "RUN-LATEST-NEWER", "mr", 1, &success, map[string]any{"no_url": true})

	seedLegacyBackfillItem(t, st, "BL-LATEST-TIE", BacklogEscalated)
	seedLegacyBackfillRun(t, st, "RUN-TIE-LOW-ATTEMPT", "BL-LATEST-TIE", PipelineEscalated, 1, base.Add(2*time.Minute), &iid)
	seedLegacyBackfillStage(t, st, "RUN-TIE-LOW-ATTEMPT", "mr", 1, &success, map[string]any{"no_url": true})
	seedLegacyBackfillRun(t, st, "RUN-TIE-HIGH-ATTEMPT", "BL-LATEST-TIE", PipelineEscalated, 2, base.Add(2*time.Minute), &iid)
	seedLegacyBackfillStage(t, st, "RUN-TIE-HIGH-ATTEMPT", "mr", 1, &success, map[string]any{"mr_url": "https://gitlab.example/mr/201", "mr_iid": 201})

	got, err := st.Pipeline.ListLegacyMRProjectBackfillCandidates(ctx, 128)
	if err != nil {
		t.Fatalf("ListLegacyMRProjectBackfillCandidates: %v", err)
	}
	if ids := legacyBackfillRunIDs(got); !reflect.DeepEqual(ids, []string{"RUN-TIE-HIGH-ATTEMPT"}) {
		t.Fatalf("candidate IDs = %v, want exact tie-broken latest run", ids)
	}
}

func TestPipelineListLegacyMRProjectBackfillCandidatesBoundsLimit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	success := StageOutcomeSuccess
	base := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 129; i++ {
		backlogID := fmt.Sprintf("BL-BOUND-%03d", i)
		runID := fmt.Sprintf("RUN-BOUND-%03d", i)
		iid := int64(1000 + i)
		seedLegacyBackfillItem(t, st, backlogID, BacklogEscalated)
		seedLegacyBackfillRun(t, st, runID, backlogID, PipelineEscalated, 1, base.Add(time.Duration(i)*time.Second), &iid)
		seedLegacyBackfillStage(t, st, runID, "mr", 1, &success, map[string]any{
			"mr_url": fmt.Sprintf("https://gitlab.example/mr/%d", iid), "mr_iid": iid,
		})
	}
	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{name: "explicit", limit: 3, want: 3},
		{name: "default", limit: 0, want: 128},
		{name: "negative", limit: -1, want: 128},
		{name: "capped", limit: 1024, want: 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.Pipeline.ListLegacyMRProjectBackfillCandidates(ctx, tc.limit)
			if err != nil {
				t.Fatalf("ListLegacyMRProjectBackfillCandidates(%d): %v", tc.limit, err)
			}
			if len(got) != tc.want {
				t.Fatalf("candidate count = %d, want %d", len(got), tc.want)
			}
		})
	}
}

func TestPipelineLegacyMRProjectBackfillCursorRotatesAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "mills.db")
	st, err := Open(ctx, Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open cursor store: %v", err)
	}
	t.Cleanup(func() {
		if st != nil {
			_ = st.Close()
		}
	})

	success := StageOutcomeSuccess
	base := time.Date(2026, 7, 23, 15, 0, 0, 0, time.UTC)
	startedAt := []time.Time{base, base, base, base.Add(time.Second), base.Add(2 * time.Second)}
	attempts := []int{2, 1, 1, 1, 1}
	for i, suffix := range []string{"A", "B", "C", "D", "E"} {
		backlogID := "BL-CURSOR-" + suffix
		runID := "RUN-CURSOR-" + suffix
		iid := int64(600 + i)
		seedLegacyBackfillItem(t, st, backlogID, BacklogEscalated)
		seedLegacyBackfillRun(t, st, runID, backlogID, PipelineEscalated, attempts[i], startedAt[i], &iid)
		seedLegacyBackfillStage(t, st, runID, "mr", 1, &success, map[string]any{
			"mr_url": fmt.Sprintf("https://gitlab.example/mr/%d", iid), "mr_iid": iid,
		})
	}

	first, err := st.Pipeline.ListLegacyMRProjectBackfillCandidates(ctx, 3)
	if err != nil {
		t.Fatalf("list first cursor page: %v", err)
	}
	if ids := legacyBackfillRunIDs(first); !reflect.DeepEqual(ids, []string{"RUN-CURSOR-A", "RUN-CURSOR-B", "RUN-CURSOR-C"}) {
		t.Fatalf("first cursor page = %v", ids)
	}
	if err := st.Pipeline.AdvanceLegacyMRProjectBackfillCursor(ctx, first[len(first)-1]); err != nil {
		t.Fatalf("advance cursor: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close cursor store: %v", err)
	}
	st = nil
	st, err = Open(ctx, Options{Path: dbPath})
	if err != nil {
		t.Fatalf("reopen cursor store: %v", err)
	}

	second, err := st.Pipeline.ListLegacyMRProjectBackfillCandidates(ctx, 3)
	if err != nil {
		t.Fatalf("list rotated cursor page: %v", err)
	}
	if ids := legacyBackfillRunIDs(second); !reflect.DeepEqual(ids, []string{"RUN-CURSOR-D", "RUN-CURSOR-E", "RUN-CURSOR-A"}) {
		t.Fatalf("rotated cursor page = %v", ids)
	}
}

func TestPipelineLegacyMRProjectBackfillCursorAdvancesBeyondFullBatch(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "mills.db")
	st, err := Open(ctx, Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open cursor store: %v", err)
	}
	t.Cleanup(func() {
		if st != nil {
			_ = st.Close()
		}
	})

	success := StageOutcomeSuccess
	base := time.Date(2026, 7, 23, 16, 0, 0, 0, time.UTC)
	for i := 0; i < 130; i++ {
		backlogID := fmt.Sprintf("BL-CURSOR-BATCH-%03d", i)
		runID := fmt.Sprintf("RUN-CURSOR-BATCH-%03d", i)
		iid := int64(7000 + i)
		seedLegacyBackfillItem(t, st, backlogID, BacklogEscalated)
		seedLegacyBackfillRun(t, st, runID, backlogID, PipelineEscalated, 1, base.Add(time.Duration(i)*time.Second), &iid)
		// A foreign-but-well-formed URL is SQL-eligible and deterministically
		// rejected by the coordinator. Leaving every row unpatched models a
		// permanently rejected head page.
		seedLegacyBackfillStage(t, st, runID, "mr", 1, &success, map[string]any{
			"mr_url": fmt.Sprintf("https://foreign.example/services/loom-core/-/merge_requests/%d", iid),
			"mr_iid": iid,
		})
	}

	first, err := st.Pipeline.ListLegacyMRProjectBackfillCandidates(ctx, 128)
	if err != nil {
		t.Fatalf("list full cursor batch: %v", err)
	}
	if len(first) != 128 {
		t.Fatalf("first cursor batch has %d rows, want 128", len(first))
	}
	if first[0].ID != "RUN-CURSOR-BATCH-000" || first[127].ID != "RUN-CURSOR-BATCH-127" {
		t.Fatalf("first cursor batch bounds = %v...%v (%d rows)", first[0].ID, first[len(first)-1].ID, len(first))
	}
	if err := st.Pipeline.AdvanceLegacyMRProjectBackfillCursor(ctx, first[len(first)-1]); err != nil {
		t.Fatalf("advance full-batch cursor: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close cursor store: %v", err)
	}
	st = nil
	st, err = Open(ctx, Options{Path: dbPath})
	if err != nil {
		t.Fatalf("reopen cursor store: %v", err)
	}

	second, err := st.Pipeline.ListLegacyMRProjectBackfillCandidates(ctx, 3)
	if err != nil {
		t.Fatalf("list post-batch cursor page: %v", err)
	}
	if ids := legacyBackfillRunIDs(second); !reflect.DeepEqual(ids, []string{
		"RUN-CURSOR-BATCH-128", "RUN-CURSOR-BATCH-129", "RUN-CURSOR-BATCH-000",
	}) {
		t.Fatalf("post-batch cursor page = %v", ids)
	}
}

func legacyBackfillRunIDs(runs []*PipelineRun) []string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids
}

type legacyStageSnapshot struct {
	pipelineRunID string
	stage         string
	attempt       int
	startedAt     string
	endedAt       sql.NullString
	outcome       sql.NullString
	spawnID       sql.NullString
	costUSD       float64
	model         sql.NullString
	backend       sql.NullString
	logTail       sql.NullString
}

func loadLegacyStageSnapshot(t *testing.T, st *Store, id int64) (legacyStageSnapshot, string) {
	t.Helper()
	var got legacyStageSnapshot
	var artifacts string
	if err := st.DB().QueryRowContext(context.Background(), `
		SELECT pipeline_run_id, stage, attempt, started_at, ended_at, outcome,
		       spawn_id, cost_usd, model, backend, log_tail, artifacts_json
		FROM stage_results WHERE id = ?
	`, id).Scan(
		&got.pipelineRunID, &got.stage, &got.attempt, &got.startedAt,
		&got.endedAt, &got.outcome, &got.spawnID, &got.costUSD, &got.model,
		&got.backend, &got.logTail, &artifacts,
	); err != nil {
		t.Fatalf("load stage %d: %v", id, err)
	}
	return got, artifacts
}

func TestPipelinePatchMRProjectArtifactPreservesRowAndIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	success := StageOutcomeSuccess
	iid := int64(301)
	seedLegacyBackfillItem(t, st, "BL-PATCH", BacklogEscalated)
	seedLegacyBackfillRun(t, st, "RUN-PATCH", "BL-PATCH", PipelineEscalated, 1, time.Now().UTC(), &iid)
	endedAt := time.Now().UTC()
	stage := &StageResult{
		PipelineRunID: "RUN-PATCH",
		Stage:         "mr",
		Attempt:       7,
		StartedAt:     endedAt.Add(-2 * time.Minute),
		EndedAt:       &endedAt,
		Outcome:       &success,
		SpawnID:       "spawn-301",
		CostUSD:       1.25,
		Model:         "gpt-test",
		Backend:       "codex",
		Artifacts: map[string]any{
			"mr_url": "https://gitlab.example/services/loom-core/-/merge_requests/301",
			"mr_iid": 301,
			"nested": map[string]any{"keep": true},
		},
		LogTail: "unchanged log",
	}
	if err := st.Pipeline.PutStage(ctx, stage); err != nil {
		t.Fatalf("put patch stage: %v", err)
	}

	before, beforeArtifacts := loadLegacyStageSnapshot(t, st, stage.ID)
	applied, err := st.Pipeline.PatchMRProjectArtifact(
		ctx, stage.ID, "RUN-PATCH",
		"https://gitlab.example/services/loom-core/-/merge_requests/301", 301,
		"  services/loom-core  ",
	)
	if err != nil || !applied {
		t.Fatalf("PatchMRProjectArtifact applied = %v, err = %v", applied, err)
	}
	after, afterArtifacts := loadLegacyStageSnapshot(t, st, stage.ID)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("non-artifact columns changed:\nbefore=%+v\nafter=%+v", before, after)
	}
	var beforeMap, afterMap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(beforeArtifacts), &beforeMap); err != nil {
		t.Fatalf("decode before artifacts: %v", err)
	}
	if err := json.Unmarshal([]byte(afterArtifacts), &afterMap); err != nil {
		t.Fatalf("decode after artifacts: %v", err)
	}
	var project string
	if err := json.Unmarshal(afterMap["mr_project"], &project); err != nil || project != "services/loom-core" {
		t.Fatalf("mr_project = %q, %v", project, err)
	}
	delete(afterMap, "mr_project")
	if !reflect.DeepEqual(afterMap, beforeMap) {
		t.Fatalf("existing artifacts changed: before=%s after=%s", beforeArtifacts, afterArtifacts)
	}

	applied, err = st.Pipeline.PatchMRProjectArtifact(
		ctx, stage.ID, "RUN-PATCH",
		"https://gitlab.example/services/loom-core/-/merge_requests/301", 301,
		"services/loom-core",
	)
	if err != nil || applied {
		t.Fatalf("idempotent patch applied = %v, err = %v", applied, err)
	}
	_, idempotentArtifacts := loadLegacyStageSnapshot(t, st, stage.ID)
	if idempotentArtifacts != afterArtifacts {
		t.Fatalf("idempotent patch rewrote artifacts: before=%q after=%q", afterArtifacts, idempotentArtifacts)
	}

	applied, err = st.Pipeline.PatchMRProjectArtifact(
		ctx, stage.ID, "RUN-PATCH",
		"https://gitlab.example/services/loom-core/-/merge_requests/301", 301,
		"services/other",
	)
	if applied || !errors.Is(err, ErrMRProjectArtifactConflict) {
		t.Fatalf("conflicting patch applied = %v, err = %v", applied, err)
	}
	_, conflictArtifacts := loadLegacyStageSnapshot(t, st, stage.ID)
	if conflictArtifacts != afterArtifacts {
		t.Fatalf("conflicting patch changed artifacts: before=%q after=%q", afterArtifacts, conflictArtifacts)
	}
}

func TestPipelinePatchMRProjectArtifactRejectsUnsafeRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	success := StageOutcomeSuccess
	failure := StageOutcomeError
	iid := int64(401)
	seedLegacyBackfillItem(t, st, "BL-PATCH-REJECT", BacklogEscalated)
	seedLegacyBackfillRun(t, st, "RUN-PATCH-REJECT", "BL-PATCH-REJECT", PipelineEscalated, 1, time.Now().UTC(), &iid)

	wrongStage := seedLegacyBackfillStage(t, st, "RUN-PATCH-REJECT", "ci_watch", 1, &success, map[string]any{"mr_url": "https://gitlab.example/mr/401"})
	failedMR := seedLegacyBackfillStage(t, st, "RUN-PATCH-REJECT", "mr", 1, &failure, map[string]any{"mr_url": "https://gitlab.example/mr/401"})
	pendingMR := seedLegacyBackfillStage(t, st, "RUN-PATCH-REJECT", "mr", 2, nil, map[string]any{"mr_url": "https://gitlab.example/mr/401"})
	nonObject := seedLegacyBackfillStage(t, st, "RUN-PATCH-REJECT", "mr", 3, &success, map[string]any{"mr_url": "https://gitlab.example/mr/401"})
	if _, err := st.DB().ExecContext(ctx, `UPDATE stage_results SET artifacts_json = '[]' WHERE id = ?`, nonObject.ID); err != nil {
		t.Fatalf("make patch artifacts non-object: %v", err)
	}
	malformed := seedLegacyBackfillStage(t, st, "RUN-PATCH-REJECT", "mr", 4, &success, map[string]any{"mr_url": "https://gitlab.example/mr/401"})
	if _, err := st.DB().ExecContext(ctx, `UPDATE stage_results SET artifacts_json = '{' WHERE id = ?`, malformed.ID); err != nil {
		t.Fatalf("malform patch artifacts: %v", err)
	}
	existingInvalid := seedLegacyBackfillStage(t, st, "RUN-PATCH-REJECT", "mr", 5, &success, map[string]any{
		"mr_url": "https://gitlab.example/mr/401", "mr_iid": 401, "mr_project": 42,
	})

	for _, tc := range []struct {
		name       string
		stage      *StageResult
		conflicted bool
	}{
		{name: "wrong stage", stage: wrongStage},
		{name: "failed outcome", stage: failedMR},
		{name: "pending outcome", stage: pendingMR},
		{name: "non-object artifacts", stage: nonObject},
		{name: "malformed artifacts", stage: malformed},
		{name: "existing non-text project", stage: existingInvalid, conflicted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, before := loadLegacyStageSnapshot(t, st, tc.stage.ID)
			applied, err := st.Pipeline.PatchMRProjectArtifact(
				ctx, tc.stage.ID, "RUN-PATCH-REJECT", "https://gitlab.example/mr/401", 401, "services/loom-core",
			)
			if applied || err == nil {
				t.Fatalf("unsafe patch applied = %v, err = %v", applied, err)
			}
			if tc.conflicted && !errors.Is(err, ErrMRProjectArtifactConflict) {
				t.Fatalf("conflict error = %v", err)
			}
			_, after := loadLegacyStageSnapshot(t, st, tc.stage.ID)
			if after != before {
				t.Fatalf("unsafe patch changed artifacts: before=%q after=%q", before, after)
			}
		})
	}
	if applied, err := st.Pipeline.PatchMRProjectArtifact(ctx, 999999, "RUN-PATCH-REJECT", "https://gitlab.example/mr/401", 401, "services/loom-core"); applied || !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing stage applied = %v, err = %v", applied, err)
	}
	if applied, err := st.Pipeline.PatchMRProjectArtifact(ctx, wrongStage.ID, "RUN-PATCH-REJECT", "https://gitlab.example/mr/401", 401, " \t "); applied || err == nil {
		t.Fatalf("blank project applied = %v, err = %v", applied, err)
	}
}

func TestPipelinePatchMRProjectArtifactRejectsChangedMRIdentity(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	success := StageOutcomeSuccess
	iid := int64(451)
	seedLegacyBackfillItem(t, st, "BL-PATCH-IDENTITY", BacklogEscalated)
	seedLegacyBackfillRun(t, st, "RUN-PATCH-IDENTITY", "BL-PATCH-IDENTITY", PipelineEscalated, 1, time.Now().UTC(), &iid)

	changedURL := seedLegacyBackfillStage(t, st, "RUN-PATCH-IDENTITY", "mr", 1, &success, map[string]any{
		"mr_url": "https://gitlab.example/mr/451", "mr_iid": 451,
	})
	if _, err := st.DB().ExecContext(ctx, `
		UPDATE stage_results
		SET artifacts_json = json_set(artifacts_json, '$.mr_url', 'https://attacker.example/mr/451')
		WHERE id = ?
	`, changedURL.ID); err != nil {
		t.Fatalf("replace verified MR URL: %v", err)
	}
	applied, err := st.Pipeline.PatchMRProjectArtifact(
		ctx, changedURL.ID, "RUN-PATCH-IDENTITY", "https://gitlab.example/mr/451", 451, "services/loom-core",
	)
	if applied || !errors.Is(err, ErrMRProjectArtifactConflict) {
		t.Fatalf("stale-URL patch applied = %v, err = %v", applied, err)
	}
	_, changedURLArtifacts := loadLegacyStageSnapshot(t, st, changedURL.ID)
	if json.Valid([]byte(changedURLArtifacts)) && jsonContainsKey(t, changedURLArtifacts, "mr_project") {
		t.Fatalf("stale-URL patch added mr_project: %s", changedURLArtifacts)
	}

	changedIID := seedLegacyBackfillStage(t, st, "RUN-PATCH-IDENTITY", "mr", 2, &success, map[string]any{
		"mr_url": "https://gitlab.example/mr/451", "mr_iid": 451,
	})
	if _, err := st.DB().ExecContext(ctx, `
		UPDATE stage_results
		SET artifacts_json = json_set(artifacts_json, '$.mr_iid', 999)
		WHERE id = ?
	`, changedIID.ID); err != nil {
		t.Fatalf("replace verified MR IID: %v", err)
	}
	applied, err = st.Pipeline.PatchMRProjectArtifact(
		ctx, changedIID.ID, "RUN-PATCH-IDENTITY", "https://gitlab.example/mr/451", 451, "services/loom-core",
	)
	if applied || !errors.Is(err, ErrMRProjectArtifactConflict) {
		t.Fatalf("stale-IID patch applied = %v, err = %v", applied, err)
	}
	_, changedIIDArtifacts := loadLegacyStageSnapshot(t, st, changedIID.ID)
	if jsonContainsKey(t, changedIIDArtifacts, "mr_project") {
		t.Fatalf("stale-IID patch added mr_project: %s", changedIIDArtifacts)
	}

	wrongRun := seedLegacyBackfillStage(t, st, "RUN-PATCH-IDENTITY", "mr", 3, &success, map[string]any{
		"mr_url": "https://gitlab.example/mr/451", "mr_iid": 451,
	})
	applied, err = st.Pipeline.PatchMRProjectArtifact(
		ctx, wrongRun.ID, "RUN-DIFFERENT", "https://gitlab.example/mr/451", 451, "services/loom-core",
	)
	if applied || !errors.Is(err, ErrMRProjectArtifactConflict) {
		t.Fatalf("wrong-run patch applied = %v, err = %v", applied, err)
	}
	_, wrongRunArtifacts := loadLegacyStageSnapshot(t, st, wrongRun.ID)
	if jsonContainsKey(t, wrongRunArtifacts, "mr_project") {
		t.Fatalf("wrong-run patch added mr_project: %s", wrongRunArtifacts)
	}

	changedRunIID := seedLegacyBackfillStage(t, st, "RUN-PATCH-IDENTITY", "mr", 4, &success, map[string]any{
		"mr_url": "https://gitlab.example/mr/451", "mr_iid": 451,
	})
	if _, err := st.DB().ExecContext(ctx, `UPDATE pipeline_runs SET mr_iid = 999 WHERE id = ?`, "RUN-PATCH-IDENTITY"); err != nil {
		t.Fatalf("replace verified run MR IID: %v", err)
	}
	applied, err = st.Pipeline.PatchMRProjectArtifact(
		ctx, changedRunIID.ID, "RUN-PATCH-IDENTITY", "https://gitlab.example/mr/451", 451, "services/loom-core",
	)
	if applied || !errors.Is(err, ErrMRProjectArtifactConflict) {
		t.Fatalf("stale-run-IID patch applied = %v, err = %v", applied, err)
	}
	_, changedRunIIDArtifacts := loadLegacyStageSnapshot(t, st, changedRunIID.ID)
	if jsonContainsKey(t, changedRunIIDArtifacts, "mr_project") {
		t.Fatalf("stale-run-IID patch added mr_project: %s", changedRunIIDArtifacts)
	}
}

func jsonContainsKey(t *testing.T, raw, key string) bool {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		t.Fatalf("decode JSON object: %v", err)
	}
	_, exists := object[key]
	return exists
}

func TestPipelinePatchMRProjectArtifactCASConflict(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	success := StageOutcomeSuccess
	iid := int64(501)
	seedLegacyBackfillItem(t, st, "BL-PATCH-CAS", BacklogEscalated)
	seedLegacyBackfillRun(t, st, "RUN-PATCH-CAS", "BL-PATCH-CAS", PipelineEscalated, 1, time.Now().UTC(), &iid)
	stage := seedLegacyBackfillStage(t, st, "RUN-PATCH-CAS", "mr", 1, &success, map[string]any{
		"mr_url": "https://gitlab.example/mr/501", "mr_iid": 501,
	})
	_, before := loadLegacyStageSnapshot(t, st, stage.ID)

	trigger := fmt.Sprintf(`
		CREATE TRIGGER reject_backfill_cas
		BEFORE UPDATE OF artifacts_json ON stage_results
		WHEN OLD.id = %d
		BEGIN
			SELECT RAISE(IGNORE);
		END
	`, stage.ID)
	if _, err := st.DB().ExecContext(ctx, trigger); err != nil {
		t.Fatalf("create CAS trigger: %v", err)
	}
	applied, err := st.Pipeline.PatchMRProjectArtifact(
		ctx, stage.ID, "RUN-PATCH-CAS", "https://gitlab.example/mr/501", 501, "services/loom-core",
	)
	if applied || !errors.Is(err, ErrMRProjectArtifactConflict) {
		t.Fatalf("CAS patch applied = %v, err = %v", applied, err)
	}
	_, after := loadLegacyStageSnapshot(t, st, stage.ID)
	if after != before {
		t.Fatalf("failed CAS changed artifacts: before=%q after=%q", before, after)
	}
}
