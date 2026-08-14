import Foundation

// Wire models for the HUD's server-side alert store and auto-fix engine.
//
// Source of truth: `internal/hud/domain/alerting/alerting.go` (routes),
// `internal/hud/domain/alerting/handlers.go` (response shapes),
// `internal/hud/alerting/types.go` and `internal/hud/autofix/autofix.go`
// (the encoded structs).
//
// IMPORTANT: these five `/api/mobile/v1` routes live in the *alerting* domain,
// not the `mobile` domain, so they are written with the HUD's plain
// `writeJSON` — they are **bare JSON**, NOT the `APIEnvelope` every other
// `/api/mobile/v1` route uses. Callers must go through
// `LoomAPIClientProtocol.requestRaw`, exactly like the Mills/Weaver proxy
// reads. Errors come back as `{"error": "..."}` and fall through to
// `APIClient.mapHTTPError`'s status-code mapping.

// MARK: - Alert store

/// One fired alert from the HUD alert engine
/// (`internal/hud/alerting.Alert`).
///
/// The store is an in-memory ring buffer capped at 200 entries
/// (`maxAlertHistory`) that resets when the HUD restarts — it is *history
/// across app launches*, not a durable audit log.
public struct ServerAlert: Decodable, Identifiable, Sendable, Equatable {
    public let id: String
    public let ruleId: String
    public let ruleName: String
    /// `critical` | `warning` | `info` (free-form server-side).
    public let severity: String
    public let title: String
    public let message: String
    public let pipeline: ServerAlertPipeline
    public let firedAt: Date
    /// Set once someone acknowledges the alert. Acking never removes the alert
    /// from the store; it stamps `acked_at`/`acked_by` in place.
    public let ackedAt: Date?
    public let ackedBy: String?
    public let resolvedAt: Date?
    public let autofixId: String?

    enum CodingKeys: String, CodingKey {
        case id
        case ruleId = "rule_id"
        case ruleName = "rule_name"
        case severity, title, message, pipeline
        case firedAt = "fired_at"
        case ackedAt = "acked_at"
        case ackedBy = "acked_by"
        case resolvedAt = "resolved_at"
        case autofixId = "autofix_id"
    }

    public init(
        id: String,
        ruleId: String = "",
        ruleName: String = "",
        severity: String,
        title: String,
        message: String,
        pipeline: ServerAlertPipeline = ServerAlertPipeline(),
        firedAt: Date = Date(),
        ackedAt: Date? = nil,
        ackedBy: String? = nil,
        resolvedAt: Date? = nil,
        autofixId: String? = nil
    ) {
        self.id = id
        self.ruleId = ruleId
        self.ruleName = ruleName
        self.severity = severity
        self.title = title
        self.message = message
        self.pipeline = pipeline
        self.firedAt = firedAt
        self.ackedAt = ackedAt
        self.ackedBy = ackedBy
        self.resolvedAt = resolvedAt
        self.autofixId = autofixId
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        ruleId = try c.decodeIfPresent(String.self, forKey: .ruleId) ?? ""
        ruleName = try c.decodeIfPresent(String.self, forKey: .ruleName) ?? ""
        severity = try c.decodeIfPresent(String.self, forKey: .severity) ?? "info"
        title = try c.decodeIfPresent(String.self, forKey: .title) ?? ""
        message = try c.decodeIfPresent(String.self, forKey: .message) ?? ""
        pipeline = try c.decodeIfPresent(ServerAlertPipeline.self, forKey: .pipeline)
            ?? ServerAlertPipeline()
        firedAt = try c.decodeIfPresent(Date.self, forKey: .firedAt) ?? Date()
        ackedAt = try c.decodeIfPresent(Date.self, forKey: .ackedAt)
        ackedBy = try c.decodeIfPresent(String.self, forKey: .ackedBy)
        resolvedAt = try c.decodeIfPresent(Date.self, forKey: .resolvedAt)
        autofixId = try c.decodeIfPresent(String.self, forKey: .autofixId)
    }

    public var isAcked: Bool { ackedAt != nil }
}

/// Lightweight pipeline reference embedded in a `ServerAlert`
/// (`internal/hud/alerting.PipelineRef`).
public struct ServerAlertPipeline: Decodable, Sendable, Equatable {
    public let id: Int
    public let project: String
    public let ref: String
    public let status: String
    public let url: String?

    enum CodingKeys: String, CodingKey {
        case id, project, ref, status, url
    }

    public init(
        id: Int = 0,
        project: String = "",
        ref: String = "",
        status: String = "",
        url: String? = nil
    ) {
        self.id = id
        self.project = project
        self.ref = ref
        self.status = status
        self.url = url
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decodeIfPresent(Int.self, forKey: .id) ?? 0
        project = try c.decodeIfPresent(String.self, forKey: .project) ?? ""
        ref = try c.decodeIfPresent(String.self, forKey: .ref) ?? ""
        status = try c.decodeIfPresent(String.self, forKey: .status) ?? ""
        url = try c.decodeIfPresent(String.self, forKey: .url)
    }

    /// Short "project #id" label, or nil when the alert carries no pipeline.
    public var label: String? {
        guard !project.isEmpty || id > 0 else { return nil }
        if project.isEmpty { return "#\(id)" }
        return id > 0 ? "\(project) #\(id)" : project
    }
}

/// `GET /api/mobile/v1/alerts` → `{"alerts": [...]}` (newest first).
public struct ServerAlertsResponse: Decodable, Sendable {
    public let alerts: [ServerAlert]

    enum CodingKeys: String, CodingKey { case alerts }

    public init(alerts: [ServerAlert]) { self.alerts = alerts }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        alerts = try c.decodeIfPresent([ServerAlert].self, forKey: .alerts) ?? []
    }
}

/// `POST /api/mobile/v1/alerts/{id}/ack` → `{"acked": true, "id": "..."}`.
public struct AlertAckResponse: Decodable, Sendable {
    public let acked: Bool
    public let id: String

    public init(acked: Bool, id: String) {
        self.acked = acked
        self.id = id
    }
}

// MARK: - Auto-fix

/// A proposed fix generated from an LLM diagnosis
/// (`internal/hud/autofix.AutoFixProposal`).
public struct AutofixProposal: Decodable, Identifiable, Sendable, Equatable {
    public let id: String
    public let diagnosisId: String
    public let description: String
    /// `agent_fix` | `retry` | `manual` — see `AutofixStrategy`.
    public let strategy: String
    public let estimatedFiles: [String]
    public let confidence: Double
    public let requiresApproval: Bool
    public let createdAt: Date

    enum CodingKeys: String, CodingKey {
        case id
        case diagnosisId = "diagnosis_id"
        case description, strategy
        case estimatedFiles = "estimated_files"
        case confidence
        case requiresApproval = "requires_approval"
        case createdAt = "created_at"
    }

    public init(
        id: String,
        diagnosisId: String = "",
        description: String = "",
        strategy: String = "manual",
        estimatedFiles: [String] = [],
        confidence: Double = 0,
        requiresApproval: Bool = true,
        createdAt: Date = Date()
    ) {
        self.id = id
        self.diagnosisId = diagnosisId
        self.description = description
        self.strategy = strategy
        self.estimatedFiles = estimatedFiles
        self.confidence = confidence
        self.requiresApproval = requiresApproval
        self.createdAt = createdAt
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        diagnosisId = try c.decodeIfPresent(String.self, forKey: .diagnosisId) ?? ""
        description = try c.decodeIfPresent(String.self, forKey: .description) ?? ""
        strategy = try c.decodeIfPresent(String.self, forKey: .strategy) ?? "manual"
        // Go emits `null` (not `[]`) for a nil slice — the field has no omitempty.
        estimatedFiles = try c.decodeIfPresent([String].self, forKey: .estimatedFiles) ?? []
        confidence = try c.decodeIfPresent(Double.self, forKey: .confidence) ?? 0
        requiresApproval = try c.decodeIfPresent(Bool.self, forKey: .requiresApproval) ?? true
        createdAt = try c.decodeIfPresent(Date.self, forKey: .createdAt) ?? Date()
    }

    public var kind: AutofixStrategy { AutofixStrategy(rawValue: strategy) }
}

/// The three execution strategies `ProposeAutoFix` can pick, plus the honest
/// description of what approving each one actually does today.
///
/// Source: `(*AutoFixEngine).ExecuteAutoFix` in
/// `internal/hud/autofix/autofix.go`.
public enum AutofixStrategy: Sendable, Equatable {
    /// Spawns a headless `claude-code` agent to attempt the fix. Fails
    /// immediately with "no spawn orchestrator available" when the HUD has no
    /// spawner wired.
    case agentFix
    /// **No-op placeholder.** The engine marks the execution `succeeded` with
    /// result "pipeline retry requested" without ever re-running the pipeline —
    /// the GitLab re-run is still a TODO. Approving one changes nothing in CI.
    case retry
    /// Anything else. Execution completes immediately as `failed` with
    /// "manual intervention required".
    case manual(String)

    public init(rawValue: String) {
        switch rawValue {
        case "agent_fix": self = .agentFix
        case "retry": self = .retry
        default: self = .manual(rawValue)
        }
    }

    public var rawValue: String {
        switch self {
        case .agentFix: return "agent_fix"
        case .retry: return "retry"
        case let .manual(raw): return raw.isEmpty ? "manual" : raw
        }
    }

    /// Short label for the strategy pill.
    public var label: String {
        switch self {
        case .agentFix: return "AGENT FIX"
        case .retry: return "RETRY"
        case let .manual(raw): return raw.isEmpty ? "MANUAL" : raw.uppercased()
        }
    }

    /// What approving actually does. Deliberately blunt about the `retry`
    /// placeholder so the operator is not told a pipeline was re-run when it
    /// was not.
    public var approveEffect: String {
        switch self {
        case .agentFix:
            return "Spawns a headless claude-code agent to attempt the fix. "
                + "If the HUD has no spawn orchestrator the execution fails immediately."
        case .retry:
            return "Records a \"pipeline retry requested\" execution only. "
                + "The HUD's retry strategy is still a placeholder — no pipeline is actually re-run."
        case .manual:
            return "Records a completed execution marked \"manual intervention required\". "
                + "No automated work is performed."
        }
    }

    /// True when approving demonstrably does nothing to CI, so the UI can warn
    /// before the operator burns a decision on it.
    public var isNoOp: Bool {
        switch self {
        case .agentFix: return false
        case .retry, .manual: return true
        }
    }
}

/// `GET /api/mobile/v1/autofix/proposals` → `{"proposals": [...]}`
/// (newest first).
public struct AutofixProposalsResponse: Decodable, Sendable {
    public let proposals: [AutofixProposal]

    enum CodingKeys: String, CodingKey { case proposals }

    public init(proposals: [AutofixProposal]) { self.proposals = proposals }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        proposals = try c.decodeIfPresent([AutofixProposal].self, forKey: .proposals) ?? []
    }
}

/// One auto-fix execution record (`internal/hud/autofix.AutoFixExecution`).
public struct AutofixExecution: Decodable, Identifiable, Sendable, Equatable {
    public let id: String
    public let proposalId: String
    /// `running` | `succeeded` | `failed` | `rejected` | `pending_approval`.
    public let status: String
    public let agentId: String?
    public let spawnId: String?
    public let result: String?
    public let startedAt: Date
    public let completedAt: Date?

    enum CodingKeys: String, CodingKey {
        case id
        case proposalId = "proposal_id"
        case status
        case agentId = "agent_id"
        case spawnId = "spawn_id"
        case result
        case startedAt = "started_at"
        case completedAt = "completed_at"
    }

    public init(
        id: String,
        proposalId: String,
        status: String,
        agentId: String? = nil,
        spawnId: String? = nil,
        result: String? = nil,
        startedAt: Date = Date(),
        completedAt: Date? = nil
    ) {
        self.id = id
        self.proposalId = proposalId
        self.status = status
        self.agentId = agentId
        self.spawnId = spawnId
        self.result = result
        self.startedAt = startedAt
        self.completedAt = completedAt
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        proposalId = try c.decodeIfPresent(String.self, forKey: .proposalId) ?? ""
        status = try c.decodeIfPresent(String.self, forKey: .status) ?? "unknown"
        agentId = try c.decodeIfPresent(String.self, forKey: .agentId)
        spawnId = try c.decodeIfPresent(String.self, forKey: .spawnId)
        result = try c.decodeIfPresent(String.self, forKey: .result)
        startedAt = try c.decodeIfPresent(Date.self, forKey: .startedAt) ?? Date()
        completedAt = try c.decodeIfPresent(Date.self, forKey: .completedAt)
    }
}

/// `POST /api/mobile/v1/autofix/proposals/{id}/approve` → HTTP 202 with
/// `{"execution": {...}}`.
public struct AutofixApproveResponse: Decodable, Sendable {
    public let execution: AutofixExecution

    public init(execution: AutofixExecution) { self.execution = execution }
}

/// `POST /api/mobile/v1/autofix/proposals/{id}/reject` →
/// `{"rejected": true, "proposal_id": "..."}`.
public struct AutofixRejectResponse: Decodable, Sendable {
    public let rejected: Bool
    public let proposalId: String

    enum CodingKeys: String, CodingKey {
        case rejected
        case proposalId = "proposal_id"
    }

    public init(rejected: Bool, proposalId: String) {
        self.rejected = rejected
        self.proposalId = proposalId
    }
}
