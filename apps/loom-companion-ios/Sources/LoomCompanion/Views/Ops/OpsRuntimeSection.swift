import SwiftUI
import LoomCompanionKit

/// Runtime section: presence agents, claims/worktrees, gateway/daemon health.
///
/// Sandbox/devbox lives in its own peek (`OpsSandboxSection`) because it's the
/// only mobile mutation surface and deserves first-class IA visibility.
struct OpsRuntimeSection: View {
    @Bindable var viewModel: OpsViewModel
    var broadcaster: SSEEventBroadcaster?

    @State private var agentDisplayLimit = 8

    var body: some View {
        VStack(spacing: LoomSpacing.cardSpacing) {
            presenceCard
                .cardAppear(index: 0)

            claimsWorktreesCard
                .cardAppear(index: 1)

            gatewayDaemonCard
                .cardAppear(index: 2)

            Text("Presence remains read-only in mobile. Spawn agents from the Spawn tab.")
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.textTertiary)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .task {
            await viewModel.loadSectionIfNeeded(.runtime)
        }
    }

    // MARK: - Presence Card

    private var presenceCard: some View {
        LoomCard {
            VStack(alignment: .leading, spacing: LoomSpacing.sm) {
                Text("Presence Summary")
                    .font(LoomTypography.headlineMedium)
                    .foregroundStyle(LoomColors.textPrimary)

                Text("Use runtime status to spot whether the fleet is available before opening the deeper Agents views.")
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.textTertiary)

                #if canImport(Charts)
                FleetCompositionChart(
                    active: viewModel.presenceSummary.activeAgents,
                    idle: viewModel.presenceSummary.idleAgents,
                    offline: viewModel.presenceSummary.offlineAgents
                )
                #endif

                HStack {
                    opsMetric(label: "Active", value: viewModel.presenceSummary.activeAgents, icon: "bolt.fill", color: LoomColors.statusHealthy)
                    Spacer()
                    opsMetric(label: "Idle", value: viewModel.presenceSummary.idleAgents, icon: "moon.fill", color: LoomColors.statusIdle)
                    Spacer()
                    opsMetric(label: "Offline", value: viewModel.presenceSummary.offlineAgents, icon: "xmark.circle.fill", color: LoomColors.statusCritical)
                }

                if viewModel.presenceAgents.isEmpty {
                    Text("No agents")
                        .font(LoomTypography.bodyRegular)
                        .foregroundStyle(LoomColors.textTertiary)
                } else {
                    ForEach(Array(viewModel.presenceAgents.prefix(agentDisplayLimit))) { agent in
                        HStack(spacing: LoomSpacing.sm) {
                            ZStack {
                                Image(systemName: LoomColors.agentTypeIcon(agent.agentType))
                                    .font(.caption)
                                    .foregroundStyle(LoomColors.agentTypeColor(agent.agentType))
                                    .frame(width: 20, height: 20)
                                PulsingDot(color: agentStatusColor(agent.status), isPulsing: agent.status.rawValue == "active")
                                    .offset(x: 8, y: 8)
                            }
                            .frame(width: 24, height: 24)
                            VStack(alignment: .leading, spacing: 2) {
                                HStack(spacing: 6) {
                                    Text(agent.agentId)
                                        .font(LoomTypography.bodyMedium)
                                        .foregroundStyle(LoomColors.agentTypeColor(agent.agentType))
                                        .lineLimit(1)
                                    StatusBadge(
                                        agent.status.rawValue,
                                        color: agentStatusColor(agent.status)
                                    )
                                }
                                if !agent.currentTask.isEmpty {
                                    Text(agent.currentTask)
                                        .font(LoomTypography.caption)
                                        .foregroundStyle(LoomColors.textSecondary)
                                        .lineLimit(1)
                                }
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                        }
                        .padding(.vertical, 2)
                    }
                    if viewModel.presenceAgents.count > agentDisplayLimit {
                        Button {
                            withAnimation(.easeInOut(duration: 0.25)) {
                                agentDisplayLimit += 8
                            }
                            HapticManager.light()
                        } label: {
                            Text("Show \(min(8, viewModel.presenceAgents.count - agentDisplayLimit)) More")
                                .font(LoomTypography.caption)
                                .foregroundStyle(LoomColors.accent)
                                .frame(maxWidth: .infinity)
                                .padding(.vertical, 6)
                        }
                    }
                }
            }
        }
    }

    // MARK: - Claims & Worktrees Card

    private var claimsWorktreesCard: some View {
        LoomCard {
            VStack(alignment: .leading, spacing: LoomSpacing.sm) {
                Text("Claims & Worktrees")
                    .font(LoomTypography.headlineMedium)
                    .foregroundStyle(LoomColors.textPrimary)

                HStack(spacing: LoomSpacing.lg) {
                    HStack(spacing: LoomSpacing.xs) {
                        Image(systemName: "lock.fill")
                            .foregroundStyle(LoomColors.accent)
                        Text("Claims:")
                            .font(LoomTypography.bodyRegular)
                            .foregroundStyle(LoomColors.textSecondary)
                        AnimatedCounter(viewModel.presenceClaims.count)
                            .font(LoomTypography.counterSmall)
                    }
                    HStack(spacing: LoomSpacing.xs) {
                        Image(systemName: "arrow.triangle.branch")
                            .foregroundStyle(LoomColors.accent)
                        Text("Worktrees:")
                            .font(LoomTypography.bodyRegular)
                            .foregroundStyle(LoomColors.textSecondary)
                        AnimatedCounter(viewModel.presenceWorktrees.count)
                            .font(LoomTypography.counterSmall)
                    }
                }

                if let topology = viewModel.topology {
                    HStack(spacing: LoomSpacing.xs) {
                        Image(systemName: "point.3.connected.trianglepath.dotted")
                            .foregroundStyle(LoomColors.statusInfo)
                        Text("Topology: \(topology.nodes.count) nodes \u{2022} \(topology.edges.count) edges")
                            .font(LoomTypography.caption)
                            .foregroundStyle(LoomColors.textSecondary)
                    }
                } else {
                    Text("Topology unavailable")
                        .font(LoomTypography.caption)
                        .foregroundStyle(LoomColors.textTertiary)
                }
            }
        }
    }

    // MARK: - Gateway & Daemon Card

    private var gatewayDaemonCard: some View {
        LoomCard {
            VStack(alignment: .leading, spacing: LoomSpacing.sm) {
                Text("Gateway & Daemon")
                    .font(LoomTypography.headlineMedium)
                    .foregroundStyle(LoomColors.textPrimary)

                if let controlPlane = viewModel.controlPlane {
                    HStack {
                        opsMetric(label: "Servers", value: controlPlane.health.totalServers, icon: "server.rack", color: LoomColors.accent)
                        Spacer()
                        opsMetric(label: "Hub", value: controlPlane.health.hubTargets, icon: "globe", color: LoomColors.statusInfo)
                        Spacer()
                        opsMetric(label: "Local", value: controlPlane.health.localTargets, icon: "desktopcomputer", color: LoomColors.statusActive)
                        Spacer()
                        opsMetric(label: "Idle", value: controlPlane.health.idleServers, icon: "moon.fill", color: LoomColors.statusIdle)
                    }

                    VStack(alignment: .leading, spacing: 4) {
                        Label("Health: \(controlPlane.health.healthyServers) healthy \u{2022} \(controlPlane.health.degradedServers) degraded \u{2022} \(controlPlane.health.downServers) down", systemImage: "heart.fill")
                            .font(LoomTypography.caption)
                            .foregroundStyle(LoomColors.textSecondary)

                        Label("RBAC: \(controlPlane.rbac.enabled ? "on" : "off") \u{2022} roles \(controlPlane.rbac.roleCount) \u{2022} bindings \(controlPlane.rbac.bindingCount) \u{2022} denied \(controlPlane.rbac.deniedCount)", systemImage: "shield.fill")
                            .font(LoomTypography.caption)
                            .foregroundStyle(LoomColors.textSecondary)

                        Label("OTel: \(controlPlane.otel.otlpConfigured ? "configured" : "off") \u{2022} traced \(controlPlane.otel.tracedServers)/\(controlPlane.otel.totalServers)", systemImage: "waveform.path")
                            .font(LoomTypography.caption)
                            .foregroundStyle(LoomColors.textSecondary)

                        Label("Cost: \(controlPlane.cost.totalCalls) calls \u{2022} errors \(controlPlane.cost.totalErrors) \u{2022} denied \(controlPlane.cost.totalDenied)", systemImage: "dollarsign.circle")
                            .font(LoomTypography.caption)
                            .foregroundStyle(LoomColors.textSecondary)
                    }
                } else {
                    Text("Control-plane telemetry unavailable")
                        .font(LoomTypography.caption)
                        .foregroundStyle(LoomColors.textTertiary)
                }
            }
        }
    }

    // MARK: - Helpers

    private func opsMetric(label: String, value: Int, icon: String, color: Color) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: 4) {
                Image(systemName: icon)
                    .font(.caption2)
                    .foregroundStyle(color)
                AnimatedCounter(value)
                    .font(LoomTypography.counterSmall)
                    .foregroundStyle(LoomColors.textPrimary)
            }
            Text(label)
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.textSecondary)
        }
    }

    private func agentStatusColor(_ status: MobilePresenceStatus) -> Color {
        switch status {
        case .active: return LoomColors.statusHealthy
        case .idle: return LoomColors.statusIdle
        case .offline: return LoomColors.statusCritical
        case .unknown: return LoomColors.statusIdle
        }
    }
}
