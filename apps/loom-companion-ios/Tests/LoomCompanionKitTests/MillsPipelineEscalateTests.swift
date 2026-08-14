import Foundation
import Testing
@testable import LoomCompanionKit

/// Pipeline escalate — the widget's per-pipeline oversight action. Covers the
/// endpoint wire shape, the control-API behavior, and the deep-link route the
/// widget button opens.
@Suite("MillsPipelineEscalate")
struct MillsPipelineEscalateTests {

    // MARK: - Endpoint wiring

    @Test("escalate endpoint: POST, correct path, mutation flag")
    func endpointShape() {
        let ep = Endpoint.millsPipelineEscalate(id: "PIPE-7f3a")
        #expect(ep.method == "POST")
        #expect(ep.path == "/api/mills/pipeline/runs/PIPE-7f3a/escalate")
        #expect(ep.isMutation)
    }

    @Test("escalate body defaults an empty reason to a source-tagged string")
    func escalateBodyDefaultReason() throws {
        let req = try Endpoint.millsPipelineEscalate(id: "P", reason: nil)
            .urlRequest(baseURL: URL(string: "https://hud.local")!)
        let body = try JSONSerialization.jsonObject(with: req.httpBody ?? Data()) as? [String: Any]
        #expect((body?["reason"] as? String)?.contains("iOS") == true)
    }

    @Test("escalate body passes an explicit reason through")
    func escalateBodyExplicitReason() throws {
        let req = try Endpoint.millsPipelineEscalate(id: "P", reason: "runaway cost")
            .urlRequest(baseURL: URL(string: "https://hud.local")!)
        let body = try JSONSerialization.jsonObject(with: req.httpBody ?? Data()) as? [String: Any]
        #expect(body?["reason"] as? String == "runaway cost")
    }

    // MARK: - Control API

    @Test("escalatePipeline decodes the operator ack and threads id + reason")
    func escalateSucceeds() async throws {
        let mock = MockAPIClient()
        mock.millsPipelineEscalateResponse = MillsPipelineEscalateAck(
            runID: "PIPE-7f3a", backlogID: "BACK-A", state: "escalated", reason: "manual"
        )
        let api = MillsControlAPI(client: mock)
        let ack = try await api.escalatePipeline(id: "PIPE-7f3a", reason: nil)
        #expect(ack.runID == "PIPE-7f3a")
        #expect(ack.state == "escalated")
        #expect(mock.lastEscalate?.id == "PIPE-7f3a")
    }

    @Test("escalatePipeline surfaces a 401 (does not swallow)")
    func escalateSurfacesAuthFailure() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/mills/pipeline/runs/P/escalate"] =
            .apiError(code: .unauthorized, message: "invalid admin token", requestId: "")
        let api = MillsControlAPI(client: mock)
        await #expect(throws: LoomAPIError.self) {
            _ = try await api.escalatePipeline(id: "P", reason: nil)
        }
    }

    @Test("escalatePipeline surfaces a 404 for a missing run")
    func escalateSurfacesNotFound() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/mills/pipeline/runs/gone/escalate"] =
            .apiError(code: .notFound, message: "pipeline run not found", requestId: "")
        let api = MillsControlAPI(client: mock)
        await #expect(throws: LoomAPIError.self) {
            _ = try await api.escalatePipeline(id: "gone", reason: nil)
        }
    }

    @Test("escalate ack decodes the operator's JSON shape")
    func escalateAckDecodes() throws {
        let json = #"{"run_id":"PIPE-1","backlog_id":"B","state":"escalated","reason":"manual escalation"}"#
            .data(using: .utf8)!
        let ack = try JSONDecoder().decode(MillsPipelineEscalateAck.self, from: json)
        #expect(ack.runID == "PIPE-1")
        #expect(ack.backlogID == "B")
        #expect(ack.state == "escalated")
    }

    // MARK: - Deep link (widget button → app)

    @Test("loom://pipeline/{id}/escalate round-trips")
    func escalateDeepLinkRoundTrips() {
        let link = DeepLink.from(URL(string: "loom://pipeline/PIPE-7f3a/escalate")!)
        #expect(link == .pipelineEscalate(id: "PIPE-7f3a"))
        #expect(DeepLink.pipelineEscalate(id: "PIPE-7f3a").urlString == "loom://pipeline/PIPE-7f3a/escalate")
        #expect(DeepLink.pipelineEscalate(id: "PIPE-7f3a").destinationGroup == .mills)
    }

    @Test("an unknown pipeline action does not parse (no silent mis-route)")
    func unknownPipelineActionIsNil() {
        #expect(DeepLink.from(URL(string: "loom://pipeline/PIPE-1/pause")!) == nil)
        #expect(DeepLink.from(URL(string: "loom://pipeline/PIPE-1")!) == nil)
    }

    @Test("escalate deep link escapes ids with URL-unsafe characters")
    func escalateDeepLinkEscapes() {
        // A run id is normally token-safe, but the builder must escape defensively.
        let link = DeepLink.pipelineEscalate(id: "PIPE 7f3a")
        let url = link.url
        #expect(url != nil)
        // Round-trips back to the same case after percent-decoding.
        #expect(DeepLink.from(url!) == .pipelineEscalate(id: "PIPE 7f3a"))
    }
}
