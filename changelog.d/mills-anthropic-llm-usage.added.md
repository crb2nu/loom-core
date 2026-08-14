- **Anthropic council traffic now reports LLM usage** (`pkg/mills/clients/anthropic.go`):
  the Anthropic Messages backend (council editor + rubric-judge tiebreaker) was
  the one chat path left out of the `pkg/llmusage` instrumentation from
  MR !1223 — its cache numbers reached only the editor's sidecar notes, so the
  warm-share queries in `docs/JOURNAL_ENGINE.md` showed no council-editor
  series at all when the deployed policy uses the anthropic backend. It now
  emits the same `llm usage` debug line and
  `mills_llm_{prompt,cached_prompt,completion}_tokens_total` counters, tagged
  `mills-council-editor` / `mills-judge`. Mapping keeps Anthropic's native
  semantics (`input_tokens` → prompt, `cache_read_input_tokens` → cached, so
  `cached_share` can exceed 1 on a warm prefix); `cache_creation_input_tokens`
  (cache writes, a quantity with no OpenAI equivalent) deliberately gets no
  counter and stays in the sidecar notes.
