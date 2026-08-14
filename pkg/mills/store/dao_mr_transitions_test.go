package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ----- fixtures -----

var headTransitionNow = time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC)

// seedHeadTransitionRun creates the backlog item + pipeline run the ledger's
// foreign keys require and returns the run id.
func seedHeadTransitionRun(t *testing.T, st *Store, id string) string {
	t.Helper()
	ctx := context.Background()
	item := &BacklogItem{
		ID:       "BL-" + id,
		Title:    "head transition fixture",
		State:    BacklogRunning,
		Priority: P2,
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	run := &PipelineRun{
		ID:        "PIPE-" + id,
		BacklogID: item.ID,
		Template:  "mills-default-pipeline",
		State:     PipelineMerging,
		Attempts:  1,
		StartedAt: headTransitionNow,
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return run.ID
}

func externalMovement(runID, reviewed, successor string) *MRHeadTransition {
	return &MRHeadTransition{
		PipelineRunID: runID,
		Project:       "services/loom-core",
		MRIID:         77,
		SourceBranch:  "feat/head-transitions",
		TargetBranch:  "main",
		ReviewedSHA:   reviewed,
		SuccessorSHA:  successor,
		Trigger:       MRHeadTriggerExternal,
		State:         MRHeadTransitionAmbiguous,
		RequestedAt:   headTransitionNow,
	}
}

func rebaseRequest(runID, reviewed string) *MRHeadTransition {
	return &MRHeadTransition{
		PipelineRunID: runID,
		Project:       "services/loom-core",
		MRIID:         77,
		SourceBranch:  "feat/head-transitions",
		TargetBranch:  "main",
		ReviewedSHA:   reviewed,
		TargetHeadSHA: "target-tip",
		Trigger:       MRHeadTriggerRebaseRequest,
		State:         MRHeadTransitionRequested,
		RequestedAt:   headTransitionNow,
	}
}

// ----- Open: seq allocation + one-open-row CAS -----

func TestMRHeadTransitions_OpenAllocatesMonotoneSeq(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "SEQ")

	first, err := st.MRHeadTransitions.Open(ctx, externalMovement(runID, "sha-a", "sha-b"))
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	if first.Seq != 1 {
		t.Fatalf("first seq = %d, want 1", first.Seq)
	}
	if first.ID == 0 {
		t.Error("first row id not populated")
	}
	// The external row opened terminal, so it is already settled and does not
	// block the next movement.
	second, err := st.MRHeadTransitions.Open(ctx, externalMovement(runID, "sha-b", "sha-c"))
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	if second.Seq != 2 {
		t.Errorf("second seq = %d, want 2", second.Seq)
	}
}

// A rebase PUT is a non-idempotent mutation. While a row is unsettled, a
// second Open must be refused so a restarted operator re-observes rather than
// mutating GitLab twice for one logical movement.
func TestMRHeadTransitions_OpenRefusesSecondUnsettledRow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "OPENCAS")

	if _, err := st.MRHeadTransitions.Open(ctx, rebaseRequest(runID, "sha-a")); err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err := st.MRHeadTransitions.Open(ctx, rebaseRequest(runID, "sha-a"))
	if !errors.Is(err, ErrHeadTransitionOpen) {
		t.Fatalf("second open err = %v, want ErrHeadTransitionOpen", err)
	}

	// Another RUN is unaffected — the guard is per-run, not global.
	otherRun := seedHeadTransitionRun(t, st, "OPENCAS-2")
	if _, err := st.MRHeadTransitions.Open(ctx, rebaseRequest(otherRun, "sha-z")); err != nil {
		t.Fatalf("open on unrelated run: %v", err)
	}
}

func TestMRHeadTransitions_OpenRejectsUnattributableExternal(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "EXTATTR")

	row := externalMovement(runID, "sha-a", "sha-b")
	row.State = MRHeadTransitionAttributed
	if _, err := st.MRHeadTransitions.Open(ctx, row); err == nil {
		t.Fatal("expected an external trigger to be refused the attributed state")
	}
}

func TestMRHeadTransitions_OpenValidatesRequiredFields(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "VALIDATE")

	cases := map[string]func(*MRHeadTransition){
		"no run":      func(r *MRHeadTransition) { r.PipelineRunID = "" },
		"no reviewed": func(r *MRHeadTransition) { r.ReviewedSHA = "" },
		"bad trigger": func(r *MRHeadTransition) { r.Trigger = "who-knows" },
		"no state":    func(r *MRHeadTransition) { r.State = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			row := externalMovement(runID, "sha-a", "sha-b")
			mutate(row)
			if _, err := st.MRHeadTransitions.Open(ctx, row); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// ----- Settle: compare-and-swap on settled_at IS NULL -----

func TestMRHeadTransitions_SettleIsCompareAndSwap(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "SETTLECAS")

	opened, err := st.MRHeadTransitions.Open(ctx, rebaseRequest(runID, "sha-a"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	settled, err := st.MRHeadTransitions.Settle(ctx, SettleRequest{
		PipelineRunID: runID,
		Seq:           opened.Seq,
		State:         MRHeadTransitionAttributed,
		SuccessorSHA:  "sha-b",
		Provenance: map[string]any{
			"classifier": "attributed",
			"reason":     "exactly one movement; commit_from == reviewed_sha",
		},
		SettledAt: headTransitionNow.Add(13 * time.Second),
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if settled.State != MRHeadTransitionAttributed || settled.SuccessorSHA != "sha-b" {
		t.Fatalf("settled row = %+v", settled)
	}
	if settled.SettledAt == nil || settled.ObservedAt == nil {
		t.Fatalf("settle must stamp observed_at and settled_at: %+v", settled)
	}
	if got := settled.Provenance["reason"]; got != "exactly one movement; commit_from == reviewed_sha" {
		t.Errorf("provenance reason = %v", got)
	}

	// A racing second observer must not overwrite the recorded verdict.
	again, err := st.MRHeadTransitions.Settle(ctx, SettleRequest{
		PipelineRunID: runID,
		Seq:           opened.Seq,
		State:         MRHeadTransitionAmbiguous,
		SuccessorSHA:  "sha-other",
		SettledAt:     headTransitionNow.Add(20 * time.Second),
	})
	if !errors.Is(err, ErrHeadTransitionSettled) {
		t.Fatalf("second settle err = %v, want ErrHeadTransitionSettled", err)
	}
	if again == nil || again.State != MRHeadTransitionAttributed || again.SuccessorSHA != "sha-b" {
		t.Fatalf("settled verdict was overwritten: %+v", again)
	}
}

func TestMRHeadTransitions_SettleRejectsNonTerminalState(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "SETTLESTATE")
	opened, err := st.MRHeadTransitions.Open(ctx, rebaseRequest(runID, "sha-a"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.MRHeadTransitions.Settle(ctx, SettleRequest{
		PipelineRunID: runID,
		Seq:           opened.Seq,
		State:         MRHeadTransitionInProgress,
	}); err == nil {
		t.Fatal("expected a non-terminal settle to be refused")
	}
}

func TestMRHeadTransitions_SettleMissingRowIsNotFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "SETTLEMISS")
	_, err := st.MRHeadTransitions.Settle(ctx, SettleRequest{
		PipelineRunID: runID,
		Seq:           9,
		State:         MRHeadTransitionAmbiguous,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("settle missing err = %v, want ErrNotFound", err)
	}
}

// ----- fence + budget counters -----

// MaxSettledSeq is the CI re-authorization fence. It must ignore rows that did
// not move the head ('noop') and rows that have not settled yet — otherwise a
// merge would fail closed on evidence of nothing happening.
func TestMRHeadTransitions_MaxSettledSeqIgnoresNoopAndUnsettled(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "FENCE")

	seq, err := st.MRHeadTransitions.MaxSettledSeq(ctx, runID)
	if err != nil {
		t.Fatalf("max settled seq: %v", err)
	}
	if seq != 0 {
		t.Fatalf("fence on a run with no ledger = %d, want 0", seq)
	}

	// seq 1: a real movement.
	if _, err := st.MRHeadTransitions.Open(ctx, externalMovement(runID, "sha-a", "sha-b")); err != nil {
		t.Fatalf("open movement: %v", err)
	}
	// seq 2: a no-op (head never moved).
	noop := externalMovement(runID, "sha-b", "sha-b")
	noop.State = MRHeadTransitionNoop
	if _, err := st.MRHeadTransitions.Open(ctx, noop); err != nil {
		t.Fatalf("open noop: %v", err)
	}
	// seq 3: still in flight.
	if _, err := st.MRHeadTransitions.Open(ctx, rebaseRequest(runID, "sha-b")); err != nil {
		t.Fatalf("open in-flight: %v", err)
	}

	seq, err = st.MRHeadTransitions.MaxSettledSeq(ctx, runID)
	if err != nil {
		t.Fatalf("max settled seq: %v", err)
	}
	if seq != 1 {
		t.Errorf("fence = %d, want 1 (noop and unsettled rows excluded)", seq)
	}
	count, err := st.MRHeadTransitions.CountSettled(ctx, runID)
	if err != nil {
		t.Fatalf("count settled: %v", err)
	}
	if count != 1 {
		t.Errorf("settled movements = %d, want 1", count)
	}
}

// ----- restart rehydration -----

// An operator that dies mid-observation must find its unsettled row on the
// next drive and settle THAT row, never mint a second one for one movement.
func TestMRHeadTransitions_RestartRehydratesOpenRow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "RESTART")

	if none, err := st.MRHeadTransitions.OpenTransition(ctx, runID); err != nil || none != nil {
		t.Fatalf("open transition on empty ledger = %v, %v; want nil, nil", none, err)
	}
	opened, err := st.MRHeadTransitions.Open(ctx, rebaseRequest(runID, "sha-a"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	got, err := st.MRHeadTransitions.OpenTransition(ctx, runID)
	if err != nil {
		t.Fatalf("open transition: %v", err)
	}
	if got == nil || got.Seq != opened.Seq || got.State != MRHeadTransitionRequested {
		t.Fatalf("rehydrated row = %+v, want the open seq %d", got, opened.Seq)
	}
	if got.ReviewedSHA != "sha-a" || got.TargetHeadSHA != "target-tip" {
		t.Errorf("rehydrated identity lost: %+v", got)
	}

	if _, err := st.MRHeadTransitions.Settle(ctx, SettleRequest{
		PipelineRunID: runID,
		Seq:           got.Seq,
		State:         MRHeadTransitionAmbiguous,
		SuccessorSHA:  "sha-b",
		SettledAt:     headTransitionNow.Add(time.Minute),
	}); err != nil {
		t.Fatalf("settle after restart: %v", err)
	}
	if none, err := st.MRHeadTransitions.OpenTransition(ctx, runID); err != nil || none != nil {
		t.Fatalf("open transition after settle = %v, %v; want nil, nil", none, err)
	}
}

func TestMRHeadTransitions_ListByRunIsNewestFirst(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "LIST")

	for _, pair := range [][2]string{{"sha-a", "sha-b"}, {"sha-b", "sha-c"}, {"sha-c", "sha-d"}} {
		if _, err := st.MRHeadTransitions.Open(ctx, externalMovement(runID, pair[0], pair[1])); err != nil {
			t.Fatalf("open %v: %v", pair, err)
		}
	}
	rows, err := st.MRHeadTransitions.ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for i, wantSeq := range []int64{3, 2, 1} {
		if rows[i].Seq != wantSeq {
			t.Errorf("rows[%d].Seq = %d, want %d", i, rows[i].Seq, wantSeq)
		}
	}
	// An unrelated run's ledger is empty, not nil-vs-populated confusion.
	other := seedHeadTransitionRun(t, st, "LIST-2")
	if rows, err := st.MRHeadTransitions.ListByRun(ctx, other); err != nil || len(rows) != 0 {
		t.Fatalf("unrelated ledger = %v, %v", rows, err)
	}
}

// The schema's CHECK constraints are the last line of defence against a typo
// minting an unclassifiable row.
func TestMRHeadTransitions_SchemaRejectsUnknownStateAndTrigger(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "CHECKS")

	_, err := st.DB().ExecContext(ctx, `
		INSERT INTO mr_head_transitions (
			pipeline_run_id, seq, project, mr_iid, source_branch, target_branch,
			reviewed_sha, target_head_sha, trigger, state, provenance_json, requested_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		runID, 1, "services/loom-core", 1, "feat/x", "main", "sha-a", "", "external", "sideways", "{}", "2026-07-25T16:00:00Z")
	if err == nil {
		t.Error("expected the state CHECK constraint to reject an unknown state")
	}
	_, err = st.DB().ExecContext(ctx, `
		INSERT INTO mr_head_transitions (
			pipeline_run_id, seq, project, mr_iid, source_branch, target_branch,
			reviewed_sha, target_head_sha, trigger, state, provenance_json, requested_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		runID, 2, "services/loom-core", 1, "feat/x", "main", "sha-a", "", "telepathy", "ambiguous", "{}", "2026-07-25T16:00:00Z")
	if err == nil {
		t.Error("expected the trigger CHECK constraint to reject an unknown trigger")
	}
}

// UNIQUE(pipeline_run_id, seq) is what makes the ledger an append-only
// sequence rather than a set of rows that can collide under concurrency.
func TestMRHeadTransitions_SchemaEnforcesSeqUniqueness(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedHeadTransitionRun(t, st, "UNIQ")

	if _, err := st.MRHeadTransitions.Open(ctx, externalMovement(runID, "sha-a", "sha-b")); err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err := st.DB().ExecContext(ctx, `
		INSERT INTO mr_head_transitions (
			pipeline_run_id, seq, project, mr_iid, source_branch, target_branch,
			reviewed_sha, target_head_sha, trigger, state, provenance_json, requested_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		runID, 1, "services/loom-core", 1, "feat/x", "main", "sha-a", "", "external", "ambiguous", "{}", "2026-07-25T16:00:00Z")
	if err == nil {
		t.Error("expected UNIQUE(pipeline_run_id, seq) to reject a duplicate seq")
	}
}
