import Foundation
import Testing
@testable import LoomCompanionKit

@Suite("AgentsViewModel conversation grouping")
struct AgentsViewModelGroupingTests {

    private func makeAgent(
        _ id: String,
        namespace: String? = nil,
        project: String? = nil,
        sessionId: String? = nil,
        rootSessionId: String? = nil
    ) -> UnifiedAgent {
        var obj: [String: Any] = [
            "agent_id": id,
            "agent_type": id.hasPrefix("codex") ? "codex" : "claude-code",
            "status": "active",
            "source": sessionId == nil ? "presence" : "presence+session",
            "has_presence": true,
        ]
        if let namespace { obj["namespace"] = namespace }
        if let project { obj["project"] = project }
        if let sessionId {
            obj["session_id"] = sessionId
            obj["has_session"] = true
        }
        if let rootSessionId { obj["root_session_id"] = rootSessionId }
        let data = try! JSONSerialization.data(withJSONObject: obj)
        return try! JSONDecoder().decode(UnifiedAgent.self, from: data)
    }

    @Test("conversationId folds Claude cross-repo and Codex twins")
    func conversationIdComputation() {
        // Claude: keep SESSION_SCOPE, drop WS_HASH — one chat across repos folds.
        #expect(UnifiedAgent.conversationId(forAgentId: "claude-code-3749726816-1105899468") == "claude-code-1105899468")
        #expect(UnifiedAgent.conversationId(forAgentId: "claude-code-401508988-1105899468") == "claude-code-1105899468")
        // Codex: workspace-anchored — scopeless and scoped fold on WS_HASH.
        #expect(UnifiedAgent.conversationId(forAgentId: "codex-1713039686") == "codex-1713039686")
        #expect(UnifiedAgent.conversationId(forAgentId: "codex-1713039686-2004540290") == "codex-1713039686")
        // No numeric suffix: already a root.
        #expect(UnifiedAgent.conversationId(forAgentId: "codex-7b28") == "codex-7b28")
    }

    @Test("server conversation_id wins over the client fallback")
    func serverConversationIdPreferred() throws {
        let json = """
        {"agent_id":"claude-code-1-2","conversation_id":"server-supplied","agent_type":"claude-code","status":"active","source":"presence","has_presence":true}
        """.data(using: .utf8)!
        let agent = try JSONDecoder().decode(UnifiedAgent.self, from: json)
        #expect(agent.conversationId == "server-supplied")
    }

    @MainActor
    @Test("one Claude chat across two repos folds into a single roster group")
    func crossRepoChatFolds() {
        let vm = AgentsViewModel(apiClient: MockAPIClient())
        vm.agents = [
            // Same SESSION_SCOPE 1105899468, different WS_HASH → one conversation.
            makeAgent("claude-code-3749726816-1105899468", namespace: "services/loom-flightdeck/main"),
            makeAgent("claude-code-401508988-1105899468", namespace: "platform/gitops/main"),
            // A separate chat in another repo (different scope) → its own group.
            makeAgent("claude-code-552019522-2804496862", namespace: "services/loom-core/main"),
        ]

        let groups = vm.groupedAgents
        #expect(groups.count == 2)

        let folded = groups.first { $0.agents.count == 2 }
        #expect(folded != nil)
        #expect(folded?.id == "conversation:claude-code-1105899468")

        let solo = groups.first { $0.agents.count == 1 }
        #expect(solo?.agents.first?.agentId == "claude-code-552019522-2804496862")
    }

    @MainActor
    @Test("independent agents in one repo consolidate into a single scope section")
    func sameRepoIndependentAgentsConsolidate() {
        let vm = AgentsViewModel(apiClient: MockAPIClient())
        // Three independent spawn agents, each its own conversation (no shared
        // numeric scope), all in services/loom-core. Previously this produced
        // three identical "loom-core" singleton headers.
        vm.agents = [
            makeAgent("spawn-codex-a0a5c56afb3d", project: "services/loom-core"),
            makeAgent("spawn-codex-f067a0e76bff", project: "services/loom-core"),
            makeAgent("spawn-codex-49256d26188a", project: "services/loom-core"),
        ]

        let groups = vm.groupedAgents
        #expect(groups.count == 1)
        #expect(groups.first?.id == "scope:proj:services/loom-core")
        #expect(groups.first?.title == "loom-core")
        #expect(groups.first?.agents.count == 3)
    }

    @MainActor
    @Test("heartbeat agents sharing a namespace consolidate, not one header each")
    func sameNamespaceAgentsConsolidate() {
        let vm = AgentsViewModel(apiClient: MockAPIClient())
        vm.agents = [
            makeAgent("claude-desktop-1713039686", namespace: "agents"),
            makeAgent("claude-code-23456224-720397157", namespace: "agents"),
            makeAgent("gemini-99887766", namespace: "agents"),
        ]

        let groups = vm.groupedAgents
        #expect(groups.count == 1)
        #expect(groups.first?.id == "scope:ns:agents")
        #expect(groups.first?.agents.count == 3)
    }

    @MainActor
    @Test("a same-repo conversation does not split from its repo's other agents")
    func sameRepoConversationMergesWithScope() {
        let vm = AgentsViewModel(apiClient: MockAPIClient())
        vm.agents = [
            // Two agents of one chat (shared scope 1830282) in loom-core...
            makeAgent("claude-code-3088042209-1830282", project: "services/loom-core"),
            makeAgent("claude-code-552019522-1830282", project: "services/loom-core"),
            // ...plus an independent agent in the same repo.
            makeAgent("spawn-codex-a0a5c56afb3d", project: "services/loom-core"),
        ]

        let groups = vm.groupedAgents
        // All three live under one repo section — the conversation does not earn
        // its own header because it never leaves loom-core.
        #expect(groups.count == 1)
        #expect(groups.first?.id == "scope:proj:services/loom-core")
        #expect(groups.first?.agents.count == 3)
    }
}
