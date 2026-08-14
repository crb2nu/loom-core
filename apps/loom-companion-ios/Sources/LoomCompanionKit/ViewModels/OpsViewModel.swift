import Foundation

/// ViewModel powering the read-only mobile parity "Ops" tab.
@MainActor
@Observable
public final class OpsViewModel {
    public var error: LoomAPIError?
    public var warningMessage: String?
    public var mutationStatusMessage: String?
    public var mutationErrorMessage: String?
    public var isMutatingSession = false

    public var tasks: [MobileTask] = []
    public var taskCounts = MobileTaskCounts(pending: 0, inProgress: 0, blocked: 0, completed: 0)

    public var workflows: [MobileWorkflow] = []
    public var pendingApprovals = 0
    public var workflowsDeprecated = false
    public var workflowsDeprecationMessage: String?

    // MARK: - Handoff inbox
    //
    // Loaded on demand when the inbox is presented (deep link
    // `loom://handoff`, a Dashboard attention lane, or the Work tab's inbox
    // button) rather than as part of the Work section, so the extra request
    // only fires when the operator actually looks at it.

    public var handoffs: [MobileHandoff] = []
    /// True once a load attempt has completed (success or failure), so the
    /// view can distinguish "still loading" from "genuinely empty".
    public var handoffsLoaded = false
    public var isLoadingHandoffs = false
    public var handoffsError: LoomAPIError?
    /// Handoff id currently being accepted/rejected — drives per-row busy UI.
    public var mutatingHandoffID: String?
    public var handoffActionMessage: String?
    public var handoffActionError: String?

    public var presenceAgents: [MobilePresenceAgent] = []
    public var presenceClaims: [MobileFileClaim] = []
    public var presenceWorktrees: [MobileWorktree] = []
    public var presenceSummary = MobilePresenceSummary(activeAgents: 0, idleAgents: 0, offlineAgents: 0, totalAgents: 0, claimCount: 0, worktreeCount: 0)

    public var memoryStats: MobileMemoryStats?
    public var memoryItems: [MobileMemoryItem] = []
    public var memoryTier: MobileMemoryTier = .working

    public var streamEntries: [MobileStreamEntry] = []

    public var topology: MobileTopologyResponse?
    public var graphStats: MobileGraphStats?
    public var graphEntities: [MobileGraphEntity] = []
    public var graphPath: MobileGraphPath?

    public var reasoningChains: [MobileReasoningChain] = []
    public var controlPlane: MobileControlPlaneResponse?

    public var pipelines: [MobilePipeline] = []
    public var recentPipelines: [MobilePipeline] = []
    public var pipelineSummary: MobilePipelineSummary?
    public var pipelinesAvailable = false

    public var sandboxSummary: MobileSandboxSummary?
    public var isMutatingSandbox = false
    public var sandboxMutationMessage: String?
    public var sandboxMutationError: String?

    /// Spawn config (projects + agent types) cached so the Work session-start
    /// form can offer a project picker instead of a free-text namespace field.
    /// Loaded lazily; falls back to a nil picker + raw text entry.
    public var spawnConfig: SpawnConfig?

    /// Sections currently awaiting one or more API responses. Marking a section
    /// before its first suspension makes lazy loads single-flight.
    public private(set) var loadingSections: Set<OpsSection> = []

    /// Sections that completed at least one load attempt. This is observed by
    /// the view so initial, refreshing, and settled states stay distinct.
    public private(set) var loadedSections: Set<OpsSection> = []

    public private(set) var workLastUpdatedAt: Date?

    public var isLoading: Bool { !loadingSections.isEmpty }

    @ObservationIgnored
    public let apiClient: any LoomAPIClientProtocol

    @ObservationIgnored
    private var sseRegistrationId: UUID?

    @ObservationIgnored
    private var sectionLoadWaiters: [OpsSection: [CheckedContinuation<Void, Never>]] = [:]

    /// SSE event types that trigger a presence/agents refresh.
    private static let refreshEventTypes: Set<String> = [
        "hud.fleet",
        "agent.heartbeat",
        "agent.session.start",
        "agent.session.end",
        "agent.session.reaped",
        "agent.spawn.building",
        "agent.spawn.running",
        "agent.spawn.completed",
        "agent.spawn.failed",
        "agent.spawn.stopped",
        "hud.pipeline",
    ]

    public init(apiClient: any LoomAPIClientProtocol) {
        self.apiClient = apiClient
    }

    /// Start listening to SSE events via the broadcaster for real-time agent updates.
    @MainActor
    public func startListening(broadcaster: SSEEventBroadcaster) {
        guard sseRegistrationId == nil else { return }
        sseRegistrationId = broadcaster.register { [weak self] event in
            await self?.handleSSEEvent(event)
        }
    }

    /// Stop listening to SSE events.
    @MainActor
    public func stopListening(broadcaster: SSEEventBroadcaster) {
        if let id = sseRegistrationId {
            broadcaster.unregister(id)
            sseRegistrationId = nil
        }
    }

    @MainActor
    private func handleSSEEvent(_ event: SSEEvent) async {
        if Self.refreshEventTypes.contains(event.type) {
            await loadPresence()
        }
        if event.type == "hud.pipeline" {
            await loadPipelines()
        }
    }

    /// Refresh just the pipelines section (called on hud.pipeline SSE events).
    public func loadPipelines() async {
        do {
            let response: MobilePipelinesResponse = try await apiClient.request(.pipelines)
            pipelines = response.pipelines
            recentPipelines = response.recentPipelines
            pipelineSummary = response.summary
            pipelinesAvailable = response.available || !response.pipelines.isEmpty || !response.recentPipelines.isEmpty
        } catch {
            // Non-critical — keep existing data on transient failures.
        }
    }

    /// Refresh just the presence/agents section (lightweight, called on SSE events).
    public func loadPresence() async {
        do {
            let response: MobilePresenceResponse = try await apiClient.request(.presence(limit: 50))
            presenceAgents = response.agents
            presenceClaims = response.claims
            presenceWorktrees = response.worktrees
            presenceSummary = response.summary
        } catch {
            // Non-critical — keep existing data on transient failures.
        }
    }

    /// Sections that can be independently loaded.
    public enum OpsSection: String, CaseIterable, Sendable {
        case work
        case pipelines
        case runtime
        case sandbox
        case context
    }

    /// Load everything (legacy entry point, used by pull-to-refresh).
    public func load() async {
        error = nil
        warningMessage = nil

        await loadWorkSection()
        await loadPipelinesSection()
        await loadRuntimeSection()
        await loadSandboxSection()
        await loadContextSection()

    }

    /// Load only data needed by the Work section: tasks and legacy workflows.
    public func loadWorkSection() async {
        guard await beginLoading(.work) else { return }
        defer { finishLoading(.work) }

        error = nil
        warningMessage = nil
        var refreshedTasks = false

        do {
            let response: MobileTasksResponse = try await apiClient.request(.tasks(limit: 50))
            tasks = response.tasks
            taskCounts = response.counts
            refreshedTasks = true
        } catch {
            if tasks.isEmpty {
                taskCounts = MobileTaskCounts(pending: 0, inProgress: 0, blocked: 0, completed: 0)
                self.error = error as? LoomAPIError ?? .networkError(underlying: error.localizedDescription)
            }
            warningMessage = "Some Ops data could not be refreshed: tasks"
        }

        do {
            let response: MobileWorkflowsResponse = try await apiClient.request(.workflows(limit: 50))
            workflows = response.workflows
            pendingApprovals = response.pendingApprovals
            if response.deprecated && response.workflows.isEmpty && response.pendingApprovals == 0 {
                workflowsDeprecated = false
                workflowsDeprecationMessage = nil
            } else {
                workflowsDeprecated = response.deprecated
                workflowsDeprecationMessage = response.deprecationMessage
            }
        } catch {
            workflows = []
            pendingApprovals = 0
            workflowsDeprecated = false
            workflowsDeprecationMessage = nil
        }

        if refreshedTasks {
            workLastUpdatedAt = Date()
        }
        loadedSections.insert(.work)
    }

    /// Load only data needed by the Pipelines section.
    public func loadPipelinesSection() async {
        guard await beginLoading(.pipelines) else { return }
        defer { finishLoading(.pipelines) }

        await loadPipelines()
        loadedSections.insert(.pipelines)
    }

    /// Load only data needed by the Runtime section: presence, topology, control plane.
    ///
    /// Sandbox loads independently via `loadSandboxSection()` so the only mobile
    /// mutation surface (start/stop sandbox) can be its own first-class peek.
    public func loadRuntimeSection() async {
        guard await beginLoading(.runtime) else { return }
        defer { finishLoading(.runtime) }

        do {
            let response: MobilePresenceResponse = try await apiClient.request(.presence(limit: 50))
            presenceAgents = response.agents
            presenceClaims = response.claims
            presenceWorktrees = response.worktrees
            presenceSummary = response.summary
        } catch {
            presenceAgents = []
            presenceClaims = []
            presenceWorktrees = []
            presenceSummary = MobilePresenceSummary(activeAgents: 0, idleAgents: 0, offlineAgents: 0, totalAgents: 0, claimCount: 0, worktreeCount: 0)
        }

        do {
            let response: MobileTopologyResponse = try await apiClient.request(.topology)
            topology = response
        } catch {
            topology = nil
        }

        do {
            let response: MobileControlPlaneResponse = try await apiClient.request(.controlPlane)
            controlPlane = response
        } catch {
            controlPlane = nil
        }

        loadedSections.insert(.runtime)
    }

    /// Load only sandbox/devbox summary. Separate from Runtime so the mutation
    /// surface (the only one allowed on mobile) renders as its own peek.
    public func loadSandboxSection() async {
        guard await beginLoading(.sandbox) else { return }
        defer { finishLoading(.sandbox) }

        do {
            let response: MobileSandboxSummary = try await apiClient.request(.sandbox)
            sandboxSummary = response
        } catch {
            sandboxSummary = nil
        }

        loadedSections.insert(.sandbox)
    }

    /// Load only data needed by the Context section: memory, stream, graph, reasoning.
    public func loadContextSection() async {
        guard await beginLoading(.context) else { return }
        defer { finishLoading(.context) }

        do {
            let response: MobileMemoryStatsResponse = try await apiClient.request(.memoryStats)
            memoryStats = response.stats
        } catch {
            memoryStats = nil
        }

        do {
            let response: MobileMemoryItemsResponse = try await apiClient.request(.memoryItems(tier: .working, limit: 50))
            memoryItems = response.items
            memoryTier = response.tier
        } catch {
            memoryItems = []
            memoryTier = .working
        }

        do {
            let response: MobileStreamResponse = try await apiClient.request(.stream(limit: 50))
            streamEntries = response.entries
        } catch {
            streamEntries = []
        }

        do {
            let response: MobileGraphStatsResponse = try await apiClient.request(.graphStats)
            graphStats = response.stats
        } catch {
            graphStats = nil
        }

        do {
            let response: MobileGraphEntitiesResponse = try await apiClient.request(.graphEntities(limit: 50))
            graphEntities = response.entities
        } catch {
            graphEntities = []
        }

        graphPath = nil
        if graphEntities.count >= 2 {
            do {
                let source = graphEntities[0].id
                let target = graphEntities[1].id
                let response: MobileGraphPathResponse = try await apiClient.request(.graphPath(sourceId: source, targetId: target, maxDepth: 5))
                graphPath = response.path
            } catch {
                // Non-critical
            }
        }

        do {
            let response: MobileReasoningChainsResponse = try await apiClient.request(.reasoningChains(limit: 50))
            reasoningChains = response.chains
        } catch {
            reasoningChains = []
        }

        loadedSections.insert(.context)
    }

    /// Load a section lazily (skip if already loaded).
    public func loadSectionIfNeeded(_ section: OpsSection) async {
        guard !loadedSections.contains(section) else { return }
        await loadSection(section)
    }

    /// Refresh a section while preserving its current data on screen.
    public func reloadSection(_ section: OpsSection) async {
        await loadSection(section)
    }

    public func isLoading(_ section: OpsSection) -> Bool {
        loadingSections.contains(section)
    }

    private func loadSection(_ section: OpsSection) async {
        switch section {
        case .work: await loadWorkSection()
        case .pipelines: await loadPipelinesSection()
        case .runtime: await loadRuntimeSection()
        case .sandbox: await loadSandboxSection()
        case .context: await loadContextSection()
        }
    }

    private func beginLoading(_ section: OpsSection) async -> Bool {
        if loadingSections.contains(section) {
            await withCheckedContinuation { continuation in
                sectionLoadWaiters[section, default: []].append(continuation)
            }
            return false
        }
        loadingSections.insert(section)
        return true
    }

    private func finishLoading(_ section: OpsSection) {
        loadingSections.remove(section)
        let waiters = sectionLoadWaiters.removeValue(forKey: section) ?? []
        for waiter in waiters {
            waiter.resume()
        }
    }

    public func loadWorkflowDetail(id: String) async throws -> MobileWorkflowDetailResponse {
        try await apiClient.request(.workflowDetail(id: id))
    }

    /// Approve a workflow's pending step. The backend returns a mutation
    /// acknowledgement (`{workflow_id, step_id, action}`), NOT the workflow
    /// record — callers re-fetch the detail to refresh state.
    @discardableResult
    public func approveWorkflow(id: String, stepId: String) async throws -> MobileWorkflowMutationResponse {
        let response: MobileWorkflowMutationResponse = try await apiClient.request(
            .workflowApprove(id: id, stepId: stepId))
        await refreshWorkflowsAfterMutation()
        return response
    }

    /// Reject a workflow's pending step with an optional operator reason.
    @discardableResult
    public func rejectWorkflow(id: String, stepId: String, reason: String? = nil) async throws -> MobileWorkflowMutationResponse {
        let response: MobileWorkflowMutationResponse = try await apiClient.request(
            .workflowReject(id: id, stepId: stepId, reason: reason))
        await refreshWorkflowsAfterMutation()
        return response
    }

    /// Best-effort refresh of the workflow list after an approve/reject so the
    /// Work queue's pending-approval count stops lying. Failures are swallowed:
    /// the mutation already succeeded, and the next poll will reconcile.
    private func refreshWorkflowsAfterMutation() async {
        do {
            let response: MobileWorkflowsResponse = try await apiClient.request(.workflows(limit: 50))
            workflows = response.workflows
            pendingApprovals = response.pendingApprovals
        } catch {
            // Non-critical.
        }
    }

    // MARK: - Handoff inbox

    /// Load the handoff inbox. Called when the inbox surface is presented and
    /// on its pull-to-refresh.
    public func loadHandoffs(force: Bool = false) async {
        guard force || !handoffsLoaded else { return }
        isLoadingHandoffs = true
        handoffsError = nil
        defer {
            isLoadingHandoffs = false
            handoffsLoaded = true
        }
        do {
            let response: MobileHandoffsResponse = try await apiClient.request(.handoffs(limit: 50))
            handoffs = response.handoffs
        } catch {
            handoffsError = error as? LoomAPIError ?? .networkError(underlying: error.localizedDescription)
        }
    }

    /// Accept a pending handoff. The HUD resolves the destination session from
    /// the handoff's target agent when no explicit session id is supplied.
    public func acceptHandoff(_ handoff: MobileHandoff, importEntries: Bool = true) async {
        let target = handoff.targetAgentId.isEmpty ? handoff.toAgent : handoff.targetAgentId
        await performHandoffAction(
            id: handoff.id,
            endpoint: .handoffAccept(id: handoff.id, targetAgentId: target, importEntries: importEntries),
            successVerb: "accepted"
        )
    }

    /// Reject a pending handoff, optionally telling the source agent why.
    public func rejectHandoff(_ handoff: MobileHandoff, reason: String? = nil) async {
        await performHandoffAction(
            id: handoff.id,
            endpoint: .handoffReject(id: handoff.id, reason: reason),
            successVerb: "rejected"
        )
    }

    private func performHandoffAction(id: String, endpoint: Endpoint, successVerb: String) async {
        guard mutatingHandoffID == nil else { return }
        mutatingHandoffID = id
        handoffActionError = nil
        handoffActionMessage = nil
        defer { mutatingHandoffID = nil }

        do {
            let response: MobileHandoffActionResponse = try await apiClient.request(endpoint)
            handoffActionMessage = "Handoff \(response.handoffId) \(successVerb)."
            await loadHandoffs(force: true)
        } catch {
            let loomError = error as? LoomAPIError ?? .networkError(underlying: error.localizedDescription)
            handoffActionError = loomError.description
        }
    }

    public func loadReasoningChainDetail(id: String) async throws -> MobileReasoningChainDetailResponse {
        try await apiClient.request(.reasoningChainDetail(id: id))
    }

    /// Load spawn config (project list + agent types) on demand. Kept best-effort:
    /// the Work session-start form degrades to a free-text namespace field if
    /// this endpoint is unavailable.
    public func loadSpawnConfig() async {
        guard spawnConfig == nil else { return }
        do {
            spawnConfig = try await apiClient.request(.spawnConfig)
        } catch {
            // Leave spawnConfig nil; callers fall back to manual text entry.
        }
    }

    /// Start a session using the mobile mutation endpoint.
    public func createSession(agentID: String, namespace: String?, description: String?, autoRecall: Bool) async {
        let trimmedAgentID = agentID.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmedAgentID.isEmpty else {
            mutationErrorMessage = "Agent ID is required"
            return
        }

        isMutatingSession = true
        mutationErrorMessage = nil
        defer { isMutatingSession = false }

        do {
            let response: SessionCreateResponse = try await apiClient.request(
                .createSession(
                    agentId: trimmedAgentID,
                    namespace: normalizedOptional(namespace),
                    description: normalizedOptional(description),
                    autoRecall: autoRecall
                )
            )
            mutationStatusMessage = response.alreadyExisted
                ? "Session already exists: \(response.sessionId)"
                : "Session started: \(response.sessionId)"
        } catch {
            mutationErrorMessage = toLoomError(error).description
        }
    }

    /// End a session using the mobile mutation endpoint.
    public func endSession(sessionID: String, summarize: Bool) async {
        let trimmedSessionID = sessionID.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmedSessionID.isEmpty else {
            mutationErrorMessage = "Session ID is required"
            return
        }

        isMutatingSession = true
        mutationErrorMessage = nil
        defer { isMutatingSession = false }

        do {
            let response: SessionEndResponse = try await apiClient.request(.endSession(id: trimmedSessionID, summarize: summarize))
            mutationStatusMessage = response.ended
                ? "Session ended: \(response.sessionId)"
                : "No active session ended for \(response.sessionId)"
        } catch {
            mutationErrorMessage = toLoomError(error).description
        }
    }

    public func startSandbox(project: String, agentID: String?) async {
        let trimmedProject = project.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmedProject.isEmpty else {
            sandboxMutationError = "Project is required"
            return
        }

        isMutatingSandbox = true
        sandboxMutationError = nil
        defer { isMutatingSandbox = false }

        do {
            let response: MobileSandboxStartResponse = try await apiClient.request(
                .sandboxStart(project: trimmedProject, agentId: normalizedOptional(agentID))
            )
            sandboxMutationMessage = response.started
                ? "Sandbox started: \(response.project)"
                : "Sandbox build queued: \(response.project)"
            await refreshSandboxSummaryAfterMutation()
        } catch {
            sandboxMutationError = toLoomError(error).description
        }
    }

    public func stopSandbox(project: String) async {
        let trimmedProject = project.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmedProject.isEmpty else {
            sandboxMutationError = "Project is required"
            return
        }

        isMutatingSandbox = true
        sandboxMutationError = nil
        defer { isMutatingSandbox = false }

        do {
            let response: MobileSandboxStopResponse = try await apiClient.request(
                .sandboxStop(project: trimmedProject)
            )
            sandboxMutationMessage = response.stopped
                ? "Sandbox stopped: \(response.project)"
                : "Sandbox stop requested: \(response.project)"
            await refreshSandboxSummaryAfterMutation()
        } catch {
            sandboxMutationError = toLoomError(error).description
        }
    }

    public func clearMutationMessages() {
        mutationStatusMessage = nil
        mutationErrorMessage = nil
        sandboxMutationMessage = nil
        sandboxMutationError = nil
    }

    private func normalizedOptional(_ value: String?) -> String? {
        guard let value else { return nil }
        let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }

    private func refreshSandboxSummaryAfterMutation() async {
        do {
            let latest: MobileSandboxSummary = try await apiClient.request(.sandbox)
            sandboxSummary = latest
        } catch {
            // Keep mutation success visible even if follow-up refresh fails.
        }
    }

    private func toLoomError(_ error: Error) -> LoomAPIError {
        if let loomError = error as? LoomAPIError {
            return loomError
        }
        return .networkError(underlying: error.localizedDescription)
    }
}
