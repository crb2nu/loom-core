import Foundation
import Testing
@testable import LoomCompanionKit

/// MillsFactoryWidget snapshot — derivation, tolerant decoding, and the
/// display semantics the widget families lean on.
@Suite("MillsWidgetData")
struct MillsWidgetDataTests {
    private let now = Date(timeIntervalSince1970: 1_800_000_000)

    private func kpi(_ metrics: [String: Double]) -> MillsKPISnapshot {
        MillsKPISnapshot(snapshotAt: now, windowSeconds: 86_400, metrics: metrics)
    }

    @Test("from() derives north-star + KPIs from the snapshot metrics")
    func derivesFromKPI() {
        let data = MillsWidgetData.from(
            kpi: kpi([
                "pipeline_merged_runs": 6,
                "queue_depth": 2,
                "active_pipeline_runs": 3,
                "pipeline_escalated_runs": 1,
                "auto_merge_rate": 0.86,
                "cost_per_merged_change_usd": 4.22,
                "gate_pass_rate": 0.91,
                "escalation_rate": 0.14,
            ]),
            runs: [], spins: [], plans: [],
            now: now
        )
        #expect(data.mergedRuns24h == 6)
        #expect(data.queueDepth == 2)
        #expect(data.activeRuns == 3)
        #expect(data.escalatedRuns == 1)
        #expect(data.autoMergeRate == 0.86)
        #expect(data.costPerMergeUSD == 4.22)
        #expect(data.lastUpdated == now)
    }

    @Test("from() falls back to run categories when the KPI snapshot is absent")
    func fallsBackToRuns() {
        let runs = [
            MillsPipelineRun(id: "A", backlogID: "b", template: "t", state: "implementing", attempts: 1),
            MillsPipelineRun(id: "B", backlogID: "b", template: "t", state: "queued", attempts: 1),
            MillsPipelineRun(id: "C", backlogID: "b", template: "t", state: "merged", attempts: 1),
        ]
        let data = MillsWidgetData.from(kpi: nil, runs: runs, spins: [], plans: [], now: now)
        #expect(data.mergedRuns24h == 0)
        #expect(data.activeRuns == 1)
        #expect(data.queueDepth == 1)
        // Terminal run (merged) never appears in the pipeline rows.
        #expect(data.pipelines.map(\.id).sorted() == ["A", "B"])
    }

    @Test("from() ranks live roots first and caps the pipeline rows")
    func ranksAndCapsPipelines() {
        let runs = [
            MillsPipelineRun(id: "queued-old", backlogID: "b", template: "t", state: "queued",
                             attempts: 1, startedAt: now.addingTimeInterval(-900)),
            MillsPipelineRun(id: "slice", backlogID: "b", template: "t", state: "implementing",
                             attempts: 1, startedAt: now.addingTimeInterval(-100),
                             parentRunID: "root", depth: 1),
            MillsPipelineRun(id: "root", backlogID: "b", template: "t", state: "implementing",
                             attempts: 1, startedAt: now.addingTimeInterval(-300), depth: 0),
        ]
        let data = MillsWidgetData.from(kpi: nil, runs: runs, spins: [], plans: [],
                                        pipelineLimit: 2, now: now)
        // Live before queued, root before its deeper slice; capped at 2.
        #expect(data.pipelines.map(\.id) == ["root", "slice"])
    }

    @Test("from() counts plan-board pressure by phase")
    func countsPlanPressure() {
        let plans = [
            MillsPlan(id: "1", title: "a", phase: "draft"),
            MillsPlan(id: "2", title: "b", phase: "draft"),
            MillsPlan(id: "3", title: "c", phase: "in_progress"),
            MillsPlan(id: "4", title: "d", phase: "planned"),
            MillsPlan(id: "5", title: "e", phase: "merged"),
            MillsPlan(id: "6", title: "f", phase: "abandoned"),
        ]
        let data = MillsWidgetData.from(kpi: nil, runs: [], spins: [], plans: plans, now: now)
        #expect(data.draftPlans == 2)
        #expect(data.activePlans == 2)
    }

    @Test("from() maps spins via the shared board semantics")
    func mapsSpins() {
        let spins = [
            MillsSpinRun(id: "live", brief: "b", frames: ["jacquard"], status: "running",
                         startedAt: now.addingTimeInterval(-60)),
            MillsSpinRun(id: "stale", brief: "b", frames: ["warp"], status: "succeeded",
                         planIDs: ["p"], startedAt: now.addingTimeInterval(-90_000),
                         endedAt: now.addingTimeInterval(-90_000)),
        ]
        let data = MillsWidgetData.from(kpi: nil, runs: [], spins: spins, plans: [], now: now)
        #expect(data.spins.map(\.id) == ["live"])
        #expect(data.hasLiveSpin)
    }

    @Test("floorLine tells one coherent story per state")
    func floorLine() {
        #expect(MillsWidgetData(operatorReachable: false).floorLine == "operator offline")
        #expect(MillsWidgetData(activeRuns: 2, queueDepth: 1, escalatedRuns: 1).floorLine
            == "2 running · 1 queued · 1 escalated")
        #expect(MillsWidgetData(escalatedRuns: 2).floorLine == "2 escalated — needs you")
        #expect(MillsWidgetData(queueDepth: 3).floorLine == "3 queued")
        #expect(MillsWidgetData().floorLine == "floor idle")
    }

    @Test("snapshot round-trips through the store codec")
    func codableRoundTrip() throws {
        let original = SharedDataStore.placeholderMills
        let encoded = try JSONEncoder().encode(original)
        let decoded = try JSONDecoder().decode(MillsWidgetData.self, from: encoded)
        #expect(decoded.mergedRuns24h == original.mergedRuns24h)
        #expect(decoded.pipelines == original.pipelines)
        #expect(decoded.spins == original.spins)
        #expect(decoded.draftPlans == original.draftPlans)
    }

    @Test("decoder tolerates an older schema (missing keys → defaults)")
    func tolerantDecode() throws {
        let json = #"{"mergedRuns24h": 4}"#.data(using: .utf8)!
        let decoded = try JSONDecoder().decode(MillsWidgetData.self, from: json)
        #expect(decoded.mergedRuns24h == 4)
        #expect(decoded.operatorReachable)
        #expect(decoded.pipelines.isEmpty)
        #expect(decoded.autoMergeRate == nil)
    }

    @Test("loom://mills deep link round-trips")
    func millsDeepLink() throws {
        let link = DeepLink.from(URL(string: "loom://mills")!)
        #expect(link == .mills)
        #expect(DeepLink.mills.urlString == "loom://mills")
        #expect(DeepLink.mills.destinationGroup == .mills)
    }
}
