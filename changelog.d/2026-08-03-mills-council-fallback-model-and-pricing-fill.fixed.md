Mills council no longer hard-fails when the remote editor backend errors, and
no longer burns its daily budget on gateway pricing omissions:

- The per-run fallback (and no-API-key degrade) editor previously inherited
  the remote frontier model id (e.g. `claude-fable-5`), which flexinfer can
  never serve — any Anthropic/OpenAI error became a guaranteed local 404 and
  a hard council failure (COUNCIL-2026-08-03-060011/-120011, triggered by a
  cluster DNS blip). The fallback now resolves to the flexinfer weaver chain,
  with an optional `council.ensemble.editor_fallback_model` policy pin.
- Reviewer lenses on known LiteLLM gateway models (`or/deepseek-chat`,
  `or/kimi-k3`) now fill a usage.cost omission from pinned ceiling prices,
  mirroring the judge's `knownRemoteJudgeCost`. Previously one omission
  marked the whole run unpriced and charged the full $15 admission
  reservation — a $0.47 run (COUNCIL-2026-08-03-000011) was billed $15.00,
  and three such charges drained the council's $50 daily budget.
