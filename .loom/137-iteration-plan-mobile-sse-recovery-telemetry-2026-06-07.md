# RALPH Iteration Plan — Mobile SSE Recovery Telemetry (2026-06-07)

## Review

- Roadmap milestone: Mobile App + HUD Polish; gap-to-backlog `.loom/32-mobile-gap-to-backlog-map.md` **MBL-5: SSE resilience + fallback SLOs** — status *In progress*, one unchecked item: "Publish recovery SLO telemetry dashboard". Acceptance also lists "Disconnect-to-recovered p95 target defined and measured" (was unmeasured).
- Prior decisions to preserve: SSE → poll fallback → SSE recovery is implemented and tested (`ConnectionHealthMonitor`, `SSENetworkChurnTests`). Don't regress those transitions.
- Prior slice (this loop): `.loom/136` attention-lane presentation (MR !667, merged `b0a402b2`).

## Align

- Slice name: **Instrument disconnect-to-recovered telemetry in `ConnectionHealthMonitor` and surface it in connection diagnostics.**

- Scope in:
  - Measure each transient outage (healthy → degradedStream/unreachable/rateLimited → healthy) as a recovery duration, using an injectable clock for deterministic tests.
  - Expose observable telemetry: `degradedSince`, `lastRecoveryDuration`, `recoverySampleSeconds` (rolling, capped), computed `recoveryStats` (count/mean/p95), `meetsRecoverySLO`, `currentOutageSeconds()`.
  - Define the SLO target: p95 ≤ 30s (one poll-fallback cycle).
  - Surface a one-line recovery summary in `ConnectionDiagnosticsView`.
  - Kit unit tests for measurement, exclusions, p95/mean, SLO verdict, rolling cap.

- Scope out:
  - HUD web dashboard / backend SLO aggregation (cross-surface) — follow-up.
  - No change to existing health-state transition semantics or polling behavior.
  - Auth/permission/gateway config errors excluded from the transient-recovery distribution (they don't self-recover).

- Acceptance criteria:
  1. Disconnect-to-recovered duration is measured and observable; p95/mean/count exposed.
  2. Cold-start failures (never healthy) and non-transient config errors are not counted.
  3. A multi-step outage is timed once, end to end.
  4. `swift test` green incl. new telemetry tests; existing `ConnectionHealthTests` + `SSENetworkChurnTests` still pass.
  5. `xcodebuild -sdk iphonesimulator` BUILD SUCCEEDED (diagnostics surface compiles).

- Dependencies/blockers: none. Branch `feat/mobile-sse-recovery-telemetry` off `origin/main` (`b0a402b2`).

## Land

- Planned file areas:
  - `apps/loom-companion-ios/Sources/LoomCompanionKit/Services/ConnectionHealthMonitor.swift`
  - `apps/loom-companion-ios/Tests/LoomCompanionKitTests/Networking/ConnectionRecoveryTelemetryTests.swift` (new)
  - `apps/loom-companion-ios/Sources/LoomCompanion/Views/Connection/ConnectionDiagnosticsView.swift`
  - `.loom/32-mobile-gap-to-backlog-map.md` (mark MBL-5 progress)
- Implementation steps:
  1. Route all health mutations through a private `apply(_:)` that records transient-outage start/recovery via an injected `now`.
  2. Add `SSERecoveryStats` + observable telemetry properties + p95/SLO computeds.
  3. Surface a recovery summary line in `ConnectionDiagnosticsView.statusDetails`.
  4. Add `ConnectionRecoveryTelemetryTests`.

## Prove

- Tests to run: `swift test --package-path apps/loom-companion-ios`; `xcodebuild -scheme LoomCompanion -sdk iphonesimulator ... build`.
- Lint/static checks: pre-commit (gofmt/goimports/vet/golangci-lint/build — Go untouched; hooks still run).
- CI checks: GitLab pipeline to terminal; fix-and-retry on red.

## Handoff/Harvest

- Docs to update: `.loom/32` MBL-5 (mark telemetry checkbox; note p95 target + measurement).
- Agent-context entries: decision (recovery telemetry + SLO target), finding (no measurement existed).
- Next-slice candidates: HUD-side recovery SLO aggregation/dashboard (cross-surface); Phase 2 Slice B Connection/Settings simplification; widget surfacing of recovery health.
