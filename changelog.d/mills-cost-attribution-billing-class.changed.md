- Mills budgets now distinguish who pays. Every stage attempt carries a
  billing class (`api` — a metered API account is charged; `subscription` —
  Claude Code / Codex turns under the cluster OAuth plans; `local` —
  flexinfer), derived from the credential path the HUD reports for the spawn
  (`auth_mode`) with a backend fallback, and migration 039 backfills history
  the same way. `budgets.*.max_usd_per_day` now caps **metered** spend only;
  the new optional `budgets.pipeline.max_subscription_usd_per_day` caps the
  subscription slice separately. `/api/mills/status` `budget.*.spent_usd` is
  the metered figure, with `total_spent_usd`, `subscription_spent_usd` and
  `subscription_cap_usd` beside it; KPIs gain `pipeline_api_cost_usd`,
  `pipeline_subscription_cost_usd`, `council_api_cost_usd`,
  `council_local_cost_usd`. Before this, subscription harness time (which
  bills nothing per token) exhausted the pipeline's $75/day cap by early
  afternoon and stalled the factory on phantom cost.
- Spawn runtime pins: Claude Code 2.1.220 → 2.1.270, Codex 0.144.6 → 0.154.0
  (`SPAWN_CODEX_VERSION` on mobile-hud too). Codex 0.154.0 was verified with
  the pinned CLI to complete a `gpt-6-astra` turn, which the policy can now
  route to.
