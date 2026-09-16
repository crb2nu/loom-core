# Mill Staff S2 overseer dry-run soak

This runbook is the canonical S2 exit contract for an overseer action class.
It applies independently to each action class; passing evidence for one class
does not authorize another class. A pass authorizes a human to consider a
promotion. It never changes an allow flag or enables a live action by itself.

## Canonical contract

The following JSON object is the machine-readable contract. Field names,
operators, and values are stable. A candidate passes only when every threshold
passes in the same closed evidence window.

```json
{
  "contract": "mills_staff_s2_overseer_dry_run_soak",
  "version": 1,
  "thresholds": {
    "consecutive_green_dry_run_days": { "operator": ">=", "value": 7 },
    "false_positive_interventions": { "operator": "==", "value": 0 },
    "regression_rate": { "operator": "==", "value": 0 }
  }
}
```

`consecutive_green_dry_run_days` is the number of contiguous, complete UTC
days in which the action class stayed in dry-run and all required evidence was
green. `false_positive_interventions` is the count of reviewed dry-run
recommendations judged to be interventions the approved policy should not have
made. `regression_rate` is the number of confirmed regressions caused by the
candidate action class divided by its reviewed interventions in the same
window; a window with no reviewable intervention evidence is not green.

## Collect and close the evidence window

1. Keep the action class in dry-run for the entire window. Record every
   decision with `overseer.RecordDryRunDecision`; dry-run recommendations must
   remain auditable in the append-only `overseer.*` evidence.
2. Use seven consecutive **complete** UTC days. Do not use the current partial
   UTC day, stitch non-contiguous windows, or substitute an open-ended log
   search. Start a new window after a configuration, policy, or action-class
   change that could affect the outcome.
3. Obtain the closed report with
   `GET /api/mills/promotion-report?actor=overseer.&window=168h`, and retain
   the report, dry-run decision records, review results, and any incident
   escalations together. The report must have `window_end > window_start`,
   `zero_evidence == false`, no truncation, and no committed action for the
   candidate action class.
4. Review the evidence for the same window. A green day requires complete,
   readable evidence, no false-positive intervention, and no confirmed
   regression. Do not treat a missing review, unreadable store, malformed
   event, or incomplete day as green.

## Fail closed

Any missing, invalid, inconsistent, or out-of-window evidence is a failed
soak. So are an unexpected committed action, one false-positive intervention,
or any non-zero regression rate. Preserve the failed evidence and escalation,
leave `dry_run` enabled, and have the Mills operator/on-call investigate. Do
not compensate by changing an allow flag, accepting partial evidence, or
reclassifying an unknown outcome as green.

Restart the seven-day window after correcting a policy, configuration, or
implementation cause. If an external dependency incident affects the window,
follow the external-dependency recovery requirements in [MILLS.md](MILLS.md)
before resuming the soak.

## Promotion readiness

When the contract passes, the operator prepares the closed evidence bundle and
requests human approval for that specific action class. Approval must verify
the contract values, the complete seven-day UTC window, retained audit records,
and the proposed rollout/rollback plan. Promotion remains evidence-gated and
fail-closed: without explicit approval, the class stays in dry-run.
