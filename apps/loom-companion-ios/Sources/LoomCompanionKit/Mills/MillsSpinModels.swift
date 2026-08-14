// Spinning Room models (mobile port of Live Beam slice 3 + async spins,
// plan .loom/166).
//
// Mirrors the operator's JSON shapes:
//   - GET  /api/mills/spinning-room/frames → MillsSpinningRoom
//   - GET  /api/mills/spin/runs            → [MillsSpinRun] (bare array)
//   - POST /api/mills/spin/async           → 202 {"spin_id": "..."}
//
// All of these ride the HUD's /api/mills/* reverse proxy and return BARE
// (non-enveloped) JSON, so callers decode through `requestRaw`. Semantics
// (status buckets, in-flight filtering) live here so they're unit-tested and
// shared with any future surface — views stay presentation-only.

import Foundation

/// One policy-allowed Spinning-Room model frame
/// (`spinning_room.frames` in operator policy).
public struct MillsFrame: Codable, Sendable, Identifiable, Hashable {
    public let name: String
    public let model: String
    public let backend: String?

    public var id: String { name }

    public init(name: String, model: String, backend: String? = nil) {
        self.name = name
        self.model = model
        self.backend = backend
    }

    /// Backend for display; the operator treats empty as flexinfer.
    public var displayBackend: String {
        let b = (backend ?? "").trimmingCharacters(in: .whitespaces)
        return b.isEmpty ? "flexinfer" : b
    }
}

/// The Spinning Room's live policy view: whether spinning is enabled +
/// reachable, and which frames the operator may pick.
public struct MillsSpinningRoom: Codable, Sendable, Hashable {
    public let enabled: Bool
    public let available: Bool
    public let defaultPriority: String
    public let frames: [MillsFrame]

    enum CodingKeys: String, CodingKey {
        case enabled
        case available
        case defaultPriority = "default_priority"
        case frames
    }

    public init(
        enabled: Bool = true,
        available: Bool = true,
        defaultPriority: String = "P2",
        frames: [MillsFrame] = []
    ) {
        self.enabled = enabled
        self.available = available
        self.defaultPriority = defaultPriority
        self.frames = frames
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        // Mirror the HUD dialog's tolerance: missing keys mean "fine".
        self.enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? true
        self.available = try c.decodeIfPresent(Bool.self, forKey: .available) ?? true
        self.defaultPriority = try c.decodeIfPresent(String.self, forKey: .defaultPriority) ?? "P2"
        self.frames = try c.decodeIfPresent([MillsFrame].self, forKey: .frames) ?? []
    }

    /// A human reason the room can't spin right now, or nil when it can.
    public var unavailableReason: String? {
        if !enabled { return "Spinning Room is disabled in policy." }
        if !available { return "Operator can't reach the Spinning Room right now." }
        if frames.isEmpty { return "No frames configured in policy." }
        return nil
    }
}

/// Status buckets for an async spin, mirroring `pkg/mills/store.SpinStatus`.
/// Free-form on the wire; unknown values bucket to `.unknown` so an operator
/// upgrade can't crash the app.
public enum MillsSpinStatus: String, Sendable, Equatable, CaseIterable {
    case pending
    case running
    case succeeded
    case failed
    case timeout
    case unknown

    public init(wire: String) {
        self = MillsSpinStatus(rawValue: wire.lowercased()) ?? .unknown
    }

    /// Terminal = the spin finished (well or badly); non-terminal keeps polling.
    public var isTerminal: Bool {
        switch self {
        case .succeeded, .failed, .timeout: return true
        case .pending, .running, .unknown: return false
        }
    }
}

/// One async Spinning-Room spin, mirroring `pkg/mills/store.SpinRun`
/// (lowercase JSON tags, RFC3339 dates).
public struct MillsSpinRun: Codable, Sendable, Identifiable, Hashable {
    public let id: String
    public let brief: String
    public let frames: [String]
    public let priority: String?
    public let project: String?
    public let namespace: String?
    public let status: String
    public let planIDs: [String]
    public let error: String?
    public let competitive: Bool
    public let startedAt: Date?
    public let endedAt: Date?

    enum CodingKeys: String, CodingKey {
        case id, brief, frames, priority, project, namespace, status
        case planIDs = "plan_ids"
        case error, competitive
        case startedAt = "started_at"
        case endedAt = "ended_at"
    }

    public init(
        id: String,
        brief: String,
        frames: [String],
        priority: String? = nil,
        project: String? = nil,
        namespace: String? = nil,
        status: String,
        planIDs: [String] = [],
        error: String? = nil,
        competitive: Bool = false,
        startedAt: Date? = nil,
        endedAt: Date? = nil
    ) {
        self.id = id
        self.brief = brief
        self.frames = frames
        self.priority = priority
        self.project = project
        self.namespace = namespace
        self.status = status
        self.planIDs = planIDs
        self.error = error
        self.competitive = competitive
        self.startedAt = startedAt
        self.endedAt = endedAt
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.id = try c.decode(String.self, forKey: .id)
        self.brief = try c.decodeIfPresent(String.self, forKey: .brief) ?? ""
        self.frames = try c.decodeIfPresent([String].self, forKey: .frames) ?? []
        self.priority = try c.decodeIfPresent(String.self, forKey: .priority)
        self.project = try c.decodeIfPresent(String.self, forKey: .project)
        self.namespace = try c.decodeIfPresent(String.self, forKey: .namespace)
        self.status = try c.decodeIfPresent(String.self, forKey: .status) ?? ""
        self.planIDs = try c.decodeIfPresent([String].self, forKey: .planIDs) ?? []
        self.error = try c.decodeIfPresent(String.self, forKey: .error)
        self.competitive = try c.decodeIfPresent(Bool.self, forKey: .competitive) ?? false
        self.startedAt = try c.decodeIfPresent(Date.self, forKey: .startedAt)
        self.endedAt = try c.decodeIfPresent(Date.self, forKey: .endedAt)
    }

    public var statusKind: MillsSpinStatus { MillsSpinStatus(wire: status) }

    /// First line of the brief, for dense list rows.
    public var briefHeadline: String {
        brief.split(separator: "\n", omittingEmptySubsequences: true)
            .first.map(String.init) ?? brief
    }
}

/// A spin request for POST /api/mills/spin/async. One frame keeps the legacy
/// `{frame}` body; 2+ switches to `{frames}` (competitive) — mirroring the
/// HUD dialog so the request works against an operator image that predates
/// `frames[]`. `respunFrom` links a respin's fresh draft back to the plan it
/// redoes so the board can show lineage and offer a supersede.
public struct MillsSpinRequest: Sendable, Equatable {
    public var brief: String
    public var frames: [String]
    public var priority: String
    public var project: String
    public var namespace: String
    public var respunFrom: String?

    /// Mirrors the operator-side cap (`spin.maxCompetitiveFrames`): each frame
    /// is a live model synthesis, so an uncapped pick multiplies spend.
    public static let maxCompetitiveFrames = 3

    public init(
        brief: String,
        frames: [String],
        priority: String = "",
        project: String = "",
        namespace: String = "",
        respunFrom: String? = nil
    ) {
        self.brief = brief
        self.frames = frames
        self.priority = priority
        self.project = project
        self.namespace = namespace
        self.respunFrom = respunFrom
    }

    /// Wire body for the async spin POST.
    public func bodyJSON() -> [String: Any] {
        var body: [String: Any] = ["brief": brief]
        if frames.count == 1 {
            body["frame"] = frames[0]
        } else {
            body["frames"] = frames
        }
        let p = priority.trimmingCharacters(in: .whitespaces)
        if !p.isEmpty { body["priority"] = p }
        let proj = project.trimmingCharacters(in: .whitespaces)
        if !proj.isEmpty { body["project"] = proj }
        let ns = namespace.trimmingCharacters(in: .whitespaces)
        if !ns.isEmpty { body["namespace"] = ns }
        if let from = respunFrom, !from.isEmpty { body["respun_from"] = from }
        return body
    }
}

/// 202 response body from POST /api/mills/spin/async.
public struct MillsSpinQueued: Codable, Sendable, Hashable {
    public let spinID: String

    enum CodingKeys: String, CodingKey {
        case spinID = "spin_id"
    }

    public init(spinID: String) {
        self.spinID = spinID
    }
}

/// Pure list semantics for the MillsScreen "Spinning Room" section: what to
/// show, in which order, without the view re-deriving it.
public enum MillsSpinBoard {
    /// Spins worth showing on the mobile board: everything still moving,
    /// plus terminal spins that ended within `terminalWindow` (default 24h) —
    /// enough to see "my spin from the couch landed" without unbounded history.
    public static func visibleRuns(
        _ runs: [MillsSpinRun],
        now: Date = Date(),
        terminalWindow: TimeInterval = 24 * 3600,
        limit: Int = 6
    ) -> [MillsSpinRun] {
        let visible = runs.filter { run in
            if !run.statusKind.isTerminal { return true }
            guard let ended = run.endedAt ?? run.startedAt else { return false }
            return now.timeIntervalSince(ended) <= terminalWindow
        }
        let sorted = visible.sorted { a, b in
            // Live spins first, then most recent.
            let aLive = !a.statusKind.isTerminal
            let bLive = !b.statusKind.isTerminal
            if aLive != bLive { return aLive }
            return (a.startedAt ?? .distantPast) > (b.startedAt ?? .distantPast)
        }
        return Array(sorted.prefix(limit))
    }

    /// Whether any run still needs polling.
    public static func hasLiveSpin(_ runs: [MillsSpinRun]) -> Bool {
        runs.contains { !$0.statusKind.isTerminal }
    }
}
