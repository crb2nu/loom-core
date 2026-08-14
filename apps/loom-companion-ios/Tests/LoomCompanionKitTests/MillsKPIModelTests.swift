import Foundation
import Testing
@testable import LoomCompanionKit

/// Covers the Mills KPI/pipeline semantics layer. The headline regression test
/// pins the canonical metric keys: the mobile screen previously read keys the
/// operator never publishes, so every KPI rendered "—". These tests fail if the
/// key set drifts from pkg/mills/kpi_writer.go again.
@Suite("MillsKPIModel")
struct MillsKPIModelTests {

    // MARK: - Canonical KPI keys (regression for the silent "—" bug)

    @Test("canonicalCards uses the keys the operator actually publishes")
    func canonicalKeysProduceCards() {
        let snap = MillsKPISnapshot(metrics: [
            "auto_merge_rate": 0.86,
            "cost_per_merged_change_usd": 4.22,
            "slice_to_merge_p50_seconds": 1_320,
            "gate_pass_rate": 0.91,
            "escalation_rate": 0.14,
            "council_roi": 1.4,
        ])
        let cards = snap.canonicalCards
        let byID = Dictionary(uniqueKeysWithValues: cards.map { ($0.id, $0) })

        #expect(byID["auto_merge_rate"]?.value == "86%")
        #expect(byID["cost_per_merged"]?.value == "$4.22")
        #expect(byID["slice_to_merge_p50"]?.value == "22.0m")
        #expect(byID["gate_pass_rate"]?.value == "91%")
        #expect(byID["escalation_rate"]?.value == "14%")
        #expect(byID["council_roi"]?.value == "1.4×")
    }

    @Test("the old (wrong) metric keys produce no cards")
    func legacyKeysProduceNothing() {
        // These are exactly the keys the screen used to read. If someone
        // reintroduces them as the source of truth, this test catches it:
        // none of them are canonical, so the grid stays empty.
        let snap = MillsKPISnapshot(metrics: [
            "pipeline_merge_rate": 0.93,
            "council_cost_per_day_usd": 4.22,
            "audit_findings_count": 7,
        ])
        #expect(snap.canonicalCards.isEmpty)
    }

    @Test("cost falls back to pipeline cost when change cost is absent")
    func costFallback() {
        let snap = MillsKPISnapshot(metrics: ["cost_per_merged_pipeline_usd": 12.5])
        let card = snap.canonicalCards.first { $0.id == "cost_per_merged" }
        #expect(card?.value == "$12.5")
    }

    @Test("absent metrics are dropped, not rendered as em dashes")
    func sparseSnapshotDropsMissing() {
        let snap = MillsKPISnapshot(metrics: ["auto_merge_rate": 0.5])
        #expect(snap.canonicalCards.count == 1)
        #expect(snap.canonicalCards.first?.id == "auto_merge_rate")
    }

    // MARK: - North-star + system summary

    @Test("mergedRuns24h and system summary read the right keys")
    func systemSummary() {
        let snap = MillsKPISnapshot(metrics: [
            "pipeline_merged_runs": 6,
            "queue_depth": 2,
            "active_pipeline_runs": 3,
        ])
        let s = snap.systemSummary
        #expect(s.mergedRuns == 6)
        #expect(s.queueDepth == 2)
        #expect(s.activeRuns == 3)
        #expect(s.isBusy == true)
    }

    @Test("system summary defaults to zero / not-busy on an empty snapshot")
    func systemSummaryDefaults() {
        let s = MillsKPISnapshot(metrics: [:]).systemSummary
        #expect(s.mergedRuns == 0)
        #expect(s.isBusy == false)
    }

    // MARK: - Threshold badges

    @Test("gate pass rate badges on/watch/off the 0.85 target")
    func gatePassBadges() {
        func status(_ v: Double) -> MillsMetricStatus? {
            MillsKPISnapshot(metrics: ["gate_pass_rate": v])
                .canonicalCards.first { $0.id == "gate_pass_rate" }?.status
        }
        #expect(status(0.90) == .onTarget)   // >= target
        #expect(status(0.80) == .watch)      // within softMargin (0.765..0.85)
        #expect(status(0.50) == .offTarget)  // well below
    }

    @Test("regression rate is flagged as a proxy metric")
    func regressionProxy() {
        let card = MillsKPISnapshot(metrics: ["regression_rate": 0.0])
            .canonicalCards.first { $0.id == "regression_rate" }
        #expect(card?.proxy == true)
        #expect(card?.status == .onTarget)  // 0% <= 0.02 target
    }

    // MARK: - Pipeline state categorization

    @Test("categorize buckets free-form operator states")
    func categorize() {
        #expect(MillsRunCategory.categorize("implementing") == .running)
        #expect(MillsRunCategory.categorize("TESTING") == .running)
        #expect(MillsRunCategory.categorize("reviewing") == .review)
        #expect(MillsRunCategory.categorize("merging") == .merging)
        #expect(MillsRunCategory.categorize("queued") == .queued)
        #expect(MillsRunCategory.categorize("escalated") == .escalated)
        #expect(MillsRunCategory.categorize("failed") == .failed)
        #expect(MillsRunCategory.categorize("done") == .done)
        #expect(MillsRunCategory.categorize("something-new") == .unknown)
    }

    @Test("terminal + live flags are consistent")
    func categoryFlags() {
        #expect(MillsRunCategory.done.isTerminal == true)
        #expect(MillsRunCategory.escalated.isTerminal == true)
        #expect(MillsRunCategory.failed.isTerminal == true)
        #expect(MillsRunCategory.running.isTerminal == false)
        #expect(MillsRunCategory.running.isLive == true)
        #expect(MillsRunCategory.queued.isLive == false)
    }

    @Test("run displayState humanizes the raw state")
    func displayState() {
        let run = MillsPipelineRun(id: "P", backlogID: "B", template: "t",
                                   state: "in_progress", attempts: 1)
        #expect(run.displayState == "In progress")
        #expect(run.category == .running)
    }

    // MARK: - Pipeline tree grouping

    @Test("build groups children under their root, preserving order")
    func treeGrouping() {
        let runs = [
            MillsPipelineRun(id: "R1", backlogID: "B", template: "t", state: "implementing", attempts: 1, depth: 0),
            MillsPipelineRun(id: "R1-A", backlogID: "B", template: "t", state: "testing", attempts: 1, parentRunID: "R1", depth: 1),
            MillsPipelineRun(id: "R2", backlogID: "B", template: "t", state: "queued", attempts: 1, depth: 0),
        ]
        let nodes = MillsPipelineTree.build(from: runs)
        #expect(nodes.count == 2)
        #expect(nodes[0].run.id == "R1")
        #expect(nodes[0].children.map(\.id) == ["R1-A"])
        #expect(nodes[1].run.id == "R2")
        #expect(nodes[1].children.isEmpty)
    }

    @Test("deep descendants attach to the top root, ordered by depth")
    func treeMultiLevel() {
        let runs = [
            MillsPipelineRun(id: "R", backlogID: "B", template: "t", state: "implementing", attempts: 1, depth: 0),
            MillsPipelineRun(id: "D2", backlogID: "B", template: "t", state: "testing", attempts: 1, parentRunID: "D1", depth: 2),
            MillsPipelineRun(id: "D1", backlogID: "B", template: "t", state: "reviewing", attempts: 1, parentRunID: "R", depth: 1),
        ]
        let nodes = MillsPipelineTree.build(from: runs)
        #expect(nodes.count == 1)
        #expect(nodes[0].run.id == "R")
        // D1 (depth 1) sorts before D2 (depth 2) even though D2 appears first.
        #expect(nodes[0].children.map(\.id) == ["D1", "D2"])
    }

    @Test("orphan child (parent absent) surfaces as its own root")
    func treeOrphan() {
        let runs = [
            MillsPipelineRun(id: "C", backlogID: "B", template: "t", state: "testing", attempts: 1, parentRunID: "GONE", depth: 1),
        ]
        let nodes = MillsPipelineTree.build(from: runs)
        #expect(nodes.count == 1)
        #expect(nodes[0].run.id == "C")
    }

    @Test("cyclic parent links don't hang the builder")
    func treeCycleGuard() {
        let runs = [
            MillsPipelineRun(id: "A", backlogID: "B", template: "t", state: "x", attempts: 1, parentRunID: "B", depth: 0),
            MillsPipelineRun(id: "B", backlogID: "B", template: "t", state: "y", attempts: 1, parentRunID: "A", depth: 0),
        ]
        // Should terminate and produce a deterministic, total partition.
        let nodes = MillsPipelineTree.build(from: runs)
        let covered = nodes.count + nodes.reduce(0) { $0 + $1.children.count }
        #expect(covered == 2)
    }
}

/// Spot checks for the formatters the Mills screen leans on.
@Suite("LoomFormat · Mills")
struct LoomFormatMillsTests {
    @Test("percent")
    func percent() {
        #expect(LoomFormat.percent(0.86) == "86%")
        #expect(LoomFormat.percent(0.925, decimals: 1) == "92.5%")
        #expect(LoomFormat.percent(nil) == "—")
    }

    @Test("usd tiers precision")
    func usd() {
        #expect(LoomFormat.usd(4.221) == "$4.22")
        #expect(LoomFormat.usd(12.5) == "$12.5")
        #expect(LoomFormat.usd(120) == "$120")
        #expect(LoomFormat.usd(nil) == "—")
    }

    @Test("fractional-seconds duration")
    func duration() {
        #expect(LoomFormat.duration(seconds: 45.0) == "45s")
        #expect(LoomFormat.duration(seconds: 90.0) == "1.5m")
        #expect(LoomFormat.duration(seconds: 5_400.0) == "1.5h")
        #expect(LoomFormat.duration(seconds: nil) == "—")
    }
}
