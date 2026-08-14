package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MergeQueueState is a merge_queue row's position in the serial-queue state
// machine. merged / evicted are terminal (settled_at set).
type MergeQueueState string

const (
	// MergeQueueQueued means the entry is waiting for its lane's head slot (or
	// holds it and has not been driven yet).
	MergeQueueQueued MergeQueueState = "queued"
	// MergeQueueRebasing means the queue requested a rebase and is waiting for
	// GitLab's async rebase to settle.
	MergeQueueRebasing MergeQueueState = "rebasing"
	// MergeQueueAwaitingPipeline means the head moved (rebase) and the queue is
	// waiting for a branch pipeline on the new head to reach a terminal state.
	MergeQueueAwaitingPipeline MergeQueueState = "awaiting_pipeline"
	// MergeQueueMerging means the head is proven on the current target tip and
	// the merge PUT is in flight (or due on the next drive).
	MergeQueueMerging MergeQueueState = "merging"
	// MergeQueueMerged is the terminal success state.
	MergeQueueMerged MergeQueueState = "merged"
	// MergeQueueEvicted is the terminal failure state; EvictionReason names why
	// and the owning run falls through to the normal escalation path.
	MergeQueueEvicted MergeQueueState = "evicted"
)

// IsTerminal reports whether s is a settled verdict.
func (s MergeQueueState) IsTerminal() bool {
	return s == MergeQueueMerged || s == MergeQueueEvicted
}

// Eviction reasons the processor records. Distinct reasons keep escalations
// diagnosable without re-querying GitLab.
const (
	MergeQueueEvictRebaseConflict  = "rebase_conflict"
	MergeQueueEvictRebaseAmbiguous = "rebase_ambiguous"
	MergeQueueEvictCIRed           = "ci_red"
	MergeQueueEvictCITimeout       = "ci_timeout"
	MergeQueueEvictHeadMoved       = "head_moved"
	MergeQueueEvictMRClosed        = "mr_closed"
	MergeQueueEvictMergeFailed     = "merge_failed"
)

// ErrMergeQueueFull is returned by Enqueue when the lane already holds
// max_depth active entries. The merge stage escalates immediately with
// reason queue_full instead of waiting.
var ErrMergeQueueFull = errors.New("mills store: merge queue lane is full")

// ErrMergeQueueConflict is returned by a compare-and-swap transition whose
// expected state no longer matches (a racing processor tick or a restart
// re-driving an already-advanced entry).
var ErrMergeQueueConflict = errors.New("mills store: merge queue entry state changed underneath the transition")

// MergeQueueEntry is one row of the merge_queue table.
type MergeQueueEntry struct {
	ID             int64           `json:"id"`
	PipelineRunID  string          `json:"pipeline_run_id"`
	BacklogID      string          `json:"backlog_id"`
	Project        string          `json:"project"`
	MRIID          int64           `json:"mr_iid"`
	SourceBranch   string          `json:"source_branch"`
	TargetBranch   string          `json:"target_branch"`
	EnqueuedSHA    string          `json:"enqueued_sha"`
	CurrentSHA     string          `json:"current_sha"`
	State          MergeQueueState `json:"state"`
	EvictionReason string          `json:"eviction_reason,omitempty"`
	// Detail carries drive-state the processor needs across ticks and restarts
	// (head-observation cursors, ledger seq, pipeline id, timestamps).
	Detail     map[string]any `json:"detail"`
	Attempts   int            `json:"attempts"`
	MergedSHA  string         `json:"merged_sha,omitempty"`
	EnqueuedAt time.Time      `json:"enqueued_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	SettledAt  *time.Time     `json:"settled_at,omitempty"`
}

// MergeQueueDAO exposes the merge_queue table.
type MergeQueueDAO struct {
	db *sql.DB
}

// Enqueue inserts a new queued entry, or returns the existing entry for the
// run when one exists (idempotent: stage retries and operator restarts re-find
// their entry, terminal or not, instead of double-queueing). created reports
// whether a new row was inserted.
//
// maxDepth bounds the lane's ACTIVE entries: when the (project, target_branch)
// lane already holds maxDepth unsettled rows the insert is refused with
// ErrMergeQueueFull. maxDepth <= 0 means unbounded. The count and insert share
// one transaction so two racing enqueues cannot both squeeze into the last
// slot.
func (d *MergeQueueDAO) Enqueue(ctx context.Context, e *MergeQueueEntry, maxDepth int) (entry *MergeQueueEntry, created bool, err error) {
	if d == nil || d.db == nil {
		return nil, false, errors.New("merge queue: dao not configured")
	}
	if e == nil {
		return nil, false, errors.New("merge queue: entry required")
	}
	for _, field := range []struct{ name, value string }{
		{"PipelineRunID", e.PipelineRunID},
		{"Project", e.Project},
		{"SourceBranch", e.SourceBranch},
		{"TargetBranch", e.TargetBranch},
		{"EnqueuedSHA", e.EnqueuedSHA},
	} {
		if field.value == "" {
			return nil, false, fmt.Errorf("merge queue: %s required", field.name)
		}
	}
	if e.MRIID <= 0 {
		return nil, false, errors.New("merge queue: positive MRIID required")
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("merge queue: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanMergeQueueEntry(tx.QueryRowContext(ctx, mergeQueueSelect+`
		WHERE pipeline_run_id = ?`, e.PipelineRunID))
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}

	if maxDepth > 0 {
		var depth int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM merge_queue
			WHERE project = ? AND target_branch = ? AND settled_at IS NULL`,
			e.Project, e.TargetBranch).Scan(&depth); err != nil {
			return nil, false, fmt.Errorf("merge queue: depth %s→%s: %w", e.Project, e.TargetBranch, err)
		}
		if depth >= maxDepth {
			return nil, false, fmt.Errorf("%w: %s→%s holds %d of %d", ErrMergeQueueFull, e.Project, e.TargetBranch, depth, maxDepth)
		}
	}

	row := *e
	now := time.Now().UTC()
	if row.EnqueuedAt.IsZero() {
		row.EnqueuedAt = now
	}
	row.UpdatedAt = now
	row.State = MergeQueueQueued
	if row.CurrentSHA == "" {
		row.CurrentSHA = row.EnqueuedSHA
	}
	detail, err := jsonField(row.Detail)
	if err != nil {
		return nil, false, fmt.Errorf("merge queue: detail: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO merge_queue (
			pipeline_run_id, backlog_id, project, mr_iid, source_branch, target_branch,
			enqueued_sha, current_sha, state, eviction_reason, detail_json, attempts,
			merged_sha, enqueued_at, updated_at, settled_at
		) VALUES (?,?,?,?,?,?,?,?,?,NULL,?,0,NULL,?,?,NULL)`,
		row.PipelineRunID, row.BacklogID, row.Project, row.MRIID, row.SourceBranch, row.TargetBranch,
		row.EnqueuedSHA, row.CurrentSHA, string(MergeQueueQueued), detail,
		timeRFC3339(row.EnqueuedAt), timeRFC3339(row.UpdatedAt))
	if err != nil {
		return nil, false, fmt.Errorf("merge queue: insert %s: %w", row.PipelineRunID, err)
	}
	if id, err := res.LastInsertId(); err == nil {
		row.ID = id
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("merge queue: commit %s: %w", row.PipelineRunID, err)
	}
	return &row, true, nil
}

// Get returns the entry for a pipeline run. ErrNotFound when absent.
func (d *MergeQueueDAO) Get(ctx context.Context, runID string) (*MergeQueueEntry, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("merge queue: dao not configured")
	}
	return scanMergeQueueEntry(d.db.QueryRowContext(ctx, mergeQueueSelect+`
		WHERE pipeline_run_id = ?`, runID))
}

// Heads returns the head (lowest id active) entry of every lane that has at
// least one active entry. This is the processor's per-tick work list: strictly
// one in-flight candidate per (project, target_branch).
func (d *MergeQueueDAO) Heads(ctx context.Context) ([]*MergeQueueEntry, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("merge queue: dao not configured")
	}
	rows, err := d.db.QueryContext(ctx, mergeQueueSelect+`
		WHERE settled_at IS NULL AND id IN (
			SELECT MIN(id) FROM merge_queue
			WHERE settled_at IS NULL
			GROUP BY project, target_branch
		)
		ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("merge queue: heads: %w", err)
	}
	defer rows.Close()
	return collectMergeQueueEntries(rows)
}

// ListActive returns every unsettled entry in FIFO order (telemetry + HUD).
func (d *MergeQueueDAO) ListActive(ctx context.Context) ([]*MergeQueueEntry, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("merge queue: dao not configured")
	}
	rows, err := d.db.QueryContext(ctx, mergeQueueSelect+`
		WHERE settled_at IS NULL ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("merge queue: list active: %w", err)
	}
	defer rows.Close()
	return collectMergeQueueEntries(rows)
}

// Position returns the 1-based position of a run's entry among its lane's
// active entries (1 = head). Terminal entries and unknown runs return 0.
func (d *MergeQueueDAO) Position(ctx context.Context, runID string) (int, error) {
	if d == nil || d.db == nil {
		return 0, errors.New("merge queue: dao not configured")
	}
	var pos sql.NullInt64
	err := d.db.QueryRowContext(ctx, `
		SELECT (
			SELECT COUNT(*) FROM merge_queue peer
			WHERE peer.project = me.project
			  AND peer.target_branch = me.target_branch
			  AND peer.settled_at IS NULL
			  AND peer.id <= me.id
		)
		FROM merge_queue me
		WHERE me.pipeline_run_id = ? AND me.settled_at IS NULL`, runID).Scan(&pos)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("merge queue: position %s: %w", runID, err)
	}
	return int(pos.Int64), nil
}

// MergeQueueTransition is one compare-and-swap advance of an active entry.
type MergeQueueTransition struct {
	ID   int64
	From MergeQueueState
	To   MergeQueueState
	// CurrentSHA, when non-empty, replaces the entry's live head (rebase
	// observed a successor).
	CurrentSHA string
	// Detail, when non-nil, replaces the entry's drive-state bundle.
	Detail map[string]any
	// BumpAttempts increments the head-drive attempt counter.
	BumpAttempts bool
}

// Transition compare-and-swaps an ACTIVE entry from one non-terminal state to
// another. ErrMergeQueueConflict when the row is not in the expected state.
func (d *MergeQueueDAO) Transition(ctx context.Context, t MergeQueueTransition) (*MergeQueueEntry, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("merge queue: dao not configured")
	}
	if t.ID <= 0 {
		return nil, errors.New("merge queue: positive ID required")
	}
	if t.From.IsTerminal() || t.To.IsTerminal() {
		return nil, errors.New("merge queue: Transition is for active states; use MarkMerged/MarkEvicted")
	}
	set := "state = ?, updated_at = ?"
	args := []any{string(t.To), timeRFC3339(time.Now().UTC())}
	if t.CurrentSHA != "" {
		set += ", current_sha = ?"
		args = append(args, t.CurrentSHA)
	}
	if t.Detail != nil {
		detail, err := jsonField(t.Detail)
		if err != nil {
			return nil, fmt.Errorf("merge queue: detail: %w", err)
		}
		set += ", detail_json = ?"
		args = append(args, detail)
	}
	if t.BumpAttempts {
		set += ", attempts = attempts + 1"
	}
	args = append(args, t.ID, string(t.From))
	res, err := d.db.ExecContext(ctx, `
		UPDATE merge_queue SET `+set+`
		WHERE id = ? AND state = ? AND settled_at IS NULL`, args...)
	if err != nil {
		return nil, fmt.Errorf("merge queue: transition %d %s→%s: %w", t.ID, t.From, t.To, err)
	}
	return d.afterCAS(ctx, res, t.ID)
}

// MarkMerged terminalizes an active entry as merged. CAS on the expected
// active state so a racing tick cannot double-settle.
func (d *MergeQueueDAO) MarkMerged(ctx context.Context, id int64, from MergeQueueState, mergedSHA string) (*MergeQueueEntry, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("merge queue: dao not configured")
	}
	now := timeRFC3339(time.Now().UTC())
	res, err := d.db.ExecContext(ctx, `
		UPDATE merge_queue
		SET state = ?, merged_sha = ?, updated_at = ?, settled_at = ?
		WHERE id = ? AND state = ? AND settled_at IS NULL`,
		string(MergeQueueMerged), nullStr(mergedSHA), now, now, id, string(from))
	if err != nil {
		return nil, fmt.Errorf("merge queue: mark merged %d: %w", id, err)
	}
	return d.afterCAS(ctx, res, id)
}

// MarkEvicted terminalizes an active entry as evicted with a reason. Unlike
// the other transitions this succeeds from ANY active state: eviction is the
// processor's fail-closed exit and must never itself wedge on a state race.
func (d *MergeQueueDAO) MarkEvicted(ctx context.Context, id int64, reason string, detail map[string]any) (*MergeQueueEntry, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("merge queue: dao not configured")
	}
	if reason == "" {
		return nil, errors.New("merge queue: eviction reason required")
	}
	set := "state = ?, eviction_reason = ?, updated_at = ?, settled_at = ?"
	now := timeRFC3339(time.Now().UTC())
	args := []any{string(MergeQueueEvicted), reason, now, now}
	if detail != nil {
		d2, err := jsonField(detail)
		if err != nil {
			return nil, fmt.Errorf("merge queue: detail: %w", err)
		}
		set += ", detail_json = ?"
		args = append(args, d2)
	}
	args = append(args, id)
	res, err := d.db.ExecContext(ctx, `
		UPDATE merge_queue SET `+set+`
		WHERE id = ? AND settled_at IS NULL`, args...)
	if err != nil {
		return nil, fmt.Errorf("merge queue: mark evicted %d: %w", id, err)
	}
	return d.afterCAS(ctx, res, id)
}

// afterCAS resolves a CAS UPDATE's outcome: the fresh row on success,
// ErrMergeQueueConflict when the predicate matched nothing but the row exists,
// ErrNotFound when the row is gone.
func (d *MergeQueueDAO) afterCAS(ctx context.Context, res sql.Result, id int64) (*MergeQueueEntry, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("merge queue: rows affected %d: %w", id, err)
	}
	got, gerr := scanMergeQueueEntry(d.db.QueryRowContext(ctx, mergeQueueSelect+`
		WHERE id = ?`, id))
	if gerr != nil {
		return nil, gerr
	}
	if n == 0 {
		return got, ErrMergeQueueConflict
	}
	return got, nil
}

const mergeQueueSelect = `
	SELECT id, pipeline_run_id, backlog_id, project, mr_iid, source_branch, target_branch,
	       enqueued_sha, current_sha, state, eviction_reason, detail_json, attempts,
	       merged_sha, enqueued_at, updated_at, settled_at
	FROM merge_queue
`

func collectMergeQueueEntries(rows *sql.Rows) ([]*MergeQueueEntry, error) {
	var out []*MergeQueueEntry
	for rows.Next() {
		e, err := scanMergeQueueEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanMergeQueueEntry(s scanner) (*MergeQueueEntry, error) {
	var (
		e          MergeQueueEntry
		state      string
		reason     sql.NullString
		detail     string
		mergedSHA  sql.NullString
		enqueuedAt string
		updatedAt  string
		settledAt  sql.NullString
	)
	err := s.Scan(&e.ID, &e.PipelineRunID, &e.BacklogID, &e.Project, &e.MRIID,
		&e.SourceBranch, &e.TargetBranch, &e.EnqueuedSHA, &e.CurrentSHA,
		&state, &reason, &detail, &e.Attempts, &mergedSHA, &enqueuedAt, &updatedAt, &settledAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("merge queue scan: %w", err)
	}
	e.State = MergeQueueState(state)
	if reason.Valid {
		e.EvictionReason = reason.String
	}
	if mergedSHA.Valid {
		e.MergedSHA = mergedSHA.String
	}
	e.Detail = map[string]any{}
	if err := jsonInto(detail, &e.Detail); err != nil {
		return nil, fmt.Errorf("merge queue detail: %w", err)
	}
	if e.EnqueuedAt, err = parseTime(enqueuedAt); err != nil {
		return nil, fmt.Errorf("enqueued_at: %w", err)
	}
	if e.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("updated_at: %w", err)
	}
	if e.SettledAt, err = nullableTime(settledAt); err != nil {
		return nil, fmt.Errorf("settled_at: %w", err)
	}
	return &e, nil
}
