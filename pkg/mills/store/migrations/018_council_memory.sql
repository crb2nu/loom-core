-- +goose Up
-- +goose StatementBegin

-- One durable journalengine Snapshot for the council LANE (not per item): the
-- cross-run memory the council editor renders as a stable prompt prefix so tick
-- N+1 knows what tick N minted and why (docs/JOURNAL_ENGINE.md, step 2 point 5).
-- Before this the council recompiled a fresh brief every tick and remembered
-- nothing of its own past deliberations — brief.Compile() truncates from the
-- tail, so the oldest context silently vanished rather than being carried.
--
-- Named council_memory rather than anything containing "journal", for the same
-- reason backlog_item_memory (migration 017) is: migration 004 and
-- pkg/mills/workflow/journal_dao.go already own that word for the exactly-once
-- workflow effect ledger, which is an unrelated mechanism.
--
-- One row, keyed by a fixed lane id ('council'), overwritten in place. The lane
-- column exists so a future second council lane (per-repo, per-squad) is a new
-- row rather than a migration. The snapshot is append-only *inside* itself
-- (journalengine.Journal.Render is a strict prefix extension between
-- consolidations), so last-write-wins on the row loses nothing a reader wanted.
CREATE TABLE council_memory (
    lane            TEXT PRIMARY KEY,
    snapshot_json   TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS council_memory;
-- +goose StatementEnd
