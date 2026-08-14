-- +goose Up
-- +goose StatementBegin

-- One durable journalengine Snapshot per backlog item: the cross-stage memory
-- the pipeline renders as a stable prompt prefix so stage N can see what stages
-- 1..N-1 actually did (docs/JOURNAL_ENGINE.md, step 2). Before this, the only
-- inter-stage prompt memory was the gate-fail retry discipline.
--
-- Named backlog_item_memory rather than anything containing "journal": migration
-- 004 and pkg/mills/workflow/journal_dao.go already own that word for the
-- exactly-once workflow effect ledger, which is an unrelated mechanism.
--
-- One row per item, overwritten in place. The snapshot is append-only *inside*
-- itself (journalengine.Journal.Render is a strict prefix extension between
-- consolidations), so last-write-wins on the row loses nothing a reader wanted.
CREATE TABLE backlog_item_memory (
    backlog_id      TEXT PRIMARY KEY REFERENCES backlog_items(id) ON DELETE CASCADE,
    snapshot_json   TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS backlog_item_memory;
-- +goose StatementEnd
