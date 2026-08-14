import LoomCompanionKit
import SwiftUI

@available(iOS 26.0, *)
struct AppleIntelligenceBriefingCard: View {
    let snapshot: LoomBriefingSnapshot

    @State private var briefing: String?
    @State private var errorMessage: String?
    @State private var isGenerating = false
    @State private var showingAskLoom = false

    private let service = AppleIntelligenceBriefingService()

    var body: some View {
        LoomCard(priority: .standard, accent: .severity(LoomColors.info)) {
            VStack(alignment: .leading, spacing: LoomSpacing.md) {
                HStack(spacing: LoomSpacing.sm) {
                    Image(systemName: "sparkles")
                        .foregroundStyle(LoomColors.info)
                    Text("APPLE INTELLIGENCE")
                        .font(LoomTypography.kindLabel)
                        .tracking(LoomTypography.kindLabelTracking)
                        .foregroundStyle(LoomColors.info)
                    Spacer()
                    Text("ON DEVICE")
                        .font(LoomTypography.monoSmall)
                        .foregroundStyle(LoomColors.fgMuted)
                }

                if let briefing {
                    Text(briefing)
                        .font(LoomTypography.bodyRegular)
                        .foregroundStyle(LoomColors.fgPrimary)
                        .textSelection(.enabled)
                } else if let errorMessage {
                    Text(errorMessage)
                        .font(LoomTypography.bodyRegular)
                        .foregroundStyle(LoomColors.fgSecondary)
                } else {
                    Text("Turn the current fleet, session, and task snapshot into a private operator briefing.")
                        .font(LoomTypography.bodyRegular)
                        .foregroundStyle(LoomColors.fgSecondary)
                }

                HStack(spacing: LoomSpacing.sm) {
                    Button {
                        Task { await generateBriefing() }
                    } label: {
                        if isGenerating {
                            ProgressView()
                                .controlSize(.small)
                        } else {
                            Label(briefing == nil ? "Brief Me" : "Refresh", systemImage: "waveform.badge.magnifyingglass")
                        }
                    }
                    .buttonStyle(.bordered)
                    .disabled(isGenerating || service.availability != .available)

                    Button {
                        showingAskLoom = true
                    } label: {
                        Label("Ask Loom", systemImage: "bubble.left.and.text.bubble.right")
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(LoomColors.info)
                    .disabled(isGenerating || service.availability != .available)
                }

                if service.availability != .available {
                    Text("Requires Apple Intelligence to be enabled and ready on this device.")
                        .font(LoomTypography.caption)
                        .foregroundStyle(LoomColors.fgMuted)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .onChange(of: snapshot) { _, _ in
            briefing = nil
            errorMessage = nil
        }
        .sheet(isPresented: $showingAskLoom) {
            AskLoomSheet(snapshot: snapshot)
        }
    }

    @MainActor
    private func generateBriefing() async {
        isGenerating = true
        errorMessage = nil
        defer { isGenerating = false }

        do {
            briefing = try await service.generate(from: snapshot)
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}
