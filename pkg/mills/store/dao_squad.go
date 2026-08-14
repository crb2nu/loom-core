package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SquadDAO exposes CRUD against squads, squad_memory, and squad_outcomes —
// the persistence surface for the v2 hierarchical-swarm tactical layer.
type SquadDAO struct {
	db *sql.DB
}

const squadColumns = `name, paths_json, tests_json, gates_json, ensemble_json,
		budget_share, recursion_enabled, enabled, last_loaded_sha,
		created_at, updated_at`

// PutSquad inserts or replaces a squad row. CreatedAt is preserved on update.
func (d *SquadDAO) PutSquad(ctx context.Context, sq *Squad) error {
	if sq == nil || sq.Name == "" {
		return errors.New("squad: Name required")
	}
	now := time.Now().UTC()
	if sq.CreatedAt.IsZero() {
		sq.CreatedAt = now
	}
	sq.UpdatedAt = now

	paths, err := jsonField(sq.Paths)
	if err != nil {
		return fmt.Errorf("paths: %w", err)
	}
	tests, err := jsonField(sq.Tests)
	if err != nil {
		return fmt.Errorf("tests: %w", err)
	}
	gates, err := jsonField(sq.Gates)
	if err != nil {
		return fmt.Errorf("gates: %w", err)
	}
	ensemble, err := jsonField(sq.Ensemble)
	if err != nil {
		return fmt.Errorf("ensemble: %w", err)
	}

	_, err = d.db.ExecContext(ctx, `
		INSERT INTO squads (`+squadColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			paths_json        = excluded.paths_json,
			tests_json        = excluded.tests_json,
			gates_json        = excluded.gates_json,
			ensemble_json     = excluded.ensemble_json,
			budget_share      = excluded.budget_share,
			recursion_enabled = excluded.recursion_enabled,
			enabled           = excluded.enabled,
			last_loaded_sha   = excluded.last_loaded_sha,
			updated_at        = excluded.updated_at
	`,
		sq.Name, paths, tests, gates, ensemble,
		sq.BudgetShare, boolInt(sq.RecursionEnabled), boolInt(sq.Enabled),
		nullStr(sq.LastLoadedSHA),
		timeRFC3339(sq.CreatedAt), timeRFC3339(sq.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("squad put %s: %w", sq.Name, err)
	}
	sq.ID = sq.Name
	return nil
}

// GetSquad returns one squad by name, or ErrNotFound.
func (d *SquadDAO) GetSquad(ctx context.Context, name string) (*Squad, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+squadColumns+` FROM squads WHERE name = ?`, name)
	return scanSquad(row)
}

// ListSquads returns every squad row, alpha-sorted by name. Disabled squads
// included; callers filter as needed.
func (d *SquadDAO) ListSquads(ctx context.Context) ([]*Squad, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+squadColumns+` FROM squads ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("squad list: %w", err)
	}
	defer rows.Close()
	var out []*Squad
	for rows.Next() {
		s, err := scanSquad(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteSquad removes a squad by name. Cascade deletes squad_memory and
// squad_outcomes for that squad.
func (d *SquadDAO) DeleteSquad(ctx context.Context, name string) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM squads WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("squad delete %s: %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ----- Working memory -----

const squadMemoryColumns = `id, squad_name, kind, title, body, refs_json,
		importance, created_at, last_seen_at`

// PutMemory upserts a squad_memory row keyed by (squad_name, kind, title).
// CreatedAt is preserved on conflict; LastSeenAt always updates to now.
func (d *SquadDAO) PutMemory(ctx context.Context, m *SquadMemory) error {
	if m == nil || m.SquadName == "" || m.Kind == "" || m.Title == "" {
		return errors.New("squad_memory: SquadName, Kind, Title required")
	}
	if m.Importance < 0 || m.Importance > 1 {
		return errors.New("squad_memory: Importance must be in [0,1]")
	}
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.LastSeenAt = now
	refs, err := jsonField(m.Refs)
	if err != nil {
		return fmt.Errorf("refs: %w", err)
	}
	_, err = d.db.ExecContext(ctx, `
		INSERT INTO squad_memory (squad_name, kind, title, body, refs_json,
			importance, created_at, last_seen_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(squad_name, kind, title) DO UPDATE SET
			body         = excluded.body,
			refs_json    = excluded.refs_json,
			importance   = excluded.importance,
			last_seen_at = excluded.last_seen_at
	`,
		m.SquadName, string(m.Kind), m.Title, m.Body, refs,
		m.Importance, timeRFC3339(m.CreatedAt), timeRFC3339(m.LastSeenAt),
	)
	if err != nil {
		return fmt.Errorf("squad memory put: %w", err)
	}
	return nil
}

// MemoryRecall returns the top `limit` memory rows for a squad, optionally
// filtered by kind, ordered by importance DESC then last_seen_at DESC.
func (d *SquadDAO) MemoryRecall(ctx context.Context, squad string, kind SquadMemoryKind, limit int) ([]*SquadMemory, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT ` + squadMemoryColumns + ` FROM squad_memory WHERE squad_name = ?`
	args := []any{squad}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, string(kind))
	}
	q += ` ORDER BY importance DESC, last_seen_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("squad memory recall: %w", err)
	}
	defer rows.Close()
	var out []*SquadMemory
	for rows.Next() {
		m, err := scanSquadMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PruneMemory deletes rows with importance < threshold older than `before`.
// Returns the number of rows removed. Used by the weekly pruner job.
func (d *SquadDAO) PruneMemory(ctx context.Context, squad string, threshold float64, before time.Time) (int64, error) {
	res, err := d.db.ExecContext(ctx, `
		DELETE FROM squad_memory
		WHERE squad_name = ? AND importance < ? AND last_seen_at < ?
	`, squad, threshold, timeRFC3339(before))
	if err != nil {
		return 0, fmt.Errorf("squad memory prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ----- Outcomes (rolling success rate) -----

const squadOutcomeColumns = `id, squad_name, path_class, pipeline_run_id,
		outcome, cost_usd, duration_seconds, created_at, grade`

// RecordOutcome appends a squad outcome row. PipelineRunID has a unique
// index; re-recording the same run is a conflict.
func (d *SquadDAO) RecordOutcome(ctx context.Context, o *SquadOutcome) error {
	if o == nil || o.SquadName == "" || o.PipelineRunID == "" {
		return errors.New("squad_outcomes: SquadName + PipelineRunID required")
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	if err := d.ensureOutcomeGradeColumn(ctx); err != nil {
		return err
	}
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO squad_outcomes (squad_name, path_class, pipeline_run_id,
			outcome, cost_usd, duration_seconds, created_at, grade)
		VALUES (?,?,?,?,?,?,?,?)
	`,
		o.SquadName, o.PathClass, o.PipelineRunID,
		string(o.Outcome), o.CostUSD, o.DurationSeconds, timeRFC3339(o.CreatedAt), nullStr(o.Grade),
	)
	if err != nil {
		return fmt.Errorf("squad outcome record: %w", err)
	}
	id, _ := res.LastInsertId()
	o.ID = id
	return nil
}

// OutcomeStats returns the legacy success rate plus the graded taste sample
// over the same newest-first outcome window. Taste scores are keep=1,
// meh=0.5, regret=0; ungraded outcomes never enter the taste denominator.
func (d *SquadDAO) OutcomeStats(ctx context.Context, squad, pathClass string, window int) (successRate float64, total int, taste float64, graded int, err error) {
	if err := d.ensureOutcomeGradeColumn(ctx); err != nil {
		return 0, 0, 0, 0, err
	}
	if window <= 0 {
		window = 30
	}
	rows, err := d.db.QueryContext(ctx, `SELECT outcome, grade FROM squad_outcomes
		WHERE squad_name = ? AND path_class = ? ORDER BY created_at DESC LIMIT ?`, squad, pathClass, window)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("squad outcome stats: %w", err)
	}
	defer rows.Close()
	var successes int
	for rows.Next() {
		var outcome string
		var grade sql.NullString
		if err := rows.Scan(&outcome, &grade); err != nil {
			return 0, 0, 0, 0, err
		}
		total++
		if SquadOutcomeKind(outcome) == SquadOutcomeMergedClean {
			successes++
		}
		switch grade.String {
		case "keep":
			taste += 1
			graded++
		case "meh":
			taste += .5
			graded++
		case "regret":
			graded++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, err
	}
	if total > 0 {
		successRate = float64(successes) / float64(total)
	}
	if graded > 0 {
		taste /= float64(graded)
	}
	return
}

// ensureOutcomeGradeColumn upgrades pre-S3 stores without widening the
// migration-file slice owned by this change. SQLite treats the duplicate-column
// race as harmless after the schema is rechecked.
func (d *SquadDAO) ensureOutcomeGradeColumn(ctx context.Context) error {
	rows, err := d.db.QueryContext(ctx, `PRAGMA table_info(squad_outcomes)`)
	if err != nil {
		return fmt.Errorf("squad outcome schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("squad outcome schema scan: %w", err)
		}
		found = found || name == "grade"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := d.db.ExecContext(ctx, `ALTER TABLE squad_outcomes ADD COLUMN grade TEXT`); err != nil {
		return fmt.Errorf("squad outcome add grade: %w", err)
	}
	return nil
}

// SuccessRate returns the rolling success fraction for a (squad, path_class)
// over the last `window` outcomes. Returns (rate, sampleSize). When sample
// size < `window/2`, the rate is unstable; callers may treat it as
// low-confidence.
func (d *SquadDAO) SuccessRate(ctx context.Context, squad, pathClass string, window int) (float64, int, error) {
	if window <= 0 {
		window = 30
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT outcome FROM squad_outcomes
		WHERE squad_name = ? AND path_class = ?
		ORDER BY created_at DESC
		LIMIT ?
	`, squad, pathClass, window)
	if err != nil {
		return 0, 0, fmt.Errorf("squad success rate: %w", err)
	}
	defer rows.Close()
	var total, success int
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			return 0, 0, fmt.Errorf("squad success scan: %w", err)
		}
		total++
		if SquadOutcomeKind(o) == SquadOutcomeMergedClean {
			success++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if total == 0 {
		return 0, 0, nil
	}
	return float64(success) / float64(total), total, nil
}

// ListOutcomes returns recent outcomes for a squad, newest-first, capped.
func (d *SquadDAO) ListOutcomes(ctx context.Context, squad string, limit int) ([]*SquadOutcome, error) {
	if err := d.ensureOutcomeGradeColumn(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT `+squadOutcomeColumns+` FROM squad_outcomes
		WHERE squad_name = ? ORDER BY created_at DESC LIMIT ?
	`, squad, limit)
	if err != nil {
		return nil, fmt.Errorf("squad list outcomes: %w", err)
	}
	defer rows.Close()
	var out []*SquadOutcome
	for rows.Next() {
		o, err := scanSquadOutcome(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ----- Scanners -----

func scanSquad(s scanner) (*Squad, error) {
	var (
		sq                            Squad
		paths, tests, gates, ensemble string
		recursionEnabled, enabled     int
		lastLoadedSHA                 sql.NullString
		createdAt, updatedAt          string
	)
	err := s.Scan(
		&sq.Name, &paths, &tests, &gates, &ensemble,
		&sq.BudgetShare, &recursionEnabled, &enabled, &lastLoadedSHA,
		&createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("squad scan: %w", err)
	}
	sq.ID = sq.Name
	sq.RecursionEnabled = recursionEnabled != 0
	sq.Enabled = enabled != 0
	if lastLoadedSHA.Valid {
		sq.LastLoadedSHA = lastLoadedSHA.String
	}
	if err := jsonInto(paths, &sq.Paths); err != nil {
		return nil, fmt.Errorf("paths: %w", err)
	}
	if err := jsonInto(tests, &sq.Tests); err != nil {
		return nil, fmt.Errorf("tests: %w", err)
	}
	if err := jsonInto(gates, &sq.Gates); err != nil {
		return nil, fmt.Errorf("gates: %w", err)
	}
	if err := jsonInto(ensemble, &sq.Ensemble); err != nil {
		return nil, fmt.Errorf("ensemble: %w", err)
	}
	if sq.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("created_at: %w", err)
	}
	if sq.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("updated_at: %w", err)
	}
	return &sq, nil
}

func scanSquadMemory(s scanner) (*SquadMemory, error) {
	var (
		m          SquadMemory
		kind       string
		refs       string
		createdAt  string
		lastSeenAt string
	)
	err := s.Scan(
		&m.ID, &m.SquadName, &kind, &m.Title, &m.Body, &refs,
		&m.Importance, &createdAt, &lastSeenAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("squad memory scan: %w", err)
	}
	m.Kind = SquadMemoryKind(kind)
	if err := jsonInto(refs, &m.Refs); err != nil {
		return nil, fmt.Errorf("refs: %w", err)
	}
	if m.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("created_at: %w", err)
	}
	if m.LastSeenAt, err = parseTime(lastSeenAt); err != nil {
		return nil, fmt.Errorf("last_seen_at: %w", err)
	}
	return &m, nil
}

func scanSquadOutcome(s scanner) (*SquadOutcome, error) {
	var (
		o         SquadOutcome
		outcome   string
		createdAt string
		grade     sql.NullString
	)
	err := s.Scan(
		&o.ID, &o.SquadName, &o.PathClass, &o.PipelineRunID,
		&outcome, &o.CostUSD, &o.DurationSeconds, &createdAt, &grade,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("squad outcome scan: %w", err)
	}
	o.Outcome = SquadOutcomeKind(outcome)
	o.Grade = grade.String
	if o.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("created_at: %w", err)
	}
	return &o, nil
}

// boolInt converts a Go bool to the SQLite 0/1 integer encoding the v2
// schema uses for boolean columns.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
