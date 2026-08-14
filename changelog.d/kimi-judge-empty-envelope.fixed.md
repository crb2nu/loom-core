- **Mills rubric judge no longer false-fails finished work on empty kimi-k3
  responses** (`pkg/mills/clients/flexinfer.go`): kimi-k3 is a thinking model
  whose chain-of-thought returns in a separate `message.reasoning_content` field
  and counts against `max_tokens`; at the 1024-token judge budget the reasoning
  frequently consumed the whole completion, so the score envelope was never
  emitted (`content=""`) and gates escalated finished work (issue #348). The
  client now decodes the reasoning-bearing message shape, and the judge's
  single boosted retry also fires on an empty completion (not just
  `finish_reason=length`), flooring the retry budget to 4096 tokens when the
  response showed reasoning activity so the chain-of-thought and the envelope
  both fit. A gate never false-fails on `raw=""` without one boosted retry, on
  both the FlexInfer proxy and the LiteLLM gateway backends. Re-flip guidance:
  set `FLEXINFER_JUDGE_MAX_TOKENS=4096` for `or/kimi-k3`.
