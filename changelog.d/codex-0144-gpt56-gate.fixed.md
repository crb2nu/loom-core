Bump the spawn codex CLI pin 0.143.0 → 0.144.6: 0.143.0 still trips OpenAI's
gpt-5.6 version gate (HTTP 400 "requires a newer version of Codex"), so every
`stage_models` codex spawn (plan_slice/implement) failed at $0 since the
2026-07-18 re-enable — the 64% plan_slice error rate in
`/api/mills/telemetry/stages`. 0.144.6 verified in-pod (cluster OAuth,
gpt-5.6-sol and gpt-5.6-terra both complete turns). Also classify the wedge's
log-tail shapes (`exited 124` / `command timed out` / `deadline exceeded
during reconciliation` / stall) as `spawn_infra` in stage telemetry instead of
`other`.
