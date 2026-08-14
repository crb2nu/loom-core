# Loom Mills — Operator Runbook

Day-2 procedures for `loom-mills-operator`. For architecture and policy reference, see `docs/MILLS.md`.

All `kubectl` commands assume `KUBECONFIG=~/workspace/platform/gitops/.kube/k3s.yaml` (or `kc-k3s` alias). Mac CLI examples use `LOOM_MILLS_OPERATOR_URL` (default `https://mills.flexinfer.ai`) for the REST + MCP listener; mutation examples also require an operator admin bearer token. The public ingress has a separate Cloudflare Access edge gate, authenticated with `LOOM_MILLS_CF_ACCESS_ID` / `LOOM_MILLS_CF_ACCESS_SECRET` (or the generic `CF_ACCESS_CLIENT_ID` / `CF_ACCESS_CLIENT_SECRET` fallback). The operator runs in namespace `loom-mills`. Lifecycle probes and Prometheus metrics live on a separate `:9090` listener and are not assumed to be exposed at the public REST URL.

The `loom` client and bundled status snapshot resolve both auth layers automatically. Literal `curl` examples against the public URL assume equivalent `Authorization` and `CF-Access-*` headers are supplied by the operator; they are omitted below for readability.

## Quick status

```bash
loom mills status                                           # one-liner from Mac
kubectl get deploy,po,pvc -n loom-mills                     # cluster snapshot
kubectl logs -n loom-mills deploy/loom-mills-operator --tail=200
curl -sf $LOOM_MILLS_OPERATOR_URL/api/mills/capabilities | jq
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/kpis?window=1d" | jq
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/telemetry/stages?window=1d" | jq

# Optional lifecycle/raw Prometheus checks. Keep this port-forward running in
# another terminal; these routes are not on LOOM_MILLS_OPERATOR_URL.
kubectl -n loom-mills port-forward svc/loom-mills-operator 9090:9090
curl -sf http://127.0.0.1:9090/readyz                       # 200 once initialized
curl -sf http://127.0.0.1:9090/metrics | grep '^mills_'
```

`/readyz` on the metrics listener is service readiness: the HTTP process,
migrations, and policy manager are initialized. It is not an autonomy approval signal. Before enabling or
trusting unattended writes, check `/api/mills/status` or
`/api/mills/capabilities` and require `autonomy_ready=true`. When this is
false, the scheduler may continue ticking for observability, but the reconciler
will not start queued backlog items.

Common autonomy blockers are intentional fail-closed states: missing admin auth,
missing or unwritable `LOOM_MILLS_REPO_ROOT/.loom`, unwired FlexInfer/GitLab/HUD
spawn/MCP hub dependencies, NoOp dispatcher coverage on write stages, or fake
council participants. Read-only APIs may stay healthy while these blockers are
present.

Stage failure counters also retain deliberate fail-fast and spawn-stall harness
runs. Group the corresponding Loki rows by run/backlog ID before declaring a
real-work regression; those failures are evidence that a negative-path gate
fired. The `mills_autonomous_merges_real` KPI excludes heartbeat canaries, but
stage-attempt telemetry is intentionally unfiltered.

In k3s, `LOOM_MILLS_REPO_ROOT=/workspace/loom-core` is backed by the same
Longhorn RWO PVC as `/var/lib/loom-mills`. There is no initContainer, git-sync
sidecar, or CronJob: the checkout is created in-process at operator boot by
`ensureRepoRoot` (`cmd/loom-mills-operator/repo_root.go`) using the GitLab
project token. If credentials, network, `git`, or the checkout itself fail, the
pod should still become service-ready, but `/api/mills/capabilities` must show
`repo_root` red and `autonomy_ready=false`.

**Known gap — the checkout does not refresh.** `ensureRepoRoot` early-returns
when `repoRootReady` is satisfied, and that check only asserts that `.git`
exists and `.loom` is a writable directory. Once the PVC holds a good clone,
those are true forever, so the `fetch` / `checkout` / `merge --ff-only` block is
never reached on a healthy pod and the tree stays pinned at whatever commit it
was first cloned at. Accumulated untracked `.loom/*COUNCIL-*` artifacts would
also make `--ff-only` warn-and-skip even if it were reached.

Consequence for planning: the roadmap extractor fills `roadmap_intents` from
whatever `ROADMAP.md` is on the PVC, so a stale checkout produces stale-but-present
intents. The fail-closed preflight stays green while the *signal* is wrong, and
`eval` `roadmap_alignment` grades against it. To check freshness:

```bash
kubectl -n loom-mills exec deploy/loom-mills-operator -- \
  git -C /workspace/loom-core log -1 --format='%H %ci'
```

If that lags `origin/main`, restore freshness out-of-band (re-clone the PVC path
or roll the pod after clearing it) and file the `ensureRepoRoot` refresh fix.

## Pipeline-start recovery

A queued item is admitted with its run, workflow identity, capacity
reservation, transition ledger row, and dispatch intent in one SQLite
transaction. The starter runs only after that transaction commits. A pod crash
between those steps is self-healing: the next reconciler tick leases the
committed intent or recovers an acknowledged-but-still-queued top-level run and
drives the same run ID forward.

Use the outbox gauge and structured events to distinguish a brief restart drain
from a stuck starter:

```bash
curl -sf http://127.0.0.1:9090/metrics | \
  grep '^mills_pipeline_dispatch_outbox_pending '
kubectl logs -n loom-mills deploy/loom-mills-operator --since=30m | \
  grep -E 'reconciler\.(started|dispatch_dead_lettered|dispatch_ack_failed)'
```

The metrics command assumes the `:9090` port-forward from Quick status is
running. The gauge may rise briefly during a restart and should then fall. Delivery is
at-least-once by design, with the run ID as the starter idempotency key. Do not
delete outbox or reservation rows to force progress: lease fencing prevents a
late consumer from acknowledging another consumer's work, and manual deletion
can hide an unaccepted start.

After the bounded retry ceiling, Mills marks the intent `dead_letter`,
escalates the current run/backlog aggregate, synchronizes the workflow terminal
state, and releases the reservation atomically. Investigate the associated
`reconciler.dispatch_dead_lettered` event and starter configuration, then use
the normal human-resolution/requeue path. Obsolete intents from an older
aggregate are retired without escalating a newer run.

The operator remains a singleton. These recovery guarantees cover process
restart under SQLite WAL; they do not provide multi-replica worker fencing.

## Run provenance

Every run is stamped once at that same post-commit boundary with a
`run.provenance` event (actor `reconciler.provenance`), keyed
`subject_kind=pipeline_run` for the DAG lane and `workflow_run` for the
imperative lane. The payload carries `policy_checksum` (sha256 of the exact
policy bytes the active policy was parsed from — the same value as the
deployment's `loom.flexinfer.ai/policy-checksum` annotation when the pod is in
sync), `stage_models`, and `prompt_hashes`. It is the join key for per-version
win-rate and cost analysis: without it a merged run cannot be attributed to the
policy revision, model pins, or prompt templates that produced it.

The stamp records START-TIME INTENT. A mid-run swap — policy hot-reload, a
council fallback editor substituting for an unreachable remote backend — is
attributable from the per-dispatch `pipeline.stage.agent_routed` events
instead, not from the stamp.

```bash
curl -sf http://127.0.0.1:9090/metrics | grep '^mills_run_provenance_stamps_total'
```

`outcome="duplicate"` is crash-recovery replay re-reaching an already-stamped
run and is normal. A rising `outcome="error"` means runs are landing without
the join key; the operator logs `append run provenance stamp failed` with the
run id. Provenance is best-effort by design — a failed stamp never blocks a
dispatch — so this is an analytics-completeness alert, not an availability one.

## Updating backlog items safely

`POST /api/mills/backlog` creates an item when its ID is new. Updating an
existing ID requires the `Revision` returned by the latest backlog GET/list
response. A stale write returns HTTP 409 with
`{"error":"stale-backlog-write"}`. Re-read the item, merge the intended
metadata change, and retry with the new `Revision`; do not blindly replay the old
body because it may overwrite a lifecycle transition.

## Pause and resume

The kill switch is `policy.enabled: false`. Edits to the mounted ConfigMap propagate via fsnotify within seconds; in-flight runs continue under their captured policy, but no new runs start.

### Pause

```bash
# Edit platform/gitops/k3s/mills/configmap-policy.yaml in the gitops repo:
#   enabled: false
# Then reconcile:
flux reconcile kustomization apps -n flux-system --with-source

# Wait for hot-reload signal in operator logs:
kubectl logs -n loom-mills deploy/loom-mills-operator --since=2m | grep "policy reloaded"

# Confirm the reconciler is parked:
loom mills status                # expected: enabled=false, queue_depth=0 (no new picks)
```

The reconciler exits cleanly within one tick (≤60s). The HTTP and metrics listeners stay up so HUD reads continue working.

### Resume

Reverse the edit (`enabled: true` or remove the field), reconcile, watch for the next `policy reloaded` log.

### Emergency pause without GitOps

Only when GitOps is unavailable (e.g., Flux is down). Patches the mounted ConfigMap directly; **must** be reverted by a GitOps commit afterwards or Flux will fight you.

```bash
kubectl patch configmap -n loom-mills loom-mills-policy \
  --type merge -p '{"data":{"policy.yaml":"<full yaml with enabled: false>"}}'
# Restore via git/Flux as soon as the issue is resolved.
```

## Force-escalate a stuck pipeline run

A run can wedge if a stage worker hangs (e.g., `ci_watch` waiting on a CI job that's been canceled). Force-escalation transitions the run to `escalated`, records a failure record in `events`, and (if configured) opens a GitLab issue + agent-context handoff.

```bash
# Find the run
loom mills pipelines list

# Escalate
curl -sf -X POST -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  $LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<run_id>/escalate

# Confirm
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?state=terminal&limit=50" | \
  jq '.[] | select(.State == "escalated")'
```

The reconciler will not auto-retry escalated items; a human must edit the linked YAML or close the GitLab issue with a deliberate `human-resolved` label to unblock.

## The imperative workflow lane

Most backlog items run the DAG pipeline (queued → plan_slice → … → merge), covered by the sections above. A backlog item can instead carry a `policy.workflow_template` selection (see "Workflow template registry (S7)" in `docs/mills-operational-guardrails.md`); admission then routes it through `ClaimWorkflowStart` into a **workflow run** — an imperative run driven by a `go.starlark.net` program from the compiled-in template registry, executed by the durable-journal runtime instead of the pipeline stage graph. These items get **no pipeline run at all**; everything about what happened lives in the workflow run and its step journal.

Key differences from a pipeline run worth knowing before you inspect one:

- The run's engine, template, template version, interpreter version, and clamped params are **frozen at start** and never re-resolved — editing the backlog item cannot re-route an in-flight run.
- Every terminal outcome — `done`, `error`, or `quarantined` — **escalates the backlog item and stops before merge**. v1 templates never open an MR or merge; a `done` run still needs a human to review and land the work product. Don't read `done` as "shipped."
- The run's work product lives on a deterministic branch named `mills-wf/<run-id>`, pushed by the run itself. There is no other record of the diff — the branch **is** the deliverable to review.

### Inspecting a run

**HUD — Workflows tab.** Under Mills, the Workflows panel lists every imperative run (`GET /api/mills/workflow/runs` under the hood, polled every 15s): run ID, engine, `template@version`, state, step count, backlog ID, cost, and start time. Click a row to open the step-timeline drawer, which shows each journaled step with a derived badge — `live` (ran a real effect this execution), `cache_hit` (replayed from the journal, no live effect), `pending` (interrupted mid-step), `failed`, or `quarantined` (the whole run halted on a nondeterminism/call-hash mismatch). The drawer also shows the interpreter version pin and the frozen selection params (content hash + clamped params/enums) when present.

If the run belongs to a backlog item, open that item's detail drawer instead (Backlog panel → row) to see the "Imperative workflow" section: the item's template selection, every run claimed for it, and — critically — the escalation attention banner once a run has settled terminal. The banner surfaces the run's outcome, a copy-to-clipboard for the `mills-wf/<run-id>` branch, and an explicit "pre-merge template — nothing was merged" reminder, so a reviewer triaging an escalated item doesn't have to cross-reference the Workflows tab manually.

**REST — `/api/mills/workflow/runs`.**

```bash
# List recent runs (default limit 50, max 200), newest first:
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs?limit=50" | \
  jq '.runs[] | {id, backlog_id, template, template_version, state, step_count, cost_usd}'

# One run plus its full step journal (badges included):
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs/<run_id>" | jq
```

The list endpoint is a flat, cheap summary (no nested steps); the detail endpoint returns the run plus every journaled step, each with the same `live` / `cache_hit` / `pending` / `failed` / `quarantined` badge the HUD drawer shows. When the run carries a frozen S7 selection or canary identity, the detail response also includes a `workflow_params` field (opaque JSON — content hash + clamped params/enums) for inspecting exactly what identity the run is pinned to.

### Responding to a `workflow.terminal_settle` escalation

When a claim-started imperative run reaches any terminal state, the same transaction that commits its lifecycle CAS also releases its budget reservation, escalates the backlog item, and records a `workflow.terminal_settle` event on the item (`GET /api/mills/backlog/<id>` events, or the HUD attention banner described above). The event payload carries `run_id`, `final_state`, the `branch` (`mills-wf/<run-id>`), the `template`, and a human-readable `reason` string.

As the operator handling the escalation:

1. **Identify the run and its outcome** from the event payload or the HUD banner — `done`, `error`, or `quarantined`.
2. **Review the work product on `mills-wf/<run-id>`** regardless of outcome. A `done` state means the run's script completed without error, not that the diff is safe to ship — nothing has been merged, and there is no CI/MR gate to trust yet.
3. **For `error` or `quarantined`**, also check the step journal (`GET /api/mills/workflow/runs/<run_id>`) for the failing/quarantined step before deciding whether to retry. `quarantined` specifically means a step's re-derived `call_hash` didn't match the journal — treat it as a nondeterminism bug in the template or its inputs, not a transient failure to retry blindly.
4. **Disposition the item** the same way you would any escalated backlog item: merge the branch manually if the work product is good, edit the item and re-trigger if it needs rework, or close it out. There is no "resume to merge" action — the workflow lane stops pre-merge by design.
5. If the run is still live and clearly wedged rather than settled (no forward step progress, past `workflows.max_run_minutes`), use the pause/fail controls below instead of waiting — a wedged run is force-terminalized as `error` by the scheduler's wall-clock bound anyway, but you don't have to wait for it.

## Pause / resume / fail a workflow run

Imperative workflow runs (the durable-journal runtime behind `policy.workflows.enabled`) have their own lifecycle mutations, separate from pipeline escalation. Use these to stop a wedged run live — e.g. a zombie whose spawn died terminally (the 2026-07-09 wf-canary loop) — instead of waiting for a code deploy.

```bash
# Find the run
curl -sf $LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs | jq '.runs[] | {id, state}'

# Pause (running → paused; takes effect between steps, on the next scheduler tick)
curl -sf -X POST -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  $LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs/<run_id>/pause

# Resume (paused → running)
curl -sf -X POST -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  $LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs/<run_id>/resume

# Fail (running|paused → error; TERMINAL — no un-fail. Optional audit reason.)
curl -sf -X POST -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  -d '{"reason":"zombie spawn, pod gone"}' \
  $LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs/<run_id>/fail
```

Expected output: the run's lifecycle view, e.g. `{"id":"wf-canary-…","state":"paused","paused_at":"…"}`. An invalid transition returns `409` with the current state. Pause/fail are eventually-consistent like the policy kill switch: an in-flight step finishes its current attempt first; the scheduler skips the run from the next tick.

## Replay a council run

Useful when you want to verify a fix to the brief assembler, swap an ensemble member, or compare A/B outputs.

### Dry-run (safe)

Runs the full council pipeline against a scratch DB; nothing is committed and no GitLab mutations happen. The sidecar + plan paths are returned for inspection.

```bash
loom mills council dryrun
# Output: sidecar JSON path, .loom/<NN>-… draft paths, total cost
```

### Real replay with a different ensemble

```bash
# 1. Edit policy.council.ensemble in platform/gitops/k3s/mills/configmap-policy.yaml
#    e.g., swap editor.model from claude-opus to codex-gpt5.
# 2. flux reconcile.
# 3. Wait for "policy reloaded" log line.
# 4. Trigger:
loom mills council run

# 5. Inspect the new run:
loom mills eval list
# HUD: Mills → Eval panel; sort by created_at desc.
```

When the run lands, compare cost + Loop A score + downstream Loop B attribution against the previous run with the same brief content (same `roadmap_intents` snapshot).

V2.1 will add first-class A/B replay UI; until then, the manual flow above is the supported path.

## Council blocked on missing roadmap intents

Symptom: every council run terminalizes immediately with `outcome=error` and a
note containing `require_roadmap_intents`; no reviewer or editor spend occurs and
`cost_usd_approx` is `0`. The fail-closed intent preflight fired because the
canonical `roadmap_intents` store is empty (see "Fail-closed intent preflight" in
`docs/MILLS.md`).

```bash
# Confirm the store is empty and the brief was marked:
loom mills council runs --limit 5          # look for the blocked note
kubectl -n loom-mills exec deploy/loom-mills-operator -- \
  sqlite3 /var/lib/loom-mills/mills.db 'SELECT COUNT(*) FROM roadmap_intents;'
```

Fix the cause, not the symptom — the extractor fills the store from
`$LOOM_MILLS_REPO_ROOT/ROADMAP.md` on every run, so an empty store means that
file is missing, unreadable, or has no open `- [ ]` bullets under a plannable
section:

```bash
kubectl -n loom-mills exec deploy/loom-mills-operator -- \
  sh -c 'ls -l /workspace/loom-core/ROADMAP.md && grep -c "^- \[ \]" /workspace/loom-core/ROADMAP.md'
```

Break-glass only, when planning must proceed against an empty store: set
`council.require_roadmap_intents: false` in
`platform/gitops/k3s/mills/configmap-policy.yaml` **and** bump
`loom.flexinfer.ai/policy-checksum` in `platform/gitops/k3s/mills/deployment.yaml`
(fsnotify misses the `..data` symlink swap). Revert once the store refills.

Note: the block happens after admission, so each blocked tick consumes a
`council_runs` row. The council tier sets no `max_runs_per_day` today, so this is
harmless; if one is ever added, a wedged store could exhaust the daily allowance
and block the recovery run too.

## Audit a merged change

Find which council brief produced the slice that produced the merge:

```bash
# Get the pipeline run by MR iid:
curl -sf \
  "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?mr_iid=<iid>"

# Eval Loop B attribution and Loop A score:
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/eval/scores?limit=200" | \
  jq '.[] | select(.SubjectID == "<run_id>" or .SubjectID == "<council_run_id>")'
```

For deeper inspection, the `events` table has the full per-stage trace (slog rows are also written via JSON to stderr; aggregated in Loki).

## Serial merge queue

GitLab CE has no merge trains. Without the queue, parallel Mills branches go
stale while 17–28 minute pipelines run; main moves underneath them and their
merge PUTs die with `has_conflicts` (killed !1509 and !1511 on 2026-08-08),
and merges land in bursts that stack uncancelable main pipelines. The serial
queue (`pkg/mills/mergequeue`) guarantees every MR is CI-tested on the exact
main it lands on, one candidate at a time per `(project, target_branch)` lane.

### Semantics

When `merge_queue.enabled` is true, the pipeline's `merge` stage validates the
full `ci_watch` authorization once (including the #374 head-movement fence),
**enqueues** the candidate into the durable `merge_queue` table, and waits.
The processor drives only the head of each lane:

1. **Up to date** (MR `diff_refs.base_sha` == target tip): merge immediately —
   the run's own green CI is the proof.
2. **Behind**: request a rebase (a durable `mr_head_transitions` ledger row;
   cursors snapshotted before the PUT, verdict settled via the #374 observer),
   await a fresh branch pipeline on the rebased head, then merge with the
   rebased SHA as the precondition.
3. **Terminal settle**: `merged` wakes the waiting stage with the merged SHA;
   `evicted` fails the stage with a distinct reason and the run falls through
   to the normal escalation path. **The queue never retries internally.**

Eviction reasons: `rebase_conflict`, `rebase_ambiguous`, `ci_red`,
`ci_timeout`, `head_moved`, `mr_closed`, `merge_failed`. An enqueue into a
full lane (depth ≥ `merge_queue.max_depth`, default 10) escalates immediately
with reason `queue_full` — backpressure surfaces instead of silently deepening
the queue.

Durability: queue state and the rebase ledger live in the canonical SQLite
store, so an operator restart resumes the head candidate exactly where the
previous process died — re-observing, never re-mutating. The resumed merge
stage re-finds its entry FIRST and skips authorization re-validation (the
queue's own rebase legitimately advances the #374 fence).

### Rollout / rollback

The queue is a policy flip — no restart, no schema action:

```yaml
merge_queue:
  enabled: true      # default false
  max_depth: 10      # optional
```

Rolling back (`enabled: false` or removing the section) halts the processor on
its next tick without losing state; runs waiting in the merge stage observe
the disable and fall back to the direct merge (a queue-rebased candidate then
fails closed on the stale fence and escalates for a clean re-gate — expected).

### Observability

- `GET /api/mills/pipeline/runs/{id}` gains a `merge_queue` block (state,
  position, eviction reason) whenever the run has an entry.
- Prometheus: `mills_mergequeue_depth`, `mills_mergequeue_wait_seconds`,
  `mills_mergequeue_evictions_total{reason}`, `mills_mergequeue_merged_total`.
- Events: `mergequeue.merged` / `mergequeue.evicted` rows against the run.
- Stage wait bound: `LOOM_MILLS_MERGE_QUEUE_WAIT_MINUTES` (default 180) —
  the merge stage escalates if the queue produces no verdict in that window
  (dead processor / saturated queue).

## Recover from a corrupted DB

The canonical SQLite DB lives on a Longhorn RWO PVC. WAL replay handles most operator restarts; nothing should be needed for a clean kill. The procedures below are for the rare cases where the DB is unrecoverable.

### Symptom: `database is locked` or schema check fails on boot

1. Capture diagnostics:

   ```bash
   kubectl logs -n loom-mills deploy/loom-mills-operator --previous --tail=500 > /tmp/mills-prev-logs.txt
   kubectl exec -n loom-mills deploy/loom-mills-operator -- sqlite3 /var/lib/loom-mills/state.db "PRAGMA integrity_check;"
   ```

2. If `integrity_check` returns anything other than `ok`, restore from the most recent nightly backup in MinIO:

   ```bash
   # List backups (nightly CronJob writes one per day):
   mc ls minio/loom-mills-backups/

   # Pick a backup to restore:
   BACKUP=2026-04-30T06-00-00.db

   # Scale operator to 0 to release the PVC:
   kubectl scale deploy -n loom-mills loom-mills-operator --replicas=0

   # Restore via a one-shot Job (the manifest lives at
   # platform/gitops/k3s/mills/jobs/restore-from-backup.yaml; render with the
   # chosen filename):
   kubectl create -n loom-mills -f - <<EOF
   apiVersion: batch/v1
   kind: Job
   metadata:
     generateName: mills-restore-
   spec:
     template:
       spec:
         restartPolicy: Never
         containers:
           - name: restore
             image: registry.harbor.lan/library/loom-mills-operator:stable
             command: ["/bin/sh", "-euxc"]
             args:
               - |
                 mc cp minio/loom-mills-backups/$BACKUP /var/lib/loom-mills/state.db
                 sqlite3 /var/lib/loom-mills/state.db 'PRAGMA integrity_check;'
             volumeMounts:
               - { name: state, mountPath: /var/lib/loom-mills }
         volumes:
           - name: state
             persistentVolumeClaim:
               claimName: mills-state
   EOF

   # Watch the restore Job to completion:
   kubectl logs -n loom-mills job/<job-name> -f

   # Bring the operator back up:
   kubectl scale deploy -n loom-mills loom-mills-operator --replicas=1
   kubectl rollout status deploy -n loom-mills loom-mills-operator
   ```

3. After restore, run a council dryrun to confirm the brief assembler still works against the restored canonical state. Then resume normal operation.

### Loss-window note

The nightly CronJob captures one snapshot per day. Items committed since the last snapshot are lost in a restore. Re-run the council to reproduce — `roadmap_intents` is idempotent and `.loom/backlog/*.yaml` is regenerated from the canonical store, so most state self-heals.

## Inspect what the operator is doing right now

```bash
# Active runs (council + pipeline):
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/status" | jq

# What gates fired in the last hour:
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?state=terminal&limit=50" | jq

# What the budget enforcer thinks:
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/kpis?window=1d" | \
  jq '{SnapshotAt, Metrics: (.Metrics | {pipeline_cost_usd, gate_pass_rate, pipeline_merged_real, regression_rate})}'
```

## Production rollout staging

The default-on flip (slice 6.6) was landed for the in-binary policy default. Each cluster overlay still owns whether the mills is enabled in *its* ConfigMap. Stage rollouts in this order; never flip more than one environment per week.

### Stage 1 — Local

A developer can run the operator against a local SQLite file and a local policy YAML for the smallest possible smoke test:

```bash
make build/loom-mills-operator
mkdir -p /tmp/loom-mills
./bin/loom-mills-operator \
  --db-path     /tmp/loom-mills/state.db \
  --policy-path testdata/policy.yaml \
  --listen      :8090 \
  --metrics-addr :9090

curl -sf localhost:9090/healthz                            # 200
curl -sf -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  localhost:8090/api/mills/status | jq
```

Local runs use the FakeReviewer/FakeEditor (`cmd/loom-mills-operator/main.go: buildCouncilRunner`) when no FlexInfer or HUD spawn is configured; this is sufficient for handler smoke tests but does not exercise real model calls.

### Stage 2 — Dev cluster (kc-k3s)

```bash
# In the platform/gitops repo on a dev branch:
# Edit platform/gitops/k3s/mills/configmap-policy.yaml: enabled: true
# Commit, push, open MR, merge.

flux reconcile kustomization apps -n flux-system --with-source
kubectl rollout status deploy -n loom-mills loom-mills-operator
```

Soak for one week. Verify each acceptance criterion against the deployed cluster:

| Criterion | Check |
|---|---|
| Council dryrun produces sidecar + 3 markdown docs in <8 min for <$5 with eval ≥0.7 | `loom mills council dryrun` and inspect cost/score in HUD |
| End-to-end backlog → merged MR via fixture run | `loom mills backlog list --json | jq '.[] | select(.State == "merged")'` after seeding a fixture |
| Eval Loops A/B/C populated | `loom mills eval list` returns recent rows for all three subject kinds |
| Idle-throttle drops reconciler cadence | `loom mills status` shows `next_tick_in_s` ≥ 240 when queue is empty |
| HUD `Mills` view renders four panels | Visit `<HUD>/mills` |
| Backups produced nightly | `mc ls minio/loom-mills-backups/` shows last 7 entries |

### Stage 3 — Production cluster

Only after dev has been green for 7 consecutive days. Same edit + reconcile flow, but watch the regression KPI:

```bash
# Keep the :9090 port-forward from Quick status running, record the starting
# counter value, then watch for any increase during rollout.
curl -sf http://127.0.0.1:9090/metrics | grep '^mills_regression_count_total'
watch -n 30 'curl -sf http://127.0.0.1:9090/metrics | grep "^mills_regression_count_total"'
```

If `mills_regression_count_total` increases from the recorded baseline in the first 24h, pause and investigate before any further flips. This counter is Alertmanager-correlated; the REST `Metrics.regression_rate` field is a label-driven backlog KPI and is not an equivalent rollout alarm. The kill switch (`enabled: false`) is the safest and lowest-blast-radius rollback.

## GitLab issue importer (Slice 1a of .loom/43)

The operator can pull open issues labelled `mills-eligible` from its
configured GitLab project (`GITLAB_PROJECT` env) and create one
BacklogItem per fresh issue. Disabled by default; opt in via policy.

```yaml
intake:
  gitlab:
    enabled: true
    eligible_label: "mills-eligible"   # required label on the issue
    poll_interval_seconds: 300         # 5 min
    default_priority: "P2"             # fallback if no priority:Px label
```

**Label semantics on the issue side:**
- `mills-eligible` (required) — the importer's selector. Add this on
  any issue you want Mills to pick up.
- `priority:P0` / `priority:P1` / `priority:P2` / `priority:P3`
  (optional) — sets the BacklogItem priority. Highest priority wins
  when multiple are present. Default is `default_priority`.

**Dedup**: BacklogItem ID is `gl-<project_id>-<issue_iid>`, so the
importer is safe to re-run. Once an item exists, the importer does NOT
update it — reconciler/council state transitions (queued → running →
merged/escalated) are preserved across ticks.

**Verify**:
```bash
# After enabling, watch for the import line in operator logs:
kubectl -n loom-mills logs deploy/loom-mills-operator -f | \
  grep "gitlab importer"
# Check the backlog API for the new item:
loom mills backlog list | grep gl-
```

**Don't enable until pipelines are reaching `merge` stage.** Per the
2026-05-24 kill-test (`.loom/local/handoffs/mills-autonomy-killtest-...`)
100% of pipeline_runs were escalating at `tests` or `plan_slice`.
Adding more intake before Slices 2c + 2e land would just deepen the
escalation pile.

## Source-branch push before MR

The `mr` stage publishes the spawn agent's commits to origin before
calling GitLab `CreateMR`. Without this, GitLab accepts the MR row
but it points at a branch with no `head_sha`, and `ci_watch` hangs
forever waiting for a pipeline that can't exist.

There are TWO push paths (belt-and-suspenders):

1. **Spawn-side push (primary)**: the `implement` stage prompt
   explicitly instructs the agent to `git push -u origin HEAD` as
   its final step. The spawn pod has git credentials configured.
   This is the path that actually fires today because the single-
   repo SpawnWorker does NOT use Mills' WorktreeAllocator — so
   the operator doesn't know the worktree path and can't push
   from outside the pod.

2. **Operator-side push (fallback, currently inert)**: when
   `jc.Run.WorktreePath` is set, `GitLabWorker.runMR` runs the
   `clients.GitBranchPusher` before `CreateMR`. This path is
   exercised in the cross-repo integrator flow where Mills does
   allocate worktrees explicitly. A future slice should propagate
   the spawn-side WorkingDir back through SpawnResponse so the
   fallback also kicks in for single-repo runs.

- Implementation: `pkg/mills/clients/branch_pusher.go`
  (`clients.GitBranchPusher`) shells `git push --force-with-lease -u
  origin HEAD:<branch>` from the run's `WorktreePath`.
- Interface: `pipeline.BranchPusher` (4-line contract). The operator
  wires the production pusher onto `GitLabWorker.BranchPusher` at
  startup; tests inject fakes.
- Idempotency: `--force-with-lease` lets retries safely overwrite a
  stale prior push without clobbering unrelated upstream work. A
  no-op push (HEAD already at origin) exits 0.

Failure mode (push errors before MR is created): the `mr` stage
returns an error and the runner retries per the Slice 2c
classification — a transport / quota error gets free retries; a
real `git push` rejection counts against MaxAttempts.

## v2 rollout staging (preview)

When `.loom/94-implementation-plan-mills-v2-hierarchical-swarm-2026-05-02.md` Phase 8 lands, each v2 feature flips behind its own policy flag with a 1-week soak between flips. Order:

1. `policy.squads.enabled: true`
2. `policy.audit.enabled: true` (advisory-only by default)
3. `policy.council.debate.enabled.incident: true`
4. `policy.cross_repo.enabled: true` — only after 3 successful loom-core+loom dogfood atomic merges
5. `policy.adaptive_policy.enabled: true` (manual-apply only)

Detail per slice in `.loom/94-…2026-05-02.md` Phase 8. The rollback playbook for v2 lives in [MILLS_V2_ROLLBACK.md](MILLS_V2_ROLLBACK.md) — covers feature flag disable, policy proposal revert, and DB restore from MinIO.

## Useful loops

```bash
# Watch council runs as they complete:
watch -n 30 'loom mills eval list | head -10'

# Watch escalation rate:
watch -n 60 'kubectl logs -n loom-mills deploy/loom-mills-operator --since=10m | jq -r "select(.msg==\"escalation\") | [.time, .item_id] | @tsv"'

# Diff the live policy vs. git:
kubectl get configmap -n loom-mills loom-mills-policy -o jsonpath='{.data.policy\.yaml}' | \
  diff - platform/gitops/k3s/mills/configmap-policy.yaml
```

## Spawn pool capacity & multi-repo

Mills pipeline stages (plan_slice / research / implement) run as **spawn pods**.
The spawn orchestrator is embedded in **mobile-hud** (`internal/hud/spawn.go`),
*not* the operator — the operator only POSTs a spawn request to the HUD. So the
spawn knobs are env vars on the **mobile-hud** Deployment
(`k8s/base/servers/mobile-hud/deployment.yaml`), which deploys via the Flux
`loom-hub-servers` Kustomization. The gitops override patch
(`platform/gitops/clusters/k3s/flux-system/kustomization-loom-hub-servers.yaml`)
does **not** touch these, so the base-manifest values are authoritative.

| Env var | Meaning | Current |
|---|---|---|
| `SPAWN_MAX_CONCURRENT` | Global cap on active spawn pods (mobile-hud is a single replica, so this is a fleet-wide ceiling). | `10` |
| `SPAWN_MAX_CONCURRENT_BUILDS` | Concurrent spawn *image* builds (buildah); throttled independently so a higher run cap can't cause a build thundering herd. | `1` |
| `SPAWN_DEFAULT_CPU` / `SPAWN_DEFAULT_MEMORY_MB` | Per-spawn resource limits. | `0.5` / `4096` |
| `SPAWN_SYNC_MODE` | Workspace sync mode. `git-clone` = the pod clones the repo fresh (source of truth); the workspace mount is used only to fingerprint the repo for the Dockerfile. | `git-clone` |
| `SPAWN_GIT_BASE_URL` | Git base the pod clones from; rooted at the `services/` group. | `http://192.168.50.218/services` |
| `SPAWN_PROJECTS` | Comma-separated repos shown in the HUD/mobile spawn **picker**. Cosmetic — *not* an enforcement gate. | see manifest |

**Project labels.** Spawn requests keep the canonical project path (for example,
`services/loom-core`) in durable state, while the pod's `loom.dev/project` label
is normalized to a Kubernetes-safe, 63-character value. The label is for pod
discovery and degraded recovery only; clone and routing logic must continue to
use the canonical request value.

**Raising concurrency.** Cluster allocatable memory is ~1.25 TiB (16 nodes), so
even `SPAWN_MAX_CONCURRENT` at 10 (= 5 CPU + 40 GiB) is <4% of capacity. Capacity
is not the constraint — demand is. Bump the value in the base manifest and let
Flux roll mobile-hud; no gitops-patch change is needed.

**Expanding to more repos.** A stage targets a repo via the pipeline worker's
`Project` (defaults to `loom-core`). For a repo to spawn cleanly:

1. **Merge token reach** — the Mills GitLab worker uses the `services`-group
   token (`loom-mills-gitlab-group/api-token`, Maintainer on group `services`),
   so only `services/*` repos can be opened+merged. Cross-group repos (libs/,
   platform/) need a separate token grant.
2. **Git-clone group** — the pod clones `SPAWN_GIT_BASE_URL/<name>.git`, i.e. the
   `services/` group. Non-services repos would need a group-aware base URL.
3. **Workspace fingerprint (best-effort)** — `resolveProjectPath` looks for the
   repo on the `loom-hub-workspace` PVC (Longhorn **RWO**, mounted read-only) to
   fingerprint it for an accurate runtime image. In **git-clone mode** a repo
   that is *not* staged on the PVC no longer hard-fails: the orchestrator falls
   back to a lexical `services/<name>` path and a generic runtime image, and the
   init-container clone provides the real source. Staging the repo on the PVC
   still yields a better-fingerprinted image (correct language toolchain), so for
   build-heavy repos prefer to stage it. Populating/resizing that RWO PVC is a
   coordinated gitops op (single-writer; co-locate the writer with mobile-hud).

### Multi-repo demand activation (S6)

The steps above make a stage *able* to run against a `services/*` repo. **S6**
makes demand itself target foreign repos: the plan-slice emitter sources an
allowlist of non-home projects and stamps each emitted item's `TargetProject`,
so the S2/S3/S4 routing carries the whole run cross-repo. Activation is
**two-key** — both must be true or foreign demand stays inert:

1. **Execution key** — `policy.cross_repo.enabled: true`. Already flipped
   2026-07-05 for the keystone (flexdeck!244). This alone changes nothing about
   demand; it only lets a foreign-targeted item *execute* instead of being
   skipped fail-closed by the reconciler.
2. **Demand key** — `policy.cross_repo.demand_projects: [services/<repo>, …]`.
   The emitter consults this list ONLY when `enabled` is also true
   (`Policy.CrossRepoDemandProjects`), so a stray allowlist can never source
   foreign demand while execution is off.

**Procedure to onboard a repo (e.g. `services/flexdeck`):**

1. **Branch pipeline** — the target repo must run a pipeline on the MR's source
   branch (flexdeck did this via flexdeck!242) so `ci_watch` has a green gate to
   merge on. Without it the autonomous merge can't confirm success.
2. **Set the allowlist** — add the repo to `cross_repo.demand_projects` in the
   gitops policy ConfigMap (`platform/gitops/k3s/mills/…`).
3. **Roll the operator** — the emitter snapshots policy at startup and fsnotify
   misses ConfigMap `..data` swaps, so bump the deployment pod-checksum (or
   `rollout-restart`) to pick up the new allowlist.
4. **Author demand** — the emitter emits from **Plans scoped to the target
   project** that have a ready (`plan_slice_emitter.ready_phase`, default
   `pending`) slice **with declared files**, in the emitter's namespace
   (`plan_slice_emitter.namespace`). A foreign repo with no such Plan produces
   nothing; author a `services/<repo>` Plan with a ready slice to drive a run.
5. **Verify** — operator log `plan-slice emitter created backlog item … demand`,
   the item's pipeline runs against the target repo, an MR opens+merges **in the
   target repo**, and `mills_autonomous_merges_real` increments.

**Plan lifecycle scan contract.** The emitter queries only `planned` and
`in_progress` Plans before looking for pending slices. Take-up queries only
`planned`, `in_progress`, `in_review`, and `merging` Plans. Terminal plan bodies
remain durable in the Plan Store but are not transferred or decoded on every
poll. If `/api/mills/safety/quiescence` shows `plan_slice_emitter` or `takeup`
active across an entire poll interval, inspect agent-context latency and the
operator logs; do not widen either query to an unfiltered namespace scan.

**Rollback** — remove the repo from `demand_projects` (stops new foreign
demand) or set `cross_repo.enabled: false` (also fail-closes any already-queued
foreign item on the next reconciler tick), then roll the operator.

## See also

- [HUD fleet-roll unavailability](runbooks/hud-fleet-roll-unavailability.md)

## Sources

- `cmd/loom-mills-operator/main.go` (lifecycle, env vars, fakes vs. wired clients)
- `cmd/loom-mills-operator/auth.go` (admin token loading)
- `pkg/mills/policy.go` (`enabled`, `IsEnabled`, kill-switch semantics)
- `pkg/mills/policy_manager.go` (fsnotify hot-reload)
- `pkg/mills/reconciler.go` (one-tick exit on disable)
- `pkg/mills/scheduler.go` (cron + idle throttle)
- `pkg/mills/eval/{judge,outcome_attributor,council_roi,cross_run}.go` (Loops A/B/C)
- `pkg/mills/store/migrate.go` (migrations on boot; safe replay)
- `platform/gitops/k3s/mills/` (manifests; not in this repo — lives in the gitops repo)
- `docs/MILLS.md` (architecture + policy reference)
- `.loom/91-implementation-plan-agent-swarm-council-pipeline-2026-04-25.md` §"Default-off rollout"
