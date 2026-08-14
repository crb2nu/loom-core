import Foundation
import Testing
@testable import LoomCompanionKit

/// Coverage for the Operator deck's per-agent command sheet: the action
/// matrix must exactly track what the HUD will accept, and each control must
/// hit its documented route.
@Suite("AgentCommandViewModel")
struct AgentCommandViewModelTests {

    private func agent(
        id: String = "claude-code-1-2",
        sessionId: String? = nil,
        sessionStatus: String? = nil,
        spawnId: String? = nil,
        spawnStatus: String? = nil
    ) throws -> UnifiedAgent {
        var obj: [String: Any] = [
            "agent_id": id,
            "agent_type": "claude-code",
            "status": "active",
            "source": "presence",
            "has_presence": true,
        ]
        if let sessionId {
            obj["session_id"] = sessionId
            obj["has_session"] = true
        }
        if let sessionStatus { obj["session_status"] = sessionStatus }
        if let spawnId { obj["spawn_id"] = spawnId }
        if let spawnStatus { obj["spawn_status"] = spawnStatus }
        let data = try JSONSerialization.data(withJSONObject: obj)
        return try JSONDecoder().decode(UnifiedAgent.self, from: data)
    }

    // MARK: - Action derivation

    @Test("CLI agent with an active session offers view + end session only")
    func sessionAgentActions() throws {
        let a = try agent(sessionId: "sess-1", sessionStatus: "active")
        #expect(AgentCommandViewModel.actions(for: a) == [.viewSession, .endSession])
    }

    @Test("Ended session keeps the view action but drops end-session")
    func endedSessionActions() throws {
        let a = try agent(sessionId: "sess-1", sessionStatus: "completed")
        #expect(AgentCommandViewModel.actions(for: a) == [.viewSession])
    }

    @Test("Running spawn with a session offers the full control set")
    func runningSpawnActions() throws {
        let a = try agent(
            sessionId: "sess-1", sessionStatus: "active",
            spawnId: "spawn-1", spawnStatus: "running")
        #expect(AgentCommandViewModel.actions(for: a) == [
            .viewSession, .endSession, .message, .interrupt, .stop,
        ])
    }

    @Test("Creating spawns are controllable; terminal spawns are not")
    func spawnStatusGate() throws {
        let creating = try agent(spawnId: "spawn-1", spawnStatus: "creating")
        #expect(AgentCommandViewModel.actions(for: creating) == [.message, .interrupt, .stop])

        for terminal in ["completed", "failed", "stopped"] {
            let done = try agent(spawnId: "spawn-1", spawnStatus: terminal)
            #expect(AgentCommandViewModel.actions(for: done).isEmpty)
        }
    }

    @Test("Presence-only agent offers nothing to mutate")
    func presenceOnlyActions() throws {
        let a = try agent()
        #expect(AgentCommandViewModel.actions(for: a).isEmpty)
    }

    // MARK: - Mutations

    @Test("End session posts to the session's end route and reports success")
    func endSession() async throws {
        let mock = MockAPIClient()
        mock.endSessionResponse = SessionEndResponse(ended: true, sessionId: "sess-1")
        let vm = AgentCommandViewModel(
            agent: try agent(sessionId: "sess-1", sessionStatus: "active"),
            apiClient: mock)

        let ok = await vm.endSession()

        #expect(ok)
        #expect(mock.lastEndSession?.id == "sess-1")
        #expect(vm.didMutate)
        #expect(vm.resultMessage == "Session ended.")
        #expect(vm.errorMessage == nil)
    }

    @Test("Send message trims the text and targets the spawn's message route")
    func sendMessage() async throws {
        let mock = MockAPIClient()
        mock.spawnControlAckResponse = SpawnControlResponse(
            spawnId: "spawn-1", timestamp: "2026-07-28T10:00:00Z")
        let vm = AgentCommandViewModel(
            agent: try agent(spawnId: "spawn-1", spawnStatus: "running"),
            apiClient: mock)

        let ok = await vm.sendMessage("  focus on the failing test  ")

        #expect(ok)
        #expect(mock.lastSpawnMessage?.id == "spawn-1")
        #expect(mock.lastSpawnMessage?.text == "focus on the failing test")
        #expect(vm.didMutate)
    }

    @Test("Whitespace-only message is refused without a network call")
    func emptyMessageRefused() async throws {
        let mock = MockAPIClient()
        let vm = AgentCommandViewModel(
            agent: try agent(spawnId: "spawn-1", spawnStatus: "running"),
            apiClient: mock)

        let ok = await vm.sendMessage("   ")

        #expect(!ok)
        #expect(mock.lastSpawnMessage == nil)
        #expect(!vm.didMutate)
    }

    @Test("Interrupt and stop target their spawn routes")
    func interruptAndStop() async throws {
        let mock = MockAPIClient()
        mock.spawnControlAckResponse = SpawnControlResponse(
            spawnId: "spawn-1", timestamp: "2026-07-28T10:00:00Z")
        mock.spawnStopAckResponse = SpawnStopAck(stopped: true, spawnId: "spawn-1")
        let vm = AgentCommandViewModel(
            agent: try agent(spawnId: "spawn-1", spawnStatus: "running"),
            apiClient: mock)

        #expect(await vm.interruptSpawn())
        #expect(mock.lastSpawnInterrupt == "spawn-1")

        #expect(await vm.stopSpawn())
        #expect(mock.lastSpawnStop == "spawn-1")
        #expect(vm.resultMessage == "Spawn stopped.")
    }

    @Test("A failed mutation surfaces the error and never flags didMutate")
    func failureSurfacesError() async throws {
        let mock = MockAPIClient()
        mock.shouldFail = true
        mock.failError = .apiError(code: .unauthorized, message: "no", requestId: "r")
        let vm = AgentCommandViewModel(
            agent: try agent(sessionId: "sess-1", sessionStatus: "active"),
            apiClient: mock)

        let ok = await vm.endSession()

        #expect(!ok)
        #expect(!vm.didMutate)
        #expect(vm.errorMessage != nil)
        #expect(vm.resultMessage == nil)
    }

    @Test("Session-less agent refuses endSession without a network call")
    func sessionlessEndRefused() async throws {
        let mock = MockAPIClient()
        let vm = AgentCommandViewModel(agent: try agent(), apiClient: mock)
        #expect(await vm.endSession() == false)
        #expect(mock.lastEndSession == nil)
    }
}
