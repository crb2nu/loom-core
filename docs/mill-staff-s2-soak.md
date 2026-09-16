# Mill Staff S2 overseer soak exit criteria

This document is the promotion contract for Mill Staff S2. Promotion remains
blocked unless a checker accepts one complete evidence bundle. A checker must
fail closed: a missing field, wrong type, invalid value, unreadable source,
incomplete window, or incomparable baseline returns `promotable: false`.

The predicate is deliberately about elapsed hours, not calendar days. A soak
must last at least `168` hours, including across daylight-saving transitions.

## Evidence bundle

The evaluator accepts one JSON object with a `soak` and a `baseline` snapshot.
Every field below is required. Counts and rate components are JSON integers;
they must be non-negative. `duration_hours` must be a JSON number, must be
finite, and must be at least zero. `start` and `end` are RFC 3339 UTC
timestamps (`Z` suffix); `end - start` must equal `duration_hours` exactly.

```json
{
  "schema_version": "mill-staff-s2-soak/v1",
  "soak": {
    "start": "2026-08-03T00:00:00Z",
    "end": "2026-08-10T00:00:00Z",
    "duration_hours": 168,
    "population": "overseer-dry-run/escalation/v1",
    "overseer_attributed_escalations": 0,
    "escalation_rate": { "numerator": 0, "denominator": 240 }
  },
  "baseline": {
    "start": "2026-07-20T00:00:00Z",
    "end": "2026-07-27T00:00:00Z",
    "duration_hours": 168,
    "population": "overseer-dry-run/escalation/v1",
    "overseer_attributed_escalations": 3,
    "escalation_rate": { "numerator": 3, "denominator": 240 }
  }
}
```

`population` is the comparability key. It pins the actor family, dry-run mode,
event classification, and denominator definition. The two values must match
byte-for-byte. Baseline collection happens before the soak begins and must use
the same versioned population; a baseline from another actor family, mode,
classification, or denominator is not comparable.

An *overseer-attributed escalation* is one durable audit event whose `actor`
has the exact `overseer.` prefix and whose `kind` identifies an escalation
action in the versioned population. This follows the audit contract: an
overseer action is stored as `overseer.<agent>.<action>` and a dry-run decision
has the `.dryrun` suffix. Observations, events from `overseerish`, and events
without a durable audit row are never counted as evidence. The snapshot
producer must retain the matching event identifiers (or a digest of their
ordered IDs) with the bundle so a reviewer can reproduce the count.

`escalation_rate` is an exact unitless ratio, not a rounded percentage:

```
rate = numerator / denominator
```

The denominator is the number of eligible escalation opportunities in the
same population and window. It must be positive. A zero denominator makes the
rate undefined, including for a baseline with zero opportunities, and blocks
promotion. An implementation must not substitute `0`, `NaN`, or infinity. In
each snapshot, `escalation_rate.numerator` must equal
`overseer_attributed_escalations`; a mismatch is internally inconsistent
evidence and blocks promotion.

## Machine-checkable predicate

Accept a bundle only when all structural rules above hold and all of these
conditions are true:

```text
soak.duration_hours >= 168
soak.overseer_attributed_escalations == 0
soak.escalation_rate.numerator * baseline.escalation_rate.denominator
  <= baseline.escalation_rate.numerator * soak.escalation_rate.denominator
```

Compare rates by cross multiplication using an integer type large enough not
to overflow; do not compare rounded floating-point values. Equality is a pass.
The checker must emit one stable result object:

```json
{
  "promotable": true,
  "fail_closed": false,
  "failure_reasons": []
}
```

On every rejection, `promotable` is `false`, `fail_closed` is `true`, and
`failure_reasons` contains machine-stable reason codes. Recommended codes are
`missing_field`, `invalid_type`, `invalid_duration`, `incomplete_window`,
`inconsistent_evidence`, `incomparable_baseline`, `undefined_rate`, `duration_too_short`,
`overseer_escalations_present`, and `escalation_rate_regressed`.

## Required fixture evaluation

The following table is the minimum conformance suite for a checker. Start from
the JSON bundle above; each row changes only the named value unless stated
otherwise. `PASS` means the result object above; all other outcomes must be a
fail-closed rejection with the indicated reason.

| Fixture | Change | Expected result |
| --- | --- | --- |
| exact-duration | none (`duration_hours: 168`) | PASS |
| duration-under | `soak.duration_hours: 167.999999` and matching timestamps | `duration_too_short` |
| zero-escalations | none (`overseer_attributed_escalations: 0`) | PASS |
| one-escalation | `soak.overseer_attributed_escalations: 1` | `overseer_escalations_present` |
| inconsistent-rate-count | set `soak.escalation_rate.numerator: 1` | `inconsistent_evidence` |
| equal-rate | soak ratio `3/240`, baseline ratio `3/240` | PASS |
| rate-epsilon-above | soak ratio `3000001/240000000`, baseline ratio `3/240` | `escalation_rate_regressed` |
| missing-field | remove `baseline.escalation_rate` | `missing_field` |
| nonnumeric-rate | set `soak.escalation_rate.numerator: "0"` | `invalid_type` |
| negative-rate | set `soak.escalation_rate.denominator: -1` | `undefined_rate` |
| malformed-duration | set `soak.duration_hours: "168h"` | `invalid_type` |
| negative-duration | set `soak.duration_hours: -1` | `invalid_duration` |
| baseline-zero-denominator | set `baseline.escalation_rate.denominator: 0` | `undefined_rate` |
| incomparable-population | set `baseline.population: "overseer-live/escalation/v1"` | `incomparable_baseline` |

The isolation cases above intentionally keep the other two exit criteria
passing. A test may report additional structural reasons only when its fixture
also violates another structural rule.

## Operator procedure

1. Capture and retain a baseline bundle before enabling the dry-run soak.
2. At the end of a closed window, produce the soak snapshot from durable audit
   rows and the same denominator source, then evaluate the bundle.
3. Promote only on `promotable: true`. On any other result, keep dry-run
   enabled, preserve the source evidence and verdict, and escalate the reason
   to the Mills operator/on-call.
4. After repairing evidence collection or the underlying issue, collect a new
   complete soak window. Do not repair a failed bundle by changing its window,
   population, or denominator after evaluation.
