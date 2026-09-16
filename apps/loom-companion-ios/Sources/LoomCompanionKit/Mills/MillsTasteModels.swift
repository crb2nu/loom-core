// Taste + merge-queue wire models (mobile catch-up for taste epic S1–S4 and
// the serial merge queue).
//
// Taste aggregates mirror `pkg/mills/store.TasteAggregates` (snake_case JSON
// tags, unlike the PascalCase untagged structs elsewhere in the mills proxy).
// The 14-day overall coverage ratio is the number the S5/S6 autonomy gate
// reads (≥60% of merged bolts graded within the rolling window), so the
// screen renders it against that threshold rather than as a bare percent.
//
// The merge queue snapshot mirrors `handleMergeQueueList`
// (cmd/loom-mills-operator/handlers_merge_queue.go): `{active: [entry...],
// summary: {depth, lanes, enabled}}` with snake_case entries.

import Foundation

/// One plan's taste rollup (`store.PlanTasteAggregate`).
public struct MillsPlanTaste: Codable, Sendable, Identifiable, Hashable {
    public let planID: String
    public let keep: Int
    public let meh: Int
    public let regret: Int
    public let graded: Int
    public let merged: Int
    public let regretRate: Double
    public let gradeCoverage: Double

    public var id: String { planID }

    enum CodingKeys: String, CodingKey {
        case planID = "plan_id"
        case keep, meh, regret, graded, merged
        case regretRate = "regret_rate"
        case gradeCoverage = "grade_coverage"
    }

    public init(
        planID: String,
        keep: Int = 0,
        meh: Int = 0,
        regret: Int = 0,
        graded: Int = 0,
        merged: Int = 0,
        regretRate: Double = 0,
        gradeCoverage: Double = 0
    ) {
        self.planID = planID
        self.keep = keep
        self.meh = meh
        self.regret = regret
        self.graded = graded
        self.merged = merged
        self.regretRate = regretRate
        self.gradeCoverage = gradeCoverage
    }
}

/// GET /api/mills/taste/aggregates — `store.TasteAggregates`.
public struct MillsTasteAggregates: Codable, Sendable, Hashable {
    public let plans: [MillsPlanTaste]?
    public let overallGraded14d: Int
    public let overallMerged14d: Int
    /// Graded share of merged bolts over the rolling 14d window, 0…1.
    public let overallCoverage14d: Double

    enum CodingKeys: String, CodingKey {
        case plans
        case overallGraded14d = "overall_graded_14d"
        case overallMerged14d = "overall_merged_14d"
        case overallCoverage14d = "overall_coverage_14d"
    }

    public init(
        plans: [MillsPlanTaste]? = nil,
        overallGraded14d: Int = 0,
        overallMerged14d: Int = 0,
        overallCoverage14d: Double = 0
    ) {
        self.plans = plans
        self.overallGraded14d = overallGraded14d
        self.overallMerged14d = overallMerged14d
        self.overallCoverage14d = overallCoverage14d
    }

    /// The S5/S6 autonomy gate threshold the coverage ratio is judged against.
    public static let coverageGate: Double = 0.6
}

/// The three one-tap taste grades. Raw values are the operator's wire
/// vocabulary (`mills.GradeRun` rejects anything else with a 422).
public enum MillsGrade: String, Sendable, CaseIterable {
    case keep
    case meh
    case regret

    /// SF Symbol the grade renders as on grade buttons and graded rows.
    public var icon: String {
        switch self {
        case .keep: return "hand.thumbsup.fill"
        case .meh: return "minus.circle.fill"
        case .regret: return "hand.thumbsdown.fill"
        }
    }
}

/// POST …/pipeline/runs/{id}/grade acknowledgement (handlePipelineGrade's
/// snake_case response map).
public struct MillsGradeAck: Codable, Sendable, Hashable {
    public let runID: String
    public let itemID: String
    public let grade: String
    public let note: String?
    public let actor: String?

    enum CodingKeys: String, CodingKey {
        case runID = "run_id"
        case itemID = "item_id"
        case grade, note, actor
    }

    public init(runID: String, itemID: String, grade: String, note: String? = nil, actor: String? = nil) {
        self.runID = runID
        self.itemID = itemID
        self.grade = grade
        self.note = note
        self.actor = actor
    }
}

/// One active merge-queue candidate (`store.MergeQueueEntry`, snake_case).
/// Decodes only the fields the screen renders — `detail` is deliberately
/// dropped (drive-state internals).
public struct MillsMergeQueueEntry: Codable, Sendable, Identifiable, Hashable {
    public let id: Int64
    public let pipelineRunID: String
    public let backlogID: String
    public let project: String
    public let mrIID: Int64
    public let targetBranch: String
    public let state: String

    enum CodingKeys: String, CodingKey {
        case id
        case pipelineRunID = "pipeline_run_id"
        case backlogID = "backlog_id"
        case project
        case mrIID = "mr_iid"
        case targetBranch = "target_branch"
        case state
    }

    public init(
        id: Int64,
        pipelineRunID: String,
        backlogID: String,
        project: String,
        mrIID: Int64,
        targetBranch: String,
        state: String
    ) {
        self.id = id
        self.pipelineRunID = pipelineRunID
        self.backlogID = backlogID
        self.project = project
        self.mrIID = mrIID
        self.targetBranch = targetBranch
        self.state = state
    }
}

/// GET /api/mills/merge-queue.
public struct MillsMergeQueueSnapshot: Codable, Sendable, Hashable {
    public struct Summary: Codable, Sendable, Hashable {
        public let depth: Int
        public let lanes: [String: Int]?
        public let enabled: Bool

        public init(depth: Int = 0, lanes: [String: Int]? = nil, enabled: Bool = false) {
            self.depth = depth
            self.lanes = lanes
            self.enabled = enabled
        }
    }

    public let active: [MillsMergeQueueEntry]?
    public let summary: Summary?

    public init(active: [MillsMergeQueueEntry]? = nil, summary: Summary? = nil) {
        self.active = active
        self.summary = summary
    }
}
