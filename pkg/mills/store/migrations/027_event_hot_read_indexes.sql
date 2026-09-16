-- Keep event window reads on the append-friendly index created by migration
-- 001. Repeating it here is harmless and documents the hot-read dependency
-- without adding another index to the hottest write path.
CREATE INDEX IF NOT EXISTS idx_events_occurred
    ON events(occurred_at);

CREATE INDEX IF NOT EXISTS idx_pipeline_escalation_window
    ON pipeline_runs(state, started_at DESC, escalation_class);

CREATE INDEX IF NOT EXISTS idx_gate_outcomes_evaluated
    ON gate_outcomes(evaluated_at DESC);
