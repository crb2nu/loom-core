# External Dependency Incident Runbook

Use this runbook for recurring failures in dependencies operated outside this
repository. Preserve the first sanitized error, UTC time window, affected
workload, cluster, and correlation or run ID. Redact credentials, connection
strings, and application data.

Commands in **Safe diagnostics** are read-only. Commands in **Operator
actions** can change live state and must be performed only by the named service
owner after the stated precondition is met. If ownership, service health, or
data integrity is uncertain, fail closed: pause the dependent automation and
escalate rather than retrying or improvising a repair.

## ClickHouse MergeTree: `Code: 432`

### Detection signature

Look for ClickHouse server or query errors containing `Code: 432` alongside a
MergeTree read or part failure, especially following a disk event or an
unexpected shutdown. Capture the table, replica, affected part names, and the
first failing query or job.

### Safe diagnostics

```sql
SELECT database, table, name, active, bytes_on_disk
FROM system.parts
WHERE database = '<database>' AND table = '<table>';

SELECT event_time, event_type, part_name, error
FROM system.part_log
WHERE database = '<database>' AND table = '<table>'
ORDER BY event_time DESC
LIMIT 50;

CHECK TABLE <database>.<table>;
```

Compare the affected replica with a known healthy replica and inspect
sanitized server logs. `CHECK TABLE` is an integrity check; it is not a repair.
Do not use `DROP`, `DETACH`, `ATTACH`, `ALTER ... DELETE`, forced merges, or
filesystem operations as diagnostics.

### Operator actions

Only after the ClickHouse owner has confirmed a healthy second copy or an
approved backup, quarantine or reattach the affected part through the database
change procedure. Fetch the part from a healthy replica or restore it from the
approved backup when that procedure requires it.

### Recovery verification

The owner confirms that replicas agree, background work is healthy, and the
affected table passes its approved integrity check. Re-run one representative
read and the affected job; no new `Code: 432` event may appear in the
observation window.

### Fail-closed rule

Never drop or delete a part, or detach a partition on a single-replica table,
until a second verified copy or restorable backup exists. Keep dependent writes
paused if replica state or recoverability is unknown.

### Escalation

Escalate to the ClickHouse/database platform owner with the sanitized error,
UTC window, table, replica, part names, and replica-comparison result. See the
related [ClickHouse merge-failure runbook](clickhouse-merge-failures.md).

## Langfuse Redis: `ECONNREFUSED`

### Detection signature

Look for `connect ECONNREFUSED <host>:<port>` in Langfuse web or worker logs,
often accompanied by restarts or failed trace ingestion. Record the Langfuse
component, Redis host and port (without credentials), affected period, and
whether traces are buffered or dropped; do not assume buffering is enabled.

### Safe diagnostics

```sh
kubectl -n <langfuse-namespace> get pods,endpoints,svc
kubectl -n <langfuse-namespace> get pods -o wide
kubectl -n <langfuse-namespace> get events --sort-by=.lastTimestamp
kubectl -n <langfuse-namespace> logs deploy/<langfuse-web-or-worker> --since=30m
kubectl -n <langfuse-namespace> describe networkpolicy
kubectl -n <langfuse-namespace> get deploy/<langfuse-web-or-worker> -o yaml
```

Verify Redis pod readiness, its Service endpoints, recent restarts, applicable
network policies, and the referenced Redis host/port configuration. The final
command may expose Secret *references*; do not print Secret values or complete
environment dumps.

### Operator actions

If the Redis owner confirms a crash loop or unavailable Redis instance, they
may restart or recover Redis through the approved platform procedure. After
Redis is ready and reachable, the Langfuse owner may rolling-restart affected
web and worker components. These are state-changing actions, not diagnostics.

### Recovery verification

Confirm that Redis endpoints are ready, Langfuse components remain ready after
the restart, and a fresh trace/export succeeds without `ECONNREFUSED`. Record
explicitly whether events during the outage were buffered and later ingested or
dropped; this depends on the deployed Langfuse configuration.

### Fail-closed rule

If tracing is mandatory for a pipeline, pause that pipeline rather than running
blind. Do not disable tracing merely to hide the error or claim recovery until
the buffering-versus-drop outcome is known.

### Escalation

Escalate to the observability/Langfuse owner with the sanitized error, Redis
endpoint identity, readiness and endpoint observations, and trace-loss status.

## Longhorn replica scheduling failure

### Detection signature

Look for a Longhorn volume stuck `Degraded`, unschedulable replica resources,
or events such as `unable to schedule a replica` and `failed to schedule
replica`. Common causes include insufficient eligible nodes, taints,
anti-affinity constraints, disk pressure, or insufficient capacity.

### Safe diagnostics

```sh
kubectl -n <longhorn-namespace> get volumes.longhorn.io,replicas.longhorn.io
kubectl -n <longhorn-namespace> describe volume.longhorn.io/<volume>
kubectl get nodes
kubectl describe node <candidate-node>
kubectl -n <workload-namespace> get events --sort-by=.lastTimestamp
```

Inspect the affected volume and replicas, node conditions and taints, and the
workload events. These commands only read state; do not delete replicas,
force-detach volumes, or change scheduling settings during diagnosis.

### Operator actions

After the storage owner identifies the constraint, free approved disposable
disk capacity, correct platform-owned scheduling constraints, or add/relocate
eligible capacity. Reducing or relocating replica count requires the volume's
data-safety procedure and an approved change.

### Recovery verification

Verify that the volume is `Healthy`, its intended replicas are scheduled and
healthy, the relevant nodes have no disk-pressure condition, and one affected
workload mounts and completes its approved read/write health check.

### Fail-closed rule

Never delete the last healthy replica. Require at least N-1 healthy replicas
before evicting one from an N-replica volume; otherwise keep the workload
paused and escalate.

### Escalation

Escalate to the Longhorn/storage platform owner with the volume and PVC,
requested replica count, scheduling events, eligible-node evidence, and data
criticality. See the related [Longhorn disk-exhaustion runbook](longhorn-disk-exhaustion.md).

## GitLab agent: `Unauthenticated`

### Detection signature

Look for gRPC `Unauthenticated` errors in `kas` or `agentk` logs and for the
agent appearing offline in GitLab. Capture the GitLab host, agent identity,
project, UTC window, request correlation ID, and referenced Secret name/key;
never record token values.

### Safe diagnostics

```sh
kubectl -n <agent-namespace> get pods,deploy,secret
kubectl -n <agent-namespace> logs deploy/<agentk-deployment> --since=30m
kubectl -n <gitlab-namespace> logs deploy/<kas-deployment> --since=30m
kubectl -n <agent-namespace> get deploy/<agentk-deployment> -o yaml
kubectl -n <agent-namespace> describe networkpolicy
```

Check that the referenced token Secret exists without reading it, determine
whether its approved rotation/expiry state changed, and verify agentk-to-kas
reachability and cluster egress using approved read-only platform checks.

### Operator actions

If the GitLab/identity owner confirms the token is expired, revoked, or invalid,
rotate or re-register it through the documented GitLab agent registration and
secret-management flow. Reconcile the agent deployment only after its intended
Secret reference has been confirmed. Do not retry with stale credentials or
place a token in Git, logs, or incident notes.

### Recovery verification

Confirm that agentk connects to kas without a fresh `Unauthenticated` error,
the agent is online in GitLab, and a least-privileged read-only GitLab probe
from the same runtime identity succeeds. Then run one affected stage.

### Fail-closed rule

GitLab-driven cluster operations are unavailable while agents are
unauthenticated. Pause dependent CI/CD deployments until the agent and the
read-only probe both recover; do not bypass the agent with ad hoc credentials.

### Escalation

Escalate to the GitLab/identity platform owner with the sanitized gRPC error,
agent and project identity, Secret reference, reachability evidence, and
correlation ID. See the related [GitLab-agent unauthenticated runbook](gitlab-agent-unauthenticated.md).
