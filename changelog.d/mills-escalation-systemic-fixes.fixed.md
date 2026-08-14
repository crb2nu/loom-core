- Mills: judge-backed gate errors are now classified instead of escalating
  unclassified. A gate whose judge CALL fails (litellm 400, provider 429,
  timeout, 5xx) gets bounded free re-judges — no respawn, no attempt spend —
  and escalates with an honest `[class=infra]`/`[class=transient]` marker so
  bounded auto-requeue can recover it; a gate error is never `code` (the
  judge produced no verdict on the diff). Known external-dependency incidents
  escalate first-sight as `config` with the incident attached.
- Mills: the FlexInfer judge client now walks its model fallback chain on
  provider 429s (parking the rate-limited model for the cooldown) and on
  generic 400s that are not the strict route-incompatible pair — a
  provider-specific rejection of model A says nothing about fallback model B.
- Mills: new spawn-transport circuit breaker holds new dispatch when ≥3
  distinct runs escalate with the same `spawn-*` infra reason inside a
  rolling window (defaults 3×/30m, 15m cooldown, `pipeline.spawn_breaker`
  policy block). A vendor outage now surfaces as one autonomy blocker
  instead of every queued item burning its attempts into the outage.
  In-flight runs are untouched; a store read error keeps the breaker closed.
