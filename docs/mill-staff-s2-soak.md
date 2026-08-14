# Mill Staff S2 overseer soak gate

Promotion from overseer dry-run is held until `EvaluateSoakGate` passes. The
gate evaluates only: it does not perform, queue, or record promotion. It passes
only when one `SoakGateTelemetry` snapshot satisfies every criterion:

- the closed soak window is at least 168 hours;
- the regression count is zero; and
- reviewed decision disagreements are strictly below 5% of reviewed decisions.

Exactly 5% disagreement fails. Missing, negative, or internally inconsistent
evidence also fails closed. At least one reviewed decision is required so the
rate has a meaningful denominator.

The input fields are `window`, `regressions`, `reviewed_decisions`, and
`disagreements`. Callers provide the window explicitly, making the 168-hour
boundary independent of wall time. The threshold comparison uses exact integer
arithmetic; the floating-point rate is output for observation only.

The JSON verdict contains `pass`, the observed
`decision_disagreement_rate`, stable `failure_reasons`, and
`mills_overseer_s2_soak_gate_pass`. The metric is numeric: `1` means all
criteria passed and `0` means promotion remains blocked.

## Persisted dry-run decision counters

Every successfully persisted overseer S2 dry-run decision increments
`loom_mills_overseer_dry_run_decisions_total`. Decisions rejected by the
persistence layer are not counted, so the counter describes the same evidence
population used by the fail-closed persisted-soak evaluation.

The counter has only two labels, each fixed to `true` or `false`:

- `would_have_acted` is the bounded dry-run verdict: `true` means approved
  execution would act and `false` means it would take no action;
- `diverged` says whether the dry-run decision disagreed with the approved
  policy for the same observation.

This produces at most four series. Subjects, action classes, policy text, and
errors are deliberately excluded from labels.

Use increases over a range when checking the soak because process restarts can
reset counters:

```promql
sum(increase(loom_mills_overseer_dry_run_decisions_total[7d]))
```

The action path was exercised when this query is greater than zero:

```promql
sum(increase(loom_mills_overseer_dry_run_decisions_total{would_have_acted="true"}[7d]))
```

The divergence exit criterion is exactly zero over the full seven-day window:

```promql
(sum(increase(loom_mills_overseer_dry_run_decisions_total{diverged="true"}[7d])) or vector(0)) == 0
```

The `or vector(0)` makes an absent `diverged="true"` series evaluate as zero;
without it, a clean soak that never creates that label combination returns an
empty vector instead of a passing result.

Promotion requires evidence for seven complete UTC days immediately preceding
the current UTC day, at least one decision and one `would_have_acted="true"`
decision in that persisted window, and zero divergences. The PromQL checks are
operational corroboration; missing day buckets, unreadable persistence, or
inconsistent counters still fail closed in `EvaluatePersistedS2Soak`.
