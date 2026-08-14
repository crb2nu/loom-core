// Vendor session bridge reads — the iOS twin of the web Operator Deck's
// "Vendor transcripts" affordance (!1251 HTTP surface).
//
// GET /api/vendor-sessions and GET /api/vendor-sessions/search are HUD-served
// bare JSON (internal/hud/domain/vendorsessions) fronting the agent-context
// bridge's transcript reader: the operator's window into what the claude and
// codex desktop CLIs did on the workstation, which presence/fleet views
// cannot see.
//
// Degradation contract, in two distinct tiers:
//   - `degraded` — the HUD answered 200 but its agent bridge is offline
//     (`degraded:true` with an empty list). Rendered as "bridge offline",
//     never as "no sessions on this host".
//   - `unavailable` — the read didn't reach a vendor-sessions handler at
//     all: an older daemon whose mobile allowlist predates these routes
//     answers 403 (`forbidden`, same failure mode as the /api/patterns
//     rollout), a daemon without the domain 404s, and 502/503 arrive as
//     `.upstreamError`/`.notConfigured`. All fold to an empty result the
//     caller can label "needs a newer HUD" instead of an error toast.

import Foundation

/// One vendor CLI session transcript on the HUD's workstation. Mirrors
/// `internal/hud/bridge.VendorSessionInfo` (snake_case wire shape).
public struct VendorSession: Codable, Sendable, Hashable {
    public let vendor: String
    public let id: String
    public let path: String
    public let cwd: String?
    public let source: String?
    /// RFC3339 strings passed through verbatim — the bridge already carries
    /// timestamps as strings, and Go emits nanosecond precision the strict
    /// date-decoding path may reject. Parse at render time with
    /// `LoomFormat.relativeCompact(fromISO:)` (which degrades to "—") so one
    /// odd timestamp can never sink the whole payload.
    public let startedAt: String?
    public let modifiedAt: String?
    public let sizeBytes: Int64?
    /// Source workstation for federated rows (the HUD mirror pushes another
    /// host's transcripts); nil for transcripts read by the HUD's own bridge.
    public let host: String?

    /// Stable list identity — session ids can collide across vendors.
    public var uid: String { "\(vendor):\(id)" }

    enum CodingKeys: String, CodingKey {
        case vendor, id, path, cwd, source, host
        case startedAt = "started_at"
        case modifiedAt = "modified_at"
        case sizeBytes = "size_bytes"
    }

    public init(
        vendor: String,
        id: String,
        path: String,
        cwd: String? = nil,
        source: String? = nil,
        startedAt: String? = nil,
        modifiedAt: String? = nil,
        sizeBytes: Int64? = nil,
        host: String? = nil
    ) {
        self.vendor = vendor
        self.id = id
        self.path = path
        self.cwd = cwd
        self.source = source
        self.startedAt = startedAt
        self.modifiedAt = modifiedAt
        self.sizeBytes = sizeBytes
        self.host = host
    }
}

/// One search hit inside a vendor session transcript. Mirrors
/// `internal/hud/bridge.VendorSessionMatch`.
public struct VendorSessionMatch: Codable, Sendable, Hashable {
    public let vendor: String
    public let sessionId: String
    public let path: String
    public let cwd: String?
    /// 1-based transcript line; 0 when the federating mirror tail-seeked a
    /// large transcript and absolute numbering is unknown — hide it then.
    public let line: Int
    public let role: String?
    public let timestamp: String?
    public let snippet: String
    public let host: String?

    /// Stable list identity — a session can match on many lines.
    public var uid: String { "\(vendor):\(sessionId):\(line):\(snippet.hashValue)" }

    enum CodingKeys: String, CodingKey {
        case vendor, path, cwd, line, role, timestamp, snippet, host
        case sessionId = "session_id"
    }

    public init(
        vendor: String,
        sessionId: String,
        path: String,
        cwd: String? = nil,
        line: Int,
        role: String? = nil,
        timestamp: String? = nil,
        snippet: String,
        host: String? = nil
    ) {
        self.vendor = vendor
        self.sessionId = sessionId
        self.path = path
        self.cwd = cwd
        self.line = line
        self.role = role
        self.timestamp = timestamp
        self.snippet = snippet
        self.host = host
    }
}

/// Wire shape of GET /api/vendor-sessions:
/// `{"sessions": [...], "count": <int>, "degraded": <bool>}`.
public struct VendorSessionListResponse: Codable, Sendable {
    public let sessions: [VendorSession]
    public let degraded: Bool

    public init(sessions: [VendorSession], degraded: Bool = false) {
        self.sessions = sessions
        self.degraded = degraded
    }
}

/// Wire shape of GET /api/vendor-sessions/search:
/// `{"query": <string>, "matches": [...], "count": <int>, "degraded": <bool>}`.
public struct VendorSessionSearchResponse: Codable, Sendable {
    public let query: String
    public let matches: [VendorSessionMatch]
    public let degraded: Bool

    public init(query: String, matches: [VendorSessionMatch], degraded: Bool = false) {
        self.query = query
        self.matches = matches
        self.degraded = degraded
    }
}

/// Outcome of a transcript list read, with the two degradation tiers the
/// screen renders differently (see the file header).
public struct VendorSessionsResult: Sendable, Equatable {
    public let sessions: [VendorSession]
    public let degraded: Bool
    public let unavailable: Bool

    public init(sessions: [VendorSession], degraded: Bool = false, unavailable: Bool = false) {
        self.sessions = sessions
        self.degraded = degraded
        self.unavailable = unavailable
    }
}

/// Outcome of a transcript substring search.
public struct VendorSessionSearchResult: Sendable, Equatable {
    public let query: String
    public let matches: [VendorSessionMatch]
    public let degraded: Bool
    public let unavailable: Bool

    public init(query: String, matches: [VendorSessionMatch], degraded: Bool = false, unavailable: Bool = false) {
        self.query = query
        self.matches = matches
        self.degraded = degraded
        self.unavailable = unavailable
    }
}

/// Read-only vendor transcript surface. Views depend on the protocol so
/// test fakes can short-circuit network calls.
public protocol VendorSessionsAPIProtocol: Sendable {
    /// Newest-modified-first transcript listing.
    func recentSessions(cwdContains: String?, limit: Int?) async throws -> VendorSessionsResult
    /// Substring grep across transcripts.
    func search(query: String, cwdContains: String?, maxResults: Int?) async throws -> VendorSessionSearchResult
}

/// Concrete client backed by `LoomAPIClientProtocol`, decoding through the
/// bare-JSON path (these routes never wrap in the mobile envelope).
public struct VendorSessionsAPI: VendorSessionsAPIProtocol, Sendable {
    private let client: LoomAPIClientProtocol

    public init(client: LoomAPIClientProtocol) {
        self.client = client
    }

    public func recentSessions(cwdContains: String? = nil, limit: Int? = nil) async throws -> VendorSessionsResult {
        do {
            let wire: VendorSessionListResponse = try await client.requestRaw(
                .vendorSessions(cwdContains: cwdContains, limit: limit))
            return VendorSessionsResult(sessions: wire.sessions, degraded: wire.degraded)
        } catch let LoomAPIError.apiError(code, _, _)
            where code == .forbidden || code == .notFound
                || code == .notConfigured || code == .upstreamError {
            return VendorSessionsResult(sessions: [], unavailable: true)
        }
    }

    public func search(query: String, cwdContains: String? = nil, maxResults: Int? = nil) async throws -> VendorSessionSearchResult {
        do {
            let wire: VendorSessionSearchResponse = try await client.requestRaw(
                .vendorSessionSearch(query: query, cwdContains: cwdContains, maxResults: maxResults))
            return VendorSessionSearchResult(query: wire.query, matches: wire.matches, degraded: wire.degraded)
        } catch let LoomAPIError.apiError(code, _, _)
            where code == .forbidden || code == .notFound
                || code == .notConfigured || code == .upstreamError {
            return VendorSessionSearchResult(query: query, matches: [], unavailable: true)
        }
    }
}
