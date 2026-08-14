# HUD Fleet-Roll Unavailability

Use this runbook when the shared HUD returns a 502 or blank page during, or
for more than five minutes after, a loom-hub fleet image roll. Classify the
first meaningful signal and fail closed: do not restart or alter the deployment
while gathering evidence.

The HUD is `deployment/mobile-hud` in namespace `loom-hub`. Its single replica
uses `Recreate` because its workspace volume is ReadWriteOnce, so a brief
unavailable interval is expected while the old pod releases the volume and the
replacement becomes Ready.

## HUD 502 or Blank Page Within Minutes of a Merged MR

### Detection signals

A loom-core or custom-server image bump may have just landed in a Flux commit
named `chore(loom-hub): auto-update loom-core image`. A mobile-hud pod that is
seconds to minutes old supports a transient-roll diagnosis.

```bash
kubectl -n loom-hub get pods -l app=mobile-hud --request-timeout=30s
kubectl -n loom-hub describe deployment/mobile-hud --request-timeout=30s
kubectl -n loom-hub logs deployment/mobile-hud --since=30m --request-timeout=30s
```

### External-dependency classification criteria

Treat the short outage as expected `Recreate` downtime only when the old pod
has terminated, a replacement is progressing, and no readiness or image error
appears in the bounded logs. A branch-owned change to the failing surface is a
repository defect, never an external incident. Use the external-dependency
path only when the earliest evidence instead identifies a shared dependency
outside the branch, such as registry or cluster control-plane availability.

### Remediation owner and escalation

1. Keep the platform operator for the loom-hub fleet as the incident owner.
2. Escalate to the image, registry, or cluster owner when evidence identifies
   that shared dependency rather than the HUD deployment.
3. Record the deployment image, pod age, first meaningful error, and related
   Flux commit before declaring an incident.

### Safe operator actions

1. Wait through the normal 30--60 second `Recreate` and readiness interval.
2. Observe the replacement with the namespaced pod query above.
3. Check bounded rollout progress without changing state:

   ```bash
   kubectl -n loom-hub rollout status deployment/mobile-hud --timeout=2m \
     --request-timeout=30s
   ```

4. Do not restart, delete, scale, patch, or reconcile the deployment mid-roll.

### Recovery verification

1. Confirm rollout completion with the bounded `rollout status` command.
2. Confirm the mobile-hud pod is Ready with zero restarts using the namespaced
   pod query above.
3. Confirm `https://hud.flexinfer.ai` answers HTTP 200 through the normal
   monitoring or browser check.

## HUD Unavailable Beyond Five Minutes After a Roll Began

### Detection signals

A `CrashLoopBackOff`, `ImagePullBackOff`, or readiness failure after five
minutes is a rollout failure, not transient `Recreate` downtime. Inspect the
deployment and its bounded logs; do not retrieve Secrets or other credentials.

```bash
kubectl -n loom-hub get pods -l app=mobile-hud --request-timeout=30s
kubectl -n loom-hub describe deployment/mobile-hud --request-timeout=30s
kubectl -n loom-hub logs deployment/mobile-hud --since=30m --request-timeout=30s
kubectl -n loom-hub rollout status deployment/mobile-hud --timeout=2m \
  --request-timeout=30s
```

### External-dependency classification criteria

Classify from the earliest useful evidence. An image-pull failure can be an
external registry incident only when the branch did not introduce the image
reference and independent evidence shows the registry unavailable. A failing
image, readiness configuration, or HUD behavior introduced by the branch is a
repository defect, never an external incident. Do not use a later 502 to mask
an earlier repository-owned failure.

### Remediation owner and escalation

1. Keep the platform operator for the loom-hub fleet as the incident owner.
2. Escalate a branch-owned image, manifest, or application failure to its
   repository owner with the bounded logs and deployment description.
3. Escalate a verified shared registry or Kubernetes failure to that service
   owner and link its incident evidence.
4. Compare the deployed image against the newest Flux image-automation commit;
   image bumps normally batch on a 30-minute cadence.

### Safe operator actions

1. Preserve the failing pod and gather only the namespaced, read-only evidence
   above.
2. Do not restart, delete, scale, patch, or reconcile `deployment/mobile-hud`
   while diagnosis is in progress.
3. If an urgent image correction is required, hand off the evidence to the
   platform operator for the approved GitOps recovery; do not perform an
   imperative write from this runbook.

### Recovery verification

1. Confirm the bounded rollout-status command above reports a successful
   rollout.
2. Confirm the replacement pod is Ready with zero restarts and bounded logs
   show no recurring startup failure.
3. Confirm `https://hud.flexinfer.ai` answers HTTP 200.

## Incident closure

Record the following evidence in the incident or merge-request discussion.

```text
Disposition: <transient-recreate|repository-defect|external-dependency>
Deployment: <namespace/name>
Image: <image-reference>
First signal: <signal>
Evidence: <read-only-command-output-or-link>
Recovery: <http-200|not-recovered>
Owner: <team-or-operator>
```

For cross-pipeline classification and escalation rules, see
`docs/mills-incident-runbook.md`.
