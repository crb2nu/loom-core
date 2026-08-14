package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrNotFound is returned when a lookup misses.
var ErrNotFound = errors.New("mills store: not found")

// ErrStaleWrite is returned when a compare-and-swap mutation loses a race.
var ErrStaleWrite = errors.New("mills store: stale write")

// ErrBacklogNotGradable is returned when a grade targets non-terminal work.
var ErrBacklogNotGradable = errors.New("mills store: backlog item is not merged or retired")

// StaleWriteError identifies the row and caller revision rejected by a
// compare-and-swap guard. It unwraps to ErrStaleWrite for stable classification.
type StaleWriteError struct {
	Entity           string
	ID               string
	ExpectedRevision int64
	Reason           string
}

func (e *StaleWriteError) Error() string {
	if e == nil {
		return ErrStaleWrite.Error()
	}
	message := fmt.Sprintf("%s: %s %s at revision %d", ErrStaleWrite, e.Entity, e.ID, e.ExpectedRevision)
	if e.Reason != "" {
		message += ": " + e.Reason
	}
	return message
}

func (e *StaleWriteError) Unwrap() error { return ErrStaleWrite }

// BacklogDAO exposes CRUD + queries against the backlog_items table.
type BacklogDAO struct {
	db *sql.DB
}

// EscalationRecheck is the durable throttle for GitLab escalation lookups.
type EscalationRecheck struct {
	MRIID        int64
	RecheckAfter time.Time
	Streak       int
}

// RecordScopeDeferral increments durable aging and atomically creates a
// reservation when either threshold is reached.
func (d *BacklogDAO) RecordScopeDeferral(ctx context.Context, id string, now time.Time, countThreshold int, ageThreshold time.Duration) (ScopeFairnessState, bool, error) {
	now = now.UTC()
	_, err := d.db.ExecContext(ctx, `INSERT INTO scope_fairness_state(backlog_id, first_deferred_at, deferral_count) VALUES(?,?,1)
		ON CONFLICT(backlog_id) DO UPDATE SET deferral_count=deferral_count+1`, id, timeRFC3339(now))
	if err != nil {
		return ScopeFairnessState{}, false, fmt.Errorf("scope fairness defer %s: %w", id, err)
	}
	state, err := d.ScopeFairness(ctx, id)
	if err != nil {
		return state, false, err
	}
	trip := state.ReservedAt == nil && (state.DeferralCount >= countThreshold || now.Sub(state.FirstDeferredAt) >= ageThreshold)
	if trip {
		res, err := d.db.ExecContext(ctx, `UPDATE scope_fairness_state SET reserved_at=? WHERE backlog_id=? AND reserved_at IS NULL`, timeRFC3339(now), id)
		if err != nil {
			return state, false, err
		}
		n, _ := res.RowsAffected()
		trip = n == 1
		if trip {
			state.ReservedAt = &now
		}
	}
	return state, trip, nil
}

func (d *BacklogDAO) ScopeFairness(ctx context.Context, id string) (ScopeFairnessState, error) {
	var s ScopeFairnessState
	var first string
	var reserved sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT backlog_id, first_deferred_at, deferral_count, reserved_at FROM scope_fairness_state WHERE backlog_id=?`, id).Scan(&s.BacklogID, &first, &s.DeferralCount, &reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	if err != nil {
		return s, err
	}
	s.FirstDeferredAt, err = parseTime(first)
	if err != nil {
		return s, err
	}
	if reserved.Valid {
		t, e := parseTime(reserved.String)
		if e != nil {
			return s, e
		}
		s.ReservedAt = &t
	}
	return s, nil
}

func (d *BacklogDAO) ScopeReservations(ctx context.Context) ([]ScopeFairnessState, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT s.backlog_id,s.first_deferred_at,s.deferral_count,s.reserved_at FROM scope_fairness_state s JOIN backlog_items b ON b.id=s.backlog_id WHERE s.reserved_at IS NOT NULL AND b.state=? ORDER BY s.reserved_at,s.backlog_id`, string(BacklogQueued))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScopeFairnessState{}
	for rows.Next() {
		var s ScopeFairnessState
		var first, reserved string
		if err := rows.Scan(&s.BacklogID, &first, &s.DeferralCount, &reserved); err != nil {
			return nil, err
		}
		s.FirstDeferredAt, _ = parseTime(first)
		t, e := parseTime(reserved)
		if e != nil {
			return nil, e
		}
		s.ReservedAt = &t
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *BacklogDAO) ResetScopeFairness(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM scope_fairness_state WHERE backlog_id=?`, id)
	return err
}

// ScopeFairnessSummary returns current durable fairness values for KPI export.
func (d *BacklogDAO) ScopeFairnessSummary(ctx context.Context, now time.Time) (deferrals, reservations int, maxAgeSeconds float64, err error) {
	var first sql.NullString
	err = d.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(s.deferral_count),0), COALESCE(SUM(CASE WHEN s.reserved_at IS NOT NULL THEN 1 ELSE 0 END),0), MIN(s.first_deferred_at)
		FROM scope_fairness_state s JOIN backlog_items b ON b.id=s.backlog_id WHERE b.state=?`, string(BacklogQueued)).Scan(&deferrals, &reservations, &first)
	if err != nil {
		return
	}
	if first.Valid {
		var t time.Time
		t, err = parseTime(first.String)
		if err == nil {
			maxAgeSeconds = now.UTC().Sub(t).Seconds()
			if maxAgeSeconds < 0 {
				maxAgeSeconds = 0
			}
		}
	}
	return
}

// EscalationRecheck returns an item's persisted sweep throttle.
func (d *BacklogDAO) EscalationRecheck(ctx context.Context, id string) (EscalationRecheck, error) {
	var state EscalationRecheck
	var after string
	err := d.db.QueryRowContext(ctx, `SELECT mr_iid, recheck_after, recheck_streak FROM escalation_sweep_state WHERE backlog_id = ?`, id).
		Scan(&state.MRIID, &after, &state.Streak)
	if errors.Is(err, sql.ErrNoRows) {
		return state, ErrNotFound
	}
	if err != nil {
		return state, fmt.Errorf("escalation recheck %s: %w", id, err)
	}
	state.RecheckAfter, err = parseTime(after)
	if err != nil {
		return state, fmt.Errorf("escalation recheck %s timestamp: %w", id, err)
	}
	return state, nil
}

// DeferEscalationRecheck transactionally advances the exponential throttle.
// A changed MR IID starts a fresh series. The delay is 30m, 1h, 2h, 4h ...
// capped at 24h.
func (d *BacklogDAO) DeferEscalationRecheck(ctx context.Context, id string, mrIID int64, now time.Time) (EscalationRecheck, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return EscalationRecheck{}, err
	}
	defer func() { _ = tx.Rollback() }()
	streak := 0
	var storedIID int64
	if err := tx.QueryRowContext(ctx, `SELECT mr_iid, recheck_streak FROM escalation_sweep_state WHERE backlog_id = ?`, id).Scan(&storedIID, &streak); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return EscalationRecheck{}, err
	}
	if storedIID != mrIID {
		streak = 0
	}
	streak++
	delay := 30 * time.Minute * time.Duration(1<<min(streak-1, 6))
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	state := EscalationRecheck{MRIID: mrIID, RecheckAfter: now.UTC().Add(delay), Streak: streak}
	_, err = tx.ExecContext(ctx, `INSERT INTO escalation_sweep_state(backlog_id, mr_iid, recheck_after, recheck_streak) VALUES(?,?,?,?)
		ON CONFLICT(backlog_id) DO UPDATE SET mr_iid=excluded.mr_iid, recheck_after=excluded.recheck_after, recheck_streak=excluded.recheck_streak`,
		id, mrIID, timeRFC3339(state.RecheckAfter), streak)
	if err != nil {
		return EscalationRecheck{}, err
	}
	if err := tx.Commit(); err != nil {
		return EscalationRecheck{}, err
	}
	return state, nil
}

// ClearEscalationRecheck removes stale throttle state after resolution.
func (d *BacklogDAO) ClearEscalationRecheck(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM escalation_sweep_state WHERE backlog_id = ?`, id)
	return err
}

const backlogColumns = `id, gitlab_issue_iid, title, labels_json, state, priority,
		spec_doc, spec_anchor, success_json, budget_json, policy_json, slices_json,
		dependencies_json, council_run_id, created_by, created_at, updated_at, plan_id,
		target_project, claim_version, row_version, grade, grade_note, grade_actor, graded_at`

// Put inserts or compare-and-swap updates a backlog item. CreatedAt is
// preserved if the row already exists; UpdatedAt is always set to now. Updates
// require item.Revision to match and increment the stored row revision.
// claim_version is preserved: only Store.ClaimPipelineStart may advance it.
func (d *BacklogDAO) Put(ctx context.Context, item *BacklogItem) error {
	if item == nil || item.ID == "" {
		return errors.New("backlog: item.ID required")
	}
	if item.Revision < 0 || item.ClaimVersion < 0 {
		return errors.New("backlog: versions must be >= 0")
	}
	if item.Revision == math.MaxInt64 || item.ClaimVersion == math.MaxInt64 {
		return errors.New("backlog: versions must be < max int64")
	}
	now := time.Now().UTC()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	item.UpdatedAt = now

	labels, err := jsonField(item.Labels)
	if err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	success, err := jsonField(item.Success)
	if err != nil {
		return fmt.Errorf("success: %w", err)
	}
	budget, err := jsonField(item.Budget)
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	policy, err := jsonField(item.Policy)
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	slices, err := jsonField(item.Slices)
	if err != nil {
		return fmt.Errorf("slices: %w", err)
	}
	deps, err := jsonField(item.Dependencies)
	if err != nil {
		return fmt.Errorf("dependencies: %w", err)
	}

	var iid sql.NullInt64
	if item.GitLabIssueIID != nil {
		iid = sql.NullInt64{Int64: *item.GitLabIssueIID, Valid: true}
	}
	var council sql.NullString
	if item.CouncilRunID != nil {
		council = sql.NullString{String: *item.CouncilRunID, Valid: true}
	}
	var gradedAt any
	if item.GradedAt != nil {
		gradedAt = timeRFC3339(*item.GradedAt)
	}

	var storedClaimVersion, storedRevision int64
	err = d.db.QueryRowContext(ctx, `
			INSERT INTO backlog_items (`+backlogColumns+`)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			gitlab_issue_iid = excluded.gitlab_issue_iid,
			title            = excluded.title,
			labels_json      = excluded.labels_json,
			state            = excluded.state,
			priority         = excluded.priority,
			spec_doc         = excluded.spec_doc,
			spec_anchor      = excluded.spec_anchor,
			success_json     = excluded.success_json,
			budget_json      = excluded.budget_json,
			policy_json      = excluded.policy_json,
			slices_json      = excluded.slices_json,
			dependencies_json= excluded.dependencies_json,
			council_run_id   = excluded.council_run_id,
			created_by       = excluded.created_by,
			updated_at       = excluded.updated_at,
			plan_id          = excluded.plan_id,
			target_project   = excluded.target_project,
			grade            = excluded.grade,
			grade_note       = excluded.grade_note,
			grade_actor      = excluded.grade_actor,
			graded_at        = excluded.graded_at,
			row_version      = backlog_items.row_version + 1
		WHERE backlog_items.row_version = ?
		RETURNING claim_version, row_version
	`,
		item.ID, iid, item.Title, labels, string(item.State), string(item.Priority),
		nullStr(item.SpecDoc), nullStr(item.SpecAnchor), success, budget, policy, slices,
		deps, council, item.CreatedBy, timeRFC3339(item.CreatedAt), timeRFC3339(item.UpdatedAt),
		nullStr(item.PlanID), nullStr(item.TargetProject), int64(0), int64(1),
		nullStr(item.Grade), nullStr(item.GradeNote), nullStr(item.GradeActor), gradedAt,
		item.Revision,
	).Scan(&storedClaimVersion, &storedRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return &StaleWriteError{
			Entity:           "backlog item",
			ID:               item.ID,
			ExpectedRevision: item.Revision,
			Reason:           "row revision changed",
		}
	}
	if err != nil {
		return fmt.Errorf("backlog put %s: %w", item.ID, err)
	}
	item.ClaimVersion = storedClaimVersion
	item.Revision = storedRevision
	return nil
}

// GradeRun atomically updates a terminal run's backlog item grade and appends
// its immutable history event. The returned item is the new denormalized head.
func (d *BacklogDAO) GradeRun(ctx context.Context, runID, grade, note, actor string, gradedAt time.Time) (*BacklogItem, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("grade run %s begin: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var itemID string
	if err := tx.QueryRowContext(ctx, `SELECT backlog_id FROM pipeline_runs WHERE id = ?`, runID).Scan(&itemID); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("grade run %s lookup: %w", runID, err)
	}

	item, err := scanBacklog(tx.QueryRowContext(ctx, `SELECT `+backlogColumns+` FROM backlog_items WHERE id = ?`, itemID))
	if err != nil {
		return nil, err
	}
	if item.State != BacklogMerged && item.State != BacklogEscalated && item.State != BacklogRetired {
		return nil, fmt.Errorf("%w: %s is %s", ErrBacklogNotGradable, item.ID, item.State)
	}
	prior := item.Grade
	res, err := tx.ExecContext(ctx, `
		UPDATE backlog_items
		SET grade = ?, grade_note = ?, grade_actor = ?, graded_at = ?,
			row_version = row_version + 1, updated_at = ?
		WHERE id = ? AND row_version = ?`,
		grade, nullStr(note), actor, timeRFC3339(gradedAt), timeRFC3339(gradedAt), item.ID, item.Revision)
	if err != nil {
		return nil, fmt.Errorf("grade backlog item %s: %w", item.ID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, &StaleWriteError{Entity: "backlog item", ID: item.ID, ExpectedRevision: item.Revision, Reason: "row revision changed"}
	}
	payload, err := jsonField(map[string]any{
		"grade": grade, "prior_grade": prior, "note": note, "actor": actor,
		"run_id": runID, "item_id": item.ID, "plan_id": item.PlanID,
	})
	if err != nil {
		return nil, fmt.Errorf("grade event payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events (occurred_at, actor, kind, subject_kind, subject_id, payload_json)
		VALUES (?,?,?,?,?,?)`, timeRFC3339(gradedAt), actor, "bolt.graded", "pipeline_run", runID, payload); err != nil {
		return nil, fmt.Errorf("grade event append: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("grade run %s commit: %w", runID, err)
	}
	item.Grade, item.GradeNote, item.GradeActor, item.GradedAt = grade, note, actor, &gradedAt
	item.Revision++
	item.UpdatedAt = gradedAt
	return item, nil
}

// TasteAggregates returns per-plan taste over all merged work and overall
// merged-grade coverage for the rolling window ending at now.
func (d *BacklogDAO) TasteAggregates(ctx context.Context, now time.Time, window time.Duration) (TasteAggregates, error) {
	if window <= 0 {
		window = 14 * 24 * time.Hour
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT plan_id,
			SUM(CASE WHEN grade = 'keep' THEN 1 ELSE 0 END),
			SUM(CASE WHEN grade = 'meh' THEN 1 ELSE 0 END),
			SUM(CASE WHEN grade = 'regret' THEN 1 ELSE 0 END),
			SUM(CASE WHEN grade IN ('keep','meh','regret') THEN 1 ELSE 0 END),
			COUNT(*)
		FROM backlog_items
		WHERE state = ? AND plan_id IS NOT NULL AND trim(plan_id) <> ''
		GROUP BY plan_id ORDER BY plan_id`, string(BacklogMerged))
	if err != nil {
		return TasteAggregates{}, fmt.Errorf("taste aggregates: %w", err)
	}
	defer rows.Close()
	out := TasteAggregates{Plans: []PlanTasteAggregate{}}
	for rows.Next() {
		var p PlanTasteAggregate
		if err := rows.Scan(&p.PlanID, &p.Keep, &p.Meh, &p.Regret, &p.Graded, &p.Merged); err != nil {
			return TasteAggregates{}, fmt.Errorf("taste aggregate scan: %w", err)
		}
		if p.Graded > 0 {
			p.RegretRate = float64(p.Regret) / float64(p.Graded)
		}
		if p.Merged > 0 {
			p.GradeCoverage = float64(p.Graded) / float64(p.Merged)
		}
		out.Plans = append(out.Plans, p)
	}
	if err := rows.Err(); err != nil {
		return TasteAggregates{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	err = d.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN grade IN ('keep','meh','regret') THEN 1 ELSE 0 END), 0), COUNT(*)
		FROM backlog_items WHERE state = ? AND updated_at >= ?`,
		string(BacklogMerged), timeRFC3339(now.UTC().Add(-window))).Scan(&out.OverallGraded14d, &out.OverallMerged14d)
	if err != nil {
		return TasteAggregates{}, fmt.Errorf("taste overall coverage: %w", err)
	}
	if out.OverallMerged14d > 0 {
		out.OverallCoverage14d = float64(out.OverallGraded14d) / float64(out.OverallMerged14d)
	}
	return out, nil
}

// TransitionState conditionally changes only a backlog item's lifecycle state.
// The aggregate claim version and caller-observed from state fence older runs,
// while deliberately ignoring row revision lets an in-flight run preserve
// metadata or policy edits committed after it started. A row already at to with
// the expected aggregate version is an idempotent success.
func (d *BacklogDAO) TransitionState(
	ctx context.Context,
	id string,
	expectedClaimVersion int64,
	from BacklogState,
	to BacklogState,
) (*BacklogItem, error) {
	if id == "" {
		return nil, errors.New("backlog transition: id required")
	}
	if expectedClaimVersion < 0 {
		return nil, errors.New("backlog transition: expected claim version must be >= 0")
	}
	if from == "" || to == "" {
		return nil, errors.New("backlog transition: from and to states required")
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("backlog transition %s begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	item, err := scanBacklog(tx.QueryRowContext(ctx, `
		UPDATE backlog_items
		SET state = ?, row_version = row_version + 1, updated_at = ?
		WHERE id = ? AND claim_version = ? AND state = ? AND state <> ?
			AND row_version < ?
		RETURNING `+backlogColumns+`
	`, string(to), timeRFC3339(now), id, expectedClaimVersion, string(from), string(to), int64(math.MaxInt64)))
	if err == nil {
		if to != BacklogQueued && to != BacklogRunning {
			if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM scope_fairness_state WHERE backlog_id=?`, id); deleteErr != nil {
				return nil, fmt.Errorf("backlog transition %s clear scope fairness: %w", id, deleteErr)
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("backlog transition %s commit: %w", id, err)
		}
		return item, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("backlog transition %s: %w", id, err)
	}

	current, err := scanBacklog(tx.QueryRowContext(ctx,
		`SELECT `+backlogColumns+` FROM backlog_items WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if current.ClaimVersion == expectedClaimVersion && current.State == to {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("backlog transition %s idempotent commit: %w", id, err)
		}
		return current, nil
	}
	return nil, &StaleWriteError{
		Entity:           "backlog aggregate",
		ID:               id,
		ExpectedRevision: expectedClaimVersion,
		Reason: fmt.Sprintf("state=%s claim_version=%d; expected state=%s claim_version=%d",
			current.State, current.ClaimVersion, from, expectedClaimVersion),
	}
}

// TransitionStateWithEvent atomically changes a backlog state and appends the
// audit event that makes the transition countable. Unlike TransitionState, an
// aggregate already at the destination is a stale write rather than an
// idempotent success: exactly one competing caller may claim the transition
// and record its event. If the event insert or transaction commit fails, the
// state change is rolled back with it.
func (d *BacklogDAO) TransitionStateWithEvent(
	ctx context.Context,
	id string,
	expectedClaimVersion int64,
	from BacklogState,
	to BacklogState,
	event *Event,
) (*BacklogItem, error) {
	if id == "" {
		return nil, errors.New("backlog transition with event: id required")
	}
	if expectedClaimVersion < 0 {
		return nil, errors.New("backlog transition with event: expected claim version must be >= 0")
	}
	if from == "" || to == "" {
		return nil, errors.New("backlog transition with event: from and to states required")
	}
	if event == nil || event.Actor == "" || event.Kind == "" {
		return nil, errors.New("backlog transition with event: event Actor + Kind required")
	}
	payload, err := jsonField(event.Payload)
	if err != nil {
		return nil, fmt.Errorf("backlog transition with event payload: %w", err)
	}
	now := time.Now().UTC()
	if event.OccurredAt.IsZero() {
		event.OccurredAt = now
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("backlog transition with event %s begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	item, err := scanBacklog(tx.QueryRowContext(ctx, `
		UPDATE backlog_items
		SET state = ?, row_version = row_version + 1, updated_at = ?
		WHERE id = ? AND claim_version = ? AND state = ?
			AND row_version < ?
		RETURNING `+backlogColumns+`
	`, string(to), timeRFC3339(now), id, expectedClaimVersion, string(from), int64(math.MaxInt64)))
	if errors.Is(err, ErrNotFound) {
		current, currentErr := scanBacklog(tx.QueryRowContext(ctx,
			`SELECT `+backlogColumns+` FROM backlog_items WHERE id = ?`, id))
		if currentErr != nil {
			return nil, currentErr
		}
		return nil, &StaleWriteError{
			Entity:           "backlog aggregate",
			ID:               id,
			ExpectedRevision: expectedClaimVersion,
			Reason: fmt.Sprintf("state=%s claim_version=%d; expected state=%s claim_version=%d",
				current.State, current.ClaimVersion, from, expectedClaimVersion),
		}
	}
	if err != nil {
		return nil, fmt.Errorf("backlog transition with event %s: %w", id, err)
	}
	// Entering escalated starts a fresh observation series; leaving escalated
	// makes any outstanding throttle state dead. Keep this reset in the same
	// transaction as the lifecycle transition so cancellation cannot leave the
	// two durable states disagreeing.
	if from == BacklogEscalated || to == BacklogEscalated {
		if _, err := tx.ExecContext(ctx, `DELETE FROM escalation_sweep_state WHERE backlog_id = ?`, id); err != nil {
			return nil, fmt.Errorf("backlog transition with event %s reset escalation sweep: %w", id, err)
		}
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO events (occurred_at, actor, kind, subject_kind, subject_id, payload_json)
		VALUES (?,?,?,?,?,?)
	`, timeRFC3339(event.OccurredAt), event.Actor, event.Kind,
		nullStr(event.SubjectKind), nullStr(event.SubjectID), payload)
	if err != nil {
		return nil, fmt.Errorf("backlog transition with event %s append: %w", id, err)
	}
	eventID, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("backlog transition with event %s commit: %w", id, err)
	}
	event.ID = eventID
	return item, nil
}

type transitionStateWithEventOnceFaultPoint string

const (
	transitionEventOnceAfterBacklog transitionStateWithEventOnceFaultPoint = "after_backlog"
	transitionEventOnceAfterEvent   transitionStateWithEventOnceFaultPoint = "after_event"
)

type transitionStateWithEventOnceFaultHook func(transitionStateWithEventOnceFaultPoint) error

// TransitionStateWithEventOnce atomically changes a backlog state and records
// the first event of its kind for the subject. A row already at the destination
// with the expected claim version is an idempotent success: a missing event is
// repaired, while an existing event returns inserted=false. The state change
// and event insert always commit or roll back together.
func (d *BacklogDAO) TransitionStateWithEventOnce(
	ctx context.Context,
	id string,
	expectedClaimVersion int64,
	from BacklogState,
	to BacklogState,
	event *Event,
) (*BacklogItem, bool, error) {
	return d.transitionStateWithEventOnce(ctx, id, expectedClaimVersion, from, to, event, nil)
}

func (d *BacklogDAO) transitionStateWithEventOnce(
	ctx context.Context,
	id string,
	expectedClaimVersion int64,
	from BacklogState,
	to BacklogState,
	event *Event,
	faultHook transitionStateWithEventOnceFaultHook,
) (*BacklogItem, bool, error) {
	if id == "" {
		return nil, false, errors.New("backlog transition with event once: id required")
	}
	if expectedClaimVersion < 0 {
		return nil, false, errors.New("backlog transition with event once: expected claim version must be >= 0")
	}
	if from == "" || to == "" {
		return nil, false, errors.New("backlog transition with event once: from and to states required")
	}
	if event == nil || event.Actor == "" || event.Kind == "" || event.SubjectKind == "" || event.SubjectID == "" {
		return nil, false, errors.New("backlog transition with event once: event Actor + Kind + SubjectKind + SubjectID required")
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("backlog transition with event once %s begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	item, err := scanBacklog(tx.QueryRowContext(ctx, `
		UPDATE backlog_items
		SET state = ?, row_version = row_version + 1, updated_at = ?
		WHERE id = ? AND claim_version = ? AND state = ? AND state <> ?
			AND row_version < ?
		RETURNING `+backlogColumns+`
	`, string(to), timeRFC3339(now), id, expectedClaimVersion, string(from), string(to), int64(math.MaxInt64)))
	if errors.Is(err, ErrNotFound) {
		current, currentErr := scanBacklog(tx.QueryRowContext(ctx,
			`SELECT `+backlogColumns+` FROM backlog_items WHERE id = ?`, id))
		if currentErr != nil {
			return nil, false, currentErr
		}
		if current.ClaimVersion != expectedClaimVersion || current.State != to {
			return nil, false, &StaleWriteError{
				Entity:           "backlog aggregate",
				ID:               id,
				ExpectedRevision: expectedClaimVersion,
				Reason: fmt.Sprintf("state=%s claim_version=%d; expected state=%s claim_version=%d",
					current.State, current.ClaimVersion, from, expectedClaimVersion),
			}
		}
		item = current
	} else if err != nil {
		return nil, false, fmt.Errorf("backlog transition with event once %s: %w", id, err)
	}
	if err := runTransitionStateWithEventOnceFault(faultHook, transitionEventOnceAfterBacklog); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	inserted, eventID, err := appendEventOnceBySubjectKind(ctx, tx, event)
	if err != nil {
		return nil, false, fmt.Errorf("backlog transition with event once %s append: %w", id, err)
	}
	if err := runTransitionStateWithEventOnceFault(faultHook, transitionEventOnceAfterEvent); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("backlog transition with event once %s commit: %w", id, err)
	}
	if inserted {
		event.ID = eventID
	}
	return item, inserted, nil
}

func runTransitionStateWithEventOnceFault(
	hook transitionStateWithEventOnceFaultHook,
	point transitionStateWithEventOnceFaultPoint,
) error {
	if hook == nil {
		return nil
	}
	if err := hook(point); err != nil {
		return fmt.Errorf("backlog transition with event once: injected fault %s: %w", point, err)
	}
	return nil
}

// Get returns the backlog item with the given id, or ErrNotFound.
func (d *BacklogDAO) Get(ctx context.Context, id string) (*BacklogItem, error) {
	row := d.db.QueryRowContext(ctx, `SELECT `+backlogColumns+` FROM backlog_items WHERE id = ?`, id)
	return scanBacklog(row)
}

// List returns every backlog item, newest-first by updated_at.
func (d *BacklogDAO) List(ctx context.Context) ([]*BacklogItem, error) {
	return d.queryMany(ctx, `SELECT `+backlogColumns+` FROM backlog_items ORDER BY updated_at DESC`)
}

// ListByState returns backlog items with the given state.
func (d *BacklogDAO) ListByState(ctx context.Context, state BacklogState) ([]*BacklogItem, error) {
	return d.queryMany(ctx,
		`SELECT `+backlogColumns+` FROM backlog_items WHERE state = ? ORDER BY priority ASC, created_at ASC, id ASC`,
		string(state),
	)
}

// ListByStateLimit returns an ordered, bounded state slice for recurring repair
// loops. A non-positive limit falls back to ListByState.
func (d *BacklogDAO) ListByStateLimit(ctx context.Context, state BacklogState, limit int) ([]*BacklogItem, error) {
	if limit <= 0 {
		return d.ListByState(ctx, state)
	}
	return d.queryMany(ctx,
		`SELECT `+backlogColumns+` FROM backlog_items WHERE state = ? ORDER BY priority ASC, created_at ASC, id ASC LIMIT ?`,
		string(state), limit,
	)
}

// ListTerminalRepairCandidates returns running backlog rows that have at least
// one pipeline run and no non-terminal run. Filtering in SQL prevents an active
// FIFO head from consuming every bounded repair slot and starving terminal
// rows later in the backlog ordering.
func (d *BacklogDAO) ListTerminalRepairCandidates(ctx context.Context, limit int) ([]*BacklogItem, error) {
	if limit <= 0 {
		limit = 100
	}
	return d.queryMany(ctx, `
		SELECT `+backlogColumns+`
		FROM backlog_items bi
		WHERE bi.state = ?
		  AND EXISTS (
			SELECT 1 FROM pipeline_runs pr
			WHERE pr.backlog_id = bi.id
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM pipeline_runs pr
			WHERE pr.backlog_id = bi.id
			  AND pr.state NOT IN ('done', 'escalated', 'paused')
		  )
		ORDER BY bi.priority ASC, bi.created_at ASC, bi.id ASC
		LIMIT ?
	`, string(BacklogRunning), limit)
}

// ListEscalatedWithMR returns escalated backlog items whose most-recent pipeline
// run carries both a non-zero mr_iid and consistent durable project provenance,
// oldest-first by updated_at. It powers the reconciler's ghost-spark reap sweep:
// an item that escalated at the merge stage whose MR later merged out-of-band
// via merge-when-pipeline-succeeds. Filtering both predicates in SQL excludes
// legacy/unroutable escalations before LIMIT so they cannot starve candidates
// that the caller can actually resolve.
//
// Project routing is deliberately not filtered through mutable target_project.
// The sweep resolves each candidate from successful stage provenance before
// issuing a per-project lookup.
//
// Oldest updated_at first drains the longest-stuck escalations first. A
// non-positive limit falls back to 128.
func (d *BacklogDAO) ListEscalatedWithMR(ctx context.Context, limit int) ([]*BacklogItem, error) {
	if limit <= 0 {
		limit = 128
	}
	return d.queryMany(ctx, `
		SELECT `+backlogColumns+`
		FROM backlog_items bi
		WHERE bi.state = ?
		  AND EXISTS (
			SELECT 1 FROM pipeline_runs pr
			WHERE pr.id = (
				SELECT latest.id
				FROM pipeline_runs latest
				WHERE latest.backlog_id = bi.id
				ORDER BY latest.started_at DESC, latest.attempts DESC
				LIMIT 1
			)
			  AND pr.mr_iid > 0
			  AND NOT EXISTS (
				SELECT 1
				FROM stage_results malformed
				WHERE malformed.pipeline_run_id = pr.id
				  AND (
					json_valid(malformed.artifacts_json) = 0
					OR json_type(CASE
						WHEN json_valid(malformed.artifacts_json)
							THEN malformed.artifacts_json
						ELSE '{}'
					END) <> 'object'
				  )
			  )
			  AND EXISTS (
				SELECT 1
				FROM stage_results sr
				JOIN json_each(CASE
					WHEN json_valid(sr.artifacts_json) THEN sr.artifacts_json
					ELSE '{}'
				END) project
				WHERE sr.pipeline_run_id = pr.id
				  AND sr.outcome = ?
				  AND (
					(sr.stage = 'mr' AND project.key = 'mr_project')
					OR (sr.stage = 'ci_watch' AND project.key = 'ci_project')
					OR (sr.stage = 'merge' AND project.key = 'merged_project')
					OR (sr.stage = 'cleanup' AND project.key = 'cleanup_project')
				  )
				GROUP BY sr.pipeline_run_id
				HAVING SUM(CASE
					WHEN project.type = 'text' AND trim(project.value) <> '' THEN 0
					ELSE 1
				END) = 0
				  AND COUNT(DISTINCT trim(project.value)) = 1
			  )
		  )
		ORDER BY bi.updated_at ASC, bi.id ASC
		LIMIT ?
	`, string(BacklogEscalated), string(StageOutcomeSuccess), limit)
}

// ListEscalatedWithoutMR is the complement of ListEscalatedWithMR: escalated
// items whose most-recent run never recorded an MR IID because it escalated
// before the mr stage (a scope or docs gate, a failed preflight). Those items
// are invisible to the MR-IID-driven sweep, yet their branch is frequently
// pushed and merged by hand afterwards — leaving the item escalated forever
// with its work already on main.
//
// Deliberately NOT filtered on stage provenance the way ListEscalatedWithMR is:
// a run that never reached the mr stage has no mr_project/ci_project artifact
// to resolve, so requiring one would exclude exactly the population this query
// exists to find. The caller compensates by restricting itself to the home
// project and matching the discovered MR's source branch against the item's
// deterministic branch contract.
//
// Items with no runs at all are excluded — there is no attempt whose work could
// have merged. Oldest updated_at first drains the longest-stuck escalations
// first. A non-positive limit falls back to 128.
func (d *BacklogDAO) ListEscalatedWithoutMR(ctx context.Context, limit int) ([]*BacklogItem, error) {
	if limit <= 0 {
		limit = 128
	}
	return d.queryMany(ctx, `
		SELECT `+backlogColumns+`
		FROM backlog_items bi
		WHERE bi.state = ?
		  AND EXISTS (
			SELECT 1 FROM pipeline_runs pr
			WHERE pr.id = (
				SELECT latest.id
				FROM pipeline_runs latest
				WHERE latest.backlog_id = bi.id
				ORDER BY latest.started_at DESC, latest.attempts DESC
				LIMIT 1
			)
			  AND (pr.mr_iid IS NULL OR pr.mr_iid = 0)
		  )
		ORDER BY bi.updated_at ASC, bi.id ASC
		LIMIT ?
	`, string(BacklogEscalated), limit)
}

// Delete removes a backlog item by id. Returns ErrNotFound if it didn't exist.
func (d *BacklogDAO) Delete(ctx context.Context, id string) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM backlog_items WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("backlog delete %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *BacklogDAO) queryMany(ctx context.Context, q string, args ...any) ([]*BacklogItem, error) {
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("backlog query: %w", err)
	}
	defer rows.Close()
	var out []*BacklogItem
	for rows.Next() {
		item, err := scanBacklog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// scanner abstracts *sql.Row and *sql.Rows for shared scan logic.
type scanner interface {
	Scan(dest ...any) error
}

func scanBacklog(s scanner) (*BacklogItem, error) {
	var (
		item             BacklogItem
		iid              sql.NullInt64
		specDoc          sql.NullString
		specAnchor       sql.NullString
		council          sql.NullString
		labels, success  string
		budget, policy   string
		slicesJSON, deps string
		createdAt        string
		updatedAt        string
		planID           sql.NullString
		targetProject    sql.NullString
		grade            sql.NullString
		gradeNote        sql.NullString
		gradeActor       sql.NullString
		gradedAt         sql.NullString
	)
	err := s.Scan(
		&item.ID, &iid, &item.Title, &labels, &item.State, &item.Priority,
		&specDoc, &specAnchor, &success, &budget, &policy, &slicesJSON,
		&deps, &council, &item.CreatedBy, &createdAt, &updatedAt, &planID,
		&targetProject, &item.ClaimVersion, &item.Revision, &grade, &gradeNote, &gradeActor, &gradedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("backlog scan: %w", err)
	}
	item.GitLabIssueIID = nullableInt64(iid)
	item.CouncilRunID = nullableString(council)
	if planID.Valid {
		item.PlanID = planID.String
	}
	if targetProject.Valid {
		item.TargetProject = targetProject.String
	}
	item.Grade = grade.String
	item.GradeNote = gradeNote.String
	item.GradeActor = gradeActor.String
	if gradedAt.Valid {
		parsed, parseErr := parseTime(gradedAt.String)
		if parseErr != nil {
			return nil, fmt.Errorf("graded_at: %w", parseErr)
		}
		item.GradedAt = &parsed
	}
	if specDoc.Valid {
		item.SpecDoc = specDoc.String
	}
	if specAnchor.Valid {
		item.SpecAnchor = specAnchor.String
	}
	if err := jsonInto(labels, &item.Labels); err != nil {
		return nil, fmt.Errorf("labels: %w", err)
	}
	if err := jsonInto(success, &item.Success); err != nil {
		return nil, fmt.Errorf("success: %w", err)
	}
	if err := jsonInto(budget, &item.Budget); err != nil {
		return nil, fmt.Errorf("budget: %w", err)
	}
	if err := jsonInto(policy, &item.Policy); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	if err := jsonInto(slicesJSON, &item.Slices); err != nil {
		return nil, fmt.Errorf("slices: %w", err)
	}
	if err := jsonInto(deps, &item.Dependencies); err != nil {
		return nil, fmt.Errorf("dependencies: %w", err)
	}
	if item.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("created_at: %w", err)
	}
	if item.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("updated_at: %w", err)
	}
	return &item, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
