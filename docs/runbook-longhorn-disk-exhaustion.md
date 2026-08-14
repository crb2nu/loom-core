# Longhorn Disk Exhaustion

Use this runbook when a Longhorn-scoped error says `no available disk` or `no
available disks`. Preserve the UTC window, cluster and namespace, PVC/PV and
Longhorn volume, requested size and replica count, candidate nodes, and the
scheduling event.

## Classification

This is an external dependency incident when platform capacity or placement
policy blocks a volume independently of the branch, unrelated volumes are also
affected, or the storage owner confirms exhausted or unschedulable disks. A
branch-owned PVC-size, replica-count, node-selector, or affinity change is a
repository/configuration issue. Compare the requested workload manifest with
the base revision before assigning external ownership.

## Fail closed and escalate

Stop repeated scheduling and restart attempts; they do not create capacity and
can add attachment churn. Keep the workload paused or scaled according to its
data-safety procedure. Use read-only diagnostics only: volume/replica health,
node and disk schedulability, DiskPressure, free bytes/inodes, reservations,
minimum-free-space policy, and anti-affinity constraints.

Do not delete replicas, snapshots, volumes, or unknown files; do not expand,
move, or repair storage from this workflow. Escalate the sanitized evidence to
the storage/Kubernetes platform owner, who owns capacity, placement, and volume
repair; the workload owner owns application integrity. Record
`external_dependency_incident`, `disposition=wait_for_dependency_recovery`,
`retry_allowed=false`, and `external-dependency-incident`. With no local
follow-up, record `external dependency incident; no actionable in-repo follow-up`.

## Verify recovery

Require fresh evidence of a healthy volume, the required replica count,
policy-compliant free-space headroom, schedulable disks, and no node disk
pressure. Schedule or attach one affected workload, verify an application-level
read/write integrity check, and confirm no new disk-availability event. A
reduced replica count or unexplained deletion is not verified recovery.
