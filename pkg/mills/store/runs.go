package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ExternalDependencyIncidentClassification is the persisted classification
// assigned to runs whose failure was caused by an external dependency.
const ExternalDependencyIncidentClassification = "external_dependency_incident"

const (
	ExternalIncidentDwellRecovered = "recovered"
	ExternalIncidentDwellTimeout   = "timeout"
	ExternalIncidentDwellFastKill  = "fast_kill"
)

// ExternalIncidentDwell is the durable lifecycle of a
// wait_for_dependency_recovery disposition.
type ExternalIncidentDwell struct {
	RunID            string
	DependencyID     string
	Dependency       string
	StartedAt        time.Time
	DeadlineAt       time.Time
	CompletedAt      *time.Time
	CompletionReason string
	ElapsedDuration  time.Duration
}

// BeginExternalIncidentDwell creates the wait once and otherwise returns the
// original timestamps. Repeated sweeps and restarts are therefore idempotent.
func (d *PipelineDAO) BeginExternalIncidentDwell(ctx context.Context, runID, dependencyID, dependency string, startedAt, deadlineAt time.Time) (ExternalIncidentDwell, error) {
	if d == nil || d.db == nil || strings.TrimSpace(runID) == "" {
		return ExternalIncidentDwell{}, errors.New("external incident dwell: store and run id required")
	}
	if !deadlineAt.After(startedAt) {
		return ExternalIncidentDwell{}, errors.New("external incident dwell: deadline must follow start")
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO external_incident_dwells
			(run_id, dependency_id, dependency, started_at, deadline_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO NOTHING`,
		runID, dependencyID, dependency, timeRFC3339(startedAt), timeRFC3339(deadlineAt))
	if err != nil {
		return ExternalIncidentDwell{}, fmt.Errorf("begin external incident dwell %s: %w", runID, err)
	}
	return d.GetExternalIncidentDwell(ctx, runID)
}

func (d *PipelineDAO) GetExternalIncidentDwell(ctx context.Context, runID string) (ExternalIncidentDwell, error) {
	var out ExternalIncidentDwell
	var started, deadline string
	var completed, reason sql.NullString
	var elapsed sql.NullInt64
	err := d.db.QueryRowContext(ctx, `
		SELECT run_id, dependency_id, dependency, started_at, deadline_at,
		       completed_at, completion_reason, elapsed_duration_millis
		FROM external_incident_dwells WHERE run_id = ?`, runID).Scan(
		&out.RunID, &out.DependencyID, &out.Dependency, &started, &deadline,
		&completed, &reason, &elapsed)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, fmt.Errorf("get external incident dwell %s: %w", runID, err)
	}
	if out.StartedAt, err = parseTime(started); err != nil {
		return out, err
	}
	if out.DeadlineAt, err = parseTime(deadline); err != nil {
		return out, err
	}
	if completed.Valid {
		value, parseErr := parseTime(completed.String)
		if parseErr != nil {
			return out, parseErr
		}
		out.CompletedAt = &value
	}
	if reason.Valid {
		out.CompletionReason = reason.String
	}
	if elapsed.Valid {
		out.ElapsedDuration = time.Duration(elapsed.Int64) * time.Millisecond
	}
	return out, nil
}

// CompleteExternalIncidentDwell is a first-writer-wins CAS. Timeout, recovery,
// and fast-kill therefore cannot publish divergent outcomes.
func (d *PipelineDAO) CompleteExternalIncidentDwell(ctx context.Context, runID, reason string, completedAt time.Time) (ExternalIncidentDwell, bool, error) {
	switch reason {
	case ExternalIncidentDwellRecovered, ExternalIncidentDwellTimeout, ExternalIncidentDwellFastKill:
	default:
		return ExternalIncidentDwell{}, false, fmt.Errorf("external incident dwell: invalid completion reason %q", reason)
	}
	result, err := d.db.ExecContext(ctx, `
		UPDATE external_incident_dwells
		SET completed_at = ?, completion_reason = ?,
		    elapsed_duration_millis = MAX(0, CAST((julianday(?) - julianday(started_at)) * 86400000 AS INTEGER))
		WHERE run_id = ? AND completed_at IS NULL`,
		timeRFC3339(completedAt), reason, timeRFC3339(completedAt), runID)
	if err != nil {
		return ExternalIncidentDwell{}, false, fmt.Errorf("complete external incident dwell %s: %w", runID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ExternalIncidentDwell{}, false, err
	}
	dwell, err := d.GetExternalIncidentDwell(ctx, runID)
	return dwell, n == 1, err
}

// RunRetryState is the durable input to pipeline retry policy. PaidRetryCount
// counts earlier, non-free attempts for the same backlog item.
type RunRetryState struct {
	RunID                string
	BacklogID            string
	Classification       string
	PaidRetryCount       int
	Retryable            *bool
	FailureClass         string
	ExternalDependencyID string
	ExternalDependency   string
}

// GetRunRetryState returns persisted classification and retry history for a
// run. The current row, rather than an in-memory classification, is
// authoritative so policy decisions remain stable after restart or resume.
func (d *PipelineDAO) GetRunRetryState(ctx context.Context, runID string) (RunRetryState, error) {
	if d == nil || d.db == nil {
		return RunRetryState{}, errors.New("pipeline retry state: store not configured")
	}
	if strings.TrimSpace(runID) == "" {
		return RunRetryState{}, errors.New("pipeline retry state: run id required")
	}

	var (
		state        RunRetryState
		dependencyID sql.NullString
		dependency   sql.NullString
		retryable    sql.NullBool
	)
	err := d.db.QueryRowContext(ctx, `
		SELECT current.id,
		       current.backlog_id,
		       COALESCE(current.escalation_failure_class, ''),
		       current.escalation_external_dependency_id,
		       current.escalation_external_dependency,
		       current.escalation_retryable,
		       (
		         SELECT COUNT(*)
		         FROM pipeline_runs prior
		         WHERE prior.backlog_id = current.backlog_id
		           AND prior.id <> current.id
		           AND (COALESCE(prior.escalation_external_dependency_id, '') <> ''
		                OR COALESCE(prior.escalation_external_dependency, '') <> '')
		           AND (prior.started_at < current.started_at
		                OR (prior.started_at = current.started_at AND prior.id < current.id))
		       )
		FROM pipeline_runs current
		WHERE current.id = ?
	`, runID).Scan(
		&state.RunID,
		&state.BacklogID,
		&state.FailureClass,
		&dependencyID,
		&dependency,
		&retryable,
		&state.PaidRetryCount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRetryState{}, ErrNotFound
	}
	if err != nil {
		return RunRetryState{}, fmt.Errorf("pipeline retry state %s: %w", runID, err)
	}
	if dependency.Valid {
		state.ExternalDependency = dependency.String
	}
	if dependencyID.Valid {
		state.ExternalDependencyID = dependencyID.String
	}
	if state.ExternalDependencyID != "" || state.ExternalDependency != "" {
		state.Classification = ExternalDependencyIncidentClassification
	}
	if retryable.Valid {
		value := retryable.Bool
		state.Retryable = &value
	}
	return state, nil
}
