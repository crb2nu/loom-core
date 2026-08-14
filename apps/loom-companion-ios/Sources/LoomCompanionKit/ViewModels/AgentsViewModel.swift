import Foundation

/// ViewModel for the unified Agents tab.
@Observable
public final class AgentsViewModel {
    public var agents: [UnifiedAgent] = []
    public var summary: UnifiedAgentsSummary?
    public var isLoading = false
    public var error: LoomAPIError?

    // Filters
    public var statusFilter: MobilePresenceStatus?
    public var typeFilter: String?
    public var searchText: String = ""
    public var attentionOnly: Bool = false

    // Session mutation state
    public var isCreating = false
    public var createError: String?

    @ObservationIgnored
    private let apiClient: any LoomAPIClientProtocol

    @ObservationIgnored
    private var sseRegistrationId: UUID?

    /// SSE event types that trigger an agents refresh.
    private static let refreshEventTypes: Set<String> = [
        "hud.fleet", "agent.heartbeat",
        "agent.session.start", "agent.session.end", "agent.session.reaped",
        "agent.spawn.building", "agent.spawn.running",
        "agent.spawn.completed", "agent.spawn.failed", "agent.spawn.stopped",
        "agent.context.added", "agent.session.stats.updated",
        "agent.task.update",
    ]

    public init(apiClient: any LoomAPIClientProtocol) {
        self.apiClient = apiClient
    }

    /// Start listening to SSE events via the broadcaster.
    @MainActor
    public func startListening(broadcaster: SSEEventBroadcaster) {
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
            await load()
        }
    }

    /// Fetch unified agents from the API.
    public func load() async {
        isLoading = true
        defer { isLoading = false }
        do {
            let response: UnifiedAgentsResponse = try await apiClient.request(.agents(limit: 50))
            agents = response.agents
            summary = response.summary
            error = nil
        } catch let err as LoomAPIError {
            error = err
        } catch {
            self.error = .networkError(underlying: error.localizedDescription)
        }
    }

    /// Create a new session and reload agents on success.
    public func createSession(
        agentId: String,
        namespace: String? = nil,
        description: String? = nil,
        autoRecall: Bool? = nil
    ) async {
        isCreating = true
        createError = nil
        defer { isCreating = false }

        do {
            let _: SessionCreateResponse = try await apiClient.request(
                .createSession(agentId: agentId, namespace: namespace, description: description, autoRecall: autoRecall)
            )
            await load()
        } catch let err as LoomAPIError {
            createError = err.description
        } catch {
            createError = error.localizedDescription
        }
    }

    /// Total agents flagged as needing attention (ignores current filters).
    public var attentionCount: Int {
        agents.reduce(0) { count, agent in
            count + ((agent.needsAttention || agent.blockedTasks > 0) ? 1 : 0)
        }
    }

    /// Agents after applying current filters.
    public var filteredAgents: [UnifiedAgent] {
        agents.filter { agent in
            if attentionOnly, !(agent.needsAttention || agent.blockedTasks > 0) {
                return false
            }
            if let statusFilter, agent.status != statusFilter {
                return false
            }
            if let typeFilter, !typeFilter.isEmpty, !agent.agentType.localizedCaseInsensitiveContains(typeFilter) {
                return false
            }
            if !searchText.isEmpty {
                let query = searchText.lowercased()
                let matches = agent.agentId.lowercased().contains(query)
                    || agent.description.lowercased().contains(query)
                    || agent.currentTask.lowercased().contains(query)
                    || agent.branch.lowercased().contains(query)
                    || (agent.project?.lowercased().contains(query) ?? false)
                    || (agent.namespace?.lowercased().contains(query) ?? false)
                if !matches { return false }
            }
            return true
        }
    }

    public var groupedAgents: [UnifiedAgentGroup] {
        let visible = filteredAgents

        // A "fold" is a conversation or spawn-root that may legitimately span
        // repos. Only folds that actually touch >= 2 scopes earn their own
        // section; every other agent groups by its scope (repo / namespace), so
        // independent agents in the same repo share ONE header instead of N
        // identical singleton headers.
        var foldScopes: [String: Set<String>] = [:]
        for agent in visible {
            guard let fold = foldKey(for: agent) else { continue }
            foldScopes[fold, default: []].insert(scopeDescriptor(for: agent).id)
        }
        let crossScopeFolds = Set(foldScopes.lazy.filter { $0.value.count >= 2 }.map(\.key))

        var grouped: [String: UnifiedAgentGroup] = [:]
        for agent in visible {
            let descriptor: (id: String, title: String, subtitle: String?)
            if let fold = foldKey(for: agent), crossScopeFolds.contains(fold) {
                descriptor = foldDescriptor(fold, for: agent)
            } else {
                descriptor = scopeDescriptor(for: agent)
            }
            if let existing = grouped[descriptor.id] {
                // Keep the title/subtitle set when the group was created.
                grouped[descriptor.id] = UnifiedAgentGroup(
                    id: existing.id,
                    title: existing.title,
                    subtitle: existing.subtitle,
                    agents: existing.agents + [agent]
                )
            } else {
                grouped[descriptor.id] = UnifiedAgentGroup(
                    id: descriptor.id,
                    title: descriptor.title,
                    subtitle: descriptor.subtitle,
                    agents: [agent]
                )
            }
        }

        return grouped.values.sorted { lhs, rhs in
            let leftRank = groupSortRank(lhs.id)
            let rightRank = groupSortRank(rhs.id)
            if leftRank != rightRank {
                return leftRank < rightRank
            }
            let leftLive = liveCount(lhs)
            let rightLive = liveCount(rhs)
            if leftLive != rightLive {
                return leftLive > rightLive
            }
            if lhs.agents.count != rhs.agents.count {
                return lhs.agents.count > rhs.agents.count
            }
            return lhs.title.localizedCaseInsensitiveCompare(rhs.title) == .orderedAscending
        }
    }

    private func liveCount(_ group: UnifiedAgentGroup) -> Int {
        group.agents.reduce(0) { $0 + ($1.status == .active ? 1 : 0) }
    }

    /// Unique agent types from current agents, for filter dropdown.
    public var availableTypes: [String] {
        Array(Set(agents.map(\.agentType))).sorted()
    }

    /// The scope bucket (repo -> namespace -> branch -> per-agent) an agent
    /// belongs to. This is the PRIMARY roster grouping: independent agents in
    /// the same repo land in one section instead of fragmenting into one
    /// identically-titled section each.
    private func scopeDescriptor(for agent: UnifiedAgent) -> (id: String, title: String, subtitle: String?) {
        // Codex infrastructure sessions (keepalive wrapper / heartbeat bootstrap)
        // cluster on their own so they don't dilute a real repo section.
        if let infraKey = codexInfrastructureGroupKey(for: agent) {
            return (infraKey, "Codex infrastructure", normalized(agent.namespace))
        }
        if let project = normalized(agent.project) {
            return ("scope:proj:\(project)", LoomFormat.lastPathComponent(project), project)
        }
        if let namespace = normalized(agent.namespace) {
            return ("scope:ns:\(namespace)", LoomFormat.lastPathComponent(namespace), namespace)
        }
        if let branch = normalized(agent.branch) {
            return ("branch:\(branch)", branch, "Branch group")
        }
        // No scope signal at all: keep the agent on its own row + header.
        return ("agent:\(agent.agentId)", displayAgentType(agent.agentType), agent.agentId)
    }

    /// The conversation / spawn-root an agent belongs to, if any. Used only to
    /// detect genuine cross-repo clusters that should stay folded together — one
    /// chat that moved across repos (Claude/Gemini) or a spawn fan-out.
    private func foldKey(for agent: UnifiedAgent) -> String? {
        if let root = normalized(agent.rootSessionId), root != normalized(agent.sessionId) {
            return "session:\(root)"
        }
        let conversation = agent.conversationId
        return conversation.isEmpty ? nil : "conversation:\(conversation)"
    }

    /// Header for a fold that spans multiple repos. Titled by vendor since no
    /// single repo represents it.
    private func foldDescriptor(_ key: String, for agent: UnifiedAgent) -> (id: String, title: String, subtitle: String?) {
        let subtitle = key.hasPrefix("session:") ? "Spawned across repos" : "Conversation across repos"
        return (key, displayAgentType(agent.agentType), subtitle)
    }

    private func codexInfrastructureGroupKey(for agent: UnifiedAgent) -> String? {
        let typeBlob = "\(agent.agentType) \(agent.agentId)".lowercased()
        guard typeBlob.contains("codex") else { return nil }
        let desc = agent.description.lowercased()
        let isInfra = desc.contains("keepalive wrapper session")
            || desc.contains("heartbeat bootstrap session")
        guard isInfra, let namespace = normalized(agent.namespace) else { return nil }
        return "codex-infra:\(namespace)"
    }

    private func groupSortRank(_ id: String) -> Int {
        if id.hasPrefix("session:") { return 0 }       // cross-repo spawn fan-out
        if id.hasPrefix("conversation:") { return 1 }  // cross-repo chat
        if id.hasPrefix("scope:proj:") { return 2 }    // repo sections
        if id.hasPrefix("scope:ns:") { return 3 }      // namespace sections
        if id.hasPrefix("codex-infra:") { return 4 }
        if id.hasPrefix("branch:") { return 5 }
        if id.hasPrefix("agent:") { return 6 }
        return 7
    }

    private func normalized(_ value: String?) -> String? {
        guard let trimmed = value?.trimmingCharacters(in: .whitespacesAndNewlines),
              !trimmed.isEmpty
        else {
            return nil
        }
        return trimmed
    }

    private func displayAgentType(_ agentType: String) -> String {
        let trimmed = agentType.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return "Runtime" }
        return trimmed
            .replacingOccurrences(of: "_", with: " ")
            .replacingOccurrences(of: "-", with: " ")
            .split(separator: " ")
            .map { $0.capitalized }
            .joined(separator: " ")
    }
}
