package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SpinDAO exposes CRUD against spin_runs — the status store for async
// Spinning-Room spins (migration 007). Mirrors CouncilDAO.
type SpinDAO struct {
	db *sql.DB
}

const spinColumns = `id, brief, frames_json, priority, project, namespace, status,
		plan_ids_json, error, competitive, started_at, ended_at`

// Put inserts or replaces a spin run record.
func (d *SpinDAO) Put(ctx context.Context, run *SpinRun) error {
	if run == nil || run.ID == "" {
		return errors.New("spin: run.ID required")
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	frames, err := jsonField(run.Frames)
	if err != nil {
		return fmt.Errorf("frames: %w", err)
	}
	planIDs, err := jsonField(run.PlanIDs)
	if err != nil {
		return fmt.Errorf("plan_ids: %w", err)
	}
	var endedAt sql.NullString
	if run.EndedAt != nil {
		endedAt = sql.NullString{String: timeRFC3339(*run.EndedAt), Valid: true}
	}
	competitive := 0
	if run.Competitive {
		competitive = 1
	}
	_, err = d.db.ExecContext(ctx, `
		INSERT INTO spin_runs (`+spinColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			brief         = excluded.brief,
			frames_json   = excluded.frames_json,
			priority      = excluded.priority,
			project       = excluded.project,
			namespace     = excluded.namespace,
			status        = excluded.status,
			plan_ids_json = excluded.plan_ids_json,
			error         = excluded.error,
			competitive   = excluded.competitive,
			started_at    = excluded.started_at,
			ended_at      = excluded.ended_at
	`,
		run.ID, run.Brief, frames, nullStr(run.Priority), nullStr(run.Project),
		nullStr(run.Namespace), string(run.Status), planIDs, nullStr(run.Error),
		competitive, timeRFC3339(run.StartedAt), endedAt,
	)
	if err != nil {
		return fmt.Errorf("spin put %s: %w", run.ID, err)
	}
	return nil
}

// Get fetches a spin run by id. Returns ErrNotFound when the id is unknown.
func (d *SpinDAO) Get(ctx context.Context, id string) (*SpinRun, error) {
	row := d.db.QueryRowContext(ctx, `SELECT `+spinColumns+` FROM spin_runs WHERE id = ?`, id)
	return scanSpin(row)
}

// List returns spin runs, newest-first by started_at.
func (d *SpinDAO) List(ctx context.Context, limit int) ([]*SpinRun, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+spinColumns+` FROM spin_runs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("spin list: %w", err)
	}
	defer rows.Close()
	var out []*SpinRun
	for rows.Next() {
		r, err := scanSpin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountActive returns the number of non-terminal (pending or running) spin
// runs. The async POST handler uses it to bound the in-flight queue so a burst
// of requests can't pile up unbounded behind the concurrency semaphore.
func (d *SpinDAO) CountActive(ctx context.Context) (int, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spin_runs WHERE status IN ('pending','running')`)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("spin count-active: %w", err)
	}
	return n, nil
}

// MarkOrphaned fails every non-terminal (pending/running) spin run and returns
// the number swept. Called ONCE at operator startup: the singleton operator's
// in-memory goroutines died with the previous process, so any row still
// pending/running is definitionally orphaned — its spin will never complete.
// The draft plan, if the spin authored one before the crash, is still durable
// in the Plan Store (agent-context); only the status row is reconciled here.
func (d *SpinDAO) MarkOrphaned(ctx context.Context) (int, error) {
	res, err := d.db.ExecContext(ctx, `
		UPDATE spin_runs
		SET status = ?, error = ?, ended_at = ?
		WHERE status IN ('pending','running')
	`, string(SpinFailed), "orphaned: operator restarted before the spin finished", timeRFC3339(time.Now().UTC()))
	if err != nil {
		return 0, fmt.Errorf("spin mark-orphaned: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spin mark-orphaned rows: %w", err)
	}
	return int(n), nil
}

func scanSpin(s scanner) (*SpinRun, error) {
	var (
		run                          SpinRun
		priority, project, namespace sql.NullString
		errStr, endedAt              sql.NullString
		frames, planIDs              string
		startedAt                    string
		competitive                  int
	)
	err := s.Scan(
		&run.ID, &run.Brief, &frames, &priority, &project, &namespace, &run.Status,
		&planIDs, &errStr, &competitive, &startedAt, &endedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("spin scan: %w", err)
	}
	if run.StartedAt, err = parseTime(startedAt); err != nil {
		return nil, fmt.Errorf("started_at: %w", err)
	}
	if run.EndedAt, err = nullableTime(endedAt); err != nil {
		return nil, fmt.Errorf("ended_at: %w", err)
	}
	if err := jsonInto(frames, &run.Frames); err != nil {
		return nil, fmt.Errorf("frames: %w", err)
	}
	if err := jsonInto(planIDs, &run.PlanIDs); err != nil {
		return nil, fmt.Errorf("plan_ids: %w", err)
	}
	if priority.Valid {
		run.Priority = priority.String
	}
	if project.Valid {
		run.Project = project.String
	}
	if namespace.Valid {
		run.Namespace = namespace.String
	}
	if errStr.Valid {
		run.Error = errStr.String
	}
	run.Competitive = competitive != 0
	// Normalise nil slices to empty so JSON renders [] not null.
	if run.Frames == nil {
		run.Frames = []string{}
	}
	if run.PlanIDs == nil {
		run.PlanIDs = []string{}
	}
	return &run, nil
}
