package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// IncidentEventKind is the stable event kind used to persist IncidentRecord.
	IncidentEventKind = "mills.incident"
	incidentActor     = "pipeline.incident-classifier"
	incidentSubject   = "incident"

	// IncidentDispositionWaitForDependencyRecovery is the durable operational
	// disposition for external dependency incidents.
	IncidentDispositionWaitForDependencyRecovery = "wait_for_dependency_recovery"
)

// IncidentRecord is the durable representation of a deterministically
// classified external-dependency failure.
type IncidentRecord struct {
	ID              string        `json:"id"`
	Fingerprint     string        `json:"fingerprint"`
	Class           IncidentClass `json:"class"`
	Source          string        `json:"source"`
	Dependency      string        `json:"dependency"`
	Shape           string        `json:"shape"`
	Summary         string        `json:"summary"`
	Evidence        string        `json:"evidence,omitempty"`
	Retryable       bool          `json:"retryable"`
	OccurredAt      time.Time     `json:"occurred_at"`
	FirstSeen       time.Time     `json:"first_seen"`
	LastSeen        time.Time     `json:"last_seen"`
	OccurrenceCount int           `json:"occurrence_count"`
}

// IncidentSummary groups persisted records that describe the same classified
// failure shape.
type IncidentSummary struct {
	Class       IncidentClass
	Source      string
	Dependency  string
	Shape       string
	Summary     string
	Disposition string
	Retryable   bool
	Occurrences int
}

// IncidentDAO persists classified incidents in the Mills event log. One row is
// maintained for each fingerprint across runner restarts.
type IncidentDAO struct {
	db     *sql.DB
	events *EventDAO
}

// Put persists an incident. It returns true when a fingerprint is first seen;
// subsequent writes atomically advance its last-seen time and occurrence count.
func (d *IncidentDAO) Put(ctx context.Context, record *IncidentRecord) (bool, error) {
	if d == nil || d.db == nil {
		return false, errors.New("incident store not configured")
	}
	normalizeIncidentRecord(record)
	if err := validateIncidentRecord(record); err != nil {
		return false, err
	}

	event := &Event{
		OccurredAt:  record.LastSeen,
		Actor:       incidentActor,
		Kind:        IncidentEventKind,
		SubjectKind: incidentSubject,
		SubjectID:   record.Fingerprint,
		Payload:     incidentPayload(*record),
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("incident put connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, fmt.Errorf("incident put begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	existing, err := firstIncidentByFingerprint(ctx, conn, record.Fingerprint)
	inserted := errors.Is(err, ErrNotFound)
	if err != nil && !inserted {
		return false, err
	}
	if inserted {
		payload, err := jsonField(event.Payload)
		if err != nil {
			return false, fmt.Errorf("incident payload: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO events (occurred_at, actor, kind, subject_kind, subject_id, payload_json)
			VALUES (?, ?, ?, ?, ?, ?)
		`, timeRFC3339(event.OccurredAt), event.Actor, event.Kind, event.SubjectKind, event.SubjectID, payload); err != nil {
			return false, fmt.Errorf("incident insert: %w", err)
		}
	} else {
		if record.LastSeen.Before(existing.LastSeen) {
			record.LastSeen = existing.LastSeen
		}
		existing.LastSeen = record.LastSeen
		existing.OccurredAt = record.LastSeen
		existing.OccurrenceCount++
		payload, err := jsonField(incidentPayload(*existing))
		if err != nil {
			return false, fmt.Errorf("incident payload: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `
			UPDATE events SET occurred_at = ?, payload_json = ?
			WHERE kind = ? AND subject_kind = ? AND subject_id = ?
		`, timeRFC3339(existing.LastSeen), payload, IncidentEventKind, incidentSubject, record.Fingerprint); err != nil {
			return false, fmt.Errorf("incident update: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, fmt.Errorf("incident put commit: %w", err)
	}
	committed = true
	return inserted, nil
}

// Get returns one incident by its stable fingerprint. Deterministic legacy IDs
// remain valid because they are normalized to fingerprints by Put.
func (d *IncidentDAO) Get(ctx context.Context, id string) (*IncidentRecord, error) {
	if d == nil || d.events == nil {
		return nil, errors.New("incident store not configured")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("incident get: id required")
	}
	event, err := d.events.FirstBySubjectKind(ctx, incidentSubject, id, IncidentEventKind)
	if err != nil {
		return nil, err
	}
	return incidentFromEvent(event)
}

// ListSince returns classified incidents at or after since, newest first.
func (d *IncidentDAO) ListSince(ctx context.Context, since time.Time, limit int) ([]IncidentRecord, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("incident store not configured")
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT `+eventColumns+`
		FROM events
		WHERE kind = ? AND occurred_at >= ?
		ORDER BY occurred_at DESC, id DESC
		LIMIT ?
	`, IncidentEventKind, timeRFC3339(since.UTC()), limit)
	if err != nil {
		return nil, fmt.Errorf("incident list: %w", err)
	}
	defer rows.Close()
	events, err := scanEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("incident list: %w", err)
	}
	out := make([]IncidentRecord, 0, len(events))
	for _, event := range events {
		record, err := incidentFromEvent(event)
		if err != nil {
			return nil, err
		}
		out = append(out, *record)
	}
	return out, nil
}

// ListAggregated returns all persisted incidents grouped by their stable
// classification identity and sorted independently of insertion order.
func (d *IncidentDAO) ListAggregated(ctx context.Context) ([]IncidentSummary, error) {
	records, err := d.ListSince(ctx, time.Time{}, int(^uint(0)>>1))
	if err != nil {
		return nil, err
	}
	type incidentKey struct {
		class      IncidentClass
		source     string
		dependency string
		shape      string
		summary    string
		retryable  bool
	}
	grouped := make(map[incidentKey]IncidentSummary, len(records))
	for _, record := range records {
		key := incidentKey{
			class: record.Class, source: record.Source, dependency: record.Dependency,
			shape: record.Shape, summary: record.Summary, retryable: record.Retryable,
		}
		summary := grouped[key]
		if summary.Occurrences == 0 {
			summary = IncidentSummary{
				Class: record.Class, Source: record.Source, Dependency: record.Dependency,
				Shape: record.Shape, Summary: record.Summary, Retryable: record.Retryable,
				Disposition: dispositionForIncidentClass(record.Class),
			}
		}
		summary.Occurrences += record.OccurrenceCount
		grouped[key] = summary
	}

	out := make([]IncidentSummary, 0, len(grouped))
	for _, summary := range grouped {
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool {
		left := out[i]
		right := out[j]
		leftKey := left.Class.String() + "\x00" + left.Dependency + "\x00" + left.Source + "\x00" + left.Shape + "\x00" + left.Summary
		rightKey := right.Class.String() + "\x00" + right.Dependency + "\x00" + right.Source + "\x00" + right.Shape + "\x00" + right.Summary
		if leftKey != rightKey {
			return leftKey < rightKey
		}
		return !left.Retryable && right.Retryable
	})
	return out, nil
}

func dispositionForIncidentClass(class IncidentClass) string {
	if class == IncidentClassExternalDependency {
		return IncidentDispositionWaitForDependencyRecovery
	}
	return ""
}

func validateIncidentRecord(record *IncidentRecord) error {
	if record == nil {
		return errors.New("incident: record required")
	}
	if strings.TrimSpace(record.ID) == "" {
		return errors.New("incident: id required")
	}
	if strings.TrimSpace(record.Fingerprint) == "" {
		return errors.New("incident: fingerprint required")
	}
	if record.Class != IncidentClassExternalDependency {
		return fmt.Errorf("incident: unsupported class %q", record.Class)
	}
	if strings.TrimSpace(record.Source) == "" || strings.TrimSpace(record.Dependency) == "" ||
		strings.TrimSpace(record.Shape) == "" || strings.TrimSpace(record.Summary) == "" {
		return errors.New("incident: source + dependency + shape + summary required")
	}
	return nil
}

func normalizeIncidentRecord(record *IncidentRecord) {
	if record == nil {
		return
	}
	if strings.TrimSpace(record.Fingerprint) == "" {
		// IDs generated by the original classifier were already deterministic
		// fingerprints. Retain that representation for rolling upgrades.
		record.Fingerprint = strings.TrimSpace(record.ID)
	}
	now := record.OccurredAt
	if now.IsZero() {
		now = record.LastSeen
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if record.FirstSeen.IsZero() {
		record.FirstSeen = now
	}
	if record.LastSeen.IsZero() || record.LastSeen.Before(now) {
		record.LastSeen = now
	}
	record.OccurredAt = record.LastSeen
	if record.OccurrenceCount <= 0 {
		record.OccurrenceCount = 1
	}
}

func incidentPayload(record IncidentRecord) map[string]any {
	return map[string]any{
		"id":               record.ID,
		"fingerprint":      record.Fingerprint,
		"class":            record.Class.String(),
		"source":           record.Source,
		"dependency":       record.Dependency,
		"shape":            record.Shape,
		"summary":          record.Summary,
		"evidence":         record.Evidence,
		"retryable":        record.Retryable,
		"occurred_at":      timeRFC3339(record.OccurredAt.UTC()),
		"first_seen":       timeRFC3339(record.FirstSeen.UTC()),
		"last_seen":        timeRFC3339(record.LastSeen.UTC()),
		"occurrence_count": record.OccurrenceCount,
	}
}

func incidentFromEvent(event *Event) (*IncidentRecord, error) {
	if event == nil {
		return nil, errors.New("incident decode: event required")
	}
	record := &IncidentRecord{
		ID:              stringValue(event.Payload["id"]),
		Fingerprint:     stringValue(event.Payload["fingerprint"]),
		Class:           IncidentClass(stringValue(event.Payload["class"])),
		Source:          stringValue(event.Payload["source"]),
		Dependency:      stringValue(event.Payload["dependency"]),
		Shape:           stringValue(event.Payload["shape"]),
		Summary:         stringValue(event.Payload["summary"]),
		Evidence:        stringValue(event.Payload["evidence"]),
		Retryable:       boolValue(event.Payload["retryable"]),
		OccurredAt:      event.OccurredAt,
		OccurrenceCount: intValue(event.Payload["occurrence_count"]),
	}
	if raw := stringValue(event.Payload["occurred_at"]); raw != "" {
		if parsed, err := parseTime(raw); err == nil {
			record.OccurredAt = parsed
		}
	}
	if raw := stringValue(event.Payload["first_seen"]); raw != "" {
		if parsed, err := parseTime(raw); err == nil {
			record.FirstSeen = parsed
		}
	}
	if raw := stringValue(event.Payload["last_seen"]); raw != "" {
		if parsed, err := parseTime(raw); err == nil {
			record.LastSeen = parsed
		}
	}
	normalizeIncidentRecord(record)
	if err := validateIncidentRecord(record); err != nil {
		return nil, fmt.Errorf("incident decode %q: %w", event.SubjectID, err)
	}
	return record, nil
}

func firstIncidentByFingerprint(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, fingerprint string) (*IncidentRecord, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+eventColumns+` FROM events
		WHERE subject_kind = ? AND subject_id = ? AND kind = ?
		ORDER BY id ASC LIMIT 1
	`, incidentSubject, fingerprint, IncidentEventKind)
	var e Event
	var occurredAt, payload string
	var subjectKind, subjectID sql.NullString
	if err := row.Scan(&e.ID, &occurredAt, &e.Actor, &e.Kind, &subjectKind, &subjectID, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("incident get: %w", err)
	}
	e.OccurredAt, _ = parseTime(occurredAt)
	e.SubjectKind, e.SubjectID = subjectKind.String, subjectID.String
	if err := jsonInto(payload, &e.Payload); err != nil {
		return nil, fmt.Errorf("incident payload decode: %w", err)
	}
	return incidentFromEvent(&e)
}

func stringValue(value any) string {
	s, _ := value.(string)
	return s
}

func boolValue(value any) bool {
	b, _ := value.(bool)
	return b
}

func intValue(value any) int {
	switch n := value.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}
