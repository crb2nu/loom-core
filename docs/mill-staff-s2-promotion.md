# Mill Staff S2 overseer promotion gate

`overseer.PromotionGate` is the fail-closed consumer of the S2 soak-complete
artifact. It turns the human promotion decision described in
[mills-staff-s2-soak.md](mills-staff-s2-soak.md) into a runtime mode:
`active` only when the deposited evidence satisfies the S2 soak contract,
`dry-run` in every other case. The gate rereads the artifact on every
decision, so depositing or removing the file takes effect without a restart.

The gate is a library decision only. The foreman, groomer, and sentinel still
take their mode from `overseers.<agent>.dry_run` in the Mills policy
(`mills.DryRunOn`); nothing consults the gate live yet, so this contract
cannot flip an overseer out of dry-run by itself.

## Artifact

The artifact is the soak projection the shift report
(`GET /api/mills/shift-report`) emits: its `generated_at` observation time and
its `soak_progress` telemetry (`overseer.SoakProgress`). A verbatim shift
report is a valid artifact; so is this minimal object:

```json
{
  "generated_at": "2026-09-08T00:00:00Z",
  "soak_progress": {
    "soak_started_at": "2026-09-01T00:00:00Z",
    "soak_elapsed_seconds": 604800,
    "soak_divergences": 0
  }
}
```

`generated_at` and `soak_started_at` are required RFC 3339 timestamps.
`soak_elapsed_seconds` and `soak_divergences` are required non-negative
integers. Unknown fields are ignored for forward compatibility, including a
projected `soak_complete`: the gate never trusts a verdict written into the
file.

The operator's shift-report handler does not yet populate `soak_progress`
(`overseer.ObserveSoakProgress` has no production caller), so today the
minimal object is assembled from the closed window's evidence:
`soak_started_at` is the window start, `soak_elapsed_seconds` its length,
`soak_divergences` the reviewed divergence count, and `generated_at` the time
the window was closed.

## Decision

There is one definition of a complete soak, `overseer.SoakProgress.CompleteAt`,
shared with the `soak_complete` field that `shiftreport.Compose` derives. The
gate evaluates it at the artifact's own `generated_at`, so it agrees with the
report the evidence came from. Active mode requires all of:

- `soak_started_at` is set and not after `generated_at`;
- `soak_elapsed_seconds` is at least `S2SoakMinimumSeconds` (`604800`, the
  `168h` of `S2SoakMinimumDuration`); exactly `604800` passes;
- `soak_divergences` is exactly `0`.

Every other outcome is `dry-run`. The structured log line carries `mode`,
`artifact`, and a stable `reason`:

| Reason | Cause |
| --- | --- |
| `soak-complete artifact is missing` | no file at the configured path |
| `soak-complete artifact is unreadable` | the read failed for a reason other than absence, such as permissions |
| `soak-complete artifact is malformed` | invalid JSON; a missing or invalid `generated_at`, `soak_progress`, or `soak_started_at`; a negative counter |
| `soak-complete artifact soak start is after its generated_at` | future-dated telemetry |
| `soak-complete artifact covers less than 168 hours` | `soak_elapsed_seconds` below `604800` |
| `soak-complete artifact records policy divergences` | `soak_divergences` above `0` |
| `soak-complete artifact does not satisfy the S2 soak contract` | rejected by `CompleteAt` for a condition the classifier does not name |

## Operator procedure

1. Run the full checklist in
   [mill-staff-s2-soak-runbook.md](mill-staff-s2-soak-runbook.md). The gate
   verifies only the soak-progress predicate; the remaining checklist items
   (would-have-acted evidence, unreviewed errors, external-dependency
   recovery) stay a human review.
2. Depositing the artifact at the configured path is the explicit approval
   act required by [mills-staff-s2-soak.md](mills-staff-s2-soak.md). Deposit
   the evidence unmodified, and do not automate the deposit: a completed soak
   would otherwise promote itself without review.
3. Never bypass a rejection by setting active mode independently. Use the
   logged `reason` to correct the evidence or its permissions, and after a
   failed or incomplete soak produce a fresh artifact from a new closed
   window.
4. To withdraw a promotion, remove the artifact: the next decision returns to
   `dry-run`.
