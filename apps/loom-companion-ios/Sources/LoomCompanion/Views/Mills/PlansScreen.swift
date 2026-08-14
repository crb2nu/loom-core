// PlansScreen — the Plan Store lifecycle board, on mobile.
//
// Mobile port of the HUD Work → Plans panel: plans grouped by lifecycle
// phase, each card carrying priority, slice progress, and respin lineage.
// The Spinning Room feeds this board (a spin lands a draft plan here), so
// in-flight spins pin to the top and the board polls while any spin is live.
//
// Mutations (spin, advance) require the paired bearer to be the HUD admin
// token; without it the board stays a useful read-only view and mutation
// failures surface as actionable copy (millsMutationFailureMessage).

import LoomCompanionKit
import SwiftUI

struct PlansScreen: View {
    let api: MillsControlAPIProtocol

    @State private var planList: MillsPlanList?
    @State private var spinRuns: [MillsSpinRun] = []
    @State private var loading = true
    @State private var loadError: String?
    @State private var showShipped = false

    @State private var spinSheet: SpinSheetRequest?
    @State private var banner: String?

    /// Identifiable wrapper so `.sheet(item:)` re-seeds per presentation.
    struct SpinSheetRequest: Identifiable {
        let id = UUID()
        let seed: SpinSeed?
    }

    private var plans: [MillsPlan] { planList?.plans ?? [] }
    private var boardPlans: [MillsPlan] {
        showShipped ? plans : plans.filter { !MillsPlanPhases.isTerminal($0.phase) }
    }

    private var phaseGroups: [(phase: String, plans: [MillsPlan])] {
        let grouped = Dictionary(grouping: boardPlans, by: { $0.phase.lowercased() })
        return grouped
            .sorted { MillsPlanPhases.sortIndex($0.key) < MillsPlanPhases.sortIndex($1.key) }
            .map { (phase: $0.key, plans: $0.value.sorted { ($0.updatedAt ?? "") > ($1.updatedAt ?? "") }) }
    }

    private var visibleSpins: [MillsSpinRun] {
        MillsSpinBoard.visibleRuns(spinRuns, terminalWindow: 2 * 3600, limit: 4)
    }

    private var isFirstLoad: Bool { loading && planList == nil && loadError == nil }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: LoomSpacing.lg) {
                if isFirstLoad {
                    loadingState
                } else if let err = loadError, planList == nil {
                    errorState(err)
                } else if planList?.available == false {
                    LoomEmptyState(
                        tone: .idle,
                        title: "Plan store not deployed",
                        detail: "The paired daemon predates the Plan Store — update loom-core to see the board."
                    )
                    .loomCard(priority: .standard)
                    .padding(.top, LoomSpacing.xxl)
                } else {
                    if let banner {
                        bannerView(banner)
                    }
                    if !visibleSpins.isEmpty {
                        spinningSection
                    }
                    if boardPlans.isEmpty {
                        LoomEmptyState(
                            tone: .idle,
                            title: "No active plans",
                            detail: "Spin one from a brief — the draft lands here for review."
                        ) {
                            Button {
                                spinSheet = SpinSheetRequest(seed: nil)
                            } label: {
                                Label("Spin a plan", systemImage: "arrow.triangle.2.circlepath")
                            }
                            .buttonStyle(.borderedProminent)
                            .tint(LoomColors.accent)
                        }
                        .loomCard(priority: .standard)
                    } else {
                        boardSections
                        footerControls
                    }
                }
            }
            .padding(.horizontal, LoomSpacing.lg)
            .padding(.vertical, LoomSpacing.lg)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(LoomColors.bgPrimary.ignoresSafeArea())
        .navigationTitle("Plans")
        .refreshable { await reload() }
        .task { await pollLoop() }
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button {
                    spinSheet = SpinSheetRequest(seed: nil)
                } label: {
                    Label("Spin", systemImage: "arrow.triangle.2.circlepath")
                }
                .tint(LoomColors.accent)
            }
        }
        .sheet(item: $spinSheet) { request in
            SpinPlanSheet(api: api, seed: request.seed) { spinID, frames in
                banner = "Spin queued on \(frames.joined(separator: " + ")) — the draft lands here when it finishes."
                Task { await reload() }
                _ = spinID
            }
        }
    }

    // MARK: - Spinning section

    private var spinningSection: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            sectionHeader("Spinning Room", count: visibleSpins.count)
            ForEach(visibleSpins) { run in
                SpinRunRow(run: run)
                    .loomCard(priority: .compact)
            }
        }
    }

    // MARK: - Board

    private var boardSections: some View {
        ForEach(phaseGroups, id: \.phase) { group in
            VStack(alignment: .leading, spacing: LoomSpacing.sm) {
                sectionHeader(
                    MillsPlanPhases.displayName(group.phase).capitalized,
                    count: group.plans.count,
                    color: PlanTonePalette.color(MillsPlanPhases.tone(for: group.phase))
                )
                ForEach(group.plans) { plan in
                    NavigationLink {
                        PlanDetailView(api: api, planID: plan.id, summary: plan) { seed in
                            spinSheet = SpinSheetRequest(seed: seed)
                        } onChanged: {
                            Task { await reload() }
                        }
                    } label: {
                        PlanCard(plan: plan, allPlans: plans)
                    }
                    .buttonStyle(.plain)
                }
            }
        }
    }

    private var footerControls: some View {
        HStack {
            Spacer(minLength: 0)
            Button {
                withAnimation(.easeInOut(duration: 0.2)) { showShipped.toggle() }
            } label: {
                Text(showShipped ? "Hide shipped & abandoned" : "Show shipped & abandoned")
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
            }
            Spacer(minLength: 0)
        }
        .padding(.top, LoomSpacing.xs)
    }

    private func sectionHeader(_ title: String, count: Int, color: Color = LoomColors.fgPrimary) -> some View {
        HStack(spacing: LoomSpacing.sm) {
            Text(title)
                .font(LoomTypography.headlineMedium)
                .foregroundStyle(LoomColors.fgPrimary)
            Text("\(count)")
                .font(LoomTypography.monoMedium)
                .foregroundStyle(color)
                .monospacedDigit()
                .padding(.horizontal, LoomSpacing.sm)
                .padding(.vertical, 2)
                .background(Capsule().fill(color.opacity(0.12)))
            Spacer(minLength: 0)
        }
        .accessibilityElement(children: .combine)
    }

    private func bannerView(_ text: String) -> some View {
        HStack(spacing: LoomSpacing.sm) {
            Image(systemName: "checkmark.circle.fill")
                .foregroundStyle(LoomColors.statusHealthy)
            Text(text)
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.fgSecondary)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
            Button {
                withAnimation { banner = nil }
            } label: {
                Image(systemName: "xmark")
                    .font(.caption2)
                    .foregroundStyle(LoomColors.fgMuted)
            }
        }
        .padding(LoomSpacing.sm)
        .background(
            RoundedRectangle(cornerRadius: 10)
                .fill(LoomColors.statusHealthy.opacity(0.08))
        )
    }

    // MARK: - States

    private var loadingState: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.md) {
            SkeletonView(width: 140, height: 18)
            ForEach(0..<3, id: \.self) { _ in
                SkeletonSessionRow().loomCard(priority: .standard)
            }
        }
    }

    private func errorState(_ message: String) -> some View {
        VStack(spacing: LoomSpacing.sm) {
            Image(systemName: "exclamationmark.triangle.fill")
                .font(.title3)
                .foregroundStyle(LoomColors.statusDegraded)
            Text("Couldn't load plans")
                .font(LoomTypography.headlineMedium)
                .foregroundStyle(LoomColors.fgPrimary)
            Text(message)
                .font(LoomTypography.monoSmall)
                .foregroundStyle(LoomColors.fgSecondary)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, LoomSpacing.lg)
        .loomCard(priority: .standard, accent: .severity(LoomColors.statusDegraded))
    }

    // MARK: - Data

    private func reload() async {
        loading = true
        async let plansTask: MillsPlanList? = try? await api.plans()
        async let spinsTask: [MillsSpinRun]? = try? await api.spinRuns(limit: 20)
        let fetchedPlans = await plansTask
        let fetchedSpins = await spinsTask
        if let fetchedPlans {
            planList = fetchedPlans
            loadError = nil
        } else if planList == nil {
            loadError = "Pull to retry."
        }
        if let fetchedSpins { spinRuns = fetchedSpins }
        loading = false
    }

    /// Reload once, then poll every 5s while any spin is live so a queued
    /// draft lands on the board without babysitting.
    private func pollLoop() async {
        await reload()
        while !Task.isCancelled {
            try? await Task.sleep(nanoseconds: 5_000_000_000)
            if Task.isCancelled { return }
            if MillsSpinBoard.hasLiveSpin(spinRuns) {
                await reload()
            }
        }
    }
}

// MARK: - Plan card

struct PlanCard: View {
    let plan: MillsPlan
    /// The full board, so respin lineage can name its source plan.
    var allPlans: [MillsPlan] = []

    private var tone: MillsPlanTone { MillsPlanPhases.tone(for: plan.phase) }
    private var color: Color { PlanTonePalette.color(tone) }
    private var progress: MillsSliceProgress? { MillsSliceProgress.build(from: plan.sliceSummary) }

    private var respunFromTitle: String? {
        guard let from = plan.respunFrom, !from.isEmpty else { return nil }
        return allPlans.first { $0.id == from }?.title ?? from
    }

    var body: some View {
        HStack(spacing: LoomSpacing.rowContentSpacing) {
            StatusAccentBar(color: color, isLive: tone == .active, prominent: true)
                .frame(minHeight: LoomSpacing.rowMinHeight)

            VStack(alignment: .leading, spacing: LoomSpacing.xxs) {
                HStack(spacing: LoomSpacing.xs) {
                    Text(plan.title)
                        .font(LoomTypography.bodyMedium)
                        .foregroundStyle(LoomColors.fgPrimary)
                        .lineLimit(2)
                    Spacer(minLength: LoomSpacing.xs)
                    if let priority = plan.priority, !priority.isEmpty {
                        LoomPill(priority, color: PlanTonePalette.priorityColor(priority), weight: .micro)
                    }
                }

                HStack(spacing: LoomSpacing.xs) {
                    LoomPill(
                        MillsPlanPhases.displayName(plan.phase),
                        color: color,
                        style: .outlined,
                        weight: .micro
                    )
                    if let project = plan.project, !project.isEmpty {
                        Text(project)
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.fgMuted)
                            .lineLimit(1)
                            .truncationMode(.head)
                    }
                    Spacer(minLength: 0)
                    if let updated = plan.updatedAt, !updated.isEmpty {
                        Text(LoomFormat.relativeCompact(fromISO: updated))
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.fgMuted)
                    }
                }

                if let progress {
                    SliceProgressBar(progress: progress)
                        .padding(.top, 2)
                }

                if let sourceTitle = respunFromTitle {
                    HStack(spacing: LoomSpacing.xxs) {
                        Image(systemName: "arrow.triangle.2.circlepath")
                            .font(.system(size: 9))
                        Text("respun from \(sourceTitle)")
                            .lineLimit(1)
                            .truncationMode(.middle)
                    }
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.tierShortTerm)
                }
            }
        }
        .padding(.vertical, LoomSpacing.xxs)
        .loomCard(priority: .standard)
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(plan.title), \(MillsPlanPhases.displayName(plan.phase))")
    }
}

// MARK: - Slice progress bar

/// A thin segmented bar (one segment per slice phase) with a merged/total
/// caption — the same read as the HUD card's progress strip.
struct SliceProgressBar: View {
    let progress: MillsSliceProgress

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            GeometryReader { geo in
                HStack(spacing: 1) {
                    ForEach(Array(progress.segments.enumerated()), id: \.offset) { _, segment in
                        RoundedRectangle(cornerRadius: 1)
                            .fill(PlanTonePalette.color(segment.tone))
                            .frame(width: max(3, geo.size.width * CGFloat(segment.count) / CGFloat(progress.total)))
                    }
                }
            }
            .frame(height: 4)
            .clipShape(Capsule())

            Text("\(progress.merged)/\(progress.total) slices merged")
                .font(LoomTypography.monoCaption)
                .foregroundStyle(LoomColors.fgMuted)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(progress.merged) of \(progress.total) slices merged")
    }
}

// MARK: - Spin run row

/// One async spin on the board: status, frames, brief headline, outcome.
struct SpinRunRow: View {
    let run: MillsSpinRun

    private var statusColor: Color {
        switch run.statusKind {
        case .pending: return LoomColors.statusIdle
        case .running: return LoomColors.statusActive
        case .succeeded: return LoomColors.statusHealthy
        case .failed: return LoomColors.statusCritical
        case .timeout: return LoomColors.statusDegraded
        case .unknown: return LoomColors.fgMuted
        }
    }

    private var statusLine: String {
        switch run.statusKind {
        case .pending: return "queued"
        case .running: return "spinning…"
        case .succeeded:
            let n = run.planIDs.count
            return n == 1 ? "1 draft landed" : "\(n) drafts landed"
        case .failed: return "failed"
        case .timeout: return "timed out"
        case .unknown: return run.status
        }
    }

    var body: some View {
        HStack(alignment: .top, spacing: LoomSpacing.sm) {
            PulsingDot(
                color: statusColor,
                size: 7,
                isPulsing: !run.statusKind.isTerminal
            )
            .padding(.top, 4)

            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: LoomSpacing.xs) {
                    Text(run.frames.joined(separator: " + "))
                        .font(LoomTypography.monoMedium)
                        .foregroundStyle(LoomColors.fgPrimary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    if run.competitive {
                        LoomPill("competitive", color: LoomColors.tierShortTerm, style: .outlined, weight: .micro)
                    }
                    Spacer(minLength: 0)
                    if let started = run.startedAt {
                        Text(LoomFormat.relativeCompact(seconds: Int(Date().timeIntervalSince(started))))
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.fgMuted)
                    }
                }
                Text(run.briefHeadline)
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgSecondary)
                    .lineLimit(1)
                    .truncationMode(.tail)
                HStack(spacing: LoomSpacing.xs) {
                    Text(statusLine)
                        .font(LoomTypography.monoCaption)
                        .foregroundStyle(statusColor)
                    if let error = run.error, !error.isEmpty {
                        Text(error)
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.fgMuted)
                            .lineLimit(1)
                            .truncationMode(.tail)
                    }
                }
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("Spin on \(run.frames.joined(separator: " and ")), \(statusLine)")
    }
}

// MARK: - Tone → color

/// Maps Kit-side semantic tones to the app's design tokens. Lives in the app
/// target because LoomColors does.
enum PlanTonePalette {
    static func color(_ tone: MillsPlanTone) -> Color {
        switch tone {
        case .draft: return LoomColors.tierShortTerm
        case .active: return LoomColors.statusActive
        case .review: return LoomColors.statusDegraded
        case .shipped: return LoomColors.statusHealthy
        case .abandoned: return LoomColors.statusCritical
        case .unknown: return LoomColors.fgMuted
        }
    }

    static func priorityColor(_ priority: String) -> Color {
        switch priority.uppercased() {
        case "P0": return LoomColors.statusCritical
        case "P1": return LoomColors.statusDegraded
        case "P2": return LoomColors.info
        default: return LoomColors.fgMuted
        }
    }
}

#Preview("Plans · board") {
    NavigationStack {
        PlansScreen(api: PlansScreenPreviewAPI())
    }
    .preferredColorScheme(.dark)
}

struct PlansScreenPreviewAPI: MillsControlAPIProtocol {
    func spinningRoom() async throws -> MillsSpinningRoom? {
        MillsSpinningRoom(frames: [
            MillsFrame(name: "jacquard", model: "claude-opus-4-8", backend: "anthropic"),
            MillsFrame(name: "warp", model: "gpt-5.4", backend: "openai"),
        ])
    }

    func spinRuns(limit: Int) async throws -> [MillsSpinRun] {
        [
            MillsSpinRun(
                id: "spin-1", brief: "Harden the GitLab importer against 5xx.",
                frames: ["jacquard"], status: "running",
                startedAt: Date().addingTimeInterval(-90)
            ),
            MillsSpinRun(
                id: "spin-2", brief: "Add retry budget dashboards.",
                frames: ["jacquard", "warp"], status: "succeeded",
                planIDs: ["plan-9", "plan-10"], competitive: true,
                startedAt: Date().addingTimeInterval(-1500),
                endedAt: Date().addingTimeInterval(-1200)
            ),
        ]
    }

    func spinRun(id: String) async throws -> MillsSpinRun? { nil }
    func spinAsync(_ request: MillsSpinRequest) async throws -> String { "spin-preview" }

    func plans() async throws -> MillsPlanList {
        MillsPlanList(plans: [
            MillsPlan(
                id: "plan-1", title: "Async spins — durable Spinning Room",
                project: "services/loom-core", phase: "in_progress", priority: "P1",
                sliceSummary: ["merged": 2, "implementing": 1, "pending": 1],
                updatedAt: "2026-07-04T09:00:00Z"
            ),
            MillsPlan(
                id: "plan-2", title: "Retry budget dashboards",
                project: "services/loom-core", phase: "draft",
                respunFrom: "plan-1",
                sliceSummary: ["pending": 3],
                updatedAt: "2026-07-04T11:20:00Z"
            ),
            MillsPlan(
                id: "plan-3", title: "HUD token sweep",
                project: "services/loom-core", phase: "merged",
                sliceSummary: ["merged": 4],
                updatedAt: "2026-07-01T10:00:00Z"
            ),
        ])
    }

    func plan(id: String) async throws -> MillsPlan? {
        MillsPlan(
            id: id, title: "Async spins — durable Spinning Room",
            project: "services/loom-core", phase: "in_progress", priority: "P1",
            slices: [
                MillsPlanSlice(id: "s1", name: "202 + background goroutine", phase: "merged",
                               files: ["cmd/loom-mills-operator/handlers_spin_async.go"]),
                MillsPlanSlice(id: "s2", name: "HUD wiring — board polls in", phase: "implementing",
                               files: ["internal/hud/frontend/src/lib/components/PlansPanel.svelte"],
                               mrRef: "!924"),
            ],
            sliceSummary: ["merged": 1, "implementing": 1],
            updatedAt: "2026-07-04T09:00:00Z"
        )
    }

    func advancePlan(id: String, toPhase: String) async throws {}
    func escalatePipeline(id: String, reason: String?) async throws -> MillsPipelineEscalateAck {
        MillsPipelineEscalateAck(runID: id, state: "escalated")
    }
}
