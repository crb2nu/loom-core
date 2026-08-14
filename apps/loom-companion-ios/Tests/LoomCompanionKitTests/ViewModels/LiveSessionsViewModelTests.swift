import Foundation
import Testing
@testable import LoomCompanionKit

@Suite("LiveSessionsViewModel")
@MainActor
struct LiveSessionsViewModelTests {
    @Test("Active session snapshot and context activity use one session identity")
    func seedsActiveSessionWithContext() async throws {
        let client = MockAPIClient()
        client.sessionsResponse = SessionsResponse(sessions: [
            SessionInfo(
                id: "session-1",
                agentId: "codex-1",
                namespace: "loom-core/session-parity",
                status: .active,
                description: "Align mobile and web",
                startedAt: "2026-07-14T18:00:00Z",
                entryCount: 3,
                totalTokens: 120
            ),
        ])
        client.sessionEventsResponse = SessionEventsResponse(
            sessionId: "session-1",
            events: [
                TimelineEntry(
                    timestamp: "2026-07-14T18:01:00Z",
                    eventType: "finding",
                    agentId: "codex-1",
                    data: [
                        "session_id": .string("session-1"),
                        "entry_type": .string("finding"),
                        "title": .string("Context is linked"),
                    ]
                ),
            ]
        )

        let viewModel = LiveSessionsViewModel(apiClient: client)
        await viewModel.loadInitialSessions()

        let session = try #require(viewModel.sessionsByID["session-1"])
        #expect(session.namespace == "loom-core/session-parity")
        #expect(session.description == "Align mobile and web")
        #expect(session.recentCalls.count == 1)
        #expect(session.recentCalls[0].source == "context")
        #expect(session.recentCalls[0].resultSummary == "Context is linked")
    }

    @Test("Agent-context lifecycle aliases populate the live session")
    func acceptsAgentContextLifecycleAlias() throws {
        let viewModel = LiveSessionsViewModel()

        viewModel.handle(event(
            type: "agent.session.bootstrap",
            data: #"{"session_id":"session-2","agent_id":"claude-1","namespace":"loom-core/mobile"}"#
        ))

        let session = try #require(viewModel.sessionsByID["session-2"])
        #expect(session.agentID == "claude-1")
        #expect(session.namespace == "loom-core/mobile")
    }

    @Test("Agent status accepts publisher new_status payload")
    func acceptsNewStatus() throws {
        let viewModel = LiveSessionsViewModel()
        viewModel.handle(event(
            type: "session.start",
            data: #"{"session_id":"session-3","agent_id":"codex-3"}"#
        ))
        viewModel.handle(event(
            type: "agent.status.change",
            data: #"{"agent_id":"codex-3","new_status":"idle"}"#
        ))

        #expect(viewModel.sessionsByID["session-3"]?.agentStatus == .idle)
    }

    @Test("Periodic snapshot reconciliation closes a session when its end event was missed")
    func reconcilesMissedSessionEnd() async throws {
        let client = MockAPIClient()
        client.sessionsResponse = SessionsResponse(sessions: [
            SessionInfo(
                id: "stale-session",
                agentId: "codex-stale",
                namespace: "loom-core/stale",
                status: .active,
                description: "Session whose end event was missed",
                startedAt: "2026-07-14T18:00:00Z",
                entryCount: 0,
                totalTokens: 0
            ),
        ])

        let broadcaster = SSEEventBroadcaster()
        let viewModel = LiveSessionsViewModel(
            apiClient: client,
            snapshotRefreshInterval: .milliseconds(20)
        )
        viewModel.subscribe(to: broadcaster)
        defer { viewModel.unsubscribe() }

        for _ in 0..<100 where viewModel.sessionsByID["stale-session"] == nil {
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(viewModel.sessionsByID["stale-session"]?.endedAt == nil)

        client.sessionsResponse = SessionsResponse(sessions: [])
        for _ in 0..<100 where viewModel.sessionsByID["stale-session"]?.endedAt == nil {
            try await Task.sleep(for: .milliseconds(10))
        }

        #expect(viewModel.sessionsByID["stale-session"]?.endedAt != nil)
        #expect(client.requestCount(for: .sessions(status: "active")) >= 2)
    }
}

private func event(type: String, data: String) -> SSEEvent {
    SSEEvent(
        type: type,
        data: """
        {"id":"event-1","type":"\(type)","timestamp":"2026-07-14T18:00:00Z","data":\(data)}
        """
    )
}
