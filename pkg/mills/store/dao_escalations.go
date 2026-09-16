package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RelaunchCandidate is the compact read model behind
// GET /api/mills/escalations/relaunch-candidates: one escalated backlog item
// whose LATEST pipeline run recorded escalation_retryable = true. No json
// tags on purpose — like the other store rows it serializes PascalCase
// ({ID, Title, EscalationClass, FailureClass, EndedAt}) and the HUD consumes
// exactly that spelling.
type RelaunchCandidate struct {
	// ID + Title identify the escalated backlog item.
	ID    string
	Title string
	// EscalationClass is the runner's historical ErrorClass spelling stamped
	// on the latest escalated run (for example "infra" or "config"); empty
	// when the run escalated without one.
	EscalationClass string
	// FailureClass is the policy-facing failure taxonomy spelling (for
	// example "infrastructure" or "configuration"); empty when unset.
	FailureClass string
	// EndedAt is when the latest run reached its terminal state. Nil when the
	// run never recorded an end time.
	EndedAt *time.Time
}

// RescuedWithoutVaccine is a merged item with a terminal escalation in its
// history whose prevention obligation has not yet been recorded.
type RescuedWithoutVaccine struct {
	BacklogID      string
	Title          string
	EscalationRun  string
	RegressionPath string
}

// ListRescuedWithoutVaccine returns merged items that actually travelled the
// rescue path. Never-escalated merges cannot match the EXISTS predicate.
func (d *BacklogDAO) ListRescuedWithoutVaccine(ctx context.Context, limit int) ([]*RescuedWithoutVaccine, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT bi.id, bi.title, pr.id, COALESCE(pr.research_diff, '')
		FROM backlog_items bi
		JOIN pipeline_runs pr ON pr.id = (
			SELECT e.id FROM pipeline_runs e
			WHERE e.backlog_id = bi.id AND e.state = 'escalated'
			ORDER BY e.started_at DESC, e.attempts DESC LIMIT 1)
		WHERE bi.state = ? AND NULLIF(TRIM(pr.vaccine_ref), '') IS NULL
		ORDER BY bi.updated_at ASC, bi.id ASC LIMIT ?`, string(BacklogMerged), limit)
	if err != nil {
		return nil, fmt.Errorf("escalations list rescued without vaccine: %w", err)
	}
	defer rows.Close()
	var out []*RescuedWithoutVaccine
	for rows.Next() {
		v := new(RescuedWithoutVaccine)
		if err := rows.Scan(&v.BacklogID, &v.Title, &v.EscalationRun, &v.RegressionPath); err != nil {
			return nil, fmt.Errorf("escalations scan rescued without vaccine: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetVaccineRef fulfills one terminal escalation's prevention obligation.
func (d *PipelineDAO) SetVaccineRef(ctx context.Context, runID, vaccineRef string) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(vaccineRef) == "" {
		return errors.New("escalations: run id and vaccine ref required")
	}
	res, err := d.db.ExecContext(ctx, `UPDATE pipeline_runs SET vaccine_ref = ? WHERE id = ? AND state = 'escalated'`, strings.TrimSpace(vaccineRef), runID)
	if err != nil {
		return fmt.Errorf("escalations set vaccine ref: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// relaunchCandidateColumns is the shared select list for relaunch-candidate
// queries: backlog identity plus the latest run's escalation metadata.
// bi aliases backlog_items; pr aliases that item's latest pipeline run.
const relaunchCandidateColumns = `bi.id, bi.title,
			COALESCE(pr.escalation_class, ''),
			COALESCE(pr.escalation_failure_class, ''),
			pr.ended_at`

// ListByEndedSince returns escalated backlog items whose LATEST pipeline run
// carries escalation_retryable = true — the relaunch candidates a human can
// requeue without a policy override — projected as RelaunchCandidate rows.
// "Latest" uses the same most-recent-run contract as ListEscalatedWithMR
// (started_at DESC, attempts DESC, LIMIT 1), so an item whose newest run is
// not retryable never surfaces on the strength of an older run.
//
// since bounds the window on the latest run's ended_at (RFC3339 text
// comparison, like every other windowed query in this package); a zero since
// applies no window. A run with a NULL ended_at never matches a non-zero
// window — there is no end time to compare.
//
// Ordering is newest ended_at first, then id ascending as a stable tiebreak.
// A non-positive limit falls back to 50; limits above 200 are capped at 200,
// matching the endpoint's documented default/max.
func (d *BacklogDAO) ListByEndedSince(ctx context.Context, since time.Time, limit int) ([]*RelaunchCandidate, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	args := []any{string(BacklogEscalated)}
	whereSince := ""
	if !since.IsZero() {
		whereSince = " AND pr.ended_at IS NOT NULL AND pr.ended_at >= ?"
		args = append(args, timeRFC3339(since))
	}
	args = append(args, limit)
	rows, err := d.db.QueryContext(ctx, `
		SELECT `+relaunchCandidateColumns+`
		FROM backlog_items bi
		JOIN pipeline_runs pr ON pr.id = (
			SELECT latest.id
			FROM pipeline_runs latest
			WHERE latest.backlog_id = bi.id
			ORDER BY latest.started_at DESC, latest.attempts DESC
			LIMIT 1
		)
		WHERE bi.state = ?
		  AND pr.escalation_retryable = 1`+whereSince+`
		ORDER BY pr.ended_at DESC, bi.id ASC
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("escalations list-by-ended-since: %w", err)
	}
	defer rows.Close()
	var out []*RelaunchCandidate
	for rows.Next() {
		var (
			candidate RelaunchCandidate
			endedAt   sql.NullString
		)
		if err := rows.Scan(
			&candidate.ID, &candidate.Title,
			&candidate.EscalationClass, &candidate.FailureClass, &endedAt,
		); err != nil {
			return nil, fmt.Errorf("escalations list-by-ended-since scan: %w", err)
		}
		candidate.EndedAt, err = nullableTime(endedAt)
		if err != nil {
			return nil, fmt.Errorf("escalations list-by-ended-since ended_at: %w", err)
		}
		out = append(out, &candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("escalations list-by-ended-since rows: %w", err)
	}
	return out, nil
}

// FailureSignatureCohort summarises the environment-delta evidence for one
// failure-shape fingerprint (shepherd B2). ClearedSince reports a DIFFERENT
// backlog item that shares the fingerprint and has since MERGED — proof the
// class stopped being fatal after the fix landed. ActiveSince counts runs of
// OTHER items that escalated with the fingerprint inside the recency window —
// proof the class is still burning and a relaunch would join it.
type FailureSignatureCohort struct {
	ClearedBy string // backlog id of the merged cohort sibling; "" = none
	Active    int    // recent same-fingerprint escalations on other items
}

// FailureSignatureCohort answers both cohort questions in two indexed reads.
// The fingerprint index (035) is partial on non-empty signatures, so both
// probes skip unstamped history.
func (d *BacklogDAO) FailureSignatureCohort(
	ctx context.Context, fingerprint, excludeItem string,
	clearedAfter, activeAfter time.Time,
) (FailureSignatureCohort, error) {
	var cohort FailureSignatureCohort
	if fingerprint == "" {
		return cohort, nil
	}
	var cleared sql.NullString
	err := d.db.QueryRowContext(ctx, `
		SELECT bi.id
		FROM pipeline_runs pr
		JOIN backlog_items bi ON bi.id = pr.backlog_id
		WHERE pr.failure_signature = ?
		  AND pr.backlog_id <> ?
		  AND bi.state = ?
		  AND bi.updated_at > ?
		ORDER BY bi.updated_at DESC
		LIMIT 1
	`, fingerprint, excludeItem, string(BacklogMerged), timeRFC3339(clearedAfter.UTC())).Scan(&cleared)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return cohort, fmt.Errorf("failure-signature cleared probe: %w", err)
	}
	if cleared.Valid {
		cohort.ClearedBy = cleared.String
	}
	if err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM pipeline_runs pr
		WHERE pr.failure_signature = ?
		  AND pr.backlog_id <> ?
		  AND pr.state = ?
		  AND pr.ended_at IS NOT NULL
		  AND pr.ended_at > ?
	`, fingerprint, excludeItem, string(PipelineEscalated), timeRFC3339(activeAfter.UTC())).Scan(&cohort.Active); err != nil {
		return cohort, fmt.Errorf("failure-signature active probe: %w", err)
	}
	return cohort, nil
}
