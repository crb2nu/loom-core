import SwiftUI
import LoomCompanionKit

/// The alert inbox: the HUD's server-side alert store merged with live SSE
/// events, plus the auto-fix proposals awaiting a decision.
///
/// Presented as a sheet from the Dashboard — reached from the `INBOX` header,
/// the critical-alert hero card, the `alert` attention lane, and the
/// `loom://alerts` / `loom://alert/<id>` deep links. There is no Alerts tab
/// (Spawn took that slot), so the Dashboard is the destination.
///
/// Backing routes (`internal/hud/domain/alerting/alerting.go`):
/// `GET /api/mobile/v1/alerts`, `POST /alerts/{id}/ack`,
/// `GET /autofix/proposals`, `POST /autofix/proposals/{id}/approve|reject`.
struct AlertsListView: View {
    @Bindable var viewModel: AlertsViewModel
    /// Alert to scroll to and highlight on appear (from `loom://alert/<id>`).
    var focusedAlertID: String?
    var onNavigate: ((AlertAction, AlertItem) -> Void)?

    @Environment(\.dismiss) private var dismiss

    enum Segment: String, CaseIterable, Identifiable {
        case alerts
        case autofix

        var id: String { rawValue }
    }

    @State private var segment: Segment = .alerts
    @State private var approveTarget: AutofixProposal?
    @State private var rejectTarget: AutofixProposal?

    var body: some View {
        VStack(spacing: 0) {
            picker

            Group {
                switch segment {
                case .alerts:
                    alertsList
                case .autofix:
                    autofixList
                }
            }
        }
        .background(LoomColors.bgPrimary.ignoresSafeArea())
        .navigationTitle("Alerts")
        #if os(iOS)
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .toolbar {
            ToolbarItem(placement: .topBarLeading) {
                Button("Done") { dismiss() }
                    .tint(LoomColors.accent)
            }
            ToolbarItemGroup(placement: .primaryAction) {
                if segment == .alerts, !viewModel.alerts.isEmpty {
                    Button {
                        HapticManager.light()
                        Task { await viewModel.markAllRead() }
                    } label: {
                        Label("Acknowledge All", systemImage: "envelope.open")
                    }
                    .disabled(viewModel.unreadCount == 0)

                    Button(role: .destructive) {
                        HapticManager.heavy()
                        viewModel.clearAll()
                    } label: {
                        Label("Clear Local List", systemImage: "trash")
                    }
                }
            }
        }
        .task {
            await viewModel.load()
            if focusedAlertID != nil { segment = .alerts }
        }
        .refreshable {
            await viewModel.load()
            HapticManager.light()
        }
        .confirmationDialog(
            "Approve this auto-fix?",
            isPresented: approveDialogBinding,
            titleVisibility: .visible,
            presenting: approveTarget
        ) { proposal in
            Button("Approve", role: proposal.kind.isNoOp ? nil : .destructive) {
                Task {
                    HapticManager.medium()
                    await viewModel.approveProposal(proposal)
                }
            }
            Button("Cancel", role: .cancel) {}
        } message: { proposal in
            // Copy is derived from what ExecuteAutoFix actually does — the
            // `retry` strategy is still a no-op placeholder server-side, and
            // saying otherwise would be a lie.
            Text("Approving runs the fix immediately.\n\n\(proposal.kind.approveEffect)")
        }
        .confirmationDialog(
            "Reject this auto-fix?",
            isPresented: rejectDialogBinding,
            titleVisibility: .visible,
            presenting: rejectTarget
        ) { proposal in
            Button("Reject", role: .destructive) {
                Task {
                    HapticManager.medium()
                    await viewModel.rejectProposal(proposal)
                }
            }
            Button("Cancel", role: .cancel) {}
        } message: { _ in
            Text("The HUD records a rejected execution. The proposal itself stays in the list for audit.")
        }
    }

    // MARK: - Chrome

    private var picker: some View {
        VStack(spacing: LoomSpacing.xs) {
            Picker("Section", selection: $segment) {
                Text(viewModel.unreadCount > 0 ? "Alerts (\(viewModel.unreadCount))" : "Alerts")
                    .tag(Segment.alerts)
                Text(viewModel.pendingProposals.isEmpty
                     ? "Auto-fix"
                     : "Auto-fix (\(viewModel.pendingProposals.count))")
                    .tag(Segment.autofix)
            }
            .pickerStyle(.segmented)

            if let message = viewModel.actionMessage {
                banner(message, tone: LoomColors.statusHealthy, icon: "checkmark.circle")
            }
            if let error = viewModel.actionError {
                banner(error, tone: LoomColors.statusCritical, icon: "exclamationmark.triangle")
            }
        }
        .padding(.horizontal, LoomSpacing.md)
        .padding(.top, LoomSpacing.sm)
        .padding(.bottom, LoomSpacing.xs)
        .onChange(of: segment) { _, _ in HapticManager.selection() }
    }

    private func banner(_ text: String, tone: Color, icon: String) -> some View {
        Label(text, systemImage: icon)
            .font(LoomTypography.caption)
            .foregroundStyle(tone)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(LoomSpacing.sm)
            .background(tone.opacity(0.08), in: RoundedRectangle(cornerRadius: 12, style: .continuous))
    }

    // MARK: - Alerts

    @ViewBuilder
    private var alertsList: some View {
        if viewModel.alerts.isEmpty {
            if viewModel.isLoading && !viewModel.hasLoaded {
                loading("Loading alerts…")
            } else if let error = viewModel.loadError {
                ContentUnavailableView {
                    Label("Alerts couldn't load", systemImage: "bell.badge.slash")
                } description: {
                    Text(error.description)
                } actions: {
                    Button("Retry") { Task { await viewModel.load() } }
                        .buttonStyle(.borderedProminent)
                }
            } else {
                ContentUnavailableView {
                    Label("No Alerts", systemImage: "bell.slash")
                } description: {
                    Text("Fired alerts from the HUD alert engine, plus live stream events, appear here.")
                }
            }
        } else {
            ScrollViewReader { proxy in
                List {
                    ForEach(viewModel.alerts) { alert in
                        AlertRowView(
                            alert: alert,
                            isAcking: viewModel.ackingAlertIDs.contains(alert.id),
                            isFocused: alert.id == focusedAlertID
                        )
                        .id(alert.id)
                        .contentShape(Rectangle())
                        .onTapGesture {
                            HapticManager.light()
                            viewModel.markRead(alert.id)
                            let action = alert.primaryAction
                            if action != .acknowledge {
                                onNavigate?(action, alert)
                            }
                        }
                        .swipeActions(edge: .trailing) {
                            // Server-backed alerts have no delete route — the
                            // HUD stamps `acked_at` in place — so dismissing
                            // one would just resurrect it on the next load.
                            if !alert.isServerBacked {
                                Button(role: .destructive) {
                                    HapticManager.medium()
                                    viewModel.removeAlert(alert.id)
                                } label: {
                                    Label("Dismiss", systemImage: "trash")
                                }
                            }
                        }
                        .swipeActions(edge: .leading) {
                            if !alert.isRead {
                                Button {
                                    HapticManager.light()
                                    viewModel.markRead(alert.id)
                                } label: {
                                    Label(
                                        alert.isServerBacked ? "Acknowledge" : "Read",
                                        systemImage: "envelope.open"
                                    )
                                }
                                .tint(LoomColors.statusActive)
                            }
                        }
                    }
                }
                .listStyle(.plain)
                .task(id: focusedAlertID) {
                    guard let focusedAlertID,
                          viewModel.alerts.contains(where: { $0.id == focusedAlertID })
                    else { return }
                    withAnimation { proxy.scrollTo(focusedAlertID, anchor: .center) }
                }
            }
        }
    }

    // MARK: - Auto-fix proposals

    @ViewBuilder
    private var autofixList: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: LoomSpacing.md) {
                if viewModel.proposals.isEmpty {
                    if viewModel.isLoading && !viewModel.hasLoaded {
                        loading("Loading proposals…")
                    } else if let error = viewModel.proposalsError {
                        ContentUnavailableView {
                            Label("Proposals couldn't load", systemImage: "wand.and.stars.inverse")
                        } description: {
                            Text(error.description)
                        } actions: {
                            Button("Retry") { Task { await viewModel.loadProposals() } }
                                .buttonStyle(.borderedProminent)
                        }
                    } else {
                        ContentUnavailableView {
                            Label("No Proposals", systemImage: "wand.and.stars")
                        } description: {
                            Text("The HUD proposes an auto-fix after diagnosing a failed pipeline. Nothing is waiting on you.")
                        }
                    }
                } else {
                    ForEach(Array(viewModel.proposals.enumerated()), id: \.element.id) { index, proposal in
                        AutofixProposalCard(
                            proposal: proposal,
                            decision: viewModel.decidedProposalIDs[proposal.id],
                            isBusy: viewModel.decidingProposalID == proposal.id,
                            isLocked: viewModel.decidingProposalID != nil,
                            onApprove: {
                                HapticManager.selection()
                                approveTarget = proposal
                            },
                            onReject: {
                                HapticManager.selection()
                                rejectTarget = proposal
                            }
                        )
                        .cardAppear(index: index)
                    }
                }
            }
            .padding()
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func loading(_ text: String) -> some View {
        HStack(spacing: LoomSpacing.sm) {
            ProgressView().tint(LoomColors.accent)
            Text(text)
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.textSecondary)
        }
        .frame(maxWidth: .infinity, alignment: .center)
        .padding(.vertical, LoomSpacing.xl)
    }

    // MARK: - Dialog bindings

    private var approveDialogBinding: Binding<Bool> {
        Binding(get: { approveTarget != nil }, set: { if !$0 { approveTarget = nil } })
    }

    private var rejectDialogBinding: Binding<Bool> {
        Binding(get: { rejectTarget != nil }, set: { if !$0 { rejectTarget = nil } })
    }
}

/// One pending auto-fix proposal with its Approve/Reject controls.
private struct AutofixProposalCard: View {
    let proposal: AutofixProposal
    /// "approved"/"rejected" once decided from this device — the HUD's proposal
    /// list is append-only, so a decided proposal keeps coming back.
    let decision: String?
    let isBusy: Bool
    let isLocked: Bool
    let onApprove: () -> Void
    let onReject: () -> Void

    private var accent: Color {
        if decision != nil { return LoomColors.statusIdle }
        return proposal.kind.isNoOp ? LoomColors.statusDegraded : LoomColors.accent
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: LoomSpacing.xs) {
                LoomPill(proposal.kind.label, color: accent, style: .tinted, weight: .micro)
                LoomPill(
                    "\(Int((proposal.confidence * 100).rounded()))% confidence",
                    color: LoomColors.textTertiary,
                    style: .outlined,
                    weight: .micro
                )
                Spacer(minLength: 0)
                if let decision {
                    LoomPill(
                        decision.uppercased(),
                        color: decision == "approved" ? LoomColors.statusHealthy : LoomColors.statusCritical,
                        style: .solid,
                        weight: .micro
                    )
                }
            }

            Text(proposal.description.isEmpty ? proposal.id : proposal.description)
                .font(LoomTypography.bodyMedium)
                .foregroundStyle(LoomColors.textPrimary)
                .fixedSize(horizontal: false, vertical: true)

            if !proposal.diagnosisId.isEmpty {
                Text(proposal.diagnosisId)
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.textTertiary)
            }

            if !proposal.estimatedFiles.isEmpty {
                Text(proposal.estimatedFiles.joined(separator: ", "))
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.textSecondary)
                    .lineLimit(3)
            }

            // Honest, up-front description of what approving does. `retry` is
            // still a no-op placeholder in the HUD's executor.
            Text(proposal.kind.approveEffect)
                .font(LoomTypography.caption)
                .foregroundStyle(proposal.kind.isNoOp ? LoomColors.statusDegraded : LoomColors.textSecondary)
                .fixedSize(horizontal: false, vertical: true)

            if decision == nil {
                HStack(spacing: LoomSpacing.sm) {
                    Button(action: onApprove) {
                        HStack(spacing: 6) {
                            if isBusy {
                                ProgressView().controlSize(.mini)
                            } else {
                                Image(systemName: "checkmark.circle")
                            }
                            Text("Approve")
                        }
                        .font(LoomTypography.caption)
                        .frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(LoomColors.statusHealthy)
                    .disabled(isLocked)
                    .accessibilityHint("Runs the fix immediately. Requires the HUD admin token.")

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
                    .accessibilityHint("Records a rejected execution on the HUD")
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
        .opacity(decision == nil ? 1 : 0.66)
    }
}
