package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

var (
	// ErrStepCallHashMismatch is returned by AppendStep when an incoming step
	// reuses an existing (run_id, step_key) but carries a different call_hash.
	// This signals nondeterminism: the recorded step is NOT overwritten, and the
	// caller (future runtime) is expected to quarantine the run. AppendStep
	// returns this error together with the existing record so the caller can act
	// on it.
	ErrStepCallHashMismatch = errors.New("workflow: step call_hash mismatch (nondeterminism)")

	// ErrWorkflowRunMetadataMismatch is the sentinel wrapped by
	// WorkflowRunMetadataMismatchError when an update attempts to change
	// creation-time workflow identity.
	ErrWorkflowRunMetadataMismatch = errors.New("workflow: immutable run metadata mismatch")

	// ErrStepTerminalConflict is returned when two same-hash terminal writers
	// disagree about the durable outcome. The first committed terminal result
	// remains authoritative; callers must consume it or stop rather than report
	// their losing result as successfully persisted.
	ErrStepTerminalConflict = errors.New("workflow: conflicting terminal step result")

	// ErrWorkflowRunExists is returned by CreateWorkflowRun when a caller-owned
	// id is already durable. Creation never updates/revives the existing row.
	ErrWorkflowRunExists = errors.New("workflow: run already exists")
)

// WorkflowRunMetadataMismatchError identifies the immutable fields an
// attempted PutWorkflowRun update disagreed with. Workflow params remain
// opaque and may contain sensitive inputs, so the error reports field names
// without echoing values.
type WorkflowRunMetadataMismatchError struct {
	RunID  string
	Fields []string
}

func (e *WorkflowRunMetadataMismatchError) Error() string {
	if e == nil {
		return ErrWorkflowRunMetadataMismatch.Error()
	}
	return fmt.Sprintf("%s for run %s: %s",
		ErrWorkflowRunMetadataMismatch, e.RunID, strings.Join(e.Fields, ", "))
}

// Unwrap lets callers use both errors.Is with the sentinel and errors.As with
// the typed mismatch details.
func (e *WorkflowRunMetadataMismatchError) Unwrap() error {
	return ErrWorkflowRunMetadataMismatch
}

// WorkflowStepTerminalConflictError identifies terminal fields on which a
// losing same-hash writer disagreed with the already-durable result. Values are
// intentionally omitted because result blobs may contain sensitive output.
type WorkflowStepTerminalConflictError struct {
	RunID   string
	StepKey string
	Fields  []string
}

func (e *WorkflowStepTerminalConflictError) Error() string {
	if e == nil {
		return ErrStepTerminalConflict.Error()
	}
	return fmt.Sprintf("%s for %s/%s: %s",
		ErrStepTerminalConflict, e.RunID, e.StepKey, strings.Join(e.Fields, ", "))
}

func (e *WorkflowStepTerminalConflictError) Unwrap() error {
	return ErrStepTerminalConflict
}

const workflowRunColumns = `id, backlog_id, engine, template, template_version,
		interpreter_version, workflow_params, state, paused_at, resumed_at,
		started_at, ended_at, cost_usd, parent_session_id`

const workflowStepColumns = `id, run_id, step_key, event_type, call_hash,
		idempotency_key, status, spawn_id, started_at, ended_at, result_blob,
		cost_usd, cost_source, effect_count`

// ----- Workflow runs -------------------------------------------------------

// CreateWorkflowRun inserts one fresh run and never updates an existing id.
// Use PutWorkflowRun only for intentional lifecycle persistence of an already
// owned row; request-idempotent launch paths must distinguish replay from
// creation so a terminal run cannot be revived by a lost-response retry.
func (d *WorkflowDAO) CreateWorkflowRun(ctx context.Context, run *WorkflowRun) error {
	if err := validateWorkflowRun(run); err != nil {
		return err
	}
	result, err := d.db.ExecContext(ctx, `
		INSERT INTO workflow_runs (`+workflowRunColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO NOTHING
	`, workflowRunValues(run)...)
	if err != nil {
		return fmt.Errorf("workflow create run %s: %w", run.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("workflow create run %s rows affected: %w", run.ID, err)
	}
	if rows != 1 {
		return fmt.Errorf("%w: %s", ErrWorkflowRunExists, run.ID)
	}
	return nil
}

// PutWorkflowRun inserts or updates a workflow run. Creation-time metadata is
// immutable for a run id: backlog/session binding, engine, template + version,
// interpreter version, and workflow params. Existing rows may update only
// lifecycle state, cost, and timestamps. The one exception is DAOJournal's
// exact workflow-seed/v0 placeholder: before it has steps or lifecycle changes,
// a real imperative run may atomically promote it to its durable identity. The
// ON CONFLICT predicate makes the check and update one SQLite statement; a
// mismatch updates zero rows and returns WorkflowRunMetadataMismatchError.
func (d *WorkflowDAO) PutWorkflowRun(ctx context.Context, run *WorkflowRun) error {
	if err := validateWorkflowRun(run); err != nil {
		return err
	}

	result, err := d.db.ExecContext(ctx, `
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
		WHERE (
			COALESCE(workflow_runs.backlog_id, '') = COALESCE(excluded.backlog_id, '')
			AND workflow_runs.engine = excluded.engine
			AND workflow_runs.template = excluded.template
			AND workflow_runs.template_version = excluded.template_version
			AND workflow_runs.interpreter_version = excluded.interpreter_version
			AND COALESCE(workflow_runs.workflow_params, '') = COALESCE(excluded.workflow_params, '')
			AND COALESCE(workflow_runs.parent_session_id, '') = COALESCE(excluded.parent_session_id, '')
		) OR (
			-- DAOJournal's lazy parent is a provisional identity only until real
			-- creation wins. Promotion is atomic with this update and is allowed
			-- only before any step or lifecycle/cost mutation makes the run real.
			workflow_runs.engine = 'imperative'
			AND excluded.engine = workflow_runs.engine
			AND workflow_runs.template = 'workflow-seed'
			AND workflow_runs.template_version = 'v0'
			AND workflow_runs.backlog_id IS NULL
			AND workflow_runs.workflow_params IS NULL
			AND workflow_runs.parent_session_id IS NULL
			AND workflow_runs.state = 'running'
			AND workflow_runs.cost_usd = 0
			AND workflow_runs.paused_at IS NULL
			AND workflow_runs.resumed_at IS NULL
			AND workflow_runs.ended_at IS NULL
			AND NOT EXISTS (
				SELECT 1 FROM workflow_steps WHERE run_id = workflow_runs.id
			)
			AND (
				excluded.template != 'workflow-seed'
				OR excluded.template_version != 'v0'
				OR excluded.backlog_id IS NOT NULL
				OR excluded.workflow_params IS NOT NULL
				OR excluded.parent_session_id IS NOT NULL
			)
		)
	`, workflowRunValues(run)...)
	if err != nil {
		return fmt.Errorf("workflow put run %s: %w", run.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("workflow put run %s rows affected: %w", run.ID, err)
	}
	if rows == 0 {
		existing, getErr := d.GetWorkflowRun(ctx, run.ID)
		if getErr != nil {
			return fmt.Errorf("workflow put run %s inspect conflict: %w", run.ID, getErr)
		}
		if mismatch := workflowRunMetadataMismatch(existing, run); mismatch != nil {
			return mismatch
		}
	}
	return nil
}

// CompareAndSetWorkflowRunLifecycle atomically applies a lifecycle transition
// only while the durable row remains in expectedState. It updates no immutable
// metadata and is the fence between an in-flight interpreter completion and an
// operator pause/fail: exactly one transition wins the SQLite write lock.
func (d *WorkflowDAO) CompareAndSetWorkflowRunLifecycle(
	ctx context.Context,
	run *WorkflowRun,
	expectedState WorkflowRunState,
) (bool, error) {
	if run == nil || run.ID == "" || run.State == "" || expectedState == "" {
		return false, errors.New("workflow: lifecycle CAS requires run id, expected state, and next state")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("workflow lifecycle CAS %s: begin: %w", run.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs
		SET state = ?, paused_at = ?, resumed_at = ?, ended_at = ?
		WHERE id = ? AND state = ?
	`, string(run.State), nullTime(run.PausedAt), nullTime(run.ResumedAt), nullTime(run.EndedAt),
		run.ID, string(expectedState))
	if err != nil {
		return false, fmt.Errorf("workflow lifecycle CAS %s %s->%s: %w",
			run.ID, expectedState, run.State, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("workflow lifecycle CAS %s rows affected: %w", run.ID, err)
	}
	if rows == 1 && workflowRunStateTerminal(run.State) {
		if err := settleClaimedWorkflowItemTx(ctx, tx, run); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("workflow lifecycle CAS %s: commit: %w", run.ID, err)
	}
	return rows == 1, nil
}

// workflowRunStateTerminal reports whether a state ends the run for good.
// Paused is NOT terminal (resumable durable work).
func workflowRunStateTerminal(state WorkflowRunState) bool {
	switch state {
	case WorkflowRunDone, WorkflowRunError, WorkflowRunEscalated, WorkflowRunQuarantined:
		return true
	}
	return false
}

// settleClaimedWorkflowItemTx completes the S7 claim lifecycle in the SAME
// transaction as the terminal run-state CAS: the budget reservation is
// released, and the claimed backlog item leaves `running` so quiescence can
// drain. The presence of an ACTIVE reservation for this run id is the claim
// provenance discriminator — only ClaimWorkflowStart writes one, so admin/test
// canaries (which may carry a backlog_id they never claimed) can never
// release someone else's item.
//
// Every terminal outcome escalates the item rather than marking it merged:
// v1 registry templates stop pre-merge, so even a `done` run's work product
// (a spawned agent's branch) needs human review. The escalation reason
// records which.
func settleClaimedWorkflowItemTx(ctx context.Context, tx *sql.Tx, run *WorkflowRun) error {
	released, err := tx.ExecContext(ctx, `
		UPDATE pipeline_budget_reservations
		SET state = 'released', released_at = ?
		WHERE run_id = ? AND state = 'active'
	`, timeRFC3339(time.Now().UTC()), run.ID)
	if err != nil {
		return fmt.Errorf("workflow terminal release reservation %s: %w", run.ID, err)
	}
	releasedRows, err := released.RowsAffected()
	if err != nil {
		return fmt.Errorf("workflow terminal reservation rows %s: %w", run.ID, err)
	}
	if releasedRows == 0 {
		// No active reservation => not a claim-started run (canary/admin) or
		// already settled. Nothing further to do; idempotent by construction.
		return nil
	}

	var backlogID string
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(backlog_id, '') FROM workflow_runs WHERE id = ?`, run.ID,
	).Scan(&backlogID); err != nil {
		return fmt.Errorf("workflow terminal load backlog id %s: %w", run.ID, err)
	}
	if backlogID == "" {
		return nil
	}
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `
		UPDATE backlog_items
		SET state = ?, claim_version = claim_version + 1,
			row_version = row_version + 1, updated_at = ?
		WHERE id = ? AND state = ?
	`, string(BacklogEscalated), timeRFC3339(now), backlogID, string(BacklogRunning))
	if err != nil {
		return fmt.Errorf("workflow terminal escalate item %s: %w", backlogID, err)
	}
	escalated, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("workflow terminal escalate rows %s: %w", backlogID, err)
	}
	if escalated == 0 {
		// The item already left running (operator action). The reservation
		// release above still stands; do not fight the newer state.
		return nil
	}
	var aggregateVersion int64
	if err := tx.QueryRowContext(ctx,
		`SELECT claim_version FROM backlog_items WHERE id = ?`, backlogID,
	).Scan(&aggregateVersion); err != nil {
		return fmt.Errorf("workflow terminal load aggregate version %s: %w", backlogID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pipeline_transitions (
			backlog_id, aggregate_version, run_id, kind,
			from_state, to_state, occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, backlogID, aggregateVersion, run.ID, WorkflowTerminalKind,
		string(BacklogRunning), string(BacklogEscalated), timeRFC3339(now)); err != nil {
		return fmt.Errorf("workflow terminal transition %s: %w", backlogID, err)
	}

	// The escalation is FOR a human: record why the item escalated and where
	// the work product lives, in the same transaction, keyed to the item so
	// every escalation surface that reads the item's events can show it.
	reason := fmt.Sprintf(
		"imperative workflow run %s ended %s; review branch %s%s (v1 templates stop pre-merge — nothing was merged)",
		run.ID, run.State, WorkflowRunBranchPrefix, run.ID)
	payload, err := json.Marshal(map[string]any{
		"run_id":      run.ID,
		"final_state": string(run.State),
		"branch":      WorkflowRunBranchPrefix + run.ID,
		"template":    run.Template,
		"reason":      reason,
	})
	if err != nil {
		return fmt.Errorf("workflow terminal settle payload %s: %w", run.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events (occurred_at, actor, kind, subject_kind, subject_id, payload_json)
		VALUES (?, 'workflow', 'workflow.terminal_settle', 'backlog', ?, ?)
	`, timeRFC3339(now), backlogID, string(payload)); err != nil {
		return fmt.Errorf("workflow terminal settle event %s: %w", backlogID, err)
	}
	return nil
}

// EnsureWorkflowRun inserts a workflow run only when its id does not already
// exist. It deliberately never updates an existing row. DAOJournal uses this
// to satisfy the workflow_steps foreign key without replacing metadata on a
// run that the scheduler or API already created.
func (d *WorkflowDAO) EnsureWorkflowRun(ctx context.Context, run *WorkflowRun) error {
	if err := validateWorkflowRun(run); err != nil {
		return err
	}
	result, err := d.db.ExecContext(ctx, `
		INSERT INTO workflow_runs (`+workflowRunColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO NOTHING
	`, workflowRunValues(run)...)
	if err != nil {
		return fmt.Errorf("workflow ensure run %s: %w", run.ID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("workflow ensure run %s rows affected: %w", run.ID, err)
	}
	if inserted == 0 {
		existing, getErr := d.GetWorkflowRun(ctx, run.ID)
		if getErr != nil {
			return fmt.Errorf("workflow ensure run %s inspect conflict: %w", run.ID, getErr)
		}
		// Ensure is intentionally tolerant of a pre-created run's richer
		// template/version/params, but the dual-source-of-truth boundary is not
		// negotiable: an imperative journal may never append beneath a DAG run.
		if existing.Engine != run.Engine {
			return &WorkflowRunMetadataMismatchError{RunID: run.ID, Fields: []string{"engine"}}
		}
	}
	return nil
}

func validateWorkflowRun(run *WorkflowRun) error {
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
	return nil
}

func workflowRunValues(run *WorkflowRun) []any {
	return []any{
		run.ID, nullStr(run.BacklogID), string(run.Engine), run.Template,
		run.TemplateVersion, run.InterpreterVersion, nullStr(run.WorkflowParams),
		string(run.State), nullTime(run.PausedAt), nullTime(run.ResumedAt),
		nullTime(run.StartedAt), nullTime(run.EndedAt), run.CostUSD,
		nullStr(run.ParentSessionID),
	}
}

func workflowRunMetadataMismatch(existing, incoming *WorkflowRun) error {
	if existing == nil || incoming == nil {
		return nil
	}
	fields := make([]string, 0, 7)
	if existing.BacklogID != incoming.BacklogID {
		fields = append(fields, "backlog_id")
	}
	if existing.Engine != incoming.Engine {
		fields = append(fields, "engine")
	}
	if existing.Template != incoming.Template {
		fields = append(fields, "template")
	}
	if existing.TemplateVersion != incoming.TemplateVersion {
		fields = append(fields, "template_version")
	}
	if existing.InterpreterVersion != incoming.InterpreterVersion {
		fields = append(fields, "interpreter_version")
	}
	if existing.WorkflowParams != incoming.WorkflowParams {
		fields = append(fields, "workflow_params")
	}
	if existing.ParentSessionID != incoming.ParentSessionID {
		fields = append(fields, "parent_session_id")
	}
	if len(fields) == 0 {
		return nil
	}
	return &WorkflowRunMetadataMismatchError{RunID: incoming.ID, Fields: fields}
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

// ListWorkflowRuns returns the most-recent workflow runs across every engine
// and state, newest-first, bounded by limit. "Newest" is ordered by
// COALESCE(started_at, ”) DESC then id DESC so a run with no started_at still
// sorts deterministically (it sinks to the bottom of the same timestamp bucket).
// A non-positive limit falls back to a sane default so the list endpoint never
// returns an unbounded payload. This powers GET /api/mills/workflow/runs (the
// HUD step-log list), the read-only counterpart to ListRunningImperativeRuns
// (which the scheduler uses to drive only running imperative work).
func (d *WorkflowDAO) ListWorkflowRuns(ctx context.Context, limit int) ([]*WorkflowRun, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+workflowRunColumns+` FROM workflow_runs
		 ORDER BY COALESCE(started_at, '') DESC, id DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("workflow list runs: %w", err)
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

// CountStepsByRun returns the number of journaled steps for each given run id,
// keyed by run_id. A run id with no steps is ABSENT from the returned map (the
// caller reads a missing key as 0). A nil/empty input returns an empty map
// without touching the DB. This is the batch companion to ListWorkflowRuns:
// it powers the step_count column in the run-LIST payload
// (GET /api/mills/workflow/runs) with a single GROUP BY over the bounded id set
// the list endpoint already fetched, so the list never fans out into one count
// query per run (no N+1 scan of the journal).
func (d *WorkflowDAO) CountStepsByRun(ctx context.Context, runIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(runIDs))
	if len(runIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(runIDs))
	args := make([]any, len(runIDs))
	for i, id := range runIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	// #nosec G202 -- IN clause is built from "?" placeholders only; values are bound via args
	rows, err := d.db.QueryContext(ctx, `
		SELECT run_id, COUNT(*) FROM workflow_steps
		WHERE run_id IN (`+strings.Join(placeholders, ",")+`)
		GROUP BY run_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("workflow count steps by run: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var runID string
		var n int
		if err := rows.Scan(&runID, &n); err != nil {
			return nil, fmt.Errorf("workflow count steps by run scan: %w", err)
		}
		out[runID] = n
	}
	return out, rows.Err()
}

// CountRunsByState returns the number of workflow runs currently in the given
// state, across every engine. Used by the KPI snapshot
// (workflow_quarantined_runs) and the WorkflowMonitor active-run count. This is
// a point-in-time count (no time window): run states are mutable, so "how many
// are quarantined right now" is the meaningful question, not "how many entered
// quarantine in the last 24h".
func (d *WorkflowDAO) CountRunsByState(ctx context.Context, state WorkflowRunState) (int, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_runs WHERE state = ?`, string(state))
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("workflow count runs by state %s: %w", state, err)
	}
	return n, nil
}

// StepCostRollup is the COST-SOURCE-BRANCHED aggregate of step costs over a
// time window. It deliberately does NOT expose a single blended total: summing
// `real` + `estimated` + `unavailable`(=0) as if comparable is the exact error
// this type exists to prevent. Callers (KPI writer) roll up only the buckets
// they can defend — by default just `real`.
type StepCostRollup struct {
	// RealCostUSD is the sum of cost_usd for steps whose cost_source='real'.
	RealCostUSD float64
	// RealSteps is the count of cost_source='real' steps (the denominator for
	// an honest average cost-per-step). Steps with estimated/unavailable cost
	// are excluded so the average is not diluted by non-comparable figures.
	RealSteps int
	// EstimatedCostUSD is the sum of cost_usd for steps whose
	// cost_source='estimated'. Surfaced SEPARATELY (never folded into Real) so
	// an operator can see estimated burn without it contaminating the real
	// rollup.
	EstimatedCostUSD float64
	// EstimatedSteps is the count of cost_source='estimated' steps.
	EstimatedSteps int
	// UnavailableSteps is the count of steps with cost_source='unavailable' (or
	// NULL). Their cost_usd is 0 by contract; the count is kept so the operator
	// can see how much of the journal has no usable cost figure at all.
	UnavailableSteps int
}

// StepCostRollupSince aggregates step costs in the window [since, now), BRANCHED
// on cost_source. This is the load-bearing primitive behind the
// CostSource-aware KPI counters: a single GROUP BY over cost_source so the
// caller can roll up `real`, surface `estimated` separately, and never blend
// the two. NULL cost_source is treated as 'unavailable' (the schema default
// intent). The window is keyed on COALESCE(started_at, ended_at) so a step that
// only carries an ended_at still falls in the right bucket.
func (d *WorkflowDAO) StepCostRollupSince(ctx context.Context, since time.Time) (StepCostRollup, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT COALESCE(cost_source, 'unavailable') AS src,
		       COALESCE(SUM(cost_usd), 0) AS total,
		       COUNT(*) AS n
		FROM workflow_steps
		WHERE COALESCE(started_at, ended_at, '') >= ?
		GROUP BY src
	`, timeRFC3339(since.UTC()))
	if err != nil {
		return StepCostRollup{}, fmt.Errorf("workflow step cost rollup: %w", err)
	}
	defer rows.Close()
	var out StepCostRollup
	for rows.Next() {
		var src string
		var total float64
		var n int
		if err := rows.Scan(&src, &total, &n); err != nil {
			return StepCostRollup{}, fmt.Errorf("workflow step cost rollup scan: %w", err)
		}
		switch WorkflowCostSource(src) {
		case WorkflowCostReal:
			out.RealCostUSD += total
			out.RealSteps += n
		case WorkflowCostEstimated:
			out.EstimatedCostUSD += total
			out.EstimatedSteps += n
		default: // unavailable or any unexpected value: count only.
			out.UnavailableSteps += n
		}
	}
	return out, rows.Err()
}

// CountStepsByStatusSince returns the number of steps that reached the given
// terminal status with an ended_at in [since, now). Drives the
// workflow_completed_steps / workflow_failed_steps KPI counters. Pending steps
// have no ended_at and are excluded.
func (d *WorkflowDAO) CountStepsByStatusSince(ctx context.Context, status WorkflowStepStatus, since time.Time) (int, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_steps
		WHERE status = ? AND COALESCE(ended_at, '') >= ?
	`, string(status), timeRFC3339(since.UTC()))
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("workflow count steps by status %s: %w", status, err)
	}
	return n, nil
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

	// Insert-first makes concurrent first appends idempotent at the UNIQUE
	// constraint instead of racing a preceding SELECT. A conflict is normal:
	// the guarded update below either enriches the same pending call or advances
	// it to terminal, and a final read distinguishes replay from hash mismatch.
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO workflow_steps (run_id, step_key, event_type, call_hash,
			idempotency_key, status, spawn_id, started_at, ended_at, result_blob,
			cost_usd, cost_source, effect_count)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id, step_key) DO NOTHING
	`,
		step.RunID, step.StepKey, string(step.EventType), step.CallHash,
		nullStr(step.IdempotencyKey), string(step.Status), nullStr(step.SpawnID),
		nullTime(step.StartedAt), nullTime(step.EndedAt), nullStr(step.ResultBlob),
		step.CostUSD, nullStr(string(step.CostSource)), step.EffectCount,
	)
	if err != nil {
		return nil, fmt.Errorf("workflow append %s/%s: %w", step.RunID, step.StepKey, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("workflow append %s/%s rows affected: %w", step.RunID, step.StepKey, err)
	}
	if inserted == 0 {
		if step.Status.IsTerminal() {
			err = d.completePendingStep(ctx, step)
		} else {
			err = d.enrichPendingStep(ctx, step)
		}
		if err != nil {
			return nil, err
		}
	}
	out, err := d.GetStep(ctx, step.RunID, step.StepKey)
	if err != nil {
		return nil, err
	}
	if out.CallHash != step.CallHash {
		return out, fmt.Errorf("%w: run=%s key=%s existing=%s incoming=%s",
			ErrStepCallHashMismatch, step.RunID, step.StepKey, out.CallHash, step.CallHash)
	}
	if step.Status.IsTerminal() && out.Status.IsTerminal() {
		if conflict := workflowStepTerminalConflict(out, step); conflict != nil {
			return out, conflict
		}
	}
	step.ID = out.ID
	return out, nil
}

func workflowStepTerminalConflict(stored, incoming *WorkflowStep) error {
	if stored == nil || incoming == nil {
		return nil
	}
	fields := make([]string, 0, 8)
	if stored.Status != incoming.Status {
		fields = append(fields, "status")
	}
	if stored.EventType != incoming.EventType {
		fields = append(fields, "event_type")
	}
	if stored.ResultBlob != incoming.ResultBlob {
		fields = append(fields, "result_blob")
	}
	if stored.CostUSD != incoming.CostUSD {
		fields = append(fields, "cost_usd")
	}
	if incoming.CostSource != "" && stored.CostSource != incoming.CostSource {
		fields = append(fields, "cost_source")
	}
	if stored.EffectCount != incoming.EffectCount {
		fields = append(fields, "effect_count")
	}
	if incoming.IdempotencyKey != "" && stored.IdempotencyKey != incoming.IdempotencyKey {
		fields = append(fields, "idempotency_key")
	}
	if incoming.SpawnID != "" && stored.SpawnID != incoming.SpawnID {
		fields = append(fields, "spawn_id")
	}
	if len(fields) == 0 {
		return nil
	}
	return &WorkflowStepTerminalConflictError{
		RunID: incoming.RunID, StepKey: incoming.StepKey, Fields: fields,
	}
}

// completePendingStep atomically advances only a same-hash pending row. Its
// non-null execution handles remain authoritative while event_type advances
// with the lifecycle (for example spawn_requested to spawn_result); the final
// read reports any terminal disagreement. A terminal row is immutable, so
// concurrent/replayed completions keep the first committed terminal result.
func (d *WorkflowDAO) completePendingStep(ctx context.Context, step *WorkflowStep) error {
	_, err := d.db.ExecContext(ctx, `
		UPDATE workflow_steps SET
			event_type      = ?,
			idempotency_key = COALESCE(idempotency_key, ?),
			status          = ?,
			spawn_id        = COALESCE(spawn_id, ?),
			started_at      = COALESCE(started_at, ?),
			ended_at        = ?,
			result_blob     = ?,
			cost_usd        = ?,
			cost_source     = COALESCE(cost_source, ?),
			effect_count    = ?
		WHERE run_id = ? AND step_key = ? AND call_hash = ? AND status = ?
	`,
		string(step.EventType), nullStr(step.IdempotencyKey), string(step.Status),
		nullStr(step.SpawnID), nullTime(step.StartedAt), nullTime(step.EndedAt),
		nullStr(step.ResultBlob), step.CostUSD, nullStr(string(step.CostSource)),
		step.EffectCount, step.RunID, step.StepKey, step.CallHash, string(WorkflowStepPending),
	)
	if err != nil {
		return fmt.Errorf("workflow complete step %s/%s: %w", step.RunID, step.StepKey, err)
	}
	return nil
}

// enrichPendingStep fills crash-recovery provenance (notably spawn_id) on an
// already-recorded same-hash pending row without clearing values supplied by
// the first writer. It cannot modify terminal rows.
func (d *WorkflowDAO) enrichPendingStep(ctx context.Context, step *WorkflowStep) error {
	_, err := d.db.ExecContext(ctx, `
		UPDATE workflow_steps SET
			idempotency_key = COALESCE(idempotency_key, ?),
			spawn_id        = COALESCE(spawn_id, ?),
			started_at      = COALESCE(started_at, ?),
			cost_source     = COALESCE(cost_source, ?)
		WHERE run_id = ? AND step_key = ? AND call_hash = ? AND status = ?
	`,
		nullStr(step.IdempotencyKey), nullStr(step.SpawnID), nullTime(step.StartedAt),
		nullStr(string(step.CostSource)), step.RunID, step.StepKey, step.CallHash,
		string(WorkflowStepPending),
	)
	if err != nil {
		return fmt.Errorf("workflow enrich pending step %s/%s: %w", step.RunID, step.StepKey, err)
	}
	return nil
}

// QuarantineStep is the explicit exception to terminal-step immutability. The
// nondeterminism tripwire freezes one already-recorded same-hash step by
// changing only its status to error; result/provenance fields remain intact.
// The call is idempotent and guarded by call_hash so it cannot quarantine a
// different logical call that happens to reuse the same step key.
func (d *WorkflowDAO) QuarantineStep(ctx context.Context, runID, stepKey, callHash string) (*WorkflowStep, error) {
	if runID == "" {
		return nil, errors.New("workflow: quarantine runID required")
	}
	if stepKey == "" {
		return nil, errors.New("workflow: quarantine stepKey required")
	}
	if callHash == "" {
		return nil, errors.New("workflow: quarantine callHash required")
	}
	_, err := d.db.ExecContext(ctx, `
		UPDATE workflow_steps SET status = ?
		WHERE run_id = ? AND step_key = ? AND call_hash = ? AND status != ?
	`, string(WorkflowStepError), runID, stepKey, callHash, string(WorkflowStepError))
	if err != nil {
		return nil, fmt.Errorf("workflow quarantine step %s/%s: %w", runID, stepKey, err)
	}
	stored, err := d.GetStep(ctx, runID, stepKey)
	if err != nil {
		return nil, err
	}
	if stored.CallHash != callHash {
		return stored, fmt.Errorf("%w: run=%s key=%s existing=%s incoming=%s",
			ErrStepCallHashMismatch, runID, stepKey, stored.CallHash, callHash)
	}
	return stored, nil
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
