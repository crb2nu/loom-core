// Mills KPI + pipeline semantics (pure, testable — no SwiftUI).
//
// The mobile Mills screen previously read three metric keys the operator
// never publishes (`pipeline_merge_rate`, `council_cost_per_day_usd`,
// `audit_findings_count`), so every KPI card rendered "—". The canonical
// key set lives in pkg/mills/kpi_writer.go and is mirrored by the HUD's
// MillsKPIRow.svelte. This file is the single Swift source for that
// mapping so the app and any future widget agree, and so the key set is
// covered by unit tests instead of silently drifting.

import Foundation

/// Whether a KPI is meeting its target. Drives the on-card status dot.
/// Most KPIs are `.neutral` (no defensible mobile target without trend
/// history); only threshold-bearing metrics (gate pass, escalation) badge.
public enum MillsMetricStatus: String, Sendable, Equatable {
    case onTarget
    case watch
    case offTarget
    case neutral
}

/// One KPI tile. `value` is already formatted for display; `proxy` flags a
/// metric whose backend derivation is still best-effort (mirrors the HUD's
/// "(proxy)" tag).
public struct MillsKPICard: Identifiable, Sendable, Equatable {
    public let id: String
    public let label: String
    public let value: String
    public let status: MillsMetricStatus
    public let proxy: Bool

    public init(id: String, label: String, value: String, status: MillsMetricStatus = .neutral, proxy: Bool = false) {
        self.id = id
        self.label = label
        self.value = value
        self.status = status
        self.proxy = proxy
    }
}

/// At-a-glance factory state derived from the KPI snapshot. The north-star
/// (`mergedRuns`) leads the screen even when it's zero, so it's a non-optional
/// Int. `queueDepth`/`activeRuns` come from the operator status fields folded
/// into the snapshot metrics map.
public struct MillsSystemSummary: Sendable, Equatable {
    public let mergedRuns: Int
    public let queueDepth: Int
    public let activeRuns: Int

    public init(mergedRuns: Int, queueDepth: Int, activeRuns: Int) {
        self.mergedRuns = mergedRuns
        self.queueDepth = queueDepth
        self.activeRuns = activeRuns
    }

    /// True when work is actively flowing (a run executing or items queued).
    /// Drives the hero's live pulse + accent.
    public var isBusy: Bool { activeRuns > 0 || queueDepth > 0 }
}

public extension MillsKPISnapshot {
    /// North-star merge count over the snapshot window. Always a real number
    /// (incl. 0) so the hero reads as data, not "no signal".
    var mergedRuns24h: Int { Int((metrics["pipeline_merged_runs"] ?? 0).rounded()) }

    /// Derived factory state for the hero header.
    var systemSummary: MillsSystemSummary {
        MillsSystemSummary(
            mergedRuns: mergedRuns24h,
            queueDepth: Int((metrics["queue_depth"] ?? 0).rounded()),
            activeRuns: Int((metrics["active_pipeline_runs"] ?? 0).rounded())
        )
    }

    /// The canonical KPI grid (north-star excluded — it owns the hero).
    /// Cards whose metric is absent are dropped so a sparse snapshot renders
    /// only what it has rather than a wall of em dashes.
    var canonicalCards: [MillsKPICard] {
        var cards: [MillsKPICard] = []

        if let rate = metrics["auto_merge_rate"] {
            cards.append(MillsKPICard(
                id: "auto_merge_rate",
                label: "Auto-merge",
                value: LoomFormat.percent(rate, decimals: 0)
            ))
        }

        let cost = metrics["cost_per_merged_change_usd"] ?? metrics["cost_per_merged_pipeline_usd"]
        if let cost {
            cards.append(MillsKPICard(
                id: "cost_per_merged",
                label: "Cost / merged",
                value: LoomFormat.usd(cost)
            ))
        }

        if let p50 = metrics["slice_to_merge_p50_seconds"] {
            cards.append(MillsKPICard(
                id: "slice_to_merge_p50",
                label: "Slice→merge p50",
                value: LoomFormat.duration(seconds: p50)
            ))
        }

        if let gate = metrics["gate_pass_rate"] {
            cards.append(MillsKPICard(
                id: "gate_pass_rate",
                label: "Gate pass",
                value: LoomFormat.percent(gate, decimals: 0),
                status: Self.targetStatus(gate, target: 0.85, softMargin: 0.10, direction: .higherBetter)
            ))
        }

        if let esc = metrics["escalation_rate"] {
            cards.append(MillsKPICard(
                id: "escalation_rate",
                label: "Escalation",
                value: LoomFormat.percent(esc, decimals: 0),
                status: Self.targetStatus(esc, target: 0.15, softMargin: 1.0, direction: .lowerBetter)
            ))
        }

        if let roi = metrics["council_roi"] {
            cards.append(MillsKPICard(
                id: "council_roi",
                label: "Council ROI",
                value: String(format: "%.1f×", roi)
            ))
        }

        if let reg = metrics["regression_rate"] {
            cards.append(MillsKPICard(
                id: "regression_rate",
                label: "Regression",
                value: LoomFormat.percent(reg, decimals: 0),
                status: Self.targetStatus(reg, target: 0.02, softMargin: 0.5, direction: .lowerBetter),
                proxy: true
            ))
        }

        return cards
    }

    // MARK: - Target evaluation

    private enum Direction { case higherBetter, lowerBetter }

    private static func targetStatus(_ value: Double, target: Double, softMargin: Double, direction: Direction) -> MillsMetricStatus {
        let meets = direction == .lowerBetter ? value <= target : value >= target
        if meets { return .onTarget }
        let soft = direction == .lowerBetter
            ? value <= target * (1 + softMargin)
            : value >= target * (1 - softMargin)
        return soft ? .watch : .offTarget
    }
}

/// Coarse lifecycle bucket for a pipeline run, derived from its free-form
/// `state` string. The operator emits many fine-grained states; this collapses
/// them into the handful the UI colors + groups by. Pure string logic so it's
/// unit-tested and shared by any future surface.
public enum MillsRunCategory: String, Sendable, Equatable, CaseIterable {
    case queued
    case running
    case review
    case merging
    case escalated
    case failed
    case done
    case unknown

    /// Terminal categories are filtered out of the "in-flight" list.
    public var isTerminal: Bool {
        switch self {
        case .escalated, .failed, .done: return true
        case .queued, .running, .review, .merging, .unknown: return false
        }
    }

    /// Live categories pulse + emphasize their accent bar.
    public var isLive: Bool {
        switch self {
        case .running, .review, .merging: return true
        case .queued, .escalated, .failed, .done, .unknown: return false
        }
    }

    public static func categorize(_ rawState: String) -> MillsRunCategory {
        switch rawState.lowercased() {
        case "queued", "pending", "created", "planning", "planned", "backlog":
            return .queued
        case "implementing", "coding", "building", "testing", "spawning",
             "running", "in_progress", "council", "deliberating", "decomposing":
            return .running
        case "reviewing", "review", "auditing", "audit", "gate",
             "evaluating", "verifying":
            return .review
        case "merging", "merge", "shipping", "landing":
            return .merging
        case "escalated":
            return .escalated
        case "failed", "error", "cancelled", "canceled":
            return .failed
        case "done", "merged", "completed", "complete", "success":
            return .done
        default:
            return .unknown
        }
    }
}

public extension MillsPipelineRun {
    var category: MillsRunCategory { MillsRunCategory.categorize(state) }

    /// Human-friendly state label: `"in_progress" -> "In progress"`.
    var displayState: String {
        let spaced = state.replacingOccurrences(of: "_", with: " ")
        guard let first = spaced.first else { return spaced }
        return first.uppercased() + spaced.dropFirst()
    }
}

/// Groups in-flight runs into root → descendant trees for the screen's pipeline
/// list, preserving operator order. Every run resolves to its top-most ancestor
/// that is present in the set; runs whose parent is absent (or who have none)
/// become roots, so nothing is hidden. Descendants at any depth attach to that
/// root and are ordered shallow-to-deep, letting the UI indent by `depth`.
public struct MillsPipelineTree {
    public struct Node: Identifiable, Sendable {
        public let run: MillsPipelineRun
        /// All descendants (any depth) of `run`, ordered by depth then appearance.
        public let children: [MillsPipelineRun]
        public var id: String { run.id }
    }

    public static func build(from runs: [MillsPipelineRun]) -> [Node] {
        let byID = Dictionary(runs.map { ($0.id, $0) }, uniquingKeysWith: { first, _ in first })

        // Walk the parent chain to the top-most ancestor present in the set.
        // `guardCount` defends against malformed cyclic parent links.
        func rootID(of run: MillsPipelineRun) -> String {
            var current = run
            var guardCount = 0
            while let parentID = current.parentRunID,
                  parentID != current.id,
                  let parent = byID[parentID],
                  guardCount < runs.count {
                current = parent
                guardCount += 1
            }
            return current.id
        }

        var descendantsByRoot: [String: [MillsPipelineRun]] = [:]
        var roots: [MillsPipelineRun] = []
        for run in runs {
            let rid = rootID(of: run)
            if rid == run.id {
                roots.append(run)
            } else {
                descendantsByRoot[rid, default: []].append(run)
            }
        }
        return roots.map { root in
            let kids = (descendantsByRoot[root.id] ?? [])
                .enumerated()
                .sorted { lhs, rhs in
                    let dl = lhs.element.depth ?? 0
                    let dr = rhs.element.depth ?? 0
                    return dl != dr ? dl < dr : lhs.offset < rhs.offset
                }
                .map(\.element)
            return Node(run: root, children: kids)
        }
    }
}
