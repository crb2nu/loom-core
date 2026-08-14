- **Mills research (weaver) api-call path is now safe for a reasoning model**
  (`pkg/mills/clients/flexinfer.go`, `cmd/loom-mills-operator`): the legacy
  research path (`WeaverClient.legacyResearch`, `research_mode=off`) hardcoded a
  1024-token budget with no env override and had no reasoning-aware recovery, so
  flipping the research model to a thinking model (`or/kimi-k3` via the LiteLLM
  gateway) would return empty/truncated notes — the same
  `message.reasoning_content` squeeze the rubric judge hit (issue #348 / !1133),
  where chain-of-thought counts against `max_tokens` and consumes the whole
  completion before the answer is emitted. Two fixes make the gitops env flip
  safe: (1) a new `FLEXINFER_WEAVER_MAX_TOKENS` env (mirrors
  `FLEXINFER_JUDGE_MAX_TOKENS`) wired into `WeaverClient.MaxTokens` at
  construction and surfaced on the `/wiring` snapshot — default stays `1024` so
  the local qwen research model is unchanged; set `>= 4096` for a reasoning
  model; (2) the weaver research call now reuses the judge's reasoning-aware
  recovery (`responseHadReasoning` + `boostedRetryTokens`, floored to 4096) so an
  empty completion from a reasoning-model budget squeeze (or a
  `finish_reason=length` truncation) triggers exactly one boosted retry instead
  of returning empty notes. The local qwen (non-reasoning) path is
  byte-identical: a non-empty answer returns on the first call, and a genuine
  empty answer (`finish_reason=stop`, no reasoning) is not retried. Re-flip
  guidance: `MILLS_WEAVER_BACKEND=litellm`, `FLEXINFER_WEAVER_MODEL=or/kimi-k3`,
  `FLEXINFER_WEAVER_MAX_TOKENS=4096`.
