import Foundation
import Testing
@testable import LoomCompanionKit

/// Coverage for the handoff-inbox wiring: the two mutation endpoints and the
/// OpsViewModel actions that drive the inbox sheet. Before this slice the
/// inbox rendered but had no accept/reject path at all — the backend routes
/// (`POST /api/mobile/v1/handoffs/{id}/accept|reject`) had zero callers.
@Suite("Handoff inbox")
struct HandoffInboxTests {

    private func handoff(
        id: String = "HO-1",
        status: String = "pending",
        targetAgentId: String = "codex"
    ) throws -> MobileHandoff {
        let json = """
        {
          "id": "\(id)",
          "from_agent": "claude-code",
          "to_agent": "codex",
          "target_agent_id": "\(targetAgentId)",
          "status": "\(status)",
          "summary": "pick up the mills slice",
          "context": "",
          "created_at": "2026-07-25T10:00:00Z"
        }
        """
        return try JSONDecoder().decode(MobileHandoff.self, from: Data(json.utf8))
    }

    // MARK: - Endpoint contract

    @Test("Handoff mutation endpoints use the documented paths and method")
    func endpointPaths() throws {
        let accept = Endpoint.handoffAccept(id: "HO-1", targetAgentId: "codex")
        let reject = Endpoint.handoffReject(id: "HO-1", reason: "not mine")
        #expect(accept.path == "/api/mobile/v1/handoffs/HO-1/accept")
        #expect(reject.path == "/api/mobile/v1/handoffs/HO-1/reject")
        #expect(accept.method == "POST")
        #expect(reject.method == "POST")
        #expect(accept.isMutation)
        #expect(reject.isMutation)
        // Neither is behind the HUD admin token — they are mobile-scope routes.
        #expect(accept.requiresAdminToken == false)
        #expect(reject.requiresAdminToken == false)
    }

    @Test("Accept body carries target_agent_id so the HUD resolves the session")
    func acceptBody() throws {
        let request = try Endpoint
            .handoffAccept(id: "HO-1", targetAgentId: "codex", importEntries: true)
            .urlRequest(baseURL: URL(string: "http://localhost:3333")!)
        let body = try #require(request.httpBody)
        let json = try #require(try JSONSerialization.jsonObject(with: body) as? [String: Any])
        #expect(json["target_agent_id"] as? String == "codex")
        #expect(json["import_entries"] as? Bool == true)
        // No session_id when the caller only knows the agent.
        #expect(json["session_id"] == nil)
    }

    @Test("Reject omits the reason key entirely when blank")
    func rejectBodyOmitsBlankReason() throws {
        let blank = try Endpoint.handoffReject(id: "HO-1", reason: "   ")
            .urlRequest(baseURL: URL(string: "http://localhost:3333")!)
        let blankBody = try #require(blank.httpBody)
        let blankJSON = try #require(
            try JSONSerialization.jsonObject(with: blankBody) as? [String: Any])
        #expect(blankJSON["reason"] == nil)

        let withReason = try Endpoint.handoffReject(id: "HO-1", reason: " busy ")
            .urlRequest(baseURL: URL(string: "http://localhost:3333")!)
        let reasonBody = try #require(withReason.httpBody)
        let reasonJSON = try #require(
            try JSONSerialization.jsonObject(with: reasonBody) as? [String: Any])
        #expect(reasonJSON["reason"] as? String == "busy")
    }

    // MARK: - ViewModel

    @Test("loadHandoffs populates the inbox and marks it loaded")
    @MainActor
    func loadsHandoffs() async throws {
        let mock = MockAPIClient()
        mock.handoffsResponse = MobileHandoffsResponse(
            handoffs: [try handoff()], total: 1)
        let vm = OpsViewModel(apiClient: mock)

        await vm.loadHandoffs()

        #expect(vm.handoffs.count == 1)
        #expect(vm.handoffsLoaded)
        #expect(vm.handoffsError == nil)
    }

    @Test("loadHandoffs surfaces the API error instead of an empty inbox")
    @MainActor
    func loadFailureSurfacesError() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/mobile/v1/handoffs"] =
            .apiError(code: .forbidden, message: "denied", requestId: "r1")
        let vm = OpsViewModel(apiClient: mock)

        await vm.loadHandoffs()

        #expect(vm.handoffs.isEmpty)
        #expect(vm.handoffsLoaded)
        #expect(vm.handoffsError != nil)
    }

    @Test("acceptHandoff posts the target agent and refreshes the inbox")
    @MainActor
    func acceptsHandoff() async throws {
        let mock = MockAPIClient()
        mock.handoffsResponse = MobileHandoffsResponse(handoffs: [], total: 0)
        mock.handoffActionResponse = MobileHandoffActionResponse(
            status: "accepted", handoffId: "HO-1", sessionId: "sess-9")
        let vm = OpsViewModel(apiClient: mock)

        await vm.acceptHandoff(try handoff())

        #expect(mock.lastHandoffAccept?.id == "HO-1")
        #expect(mock.lastHandoffAccept?.targetAgentId == "codex")
        #expect(vm.handoffActionError == nil)
        #expect(vm.handoffActionMessage?.contains("accepted") == true)
        // The list is re-read so a resolved handoff leaves the inbox.
        #expect(mock.requestCount(for: .handoffs()) == 1)
        #expect(vm.mutatingHandoffID == nil)
    }

    @Test("acceptHandoff falls back to to_agent when target_agent_id is blank")
    @MainActor
    func acceptFallsBackToTargetAgent() async throws {
        let mock = MockAPIClient()
        mock.handoffsResponse = MobileHandoffsResponse(handoffs: [], total: 0)
        mock.handoffActionResponse = MobileHandoffActionResponse(
            status: "accepted", handoffId: "HO-2")
        let vm = OpsViewModel(apiClient: mock)

        await vm.acceptHandoff(try handoff(id: "HO-2", targetAgentId: ""))

        #expect(mock.lastHandoffAccept?.targetAgentId == "codex")
    }

    @Test("rejectHandoff forwards the operator reason and reports failures")
    @MainActor
    func rejectsHandoff() async throws {
        let mock = MockAPIClient()
        mock.handoffsResponse = MobileHandoffsResponse(handoffs: [], total: 0)
        mock.handoffActionResponse = MobileHandoffActionResponse(
            status: "rejected", handoffId: "HO-1")
        let vm = OpsViewModel(apiClient: mock)

        await vm.rejectHandoff(try handoff(), reason: "wrong agent")
        #expect(mock.lastHandoffReject?.reason == "wrong agent")
        #expect(vm.handoffActionMessage?.contains("rejected") == true)

        mock.endpointFailures["/api/mobile/v1/handoffs/HO-1/reject"] =
            .apiError(code: .upstreamError, message: "bridge down", requestId: "r2")
        await vm.rejectHandoff(try handoff(), reason: nil)
        #expect(vm.handoffActionError?.contains("bridge down") == true)
        #expect(vm.mutatingHandoffID == nil)
    }
}

/// The workflow approve/reject wiring: OpsWorkflowDetailView's new buttons go
/// through these, and the response is a mutation acknowledgement — decoding it
/// as `MobileWorkflowDetail` (the previous deep-link behaviour) always failed.
@Suite("Workflow decisions")
struct WorkflowDecisionTests {

    @Test("Mutation response decodes the acknowledgement shape the HUD returns")
    func decodesMutationAck() throws {
        let json = """
        {"workflow_id":"wf-1","step_id":"step-2","action":"approved","deprecated":true,
         "deprecation_message":"legacy"}
        """
        let ack = try JSONDecoder().decode(
            MobileWorkflowMutationResponse.self, from: Data(json.utf8))
        #expect(ack.workflowId == "wf-1")
        #expect(ack.stepId == "step-2")
        #expect(ack.action == "approved")
        #expect(ack.deprecated)
        #expect(ack.deprecationMessage == "legacy")
    }

    @Test("approveWorkflow and rejectWorkflow hit the mobile routes")
    @MainActor
    func approveAndReject() async throws {
        let mock = MockAPIClient()
        mock.workflowMutationResponse = MobileWorkflowMutationResponse(
            workflowId: "wf-1", stepId: "step-2", action: "approved")
        let vm = OpsViewModel(apiClient: mock)

        let approved = try await vm.approveWorkflow(id: "wf-1", stepId: "step-2")
        #expect(approved.action == "approved")

        mock.workflowMutationResponse = MobileWorkflowMutationResponse(
            workflowId: "wf-1", stepId: "step-2", action: "rejected")
        let rejected = try await vm.rejectWorkflow(id: "wf-1", stepId: "step-2", reason: "no")
        #expect(rejected.action == "rejected")
    }
}
