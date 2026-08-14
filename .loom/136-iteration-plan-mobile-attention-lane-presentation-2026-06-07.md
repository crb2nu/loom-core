# RALPH Iteration Plan — Mobile Attention-Lane Presentation Alignment (2026-06-07)

## Review

- Roadmap milestone: Mobile App + HUD Polish (`.loom/72-implementation-plan-mobile-hud-polish-2026-03-31.md`), Phase 2 "Mobile Shell + Flow Decomposition". Slice A (OpsView decomposition) shipped at `e665fe4e`.
- Spec section(s): `.loom/71-product-spec-mobile-hud-polish-2026-03-31.md` (attention-first home surface), `.loom/32-mobile-gap-to-backlog-map.md`.
- Prior decisions to preserve:
  - "Mobile and HUD polish should prioritize IA over new feature expansion" (codex-planner, 2026-03-31).
  - Mobile coordination/attention vocabulary is shared backend language; don't invent new product language (2026-04-01 addendum to plan 72).

## Align

- Slice name: **Centralize attention-lane presentation in `LoomCompanionKit`, aligned with the live mobile backend lane contract.**

- Problem (finding): The mobile dashboard's two primary triage surfaces — the hero `NextActionCard` and the `AttentionLanesCard` queue — each carry their own copy of a lane `type` → (icon, title) switch. Both switches key off a HUD-flavored vocabulary (`approval`, `degraded_server`, `idle_agent`, `handoff`, `merge_conflict`) that the mobile dashboard backend **never emits**. `internal/hud/domain/mobile.buildMobileAttentionLanes` emits exactly four `type` values — `agent`, `namespace`, `merge`, `conflict` (frozen in `internal/contracts/testdata/mobile_attention_lanes.golden`). Of those, only `conflict` is matched; `agent`, `namespace`, and `merge` fall through to a generic `flag.fill` icon on the most prominent mobile surfaces.

- Scope in:
  - New Kit type `AttentionLaneKind` + `DashboardAttentionLane.kind` classifier (single source of truth).
  - Kit presentation: `heroIcon`, `rowIcon`, `singularTitle`, `aggregateTitleIfKnown`.
  - Refactor `NextActionCard` and `AttentionLanesCard` to consume the Kit type for **icons** and final-fallback titles.
  - Kit unit tests covering all four live backend types + legacy types + `other`.
  - Go producer test exercising the real `buildMobileAttentionLanes` and asserting the emitted `type` allowlist (pins the cross-language contract; the existing golden hand-builds lanes and never calls the producer).

- Scope out:
  - No backend lane-vocabulary changes (the contract stays `agent`/`namespace`/`merge`/`conflict`).
  - No change to lane **routing/nav** semantics (preserve current People/Work/Connection mapping) beyond re-expressing it via `kind`.
  - No change to the rich title-composition logic (prefer non-generic label, then summary); `kind` only supplies the last-resort fallback.
  - No new mobile capabilities or mutation surfaces.

- Acceptance criteria:
  1. `agent` lanes render a person icon (not `flag.fill`); `merge` lanes render a merge icon; on both the hero and queue cards.
  2. Lane → icon/title resolution lives in exactly one place (`LoomCompanionKit`); both cards delegate.
  3. Classifier maps `merge` (which carries `target_kind: task_filter`) to `.merge`, not `.work` — i.e. explicit `type` wins over the `isTaskLane` fallback.
  4. `swift test --package-path apps/loom-companion-ios` green, including new `AttentionLaneKind` tests.
  5. New Go test proves `buildMobileAttentionLanes` only ever emits `type ∈ {agent, namespace, merge, conflict}`; `go test ./internal/hud/domain/mobile/... ./internal/contracts/...` green.

- Dependencies/blockers: none. Branch `feat/mobile-attention-lane-presentation` off `origin/main` (`cec3d274`).

## Riskiest assumption + kill-test

**Load-bearing assumption**: The mobile dashboard endpoint emits attention-lane `type` values from a closed set `{agent, namespace, merge, conflict}`, so the iOS presentation table can be authored against exactly those.

**Kill test**: `go test ./internal/hud/domain/mobile -run TestBuildMobileAttentionLanes_TypeContract` — drives the real producer with a snapshot that triggers every lane branch and asserts each emitted `type` is in the allowlist. Cross-checked against the frozen `mobile_attention_lanes.golden`.

**Failure mode if wrong**: a backend lane type outside the set would fall to `.other` (generic flag) — degraded, not broken. Non-fatal.

**Status**: passed 2026-06-07 (see Prove).

Secondary (cosmetic) assumption: SF Symbol `arrow.triangle.pull` renders on iOS 17. If absent it shows a placeholder glyph — manual-verify item, not a test/build failure.

## Land

- Planned file areas:
  - `apps/loom-companion-ios/Sources/LoomCompanionKit/Models/AttentionLaneKind.swift` (new)
  - `apps/loom-companion-ios/Tests/LoomCompanionKitTests/Models/AttentionLaneKindTests.swift` (new)
  - `apps/loom-companion-ios/Sources/LoomCompanion/Views/Dashboard/NextActionCard.swift`
  - `apps/loom-companion-ios/Sources/LoomCompanion/Views/Dashboard/AttentionLanesCard.swift`
  - `internal/hud/domain/mobile/helpers_test.go` (new or extend)
- Implementation steps:
  1. Add `AttentionLaneKind` enum + `DashboardAttentionLane.kind` (type-first, `isTaskLane` fallback) + static `init(typeKey:)` + `heroIcon`/`rowIcon`/`singularTitle`/`aggregateTitleIfKnown`.
  2. Refactor `NextActionCard`: icon ← `lane.kind.heroIcon`; nav via `kind`; last-resort title ← `kind.singularTitle`.
  3. Refactor `AttentionLanesCard`: row icon ← `kind.rowIcon`; group title known-check ← `kind.aggregateTitleIfKnown`.
  4. Add `AttentionLaneKindTests` (classification + icons + titles).
  5. Add Go `TestBuildMobileAttentionLanes_TypeContract`.

## Prove

- Tests to run:
  - `swift test --package-path apps/loom-companion-ios`
  - `go test ./internal/hud/domain/mobile/... ./internal/contracts/... -count=1`
- Lint/static checks: `gofmt`/`go vet` via pre-commit; `swift build` clean.
- CI checks: GitLab pipeline to terminal state; fix-and-retry on red.

## Handoff/Harvest

- Docs to update: mark this slice in `.loom/72` continuation log; record decision in agent context.
- Agent-context entries to add: decision (centralized lane presentation), finding (View↔backend lane vocab drift).
- Next-slice candidates: MBL-5 SSE recovery SLO telemetry; Phase 2 Slice B (Connection/Settings simplification); extend `kind` routing into `OpsView` lane surfaces if any.
