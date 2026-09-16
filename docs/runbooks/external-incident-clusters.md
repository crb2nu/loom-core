# Recurring External Incident Clusters

Use this reference to recognize recurring infrastructure failures in Mills
evidence and route them to the system that owns recovery. The council classifier
emits `external_dependency_incident` with the dependency value shown below. Do
not turn an unrecognized variation into a new label: preserve the evidence and
escalate it for human triage.

## ClickHouse merge failure

- **Recognizable log signature:** `ClickHouse exception Code: 432.
  DB::Exception: Cannot merge parts because a merge with the same resulting part
  is already running` (ClickHouse/MergeTree evidence with Code 432 and a merge).
- **Owning system:** ClickHouse database platform.
- **Mills classifier label:** `external_dependency_incident` with dependency
  `clickhouse`; the pipeline pattern ID is
  `external_dependency.clickhouse.merge_task`.

## Longhorn disk or replica scheduling failure

- **Recognizable log signature:** `Longhorn volume degraded: failed to schedule
  replica: no available disk candidates to create a new replica`.
- **Owning system:** Longhorn storage platform and the node/disk capacity it
  manages.
- **Mills classifier label:** `external_dependency_incident` with dependency
  `longhorn`; the pipeline pattern ID is
  `external_dependency.longhorn.no_available_disk`.

## GitLab agent authentication failure

- **Recognizable log signature:** `rpc error: code = Unauthenticated desc =
  GitLab Agent is unauthenticated` from `gitlab-agent`, `gitlab agent`, or
  `agentk`.
- **Owning system:** GitLab Agent and its GitLab identity or credential
  configuration.
- **Mills classifier label:** `external_dependency_incident` with dependency
  `gitlab_agent` for CI dependency evidence; the authentication pipeline pattern
  ID is `external_dependency.gitlab.auth_failure`.

## PostgreSQL root-role failure

- **Recognizable log signature:** `FATAL: role "root" does not exist` from a
  PostgreSQL or `psql`-scoped service.
- **Owning system:** PostgreSQL database and role-provisioning administration.
- **Mills classifier label:** `external_dependency_incident` with dependency
  `postgres`.

## LiteLLM missing API key

- **Recognizable log signature:** `LiteLLM proxy authentication failed: missing
  API key` (including `API key is missing` or `LITELLM_API_KEY not set`).
- **Owning system:** LiteLLM gateway and the secret or configuration supplying
  its provider key.
- **Mills classifier label:** `external_dependency_incident` with dependency
  `litellm`; the pipeline pattern ID is
  `external_dependency.litellm.missing_api_key`.

## Related procedures

For recovery steps and safety constraints, use the dedicated
[external dependency incident runbook](external-dependency-incidents.md) and
the linked per-dependency procedures.
