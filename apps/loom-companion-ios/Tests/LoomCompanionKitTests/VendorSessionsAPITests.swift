import Foundation
import Testing
@testable import LoomCompanionKit

/// Vendor session bridge reads (!1251 HTTP surface, iOS twin of the web
/// Operator Deck's VendorTranscripts affordance).
@Suite("VendorSessionsAPI")
struct VendorSessionsAPITests {
    private func makeClient() -> APIClient {
        APIClient(baseURL: URL(string: "https://localhost:3333")!, token: "test-token")
    }

    // MARK: - Raw decode (handler-shaped bytes)

    @Test("list payload decodes the handler's snake_case shape")
    func decodesListPayload() throws {
        // Shape from internal/hud/domain/vendorsessions.handleList; timestamps
        // are RFC3339Nano strings passed through the bridge verbatim.
        let json = """
        {"sessions":[
          {"vendor":"claude","id":"9adcaf2d-75d6-41c5-a9dd-cb686bcbe311","path":"/Users/u/.claude/projects/-x/9adcaf2d.jsonl","cwd":"/Users/u/workspace/services/loom-core","source":"projects","started_at":"2026-07-26T08:01:02.123456789-05:00","modified_at":"2026-07-26T09:15:32.987654321-05:00","size_bytes":48213},
          {"vendor":"codex","id":"0198c2f4","path":"/Users/u/.codex/sessions/2026/07/26/rollout-0198c2f4.jsonl","modified_at":"2026-07-26T07:00:00Z","size_bytes":1024}
        ],"count":2,"degraded":false}
        """
        let resp: VendorSessionListResponse = try makeClient().decodeRaw(Data(json.utf8), statusCode: 200)
        #expect(resp.sessions.count == 2)
        #expect(resp.degraded == false)
        #expect(resp.sessions[0].vendor == "claude")
        #expect(resp.sessions[0].cwd == "/Users/u/workspace/services/loom-core")
        #expect(resp.sessions[0].sizeBytes == 48213)
        // Nanosecond-precision timestamps must survive as strings the
        // formatter can parse at render time.
        #expect(resp.sessions[0].modifiedAt?.hasPrefix("2026-07-26T09:15:32") == true)
        // Optional fields (cwd/source/started_at) may be omitted entirely.
        #expect(resp.sessions[1].cwd == nil)
        #expect(resp.sessions[1].startedAt == nil)
        #expect(resp.sessions[1].uid == "codex:0198c2f4")
    }

    @Test("degraded list placeholder decodes (bridge offline)")
    func decodesDegradedList() throws {
        let json = #"{"sessions":[],"count":0,"degraded":true}"#
        let resp: VendorSessionListResponse = try makeClient().decodeRaw(Data(json.utf8), statusCode: 200)
        #expect(resp.sessions.isEmpty)
        #expect(resp.degraded)
    }

    @Test("search payload decodes matches with session_id and line")
    func decodesSearchPayload() throws {
        let json = """
        {"query":"pipeline","matches":[
          {"vendor":"claude","session_id":"9adcaf2d","path":"/p.jsonl","cwd":"/Users/u/workspace","line":42,"role":"assistant","timestamp":"2026-07-26T09:15:32Z","snippet":"…the pipeline wedged on…"},
          {"vendor":"codex","session_id":"0198c2f4","path":"/q.jsonl","line":7,"snippet":"pipeline retry"}
        ],"count":2,"degraded":false}
        """
        let resp: VendorSessionSearchResponse = try makeClient().decodeRaw(Data(json.utf8), statusCode: 200)
        #expect(resp.query == "pipeline")
        #expect(resp.matches.count == 2)
        #expect(resp.matches[0].sessionId == "9adcaf2d")
        #expect(resp.matches[0].line == 42)
        #expect(resp.matches[0].role == "assistant")
        #expect(resp.matches[1].role == nil)
        // uid ends with the snippet's run-seeded hash (so federated line-0
        // matches stay distinct in ForEach) — assert the stable prefix only.
        #expect(resp.matches[1].uid.hasPrefix("codex:0198c2f4:7:"))
    }

    @Test("matches sharing session and line=0 keep distinct uids")
    func lineZeroMatchesStayDistinct() throws {
        let a = VendorSessionMatch(vendor: "claude", sessionId: "s", path: "/p", line: 0, snippet: "first hit")
        let b = VendorSessionMatch(vendor: "claude", sessionId: "s", path: "/p", line: 0, snippet: "second hit")
        #expect(a.uid != b.uid)
    }

    @Test("federated rows decode host and unknown fields stay ignorable")
    func decodesHostField() throws {
        let json = """
        {"sessions":[{"vendor":"claude","id":"abc","path":"/p.jsonl","modified_at":"2026-07-26T09:00:00Z","size_bytes":10,"host":"codys-mac","future_field":true}],"count":1,"degraded":false}
        """
        let resp: VendorSessionListResponse = try makeClient().decodeRaw(Data(json.utf8), statusCode: 200)
        #expect(resp.sessions[0].host == "codys-mac")
    }

    // MARK: - API wrapper degradation folds

    @Test("recentSessions passes degraded through and lists sessions")
    func recentSessionsPassthrough() async throws {
        let mock = MockAPIClient()
        mock.vendorSessionsResponse = VendorSessionListResponse(
            sessions: [VendorSession(vendor: "claude", id: "abc", path: "/p.jsonl")],
            degraded: false
        )
        let api = VendorSessionsAPI(client: mock)
        let res = try await api.recentSessions(cwdContains: nil, limit: 8)
        #expect(res.sessions.count == 1)
        #expect(!res.degraded)
        #expect(!res.unavailable)
        #expect(mock.lastVendorSessionsQuery?.limit == 8)
    }

    @Test("recentSessions folds 403 to unavailable (older daemon allowlist)")
    func recentSessionsFoldsForbidden() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/vendor-sessions"] =
            .apiError(code: .forbidden, message: "path not allowed", requestId: "")
        let api = VendorSessionsAPI(client: mock)
        let res = try await api.recentSessions(cwdContains: nil, limit: nil)
        #expect(res.sessions.isEmpty)
        #expect(res.unavailable)
        #expect(!res.degraded)
    }

    @Test("search folds 502 to unavailable (bridge call error)")
    func searchFoldsUpstream() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/vendor-sessions/search"] =
            .apiError(code: .upstreamError, message: "vendor session search", requestId: "")
        let api = VendorSessionsAPI(client: mock)
        let res = try await api.search(query: "x", cwdContains: nil, maxResults: nil)
        #expect(res.matches.isEmpty)
        #expect(res.unavailable)
        #expect(res.query == "x")
    }

    @Test("search reports the bridge-offline placeholder as degraded")
    func searchDegraded() async throws {
        let mock = MockAPIClient()
        mock.vendorSessionSearchResponse = VendorSessionSearchResponse(
            query: "x", matches: [], degraded: true
        )
        let api = VendorSessionsAPI(client: mock)
        let res = try await api.search(query: "x", cwdContains: "loom-core", maxResults: 20)
        #expect(res.degraded)
        #expect(!res.unavailable)
        #expect(mock.lastVendorSearch?.query == "x")
        #expect(mock.lastVendorSearch?.cwdContains == "loom-core")
        #expect(mock.lastVendorSearch?.maxResults == 20)
    }

    @Test("unexpected errors still throw (not silently folded)")
    func unexpectedErrorsThrow() async {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/vendor-sessions"] =
            .apiError(code: .unauthorized, message: "bad token", requestId: "")
        let api = VendorSessionsAPI(client: mock)
        await #expect(throws: LoomAPIError.self) {
            _ = try await api.recentSessions(cwdContains: nil, limit: nil)
        }
    }

    // MARK: - Endpoint assembly

    @Test("list endpoint builds query params only when set")
    func listEndpointQuery() throws {
        let base = URL(string: "https://localhost:3333")!
        let bare = try Endpoint.vendorSessions().urlRequest(baseURL: base)
        #expect(bare.url?.absoluteString == "https://localhost:3333/api/vendor-sessions")
        #expect(bare.httpMethod == "GET")

        let filtered = try Endpoint.vendorSessions(cwdContains: "loom-core", limit: 8)
            .urlRequest(baseURL: base)
        let comps = URLComponents(url: filtered.url!, resolvingAgainstBaseURL: false)!
        #expect(comps.path == "/api/vendor-sessions")
        #expect(comps.queryItems?.contains(URLQueryItem(name: "cwd_contains", value: "loom-core")) == true)
        #expect(comps.queryItems?.contains(URLQueryItem(name: "limit", value: "8")) == true)
    }

    @Test("search endpoint always carries the required query param")
    func searchEndpointQuery() throws {
        let base = URL(string: "https://localhost:3333")!
        let req = try Endpoint.vendorSessionSearch(query: "hub mux", maxResults: 20)
            .urlRequest(baseURL: base)
        let comps = URLComponents(url: req.url!, resolvingAgainstBaseURL: false)!
        #expect(comps.path == "/api/vendor-sessions/search")
        #expect(comps.queryItems?.contains(URLQueryItem(name: "query", value: "hub mux")) == true)
        #expect(comps.queryItems?.contains(URLQueryItem(name: "max_results", value: "20")) == true)
        #expect(comps.queryItems?.contains(where: { $0.name == "cwd_contains" }) == false)
    }
}
