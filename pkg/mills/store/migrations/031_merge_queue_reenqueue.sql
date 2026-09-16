-- +goose Up
-- +goose StatementBegin

-- Re-admission after eviction (shepherd theme A1). Migration 024 made
-- pipeline_run_id column-UNIQUE so a stage retry re-finds its candidate
-- instead of double-queueing. That also made every eviction permanent for
-- the run: after a head_moved rewind re-gates and re-proves the successor
-- SHA, the merge stage's re-enqueue found the settled row and re-reported
-- the stale eviction — three no-op attempts, then an escalation for work
-- the queue was never allowed to retry.
--
-- The idempotency contract the stage actually needs is one ACTIVE row per
-- run: resumes re-find their in-flight candidate, a merged verdict stays
-- authoritative forever, and only an evicted run that comes back through
-- the full enqueue-time validation (the #374 fence re-authorizes the new
-- head) may insert a fresh row. History rows are retained per 024's audit
-- contract; readers take the newest row for a run.
CREATE TABLE merge_queue_new (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    pipeline_run_id   TEXT    NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    backlog_id        TEXT    NOT NULL,
    project           TEXT    NOT NULL,
    mr_iid            INTEGER NOT NULL,
    source_branch     TEXT    NOT NULL,
    target_branch     TEXT    NOT NULL,
    enqueued_sha      TEXT    NOT NULL,
    current_sha       TEXT    NOT NULL,
    state             TEXT    NOT NULL CHECK (state IN
                        ('queued','rebasing','awaiting_pipeline','merging','merged','evicted')),
    eviction_reason   TEXT,
    detail_json       TEXT    NOT NULL DEFAULT '{}',
    attempts          INTEGER NOT NULL DEFAULT 0,
    merged_sha        TEXT,
    enqueued_at       TEXT    NOT NULL,
    updated_at        TEXT    NOT NULL,
    settled_at        TEXT
);

INSERT INTO merge_queue_new (
    id, pipeline_run_id, backlog_id, project, mr_iid, source_branch,
    target_branch, enqueued_sha, current_sha, state, eviction_reason,
    detail_json, attempts, merged_sha, enqueued_at, updated_at, settled_at)
SELECT id, pipeline_run_id, backlog_id, project, mr_iid, source_branch,
    target_branch, enqueued_sha, current_sha, state, eviction_reason,
    detail_json, attempts, merged_sha, enqueued_at, updated_at, settled_at
FROM merge_queue;

DROP TABLE merge_queue;
ALTER TABLE merge_queue_new RENAME TO merge_queue;

CREATE UNIQUE INDEX uq_merge_queue_active_run
    ON merge_queue(pipeline_run_id)
    WHERE settled_at IS NULL;

CREATE INDEX idx_merge_queue_active
    ON merge_queue(project, target_branch, id)
    WHERE settled_at IS NULL;

-- +goose StatementEnd
