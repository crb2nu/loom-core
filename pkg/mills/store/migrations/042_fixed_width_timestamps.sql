-- +goose Up
-- +goose StatementBegin
-- Rewrite every stored timestamp to the fixed-width layout the store now
-- writes (pkg/mills/store/jsonutil.go timeLayout): UTC, nine fractional
-- digits, literal Z, always 30 bytes. Rows were previously written with
-- time.RFC3339Nano, which trims trailing fractional zeros, so "…28.8481Z"
-- sorted AFTER "…28.84815Z" under SQLite's byte-wise TEXT collation ('Z' >
-- '5'), misordering rows inside the same second and letting keyset cursors
-- rebuilt from a parsed row miss or re-match their own row.
--
-- Only rows in the trimmed form are touched: already fixed-width values
-- (length 30) and anything not shaped like a UTC RFC3339 timestamp are left
-- as they are, so the statements are idempotent and safe to re-run.
--
-- Column list = every TEXT column named *_at (plus recheck_after and
-- revert_deadline, written the same way) in the schema as of this version;
-- pkg/mills/store/migrate_042_test.go checks it stays complete.
UPDATE audit_findings
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE auto_requeue_sweep_cursor
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE backlog_item_memory
SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE updated_at IS NOT NULL AND length(updated_at) < 30
  AND (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE backlog_items
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE backlog_items
SET graded_at = substr(graded_at, 1, 19) || '.' || substr(substr(rtrim(graded_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE graded_at IS NOT NULL AND length(graded_at) < 30
  AND (graded_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR graded_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE backlog_items
SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE updated_at IS NOT NULL AND length(updated_at) < 30
  AND (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE bootstrapped_projects
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE council_budget_reservations
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE council_budget_reservations
SET expires_at = substr(expires_at, 1, 19) || '.' || substr(substr(rtrim(expires_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE expires_at IS NOT NULL AND length(expires_at) < 30
  AND (expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE council_budget_reservations
SET released_at = substr(released_at, 1, 19) || '.' || substr(substr(rtrim(released_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE released_at IS NOT NULL AND length(released_at) < 30
  AND (released_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR released_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE council_debate_rounds
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE council_memory
SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE updated_at IS NOT NULL AND length(updated_at) < 30
  AND (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE council_runs
SET ended_at = substr(ended_at, 1, 19) || '.' || substr(substr(rtrim(ended_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE ended_at IS NOT NULL AND length(ended_at) < 30
  AND (ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE council_runs
SET started_at = substr(started_at, 1, 19) || '.' || substr(substr(rtrim(started_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE started_at IS NOT NULL AND length(started_at) < 30
  AND (started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE cross_repo_stamps
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE escalation_sweep_state
SET recheck_after = substr(recheck_after, 1, 19) || '.' || substr(substr(rtrim(recheck_after, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE recheck_after IS NOT NULL AND length(recheck_after) < 30
  AND (recheck_after GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR recheck_after GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE eval_scores
SET evaluated_at = substr(evaluated_at, 1, 19) || '.' || substr(substr(rtrim(evaluated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE evaluated_at IS NOT NULL AND length(evaluated_at) < 30
  AND (evaluated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR evaluated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE events
SET occurred_at = substr(occurred_at, 1, 19) || '.' || substr(substr(rtrim(occurred_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE occurred_at IS NOT NULL AND length(occurred_at) < 30
  AND (occurred_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR occurred_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE external_incident_dwells
SET completed_at = substr(completed_at, 1, 19) || '.' || substr(substr(rtrim(completed_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE completed_at IS NOT NULL AND length(completed_at) < 30
  AND (completed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR completed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE external_incident_dwells
SET deadline_at = substr(deadline_at, 1, 19) || '.' || substr(substr(rtrim(deadline_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE deadline_at IS NOT NULL AND length(deadline_at) < 30
  AND (deadline_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR deadline_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE external_incident_dwells
SET started_at = substr(started_at, 1, 19) || '.' || substr(substr(rtrim(started_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE started_at IS NOT NULL AND length(started_at) < 30
  AND (started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE gate_outcomes
SET evaluated_at = substr(evaluated_at, 1, 19) || '.' || substr(substr(rtrim(evaluated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE evaluated_at IS NOT NULL AND length(evaluated_at) < 30
  AND (evaluated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR evaluated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE kpi_snapshots
SET snapshot_at = substr(snapshot_at, 1, 19) || '.' || substr(substr(rtrim(snapshot_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE snapshot_at IS NOT NULL AND length(snapshot_at) < 30
  AND (snapshot_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR snapshot_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE maintenance_cursors
SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE updated_at IS NOT NULL AND length(updated_at) < 30
  AND (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE merge_queue
SET enqueued_at = substr(enqueued_at, 1, 19) || '.' || substr(substr(rtrim(enqueued_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE enqueued_at IS NOT NULL AND length(enqueued_at) < 30
  AND (enqueued_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR enqueued_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE merge_queue
SET settled_at = substr(settled_at, 1, 19) || '.' || substr(substr(rtrim(settled_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE settled_at IS NOT NULL AND length(settled_at) < 30
  AND (settled_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR settled_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE merge_queue
SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE updated_at IS NOT NULL AND length(updated_at) < 30
  AND (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE mr_head_transitions
SET observed_at = substr(observed_at, 1, 19) || '.' || substr(substr(rtrim(observed_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE observed_at IS NOT NULL AND length(observed_at) < 30
  AND (observed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR observed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE mr_head_transitions
SET requested_at = substr(requested_at, 1, 19) || '.' || substr(substr(rtrim(requested_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE requested_at IS NOT NULL AND length(requested_at) < 30
  AND (requested_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR requested_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE mr_head_transitions
SET settled_at = substr(settled_at, 1, 19) || '.' || substr(substr(rtrim(settled_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE settled_at IS NOT NULL AND length(settled_at) < 30
  AND (settled_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR settled_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE outcome_writebacks
SET written_at = substr(written_at, 1, 19) || '.' || substr(substr(rtrim(written_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE written_at IS NOT NULL AND length(written_at) < 30
  AND (written_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR written_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE pending_dispatches
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE pending_dispatches
SET dead_lettered_at = substr(dead_lettered_at, 1, 19) || '.' || substr(substr(rtrim(dead_lettered_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE dead_lettered_at IS NOT NULL AND length(dead_lettered_at) < 30
  AND (dead_lettered_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR dead_lettered_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE pending_dispatches
SET delivered_at = substr(delivered_at, 1, 19) || '.' || substr(substr(rtrim(delivered_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE delivered_at IS NOT NULL AND length(delivered_at) < 30
  AND (delivered_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR delivered_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE pending_dispatches
SET lease_expires_at = substr(lease_expires_at, 1, 19) || '.' || substr(substr(rtrim(lease_expires_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE lease_expires_at IS NOT NULL AND length(lease_expires_at) < 30
  AND (lease_expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR lease_expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE pending_dispatches
SET next_attempt_at = substr(next_attempt_at, 1, 19) || '.' || substr(substr(rtrim(next_attempt_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE next_attempt_at IS NOT NULL AND length(next_attempt_at) < 30
  AND (next_attempt_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR next_attempt_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE pending_dispatches
SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE updated_at IS NOT NULL AND length(updated_at) < 30
  AND (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE pipeline_budget_reservations
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE pipeline_budget_reservations
SET released_at = substr(released_at, 1, 19) || '.' || substr(substr(rtrim(released_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE released_at IS NOT NULL AND length(released_at) < 30
  AND (released_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR released_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE pipeline_runs
SET ended_at = substr(ended_at, 1, 19) || '.' || substr(substr(rtrim(ended_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE ended_at IS NOT NULL AND length(ended_at) < 30
  AND (ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE pipeline_runs
SET started_at = substr(started_at, 1, 19) || '.' || substr(substr(rtrim(started_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE started_at IS NOT NULL AND length(started_at) < 30
  AND (started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE pipeline_transitions
SET occurred_at = substr(occurred_at, 1, 19) || '.' || substr(substr(rtrim(occurred_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE occurred_at IS NOT NULL AND length(occurred_at) < 30
  AND (occurred_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR occurred_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE policy_proposals
SET applied_at = substr(applied_at, 1, 19) || '.' || substr(substr(rtrim(applied_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE applied_at IS NOT NULL AND length(applied_at) < 30
  AND (applied_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR applied_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE policy_proposals
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE policy_proposals
SET revert_deadline = substr(revert_deadline, 1, 19) || '.' || substr(substr(rtrim(revert_deadline, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE revert_deadline IS NOT NULL AND length(revert_deadline) < 30
  AND (revert_deadline GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR revert_deadline GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE report_rollup_snapshots
SET snapshot_at = substr(snapshot_at, 1, 19) || '.' || substr(substr(rtrim(snapshot_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE snapshot_at IS NOT NULL AND length(snapshot_at) < 30
  AND (snapshot_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR snapshot_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE roadmap_intents
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE roadmap_intents
SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE updated_at IS NOT NULL AND length(updated_at) < 30
  AND (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE schema_migrations
SET applied_at = substr(applied_at, 1, 19) || '.' || substr(substr(rtrim(applied_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE applied_at IS NOT NULL AND length(applied_at) < 30
  AND (applied_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR applied_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE scope_fairness_state
SET first_deferred_at = substr(first_deferred_at, 1, 19) || '.' || substr(substr(rtrim(first_deferred_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE first_deferred_at IS NOT NULL AND length(first_deferred_at) < 30
  AND (first_deferred_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR first_deferred_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE scope_fairness_state
SET reserved_at = substr(reserved_at, 1, 19) || '.' || substr(substr(rtrim(reserved_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE reserved_at IS NOT NULL AND length(reserved_at) < 30
  AND (reserved_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR reserved_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE spin_runs
SET ended_at = substr(ended_at, 1, 19) || '.' || substr(substr(rtrim(ended_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE ended_at IS NOT NULL AND length(ended_at) < 30
  AND (ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE spin_runs
SET started_at = substr(started_at, 1, 19) || '.' || substr(substr(rtrim(started_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE started_at IS NOT NULL AND length(started_at) < 30
  AND (started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE squad_memory
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE squad_memory
SET last_seen_at = substr(last_seen_at, 1, 19) || '.' || substr(substr(rtrim(last_seen_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE last_seen_at IS NOT NULL AND length(last_seen_at) < 30
  AND (last_seen_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR last_seen_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE squad_outcomes
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE squads
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE squads
SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(rtrim(updated_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE updated_at IS NOT NULL AND length(updated_at) < 30
  AND (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE stage_results
SET ended_at = substr(ended_at, 1, 19) || '.' || substr(substr(rtrim(ended_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE ended_at IS NOT NULL AND length(ended_at) < 30
  AND (ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE stage_results
SET started_at = substr(started_at, 1, 19) || '.' || substr(substr(rtrim(started_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE started_at IS NOT NULL AND length(started_at) < 30
  AND (started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE watches
SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(rtrim(created_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE created_at IS NOT NULL AND length(created_at) < 30
  AND (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE watches
SET expires_at = substr(expires_at, 1, 19) || '.' || substr(substr(rtrim(expires_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE expires_at IS NOT NULL AND length(expires_at) < 30
  AND (expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE watches
SET resolved_at = substr(resolved_at, 1, 19) || '.' || substr(substr(rtrim(resolved_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE resolved_at IS NOT NULL AND length(resolved_at) < 30
  AND (resolved_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR resolved_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE workflow_runs
SET ended_at = substr(ended_at, 1, 19) || '.' || substr(substr(rtrim(ended_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE ended_at IS NOT NULL AND length(ended_at) < 30
  AND (ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE workflow_runs
SET paused_at = substr(paused_at, 1, 19) || '.' || substr(substr(rtrim(paused_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE paused_at IS NOT NULL AND length(paused_at) < 30
  AND (paused_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR paused_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE workflow_runs
SET resumed_at = substr(resumed_at, 1, 19) || '.' || substr(substr(rtrim(resumed_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE resumed_at IS NOT NULL AND length(resumed_at) < 30
  AND (resumed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR resumed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE workflow_runs
SET started_at = substr(started_at, 1, 19) || '.' || substr(substr(rtrim(started_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE started_at IS NOT NULL AND length(started_at) < 30
  AND (started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE workflow_steps
SET ended_at = substr(ended_at, 1, 19) || '.' || substr(substr(rtrim(ended_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE ended_at IS NOT NULL AND length(ended_at) < 30
  AND (ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
UPDATE workflow_steps
SET started_at = substr(started_at, 1, 19) || '.' || substr(substr(rtrim(started_at, 'Z'), 21) || '000000000', 1, 9) || 'Z'
WHERE started_at IS NOT NULL AND length(started_at) < 30
  AND (started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
    OR started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
