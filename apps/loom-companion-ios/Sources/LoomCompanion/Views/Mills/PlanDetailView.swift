// PlanDetailView — one plan's lifecycle, slices, and controls.
//
// Renders the list-row summary immediately, then enriches with the full
// record (slices) from GET /api/plans/{id}. Actions mirror the HUD drawer:
// advance the lifecycle phase (server enforces legality — 422 surfaces as
// copy, not a crash), respin the plan or a single slice into the Spinning
// Room, and supersede (respin's one-click "advance the old plan to
// abandoned" companion).

import LoomCompanionKit
import SwiftUI

struct PlanDetailView: View {
    let api: MillsControlAPIProtocol
    let planID: String
    /// Cached list row shown while the detail fetch is in flight.
    var summary: MillsPlan?
    /// Hands a seeded respin up to the board, which owns the spin sheet.
    var onRespin: (SpinSeed) -> Void
    /// Fired after any successful mutation so the board can refresh.
    var onChanged: () -> Void

    @State private var plan: MillsPlan?
    @State private var actionError: String?
    @State private var busy = false
    @State private var confirmAbandon = false

    private var current: MillsPlan? { plan ?? summary }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: LoomSpacing.lg) {
                if let p = current {
                    headerCard(p)
                    if let err = actionError {
                        errorNote(err)
                    }
                    actionsRow(p)
                    slicesSection(p)
                    metaSection(p)
                } else {
                    SkeletonSessionRow().loomCard(priority: .standard)
                }
            }
            .padding(.horizontal, LoomSpacing.lg)
            .padding(.vertical, LoomSpacing.lg)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(LoomColors.bgPrimary.ignoresSafeArea())
        .navigationTitle("Plan")
        .navigationBarTitleDisplayMode(.inline)
        .task { plan = try? await api.plan(id: planID) }
        .refreshable {
            plan = try? await api.plan(id: planID)
            HapticManager.light()
        }
        .confirmationDialog(
            "Abandon this plan?",
            isPresented: $confirmAbandon,
            titleVisibility: .visible
        ) {
            Button("Abandon plan", role: .destructive) {
                Task { await advance(to: "abandoned") }
            }
        } message: {
            Text("Marks the plan abandoned — usually after a respin's fresh draft replaced it. This drops its queued Mills work.")
        }
    }

    // MARK: - Header

    private func headerCard(_ p: MillsPlan) -> some View {
        let tone = MillsPlanPhases.tone(for: p.phase)
        let color = PlanTonePalette.color(tone)
        return LoomCard(priority: .standard, accent: .severity(color, pulse: tone == .active)) {
            VStack(alignment: .leading, spacing: LoomSpacing.sm) {
                Text(p.title)
                    .font(LoomTypography.headlineMedium)
                    .foregroundStyle(LoomColors.fgPrimary)
                    .fixedSize(horizontal: false, vertical: true)

                HStack(spacing: LoomSpacing.xs) {
                    LoomPill(MillsPlanPhases.displayName(p.phase), color: color, weight: .compact)
                    if let priority = p.priority, !priority.isEmpty {
                        LoomPill(priority, color: PlanTonePalette.priorityColor(priority), weight: .compact)
                    }
                    if let kill = p.killTestStatus, !kill.isEmpty {
                        LoomPill(
                            "kill-test \(kill)",
                            color: kill.lowercased() == "passed"
                                ? LoomColors.statusHealthy : LoomColors.statusDegraded,
                            style: .outlined,
                            weight: .micro
                        )
                    }
                    Spacer(minLength: 0)
                }

                if let progress = MillsSliceProgress.build(from: p.sliceSummary) {
                    SliceProgressBar(progress: progress)
                }

                VStack(alignment: .leading, spacing: 2) {
                    if let project = p.project, !project.isEmpty {
                        metaLine(icon: "folder", text: project)
                    }
                    if let from = p.respunFrom, !from.isEmpty {
                        metaLine(icon: "arrow.triangle.2.circlepath", text: "respun from \(from)")
                    }
                    if let backlog = p.millsBacklogID, !backlog.isEmpty {
                        metaLine(icon: "tray.full", text: "Mills backlog \(backlog)")
                    }
                    if let updated = p.updatedAt, !updated.isEmpty {
                        metaLine(icon: "clock", text: "updated \(LoomFormat.relative(fromISO: updated))")
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func metaLine(icon: String, text: String) -> some View {
        HStack(spacing: LoomSpacing.xxs) {
            Image(systemName: icon)
                .font(.system(size: 10))
                .foregroundStyle(LoomColors.fgMuted)
                .frame(width: 14)
            Text(text)
                .font(LoomTypography.monoCaption)
                .foregroundStyle(LoomColors.fgSecondary)
                .lineLimit(1)
                .truncationMode(.middle)
        }
    }

    // MARK: - Actions

    private func actionsRow(_ p: MillsPlan) -> some View {
        HStack(spacing: LoomSpacing.sm) {
            Menu {
                ForEach(advanceTargets(for: p), id: \.self) { target in
                    Button {
                        if target == "abandoned" {
                            confirmAbandon = true
                        } else {
                            Task { await advance(to: target) }
                        }
                    } label: {
                        Label(
                            MillsPlanPhases.displayName(target),
                            systemImage: target == "abandoned" ? "xmark.circle" : "arrow.right.circle"
                        )
                    }
                }
            } label: {
                actionChip(icon: "arrow.right.circle", label: "Advance")
            }
            .disabled(busy)

            Button {
                onRespin(.forPlan(p))
            } label: {
                actionChip(icon: "arrow.triangle.2.circlepath", label: "Respin")
            }
            .disabled(busy)

            if busy {
                ProgressView().controlSize(.small)
            }
            Spacer(minLength: 0)
        }
    }

    private func actionChip(icon: String, label: String) -> some View {
        HStack(spacing: LoomSpacing.xxs) {
            Image(systemName: icon)
                .font(.system(size: 12))
            Text(label)
                .font(LoomTypography.labelLarge)
        }
        .padding(.horizontal, LoomSpacing.md)
        .padding(.vertical, LoomSpacing.xs)
        .foregroundStyle(LoomColors.accent)
        .background(
            Capsule().strokeBorder(LoomColors.accent.opacity(0.5), lineWidth: 1)
        )
        .contentShape(Capsule())
    }

    /// Everything but the current phase; the store rejects illegal jumps.
    private func advanceTargets(for p: MillsPlan) -> [String] {
        MillsPlanPhases.advanceTargets.filter { $0 != p.phase.lowercased() }
    }

    // MARK: - Slices

    @ViewBuilder private func slicesSection(_ p: MillsPlan) -> some View {
        let slices = (p.slices ?? []).sorted { ($0.order ?? 0) < ($1.order ?? 0) }
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            HStack(spacing: LoomSpacing.sm) {
                Text("Slices")
                    .font(LoomTypography.headlineMedium)
                    .foregroundStyle(LoomColors.fgPrimary)
                if !slices.isEmpty {
                    Text("\(slices.count)")
                        .font(LoomTypography.monoMedium)
                        .foregroundStyle(LoomColors.fgSecondary)
                        .monospacedDigit()
                }
                Spacer(minLength: 0)
            }

            if slices.isEmpty {
                LoomEmptyState(
                    tone: .idle,
                    title: plan == nil ? "Loading slices…" : "No slices yet",
                    detail: plan == nil ? nil : "Respin the plan to decompose it into shippable slices."
                )
                .loomCard(priority: .compact)
            } else {
                LoomCard(priority: .standard) {
                    VStack(spacing: 0) {
                        ForEach(Array(slices.enumerated()), id: \.element.id) { index, slice in
                            if index > 0 {
                                Divider().overlay(LoomColors.borderSubtle)
                            }
                            sliceRow(slice, of: p)
                        }
                    }
                }
            }
        }
    }

    private func sliceRow(_ slice: MillsPlanSlice, of p: MillsPlan) -> some View {
        let tone = MillsSliceProgress.sliceTone(slice.phase)
        let color = PlanTonePalette.color(tone)
        return HStack(alignment: .top, spacing: LoomSpacing.sm) {
            StatusAccentBar(color: color, isLive: tone == .active, prominent: false)
                .frame(minHeight: 36)

            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: LoomSpacing.xs) {
                    Text(slice.name)
                        .font(LoomTypography.bodyMedium)
                        .foregroundStyle(LoomColors.fgPrimary)
                        .lineLimit(2)
                    Spacer(minLength: LoomSpacing.xs)
                    LoomPill(
                        MillsPlanPhases.displayName(slice.phase),
                        color: color,
                        style: .outlined,
                        weight: .micro
                    )
                }
                HStack(spacing: LoomSpacing.xs) {
                    if let files = slice.files, !files.isEmpty {
                        Text("\(files.count) file\(files.count == 1 ? "" : "s")")
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.fgMuted)
                    }
                    if let mr = slice.mrRef, !mr.isEmpty {
                        Text(mr)
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.info)
                    }
                    if let agent = slice.assignedAgentID, !agent.isEmpty {
                        Text(agent)
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.fgMuted)
                            .lineLimit(1)
                            .truncationMode(.middle)
                    }
                    Spacer(minLength: 0)
                    Button {
                        onRespin(.forSlice(slice, of: p))
                    } label: {
                        Image(systemName: "arrow.triangle.2.circlepath")
                            .font(.system(size: 12))
                            .foregroundStyle(LoomColors.accent)
                            .padding(4)
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel("Respin slice \(slice.name)")
                }
            }
        }
        .padding(.vertical, LoomSpacing.xs)
    }

    // MARK: - Meta

    @ViewBuilder private func metaSection(_ p: MillsPlan) -> some View {
        if let mrs = p.mrRefs, !mrs.isEmpty {
            VStack(alignment: .leading, spacing: LoomSpacing.xs) {
                Text("Merge requests")
                    .font(LoomTypography.sectionTitle)
                    .tracking(0.8)
                    .foregroundStyle(LoomColors.fgSecondary)
                ForEach(mrs, id: \.self) { mr in
                    Text(mr)
                        .font(LoomTypography.monoSmall)
                        .foregroundStyle(LoomColors.info)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
            }
            .loomCard(priority: .compact)
        }
    }

    private func errorNote(_ text: String) -> some View {
        Text(text)
            .font(LoomTypography.monoSmall)
            .foregroundStyle(LoomColors.statusCritical)
            .padding(LoomSpacing.sm)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 8).fill(LoomColors.statusCritical.opacity(0.08)))
    }

    // MARK: - Mutations

    private func advance(to phase: String) async {
        busy = true
        actionError = nil
        do {
            try await api.advancePlan(id: planID, toPhase: phase)
            HapticManager.success()
            plan = try? await api.plan(id: planID)
            onChanged()
        } catch {
            HapticManager.error()
            actionError = millsMutationFailureMessage(error)
        }
        busy = false
    }
}

#Preview("Plan · detail") {
    NavigationStack {
        PlanDetailView(
            api: PlansScreenPreviewAPI(),
            planID: "plan-1",
            onRespin: { _ in },
            onChanged: {}
        )
    }
    .preferredColorScheme(.dark)
}
