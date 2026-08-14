package store

// ClaimWorkflowStart is the S7 transactional start kernel for imperative
// workflow runs — the workflow-engine sibling of ClaimPipelineStart. One
// SQLite transaction: queued→running CAS on the backlog item, budget
// admission against the SHARED pipeline budget tier (spent + reserved across
// BOTH pipeline and imperative runs, so the two lanes cannot jointly
// oversubscribe the window), an imperative workflow_runs insert carrying the
// frozen template selection verbatim, a budget reservation, and the aggregate
// transition. It deliberately writes NO pending_dispatches row and NO
// pipeline_runs row: the imperative scheduler discovers running imperative
// runs directly (ListRunningImperativeRuns), so there is no dispatch intent
// to acknowledge and no DAG lane to drive.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// WorkflowStartKind labels the aggregate transition a committed imperative
// start appends, distinguishing it from PipelineStartDispatchKind in the
// shared pipeline_transitions journal.
const (
	WorkflowStartKind = "workflow_start"
	// WorkflowTerminalKind labels the settle transition a terminal imperative
	// run appends (item running->escalated).
	WorkflowTerminalKind = "workflow_terminal"
)

// ClaimWorkflowStartRequest carries one imperative start attempt.
type ClaimWorkflowStartRequest struct {
	BacklogID            string
	ExpectedClaimVersion int64
	ExpectedRevision     int64
	// Selection is the frozen template identity (engine must be imperative).
	// Stamped onto the run verbatim; the kernel never re-resolves it.
	Selection       WorkflowSelection
	EstimateUSD     float64
	ParentSessionID string
	// Limits is the same pipeline budget tier ClaimPipelineStart enforces —
	// imperative runs draw from the shared pool, not a private one.
	Limits PipelineStartLimits
	Now    time.Time
}

// ClaimWorkflowStartResult is the committed start boundary.
type ClaimWorkflowStartResult struct {
	Backlog     *BacklogItem
	Run         *WorkflowRun
	Reservation *PipelineBudgetReservation
}

func validateClaimWorkflowStartRequest(req ClaimWorkflowStartRequest) error {
	if req.BacklogID == "" {
		return fmt.Errorf("%w: backlog id required", ErrInvalidClaim)
	}
	if req.ExpectedClaimVersion < 0 || req.ExpectedRevision < 0 {
		return fmt.Errorf("%w: expected versions must be non-negative", ErrInvalidClaim)
	}
	sel := req.Selection
	if sel.Engine != WorkflowEngineImperative {
		return fmt.Errorf("%w: selection engine %q is not imperative", ErrInvalidClaim, sel.Engine)
	}
	if strings.TrimSpace(sel.Template) == "" || strings.TrimSpace(sel.TemplateVersion) == "" {
		return fmt.Errorf("%w: selection template and version required", ErrInvalidClaim)
	}
	if strings.TrimSpace(sel.InterpreterVersion) == "" {
		return fmt.Errorf("%w: selection interpreter version required", ErrInvalidClaim)
	}
	if strings.TrimSpace(sel.ParamsJSON) == "" {
		return fmt.Errorf("%w: selection params required", ErrInvalidClaim)
	}
	if req.EstimateUSD < 0 {
		return fmt.Errorf("%w: estimate must be non-negative", ErrInvalidClaim)
	}
	return nil
}

// ClaimWorkflowStart commits the complete imperative start boundary. A
// concurrent reconciler can lose only with ErrClaimConflict, never by leaving
// a half-created run or oversubscribing a checked cap.
func (s *Store) ClaimWorkflowStart(ctx context.Context, req ClaimWorkflowStartRequest) (*ClaimWorkflowStartResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("%w: store is nil", ErrInvalidClaim)
	}
	req.BacklogID = strings.TrimSpace(req.BacklogID)
	req.ParentSessionID = strings.TrimSpace(req.ParentSessionID)
	if err := validateClaimWorkflowStartRequest(req); err != nil {
		return nil, err
	}

	now := req.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("workflow start: generate run id: %w", err)
	}
	runID := "WF-" + req.BacklogID + "-" + id.String()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workflow start: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Same serialization discipline as the pipeline kernel: the CAS is the
	// first statement, so contenders order before any snapshot is observed.
	res, err := tx.ExecContext(ctx, `
		UPDATE backlog_items
		SET state = ?, claim_version = claim_version + 1,
			row_version = row_version + 1, updated_at = ?
		WHERE id = ? AND state = ? AND claim_version = ? AND row_version = ?
	`, string(BacklogRunning), timeRFC3339(now), req.BacklogID,
		string(BacklogQueued), req.ExpectedClaimVersion, req.ExpectedRevision)
	if err != nil {
		return nil, fmt.Errorf("workflow start: claim backlog %s: %w", req.BacklogID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("workflow start: claim rows affected: %w", err)
	}
	if rows != 1 {
		return nil, fmt.Errorf("%w: backlog=%s expected_claim_version=%d expected_revision=%d",
			ErrClaimConflict, req.BacklogID, req.ExpectedClaimVersion, req.ExpectedRevision)
	}

	item, err := scanBacklog(tx.QueryRowContext(ctx,
		`SELECT `+backlogColumns+` FROM backlog_items WHERE id = ?`, req.BacklogID))
	if err != nil {
		return nil, fmt.Errorf("workflow start: load claimed backlog: %w", err)
	}
	aggregateVersion := item.ClaimVersion

	snapshot, err := readWorkflowStartBudgetSnapshot(ctx, tx, now.Add(-24*time.Hour), req.Limits)
	if err != nil {
		return nil, err
	}
	if exceeded := evaluateWorkflowStartBudget(req, snapshot); exceeded != nil {
		return nil, exceeded
	}

	startedAt := now
	run := &WorkflowRun{
		ID:                 runID,
		BacklogID:          req.BacklogID,
		Engine:             WorkflowEngineImperative,
		Template:           req.Selection.Template,
		TemplateVersion:    req.Selection.TemplateVersion,
		InterpreterVersion: req.Selection.InterpreterVersion,
		WorkflowParams:     req.Selection.ParamsJSON,
		State:              WorkflowRunRunning,
		StartedAt:          &startedAt,
		ParentSessionID:    req.ParentSessionID,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_runs (
			id, backlog_id, engine, template, template_version,
			interpreter_version, workflow_params, state, started_at,
			cost_usd, parent_session_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
	`, run.ID, nullStr(run.BacklogID), string(run.Engine),
		run.Template, run.TemplateVersion, run.InterpreterVersion,
		nullStr(run.WorkflowParams), string(run.State),
		timeRFC3339(startedAt), nullStr(run.ParentSessionID)); err != nil {
		return nil, fmt.Errorf("workflow start: insert run %s: %w", run.ID, err)
	}

	reservationResult, err := tx.ExecContext(ctx, `
		INSERT INTO pipeline_budget_reservations (
			run_id, backlog_id, reserved_usd, state, created_at
		) VALUES (?, ?, ?, ?, ?)
	`, run.ID, run.BacklogID, req.EstimateUSD, reservationStateActive, timeRFC3339(now))
	if err != nil {
		return nil, fmt.Errorf("workflow start: reserve budget %s: %w", run.ID, err)
	}
	reservationID, err := reservationResult.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("workflow start: reservation id: %w", err)
	}
	reservation := &PipelineBudgetReservation{
		ID:          reservationID,
		RunID:       run.ID,
		BacklogID:   run.BacklogID,
		ReservedUSD: req.EstimateUSD,
		State:       reservationStateActive,
		CreatedAt:   now,
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pipeline_transitions (
			backlog_id, aggregate_version, run_id, kind,
			from_state, to_state, occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, run.BacklogID, aggregateVersion, run.ID, WorkflowStartKind,
		string(BacklogQueued), string(BacklogRunning), timeRFC3339(now)); err != nil {
		return nil, fmt.Errorf("workflow start: insert transition %s/%d: %w",
			run.BacklogID, aggregateVersion, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workflow start: commit %s: %w", run.ID, err)
	}
	return &ClaimWorkflowStartResult{Backlog: item, Run: run, Reservation: reservation}, nil
}

// readWorkflowStartBudgetSnapshot is the imperative claim's admission view of
// the SHARED pipeline budget tier: spent covers terminal-and-active pipeline
// runs AND imperative workflow runs; reservations cover both lanes (the
// reservation table is shared); run counts and concurrency cover both. It is
// a strict superset of the pipeline claim's view so an imperative start can
// never admit spend a pipeline start would have refused.
func readWorkflowStartBudgetSnapshot(
	ctx context.Context,
	tx *sql.Tx,
	since time.Time,
	limits PipelineStartLimits,
) (pipelineStartBudgetSnapshot, error) {
	var out pipelineStartBudgetSnapshot
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COALESCE((
				SELECT SUM(pr.cost_usd) FROM pipeline_runs pr WHERE pr.started_at >= ?
			), 0) + COALESCE((
				SELECT SUM(wr.cost_usd) FROM workflow_runs wr
				WHERE wr.engine = 'imperative' AND wr.started_at >= ?
			), 0),
			COALESCE((
				SELECT SUM(MAX(r.reserved_usd - COALESCE(pr.cost_usd, wr.cost_usd, 0), 0))
				FROM pipeline_budget_reservations r
				LEFT JOIN pipeline_runs pr ON pr.id = r.run_id
				LEFT JOIN workflow_runs wr ON wr.id = r.run_id AND wr.engine = 'imperative'
				WHERE r.state = 'active' AND r.created_at >= ?
			), 0),
			COALESCE((
				SELECT COUNT(*) FROM pipeline_runs pr WHERE pr.started_at >= ?
			), 0) + COALESCE((
				SELECT COUNT(*) FROM workflow_runs wr
				WHERE wr.engine = 'imperative' AND wr.started_at >= ?
			), 0),
			COALESCE((
				SELECT COUNT(*) FROM pipeline_runs pr
				WHERE pr.state IN ('queued', 'running', 'paused')
			), 0) + COALESCE((
				SELECT COUNT(*) FROM workflow_runs wr
				WHERE wr.engine = 'imperative' AND wr.state IN ('running', 'paused')
			), 0)
	`, timeRFC3339(since), timeRFC3339(since), timeRFC3339(since),
		timeRFC3339(since), timeRFC3339(since)).Scan(
		&out.spentUSD, &out.reservedUSD, &out.runs, &out.activeRuns,
	); err != nil {
		return pipelineStartBudgetSnapshot{}, fmt.Errorf("workflow start: budget snapshot: %w", err)
	}
	return out, nil
}

func evaluateWorkflowStartBudget(req ClaimWorkflowStartRequest, snap pipelineStartBudgetSnapshot) *BudgetExceededError {
	var reasons []string
	if max := req.Limits.MaxUSDPerRun; max > 0 && req.EstimateUSD > max {
		reasons = append(reasons, fmt.Sprintf("estimate %.2f exceeds max_usd_per_run %.2f", req.EstimateUSD, max))
	}
	if max := req.Limits.MaxUSDPerDay; max > 0 && snap.spentUSD+snap.reservedUSD+req.EstimateUSD > max {
		reasons = append(reasons, fmt.Sprintf(
			"daily USD cap %.2f reached: spent %.2f + reserved %.2f + estimate %.2f",
			max, snap.spentUSD, snap.reservedUSD, req.EstimateUSD))
	}
	if max := req.Limits.MaxRunsPerDay; max > 0 && snap.runs >= max {
		reasons = append(reasons, fmt.Sprintf("daily run count %d reached cap %d", snap.runs, max))
	}
	if max := req.Limits.MaxConcurrentRuns; max > 0 && snap.activeRuns >= max {
		reasons = append(reasons, fmt.Sprintf("active runs %d reached cap %d", snap.activeRuns, max))
	}
	if len(reasons) == 0 {
		return nil
	}
	return &BudgetExceededError{
		Reasons:     reasons,
		SpentUSD:    snap.spentUSD,
		ReservedUSD: snap.reservedUSD,
		Runs:        snap.runs,
		ActiveRuns:  snap.activeRuns,
		Limits:      req.Limits,
	}
}
