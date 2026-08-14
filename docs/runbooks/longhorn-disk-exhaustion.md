# Longhorn Disk Exhaustion

Use this runbook for classifier pattern
`external_dependency.longhorn.no_available_disk` when Longhorn cannot place a
volume replica. Do not delete replicas, snapshots, or volume data as a capacity
shortcut.

## Detection

The classifier requires a Longhorn-scoped error containing `no available disk`
or `no available disks`. Preserve the event timestamp, cluster, namespace,
PVC/PV and Longhorn volume, requested replica count and size, candidate nodes,
and the scheduling event.

Safe diagnostics are read-only: inspect volume and replica health, node/disk
schedulability, Kubernetes disk-pressure conditions, actual free bytes and
inodes, Longhorn storage reservations and minimum-free-space settings, and
replica anti-affinity constraints.

## Classification

Classify as an external Longhorn incident when existing platform capacity or
placement policy prevents scheduling independently of the branch, multiple
unrelated volumes are affected, or the storage owner confirms exhausted or
unschedulable disks.

Likely false positives are a branch-owned PVC size increase, replica-count or
node-selector change, a deliberately disabled disk, an impossible affinity
rule introduced by the deployment, or a generic Kubernetes ephemeral-storage
failure. Compare the requested volume and workload manifests with the base
revision before assigning external ownership.

## Operator Action

1. Stop repeated rescheduling and restarts; they do not create capacity and can
   add attachment churn.
2. Escalate to the storage owner with volume identity, placement constraints,
   requested capacity, and sanitized events. Keep the affected workload scaled
   or paused according to its data-safety procedure.
3. The storage owner restores headroom or schedulability through the approved
   capacity process: adding capacity, safely reclaiming known disposable data,
   or correcting platform-owned placement/reservation settings. Mills must not
   delete Longhorn replicas, snapshots, or unknown files.
4. Confirm the volume is healthy, the required number of replicas are placed,
   disks remain schedulable with policy-compliant free-space headroom, and the
   node has no disk-pressure condition.
5. Attach or schedule one affected workload, verify read/write health at the
   application layer, and observe that no new disk-availability event appears
   before resuming normal automation.

If the only apparent recovery is a reduced replica count or unexplained data
deletion, treat recovery as unverified and keep the incident open.
