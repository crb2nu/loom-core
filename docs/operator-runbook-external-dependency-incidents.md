# Operator Runbook: External-Dependency Classifier Incidents

Use this runbook when `ClassifyFailureRecord` emits an
`external_dependency_id`. Preserve the first matching error, run ID, commit,
stage, and UTC timestamp before remediation. Confirm that the failure is
outside the branch diff; if the branch changed the affected endpoint,
credential wiring, storage configuration, or request, treat it as a
repository/configuration regression instead.

The pattern ID is the link from persisted Mills classification to the
appropriate procedure below. Retry only where the pattern explicitly permits
a bounded retry. A successful retry without dependency-health evidence is not
proof that an incident is resolved.

## `external_dependency_incident` policy

This classification is a **non-action** policy for the external system:
classify the signal, retain its evidence, park or bound retries as the
classifier permits, and optionally notify the owning team. It never authorizes
Mills or a repository change to remediate the external service.

Do not autonomously restart or fail over a dependency; change its capacity,
replica placement, credentials, data, parts, or configuration; or alter
repository code merely to suppress a valid signal. An operator may resume the
affected work only after the owner supplies the recovery evidence named for
the matched cluster and a fresh bounded probe succeeds.

Repository-owned follow-ups remain valid when they improve how loom-core
observes or handles the incident without modifying the external system:

- classifiers;
- telemetry;
- retry policy;
- configuration;
- tests; and
- documentation.

## Recurring workspace-signal clusters

The following clusters are owned outside loom-core. Use the exact signature
when recording or routing an incident; do not treat a similarly worded
repository error as proof of an external incident.

### ClickHouse MergeTree merge/mutation/part pressure

- **Owning system:** the external ClickHouse/database platform, including its
  MergeTree merge queues, mutations, replicas, parts, and storage capacity.
- **Signature and escalation:** record the ClickHouse/MergeTree error (such as
  a merge task failure, mutation failure, `Code: 432`, or part-pressure
  signal), affected table/partition, UTC window, and job/query identity.
  Escalate through the database-platform incident path to the ClickHouse
  owner.
- **Recovery evidence:** the owner confirms healthy replicas and storage,
  cleared or safely recovered merge/mutation work, and no active part pressure;
  then one fresh affected query or write completes without the signature.
- **Prohibited actions:** do not restart ClickHouse, kill merges, detach or
  mutate parts, change table settings, or make a branch change solely to hide
  the signal.

### Langfuse Redis `ECONNREFUSED`

- **Owning system:** the external Langfuse tracing backend and its Redis
  service, networking, credentials, and availability.
- **Signature and escalation:** preserve the Langfuse component, Redis target,
  sanitized `ECONNREFUSED` error, UTC window, and affected trace/export run.
  Escalate through the observability-platform incident path to the Langfuse
  owner, who coordinates Redis recovery.
- **Recovery evidence:** the owner confirms Redis reachability and Langfuse
  backend health from the same network boundary; a fresh trace/export probe
  completes without `ECONNREFUSED`.
- **Prohibited actions:** do not restart or fail over Redis or Langfuse, change
  Redis data or credentials, disable tracing to conceal the failure, or modify
  repository code to suppress the signal.

### Longhorn replica scheduling

- **Owning system:** the external Longhorn/storage platform, including node and
  disk eligibility, capacity, anti-affinity, volume health, and replica
  placement.
- **Signature and escalation:** preserve the Longhorn volume/PVC, requested
  replica count, candidate-node evidence, UTC window, and a message such as
  `unable to schedule a replica` or `failed to schedule replica`. Escalate to
  the storage/platform owner through the shared infrastructure incident path.
- **Recovery evidence:** the owner confirms adequate schedulable capacity and
  eligible nodes, intended replicas are healthy and placed, and one affected
  workload schedules or attaches without another replica-scheduling error.
- **Prohibited actions:** do not restart workloads to force placement, delete
  replicas or snapshots, alter disk/node eligibility or capacity, expand a
  volume, or make a repository change solely to hide the signal.

## Pattern procedures

### `external_dependency.blob_storage.manifest_write`

- **Detection:** Registry cache or manifest writes report `error writing
  manifest blob`, `/manifests/buildcache`, `registry cache export failed`,
  `blob upload unknown`, or `blob upload invalid`.
- **Remediation:** Stop cache/manifest pushes for the affected registry and
  escalate to the registry/storage owner. Check registry capacity, object-store
  health, and upload consistency. Do not delete cache or registry data from a
  Mills retry.
- **Verification:** The owner confirms storage health and a fresh, disposable
  manifest/cache write and read succeeds. Then run one affected pipeline and
  confirm no blob-write signature appears.

### `external_dependency.gitlab.auth_failure`

- **Detection:** A GitLab request reports 401/unauthorized, authentication
  failure, invalid token, or an unauthenticated GitLab agent.
- **Remediation:** Stop retries. Confirm the intended GitLab identity and token
  source with the credential/platform owner, then restore or rotate the
  credential through the approved secret-management path. Never put tokens in
  incident notes or logs.
- **Verification:** From the same runtime identity, perform a read-only GitLab
  API probe and confirm the required project access. For an agent failure,
  confirm agent authentication and connectivity, then run one affected stage.

### `external_dependency.gitlab.rate_limit`

- **Detection:** A GitLab-scoped error reports HTTP 429, `too many requests`,
  or a rate-limit message.
- **Remediation:** Honor `Retry-After` or the platform owner's reset time,
  suppress concurrent polling, and use only the configured bounded/free-retry
  path. Escalate sustained or cross-project throttling to the GitLab owner.
- **Verification:** After the reset window, a read-only request from the same
  identity succeeds and rate-limit headers show capacity. Run one affected
  stage and confirm it completes without another 429.

### `external_dependency.gitlab.service_unavailable`

- **Detection:** A GitLab-scoped request reports HTTP 500/502/503/504, bad
  gateway, service unavailable, gateway timeout, or an explicit GitLab CI
  pipeline failure while polling.
- **Remediation:** Pause repeated CI polling and check the GitLab/platform
  incident channel. Preserve the affected endpoint and correlation ID where
  available. Wait for owner recovery evidence; use only bounded backoff.
- **Verification:** GitLab health and the exact read-only API path recover from
  the same network boundary. Run one fresh affected stage and confirm no 5xx or
  pipeline-unavailable classification recurs.

### `external_dependency.model_provider.judge_ungradeable_envelope`

- **Detection:** The rubric judge reports that its response could not be parsed
  into a score envelope, that no parseable score envelope exists, or that the
  response is unparseable (commonly with empty `raw=""`).
- **Remediation:** Stop respawning completed upstream work. Preserve sanitized
  provider response metadata and escalate to the model-provider owner. Check
  model availability, output/token limits, and the judge request path.
- **Verification:** A fresh judge probe using the same rubric/model returns a
  valid complete score envelope, then the affected gate succeeds once without
  an empty or unparseable response.

### `external_dependency.clickhouse.merge_task`

- **Detection:** A ClickHouse-scoped error contains `merge task failed`,
  `merge task failure`, or `failed to execute merge task`.
- **Remediation:** Pause affected writes/jobs and escalate to the ClickHouse
  owner. Inspect replica health, disk/capacity, failed parts, and server logs;
  do not mutate or detach parts through Mills.
- **Verification:** The owner reports healthy replicas and background merges,
  the failed merge is cleared or safely recovered, and a fresh affected query
  or write completes without a merge-task error.

### `external_dependency.longhorn.no_available_disk`

- **Detection:** A Longhorn-scoped error reports `no available disk` or `no
  available disks`.
- **Remediation:** Stop rescheduling/restarting the workload and escalate to
  the storage owner. Inspect schedulable nodes, disk pressure, reservations,
  replica placement, and volume health. Capacity or placement changes require
  the normal storage change process.
- **Verification:** Longhorn reports a healthy volume with sufficient
  schedulable capacity and replicas placed as intended. Reattach or schedule
  one affected workload and confirm no disk-availability event recurs.

### `external_dependency.openrouter.credits_exhausted`

- **Detection:** OpenRouter's HTTP 402 wording `requires more credits` reaches
  Mills through the litellm error chain (`OpenrouterException … "This request
  requires more credits, or fewer max_tokens"`). First live cluster 2026-08-06:
  three unrelated runs 402'd at the research stage during one balance dip.
- **Remediation:** Stop manual requeues — retries against an empty balance only
  burn attempts. Top up credits on the OpenRouter dashboard (operator spend
  decision) or reroute the affected litellm model group through gitops; note
  `or/kimi-k3` has no fallback group, so a 402 is terminal for it.
- **Verification:** One canary run through the same route after the balance
  changes; live evidence shows recovery within the hour with no other change.
  See `docs/runbooks/openrouter-credits-exhausted.md`.

### `external_dependency.litellm.missing_api_key`

- **Detection:** A LiteLLM-scoped error reports `missing API key`, `API key is
  missing`, or `no API key`.
- **Remediation:** Stop requests and identify the selected provider and secret
  reference. Have the LiteLLM/secret owner restore the key through the approved
  secret path; never paste it into logs, config diffs, or incident records.
- **Verification:** Confirm the runtime sees the expected secret reference
  without exposing its value, then make a minimal authenticated request through
  the same LiteLLM route and run one affected stage.

## Closeout

Record the pattern ID, dependency owner, remediation reference, fresh
verification evidence, and bounded retry result. If no repository-owned
classifier, retry-policy, telemetry, configuration, test, or documentation
change is needed, use the disposition
`external dependency incident; no actionable in-repo follow-up`.

See also `docs/runbook-external-dependency-incidents.md` for parking, dwell,
threshold, and manual-override mechanics.
