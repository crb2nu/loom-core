// Operator status wire model + semantics (pure, testable — no SwiftUI).
//
// Mirrors the map `handleStatusFull` builds (cmd/loom-mills-operator/
// handlers_status.go). The HUD proxies it verbatim at GET /api/mills/status,
// and the mobile screen had never read it: the phone showed merges and
// KPIs but nothing about whether the factory was *allowed* to run — policy
// switch, autonomy blockers, health-gate admission, budget headroom, or
// whether the council was still producing work. Every field is optional so
// an older operator (or a partial payload) decodes to a sparse status
// rather than a decode error; the semantics below say "unknown" instead of
// guessing.

import Foundation

/// One row of the operator's capability matrix.
public struct MillsCapability: Codable, Sendable, Identifiable, Hashable {
    public let id: String
    public let status: String
    public let mode: String?
    public let requiredForAutonomy: Bool
    public let message: String?

    enum CodingKeys: String, CodingKey {
        case id, status, mode, message
        case requiredForAutonomy = "required_for_autonomy"
    }

    public init(id: String, status: String, mode: String? = nil, requiredForAutonomy: Bool = false, message: String? = nil) {
        self.id = id
        self.status = status
        self.mode = mode
        self.requiredForAutonomy = requiredForAutonomy
        self.message = message
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        status = try c.decodeIfPresent(String.self, forKey: .status) ?? "unknown"
        mode = try c.decodeIfPresent(String.self, forKey: .mode)
        requiredForAutonomy = try c.decodeIfPresent(Bool.self, forKey: .requiredForAutonomy) ?? false
        message = try c.decodeIfPresent(String.self, forKey: .message)
    }

    public var isGreen: Bool { status == "green" }
}

/// A tier's rolling-24h spend against its policy caps (`mills.WindowUsage`).
/// `runsCap == 0` means "no run cap" (the council tier).
public struct MillsBudgetTier: Codable, Sendable, Hashable {
    public let spentUSD: Double
    public let capUSD: Double
    public let runs: Int
    public let runsCap: Int

    enum CodingKeys: String, CodingKey {
        case spentUSD = "spent_usd"
        case capUSD = "cap_usd"
        case runs
        case runsCap = "runs_cap"
    }

    public init(spentUSD: Double = 0, capUSD: Double = 0, runs: Int = 0, runsCap: Int = 0) {
        self.spentUSD = spentUSD
        self.capUSD = capUSD
        self.runs = runs
        self.runsCap = runsCap
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        spentUSD = try c.decodeIfPresent(Double.self, forKey: .spentUSD) ?? 0
        capUSD = try c.decodeIfPresent(Double.self, forKey: .capUSD) ?? 0
        runs = try c.decodeIfPresent(Int.self, forKey: .runs) ?? 0
        runsCap = try c.decodeIfPresent(Int.self, forKey: .runsCap) ?? 0
    }

    /// Spend as a share of the cap, clamped to 0…1. Zero when there is no
    /// cap, so a capless tier draws an empty bar rather than a full one.
    public var spentFraction: Double {
        guard capUSD > 0 else { return 0 }
        return min(1, max(0, spentUSD / capUSD))
    }

    /// True once spend crosses 85% of a positive cap — the point where the
    /// next run is likely to be budget-deferred.
    public var isNearCap: Bool { capUSD > 0 && spentFraction >= 0.85 }
}

/// The infrastructure admission verdict (`gates.HealthGateReport`), trimmed
/// to what the card renders.
public struct MillsHealthGates: Codable, Sendable, Hashable {
    public let allowed: Bool
    public let failClosed: Bool
    public let status: String?
    public let reasons: [String]?

    enum CodingKeys: String, CodingKey {
        case allowed, status, reasons
        case failClosed = "fail_closed"
    }

    public init(allowed: Bool, failClosed: Bool = false, status: String? = nil, reasons: [String]? = nil) {
        self.allowed = allowed
        self.failClosed = failClosed
        self.status = status
        self.reasons = reasons
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        allowed = try c.decodeIfPresent(Bool.self, forKey: .allowed) ?? false
        failClosed = try c.decodeIfPresent(Bool.self, forKey: .failClosed) ?? false
        status = try c.decodeIfPresent(String.self, forKey: .status)
        reasons = try c.decodeIfPresent([String].self, forKey: .reasons)
    }
}

/// Council yield: finished council runs since one last produced a backlog
/// delta, and what they cost. `lastDeltaAt == nil` with
/// `runsSinceLastDelta == sampleSize` means the dry spell is at least the
/// whole sample, not that it began there.
public struct MillsCouncilYield: Codable, Sendable, Hashable {
    public let runsSinceLastDelta: Int
    public let costSinceLastDeltaUSD: Double
    public let lastDeltaAt: Date?
    public let lastDeltaRunID: String?
    public let sampleSize: Int

    enum CodingKeys: String, CodingKey {
        case runsSinceLastDelta = "runs_since_last_delta"
        case costSinceLastDeltaUSD = "cost_since_last_delta_usd"
        case lastDeltaAt = "last_delta_at"
        case lastDeltaRunID = "last_delta_run_id"
        case sampleSize = "sample_size"
    }

    public init(
        runsSinceLastDelta: Int = 0,
        costSinceLastDeltaUSD: Double = 0,
        lastDeltaAt: Date? = nil,
        lastDeltaRunID: String? = nil,
        sampleSize: Int = 0
    ) {
        self.runsSinceLastDelta = runsSinceLastDelta
        self.costSinceLastDeltaUSD = costSinceLastDeltaUSD
        self.lastDeltaAt = lastDeltaAt
        self.lastDeltaRunID = lastDeltaRunID
        self.sampleSize = sampleSize
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        runsSinceLastDelta = try c.decodeIfPresent(Int.self, forKey: .runsSinceLastDelta) ?? 0
        costSinceLastDeltaUSD = try c.decodeIfPresent(Double.self, forKey: .costSinceLastDeltaUSD) ?? 0
        lastDeltaAt = try c.decodeIfPresent(Date.self, forKey: .lastDeltaAt)
        lastDeltaRunID = try c.decodeIfPresent(String.self, forKey: .lastDeltaRunID)
        sampleSize = try c.decodeIfPresent(Int.self, forKey: .sampleSize) ?? 0
    }

    /// True once the council has gone at least `dryRunThreshold` finished
    /// runs without minting work — the "deliberating but producing nothing"
    /// signal an operator should look at.
    public var isDrySpell: Bool { runsSinceLastDelta >= Self.dryRunThreshold }

    /// Four runs is a full day of the 6-hourly cron cadence.
    public static let dryRunThreshold = 4

    /// One-line human summary. Mirrors `loom mills status` so the phone and
    /// the CLI describe the same yield the same way.
    public func summary(now: Date = Date()) -> String {
        if sampleSize == 0 { return "No finished council runs yet." }
        if runsSinceLastDelta == 0 { return "Last council run produced backlog deltas." }
        let runs = "\(runsSinceLastDelta) run\(runsSinceLastDelta == 1 ? "" : "s") without a backlog delta"
        let cost = " (\(LoomFormat.usd(costSinceLastDeltaUSD)))"
        if let lastDeltaAt {
            return runs + cost + " · last delta \(LoomFormat.relative(from: lastDeltaAt, now: now))"
        }
        return runs + cost + " · none in the last \(sampleSize) examined"
    }
}

/// GET /api/mills/status.
public struct MillsOperatorStatus: Codable, Sendable, Hashable {
    public let buildSHA: String?
    public let dbOK: Bool
    public let policyEnabled: Bool
    public let policyVersion: Int?
    public let autonomyReady: Bool?
    public let autonomyBlockers: [String]?
    public let capabilities: [MillsCapability]?
    public let budget: [String: MillsBudgetTier]?
    public let queueDepth: Int?
    public let activePipelineRuns: Int?
    public let lastCouncilAt: Date?
    public let lastMergeAt: Date?
    public let healthGates: MillsHealthGates?
    public let healthGatesMode: String?
    public let councilYield: MillsCouncilYield?

    enum CodingKeys: String, CodingKey {
        case buildSHA = "build_sha"
        case dbOK = "db_ok"
        case policyEnabled = "policy_enabled"
        case policyVersion = "policy_version"
        case autonomyReady = "autonomy_ready"
        case autonomyBlockers = "autonomy_blockers"
        case capabilities, budget
        case queueDepth = "queue_depth"
        case activePipelineRuns = "active_pipeline_runs"
        case lastCouncilAt = "last_council_at"
        case lastMergeAt = "last_merge_at"
        case healthGates = "health_gates"
        case healthGatesMode = "health_gates_mode"
        case councilYield = "council_yield"
    }

    public init(
        buildSHA: String? = nil,
        dbOK: Bool = true,
        policyEnabled: Bool = true,
        policyVersion: Int? = nil,
        autonomyReady: Bool? = nil,
        autonomyBlockers: [String]? = nil,
        capabilities: [MillsCapability]? = nil,
        budget: [String: MillsBudgetTier]? = nil,
        queueDepth: Int? = nil,
        activePipelineRuns: Int? = nil,
        lastCouncilAt: Date? = nil,
        lastMergeAt: Date? = nil,
        healthGates: MillsHealthGates? = nil,
        healthGatesMode: String? = nil,
        councilYield: MillsCouncilYield? = nil
    ) {
        self.buildSHA = buildSHA
        self.dbOK = dbOK
        self.policyEnabled = policyEnabled
        self.policyVersion = policyVersion
        self.autonomyReady = autonomyReady
        self.autonomyBlockers = autonomyBlockers
        self.capabilities = capabilities
        self.budget = budget
        self.queueDepth = queueDepth
        self.activePipelineRuns = activePipelineRuns
        self.lastCouncilAt = lastCouncilAt
        self.lastMergeAt = lastMergeAt
        self.healthGates = healthGates
        self.healthGatesMode = healthGatesMode
        self.councilYield = councilYield
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        buildSHA = try c.decodeIfPresent(String.self, forKey: .buildSHA)
        dbOK = try c.decodeIfPresent(Bool.self, forKey: .dbOK) ?? false
        policyEnabled = try c.decodeIfPresent(Bool.self, forKey: .policyEnabled) ?? false
        policyVersion = try c.decodeIfPresent(Int.self, forKey: .policyVersion)
        autonomyReady = try c.decodeIfPresent(Bool.self, forKey: .autonomyReady)
        autonomyBlockers = try c.decodeIfPresent([String].self, forKey: .autonomyBlockers)
        capabilities = try c.decodeIfPresent([MillsCapability].self, forKey: .capabilities)
        // `budget` is `{}` when the budget reader is unwired and a tier is
        // omitted when its window read failed — never null.
        budget = try c.decodeIfPresent([String: MillsBudgetTier].self, forKey: .budget)
        queueDepth = try c.decodeIfPresent(Int.self, forKey: .queueDepth)
        activePipelineRuns = try c.decodeIfPresent(Int.self, forKey: .activePipelineRuns)
        lastCouncilAt = try c.decodeIfPresent(Date.self, forKey: .lastCouncilAt)
        lastMergeAt = try c.decodeIfPresent(Date.self, forKey: .lastMergeAt)
        healthGates = try c.decodeIfPresent(MillsHealthGates.self, forKey: .healthGates)
        healthGatesMode = try c.decodeIfPresent(String.self, forKey: .healthGatesMode)
        councilYield = try c.decodeIfPresent(MillsCouncilYield.self, forKey: .councilYield)
    }

    // MARK: - Semantics

    /// The single verdict the card leads with. Precedence matters: a paused
    /// policy explains everything downstream, a fail-closed health gate holds
    /// dispatch regardless of capabilities, and only then does autonomy
    /// readiness (which folds the capability matrix) get a say.
    public enum Verdict: Sendable, Equatable {
        /// `policy.enabled=false` — the kill switch is on.
        case paused
        /// Health gates are blocking admission (only when the mode enforces).
        case held(reasons: [String])
        /// A required capability is red or stubbed.
        case blocked(reasons: [String])
        /// Everything required is green and the policy is on.
        case ready
        /// An operator predating the capability report — no verdict data.
        case unknown

        public var label: String {
            switch self {
            case .paused: return "Paused"
            case .held: return "Held"
            case .blocked: return "Blocked"
            case .ready: return "Ready"
            case .unknown: return "Unknown"
            }
        }
    }

    public var verdict: Verdict {
        if !policyEnabled { return .paused }
        if let healthGates, !healthGates.allowed, healthGatesEnforcing {
            return .held(reasons: healthGates.reasons ?? [])
        }
        guard let autonomyReady else { return .unknown }
        if autonomyReady { return .ready }
        let reasons = (autonomyBlockers?.isEmpty == false)
            ? autonomyBlockers!
            : degradedCapabilities.map { "\($0.id) is \($0.status)" }
        return .blocked(reasons: reasons)
    }

    /// True when a blocking health-gate verdict actually holds dispatch. In
    /// `observe` mode the gates only report, so a `block` there is a warning,
    /// not a hold — the card shows it as a chip instead of the verdict.
    public var healthGatesEnforcing: Bool {
        guard let mode = healthGatesMode?.lowercased() else { return true }
        return mode != "observe"
    }

    /// Capability rows that are not green, required ones first, then by id —
    /// the rows an operator needs to see without opening a debug view.
    public var degradedCapabilities: [MillsCapability] {
        (capabilities ?? [])
            .filter { !$0.isGreen }
            .sorted { lhs, rhs in
                if lhs.requiredForAutonomy != rhs.requiredForAutonomy { return lhs.requiredForAutonomy }
                return lhs.id < rhs.id
            }
    }

    /// "10/11 green" for the capability chip; nil without a matrix.
    public var capabilityRollup: String? {
        guard let capabilities, !capabilities.isEmpty else { return nil }
        let green = capabilities.filter(\.isGreen).count
        return "\(green)/\(capabilities.count) green"
    }

    /// Budget tiers in display order (pipeline first, council second, any
    /// extra tier alphabetically) — the map's iteration order is random.
    public var budgetTiers: [(name: String, tier: MillsBudgetTier)] {
        guard let budget else { return [] }
        let known = ["pipeline", "council"]
        var ordered: [(String, MillsBudgetTier)] = known.compactMap { name in
            budget[name].map { (name, $0) }
        }
        for name in budget.keys.sorted() where !known.contains(name) {
            ordered.append((name, budget[name]!))
        }
        return ordered.map { (name: $0.0, tier: $0.1) }
    }

    /// True when anything on the card wants attention beyond "ready":
    /// a non-ready verdict, an observed gate block, a near-cap tier, or a
    /// council dry spell. Drives the card accent.
    public var needsAttention: Bool {
        if verdict != .ready { return verdict != .unknown }
        if let healthGates, !healthGates.allowed { return true }
        if budgetTiers.contains(where: { $0.tier.isNearCap }) { return true }
        if councilYield?.isDrySpell == true { return true }
        return false
    }
}
