# Mill Staff S2 overseer promotion soak runbook

Use this runbook to make the S2 promotion decision after the overseer has run
in dry-run mode. It is a closed-window, fail-closed decision: evidence that is
missing, unreadable, inconsistent, or not attributable to the required window
is a failed soak, never a reason to promote.

## Decision window and evidence

Evaluate the seven **complete** UTC calendar days immediately before the
current UTC day. The current, still-open UTC day is not evidence. This is one
closed `168h` window; do not substitute an open-ended log search or count a
partial day at either boundary.

Fetch the authoritative artifact:

```
GET /api/mills/promotion-report?actor=overseer.&window=168h
```

Keep the returned promotion report, persisted UTC day buckets, overseer status,
and relevant audit evidence with the decision. The report must be complete and
readable, with `window_end > window_start`, actor prefix exactly `overseer.`,
`zero_evidence == false`, and `total_actions > 0`. A report error, truncation,
or saturated event scan is incomplete evidence and fails the soak.

## Promotion checklist

Promote only when every item is true for the same closed window:

- Seven ordered, complete UTC day buckets are present; each has at least one
  dry-run decision.
- `mills_overseer_soak_elapsed_days` is at least 7 whole days (`168h`).
- `mills_overseer_soak_dry_run_decisions` is at least 1.
- `mills_overseer_soak_would_have_acted` is at least 1. This proves the
  overseer evaluated an action that approved execution would have taken.
- `mills_overseer_soak_divergences` equals 0. A reviewed policy disagreement
  and any committed action within the dry-run window are divergences.
- The evidence is readable and internally consistent: persisted counters,
  report totals, and audit/status evidence describe the same window.
- There is no unreviewed `LastError`, unexpected committed action, or missing
  required external-dependency recovery evidence.
- The verdict reports `promotable: true` and `fail_closed: false` (with no
  `failure_reasons`).

The stable verdict fields to record are
`mills_overseer_soak_elapsed_days`, `mills_overseer_soak_dry_run_decisions`,
`mills_overseer_soak_would_have_acted`, `mills_overseer_soak_divergences`,
`promotable`, `fail_closed`, and `failure_reasons`.

The Prometheus dry-run counter is corroborating evidence, not a replacement for
the persisted evaluation. Use `increase` over the closed seven-day interval,
because process restarts reset counters. In particular, require at least one
`would_have_acted="true"` decision and exactly zero `diverged="true"`
decisions; missing day buckets or unreadable persistence still fail closed.

## Fail-closed abort and escalation

Abort the promotion attempt immediately if any checklist item is false or
unavailable. This includes a short, partial, or timezone-ambiguous window; a
missing day; unreadable or malformed evidence; counter disagreement; zero
would-have-acted decisions; any divergence; any committed action; or a verdict
with `promotable: false` or `fail_closed: true`.

On abort:

1. Preserve the report, UTC buckets, status/audit evidence, and the failure
   reason without modifying them.
2. Leave the overseer in `dry_run`; do not flip an allow flag or retry into a
   passing verdict by changing the window.
3. Escalate to the Mills operator/on-call and, when applicable, the named
   dependency owner. Include the closed UTC window, failed checklist item,
   verdict fields, and supporting evidence.
4. Resolve the evidence or underlying policy/dependency issue, then begin a
   fresh closed seven-complete-day soak. Do not reuse partial or failed-window
   evidence for promotion.

Record an S2 promotion decision only after the full checklist passes. If an
operator cannot establish a pass from the durable evidence, the correct result
is abort and escalation.
