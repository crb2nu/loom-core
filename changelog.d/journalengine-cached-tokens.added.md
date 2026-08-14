- `pkg/llmusage`: read-only instrumentation of
  `usage.prompt_tokens_details.cached_tokens` (and the Responses API's
  `input_tokens_details` equivalent) across every OpenAI-compatible chat client
  in the repo — `pkg/flexinfer`, `pkg/mills/clients`, `pkg/weaver`,
  `pkg/openairesponses`, `pkg/agentloop`, `cmd/mcp-morph-fast-apply`. Each
  completion emits one structured debug line under the message `llm usage`
  with `llm_component` / `llm_model` / `prompt_tokens` / `cached_tokens` /
  `cached_share`. Prometheus counters were added only where a registry already
  existed: `mills_llm_{prompt,cached_prompt,completion}_tokens_total`,
  `loom_coordinator_llm_{prompt,cached_prompt,completion}_tokens_total`, and
  `loom_weaver_tokens_total{direction="cached_prompt"}`. Mills traffic is
  labelled per caller (judge / weaver / council editor / council reviewer /
  eval judge) since one client serves all of them. `pkg/agentloop` now prefers
  the response body's cached count over the proxy's
  `X-Flexinfer-Cached-Tokens` header, so lanes reached without the FlexInfer
  proxy report cached tokens where they previously reported none. Zero behavior
  change: no request is altered and nothing reads the numbers back. This is the
  prerequisite measurement for the `pkg/journalengine` adoption in
  `docs/JOURNAL_ENGINE.md` — see its new "Reading the cache data" section for
  the log/metric queries, and "Step 1 revisited" for why `pkg/agentcontext`
  turned out not to be a viable first adopter.
