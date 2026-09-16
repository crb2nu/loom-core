import Foundation
import Testing
@testable import LoomCompanionKit

/// Operator status card — wire decoding against a captured production payload
/// plus the verdict / budget / yield semantics the card renders.
@Suite("MillsOperatorStatus")
struct MillsOperatorStatusTests {

    /// Trimmed from a live `GET /api/mills/status` (2026-09-01, build
    /// c9fe3ed7): the shape the HUD proxies verbatim. Dates carry Go's
    /// nanosecond precision, `last_delta_at` is null, `budget` is a map.
    static let liveJSON = """
    {
      "active_pipeline_runs": 0,
      "autonomy_blockers": null,
      "autonomy_ready": true,
      "budget": {
        "council": {"spent_usd": 2.7743725996, "cap_usd": 50, "runs": 4, "runs_cap": 0},
        "pipeline": {"spent_usd": 2.6389679999999998, "cap_usd": 75, "runs": 11, "runs_cap": 60}
      },
      "build_sha": "c9fe3ed7",
      "capabilities": [
        {"id": "sqlite_store", "status": "green", "mode": "real", "required_for_autonomy": true, "last_checked_at": "2026-09-01T22:10:56Z", "message": "canonical SQLite store is reachable"},
        {"id": "hud_spawn", "status": "red", "mode": "stub", "required_for_autonomy": true, "last_checked_at": "2026-09-01T22:10:56Z", "message": "HUD spawn client is not configured"},
        {"id": "kpi_writer", "status": "yellow", "mode": "real", "required_for_autonomy": false, "last_checked_at": "2026-09-01T22:10:56Z", "message": "KPI writer lagging"}
      ],
      "council_yield": {
        "runs_since_last_delta": 5,
        "cost_since_last_delta_usd": 3.4837,
        "last_delta_at": null,
        "sample_size": 5
      },
      "db_ok": true,
      "gitlab_base_url": "https://gitlab.example",
      "health_gates": {
        "allowed": true,
        "fail_closed": false,
        "status": "pass",
        "checked_at": "2026-09-01T22:10:56.69612803Z",
        "components": [{"name": "mills-store", "state": "healthy", "critical": true}]
      },
      "health_gates_mode": "observe",
      "last_council_at": "2026-09-01T18:00:16.24599927Z",
      "last_merge_at": "2026-09-01T14:01:17.830746597Z",
      "policy_enabled": true,
      "policy_version": 2,
      "queue_depth": 1,
      "slice": "2.4-rest-surface"
    }
    """

    /// Decodes through the real client's raw decoder (Go RFC3339Nano dates,
    /// bare bodies) so the test exercises the production date strategy.
    private func decode(_ json: String) throws -> MillsOperatorStatus {
        let client = APIClient(baseURL: URL(string: "https://localhost:3333")!, token: "test-token")
        return try client.decodeRaw(Data(json.utf8), statusCode: 200)
    }

    @Test("decodes the live operator payload")
    func decodesLivePayload() throws {
        let status = try decode(Self.liveJSON)
        #expect(status.buildSHA == "c9fe3ed7")
        #expect(status.dbOK)
        #expect(status.policyEnabled)
        #expect(status.policyVersion == 2)
        #expect(status.autonomyReady == true)
        #expect(status.autonomyBlockers == nil)
        #expect(status.queueDepth == 1)
        #expect(status.activePipelineRuns == 0)
        #expect(status.lastCouncilAt != nil)
        #expect(status.lastMergeAt != nil)
        #expect(status.healthGates?.allowed == true)
        #expect(status.healthGates?.status == "pass")
        #expect(status.healthGatesMode == "observe")
        #expect(status.capabilities?.count == 3)
        #expect(status.budget?["pipeline"]?.runsCap == 60)
        #expect(status.budget?["council"]?.runsCap == 0)
        #expect(status.councilYield?.runsSinceLastDelta == 5)
        #expect(status.councilYield?.lastDeltaAt == nil)
        #expect(status.councilYield?.sampleSize == 5)
    }

    @Test("a sparse older-operator payload decodes to unknowns, not an error")
    func decodesSparsePayload() throws {
        let status = try decode(#"{"db_ok": true, "policy_enabled": true, "policy_version": 1, "slice": "1.2-skeleton"}"#)
        #expect(status.autonomyReady == nil)
        #expect(status.verdict == .unknown)
        #expect(status.capabilityRollup == nil)
        #expect(status.budgetTiers.isEmpty)
        #expect(status.councilYield == nil)
        #expect(!status.needsAttention)
    }

    @Test("verdict precedence: paused > held > blocked > ready")
    func verdictPrecedence() {
        let blocked = MillsOperatorStatus(policyEnabled: true, autonomyReady: false, autonomyBlockers: ["hud_spawn is red"])
        #expect(blocked.verdict == .blocked(reasons: ["hud_spawn is red"]))

        let paused = MillsOperatorStatus(policyEnabled: false, autonomyReady: false, autonomyBlockers: ["x"])
        #expect(paused.verdict == .paused)

        let heldEnforced = MillsOperatorStatus(
            policyEnabled: true, autonomyReady: true,
            healthGates: MillsHealthGates(allowed: false, failClosed: true, status: "block", reasons: ["mills-store unhealthy"]),
            healthGatesMode: "enforce")
        #expect(heldEnforced.verdict == .held(reasons: ["mills-store unhealthy"]))

        // Observe mode reports but does not hold: the verdict stays ready and
        // the block surfaces only as attention.
        let heldObserved = MillsOperatorStatus(
            policyEnabled: true, autonomyReady: true,
            healthGates: MillsHealthGates(allowed: false, status: "block", reasons: ["x"]),
            healthGatesMode: "observe")
        #expect(heldObserved.verdict == .ready)
        #expect(heldObserved.needsAttention)

        let ready = MillsOperatorStatus(policyEnabled: true, autonomyReady: true)
        #expect(ready.verdict == .ready)
        #expect(!ready.needsAttention)
    }

    @Test("blocked verdict falls back to degraded required capabilities when blockers are empty")
    func blockedFallsBackToCapabilities() throws {
        let status = MillsOperatorStatus(
            policyEnabled: true, autonomyReady: false, autonomyBlockers: [],
            capabilities: [
                MillsCapability(id: "kpi_writer", status: "yellow", requiredForAutonomy: false),
                MillsCapability(id: "hud_spawn", status: "red", mode: "stub", requiredForAutonomy: true),
                MillsCapability(id: "sqlite_store", status: "green", requiredForAutonomy: true),
            ])
        #expect(status.verdict == .blocked(reasons: ["hud_spawn is red", "kpi_writer is yellow"]))
        #expect(status.degradedCapabilities.map(\.id) == ["hud_spawn", "kpi_writer"])
        #expect(status.capabilityRollup == "1/3 green")
    }

    @Test("budget tiers order pipeline first and compute headroom")
    func budgetTiers() throws {
        let status = try decode(Self.liveJSON)
        let tiers = status.budgetTiers
        #expect(tiers.map(\.name) == ["pipeline", "council"])
        #expect(abs(tiers[0].tier.spentFraction - 2.638968 / 75) < 1e-6)
        #expect(!tiers[0].tier.isNearCap)

        let hot = MillsBudgetTier(spentUSD: 64, capUSD: 75, runs: 40, runsCap: 60)
        #expect(hot.isNearCap)
        #expect(MillsBudgetTier(spentUSD: 9, capUSD: 0).spentFraction == 0)
        #expect(!MillsBudgetTier(spentUSD: 9, capUSD: 0).isNearCap)

        let extra = MillsOperatorStatus(budget: ["spin": MillsBudgetTier(spentUSD: 1, capUSD: 2), "council": MillsBudgetTier()])
        #expect(extra.budgetTiers.map(\.name) == ["council", "spin"])
    }

    @Test("council yield summary mirrors the CLI wording")
    func councilYieldSummary() {
        let now = Date(timeIntervalSince1970: 1_800_000_000)
        #expect(MillsCouncilYield().summary(now: now) == "No finished council runs yet.")
        #expect(MillsCouncilYield(sampleSize: 3).summary(now: now) == "Last council run produced backlog deltas.")

        let spell = MillsCouncilYield(runsSinceLastDelta: 5, costSinceLastDeltaUSD: 3.48, sampleSize: 5)
        #expect(spell.isDrySpell)
        #expect(spell.summary(now: now) == "5 runs without a backlog delta ($3.48) · none in the last 5 examined")

        let one = MillsCouncilYield(runsSinceLastDelta: 1, costSinceLastDeltaUSD: 0.7,
                                    lastDeltaAt: now.addingTimeInterval(-3 * 3600), sampleSize: 2)
        #expect(!one.isDrySpell)
        #expect(one.summary(now: now) == "1 run without a backlog delta ($0.70) · last delta 3h ago")
    }

    @Test("live payload flags attention on the council dry spell alone")
    func livePayloadAttention() throws {
        let status = try decode(Self.liveJSON)
        // autonomy_ready is true on the wire, so the verdict is ready even
        // though hud_spawn is red in the (test-edited) matrix …
        #expect(status.verdict == .ready)
        // … but the 5-run council dry spell still lights the card.
        #expect(status.needsAttention)
        #expect(status.councilYield?.isDrySpell == true)
    }
}
