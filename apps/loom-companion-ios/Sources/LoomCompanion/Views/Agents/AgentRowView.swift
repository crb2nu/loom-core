import SwiftUI
import LoomCompanionKit

struct AgentRowView: View {
    let agent: UnifiedAgent
    /// When the enclosing section header already names the repo/namespace, the
    /// row drops the scope prefix to avoid repeating it three times per agent.
    var showsScope: Bool = true

    private var isLive: Bool { agent.status == .active }

    /// Secondary lane: project/namespace · current task or description.
    private var subtitle: String {
        var parts: [String] = []
        if showsScope {
            if let project = agent.project, !project.isEmpty {
                parts.append(project)
            } else if let ns = agent.namespace, !ns.isEmpty {
                parts.append(ns)
            }
        }
        let activity = agent.cleanedActivityLine
        if !activity.isEmpty { parts.append(activity) }
        return parts.joined(separator: " · ")
    }

    /// One-glance metric: tokens when working, last-seen when not.
    @ViewBuilder
    private var primaryMetric: some View {
        if isLive, agent.totalTokens > 0 {
            LoomRowMetric(
                LoomFormat.compact(agent.totalTokens),
                unit: "tok",
                color: LoomColors.statusHealthy
            )
        } else if agent.heartbeatAgeSeconds > 0 {
            LoomRowMetric(
                LoomFormat.duration(seconds: agent.heartbeatAgeSeconds),
                unit: isLive ? nil : "ago",
                color: LoomColors.textTertiary
            )
        } else {
            EmptyView()
        }
    }

    var body: some View {
        LoomListRow(
            accentColor: LoomColors.presenceStatusColor(agent.status),
            // Inside a conversation fold the header already names the harness
            // and every member shares one displayTitle — title by workspace
            // instead so the rows differentiate. Scope sections keep the
            // harness+tag title. Raw agent ids stay on detail/share surfaces.
            title: showsScope ? agent.workspaceTitle : agent.displayTitle,
            subtitle: subtitle,
            isLive: isLive,
            needsAttention: agent.needsAttention,
            emphasizeTitle: isLive,
            leading: {
                LoomRowIcon(
                    systemName: LoomColors.agentTypeIcon(agent.agentType),
                    color: LoomColors.agentTypeColor(agent.agentType)
                )
            },
            trailing: { primaryMetric },
            footer: { pillStrip }
        )
        .loomShareContextMenu(.agent(id: agent.agentId))
    }

    // MARK: - Pill Strip (footer lane)

    /// Flexible metadata strip. Order signals importance: live/status first,
    /// then branch, blockers, session evidence, spawn, pipeline.
    @ViewBuilder
    private var pillStrip: some View {
        // Telemetry / live badge
        if isLive {
            LoomPill(
                "live",
                icon: "bolt.fill",
                color: LoomColors.statusActive,
                weight: .micro
            )
        } else if !agent.telemetryStatus.isEmpty && agent.telemetryStatus != "offline" {
            LoomPill(
                agent.telemetryStatus,
                color: telemetryColor,
                style: .outlined,
                weight: .micro
            )
        }

        // Branch — high signal for working agents
        if !agent.branch.isEmpty {
            LoomPill(
                agent.branch,
                icon: "arrow.triangle.branch",
                color: LoomColors.accent,
                weight: .micro
            )
        }

        // Blocked tasks — demands attention, surface early
        if agent.blockedTasks > 0 {
            LoomPill(
                "\(agent.blockedTasks) blocked",
                icon: "exclamationmark.triangle.fill",
                color: LoomColors.statusBlocked,
                weight: .micro
            )
        } else if agent.taskCount > 0 {
            LoomPill(
                "\(agent.taskCount) tasks",
                icon: "checklist",
                color: LoomColors.textSecondary,
                style: .outlined,
                weight: .micro
            )
        }

        // Session evidence — entry count
        if agent.hasSession, agent.entryCount > 0 {
            LoomPill(
                "\(agent.entryCount)",
                icon: "doc.text",
                color: LoomColors.accent,
                style: .outlined,
                weight: .micro
            )
        }

        // Spawned (remote execution)
        if agent.isSpawned {
            LoomPill(
                "k8s",
                icon: "cloud",
                color: LoomColors.tierShortTerm,
                weight: .micro
            )
        }

        // CI state — only if meaningful
        if let status = agent.pipelineStatus, !status.isEmpty {
            LoomPill(
                "CI \(status)",
                icon: "arrow.triangle.2.circlepath",
                color: LoomColors.pipelineStatusColor(status),
                weight: .micro
            )
        }
    }

    // MARK: - Color helpers

    private var telemetryColor: Color {
        switch agent.telemetryStatus {
        case "live": return LoomColors.statusActive
        case "idle": return LoomColors.statusIdle
        case "stale": return LoomColors.statusDegraded
        case "session_only": return LoomColors.accent
        case "offline": return LoomColors.textTertiary
        default: return LoomColors.textSecondary
        }
    }

}
