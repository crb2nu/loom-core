// Package store is the canonical persistence layer for the Loom Mills.
//
// All mills state — backlog items, council runs, pipeline runs, stage and gate
// outcomes, KPI snapshots, evaluation scores, and a generic event log — lives
// in a single SQLite database with WAL journaling. This package owns the
// schema and exposes typed DAOs; nothing outside the mills package tree should
// open the DB directly.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/telemetry"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// Store wraps the SQLite database handle and exposes typed DAOs.
type Store struct {
	db *sql.DB

	Backlog  *BacklogDAO
	Council  *CouncilDAO
	Pipeline *PipelineDAO
	KPI      *KPIDAO
	Eval     *EvalDAO
	Events   *EventDAO
	// Incidents persists deterministic external-dependency classifications.
	Incidents *IncidentDAO
	// ClassificationVerdicts persists the first resolved dual-source failure
	// verdict for each escalated pipeline run in the append-only event log.
	ClassificationVerdicts *ClassificationVerdictDAO
	Roadmap                *RoadmapDAO

	// Mills v2 — Hierarchical Swarm DAOs.
	Squads    *SquadDAO
	Audit     *AuditDAO
	CrossRepo *CrossRepoDAO
	// Stamps persists target-bound cross-repository stamp intents. A stamp can
	// never be written without an explicit destination project.
	Stamps          *StampDAO
	Debate          *DebateDAO
	PolicyProposals *PolicyProposalDAO

	// Layer-2 durable workflow run/step journal (migration 004), written by
	// pkg/mills/workflow. It is also the source of truth for the
	// workflow_active_runs, workflow_completed_steps, and
	// workflow_failed_steps KPI snapshot values. Keep those values derived
	// from this DAO: lifecycle callbacks or process-local counters would drift
	// across scheduler/operator restarts. Legacy 'dag' runs never touch these
	// tables.
	Workflow *WorkflowDAO

	// Async Spinning-Room spin status store (migration 007). Backs
	// POST /api/mills/spin/async + GET /api/mills/spin/runs.
	Spin *SpinDAO

	// Runtime-minted project registry (migration 009). Backs
	// POST /api/mills/projects/bootstrap + the emitter's dynamic demand union.
	Bootstrap *BootstrapDAO

	// Durable MR source-head movement ledger (migration 016). Every observed
	// head movement on a run's merge request becomes an immutable row; a
	// settled movement invalidates the CI authorization and forces a re-gate
	// of the successor SHA (#374).
	MRHeadTransitions *MRHeadTransitionDAO

	// Per-backlog-item cross-stage memory journal (migration 017). One
	// journalengine.Snapshot per item; the pipeline records each stage outcome
	// into it and prompt builders render it as the stable prefix. Gated at the
	// call sites by LOOM_MILLS_ITEM_JOURNAL, default off.
	ItemMemory *ItemMemoryDAO

	// Council-lane cross-run memory journal (migration 018). One
	// journalengine.Snapshot for the whole lane; the council runner records one
	// turn per completed run and the editor prompt renders it inside the stable
	// prefix. Gated at the call sites by LOOM_MILLS_COUNCIL_MEMORY, default off.
	CouncilMemory *CouncilMemoryDAO

	// Serial merge queue (migration 024). One entry per pipeline run whose
	// merge stage handed the merge to the queue; the mergequeue processor
	// drives one head per (project, target_branch) lane through
	// rebase → pipeline → merge. Gated by policy merge_queue.enabled.
	MergeQueue *MergeQueueDAO
}

const transientRequeueEventKind = "pipeline.transient_requeue.claimed"

const overseerSoakTelemetryEventKind = "overseer.soak.daily"

// OverseerSoakDailyCounters is one durable UTC-day bucket of dry-run evidence.
// Day is always returned at UTC midnight. Decisions is the denominator for the
// other counters, so either derived count exceeding it is corrupt evidence.
type OverseerSoakDailyCounters struct {
	Day            time.Time `json:"day"`
	Decisions      int       `json:"decisions"`
	WouldHaveActed int       `json:"would_have_acted"`
	Disagreements  int       `json:"policy_disagreements"`
}

// RecordOverseerSoakDecision durably increments the UTC-day bucket for one
// overseer dry-run decision. One append-only event represents one decision;
// aggregation therefore remains safe across process restarts and concurrent
// writers without a read/modify/write race.
func (s *Store) RecordOverseerSoakDecision(ctx context.Context, at time.Time, wouldHaveActed, policyDisagreement bool) error {
	if s == nil || s.db == nil {
		return errors.New("overseer soak telemetry: store not configured")
	}
	if at.IsZero() {
		return errors.New("overseer soak telemetry: decision time required")
	}
	day := at.UTC().Truncate(24 * time.Hour)
	wouldAct, disagreement := 0, 0
	if wouldHaveActed {
		wouldAct = 1
	}
	if policyDisagreement {
		disagreement = 1
	}
	payload, err := json.Marshal(map[string]int{
		"decisions": 1, "would_have_acted": wouldAct, "policy_disagreements": disagreement,
	})
	if err != nil {
		return fmt.Errorf("overseer soak telemetry payload: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO events (occurred_at, actor, kind, subject_kind, subject_id, payload_json)
		VALUES (?, 'overseer', ?, 'utc_day', ?, ?)
	`, timeRFC3339(at.UTC()), overseerSoakTelemetryEventKind, day.Format("2006-01-02"), string(payload))
	if err != nil {
		return fmt.Errorf("overseer soak telemetry record: %w", err)
	}
	return nil
}

// OverseerSoakTelemetry returns the seven complete UTC days immediately before
// the UTC day containing windowEnd. Missing days are intentionally omitted so
// the evaluator can distinguish absent evidence from an explicitly populated
// bucket. Any malformed stored counter makes the entire read fail closed.
func (s *Store) OverseerSoakTelemetry(ctx context.Context, windowEnd time.Time) ([]OverseerSoakDailyCounters, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("overseer soak telemetry: store not configured")
	}
	if windowEnd.IsZero() {
		return nil, errors.New("overseer soak telemetry: window end required")
	}
	end := windowEnd.UTC().Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -7)
	rows, err := s.db.QueryContext(ctx, `
		SELECT subject_id, payload_json
		FROM events
		WHERE kind = ? AND subject_kind = 'utc_day'
		  AND subject_id >= ? AND subject_id < ?
		ORDER BY subject_id, id
	`, overseerSoakTelemetryEventKind, start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("overseer soak telemetry read: %w", err)
	}
	defer rows.Close()

	byDay := make(map[string]*OverseerSoakDailyCounters, 7)
	for rows.Next() {
		var dayText, payload string
		if err := rows.Scan(&dayText, &payload); err != nil {
			return nil, fmt.Errorf("overseer soak telemetry scan: %w", err)
		}
		day, err := time.Parse("2006-01-02", dayText)
		if err != nil || day.Before(start) || !day.Before(end) {
			return nil, fmt.Errorf("overseer soak telemetry: invalid UTC day %q", dayText)
		}
		var event struct {
			Decisions      int `json:"decisions"`
			WouldHaveActed int `json:"would_have_acted"`
			Disagreements  int `json:"policy_disagreements"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil || event.Decisions != 1 || event.WouldHaveActed < 0 || event.WouldHaveActed > 1 || event.Disagreements < 0 || event.Disagreements > 1 {
			return nil, fmt.Errorf("overseer soak telemetry: malformed counters for %s", dayText)
		}
		bucket := byDay[dayText]
		if bucket == nil {
			bucket = &OverseerSoakDailyCounters{Day: day}
			byDay[dayText] = bucket
		}
		bucket.Decisions += event.Decisions
		bucket.WouldHaveActed += event.WouldHaveActed
		bucket.Disagreements += event.Disagreements
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("overseer soak telemetry rows: %w", err)
	}
	result := make([]OverseerSoakDailyCounters, 0, len(byDay))
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		if bucket := byDay[day.Format("2006-01-02")]; bucket != nil {
			result = append(result, *bucket)
		}
	}
	return result, nil
}

// TransientRequeueClaim is the durable result of attempting to consume one
// automatic transient-failure requeue. AttemptsUsed is capped at Cap even
// when the allowance was already exhausted.
type TransientRequeueClaim struct {
	Claimed      bool
	AttemptsUsed int
	Cap          int
}

// ClaimTransientRequeue atomically consumes one requeue allowance for the
// durable backlog identity. The event log is intentionally used as the
// ledger: it survives process restarts and avoids an attempt-local counter.
// SQLite serializes the conditional INSERT, so concurrent callers cannot
// create more than cap claim rows.
func (s *Store) ClaimTransientRequeue(ctx context.Context, backlogID string, cap int) (TransientRequeueClaim, error) {
	result := TransientRequeueClaim{Cap: cap}
	if s == nil || s.db == nil {
		return result, errors.New("transient requeue claim: store not configured")
	}
	backlogID = strings.TrimSpace(backlogID)
	if backlogID == "" {
		return result, errors.New("transient requeue claim: backlog id required")
	}
	if cap <= 0 {
		return result, errors.New("transient requeue claim: cap must be positive")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("transient requeue claim begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := timeRFC3339(time.Now().UTC())
	insert, err := tx.ExecContext(ctx, `
		INSERT INTO events (occurred_at, actor, kind, subject_kind, subject_id, payload_json)
		SELECT ?, 'pipeline.transient-requeue', ?, 'backlog_item', ?, '{}'
		WHERE (SELECT COUNT(*) FROM events
		       WHERE kind = ? AND subject_kind = 'backlog_item' AND subject_id = ?) < ?
	`, now, transientRequeueEventKind, backlogID, transientRequeueEventKind, backlogID, cap)
	if err != nil {
		return result, fmt.Errorf("transient requeue claim insert: %w", err)
	}
	rows, err := insert.RowsAffected()
	if err != nil {
		return result, fmt.Errorf("transient requeue claim rows: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM events
		WHERE kind = ? AND subject_kind = 'backlog_item' AND subject_id = ?
	`, transientRequeueEventKind, backlogID).Scan(&result.AttemptsUsed); err != nil {
		return result, fmt.Errorf("transient requeue claim count: %w", err)
	}
	if result.AttemptsUsed > cap {
		result.AttemptsUsed = cap
	}
	result.Claimed = rows == 1
	if err := tx.Commit(); err != nil {
		return TransientRequeueClaim{Cap: cap}, fmt.Errorf("transient requeue claim commit: %w", err)
	}
	return result, nil
}

const (
	// ExternalDependencyIncidentEventKind is the stable event kind used for
	// one external dependency incident cluster.
	ExternalDependencyIncidentEventKind = "external_dependency_incident"
	// ExternalDependencyIncidentRefSubject identifies the affected git ref in
	// the event subject fields.
	ExternalDependencyIncidentRefSubject = "git_ref"
)

// CountExternalDependencyIncidentClusters returns the number of incident
// cluster events recorded for one ref at or after since. Keeping this query on
// Store gives policy callers one narrow contract without exposing the SQL
// handle or requiring them to scan the generic event log.
func (s *Store) CountExternalDependencyIncidentClusters(ctx context.Context, ref string, since time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("external incident clusters: store not configured")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, errors.New("external incident clusters: ref required")
	}
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM events
		WHERE kind = ? AND subject_kind = ? AND subject_id = ? AND occurred_at >= ?
	`, ExternalDependencyIncidentEventKind, ExternalDependencyIncidentRefSubject, ref, timeRFC3339(since.UTC())).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("external incident clusters count: %w", err)
	}
	return count, nil
}

// Options controls Store creation.
type Options struct {
	// Path is the filesystem path to the SQLite database. Use ":memory:" for
	// in-process tests.
	Path string

	// SkipMigrations omits the goose migration step. Useful for tests that
	// want to inspect a known-empty file.
	SkipMigrations bool
}

// Open opens (or creates) the SQLite database at opts.Path, sets the required
// PRAGMAs, and applies pending migrations.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, errors.New("store: Options.Path must not be empty")
	}

	// PRAGMAs are embedded in the DSN so every connection in the pool inherits
	// them. ExecContext-only PRAGMAs would only stick on one pooled connection,
	// which causes the next acquired conn to lack busy_timeout / foreign_keys
	// and hit SQLITE_BUSY under concurrent writers.
	dsn := buildDSN(opts.Path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}

	// One writer + many readers under WAL. busy_timeout retries inside the
	// driver, so concurrent writers serialise without surfacing SQLITE_BUSY.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	if !opts.SkipMigrations {
		if err := Migrate(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: migrate: %w", err)
		}
	}

	s := &Store{db: db}
	s.Backlog = &BacklogDAO{db: db}
	s.Council = &CouncilDAO{db: db}
	s.Pipeline = &PipelineDAO{
		db:                       db,
		terminalConflictRecorder: telemetry.DefaultTerminalStateConflictRecorder(),
	}
	s.KPI = &KPIDAO{db: db}
	s.Eval = &EvalDAO{db: db}
	s.Events = &EventDAO{db: db}
	s.Incidents = &IncidentDAO{db: db, events: s.Events}
	s.ClassificationVerdicts = &ClassificationVerdictDAO{events: s.Events}
	s.Roadmap = &RoadmapDAO{db: db}

	// Mills v2.
	s.Squads = &SquadDAO{db: db}
	s.Audit = &AuditDAO{db: db}
	s.CrossRepo = &CrossRepoDAO{db: db}
	s.Stamps = &StampDAO{db: db}
	s.Debate = &DebateDAO{db: db}
	s.PolicyProposals = &PolicyProposalDAO{db: db}

	// Layer-2 durable workflow journal. KPI snapshots query this same DAO;
	// there is deliberately no separate runner-side counter wiring.
	s.Workflow = &WorkflowDAO{db: db}

	// Async Spinning-Room spin status store.
	s.Spin = &SpinDAO{db: db}

	// Runtime-minted project registry.
	s.Bootstrap = &BootstrapDAO{db: db}

	// MR source-head movement ledger.
	s.MRHeadTransitions = &MRHeadTransitionDAO{db: db}

	// Per-backlog-item cross-stage memory journal.
	s.ItemMemory = &ItemMemoryDAO{db: db}

	// Council-lane cross-run memory journal.
	s.CouncilMemory = &CouncilMemoryDAO{db: db}

	// Serial merge queue.
	s.MergeQueue = &MergeQueueDAO{db: db}
	return s, nil
}

const (
	classificationVerdictActor       = "pipeline.classification-reconciler"
	classificationVerdictEventKind   = "pipeline.classification.verdict"
	classificationVerdictSubjectKind = "pipeline_run"
)

// ClassificationVerdictRecord is the persistence representation of a
// dual-source verdict. Strings keep the store independent of pipeline policy
// types and prevent a package import cycle.
type ClassificationVerdictRecord struct {
	FailureID        string    `json:"failure_id"`
	PrimarySource    string    `json:"primary_source"`
	PrimaryClass     string    `json:"primary_class"`
	SecondarySource  string    `json:"secondary_source"`
	SecondaryClass   string    `json:"secondary_class"`
	ResolvedClass    string    `json:"resolved_class"`
	Disagreement     bool      `json:"disagreement"`
	ResolutionReason string    `json:"resolution_reason"`
	ResolutionRule   string    `json:"resolution_rule"`
	ResolvedAt       time.Time `json:"resolved_at"`
}

// ClassificationVerdictDAO stores verdicts as keyed append-only events. This
// gives first-writer stability and durable round trips without duplicating the
// event log's idempotency and migration machinery.
type ClassificationVerdictDAO struct {
	events *EventDAO
}

// PutClassificationVerdict records the first verdict for a failure. It returns
// false when that failure already has a durable verdict.
func (d *ClassificationVerdictDAO) PutClassificationVerdict(
	ctx context.Context,
	v ClassificationVerdictRecord,
) (bool, error) {
	if d == nil || d.events == nil {
		return false, errors.New("classification verdict store not configured")
	}
	if strings.TrimSpace(v.FailureID) == "" {
		return false, errors.New("classification verdict: failure ID required")
	}
	payload := make(map[string]any)
	raw, err := json.Marshal(v)
	if err != nil {
		return false, fmt.Errorf("classification verdict marshal: %w", err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, fmt.Errorf("classification verdict payload: %w", err)
	}
	return d.events.AppendOnceBySubjectKind(ctx, &Event{
		OccurredAt:  v.ResolvedAt,
		Actor:       classificationVerdictActor,
		Kind:        classificationVerdictEventKind,
		SubjectKind: classificationVerdictSubjectKind,
		SubjectID:   v.FailureID,
		Payload:     payload,
	})
}

// GetClassificationVerdict retrieves the stable verdict for a failure.
func (d *ClassificationVerdictDAO) GetClassificationVerdict(
	ctx context.Context,
	failureID string,
) (ClassificationVerdictRecord, error) {
	if d == nil || d.events == nil {
		return ClassificationVerdictRecord{}, errors.New("classification verdict store not configured")
	}
	event, err := d.events.FirstBySubjectKind(
		ctx, classificationVerdictSubjectKind, failureID, classificationVerdictEventKind,
	)
	if err != nil {
		return ClassificationVerdictRecord{}, err
	}
	raw, err := json.Marshal(event.Payload)
	if err != nil {
		return ClassificationVerdictRecord{}, fmt.Errorf("classification verdict encode payload: %w", err)
	}
	var v ClassificationVerdictRecord
	if err := json.Unmarshal(raw, &v); err != nil {
		return ClassificationVerdictRecord{}, fmt.Errorf("classification verdict decode payload: %w", err)
	}
	return v, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB exposes the raw handle for advanced callers (migrations, ad-hoc reads).
// Most code should use the typed DAOs.
func (s *Store) DB() *sql.DB {
	return s.db
}

// buildDSN composes a modernc.org/sqlite DSN with the PRAGMAs every mills
// connection needs:
//   - journal_mode=WAL: durable + concurrent-read friendly.
//   - synchronous=NORMAL: safe under WAL with negligible durability cost.
//   - foreign_keys=ON: enforce REFERENCES; off-by-default in SQLite.
//   - busy_timeout=5000: in-driver retry on SQLITE_BUSY for up to 5s.
//
// The driver evaluates `_pragma=` query params on every new pooled connection,
// so each acquired conn arrives with the right settings.
func buildDSN(path string) string {
	pragmas := []string{
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"foreign_keys(ON)",
		"busy_timeout(5000)",
		// temp_store(MEMORY): statements that need SQLite temp storage — the
		// statement journal of a table-rebuild migration (INSERT…SELECT +
		// DROP + RENAME, first hit by migration 019), large sorts — must not
		// touch a temp FILE. The operator container runs with
		// readOnlyRootFilesystem and only the DB PVC writable, so the default
		// file-backed temp store fails the whole boot with
		// SQLITE_IOERR_GETTEMPPATH (the 2026-08-01 crashloop). Mills tables
		// are small; memory temp storage is safe and deterministic.
		"temp_store(MEMORY)",
	}
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + q.Encode()
}
