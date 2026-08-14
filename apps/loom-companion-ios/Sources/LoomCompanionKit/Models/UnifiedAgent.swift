import Foundation

/// Unified agent model merging presence, session, and spawn data.
public struct UnifiedAgent: Decodable, Identifiable, Sendable {
    public let agentId: String
    /// Server-provided conversation identity (`conversation_id`). Nil when an
    /// older server omits it; use `conversationId`, which falls back to a
    /// client-side computation.
    public let conversationIdRaw: String?
    public let agentType: String
    public let status: MobilePresenceStatus
    public let source: String
    public let description: String
    public let currentTask: String
    public let branch: String
    public let lastHeartbeat: String
    public let sessionId: String?
    public let parentSessionId: String?
    public let rootSessionId: String?
    public let namespace: String?
    public let sessionStatus: String?
    public let sessionStartedAt: String?
    public let entryCount: Int
    public let totalTokens: Int
    public let spawnId: String?
    public let spawnStatus: String?
    public let project: String?
    public let activeFileCount: Int
    public let needsAttention: Bool
    public let attentionReasons: [String]
    public let taskCount: Int
    public let blockedTasks: Int
    public let claimCount: Int
    public let pipelineCount: Int
    public let pipelineStatus: String?
    public let heartbeatAgeSeconds: Int
    public let sessionAgeSeconds: Int
    public let telemetryStatus: String
    public let hasPresenceEvidence: Bool
    public let hasSessionEvidence: Bool
    /// Presence registered but no live session past the orphan grace window
    /// (`fleetview` orphan verdict, carried through the mobile join).
    public let isOrphan: Bool
    public let orphanAgeSeconds: Int

    public var id: String { agentId }
    public var hasSession: Bool { hasSessionEvidence || (sessionId != nil && !(sessionId?.isEmpty ?? true)) }
    public var isSpawned: Bool { spawnId != nil && !(spawnId?.isEmpty ?? true) }

    /// The conversation this agent belongs to — one chat that moved across repos
    /// (Claude/Gemini) or one app's twins in a workspace (Codex). Prefers the
    /// server value; falls back to a client-side computation so grouping works
    /// against servers that predate `conversation_id`.
    public var conversationId: String {
        if let raw = conversationIdRaw?.trimmingCharacters(in: .whitespacesAndNewlines), !raw.isEmpty {
            return raw
        }
        return UnifiedAgent.conversationId(forAgentId: agentId)
    }

    private static let workspaceAnchoredBases: Set<String> = ["codex"]

    /// Pure port of `conversationId()` in the web client's agents.ts. Mirrors
    /// `fleetview.ConversationID` on the Go server.
    public static func conversationId(forAgentId agentId: String) -> String {
        let id = agentId.trimmingCharacters(in: .whitespacesAndNewlines)
        if id.isEmpty { return "" }
        let parts = id.split(separator: "-", omittingEmptySubsequences: false).map(String.init)
        func isDigits(_ s: String) -> Bool { !s.isEmpty && s.allSatisfy { $0 >= "0" && $0 <= "9" } }
        guard let wsIdx = parts.firstIndex(where: isDigits) else { return id }
        let base = parts[0..<wsIdx].joined(separator: "-")
        if workspaceAnchoredBases.contains(base) {
            return parts[0...wsIdx].joined(separator: "-")
        }
        var scope = ""
        var i = parts.count - 1
        while i > wsIdx {
            if isDigits(parts[i]) { scope = parts[i]; break }
            i -= 1
        }
        if scope.isEmpty { return id }
        return base.isEmpty ? scope : "\(base)-\(scope)"
    }

    enum CodingKeys: String, CodingKey {
        case agentId = "agent_id"
        case conversationIdRaw = "conversation_id"
        case agentType = "agent_type"
        case status
        case source
        case description
        case currentTask = "current_task"
        case branch
        case lastHeartbeat = "last_heartbeat"
        case sessionId = "session_id"
        case parentSessionId = "parent_session_id"
        case rootSessionId = "root_session_id"
        case namespace
        case sessionStatus = "session_status"
        case sessionStartedAt = "session_started_at"
        case entryCount = "entry_count"
        case totalTokens = "total_tokens"
        case spawnId = "spawn_id"
        case spawnStatus = "spawn_status"
        case project
        case activeFileCount = "active_file_count"
        case needsAttention = "needs_attention"
        case attentionReasons = "attention_reasons"
        case taskCount = "task_count"
        case blockedTasks = "blocked_tasks"
        case claimCount = "claim_count"
        case pipelineCount = "pipeline_count"
        case pipelineStatus = "pipeline_status"
        case heartbeatAgeSeconds = "heartbeat_age_seconds"
        case sessionAgeSeconds = "session_age_seconds"
        case telemetryStatus = "telemetry_status"
        case hasPresenceEvidence = "has_presence"
        case hasSessionEvidence = "has_session"
        case isOrphan = "is_orphan"
        case orphanAgeSeconds = "orphan_age_seconds"
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        self.agentId = try container.decode(String.self, forKey: .agentId)
        self.conversationIdRaw = try container.decodeIfPresent(String.self, forKey: .conversationIdRaw)
        self.agentType = try container.decodeIfPresent(String.self, forKey: .agentType) ?? "unknown"
        self.status = try container.decode(MobilePresenceStatus.self, forKey: .status)
        self.source = try container.decodeIfPresent(String.self, forKey: .source) ?? "presence"
        self.description = try container.decodeIfPresent(String.self, forKey: .description) ?? ""
        self.currentTask = try container.decodeIfPresent(String.self, forKey: .currentTask) ?? ""
        self.branch = try container.decodeIfPresent(String.self, forKey: .branch) ?? ""
        self.lastHeartbeat = try container.decodeIfPresent(String.self, forKey: .lastHeartbeat) ?? ""
        self.sessionId = try container.decodeIfPresent(String.self, forKey: .sessionId)
        self.parentSessionId = try container.decodeIfPresent(String.self, forKey: .parentSessionId)
        self.rootSessionId = try container.decodeIfPresent(String.self, forKey: .rootSessionId)
        self.namespace = try container.decodeIfPresent(String.self, forKey: .namespace)
        self.sessionStatus = try container.decodeIfPresent(String.self, forKey: .sessionStatus)
        self.sessionStartedAt = try container.decodeIfPresent(String.self, forKey: .sessionStartedAt)
        self.entryCount = try container.decodeIfPresent(Int.self, forKey: .entryCount) ?? 0
        self.totalTokens = try container.decodeIfPresent(Int.self, forKey: .totalTokens) ?? 0
        self.spawnId = try container.decodeIfPresent(String.self, forKey: .spawnId)
        self.spawnStatus = try container.decodeIfPresent(String.self, forKey: .spawnStatus)
        self.project = try container.decodeIfPresent(String.self, forKey: .project)
        self.activeFileCount = try container.decodeIfPresent(Int.self, forKey: .activeFileCount) ?? 0
        self.needsAttention = try container.decodeIfPresent(Bool.self, forKey: .needsAttention) ?? false
        self.attentionReasons = try container.decodeIfPresent([String].self, forKey: .attentionReasons) ?? []
        self.taskCount = try container.decodeIfPresent(Int.self, forKey: .taskCount) ?? 0
        self.blockedTasks = try container.decodeIfPresent(Int.self, forKey: .blockedTasks) ?? 0
        self.claimCount = try container.decodeIfPresent(Int.self, forKey: .claimCount) ?? 0
        self.pipelineCount = try container.decodeIfPresent(Int.self, forKey: .pipelineCount) ?? 0
        self.pipelineStatus = try container.decodeIfPresent(String.self, forKey: .pipelineStatus)
        self.heartbeatAgeSeconds = try container.decodeIfPresent(Int.self, forKey: .heartbeatAgeSeconds) ?? 0
        self.sessionAgeSeconds = try container.decodeIfPresent(Int.self, forKey: .sessionAgeSeconds) ?? 0
        self.telemetryStatus = try container.decodeIfPresent(String.self, forKey: .telemetryStatus) ?? ""
        self.hasPresenceEvidence = try container.decodeIfPresent(Bool.self, forKey: .hasPresenceEvidence) ?? (self.source != "session" && self.source != "session_only")
        self.hasSessionEvidence = try container.decodeIfPresent(Bool.self, forKey: .hasSessionEvidence) ?? (self.sessionId != nil && !(self.sessionId?.isEmpty ?? true))
        self.isOrphan = try container.decodeIfPresent(Bool.self, forKey: .isOrphan) ?? false
        self.orphanAgeSeconds = try container.decodeIfPresent(Int.self, forKey: .orphanAgeSeconds) ?? 0
    }
}

/// Summary counts from the agents endpoint.
public struct UnifiedAgentsSummary: Decodable, Sendable {
    public let totalAgents: Int
    public let activeAgents: Int
    public let idleAgents: Int
    public let offlineAgents: Int
    public let spawnedAgents: Int
    public let withSessions: Int
    public let orphans: Int

    enum CodingKeys: String, CodingKey {
        case totalAgents = "total_agents"
        case activeAgents = "active_agents"
        case idleAgents = "idle_agents"
        case offlineAgents = "offline_agents"
        case spawnedAgents = "spawned_agents"
        case withSessions = "with_sessions"
        case orphans
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        self.totalAgents = try container.decodeIfPresent(Int.self, forKey: .totalAgents) ?? 0
        self.activeAgents = try container.decodeIfPresent(Int.self, forKey: .activeAgents) ?? 0
        self.idleAgents = try container.decodeIfPresent(Int.self, forKey: .idleAgents) ?? 0
        self.offlineAgents = try container.decodeIfPresent(Int.self, forKey: .offlineAgents) ?? 0
        self.spawnedAgents = try container.decodeIfPresent(Int.self, forKey: .spawnedAgents) ?? 0
        self.withSessions = try container.decodeIfPresent(Int.self, forKey: .withSessions) ?? 0
        self.orphans = try container.decodeIfPresent(Int.self, forKey: .orphans) ?? 0
    }
}

/// Response wrapper for the /api/mobile/v1/agents endpoint.
public struct UnifiedAgentsResponse: Decodable, Sendable {
    public let agents: [UnifiedAgent]
    public let summary: UnifiedAgentsSummary
}

public struct UnifiedAgentGroup: Identifiable, Sendable {
    public let id: String
    public let title: String
    public let subtitle: String?
    public let agents: [UnifiedAgent]

    public init(id: String, title: String, subtitle: String? = nil, agents: [UnifiedAgent]) {
        self.id = id
        self.title = title
        self.subtitle = subtitle
        self.agents = agents
    }
}
