import Foundation

/// Acknowledgement from `POST /api/mobile/v1/agent/spawn/{id}/stop`.
///
/// Public (rather than a caller-private struct like SpawnViewModel's) so the
/// command sheet's stop path can be exercised by unit tests through
/// `MockAPIClient` without a live HUD.
public struct SpawnStopAck: Decodable, Sendable {
    public let stopped: Bool
    public let spawnId: String

    enum CodingKeys: String, CodingKey {
        case stopped
        case spawnId = "spawn_id"
    }

    public init(stopped: Bool, spawnId: String) {
        self.stopped = stopped
        self.spawnId = spawnId
    }
}

/// Drives the Operator deck's per-agent command sheet: which controls a
/// roster row legitimately offers, and the mutations behind them.
///
/// Action derivation is pure and static so it is testable without a view or
/// network — the sheet must never render a control the HUD would refuse
/// (e.g. spawn controls for a CLI agent, or end-session for a session that
/// already ended).
@Observable
public final class AgentCommandViewModel {
    /// Controls the command sheet can offer for one agent.
    public enum Action: String, CaseIterable, Identifiable, Sendable {
        /// Navigate to the live session detail (view-only; routing is the
        /// sheet's job, not an API call).
        case viewSession
        /// End the agent's active session (`POST /sessions/{id}/end`).
        case endSession
        /// Queue a follow-up message on a live multi-turn spawn.
        case message
        /// Abort the spawn's in-flight turn, keeping the pod alive.
        case interrupt
        /// Stop the spawn pod entirely.
        case stop

        public var id: String { rawValue }
    }

    public let agent: UnifiedAgent
    public private(set) var isBusy = false
    public private(set) var resultMessage: String?
    public private(set) var errorMessage: String?
    /// Flips true after any successful mutation so the presenting deck knows
    /// to refresh its roster snapshot when the sheet closes.
    public private(set) var didMutate = false

    @ObservationIgnored
    private let apiClient: any LoomAPIClientProtocol

    public init(agent: UnifiedAgent, apiClient: any LoomAPIClientProtocol) {
        self.agent = agent
        self.apiClient = apiClient
    }

    // MARK: - Action derivation

    /// Spawn states whose message/interrupt/stop routes the HUD accepts —
    /// mirrors `SpawnInfo.isActive` ("creating" | "running"). A completed or
    /// failed spawn keeps its roster row for a while; offering controls there
    /// would just surface 4xx errors.
    private static let controllableSpawnStatuses: Set<String> = ["creating", "running"]

    public var actions: [Action] { Self.actions(for: agent) }

    public static func actions(for agent: UnifiedAgent) -> [Action] {
        var actions: [Action] = []
        if let sessionId = agent.sessionId, !sessionId.isEmpty {
            actions.append(.viewSession)
            if agent.sessionStatus?.lowercased() == "active" {
                actions.append(.endSession)
            }
        }
        if let spawnId = agent.spawnId, !spawnId.isEmpty,
           controllableSpawnStatuses.contains((agent.spawnStatus ?? "").lowercased()) {
            actions.append(contentsOf: [.message, .interrupt, .stop])
        }
        return actions
    }

    // MARK: - Mutations

    /// End the agent's active session. Returns true on success.
    @discardableResult
    public func endSession(summarize: Bool = true) async -> Bool {
        guard let sessionId = agent.sessionId, !sessionId.isEmpty else { return false }
        return await perform {
            let response: SessionEndResponse = try await self.apiClient.request(
                .endSession(id: sessionId, summarize: summarize))
            return response.ended
                ? "Session ended."
                : "The HUD did not confirm the end — check the roster."
        }
    }

    /// Queue a follow-up message on the live spawn. Returns true on success
    /// so the sheet can clear its input field.
    @discardableResult
    public func sendMessage(_ text: String) async -> Bool {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let spawnId = agent.spawnId, !spawnId.isEmpty, !trimmed.isEmpty else { return false }
        return await perform {
            let _: SpawnControlResponse = try await self.apiClient.request(
                .spawnSendMessage(id: spawnId, text: trimmed))
            return "Message queued for \(self.agent.agentId)."
        }
    }

    /// Abort the spawn's in-flight turn. Returns true on success.
    @discardableResult
    public func interruptSpawn() async -> Bool {
        guard let spawnId = agent.spawnId, !spawnId.isEmpty else { return false }
        return await perform {
            let _: SpawnControlResponse = try await self.apiClient.request(
                .spawnInterrupt(id: spawnId))
            return "Interrupt sent — the current turn is being aborted."
        }
    }

    /// Stop the spawn pod. Returns true on success.
    @discardableResult
    public func stopSpawn() async -> Bool {
        guard let spawnId = agent.spawnId, !spawnId.isEmpty else { return false }
        return await perform {
            let response: SpawnStopAck = try await self.apiClient.request(.spawnStop(id: spawnId))
            return response.stopped
                ? "Spawn stopped."
                : "The HUD did not confirm the stop — check the Spawn tab."
        }
    }

    /// Shared mutation wrapper: single-flight busy latch, success copy on the
    /// result lane, `LoomAPIError` descriptions on the error lane.
    private func perform(_ mutation: () async throws -> String) async -> Bool {
        guard !isBusy else { return false }
        isBusy = true
        errorMessage = nil
        resultMessage = nil
        defer { isBusy = false }
        do {
            resultMessage = try await mutation()
            didMutate = true
            return true
        } catch let error as LoomAPIError {
            errorMessage = error.description
            return false
        } catch {
            errorMessage = error.localizedDescription
            return false
        }
    }
}
