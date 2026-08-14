package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EventDAO appends to the audit/debug event log.
type EventDAO struct {
	db *sql.DB
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

const eventColumns = `id, occurred_at, actor, kind, subject_kind, subject_id, payload_json`

// Append writes one event. Auto-fills OccurredAt if zero.
func (d *EventDAO) Append(ctx context.Context, e *Event) error {
	if e == nil || e.Actor == "" || e.Kind == "" {
		return errors.New("event: Actor + Kind required")
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	payload, err := jsonField(e.Payload)
	if err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO events (occurred_at, actor, kind, subject_kind, subject_id, payload_json)
		VALUES (?,?,?,?,?,?)
	`,
		timeRFC3339(e.OccurredAt), e.Actor, e.Kind,
		nullStr(e.SubjectKind), nullStr(e.SubjectID), payload,
	)
	if err != nil {
		return fmt.Errorf("event append: %w", err)
	}
	id, _ := res.LastInsertId()
	e.ID = id
	return nil
}

// AppendOnceBySubjectKind records the first event of a kind for a subject and
// leaves an existing attribution untouched. SQLite serializes the single
// INSERT...WHERE NOT EXISTS statement, providing first-writer stability without
// a migration-time unique index that could reject legacy duplicate events.
func (d *EventDAO) AppendOnceBySubjectKind(ctx context.Context, e *Event) (bool, error) {
	inserted, eventID, err := appendEventOnceBySubjectKind(ctx, d.db, e)
	if err != nil {
		return false, err
	}
	if inserted {
		e.ID = eventID
	}
	return inserted, nil
}

// appendEventOnceBySubjectKind is shared by EventDAO and aggregate
// transactions that must commit a first-writer event atomically with state.
// It returns the prospective row id without mutating e; transactional callers
// assign e.ID only after their surrounding commit succeeds.
func appendEventOnceBySubjectKind(ctx context.Context, exec contextExecer, e *Event) (bool, int64, error) {
	if e == nil || e.Actor == "" || e.Kind == "" || e.SubjectKind == "" || e.SubjectID == "" {
		return false, 0, errors.New("event: Actor + Kind + SubjectKind + SubjectID required")
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	payload, err := jsonField(e.Payload)
	if err != nil {
		return false, 0, fmt.Errorf("payload: %w", err)
	}
	res, err := exec.ExecContext(ctx, `
		INSERT INTO events (occurred_at, actor, kind, subject_kind, subject_id, payload_json)
		SELECT ?, ?, ?, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM events
			WHERE kind = ? AND subject_kind = ? AND subject_id = ?
		)
	`, timeRFC3339(e.OccurredAt), e.Actor, e.Kind, e.SubjectKind, e.SubjectID, payload,
		e.Kind, e.SubjectKind, e.SubjectID)
	if err != nil {
		return false, 0, fmt.Errorf("event append-once: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("event append-once rows: %w", err)
	}
	if rows == 0 {
		return false, 0, nil
	}
	eventID, _ := res.LastInsertId()
	return true, eventID, nil
}

// FirstBySubjectKind returns the oldest event of one kind for a subject. Squad
// outcome attribution uses this to remain stable even on databases containing
// duplicate events written by older operator versions.
func (d *EventDAO) FirstBySubjectKind(ctx context.Context, subjectKind, subjectID, eventKind string) (*Event, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT `+eventColumns+`
		FROM events
		WHERE subject_kind = ? AND subject_id = ? AND kind = ?
		ORDER BY occurred_at ASC, id ASC
		LIMIT 1
	`, subjectKind, subjectID, eventKind)
	var (
		e          Event
		occurredAt string
		payload    string
		storedKind sql.NullString
		storedID   sql.NullString
	)
	if err := row.Scan(&e.ID, &occurredAt, &e.Actor, &e.Kind, &storedKind, &storedID, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("event first-subject-kind: %w", err)
	}
	parsed, err := parseTime(occurredAt)
	if err != nil {
		return nil, fmt.Errorf("event first-subject-kind occurred_at: %w", err)
	}
	e.OccurredAt = parsed
	if storedKind.Valid {
		e.SubjectKind = storedKind.String
	}
	if storedID.Valid {
		e.SubjectID = storedID.String
	}
	if err := jsonInto(payload, &e.Payload); err != nil {
		return nil, fmt.Errorf("event first-subject-kind payload: %w", err)
	}
	return &e, nil
}

// CountBySubjectKind returns the all-time number of events of one kind recorded
// for a (subject_kind, subject_id). It backs the auto-requeue per-item lifetime
// cap: the count is read from the durable events table each tick, so the cap
// survives an operator restart without a dedicated counter column. Empty
// arguments yield a plain error rather than a silent 0 so a miswired caller is
// visible.
func (d *EventDAO) CountBySubjectKind(ctx context.Context, subjectKind, subjectID, kind string) (int, error) {
	if subjectKind == "" || subjectID == "" || kind == "" {
		return 0, errors.New("event count-by-subject-kind: subjectKind + subjectID + kind required")
	}
	var n int
	err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM events
		WHERE kind = ? AND subject_kind = ? AND subject_id = ?
	`, kind, subjectKind, subjectID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("event count-by-subject-kind: %w", err)
	}
	return n, nil
}

// CountByKindSince returns the number of events of one kind with occurred_at >=
// since. It backs the auto-requeue fleet-wide rolling-24h cap: a bounded count
// of every auto-requeue across items in the window, independent of subject.
func (d *EventDAO) CountByKindSince(ctx context.Context, kind string, since time.Time) (int, error) {
	if kind == "" {
		return 0, errors.New("event count-by-kind-since: kind required")
	}
	var n int
	err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM events
		WHERE kind = ? AND occurred_at >= ?
	`, kind, timeRFC3339(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("event count-by-kind-since: %w", err)
	}
	return n, nil
}

// ListBySubject returns events for the given (subject_kind, subject_id), newest-first.
func (d *EventDAO) ListBySubject(ctx context.Context, kind, id string, limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+eventColumns+`
		 FROM events
		 WHERE subject_kind = ? AND subject_id = ?
		 ORDER BY occurred_at DESC
		 LIMIT ?`,
		kind, id, limit)
	if err != nil {
		return nil, fmt.Errorf("event list-subject: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// ListByActorSince returns one actor's events occurred_at >= since,
// newest-first. It backs the overseers' recent-actions API: each supervisory
// agent writes its audit trail under a stable actor ("overseer.groomer" etc.)
// and this query reads it back per agent. It deliberately rides the existing
// idx_events_occurred index (newest-first window scan, filter on actor)
// rather than a dedicated (actor, occurred_at) index: the read is a bounded,
// low-traffic status query, while a new index would tax EVERY event append —
// the hot write path the fleet-reliability benchmark gate protects.
func (d *EventDAO) ListByActorSince(ctx context.Context, actor string, since time.Time, limit int) ([]*Event, error) {
	if actor == "" {
		return nil, errors.New("event list-by-actor-since: actor required")
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+eventColumns+`
		 FROM events
		 WHERE actor = ? AND occurred_at >= ?
		 ORDER BY occurred_at DESC, id DESC
		 LIMIT ?`,
		actor, timeRFC3339(since), limit)
	if err != nil {
		return nil, fmt.Errorf("event list-by-actor-since: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// ListSinceByActorPrefix returns events occurred_at >= since whose actor
// starts with prefix, newest-first. It backs the guard package's promotion
// report: the report's truncation cap must count the reviewed actors' events,
// not the whole firehose — a busy mill writes enough pipeline bookkeeping
// that an unfiltered window scan trips the cap while the audited actors hold
// a few hundred rows. substr rather than LIKE so the match is byte-exact
// (LIKE is case-insensitive by default and needs wildcard escaping). Like
// ListByActorSince above, it deliberately rides idx_events_occurred instead
// of taxing the hot append path with a new index.
func (d *EventDAO) ListSinceByActorPrefix(ctx context.Context, prefix string, since time.Time, limit int) ([]*Event, error) {
	if prefix == "" {
		return nil, errors.New("event list-since-by-actor-prefix: prefix required")
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+eventColumns+`
		 FROM events
		 WHERE occurred_at >= ? AND substr(actor, 1, ?) = ?
		 ORDER BY occurred_at DESC
		 LIMIT ?`,
		timeRFC3339(since), len(prefix), prefix, limit)
	if err != nil {
		return nil, fmt.Errorf("event list-since-by-actor-prefix: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// ListSinceByKinds returns events occurred_at >= since whose kind is one of
// kinds, newest-first. It backs the guard package's judge-calibration and
// config-outcome reports for the same reason as ListSinceByActorPrefix: the
// truncation cap has to count the report's own event kinds, not every writer
// in the table.
func (d *EventDAO) ListSinceByKinds(ctx context.Context, kinds []string, since time.Time, limit int) ([]*Event, error) {
	if len(kinds) == 0 {
		return nil, errors.New("event list-since-by-kinds: kinds required")
	}
	if limit <= 0 {
		limit = 200
	}
	placeholders := strings.Repeat("?,", len(kinds)-1) + "?"
	args := make([]any, 0, len(kinds)+2)
	args = append(args, timeRFC3339(since))
	for _, k := range kinds {
		args = append(args, k)
	}
	args = append(args, limit)
	// #nosec G202 -- IN clause is built from "?" placeholders only; values are bound via args
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+eventColumns+`
		 FROM events
		 WHERE occurred_at >= ? AND kind IN (`+placeholders+`)
		 ORDER BY occurred_at DESC
		 LIMIT ?`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("event list-since-by-kinds: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// ListSince returns events occurred_at >= since, newest-first.
func (d *EventDAO) ListSince(ctx context.Context, since time.Time, limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+eventColumns+`
		 FROM events
		 WHERE occurred_at >= ?
		 ORDER BY occurred_at DESC
		 LIMIT ?`,
		timeRFC3339(since), limit)
	if err != nil {
		return nil, fmt.Errorf("event list-since: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]*Event, error) {
	var out []*Event
	for rows.Next() {
		var (
			e           Event
			occurredAt  string
			payload     string
			subjectKind sql.NullString
			subjectID   sql.NullString
		)
		if err := rows.Scan(&e.ID, &occurredAt, &e.Actor, &e.Kind,
			&subjectKind, &subjectID, &payload); err != nil {
			return nil, fmt.Errorf("event scan: %w", err)
		}
		t, err := parseTime(occurredAt)
		if err != nil {
			return nil, fmt.Errorf("occurred_at: %w", err)
		}
		e.OccurredAt = t
		if subjectKind.Valid {
			e.SubjectKind = subjectKind.String
		}
		if subjectID.Valid {
			e.SubjectID = subjectID.String
		}
		if err := jsonInto(payload, &e.Payload); err != nil {
			return nil, fmt.Errorf("payload: %w", err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
