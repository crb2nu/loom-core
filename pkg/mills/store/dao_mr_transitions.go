package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MRHeadTransitionTrigger records why a head-movement row exists.
type MRHeadTransitionTrigger string

const (
	// MRHeadTriggerRebaseRequest marks a movement Mills asked GitLab for.
	// Requesting one is cheap; the resulting head is never trusted.
	MRHeadTriggerRebaseRequest MRHeadTransitionTrigger = "rebase_request"
	// MRHeadTriggerExternal marks a movement Mills only observed after the
	// fact (a human push, another bot, a UI rebase). It is by construction
	// unattributable, so it can never settle 'attributed'.
	MRHeadTriggerExternal MRHeadTransitionTrigger = "external"
)

// MRHeadTransitionState is the ledger row's position in the §5.2 state
// machine. noop / attributed / ambiguous / failed are terminal (settled_at set).
type MRHeadTransitionState string

const (
	MRHeadTransitionRequested  MRHeadTransitionState = "requested"
	MRHeadTransitionInProgress MRHeadTransitionState = "in_progress"
	MRHeadTransitionObserved   MRHeadTransitionState = "observed"
	MRHeadTransitionAttributed MRHeadTransitionState = "attributed"
	MRHeadTransitionAmbiguous  MRHeadTransitionState = "ambiguous"
	MRHeadTransitionFailed     MRHeadTransitionState = "failed"
	MRHeadTransitionNoop       MRHeadTransitionState = "noop"
)

// IsSettled reports whether s is one of the four terminal verdicts.
func (s MRHeadTransitionState) IsSettled() bool {
	switch s {
	case MRHeadTransitionAttributed, MRHeadTransitionAmbiguous,
		MRHeadTransitionFailed, MRHeadTransitionNoop:
		return true
	default:
		return false
	}
}

// IsMovement reports whether a settled state represents a head that actually
// moved. 'noop' is settled but did NOT move (decided by SHA equality on the MR
// itself), so it neither invalidates a CI authorization nor spends the
// per-run transition budget.
func (s MRHeadTransitionState) IsMovement() bool {
	return s.IsSettled() && s != MRHeadTransitionNoop
}

// MRHeadTransition is one durable row of the mr_head_transitions ledger
// (migration 016): a single movement of an MR source-branch head observed
// while a pipeline run held, or was about to use, a CI authorization.
//
// GitLab's rebase endpoint accepts no source-SHA precondition, so provenance
// can never prove that a head movement was solely the replay Mills asked for
// (#374). The ledger consequently exists to make every movement *durable and
// auditable*, not trusted: a settled movement invalidates the CI authorization
// stamped for ReviewedSHA and forces SuccessorSHA through a full re-gate.
type MRHeadTransition struct {
	ID            int64                   `json:"id"`
	PipelineRunID string                  `json:"pipeline_run_id"`
	Seq           int64                   `json:"seq"`
	Project       string                  `json:"project"`
	MRIID         int64                   `json:"mr_iid"`
	SourceBranch  string                  `json:"source_branch"`
	TargetBranch  string                  `json:"target_branch"`
	ReviewedSHA   string                  `json:"reviewed_sha"`
	TargetHeadSHA string                  `json:"target_head_sha"`
	SuccessorSHA  string                  `json:"successor_sha,omitempty"`
	Trigger       MRHeadTransitionTrigger `json:"trigger"`
	State         MRHeadTransitionState   `json:"state"`
	// Provenance is the evidence bundle (cursors, version rows, push events,
	// poll telemetry, classifier verdict + reason) so an escalation is
	// diagnosable without re-querying GitLab.
	Provenance  map[string]any `json:"provenance"`
	RequestedAt time.Time      `json:"requested_at"`
	ObservedAt  *time.Time     `json:"observed_at,omitempty"`
	SettledAt   *time.Time     `json:"settled_at,omitempty"`
}

// MRHeadTransitionDAO exposes the mr_head_transitions ledger.
//
// Append-then-settle by design: Open mints an unsettled row with the next
// per-run seq, Settle terminalizes exactly that row once. Neither operation
// ever rewrites history — a second movement is a second row.
type MRHeadTransitionDAO struct {
	db *sql.DB
}

var (
	// ErrHeadTransitionOpen is returned by Open when the run already has an
	// unsettled row. The rebase PUT is a non-idempotent mutation, so an
	// operator that died mid-flight must RE-OBSERVE the existing row rather
	// than mint (and mutate) a second one.
	ErrHeadTransitionOpen = errors.New("mills store: run already has an unsettled mr head transition")
	// ErrHeadTransitionSettled is returned by Settle when the targeted row is
	// already terminal. Settling is compare-and-swap on settled_at IS NULL so
	// two racing observers cannot record conflicting verdicts.
	ErrHeadTransitionSettled = errors.New("mills store: mr head transition already settled")
)

// Open mints the next transition row for a run and returns it with Seq/ID
// populated. seq is allocated as MAX(seq)+1 inside the same transaction as the
// insert, so concurrent opens serialise on the UNIQUE(pipeline_run_id, seq)
// constraint rather than racing to a duplicate.
//
// state must be a non-terminal state ('requested', 'in_progress') for a row
// that will be settled later, or a terminal state when the caller already
// knows the verdict (the external-movement path, which observes a movement
// that has already happened and therefore opens and settles in one write).
func (d *MRHeadTransitionDAO) Open(ctx context.Context, t *MRHeadTransition) (*MRHeadTransition, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("mr head transition: dao not configured")
	}
	if t == nil {
		return nil, errors.New("mr head transition: row required")
	}
	if t.PipelineRunID == "" {
		return nil, errors.New("mr head transition: PipelineRunID required")
	}
	if t.ReviewedSHA == "" {
		return nil, errors.New("mr head transition: ReviewedSHA required")
	}
	switch t.Trigger {
	case MRHeadTriggerRebaseRequest, MRHeadTriggerExternal:
	default:
		return nil, fmt.Errorf("mr head transition: unsupported trigger %q", t.Trigger)
	}
	if t.State == "" {
		return nil, errors.New("mr head transition: State required")
	}
	if t.Trigger == MRHeadTriggerExternal && t.State == MRHeadTransitionAttributed {
		// An unrequested movement is not attributable to anything Mills did.
		return nil, errors.New("mr head transition: external trigger cannot settle attributed")
	}
	row := *t
	if row.RequestedAt.IsZero() {
		row.RequestedAt = time.Now().UTC()
	}
	if row.State.IsSettled() && row.SettledAt == nil {
		settled := row.RequestedAt
		row.SettledAt = &settled
	}
	provenance, err := jsonField(row.Provenance)
	if err != nil {
		return nil, fmt.Errorf("mr head transition: provenance: %w", err)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mr head transition: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var open int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mr_head_transitions
		WHERE pipeline_run_id = ? AND settled_at IS NULL`, row.PipelineRunID).Scan(&open); err != nil {
		return nil, fmt.Errorf("mr head transition: count open %s: %w", row.PipelineRunID, err)
	}
	if open > 0 {
		return nil, ErrHeadTransitionOpen
	}

	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(seq) FROM mr_head_transitions WHERE pipeline_run_id = ?`,
		row.PipelineRunID).Scan(&maxSeq); err != nil {
		return nil, fmt.Errorf("mr head transition: max seq %s: %w", row.PipelineRunID, err)
	}
	row.Seq = maxSeq.Int64 + 1

	res, err := tx.ExecContext(ctx, `
		INSERT INTO mr_head_transitions (
			pipeline_run_id, seq, project, mr_iid, source_branch, target_branch,
			reviewed_sha, target_head_sha, successor_sha, trigger, state,
			provenance_json, requested_at, observed_at, settled_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.PipelineRunID, row.Seq, row.Project, row.MRIID, row.SourceBranch, row.TargetBranch,
		row.ReviewedSHA, row.TargetHeadSHA, nullStr(row.SuccessorSHA), string(row.Trigger), string(row.State),
		provenance, timeRFC3339(row.RequestedAt), nullTime(row.ObservedAt), nullTime(row.SettledAt))
	if err != nil {
		return nil, fmt.Errorf("mr head transition: insert %s/%d: %w", row.PipelineRunID, row.Seq, err)
	}
	if id, err := res.LastInsertId(); err == nil {
		row.ID = id
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("mr head transition: commit %s/%d: %w", row.PipelineRunID, row.Seq, err)
	}
	return &row, nil
}

// SettleRequest terminalizes one open row.
type SettleRequest struct {
	PipelineRunID string
	Seq           int64
	State         MRHeadTransitionState
	SuccessorSHA  string
	Provenance    map[string]any
	ObservedAt    *time.Time
	SettledAt     time.Time
}

// Settle compare-and-swaps an open row to a terminal verdict. The UPDATE
// predicate includes settled_at IS NULL, so a second observer (a racing
// goroutine, or a restarted operator re-observing the same movement) gets
// ErrHeadTransitionSettled instead of overwriting the recorded verdict.
func (d *MRHeadTransitionDAO) Settle(ctx context.Context, req SettleRequest) (*MRHeadTransition, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("mr head transition: dao not configured")
	}
	if req.PipelineRunID == "" || req.Seq <= 0 {
		return nil, errors.New("mr head transition: PipelineRunID and positive Seq required")
	}
	if !req.State.IsSettled() {
		return nil, fmt.Errorf("mr head transition: %q is not a terminal state", req.State)
	}
	provenance, err := jsonField(req.Provenance)
	if err != nil {
		return nil, fmt.Errorf("mr head transition: provenance: %w", err)
	}
	settledAt := req.SettledAt
	if settledAt.IsZero() {
		settledAt = time.Now().UTC()
	}
	observedAt := req.ObservedAt
	if observedAt == nil && req.State != MRHeadTransitionFailed {
		observedAt = &settledAt
	}
	res, err := d.db.ExecContext(ctx, `
		UPDATE mr_head_transitions
		SET state = ?, successor_sha = ?, provenance_json = ?, observed_at = ?, settled_at = ?
		WHERE pipeline_run_id = ? AND seq = ? AND settled_at IS NULL`,
		string(req.State), nullStr(req.SuccessorSHA), provenance,
		nullTime(observedAt), timeRFC3339(settledAt),
		req.PipelineRunID, req.Seq)
	if err != nil {
		return nil, fmt.Errorf("mr head transition: settle %s/%d: %w", req.PipelineRunID, req.Seq, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("mr head transition: settle rows %s/%d: %w", req.PipelineRunID, req.Seq, err)
	}
	if n == 0 {
		// Distinguish "never existed" from "already settled" so a restart-safe
		// re-observation can tell a lost row from a completed one.
		got, gerr := d.Get(ctx, req.PipelineRunID, req.Seq)
		if gerr != nil {
			return nil, gerr
		}
		return got, ErrHeadTransitionSettled
	}
	return d.Get(ctx, req.PipelineRunID, req.Seq)
}

// Get returns one row by (run, seq). ErrNotFound when absent.
func (d *MRHeadTransitionDAO) Get(ctx context.Context, runID string, seq int64) (*MRHeadTransition, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("mr head transition: dao not configured")
	}
	row := d.db.QueryRowContext(ctx, mrHeadTransitionSelect+`
		WHERE pipeline_run_id = ? AND seq = ?`, runID, seq)
	return scanMRHeadTransition(row)
}

// ListByRun returns the run's ledger newest-first (the HUD/diagnostics order).
// Never returns a nil slice for an existing run with rows.
func (d *MRHeadTransitionDAO) ListByRun(ctx context.Context, runID string) ([]*MRHeadTransition, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("mr head transition: dao not configured")
	}
	rows, err := d.db.QueryContext(ctx, mrHeadTransitionSelect+`
		WHERE pipeline_run_id = ? ORDER BY seq DESC`, runID)
	if err != nil {
		return nil, fmt.Errorf("mr head transition: list %s: %w", runID, err)
	}
	defer rows.Close()
	var out []*MRHeadTransition
	for rows.Next() {
		t, err := scanMRHeadTransition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// OpenTransition returns the run's single unsettled row, or nil. This is the
// restart-safety read: an operator that died between the rebase PUT and the
// observation finds the row here and re-observes instead of re-mutating.
func (d *MRHeadTransitionDAO) OpenTransition(ctx context.Context, runID string) (*MRHeadTransition, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("mr head transition: dao not configured")
	}
	row := d.db.QueryRowContext(ctx, mrHeadTransitionSelect+`
		WHERE pipeline_run_id = ? AND settled_at IS NULL ORDER BY seq ASC LIMIT 1`, runID)
	t, err := scanMRHeadTransition(row)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return t, err
}

// MaxSettledSeq is the CI re-authorization fence value: the highest seq among
// settled rows that actually MOVED the head. 0 when the run has never moved.
//
// 'noop' rows are excluded on purpose. A noop is decided by SHA equality on
// the MR itself — the head did not move, so the CI verdict for reviewed_sha is
// still a verdict for the live head and must keep authorizing the merge.
// Counting noops here would fail-close a merge that nothing invalidated.
func (d *MRHeadTransitionDAO) MaxSettledSeq(ctx context.Context, runID string) (int64, error) {
	if d == nil || d.db == nil {
		return 0, errors.New("mr head transition: dao not configured")
	}
	var maxSeq sql.NullInt64
	if err := d.db.QueryRowContext(ctx, `
		SELECT MAX(seq) FROM mr_head_transitions
		WHERE pipeline_run_id = ? AND settled_at IS NOT NULL AND state <> 'noop'`,
		runID).Scan(&maxSeq); err != nil {
		return 0, fmt.Errorf("mr head transition: max settled seq %s: %w", runID, err)
	}
	return maxSeq.Int64, nil
}

// CountSettled counts settled head MOVEMENTS for a run — the budget counter
// behind LOOM_MILLS_MERGE_MAX_HEAD_TRANSITIONS. Same noop exclusion as
// MaxSettledSeq: a movement that did not move must not spend the budget that
// bounds rebase↔push ping-pong.
func (d *MRHeadTransitionDAO) CountSettled(ctx context.Context, runID string) (int, error) {
	if d == nil || d.db == nil {
		return 0, errors.New("mr head transition: dao not configured")
	}
	var n int
	if err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mr_head_transitions
		WHERE pipeline_run_id = ? AND settled_at IS NOT NULL AND state <> 'noop'`,
		runID).Scan(&n); err != nil {
		return 0, fmt.Errorf("mr head transition: count settled %s: %w", runID, err)
	}
	return n, nil
}

const mrHeadTransitionSelect = `
	SELECT id, pipeline_run_id, seq, project, mr_iid, source_branch, target_branch,
	       reviewed_sha, target_head_sha, successor_sha, trigger, state,
	       provenance_json, requested_at, observed_at, settled_at
	FROM mr_head_transitions
`

func scanMRHeadTransition(s scanner) (*MRHeadTransition, error) {
	var (
		t           MRHeadTransition
		successor   sql.NullString
		trigger     string
		state       string
		provenance  string
		requestedAt string
		observedAt  sql.NullString
		settledAt   sql.NullString
	)
	err := s.Scan(&t.ID, &t.PipelineRunID, &t.Seq, &t.Project, &t.MRIID,
		&t.SourceBranch, &t.TargetBranch, &t.ReviewedSHA, &t.TargetHeadSHA,
		&successor, &trigger, &state, &provenance, &requestedAt, &observedAt, &settledAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("mr head transition scan: %w", err)
	}
	if successor.Valid {
		t.SuccessorSHA = successor.String
	}
	t.Trigger = MRHeadTransitionTrigger(trigger)
	t.State = MRHeadTransitionState(state)
	t.Provenance = map[string]any{}
	if err := jsonInto(provenance, &t.Provenance); err != nil {
		return nil, fmt.Errorf("mr head transition provenance: %w", err)
	}
	if t.RequestedAt, err = parseTime(requestedAt); err != nil {
		return nil, fmt.Errorf("requested_at: %w", err)
	}
	if t.ObservedAt, err = nullableTime(observedAt); err != nil {
		return nil, fmt.Errorf("observed_at: %w", err)
	}
	if t.SettledAt, err = nullableTime(settledAt); err != nil {
		return nil, fmt.Errorf("settled_at: %w", err)
	}
	return &t, nil
}
