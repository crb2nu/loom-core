-- +goose Up
-- +goose StatementBegin

-- Serial merge queue (phase 1, no speculation) — one row per pipeline run
-- whose merge stage handed its merge to the queue instead of merging
-- directly. GitLab CE has no merge trains, so parallel Mills branches go
-- stale while long pipelines run and die with has_conflicts at the merge
-- PUT (killed !1509 and !1511 on 2026-08-08). The queue guarantees every
-- MR is CI-tested on the exact target-branch tip it lands on: rebase when
-- behind, await the rebased head's pipeline, merge, then promote the next
-- head.
--
-- FIFO order is the rowid (id) within a (project, target_branch) lane.
-- UNIQUE(pipeline_run_id) makes the merge stage's enqueue idempotent: a
-- stage retry or operator restart re-finds its entry instead of queueing
-- the same run twice. Terminal rows (merged/evicted) are retained as the
-- audit trail; the partial index keeps lane scans independent of history
-- depth, mirroring idx_mr_head_transitions_open.
CREATE TABLE merge_queue (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    pipeline_run_id   TEXT    NOT NULL UNIQUE REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    backlog_id        TEXT    NOT NULL,
    project           TEXT    NOT NULL,
    mr_iid            INTEGER NOT NULL,
    source_branch     TEXT    NOT NULL,
    target_branch     TEXT    NOT NULL,
    enqueued_sha      TEXT    NOT NULL,          -- head SHA ci_watch authorized at enqueue
    current_sha       TEXT    NOT NULL,          -- live head the queue is driving (advances on rebase)
    state             TEXT    NOT NULL CHECK (state IN
                        ('queued','rebasing','awaiting_pipeline','merging','merged','evicted')),
    eviction_reason   TEXT,                      -- NULL unless state='evicted'
    detail_json       TEXT    NOT NULL DEFAULT '{}',
    attempts          INTEGER NOT NULL DEFAULT 0, -- head-drive attempts (bounds rebase noop loops)
    merged_sha        TEXT,                      -- NULL until state='merged'
    enqueued_at       TEXT    NOT NULL,
    updated_at        TEXT    NOT NULL,
    settled_at        TEXT                       -- set when state IN ('merged','evicted')
);

CREATE INDEX idx_merge_queue_active
    ON merge_queue(project, target_branch, id)
    WHERE settled_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_merge_queue_active;
DROP TABLE IF EXISTS merge_queue;
-- +goose StatementEnd
