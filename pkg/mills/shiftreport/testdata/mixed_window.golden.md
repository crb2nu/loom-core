# Mills shift report — 2026-08-25 16:00 UTC

The floor wove 3 bolts and struck 1 spark over the last 24 hours.
Pattern go-rest-service stamped twice — 1 merge, 1 escalation.
1 run needed extra passes (worst: BL-2 at 3 attempts).
Busiest hour 14:00–15:00 — 2 departures.
The shift burned $2.50 of pipeline fuel.
Finishing: 1 of 3 bolts live in the running operator (build 2d5e44b5); 1 pending rollout, 1 unknown.
KPI movement: escalation 20.0% → 10.0%; test errors 10.0% → 5.0%; cost/merge $3.00 → $2.00; merges 2 → 4; regressions 1 → 0.
1 of 3 bolts graded this shift; 14-day taste coverage 70.0% (gate 60.0%), ranker armed.

## Throughput guardrail — NOT BREACHED

All reported throughput signals are within policy thresholds.

## Bolts
| when (UTC) | item | change | eval | cost |
|---|---|---:|---:|---:|
| 13:00 | BL-5 | 0 files +0/-0 | — | $0.00 |
| 14:00 | Add ledger (!42) | 3 files +20/-4 | 0.91 | $1.25 |
| 15:00 | BL-3 | 2 files +5/-1 | — | $0.50 |

## Sparks
| when (UTC) | item | class | signature | week occurrences |
|---|---|---|---|---:|
| 14:30 | BL-2 | code | sig-1 | 2 |

## Taste
| graded this shift | bolts this shift | coverage 14d | gate | ranker | last grade |
|---:|---:|---:|---:|---|---|
| 1 | 3 | 70.0% | 60.0% | armed | 2026-08-25T15:00:00Z |
