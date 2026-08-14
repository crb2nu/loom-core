// SpinPlanSheet — the Spinning Room, on mobile.
//
// Mobile port of the HUD's SpinPlanDialog (Live Beam slice 3): pick 1–3 model
// frames, hand them a brief, and the operator spins draft plan(s) into the
// Plan Store. Fires ASYNCHRONOUSLY (POST /api/mills/spin/async → 202 +
// spin_id) — a frontier frame runs minutes, so the sheet never holds the
// request open; the caller tracks the spin on the board via onQueued.
//
// A respin seeds the brief + scope from an existing plan/slice (see
// MillsRespinBrief) and threads `respunFrom` so the fresh draft links back.

import LoomCompanionKit
import SwiftUI

/// Pre-fill for a respin; nil = a fresh spin with empty fields.
struct SpinSeed {
    var brief: String
    var project: String
    var namespace: String
    var priority: String
    var label: String
    var respunFrom: String

    static func forPlan(_ plan: MillsPlan) -> SpinSeed {
        SpinSeed(
            brief: MillsRespinBrief.forPlan(plan),
            project: plan.project ?? "",
            namespace: plan.namespace ?? "",
            priority: plan.priority ?? "",
            label: "Respin plan",
            respunFrom: plan.id
        )
    }

    static func forSlice(_ slice: MillsPlanSlice, of plan: MillsPlan) -> SpinSeed {
        SpinSeed(
            brief: MillsRespinBrief.forSlice(slice, of: plan),
            project: plan.project ?? "",
            namespace: plan.namespace ?? "",
            priority: plan.priority ?? "",
            label: "Respin slice",
            respunFrom: plan.id
        )
    }
}

struct SpinPlanSheet: View {
    let api: MillsControlAPIProtocol
    var seed: SpinSeed?
    /// Called once the spin is ACCEPTED (202) — not when the draft is ready.
    var onQueued: (_ spinID: String, _ frames: [String]) -> Void

    @Environment(\.dismiss) private var dismiss

    @State private var room: MillsSpinningRoom?
    @State private var loadingRoom = true
    @State private var roomError: String?

    @State private var brief = ""
    @State private var selectedFrames: [String] = []
    @State private var priority = ""
    @State private var project = ""
    @State private var namespace = ""
    @State private var showScope = false

    @State private var busy = false
    @State private var submitError: String?

    private var frames: [MillsFrame] { room?.frames ?? [] }

    private var canSpin: Bool {
        guard let room, room.unavailableReason == nil else { return false }
        return !busy
            && !selectedFrames.isEmpty
            && !brief.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: LoomSpacing.lg) {
                    header
                    if let note = statusNote {
                        noteView(note, isError: roomError != nil)
                    }
                    briefField
                    framesField
                    priorityField
                    scopeFields
                    spinHint
                    if let submitError {
                        noteView(submitError, isError: true)
                    }
                }
                .padding(.horizontal, LoomSpacing.lg)
                .padding(.vertical, LoomSpacing.md)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .background(LoomColors.bgPrimary.ignoresSafeArea())
            .navigationTitle(seed?.label ?? "Spin a plan")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                        .disabled(busy)
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button(action: { Task { await submit() } }) {
                        if busy {
                            ProgressView().controlSize(.small)
                        } else {
                            Text(selectedFrames.count > 1 ? "Spin \(selectedFrames.count)" : "Spin")
                                .fontWeight(.semibold)
                        }
                    }
                    .disabled(!canSpin)
                    .tint(LoomColors.accent)
                }
            }
            .task { await loadRoom() }
        }
        .preferredColorScheme(.dark)
    }

    // MARK: - Sections

    private var header: some View {
        Text(seed != nil
            ? "Redoing an existing plan — review the seeded brief, pick a frame, and it spins a fresh draft to compare."
            : "Pick a model frame and hand it a brief — it spins a draft plan onto the board for review.")
            .font(LoomTypography.caption)
            .foregroundStyle(LoomColors.fgSecondary)
            .fixedSize(horizontal: false, vertical: true)
    }

    private var statusNote: String? {
        if let roomError { return "Couldn't load frames: \(roomError)" }
        if loadingRoom { return nil }
        guard let room else { return "Operator can't reach the Spinning Room right now." }
        return room.unavailableReason
    }

    private func noteView(_ text: String, isError: Bool) -> some View {
        Text(text)
            .font(LoomTypography.monoSmall)
            .foregroundStyle(isError ? LoomColors.statusCritical : LoomColors.fgSecondary)
            .padding(LoomSpacing.sm)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(
                RoundedRectangle(cornerRadius: 8)
                    .fill(LoomColors.bgTertiary)
            )
    }

    private var briefField: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xxs) {
            fieldLabel("BRIEF (ROVING)")
            TextEditor(text: $brief)
                .font(LoomTypography.monoSmall)
                .foregroundStyle(LoomColors.fgPrimary)
                .scrollContentBackground(.hidden)
                .frame(minHeight: 120, maxHeight: 220)
                .padding(LoomSpacing.xs)
                .background(
                    RoundedRectangle(cornerRadius: 10)
                        .fill(LoomColors.bgTertiary)
                )
                .overlay(
                    RoundedRectangle(cornerRadius: 10)
                        .strokeBorder(LoomColors.borderSubtle, lineWidth: 1)
                )
                .disabled(busy)
                .overlay(alignment: .topLeading) {
                    if brief.isEmpty {
                        Text("What should this plan accomplish? e.g. Harden the GitLab importer against 5xx with retries + tests.")
                            .font(LoomTypography.monoSmall)
                            .foregroundStyle(LoomColors.fgMuted)
                            .padding(.horizontal, LoomSpacing.sm)
                            .padding(.vertical, LoomSpacing.sm + 2)
                            .allowsHitTesting(false)
                    }
                }
        }
    }

    private var framesField: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xs) {
            HStack(spacing: LoomSpacing.xs) {
                fieldLabel("FRAMES")
                Spacer(minLength: 0)
                Text("pick 2+ to spin competitively (max \(MillsSpinRequest.maxCompetitiveFrames))")
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
            }
            if loadingRoom {
                HStack(spacing: LoomSpacing.sm) {
                    SkeletonView(width: 120, height: 32)
                    SkeletonView(width: 120, height: 32)
                }
            } else if frames.isEmpty {
                Text("— none —")
                    .font(LoomTypography.monoSmall)
                    .foregroundStyle(LoomColors.fgMuted)
            } else {
                VStack(spacing: LoomSpacing.xs) {
                    ForEach(frames) { frame in
                        frameChip(frame)
                    }
                }
            }
        }
    }

    private func frameChip(_ frame: MillsFrame) -> some View {
        let picked = selectedFrames.contains(frame.name)
        let atCap = !picked && selectedFrames.count >= MillsSpinRequest.maxCompetitiveFrames
        return Button {
            toggleFrame(frame.name)
            HapticManager.selection()
        } label: {
            HStack(spacing: LoomSpacing.sm) {
                Image(systemName: picked ? "checkmark.circle.fill" : "circle")
                    .font(.system(size: 16))
                    .foregroundStyle(picked ? LoomColors.accent : LoomColors.fgMuted)
                VStack(alignment: .leading, spacing: 1) {
                    Text(frame.name)
                        .font(LoomTypography.monoMedium)
                        .foregroundStyle(picked ? LoomColors.fgPrimary : LoomColors.fgSecondary)
                    Text("\(frame.model) · \(frame.displayBackend)")
                        .font(LoomTypography.monoCaption)
                        .foregroundStyle(LoomColors.fgMuted)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                Spacer(minLength: 0)
            }
            .padding(.horizontal, LoomSpacing.md)
            .padding(.vertical, LoomSpacing.sm)
            .background(
                RoundedRectangle(cornerRadius: 10)
                    .fill(picked ? LoomColors.accent.opacity(0.10) : LoomColors.bgTertiary)
            )
            .overlay(
                RoundedRectangle(cornerRadius: 10)
                    .strokeBorder(
                        picked ? LoomColors.accent.opacity(0.5) : LoomColors.borderSubtle,
                        lineWidth: 1
                    )
            )
            .opacity(atCap || busy ? 0.5 : 1)
        }
        .buttonStyle(.plain)
        .disabled(atCap || busy)
        .accessibilityLabel("\(frame.name), \(frame.model)")
        .accessibilityAddTraits(picked ? [.isSelected] : [])
    }

    private var priorityField: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xxs) {
            fieldLabel("PRIORITY")
            Picker("Priority", selection: $priority) {
                Text("default (\(room?.defaultPriority ?? "P2"))").tag("")
                ForEach(MillsPlanPhases.priorities, id: \.self) { p in
                    Text(p).tag(p)
                }
            }
            .pickerStyle(.segmented)
            .disabled(busy)
        }
    }

    private var scopeFields: some View {
        DisclosureGroup(isExpanded: $showScope) {
            VStack(alignment: .leading, spacing: LoomSpacing.sm) {
                scopeField("Project", placeholder: "services/loom-core", text: $project)
                scopeField("Namespace", placeholder: "mills/spun", text: $namespace)
            }
            .padding(.top, LoomSpacing.xs)
        } label: {
            HStack(spacing: LoomSpacing.xs) {
                fieldLabel("SCOPE")
                Text("optional")
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
            }
        }
        .tint(LoomColors.fgSecondary)
    }

    private func scopeField(_ label: String, placeholder: String, text: Binding<String>) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(label)
                .font(LoomTypography.monoCaption)
                .foregroundStyle(LoomColors.fgSecondary)
            TextField(placeholder, text: text)
                .font(LoomTypography.monoSmall)
                .foregroundStyle(LoomColors.fgPrimary)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .padding(LoomSpacing.sm)
                .background(
                    RoundedRectangle(cornerRadius: 8)
                        .fill(LoomColors.bgTertiary)
                )
                .disabled(busy)
        }
    }

    @ViewBuilder private var spinHint: some View {
        let picked = frames.filter { selectedFrames.contains($0.name) }
        if picked.count == 1 {
            Text("Spinning on **\(picked[0].model)**. The draft lands in **draft** — advance it to *planned* to feed the beam.")
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.fgSecondary)
        } else if picked.count > 1 {
            Text("Competitive spin: the same roving goes to **\(picked.map(\.model).joined(separator: " vs "))** — one draft per frame. Keep the better yarn and leave the rest in draft.")
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.fgSecondary)
        } else {
            Text("Spins run in the background — the draft appears on the board when it lands (a frontier frame can take a few minutes).")
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.fgMuted)
                .italic()
        }
    }

    private func fieldLabel(_ text: String) -> some View {
        Text(text)
            .font(LoomTypography.sectionTitle)
            .tracking(0.8)
            .foregroundStyle(LoomColors.fgSecondary)
    }

    // MARK: - Behavior

    private func toggleFrame(_ name: String) {
        if let idx = selectedFrames.firstIndex(of: name) {
            selectedFrames.remove(at: idx)
        } else if selectedFrames.count < MillsSpinRequest.maxCompetitiveFrames {
            selectedFrames.append(name)
        }
    }

    private func loadRoom() async {
        // Seed the form once; the room fetch can race a re-render, so fields
        // are only overwritten while still untouched.
        if brief.isEmpty, let seed {
            brief = seed.brief
            project = seed.project
            namespace = seed.namespace
            priority = seed.priority
            showScope = !(seed.project.isEmpty && seed.namespace.isEmpty)
        }
        loadingRoom = true
        roomError = nil
        do {
            room = try await api.spinningRoom()
        } catch {
            roomError = millsMutationFailureMessage(error)
        }
        loadingRoom = false
        // Drop picks the policy no longer allows; seed from the first frame
        // when nothing valid remains — mirrors the HUD dialog.
        selectedFrames = selectedFrames.filter { name in frames.contains { $0.name == name } }
        if selectedFrames.isEmpty, let first = frames.first {
            selectedFrames = [first.name]
        }
    }

    private func submit() async {
        let b = brief.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !b.isEmpty, !selectedFrames.isEmpty else { return }
        busy = true
        submitError = nil
        let request = MillsSpinRequest(
            brief: b,
            frames: selectedFrames,
            priority: priority,
            project: project,
            namespace: namespace,
            respunFrom: seed?.respunFrom
        )
        do {
            let spinID = try await api.spinAsync(request)
            HapticManager.success()
            onQueued(spinID, selectedFrames)
            dismiss()
        } catch {
            HapticManager.error()
            submitError = millsMutationFailureMessage(error)
        }
        busy = false
    }
}

#Preview("Spin · fresh") {
    SpinPlanSheet(api: SpinSheetPreviewAPI(), onQueued: { _, _ in })
}

private struct SpinSheetPreviewAPI: MillsControlAPIProtocol {
    func spinningRoom() async throws -> MillsSpinningRoom? {
        MillsSpinningRoom(frames: [
            MillsFrame(name: "jacquard", model: "claude-opus-4-8", backend: "anthropic"),
            MillsFrame(name: "warp", model: "gpt-5.4", backend: "openai"),
            MillsFrame(name: "weft", model: "qwen3-coder", backend: "flexinfer"),
        ])
    }

    func spinRuns(limit: Int) async throws -> [MillsSpinRun] { [] }
    func spinRun(id: String) async throws -> MillsSpinRun? { nil }
    func spinAsync(_ request: MillsSpinRequest) async throws -> String { "spin-preview" }
    func plans() async throws -> MillsPlanList { MillsPlanList() }
    func plan(id: String) async throws -> MillsPlan? { nil }
    func advancePlan(id: String, toPhase: String) async throws {}
    func escalatePipeline(id: String, reason: String?) async throws -> MillsPipelineEscalateAck {
        MillsPipelineEscalateAck(runID: id, state: "escalated")
    }
}
