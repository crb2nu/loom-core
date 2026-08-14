# Merge queue phase 2 — field requirements from the fi-fhir sprint program

**Source**: two days of live multi-agent merge coordination on `libs/fi-fhir` (Sprints 3–5,
2026-08-08 → 2026-08-10): ~40 MRs merged across 5–7 concurrent agent lanes, with a human-driven
coordinator session performing exactly the job the Mills merge queue automates. Every requirement
below was *paid for* at least once in that program; citations reference the fi-fhir MR numbers.

**Baseline**: phase 1 as shipped 2026-08-09 (`bl-mills-merge-queue-serial`,
`merge-queue-loomd-local-agents`, `docs/MILLS_MERGE_QUEUE_FLEET_COVERAGE.md`): serial
per-`(project, target_branch)` lane, durable SHA-pinned candidates with producer+idempotency
identity, rebase-onto-tip + fresh-pipeline re-prove, evictions with distinct reasons, HUD/loomd
proxy for local agents, mrwatch join.

Items marked **[verify]** are derived from the phase-1 docs, not from reading the processor code —
confirm current behavior before scoping.

---

## P2.1 — Mechanical robustness (small, each independently shippable)

### R1. Policy-driven manual-job play during re-prove
fi-fhir's `test:benchmark` is a **blocking manual** job on Go-touching MRs; until played it also
parks every `security:*` job in `created`, so a queue that waits for "pipeline succeeds" waits
forever. The coordinator's watcher auto-played it on every armed MR. **[verify]** whether the
phase-1 re-prove handles manual jobs at all.
- Per-repo queue policy: `auto_play_jobs: [test:benchmark]` (name or regex), played by the
  processor as soon as the re-prove pipeline is created.
- A manual job NOT on the allowlist that blocks a candidate → new eviction reason
  `manual_job_blocked` (names the job), not a silent stall.

### R2. Ground-truth mergeability, not GitLab's cached flag
GitLab's `detailed_merge_status` went stale twice in one day (showed `conflict`; `git merge-tree`
proved zero conflicts; recompute required pushing a no-op/fresh SHA). The processor must treat
`merge-tree` against the pinned SHA and lane tip as authoritative and force GitLab to recompute
(fresh pipeline / push) rather than evicting on the cached flag.

### R3. Pipeline-ref race recovery
Observed signature: **all** jobs of a pipeline die within seconds on
`couldn't find remote ref refs/pipelines/<id>` — the pipeline's own ref never materialized
(race with a force-push landing while the pipeline spun up). The fix is *re-trigger a fresh
MR pipeline for the same SHA*, never diagnosis. Processor should recognize the signature
(N jobs, same error class, ~same timestamp) and retry once before evicting as `ci_red`.
Corollary: failure *synchrony across jobs* is the cheap discriminator between infra and code.

### R4. `requeue_on_head_move` producer option
Phase 1 evicts on `head_moved` (right default: SHA-pinned identity). But an agent lane that
amends its own MR (fix a lint, append a CI include) currently loses its queue position and must
re-enqueue manually. Optional per-candidate flag: on head move, re-pin to the new head and keep
lane position (bounded: max N re-pins, only when the producer of the push matches the candidate's
producer identity — otherwise it is a hijack signal and evicts as today).

### R5. Arm/merge API race tolerance
GitLab returns 422 on merge-when-pipeline-succeeds until the pipeline is *registered* (seconds
after MR creation/push). Any queue-side arm/merge call needs the retry-next-tick semantics the
coordinator's watcher had — treat 405/406/422 as "not yet", not failure. **[verify]** phase-1
processor likely merges directly and may not hit this; the loomd/mrwatch shepherd path does.

---

## P2.2 — Conflict intelligence (the actual time sink)

Two days of data: **every single rebase conflict** (≈12 occurrences) fell into exactly three
classes, two of them mechanically resolvable:

| Class | Example | Resolution | Who |
|---|---|---|---|
| Shared-EOF append | `.loom/40-decisions.md` dated entries; pre-split `.loom/50-worklog.md` | Union (ours-then-theirs, strip markers) | machine |
| Structured append-point | `.gitlab-ci.yml` job blocks, `Makefile` `.PHONY` lane lines | Re-apply block from branch commit + invariant check (YAML valid, job named once) | machine + check |
| Semantic overlap | `.PHONY` restructure landing under a sibling's new targets (fi-fhir !169/!170) | Lane/owner decision | human/agent |

### R6. Union auto-resolution for allow-listed paths
On `rebase_conflict`, before evicting: if **every** conflicted path matches a repo-policy
allowlist (`union_merge_paths: [".loom/40-decisions.md", ".loom/worklog/*"]`), auto-resolve by
union (ours-then-theirs), verify zero markers remain and the diff vs target is additive-only for
those paths, continue. The fi-fhir coordinator ran this exact recipe ~10× with a 100% success
rate and spot-verified output each round.
- Also recommend (docs + repo-intake skill): `.gitattributes merge=union` for such files — the
  queue's own local rebase honors it, which makes R6 nearly free where adopted.

### R7. Eviction payloads carry the conflict classification
`rebase_conflict` evictions must include the conflicted **paths** and, per path, whether it
matched an auto-resolve class. Tonight's division of labor — coordinator does mechanical
recovery, lane agent does semantic surgery — is only automatable if the eviction says which one
is needed. An escalation that says "conflict in Makefile (structured append-point)" routes
itself; "conflict" does not.

### R8. Append-file disease detector (advisor, not gate)
The same pathology recurred three times in one program (worklog → fixed by per-entry files;
`.loom/40-decisions.md` → currently sick; `.gitlab-ci.yml`/`Makefile` → fixed by per-lane
include files and per-lane `.PHONY` lines). Detector: N≥2 active lane candidates whose diffs
touch the **tail region of the same file** → HUD lane-pressure warning naming the file, with the
prescription ("split into per-entry files + generated index; see fi-fhir !147"). This converts a
conflict-by-construction from a recurring rebase tax into a one-time convention change.

---

## P2.3 — Stack awareness

fi-fhir Sprint 5 shipped almost every lane as a **stacked pair**: a day-1 gate MR (test-only)
with an implementation MR on top. The queue has no notion of this; the coordinator sequenced by
hand.

### R9. `depends_on` in the enqueue payload
Candidate may name a parent MR IID. Processor orders within the lane (parent first), holds the
child while the parent is active, and treats a parent eviction as a child hold + escalation
(never a silent child rebase that swallows the parent's diff).

### R10. Post-parent-merge child normalization
With merge-commit method the child's merge-base advances automatically (observed: fi-fhir
!166→!172). With squash/re-author the child's diff drags the parent's files until a
`rebase --onto` that auto-skips applied patches (fi-fhir Sprint 4 !70 lesson; recurred with
!168→!175). Processor should detect the repo's merge method and perform the correct
normalization before re-prove, instead of evicting the child as conflicted.

---

## P2.4 — Event subscriptions (kill the polling watchers)

The fi-fhir coordinator ran shell watch-loops because nothing pushes MR terminal-state events to
a local agent. Batch watchers only woke the coordinator when a whole group finished — too coarse
for queue advancement ("when the CI-split MR merges, resume lane F; when the gate merges, rebase
the child").

### R11. Terminal-state notification per candidate
Enqueue accepts an optional notification target (HUD event; or agent-context
`agent_task_dispatch` to a named agent). Fired once on merge/eviction with the full candidate
detail. Local agents get per-event wakes through loomd instead of polling; the mrwatch-join HUD
panel already has the data model for this.

### R12. Queue-performed pushes are first-class candidate history
S5-C caught the coordinator claiming "remote unchanged" when an earlier sweep round *had*
force-pushed its branch. Any push the queue performs (rebase, union-resolve, normalization)
must be recorded in the candidate detail (before/after SHAs, action, timestamp) so a resuming
agent can enumerate every non-self push to its branch before acting. This is the verify-first
protocol made structural.

---

## P2.5 — Conflict-graph parallelism (the big phase-2 design item)

Strict serial re-prove costs `N × pipeline_time` (fi-fhir: ~40 min pipelines; 6 queued MRs =
4 h). Tonight's empirical result: parallel arming was safe for every pair of candidates whose
diffs did not intersect — the only invalidations were the shared-append files (curable via
R6/R8) and true sibling overlaps (rare, correctly serialized).

- Build the lane's **path-intersection graph** (diff manifests are cheap; `merge-tree` for
  suspected pairs). Candidates in disjoint components re-prove **concurrently**; only
  intersecting candidates serialize behind each other.
- Guardrail stays: the final merge itself remains serial per lane; what parallelizes is the
  re-prove pipeline spend.
- This is the difference between "merge train" wall-clock and tonight's hand-tuned ~2× speedup,
  without GitLab Premium's speculative merged-result pipelines.

---

## Repo-side conventions the queue docs should prescribe (not build)

These belong in the landing-procedure / repo-intake skills as the *prevention* half:

1. **Per-entry files for any multi-writer log** (worklog, decisions journal) + generated index;
   CI format gate (`worklog.sh check` pattern). One entry = one file = zero conflicts.
2. **Per-lane CI include files** (`ci/<lane>.yml` + one `include:` line) and **per-lane `.PHONY`
   lines** — never a shared continuation block. Hidden shared jobs live in `ci/_shared.yml`
   reachable via `extends:`/`!reference` (anchors do not cross include boundaries).
3. **Existence guards on every required proof job** (`-list | rg -x | awk` arity check) and the
   negative control **in the same invocation** — a renamed proof must make the job red, and a
   green job must imply the control ran.
4. **`.gitattributes merge=union`** for genuinely append-only files.

## Out of scope here, logged for other backlogs

- **Context-budget lint** (fi-fhir D2 + the `release()` bug are one shape: an operation whose
  correctness matters after another completes, silently inheriting that operation's deadline —
  "any context crossing a durability boundary deserves its own budget"). Belongs in a Go lint /
  deslopper rule, not the queue.
- **Benchmark truthfulness on heterogeneous runners** (pin `-benchtime`; allocs/op is only
  bit-identical on CPU-pure paths) — fi-fhir `.loom/33` corrections; relevant to loom-core's own
  CI someday, not to the queue.

## Suggested slicing (Mills backlog form)

| Slice | Contents | Size |
|---|---|---|
| bl-mq-manual-jobs | R1 + R5 | S |
| bl-mq-ground-truth | R2 + R3 | S |
| bl-mq-head-repin | R4 | S |
| bl-mq-union-resolve | R6 + R7 | M |
| bl-mq-append-advisor | R8 (HUD) | S |
| bl-mq-stacks | R9 + R10 | M |
| bl-mq-notify | R11 + R12 | M |
| bl-mq-conflict-graph | P2.5 | L (design doc first) |
| docs/skills | conventions §above | S |
