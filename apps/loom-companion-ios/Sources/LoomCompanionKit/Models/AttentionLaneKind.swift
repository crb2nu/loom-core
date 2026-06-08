import Foundation

/// Semantic classification of a dashboard attention lane.
///
/// The mobile dashboard backend
/// (`internal/hud/domain/mobile.buildMobileAttentionLanes`) emits exactly four
/// lane `type` values today — `agent`, `namespace`, `merge`, and `conflict`
/// (frozen in `internal/contracts/testdata/mobile_attention_lanes.golden`) —
/// plus richer signals (`isTaskLane`, `route`, `severity`). Earlier UI revisions
/// matched against a HUD-flavored vocabulary (`approval`, `degraded_server`,
/// `idle_agent`, `handoff`, `merge_conflict`, …) that the mobile endpoint never
/// produces, so the live lanes fell through to a generic flag icon on the hero
/// (`NextActionCard`) and queue (`AttentionLanesCard`) surfaces.
///
/// This enum is the single source of truth both cards resolve against. It covers
/// the live mobile contract first and retains the legacy HUD types for
/// forward-compatibility, so the two surfaces can never drift again.
public enum AttentionLaneKind: String, Sendable, CaseIterable {
    /// An agent needs attention (backend `type: "agent"`).
    case agent
    /// Work-queue / namespace pressure, including blocked or orphaned tasks
    /// (backend `type: "namespace"`; legacy `blocked_task`/`blocker`/`task`).
    case work
    /// Branches ready to merge (backend `type: "merge"`).
    case merge
    /// File / coordination conflicts (backend `type: "conflict"`; legacy
    /// `merge_conflict`).
    case conflict
    /// A pending workflow approval (legacy HUD vocabulary).
    case approval
    /// A degraded or down server (legacy HUD vocabulary).
    case server
    /// An idle agent / stale heartbeat (legacy HUD vocabulary).
    case idleAgent
    /// A pending handoff (legacy HUD vocabulary).
    case handoff
    /// Unknown or untyped lane — falls back to generic presentation.
    case other

    /// Classify by the raw lane `type` string alone (no task-shape inference).
    /// Used for grouping buckets where only the type key is available.
    public init(typeKey: String) {
        switch typeKey {
        case "agent":
            self = .agent
        case "namespace", "work", "blocked_task", "blocker", "orphan_task", "task", "task_filter":
            self = .work
        case "merge", "merge_ready":
            self = .merge
        case "conflict", "merge_conflict", "file_conflict":
            self = .conflict
        case "approval", "workflow_approval":
            self = .approval
        case "degraded_server", "server_health":
            self = .server
        case "idle_agent", "stale_heartbeat":
            self = .idleAgent
        case "handoff":
            self = .handoff
        default:
            self = .other
        }
    }
}

public extension AttentionLaneKind {
    /// Filled SF Symbol for prominent (hero) contexts.
    var heroIcon: String {
        switch self {
        case .agent: return "person.fill.questionmark"
        case .work: return "hand.raised.fill"
        case .merge: return "arrow.triangle.pull"
        case .conflict: return "arrow.triangle.merge"
        case .approval: return "checkmark.seal.fill"
        case .server: return "exclamationmark.triangle.fill"
        case .idleAgent: return "person.fill.questionmark"
        case .handoff: return "arrow.right.arrow.left"
        case .other: return "flag.fill"
        }
    }

    /// Outline SF Symbol for calmer list/row contexts.
    var rowIcon: String {
        switch self {
        case .agent: return "person.fill.questionmark"
        case .work: return "hand.raised"
        case .merge: return "arrow.triangle.pull"
        case .conflict: return "arrow.triangle.merge"
        case .approval: return "checkmark.seal"
        case .server: return "exclamationmark.triangle"
        case .idleAgent: return "person.fill.questionmark"
        case .handoff: return "arrow.right.arrow.left"
        case .other: return "flag.fill"
        }
    }

    /// Singular, action-oriented title used only as a last-resort fallback when
    /// the backend supplies no usable label or summary.
    var singularTitle: String {
        switch self {
        case .agent: return "Agent needs attention"
        case .work: return "Unblock stalled task"
        case .merge: return "Branches ready to merge"
        case .conflict: return "Resolve merge conflict"
        case .approval: return "Approve workflow step"
        case .server: return "Investigate degraded server"
        case .idleAgent: return "Check idle agent"
        case .handoff: return "Accept pending handoff"
        case .other: return "Review attention lane"
        }
    }

    /// Plural, aggregate title for grouped rows. `nil` for `.other` so unknown
    /// buckets fall back to a summary-derived title.
    var aggregateTitleIfKnown: String? {
        switch self {
        case .agent: return "Agents need attention"
        case .work: return "Work lanes"
        case .merge: return "Merge-ready branches"
        case .conflict: return "Merge conflicts"
        case .approval: return "Pending approvals"
        case .server: return "Degraded servers"
        case .idleAgent: return "Idle agents"
        case .handoff: return "Pending handoffs"
        case .other: return nil
        }
    }
}

public extension DashboardAttentionLane {
    /// Semantic kind for presentation.
    ///
    /// Explicit backend `type` wins first — important because the `merge` lane
    /// is tagged `target_kind: "task_filter"` (so `isTaskLane` is true for it),
    /// and we must not collapse merge-ready into the generic work bucket. Only
    /// when the type is unknown/empty do we fall back to the richer task-shape
    /// signal so summary-only "blocked task" lanes still route to Work.
    var kind: AttentionLaneKind {
        let byType = AttentionLaneKind(typeKey: type)
        if byType != .other { return byType }
        return isTaskLane ? .work : .other
    }
}
