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

// StampDAO persists target-bound cross-repository stamps.
type StampDAO struct {
	db *sql.DB
}

// Put inserts a stamp once. Both identity and destination are normalized and
// validated before SQLite is called; replaying an ID returns the constraint
// error instead of silently changing its destination.
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
		return fmt.Errorf("stamp put %s: %w", stamp.ID, err)
	}
	return nil
}

// Get returns a stamp by ID. A corrupt target-less row fails closed rather
// than being surfaced as a deliverable intent.
func (d *StampDAO) Get(ctx context.Context, id string) (*Stamp, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("stamp: store not configured")
	}
	var stamp Stamp
	var createdAt string
	err := d.db.QueryRowContext(ctx, `
		SELECT id, target_project, created_at
		FROM cross_repo_stamps
		WHERE id = ?
	`, strings.TrimSpace(id)).Scan(&stamp.ID, &stamp.TargetProject, &createdAt)
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
