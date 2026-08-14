import Foundation
import Testing
@testable import LoomCompanionKit

/// Coverage for the deck's "Needs you" inbox: aggregation across the three
/// pending-decision surfaces, independent degradation, and each decision's
/// route + list bookkeeping.
@Suite("OperatorInboxViewModel")
struct OperatorInboxViewModelTests {

    // MARK: - Fixtures

    private func workflowsResponse(_ statuses: [String: String]) throws -> MobileWorkflowsResponse {
        let workflows = statuses
            .sorted { $0.key < $1.key }
            .map { id, status in
                """
                {"id":"\(id)","name":"wf \(id)","status":"\(status)","progress":0.5,"started_at":"2026-07-28T09:00:00Z"}
                """
            }
            .joined(separator: ",")
        let json = """
        {"workflows":[\(workflows)],"pending_approvals":1,"deprecated_pending_approvals":0,"deprecated":false}
        """
        return try JSONDecoder().decode(MobileWorkflowsResponse.self, from: Data(json.utf8))
    }

    private func workflowDetail(id: String, stepStatus: String) throws -> MobileWorkflowDetailResponse {
        let json = """
        {
          "workflow": {
            "id": "\(id)", "name": "wf \(id)", "status": "waiting_approval",
            "progress": 0.5, "started_at": "2026-07-28T09:00:00Z",
            "steps": [
              {"id": "step-1", "name": "plan", "status": "completed"},
              {"id": "step-2", "name": "apply", "status": "\(stepStatus)"}
            ]
          },
          "events": []
        }
        """
        return try JSONDecoder().decode(MobileWorkflowDetailResponse.self, from: Data(json.utf8))
    }

    private func handoffsResponse(_ statuses: [String: String]) throws -> MobileHandoffsResponse {
        let handoffs = statuses
            .sorted { $0.key < $1.key }
            .map { id, status in
                """
                {"id":"\(id)","from_agent":"claude-code","to_agent":"codex","target_agent_id":"codex","status":"\(status)","summary":"take this","context":"","created_at":"2026-07-28T09:00:00Z"}
                """
            }
            .joined(separator: ",")
        let json = "{\"handoffs\":[\(handoffs)],\"total\":\(statuses.count)}"
        return try JSONDecoder().decode(MobileHandoffsResponse.self, from: Data(json.utf8))
    }

    private func loadedVM(_ mock: MockAPIClient) async -> OperatorInboxViewModel {
        let vm = OperatorInboxViewModel(apiClient: mock)
        await vm.load()
        return vm
    }

    // MARK: - Aggregation

    @Test("Load keeps only pending decisions from each surface")
    func loadFiltersToPending() async throws {
        let mock = MockAPIClient()
        mock.workflowsResponse = try workflowsResponse([
            "wf-a": "waiting_approval", "wf-b": "running", "wf-c": "completed",
        ])
        mock.handoffsResponse = try handoffsResponse([
            "HO-1": "pending", "HO-2": "accepted",
        ])
        mock.autofixProposalsResponse = AutofixProposalsResponse(
            proposals: [AutofixProposal(id: "AF-1", description: "restart the pod")])

        let vm = await loadedVM(mock)

        #expect(vm.approvals.map(\.id) == ["wf-a"])
        #expect(vm.handoffs.map(\.id) == ["HO-1"])
        #expect(vm.proposals.map(\.id) == ["AF-1"])
        #expect(vm.totalCount == 3)
        #expect(vm.loaded)
        #expect(vm.loadError == nil)
    }

    @Test("One failed surface degrades alone — the others still load")
    func partialDegradation() async throws {
        let mock = MockAPIClient()
        mock.workflowsResponse = try workflowsResponse(["wf-a": "waiting_approval"])
        mock.endpointFailures["/api/mobile/v1/handoffs"] =
            .apiError(code: .upstreamError, message: "down", requestId: "r")
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [])

        let vm = await loadedVM(mock)

        #expect(vm.approvals.map(\.id) == ["wf-a"])
        #expect(vm.handoffs.isEmpty)
        #expect(vm.loadError == nil)
    }

    @Test("A total miss across every surface reports one load error")
    func totalMiss() async {
        let mock = MockAPIClient()
        mock.shouldFail = true

        let vm = await loadedVM(mock)

        #expect(vm.loaded)
        #expect(vm.loadError != nil)
        #expect(vm.totalCount == 0)
    }

    // MARK: - Workflow decisions

    @Test("Approve resolves the waiting step from the detail, then approves it")
    func approveWorkflow() async throws {
        let mock = MockAPIClient()
        mock.workflowsResponse = try workflowsResponse(["wf-a": "waiting_approval"])
        mock.workflowDetailResponse = try workflowDetail(id: "wf-a", stepStatus: "waiting_approval")
        mock.workflowMutationResponse = MobileWorkflowMutationResponse(
            workflowId: "wf-a", stepId: "step-2", action: "approved")
        let vm = await loadedVM(mock)

        await vm.approveWorkflow(vm.approvals[0])

        #expect(mock.lastWorkflowDecision?.id == "wf-a")
        #expect(mock.lastWorkflowDecision?.stepId == "step-2")
        #expect(mock.lastWorkflowDecision?.verb == "approved")
        #expect(vm.approvals.isEmpty)
        #expect(vm.actionMessage != nil)
        #expect(vm.actionError == nil)
    }

    @Test("Approve with no waiting step fails cleanly and keeps the row")
    func approveWithoutWaitingStep() async throws {
        let mock = MockAPIClient()
        mock.workflowsResponse = try workflowsResponse(["wf-a": "waiting_approval"])
        // Detail says the step already ran — the workflow moved on.
        mock.workflowDetailResponse = try workflowDetail(id: "wf-a", stepStatus: "completed")
        let vm = await loadedVM(mock)

        await vm.approveWorkflow(vm.approvals[0])

        #expect(mock.lastWorkflowDecision == nil)
        #expect(vm.approvals.count == 1)
        #expect(vm.actionError != nil)
    }

    @Test("Reject sends the reject route for the waiting step")
    func rejectWorkflow() async throws {
        let mock = MockAPIClient()
        mock.workflowsResponse = try workflowsResponse(["wf-a": "waiting_approval"])
        mock.workflowDetailResponse = try workflowDetail(id: "wf-a", stepStatus: "waiting_approval")
        mock.workflowMutationResponse = MobileWorkflowMutationResponse(
            workflowId: "wf-a", stepId: "step-2", action: "rejected")
        let vm = await loadedVM(mock)

        await vm.rejectWorkflow(vm.approvals[0], reason: "not yet")

        #expect(mock.lastWorkflowDecision?.verb == "rejected")
        #expect(vm.approvals.isEmpty)
    }

    // MARK: - Handoff decisions

    @Test("Accept resolves the target agent and removes the handoff")
    func acceptHandoff() async throws {
        let mock = MockAPIClient()
        mock.handoffsResponse = try handoffsResponse(["HO-1": "pending"])
        mock.handoffActionResponse = MobileHandoffActionResponse(
            status: "accepted", handoffId: "HO-1")
        let vm = await loadedVM(mock)

        await vm.acceptHandoff(vm.handoffs[0])

        #expect(mock.lastHandoffAccept?.id == "HO-1")
        #expect(mock.lastHandoffAccept?.targetAgentId == "codex")
        #expect(vm.handoffs.isEmpty)
        #expect(vm.actionMessage != nil)
    }

    @Test("Reject forwards the reason and removes the handoff")
    func rejectHandoff() async throws {
        let mock = MockAPIClient()
        mock.handoffsResponse = try handoffsResponse(["HO-1": "pending"])
        mock.handoffActionResponse = MobileHandoffActionResponse(
            status: "rejected", handoffId: "HO-1")
        let vm = await loadedVM(mock)

        await vm.rejectHandoff(vm.handoffs[0], reason: "wrong agent")

        #expect(mock.lastHandoffReject?.id == "HO-1")
        #expect(mock.lastHandoffReject?.reason == "wrong agent")
        #expect(vm.handoffs.isEmpty)
    }

    // MARK: - Auto-fix decisions

    @Test("Approve executes the proposal and reports the execution")
    func approveProposal() async {
        let mock = MockAPIClient()
        mock.autofixProposalsResponse = AutofixProposalsResponse(
            proposals: [AutofixProposal(id: "AF-1")])
        mock.autofixApproveResponse = AutofixApproveResponse(
            execution: AutofixExecution(id: "EX-1", proposalId: "AF-1", status: "running"))
        let vm = await loadedVM(mock)

        await vm.approveProposal(vm.proposals[0])

        #expect(mock.lastAutofixDecision?.id == "AF-1")
        #expect(mock.lastAutofixDecision?.verb == "approved")
        #expect(vm.proposals.isEmpty)
        #expect(vm.actionMessage?.contains("EX-1") == true)
    }

    @Test("Approve without the admin token maps 401 to actionable copy")
    func approveProposalUnauthorized() async {
        let mock = MockAPIClient()
        mock.autofixProposalsResponse = AutofixProposalsResponse(
            proposals: [AutofixProposal(id: "AF-1")])
        mock.endpointFailures["/api/mobile/v1/autofix/proposals/AF-1/approve"] =
            .apiError(code: .unauthorized, message: "admin token required", requestId: "r")
        let vm = await loadedVM(mock)

        await vm.approveProposal(vm.proposals[0])

        #expect(vm.proposals.count == 1)
        #expect(vm.actionError?.contains("admin token") == true)
    }

    @Test("Reject records the decision and removes the proposal")
    func rejectProposal() async {
        let mock = MockAPIClient()
        mock.autofixProposalsResponse = AutofixProposalsResponse(
            proposals: [AutofixProposal(id: "AF-1")])
        mock.autofixRejectResponse = AutofixRejectResponse(rejected: true, proposalId: "AF-1")
        let vm = await loadedVM(mock)

        await vm.rejectProposal(vm.proposals[0])

        #expect(mock.lastAutofixDecision?.verb == "rejected")
        #expect(vm.proposals.isEmpty)
    }
}
