# Sprint Plan — Integration Wrap-up → Mills Utilization on the Fleet Substrate (2026-07-14)

Owner: claude-code (integration sweep session)
Plan-store anchor: `plan-loom-core-fleet-reliability-arch-20260710` (phase: in_review)
Companion docs: [.loom/182-research-loom-core-fleet-architecture-2026-07-10.md](182-research-loom-core-fleet-architecture-2026-07-10.md), [.loom/183-plan-loom-core-fleet-performance-reliability-2026-07-10.md](183-plan-loom-core-fleet-performance-reliability-2026-07-10.md)

## Riskiest assumption + kill-test

**Load-bearing assumption**: The S2 Mills transactional transition kernel
(merged 2026-07-12, `codex/arch-fleet-s2-mills-transactions`) survives real
process death on the DEPLOYED mills operator — i.e. the S1c dual-crash
kill-test passes 3 consecutive runs against the live k3s deployment, not just
locally and in CI.

**Kill test**: Run the S1c canary per
`docs/runbooks/mills-workflow-s1c-killtest.md` against the deployed
`loom-mills-operator` (3 consecutive dual-crash runs via
`cmd/mills-workflow-killtest`). Observable outcome: each run reaches terminal
state with zero duplicate spawns, zero zombie runs, and evidence bundle
verifying (`verify.go` contract). ≤30 min.

**Failure mode if wrong**: Sprints 2–3 build S3/S4 (admission, durable
execution) on top of a claim-transaction kernel that wedges or duplicates
effects under real crashes; Mills utilization scale-up would multiply a
correctness bug instead of throughput.

**Status**: not run (local + branch CI green: pipelines 18371/18385; deployed
canary is the gate). This is Sprint 1's first work item and blocks Sprint 2.

## State of the world (verified 2026-07-14 evening)

- `main` @ `0a9680ed` — codex S1c evidence contract + kilocode kilo.json
  validation merged. Local binaries (`~/.local/bin`, `~/go/bin`) rebuilt at
  this SHA; all `loom sync` profiles in-sync (kilocode regen'd, gitops mirror
  confirmed current on origin/main). loomd restart pending (running daemon
  predates today's install).
- Fleet reliability plan S0–S2 **merged** (branch gauntlet, transport
  generations, Mills transactional kernel). S3–S7 pending.
- MR rescue: !1050 (lazy master key) and !1049 (debounce deflake) rebased onto
  main, conflicts resolved (kept main's ctx-aware keychain API, adopted lazy
  resolution), auto-merge re-armed (pipelines 18944/18945). CHANGELOG overlap
  means the second may need one union-merge re-arm.
- In-flight codex session in `.worktrees/ios-ux-reliability`
  (`fix(ios): harden first-load operator UX` + 9 dirty HUD files, mtime
  minutes old) — left untouched; ships when that session completes.
- Plan store reconciled: Plan Store plan → done; 3 xrepo kill-test + 2 canary
  + ticklabel plans → merged; 17 duplicate council/draft plans → abandoned.
  Surviving council item: `plan-council-propagate-incident-classification-
  into-council-planning-cont`.
- Renovate backlog: !998 (all-minor-patch), !993 (kubernetes-go), !989
  (starlark digest), !988 (fi-accel digest), !982 (artifact actions major).

## Sprint order

### Sprint 0 — land what's in flight (hours)

1. !1050 + !1049 to merge (auto-merge armed; re-arm second on CHANGELOG union
   conflict per `reference_mr_cascade_rescue_recipe`).
2. Restart loomd via `reference_loomd_restart_recipe` (both bins already
   refreshed) — picks up S1c + kilocode code paths locally.
3. Renovate shepherding per `reference_renovate_stale_mr_shepherding`: !989,
   !988, !993, !982 first; **!998 all-minor-patch LAST**.
4. When the codex ios-ux session finishes: ship `codex/ios-ux-reliability` as
   MR (commit exists; dirty HUD files belong to that session's scope).

### Sprint 1 — foundational gates (1–2 days)

1. **Deployed S1c 3-run dual-crash canary** (the kill-test above). On pass:
   advance fleet plan `in_review → merging/merged` and record evidence in
   `kill_test_status`.
2. **#300 weaver liveness**: mobile-hud restart orphans in-flight turns
   (S1c liveness FAILED per `plan-weaver-squads` gap map). The S4 durable
   execution slice subsumes the full fix; do the minimal orphan-adoption
   patch now only if Mills throughput is actively bleeding on it.
3. Commit/track outstanding foundational docs: this sprint doc + .loom/182 +
   .loom/183 (this MR).

### Sprint 2 — codex scaling slices (fleet plan Phase B, ~1 week)

Run as codex-led slices on the fleet plan, in plan-slice order:

1. **S3 — invocation identity + bounded admission**
   (`codex/arch-fleet-s3-invocation-admission`). This is the direct scaling
   enabler: fair per-agent queues + reserved control lane means more
   concurrent Mills/spawn/agent traffic without heartbeat storms or noisy-
   neighbor starvation.
2. **S4 — durable execution, fenced leases, effect ledger**
   (`codex/arch-fleet-s4-durable-execution`). Makes Mills admission, worker
   ownership, cancellation, and external effects (spawn/MR/merge) crash-safe;
   closes #300-class orphans for good.
   - **Status (2026-07-17): pod-owned execution supervisor/reaper landed in code**
     on branch `arch/fleet-s4-durable-execution` (the S1c process-continuity
     keystone). A supervised spawn runs its agent turn + completion hold under a
     detached, PID-1-reparented in-pod reaper that records the outcome durably;
     mobile-hud RE-ATTACHES on restart (tail + collect) instead of re-driving, so
     the original `(hold, wrapper)` process pair survives both crashes. Gated by
     `LOOM_SPAWN_SUPERVISED_EXECUTION` (default on). Remaining before the gate
     flips: deploy the operator + mobile-hud images carrying S4, then run the
     deployed 3-run dual-crash canary (`docs/runbooks/mills-workflow-s1c-killtest.md`),
     now expected to PASS via reattach. Fenced-lease / effect-ledger sub-items of
     S4 remain future work; this slice delivers the process-continuity substrate
     the dynamic-workflows program (Sprint 3.4) rides on.
3. **S5 — endpoint truth + capability ledger + telemetry**
   (`codex/arch-fleet-s5-truth-ledger`) can start in parallel once S3 review
   is out; it has no storage dependency on S4.

Gate: each slice's acceptance criteria in the plan store; no slice ships
without its S0 gauntlet lane green.

### Sprint 3 — Mills utilization on the hardened substrate (~1–2 weeks)

With S3/S4 merged, raise Mills autonomy and throughput:

1. **Raise Mills concurrency** — bump reconciler/spawn-pool limits under the
   new admission caps; watch KPI snapshot + factory shift reports.
2. **Pattern Loom open items**: A2 engram verification, cross-repo stamping
   (catalog already 1→4). Feed via Mills backlog (PascalCase wire contract).
3. **Escalation auto-close/bulk-triage** (DEBT-073 remainder) — the last
   de-noise item; unlocks unattended overnight shifts.
4. **Mills dynamic workflows next slice** (Starlark engine, S1 kill-test
   already PASSED) — durable workflow definitions ride on S4's execution
   substrate rather than a parallel scheduler.

### Deferred / not this cycle

- Fleet S6 (hub child broker, clustered Postgres) and S7 (MCP Tasks adapter):
  gated on S3–S5 evidence per plan decision gates.
- Weaver S6-full/S7 registry, squad planner (per gap map).
- Legacy straggler worktrees (bold-sanderson, funny-hypatia, etc., last
  touched ≤2026-07-02): salvage-or-drop pass via
  `bin/workspace-salvage --dry-run`, separate hygiene session.

## Evidence

- Worktree sweep: `git worktree list` + per-tree `git rev-list --count
  origin/main..HEAD` classification, 2026-07-14 (59 trees; only 2 with
  unmerged recent work, both codex, both accounted for above).
- s1c-canary worktree HEAD `bc58ae92` verified content-identical to merged
  `7b0d95ee` (`git diff bc58ae92 7b0d95ee` empty; `git cherry` `-`); worktree
  removed.
- Binary provenance: `go version -m ~/.local/bin/loomd` →
  `vcs.revision=0a9680ed…`.
- Plan-store phase history entries stamped 2026-07-14 by claude-code.
