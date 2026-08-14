// MillsScreen — the autonomous software factory, on mobile.
//
// Visual thesis: an operations console for the Mills pipeline. It leads with
// the north-star (autonomous merges / 24h), backs it with the canonical KPI
// grid, and shows in-flight pipelines as a depth-aware root→slice tree that
// pulses while work is live and falls calm when the floor is idle.
//
// KPIs and pipelines stay read-only; the Spinning Room section carries the
// screen's one mutation (fire an async spin) and the toolbar links out to
// the Plans board. Semantics (which metric keys are canonical, how a run's
// free-form state buckets, spin-board filtering) live in
// `LoomCompanionKit/Mills/` so they're unit-tested and shared with any future
// surface — this file is presentation only.

import LoomCompanionKit
import SwiftUI

public struct MillsScreen: View {
    @State private var pipelineRuns: [MillsPipelineRun] = []
    @State private var kpi: MillsKPISnapshot?
    @State private var spinRuns: [MillsSpinRun] = []
    @State private var loading = true
    @State private var loadError: String?
    /// True when the last failed read was an auth/permission problem
    /// (401/403) rather than connectivity. Drives distinct copy: a bad
    /// operator token is a fixable configuration problem, not a flaky floor.
    @State private var loadErrorIsAuth = false
    @State private var showSpinSheet = false
    @State private var showShiftReport = false

    // Pipeline escalate (from the Mills widget's per-pipeline button). The
    // widget deep-links `loom://pipeline/{id}/escalate`; ContentView routes it
    // here by setting `pendingEscalatePipelineID`, which drives a confirm sheet.
    @State private var actionBanner: String?
    @State private var actionBannerIsError = false

    /// Stashed when initialized with `apiClient:` so the toolbar "Weaver"
    /// button can build the WeaverAPI without the connection VM. Nil under the
    /// test initializer (toolbar button hidden).
    private let apiClientForLinks: APIClient?
    private let api: MillsAPIProtocol?
    /// Spinning Room + Plans control surface; nil hides those affordances
    /// (test initializer without a control fake).
    private let controlAPI: MillsControlAPIProtocol?
    /// Set by a widget escalate deep link; non-nil pops the confirm sheet.
    private let pendingEscalatePipelineID: Binding<String?>

    /// Production initializer. When the user hasn't paired with a HUD,
    /// `connectionVM.buildAPIClient()` returns nil and the screen renders a
    /// "pair to view Mills" empty state.
    public init(
        apiClient: APIClient?,
        pendingEscalatePipelineID: Binding<String?> = .constant(nil)
    ) {
        self.api = apiClient.map(MillsAPI.init(client:))
        self.controlAPI = apiClient.map(MillsControlAPI.init(client:))
        self.apiClientForLinks = apiClient
        self.pendingEscalatePipelineID = pendingEscalatePipelineID
    }

    /// Test-only initializer accepting fake protocol implementations.
    public init(
        api: MillsAPIProtocol?,
        controlAPI: MillsControlAPIProtocol? = nil,
        pendingEscalatePipelineID: Binding<String?> = .constant(nil)
    ) {
        self.api = api
        self.controlAPI = controlAPI
        self.apiClientForLinks = nil
        self.pendingEscalatePipelineID = pendingEscalatePipelineID
    }

    // MARK: - Derived

    private var summary: MillsSystemSummary? { kpi?.systemSummary }
    private var cards: [MillsKPICard] { kpi?.canonicalCards ?? [] }
    private var inFlight: [MillsPipelineRun] { pipelineRuns.filter { !$0.category.isTerminal } }
    private var trees: [MillsPipelineTree.Node] { MillsPipelineTree.build(from: inFlight) }
    private var isFirstLoad: Bool { loading && kpi == nil && pipelineRuns.isEmpty && loadError == nil }

    /// Nothing loaded at all AND the runs read failed. Previously this fell
    /// through to the normal layout, where a zeroed hero + "No pipelines
    /// running" made a total outage look like a quiet, healthy floor.
    private var isTotalOutage: Bool { loadError != nil && kpi == nil && pipelineRuns.isEmpty }

    public var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: LoomSpacing.lg) {
                if api == nil {
                    disconnectedState
                } else if isFirstLoad {
                    loadingState
                } else if isTotalOutage {
                    floorErrorState
                } else {
                    if let actionBanner { escalateBanner(actionBanner) }
                    heroCard
                    shiftReportButton
                    if !cards.isEmpty { kpiGrid }
                    if controlAPI != nil { spinningRoomSection }
                    pipelinesSection
                    if let snapshotAt = kpi?.snapshotAt {
                        Text("Snapshot \(LoomFormat.relative(from: snapshotAt))")
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.fgMuted)
                            .frame(maxWidth: .infinity, alignment: .center)
                            .padding(.top, LoomSpacing.xs)
                    }
                }
            }
            .padding(.horizontal, LoomSpacing.lg)
            .padding(.vertical, LoomSpacing.lg)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(LoomColors.bgPrimary.ignoresSafeArea())
        .navigationTitle("Mills")
        .refreshable { await reload() }
        .task { await pollLoop() }
        .toolbar {
            if let controlAPI {
                ToolbarItem(placement: .topBarTrailing) {
                    NavigationLink {
                        PlansScreen(api: controlAPI)
                    } label: {
                        Label("Plans", systemImage: "square.stack.3d.up")
                    }
                    .tint(LoomColors.accent)
                }
            }
            if apiClientForLinks != nil {
                ToolbarItem(placement: .topBarTrailing) {
                    NavigationLink {
                        WeaverScreen(apiClient: apiClientForLinks)
                    } label: {
                        Label("Weaver", systemImage: "circles.hexagonpath")
                    }
                    .tint(LoomColors.info)
                }
            }
        }
        .sheet(isPresented: $showSpinSheet) {
            if let controlAPI {
                SpinPlanSheet(api: controlAPI, seed: nil) { _, _ in
                    Task { await reload() }
                }
            }
        }
        .sheet(isPresented: $showShiftReport) {
            if let api {
                ShiftReportSheet(api: api)
            }
        }
        .confirmationDialog(
            "Escalate pipeline?",
            isPresented: escalateConfirmPresented,
            presenting: pendingEscalatePipelineID.wrappedValue
        ) { runID in
            Button("Escalate — hold for review", role: .destructive) {
                Task { await escalate(runID: runID) }
            }
            Button("Cancel", role: .cancel) {
                pendingEscalatePipelineID.wrappedValue = nil
            }
        } message: { runID in
            Text("Pulls \(runID) out of the autonomous loop and flags it for human review. This ends the run — it won't auto-merge.")
        }
    }

    // MARK: - Escalate (widget button → confirm → act)

    /// Drives the confirm sheet: present while a pending run id is set; a
    /// dismiss (cancel/swipe) clears it so the sheet doesn't re-pop.
    private var escalateConfirmPresented: Binding<Bool> {
        Binding(
            get: { pendingEscalatePipelineID.wrappedValue != nil },
            set: { presented in
                if !presented { pendingEscalatePipelineID.wrappedValue = nil }
            }
        )
    }

    private func escalateBanner(_ text: String) -> some View {
        HStack(spacing: LoomSpacing.sm) {
            Image(systemName: actionBannerIsError ? "exclamationmark.triangle.fill" : "checkmark.circle.fill")
                .foregroundStyle(actionBannerIsError ? LoomColors.statusCritical : LoomColors.statusHealthy)
            Text(text)
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.fgSecondary)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
            Button {
                withAnimation { actionBanner = nil }
            } label: {
                Image(systemName: "xmark")
                    .font(.caption2)
                    .foregroundStyle(LoomColors.fgMuted)
            }
            .accessibilityLabel("Dismiss")
        }
        .padding(LoomSpacing.sm)
        .background(
            RoundedRectangle(cornerRadius: 10)
                .fill((actionBannerIsError ? LoomColors.statusCritical : LoomColors.statusHealthy).opacity(0.08))
        )
    }

    private func escalate(runID: String) async {
        pendingEscalatePipelineID.wrappedValue = nil
        guard let controlAPI else {
            actionBannerIsError = true
            actionBanner = "Not connected — can't escalate \(runID)."
            return
        }
        do {
            let ack = try await controlAPI.escalatePipeline(id: runID, reason: nil)
            HapticManager.success()
            actionBannerIsError = false
            actionBanner = "Escalated \(ack.runID) — pulled from the autonomous loop for review."
            await reload()
        } catch {
            HapticManager.error()
            actionBannerIsError = true
            actionBanner = millsMutationFailureMessage(error)
        }
    }

    // MARK: - Shift report

    /// Opens the end-of-shift story — the same deterministic narrative the
    /// web Factory panel's "shift report" overlay tells, share-sheet ready.
    private var shiftReportButton: some View {
        Button {
            showShiftReport = true
        } label: {
            HStack(spacing: LoomSpacing.xs) {
                Image(systemName: "doc.plaintext")
                    .font(.system(size: 12))
                    .foregroundStyle(LoomColors.accent)
                Text("Shift report")
                    .font(LoomTypography.labelLarge)
                    .foregroundStyle(LoomColors.fgPrimary)
                Text("the last 24h, told straight")
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
                    .lineLimit(1)
                Spacer(minLength: 0)
                Image(systemName: "chevron.right")
                    .font(.system(size: 11, weight: .semibold))
                    .foregroundStyle(LoomColors.fgMuted)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            // With .plain button style only the rendered glyphs hit-test — the
            // Spacer stretch (most of the card) swallowed taps. Make the whole
            // row the tap target.
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .loomCard(priority: .compact)
        .accessibilityLabel("Open shift report — the last 24 hours")
    }

    // MARK: - Spinning Room

    private var visibleSpins: [MillsSpinRun] {
        MillsSpinBoard.visibleRuns(spinRuns, terminalWindow: 6 * 3600, limit: 3)
    }

    private var spinningRoomSection: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            HStack(spacing: LoomSpacing.sm) {
                Text("Spinning Room")
                    .font(LoomTypography.headlineMedium)
                    .foregroundStyle(LoomColors.fgPrimary)
                Spacer(minLength: 0)
                Button {
                    showSpinSheet = true
                } label: {
                    HStack(spacing: LoomSpacing.xxs) {
                        Image(systemName: "arrow.triangle.2.circlepath")
                            .font(.system(size: 12))
                        Text("Spin a plan")
                            .font(LoomTypography.labelLarge)
                    }
                    .padding(.horizontal, LoomSpacing.md)
                    .padding(.vertical, LoomSpacing.xs)
                    .foregroundStyle(LoomColors.accent)
                    .background(
                        Capsule().strokeBorder(LoomColors.accent.opacity(0.5), lineWidth: 1)
                    )
                    // Stroke-only background: the capsule interior is
                    // transparent and .plain buttons only hit-test drawn
                    // pixels — cover the whole capsule.
                    .contentShape(Capsule())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Spin a plan")
            }

            if visibleSpins.isEmpty {
                Text("Hand a frame a brief — it spins a draft plan onto the board.")
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
            } else {
                ForEach(visibleSpins) { run in
                    SpinRunRow(run: run)
                        .loomCard(priority: .compact)
                }
            }
        }
    }

    // MARK: - Hero (north-star)

    private var heroCard: some View {
        LoomCard(priority: .hero, accent: heroAccent) {
            VStack(alignment: .leading, spacing: LoomSpacing.md) {
                HStack(spacing: LoomSpacing.xs) {
                    PulsingDot(
                        color: heroDotColor,
                        size: 7,
                        isPulsing: summary?.isBusy ?? false
                    )
                    Text("AUTONOMOUS FACTORY")
                        .font(LoomTypography.sectionTitle)
                        .tracking(LoomTypography.kindLabelTracking)
                        .foregroundStyle(LoomColors.fgSecondary)
                    Spacer(minLength: 0)
                    Text("24H")
                        .font(LoomTypography.monoCaption)
                        .foregroundStyle(LoomColors.fgMuted)
                }

                HStack(alignment: .firstTextBaseline, spacing: LoomSpacing.sm) {
                    if let summary {
                        AnimatedCounter(
                            summary.mergedRuns,
                            font: LoomTypography.counterLarge,
                            color: LoomColors.fgPrimary
                        )
                        Text(summary.mergedRuns == 1 ? "merge" : "merges")
                            .font(LoomTypography.headlineMedium)
                            .foregroundStyle(LoomColors.fgSecondary)
                    } else {
                        Text("—")
                            .font(LoomTypography.counterLarge)
                            .foregroundStyle(LoomColors.fgMuted)
                        Text("merges")
                            .font(LoomTypography.headlineMedium)
                            .foregroundStyle(LoomColors.fgMuted)
                    }
                    Spacer(minLength: 0)
                }

                if let summary {
                    HStack(spacing: LoomSpacing.xs) {
                        LoomPill(
                            "\(summary.activeRuns) running",
                            icon: "bolt.fill",
                            color: summary.activeRuns > 0 ? LoomColors.statusActive : LoomColors.fgMuted,
                            style: summary.activeRuns > 0 ? .tinted : .outlined
                        )
                        LoomPill(
                            "\(summary.queueDepth) queued",
                            icon: "tray.full.fill",
                            color: summary.queueDepth > 0 ? LoomColors.statusDegraded : LoomColors.fgMuted,
                            style: summary.queueDepth > 0 ? .tinted : .outlined
                        )
                    }
                }

                Text(heroSubtitle)
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.fgMuted)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private var heroAccent: LoomCardAccent {
        guard let s = summary else { return .none }
        if s.activeRuns > 0 { return .severity(LoomColors.statusActive, pulse: true) }
        if s.queueDepth > 0 { return .severity(LoomColors.statusDegraded) }
        if s.mergedRuns > 0 { return .severity(LoomColors.statusHealthy) }
        return .none
    }

    private var heroDotColor: Color {
        guard let s = summary else { return LoomColors.fgMuted }
        if s.activeRuns > 0 { return LoomColors.statusActive }
        if s.queueDepth > 0 { return LoomColors.statusDegraded }
        if s.mergedRuns > 0 { return LoomColors.statusHealthy }
        return LoomColors.fgMuted
    }

    private var heroSubtitle: String {
        guard let s = summary else { return "Waiting for the first KPI snapshot." }
        if s.activeRuns > 0 {
            let q = s.queueDepth > 0 ? " · \(s.queueDepth) waiting in the queue" : ""
            return "Mills is building right now\(q)."
        }
        if s.queueDepth > 0 {
            return "\(s.queueDepth) item\(s.queueDepth == 1 ? "" : "s") queued — pipelines spin up shortly."
        }
        if s.mergedRuns > 0 {
            return "Floor is idle. \(s.mergedRuns) autonomous merge\(s.mergedRuns == 1 ? "" : "s") landed in the last 24h."
        }
        return "Floor is idle. No autonomous merges yet in this window."
    }

    // MARK: - KPI grid

    private var kpiGrid: some View {
        LazyVGrid(
            columns: [
                GridItem(.flexible(), spacing: LoomSpacing.md),
                GridItem(.flexible(), spacing: LoomSpacing.md),
            ],
            spacing: LoomSpacing.md
        ) {
            ForEach(Array(cards.enumerated()), id: \.element.id) { index, card in
                kpiTile(card).cardAppear(index: index)
            }
        }
    }

    private func kpiTile(_ card: MillsKPICard) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xs) {
            HStack(spacing: LoomSpacing.xxs) {
                Text(card.label.uppercased())
                    .font(LoomTypography.sectionTitle)
                    .tracking(0.8)
                    .foregroundStyle(LoomColors.fgSecondary)
                    .lineLimit(1)
                    .minimumScaleFactor(0.7)
                Spacer(minLength: 2)
                if card.status != .neutral {
                    Circle()
                        .fill(statusColor(card.status))
                        .frame(width: 6, height: 6)
                        .shadow(color: statusColor(card.status).opacity(0.6), radius: 4)
                        .accessibilityLabel(statusLabel(card.status))
                }
            }
            Text(card.value)
                .font(LoomTypography.counterMedium)
                .foregroundStyle(LoomColors.fgPrimary)
                .monospacedDigit()
                .lineLimit(1)
                .minimumScaleFactor(0.6)
            if card.proxy {
                Text("proxy")
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .loomCard(priority: .compact)
    }

    // MARK: - Pipelines

    private var pipelinesSection: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            HStack(spacing: LoomSpacing.sm) {
                Text("In-flight pipelines")
                    .font(LoomTypography.headlineMedium)
                    .foregroundStyle(LoomColors.fgPrimary)
                Spacer(minLength: 0)
                if !inFlight.isEmpty {
                    Text("\(inFlight.count)")
                        .font(LoomTypography.monoMedium)
                        .foregroundStyle(LoomColors.statusActive)
                        .monospacedDigit()
                        .padding(.horizontal, LoomSpacing.sm)
                        .padding(.vertical, 2)
                        .background(
                            Capsule().fill(LoomColors.statusActive.opacity(0.12))
                        )
                }
            }
            .accessibilityElement(children: .combine)

            if let err = loadError, inFlight.isEmpty {
                errorInline(err)
            } else if inFlight.isEmpty {
                LoomEmptyState(
                    tone: .nominal,
                    title: "No pipelines running",
                    detail: "Idle mills — autonomous work appears here the moment it spins up."
                )
                .loomCard(priority: .compact)
            } else {
                ForEach(Array(trees.enumerated()), id: \.element.id) { index, node in
                    treeCard(node).cardAppear(index: min(index, 6))
                }
            }
        }
    }

    private func treeCard(_ node: MillsPipelineTree.Node) -> some View {
        LoomCard(priority: .standard) {
            VStack(spacing: 0) {
                pipelineRow(node.run, isChild: false)
                ForEach(node.children) { child in
                    Divider()
                        .overlay(LoomColors.borderSubtle)
                        .padding(.leading, childIndent(child))
                    pipelineRow(child, isChild: true)
                }
            }
        }
    }

    /// Indent a child by its depth so d2 slices nest under d1 visually.
    private func childIndent(_ run: MillsPipelineRun) -> CGFloat {
        let depth = max(1, run.depth ?? 1)
        return CGFloat(min(depth, 3)) * 14
    }

    private func pipelineRow(_ run: MillsPipelineRun, isChild: Bool) -> some View {
        let category = run.category
        let color = categoryColor(category)
        return HStack(spacing: LoomSpacing.rowContentSpacing) {
            StatusAccentBar(color: color, isLive: category.isLive, prominent: !isChild)
                .frame(minHeight: isChild ? 36 : LoomSpacing.rowMinHeight)

            VStack(alignment: .leading, spacing: LoomSpacing.xxs) {
                HStack(spacing: LoomSpacing.xs) {
                    LoomRowIcon(systemName: categoryIcon(category), color: color)
                    Text(run.id)
                        .font(isChild ? LoomTypography.monoSmall : LoomTypography.monoMedium)
                        .foregroundStyle(isChild ? LoomColors.fgSecondary : LoomColors.fgPrimary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Spacer(minLength: LoomSpacing.xs)
                    LoomPill(
                        run.displayState,
                        color: color,
                        weight: isChild ? .micro : .compact
                    )
                }
                HStack(spacing: LoomSpacing.xs) {
                    if let depth = run.depth, depth > 0 {
                        LoomPill("d\(depth)", color: LoomColors.accent, style: .outlined, weight: .micro)
                    } else if !isChild {
                        LoomPill("root", color: LoomColors.fgMuted, style: .outlined, weight: .micro)
                    }
                    Text(run.template)
                        .font(LoomTypography.monoCaption)
                        .foregroundStyle(LoomColors.fgMuted)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    if run.attempts > 1 {
                        Text("· attempt \(run.attempts)")
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.statusDegraded)
                    }
                    Spacer(minLength: 0)
                    if let started = run.startedAt {
                        Text(LoomFormat.relativeCompact(seconds: Int(Date().timeIntervalSince(started))))
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.fgMuted)
                    }
                }
            }
        }
        .padding(.vertical, LoomSpacing.xxs)
        .loomValueChangeFlash(run.state, color: color)
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(run.id), \(run.displayState), \(run.template)")
    }

    // MARK: - States

    private var disconnectedState: some View {
        LoomEmptyState(
            tone: .idle,
            title: "Mills not connected",
            detail: "Pair with a Loom HUD to watch the autonomous factory — merges, costs, and in-flight pipelines."
        )
        .loomCard(priority: .standard)
        .padding(.top, LoomSpacing.xxl)
    }

    /// Screen-level failure state for a total outage — the whole floor is
    /// unreadable, so we say that instead of rendering an empty-looking mill.
    private var floorErrorState: some View {
        VStack(spacing: LoomSpacing.sm) {
            Image(systemName: loadErrorIsAuth ? "lock.trianglebadge.exclamationmark.fill" : "exclamationmark.triangle.fill")
                .font(.title2)
                .foregroundStyle(loadErrorIsAuth ? LoomColors.statusCritical : LoomColors.statusDegraded)
            Text(loadErrorIsAuth ? "Mills access denied" : "Couldn't reach Mills")
                .font(LoomTypography.headlineMedium)
                .foregroundStyle(LoomColors.fgPrimary)
            Text(loadError ?? "")
                .font(LoomTypography.monoSmall)
                .foregroundStyle(LoomColors.fgSecondary)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
            Button {
                Task { await reload() }
            } label: {
                Label("Try again", systemImage: "arrow.clockwise")
                    .font(LoomTypography.labelLarge)
            }
            .buttonStyle(.borderedProminent)
            .tint(LoomColors.accent)
            .disabled(loading)
            .padding(.top, LoomSpacing.xs)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, LoomSpacing.lg)
        .loomCard(
            priority: .standard,
            accent: .severity(loadErrorIsAuth ? LoomColors.statusCritical : LoomColors.statusDegraded)
        )
        .padding(.top, LoomSpacing.xxl)
        .accessibilityElement(children: .combine)
    }

    private var loadingState: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.lg) {
            SkeletonHeroCard()
            LazyVGrid(
                columns: [
                    GridItem(.flexible(), spacing: LoomSpacing.md),
                    GridItem(.flexible(), spacing: LoomSpacing.md),
                ],
                spacing: LoomSpacing.md
            ) {
                ForEach(0..<4, id: \.self) { _ in
                    VStack(alignment: .leading, spacing: LoomSpacing.xs) {
                        SkeletonView(width: 70, height: 9)
                        SkeletonView(width: 56, height: 20)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .loomCard(priority: .compact)
                }
            }
            ForEach(0..<2, id: \.self) { _ in
                SkeletonSessionRow().loomCard(priority: .standard)
            }
        }
    }

    private func errorInline(_ message: String) -> some View {
        VStack(spacing: LoomSpacing.sm) {
            Image(systemName: "exclamationmark.triangle.fill")
                .font(.title3)
                .foregroundStyle(LoomColors.statusDegraded)
            Text("Couldn't reach Mills")
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

    // MARK: - Semantics → color

    private func statusColor(_ status: MillsMetricStatus) -> Color {
        switch status {
        case .onTarget: return LoomColors.statusHealthy
        case .watch: return LoomColors.statusDegraded
        case .offTarget: return LoomColors.statusCritical
        case .neutral: return LoomColors.fgMuted
        }
    }

    private func statusLabel(_ status: MillsMetricStatus) -> String {
        switch status {
        case .onTarget: return "on target"
        case .watch: return "watch"
        case .offTarget: return "off target"
        case .neutral: return ""
        }
    }

    private func categoryColor(_ category: MillsRunCategory) -> Color {
        switch category {
        case .queued: return LoomColors.statusIdle
        case .running: return LoomColors.statusActive
        case .review: return LoomColors.tierShortTerm
        case .merging: return LoomColors.accent
        case .escalated: return LoomColors.statusDegraded
        case .failed: return LoomColors.statusCritical
        case .done: return LoomColors.statusHealthy
        case .unknown: return LoomColors.fgMuted
        }
    }

    private func categoryIcon(_ category: MillsRunCategory) -> String {
        switch category {
        case .queued: return "tray.and.arrow.down.fill"
        case .running: return "hammer.fill"
        case .review: return "checkmark.shield.fill"
        case .merging: return "arrow.triangle.merge"
        case .escalated: return "exclamationmark.triangle.fill"
        case .failed: return "xmark.octagon.fill"
        case .done: return "checkmark.circle.fill"
        case .unknown: return "questionmark.circle.fill"
        }
    }

    // MARK: - Reload

    private func reload() async {
        guard let api else {
            loading = false
            return
        }
        loading = true
        async let snapTask: MillsKPISnapshot? = try? await api.latestKPI(window: "1d")
        async let spinsTask: [MillsSpinRun]? = fetchSpins()

        var runs: [MillsPipelineRun]?
        var runsError: LoomAPIError?
        do {
            runs = try await api.pipelineRuns()
        } catch {
            runsError = error as? LoomAPIError ?? .networkError(underlying: error.localizedDescription)
        }

        let snap = await snapTask
        let spins = await spinsTask
        if let runs {
            pipelineRuns = runs
            loadError = nil
            loadErrorIsAuth = false
        } else if pipelineRuns.isEmpty {
            // A total outage should read as an outage, not as an idle floor —
            // so this surfaces at screen level (see `floorErrorState`), and
            // 401/403 gets its own actionable copy.
            loadErrorIsAuth = Self.isAuthFailure(runsError)
            loadError = loadErrorIsAuth
                ? "Mills refused the request. Check your operator token in Settings."
                : "Couldn't reach the mill floor. Pull to retry."
        }
        // Keep the last good snapshot across transient KPI failures so the
        // hero/grid don't blink back to "—" on a single dropped poll.
        if let snap { kpi = snap }
        if let spins { spinRuns = spins }
        loading = false
    }

    /// 401/403 — the pairing bearer is missing, revoked, or not allowed to
    /// reach `/api/mills/*`. Distinct from connectivity failures, which the
    /// operator fixes by waiting, not by re-pairing.
    private static func isAuthFailure(_ error: LoomAPIError?) -> Bool {
        guard case let .apiError(code, _, _) = error else { return false }
        return code == .unauthorized || code == .tokenRevoked || code == .forbidden
    }

    /// nil when the screen has no control surface (test init without a fake);
    /// [] on any fetch failure so a flaky operator can't wedge the section.
    private func fetchSpins() async -> [MillsSpinRun]? {
        guard let controlAPI else { return nil }
        return (try? await controlAPI.spinRuns(limit: 10)) ?? []
    }

    /// Reload once, then poll every 5s while a spin is live so a queued
    /// draft's landing shows without babysitting the screen.
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

#Preview("Mills · busy") {
    NavigationStack {
        MillsScreen(api: MillsScreenPreviewAPI(state: .busy))
    }
    .preferredColorScheme(.dark)
}

#Preview("Mills · idle") {
    NavigationStack {
        MillsScreen(api: MillsScreenPreviewAPI(state: .idle))
    }
    .preferredColorScheme(.dark)
}

private struct MillsScreenPreviewAPI: MillsAPIProtocol, Sendable {
    enum State { case busy, idle }
    let state: State

    func pipelineRuns() async throws -> [MillsPipelineRun] {
        guard state == .busy else { return [] }
        let now = Date()
        return [
            MillsPipelineRun(id: "PIPE-7f3a", backlogID: "BACK-A", template: "mills-default",
                             state: "implementing", attempts: 1,
                             startedAt: now.addingTimeInterval(-320), depth: 0),
            MillsPipelineRun(id: "PIPE-7f3a-S1", backlogID: "BACK-A-1", template: "mills-default",
                             state: "testing", attempts: 1,
                             startedAt: now.addingTimeInterval(-140), parentRunID: "PIPE-7f3a", depth: 1),
            MillsPipelineRun(id: "PIPE-7f3a-S2", backlogID: "BACK-A-2", template: "mills-default",
                             state: "reviewing", attempts: 2,
                             startedAt: now.addingTimeInterval(-60), parentRunID: "PIPE-7f3a", depth: 1),
            MillsPipelineRun(id: "PIPE-91b2", backlogID: "BACK-B", template: "mills-council",
                             state: "queued", attempts: 1,
                             startedAt: now.addingTimeInterval(-12), depth: 0),
        ]
    }

    func terminalRuns(limit: Int) async throws -> [MillsPipelineRun] {
        guard state == .busy else { return [] }
        let now = Date()
        return [
            MillsPipelineRun(id: "PIPE-1a2b", backlogID: "psl-factory-shift", template: "implement",
                             state: "merged", attempts: 1,
                             endedAt: now.addingTimeInterval(-3 * 3600), costUSD: 2.10),
            MillsPipelineRun(id: "PIPE-3c4d", backlogID: "psl-gate-tiebreak", template: "implement",
                             state: "escalated", attempts: 3,
                             endedAt: now.addingTimeInterval(-7 * 3600), costUSD: 1.34),
        ]
    }

    func runDetail(id: String) async throws -> MillsPipelineRunDetail? {
        MillsPipelineRunDetail(gates: [
            MillsGateOutcome(gateName: "judge_gate", outcome: "fail"),
            MillsGateOutcome(gateName: "lint", outcome: "pass"),
        ])
    }

    func backlog() async throws -> [MillsBacklogItem] { [] }

    func approvedPatterns() async throws -> [MillsPatternInfo] { [] }

    func latestKPI(window: String) async throws -> MillsKPISnapshot? {
        MillsKPISnapshot(
            snapshotAt: Date().addingTimeInterval(-45),
            windowSeconds: 86_400,
            metrics: state == .busy
                ? [
                    "pipeline_merged_runs": 6,
                    "queue_depth": 1,
                    "active_pipeline_runs": 3,
                    "auto_merge_rate": 0.86,
                    "cost_per_merged_change_usd": 4.22,
                    "slice_to_merge_p50_seconds": 1_320,
                    "gate_pass_rate": 0.91,
                    "escalation_rate": 0.14,
                    "council_roi": 1.4,
                ]
                : [
                    "pipeline_merged_runs": 2,
                    "queue_depth": 0,
                    "active_pipeline_runs": 0,
                    "auto_merge_rate": 0.67,
                    "cost_per_merged_change_usd": 6.10,
                    "slice_to_merge_p50_seconds": 2_640,
                    "gate_pass_rate": 0.78,
                    "escalation_rate": 0.33,
                ]
        )
    }
}
