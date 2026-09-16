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
	// writer, when set, is the ledger's private single-connection write
	// handle (Store.writer): appends ride it so they never queue behind the
	// read pool's long scans. Nil (direct test constructions) falls back to
	// db, the pre-2026-09-08 behaviour.
	writer  *sql.DB
	hotRead hotReadConfig
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// appendExecer is the handle ledger INSERTs run on: the dedicated writer
// when the store opened one, else the shared pool.
func (d *EventDAO) appendExecer() contextExecer {
	if d.writer != nil {
		return d.writer
	}
	return d.db
}

const eventColumns = `id, occurred_at, actor, kind, subject_kind, subject_id, payload_json`

// Append writes one event. Auto-fills OccurredAt if zero. The INSERT runs on
// the ledger's dedicated write connection (see EventDAO.writer), so a ledger
// row is never starved by read-pool exhaustion; the caller's context still
// bounds it.
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
	res, err := d.appendExecer().ExecContext(ctx, `
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
	inserted, eventID, err := appendEventOnceBySubjectKind(ctx, d.appendExecer(), e)
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
	return scanEventRow(row, "event first-subject-kind")
}

// LatestByKind returns the newest durable event of one kind, or ErrNotFound.
// It backs the finished-goods digest's "latest day" read: an equality seek
// on kind plus a one-row backward walk of idx_events_kind_occurred (kind,
// occurred_at, rowid) — never the time window the capped listers walk —
// and TestEventReadIndexes_QueryPlans pins that plan. Fixed-width
// occurred_at (migration 042) keeps the DESC order byte-correct.
func (d *EventDAO) LatestByKind(ctx context.Context, eventKind string) (*Event, error) {
	if eventKind == "" {
		return nil, errors.New("event latest-by-kind: kind required")
	}
	row := d.db.QueryRowContext(ctx, `
		SELECT `+eventColumns+`
		FROM events
		WHERE kind = ?
		ORDER BY occurred_at DESC, id DESC
		LIMIT 1
	`, eventKind)
	return scanEventRow(row, "event latest-by-kind")
}

// rowScanner is the *sql.Row surface scanEventRow needs.
type rowScanner interface{ Scan(...any) error }

// scanEventRow decodes one eventColumns row. sql.ErrNoRows maps to
// ErrNotFound; label prefixes every other error so callers stay
// distinguishable in logs.
func scanEventRow(row rowScanner, label string) (*Event, error) {
	var e Event
	var occurredAt, payload string
	var subjectKind, subjectID sql.NullString
	if err := row.Scan(&e.ID, &occurredAt, &e.Actor, &e.Kind, &subjectKind, &subjectID, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	at, err := parseTime(occurredAt)
	if err != nil {
		return nil, fmt.Errorf("%s occurred_at: %w", label, err)
	}
	e.OccurredAt, e.SubjectKind, e.SubjectID = at, subjectKind.String, subjectID.String
	if err := jsonInto(payload, &e.Payload); err != nil {
		return nil, fmt.Errorf("%s payload: %w", label, err)
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
// and this query reads it back per agent. Since migration 029 it rides
// idx_events_actor_occurred: the original ride-idx_events_occurred bet
// ("bounded, low-traffic status query") was falsified 2026-08-15 when storm
// volume plus concurrent HUD panels turned the window scans into a
// core-pinning congestion source on an idle operator
// (bl-mills-event-read-index-congestion-20260816). The append tax of the
// index was measured against BenchmarkFleetMillsEventAppend before merging,
// not assumed — bring numbers before trading this back.
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

const walkActorWindowQuery = `
	WITH RECURSIVE actors(actor) AS (
		SELECT (SELECT actor FROM events
			WHERE actor >= ?1 AND actor < ?2 ORDER BY actor LIMIT 1)
		UNION ALL
		SELECT (SELECT e.actor FROM events e
			WHERE e.actor > actors.actor AND e.actor < ?2 ORDER BY e.actor LIMIT 1)
		FROM actors WHERE actor IS NOT NULL
	)
	SELECT e.id, e.occurred_at, e.actor, e.kind, e.subject_kind, e.subject_id
	FROM actors CROSS JOIN events e
	WHERE actors.actor IS NOT NULL AND e.actor = actors.actor
		AND e.occurred_at >= ?3 AND e.occurred_at <= ?4`

// WalkActorWindow visits every matching event in the inclusive [since, until]
// window, in unspecified order. Payload is left nil: reports only need metadata.
// Next-actor seeks skip historical duplicates, then each actor's exact time
// range streams through the covering actor index without sorting or a row cap.
// Migration 044 includes the metadata projection in that index so a cold walk
// does not fetch the payload-bearing table page for every matching event.
// DISTINCT actor alone still scans duplicates when SQLite has no statistics.
// CROSS JOIN keeps actor enumeration outermost, and the single statement keeps
// one read snapshot without needing another connection. Cancellation or any
// scan/visitor error stops the walk and is returned.
func (d *EventDAO) WalkActorWindow(ctx context.Context, prefix string, since, until time.Time, visit func(*Event) error) error {
	if prefix == "" || !since.Before(until) || visit == nil {
		return errors.New("event walk-actor-window: prefix, increasing window, and visitor required")
	}
	rows, err := d.db.QueryContext(ctx, walkActorWindowQuery,
		prefix, prefix+"\U0010FFFF", timeRFC3339(since), timeRFC3339(until))
	if err != nil {
		return fmt.Errorf("event walk-actor-window: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var e Event
		var occurredAt string
		var subjectKind, subjectID sql.NullString
		if err := rows.Scan(&e.ID, &occurredAt, &e.Actor, &e.Kind, &subjectKind, &subjectID); err != nil {
			return fmt.Errorf("event walk-actor-window scan: %w", err)
		}
		e.OccurredAt, err = parseTime(occurredAt)
		if err != nil {
			return fmt.Errorf("event walk-actor-window occurred_at: %w", err)
		}
		e.SubjectKind, e.SubjectID = subjectKind.String, subjectID.String
		if err := visit(&e); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("event walk-actor-window: %w", err)
	}
	return ctx.Err()
}

// ListSinceByActorPrefix returns events occurred_at >= since whose actor
// starts with prefix, newest-first. It backs the guard package's promotion
// report: the report's truncation cap must count the reviewed actors' events,
// not the whole firehose — a busy mill writes enough pipeline bookkeeping
// that an unfiltered window scan trips the cap while the audited actors hold
// a few hundred rows. substr rather than LIKE so the match is byte-exact
// (LIKE is case-insensitive by default and needs wildcard escaping). Since
// migration 029, exact-actor reads use idx_events_actor_occurred; prefix
// matches still walk the time window (an expression index can't serve
// substr-prefix), which stays acceptable because the promotion report is
// cap-bounded — revisit with numbers if it shows up hot again.
func (d *EventDAO) ListSinceByActorPrefix(ctx context.Context, prefix string, since time.Time, limit int) ([]*Event, error) {
	if prefix == "" {
		return nil, errors.New("event list-since-by-actor-prefix: prefix required")
	}
	if limit <= 0 {
		limit = 200
	}
	// Half-open actor range [prefix, prefix+U+10FFFF) is the index-friendly
	// spelling of "starts with prefix": SQLite compares TEXT with memcmp under
	// the default BINARY collation, so every actor that begins with prefix
	// sorts inside the range and idx_events_actor_occurred serves it as a
	// SEARCH (measured on the 2026-09-02 production copy, 285k events: the
	// substr() form walked the 14-day window in ~30ms idle; the range form
	// is sub-millisecond). The former substr() predicate could only ride the
	// time index and re-walked the whole window per call.
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+eventColumns+`
		 FROM events
		 WHERE occurred_at >= ? AND actor >= ? AND actor < ?
		 ORDER BY occurred_at DESC
		 LIMIT ?`,
		timeRFC3339(since), prefix, prefix+"􏿿", limit)
	if err != nil {
		return nil, fmt.Errorf("event list-since-by-actor-prefix: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// ListSinceByKinds returns events occurred_at >= since whose kind is one of
// kinds, newest-first. Since migration 029 it uses idx_events_kind_occurred
// (kind-first covering index) rather than scanning the whole time window and
// filtering — the window-scan approach congested the operator after the
// 2026-08-15 storm (see 029_event_read_indexes.sql).
func (d *EventDAO) ListSinceByKinds(ctx context.Context, kinds []string, since time.Time, limit int) ([]*Event, error) {
	if len(kinds) == 0 {
		return nil, errors.New("event list-since-by-kinds: kinds required")
	}
	if limit <= 0 {
		return nil, errors.New("event list-since-by-kinds: positive limit required")
	}
	placeholders := strings.Repeat("?,", len(kinds)-1) + "?"
	args := make([]any, 0, len(kinds)+2)
	args = append(args, timeRFC3339(since))
	for _, k := range kinds {
		args = append(args, k)
	}
	args = append(args, limit)
	// NOT pinned to idx_events_occurred any more. The pin predated migration
	// 029's kind index and silently outlived it: with the pin the planner had
	// to walk the whole time window and filter by kind — for the KPI writer's
	// 30-day superseded-run scan that was ~0.5s on an idle NVMe laptop and
	// well past the 10s KPI budget on the loaded operator node, which is the
	// "kpi snapshot failed: event scan: context deadline exceeded" logged ~10×/h
	// all of 2026-09-02. Unpinned, the planner picks idx_events_kind_occurred
	// (one range per kind, temp b-tree for the ORDER BY) and the same scan is
	// sub-millisecond; TestEventReadIndexes_QueryPlans pins that plan.
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

// PruneKindsBefore deletes events of the given kinds whose occurred_at is
// older than before, in bounded batches so a nightly sweep never holds the
// single SQLite writer for long. It exists for the reconciler's per-tick
// bookkeeping kinds (deferred / skipped / ghost-spark-skipped / tick …),
// which are operational exhaust with a short useful life, NOT for audit
// kinds — 2026-09-02 those kinds were 92% of the last week's 75k rows and
// the events table plus its three read indexes had grown to ~140MB.
// Returns the number of rows deleted.
func (d *EventDAO) PruneKindsBefore(ctx context.Context, kinds []string, before time.Time, batch int) (int64, error) {
	if len(kinds) == 0 {
		return 0, errors.New("event prune-kinds-before: kinds required")
	}
	if before.IsZero() {
		return 0, errors.New("event prune-kinds-before: cutoff required")
	}
	if batch <= 0 {
		batch = 5000
	}
	placeholders := strings.Repeat("?,", len(kinds)-1) + "?"
	args := make([]any, 0, len(kinds)+2)
	for _, k := range kinds {
		args = append(args, k)
	}
	args = append(args, timeRFC3339(before), batch)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		// #nosec G202 -- IN clause is built from "?" placeholders only; values are bound via args
		res, err := d.db.ExecContext(ctx,
			`DELETE FROM events WHERE id IN (
			   SELECT id FROM events
			   WHERE kind IN (`+placeholders+`) AND occurred_at < ?
			   LIMIT ?)`, args...)
		if err != nil {
			return total, fmt.Errorf("event prune-kinds-before: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(batch) {
			return total, nil
		}
	}
}

// ListSince returns events occurred_at >= since, newest-first.
func (d *EventDAO) ListSince(ctx context.Context, since time.Time, limit int) ([]*Event, error) {
	if limit <= 0 {
		return nil, errors.New("event list-since: positive limit required")
	}
	limit = d.hotRead.bound(limit, 200)
	queryCtx, cancel := d.hotRead.context(ctx)
	defer cancel()
	rows, err := d.db.QueryContext(queryCtx,
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
	events, err := scanEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("event list-since: %w", err)
	}
	return events, nil
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
