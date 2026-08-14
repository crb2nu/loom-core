import SwiftUI
import LoomCompanionKit

/// Sandbox section: the only mutation surface allowed on mobile.
///
/// Promoted out of Runtime so the start/stop control is first-class and
/// not buried alongside presence/claims/gateway diagnostics. Loads lazily
/// via `loadSectionIfNeeded(.sandbox)`.
struct OpsSandboxSection: View {
    @Bindable var viewModel: OpsViewModel

    @State private var startSandboxProject = ""
    @State private var startSandboxAgentID = ""
    @State private var showSandboxStartConfirmation = false

    var body: some View {
        LoomCard {
            VStack(alignment: .leading, spacing: LoomSpacing.sm) {
                Text("Sandbox / Devbox")
                    .font(LoomTypography.headlineMedium)
                    .foregroundStyle(LoomColors.textPrimary)

                Text("Scoped mobile mutations: sandbox start/stop only.")
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.textTertiary)

                TextField("Project (e.g. loom-core)", text: $startSandboxProject)
                    .textFieldStyle(.roundedBorder)
                    .autocorrectionDisabled()
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    #endif

                TextField("Agent ID (optional)", text: $startSandboxAgentID)
                    .textFieldStyle(.roundedBorder)
                    .autocorrectionDisabled()
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    #endif

                Button {
                    viewModel.clearMutationMessages()
                    showSandboxStartConfirmation = true
                } label: {
                    if viewModel.isMutatingSandbox {
                        ProgressView()
                            .frame(maxWidth: .infinity)
                    } else {
                        Label("Start Sandbox", systemImage: "play.circle")
                            .frame(maxWidth: .infinity)
                    }
                }
                .buttonStyle(.borderedProminent)
                .disabled(
                    startSandboxProject.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ||
                    viewModel.isMutatingSandbox
                )

                Divider()

                if let sandbox = viewModel.sandboxSummary {
                    if sandbox.available {
                        HStack {
                            sandboxMetric(label: "Running", value: sandbox.totalRunning, icon: "play.circle.fill", color: LoomColors.statusHealthy)
                            Spacer()
                            VStack(alignment: .leading, spacing: 2) {
                                Text(sandbox.backend)
                                    .font(LoomTypography.counterSmall)
                                    .foregroundStyle(LoomColors.textPrimary)
                                Text("Backend")
                                    .font(LoomTypography.caption)
                                    .foregroundStyle(LoomColors.textSecondary)
                            }
                        }

                        if sandbox.projects.isEmpty {
                            Text("No active sandboxes")
                                .font(LoomTypography.bodyRegular)
                                .foregroundStyle(LoomColors.textTertiary)
                        } else {
                            ForEach(sandbox.projects) { project in
                                HStack {
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(project.project)
                                            .font(LoomTypography.bodyMedium)
                                            .foregroundStyle(LoomColors.textPrimary)
                                        Text("\(project.status) \u{2022} \(project.agentId) \u{2022} \(project.uptime)")
                                            .font(LoomTypography.caption)
                                            .foregroundStyle(LoomColors.textSecondary)
                                    }
                                    Spacer()
                                    Button(role: .destructive) {
                                        Task { await viewModel.stopSandbox(project: project.project) }
                                    } label: {
                                        Image(systemName: "stop.circle")
                                    }
                                    .buttonStyle(.borderless)
                                    .disabled(viewModel.isMutatingSandbox)
                                }
                                .padding(.vertical, 2)
                            }
                        }
                    } else {
                        Text("Devbox unavailable")
                            .font(LoomTypography.bodyRegular)
                            .foregroundStyle(LoomColors.textTertiary)
                    }
                } else {
                    Text("Sandbox data unavailable")
                        .font(LoomTypography.bodyRegular)
                        .foregroundStyle(LoomColors.textTertiary)
                }

                if let msg = viewModel.sandboxMutationMessage {
                    Text(msg)
                        .font(LoomTypography.caption)
                        .foregroundStyle(LoomColors.textSecondary)
                }
                if let err = viewModel.sandboxMutationError {
                    Text(err)
                        .font(LoomTypography.caption)
                        .foregroundStyle(LoomColors.statusCritical)
                }
            }
        }
        .confirmationDialog("Start Sandbox?", isPresented: $showSandboxStartConfirmation, titleVisibility: .visible) {
            Button("Start Sandbox") {
                Task {
                    await viewModel.startSandbox(
                        project: startSandboxProject,
                        agentID: startSandboxAgentID
                    )
                }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("This triggers sandbox start/build for the selected project.")
        }
    }

    private func sandboxMetric(label: String, value: Int, icon: String, color: Color) -> some View {
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
}
