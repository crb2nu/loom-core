Stop the hub's agent-context, codebase-memory and mobile-hud deployments from
asking FlexInfer for the retired Morph embedding model. Their manifests still
set `MORPH_BASE_URL`/`MORPH_EMBED_MODEL=morph-embedding-v3`, and those names sit
ahead of the FlexInfer defaults in every embed env chain, so each embed call
went to `flexinfer-proxy` for a model it does not serve (`Model
'morph-embedding-v3' not found`) and fell back to keyword search — about 5,000
failures in six hours on agent-context alone on 2026-09-05, and one of the
things that kept its per-session child processes alive long enough to OOM the
pod. With the trio removed the chains land on `pkg/flexinfer.DefaultEmbedBaseURL`
and `DefaultEmbedModel` (`embeddings-1536`, verified 200 / dim 1536 on the
proxy). `pkg/agentcontext` also gains the same `normalizeRetiredEmbedConfig`
guard `pkg/pm` and `pkg/codebase` already had, so a retired Morph model, host
or provider in any environment collapses to the FlexInfer defaults instead of
being honored. `morph-fast-apply` keeps its `MORPH_*` settings: that is the separate
chat-completions product, not embeddings. Collections written with Morph
vectors before 2026-08-13 live in a different vector space and still need a
re-embed for best recall; everything written since was a fallback vector.
