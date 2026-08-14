import Foundation

/// Severity level for in-app alert notifications.
public enum AlertSeverity: String, Sendable, Comparable, Codable {
    case info
    case warning
    case critical

    public static func < (lhs: AlertSeverity, rhs: AlertSeverity) -> Bool {
        let order: [AlertSeverity] = [.info, .warning, .critical]
        return order.firstIndex(of: lhs)! < order.firstIndex(of: rhs)!
    }
}

/// Maps to iOS UNNotificationInterruptionLevel for future APNs integration.
/// Controls how prominently an alert interrupts the user.
public enum InterruptionLevel: String, Sendable, Codable, Comparable {
    /// Silently added to notification list; no sound/banner.
    case passive
    /// Default notification behavior (sound + banner).
    case active
    /// Breaks through Focus/DND for time-critical operational events.
    case timeSensitive = "time_sensitive"
    /// Reserved for emergencies requiring immediate attention (not used in v1).
    case critical

    public static func < (lhs: InterruptionLevel, rhs: InterruptionLevel) -> Bool {
        let order: [InterruptionLevel] = [.passive, .active, .timeSensitive, .critical]
        return order.firstIndex(of: lhs)! < order.firstIndex(of: rhs)!
    }
}

/// Safe navigation actions available from an alert. Restricted to read-only
/// operations to maintain v1 scope discipline.
public enum AlertAction: String, Sendable, Codable {
    /// Navigate to the related session detail screen.
    case viewSession = "view_session"
    /// Navigate to the related workflow detail screen.
    case viewWorkflow = "view_workflow"
    /// Navigate to the dashboard screen.
    case viewDashboard = "view_dashboard"
    /// Mark as acknowledged (no navigation).
    case acknowledge
}

/// Where an ``AlertItem`` came from, which decides whether its read state is
/// purely local or is backed by a server-side acknowledgement.
public enum AlertOrigin: String, Sendable, Codable {
    /// Derived from a live SSE event that has no counterpart in the HUD alert
    /// store (session lifecycle, nudges, handoffs, health). Read state is local
    /// to this app install and dies with the process.
    case stream
    /// Backed by an entry in the HUD alert store
    /// (`GET /api/mobile/v1/alerts`). Its `id` IS the server alert id, and
    /// "read" means the server-side ack (`acked_at`).
    case server
}

/// A single in-app alert — either derived from an SSE event or mirrored from
/// the HUD's server-side alert store.
///
/// `id` is a `String` (not a `UUID`) because server alerts carry the HUD's own
/// id (`alert-<rule>-<ts>`), which is what `POST /alerts/{id}/ack` and the
/// `loom://alert/<id>` deep link address. Stream-only alerts get a generated
/// UUID string so the two kinds can share one list and dedupe by id.
public struct AlertItem: Identifiable, Sendable {
    public let id: String
    public let timestamp: Date
    public let severity: AlertSeverity
    public let interruptionLevel: InterruptionLevel
    public let title: String
    public let message: String
    public let eventType: String
    public let relatedSessionId: String?
    public let relatedWorkflowId: String?
    public let allowedActions: [AlertAction]
    /// Read state. For `.server` alerts this is the local projection of the
    /// server ack — see ``AlertsViewModel`` for the reconciliation rules.
    public var isRead: Bool

    /// Origin — decides whether marking read performs a server ack.
    public let origin: AlertOrigin
    /// Server-side acknowledgement stamp, mirrored from `acked_at`. Always nil
    /// for `.stream` alerts.
    public var ackedAt: Date?
    /// Who acked it server-side (`acked_by`).
    public var ackedBy: String?
    /// Pipeline the alert fired against, when the alert came from the store.
    public let pipeline: ServerAlertPipeline?

    public init(
        id: String = UUID().uuidString,
        timestamp: Date = Date(),
        severity: AlertSeverity,
        interruptionLevel: InterruptionLevel = .active,
        title: String,
        message: String,
        eventType: String,
        relatedSessionId: String? = nil,
        relatedWorkflowId: String? = nil,
        allowedActions: [AlertAction] = [.acknowledge],
        isRead: Bool = false,
        origin: AlertOrigin = .stream,
        ackedAt: Date? = nil,
        ackedBy: String? = nil,
        pipeline: ServerAlertPipeline? = nil
    ) {
        self.id = id
        self.timestamp = timestamp
        self.severity = severity
        self.interruptionLevel = interruptionLevel
        self.title = title
        self.message = message
        self.eventType = eventType
        self.relatedSessionId = relatedSessionId
        self.relatedWorkflowId = relatedWorkflowId
        self.allowedActions = allowedActions
        self.isRead = isRead
        self.origin = origin
        self.ackedAt = ackedAt
        self.ackedBy = ackedBy
        self.pipeline = pipeline
    }

    /// Mirror one entry of the HUD alert store.
    ///
    /// Read state is taken straight from the server ack so the app never holds
    /// a read flag that contradicts `acked_at`.
    public init(serverAlert alert: ServerAlert) {
        let severity = AlertSeverity(serverSeverity: alert.severity)
        self.init(
            id: alert.id,
            timestamp: alert.firedAt,
            severity: severity,
            interruptionLevel: severity.defaultInterruptionLevel,
            title: alert.title.isEmpty ? alert.ruleName : alert.title,
            message: alert.message,
            eventType: "pipeline.alert",
            allowedActions: [.acknowledge],
            isRead: alert.isAcked,
            origin: .server,
            ackedAt: alert.ackedAt,
            ackedBy: alert.ackedBy,
            pipeline: alert.pipeline
        )
    }

    /// True when marking this alert read must round-trip to the HUD.
    public var isServerBacked: Bool { origin == .server }

    /// The primary quick-action for this alert (first non-acknowledge action, or acknowledge).
    public var primaryAction: AlertAction {
        allowedActions.first { $0 != .acknowledge } ?? .acknowledge
    }
}

public extension AlertSeverity {
    /// Map the HUD's free-form severity string onto the app's enum. Unknown
    /// values degrade to `.info` rather than dropping the alert.
    init(serverSeverity: String) {
        switch serverSeverity.lowercased() {
        case "critical", "fatal", "error":
            self = .critical
        case "warning", "warn", "degraded":
            self = .warning
        default:
            self = .info
        }
    }

    /// Interruption level a server alert of this severity is presented at.
    var defaultInterruptionLevel: InterruptionLevel {
        switch self {
        case .critical: return .timeSensitive
        case .warning: return .active
        case .info: return .passive
        }
    }
}

/// Policy entry describing how an SSE event type maps to an alert.
public struct NotificationPolicyEntry: Sendable {
    public let severity: AlertSeverity
    public let interruptionLevel: InterruptionLevel
    public let titleTemplate: String
    public let allowedActions: [AlertAction]
}

/// Maps SSE event types to notification severity, interruption level, and allowed actions.
///
/// Event-to-interruption-level matrix (MBL-6):
///
/// | Event Type               | Severity | Interruption   | Actions                        |
/// |--------------------------|----------|----------------|--------------------------------|
/// | hud.health (down)        | critical | timeSensitive  | viewDashboard, acknowledge     |
/// | hud.health (degraded)    | warning  | active         | viewDashboard, acknowledge     |
/// | agent.session.reaped     | warning  | active         | viewSession, acknowledge       |
/// | hud.workflow.reject      | warning  | active         | viewWorkflow, acknowledge      |
/// | agent.session.start      | info     | passive        | viewSession, acknowledge       |
/// | agent.session.end        | info     | passive        | viewSession, acknowledge       |
/// | agent.nudge.created      | info     | passive        | acknowledge                    |
/// | hud.workflow.approve     | info     | passive        | viewWorkflow, acknowledge      |
/// | hud.handoff.created      | info     | passive        | acknowledge                    |
/// | coordinator.plan.complete| info     | passive        | acknowledge                    |
public enum NotificationPolicy {
    /// Classify an SSE event into an AlertItem, or nil if the event is not alert-worthy.
    public static func classify(event: SSEEvent) -> AlertItem? {
        // `pipeline.alert` is the one event type that mirrors a record in the
        // HUD alert store: the dispatcher marshals `alerting.Alert` verbatim
        // (`internal/hud/alerting/dispatcher.go`). Decode it so the live event
        // and the `GET /alerts` history carry the SAME id and dedupe instead of
        // double-listing. A malformed payload falls through to the generic
        // policy path below rather than dropping the alert.
        if event.type == pipelineAlertEventType,
           let alert = decodeServerAlert(event.data) {
            return AlertItem(serverAlert: alert)
        }

        let entry = policyEntry(for: event)
        guard let entry else { return nil }

        let message = buildMessage(event: event)
        let sessionId = extractSessionId(from: event.data)
        let workflowId = extractWorkflowId(from: event.data)

        return AlertItem(
            severity: entry.severity,
            interruptionLevel: entry.interruptionLevel,
            title: entry.titleTemplate,
            message: message,
            eventType: event.type,
            relatedSessionId: sessionId,
            relatedWorkflowId: workflowId,
            allowedActions: entry.allowedActions
        )
    }

    /// SSE event type the HUD alert dispatcher broadcasts for a fired alert.
    public static let pipelineAlertEventType = "pipeline.alert"

    /// Decode the `alerting.Alert` payload carried by a `pipeline.alert` event.
    /// Public so ``AlertsViewModel`` and tests can exercise the same path.
    public static func decodeServerAlert(_ data: String) -> ServerAlert? {
        guard let jsonData = data.data(using: .utf8) else { return nil }
        return try? serverAlertDecoder.decode(ServerAlert.self, from: jsonData)
    }

    /// Go emits `time.Time` as RFC3339; parse with the shared ISO helper rather
    /// than `JSONDecoder`'s epoch-number default (which throws on every field).
    private static let serverAlertDecoder: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { d in
            let container = try d.singleValueContainer()
            let raw = try container.decode(String.self)
            if let date = LoomFormat.date(fromISO: raw) { return date }
            throw DecodingError.dataCorruptedError(
                in: container, debugDescription: "Expected ISO-8601 date, got \(raw)")
        }
        return decoder
    }()

    /// Look up the policy entry for a given SSE event, applying conditional logic for health events.
    public static func policyEntry(for event: SSEEvent) -> NotificationPolicyEntry? {
        switch event.type {
        case "hud.health":
            return classifyHealthEvent(data: event.data)
        case pipelineAlertEventType:
            // Fallback for a `pipeline.alert` whose payload didn't decode:
            // still surface it, tinted by whatever `severity` we can read.
            return classifyPipelineAlertEvent(data: event.data)
        case "agent.session.reaped":
            return NotificationPolicyEntry(severity: .warning, interruptionLevel: .active, titleTemplate: "Session Reaped", allowedActions: [.viewSession, .acknowledge])
        case "agent.nudge.created":
            return NotificationPolicyEntry(severity: .info, interruptionLevel: .passive, titleTemplate: "Agent Nudge Queued", allowedActions: [.acknowledge])
        case "hud.workflow.approve":
            return NotificationPolicyEntry(severity: .info, interruptionLevel: .passive, titleTemplate: "Workflow Approved", allowedActions: [.viewWorkflow, .acknowledge])
        case "hud.workflow.reject":
            return NotificationPolicyEntry(severity: .warning, interruptionLevel: .active, titleTemplate: "Workflow Rejected", allowedActions: [.viewWorkflow, .acknowledge])
        case "agent.session.start":
            return NotificationPolicyEntry(severity: .info, interruptionLevel: .passive, titleTemplate: "Session Started", allowedActions: [.viewSession, .acknowledge])
        case "agent.session.end":
            return NotificationPolicyEntry(severity: .info, interruptionLevel: .passive, titleTemplate: "Session Ended", allowedActions: [.viewSession, .acknowledge])
        case "hud.handoff.created":
            return NotificationPolicyEntry(severity: .info, interruptionLevel: .passive, titleTemplate: "Handoff Created", allowedActions: [.acknowledge])
        case "coordinator.plan.complete":
            return NotificationPolicyEntry(severity: .info, interruptionLevel: .passive, titleTemplate: "Plan Complete", allowedActions: [.acknowledge])
        default:
            return nil
        }
    }

    // MARK: - Private

    private static func classifyHealthEvent(data: String) -> NotificationPolicyEntry? {
        guard let jsonData = data.data(using: .utf8),
              let payload = try? JSONSerialization.jsonObject(with: jsonData) as? [String: Any]
        else {
            return nil
        }

        let down = payload["down_servers"] as? Int ?? 0
        let degraded = payload["degraded_servers"] as? Int ?? 0

        if down > 0 {
            return NotificationPolicyEntry(severity: .critical, interruptionLevel: .timeSensitive, titleTemplate: "Server Down", allowedActions: [.viewDashboard, .acknowledge])
        }
        if degraded > 0 {
            return NotificationPolicyEntry(severity: .warning, interruptionLevel: .active, titleTemplate: "Server Degraded", allowedActions: [.viewDashboard, .acknowledge])
        }
        return nil
    }

    private static func classifyPipelineAlertEvent(data: String) -> NotificationPolicyEntry? {
        var severity = AlertSeverity.info
        var title = "Pipeline Alert"
        if let jsonData = data.data(using: .utf8),
           let payload = try? JSONSerialization.jsonObject(with: jsonData) as? [String: Any] {
            if let raw = payload["severity"] as? String {
                severity = AlertSeverity(serverSeverity: raw)
            }
            if let t = payload["title"] as? String, !t.isEmpty {
                title = t
            }
        }
        return NotificationPolicyEntry(
            severity: severity,
            interruptionLevel: severity.defaultInterruptionLevel,
            titleTemplate: title,
            allowedActions: [.acknowledge]
        )
    }

    private static func extractSessionId(from data: String) -> String? {
        guard let jsonData = data.data(using: .utf8),
              let payload = try? JSONSerialization.jsonObject(with: jsonData) as? [String: Any]
        else {
            return nil
        }
        return payload["session_id"] as? String
    }

    private static func extractWorkflowId(from data: String) -> String? {
        guard let jsonData = data.data(using: .utf8),
              let payload = try? JSONSerialization.jsonObject(with: jsonData) as? [String: Any]
        else {
            return nil
        }
        return payload["workflow_id"] as? String
    }

    private static func buildMessage(event: SSEEvent) -> String {
        guard let jsonData = event.data.data(using: .utf8),
              let payload = try? JSONSerialization.jsonObject(with: jsonData) as? [String: Any]
        else {
            return event.type
        }

        switch event.type {
        case "hud.health":
            let down = payload["down_servers"] as? Int ?? 0
            let degraded = payload["degraded_servers"] as? Int ?? 0
            if down > 0 { return "\(down) server(s) down" }
            if degraded > 0 { return "\(degraded) server(s) degraded" }
            return "Health update"
        case "agent.session.start":
            let agentId = payload["agent_id"] as? String ?? "unknown"
            return "Agent \(agentId) started a session"
        case "agent.session.end":
            let sessionId = payload["session_id"] as? String ?? "unknown"
            return "Session \(sessionId) ended"
        case "agent.session.reaped":
            let sessionId = payload["session_id"] as? String ?? "unknown"
            return "Session \(sessionId) was reaped due to inactivity"
        case "agent.nudge.created":
            let agentId = payload["agent_id"] as? String ?? "unknown"
            return "Nudge queued for agent \(agentId)"
        case "hud.workflow.approve":
            let workflowId = payload["workflow_id"] as? String ?? "unknown"
            return "Workflow \(workflowId) approved"
        case "hud.workflow.reject":
            let workflowId = payload["workflow_id"] as? String ?? "unknown"
            return "Workflow \(workflowId) rejected"
        case "hud.handoff.created":
            let from = payload["from_agent"] as? String ?? "unknown"
            let to = payload["to_agent"] as? String ?? "unknown"
            return "Handoff from \(from) to \(to)"
        case "coordinator.plan.complete":
            let planId = payload["plan_id"] as? String ?? payload["session_id"] as? String ?? "unknown"
            return "Plan \(planId) completed"
        case pipelineAlertEventType:
            if let message = payload["message"] as? String, !message.isEmpty {
                return message
            }
            return "Pipeline alert fired"
        default:
            return event.type
        }
    }
}
