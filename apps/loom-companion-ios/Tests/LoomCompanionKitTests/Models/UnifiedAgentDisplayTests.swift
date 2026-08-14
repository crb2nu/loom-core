import Foundation
import Testing
@testable import LoomCompanionKit

@Suite("UnifiedAgent display titles and conversation grouping")
struct UnifiedAgentDisplayTests {

    private func makeAgent(
        _ id: String,
        type: String? = nil,
        status: String = "active",
        description: String? = nil,
        currentTask: String? = nil,
        spawnId: String? = nil,
        branch: String? = nil,
        namespace: String? = nil,
        project: String? = nil
    ) -> UnifiedAgent {
        var obj: [String: Any] = [
            "agent_id": id,
            "status": status,
            "source": "presence",
            "has_presence": true,
        ]
        if let type { obj["agent_type"] = type }
        if let description { obj["description"] = description }
        if let currentTask { obj["current_task"] = currentTask }
        if let spawnId { obj["spawn_id"] = spawnId }
        if let branch { obj["branch"] = branch }
        if let namespace { obj["namespace"] = namespace }
        if let project { obj["project"] = project }
        let data = try! JSONSerialization.data(withJSONObject: obj)
        return try! JSONDecoder().decode(UnifiedAgent.self, from: data)
    }

    @Test("displayTitle leads with the harness name plus a short conversation tag")
    func displayTitleShape() {
        let agent = makeAgent("claude-code-2928522331-1011922604", type: "claude-code")
        #expect(agent.harnessDisplayName == "Claude Code")
        // conversationId = claude-code-1011922604 → tag is last 4 of the scope.
        #expect(agent.conversationTag == "#2604")
        #expect(agent.displayTitle == "Claude Code · #2604")
    }

    @Test("harness name falls back to the agent-id base when type is missing")
    func harnessFallback() {
        #expect(makeAgent("claude-desktop-1713039686").harnessDisplayName == "Claude Desktop")
        #expect(makeAgent("codex-1713039686").harnessDisplayName == "Codex")
        // Unknown harness title-cases per segment.
        #expect(makeAgent("fancy-new-agent-42").harnessDisplayName == "Fancy New Agent")
        // No digits at all → base is the whole id, humanized; title has no tag.
        let bare = makeAgent("mystery")
        #expect(bare.conversationTag.isEmpty)
        #expect(bare.displayTitle == "Mystery")
    }

    @Test("agentIdBase stops at the first numeric segment")
    func base() {
        #expect(UnifiedAgent.agentIdBase("claude-code-123-456") == "claude-code")
        #expect(UnifiedAgent.agentIdBase("codex-1713039686") == "codex")
        #expect(UnifiedAgent.agentIdBase("123-claude") == "")
        #expect(UnifiedAgent.agentIdBase("") == "")
    }

    @Test("grouping collapses one conversation's twins into a single group, preserving order")
    func grouping() {
        let roster = [
            makeAgent("claude-code-238852444-616839048", type: "claude-code"),
            makeAgent("claude-code-2928522331-1011922604", type: "claude-code"),
            makeAgent("claude-code-3584052565-616839048", type: "claude-code"),
        ]
        let groups = UnifiedConversationGroup.group(roster)
        #expect(groups.count == 2)
        // First-seen order: the 616839048 conversation appeared first.
        #expect(groups[0].memberCount == 2)
        #expect(groups[0].representative.agentId == "claude-code-238852444-616839048")
        #expect(groups[1].memberCount == 1)
        #expect(groups[1].representative.agentId == "claude-code-2928522331-1011922604")
    }

    @Test("activity line strips redundant harness prefix and quiets heartbeat bootstrap")
    func activityCleaning() {
        let redundant = makeAgent(
            "claude-code-4194677927-2860056238", type: "claude-code",
            description: "Claude Code · libs/fi-fhir")
        #expect(redundant.cleanedActivityLine == "libs/fi-fhir")

        let heartbeat = makeAgent(
            "codex-228652627", type: "codex",
            description: "Heartbeat bootstrap session")
        #expect(heartbeat.cleanedActivityLine == "heartbeat only")

        let task = makeAgent(
            "claude-code-1-2", type: "claude-code",
            description: "ignored", currentTask: "renumber dwell migration")
        #expect(task.cleanedActivityLine == "renumber dwell migration")

        #expect(makeAgent("claude-code-1-2", type: "claude-code").cleanedActivityLine.isEmpty)
    }

    @Test("spawn pods get spawn-tail tags and branch-slug activity, not prompt text")
    func spawnRowPolish() {
        let spawn = makeAgent(
            "spawn-claude-6e92ff16d7da", type: "claude-code",
            currentTask: "WHAT THIS PIPELINE HAS ALREADY DONE: implement stage…",
            spawnId: "spawn-6e92ff16d7da",
            branch: "feat/bl-hud-spawn-split-a-runtime-image-20260808/extract")
        // Hex id digit-runs are degenerate ("#7") — the tag comes from the
        // spawn id tail instead.
        #expect(spawn.conversationTag == "#d7da")
        #expect(spawn.displayTitle == "Claude Code · #d7da")
        // Branch slug beats the prompt head.
        #expect(spawn.cleanedActivityLine == "bl-hud-spawn-split-a-runtime-image-20260808")
    }

    @Test("branchSlug drops type prefix and slice suffix, passes through odd shapes")
    func branchSlugShapes() {
        #expect(UnifiedAgent.branchSlug("feat/bl-x-20260808/extract") == "bl-x-20260808")
        #expect(UnifiedAgent.branchSlug("hotfix/bl-ci-gosec-oom-hardening-20260807/gosec-oom-hardening") == "bl-ci-gosec-oom-hardening-20260807")
        #expect(UnifiedAgent.branchSlug("main") == "main")
        #expect(UnifiedAgent.branchSlug("") == "")
    }

    @Test("workspaceTitle discriminates fold members by namespace tail, branch, then ws tag")
    func workspaceTitles() {
        let worktree = makeAgent(
            "claude-code-3708675240-1011922604", type: "claude-code",
            namespace: "services/loom-core/fix/hud-ios-spawn-row-polish",
            project: "services/loom-core")
        #expect(worktree.workspaceTitle == "fix/hud-ios-spawn-row-polish")

        let nsEqualsProject = makeAgent(
            "claude-code-2113270440-1011922604", type: "claude-code",
            branch: "main",
            namespace: "services/loom-core",
            project: "services/loom-core")
        #expect(nsEqualsProject.workspaceTitle == "main")

        let bare = makeAgent("claude-code-552019522-1011922604", type: "claude-code")
        #expect(bare.workspaceTitle == "ws #9522")
    }

    @Test("agents with no derivable conversation never collapse together")
    func emptyConversationStaysDistinct() {
        let roster = [makeAgent("alpha"), makeAgent("beta")]
        // Neither id carries digits → conversationId falls back to the id
        // itself (non-empty), so they stay distinct rows; the guard in
        // group() additionally keys genuinely-empty conversations by agent id.
        let groups = UnifiedConversationGroup.group(roster)
        #expect(groups.count == 2)
    }
}
