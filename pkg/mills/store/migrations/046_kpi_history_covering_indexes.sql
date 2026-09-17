-- +goose Up
-- +goose StatementBegin

-- KPI gate aggregates need only these three fields, never reasons_json.
-- Retain the leading timestamp so existing window readers keep their range scan.
DROP INDEX idx_gate_outcomes_evaluated;
CREATE INDEX idx_gate_outcomes_evaluated
    ON gate_outcomes(evaluated_at DESC, outcome, judged_by);

-- Retry burn is windowed by the parent run, not the stage timestamp. Cover the
-- per-run retry lookup without reading stage artifacts or log_tail pages.
CREATE INDEX idx_stage_retry_cost
    ON stage_results(pipeline_run_id, attempt, cost_usd)
    WHERE attempt > 1;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_stage_retry_cost;
DROP INDEX idx_gate_outcomes_evaluated;
CREATE INDEX idx_gate_outcomes_evaluated ON gate_outcomes(evaluated_at DESC);
-- +goose StatementEnd
