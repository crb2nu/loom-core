import Foundation
import Testing
@testable import LoomCompanionKit

/// Real-bytes coverage for the bare (non-enveloped) decode path that the
/// `/api/mills/*` proxy requires. Regression for the "Couldn't reach Mills"
/// bug: the mills endpoints return raw operator JSON (a bare array / object,
/// with the empty active-runs set encoded as `null`), but the client used to
/// force `APIEnvelope` decoding — so every response failed to decode.
@Suite("Mills raw decode")
struct MillsRawDecodeTests {
    private func makeClient() -> APIClient {
        APIClient(baseURL: URL(string: "https://localhost:3333")!, token: "test-token")
    }

    @Test("bare pipeline-runs array decodes with RFC3339 dates")
    func bareArrayDecodes() throws {
        let json = """
        [
          {"ID":"PIPE-A","BacklogID":"B","Template":"mills-default","State":"implementing","Attempts":1,"Depth":0,"StartedAt":"2026-06-28T12:00:00Z"},
          {"ID":"PIPE-A-S1","BacklogID":"B1","Template":"mills-default","State":"testing","Attempts":1,"ParentRunID":"PIPE-A","Depth":1,"StartedAt":"2026-06-28T12:01:30.250Z"}
        ]
        """
        let runs: [MillsPipelineRun] = try makeClient().decodeRaw(Data(json.utf8), statusCode: 200)
        #expect(runs.count == 2)
        #expect(runs[0].id == "PIPE-A")
        #expect(runs[0].startedAt != nil)
        #expect(runs[1].parentRunID == "PIPE-A")
        // Fractional-second RFC3339 must parse too (Go emits RFC3339Nano).
        #expect(runs[1].startedAt != nil)
    }

    @Test("null body decodes to nil (operator's empty active set)")
    func nullBodyDecodesToNilOptional() throws {
        let runs: [MillsPipelineRun]? = try makeClient().decodeRaw(Data("null".utf8), statusCode: 200)
        #expect(runs == nil)
    }

    @Test("bare KPI object decodes")
    func bareKPIObjectDecodes() throws {
        let json = """
        {"ID":1,"WindowSeconds":86400,"Metrics":{"pipeline_merged_runs":6,"auto_merge_rate":0.86,"queue_depth":1}}
        """
        let snap: MillsKPISnapshot = try makeClient().decodeRaw(Data(json.utf8), statusCode: 200)
        #expect(snap.metrics["pipeline_merged_runs"] == 6)
        #expect(snap.systemSummary.mergedRuns == 6)
    }

    @Test("404 maps to notFound (no snapshot yet)")
    func notFoundMapping() throws {
        do {
            let _: MillsKPISnapshot = try makeClient().decodeRaw(Data("not found".utf8), statusCode: 404)
            Issue.record("expected notFound")
        } catch let LoomAPIError.apiError(code, _, _) {
            #expect(code == .notFound)
        }
    }

    @Test("503 not_configured envelope maps to notConfigured")
    func notConfiguredMapping() throws {
        let body = """
        {"ok":false,"error":{"code":"not_configured","message":"loom-mills operator not configured"},"meta":{}}
        """
        do {
            let _: MillsKPISnapshot = try makeClient().decodeRaw(Data(body.utf8), statusCode: 503)
            Issue.record("expected notConfigured")
        } catch let LoomAPIError.apiError(code, _, _) {
            #expect(code == .notConfigured)
        }
    }

    // The two tests below capture the REAL bytes the HUD mills proxy emits —
    // not the hand-crafted enveloped body above, which the proxy never
    // actually produces. The proxy passes operator/HUD errors through as
    // BARE (non-enveloped) bodies, so they arrive with no `code` and map to
    // `.upstreamError` via the HTTP-status fallback. This is the regression
    // for "mills is broken in the mobile app": the operator-unset / operator-
    // down paths surfaced as a hard "Couldn't reach Mills" error card.

    @Test("bare 503 operator-not-configured body maps to upstreamError")
    func bareNotConfiguredMapsToUpstream() throws {
        // Exact bytes from internal/hud/domain/mills/mills.go handleProxyGet
        // WriteError → `{"error":"loom-mills operator not configured"}`.
        let body = #"{"error":"loom-mills operator not configured"}"#
        do {
            let _: MillsKPISnapshot = try makeClient().decodeRaw(Data(body.utf8), statusCode: 503)
            Issue.record("expected upstreamError")
        } catch let LoomAPIError.apiError(code, _, _) {
            #expect(code == .upstreamError)
        }
    }

    @Test("bare 502 operator-unreachable body maps to upstreamError")
    func bareUnreachableMapsToUpstream() throws {
        // Exact shape from proxy.go errorHandler → plain-text http.Error at 502.
        let body = "loom-mills operator unreachable: dial tcp 10.0.0.1:8090: connect: connection refused\n"
        do {
            let _: [MillsPipelineRun]? = try makeClient().decodeRaw(Data(body.utf8), statusCode: 502)
            Issue.record("expected upstreamError")
        } catch let LoomAPIError.apiError(code, _, _) {
            #expect(code == .upstreamError)
        }
    }
}

/// MillsAPI-level behavior over the protocol: benign empties resolve to an
/// empty list (calm UI), genuine errors propagate (error UI).
@Suite("MillsAPI resilience")
struct MillsAPIResilienceTests {
    @Test("pipelineRuns swallows notConfigured to an empty list")
    func swallowsNotConfigured() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/mills/pipeline/runs"] = .apiError(code: .notConfigured, message: "unset", requestId: "")
        let api = MillsAPI(client: mock)
        let runs = try await api.pipelineRuns()
        #expect(runs.isEmpty)
    }

    @Test("pipelineRuns propagates a genuine transport error")
    func propagatesRealError() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/mills/pipeline/runs"] = .networkError(underlying: "offline")
        let api = MillsAPI(client: mock)
        await #expect(throws: LoomAPIError.self) {
            _ = try await api.pipelineRuns()
        }
    }

    // Regression: operator unset (bare 503) / operator down (bare 502) arrive
    // as `.upstreamError` (see MillsRawDecodeTests). Both endpoints must
    // degrade to the calm empty state, NOT re-throw — otherwise the Mills
    // screen shows "Couldn't reach Mills" every time the operator restarts.

    @Test("pipelineRuns swallows upstreamError (operator down/unset) to an empty list")
    func swallowsUpstreamError() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/mills/pipeline/runs"] =
            .apiError(code: .upstreamError, message: "Upstream service error (HTTP 502)", requestId: "")
        let api = MillsAPI(client: mock)
        let runs = try await api.pipelineRuns()
        #expect(runs.isEmpty)
    }

    @Test("latestKPI swallows upstreamError (operator down/unset) to nil")
    func kpiSwallowsUpstreamError() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/mills/kpis"] =
            .apiError(code: .upstreamError, message: "Upstream service error (HTTP 503)", requestId: "")
        let api = MillsAPI(client: mock)
        let snap = try await api.latestKPI(window: "1d")
        #expect(snap == nil)
    }
}
