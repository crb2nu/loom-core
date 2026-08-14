import SwiftUI
import LoomCompanionKit

/// Handoff inbox: pending agent-to-agent handoffs with accept/reject actions.
///
/// Presented as a sheet from the Work tab — reached from the `loom://handoff`
/// deep link, the Dashboard's handoff attention lane, and the Work toolbar.
/// Accept/reject hit `POST /api/mobile/v1/handoffs/{id}/accept|reject`
/// (`internal/hud/domain/mobile/handler_ops.go`); the list reloads afterwards
/// so a resolved handoff leaves the inbox.
struct HandoffInboxView: View {
    @Bindable var viewModel: OpsViewModel

    @Environment(\.dismiss) private var dismiss

    /// Handoff awaiting reject confirmation (the reason prompt).
    @State private var rejectTarget: MobileHandoff?
    @State private var rejectReason = ""

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: LoomSpacing.md) {
                if let message = viewModel.handoffActionMessage {
                    banner(message, tone: LoomColors.statusHealthy, icon: "checkmark.circle")
                }
                if let error = viewModel.handoffActionError {
                    banner(error, tone: LoomColors.statusCritical, icon: "exclamationmark.triangle")
                }

                content
            }
            .padding()
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(LoomColors.bgPrimary.ignoresSafeArea())
        .navigationTitle("Handoffs")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarLeading) {
                Button("Done") { dismiss() }
                    .tint(LoomColors.accent)
            }
        }
        .task { await viewModel.loadHandoffs() }
        .refreshable {
            await viewModel.loadHandoffs(force: true)
            HapticManager.light()
        }
        .alert("Reject handoff?", isPresented: rejectAlertBinding, presenting: rejectTarget) { handoff in
            TextField("Reason (optional)", text: $rejectReason)
            Button("Reject", role: .destructive) {
                let reason = rejectReason
                rejectReason = ""
                Task {
                    HapticManager.medium()
                    await viewModel.rejectHandoff(handoff, reason: reason.isEmpty ? nil : reason)
                }
            }
            Button("Cancel", role: .cancel) { rejectReason = "" }
        } message: { handoff in
            Text("\(handoff.fromAgent) → \(handoff.toAgent). The reason is forwarded to the source agent.")
        }
    }

    private var rejectAlertBinding: Binding<Bool> {
        Binding(
            get: { rejectTarget != nil },
            set: { if !$0 { rejectTarget = nil } }
        )
    }

    @ViewBuilder
    private var content: some View {
        if viewModel.isLoadingHandoffs && viewModel.handoffs.isEmpty {
            HStack(spacing: LoomSpacing.sm) {
                ProgressView().tint(LoomColors.accent)
                Text("Loading handoffs…")
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.textSecondary)
            }
            .frame(maxWidth: .infinity, alignment: .center)
            .padding(.vertical, LoomSpacing.xl)
        } else if let error = viewModel.handoffsError, viewModel.handoffs.isEmpty {
            ContentUnavailableView {
                Label("Handoffs couldn't load", systemImage: "arrow.left.arrow.right")
            } description: {
                Text(error.description)
            } actions: {
                Button("Retry") {
                    Task { await viewModel.loadHandoffs(force: true) }
                }
                .buttonStyle(.borderedProminent)
            }
        } else if viewModel.handoffs.isEmpty {
            ContentUnavailableView {
                Label("No Handoffs", systemImage: "arrow.left.arrow.right")
            } description: {
                Text("Pending agent handoffs will appear here.")
            }
        } else {
            LazyVStack(spacing: 12) {
                ForEach(Array(viewModel.handoffs.enumerated()), id: \.element.id) { index, handoff in
                    HandoffCard(
                        handoff: handoff,
                        isBusy: viewModel.mutatingHandoffID == handoff.id,
                        isLocked: viewModel.mutatingHandoffID != nil,
                        onAccept: {
                            Task {
                                HapticManager.medium()
                                await viewModel.acceptHandoff(handoff)
                            }
                        },
                        onReject: {
                            rejectReason = ""
                            rejectTarget = handoff
                        }
                    )
                    .cardAppear(index: index)
                }
            }
        }
    }

    private func banner(_ text: String, tone: Color, icon: String) -> some View {
        Label(text, systemImage: icon)
            .font(LoomTypography.caption)
            .foregroundStyle(tone)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(LoomSpacing.md)
            .background(tone.opacity(0.08), in: RoundedRectangle(cornerRadius: 14, style: .continuous))
    }
}

private struct HandoffCard: View {
    let handoff: MobileHandoff
    let isBusy: Bool
    let isLocked: Bool
    let onAccept: () -> Void
    let onReject: () -> Void

    /// Only pending handoffs are actionable — accepted/rejected/viewed rows
    /// stay in the list as history until the next inbox refresh drops them.
    private var isActionable: Bool { handoff.status == "pending" }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Image(systemName: "arrow.right.circle.fill")
                    .foregroundStyle(LoomColors.statusDegraded)
                VStack(alignment: .leading, spacing: 2) {
                    Text(handoff.fromAgent)
                        .font(.subheadline)
                        .fontWeight(.medium)
                    Text("to \(handoff.toAgent)")
                        .font(.caption)
                        .foregroundStyle(LoomColors.fgSecondary)
                }
                Spacer()
                statusBadge(handoff.status)
            }

            if !handoff.summary.isEmpty {
                Text(handoff.summary)
                    .font(.caption)
                    .foregroundStyle(LoomColors.fgPrimary)
                    .lineLimit(3)
            }

            HStack {
                Text(handoff.createdAt)
                    .font(.caption2)
                    .foregroundStyle(LoomColors.fgMuted)
                Spacer()
            }

            if isActionable {
                HStack(spacing: LoomSpacing.sm) {
                    Button(action: onAccept) {
                        HStack(spacing: 6) {
                            if isBusy {
                                ProgressView().controlSize(.mini)
                            } else {
                                Image(systemName: "checkmark.circle")
                            }
                            Text("Accept")
                        }
                        .font(LoomTypography.caption)
                        .frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(LoomColors.statusHealthy)
                    .disabled(isLocked)
                    .accessibilityHint("Accepts the handoff into the target agent's active session")

                    Button(action: onReject) {
                        HStack(spacing: 6) {
                            Image(systemName: "xmark.circle")
                            Text("Reject")
                        }
                        .font(LoomTypography.caption)
                        .frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.bordered)
                    .tint(LoomColors.statusCritical)
                    .disabled(isLocked)
                    .accessibilityHint("Rejects the handoff and notifies the source agent")
                }
                .padding(.top, 2)
            }
        }
        .padding(12)
        .background(
            RoundedRectangle(cornerRadius: 10)
                .fill(.background)
                .shadow(color: .black.opacity(0.06), radius: 4, y: 2)
        )
    }

    @ViewBuilder
    private func statusBadge(_ status: String) -> some View {
        Text(status.replacingOccurrences(of: "_", with: " ").capitalized)
            .font(.caption2)
            .fontWeight(.medium)
            .padding(.horizontal, 8)
            .padding(.vertical, 3)
            .background(statusColor(status).opacity(0.15))
            .foregroundStyle(statusColor(status))
            .clipShape(Capsule())
    }

    private func statusColor(_ status: String) -> Color {
        switch status {
        case "pending": return LoomColors.statusDegraded
        case "accepted": return LoomColors.statusHealthy
        case "rejected": return LoomColors.statusCritical
        case "viewed": return LoomColors.info
        default: return LoomColors.fgMuted
        }
    }
}
