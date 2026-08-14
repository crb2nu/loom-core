- **Mills stage results now carry token counts, not just cost USD**
  (`pkg/mills/pipeline`, `pkg/mills/clients`): a spawn-dispatched stage
  (implement / plan_slice / pr_self_review) reported `total_cost_usd` and
  nothing else, so "is the agent harness getting its prompt prefix cached"
  was unanswerable for the stages that burn the most budget. The HUD had
  accumulated per-turn token usage all along and already serialised it as
  `token_usage` on the spawn detail wire — the operator's telemetry subset
  simply never decoded it. It now does, and `input_tokens`, `output_tokens`,
  `cache_read_tokens`, and `cache_creation_tokens` land in
  `stage_results.artifacts_json`. The research stage gains the completions
  equivalent (`prompt_tokens`, `completion_tokens`, `cached_prompt_tokens`)
  from `pkg/llmusage`, which already parsed both the `prompt_tokens_details`
  and `input_tokens_details` cached-token dialects.
  Purely additive and read-only: no request changes, nothing decides on the
  numbers, and a lane that reports no usage keeps a byte-identical artifact
  map rather than gaining zeros an aggregation would misread as a measured
  0% hit rate. Producer coverage: Claude reports all four counts; Codex and
  Gemini report input/output/cache-read but no cache-creation count.
