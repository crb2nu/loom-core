# RALPH Iteration Plan — iOS Recovery-SLO Telemetry Uploader (2026-06-08)

## Review

- Roadmap milestone: Mobile App + HUD Polish; gap-to-backlog `.loom/32` **MBL-5: SSE
  resilience + fallback SLOs** — last unchecked item: "Publish recovery SLO telemetry to
  a cross-surface (HUD) dashboard", decomposed into 3 slices.
- Slice 1 (`.loom/138`, merged `!670`/`d1f989f8`): backend ingest/aggregate/read API —
  `POST`/`GET /api/mobile/v1/telemetry/recovery`, in-memory `recoveryStore`, `mobile:telemetry`
  scope off by default, server p95 matches the iOS nearest-rank formula.
- This is **slice 2 of 3**: the iOS uploader that POSTs a device's rolling
  `ConnectionHealthMonitor.recoverySampleSeconds` window to the slice-1 ingest endpoint.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The slice-1 contract is exactly what the device can emit with
no transform — POSTing the raw `recoverySampleSeconds` array as `samples` + the constant
`recoveryP95TargetSeconds` as `slo_target_seconds`, the server **replaces** the device's
snapshot, and the existing `APIClient` already attaches the `X-Device-ID` keying header.

**Kill test**: Read the merged slice-1 source: `recovery_telemetry.go:96-115` (`Ingest`
**replaces** `s.devices[deviceID]` — full-window resend is idempotent, no accumulation);
`handler_telemetry.go:14-15` (body `{samples, slo_target_seconds}`); `APIClient.applyAuthHeaders`
sets `X-Device-ID` for every request when `deviceId` is non-empty. → All three hold, so the
uploader is pure wiring (no contract change, no on-device recompute). **Status**: passed
2026-06-08 (verified against merged slice-1 code, this session).

**Failure mode if wrong**: duplicate/accumulating server-side samples, a missing device key,
or an on-device transform that diverges from the in-app p95 operators already see.

## Align

- Slice name: **iOS recovery-telemetry uploader → backend ingest endpoint.**

- Scope in:
  - `Endpoint.recoveryTelemetryUpload(samples:sloTargetSeconds:)` — POST to
    `/api/mobile/v1/telemetry/recovery`, `isMutation == true`, JSON body
    `{ "samples": [Double], "slo_target_seconds": Double }`.
  - `RecoveryTelemetryAck` decodable (`{ accepted: Bool }`, from the standard envelope `data`).
  - `RecoveryTelemetryUploader` (Kit actor) — snapshots the window and POSTs it; **dedups**
    an unchanged window; **stops** retrying after a 403 (scope off by default — disciplined
    ingress); backs off on 429. Returns a typed `RecoveryUploadOutcome` for testability.
  - `ConnectionHealthMonitor.onRecovery` hook — fired in `recordRecovery` with a snapshot of
    the rolling window (symmetric to the existing `onPollRefresh`).
  - Wire in `ContentView.setupSSE()` (owns the `apiClient`); cleared in `teardownSSE()`.
  - Swift tests (swift-testing): endpoint path/method/mutation/body; ack decode via
    `decodeResponse`; uploader empty-skip / dedup / success / scope-denied-stop / 429 / retry.
  - Extend `MockAPIClient` for the new endpoint case.

- Scope out:
  - HUD Svelte recovery-SLO tile reading the aggregate — **slice 3**.
  - Granting the `mobile:telemetry` scope to device tokens — a deployment/config concern; the
    uploader degrades gracefully (403 → stop) until the scope is enabled.
  - Background/periodic upload scheduling — recovery-triggered + dedup is sufficient for v1.

- Acceptance criteria:
  1. `Endpoint.recoveryTelemetryUpload` builds `POST /api/mobile/v1/telemetry/recovery` with
     `Content-Type: application/json` and body `{samples, slo_target_seconds}`; `isMutation==true`.
  2. `RecoveryTelemetryAck` decodes from the `{ok,data,meta}` envelope's `data.accepted`.
  3. Uploader: empty window → `.skippedEmpty` (no request); identical window twice →
     `.skippedDuplicate` on the 2nd (one request total); success → `.uploaded` + dedup armed.
  4. Uploader: 403 → `.scopeDenied` and **all** subsequent calls short-circuit to `.scopeDenied`
     (no further requests); 429 → `.rateLimited` (dedup NOT armed → retried later); other error →
     `.failed` (dedup NOT armed).
  5. Monitor fires `onRecovery` with the current window exactly when a transient outage resolves
     (a recovery sample is recorded); existing recovery-telemetry tests stay green.
  6. iOS app builds (`xcodebuild` BUILD SUCCEEDED); Kit suite green (301 → 301+N).

- Dependencies/blockers: none. Branch `feat/mobile-recovery-slo-uploader` off `main`
  (`d1f989f8`).

## Land

- Planned file areas:
  - `apps/loom-companion-ios/Sources/LoomCompanionKit/Networking/Endpoint.swift` — new case + method/path/isMutation/body.
  - `apps/loom-companion-ios/Sources/LoomCompanionKit/Networking/RecoveryTelemetryUploader.swift` (new) — uploader actor + outcome enum + ack model.
  - `apps/loom-companion-ios/Sources/LoomCompanionKit/Services/ConnectionHealthMonitor.swift` — `onRecovery` hook.
  - `apps/loom-companion-ios/Sources/LoomCompanion/ContentView.swift` — own/wire/teardown the uploader.
  - `apps/loom-companion-ios/Tests/LoomCompanionKitTests/Networking/RecoveryTelemetryUploaderTests.swift` (new).
  - `apps/loom-companion-ios/Tests/LoomCompanionKitTests/Networking/APIClientTests.swift` — endpoint + ack-decode cases.
  - `apps/loom-companion-ios/Tests/LoomCompanionKitTests/MockAPIClient.swift` — new endpoint case.
  - `.loom/32-mobile-gap-to-backlog-map.md` — MBL-5 progress note.

## Prove

- Tests: `swift test` (Package.swift; LoomCompanionKit suite) — full suite, expect 301→301+N.
- App compile: `xcodebuild -project LoomCompanion.xcodeproj -scheme LoomCompanion -sdk iphonesimulator -destination 'platform=iOS Simulator,name=iPhone 17 Pro,OS=26.2' build`.
- No Go changes this slice. CI: GitLab pipeline to terminal; fix-and-retry on red.

## Handoff/Harvest

- Docs: `.loom/32` MBL-5 — check the "iOS uploader" sub-item; note HUD tile is the last slice.
- Agent-context: decision (full-window resend idempotent via Ingest-replace; 403 → stop for
  disciplined ingress), finding (no contract change required — slice-1 contract verified faithful).
- Next-slice candidate: (3) HUD Svelte recovery-SLO tile reading `GET /api/mobile/v1/telemetry/recovery`.
