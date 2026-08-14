// AgentCommandSheet — the Operator deck's per-agent control surface.
//
// Tapping a roster row on the deck opens this sheet in place, so viewing and
// commanding an agent is one gesture instead of a cross-tab expedition. The
// action set comes from AgentCommandViewModel (Kit, unit-tested) so the sheet
// never offers a control the HUD would refuse: end-session only for a live
// session, message/interrupt/stop only for a live spawn pod.

import LoomCompanionKit
import SwiftUI

struct AgentCommandSheet: View {
    /// Cross-tab jumps the sheet can request; the deck routes them after
    /// dismissal (same closure pattern as OperatorScreen.QuickAction).
    enum NavigationTarget {
        case session(String)
        case agentDetail(String)
    }

    private let agent: UnifiedAgent
    @State private var viewModel: AgentCommandViewModel
    private let onNavigate: (NavigationTarget) -> Void
    /// Fired after any successful mutation so the deck refreshes its roster.
    private let onMutated: () -> Void

    @Environment(\.dismiss) private var dismiss
    @State private var confirmEndSession = false
    @State private var confirmStop = false
    @State private var messageText = ""
    @FocusState private var messageFocused: Bool

    init(
        agent: UnifiedAgent,
        apiClient: any LoomAPIClientProtocol,
        onNavigate: @escaping (NavigationTarget) -> Void = { _ in },
        onMutated: @escaping () -> Void = {}
    ) {
        self.agent = agent
        _viewModel = State(initialValue: AgentCommandViewModel(agent: agent, apiClient: apiClient))
        self.onNavigate = onNavigate
        self.onMutated = onMutated
    }

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: LoomSpacing.lg) {
                    header
                    metaGrid
                    if !agent.attentionReasons.isEmpty {
                        attentionSection
                    }
                    controlsSection
                    feedbackSection
                }
                .padding(LoomSpacing.lg)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .background(LoomColors.bgPrimary.ignoresSafeArea())
            .navigationTitle(agent.agentId)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button("Done") { finish() }
                        .tint(LoomColors.info)
                }
            }
        }
        .preferredColorScheme(.dark)
        .presentationDetents([.medium, .large])
        .presentationDragIndicator(.visible)
        .confirmationDialog(
            "End this session?",
            isPresented: $confirmEndSession,
            titleVisibility: .visible
        ) {
            Button("End session", role: .destructive) {
                Task {
                    if await viewModel.endSession() {
                        HapticManager.medium()
                        onMutated()
                    }
                }
            }
        } message: {
            Text("The agent's session is summarized and closed on the HUD. The agent process itself keeps running.")
        }
        .confirmationDialog(
            "Stop this spawn?",
            isPresented: $confirmStop,
            titleVisibility: .visible
        ) {
            Button("Stop spawn", role: .destructive) {
                Task {
                    if await viewModel.stopSpawn() {
                        HapticManager.medium()
                        onMutated()
                    }
                }
            }
        } message: {
            Text("The remote pod is torn down. Unpushed work inside it is lost.")
        }
    }

    // MARK: - Header

    private var header: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xs) {
            HStack(spacing: LoomSpacing.sm) {
                Image(systemName: LoomColors.agentTypeIcon(agent.agentType))
                    .font(.system(size: 16))
                    .foregroundStyle(LoomColors.agentTypeColor(agent.agentType))
                Text(agent.agentType)
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.textSecondary)
                Spacer()
                LoomPill(
                    agent.status.rawValue,
                    color: LoomColors.presenceStatusColor(agent.status)
                )
            }
            HStack(spacing: LoomSpacing.xs) {
                if agent.isSpawned {
                    LoomPill("k8s spawn", icon: "cloud", color: LoomColors.tierShortTerm, weight: .micro)
                }
                if agent.isOrphan {
                    LoomPill(
                        "orphan " + LoomFormat.duration(seconds: agent.orphanAgeSeconds),
                        icon: "questionmark.circle",
                        color: LoomColors.statusDegraded,
                        style: .outlined,
                        weight: .micro
                    )
                }
                if agent.needsAttention {
                    LoomPill("attention", icon: "exclamationmark.triangle.fill", color: LoomColors.statusDegraded, weight: .micro)
                }
                if let status = agent.spawnStatus, !status.isEmpty {
                    LoomPill(status, color: LoomColors.textSecondary, style: .outlined, weight: .micro)
                }
            }
            if !agent.currentTask.isEmpty || !agent.description.isEmpty {
                Text(agent.currentTask.isEmpty ? agent.description : agent.currentTask)
                    .font(LoomTypography.bodyMedium)
                    .foregroundStyle(LoomColors.textPrimary)
                    .lineLimit(3)
            }
        }
    }

    // MARK: - Meta

    private var metaGrid: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xxs) {
            if let project = agent.project, !project.isEmpty {
                metaRow(icon: "shippingbox", label: "Project", value: project)
            }
            if let ns = agent.namespace, !ns.isEmpty {
                metaRow(icon: "folder", label: "Namespace", value: ns)
            }
            if !agent.branch.isEmpty {
                metaRow(icon: "arrow.triangle.branch", label: "Branch", value: agent.branch)
            }
            if agent.hasSession {
                metaRow(
                    icon: "doc.text",
                    label: "Session",
                    value: "\(agent.entryCount) entries · \(LoomFormat.compact(agent.totalTokens)) tok"
                )
            }
            if agent.heartbeatAgeSeconds > 0 {
                metaRow(
                    icon: "waveform.path.ecg",
                    label: "Heartbeat",
                    value: LoomFormat.duration(seconds: agent.heartbeatAgeSeconds) + " ago"
                )
            }
            if let status = agent.pipelineStatus, !status.isEmpty {
                metaRow(icon: "arrow.triangle.2.circlepath", label: "CI", value: status)
            }
        }
    }

    private func metaRow(icon: String, label: String, value: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: LoomSpacing.sm) {
            Image(systemName: icon)
                .font(.system(size: 11))
                .foregroundStyle(LoomColors.fgMuted)
                .frame(width: 16)
            Text(label)
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.fgMuted)
                .frame(width: 76, alignment: .leading)
            Text(value)
                .font(LoomTypography.monoCaption)
                .foregroundStyle(LoomColors.fgSecondary)
                .lineLimit(1)
                .truncationMode(.middle)
        }
    }

    private var attentionSection: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xxs) {
            sectionTitle("Attention")
            ForEach(agent.attentionReasons, id: \.self) { reason in
                Text(reason)
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.statusDegraded)
            }
        }
    }

    // MARK: - Controls

    private var controlsSection: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            sectionTitle("Controls")

            // Navigation is always offered — a row you can inspect but not
            // command still deserves its full surface one tap away.
            if viewModel.actions.contains(.viewSession), let sessionId = agent.sessionId {
                controlButton(
                    "Open session detail",
                    icon: "rectangle.stack.person.crop",
                    color: LoomColors.info
                ) {
                    finish(.session(sessionId))
                }
            } else {
                controlButton(
                    "Open agent detail",
                    icon: "person.text.rectangle",
                    color: LoomColors.info
                ) {
                    finish(.agentDetail(agent.agentId))
                }
            }

            if viewModel.actions.contains(.message) {
                messageComposer
            }
            if viewModel.actions.contains(.interrupt) {
                controlButton(
                    "Interrupt current turn",
                    icon: "hand.raised",
                    color: LoomColors.statusDegraded,
                    disabled: viewModel.isBusy
                ) {
                    Task {
                        if await viewModel.interruptSpawn() {
                            HapticManager.medium()
                            onMutated()
                        }
                    }
                }
            }
            if viewModel.actions.contains(.endSession) {
                controlButton(
                    "End session",
                    icon: "stop.circle",
                    color: LoomColors.statusCritical,
                    disabled: viewModel.isBusy
                ) {
                    confirmEndSession = true
                }
            }
            if viewModel.actions.contains(.stop) {
                controlButton(
                    "Stop spawn",
                    icon: "xmark.octagon",
                    color: LoomColors.statusCritical,
                    disabled: viewModel.isBusy
                ) {
                    confirmStop = true
                }
            }

            if viewModel.actions.isEmpty {
                Text("Nothing to command — this agent has no live session or spawn the HUD can act on.")
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.fgMuted)
            }
        }
    }

    private var messageComposer: some View {
        HStack(spacing: LoomSpacing.xs) {
            TextField("Send a follow-up to this agent…", text: $messageText, axis: .vertical)
                .font(LoomTypography.caption)
                .textFieldStyle(.plain)
                .lineLimit(1 ... 3)
                .focused($messageFocused)
                .padding(.horizontal, LoomSpacing.sm)
                .padding(.vertical, LoomSpacing.xs)
                .background(LoomColors.bgSecondary)
                .clipShape(RoundedRectangle(cornerRadius: 8))
                .overlay(
                    RoundedRectangle(cornerRadius: 8)
                        .strokeBorder(LoomColors.borderSubtle, lineWidth: 1)
                )
            Button {
                let text = messageText
                Task {
                    if await viewModel.sendMessage(text) {
                        HapticManager.light()
                        messageText = ""
                        messageFocused = false
                        onMutated()
                    }
                }
            } label: {
                Image(systemName: "paperplane.fill")
                    .font(.system(size: 14))
                    .foregroundStyle(canSend ? LoomColors.info : LoomColors.fgMuted)
            }
            .buttonStyle(.plain)
            .disabled(!canSend)
            .accessibilityLabel("Send message")
        }
    }

    private var canSend: Bool {
        !viewModel.isBusy
            && !messageText.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    // MARK: - Feedback

    @ViewBuilder
    private var feedbackSection: some View {
        if viewModel.isBusy {
            HStack(spacing: LoomSpacing.xs) {
                ProgressView().controlSize(.small)
                Text("Talking to the HUD…")
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.fgMuted)
            }
        }
        if let message = viewModel.resultMessage {
            Label(message, systemImage: "checkmark.circle")
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.statusHealthy)
        }
        if let error = viewModel.errorMessage {
            Label(error, systemImage: "exclamationmark.triangle")
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.statusCritical)
        }
    }

    // MARK: - Helpers

    private func sectionTitle(_ title: String) -> some View {
        Text(title.uppercased())
            .font(LoomTypography.sectionTitle)
            .tracking(LoomTypography.kindLabelTracking)
            .foregroundStyle(LoomColors.fgMuted)
    }

    private func controlButton(
        _ title: String,
        icon: String,
        color: Color,
        disabled: Bool = false,
        action: @escaping () -> Void
    ) -> some View {
        Button(action: action) {
            HStack(spacing: LoomSpacing.sm) {
                Image(systemName: icon)
                    .font(.system(size: 13))
                    .frame(width: 18)
                Text(title)
                    .font(LoomTypography.labelLarge)
                Spacer()
            }
            .foregroundStyle(disabled ? LoomColors.fgMuted : color)
            .padding(.horizontal, LoomSpacing.md)
            .padding(.vertical, LoomSpacing.sm)
            .background(LoomColors.bgSecondary)
            .clipShape(RoundedRectangle(cornerRadius: 10))
            .overlay(
                RoundedRectangle(cornerRadius: 10)
                    .strokeBorder(LoomColors.borderSubtle, lineWidth: 1)
            )
        }
        .buttonStyle(.plain)
        .disabled(disabled)
    }

    /// Dismiss, then hand any navigation to the deck AFTER the sheet is off
    /// screen so the tab switch doesn't fight the dismissal animation.
    private func finish(_ target: NavigationTarget? = nil) {
        dismiss()
        if let target {
            onNavigate(target)
        }
    }
}
