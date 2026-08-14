package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// PipelineDAO exposes CRUD against pipeline_runs, stage_results, gate_outcomes.
type PipelineDAO struct {
	db                       *sql.DB
	terminalConflictRecorder terminalConflictRecorder
}

type terminalConflictRecorder interface {
	RecordTerminalStateConflict(requestedState string)
}

// ErrPipelineProjectUnavailable marks a run whose successful MR lifecycle
// stages do not contain one consistent durable project. Callers must not fall
// back to mutable backlog routing for MR-IID-scoped operations.
var ErrPipelineProjectUnavailable = errors.New("pipeline: durable project unavailable")

const pipelineColumns = `id, backlog_id, aggregate_version, row_version, template, state, current_stage, attempts,
			worktree_path, mr_iid, started_at, ended_at, cost_usd, parent_session_id,
			parent_run_id, depth, escalation_class, escalation_failure_class,
			escalation_external_dependency_id, escalation_external_dependency,
			escalation_retryable, retry_exhausted`

type pipelineRunQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// PutRun inserts or replaces a pipeline run. Terminal transitions also release
// the run's active budget reservation and synchronize its DAG workflow metadata
// in the same transaction, so subsequent admissions immediately see the slot
// and reserved spend as available.
func (d *PipelineDAO) PutRun(ctx context.Context, run *PipelineRun) error {
	if isPipelineTerminal(run) {
		return d.putTerminalRun(ctx, run)
	}
	return putPipelineRun(ctx, d.db, run)
}

// PauseRunWithBacklog atomically parks a running pipeline and its backlog item.
// PutRun's terminal protection still applies; the backlog transition is part
// of the same transaction so a stale backlog claim cannot leave split state.
func (d *PipelineDAO) PauseRunWithBacklog(ctx context.Context, run *PipelineRun, backlogFrom BacklogState, now time.Time) error {
	if run == nil || run.ID == "" {
		return errors.New("pipeline: run.ID required")
	}
	if backlogFrom == "" {
		return errors.New("pipeline pause: backlog from state required")
	}
	paused := *run
	paused.State = PipelinePaused
	paused.EndedAt = &now
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pipeline pause %s begin: %w", run.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := putTerminalRunTx(ctx, tx, &paused); err != nil {
		return err
	}
	if _, err := transitionBacklogStateTx(ctx, tx, run.BacklogID, run.AggregateVersion, backlogFrom, BacklogPaused); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pipeline pause %s commit: %w", run.ID, err)
	}
	run.State = paused.State
	run.EndedAt = paused.EndedAt
	run.Revision = paused.Revision
	return nil
}

// ResumePausedRun atomically reopens a paused run. PutRun deliberately never
// permits a terminal row to be overwritten; this narrow transition is the one
// operator-authorised exception and remains revision-CAS protected.
func (d *PipelineDAO) ResumePausedRun(ctx context.Context, run *PipelineRun) error {
	return d.ResumePausedRunWithBacklog(ctx, run, "")
}

// ResumePausedRunWithBacklog atomically reopens a paused run and queues its
// backlog item exactly once.
func (d *PipelineDAO) ResumePausedRunWithBacklog(ctx context.Context, run *PipelineRun, backlogFrom BacklogState) error {
	if run == nil || run.ID == "" {
		return errors.New("pipeline: run.ID required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pipeline resume %s begin: %w", run.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE pipeline_runs
		SET state = 'queued', ended_at = NULL, row_version = row_version + 1
		WHERE id = ? AND aggregate_version = ? AND row_version = ? AND state = 'paused'`,
		run.ID, run.AggregateVersion, run.Revision)
	if err != nil {
		return fmt.Errorf("pipeline resume %s: %w", run.ID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("pipeline resume %s rows: %w", run.ID, err)
	}
	if n != 1 {
		return &StaleWriteError{Entity: "pipeline run", ID: run.ID, ExpectedRevision: run.Revision, Reason: "aggregate version, row revision, or paused state changed"}
	}
	if backlogFrom != "" {
		if _, err := transitionBacklogStateTx(ctx, tx, run.BacklogID, run.AggregateVersion, backlogFrom, BacklogQueued); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs
		SET state = ?, paused_at = NULL, resumed_at = ?, ended_at = NULL
		WHERE id = ? AND engine = ?
	`, string(WorkflowRunRunning), timeRFC3339(now), run.ID, string(WorkflowEngineDAG)); err != nil {
		return fmt.Errorf("pipeline resume sync workflow %s: %w", run.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pipeline resume %s commit: %w", run.ID, err)
	}
	run.State = PipelineQueued
	run.EndedAt = nil
	run.Revision++
	return nil
}

func putPipelineRun(ctx context.Context, queryer pipelineRunQueryer, run *PipelineRun) error {
	if err := validatePipelineRun(run); err != nil {
		return err
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	var (
		mrIID                     sql.NullInt64
		endedAt                   sql.NullString
		parentRun                 sql.NullString
		retryable, retryExhausted sql.NullBool
	)
	if run.MRIID != nil {
		mrIID = sql.NullInt64{Int64: *run.MRIID, Valid: true}
	}
	if run.EndedAt != nil {
		endedAt = sql.NullString{String: timeRFC3339(*run.EndedAt), Valid: true}
	}
	if run.ParentRunID != nil && *run.ParentRunID != "" {
		parentRun = sql.NullString{String: *run.ParentRunID, Valid: true}
	}
	if run.EscalationRetryable != nil {
		retryable = sql.NullBool{Bool: *run.EscalationRetryable, Valid: true}
	}
	if run.RetryExhausted != nil {
		retryExhausted = sql.NullBool{Bool: *run.RetryExhausted, Valid: true}
	}
	var storedRevision int64
	var storedEndedAt sql.NullString
	err := queryer.QueryRowContext(ctx, `
			INSERT INTO pipeline_runs (`+pipelineColumns+`)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET
				backlog_id        = excluded.backlog_id,
				template          = excluded.template,
				state             = excluded.state,
				current_stage     = excluded.current_stage,
				attempts          = excluded.attempts,
				worktree_path     = excluded.worktree_path,
				mr_iid            = excluded.mr_iid,
				ended_at          = CASE
					WHEN pipeline_runs.state IN ('done', 'escalated', 'paused')
						THEN COALESCE(pipeline_runs.ended_at, excluded.ended_at)
					ELSE excluded.ended_at
				END,
				cost_usd          = excluded.cost_usd,
				parent_session_id = excluded.parent_session_id,
				parent_run_id     = excluded.parent_run_id,
				depth             = excluded.depth,
				escalation_class  = excluded.escalation_class,
				escalation_failure_class = excluded.escalation_failure_class,
				escalation_external_dependency_id = excluded.escalation_external_dependency_id,
				escalation_external_dependency = excluded.escalation_external_dependency,
				escalation_retryable = excluded.escalation_retryable,
				retry_exhausted     = excluded.retry_exhausted,
				row_version       = pipeline_runs.row_version + 1
			WHERE excluded.aggregate_version = pipeline_runs.aggregate_version
				AND pipeline_runs.row_version = ?
				AND (pipeline_runs.state NOT IN ('done', 'escalated', 'paused')
					OR excluded.state = pipeline_runs.state)
			RETURNING row_version, ended_at
		`,
		run.ID, run.BacklogID, run.AggregateVersion, int64(1), run.Template, string(run.State),
		nullStr(run.CurrentStage), run.Attempts, nullStr(run.WorktreePath), mrIID,
		timeRFC3339(run.StartedAt), endedAt, run.CostUSD, nullStr(run.ParentSessionID),
		parentRun, run.Depth, nullStr(run.EscalationClass), nullStr(run.FailureClass),
		nullStr(run.ExternalDependencyID), nullStr(run.ExternalDependency), retryable, retryExhausted,
		run.Revision,
	).Scan(&storedRevision, &storedEndedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return &StaleWriteError{
			Entity:           "pipeline run",
			ID:               run.ID,
			ExpectedRevision: run.Revision,
			Reason:           "aggregate version, row revision, or terminal state changed",
		}
	}
	if err != nil {
		return fmt.Errorf("pipeline put %s: %w", run.ID, err)
	}
	persistedEndedAt, err := nullableTime(storedEndedAt)
	if err != nil {
		return fmt.Errorf("pipeline put %s ended_at: %w", run.ID, err)
	}
	run.Revision = storedRevision
	run.EndedAt = persistedEndedAt
	return nil
}

func validatePipelineRun(run *PipelineRun) error {
	if run == nil || run.ID == "" {
		return errors.New("pipeline: run.ID required")
	}
	if run.BacklogID == "" {
		return errors.New("pipeline: run.BacklogID required")
	}
	if run.Depth < 0 {
		return errors.New("pipeline: run.Depth must be >= 0")
	}
	if run.AggregateVersion < 0 {
		return errors.New("pipeline: run.AggregateVersion must be >= 0")
	}
	if run.Revision < 0 {
		return errors.New("pipeline: run.Revision must be >= 0")
	}
	if run.Revision == math.MaxInt64 {
		return errors.New("pipeline: run.Revision must be < max int64")
	}
	if !isFiniteNonNegative(run.CostUSD) {
		return errors.New("pipeline: run.CostUSD must be finite and >= 0")
	}
	return nil
}

func isFiniteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func isPipelineTerminal(run *PipelineRun) bool {
	if run == nil {
		return false
	}
	return IsPipelineTerminalState(run.State)
}

func (d *PipelineDAO) putTerminalRun(ctx context.Context, run *PipelineRun) error {
	terminalRun := *run
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pipeline terminal begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := putTerminalRunTx(ctx, tx, &terminalRun); err != nil {
		if errors.Is(err, ErrStaleWrite) &&
			d.terminalConflictRecorder != nil &&
			hasIncompatibleTerminalState(ctx, tx, terminalRun.ID, terminalRun.State) {
			d.terminalConflictRecorder.RecordTerminalStateConflict(string(terminalRun.State))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pipeline terminal commit %s: %w", terminalRun.ID, err)
	}
	run.StartedAt = terminalRun.StartedAt
	run.EndedAt = terminalRun.EndedAt
	run.Revision = terminalRun.Revision
	return nil
}

func hasIncompatibleTerminalState(ctx context.Context, tx *sql.Tx, runID string, requested PipelineState) bool {
	if requested != PipelineDone && requested != PipelineEscalated {
		return false
	}
	var stored PipelineState
	if err := tx.QueryRowContext(ctx, `SELECT state FROM pipeline_runs WHERE id = ?`, runID).Scan(&stored); err != nil {
		return false
	}
	return (stored == PipelineDone || stored == PipelineEscalated) && stored != requested
}

func putTerminalRunTx(ctx context.Context, tx *sql.Tx, run *PipelineRun) error {
	now := time.Now().UTC()
	if run.State != PipelinePaused && run.EndedAt == nil {
		run.EndedAt = &now
	}
	if err := putPipelineRun(ctx, tx, run); err != nil {
		return err
	}
	if run.EndedAt != nil {
		now = run.EndedAt.UTC()
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE pipeline_budget_reservations
		SET state = 'released', released_at = ?
		WHERE run_id = ? AND state = 'active'
	`, timeRFC3339(now), run.ID); err != nil {
		return fmt.Errorf("pipeline terminal release reservation %s: %w", run.ID, err)
	}

	workflowState := WorkflowRunError
	switch run.State {
	case PipelineDone:
		workflowState = WorkflowRunDone
	case PipelineEscalated:
		workflowState = WorkflowRunEscalated
	case PipelinePaused:
		workflowState = WorkflowRunPaused
	}
	if run.State == PipelinePaused {
		if _, err := tx.ExecContext(ctx, `
			UPDATE workflow_runs
			SET state = ?, paused_at = ?, cost_usd = ?
			WHERE id = ? AND engine = ?
		`, string(workflowState), timeRFC3339(now), run.CostUSD, run.ID, string(WorkflowEngineDAG)); err != nil {
			return fmt.Errorf("pipeline terminal sync workflow %s: %w", run.ID, err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs
		SET state = ?, ended_at = ?, cost_usd = ?
		WHERE id = ? AND engine = ?
	`, string(workflowState), timeRFC3339(now), run.CostUSD, run.ID, string(WorkflowEngineDAG)); err != nil {
		return fmt.Errorf("pipeline terminal sync workflow %s: %w", run.ID, err)
	}
	return nil
}

func transitionBacklogStateTx(ctx context.Context, tx *sql.Tx, id string, expectedClaimVersion int64, from BacklogState, to BacklogState) (*BacklogItem, error) {
	if id == "" {
		return nil, errors.New("backlog transition: id required")
	}
	if expectedClaimVersion < 0 {
		return nil, errors.New("backlog transition: expected claim version must be >= 0")
	}
	if from == "" || to == "" {
		return nil, errors.New("backlog transition: from and to states required")
	}
	now := time.Now().UTC()
	item, err := scanBacklog(tx.QueryRowContext(ctx, `
		UPDATE backlog_items
		SET state = ?, row_version = row_version + 1, updated_at = ?
		WHERE id = ? AND claim_version = ? AND state = ? AND state <> ?
			AND row_version < ?
		RETURNING `+backlogColumns+`
	`, string(to), timeRFC3339(now), id, expectedClaimVersion, string(from), string(to), int64(math.MaxInt64)))
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("backlog transition %s: %w", id, err)
	}
	current, err := scanBacklog(tx.QueryRowContext(ctx,
		`SELECT `+backlogColumns+` FROM backlog_items WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if current.ClaimVersion == expectedClaimVersion && current.State == to {
		return current, nil
	}
	return nil, &StaleWriteError{
		Entity:           "backlog aggregate",
		ID:               id,
		ExpectedRevision: expectedClaimVersion,
		Reason: fmt.Sprintf("state=%s claim_version=%d; expected state=%s claim_version=%d",
			current.State, current.ClaimVersion, from, expectedClaimVersion),
	}
}

// CreateSubrun inserts one new pipeline_runs row for a v2 recursion
// subrun. The caller (pkg/mills/pipeline/recursion.SubrunGuard) is
// responsible for the depth/budget/cycle guards and for filling
// run.ParentRunID + run.Depth before this call. CreateSubrun adds an
// existence check on parent_run_id (so a misuse can't silently dangle)
// and fails if the row id already exists (subruns are insert-only;
// PutRun is the upsert-friendly path for ongoing rollups).
func (d *PipelineDAO) CreateSubrun(ctx context.Context, run *PipelineRun) error {
	if run == nil || run.ID == "" {
		return errors.New("pipeline: subrun.ID required")
	}
	if err := validatePipelineRun(run); err != nil {
		return err
	}
	if run.ParentRunID == nil || *run.ParentRunID == "" {
		return errors.New("pipeline: subrun.ParentRunID required")
	}
	if run.Depth <= 0 {
		return fmt.Errorf("pipeline: subrun.Depth must be > 0 (got %d)", run.Depth)
	}
	// Defensive existence check on the parent — prevents a
	// dangling subrun row when the caller forgets to verify
	// upstream. The recursion guard already does this lookup, but
	// the DAO can be invoked directly from tests / future callers.
	row := d.db.QueryRowContext(ctx,
		`SELECT 1 FROM pipeline_runs WHERE id = ?`, *run.ParentRunID)
	var got int
	if err := row.Scan(&got); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("pipeline: subrun parent %q does not exist", *run.ParentRunID)
		}
		return fmt.Errorf("pipeline: subrun parent lookup: %w", err)
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	parentRun := sql.NullString{String: *run.ParentRunID, Valid: true}
	_, err := d.db.ExecContext(ctx, `
			INSERT INTO pipeline_runs (`+pipelineColumns+`)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		`,
		run.ID, run.BacklogID, run.AggregateVersion, int64(1), run.Template, string(run.State),
		nullStr(run.CurrentStage), run.Attempts, nullStr(run.WorktreePath), sql.NullInt64{},
		timeRFC3339(run.StartedAt), sql.NullString{}, run.CostUSD, nullStr(run.ParentSessionID),
		parentRun, run.Depth, nullStr(run.EscalationClass), nullStr(run.FailureClass),
		nullStr(run.ExternalDependencyID), nullStr(run.ExternalDependency), sql.NullBool{}, sql.NullBool{},
	)
	if err != nil {
		return fmt.Errorf("pipeline create-subrun %s: %w", run.ID, err)
	}
	run.Revision = 1
	return nil
}

// ListQueuedSubruns returns every pipeline run that's still in
// `queued` state AND has a non-null parent_run_id AND has not yet
// been picked up by a worker (attempts = 0). The reconciler uses
// this on each tick (Phase 6 slice 6.2) to start subruns the way it
// starts queued backlog items. Ordered by started_at so the oldest
// subrun runs first; the (state, parent_run_id) predicate naturally
// excludes non-recursive runs.
func (d *PipelineDAO) ListQueuedSubruns(ctx context.Context) ([]*PipelineRun, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT `+pipelineColumns+` FROM pipeline_runs
		WHERE state = ? AND parent_run_id IS NOT NULL AND attempts = 0
		ORDER BY started_at ASC
	`, string(PipelineQueued))
	if err != nil {
		return nil, fmt.Errorf("pipeline list-queued-subruns: %w", err)
	}
	defer rows.Close()
	var out []*PipelineRun
	for rows.Next() {
		r, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListSubruns returns every direct child of the given parent pipeline run,
// ordered by started_at. Empty result is not an error. v2 recursion path.
func (d *PipelineDAO) ListSubruns(ctx context.Context, parentRunID string) ([]*PipelineRun, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+pipelineColumns+` FROM pipeline_runs WHERE parent_run_id = ? ORDER BY started_at ASC`,
		parentRunID)
	if err != nil {
		return nil, fmt.Errorf("pipeline list-subruns: %w", err)
	}
	defer rows.Close()
	var out []*PipelineRun
	for rows.Next() {
		r, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRun fetches one pipeline run by id.
func (d *PipelineDAO) GetRun(ctx context.Context, id string) (*PipelineRun, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+pipelineColumns+` FROM pipeline_runs WHERE id = ?`, id)
	return scanPipelineRun(row)
}

// ListByState returns pipeline runs in the given state, oldest-first.
func (d *PipelineDAO) ListByState(ctx context.Context, state PipelineState) ([]*PipelineRun, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+pipelineColumns+` FROM pipeline_runs WHERE state = ? ORDER BY started_at ASC`,
		string(state))
	if err != nil {
		return nil, fmt.Errorf("pipeline list: %w", err)
	}
	defer rows.Close()
	var out []*PipelineRun
	for rows.Next() {
		r, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListByStateSince returns pipeline runs in the given state with
// started_at on-or-after `since`, oldest-first. Used by the KPI
// writer to compute window-bounded aggregates (e.g. slice→merge
// duration p50) without pulling the full table.
func (d *PipelineDAO) ListByStateSince(ctx context.Context, state PipelineState, since time.Time) ([]*PipelineRun, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+pipelineColumns+` FROM pipeline_runs
		WHERE state = ? AND started_at >= ?
		ORDER BY started_at ASC`,
		string(state), timeRFC3339(since))
	if err != nil {
		return nil, fmt.Errorf("pipeline list-since: %w", err)
	}
	defer rows.Close()
	var out []*PipelineRun
	for rows.Next() {
		r, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListInFlight returns already-started, non-terminal pipeline runs. In addition
// to rows past queued, it includes top-level queued runs whose durable start
// intent was delivered and whose backlog still names the same running aggregate:
// a process may crash after acknowledging dispatch but before the runner
// persists its first state transition. Undelivered or superseded queued roots
// stay out of recovery, and queued subruns keep their dedicated path.
func (d *PipelineDAO) ListInFlight(ctx context.Context) ([]*PipelineRun, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+pipelineColumns+` FROM pipeline_runs
		WHERE state NOT IN ('done', 'escalated', 'paused')
		  AND (
			state <> 'queued'
			OR (
				parent_run_id IS NULL AND attempts > 0
				AND EXISTS (
					SELECT 1 FROM pending_dispatches pd
					WHERE pd.run_id = pipeline_runs.id AND pd.status = 'delivered'
				)
				AND EXISTS (
					SELECT 1 FROM backlog_items bi
					WHERE bi.id = pipeline_runs.backlog_id
					  AND bi.state = 'running'
					  AND bi.claim_version = pipeline_runs.aggregate_version
				)
			)
		  )
		ORDER BY started_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("pipeline list in-flight: %w", err)
	}
	defer rows.Close()
	var out []*PipelineRun
	for rows.Next() {
		r, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestMergedAt returns the EndedAt of the most recently merged
// (state=done) pipeline run, or nil when none has ever merged. Unlike
// the windowed KPI counts this is an all-time lookup: it answers "when
// did autonomy last successfully merge anything?" — the exact signal the
// Overview health banner needs when zero runs merged in the last 24h.
// The active-only pipeline-run list the HUD polls can never surface a
// terminal merged run, so the frontend cannot derive this itself.
func (d *PipelineDAO) LatestMergedAt(ctx context.Context) (*time.Time, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT ended_at FROM pipeline_runs
		WHERE state = ? AND ended_at IS NOT NULL AND ended_at != ''
		ORDER BY ended_at DESC
		LIMIT 1
	`, string(PipelineDone))
	var endedAt sql.NullString
	if err := row.Scan(&endedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pipeline latest-merged-at: %w", err)
	}
	return nullableTime(endedAt)
}

// SumCostSince totals pipeline spend since the given timestamp.
func (d *PipelineDAO) SumCostSince(ctx context.Context, since time.Time) (float64, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM pipeline_runs WHERE started_at >= ?`,
		timeRFC3339(since))
	var total float64
	if err := row.Scan(&total); err != nil {
		return 0, fmt.Errorf("pipeline sum-cost: %w", err)
	}
	return total, nil
}

// CountSince returns the number of pipeline runs started at-or-after `since`.
// This is the raw total (every row) — used by the KPI writer for the "runs in
// the last 24h" telemetry. The pipeline budget cap uses CountBudgetedSince
// instead, which discounts no-op capacity escalations.
func (d *PipelineDAO) CountSince(ctx context.Context, since time.Time) (int, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipeline_runs WHERE started_at >= ?`,
		timeRFC3339(since))
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("pipeline count-since: %w", err)
	}
	return n, nil
}

// escalationClassNoWorkQuota is the escalation_class value stamped on a run
// that escalated at its spawn call because the HUD spawn pool was saturated
// (or an upstream returned a rate-limit/quota response) — pipeline.ErrorClass
// "transient_quota". Such a run never acquired a worker: cost $0, zero stage
// progress. Kept in sync with pkg/mills/pipeline.ClassTransientQuota (the
// runner stamps string(cls) via SetEscalationClass); the two live in separate
// packages so the literal is duplicated with this pointer.
const escalationClassNoWorkQuota = "transient_quota"

// escalationClassTerminalConfig is the escalation_class stamped on a run that
// escalated on a terminal config verdict — a gate or stage failure that no
// retry can change (e.g. the scope gate's "no slices" fail, an unmergeable MR
// config). Kept in sync with pkg/mills/pipeline.ClassConfig; see
// escalationClassNoWorkQuota for why the literal is duplicated.
const escalationClassTerminalConfig = "config"

// CountBudgetedSince returns the number of pipeline runs started at-or-after
// `since` that should count toward the pipeline tier's MaxRunsPerDay cap.
//
// It excludes escalations that cannot recur without human action and whose
// counting only starves legitimate work:
//
//   - No-op capacity escalations: state='escalated', cost_usd = 0, class
//     transient_quota — a run that never got a worker because the spawn pool
//     was saturated / an upstream was rate-limited. Counting those exhausted
//     the day's run budget during the 2026-07-02 spawn-pool wedge (18/20 runs
//     were ~9ms no-op escalations), starving ready items for hours until the
//     rows aged out of the rolling window.
//
//   - Terminal config escalations: state='escalated', class config — the
//     runner escalated on first sight of a verdict no retry can change (gate
//     fail marked Terminal, unmergeable MR config). The item parks as
//     escalated and the auto-retry path never touches terminal classes, so
//     one item can contribute at most one such run per human requeue — there
//     is no loop for the cap to protect against. Counting them turned the
//     2026-07-07 bootstrapped-plan scope-gate storm (7 slice-less items +
//     sibling failures = 19 escalated runs) into a full-day 20/20 run-cap
//     freeze that 409'd the operator's own recovery hand-off. No cost guard
//     here: these runs often did real (discarded) implement work, and the
//     dollar cap independently bounds spend.
//
// Every other terminal fault (code, infra, generic transient) still counts,
// so the cap stays protective against real runaway loops. The dollar cap
// (MaxUSDPerDay) is unaffected by any of this.
func (d *PipelineDAO) CountBudgetedSince(ctx context.Context, since time.Time) (int, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pipeline_runs
		WHERE started_at >= ?
		  AND NOT (
		      state = 'escalated'
		      AND (
		          (cost_usd = 0 AND COALESCE(escalation_class, '') = ?)
		          OR COALESCE(escalation_class, '') = ?
		      )
		  )
	`, timeRFC3339(since), escalationClassNoWorkQuota, escalationClassTerminalConfig)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("pipeline count-budgeted-since: %w", err)
	}
	return n, nil
}

// CountEscalationsByClassSince returns the number of escalated pipeline runs
// started at-or-after `since`, grouped by their terminal fault class
// (pipeline_runs.escalation_class). Runs escalated without a class marker
// (NULL/empty — gate fail, cross-repo, drive failure) are bucketed under
// "unclassified", so the counts sum to the total escalated-run count and stay
// consistent with the mills_pipeline_escalation_class_total metric's
// classified/unclassified split. The result is a never-nil (possibly empty)
// map so callers can range or JSON-marshal it without a nil check — it feeds
// the KPI snapshot's escalations_by_class breakdown surfaced in the HUD.
func (d *PipelineDAO) CountEscalationsByClassSince(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(escalation_class, ''), 'unclassified') AS class, COUNT(*)
		FROM pipeline_runs
		WHERE state = 'escalated' AND started_at >= ?
		GROUP BY class
	`, timeRFC3339(since))
	if err != nil {
		return nil, fmt.Errorf("pipeline count-escalations-by-class: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var class string
		var n int
		if err := rows.Scan(&class, &n); err != nil {
			return nil, fmt.Errorf("pipeline count-escalations-by-class scan: %w", err)
		}
		out[class] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline count-escalations-by-class rows: %w", err)
	}
	return out, nil
}

// ListRecentClassifiedCIFailures returns recent escalated ci_watch failures
// that carry classification metadata. It is intentionally a compact summary
// projection, not a replacement for ListRecentTerminal: council briefs and HUD
// summaries need to show the failure class and external dependency without
// rehydrating stage logs or escalation issues.
func (d *PipelineDAO) ListRecentClassifiedCIFailures(ctx context.Context, since time.Time, limit int) ([]*ClassifiedCIFailureSummary, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	args := []any{limit}
	whereSince := ""
	if !since.IsZero() {
		whereSince = " AND pr.started_at >= ?"
		args = []any{timeRFC3339(since), limit}
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT pr.id, pr.backlog_id, COALESCE(b.title, ''), pr.started_at,
		       COALESCE(pr.escalation_class, ''),
		       COALESCE(pr.escalation_failure_class, ''),
		       COALESCE(pr.escalation_external_dependency_id, ''),
		       COALESCE(pr.escalation_external_dependency, ''),
		       pr.escalation_retryable
		FROM pipeline_runs pr
		LEFT JOIN backlog_items b ON b.id = pr.backlog_id
		WHERE pr.state = 'escalated'
		  AND (
		      pr.current_stage = 'ci_watch'
		      OR EXISTS (
		          SELECT 1 FROM stage_results sr
		          WHERE sr.pipeline_run_id = pr.id AND sr.stage = 'ci_watch'
		      )
		  )
		  AND (
		      COALESCE(pr.escalation_class, '') != ''
		      OR COALESCE(pr.escalation_failure_class, '') != ''
		      OR COALESCE(pr.escalation_external_dependency_id, '') != ''
		      OR pr.escalation_retryable IS NOT NULL
		  )`+whereSince+`
		ORDER BY pr.started_at DESC
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("pipeline list-recent-classified-ci-failures: %w", err)
	}
	defer rows.Close()
	var out []*ClassifiedCIFailureSummary
	for rows.Next() {
		var (
			summary   ClassifiedCIFailureSummary
			startedAt string
			retryable sql.NullBool
		)
		if err := rows.Scan(
			&summary.RunID, &summary.BacklogID, &summary.BacklogTitle, &startedAt,
			&summary.EscalationClass, &summary.FailureClass,
			&summary.ExternalDependencyID, &summary.ExternalDependency, &retryable,
		); err != nil {
			return nil, fmt.Errorf("pipeline list-recent-classified-ci-failures scan: %w", err)
		}
		parsed, err := parseTime(startedAt)
		if err != nil {
			return nil, fmt.Errorf("pipeline list-recent-classified-ci-failures started_at: %w", err)
		}
		summary.StartedAt = parsed
		if retryable.Valid {
			summary.Retryable = &retryable.Bool
		}
		summary.applyClassificationSemantics()
		out = append(out, &summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline list-recent-classified-ci-failures rows: %w", err)
	}
	return out, nil
}

// ListEscalationEvidence returns the free-text failure evidence of the runs
// escalated since a cutoff, newest first. Evidence is the run's last non-empty
// stage log tail — the escalation event payload carries only the short reason,
// so the log tail is the only durable place the raw error text survives.
//
// It deliberately returns BOTH classified and unclassified rows, flagged: a
// signature miner clusters the unclassified ones but must score a proposed
// phrase against the whole window, and a phrase that also matches classified
// failures is a phrase that would over-fire once promoted.
//
// The classified predicate treats all four escalation markers as one group,
// mirroring ListRecentClassifiedCIFailures: legacy runs may carry
// escalation_class with escalation_failure_class still NULL, and a run that is
// classified by any marker is not an unexplained failure.
func (d *PipelineDAO) ListEscalationEvidence(ctx context.Context, since time.Time, limit int) ([]*EscalationEvidence, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	args := []any{}
	whereSince := ""
	if !since.IsZero() {
		whereSince = " AND pr.started_at >= ?"
		args = append(args, timeRFC3339(since))
	}
	args = append(args, limit)
	rows, err := d.db.QueryContext(ctx, `
		SELECT pr.id, pr.backlog_id, pr.started_at,
		       COALESCE(sr.stage, ''), COALESCE(sr.log_tail, ''),
		       CASE WHEN COALESCE(pr.escalation_class, '') != ''
		              OR COALESCE(pr.escalation_failure_class, '') != ''
		              OR COALESCE(pr.escalation_external_dependency_id, '') != ''
		              OR COALESCE(pr.escalation_external_dependency, '') != ''
		              OR pr.escalation_retryable IS NOT NULL
		            THEN 1 ELSE 0 END
		FROM pipeline_runs pr
		LEFT JOIN stage_results sr ON sr.id = (
		    SELECT s.id FROM stage_results s
		    WHERE s.pipeline_run_id = pr.id
		      AND s.log_tail IS NOT NULL AND s.log_tail != ''
		    ORDER BY s.started_at DESC, s.attempt DESC, s.id DESC
		    LIMIT 1
		)
		WHERE pr.state = 'escalated'`+whereSince+`
		ORDER BY pr.started_at DESC, pr.id DESC
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("pipeline list-escalation-evidence: %w", err)
	}
	defer rows.Close()
	var out []*EscalationEvidence
	for rows.Next() {
		var (
			rec        EscalationEvidence
			startedAt  string
			classified int
		)
		if err := rows.Scan(&rec.RunID, &rec.BacklogID, &startedAt, &rec.Stage, &rec.Evidence, &classified); err != nil {
			return nil, fmt.Errorf("pipeline list-escalation-evidence scan: %w", err)
		}
		parsed, err := parseTime(startedAt)
		if err != nil {
			return nil, fmt.Errorf("pipeline list-escalation-evidence started_at: %w", err)
		}
		rec.StartedAt = parsed
		rec.Classified = classified == 1
		out = append(out, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline list-escalation-evidence rows: %w", err)
	}
	return out, nil
}

const classifiedCIFailureClassifier = "mills-failure-classifier"

func (s *ClassifiedCIFailureSummary) applyClassificationSemantics() {
	if s == nil {
		return
	}
	class := normalizedFailureClass(s.FailureClass)
	if class == "" {
		return
	}
	s.Classifier = classifiedCIFailureClassifier
	freeRetry := class == "transient" || class == "transient_quota"
	terminal := class == "configuration"
	s.FreeRetry = &freeRetry
	s.Terminal = &terminal
}

func normalizedFailureClass(class string) string {
	switch class {
	case "transient", "transient_quota", "infrastructure", "code", "configuration":
		return class
	case "":
		return ""
	default:
		return "code"
	}
}

// SetEscalationClass stamps the terminal fault class (an ErrorClass string,
// e.g. "transient_quota", "code", "infra") on an escalated pipeline run. The
// runner calls it best-effort right after transitioning a run to escalated so
// CountBudgetedSince can discount no-op capacity escalations. Passing an empty
// class is a no-op (the column stays NULL → the run counts). Returns
// ErrNotFound when the run id does not exist.
func (d *PipelineDAO) SetEscalationClass(ctx context.Context, runID, class string) error {
	if runID == "" {
		return errors.New("pipeline set-escalation-class: run id required")
	}
	if class == "" {
		return nil
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE pipeline_runs SET escalation_class = ? WHERE id = ?`,
		class, runID,
	)
	if err != nil {
		return fmt.Errorf("pipeline set-escalation-class: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// EscalationMetadata is the persisted classification payload for an escalated
// pipeline run. Empty string fields leave existing values unchanged. Retryable
// nil leaves escalation_retryable unchanged so partial updates do not erase an
// explicit retry policy verdict.
type EscalationMetadata struct {
	EscalationClass      string
	FailureClass         string
	ExternalDependencyID string
	ExternalDependency   string
	Retryable            *bool
	RetryExhausted       *bool
}

// SetEscalationMetadata stamps the full terminal classification payload on an
// escalated pipeline run. It subsumes SetEscalationClass for new callers while
// preserving the older method for budget-accounting tests and compatibility.
func (d *PipelineDAO) SetEscalationMetadata(ctx context.Context, runID string, md EscalationMetadata) error {
	if runID == "" {
		return errors.New("pipeline set-escalation-metadata: run id required")
	}
	if md.EscalationClass == "" && md.FailureClass == "" &&
		md.ExternalDependencyID == "" && md.ExternalDependency == "" &&
		md.Retryable == nil && md.RetryExhausted == nil {
		return nil
	}
	var retryable sql.NullBool
	if md.Retryable != nil {
		retryable = sql.NullBool{Bool: *md.Retryable, Valid: true}
	}
	var retryExhausted sql.NullBool
	if md.RetryExhausted != nil {
		retryExhausted = sql.NullBool{Bool: *md.RetryExhausted, Valid: true}
	}
	res, err := d.db.ExecContext(ctx, `
		UPDATE pipeline_runs
		SET escalation_class = COALESCE(NULLIF(?, ''), escalation_class),
		    escalation_failure_class = COALESCE(NULLIF(?, ''), escalation_failure_class),
		    escalation_external_dependency_id = COALESCE(NULLIF(?, ''), escalation_external_dependency_id),
		    escalation_external_dependency = COALESCE(NULLIF(?, ''), escalation_external_dependency),
		    escalation_retryable = COALESCE(?, escalation_retryable),
		    retry_exhausted = COALESCE(?, retry_exhausted)
		WHERE id = ?
	`,
		md.EscalationClass, md.FailureClass, md.ExternalDependencyID,
		md.ExternalDependency, retryable, retryExhausted, runID,
	)
	if err != nil {
		return fmt.Errorf("pipeline set-escalation-metadata: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// ListActive returns every non-terminal pipeline run in a single query,
// grouped by pipeline progression (queued → merging) then oldest-first
// within a state — the same order the HUD's active board rendered when
// the runs-list handler issued one SELECT per state. One query replaces
// that per-state fan-out and can't observe a run twice (or miss it) when
// a state transition lands between per-state SELECTs. The predicate
// matches CountActive so the list and the concurrency cap agree.
func (d *PipelineDAO) ListActive(ctx context.Context) ([]*PipelineRun, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+pipelineColumns+` FROM pipeline_runs
		WHERE state NOT IN ('done', 'escalated', 'paused')
		ORDER BY CASE state
			WHEN 'queued' THEN 0
			WHEN 'planning' THEN 1
			WHEN 'slicing' THEN 2
			WHEN 'implementing' THEN 3
			WHEN 'testing' THEN 4
			WHEN 'reviewing' THEN 5
			WHEN 'mr' THEN 6
			WHEN 'ci' THEN 7
			WHEN 'merging' THEN 8
			ELSE 9 END, started_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("pipeline list-active: %w", err)
	}
	defer rows.Close()
	var out []*PipelineRun
	for rows.Next() {
		r, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountActive returns the number of pipeline runs in any non-terminal state.
// "Terminal" = done, escalated, paused. Used by the concurrency cap.
func (d *PipelineDAO) CountActive(ctx context.Context) (int, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pipeline_runs
		WHERE state NOT IN ('done', 'escalated', 'paused')
	`)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("pipeline count-active: %w", err)
	}
	return n, nil
}

// ListByBacklog returns all pipeline runs (across attempts) for a backlog item.
// SetResearchDiff persists a JSON-encoded shadow vs. legacy research
// comparison for the pipeline run. Migration 003 adds the column; this
// is the only writer. Pass a non-nil JSON byte slice; the empty string
// is allowed and clears the column.
//
// Used by ResearchModeShadow in pkg/mills/clients/flexinfer.go during
// the soak window (MILLS_RESEARCH_VIA_WEAVER=shadow). Wiring from
// WeaverClient.DiffRecorder to this method lands when the operator
// gains a way to thread the current run id through WeaverRequest;
// until then this stays available for tests + future use.
func (d *PipelineDAO) SetResearchDiff(ctx context.Context, runID string, diffJSON string) error {
	if runID == "" {
		return errors.New("pipeline set-research-diff: run id required")
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE pipeline_runs SET research_diff = ? WHERE id = ?`,
		diffJSON, runID,
	)
	if err != nil {
		return fmt.Errorf("pipeline set-research-diff: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *PipelineDAO) ListByBacklog(ctx context.Context, backlogID string) ([]*PipelineRun, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+pipelineColumns+` FROM pipeline_runs WHERE backlog_id = ? ORDER BY attempts ASC`,
		backlogID)
	if err != nil {
		return nil, fmt.Errorf("pipeline list-backlog: %w", err)
	}
	defer rows.Close()
	var out []*PipelineRun
	for rows.Next() {
		r, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// pipelineTerminalStates is the set of states a run can no longer leave:
// it has merged (done), been handed to a human (escalated), or been
// parked (paused). ListRecentTerminal + the HUD "history" view read these;
// CountActive/ListInFlight use the complement. Keep the two predicates in
// sync — a state added here must be excluded from "active" everywhere.
var pipelineTerminalStates = []PipelineState{
	PipelineDone, PipelineEscalated, PipelinePaused,
}

// ListRecentTerminal returns finished pipeline runs (done / escalated /
// paused), newest-first, so the HUD can show run history instead of only
// what's in flight. `since` bounds the window (zero = no lower bound);
// `limit` caps the row count (<=0 falls back to 50, hard-capped at 200 so
// a caller can't pull the whole table). This is the read side of the
// monitoring gap the active-only list left open.
func (d *PipelineDAO) ListRecentTerminal(ctx context.Context, since time.Time, limit int) ([]*PipelineRun, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	// Build the IN (?,?,?) placeholder list from the shared terminal set so
	// the query and the predicate can never drift.
	args := make([]any, 0, len(pipelineTerminalStates)+2)
	placeholders := ""
	for i, s := range pipelineTerminalStates {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, string(s))
	}
	q := `SELECT ` + pipelineColumns + ` FROM pipeline_runs WHERE state IN (` + placeholders + `)`
	if !since.IsZero() {
		q += ` AND started_at >= ?`
		args = append(args, timeRFC3339(since))
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pipeline list-recent-terminal: %w", err)
	}
	defer rows.Close()
	var out []*PipelineRun
	for rows.Next() {
		r, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListTerminalOutcomesSince returns the compact (run, backlog, terminal state,
// cost, MR) projection for runs started on-or-after `since`, newest-first.
// `limit` <= 0 falls back to 2000, hard-capped at 10000.
//
// Ground-truth joins (judge calibration, regression attribution,
// config-outcome analytics) need every terminal run in a multi-week window,
// not the newest 200 ListRecentTerminal will return: a run missing from the
// join is silently reclassified as "outcome unknown", which is exactly the
// guess these reports must not make. Five scalar columns per row is what makes
// the wider window affordable.
func (d *PipelineDAO) ListTerminalOutcomesSince(ctx context.Context, since time.Time, limit int) ([]*RunTerminalOutcome, error) {
	if limit <= 0 {
		limit = 2000
	}
	if limit > 10000 {
		limit = 10000
	}
	args := make([]any, 0, len(pipelineTerminalStates)+2)
	placeholders := ""
	for i, s := range pipelineTerminalStates {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, string(s))
	}
	q := `SELECT id, backlog_id, state, cost_usd, mr_iid FROM pipeline_runs WHERE state IN (` + placeholders + `)`
	if !since.IsZero() {
		q += ` AND started_at >= ?`
		args = append(args, timeRFC3339(since))
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pipeline list-terminal-outcomes: %w", err)
	}
	defer rows.Close()
	var out []*RunTerminalOutcome
	for rows.Next() {
		var (
			o     RunTerminalOutcome
			state string
			mrIID sql.NullInt64
		)
		if err := rows.Scan(&o.RunID, &o.BacklogID, &state, &o.CostUSD, &mrIID); err != nil {
			return nil, fmt.Errorf("pipeline list-terminal-outcomes scan: %w", err)
		}
		o.State = PipelineState(state)
		o.MRIID = nullableInt64(mrIID)
		out = append(out, &o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline list-terminal-outcomes rows: %w", err)
	}
	return out, nil
}

// ListByMRIID returns pipeline runs whose mr_iid matches, newest-first.
// Powers the HUD "audit by MR iid" lookup (Loop B attribution): given a
// merged MR's iid, find the pipeline run(s) that produced it.
func (d *PipelineDAO) ListByMRIID(ctx context.Context, mrIID int64) ([]*PipelineRun, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+pipelineColumns+` FROM pipeline_runs WHERE mr_iid = ? ORDER BY started_at DESC`,
		mrIID)
	if err != nil {
		return nil, fmt.Errorf("pipeline list-mriid: %w", err)
	}
	defer rows.Close()
	var out []*PipelineRun
	for rows.Next() {
		r, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListLegacyMRProjectBackfillCandidates returns the exact latest run for
// currently escalated backlog items whose successful MR stage predates durable
// project provenance. Invalid stage artifact payloads fail closed: the caller
// needs to inspect the original MR URL before it can safely patch the row.
func (d *PipelineDAO) ListLegacyMRProjectBackfillCandidates(ctx context.Context, limit int) ([]*PipelineRun, error) {
	if limit <= 0 || limit > 128 {
		limit = 128
	}
	rows, err := d.db.QueryContext(ctx, `
		WITH raw_cursor AS (
			SELECT cursor_json
			FROM maintenance_cursors
			WHERE name = ?
		), cursor_document AS (
			SELECT CASE
				WHEN json_valid(cursor_json)
				 AND json_type(CASE WHEN json_valid(cursor_json) THEN cursor_json ELSE '{}' END) = 'object'
					THEN cursor_json
				ELSE '{}'
			END AS value
			FROM raw_cursor
		), cursor AS (
			SELECT json_extract(value, '$.started_at') AS cursor_started_at,
			       json_extract(value, '$.attempts') AS cursor_attempts,
			       json_extract(value, '$.run_id') AS cursor_run_id
			FROM cursor_document
			WHERE json_type(value, '$.started_at') = 'text'
			  AND trim(json_extract(value, '$.started_at')) <> ''
			  AND json_type(value, '$.attempts') = 'integer'
			  AND json_extract(value, '$.attempts') >= 0
			  AND json_type(value, '$.run_id') = 'text'
			  AND trim(json_extract(value, '$.run_id')) <> ''
		), stage_artifacts AS (
			SELECT sr.pipeline_run_id, sr.stage, sr.outcome,
			       CASE
				WHEN json_valid(sr.artifacts_json)
				 AND json_type(CASE WHEN json_valid(sr.artifacts_json) THEN sr.artifacts_json ELSE '{}' END) = 'object'
					THEN sr.artifacts_json
			       END AS value
			FROM stage_results sr
		), successful_mr AS (
			SELECT pipeline_run_id,
			       json_type(value, '$.mr_url') AS mr_url_type,
			       json_extract(value, '$.mr_url') AS mr_url,
			       json_type(value, '$.mr_iid') AS mr_iid_type,
			       json_extract(value, '$.mr_iid') AS mr_iid
			FROM stage_artifacts
			WHERE stage = 'mr' AND outcome = 'success'
		)
		SELECT `+pipelineColumns+`
		FROM pipeline_runs candidate
		LEFT JOIN cursor ON 1 = 1
		WHERE candidate.id IN (
			SELECT pr.id
			FROM backlog_items bi
			JOIN pipeline_runs pr ON pr.id = (
				SELECT latest.id
				FROM pipeline_runs latest
				WHERE latest.backlog_id = bi.id
				ORDER BY latest.started_at DESC, latest.attempts DESC
				LIMIT 1
			)
			WHERE bi.state = 'escalated'
			  AND pr.state = 'escalated'
			  AND pr.mr_iid > 0
			  AND NOT EXISTS (
				SELECT 1
				FROM stage_artifacts malformed
				WHERE malformed.pipeline_run_id = pr.id
				  AND malformed.value IS NULL
			  )
			  AND NOT EXISTS (
				SELECT 1
				FROM stage_artifacts durable
				WHERE durable.pipeline_run_id = pr.id
				  AND durable.outcome = 'success'
				  AND CASE durable.stage
					WHEN 'mr' THEN json_type(durable.value, '$.mr_project')
					WHEN 'ci_watch' THEN json_type(durable.value, '$.ci_project')
					WHEN 'merge' THEN json_type(durable.value, '$.merged_project')
					WHEN 'cleanup' THEN json_type(durable.value, '$.cleanup_project')
				  END IS NOT NULL
			  )
			  AND NOT EXISTS (
				SELECT 1
				FROM successful_mr invalid_mr
				WHERE invalid_mr.pipeline_run_id = pr.id
				  AND invalid_mr.mr_url_type IS NOT NULL
				  AND COALESCE(
					invalid_mr.mr_url_type = 'text'
					AND trim(
						invalid_mr.mr_url,
						char(9) || char(10) || char(11) || char(12) || char(13) || char(32) ||
						char(133) || char(160) || char(5760) || char(8192) || char(8193) ||
						char(8194) || char(8195) || char(8196) || char(8197) || char(8198) ||
						char(8199) || char(8200) || char(8201) || char(8202) || char(8232) ||
						char(8233) || char(8239) || char(8287) || char(12288)
					) <> ''
					AND invalid_mr.mr_iid_type = 'integer'
					AND invalid_mr.mr_iid > 0
					AND invalid_mr.mr_iid = pr.mr_iid,
					0
				  ) = 0
			  )
			  AND EXISTS (
				SELECT 1
				FROM successful_mr valid_mr
				WHERE valid_mr.pipeline_run_id = pr.id
				  AND valid_mr.mr_url_type = 'text'
				  AND trim(
					valid_mr.mr_url,
					char(9) || char(10) || char(11) || char(12) || char(13) || char(32) ||
					char(133) || char(160) || char(5760) || char(8192) || char(8193) ||
					char(8194) || char(8195) || char(8196) || char(8197) || char(8198) ||
					char(8199) || char(8200) || char(8201) || char(8202) || char(8232) ||
					char(8233) || char(8239) || char(8287) || char(12288)
				  ) <> ''
				  AND valid_mr.mr_iid_type = 'integer'
				  AND valid_mr.mr_iid > 0
				  AND valid_mr.mr_iid = pr.mr_iid
			  )
		)
		ORDER BY CASE
			WHEN cursor.cursor_started_at IS NULL
			  OR candidate.started_at > cursor.cursor_started_at
			  OR (candidate.started_at = cursor.cursor_started_at AND candidate.attempts < cursor.cursor_attempts)
			  OR (candidate.started_at = cursor.cursor_started_at AND candidate.attempts = cursor.cursor_attempts AND candidate.id > cursor.cursor_run_id)
				THEN 0
			ELSE 1
		END,
		candidate.started_at ASC, candidate.attempts DESC, candidate.id ASC
		LIMIT ?
	`, legacyMRProjectBackfillCursorName, limit)
	if err != nil {
		return nil, fmt.Errorf("pipeline list legacy mr project backfill candidates: %w", err)
	}
	defer rows.Close()

	var out []*PipelineRun
	for rows.Next() {
		run, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline list legacy mr project backfill candidates rows: %w", err)
	}
	return out, nil
}

const legacyMRProjectBackfillCursorName = "legacy_mr_project_backfill"

// AdvanceLegacyMRProjectBackfillCursor durably records the last candidate a
// bounded backfill pass attempted. Subsequent lists start strictly after this
// run's sort tuple and wrap, preventing permanently rejected legacy URLs from
// monopolizing the first page across operator restarts.
func (d *PipelineDAO) AdvanceLegacyMRProjectBackfillCursor(ctx context.Context, run *PipelineRun) error {
	if run == nil || strings.TrimSpace(run.ID) == "" {
		return errors.New("pipeline advance legacy MR project cursor: run ID required")
	}
	if run.StartedAt.IsZero() {
		return errors.New("pipeline advance legacy MR project cursor: run started_at required")
	}
	if run.Attempts < 0 {
		return errors.New("pipeline advance legacy MR project cursor: run attempts must be non-negative")
	}
	cursorJSON, err := json.Marshal(struct {
		StartedAt string `json:"started_at"`
		Attempts  int    `json:"attempts"`
		RunID     string `json:"run_id"`
	}{
		StartedAt: timeRFC3339(run.StartedAt),
		Attempts:  run.Attempts,
		RunID:     run.ID,
	})
	if err != nil {
		return fmt.Errorf("pipeline advance legacy MR project cursor encode: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO maintenance_cursors (name, cursor_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			cursor_json = excluded.cursor_json,
			updated_at = excluded.updated_at
	`, legacyMRProjectBackfillCursorName, string(cursorJSON), timeRFC3339(time.Now())); err != nil {
		return fmt.Errorf("pipeline advance legacy MR project cursor: %w", err)
	}
	return nil
}

func scanPipelineRun(s scanner) (*PipelineRun, error) {
	var (
		run PipelineRun
		currentStage, worktreePath, parentSession, endedAt, state, parentRun,
		escalationClass, failureClass, externalDependencyID, externalDependency sql.NullString
		mrIID                     sql.NullInt64
		retryable, retryExhausted sql.NullBool
		startedAt                 string
	)
	err := s.Scan(
		&run.ID, &run.BacklogID, &run.AggregateVersion, &run.Revision, &run.Template, &state,
		&currentStage, &run.Attempts, &worktreePath, &mrIID,
		&startedAt, &endedAt, &run.CostUSD, &parentSession,
		&parentRun, &run.Depth, &escalationClass, &failureClass,
		&externalDependencyID, &externalDependency, &retryable, &retryExhausted,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("pipeline scan: %w", err)
	}
	if state.Valid {
		run.State = PipelineState(state.String)
	}
	if currentStage.Valid {
		run.CurrentStage = currentStage.String
	}
	if worktreePath.Valid {
		run.WorktreePath = worktreePath.String
	}
	if parentSession.Valid {
		run.ParentSessionID = parentSession.String
	}
	run.MRIID = nullableInt64(mrIID)
	run.ParentRunID = nullableString(parentRun)
	if escalationClass.Valid {
		run.EscalationClass = escalationClass.String
	}
	if failureClass.Valid {
		run.FailureClass = failureClass.String
	}
	if externalDependencyID.Valid {
		run.ExternalDependencyID = externalDependencyID.String
	}
	if externalDependency.Valid {
		run.ExternalDependency = externalDependency.String
	}
	if retryable.Valid {
		run.EscalationRetryable = &retryable.Bool
	}
	if retryExhausted.Valid {
		run.RetryExhausted = &retryExhausted.Bool
	}
	if run.StartedAt, err = parseTime(startedAt); err != nil {
		return nil, fmt.Errorf("started_at: %w", err)
	}
	if run.EndedAt, err = nullableTime(endedAt); err != nil {
		return nil, fmt.Errorf("ended_at: %w", err)
	}
	return &run, nil
}

// ----- Stage results -----

const stageColumns = `id, pipeline_run_id, stage, attempt, started_at, ended_at,
		outcome, spawn_id, cost_usd, model, backend, artifacts_json, log_tail`

var ErrStageSpawnConflict = errors.New("pipeline: stage attempt already has an accepted spawn")

// ErrMRProjectArtifactConflict means a legacy MR stage already contains a
// different project artifact or changed while a backfill attempted its CAS.
var ErrMRProjectArtifactConflict = errors.New("pipeline: mr project artifact conflict")

// PutStage inserts a stage result. The unique (pipeline_run_id, stage, attempt)
// index makes retries idempotent: re-recording the same attempt is a no-op
// upsert that updates ended_at/outcome.
func (d *PipelineDAO) PutStage(ctx context.Context, sr *StageResult) error {
	if sr == nil || sr.PipelineRunID == "" || sr.Stage == "" {
		return errors.New("pipeline: stage result requires PipelineRunID + Stage")
	}
	if sr.StartedAt.IsZero() {
		sr.StartedAt = time.Now().UTC()
	}
	artifacts, err := jsonField(sr.Artifacts)
	if err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	var (
		endedAt sql.NullString
		outcome sql.NullString
	)
	if sr.EndedAt != nil {
		endedAt = sql.NullString{String: timeRFC3339(*sr.EndedAt), Valid: true}
	}
	if sr.Outcome != nil {
		outcome = sql.NullString{String: string(*sr.Outcome), Valid: true}
	}
	if sr.SpawnID != "" {
		var existingSpawn, existingOutcome sql.NullString
		err := d.db.QueryRowContext(ctx, `
			SELECT spawn_id, outcome
			FROM stage_results
			WHERE pipeline_run_id = ? AND stage = ? AND attempt = ?
		`, sr.PipelineRunID, sr.Stage, sr.Attempt).Scan(&existingSpawn, &existingOutcome)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("stage conflict check %s/%s/%d: %w", sr.PipelineRunID, sr.Stage, sr.Attempt, err)
		}
		if err == nil && !existingOutcome.Valid && existingSpawn.Valid && existingSpawn.String != "" && existingSpawn.String != sr.SpawnID {
			return fmt.Errorf("%w: %s/%s/%d existing=%s incoming=%s",
				ErrStageSpawnConflict, sr.PipelineRunID, sr.Stage, sr.Attempt, existingSpawn.String, sr.SpawnID)
		}
	}
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO stage_results (pipeline_run_id, stage, attempt, started_at,
			ended_at, outcome, spawn_id, cost_usd, model, backend, artifacts_json, log_tail)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(pipeline_run_id, stage, attempt) DO UPDATE SET
			ended_at        = excluded.ended_at,
			outcome         = excluded.outcome,
			spawn_id        = COALESCE(NULLIF(excluded.spawn_id, ''), stage_results.spawn_id),
			cost_usd        = excluded.cost_usd,
			-- Preserve an earlier attribution when a later idempotent write (e.g.
			-- the pre-dispatch spawn-accept row) carries no model/backend, the
			-- same COALESCE(NULLIF(...)) guard spawn_id uses above.
			model           = COALESCE(NULLIF(excluded.model, ''), stage_results.model),
			backend         = COALESCE(NULLIF(excluded.backend, ''), stage_results.backend),
			artifacts_json  = excluded.artifacts_json,
			log_tail        = excluded.log_tail
	`,
		sr.PipelineRunID, sr.Stage, sr.Attempt, timeRFC3339(sr.StartedAt),
		endedAt, outcome, nullStr(sr.SpawnID), sr.CostUSD, nullStr(sr.Model), nullStr(sr.Backend), artifacts, nullStr(sr.LogTail),
	)
	if err != nil {
		return fmt.Errorf("stage put %s/%s/%d: %w", sr.PipelineRunID, sr.Stage, sr.Attempt, err)
	}
	if id, err := res.LastInsertId(); err == nil && sr.ID == 0 {
		sr.ID = id
	}
	return nil
}

// ListStages returns every stage attempt for a pipeline run, in execution order.
func (d *PipelineDAO) ListStages(ctx context.Context, pipelineRunID string) ([]*StageResult, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+stageColumns+` FROM stage_results WHERE pipeline_run_id = ? ORDER BY started_at ASC`,
		pipelineRunID)
	if err != nil {
		return nil, fmt.Errorf("stage list: %w", err)
	}
	defer rows.Close()
	var out []*StageResult
	for rows.Next() {
		var (
			sr        StageResult
			endedAt   sql.NullString
			outcome   sql.NullString
			spawnID   sql.NullString
			model     sql.NullString
			backend   sql.NullString
			logTail   sql.NullString
			artifacts string
			startedAt string
		)
		if err := rows.Scan(&sr.ID, &sr.PipelineRunID, &sr.Stage, &sr.Attempt,
			&startedAt, &endedAt, &outcome, &spawnID, &sr.CostUSD, &model, &backend, &artifacts, &logTail); err != nil {
			return nil, fmt.Errorf("stage scan: %w", err)
		}
		if sr.StartedAt, err = parseTime(startedAt); err != nil {
			return nil, fmt.Errorf("started_at: %w", err)
		}
		if sr.EndedAt, err = nullableTime(endedAt); err != nil {
			return nil, fmt.Errorf("ended_at: %w", err)
		}
		if outcome.Valid {
			o := StageOutcome(outcome.String)
			sr.Outcome = &o
		}
		if spawnID.Valid {
			sr.SpawnID = spawnID.String
		}
		if model.Valid {
			sr.Model = model.String
		}
		if backend.Valid {
			sr.Backend = backend.String
		}
		if logTail.Valid {
			sr.LogTail = logTail.String
		}
		if err := jsonInto(artifacts, &sr.Artifacts); err != nil {
			return nil, fmt.Errorf("artifacts: %w", err)
		}
		out = append(out, &sr)
	}
	return out, rows.Err()
}

// PatchMRProjectArtifact adds mr_project to one successful legacy MR stage.
// It never rewrites an existing project value and requires the run, MR URL,
// stage IID, and current run IID to still match the identity the caller
// externally verified. The write then compare-and-swaps the exact artifacts
// JSON read in the transaction. The returned bool is true only when this call
// applied the patch; false with a nil error is an idempotent replay.
func (d *PipelineDAO) PatchMRProjectArtifact(ctx context.Context, stageResultID int64, expectedRunID, expectedMRURL string, expectedMRIID int64, project string) (bool, error) {
	expectedMRURL = strings.TrimSpace(expectedMRURL)
	project = strings.TrimSpace(project)
	if stageResultID <= 0 {
		return false, errors.New("pipeline patch mr project: stage result id must be positive")
	}
	if strings.TrimSpace(expectedRunID) == "" {
		return false, errors.New("pipeline patch mr project: expected run id required")
	}
	if expectedMRURL == "" {
		return false, errors.New("pipeline patch mr project: expected MR URL required")
	}
	if expectedMRIID <= 0 {
		return false, errors.New("pipeline patch mr project: expected MR IID must be positive")
	}
	if project == "" {
		return false, errors.New("pipeline patch mr project: project required")
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("pipeline patch mr project begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var pipelineRunID, stage string
	var outcome sql.NullString
	var runMRIID sql.NullInt64
	var artifactsJSON string
	if err := tx.QueryRowContext(ctx, `
		SELECT sr.pipeline_run_id, sr.stage, sr.outcome, sr.artifacts_json, pr.mr_iid
		FROM stage_results sr
		JOIN pipeline_runs pr ON pr.id = sr.pipeline_run_id
		WHERE sr.id = ?
	`, stageResultID).Scan(&pipelineRunID, &stage, &outcome, &artifactsJSON, &runMRIID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("pipeline patch mr project read %d: %w", stageResultID, err)
	}
	if pipelineRunID != expectedRunID {
		return false, fmt.Errorf("%w: stage result %d pipeline run changed", ErrMRProjectArtifactConflict, stageResultID)
	}
	if !runMRIID.Valid || runMRIID.Int64 != expectedMRIID {
		return false, fmt.Errorf("%w: pipeline run %s mr_iid changed", ErrMRProjectArtifactConflict, expectedRunID)
	}
	if stage != "mr" || !outcome.Valid || StageOutcome(outcome.String) != StageOutcomeSuccess {
		return false, fmt.Errorf("pipeline patch mr project %d: successful mr stage required", stageResultID)
	}

	var artifacts map[string]json.RawMessage
	if !json.Valid([]byte(artifactsJSON)) || json.Unmarshal([]byte(artifactsJSON), &artifacts) != nil || artifacts == nil {
		return false, fmt.Errorf("pipeline patch mr project %d: artifacts must be a valid JSON object", stageResultID)
	}
	var currentMRURL string
	if raw, exists := artifacts["mr_url"]; !exists || json.Unmarshal(raw, &currentMRURL) != nil || strings.TrimSpace(currentMRURL) != expectedMRURL {
		return false, fmt.Errorf("%w: stage result %d mr_url changed", ErrMRProjectArtifactConflict, stageResultID)
	}
	var currentMRIID int64
	if raw, exists := artifacts["mr_iid"]; !exists || json.Unmarshal(raw, &currentMRIID) != nil || currentMRIID != expectedMRIID {
		return false, fmt.Errorf("%w: stage result %d mr_iid changed", ErrMRProjectArtifactConflict, stageResultID)
	}
	applied := true
	patchedJSON := artifactsJSON
	if raw, exists := artifacts["mr_project"]; exists {
		var existing string
		if err := json.Unmarshal(raw, &existing); err == nil && strings.TrimSpace(existing) == project {
			applied = false
		} else {
			return false, fmt.Errorf("%w: stage result %d already has mr_project", ErrMRProjectArtifactConflict, stageResultID)
		}
	} else {
		projectJSON, err := json.Marshal(project)
		if err != nil {
			return false, fmt.Errorf("pipeline patch mr project encode %d: %w", stageResultID, err)
		}
		openingBrace := strings.IndexByte(artifactsJSON, '{')
		closingBrace := strings.LastIndexByte(artifactsJSON, '}')
		if openingBrace < 0 || closingBrace <= openingBrace {
			return false, fmt.Errorf("pipeline patch mr project %d: artifacts object bounds invalid", stageResultID)
		}
		separator := ","
		if strings.TrimSpace(artifactsJSON[openingBrace+1:closingBrace]) == "" {
			separator = ""
		}
		patchedJSON = artifactsJSON[:closingBrace] + separator + `"mr_project":` + string(projectJSON) + artifactsJSON[closingBrace:]
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE stage_results
		SET artifacts_json = ?
		WHERE id = ?
		  AND pipeline_run_id = ?
		  AND artifacts_json = ?
		  AND stage = 'mr'
		  AND outcome = 'success'
		  AND EXISTS (
			SELECT 1 FROM pipeline_runs pr
			WHERE pr.id = ? AND pr.mr_iid = ?
		  )
	`, patchedJSON, stageResultID, expectedRunID, artifactsJSON, expectedRunID, expectedMRIID)
	if err != nil {
		return false, fmt.Errorf("pipeline patch mr project update %d: %w", stageResultID, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("pipeline patch mr project rows %d: %w", stageResultID, err)
	}
	if updated != 1 {
		return false, fmt.Errorf("%w: stage result %d changed during patch", ErrMRProjectArtifactConflict, stageResultID)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("pipeline patch mr project commit %d: %w", stageResultID, err)
	}
	return applied, nil
}

// AuthorizedProject returns the one project consistently recorded by the
// successful MR, CI, merge, and cleanup stages for a run. It is intentionally
// derived from immutable stage results rather than backlog_items.target_project,
// which may be edited after an MR is created. Legacy or contradictory rows fail
// closed so callers cannot resolve a per-project MR IID in the wrong project.
func (d *PipelineDAO) AuthorizedProject(ctx context.Context, pipelineRunID string) (string, error) {
	if strings.TrimSpace(pipelineRunID) == "" {
		return "", fmt.Errorf("pipeline project: run id required: %w", ErrPipelineProjectUnavailable)
	}
	stages, err := d.ListStages(ctx, pipelineRunID)
	if err != nil {
		return "", err
	}
	project := ""
	for _, sr := range stages {
		if sr == nil || sr.Outcome == nil || *sr.Outcome != StageOutcomeSuccess || sr.Artifacts == nil {
			continue
		}
		key := ""
		switch sr.Stage {
		case "mr":
			key = "mr_project"
		case "ci_watch":
			key = "ci_project"
		case "merge":
			key = "merged_project"
		case "cleanup":
			key = "cleanup_project"
		default:
			continue
		}
		raw, exists := sr.Artifacts[key]
		if !exists {
			continue
		}
		value, ok := raw.(string)
		value = strings.TrimSpace(value)
		if !ok || value == "" {
			return "", fmt.Errorf("pipeline project: run %s stage %s has invalid %s: %w", pipelineRunID, sr.Stage, key, ErrPipelineProjectUnavailable)
		}
		if project == "" {
			project = value
			continue
		}
		if value != project {
			return "", fmt.Errorf("pipeline project: run %s stage %s project %q conflicts with %q: %w", pipelineRunID, sr.Stage, value, project, ErrPipelineProjectUnavailable)
		}
	}
	if project == "" {
		return "", fmt.Errorf("pipeline project: run %s has no successful MR provenance: %w", pipelineRunID, ErrPipelineProjectUnavailable)
	}
	return project, nil
}

// ----- Gate outcomes -----

const gateColumns = `id, pipeline_run_id, after_stage, gate_name, outcome,
		reasons_json, judged_by, evaluated_at`

// PutGate appends a gate outcome record.
func (d *PipelineDAO) PutGate(ctx context.Context, g *GateOutcome) error {
	if g == nil || g.PipelineRunID == "" || g.GateName == "" {
		return errors.New("pipeline: gate outcome requires PipelineRunID + GateName")
	}
	if g.EvaluatedAt.IsZero() {
		g.EvaluatedAt = time.Now().UTC()
	}
	reasons, err := jsonField(g.Reasons)
	if err != nil {
		return fmt.Errorf("reasons: %w", err)
	}
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO gate_outcomes (pipeline_run_id, after_stage, gate_name, outcome,
			reasons_json, judged_by, evaluated_at)
		VALUES (?,?,?,?,?,?,?)
	`,
		g.PipelineRunID, g.AfterStage, g.GateName, string(g.Outcome),
		reasons, g.JudgedBy, timeRFC3339(g.EvaluatedAt),
	)
	if err != nil {
		return fmt.Errorf("gate put: %w", err)
	}
	id, _ := res.LastInsertId()
	g.ID = id
	return nil
}

// ListGates returns every gate outcome for a pipeline run, oldest-first.
func (d *PipelineDAO) ListGates(ctx context.Context, pipelineRunID string) ([]*GateOutcome, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+gateColumns+` FROM gate_outcomes WHERE pipeline_run_id = ? ORDER BY evaluated_at ASC`,
		pipelineRunID)
	if err != nil {
		return nil, fmt.Errorf("gate list: %w", err)
	}
	defer rows.Close()
	var out []*GateOutcome
	for rows.Next() {
		var (
			g           GateOutcome
			reasons     string
			outcome     string
			evaluatedAt string
		)
		if err := rows.Scan(&g.ID, &g.PipelineRunID, &g.AfterStage, &g.GateName,
			&outcome, &reasons, &g.JudgedBy, &evaluatedAt); err != nil {
			return nil, fmt.Errorf("gate scan: %w", err)
		}
		g.Outcome = GateOutcomeKind(outcome)
		if g.EvaluatedAt, err = parseTime(evaluatedAt); err != nil {
			return nil, fmt.Errorf("evaluated_at: %w", err)
		}
		if err := jsonInto(reasons, &g.Reasons); err != nil {
			return nil, fmt.Errorf("reasons: %w", err)
		}
		out = append(out, &g)
	}
	return out, rows.Err()
}
