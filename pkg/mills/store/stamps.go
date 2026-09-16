package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Stamp is a durable cross-repository delivery intent. TargetProject is the
// canonical GitLab project path that will receive the stamped work. Legacy,
// target-less stamps are not representable: reads fail if persisted data does
// not identify a destination.
type Stamp struct {
	ID            string    `json:"id"`
	TargetProject string    `json:"target_project"`
	CreatedAt     time.Time `json:"created_at"`
}

// ErrStampCollision means a stamp already exists for the same target project
// and pattern ID. Callers may use errors.Is to distinguish a replay from an
// unrelated persistence failure.
var ErrStampCollision = errors.New("stamp identity collision")

// StampDAO persists target-bound cross-repository stamps.
type StampDAO struct {
	db *sql.DB
}

// Put inserts a stamp once. Both identity fields are normalized and validated
// before SQLite is called. The same pattern ID may be used in different target
// projects, but replaying the composite identity returns ErrStampCollision.
func (d *StampDAO) Put(ctx context.Context, stamp *Stamp) error {
	if d == nil || d.db == nil {
		return errors.New("stamp: store not configured")
	}
	if stamp == nil {
		return errors.New("stamp: value required")
	}
	stamp.ID = strings.TrimSpace(stamp.ID)
	if stamp.ID == "" {
		return errors.New("stamp: ID required")
	}
	stamp.TargetProject = strings.TrimSpace(stamp.TargetProject)
	if stamp.TargetProject == "" {
		return errors.New("stamp: target project required")
	}
	if stamp.CreatedAt.IsZero() {
		stamp.CreatedAt = time.Now().UTC()
	} else {
		stamp.CreatedAt = stamp.CreatedAt.UTC()
	}

	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO cross_repo_stamps (id, target_project, created_at)
		VALUES (?, ?, ?)
	`, stamp.ID, stamp.TargetProject, timeRFC3339(stamp.CreatedAt)); err != nil {
		var exists int
		lookupErr := d.db.QueryRowContext(ctx, `
			SELECT 1 FROM cross_repo_stamps WHERE target_project = ? AND id = ?
		`, stamp.TargetProject, stamp.ID).Scan(&exists)
		if lookupErr == nil {
			return fmt.Errorf("stamp put %s for %s: %w", stamp.ID, stamp.TargetProject, ErrStampCollision)
		}
		return fmt.Errorf("stamp put %s: %w", stamp.ID, err)
	}
	return nil
}

// Get returns a stamp by its target project and pattern ID. A corrupt
// target-less row fails closed rather than being surfaced as a deliverable
// intent.
func (d *StampDAO) Get(ctx context.Context, targetProject, id string) (*Stamp, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("stamp: store not configured")
	}
	var stamp Stamp
	var createdAt string
	err := d.db.QueryRowContext(ctx, `
		SELECT id, target_project, created_at
		FROM cross_repo_stamps
		WHERE target_project = ? AND id = ?
	`, strings.TrimSpace(targetProject), strings.TrimSpace(id)).Scan(&stamp.ID, &stamp.TargetProject, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("stamp get %q: %w", id, err)
	}
	if strings.TrimSpace(stamp.TargetProject) == "" {
		return nil, fmt.Errorf("stamp get %q: target project missing", id)
	}
	stamp.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("stamp get %q created_at: %w", id, err)
	}
	return &stamp, nil
}

// GetForSource reads a stamp and upgrades a legacy target-less record to its
// source repository before returning it. New writes remain target-required;
// this compatibility path exists only for already-persisted legacy rows.
func (d *StampDAO) GetForSource(ctx context.Context, id, sourceProject string) (*Stamp, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("stamp: store not configured")
	}
	var stamp Stamp
	var createdAt string
	err := d.db.QueryRowContext(ctx, `
		SELECT id, target_project, created_at
		FROM cross_repo_stamps
		WHERE id = ?
		  AND (target_project = ? OR length(trim(target_project, char(9) || char(10) || char(11) || char(12) || char(13) || ' ')) = 0)
		ORDER BY CASE WHEN target_project = ? THEN 0 ELSE 1 END
		LIMIT 1
	`, strings.TrimSpace(id), strings.TrimSpace(sourceProject), strings.TrimSpace(sourceProject)).Scan(&stamp.ID, &stamp.TargetProject, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("stamp get %q: %w", id, err)
	}
	stamp.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("stamp get %q created_at: %w", id, err)
	}
	if strings.TrimSpace(stamp.TargetProject) == "" {
		updated, err := d.BackfillTargetProject(ctx, stamp.ID, sourceProject)
		if err != nil {
			return nil, err
		}
		if !updated {
			// A concurrent resolver won the conditional update; reload its value.
			return d.Get(ctx, sourceProject, id)
		}
		stamp.TargetProject = strings.TrimSpace(sourceProject)
	}
	return &stamp, nil
}

// BackfillTargetProject assigns sourceProject to a legacy target-less row.
// The empty-target predicate makes repeated and concurrent backfills
// idempotent and prevents an existing destination from being overwritten.
func (d *StampDAO) BackfillTargetProject(ctx context.Context, id, sourceProject string) (bool, error) {
	if d == nil || d.db == nil {
		return false, errors.New("stamp: store not configured")
	}
	id = strings.TrimSpace(id)
	sourceProject = strings.TrimSpace(sourceProject)
	if id == "" {
		return false, errors.New("stamp: ID required")
	}
	if sourceProject == "" {
		return false, errors.New("stamp: source project required")
	}
	result, err := d.db.ExecContext(ctx, `
		UPDATE cross_repo_stamps
		SET target_project = ?
		WHERE id = ? AND length(trim(target_project, char(9) || char(10) || char(11) || char(12) || char(13) || ' ')) = 0
	`, sourceProject, id)
	if err != nil {
		return false, fmt.Errorf("stamp backfill %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("stamp backfill %s affected rows: %w", id, err)
	}
	return rows == 1, nil
}
