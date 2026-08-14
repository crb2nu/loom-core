-- +goose Up
-- +goose StatementBegin

-- One immutable row per observed movement of an MR source-branch head while a
-- pipeline run holds (or held) a CI authorization for it. Mirrors the
-- pipeline_transitions ledger shape from 011: the generic events table stays
-- advisory and unindexed, so a state machine that must QUERY its own history
-- gets a dedicated, indexable table.
--
-- GitLab's rebase endpoint has no source-SHA precondition (issue #374), so a
-- head movement can never be *proved* to be the replay Mills asked for. The
-- ledger therefore records movements rather than trusting them: every settled
-- non-noop row invalidates the CI authorization stamped for reviewed_sha and
-- forces a full re-gate of successor_sha.
CREATE TABLE mr_head_transitions (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    pipeline_run_id   TEXT    NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    seq               INTEGER NOT NULL,          -- 1-based, monotone per run
    project           TEXT    NOT NULL,
    mr_iid            INTEGER NOT NULL,
    source_branch     TEXT    NOT NULL,
    target_branch     TEXT    NOT NULL,
    reviewed_sha      TEXT    NOT NULL,          -- head that held CI authorization
    target_head_sha   TEXT    NOT NULL,          -- target tip observed at request time ('' when unread)
    successor_sha     TEXT,                      -- NULL until observed
    trigger           TEXT    NOT NULL CHECK (trigger IN ('rebase_request','external')),
    state             TEXT    NOT NULL CHECK (state IN
                        ('requested','in_progress','observed','attributed','ambiguous','failed','noop')),
    provenance_json   TEXT    NOT NULL DEFAULT '{}',
    requested_at      TEXT    NOT NULL,
    observed_at       TEXT,
    settled_at        TEXT,
    UNIQUE(pipeline_run_id, seq)
);

-- The merge stage's pre-flight looks for an unsettled row on every drive so a
-- process that died mid-rebase re-observes instead of re-mutating. Partial
-- index keeps that lookup independent of settled ledger depth.
CREATE INDEX idx_mr_head_transitions_open
    ON mr_head_transitions(pipeline_run_id, state)
    WHERE settled_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_mr_head_transitions_open;
DROP TABLE IF EXISTS mr_head_transitions;
-- +goose StatementEnd
