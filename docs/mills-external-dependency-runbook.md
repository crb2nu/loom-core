# Mills failure-class retry telemetry

Mills attributes `retry_cost_usd` and `auto_requeues` to the closed failure
taxonomy in `pkg/mills/pipeline/failure_classifier.go`. Operators should use
the aggregate `total` values for compatibility and `by_class` to identify the
kind of failure consuming retry budget. Raw errors, signatures, and dependency
names are never telemetry classes.

## Classes and response

| `failure_class` | Interpretation | Operator response |
| --- | --- | --- |
| `transient` | A short-lived transport or service failure. | Watch the bounded retry. Investigate the dependency if cost or requeues remain elevated. |
| `transient_quota` | Provider throttling, quota exhaustion, or rate limiting. | Check provider quota and concurrency; wait for recovery rather than increasing retry pressure. |
| `infrastructure` | Runner, executor, network, or cluster infrastructure failure. | Inspect runner and cluster health, then requeue only after the fault clears. |
| `configuration` | Authentication, policy, or configuration failure; retrying unchanged work is terminal. | Correct configuration or credentials. Do not repeatedly requeue. |
| `code` | Repository-owned failure, or the fail-closed destination for an unknown/unrecognized class or external signature. | Review the original failure and classifier evidence. Add a bounded classifier rule before automating retries. |

An absent class means that class had no observations in the snapshot. Per-class
cost and requeue sums must equal `total.retry_cost_usd` and
`total.auto_requeues`. A mismatch indicates an instrumentation defect.

## Recognized external-dependency signatures

The classifier recognizes these observed signatures before applying its normal
bounded failure rules:

| Pattern ID | Required signature | Result |
| --- | --- | --- |
| `external_dependency.blob_storage.manifest_write` | A manifest/cache write failure containing a bounded blob-storage phrase such as `error writing manifest blob`, `registry cache export failed`, or `blob upload unknown`/`invalid` | `configuration`; stop retries and escalate to the registry/storage owner. |
| `external_dependency.gitlab.auth_failure` | A GitLab-scoped 401, `unauthorized`, `authentication failed`, `incorrect api`, or `invalid token` | `configuration`; repair the credential or access configuration before retrying. |
| `external_dependency.gitlab.rate_limit` | A GitLab CI/status request returning 429 or a bounded rate-limit phrase | `transient_quota`; honor backoff and wait for quota recovery. |
| `external_dependency.gitlab.service_unavailable` | A GitLab CI/status request returning a recognized 5xx/service-unavailable response | `transient`; honor the bounded retry and wait for service recovery. |
| `external_dependency.model_provider.judge_ungradeable_envelope` | A rubric judge response containing `could not be parsed into a score envelope`, `no parseable score envelope`, or `rubric judge: unparseable response` | `configuration`; stop respawning unchanged work and investigate the judge provider or response budget. |
| `external_dependency.clickhouse.merge_task` | `clickhouse` plus `merge task failed`, `merge task failure`, or `failed to execute merge task` | `configuration`; stop paid retries and follow the ClickHouse recovery runbook. |
| `external_dependency.longhorn.no_available_disk` | `longhorn` plus `no available disk(s)` | `configuration`; restore storage capacity before retrying. |
| `external_dependency.litellm.missing_api_key` | `litellm` plus `missing api key`, `api key is missing`, or `no api key` | `configuration`; repair the referenced secret/configuration. |

The source of truth for exact matching and retry semantics is
`pkg/mills/pipeline/failure_classifier.go` and the shared external incident
codes it invokes.

Any signature or class that is not recognized is attributed to `code`. This is
intentional fail-closed behavior: do not create a telemetry label from the raw
value and do not infer that an unknown external failure is safe to retry.

## Triage procedure

1. Compare the per-class values with the aggregate totals.
2. Find the dominant class and inspect the corresponding run logs for the
   bounded classifier record and external dependency ID.
3. For `transient` and `transient_quota`, confirm the retry ceiling and wait for
   dependency recovery. For `infrastructure` or `configuration`, repair the
   owning system before requeueing.
4. Treat `code` as requiring review. If it contains a recurring external
   signature, add a tested bounded classifier rule and update this runbook;
   never use the raw signature as a metric label.

Related detailed procedures are in
[`docs/runbooks/external-dependency-incidents.md`](runbooks/external-dependency-incidents.md).
