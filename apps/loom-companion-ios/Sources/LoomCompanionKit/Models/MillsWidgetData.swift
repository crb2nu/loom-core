import Foundation

// MARK: - Mills factory widget data
//
// App-Group snapshot for the MillsFactoryWidget: everything the home/lock
// screen needs to oversee the autonomous factory without opening the app —
// the north-star (merges 24h), floor state (active/queued/escalated), the
// canonical efficiency KPIs, the in-flight pipeline tree's top rows, live
// Spinning-Room spins, and the plan board's review pressure (drafts).
//
// Written separately from the main `WidgetData` blob (SpawnBudget pattern):
// `MillsWidgetPublisher` is the only writer and runs on its own cadence
// (dashboard sync + Mills screen loads), so it never contends with
// DashboardViewModel's fleet snapshot.

/// Stable widget-kind identifier shared between the widget extension
/// (`StaticConfiguration(kind:)`) and the publisher
/// (`WidgetCenter.reloadTimelines(ofKind:)`) so Mills refreshes don't reload
/// every widget on every poll.
public let MillsFactoryWidgetKind = "MillsFactoryWidget"

/// One in-flight pipeline row, pre-ranked for the widget (roots before
/// slices, running before queued). `state` stays the operator's free-form
/// string — the widget buckets it via `MillsRunCategory.categorize`, the same
/// unit-tested semantics the Mills screen uses.
public struct MillsWidgetPipelineRow: Codable, Sendable, Equatable, Hashable, Identifiable {
    public let id: String
    public let state: String
    public let template: String
    public let attempts: Int
    public let depth: Int
    public let startedAt: Date?

    public init(
        id: String,
        state: String,
        template: String,
        attempts: Int = 1,
        depth: Int = 0,
        startedAt: Date? = nil
    ) {
        self.id = id
        self.state = state
        self.template = template
        self.attempts = attempts
        self.depth = depth
        self.startedAt = startedAt
    }

    public var category: MillsRunCategory { MillsRunCategory.categorize(state) }
}

/// One Spinning-Room spin row for the widget.
public struct MillsWidgetSpinRow: Codable, Sendable, Equatable, Hashable, Identifiable {
    public let id: String
    public let frames: [String]
    public let status: String
    public let planCount: Int
    public let startedAt: Date?

    public init(id: String, frames: [String], status: String, planCount: Int = 0, startedAt: Date? = nil) {
        self.id = id
        self.frames = frames
        self.status = status
        self.planCount = planCount
        self.startedAt = startedAt
    }

    public var statusKind: MillsSpinStatus { MillsSpinStatus(wire: status) }
}

/// Snapshot of the autonomous factory for WidgetKit. Tolerant decoding: every
/// field defaults so a widget process running an older/newer schema than the
/// app renders a partial snapshot instead of dropping the whole blob.
public struct MillsWidgetData: Codable, Sendable, Equatable {
    /// False when the operator was unreachable on the last publish — the
    /// widget shows "operator offline" instead of misleading zeros.
    public let operatorReachable: Bool

    // North-star + floor state (KPI snapshot window, 24h).
    public let mergedRuns24h: Int
    public let activeRuns: Int
    public let queueDepth: Int
    public let escalatedRuns: Int

    // Canonical efficiency KPIs (nil = metric absent from the snapshot).
    public let autoMergeRate: Double?
    public let costPerMergeUSD: Double?
    public let gatePassRate: Double?
    public let escalationRate: Double?

    public let pipelines: [MillsWidgetPipelineRow]
    public let spins: [MillsWidgetSpinRow]

    // Plan board pressure: drafts awaiting operator review feed the beam.
    public let draftPlans: Int
    public let activePlans: Int

    public let lastUpdated: Date

    public init(
        operatorReachable: Bool = true,
        mergedRuns24h: Int = 0,
        activeRuns: Int = 0,
        queueDepth: Int = 0,
        escalatedRuns: Int = 0,
        autoMergeRate: Double? = nil,
        costPerMergeUSD: Double? = nil,
        gatePassRate: Double? = nil,
        escalationRate: Double? = nil,
        pipelines: [MillsWidgetPipelineRow] = [],
        spins: [MillsWidgetSpinRow] = [],
        draftPlans: Int = 0,
        activePlans: Int = 0,
        lastUpdated: Date = .now
    ) {
        self.operatorReachable = operatorReachable
        self.mergedRuns24h = mergedRuns24h
        self.activeRuns = activeRuns
        self.queueDepth = queueDepth
        self.escalatedRuns = escalatedRuns
        self.autoMergeRate = autoMergeRate
        self.costPerMergeUSD = costPerMergeUSD
        self.gatePassRate = gatePassRate
        self.escalationRate = escalationRate
        self.pipelines = pipelines
        self.spins = spins
        self.draftPlans = draftPlans
        self.activePlans = activePlans
        self.lastUpdated = lastUpdated
    }

    enum CodingKeys: String, CodingKey {
        case operatorReachable, mergedRuns24h, activeRuns, queueDepth, escalatedRuns
        case autoMergeRate, costPerMergeUSD, gatePassRate, escalationRate
        case pipelines, spins, draftPlans, activePlans, lastUpdated
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.operatorReachable = try c.decodeIfPresent(Bool.self, forKey: .operatorReachable) ?? true
        self.mergedRuns24h = try c.decodeIfPresent(Int.self, forKey: .mergedRuns24h) ?? 0
        self.activeRuns = try c.decodeIfPresent(Int.self, forKey: .activeRuns) ?? 0
        self.queueDepth = try c.decodeIfPresent(Int.self, forKey: .queueDepth) ?? 0
        self.escalatedRuns = try c.decodeIfPresent(Int.self, forKey: .escalatedRuns) ?? 0
        self.autoMergeRate = try c.decodeIfPresent(Double.self, forKey: .autoMergeRate)
        self.costPerMergeUSD = try c.decodeIfPresent(Double.self, forKey: .costPerMergeUSD)
        self.gatePassRate = try c.decodeIfPresent(Double.self, forKey: .gatePassRate)
        self.escalationRate = try c.decodeIfPresent(Double.self, forKey: .escalationRate)
        self.pipelines = try c.decodeIfPresent([MillsWidgetPipelineRow].self, forKey: .pipelines) ?? []
        self.spins = try c.decodeIfPresent([MillsWidgetSpinRow].self, forKey: .spins) ?? []
        self.draftPlans = try c.decodeIfPresent(Int.self, forKey: .draftPlans) ?? 0
        self.activePlans = try c.decodeIfPresent(Int.self, forKey: .activePlans) ?? 0
        self.lastUpdated = try c.decodeIfPresent(Date.self, forKey: .lastUpdated) ?? .now
    }

    /// One-line floor status, shared by the widget families and accessory
    /// text so every surface tells the same story.
    public var floorLine: String {
        guard operatorReachable else { return "operator offline" }
        if activeRuns > 0 {
            var parts = ["\(activeRuns) running"]
            if queueDepth > 0 { parts.append("\(queueDepth) queued") }
            if escalatedRuns > 0 { parts.append("\(escalatedRuns) escalated") }
            return parts.joined(separator: " · ")
        }
        if escalatedRuns > 0 { return "\(escalatedRuns) escalated — needs you" }
        if queueDepth > 0 { return "\(queueDepth) queued" }
        return "floor idle"
    }

    /// True when a spin is mid-flight (drives faster widget refresh).
    public var hasLiveSpin: Bool {
        spins.contains { !$0.statusKind.isTerminal }
    }
}

// MARK: - Derivation

public extension MillsWidgetData {
    /// Build the widget snapshot from live Mills fetches. Pure + injectable
    /// clock so ranking/caps are unit-testable.
    ///
    /// - pipelines: in-flight only, roots before deeper slices, running
    ///   before queued, newest first within a bucket; capped (default 6).
    /// - spins: `MillsSpinBoard.visibleRuns` semantics (live pinned first,
    ///   terminal kept 6h), capped at 3.
    /// - plans: drafts = review pressure; active = planned/in_progress.
    static func from(
        kpi: MillsKPISnapshot?,
        runs: [MillsPipelineRun],
        spins: [MillsSpinRun],
        plans: [MillsPlan],
        operatorReachable: Bool = true,
        pipelineLimit: Int = 6,
        now: Date = .now
    ) -> MillsWidgetData {
        let summary = kpi?.systemSummary
        let metrics = kpi?.metrics ?? [:]

        let inFlight = runs.filter { !$0.category.isTerminal }
        let ranked = inFlight.sorted { a, b in
            let aLive = a.category == .running || a.category == .merging || a.category == .review
            let bLive = b.category == .running || b.category == .merging || b.category == .review
            if aLive != bLive { return aLive }
            let aDepth = a.depth ?? 0
            let bDepth = b.depth ?? 0
            if aDepth != bDepth { return aDepth < bDepth }
            return (a.startedAt ?? .distantPast) > (b.startedAt ?? .distantPast)
        }
        let pipelineRows = ranked.prefix(max(pipelineLimit, 0)).map { run in
            MillsWidgetPipelineRow(
                id: run.id,
                state: run.state,
                template: run.template,
                attempts: run.attempts,
                depth: run.depth ?? 0,
                startedAt: run.startedAt
            )
        }

        let spinRows = MillsSpinBoard.visibleRuns(spins, now: now, terminalWindow: 6 * 3600, limit: 3)
            .map { run in
                MillsWidgetSpinRow(
                    id: run.id,
                    frames: run.frames,
                    status: run.status,
                    planCount: run.planIDs.count,
                    startedAt: run.startedAt
                )
            }

        let drafts = plans.filter { $0.phase.lowercased() == "draft" }.count
        let active = plans.filter {
            let p = $0.phase.lowercased()
            return p == "planned" || p == "in_progress"
        }.count

        return MillsWidgetData(
            operatorReachable: operatorReachable,
            mergedRuns24h: summary?.mergedRuns ?? 0,
            activeRuns: summary?.activeRuns ?? inFlight.filter { $0.category == .running }.count,
            queueDepth: summary?.queueDepth ?? inFlight.filter { $0.category == .queued }.count,
            escalatedRuns: Int((metrics["pipeline_escalated_runs"] ?? 0).rounded()),
            autoMergeRate: metrics["auto_merge_rate"],
            costPerMergeUSD: metrics["cost_per_merged_change_usd"] ?? metrics["cost_per_merged_pipeline_usd"],
            gatePassRate: metrics["gate_pass_rate"],
            escalationRate: metrics["escalation_rate"],
            pipelines: Array(pipelineRows),
            spins: spinRows,
            draftPlans: drafts,
            activePlans: active,
            lastUpdated: now
        )
    }
}

// MARK: - Store

private let millsWidgetKey = "millsWidgetData"

public extension SharedDataStore {
    /// Persist the Mills factory snapshot for the MillsFactoryWidget.
    static func saveMills(_ data: MillsWidgetData) {
        guard let defaults = UserDefaults(suiteName: appGroupID),
              let encoded = try? JSONEncoder().encode(data) else { return }
        defaults.set(encoded, forKey: millsWidgetKey)
    }

    /// Load the Mills snapshot. nil = never written; callers fall back to
    /// `placeholderMills`.
    static func loadMills() -> MillsWidgetData? {
        guard let defaults = UserDefaults(suiteName: appGroupID),
              let data = defaults.data(forKey: millsWidgetKey),
              let decoded = try? JSONDecoder().decode(MillsWidgetData.self, from: data) else { return nil }
        return decoded
    }

    static var placeholderMills: MillsWidgetData {
        MillsWidgetData(
            operatorReachable: true,
            mergedRuns24h: 6,
            activeRuns: 2,
            queueDepth: 1,
            escalatedRuns: 1,
            autoMergeRate: 0.86,
            costPerMergeUSD: 4.22,
            gatePassRate: 0.91,
            escalationRate: 0.14,
            pipelines: [
                MillsWidgetPipelineRow(id: "PIPE-7f3a", state: "implementing", template: "mills-default",
                                       startedAt: Date().addingTimeInterval(-320)),
                MillsWidgetPipelineRow(id: "PIPE-7f3a-S1", state: "testing", template: "mills-default",
                                       depth: 1, startedAt: Date().addingTimeInterval(-140)),
                MillsWidgetPipelineRow(id: "PIPE-91b2", state: "queued", template: "mills-council",
                                       startedAt: Date().addingTimeInterval(-12)),
            ],
            spins: [
                MillsWidgetSpinRow(id: "spin-1", frames: ["jacquard"], status: "running",
                                   startedAt: Date().addingTimeInterval(-90)),
            ],
            draftPlans: 2,
            activePlans: 3
        )
    }
}
