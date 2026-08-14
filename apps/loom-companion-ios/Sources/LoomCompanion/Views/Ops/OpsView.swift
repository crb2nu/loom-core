import SwiftUI
import LoomCompanionKit

/// Work tab — Queue is primary, everything else is a collapsible peek.
///
/// The previous layout used a 4-way segmented picker (Queue / Pipelines /
/// Runtime / Context) that treated all four as equals. In practice, Queue is
/// the critical path — pending tasks, blockers, approvals. Pipelines, Runtime,
/// Sandbox, and Context are supporting context: useful when triaging, noise
/// otherwise.
///
/// New layout:
///   1. Queue section renders at the top of the scroll, always visible.
///   2. Four DisclosureGroups below (Pipelines, Runtime, Sandbox, Context) with
///      a summary line — expand inline to peek without losing Queue context.
///   3. Sections load lazily on first expand + on pull-to-refresh when open.
///   4. Sandbox is its own peek because it's the only mobile mutation surface
///      and deserves first-class IA, not buried inside Runtime.
struct OpsView: View {
    @State private var viewModel: OpsViewModel
    @Binding private var deepLinkWorkflowID: String?
    @Binding private var taskFilter: NavigationCoordinator.TasksFilter?
    @Binding private var prefillEndSessionID: String?
    private var broadcaster: SSEEventBroadcaster?

    @State private var deepLinkedWorkflow: MobileWorkflow?
    @State private var pendingDeepLinkWorkflowID: String?
    @State private var toastMessage: String?
    @State private var showToast = false

    /// Handoff inbox presentation. `NavigationCoordinator.pendingHandoffInbox`
    /// is set by the `loom://handoff` deep link and the Dashboard's handoff
    /// attention lane; ContentView switches to the Work tab, and this view is
    /// the surface that actually presents the inbox.
    @State private var showingHandoffInbox = false
    @Environment(\.navigationCoordinator) private var navigationCoordinator

    @State private var expandedPipelines = false
    @State private var expandedRuntime = false
    @State private var expandedSandbox = false
    @State private var expandedContext = false

    init(
        apiClient: APIClient?,
        broadcaster: SSEEventBroadcaster? = nil,
        deepLinkWorkflowID: Binding<String?> = .constant(nil),
        taskFilter: Binding<NavigationCoordinator.TasksFilter?> = .constant(nil),
        prefillEndSessionID: Binding<String?> = .constant(nil)
    ) {
        let client = apiClient ?? APIClient(baseURL: URL(string: "http://localhost")!, token: "mock-token")
        self.broadcaster = broadcaster
        _deepLinkWorkflowID = deepLinkWorkflowID
        _taskFilter = taskFilter
        _prefillEndSessionID = prefillEndSessionID
        _viewModel = State(initialValue: OpsViewModel(apiClient: client))
    }

    var body: some View {
        ScrollView {
            VStack(spacing: LoomSpacing.md) {
                diagnostics

                // Queue — primary, full-weight
                queueHero

                // Peek disclosures — supporting context
                peekDisclosure(
                    section: .pipelines,
                    title: "Pipelines",
                    icon: "arrow.triangle.2.circlepath",
                    color: pipelinesAccent,
                    summary: pipelinesSummary,
                    isExpanded: $expandedPipelines
                ) {
                    OpsPipelinesSection(viewModel: viewModel)
                }

                peekDisclosure(
                    section: .runtime,
                    title: "Runtime",
                    icon: "cpu",
                    color: runtimeAccent,
                    summary: runtimeSummary,
                    isExpanded: $expandedRuntime
                ) {
                    OpsRuntimeSection(viewModel: viewModel, broadcaster: broadcaster)
                }

                peekDisclosure(
                    section: .sandbox,
                    title: "Sandbox",
                    icon: "shippingbox",
                    color: sandboxAccent,
                    summary: sandboxSummary,
                    isExpanded: $expandedSandbox
                ) {
                    OpsSandboxSection(viewModel: viewModel)
                }

                peekDisclosure(
                    section: .context,
                    title: "Context",
                    icon: "brain",
                    color: LoomColors.tierShortTerm,
                    summary: contextSummary,
                    isExpanded: $expandedContext
                ) {
                    OpsContextSection(viewModel: viewModel)
                }
            }
            .padding()
            .animation(.spring(duration: 0.35, bounce: 0.18), value: expandedPipelines)
            .animation(.spring(duration: 0.35, bounce: 0.18), value: expandedRuntime)
            .animation(.spring(duration: 0.35, bounce: 0.18), value: expandedSandbox)
            .animation(.spring(duration: 0.35, bounce: 0.18), value: expandedContext)
        }
        .loomTabBarClearance()
        .navigationTitle("Work")
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button {
                    HapticManager.selection()
                    showingHandoffInbox = true
                } label: {
                    Label("Handoffs", systemImage: "arrow.left.arrow.right")
                }
                .tint(LoomColors.accent)
                .accessibilityLabel("Handoff inbox")
            }
        }
        .task {
            if let broadcaster {
                viewModel.startListening(broadcaster: broadcaster)
            }
            // A deep link / attention lane may have requested the inbox before
            // this view existed; consume the pending flag on first appear.
            consumePendingHandoffInbox()
            await viewModel.loadSectionIfNeeded(.work)
            resolveDeepLinkWorkflow()
            await viewModel.loadSpawnConfig()
        }
        .refreshable {
            // Always refresh the Queue (primary).
            await viewModel.reloadSection(.work)
            // Refresh any expanded panels so the peek stays in sync.
            if expandedPipelines {
                await viewModel.reloadSection(.pipelines)
            }
            if expandedRuntime {
                await viewModel.reloadSection(.runtime)
            }
            if expandedSandbox {
                await viewModel.reloadSection(.sandbox)
            }
            if expandedContext {
                await viewModel.reloadSection(.context)
            }
            resolveDeepLinkWorkflow()
            HapticManager.light()
        }
        .onDisappear {
            if let broadcaster {
                viewModel.stopListening(broadcaster: broadcaster)
            }
        }
        .onChange(of: deepLinkWorkflowID) { _, newValue in
            pendingDeepLinkWorkflowID = newValue
            resolveDeepLinkWorkflow()
        }
        .onChange(of: viewModel.workflows.map(\.id)) { _, _ in
            resolveDeepLinkWorkflow()
        }
        .onChange(of: prefillEndSessionID) { _, newValue in
            guard let newValue else { return }
            prefillEndSession(with: newValue)
            prefillEndSessionID = nil
        }
        .onChange(of: navigationCoordinator?.pendingHandoffInbox ?? false) { _, requested in
            guard requested else { return }
            consumePendingHandoffInbox()
        }
        .sheet(item: $deepLinkedWorkflow) { workflow in
            NavigationStack {
                OpsWorkflowDetailView(
                    workflow: workflow,
                    loadDetail: viewModel.loadWorkflowDetail(id:),
                    approve: { id, stepId in
                        _ = try await viewModel.approveWorkflow(id: id, stepId: stepId)
                    },
                    reject: { id, stepId, reason in
                        _ = try await viewModel.rejectWorkflow(id: id, stepId: stepId, reason: reason)
                    }
                )
            }
        }
        .sheet(isPresented: $showingHandoffInbox) {
            NavigationStack {
                HandoffInboxView(viewModel: viewModel)
            }
        }
        .overlay(alignment: .top) {
            if showToast, let toastMessage {
                Text(toastMessage)
                    .font(.caption)
                    .foregroundStyle(.white)
                    .padding(.horizontal, LoomSpacing.md)
                    .padding(.vertical, LoomSpacing.sm)
                    .background(Color.black.opacity(0.85))
                    .clipShape(Capsule())
                    .padding(.top, LoomSpacing.sm)
                    .transition(.opacity)
            }
        }
    }

    // MARK: - Diagnostics banners (error / warning / mutation status)

    @ViewBuilder
    private var diagnostics: some View {
        if let error = viewModel.error, hasWorkContent {
            Label {
                Text("Showing the last work snapshot. \(error.description)")
            } icon: {
                Image(systemName: "arrow.trianglehead.2.clockwise.rotate.90")
            }
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.statusDegraded)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(LoomSpacing.md)
                .background(
                    LoomColors.statusDegraded.opacity(0.08),
                    in: RoundedRectangle(cornerRadius: 14, style: .continuous)
                )
        }
        if let warning = viewModel.warningMessage, viewModel.error == nil {
            Label(warning, systemImage: "exclamationmark.circle")
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.textSecondary)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(LoomSpacing.md)
                .background(
                    LoomColors.statusInfo.opacity(0.08),
                    in: RoundedRectangle(cornerRadius: 14, style: .continuous)
                )
        }
        if let status = viewModel.mutationStatusMessage {
            Text(status)
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.fgSecondary)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        if let mutationError = viewModel.mutationErrorMessage {
            Text(mutationError)
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.statusCritical)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    // MARK: - Queue hero

    @ViewBuilder
    private var queueHero: some View {
        if let error = viewModel.error, !hasWorkContent, viewModel.loadedSections.contains(.work) {
            workFailureState(error)
                .cardAppear(index: 0)
        } else if !viewModel.loadedSections.contains(.work) {
            workLoadingState
                .cardAppear(index: 0)
        } else {
            VStack(spacing: LoomSpacing.sm) {
                if viewModel.isLoading(.work) {
                    workRefreshStatus
                        .transition(.move(edge: .top).combined(with: .opacity))
                }

                OpsWorkSection(
                    viewModel: viewModel,
                    taskFilter: taskFilter,
                    clearTaskFilter: { taskFilter = nil },
                    prefillEndSession: { sessionID in
                        prefillEndSession(with: sessionID)
                    }
                )
            }
            .animation(.easeInOut(duration: 0.2), value: viewModel.isLoading(.work))
        }
    }

    private var hasWorkContent: Bool {
        !viewModel.tasks.isEmpty || !viewModel.workflows.isEmpty
    }

    private var workLoadingState: some View {
        LoomCard {
            VStack(alignment: .leading, spacing: LoomSpacing.lg) {
                HStack(spacing: LoomSpacing.md) {
                    ZStack {
                        Circle()
                            .fill(LoomColors.accent.opacity(0.12))
                            .frame(width: 46, height: 46)
                        ProgressView()
                            .tint(LoomColors.accent)
                    }

                    VStack(alignment: .leading, spacing: 3) {
                        Text("Loading operator queue")
                            .font(LoomTypography.headlineMedium)
                            .foregroundStyle(LoomColors.textPrimary)
                        Text("Gathering tasks, approvals, and session controls.")
                            .font(LoomTypography.caption)
                            .foregroundStyle(LoomColors.textSecondary)
                    }
                }

                SkeletonCompactRow()
                SkeletonCompactRow()
                SkeletonCompactRow()
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("Loading operator queue")
        .accessibilityIdentifier("work.loading")
    }

    private func workFailureState(_ error: LoomAPIError) -> some View {
        LoomCard {
            VStack(alignment: .leading, spacing: LoomSpacing.md) {
                Image(systemName: "tray.and.arrow.down.fill")
                    .font(.system(size: 26, weight: .semibold))
                    .foregroundStyle(LoomColors.statusDegraded)
                    .padding(10)
                    .background(LoomColors.statusDegraded.opacity(0.12), in: Circle())

                Text("Work couldn't load")
                    .font(LoomTypography.headlineMedium)
                    .foregroundStyle(LoomColors.textPrimary)

                Text(error.description)
                    .font(LoomTypography.bodyRegular)
                    .foregroundStyle(LoomColors.textSecondary)

                Text("Nothing was changed. Retry here, or pull down when the service is ready.")
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.textTertiary)

                Button {
                    Task { await viewModel.reloadSection(.work) }
                } label: {
                    Label("Try Again", systemImage: "arrow.clockwise")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .disabled(viewModel.isLoading(.work))
                .accessibilityHint("Reloads tasks and approvals")
            }
        }
    }

    private var workRefreshStatus: some View {
        HStack(spacing: LoomSpacing.sm) {
            ProgressView()
                .controlSize(.small)
                .tint(LoomColors.accent)
            Text("Refreshing work")
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.textSecondary)
            Spacer()
            if let updatedAt = viewModel.workLastUpdatedAt {
                Text(updatedAt, style: .relative)
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.textTertiary)
            }
        }
        .padding(.horizontal, LoomSpacing.md)
        .padding(.vertical, LoomSpacing.sm)
        .background(LoomColors.accent.opacity(0.08), in: Capsule())
        .accessibilityElement(children: .combine)
        .accessibilityLabel("Refreshing work queue")
    }

    // MARK: - Peek disclosure

    /// Collapsible panel that lazily loads its content on first expand.
    /// Header reads like a LoomListRow summary for visual consistency.
    @ViewBuilder
    private func peekDisclosure<Content: View>(
        section: OpsViewModel.OpsSection,
        title: String,
        icon: String,
        color: Color,
        summary: String,
        isExpanded: Binding<Bool>,
        @ViewBuilder content: @escaping () -> Content
    ) -> some View {
        LoomCard(priority: .compact) {
            VStack(alignment: .leading, spacing: 0) {
                Button {
                    HapticManager.selection()
                    isExpanded.wrappedValue.toggle()
                    if isExpanded.wrappedValue {
                        Task { await viewModel.loadSectionIfNeeded(section) }
                    }
                } label: {
                    peekHeader(title: title, icon: icon, color: color, summary: summary, isExpanded: isExpanded.wrappedValue)
                }
                .buttonStyle(.plain)

                if isExpanded.wrappedValue {
                    Divider()
                        .overlay(LoomColors.border)
                        .padding(.vertical, LoomSpacing.sm)
                    if viewModel.isLoading(section) && !viewModel.loadedSections.contains(section) {
                        lazySectionLoadingState(title: title)
                    } else {
                        content()
                            .transition(
                                .asymmetric(
                                    insertion: .opacity.combined(with: .move(edge: .top)),
                                    removal: .opacity
                                )
                            )
                    }
                }
            }
        }
    }

    private func lazySectionLoadingState(title: String) -> some View {
        HStack(spacing: LoomSpacing.sm) {
            ProgressView()
                .controlSize(.small)
                .tint(LoomColors.accent)
            Text("Loading \(title.lowercased())…")
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.textSecondary)
            Spacer()
        }
        .padding(.vertical, LoomSpacing.md)
        .accessibilityElement(children: .combine)
    }

    private func peekHeader(title: String, icon: String, color: Color, summary: String, isExpanded: Bool) -> some View {
        HStack(spacing: LoomSpacing.sm) {
            LoomRowIcon(systemName: icon, color: color, size: 12)

            VStack(alignment: .leading, spacing: 1) {
                Text(title)
                    .font(LoomTypography.labelLarge)
                    .foregroundStyle(LoomColors.fgPrimary)
                Text(summary)
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
                    .lineLimit(1)
            }

            Spacer()

            Image(systemName: "chevron.down")
                .font(.system(size: 11, weight: .semibold))
                .foregroundStyle(LoomColors.fgMuted)
                .rotationEffect(.degrees(isExpanded ? 0 : -90))
                .animation(.spring(duration: 0.25), value: isExpanded)
        }
        .contentShape(Rectangle())
    }

    // MARK: - Summaries (collapsed state)

    private var pipelinesSummary: String {
        if viewModel.isLoading(.pipelines) { return "loading…" }
        if !viewModel.pipelinesAvailable && viewModel.loadedSections.contains(.pipelines) {
            return "not available"
        }
        let summary = viewModel.pipelineSummary
        let running = summary?.running ?? 0
        let failed = summary?.failed ?? 0
        let total = viewModel.pipelines.count
        if total == 0 { return "no pipelines" }
        var parts: [String] = []
        if running > 0 { parts.append("\(running) running") }
        if failed > 0 { parts.append("\(failed) failed") }
        if parts.isEmpty { parts.append("\(total) idle") }
        return parts.joined(separator: " · ")
    }

    private var pipelinesAccent: Color {
        let failed = viewModel.pipelineSummary?.failed ?? 0
        let running = viewModel.pipelineSummary?.running ?? 0
        if failed > 0 { return LoomColors.statusCritical }
        if running > 0 { return LoomColors.statusActive }
        return LoomColors.fgMuted
    }

    private var runtimeSummary: String {
        if viewModel.isLoading(.runtime) { return "loading…" }
        let active = viewModel.presenceSummary.activeAgents
        let idle = viewModel.presenceSummary.idleAgents
        let offline = viewModel.presenceSummary.offlineAgents
        let claims = viewModel.presenceSummary.claimCount
        if active == 0 && idle == 0 && offline == 0 {
            return "no agents"
        }
        var parts: [String] = ["\(active) active"]
        if idle > 0 { parts.append("\(idle) idle") }
        if offline > 0 { parts.append("\(offline) offline") }
        if claims > 0 { parts.append("\(claims) claims") }
        return parts.joined(separator: " · ")
    }

    private var runtimeAccent: Color {
        let active = viewModel.presenceSummary.activeAgents
        let offline = viewModel.presenceSummary.offlineAgents
        if offline > 0 && active == 0 { return LoomColors.statusDegraded }
        if active > 0 { return LoomColors.statusHealthy }
        return LoomColors.fgMuted
    }

    private var sandboxSummary: String {
        if viewModel.isLoading(.sandbox) { return "loading…" }
        guard let sandbox = viewModel.sandboxSummary else {
            return viewModel.loadedSections.contains(.sandbox) ? "no data" : "tap to load"
        }
        if !sandbox.available {
            return "devbox unavailable"
        }
        let running = sandbox.totalRunning
        if running == 0 {
            return "idle · \(sandbox.backend)"
        }
        return "\(running) running · \(sandbox.backend)"
    }

    private var sandboxAccent: Color {
        guard let sandbox = viewModel.sandboxSummary else {
            return LoomColors.fgMuted
        }
        if !sandbox.available { return LoomColors.statusDegraded }
        if sandbox.totalRunning > 0 { return LoomColors.statusHealthy }
        return LoomColors.fgMuted
    }

    private var contextSummary: String {
        if viewModel.isLoading(.context) { return "loading…" }
        guard let stats = viewModel.memoryStats else {
            return viewModel.loadedSections.contains(.context) ? "no memory" : "tap to load"
        }
        let working = stats.workingMemory.items
        let shortTerm = stats.shortTermMemory.items
        let longTerm = stats.longTermMemory.items
        return "\(working) working · \(shortTerm) short · \(longTerm) long"
    }

    // MARK: - Deep link & prefill

    private func resolveDeepLinkWorkflow() {
        let requested = pendingDeepLinkWorkflowID ?? deepLinkWorkflowID
        guard let workflowID = requested?.trimmingCharacters(in: .whitespacesAndNewlines),
              !workflowID.isEmpty
        else {
            return
        }

        if let workflow = viewModel.workflows.first(where: { $0.id == workflowID }) {
            deepLinkedWorkflow = workflow
            pendingDeepLinkWorkflowID = nil
            deepLinkWorkflowID = nil
            return
        }

        guard !viewModel.isLoading else { return }

        pendingDeepLinkWorkflowID = nil
        deepLinkWorkflowID = nil
        showToastMessage("Workflow \(workflowID) is not in the current list")
    }

    /// Present the handoff inbox if navigation requested it, then clear the
    /// coordinator flag so a later request re-fires `onChange`.
    private func consumePendingHandoffInbox() {
        guard let coordinator = navigationCoordinator, coordinator.pendingHandoffInbox else { return }
        showingHandoffInbox = true
        coordinator.clearPendingHandoffInbox()
    }

    private func prefillEndSession(with sessionID: String) {
        let trimmed = sessionID.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        showToastMessage("End Session prefilled: \(trimmed)")
    }

    private func showToastMessage(_ message: String) {
        toastMessage = message
        withAnimation {
            showToast = true
        }
        Task {
            try? await Task.sleep(for: .seconds(2.5))
            await MainActor.run {
                withAnimation {
                    showToast = false
                }
            }
        }
    }
}
