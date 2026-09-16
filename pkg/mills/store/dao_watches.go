package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type WatchDAO struct{ db *sql.DB }

const watchColumns = `id, subject_kind, subject_id, terminal_condition, note, state, created_at, expires_at, resolved_at, resolution`

func (d *WatchDAO) Register(ctx context.Context, w *Watch) error {
	if w == nil {
		return errors.New("watch: value required")
	}
	if w.ID == "" {
		w.ID = uuid.NewString()
	}
	w.SubjectID, w.TerminalCondition, w.Note = strings.TrimSpace(w.SubjectID), strings.TrimSpace(w.TerminalCondition), strings.TrimSpace(w.Note)
	if !validWatchSubject(w.SubjectKind) || w.SubjectID == "" || w.TerminalCondition == "" {
		return errors.New("watch: valid subject kind, subject id, and terminal condition required")
	}
	if w.CreatedAt.IsZero() {
		w.CreatedAt = time.Now().UTC()
	}
	if !w.ExpiresAt.After(w.CreatedAt) {
		return errors.New("watch: expires_at must be after created_at")
	}
	if w.State == "" {
		w.State = WatchActive
	}
	if w.State != WatchActive {
		return errors.New("watch: new watch must be active")
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO watches (`+watchColumns+`) VALUES (?,?,?,?,?,?,?,?,NULL,'')`, w.ID, w.SubjectKind, w.SubjectID, w.TerminalCondition, w.Note, w.State, timeRFC3339(w.CreatedAt), timeRFC3339(w.ExpiresAt))
	if err != nil {
		return fmt.Errorf("watch register: %w", err)
	}
	return nil
}

func validWatchSubject(k WatchSubjectKind) bool {
	return k == WatchSubjectBacklogItem || k == WatchSubjectMergeRequest || k == WatchSubjectPipelineRun
}

func (d *WatchDAO) Get(ctx context.Context, id string) (*Watch, error) {
	return scanWatch(d.db.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE id=?`, id))
}

func (d *WatchDAO) List(ctx context.Context, f WatchFilter) ([]*Watch, error) {
	q, args := `SELECT `+watchColumns+` FROM watches WHERE 1=1`, []any{}
	if f.State != "" {
		q += ` AND state=?`
		args = append(args, f.State)
	}
	if f.SubjectKind != "" {
		q += ` AND subject_kind=?`
		args = append(args, f.SubjectKind)
	}
	if f.SubjectID != "" {
		q += ` AND subject_id=?`
		args = append(args, f.SubjectID)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("watch list: %w", err)
	}
	defer rows.Close()
	out := []*Watch{}
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (d *WatchDAO) ListActive(ctx context.Context, limit int) ([]*Watch, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE state='active' ORDER BY expires_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Watch{}
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Resolve conditionally completes an active watch. won=false means another
// actor already completed it; the stored terminal result is never overwritten.
func (d *WatchDAO) Resolve(ctx context.Context, id string, state WatchState, resolution string, at time.Time) (bool, error) {
	return d.resolve(ctx, id, state, resolution, at, nil)
}

// ResolveWithEvent atomically completes a watch and appends its one attention event.
func (d *WatchDAO) ResolveWithEvent(ctx context.Context, id string, state WatchState, resolution string, at time.Time, e *Event) (bool, error) {
	return d.resolve(ctx, id, state, resolution, at, e)
}

func (d *WatchDAO) resolve(ctx context.Context, id string, state WatchState, resolution string, at time.Time, e *Event) (bool, error) {
	if id == "" || (state != WatchMet && state != WatchExpired && state != WatchCancelled) {
		return false, errors.New("watch resolve: id and terminal state required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE watches SET state=?,resolved_at=?,resolution=? WHERE id=? AND state='active'`, state, timeRFC3339(at), strings.TrimSpace(resolution), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT 1 FROM watches WHERE id=?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		} else if err != nil {
			return false, err
		}
		return false, nil
	}
	if e != nil {
		if _, _, err = appendEventOnceBySubjectKind(ctx, tx, e); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

type watchScanner interface{ Scan(...any) error }

func scanWatch(s watchScanner) (*Watch, error) {
	var w Watch
	var created, expires string
	var resolved sql.NullString
	err := s.Scan(&w.ID, &w.SubjectKind, &w.SubjectID, &w.TerminalCondition, &w.Note, &w.State, &created, &expires, &resolved, &w.Resolution)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("watch scan: %w", err)
	}
	if w.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if w.ExpiresAt, err = parseTime(expires); err != nil {
		return nil, err
	}
	if resolved.Valid {
		t, e := parseTime(resolved.String)
		if e != nil {
			return nil, e
		}
		w.ResolvedAt = &t
	}
	return &w, nil
}
