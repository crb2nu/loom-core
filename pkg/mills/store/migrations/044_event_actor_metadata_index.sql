-- Promotion reports stream event metadata without payloads, but the narrow
-- actor/time index still required a table-page fetch for every matching row.
-- Cover the projection so cold reports need not read payload-bearing pages.
-- Keep id immediately after occurred_at: recent-actor reads order timestamp
-- ties by id DESC and must retain their sort-free backward index walk.
-- The migrator wraps this replacement and its version record in one transaction.
-- Replacing the narrow index avoids maintaining two actor indexes per append.
DROP INDEX IF EXISTS idx_events_actor_occurred;
CREATE INDEX idx_events_actor_occurred
    ON events(actor, occurred_at, id, kind, subject_kind, subject_id);
