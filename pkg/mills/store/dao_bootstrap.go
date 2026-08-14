package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BootstrappedProject is one runtime-minted repo: a GitLab project the
// operator created from a Spinning Room plan via POST /api/mills/projects/
// bootstrap (migration 009). Project is the full GitLab path
// ("services/procmodel") — the same string the plan is re-scoped to and the
// plan-slice emitter stamps as TargetProject.
type BootstrappedProject struct {
	Project   string    `json:"project"`
	PlanID    string    `json:"plan_id"`
	WebURL    string    `json:"web_url"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// BootstrapDAO exposes the bootstrapped_projects registry. Insert-only by
// design: a project path is minted once — re-bootstrapping the same path is
// a conflict surfaced to the caller (the repo already exists on GitLab).
type BootstrapDAO struct {
	db *sql.DB
}

// ErrAlreadyBootstrapped is returned by Insert when the project path already
// has a registry row.
var ErrAlreadyBootstrapped = errors.New("project already bootstrapped")

// Insert records a newly-minted project. Returns ErrAlreadyBootstrapped when
// the path was bootstrapped before (PRIMARY KEY conflict).
func (d *BootstrapDAO) Insert(ctx context.Context, p *BootstrappedProject) error {
	if p == nil || p.Project == "" {
		return errors.New("bootstrap: Project required")
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO bootstrapped_projects (project, plan_id, web_url, created_by, created_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(project) DO NOTHING
	`, p.Project, p.PlanID, p.WebURL, p.CreatedBy, timeRFC3339(p.CreatedAt))
	if err != nil {
		return fmt.Errorf("bootstrap insert %s: %w", p.Project, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("bootstrap insert rows %s: %w", p.Project, err)
	}
	if n == 0 {
		return ErrAlreadyBootstrapped
	}
	return nil
}

// Get fetches one registry row by project path. Returns ErrNotFound when the
// path was never bootstrapped.
func (d *BootstrapDAO) Get(ctx context.Context, project string) (*BootstrappedProject, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT project, plan_id, web_url, created_by, created_at
		FROM bootstrapped_projects WHERE project = ?`, project)
	return scanBootstrapped(row)
}

// List returns every bootstrapped project, oldest-first (stable demand order
// for the emitter's per-tick union).
func (d *BootstrapDAO) List(ctx context.Context) ([]*BootstrappedProject, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT project, plan_id, web_url, created_by, created_at
		FROM bootstrapped_projects ORDER BY created_at ASC, project ASC`)
	if err != nil {
		return nil, fmt.Errorf("bootstrap list: %w", err)
	}
	defer rows.Close()
	var out []*BootstrappedProject
	for rows.Next() {
		p, err := scanBootstrapped(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanBootstrapped(s scanner) (*BootstrappedProject, error) {
	var (
		p         BootstrappedProject
		createdAt string
	)
	err := s.Scan(&p.Project, &p.PlanID, &p.WebURL, &p.CreatedBy, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("bootstrap scan: %w", err)
	}
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("created_at: %w", err)
	}
	return &p, nil
}
