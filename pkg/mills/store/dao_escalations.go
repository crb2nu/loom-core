package store

import (
	"context"
	"database/sql"
	"fmt"
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
