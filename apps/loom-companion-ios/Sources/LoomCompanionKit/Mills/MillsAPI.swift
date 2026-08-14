// Mills API client (Phase 7 slice 7.5).
//
// Thin wrapper over LoomAPIClientProtocol that exposes the two read
// endpoints the mobile Mills screen consumes: pipeline runs and the
// latest KPI snapshot. Both are proxied by the HUD's /api/mills/* tier
// (see internal/hud/domain/mills/mills.go); this file does NOT talk to
// the operator directly.
//
// Degradation contract: the mills proxy returns BARE (non-enveloped)
// error bodies, so operator-absence and operator-unreachability reach us
// only as HTTP status codes, never as an enveloped `not_configured`
// error. When the operator URL is unset the HUD returns HTTP 503
// (`{"error":"loom-mills operator not configured"}`); when the operator
// is down the reverse proxy returns HTTP 502. APIClient maps both to
// `LoomAPIError.apiError(code: .upstreamError, ...)`. This client treats
// that — alongside 404 (no data yet) — as a calm empty state rather than
// an error, so a restarting operator doesn't flash "Couldn't reach Mills".

import Foundation

/// Mirrors `pkg/mills/store.PipelineRun` (default Go JSON encoding uses
/// uppercase field names, which is why CodingKeys do, too).
public struct MillsPipelineRun: Codable, Sendable, Identifiable, Hashable {
    public let id: String
    public let backlogID: String
    public let template: String
    public let state: String
    public let attempts: Int
    public let startedAt: Date?
    public let endedAt: Date?
    public let parentRunID: String?
    public let depth: Int?
    /// Aggregate stage spend. Detail-only on the wire (list responses may
    /// omit it), so it's optional; the shift report sums whatever arrives.
    public let costUSD: Double?

    enum CodingKeys: String, CodingKey {
        case id = "ID"
        case backlogID = "BacklogID"
        case template = "Template"
        case state = "State"
        case attempts = "Attempts"
        case startedAt = "StartedAt"
        case endedAt = "EndedAt"
        case parentRunID = "ParentRunID"
        case depth = "Depth"
        case costUSD = "CostUSD"
    }

    public init(
        id: String,
        backlogID: String,
        template: String,
        state: String,
        attempts: Int,
        startedAt: Date? = nil,
        endedAt: Date? = nil,
        parentRunID: String? = nil,
        depth: Int? = nil,
        costUSD: Double? = nil
    ) {
        self.id = id
        self.backlogID = backlogID
        self.template = template
        self.state = state
        self.attempts = attempts
        self.startedAt = startedAt
        self.endedAt = endedAt
        self.parentRunID = parentRunID
        self.depth = depth
        self.costUSD = costUSD
    }
}

/// Mirrors `pkg/mills/store.KPISnapshot`. `metrics` is an open map so
/// the screen can pluck whatever the operator decides to publish without
/// the Swift side needing a release for every metric addition.
public struct MillsKPISnapshot: Codable, Sendable, Hashable {
    public let id: Int64?
    public let snapshotAt: Date?
    public let windowSeconds: Int?
    public let metrics: [String: Double]

    enum CodingKeys: String, CodingKey {
        case id = "ID"
        case snapshotAt = "SnapshotAt"
        case windowSeconds = "WindowSeconds"
        case metrics = "Metrics"
    }

    public init(
        id: Int64? = nil,
        snapshotAt: Date? = nil,
        windowSeconds: Int? = nil,
        metrics: [String: Double] = [:]
    ) {
        self.id = id
        self.snapshotAt = snapshotAt
        self.windowSeconds = windowSeconds
        self.metrics = metrics
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.id = try c.decodeIfPresent(Int64.self, forKey: .id)
        self.snapshotAt = try c.decodeIfPresent(Date.self, forKey: .snapshotAt)
        self.windowSeconds = try c.decodeIfPresent(Int.self, forKey: .windowSeconds)
        // The Go side serializes `Metrics` as map[string]any. Coerce
        // to [String: Double] best-effort; non-numeric values are dropped
        // since the screen only renders numbers.
        let raw = try c.decodeIfPresent([String: AnyCodable].self, forKey: .metrics) ?? [:]
        var coerced: [String: Double] = [:]
        for (k, v) in raw {
            if let d = v.doubleValue { coerced[k] = d }
        }
        self.metrics = coerced
    }
}

/// Read-only Mills client surface. ViewModels and previews depend on the
/// protocol so test fakes can short-circuit network calls.
public protocol MillsAPIProtocol: Sendable {
    func pipelineRuns() async throws -> [MillsPipelineRun]
    func latestKPI(window: String) async throws -> MillsKPISnapshot?

    // Shift report reads (port of the web Factory panel's overlay). All
    // degrade to empty/nil like the two reads above so a missing operator
    // renders a quiet shift, not an error.
    /// Up to `limit` terminal runs from the operator's archive window
    /// (GET /api/mills/pipeline/runs?state=terminal&limit=N).
    func terminalRuns(limit: Int) async throws -> [MillsPipelineRun]
    /// One run's detail (gates included); nil when unavailable — the
    /// report degrades to "no gate detail" for that spark.
    func runDetail(id: String) async throws -> MillsPipelineRunDetail?
    /// The backlog list (run → backlog → PlanID pattern attribution).
    func backlog() async throws -> [MillsBacklogItem]
    /// Approved Pattern Loom catalog entries (GET /api/patterns?status=approved).
    func approvedPatterns() async throws -> [MillsPatternInfo]
    /// Same read, but reporting whether the catalog was actually reachable so
    /// callers can say "patterns unavailable" instead of silently rendering a
    /// shift with no pattern attribution. Defaulted so existing conformances
    /// (test fakes) keep compiling.
    func approvedPatternsResult() async throws -> MillsPatternsResult
}

/// Outcome of a Pattern Loom catalog read.
///
/// `unavailable` is true when the catalog could not be read at all — an older
/// daemon whose mobile allowlist predates `/api/patterns` answers 403
/// (`forbidden`), a daemon without the patterns surface answers 404, and an
/// unconfigured/unreachable operator answers 503/502. All of those degrade to
/// an empty catalog rather than an error, but the caller should say so.
public struct MillsPatternsResult: Sendable, Equatable {
    public let patterns: [MillsPatternInfo]
    public let unavailable: Bool

    public init(patterns: [MillsPatternInfo], unavailable: Bool = false) {
        self.patterns = patterns
        self.unavailable = unavailable
    }
}

extension MillsAPIProtocol {
    public func approvedPatternsResult() async throws -> MillsPatternsResult {
        MillsPatternsResult(patterns: try await approvedPatterns())
    }
}

/// Concrete client backed by the existing `LoomAPIClientProtocol`. The
/// HUD's mills proxy returns 404 when no KPI snapshot exists yet — that
/// surfaces as `LoomAPIError.notFound` from the underlying client; the
/// Mills screen treats that as "no data yet" rather than an error.
public struct MillsAPI: MillsAPIProtocol, Sendable {
    private let client: LoomAPIClientProtocol

    public init(client: LoomAPIClientProtocol) {
        self.client = client
    }

    public func pipelineRuns() async throws -> [MillsPipelineRun] {
        do {
            // The `/api/mills/*` proxy returns bare operator JSON, so decode
            // without the mobile envelope. The empty active-runs set has been
            // encoded as both JSON `null` (older operator) and `[]` (current),
            // so decode through an optional and coalesce — either means "no
            // in-flight runs", not a failure.
            let runs: [MillsPipelineRun]? = try await client.requestRaw(.millsPipelineRuns())
            return runs ?? []
        } catch let LoomAPIError.apiError(code, _, _)
            where code == .notFound || code == .notConfigured || code == .upstreamError {
            // Operator absent (bare 503), unreachable (bare 502 → .upstreamError),
            // or unconfigured → no runs (calm empty state). See the type-level
            // degradation contract above for why 502/503 arrive as .upstreamError.
            return []
        }
    }

    public func latestKPI(window: String = "1d") async throws -> MillsKPISnapshot? {
        do {
            let snap: MillsKPISnapshot = try await client.requestRaw(.millsKPIs(window: window))
            return snap
        } catch let LoomAPIError.apiError(code, _, _)
            where code == .notFound || code == .notConfigured || code == .upstreamError {
            // No snapshot yet (404), operator URL unset (bare 503), or operator
            // unreachable (bare 502 → .upstreamError) → render the empty state
            // instead of an error toast.
            return nil
        }
    }

    public func terminalRuns(limit: Int = 500) async throws -> [MillsPipelineRun] {
        do {
            let runs: [MillsPipelineRun]? = try await client.requestRaw(
                .millsPipelineRuns(state: "terminal", limit: limit))
            return runs ?? []
        } catch let LoomAPIError.apiError(code, _, _)
            where code == .notFound || code == .notConfigured || code == .upstreamError {
            return []
        }
    }

    public func runDetail(id: String) async throws -> MillsPipelineRunDetail? {
        do {
            let detail: MillsPipelineRunDetail = try await client.requestRaw(
                .millsPipelineRunDetail(id: id))
            return detail
        } catch let LoomAPIError.apiError(code, _, _)
            where code == .notFound || code == .notConfigured || code == .upstreamError {
            return nil
        }
    }

    public func backlog() async throws -> [MillsBacklogItem] {
        do {
            let items: [MillsBacklogItem]? = try await client.requestRaw(.millsBacklog)
            return items ?? []
        } catch let LoomAPIError.apiError(code, _, _)
            where code == .notFound || code == .notConfigured || code == .upstreamError {
            return []
        }
    }

    public func approvedPatterns() async throws -> [MillsPatternInfo] {
        try await approvedPatternsResult().patterns
    }

    public func approvedPatternsResult() async throws -> MillsPatternsResult {
        do {
            // /api/patterns is HUD-served (not the mills proxy) but bare
            // JSON all the same: {"patterns": [...]}, null when empty.
            let catalog: MillsPatternCatalog = try await client.requestRaw(
                .patternsCatalog(status: "approved"))
            return MillsPatternsResult(patterns: catalog.patterns ?? [])
        } catch let LoomAPIError.apiError(code, _, _)
            // `.forbidden` covers daemons predating the /api/patterns entry in
            // the mobile companion allowlist (internal/hud/api_mobile.go) —
            // without it the shift report's pattern read threw and the whole
            // section vanished with no explanation.
            where code == .notFound || code == .notConfigured
                || code == .upstreamError || code == .forbidden {
            return MillsPatternsResult(patterns: [], unavailable: true)
        }
    }
}
