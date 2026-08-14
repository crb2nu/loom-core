// Wire models the shift report needs beyond MillsPipelineRun: the backlog
// list (run → backlog → PlanID pattern attribution), the pattern catalog
// (approved slugs + names), and the per-run detail (failing gate names for
// sparks). All three are bare (non-enveloped) JSON like the rest of the
// mills proxy, decoded via `requestRaw`. Each model decodes only the fields
// the report reads — extra wire keys are ignored, so operator additions
// never need a Swift release.

import Foundation

/// Mirrors the fields of `pkg/mills/store.BacklogItem` the report reads.
/// The operator's list endpoint serializes the full untagged Go struct
/// (PascalCase keys). Stamped plans embed their pattern slug in PlanID
/// (`plan-stamp-<slug>-<primary>`), which is what pattern attribution
/// derives from.
public struct MillsBacklogItem: Codable, Sendable, Identifiable, Hashable {
    public let id: String
    public let planID: String?

    enum CodingKeys: String, CodingKey {
        case id = "ID"
        case planID = "PlanID"
    }

    public init(id: String, planID: String? = nil) {
        self.id = id
        self.planID = planID
    }
}

/// One Pattern Loom catalog entry, from the HUD's GET /api/patterns
/// (snake_case JSON, wrapped in `{"patterns": [...]}`). The report only
/// needs slug/name/status to attribute stamped runs to their books.
public struct MillsPatternInfo: Codable, Sendable, Hashable {
    public let slug: String
    public let name: String
    public let status: String

    public init(slug: String, name: String, status: String) {
        self.slug = slug
        self.name = name
        self.status = status
    }
}

/// Envelope of GET /api/patterns: `{"patterns": [...]}` (null when the
/// catalog is empty).
public struct MillsPatternCatalog: Codable, Sendable {
    public let patterns: [MillsPatternInfo]?

    public init(patterns: [MillsPatternInfo]?) {
        self.patterns = patterns
    }
}

/// Mirrors the fields of `pkg/mills/store.GateOutcome` the report reads —
/// one evaluated gate after a stage (PascalCase Go field names).
public struct MillsGateOutcome: Codable, Sendable, Hashable {
    public let gateName: String
    /// "pass" | "fail" | "skip".
    public let outcome: String

    enum CodingKeys: String, CodingKey {
        case gateName = "GateName"
        case outcome = "Outcome"
    }

    public init(gateName: String, outcome: String) {
        self.gateName = gateName
        self.outcome = outcome
    }
}

/// Shape of GET /api/mills/pipeline/runs/{id} (operator
/// handlePipelineRunGet): run + stages + gates in one round-trip. The
/// report only reads `gates`; keys are lowercase on this wrapper (unlike
/// the PascalCase structs inside it).
public struct MillsPipelineRunDetail: Codable, Sendable {
    public let run: MillsPipelineRun?
    public let gates: [MillsGateOutcome]?

    public init(run: MillsPipelineRun? = nil, gates: [MillsGateOutcome]? = nil) {
        self.run = run
        self.gates = gates
    }
}
