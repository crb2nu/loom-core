import Foundation

/// Aggregates everything that is *waiting on the operator* into one deck
/// section: workflow steps pending approval, agent-to-agent handoffs, and
/// auto-fix proposals. The point of the Operator deck is "one place to view
/// and control the fleet" — this is the control half.
///
/// Each surface loads independently and degrades independently: a HUD whose
/// workflow store is down still shows handoffs, and vice versa. Only a total
/// miss across every read surfaces `loadError`.
@Observable
public final class OperatorInboxViewModel {
    /// Workflows currently blocked on a human approval.
    public private(set) var approvals: [MobileWorkflow] = []
    /// Handoffs still pending a decision (accepted/rejected rows are the Work
    /// tab's history problem, not the deck's).
    public private(set) var handoffs: [MobileHandoff] = []
    /// Auto-fix proposals awaiting approve/reject.
    public private(set) var proposals: [AutofixProposal] = []

    public private(set) var loaded = false
    public private(set) var loadError: String?
    /// IDs with an in-flight decision, so a row can disable its own buttons
    /// without freezing its neighbours.
    public private(set) var busyIDs: Set<String> = []
    public private(set) var actionMessage: String?
    public private(set) var actionError: String?

    @ObservationIgnored
    private let apiClient: any LoomAPIClientProtocol

    public init(apiClient: any LoomAPIClientProtocol) {
        self.apiClient = apiClient
    }

    /// Items across all three surfaces.
    public var totalCount: Int { approvals.count + handoffs.count + proposals.count }

    // MARK: - Load

    public func load() async {
        var anySucceeded = false

        // Workflows are fetched unfiltered and reduced client-side: the list
        // route's `pending_approvals` count is computed over the whole store,
        // so a server-side status filter buys nothing and risks drift.
        if let response: MobileWorkflowsResponse = try? await apiClient.request(
            .workflows(limit: 50)) {
            approvals = response.workflows.filter { $0.status == .waitingApproval }
            anySucceeded = true
        }
        if let response: MobileHandoffsResponse = try? await apiClient.request(
            .handoffs(limit: 50)) {
            handoffs = response.handoffs.filter { $0.status == "pending" }
            anySucceeded = true
        }
        // Bare JSON — the alerting domain writes with the HUD's plain
        // writeJSON, not the mobile envelope. See ServerAlert.swift.
        if let response: AutofixProposalsResponse = try? await apiClient.requestRaw(
            .autofixProposals) {
            proposals = response.proposals
            anySucceeded = true
        }

        loaded = true
        loadError = anySucceeded ? nil : "Couldn't read the operator inbox."
    }

    // MARK: - Workflow decisions

    /// Approve the workflow's pending step. The list route doesn't carry
    /// steps, so resolve the waiting step from the detail first — same
    /// two-call shape as the `loom://workflow?approve` deep link.
    public func approveWorkflow(_ workflow: MobileWorkflow) async {
        await decideWorkflow(workflow, verb: "approved") { stepId in
            .workflowApprove(id: workflow.id, stepId: stepId)
        }
    }

    public func rejectWorkflow(_ workflow: MobileWorkflow, reason: String? = nil) async {
        await decideWorkflow(workflow, verb: "rejected") { stepId in
            .workflowReject(id: workflow.id, stepId: stepId, reason: reason)
        }
    }

    private func decideWorkflow(
        _ workflow: MobileWorkflow,
        verb: String,
        endpoint: (String) -> Endpoint
    ) async {
        await performDecision(id: workflow.id) {
            let detail: MobileWorkflowDetailResponse = try await self.apiClient.request(
                .workflowDetail(id: workflow.id))
            guard let step = detail.workflow.steps?.first(where: { $0.status == .waitingApproval }) else {
                throw LoomAPIError.apiError(
                    code: .notFound,
                    message: "No step is waiting for approval — the workflow may have moved on.",
                    requestId: "")
            }
            let _: MobileWorkflowMutationResponse = try await self.apiClient.request(endpoint(step.id))
            self.approvals.removeAll { $0.id == workflow.id }
            return "Workflow \(workflow.name ?? workflow.id) \(verb)."
        }
    }

    // MARK: - Handoff decisions

    /// Accept a pending handoff. The HUD resolves the destination session
    /// from the handoff's target agent when no explicit session is supplied —
    /// same contract as the Work tab's inbox.
    public func acceptHandoff(_ handoff: MobileHandoff, importEntries: Bool = true) async {
        let target = handoff.targetAgentId.isEmpty ? handoff.toAgent : handoff.targetAgentId
        await performDecision(id: handoff.id) {
            let response: MobileHandoffActionResponse = try await self.apiClient.request(
                .handoffAccept(id: handoff.id, targetAgentId: target, importEntries: importEntries))
            self.handoffs.removeAll { $0.id == handoff.id }
            return "Handoff \(response.handoffId) accepted."
        }
    }

    public func rejectHandoff(_ handoff: MobileHandoff, reason: String? = nil) async {
        await performDecision(id: handoff.id) {
            let response: MobileHandoffActionResponse = try await self.apiClient.request(
                .handoffReject(id: handoff.id, reason: reason))
            self.handoffs.removeAll { $0.id == handoff.id }
            return "Handoff \(response.handoffId) rejected."
        }
    }

    // MARK: - Auto-fix decisions

    /// Approve executes the proposal immediately server-side. Admin-gated:
    /// this is the one mobile route that needs the HUD admin token, so a
    /// pairing-only device gets a 401 — folded into actionable copy below.
    public func approveProposal(_ proposal: AutofixProposal) async {
        await performDecision(id: proposal.id) {
            let response: AutofixApproveResponse = try await self.apiClient.requestRaw(
                .autofixApprove(id: proposal.id))
            self.proposals.removeAll { $0.id == proposal.id }
            return "Auto-fix approved — execution \(response.execution.id) \(response.execution.status)."
        }
    }

    public func rejectProposal(_ proposal: AutofixProposal) async {
        await performDecision(id: proposal.id) {
            let _: AutofixRejectResponse = try await self.apiClient.requestRaw(
                .autofixReject(id: proposal.id))
            self.proposals.removeAll { $0.id == proposal.id }
            return "Auto-fix proposal rejected."
        }
    }

    // MARK: - Shared decision wrapper

    private func performDecision(id: String, _ mutation: () async throws -> String) async {
        guard !busyIDs.contains(id) else { return }
        busyIDs.insert(id)
        actionError = nil
        actionMessage = nil
        defer { busyIDs.remove(id) }
        do {
            actionMessage = try await mutation()
        } catch let error as LoomAPIError {
            if case let .apiError(code, _, _) = error, code == .unauthorized {
                // The admin-gated approve path without a stored admin token.
                actionError = "The HUD refused this action — add your admin token in Settings → Connection."
            } else {
                actionError = error.description
            }
        } catch {
            actionError = error.localizedDescription
        }
    }
}
