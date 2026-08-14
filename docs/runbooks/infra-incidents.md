# External Infrastructure Incident Runbook

Use this runbook for recurring dependency signatures that are owned outside the
repository. For every confirmed incident, record
`external_dependency_incident`, set
`disposition=wait_for_dependency_recovery`, apply the
`external-dependency-incident` label, and set `retry_allowed=false`. Operators
preserve evidence, make safe observations, escalate to the owner, and verify
recovery; they do not repair the external service.

Before classifying any signature, capture the UTC window, affected workload,
first meaningful error, and one independent corroborating signal. Redact
credentials, connection strings, and customer data. A branch-owned change to
SQL, manifests, secret references, role names, or provider configuration is a
repository defect, not an external dependency incident.

## ClickHouse Code 432 merge failures

### Detection signals

Look for ClickHouse errors containing `Code: 432` with a failed merge task,
repeated merge failures on a replica, growing merge queues, or alerts for disk,
inode, replication, or coordination health. Read-only examples:

```sh
kubectl -n <namespace> logs deploy/<clickhouse-workload> --since=30m
kubectl -n <namespace> get pods -l app.kubernetes.io/name=clickhouse
```

### External-dependency classification criteria

Classify when the Code 432 merge signature occurs in ClickHouse independently
of the candidate branch, affects unrelated writers or replicas, or is confirmed
by the ClickHouse owner. Do not classify a new invalid query, schema change, or
partition change introduced by the branch as external.

### Remediation owner and escalation

The ClickHouse/platform data owner owns diagnosis and repair of parts,
replication, storage capacity, merge pools, and coordination services. Escalate
with the sanitized error, table and replica identity, failed-part identifiers,
UTC window, and disk/inode observations through the data-platform incident
path.

### Safe operator actions

Pause affected write-heavy work only when the owner directs it. Inspect logs,
health, queues, and capacity read-only. Do not detach, drop, delete, rewrite,
or manually repair parts; do not force merges or alter ClickHouse settings.

### Recovery verification

Obtain owner confirmation, then verify replicas are healthy, queues drain,
capacity is adequate, and no new Code 432 event appears. Run one small approved
query or write and one affected job before resuming normal load.

## Longhorn replica scheduling failures

### Detection signals

Look for Longhorn conditions or events reporting unschedulable replicas,
insufficient eligible disks/nodes, degraded volumes, or repeated replica
rebuilds. Inspect only the affected namespace and resources:

```sh
kubectl -n <longhorn-namespace> get volumes,replicas
kubectl -n <workload-namespace> get events --sort-by=.lastTimestamp
```

### External-dependency classification criteria

Classify when a previously working volume cannot schedule replicas because of
shared Longhorn capacity, disk tags, node availability, anti-affinity, or
controller state, especially when unrelated workloads show the same symptom.
Do not classify a branch-owned PVC, storage-class, affinity, or resource change
as external.

### Remediation owner and escalation

The storage/platform owner owns node and disk eligibility, capacity, Longhorn
settings, replica rebuilds, and controller recovery. Escalate with the volume
identifier, namespace, condition/event text, affected nodes/disks, UTC window,
and current workload impact.

### Safe operator actions

Stop or hold workloads only if availability or data-integrity policy requires
it. Collect read-only volume, replica, node, and event state. Do not delete
replicas, force-detach volumes, change disk tags, or alter replica counts or
scheduling settings without the storage owner.

### Recovery verification

After owner confirmation, verify the volume is healthy, the intended replica
count is scheduled, rebuild activity has settled, and the workload can mount
and perform its approved health check. Confirm events stop reporting scheduling
failure before requeueing work.

## LiteLLM missing API keys

### Detection signals

Look for LiteLLM logs or responses stating `missing API key`, `API key is
missing`, or `no API key`, including failures shared by multiple callers of one
route or provider. Inspect sanitized configuration references only:

```sh
kubectl -n <namespace> logs deploy/<litellm-workload> --since=30m
kubectl -n <namespace> get deploy/<litellm-workload> -o yaml
```

### External-dependency classification criteria

Classify when a previously working LiteLLM route loses provider credentials
without a repository change, multiple callers fail on the same route/provider,
or the LiteLLM/secret owner confirms missing credential material. A changed
secret reference, unsupported model/provider, wrong gateway URL, or caller
authentication mistake is repository-owned.

### Remediation owner and escalation

The LiteLLM and secret-management owner owns credential restoration, rotation,
approved secret injection, and gateway reconciliation. Escalate the route,
model alias, provider, workload identity, UTC window, and sanitized error;
never include a key, header, Secret value, or key length.

### Safe operator actions

Hold requests for the affected route to avoid paid retries. Confirm only that
the expected Secret reference and workload configuration exist. Do not print
environment variables, substitute a personal key, bypass LiteLLM, or switch to
an unapproved provider.

### Recovery verification

After the owner restores the approved credential path, send one minimal request
through the same route and model alias. Verify a valid response, successful
provider initialization, and no new missing-key metric or log event before
running one affected stage.

## PostgreSQL missing roles

### Detection signals

Look for PostgreSQL errors such as `role \"<role>\" does not exist`, failed
role authentication, or denied operations for an expected approved identity.
Record the database service identity, failed operation, and sanitized error;
never record passwords, DSNs, certificates, or secret-bearing SQL.

```sh
kubectl -n <namespace> logs deploy/<workload> --since=30m
psql "$PGHOST" -U <approved-readonly-role> -d <database> -c '\du'
```

### External-dependency classification criteria

Classify when a previously approved role or grant changes outside the branch,
unrelated workloads fail with the same identity, or the database owner confirms
role drift. A branch-owned migration, connection role reference, privilege
requirement, or use of an unapproved role is repository-owned.

### Remediation owner and escalation

The PostgreSQL/database owner owns role creation, grants, authentication,
credential rotation, and emergency access. Escalate the endpoint/service
identity, approved role name where safe, failed operation, UTC window, and
sanitized evidence through the database incident path.

### Safe operator actions

Stop the affected bootstrap or migration and avoid autonomous retries. Perform
only approved read-only checks. Do not create or alter roles, grant broad
privileges, reset credentials, change authentication, or use a root credential
as a workaround.

### Recovery verification

Require owner confirmation that the intended least-privileged hierarchy and
grants are restored. Re-run the original operation with the approved identity
and verify it succeeds without a broad grant or root credential; only then may
the work be requeued.

## Incident closure

Attach the recovery probe and UTC result to the incident record. If the same
signature persists, keep the incident parked, update the evidence and
escalation, and do not broaden the retry loop. When no repository-owned action
remains, record: `external dependency incident; no actionable in-repo
follow-up`.
