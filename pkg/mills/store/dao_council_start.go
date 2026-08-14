package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidCouncilClaim     = errors.New("council start: invalid claim")
	ErrCouncilAdmissionExpired = errors.New("council admission is no longer active")
)

const defaultCouncilReservationLeaseDuration = 6 * time.Hour

// CouncilBudgetExceededError reports every cap rejected by one serialized
// Council admission snapshot.
type CouncilBudgetExceededError struct {
	Reasons     []string
	SpentUSD    float64
	ReservedUSD float64
	Runs        int
	ActiveRuns  int
	Limits      CouncilStartLimits
}

func (e *CouncilBudgetExceededError) Error() string {
	if e == nil || len(e.Reasons) == 0 {
		return "council start: budget exceeded"
	}
	return "council start: budget exceeded: " + strings.Join(e.Reasons, "; ")
}

// ClaimCouncilStartRequest is the immutable input to one Council admission.
// The estimate is reserved until FinalizeCouncilRun commits actual spend.
type ClaimCouncilStartRequest struct {
	RunID       string
	Trigger     CouncilTrigger
	EstimateUSD float64
	Limits      CouncilStartLimits
	Now         time.Time
	// LeaseDuration bounds how long a crashed worker may hold concurrency.
	// Zero selects the conservative six-hour default.
	LeaseDuration time.Duration
	Notes         string
}

// ClaimCouncilStartResult contains the provisional run and active reservation
// committed by a successful admission.
type ClaimCouncilStartResult struct {
	Run         *CouncilRun
	Reservation *CouncilBudgetReservation
}

type councilStartBudgetSnapshot struct {
	spentUSD    float64
	reservedUSD float64
	runs        int
	activeRuns  int
}

// ClaimCouncilStart atomically serializes Council admission, evaluates all
// configured caps, inserts a provisional running row, and reserves the
// conservative estimate. No Council participant may run before it succeeds.
func (s *Store) ClaimCouncilStart(ctx context.Context, req ClaimCouncilStartRequest) (*ClaimCouncilStartResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("%w: store is nil", ErrInvalidCouncilClaim)
	}
	req.RunID = strings.TrimSpace(req.RunID)
	req.Notes = strings.TrimSpace(req.Notes)
	if err := validateClaimCouncilStartRequest(req); err != nil {
		return nil, err
	}
	now := req.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	leaseDuration := req.LeaseDuration
	if leaseDuration == 0 {
		leaseDuration = defaultCouncilReservationLeaseDuration
	}
	expiresAt := now.Add(leaseDuration)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("council start: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The provisional insert is deliberately the first statement. It takes
	// SQLite's writer slot before the budget snapshot, serializing concurrent
	// scheduled/manual contenders. The row is excluded from the snapshot and
	// is removed before a denied decision commits any stale-lease cleanup.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO council_runs (`+councilColumns+`)
		VALUES (?, ?, ?, NULL, ?, 0, 0, '[]', '{}', '{}', NULL, NULL, ?)
	`, req.RunID, string(req.Trigger), timeRFC3339(now), string(CouncilOutcomeRunning), nullStr(req.Notes)); err != nil {
		return nil, fmt.Errorf("council start: insert provisional run %s: %w", req.RunID, err)
	}
	if err := reapExpiredCouncilAdmissions(ctx, tx, now); err != nil {
		return nil, err
	}

	snapshot, err := readCouncilStartBudgetSnapshot(ctx, tx, now.Add(-24*time.Hour), req.RunID)
	if err != nil {
		return nil, err
	}
	if exceeded := evaluateCouncilStartBudget(req, snapshot); exceeded != nil {
		if err := removeDeniedCouncilAdmission(ctx, tx, req.RunID); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("council start: commit denied admission cleanup %s: %w", req.RunID, err)
		}
		return nil, exceeded
	}

	reservationResult, err := tx.ExecContext(ctx, `
		INSERT INTO council_budget_reservations (
			run_id, reserved_usd, state, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?)
	`, req.RunID, req.EstimateUSD, reservationStateActive, timeRFC3339(now), timeRFC3339(expiresAt))
	if err != nil {
		return nil, fmt.Errorf("council start: reserve budget %s: %w", req.RunID, err)
	}
	reservationID, err := reservationResult.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("council start: reservation id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("council start: commit %s: %w", req.RunID, err)
	}

	run := &CouncilRun{
		ID:        req.RunID,
		Trigger:   req.Trigger,
		StartedAt: now,
		Outcome:   CouncilOutcomeRunning,
		Sidecar:   map[string]any{},
		Notes:     req.Notes,
	}
	return &ClaimCouncilStartResult{
		Run: run,
		Reservation: &CouncilBudgetReservation{
			ID:          reservationID,
			RunID:       req.RunID,
			ReservedUSD: req.EstimateUSD,
			State:       reservationStateActive,
			CreatedAt:   now,
			ExpiresAt:   expiresAt,
		},
	}, nil
}

func validateClaimCouncilStartRequest(req ClaimCouncilStartRequest) error {
	if strings.TrimSpace(req.RunID) == "" {
		return fmt.Errorf("%w: run id required", ErrInvalidCouncilClaim)
	}
	if strings.TrimSpace(string(req.Trigger)) == "" {
		return fmt.Errorf("%w: trigger required", ErrInvalidCouncilClaim)
	}
	if !isFiniteNonNegative(req.EstimateUSD) {
		return fmt.Errorf("%w: estimate USD must be finite and >= 0", ErrInvalidCouncilClaim)
	}
	if !isFiniteNonNegative(req.Limits.MaxUSDPerRun) ||
		!isFiniteNonNegative(req.Limits.MaxUSDPerDay) ||
		req.Limits.MaxRunsPerDay < 0 || req.Limits.MaxConcurrentRuns < 0 {
		return fmt.Errorf("%w: limits must be finite and >= 0", ErrInvalidCouncilClaim)
	}
	if req.LeaseDuration < 0 {
		return fmt.Errorf("%w: lease duration must be >= 0", ErrInvalidCouncilClaim)
	}
	return nil
}

type expiredCouncilAdmission struct {
	runID           string
	reservedUSD     float64
	costFrontierUSD float64
	costLocalUSD    float64
	notes           sql.NullString
}

// reapExpiredCouncilAdmissions repairs workers that died after admission. The
// full unaccounted reservation becomes frontier spend before release, keeping
// later admissions conservative even though the exact provider spend is lost.
func reapExpiredCouncilAdmissions(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT r.run_id, r.reserved_usd,
			cr.cost_frontier_usd, cr.cost_local_usd, cr.notes
		FROM council_budget_reservations r
		JOIN council_runs cr ON cr.id = r.run_id
		WHERE r.state = ?
			AND julianday(r.expires_at) <= julianday(?)
			AND cr.outcome = ? AND cr.ended_at IS NULL
		ORDER BY r.id
	`, reservationStateActive, timeRFC3339(now), string(CouncilOutcomeRunning))
	if err != nil {
		return fmt.Errorf("council start: find expired admissions: %w", err)
	}
	var expired []expiredCouncilAdmission
	for rows.Next() {
		var admission expiredCouncilAdmission
		if err := rows.Scan(
			&admission.runID, &admission.reservedUSD,
			&admission.costFrontierUSD, &admission.costLocalUSD, &admission.notes,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("council start: scan expired admission: %w", err)
		}
		expired = append(expired, admission)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("council start: list expired admissions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("council start: close expired admissions: %w", err)
	}

	for _, admission := range expired {
		actualUSD := admission.costFrontierUSD + admission.costLocalUSD
		if actualUSD < admission.reservedUSD {
			admission.costFrontierUSD += admission.reservedUSD - actualUSD
		}
		notes := appendCouncilStoreNote(admission.notes.String,
			"reservation lease expired; charged conservative reserved cost")
		result, err := tx.ExecContext(ctx, `
			UPDATE council_runs
			SET ended_at = ?, outcome = ?, cost_frontier_usd = ?, notes = ?
			WHERE id = ? AND outcome = ? AND ended_at IS NULL
		`, timeRFC3339(now), string(CouncilOutcomeError), admission.costFrontierUSD,
			nullStr(notes), admission.runID, string(CouncilOutcomeRunning))
		if err != nil {
			return fmt.Errorf("council start: terminalize expired admission %s: %w", admission.runID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("council start: expired admission rows %s: %w", admission.runID, err)
		}
		if affected != 1 {
			return fmt.Errorf("council start: expired admission %s changed concurrently", admission.runID)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE council_budget_reservations
		SET state = ?, released_at = ?
		WHERE state = ? AND julianday(expires_at) <= julianday(?)
	`, reservationStateReleased, timeRFC3339(now), reservationStateActive, timeRFC3339(now)); err != nil {
		return fmt.Errorf("council start: release expired reservations: %w", err)
	}
	return nil
}

func removeDeniedCouncilAdmission(ctx context.Context, tx *sql.Tx, runID string) error {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM council_runs
		WHERE id = ? AND outcome = ? AND ended_at IS NULL
	`, runID, string(CouncilOutcomeRunning))
	if err != nil {
		return fmt.Errorf("council start: remove denied provisional run %s: %w", runID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("council start: denied provisional rows %s: %w", runID, err)
	}
	if affected != 1 {
		return fmt.Errorf("council start: denied provisional run %s changed concurrently", runID)
	}
	return nil
}

func appendCouncilStoreNote(existing, note string) string {
	existing = strings.TrimSpace(existing)
	note = strings.TrimSpace(note)
	if existing == "" {
		return note
	}
	if note == "" {
		return existing
	}
	return existing + "; " + note
}

func readCouncilStartBudgetSnapshot(ctx context.Context, tx *sql.Tx, since time.Time, currentRunID string) (councilStartBudgetSnapshot, error) {
	var out councilStartBudgetSnapshot
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COALESCE((
				SELECT SUM(cost_frontier_usd + cost_local_usd)
				FROM council_runs
				WHERE started_at >= ? AND id <> ?
			), 0),
			COALESCE((
				SELECT SUM(MAX(r.reserved_usd - (cr.cost_frontier_usd + cr.cost_local_usd), 0))
				FROM council_budget_reservations r
				JOIN council_runs cr ON cr.id = r.run_id
				WHERE r.state = 'active' AND r.created_at >= ? AND r.run_id <> ?
			), 0),
			COALESCE((
				SELECT COUNT(*) FROM council_runs
				WHERE started_at >= ? AND id <> ?
			), 0),
			COALESCE((
				SELECT COUNT(*)
				FROM council_budget_reservations r
				JOIN council_runs cr ON cr.id = r.run_id
				WHERE r.state = 'active' AND cr.ended_at IS NULL AND r.run_id <> ?
			), 0)
	`, timeRFC3339(since), currentRunID, timeRFC3339(since), currentRunID,
		timeRFC3339(since), currentRunID, currentRunID).Scan(
		&out.spentUSD, &out.reservedUSD, &out.runs, &out.activeRuns,
	); err != nil {
		return councilStartBudgetSnapshot{}, fmt.Errorf("council start: budget snapshot: %w", err)
	}
	return out, nil
}

func evaluateCouncilStartBudget(req ClaimCouncilStartRequest, snap councilStartBudgetSnapshot) *CouncilBudgetExceededError {
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
	return &CouncilBudgetExceededError{
		Reasons:     reasons,
		SpentUSD:    snap.spentUSD,
		ReservedUSD: snap.reservedUSD,
		Runs:        snap.runs,
		ActiveRuns:  snap.activeRuns,
		Limits:      req.Limits,
	}
}

// FinalizeCouncilRun atomically persists a terminal Council result and
// releases its active reservation. Actual cost on the run then replaces the
// estimate in subsequent admission snapshots.
func (s *Store) FinalizeCouncilRun(ctx context.Context, run *CouncilRun) error {
	if s == nil || s.db == nil {
		return errors.New("finalize council run: store is nil")
	}
	if run == nil || strings.TrimSpace(run.ID) == "" {
		return errors.New("finalize council run: run id required")
	}
	if run.EndedAt == nil || run.EndedAt.IsZero() {
		return errors.New("finalize council run: terminal ended_at required")
	}
	if run.Outcome == "" || run.Outcome == CouncilOutcomeRunning {
		return errors.New("finalize council run: terminal outcome required")
	}
	if !isFiniteNonNegative(run.CostFrontierUSD) || !isFiniteNonNegative(run.CostLocalUSD) {
		return errors.New("finalize council run: costs must be finite and >= 0")
	}
	artifacts, err := jsonField(run.Artifacts)
	if err != nil {
		return fmt.Errorf("finalize council run: artifacts: %w", err)
	}
	deltas, err := jsonField(run.BacklogDeltas)
	if err != nil {
		return fmt.Errorf("finalize council run: backlog deltas: %w", err)
	}
	sidecar, err := jsonField(run.Sidecar)
	if err != nil {
		return fmt.Errorf("finalize council run: sidecar: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finalize council run: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE council_runs SET
			ended_at = ?, outcome = ?,
			cost_frontier_usd = ?, cost_local_usd = ?, artifacts_json = ?,
			backlog_deltas_json = ?, sidecar_json = ?, branch_name = ?,
			commit_sha = ?, notes = ?
		WHERE id = ? AND outcome = ? AND ended_at IS NULL
	`, timeRFC3339(*run.EndedAt), string(run.Outcome),
		run.CostFrontierUSD, run.CostLocalUSD, artifacts, deltas, sidecar,
		nullStr(run.BranchName), nullStr(run.CommitSHA), nullStr(run.Notes), run.ID,
		string(CouncilOutcomeRunning))
	if err != nil {
		return fmt.Errorf("finalize council run: update %s: %w", run.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("finalize council run: update rows %s: %w", run.ID, err)
	}
	if rows != 1 {
		var outcome string
		if err := tx.QueryRowContext(ctx,
			`SELECT outcome FROM council_runs WHERE id = ?`, run.ID,
		).Scan(&outcome); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("finalize council run: provisional run %s: %w", run.ID, ErrNotFound)
			}
			return fmt.Errorf("finalize council run: inspect provisional run %s: %w", run.ID, err)
		}
		return fmt.Errorf("finalize council run: provisional run %s has outcome %q: %w",
			run.ID, outcome, ErrCouncilAdmissionExpired)
	}

	releasedAt := run.EndedAt.UTC()
	result, err = tx.ExecContext(ctx, `
		UPDATE council_budget_reservations
		SET state = ?, released_at = ?
		WHERE run_id = ? AND state = ?
	`, reservationStateReleased, timeRFC3339(releasedAt), run.ID, reservationStateActive)
	if err != nil {
		return fmt.Errorf("finalize council run: release reservation %s: %w", run.ID, err)
	}
	rows, err = result.RowsAffected()
	if err != nil {
		return fmt.Errorf("finalize council run: release rows %s: %w", run.ID, err)
	}
	if rows != 1 {
		return fmt.Errorf("finalize council run: reservation %s: %w", run.ID, ErrCouncilAdmissionExpired)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finalize council run: commit %s: %w", run.ID, err)
	}
	return nil
}
