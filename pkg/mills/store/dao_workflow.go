package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WorkflowDAO is the Layer-2 durable step/event journal: workflow_runs +
// workflow_steps (migration 004). It is the stable persistence layer for the
// Mills durable workflow engine; the imperative runtime that drives it ships
// in a later slice.
//
// DUAL SOURCE-OF-TRUTH (see migration 004 + types.go): legacy `dag` runs do
// NOT write workflow_steps; only `imperative` runs do. workflow_steps is the
// source of truth for imperative resume — the runtime replays this append-only
// log to reconstruct state. The generic Event log (events table) stays
// advisory (audit/debug) and is never consulted for resume.
//
// All writes follow the WAL one-writer model (Store.Open sets busy_timeout so
// concurrent writers serialise inside the driver).
type WorkflowDAO struct {
	db *sql.DB
}

// ErrStepCallHashMismatch is returned by AppendStep when an incoming step
// reuses an existing (run_id, step_key) but carries a different call_hash.
// This signals nondeterminism: the recorded step is NOT overwritten, and the
// caller (future runtime) is expected to quarantine the run. AppendStep
// returns this error together with the existing record so the caller can act
// on it.
var ErrStepCallHashMismatch = errors.New("workflow: step call_hash mismatch (nondeterminism)")

const workflowRunColumns = `id, backlog_id, engine, template, template_version,
		interpreter_version, workflow_params, state, paused_at, resumed_at,
		started_at, ended_at, cost_usd, parent_session_id`

const workflowStepColumns = `id, run_id, step_key, event_type, call_hash,
		idempotency_key, status, spawn_id, started_at, ended_at, result_blob,
		cost_usd, cost_source, effect_count`

// ----- Workflow runs -------------------------------------------------------

// PutWorkflowRun inserts or updates a workflow run. Engine is treated as an
// immutable discriminator by convention (the runtime sets it once at
// creation); PutWorkflowRun does not re-validate immutability so that rollups
// of the same run id remain a simple upsert.
func (d *WorkflowDAO) PutWorkflowRun(ctx context.Context, run *WorkflowRun) error {
	if run == nil || run.ID == "" {
		return errors.New("workflow: run.ID required")
	}
	if run.Engine == "" {
		return errors.New("workflow: run.Engine required")
	}
	if run.Template == "" {
		return errors.New("workflow: run.Template required")
	}
	if run.State == "" {
		return errors.New("workflow: run.State required")
	}

	pausedAt := nullTime(run.PausedAt)
	resumedAt := nullTime(run.ResumedAt)
	startedAt := nullTime(run.StartedAt)
	endedAt := nullTime(run.EndedAt)

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO workflow_runs (`+workflowRunColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			backlog_id          = excluded.backlog_id,
			engine              = excluded.engine,
			template            = excluded.template,
			template_version    = excluded.template_version,
			interpreter_version = excluded.interpreter_version,
			workflow_params     = excluded.workflow_params,
			state               = excluded.state,
			paused_at           = excluded.paused_at,
			resumed_at          = excluded.resumed_at,
			started_at          = excluded.started_at,
			ended_at            = excluded.ended_at,
			cost_usd            = excluded.cost_usd,
			parent_session_id   = excluded.parent_session_id
	`,
		run.ID, nullStr(run.BacklogID), string(run.Engine), run.Template,
		run.TemplateVersion, run.InterpreterVersion, nullStr(run.WorkflowParams),
		string(run.State), pausedAt, resumedAt, startedAt, endedAt,
		run.CostUSD, nullStr(run.ParentSessionID),
	)
	if err != nil {
		return fmt.Errorf("workflow put run %s: %w", run.ID, err)
	}
	return nil
}

// GetWorkflowRun loads one run by id. Returns ErrNotFound if absent.
func (d *WorkflowDAO) GetWorkflowRun(ctx context.Context, id string) (*WorkflowRun, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+workflowRunColumns+` FROM workflow_runs WHERE id = ?`, id)
	return scanWorkflowRun(row.Scan)
}

// ListRunningImperativeRuns returns every workflow run in state='running' with
// engine='imperative', oldest first (by rowid via the implicit insertion order
// of the id PK is not guaranteed; we order by started_at then id for a stable,
// fairness-preserving scan). This is the WorkflowScheduler's tick query: only
// imperative runs are driven by the imperative runtime, and only 'running' ones
// need an advance — paused/done/escalated/quarantined runs are skipped here so
// the scheduler never touches a stopped run. The dual-source-of-truth invariant
// holds: legacy 'dag' runs are excluded by the engine predicate.
func (d *WorkflowDAO) ListRunningImperativeRuns(ctx context.Context) ([]*WorkflowRun, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+workflowRunColumns+` FROM workflow_runs
		 WHERE state = ? AND engine = ?
		 ORDER BY COALESCE(started_at, '') ASC, id ASC`,
		string(WorkflowRunRunning), string(WorkflowEngineImperative))
	if err != nil {
		return nil, fmt.Errorf("workflow list running imperative: %w", err)
	}
	defer rows.Close()
	var out []*WorkflowRun
	for rows.Next() {
		run, err := scanWorkflowRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func scanWorkflowRun(scan func(dest ...any) error) (*WorkflowRun, error) {
	var (
		run            WorkflowRun
		backlogID      sql.NullString
		engine, state  string
		workflowParams sql.NullString
		pausedAt       sql.NullString
		resumedAt      sql.NullString
		startedAt      sql.NullString
		endedAt        sql.NullString
		parentSession  sql.NullString
	)
	err := scan(&run.ID, &backlogID, &engine, &run.Template, &run.TemplateVersion,
		&run.InterpreterVersion, &workflowParams, &state, &pausedAt, &resumedAt,
		&startedAt, &endedAt, &run.CostUSD, &parentSession)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("workflow run scan: %w", err)
	}
	run.Engine = WorkflowEngine(engine)
	run.State = WorkflowRunState(state)
	if backlogID.Valid {
		run.BacklogID = backlogID.String
	}
	if workflowParams.Valid {
		run.WorkflowParams = workflowParams.String
	}
	if parentSession.Valid {
		run.ParentSessionID = parentSession.String
	}
	var err2 error
	if run.PausedAt, err2 = nullableTime(pausedAt); err2 != nil {
		return nil, fmt.Errorf("paused_at: %w", err2)
	}
	if run.ResumedAt, err2 = nullableTime(resumedAt); err2 != nil {
		return nil, fmt.Errorf("resumed_at: %w", err2)
	}
	if run.StartedAt, err2 = nullableTime(startedAt); err2 != nil {
		return nil, fmt.Errorf("started_at: %w", err2)
	}
	if run.EndedAt, err2 = nullableTime(endedAt); err2 != nil {
		return nil, fmt.Errorf("ended_at: %w", err2)
	}
	return &run, nil
}

// ----- Workflow steps ------------------------------------------------------

// AppendStep records (or advances) one step in the durable journal, keyed by
// the unique (run_id, step_key). Semantics:
//
//   - First append of a (run_id, step_key): inserts the row as given.
//   - Re-append of an identical recorded step (same call_hash): a no-op that
//     returns the EXISTING record. This makes replay idempotent.
//   - A 'pending' row advancing to a terminal status (success/error/
//     gate_fail/skipped) with the same call_hash: updates status, ended_at,
//     result_blob, cost, effect_count, spawn_id. This is the record-before-
//     result completion path.
//   - A call_hash MISMATCH on an existing step_key: NOT applied. AppendStep
//     returns the existing record together with ErrStepCallHashMismatch so the
//     caller can quarantine the run. The journal is never silently overwritten
//     on a hash mismatch.
//
// On return, when err is nil, step.ID is populated. When err is
// ErrStepCallHashMismatch, the returned *WorkflowStep is the EXISTING record
// (the incoming step is rejected).
func (d *WorkflowDAO) AppendStep(ctx context.Context, step *WorkflowStep) (*WorkflowStep, error) {
	if step == nil || step.RunID == "" {
		return nil, errors.New("workflow: step.RunID required")
	}
	if step.StepKey == "" {
		return nil, errors.New("workflow: step.StepKey required")
	}
	if step.EventType == "" {
		return nil, errors.New("workflow: step.EventType required")
	}
	if step.CallHash == "" {
		return nil, errors.New("workflow: step.CallHash required")
	}
	if step.Status == "" {
		step.Status = WorkflowStepPending
	}

	// Look up any existing row for this (run_id, step_key). The UNIQUE index
	// guarantees at most one. We resolve idempotency / mismatch in Go rather
	// than via ON CONFLICT so a hash mismatch can be surfaced (ON CONFLICT
	// cannot return "the conflicting row is incompatible").
	existing, err := d.GetStep(ctx, step.RunID, step.StepKey)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("workflow append lookup %s/%s: %w", step.RunID, step.StepKey, err)
	}

	if existing != nil {
		// Determinism guard: never overwrite on a call_hash mismatch.
		if existing.CallHash != step.CallHash {
			return existing, fmt.Errorf("%w: run=%s key=%s existing=%s incoming=%s",
				ErrStepCallHashMismatch, step.RunID, step.StepKey, existing.CallHash, step.CallHash)
		}
		// Same call_hash. If the recorded step is already terminal and the
		// incoming one carries no new terminal status, this is a pure replay
		// no-op: return the existing record untouched.
		if existing.Status.IsTerminal() && !step.Status.IsTerminal() {
			return existing, nil
		}
		// Same call_hash + (pending->terminal) or (replay of identical
		// terminal): advance the recorded row. Updating to the same values is
		// harmless and keeps the path branch-free.
		if err := d.updateStep(ctx, existing.ID, step); err != nil {
			return nil, err
		}
		updated, err := d.GetStep(ctx, step.RunID, step.StepKey)
		if err != nil {
			return nil, err
		}
		step.ID = updated.ID
		return updated, nil
	}

	// Fresh insert.
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO workflow_steps (run_id, step_key, event_type, call_hash,
			idempotency_key, status, spawn_id, started_at, ended_at, result_blob,
			cost_usd, cost_source, effect_count)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
	`,
		step.RunID, step.StepKey, string(step.EventType), step.CallHash,
		nullStr(step.IdempotencyKey), string(step.Status), nullStr(step.SpawnID),
		nullTime(step.StartedAt), nullTime(step.EndedAt), nullStr(step.ResultBlob),
		step.CostUSD, nullStr(string(step.CostSource)), step.EffectCount,
	)
	if err != nil {
		return nil, fmt.Errorf("workflow append %s/%s: %w", step.RunID, step.StepKey, err)
	}
	if id, err := res.LastInsertId(); err == nil {
		step.ID = id
	}
	out, err := d.GetStep(ctx, step.RunID, step.StepKey)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// updateStep advances an existing journaled step (same call_hash) to the
// incoming status/result. Used by AppendStep on the pending->terminal path.
func (d *WorkflowDAO) updateStep(ctx context.Context, id int64, step *WorkflowStep) error {
	_, err := d.db.ExecContext(ctx, `
		UPDATE workflow_steps SET
			event_type      = ?,
			idempotency_key = ?,
			status          = ?,
			spawn_id        = COALESCE(?, spawn_id),
			started_at      = COALESCE(?, started_at),
			ended_at        = ?,
			result_blob     = ?,
			cost_usd        = ?,
			cost_source     = ?,
			effect_count    = ?
		WHERE id = ?
	`,
		string(step.EventType), nullStr(step.IdempotencyKey), string(step.Status),
		nullStr(step.SpawnID), nullTime(step.StartedAt), nullTime(step.EndedAt),
		nullStr(step.ResultBlob), step.CostUSD, nullStr(string(step.CostSource)),
		step.EffectCount, id,
	)
	if err != nil {
		return fmt.Errorf("workflow update step %d: %w", id, err)
	}
	return nil
}

// GetStep loads one journaled step by (run_id, step_key). Returns ErrNotFound
// if absent.
func (d *WorkflowDAO) GetStep(ctx context.Context, runID, stepKey string) (*WorkflowStep, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+workflowStepColumns+` FROM workflow_steps WHERE run_id = ? AND step_key = ?`,
		runID, stepKey)
	return scanWorkflowStep(row.Scan)
}

// ListPending returns every step still in 'pending' status for a run, in
// insertion order. This drives crash-between-writes reconciliation: a step
// appended pending whose effect was interrupted before the success update is
// recoverable here.
func (d *WorkflowDAO) ListPending(ctx context.Context, runID string) ([]*WorkflowStep, error) {
	return d.querySteps(ctx,
		`SELECT `+workflowStepColumns+` FROM workflow_steps
		 WHERE run_id = ? AND status = 'pending' ORDER BY id ASC`,
		runID)
}

// ListByRun returns every step for a run in insertion (append) order. This is
// the replay log the imperative runtime consumes to reconstruct state.
func (d *WorkflowDAO) ListByRun(ctx context.Context, runID string) ([]*WorkflowStep, error) {
	return d.querySteps(ctx,
		`SELECT `+workflowStepColumns+` FROM workflow_steps
		 WHERE run_id = ? ORDER BY id ASC`,
		runID)
}

func (d *WorkflowDAO) querySteps(ctx context.Context, query string, args ...any) ([]*WorkflowStep, error) {
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("workflow step query: %w", err)
	}
	defer rows.Close()
	var out []*WorkflowStep
	for rows.Next() {
		st, err := scanWorkflowStep(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func scanWorkflowStep(scan func(dest ...any) error) (*WorkflowStep, error) {
	var (
		st             WorkflowStep
		eventType      string
		status         string
		idempotencyKey sql.NullString
		spawnID        sql.NullString
		startedAt      sql.NullString
		endedAt        sql.NullString
		resultBlob     sql.NullString
		costSource     sql.NullString
	)
	err := scan(&st.ID, &st.RunID, &st.StepKey, &eventType, &st.CallHash,
		&idempotencyKey, &status, &spawnID, &startedAt, &endedAt, &resultBlob,
		&st.CostUSD, &costSource, &st.EffectCount)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("workflow step scan: %w", err)
	}
	st.EventType = WorkflowEventType(eventType)
	st.Status = WorkflowStepStatus(status)
	if idempotencyKey.Valid {
		st.IdempotencyKey = idempotencyKey.String
	}
	if spawnID.Valid {
		st.SpawnID = spawnID.String
	}
	if resultBlob.Valid {
		st.ResultBlob = resultBlob.String
	}
	if costSource.Valid {
		st.CostSource = WorkflowCostSource(costSource.String)
	}
	var err2 error
	if st.StartedAt, err2 = nullableTime(startedAt); err2 != nil {
		return nil, fmt.Errorf("started_at: %w", err2)
	}
	if st.EndedAt, err2 = nullableTime(endedAt); err2 != nil {
		return nil, fmt.Errorf("ended_at: %w", err2)
	}
	return &st, nil
}

// nullTime renders a *time.Time as a nullable RFC3339Nano TEXT value for
// SQLite. A nil pointer stores NULL.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return timeRFC3339(*t)
}
