import LoomCompanionKit
import SwiftUI

@available(iOS 26.0, *)
struct AskLoomSheet: View {
    let snapshot: LoomBriefingSnapshot

    @Environment(\.dismiss) private var dismiss
    @FocusState private var questionFocused: Bool
    @State private var question = ""
    @State private var answer: String?
    @State private var errorMessage: String?
    @State private var isAsking = false

    private let service = AppleIntelligenceQueryService()
    private let suggestions = [
        "What needs my attention?",
        "Are any servers unhealthy?",
        "How much work is blocked?",
    ]

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: LoomSpacing.lg) {
                    privacyHeader
                    questionComposer

                    if let answer {
                        answerCard(answer)
                            .transition(.opacity.combined(with: .move(edge: .bottom)))
                    } else if let errorMessage {
                        errorCard(errorMessage)
                    } else {
                        suggestionList
                    }
                }
                .padding(.horizontal, LoomSpacing.lg)
                .padding(.vertical, LoomSpacing.lg)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .background(LoomColors.bgPrimary.ignoresSafeArea())
            .navigationTitle("Ask Loom")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    Button("Done") { dismiss() }
                        .disabled(isAsking)
                }
            }
        }
        .preferredColorScheme(.dark)
        .interactiveDismissDisabled(isAsking)
    }

    private var privacyHeader: some View {
        HStack(alignment: .top, spacing: LoomSpacing.sm) {
            Image(systemName: "lock.shield.fill")
                .font(.system(size: 18, weight: .semibold))
                .foregroundStyle(LoomColors.info)

            VStack(alignment: .leading, spacing: LoomSpacing.xxs) {
                Text("READ-ONLY · ON DEVICE")
                    .font(LoomTypography.kindLabel)
                    .tracking(LoomTypography.kindLabelTracking)
                    .foregroundStyle(LoomColors.info)
                Text("Answers use only the current fleet, session, task, and attention snapshot. Ask Loom cannot run commands or change state.")
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.fgSecondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    private var questionComposer: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            TextField("What do you want to know?", text: $question, axis: .vertical)
                .font(LoomTypography.bodyRegular)
                .foregroundStyle(LoomColors.fgPrimary)
                .lineLimit(2...5)
                .focused($questionFocused)
                .submitLabel(.send)
                .onSubmit { submitQuestion() }
                .onChange(of: question) { _, newValue in
                    if newValue.count > LoomOperatorQuestion.maximumLength {
                        question = String(newValue.prefix(LoomOperatorQuestion.maximumLength))
                    }
                }
                .padding(LoomSpacing.md)
                .background(
                    RoundedRectangle(cornerRadius: 12, style: .continuous)
                        .fill(LoomColors.bgTertiary)
                )
                .overlay(
                    RoundedRectangle(cornerRadius: 12, style: .continuous)
                        .strokeBorder(questionFocused ? LoomColors.info.opacity(0.7) : LoomColors.border, lineWidth: 1)
                )
                .disabled(isAsking)

            Button(action: submitQuestion) {
                HStack(spacing: LoomSpacing.sm) {
                    if isAsking {
                        ProgressView()
                            .controlSize(.small)
                    } else {
                        Image(systemName: "sparkles")
                    }
                    Text(isAsking ? "Reading Snapshot" : "Ask On Device")
                        .font(LoomTypography.labelLarge)
                    Spacer()
                    if !isAsking {
                        Image(systemName: "arrow.up.circle.fill")
                    }
                }
                .foregroundStyle(LoomColors.bgPrimary)
                .padding(.horizontal, LoomSpacing.md)
                .padding(.vertical, LoomSpacing.sm + 2)
                .background(
                    RoundedRectangle(cornerRadius: 10, style: .continuous)
                        .fill(LoomColors.info)
                )
            }
            .buttonStyle(.plain)
            .disabled(isAsking || LoomOperatorQuestion(question) == nil || service.availability != .available)
            .opacity(LoomOperatorQuestion(question) == nil ? 0.55 : 1)
        }
    }

    private var suggestionList: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            Text("TRY ASKING")
                .font(LoomTypography.sectionTitle)
                .tracking(0.8)
                .foregroundStyle(LoomColors.fgSecondary)

            ForEach(suggestions, id: \.self) { suggestion in
                Button {
                    question = suggestion
                    questionFocused = true
                } label: {
                    HStack(spacing: LoomSpacing.sm) {
                        Image(systemName: "text.bubble")
                            .foregroundStyle(LoomColors.info)
                        Text(suggestion)
                            .font(LoomTypography.bodyRegular)
                            .foregroundStyle(LoomColors.fgPrimary)
                        Spacer()
                        Image(systemName: "arrow.up.left")
                            .font(.system(size: 11, weight: .semibold))
                            .foregroundStyle(LoomColors.fgMuted)
                    }
                    .padding(LoomSpacing.md)
                    .background(
                        RoundedRectangle(cornerRadius: 10, style: .continuous)
                            .fill(LoomColors.bgSecondary)
                    )
                    .overlay(
                        RoundedRectangle(cornerRadius: 10, style: .continuous)
                            .strokeBorder(LoomColors.borderSubtle, lineWidth: 1)
                    )
                }
                .buttonStyle(.plain)
            }
        }
    }

    private func answerCard(_ text: String) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            HStack(spacing: LoomSpacing.xs) {
                Image(systemName: "sparkles")
                    .foregroundStyle(LoomColors.info)
                Text("GROUNDED ANSWER")
                    .font(LoomTypography.kindLabel)
                    .tracking(LoomTypography.kindLabelTracking)
                    .foregroundStyle(LoomColors.info)
            }
            Text(text)
                .font(LoomTypography.bodyRegular)
                .foregroundStyle(LoomColors.fgPrimary)
                .textSelection(.enabled)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .loomCard(priority: .standard, accent: .severity(LoomColors.info))
    }

    private func errorCard(_ message: String) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            Label("Couldn’t answer", systemImage: "exclamationmark.bubble.fill")
                .font(LoomTypography.labelLarge)
                .foregroundStyle(LoomColors.statusDegraded)
            Text(message)
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.fgSecondary)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .loomCard(priority: .standard, accent: .severity(LoomColors.statusDegraded))
    }

    private func submitQuestion() {
        guard !isAsking, LoomOperatorQuestion(question) != nil else { return }
        Task { await ask() }
    }

    @MainActor
    private func ask() async {
        isAsking = true
        answer = nil
        errorMessage = nil
        questionFocused = false
        defer { isAsking = false }

        do {
            let response = try await service.answer(question, from: snapshot)
            withAnimation(.easeOut(duration: 0.25)) {
                answer = response
            }
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}
