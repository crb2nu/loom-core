import Foundation
import Testing
@testable import LoomCompanionKit

@Suite("AgentsViewModel conversation grouping")
struct AgentsViewModelGroupingTests {

    private func makeAgent(
        _ id: String,
        namespace: String? = nil,
        sessionId: String? = nil
    ) -> UnifiedAgent {
        var obj: [String: Any] = [
            "agent_id": id,
            "agent_type": id.hasPrefix("codex") ? "codex" : "claude-code",
            "status": "active",
            "source": sessionId == nil ? "presence" : "presence+session",
            "has_presence": true,
        ]
        if let namespace { obj["namespace"] = namespace }
        if let sessionId {
            obj["session_id"] = sessionId
            obj["has_session"] = true
        }
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
}
