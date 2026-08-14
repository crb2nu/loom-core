package store

import (
	"context"
	"fmt"
	"time"
)

// ExternalIncidentQuery identifies a dependency incident cluster. The ID is
// preferred because it captures the exact signature; dependency name is the
// fallback for legacy rows that predate signature-level metadata.
type ExternalIncidentQuery struct {
	ExternalDependencyID string
	ExternalDependency   string
	Since                time.Time
}

// CountRecentExternalDependencyIncidents counts terminal escalations for the
// same external dependency in a bounded window. It is used by the escalation
// publisher to switch repeated provider incidents into degraded-mode handling.
func (d *PipelineDAO) CountRecentExternalDependencyIncidents(ctx context.Context, q ExternalIncidentQuery) (int, error) {
	if d == nil || d.db == nil {
		return 0, fmt.Errorf("pipeline external incidents: dao not configured")
	}
	if q.ExternalDependencyID == "" && q.ExternalDependency == "" {
		return 0, nil
	}

	args := []any{string(PipelineEscalated)}
	query := `SELECT COUNT(*) FROM pipeline_runs WHERE state = ?`
	if !q.Since.IsZero() {
		query += ` AND started_at >= ?`
		args = append(args, timeRFC3339(q.Since.UTC()))
	}
	if q.ExternalDependencyID != "" {
		query += ` AND escalation_external_dependency_id = ?`
		args = append(args, q.ExternalDependencyID)
	} else {
		query += ` AND escalation_external_dependency = ?`
		args = append(args, q.ExternalDependency)
	}

	var count int
	if err := d.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("pipeline external incidents count: %w", err)
	}
	return count, nil
}
