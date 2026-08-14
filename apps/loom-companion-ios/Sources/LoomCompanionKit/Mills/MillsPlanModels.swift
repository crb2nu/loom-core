// Plan Store models (mobile port of the HUD Work → Plans board).
//
// Mirrors the HUD plans domain (`internal/hud/domain/plans`, bridge
// `PlanInfo`/`PlanSliceInfo`):
//   - GET  /api/plans           → {available, plans: [...], count}
//   - GET  /api/plans/{id}      → {available, plan: {...}}
//   - POST /api/plans/{id}/advance → {status, plan_id, from_phase, to_phase}
//
// Bare (non-enveloped) JSON — decode via `requestRaw`. Timestamps arrive as
// RFC3339 STRINGS (the bridge passes them through), so they stay `String`
// here and views format via `LoomFormat.relative(fromISO:)`.
//
// Semantics (phase buckets, advance targets, slice progress, respin briefs)
// live here so they're unit-tested and shared — views stay presentation-only.

import Foundation

/// One slice of a plan, mirroring `bridge.PlanSliceInfo`.
public struct MillsPlanSlice: Codable, Sendable, Identifiable, Hashable {
    public let id: String
    public let name: String
    public let phase: String
    public let order: Int?
    public let files: [String]?
    public let branchName: String?
    public let assignedAgentID: String?
    public let mrRef: String?
    public let decisions: [String]?

    enum CodingKeys: String, CodingKey {
        case id, name, phase, order, files, decisions
        case branchName = "branch_name"
        case assignedAgentID = "assigned_agent_id"
        case mrRef = "mr_ref"
    }

    public init(
        id: String,
        name: String,
        phase: String,
        order: Int? = nil,
        files: [String]? = nil,
        branchName: String? = nil,
        assignedAgentID: String? = nil,
        mrRef: String? = nil,
        decisions: [String]? = nil
    ) {
        self.id = id
        self.name = name
        self.phase = phase
        self.order = order
        self.files = files
        self.branchName = branchName
        self.assignedAgentID = assignedAgentID
        self.mrRef = mrRef
        self.decisions = decisions
    }
}

/// A plan and its lifecycle, mirroring `bridge.PlanInfo`.
public struct MillsPlan: Codable, Sendable, Identifiable, Hashable {
    public let id: String
    public let title: String
    public let project: String?
    public let namespace: String?
    public let phase: String
    public let priority: String?
    /// Source plan id when this draft was respun from an existing plan
    /// (commit 6e678e88) — the board shows lineage from it.
    public let respunFrom: String?
    public let createdBy: String?
    public let mrRefs: [String]?
    public let millsBacklogID: String?
    public let killTestStatus: String?
    public let slices: [MillsPlanSlice]?
    /// phase→count rollup the store computes on list, so cards can show
    /// slice progress without a detail fetch.
    public let sliceSummary: [String: Int]?
    public let createdAt: String?
    public let updatedAt: String?

    enum CodingKeys: String, CodingKey {
        case id, title, project, namespace, phase, priority, slices
        case respunFrom = "respun_from"
        case createdBy = "created_by"
        case mrRefs = "mr_refs"
        case millsBacklogID = "mills_backlog_id"
        case killTestStatus = "kill_test_status"
        case sliceSummary = "slice_summary"
        case createdAt = "created_at"
        case updatedAt = "updated_at"
    }

    public init(
        id: String,
        title: String,
        project: String? = nil,
        namespace: String? = nil,
        phase: String,
        priority: String? = nil,
        respunFrom: String? = nil,
        createdBy: String? = nil,
        mrRefs: [String]? = nil,
        millsBacklogID: String? = nil,
        killTestStatus: String? = nil,
        slices: [MillsPlanSlice]? = nil,
        sliceSummary: [String: Int]? = nil,
        createdAt: String? = nil,
        updatedAt: String? = nil
    ) {
        self.id = id
        self.title = title
        self.project = project
        self.namespace = namespace
        self.phase = phase
        self.priority = priority
        self.respunFrom = respunFrom
        self.createdBy = createdBy
        self.mrRefs = mrRefs
        self.millsBacklogID = millsBacklogID
        self.killTestStatus = killTestStatus
        self.slices = slices
        self.sliceSummary = sliceSummary
        self.createdAt = createdAt
        self.updatedAt = updatedAt
    }
}

/// GET /api/plans response. `available == false` means the paired daemon
/// predates the plan store — a deploy-pending state, not an error.
public struct MillsPlanList: Codable, Sendable {
    public let available: Bool
    public let plans: [MillsPlan]

    enum CodingKeys: String, CodingKey {
        case available, plans
    }

    public init(available: Bool = true, plans: [MillsPlan] = []) {
        self.available = available
        self.plans = plans
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.available = try c.decodeIfPresent(Bool.self, forKey: .available) ?? true
        self.plans = try c.decodeIfPresent([MillsPlan].self, forKey: .plans) ?? []
    }
}

/// GET /api/plans/{id} response.
public struct MillsPlanDetail: Codable, Sendable {
    public let available: Bool
    public let plan: MillsPlan?

    enum CodingKeys: String, CodingKey {
        case available, plan
    }

    public init(available: Bool = true, plan: MillsPlan? = nil) {
        self.available = available
        self.plan = plan
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.available = try c.decodeIfPresent(Bool.self, forKey: .available) ?? true
        self.plan = try c.decodeIfPresent(MillsPlan.self, forKey: .plan)
    }
}

/// POST /api/plans/{id}/advance response.
public struct MillsPlanAdvanceAck: Codable, Sendable {
    public let status: String?
    public let planID: String?
    public let fromPhase: String?
    public let toPhase: String?

    enum CodingKeys: String, CodingKey {
        case status
        case planID = "plan_id"
        case fromPhase = "from_phase"
        case toPhase = "to_phase"
    }

    public init(
        status: String? = nil,
        planID: String? = nil,
        fromPhase: String? = nil,
        toPhase: String? = nil
    ) {
        self.status = status
        self.planID = planID
        self.fromPhase = fromPhase
        self.toPhase = toPhase
    }
}

// MARK: - Phase semantics

/// Semantic tone for a plan/slice phase — the app maps tones to LoomColors.
/// Mirrors the HUD's `planPhaseVariant`.
public enum MillsPlanTone: Sendable, Equatable {
    /// draft / unset — parked, needs review.
    case draft
    /// planned / in_progress — moving.
    case active
    /// in_review / merging — human-or-CI gate.
    case review
    /// merged / deployed / done — landed.
    case shipped
    /// abandoned — dead end.
    case abandoned
    /// anything the store mints later.
    case unknown
}

public enum MillsPlanPhases {
    /// Canonical lifecycle order (mirrors HUD PLAN_PHASES).
    public static let ordered = [
        "draft", "planned", "in_progress", "in_review",
        "merging", "merged", "deployed", "done",
    ]

    /// Legal advance targets = every canonical phase + abandoned. The store
    /// enforces legality server-side (422 on an illegal transition), so the
    /// picker offers everything except the current phase.
    public static let advanceTargets = ordered + ["abandoned"]

    /// Warp-beam priority buckets (P0 dispatches first).
    public static let priorities = ["P0", "P1", "P2", "P3"]

    public static func tone(for phase: String) -> MillsPlanTone {
        switch phase.lowercased() {
        case "draft": return .draft
        case "planned", "in_progress": return .active
        case "in_review", "merging": return .review
        case "merged", "deployed", "done": return .shipped
        case "abandoned": return .abandoned
        default: return .unknown
        }
    }

    /// Terminal phases drop out of the mobile board's default view.
    public static func isTerminal(_ phase: String) -> Bool {
        switch phase.lowercased() {
        case "merged", "deployed", "done", "abandoned": return true
        default: return false
        }
    }

    /// "in_review" → "in review" for display.
    public static func displayName(_ phase: String) -> String {
        phase.replacingOccurrences(of: "_", with: " ")
    }

    /// Sort key: canonical phases in lifecycle order, then unknown, then
    /// abandoned last.
    public static func sortIndex(_ phase: String) -> Int {
        if let i = ordered.firstIndex(of: phase.lowercased()) { return i }
        if phase.lowercased() == "abandoned" { return ordered.count + 1 }
        return ordered.count
    }
}

// MARK: - Slice progress

/// A slice-progress rollup for a plan card: total slices, merged count, and
/// per-phase segments ordered pipeline-first. Mirrors the HUD `sliceProgress`.
public struct MillsSliceProgress: Sendable, Equatable {
    public struct Segment: Sendable, Equatable {
        public let phase: String
        public let count: Int
        public let tone: MillsPlanTone

        public init(phase: String, count: Int, tone: MillsPlanTone) {
            self.phase = phase
            self.count = count
            self.tone = tone
        }
    }

    public let total: Int
    public let merged: Int
    public let segments: [Segment]

    /// Canonical slice phase order (mirrors HUD SLICE_PHASES).
    public static let slicePhases = [
        "pending", "claimed", "implementing", "implemented",
        "in_review", "integrated", "merged",
    ]

    public static func sliceTone(_ phase: String) -> MillsPlanTone {
        switch phase.lowercased() {
        case "merged", "integrated": return .shipped
        case "in_review": return .review
        case "implementing", "implemented", "claimed": return .active
        case "pending": return .draft
        default: return .unknown
        }
    }

    /// Build from a plan's `slice_summary`; nil when there are no slices.
    public static func build(from summary: [String: Int]?) -> MillsSliceProgress? {
        guard let summary else { return nil }
        let total = summary.values.reduce(0, +)
        guard total > 0 else { return nil }
        var segments: [Segment] = []
        for phase in slicePhases {
            if let count = summary[phase], count > 0 {
                segments.append(Segment(phase: phase, count: count, tone: sliceTone(phase)))
            }
        }
        // Surface any non-canonical phases at the end so nothing is dropped.
        let known = Set(slicePhases)
        for (phase, count) in summary.sorted(by: { $0.key < $1.key })
            where !known.contains(phase) && count > 0
        {
            segments.append(Segment(phase: phase, count: count, tone: sliceTone(phase)))
        }
        return MillsSliceProgress(
            total: total,
            merged: summary["merged"] ?? 0,
            segments: segments
        )
    }
}

// MARK: - Respin briefs

/// Brief builders for a respin — seeding the Spinning Room with enough of the
/// original plan that the frame can redo and expand it. Mirrors the HUD's
/// `buildPlanRespinBrief` / `buildSliceRespinBrief` so drafts spun from the
/// phone match ones spun from the board.
public enum MillsRespinBrief {
    public static func forPlan(_ plan: MillsPlan) -> String {
        var lines = [
            "Redo and expand this existing plan into a richer, fully-decomposed draft. "
                + "Preserve the intent; where the original is sparse, add the missing slices, "
                + "concrete file scopes, and acceptance criteria.",
            "",
            "Plan: \(plan.title)",
        ]
        if let p = plan.priority, !p.isEmpty { lines.append("Priority: \(p)") }
        if let proj = plan.project, !proj.isEmpty { lines.append("Project: \(proj)") }
        lines.append("")
        let slices = plan.slices ?? []
        if slices.isEmpty {
            lines.append("(The original plan has no slices — decompose it from the title/intent above.)")
        } else {
            lines.append("Existing slices:")
            for s in slices {
                let files = (s.files?.isEmpty == false) ? " (files: \(s.files!.joined(separator: ", ")))" : ""
                lines.append("- \(s.name)\(files)")
            }
        }
        return lines.joined(separator: "\n")
    }

    public static func forSlice(_ slice: MillsPlanSlice, of plan: MillsPlan) -> String {
        var lines = [
            "Expand this single slice from an existing plan into a fuller, self-contained "
                + "draft plan with concrete, independently-shippable implementation slices.",
            "",
            "From plan: \(plan.title)",
            "Slice: \(slice.name)",
        ]
        if let files = slice.files, !files.isEmpty {
            lines.append("Files: \(files.joined(separator: ", "))")
        }
        if let decisions = slice.decisions, !decisions.isEmpty {
            lines.append("Notes:")
            for d in decisions { lines.append("- \(d)") }
        }
        return lines.joined(separator: "\n")
    }
}
