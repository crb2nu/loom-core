package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrClaimConflict means the backlog row no longer matches the queued state
	// and claim version observed by the caller. It is an expected race outcome,
	// not a store failure.
	ErrClaimConflict = errors.New("pipeline start: backlog claim conflict")
	// ErrScopeReservationConflict means an older queued item has reserved an
	// overlapping scope. ClaimPipelineStart returns this stable reason before
	// attempting the backlog CAS, so a stale revision cannot mask writer
	// preference as ErrClaimConflict.
	ErrScopeReservationConflict = errors.New("pipeline start: scope reservation conflict")
	// ErrInvalidClaim reports malformed transactional-admission input.
	ErrInvalidClaim = errors.New("pipeline start: invalid claim")
	// ErrDispatchClaimConflict means an outbox row is not currently claimable
	// or the caller no longer owns its delivery token.
	ErrDispatchClaimConflict = errors.New("pending dispatch: claim conflict")
)

const (
	PipelineStartDispatchKind      = "pipeline_start"
	PipelineDispatchDeadLetterKind = "pipeline_dispatch_dead_letter"
	reservationStateActive         = "active"
	reservationStateReleased       = "released"
	dagTemplateVersion             = "v1"
	dagInterpreterVersion          = "pipeline-runner/v1"
)

// BudgetExceededError reports every cap rejected by one serialized budget
// snapshot. The numeric fields make the decision explainable without re-reading
// the store after the winning transaction has changed it.
type BudgetExceededError struct {
	Reasons     []string
	SpentUSD    float64
	ReservedUSD float64
	Runs        int
	ActiveRuns  int
	Limits      PipelineStartLimits
}

func (e *BudgetExceededError) Error() string {
	if e == nil || len(e.Reasons) == 0 {
		return "pipeline start: budget exceeded"
	}
	return "pipeline start: budget exceeded: " + strings.Join(e.Reasons, "; ")
}

// ScopeConflictError reports an overlapping running backlog item found after
// the claim CAS acquired SQLite's writer slot. Returning it rolls the claim
// back, so the caller may safely classify it as a deferred admission.
type ScopeConflictError struct {
	BacklogID string
	BlockerID string
	Witness   string
}

// ScopeReservationConflictError identifies the queued item whose reservation
// rejected admission. It is errors.Is-matchable with
// ErrScopeReservationConflict and is deliberately distinct from a running
// scope conflict.
type ScopeReservationConflictError struct {
	BacklogID string
	BlockerID string
	Witness   string
}

func (e *ScopeReservationConflictError) Error() string {
	if e == nil {
		return ErrScopeReservationConflict.Error()
	}
	return fmt.Sprintf("%s: backlog %s overlaps reserved backlog %s at %s",
		ErrScopeReservationConflict, e.BacklogID, e.BlockerID, e.Witness)
}

func (e *ScopeReservationConflictError) Unwrap() error { return ErrScopeReservationConflict }

func (e *ScopeConflictError) Error() string {
	if e == nil {
		return "pipeline start: scope conflict"
	}
	return fmt.Sprintf("pipeline start: backlog %s overlaps running backlog %s at %s",
		e.BacklogID, e.BlockerID, e.Witness)
}

// ClaimPipelineStartFaultPoint names deterministic crash boundaries in the
// transaction. Tests inject an error after each SQL statement and assert the
// backlog CAS and every dependent row roll back together.
type ClaimPipelineStartFaultPoint string

const (
	ClaimFaultAfterBacklogCAS       ClaimPipelineStartFaultPoint = "after_backlog_cas"
	ClaimFaultAfterBacklogLoad      ClaimPipelineStartFaultPoint = "after_backlog_load"
	ClaimFaultAfterScopeCheck       ClaimPipelineStartFaultPoint = "after_scope_check"
	ClaimFaultAfterBudgetSnapshot   ClaimPipelineStartFaultPoint = "after_budget_snapshot"
	ClaimFaultAfterAttemptAllocated ClaimPipelineStartFaultPoint = "after_attempt_allocated"
	ClaimFaultAfterPipelineInsert   ClaimPipelineStartFaultPoint = "after_pipeline_insert"
	ClaimFaultAfterReservation      ClaimPipelineStartFaultPoint = "after_reservation_insert"
	ClaimFaultAfterWorkflowInsert   ClaimPipelineStartFaultPoint = "after_workflow_insert"
	ClaimFaultAfterTransition       ClaimPipelineStartFaultPoint = "after_transition_insert"
	ClaimFaultAfterDispatch         ClaimPipelineStartFaultPoint = "after_dispatch_insert"
)

var claimPipelineStartFaultPoints = []ClaimPipelineStartFaultPoint{
	ClaimFaultAfterBacklogCAS,
	ClaimFaultAfterBacklogLoad,
	ClaimFaultAfterScopeCheck,
	ClaimFaultAfterBudgetSnapshot,
	ClaimFaultAfterAttemptAllocated,
	ClaimFaultAfterPipelineInsert,
	ClaimFaultAfterReservation,
	ClaimFaultAfterWorkflowInsert,
	ClaimFaultAfterTransition,
	ClaimFaultAfterDispatch,
}

// ClaimPipelineStartFaultHook is a deterministic failure-injection seam. It is
// nil in production.
type ClaimPipelineStartFaultHook func(ClaimPipelineStartFaultPoint) error

// ClaimPipelineStartRequest is the complete immutable input to one admission
// attempt. ExpectedClaimVersion comes from BacklogItem.ClaimVersion. Limits <= 0
// remain uncapped, matching the existing policy semantics.
type ClaimPipelineStartRequest struct {
	BacklogID                  string
	ExpectedClaimVersion       int64
	ExpectedRevision           int64
	SerializeOverlappingScopes bool
	EnforceScopeReservations   bool
	HomeProject                string
	Template                   string
	EstimateUSD                float64
	Limits                     PipelineStartLimits
	RunID                      string
	ParentSessionID            string
	Now                        time.Time
	FaultHook                  ClaimPipelineStartFaultHook
}

// ClaimPipelineStartResult contains the rows committed by one successful
// aggregate transition.
type ClaimPipelineStartResult struct {
	Backlog     *BacklogItem
	Run         *PipelineRun
	WorkflowRun *WorkflowRun
	Reservation *PipelineBudgetReservation
	Dispatch    *PendingDispatch
}

type pipelineStartBudgetSnapshot struct {
	spentUSD    float64
	reservedUSD float64
	runs        int
	activeRuns  int
}

// ClaimPipelineStart atomically claims one queued backlog item, reserves its
// budget and capacity, materializes the pipeline/DAG workflow rows, records the
// transition, and commits one pending dispatch intent. No PipelineStarter may be
// invoked until this method returns successfully.
func (s *Store) ClaimPipelineStart(ctx context.Context, req ClaimPipelineStartRequest) (*ClaimPipelineStartResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("%w: store is nil", ErrInvalidClaim)
	}
	req.BacklogID = strings.TrimSpace(req.BacklogID)
	req.Template = strings.TrimSpace(req.Template)
	req.ParentSessionID = strings.TrimSpace(req.ParentSessionID)
	req.RunID = strings.TrimSpace(req.RunID)
	req.HomeProject = strings.TrimSpace(req.HomeProject)
	if err := validateClaimPipelineStartRequest(req); err != nil {
		return nil, err
	}

	// Reservation rejection has precedence over the claim CAS contract. Read
	// the current queued envelope and reject an existing reservation before any
	// revision-bumping write. The same predicate is checked again after the CAS
	// below to close the concurrent-reservation race transactionally.
	if req.SerializeOverlappingScopes && req.EnforceScopeReservations {
		item, err := scanBacklog(s.db.QueryRowContext(ctx,
			`SELECT `+backlogColumns+` FROM backlog_items WHERE id = ?`, req.BacklogID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("pipeline start: load reservation candidate: %w", err)
		}
		if err == nil && item.State == BacklogQueued {
			conflict, err := findPipelineStartReservationConflict(ctx, s.db, item, req.HomeProject)
			if err != nil {
				return nil, err
			}
			if conflict != nil {
				return nil, conflict
			}
		}
	}

	now := req.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	runID := req.RunID
	if runID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return nil, fmt.Errorf("pipeline start: generate run id: %w", err)
		}
		runID = "PIPE-" + req.BacklogID + "-" + id.String()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pipeline start: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// This CAS is deliberately the first SQL statement in the transaction. It
	// serializes contenders before any budget or attempt snapshot is observed.
	res, err := tx.ExecContext(ctx, `
		UPDATE backlog_items
		SET state = ?, claim_version = claim_version + 1,
			row_version = row_version + 1, updated_at = ?
		WHERE id = ? AND state = ? AND claim_version = ? AND row_version = ?
	`, string(BacklogRunning), timeRFC3339(now), req.BacklogID,
		string(BacklogQueued), req.ExpectedClaimVersion, req.ExpectedRevision)
	if err != nil {
		return nil, fmt.Errorf("pipeline start: claim backlog %s: %w", req.BacklogID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("pipeline start: claim rows affected: %w", err)
	}
	if rows != 1 {
		return nil, fmt.Errorf("%w: backlog=%s expected_claim_version=%d expected_revision=%d",
			ErrClaimConflict, req.BacklogID, req.ExpectedClaimVersion, req.ExpectedRevision)
	}
	if err := runClaimPipelineStartFault(req.FaultHook, ClaimFaultAfterBacklogCAS); err != nil {
		return nil, err
	}

	item, err := scanBacklog(tx.QueryRowContext(ctx,
		`SELECT `+backlogColumns+` FROM backlog_items WHERE id = ?`, req.BacklogID))
	if err != nil {
		return nil, fmt.Errorf("pipeline start: load claimed backlog: %w", err)
	}
	if err := runClaimPipelineStartFault(req.FaultHook, ClaimFaultAfterBacklogLoad); err != nil {
		return nil, err
	}
	aggregateVersion := item.ClaimVersion

	if req.SerializeOverlappingScopes {
		conflict, err := findPipelineStartScopeConflict(ctx, tx, item, req.HomeProject)
		if err != nil {
			return nil, err
		}
		if err := runClaimPipelineStartFault(req.FaultHook, ClaimFaultAfterScopeCheck); err != nil {
			return nil, err
		}
		if conflict != nil {
			return nil, conflict
		}
		if req.EnforceScopeReservations {
			reservationConflict, err := findPipelineStartReservationConflict(ctx, tx, item, req.HomeProject)
			if err != nil {
				return nil, err
			}
			if reservationConflict != nil {
				return nil, reservationConflict
			}
		}
	}

	snapshot, err := readPipelineStartBudgetSnapshot(ctx, tx, now.Add(-24*time.Hour), req.Limits)
	if err != nil {
		return nil, err
	}
	if err := runClaimPipelineStartFault(req.FaultHook, ClaimFaultAfterBudgetSnapshot); err != nil {
		return nil, err
	}
	if exceeded := evaluatePipelineStartBudget(req, snapshot); exceeded != nil {
		return nil, exceeded
	}

	var attempt int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(attempts), 0) + 1
		FROM pipeline_runs
		WHERE backlog_id = ?
	`, req.BacklogID).Scan(&attempt); err != nil {
		return nil, fmt.Errorf("pipeline start: allocate attempt: %w", err)
	}
	if err := runClaimPipelineStartFault(req.FaultHook, ClaimFaultAfterAttemptAllocated); err != nil {
		return nil, err
	}

	run := &PipelineRun{
		ID:               runID,
		BacklogID:        req.BacklogID,
		AggregateVersion: aggregateVersion,
		Revision:         1,
		Template:         req.Template,
		State:            PipelineQueued,
		Attempts:         attempt,
		StartedAt:        now,
		ParentSessionID:  req.ParentSessionID,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pipeline_runs (
			id, backlog_id, aggregate_version, row_version, template, state, attempts,
			started_at, cost_usd, parent_session_id, depth
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0)
	`, run.ID, run.BacklogID, run.AggregateVersion, run.Revision, run.Template,
		string(run.State), run.Attempts, timeRFC3339(run.StartedAt),
		nullStr(run.ParentSessionID)); err != nil {
		return nil, fmt.Errorf("pipeline start: insert run %s: %w", run.ID, err)
	}
	if err := runClaimPipelineStartFault(req.FaultHook, ClaimFaultAfterPipelineInsert); err != nil {
		return nil, err
	}

	reservationResult, err := tx.ExecContext(ctx, `
		INSERT INTO pipeline_budget_reservations (
			run_id, backlog_id, reserved_usd, state, created_at
		) VALUES (?, ?, ?, ?, ?)
	`, run.ID, run.BacklogID, req.EstimateUSD, reservationStateActive, timeRFC3339(now))
	if err != nil {
		return nil, fmt.Errorf("pipeline start: reserve budget %s: %w", run.ID, err)
	}
	reservationID, err := reservationResult.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("pipeline start: reservation id: %w", err)
	}
	if err := runClaimPipelineStartFault(req.FaultHook, ClaimFaultAfterReservation); err != nil {
		return nil, err
	}
	reservation := &PipelineBudgetReservation{
		ID:          reservationID,
		RunID:       run.ID,
		BacklogID:   run.BacklogID,
		ReservedUSD: req.EstimateUSD,
		State:       reservationStateActive,
		CreatedAt:   now,
	}

	startedAt := now
	workflowRun := &WorkflowRun{
		ID:                 run.ID,
		BacklogID:          run.BacklogID,
		Engine:             WorkflowEngineDAG,
		Template:           run.Template,
		TemplateVersion:    dagTemplateVersion,
		InterpreterVersion: dagInterpreterVersion,
		WorkflowParams:     fmt.Sprintf(`{"aggregate_version":%d}`, aggregateVersion),
		State:              WorkflowRunRunning,
		StartedAt:          &startedAt,
		ParentSessionID:    run.ParentSessionID,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_runs (
			id, backlog_id, engine, template, template_version,
			interpreter_version, workflow_params, state, started_at,
			cost_usd, parent_session_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
	`, workflowRun.ID, nullStr(workflowRun.BacklogID), string(workflowRun.Engine),
		workflowRun.Template, workflowRun.TemplateVersion, workflowRun.InterpreterVersion,
		nullStr(workflowRun.WorkflowParams), string(workflowRun.State),
		timeRFC3339(startedAt), nullStr(workflowRun.ParentSessionID)); err != nil {
		return nil, fmt.Errorf("pipeline start: insert DAG workflow %s: %w", run.ID, err)
	}
	if err := runClaimPipelineStartFault(req.FaultHook, ClaimFaultAfterWorkflowInsert); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pipeline_transitions (
			backlog_id, aggregate_version, run_id, kind,
			from_state, to_state, occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, run.BacklogID, aggregateVersion, run.ID, PipelineStartDispatchKind,
		string(BacklogQueued), string(BacklogRunning), timeRFC3339(now)); err != nil {
		return nil, fmt.Errorf("pipeline start: insert transition %s/%d: %w",
			run.BacklogID, aggregateVersion, err)
	}
	if err := runClaimPipelineStartFault(req.FaultHook, ClaimFaultAfterTransition); err != nil {
		return nil, err
	}

	dispatchResult, err := tx.ExecContext(ctx, `
		INSERT INTO pending_dispatches (
			run_id, backlog_id, aggregate_version, kind, status,
			attempts, next_attempt_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)
	`, run.ID, run.BacklogID, aggregateVersion, PipelineStartDispatchKind,
		string(DispatchPending), timeRFC3339(now), timeRFC3339(now), timeRFC3339(now))
	if err != nil {
		return nil, fmt.Errorf("pipeline start: insert dispatch %s: %w", run.ID, err)
	}
	dispatchID, err := dispatchResult.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("pipeline start: dispatch id: %w", err)
	}
	if err := runClaimPipelineStartFault(req.FaultHook, ClaimFaultAfterDispatch); err != nil {
		return nil, err
	}
	dispatch := &PendingDispatch{
		ID:               dispatchID,
		RunID:            run.ID,
		BacklogID:        run.BacklogID,
		AggregateVersion: aggregateVersion,
		Kind:             PipelineStartDispatchKind,
		Status:           DispatchPending,
		NextAttemptAt:    now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	// Starting the reserved writer releases its fairness state in the same
	// transaction as the claim. A separate best-effort delete can silently
	// leave stale aging state behind when the claim has already committed.
	if _, err := tx.ExecContext(ctx, `DELETE FROM scope_fairness_state WHERE backlog_id = ?`, req.BacklogID); err != nil {
		return nil, fmt.Errorf("pipeline start: clear scope fairness %s: %w", req.BacklogID, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pipeline start: commit %s: %w", run.ID, err)
	}
	return &ClaimPipelineStartResult{
		Backlog:     item,
		Run:         run,
		WorkflowRun: workflowRun,
		Reservation: reservation,
		Dispatch:    dispatch,
	}, nil
}

func validateClaimPipelineStartRequest(req ClaimPipelineStartRequest) error {
	if strings.TrimSpace(req.BacklogID) == "" {
		return fmt.Errorf("%w: backlog id required", ErrInvalidClaim)
	}
	if req.ExpectedClaimVersion < 0 {
		return fmt.Errorf("%w: expected claim version must be >= 0", ErrInvalidClaim)
	}
	if req.ExpectedClaimVersion == math.MaxInt64 {
		return fmt.Errorf("%w: expected claim version cannot be MaxInt64", ErrInvalidClaim)
	}
	if req.ExpectedRevision < 0 {
		return fmt.Errorf("%w: expected revision must be >= 0", ErrInvalidClaim)
	}
	if req.ExpectedRevision == math.MaxInt64 {
		return fmt.Errorf("%w: expected revision cannot be MaxInt64", ErrInvalidClaim)
	}
	if strings.TrimSpace(req.Template) == "" {
		return fmt.Errorf("%w: template required", ErrInvalidClaim)
	}
	if !isFiniteNonNegative(req.EstimateUSD) {
		return fmt.Errorf("%w: estimate USD must be finite and >= 0", ErrInvalidClaim)
	}
	if !isFiniteNonNegative(req.Limits.MaxUSDPerRun) ||
		!isFiniteNonNegative(req.Limits.MaxUSDPerDay) ||
		req.Limits.MaxRunsPerDay < 0 || req.Limits.MaxConcurrentRuns < 0 {
		return fmt.Errorf("%w: limits must be finite and >= 0", ErrInvalidClaim)
	}
	return nil
}

func findPipelineStartScopeConflict(
	ctx context.Context,
	tx *sql.Tx,
	item *BacklogItem,
	homeProject string,
) (*ScopeConflictError, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT `+backlogColumns+`
		FROM backlog_items
		WHERE state = ? AND id <> ?
		ORDER BY id ASC
	`, string(BacklogRunning), item.ID)
	if err != nil {
		return nil, fmt.Errorf("pipeline start: scope check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		other, err := scanBacklog(rows)
		if err != nil {
			return nil, fmt.Errorf("pipeline start: scope check scan: %w", err)
		}
		if overlaps, witness := BacklogScopesOverlap(item, other, homeProject); overlaps {
			return &ScopeConflictError{
				BacklogID: item.ID,
				BlockerID: other.ID,
				Witness:   witness,
			}, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline start: scope check rows: %w", err)
	}
	return nil, nil
}

// findPipelineStartReservationConflict closes the race between the
// reconciler's advisory reservation check and the atomic start claim. A
// reservation created by another reconciler before this transaction obtains
// its write lock must win over a newer overlapping admission.
type pipelineStartQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func findPipelineStartReservationConflict(
	ctx context.Context,
	q pipelineStartQueryer,
	item *BacklogItem,
	homeProject string,
) (*ScopeReservationConflictError, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+backlogColumns+`
		FROM backlog_items b
		JOIN scope_fairness_state s ON s.backlog_id = b.id
		WHERE b.state = ? AND b.id <> ? AND s.reserved_at IS NOT NULL
		ORDER BY s.reserved_at ASC, b.id ASC
	`, string(BacklogQueued), item.ID)
	if err != nil {
		return nil, fmt.Errorf("pipeline start: scope reservation check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		other, err := scanBacklog(rows)
		if err != nil {
			return nil, fmt.Errorf("pipeline start: scope reservation scan: %w", err)
		}
		if hit, witness := BacklogScopesOverlap(item, other, homeProject); hit {
			return &ScopeReservationConflictError{
				BacklogID: item.ID,
				BlockerID: other.ID,
				Witness:   witness,
			}, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline start: scope reservation rows: %w", err)
	}
	return nil, nil
}

func runClaimPipelineStartFault(hook ClaimPipelineStartFaultHook, point ClaimPipelineStartFaultPoint) error {
	if hook == nil {
		return nil
	}
	if err := hook(point); err != nil {
		return fmt.Errorf("pipeline start: injected fault %s: %w", point, err)
	}
	return nil
}

func readPipelineStartBudgetSnapshot(
	ctx context.Context,
	tx *sql.Tx,
	since time.Time,
	limits PipelineStartLimits,
) (pipelineStartBudgetSnapshot, error) {
	q, args := buildPipelineStartBudgetSnapshotQuery(since, limits)
	var out pipelineStartBudgetSnapshot
	if err := tx.QueryRowContext(ctx, q, args...).Scan(
		&out.spentUSD, &out.reservedUSD, &out.runs, &out.activeRuns,
	); err != nil {
		return pipelineStartBudgetSnapshot{}, fmt.Errorf("pipeline start: budget snapshot: %w", err)
	}
	return out, nil
}

// buildPipelineStartBudgetSnapshotQuery preserves one deterministic SQL
// boundary for fault injection while replacing uncapped dimensions with
// constants. Claims with no daily cap therefore never scan pipeline history,
// and concurrency-only claims use the covering state index via a positive IN
// predicate rather than scanning the terminal complement.
func buildPipelineStartBudgetSnapshotQuery(since time.Time, limits PipelineStartLimits) (string, []any) {
	expressions := []string{"0", "0", "0", "0"}
	args := make([]any, 0, 5)
	if limits.MaxUSDPerDay > 0 {
		expressions[0] = `COALESCE((
			SELECT SUM(pr.cost_usd)
			FROM pipeline_runs pr
			WHERE pr.started_at >= ?
		), 0)`
		// The reservation table is shared with the S7 imperative start kernel
		// (ClaimWorkflowStart), so an active reservation may belong to either
		// a pipeline run or an imperative workflow run. Both LEFT JOINs keep
		// every active reservation visible — a reservation held by the other
		// lane must still count against this lane's daily cap, or concurrent
		// pipeline + imperative claims could jointly oversubscribe it. The
		// engine predicate excludes DAG mirror rows (same id as the pipeline
		// run) from double-matching.
		expressions[1] = `COALESCE((
			SELECT SUM(MAX(r.reserved_usd - COALESCE(pr.cost_usd, wr.cost_usd, 0), 0))
			FROM pipeline_budget_reservations r
			LEFT JOIN pipeline_runs pr ON pr.id = r.run_id
			LEFT JOIN workflow_runs wr ON wr.id = r.run_id AND wr.engine = 'imperative'
			WHERE r.state = 'active' AND r.created_at >= ?
		), 0)`
		args = append(args, timeRFC3339(since), timeRFC3339(since))
	}
	if limits.MaxRunsPerDay > 0 {
		expressions[2] = `COALESCE((
			SELECT COUNT(*)
			FROM pipeline_runs pr
			WHERE pr.started_at >= ?
			  AND NOT (
				pr.state = 'escalated'
				AND (
					(pr.cost_usd = 0 AND COALESCE(pr.escalation_class, '') = ?)
					OR COALESCE(pr.escalation_class, '') = ?
				)
			  )
		), 0)`
		args = append(args, timeRFC3339(since), escalationClassNoWorkQuota, escalationClassTerminalConfig)
	}
	if limits.MaxConcurrentRuns > 0 {
		expressions[3] = `COALESCE((
			SELECT COUNT(*)
			FROM pipeline_runs pr
			WHERE pr.state IN (
				'queued', 'planning', 'slicing', 'implementing', 'testing',
				'reviewing', 'mr', 'ci', 'merging'
			)
		), 0)`
	}
	return "SELECT\n\t" + strings.Join(expressions, ",\n\t"), args
}

func evaluatePipelineStartBudget(req ClaimPipelineStartRequest, snap pipelineStartBudgetSnapshot) *BudgetExceededError {
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

const pendingDispatchColumns = `id, run_id, backlog_id, aggregate_version, kind, status,
		attempts, last_error, next_attempt_at, lease_token, lease_expires_at,
		created_at, updated_at, delivered_at, dead_lettered_at`

const defaultDispatchLeaseDuration = time.Minute

// DefaultDispatchRetryPolicy returns the bounded retry policy used by the
// reconciler. Tests and repair tools may pass a tighter policy explicitly.
func DefaultDispatchRetryPolicy() DispatchRetryPolicy {
	return DispatchRetryPolicy{
		BaseDelay:   time.Second,
		MaxDelay:    5 * time.Minute,
		MaxAttempts: 8,
	}
}

// DefaultDispatchLeaseDuration is long enough for PipelineStarter's documented
// accept-and-return contract while still making a process crash recoverable.
func DefaultDispatchLeaseDuration() time.Duration { return defaultDispatchLeaseDuration }

// ListPendingDispatches is an inspection API. Delivery consumers must use
// ClaimPendingDispatch or ClaimPendingDispatches and present the returned token
// when recording success or failure.
func (s *Store) ListPendingDispatches(ctx context.Context, limit int) ([]*PendingDispatch, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("pending dispatches: store is nil")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+pendingDispatchColumns+`
		FROM pending_dispatches
		WHERE status = 'pending'
		ORDER BY attempts ASC, next_attempt_at ASC, id ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("pending dispatches: list: %w", err)
	}
	defer rows.Close()
	var out []*PendingDispatch
	for rows.Next() {
		dispatch, err := scanPendingDispatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dispatch)
	}
	return out, rows.Err()
}

// ClaimPendingDispatch atomically acquires one specific ready intent. It is used
// immediately after a successful admission commit. A competing consumer or an
// unexpired lease returns ErrDispatchClaimConflict without invoking a starter.
func (s *Store) ClaimPendingDispatch(
	ctx context.Context,
	id int64,
	now time.Time,
	leaseDuration time.Duration,
) (*PendingDispatch, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("pending dispatches: store is nil")
	}
	if id <= 0 {
		return nil, errors.New("pending dispatches: id must be positive")
	}
	now, leaseDuration = normalizeDispatchLease(now, leaseDuration)
	tokenPrefix := uuid.NewString()
	row := s.db.QueryRowContext(ctx, `
		UPDATE pending_dispatches
		SET lease_token = ? || '-' || id,
			lease_expires_at = ?, attempts = attempts + 1, updated_at = ?
		WHERE id = ? AND status = 'pending' AND next_attempt_at <= ?
			AND (lease_token IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= ?)
		RETURNING `+pendingDispatchColumns+`
	`, tokenPrefix, timeRFC3339(now.Add(leaseDuration)), timeRFC3339(now), id,
		timeRFC3339(now), timeRFC3339(now))
	dispatch, err := scanPendingDispatch(row)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: dispatch=%d", ErrDispatchClaimConflict, id)
		}
		return nil, fmt.Errorf("pending dispatches: claim %d: %w", id, err)
	}
	return dispatch, nil
}

// ClaimPendingDispatches atomically leases a bounded batch of ready intents.
// Scheduled retries are excluded until due, so fresh work can bypass poison
// rows. Each returned row receives a unique token derived from this claim.
func (s *Store) ClaimPendingDispatches(
	ctx context.Context,
	limit int,
	now time.Time,
	leaseDuration time.Duration,
) ([]*PendingDispatch, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("pending dispatches: store is nil")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	now, leaseDuration = normalizeDispatchLease(now, leaseDuration)
	tokenPrefix := uuid.NewString()
	rows, err := s.db.QueryContext(ctx, `
		WITH ready AS (
			SELECT id
			FROM pending_dispatches
			WHERE status = 'pending' AND next_attempt_at <= ?
				AND (lease_token IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= ?)
			ORDER BY attempts ASC, next_attempt_at ASC, id ASC
			LIMIT ?
		)
		UPDATE pending_dispatches
		SET lease_token = ? || '-' || id,
			lease_expires_at = ?, attempts = attempts + 1, updated_at = ?
		WHERE id IN (SELECT id FROM ready)
			AND status = 'pending' AND next_attempt_at <= ?
			AND (lease_token IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= ?)
		RETURNING `+pendingDispatchColumns+`
	`, timeRFC3339(now), timeRFC3339(now), limit, tokenPrefix,
		timeRFC3339(now.Add(leaseDuration)), timeRFC3339(now),
		timeRFC3339(now), timeRFC3339(now))
	if err != nil {
		return nil, fmt.Errorf("pending dispatches: claim batch: %w", err)
	}
	defer rows.Close()
	claimed := make([]*PendingDispatch, 0, limit)
	for rows.Next() {
		dispatch, err := scanPendingDispatch(rows)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, dispatch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pending dispatches: claim batch rows: %w", err)
	}
	return claimed, nil
}

// MarkDispatchDelivered acknowledges only the current delivery owner. A stale
// token cannot close a lease acquired after expiry. Repeating an acknowledgment
// after delivery remains idempotent.
func (s *Store) MarkDispatchDelivered(ctx context.Context, id int64, token string, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("pending dispatches: store is nil")
	}
	token = strings.TrimSpace(token)
	if id <= 0 || token == "" {
		return errors.New("pending dispatches: id and claim token required")
	}
	now = normalizeDispatchNow(now)
	res, err := s.db.ExecContext(ctx, `
		UPDATE pending_dispatches
		SET status = 'delivered', last_error = NULL, lease_token = NULL,
			lease_expires_at = NULL, updated_at = ?, delivered_at = ?
		WHERE id = ? AND status = 'pending' AND lease_token = ?
	`, timeRFC3339(now), timeRFC3339(now), id, token)
	if err != nil {
		return fmt.Errorf("pending dispatches: mark delivered %d: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("pending dispatches: mark delivered rows %d: %w", id, err)
	}
	if rows > 0 {
		return nil
	}
	var status DispatchStatus
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM pending_dispatches WHERE id = ?`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("pending dispatches: inspect delivered %d: %w", id, err)
	}
	if status == DispatchDelivered {
		return nil
	}
	return fmt.Errorf("%w: dispatch=%d token=%s", ErrDispatchClaimConflict, id, token)
}

// MarkDispatchFailed reschedules the current owner's intent with bounded
// exponential backoff. At the retry ceiling, a run that never left queued is
// atomically escalated with its backlog/workflow, reservation is released, a
// new aggregate transition is appended, and the dispatch is dead-lettered.
func (s *Store) MarkDispatchFailed(
	ctx context.Context,
	id int64,
	token string,
	message string,
	now time.Time,
	policy DispatchRetryPolicy,
) (*DispatchFailureResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("pending dispatches: store is nil")
	}
	token = strings.TrimSpace(token)
	if id <= 0 || token == "" {
		return nil, errors.New("pending dispatches: id and claim token required")
	}
	now = normalizeDispatchNow(now)
	policy = normalizeDispatchRetryPolicy(policy)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pending dispatches: fail begin %d: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Acquire SQLite's writer slot before inspecting mutable run/backlog state.
	guard, err := tx.ExecContext(ctx, `
		UPDATE pending_dispatches SET updated_at = updated_at
		WHERE id = ? AND status = 'pending' AND lease_token = ?
	`, id, token)
	if err != nil {
		return nil, fmt.Errorf("pending dispatches: fail guard %d: %w", id, err)
	}
	guarded, err := guard.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("pending dispatches: fail guard rows %d: %w", id, err)
	}
	if guarded != 1 {
		return nil, fmt.Errorf("%w: dispatch=%d token=%s", ErrDispatchClaimConflict, id, token)
	}

	dispatch, err := scanPendingDispatch(tx.QueryRowContext(ctx,
		`SELECT `+pendingDispatchColumns+` FROM pending_dispatches WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("pending dispatches: fail load %d: %w", id, err)
	}
	message = strings.TrimSpace(message)
	if dispatch.Attempts >= policy.MaxAttempts {
		deadLettered, err := deadLetterQueuedDispatch(ctx, tx, dispatch, token, message, now)
		if err != nil {
			return nil, err
		}
		if deadLettered {
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("pending dispatches: dead-letter commit %d: %w", id, err)
			}
			return &DispatchFailureResult{Attempts: dispatch.Attempts, DeadLettered: true}, nil
		}
	}

	next := now.Add(dispatchRetryDelay(policy, dispatch.Attempts))
	res, err := tx.ExecContext(ctx, `
		UPDATE pending_dispatches
		SET last_error = ?, next_attempt_at = ?, lease_token = NULL,
			lease_expires_at = NULL, updated_at = ?
		WHERE id = ? AND status = 'pending' AND lease_token = ?
	`, nullStr(message), timeRFC3339(next), timeRFC3339(now), id, token)
	if err != nil {
		return nil, fmt.Errorf("pending dispatches: reschedule %d: %w", id, err)
	}
	updated, err := res.RowsAffected()
	if err != nil || updated != 1 {
		return nil, fmt.Errorf("%w: dispatch=%d token=%s", ErrDispatchClaimConflict, id, token)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pending dispatches: reschedule commit %d: %w", id, err)
	}
	return &DispatchFailureResult{Attempts: dispatch.Attempts, NextAttemptAt: &next}, nil
}

func deadLetterQueuedDispatch(
	ctx context.Context,
	tx *sql.Tx,
	dispatch *PendingDispatch,
	token string,
	message string,
	now time.Time,
) (bool, error) {
	var runState, backlogState string
	var claimVersion int64
	if err := tx.QueryRowContext(ctx, `
		SELECT pr.state, bi.state, bi.claim_version
		FROM pipeline_runs pr
		JOIN backlog_items bi ON bi.id = pr.backlog_id
		WHERE pr.id = ? AND bi.id = ?
	`, dispatch.RunID, dispatch.BacklogID).Scan(&runState, &backlogState, &claimVersion); err != nil {
		return false, fmt.Errorf("pending dispatches: dead-letter load %d: %w", dispatch.ID, err)
	}
	if PipelineState(runState) != PipelineQueued {
		return false, nil
	}
	currentAggregate := BacklogState(backlogState) == BacklogRunning &&
		claimVersion == dispatch.AggregateVersion

	endedAt := timeRFC3339(now)
	res, err := tx.ExecContext(ctx, `
		UPDATE pipeline_runs
		SET state = 'escalated', current_stage = 'dispatch', ended_at = ?,
			escalation_class = ?, escalation_failure_class = 'configuration',
			escalation_retryable = 0, row_version = row_version + 1
		WHERE id = ? AND state = 'queued' AND row_version < ?
	`, endedAt, escalationClassTerminalConfig, dispatch.RunID, int64(math.MaxInt64))
	if err != nil {
		return false, fmt.Errorf("pending dispatches: dead-letter run %s: %w", dispatch.RunID, err)
	}
	if rows, err := res.RowsAffected(); err != nil || rows != 1 {
		return false, fmt.Errorf("pending dispatches: dead-letter run %s lost state race", dispatch.RunID)
	}

	if currentAggregate {
		newVersion := claimVersion + 1
		res, err = tx.ExecContext(ctx, `
			UPDATE backlog_items
			SET state = 'escalated', claim_version = claim_version + 1,
				row_version = row_version + 1, updated_at = ?
			WHERE id = ? AND state = 'running' AND claim_version = ?
		`, endedAt, dispatch.BacklogID, dispatch.AggregateVersion)
		if err != nil {
			return false, fmt.Errorf("pending dispatches: dead-letter backlog %s: %w", dispatch.BacklogID, err)
		}
		if rows, err := res.RowsAffected(); err != nil || rows != 1 {
			return false, fmt.Errorf("pending dispatches: dead-letter backlog %s lost state race", dispatch.BacklogID)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pipeline_transitions (
				backlog_id, aggregate_version, run_id, kind,
				from_state, to_state, occurred_at
			) VALUES (?, ?, ?, ?, 'running', 'escalated', ?)
		`, dispatch.BacklogID, newVersion, dispatch.RunID,
			PipelineDispatchDeadLetterKind, endedAt); err != nil {
			return false, fmt.Errorf("pending dispatches: dead-letter transition %s/%d: %w",
				dispatch.BacklogID, newVersion, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE pipeline_budget_reservations
		SET state = 'released', released_at = ?
		WHERE run_id = ? AND state = 'active'
	`, endedAt, dispatch.RunID); err != nil {
		return false, fmt.Errorf("pending dispatches: dead-letter reservation %s: %w", dispatch.RunID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs
		SET state = 'escalated', ended_at = ?
		WHERE id = ? AND engine = ?
	`, endedAt, dispatch.RunID, string(WorkflowEngineDAG)); err != nil {
		return false, fmt.Errorf("pending dispatches: dead-letter workflow %s: %w", dispatch.RunID, err)
	}
	res, err = tx.ExecContext(ctx, `
		UPDATE pending_dispatches
		SET status = 'dead_letter', last_error = ?, lease_token = NULL,
			lease_expires_at = NULL, updated_at = ?, dead_lettered_at = ?
		WHERE id = ? AND status = 'pending' AND lease_token = ?
	`, nullStr(message), endedAt, endedAt, dispatch.ID, token)
	if err != nil {
		return false, fmt.Errorf("pending dispatches: dead-letter intent %d: %w", dispatch.ID, err)
	}
	if rows, err := res.RowsAffected(); err != nil || rows != 1 {
		return false, fmt.Errorf("%w: dispatch=%d token=%s", ErrDispatchClaimConflict, dispatch.ID, token)
	}
	return true, nil
}

func normalizeDispatchLease(now time.Time, leaseDuration time.Duration) (time.Time, time.Duration) {
	now = normalizeDispatchNow(now)
	if leaseDuration <= 0 {
		leaseDuration = defaultDispatchLeaseDuration
	}
	return now, leaseDuration
}

func normalizeDispatchNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func normalizeDispatchRetryPolicy(policy DispatchRetryPolicy) DispatchRetryPolicy {
	defaults := DefaultDispatchRetryPolicy()
	if policy.BaseDelay <= 0 {
		policy.BaseDelay = defaults.BaseDelay
	}
	if policy.MaxDelay <= 0 {
		policy.MaxDelay = defaults.MaxDelay
	}
	if policy.MaxDelay < policy.BaseDelay {
		policy.MaxDelay = policy.BaseDelay
	}
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = defaults.MaxAttempts
	}
	return policy
}

func dispatchRetryDelay(policy DispatchRetryPolicy, attempts int) time.Duration {
	delay := policy.BaseDelay
	for attempt := 1; attempt < attempts && delay < policy.MaxDelay; attempt++ {
		if delay > policy.MaxDelay/2 {
			return policy.MaxDelay
		}
		delay *= 2
	}
	if delay > policy.MaxDelay {
		return policy.MaxDelay
	}
	return delay
}

func (s *Store) CountPendingDispatches(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("pending dispatches: store is nil")
	}
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_dispatches WHERE status = 'pending'`).Scan(&count); err != nil {
		return 0, fmt.Errorf("pending dispatches: count: %w", err)
	}
	return count, nil
}

// ReleasePipelineReservation is an idempotent explicit release path for
// reconciliation/repair. Normal terminal PipelineDAO.PutRun calls release and
// synchronize the DAG workflow row atomically.
func (s *Store) ReleasePipelineReservation(ctx context.Context, runID string) error {
	if s == nil || s.db == nil {
		return errors.New("pipeline reservation: store is nil")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE pipeline_budget_reservations
		SET state = ?, released_at = ?
		WHERE run_id = ? AND state = ?
	`, reservationStateReleased, timeRFC3339(time.Now().UTC()), runID, reservationStateActive)
	if err != nil {
		return fmt.Errorf("pipeline reservation: release %s: %w", runID, err)
	}
	return nil
}

func scanPendingDispatch(s scanner) (*PendingDispatch, error) {
	var (
		dispatch                                    PendingDispatch
		lastError, leaseToken, leaseExpiresAt       sql.NullString
		deliveredAt, deadLetteredAt                 sql.NullString
		nextAttemptAt, createdAt, updatedAt, status string
	)
	if err := s.Scan(
		&dispatch.ID, &dispatch.RunID, &dispatch.BacklogID,
		&dispatch.AggregateVersion, &dispatch.Kind, &status,
		&dispatch.Attempts, &lastError, &nextAttemptAt, &leaseToken, &leaseExpiresAt,
		&createdAt, &updatedAt, &deliveredAt, &deadLetteredAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("pending dispatches: scan: %w", err)
	}
	dispatch.Status = DispatchStatus(status)
	if lastError.Valid {
		dispatch.LastError = lastError.String
	}
	if leaseToken.Valid {
		dispatch.LeaseToken = leaseToken.String
	}
	var err error
	if dispatch.NextAttemptAt, err = parseTime(nextAttemptAt); err != nil {
		return nil, fmt.Errorf("pending dispatches: next_attempt_at: %w", err)
	}
	if dispatch.LeaseExpiresAt, err = nullableTime(leaseExpiresAt); err != nil {
		return nil, fmt.Errorf("pending dispatches: lease_expires_at: %w", err)
	}
	if dispatch.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("pending dispatches: created_at: %w", err)
	}
	if dispatch.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("pending dispatches: updated_at: %w", err)
	}
	if dispatch.DeliveredAt, err = nullableTime(deliveredAt); err != nil {
		return nil, fmt.Errorf("pending dispatches: delivered_at: %w", err)
	}
	if dispatch.DeadLetteredAt, err = nullableTime(deadLetteredAt); err != nil {
		return nil, fmt.Errorf("pending dispatches: dead_lettered_at: %w", err)
	}
	return &dispatch, nil
}
