// Mills control-plane client: the Spinning Room + the Plan Store board.
//
// Splits from `MillsAPI` (the read-only KPI/pipeline surface) because this
// side carries MUTATIONS. Reads keep the same degradation contract as
// MillsAPI (bare 502/503/404 → calm empty state, see MillsAPI.swift); the
// mutations do NOT swallow errors — a failed spin or advance must surface,
// and a 401 specifically means "the paired bearer isn't the HUD admin token".

import Foundation

/// Control surface for the Spinning Room + Plans board. ViewModels and
/// previews depend on the protocol so test fakes can short-circuit network.
public protocol MillsControlAPIProtocol: Sendable {
    /// Live Spinning-Room policy: enabled/available + the frame catalog.
    /// nil = operator absent/unreachable (calm "room offline" state).
    func spinningRoom() async throws -> MillsSpinningRoom?

    /// Recent async spins, newest first. Operator absent → [].
    func spinRuns(limit: Int) async throws -> [MillsSpinRun]

    /// One spin by id, or nil while the row isn't visible yet (poll on).
    func spinRun(id: String) async throws -> MillsSpinRun?

    /// Fire an async spin (202). Returns the spin_id to poll. Throws on
    /// rejection — including 401 when the pairing token isn't admin.
    func spinAsync(_ request: MillsSpinRequest) async throws -> String

    /// The plan board. `available == false` = daemon predates the plan store.
    func plans() async throws -> MillsPlanList

    /// One plan with slices, or nil when unavailable.
    func plan(id: String) async throws -> MillsPlan?

    /// Advance a plan's lifecycle phase. Throws on an illegal transition
    /// (HTTP 422) or auth failure.
    func advancePlan(id: String, toPhase: String) async throws

    /// Escalate a running pipeline out of the autonomous loop and hold it for
    /// human review. Returns the run's resulting state. Throws on a missing
    /// run (404) or auth failure (401). The operator's pause/resume are still
    /// unimplemented (501), so this is the only real per-run intervention.
    @discardableResult
    func escalatePipeline(id: String, reason: String?) async throws -> MillsPipelineEscalateAck
}

/// Operator response to POST …/pipeline/runs/{id}/escalate.
public struct MillsPipelineEscalateAck: Codable, Sendable, Hashable {
    public let runID: String
    public let backlogID: String?
    public let state: String
    public let reason: String?

    enum CodingKeys: String, CodingKey {
        case runID = "run_id"
        case backlogID = "backlog_id"
        case state
        case reason
    }

    public init(runID: String, backlogID: String? = nil, state: String, reason: String? = nil) {
        self.runID = runID
        self.backlogID = backlogID
        self.state = state
        self.reason = reason
    }
}

public struct MillsControlAPI: MillsControlAPIProtocol, Sendable {
    private let client: LoomAPIClientProtocol

    public init(client: LoomAPIClientProtocol) {
        self.client = client
    }

    public func spinningRoom() async throws -> MillsSpinningRoom? {
        do {
            let room: MillsSpinningRoom = try await client.requestRaw(.millsSpinningRoomFrames)
            return room
        } catch let LoomAPIError.apiError(code, _, _)
            where code == .notFound || code == .notConfigured || code == .upstreamError {
            return nil
        }
    }

    public func spinRuns(limit: Int = 20) async throws -> [MillsSpinRun] {
        do {
            let runs: [MillsSpinRun]? = try await client.requestRaw(.millsSpinRuns(limit: limit))
            return runs ?? []
        } catch let LoomAPIError.apiError(code, _, _)
            where code == .notFound || code == .notConfigured || code == .upstreamError {
            // 404 also covers an operator image that predates async spins.
            return []
        }
    }

    public func spinRun(id: String) async throws -> MillsSpinRun? {
        do {
            let run: MillsSpinRun = try await client.requestRaw(.millsSpinRun(id: id))
            return run
        } catch let LoomAPIError.apiError(code, _, _) where code == .notFound {
            // The background goroutine's row may not be visible yet — poll on.
            return nil
        }
    }

    public func spinAsync(_ request: MillsSpinRequest) async throws -> String {
        let queued: MillsSpinQueued = try await client.requestRaw(.millsSpinAsync(request: request))
        return queued.spinID
    }

    public func plans() async throws -> MillsPlanList {
        do {
            return try await client.requestRaw(.plans)
        } catch let LoomAPIError.apiError(code, _, _)
            where code == .notFound || code == .notConfigured || code == .upstreamError {
            // Older HUD without the plans domain → deploy-pending state.
            return MillsPlanList(available: false)
        }
    }

    public func plan(id: String) async throws -> MillsPlan? {
        do {
            let detail: MillsPlanDetail = try await client.requestRaw(.planDetail(id: id))
            return detail.plan
        } catch let LoomAPIError.apiError(code, _, _) where code == .notFound {
            return nil
        }
    }

    public func advancePlan(id: String, toPhase: String) async throws {
        let _: MillsPlanAdvanceAck = try await client.requestRaw(
            .planAdvance(id: id, toPhase: toPhase)
        )
    }

    @discardableResult
    public func escalatePipeline(id: String, reason: String? = nil) async throws -> MillsPipelineEscalateAck {
        try await client.requestRaw(.millsPipelineEscalate(id: id, reason: reason))
    }
}

/// Human-readable failure line for a control-plane mutation, folding the
/// admin-token case into actionable copy.
public func millsMutationFailureMessage(_ error: Error) -> String {
    if let apiError = error as? LoomAPIError {
        if case let .apiError(code, message, _) = apiError {
            switch code {
            case .unauthorized, .forbidden, .tokenRevoked:
                return "Needs the HUD admin token — repair with the admin token to spin from here."
            case .notFound:
                return "The HUD is still deploying this feature — try again shortly."
            default:
                return message.isEmpty ? apiError.description : message
            }
        }
        return apiError.description
    }
    return error.localizedDescription
}
