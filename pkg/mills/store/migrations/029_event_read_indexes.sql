-- Covering read indexes for the event window queries — overturning, on
-- measured evidence, the bet that 001/027 made twice: that ListByActorSince,
-- ListSinceByActorPrefix and ListSinceByKinds could ride idx_events_occurred
-- (full window scan + row filter) because they were "bounded, low-traffic
-- status queries" and a read index "would tax EVERY event append".
--
-- 2026-08-15 falsified both premises in one day: the uplift storm multiplied
-- the 14-day event window, and the HUD fires ~a dozen of these scans
-- concurrently per dashboard load. Observed on the idle operator
-- (2026-08-16 ~03:10Z): ~992m CPU at queue depth 0, a log storm of
-- `context canceled` on every 336h report query, /api/mills/kpis timing out
-- >20s behind the scans, take-up tick 10.7s, plan-slice emitter tick 31.3s.
--
-- The append tax is MEASURED, not assumed (BenchmarkFleetMillsEventAppend,
-- 2s x3, darwin/arm64): baseline ~23.7µs/op; actor index alone +24%; kind
-- index alone +28%; both +52%. That breaches the fleet-reliability gate's
-- 10% budget, and the operator approved a documented, EXPIRING waiver
-- (scripts/ci/fleet_reliability_suite_v1.json "waivers", until 2026-09-15)
-- because the absolute append cost (~+13µs at low append rates) buys back an
-- entire pinned core on the read side. The durable fix — materialized report
-- rollups (bl-mills-report-rollups-20260816) — may allow dropping these
-- indexes again; if so, bring numbers, as this migration did.
CREATE INDEX IF NOT EXISTS idx_events_actor_occurred
    ON events(actor, occurred_at);

CREATE INDEX IF NOT EXISTS idx_events_kind_occurred
    ON events(kind, occurred_at);
