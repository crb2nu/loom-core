# Roadmap — Mills next wave (2026-08-04)

Owner: claude-code (mills-improvements session). Successor to
`190-roadmap-core-refinement-2026-07-18.md` (Waves 1–2 substantially drained)
and executor context for the `ROADMAP.md` refresh landed with this doc.
Direction (Now-wave = Mill Staff completion + Pattern Loom expansion +
MR-awareness remainder; Debate Mode stays parked) confirmed by operator
2026-08-04.

## Riskiest assumption + kill-test

**Load-bearing assumption**: the Mill Staff S2 dry-run soak is actually
producing observable evidence — agents ticking on the live policy, guarded
actions audited — so the 2026-08-08 promotion review has something to review.
(Known failure mode: fsnotify misses ConfigMap swaps; a silent no-op soak
would "pass" trivially.)

**Kill-test — RUN 2026-08-04, PASSED with caveat**: Prometheus
`increase(mills_overseer_ticks_total[24h])` → sentinel ≈279 (5-min cadence),
foreman ≈89, groomer ≈18 (hourly); `mills_overseer_actions_total` shows
audited guarded actions (foreman pause/alert/anomaly_opened/file_issue,
council.mutator dedup_skip ×3). **Caveat**: zero groomer dedup verdicts so
far — the promotion gate "zero false positives over ≥1 week" is trivially
satisfiable with zero samples. The S2p slice below therefore requires reading
the `overseer.groomer.*.dryrun` events and making verdict volume explicit
before any flip.

## State of the world (verified 2026-08-04)

- Council demand-sourcing queue fully drained: every queued/in-flight council
  plan verified shipped on main and reconciled in the plan store (7 plans →
  `merged` with evidence notes). Only live engineering item was the embedder
  fail-closed thresholds → shipped as !1417 (auto-merge armed this session).
- Mill Staff: S0 charter + S1 foundation (merge 1bad3757) + S3 attribution
  (`routeToSquadSubject`, squads outcome recorder) + guard substrate
  (`pkg/mills/guard`, council mutator + overseers riding it) all on main.
  S2 deployed dry-run via gitops 9f5f02330 (2026-08-01 18:48 -0400).
- MR-awareness: M1, M2 (`agent_mr_status` + `loom agent mr-status`), M3b
  (proxy trailer `cmd/loom/proxy_mrtrailer.go`), M4 shepherd
  (`internal/hud/mrwatch`, bounded audit log), mobile merge attention lane —
  all shipped. M3a (hook additionalContext injection) not started, gated on
  its kill-test (a).
- Pattern Loom: S0/S1/A1/B1/A2 merged (!831/!836/!840/!870). Open: live
  queued-proof kill-test, live tools-manifest probing, A2 merged-instance
  checkout wiring, B2 taste gate, cross-repo stamping.
- Roadmap intent pipe: council `preflightRoadmapIntents` + IntentsMissing gate
  live; ROADMAP.md unchecked Now/Next bullets are the intent source — the
  refresh in this MR is the demand feed for the next council cycles.

## Now-wave slices

| Slice | Scope | Gate / blocked by |
|---|---|---|
| **S2p** Alley promotion | Audit `overseer.groomer.*.dryrun` events; record verdict volume; if zero verdicts, extend soak or take an explicit low-evidence decision; then flip `allow.dedup_close` (gitops policy ConfigMap + policy-checksum bump) | soak ≥1 week → 2026-08-08 |
| **S4h** Mill Staff HUD | "Mill Staff" panel group: Drawing Office / Drawing-in / The Alley labels, recent-actions strip via `Events.ListByActorSince`; panelRegistry + createPoller conventions | — |
| **S4m** manifest decision | Enforce squad-manifest `budget_share`/`gates` at dispatch or remove from schema (live ConfigMap sets `budget_share` on both squads — removal is ConfigMap-visible) | — |
| **SHD** shed list | Verify `council/ci_incident_classifier.go` off live path → fold-or-delete; implement-or-drop council trigger keys | — |
| **PL1** Pattern Loom close-out | Live queued-proof kill-test (enqueue synthetic stamp → reconciler pickup → pause), live tools-manifest probing, A2 checkout wiring into `repo_root`, B2 taste gate | agent_pattern_* tools live (deployed) |
| **PL2** cross-repo stamping | Target-project on stamps (BacklogItem Project field or equivalent routing); retires the `target_dir` monorepo stopgap; builds on the cross-repo keystone (group token) | PL1 evidence |
| **MRA** M3a + close-out | Run MR-awareness kill-test (a); land delta-gated hook additionalContext injection (Claude/Gemini); audit shipped M4/M5 against spec §5 | kill-test (a) |

Next-wave (autonomy throughput: concurrency raise + advisory bulk-close;
embedder-degradation Grafana panel/alert; splits; OTel) and Later items are in
ROADMAP.md — unchecked bullets there are the canonical intent feed.

## Evidence

- Plan-store reconciliation + queue verification: this session's slice/phase
  updates (agent_plan_slice_update / lifecycle_advance entries, 2026-08-04).
- Soak liveness: Prometheus instant queries 2026-08-04 ~20:15Z (tick increase
  24h + actions by agent/action/outcome).
- Code-state greps for M2/M3b/M4/M5, guard substrate, textsim, incident
  threshold/banner, intent preflight: session transcript 2026-08-04.
- Gitops: `9f5f02330 ops(mills): deploy overseers — dry-run soak (Mill Staff
  S2)` on origin/main; canonical policy ConfigMap carries `overseers:` with
  all three agents `dry_run: true`.
- Embedder fail-closed: loom-core !1417 (branch
  `claude/mills-improvements-e2f674`, commit dafaf89d).
