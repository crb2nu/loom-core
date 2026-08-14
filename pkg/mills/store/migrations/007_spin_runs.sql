-- 007_spin_runs.sql — job/status store for ASYNC Spinning-Room spins.
--
-- Motivation (slow-frame durability, plan .loom/166): a synchronous spin holds
-- the HTTP connection for the whole model synthesis + plan-store write. A
-- frontier frame (claude-opus-4-8, adaptive thinking) legitimately runs minutes,
-- which exceeds the client-facing proxy timeout (Cloudflare 524 at ~100s) no
-- matter what the operator does server-side. The durable fix is async spins:
-- POST /api/mills/spin/async returns 202 + a spin_id immediately, the operator
-- spins in a detached goroutine, and the resulting draft plan lands in the Plan
-- Store. This table is that job's status record so a client (HUD) can poll
-- GET /api/mills/spin/runs/{id} instead of holding a connection open.
--
-- Mirrors council_runs (migration 001): a durable row per run so status
-- survives an operator restart (which happens on every deploy). On startup the
-- operator sweeps any non-terminal row to failed(orphaned) — the goroutine died
-- with the old pod; the draft plan, if it was authored before the crash, is
-- still durable in the Plan Store (agent-context), not here.
--
-- status:      pending (queued behind the concurrency semaphore) → running →
--              succeeded | failed | timeout. plan_ids_json holds the 0..N draft
--              plan ids the spin authored (>1 for a competitive spin).
-- competitive: 1 when the request used frames[] (one draft per frame).
-- Append-only: nothing in 001–006 changes.

CREATE TABLE spin_runs (
    id            TEXT PRIMARY KEY,
    brief         TEXT NOT NULL DEFAULT '',
    frames_json   TEXT NOT NULL DEFAULT '[]',
    priority      TEXT,
    project       TEXT,
    namespace     TEXT,
    status        TEXT NOT NULL,
    plan_ids_json TEXT NOT NULL DEFAULT '[]',
    error         TEXT,
    competitive   INTEGER NOT NULL DEFAULT 0,
    started_at    TEXT NOT NULL,
    ended_at      TEXT
);

-- List() orders newest-first by started_at.
CREATE INDEX idx_spin_started ON spin_runs(started_at);

-- The startup orphan-sweep and the "spins in flight" read both filter on the
-- non-terminal states; a partial index keeps that cheap without weighing on the
-- common terminal-row reads.
CREATE INDEX idx_spin_status_active
  ON spin_runs(status)
  WHERE status IN ('pending', 'running');
