import SwiftUI
import LoomCompanionKit

struct DashboardView: View {
    @State private var viewModel: DashboardViewModel
    let healthMonitor: ConnectionHealthMonitor
    let alertsViewModel: AlertsViewModel
    let broadcaster: SSEEventBroadcaster?
    var onNavigate: ((DashboardNavAction) -> Void)?
    @State private var showingAgents = false
    @State private var showingConnection = false
    @State private var updatedAgo: String?
    @State private var refreshTimer: Timer?
    @Environment(\.navigationCoordinator) private var navigationCoordinator
    @Environment(\.openURL) private var openURL

    /// Alert-inbox sheet presentation. The Alerts tab was removed when Spawn
    /// was promoted, so the Dashboard is the surface that presents the inbox —
    /// reached from the INBOX header, the critical-alert hero, the `alert`
    /// attention lane, and the `loom://alerts` / `loom://alert/<id>` links.
    @State private var showingAlertInbox = false
    @State private var focusedAlertID: String?

    /// Retained so the alert inbox can be bound to the same authenticated
    /// client as the dashboard read.
    private let apiClient: any LoomAPIClientProtocol

    enum DashboardNavAction {
        case people
        case work
        case mills
        case connection
        case liveActivities
        case alerts
    }

    // `alertsViewModel` defaults to nil rather than to a fresh
    // `AlertsViewModel()`: the inbox VM is main-actor isolated now that it
    // performs API calls, and default-argument expressions are always evaluated
    // in a nonisolated context. The fallback is built in the (isolated) body.
    @MainActor
    init(apiClient: APIClient?, healthMonitor: ConnectionHealthMonitor, alertsViewModel: AlertsViewModel? = nil, broadcaster: SSEEventBroadcaster? = nil, onNavigate: ((DashboardNavAction) -> Void)? = nil) {
        let client: any LoomAPIClientProtocol = apiClient ?? NoOpAPIClient()
        let alerts = alertsViewModel ?? AlertsViewModel()
        self.apiClient = client
        self.alertsViewModel = alerts
        self.broadcaster = broadcaster
        self.onNavigate = onNavigate
        _viewModel = State(initialValue: DashboardViewModel(apiClient: client, alertsViewModel: alerts))
        self.healthMonitor = healthMonitor
    }

    var body: some View {
        ScrollView {
            VStack(spacing: LoomSpacing.lg) {
                ErrorBanner(health: healthMonitor.health)

                // Triage-first header — mirrors the HUD A2 inbox strip.
                // Operator's at-a-glance answer to "do I need to do anything?"
                if viewModel.dashboard != nil {
                    Button {
                        HapticManager.selection()
                        openAlertInbox()
                    } label: {
                        DashboardInboxHeader(
                            pressureCount: pressureCount,
                            topSeverity: topSeverity,
                            updatedAgo: updatedAgo,
                            isInteractive: true
                        )
                    }
                    .buttonStyle(.plain)
                    .accessibilityHint("Opens the alert inbox and auto-fix proposals")
                }

                // Critical alerts are now folded into NextActionCard as the
                // highest-priority action — no separate banner needed.

                #if os(iOS)
                if #available(iOS 16.2, *) {
                    let lam = LiveActivityManager.shared
                    if lam.activeCount > 0 {
                        LiveActivityBanner(
                            sessionCount: lam.activeSessionCount,
                            workflowCount: lam.activeWorkflowCount,
                            pipelineCount: lam.activePipelineCount
                        ) {
                            onNavigate?(.liveActivities)
                        }
                    }
                }
                #endif

                if let dashboard = viewModel.dashboard {
                    if isClear {
                        clearState(dashboard: dashboard)
                    } else {
                        pressureState(dashboard: dashboard)
                    }

                    if #available(iOS 26.0, *) {
                        AppleIntelligenceBriefingCard(
                            snapshot: LoomBriefingSnapshot(
                                dashboard: dashboard,
                                taskCounts: viewModel.taskCounts
                            )
                        )
                        .cardAppear(index: 6)
                    }
                } else if viewModel.isLoading {
                    VStack(spacing: LoomSpacing.lg) {
                        SkeletonHeroCard()
                            .cardAppear(index: 0)
                        SkeletonDashboardCard()
                            .cardAppear(index: 1)
                        SkeletonCompactRow()
                            .cardAppear(index: 2)
                        SkeletonCompactRow()
                            .cardAppear(index: 3)
                    }
                } else if let error = viewModel.error {
                    ContentUnavailableView {
                        Label(error.dashboardTitle, systemImage: "wifi.exclamationmark")
                    } description: {
                        Text(error.description)
                    } actions: {
                        Button("Retry") {
                            Task { await refreshDashboard() }
                        }
                        .buttonStyle(.borderedProminent)

                        if error.shouldSuggestConnectionTab {
                            Button("Open Connection") {
                                onNavigate?(.connection)
                            }
                            .buttonStyle(.bordered)
                        }
                    }
                } else {
                    ContentUnavailableView {
                        Label("No Dashboard Data", systemImage: "square.grid.2x2")
                    } description: {
                        // The Connection tab was removed when Spawn was
                        // promoted; connection lives behind the Settings gear.
                        Text("Connect to a Loom server to view health, fleet, and task data. Check your connection settings in Settings (the gear in the toolbar).")
                    } actions: {
                        Button("Refresh") {
                            Task { await refreshDashboard() }
                        }
                        .buttonStyle(.borderedProminent)
                    }
                }
            }
            .padding()
        }
        .loomTabBarClearance()
        .navigationTitle("Dashboard")
        #if os(iOS)
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .onChange(of: viewModel.dashboard?.updatedAt) { _, newValue in
            if let ts = newValue {
                withAnimation { updatedAgo = Self.relativeTime(ts) }
            }
        }
        .onAppear {
            // Auto-refresh the "Updated Xs ago" label every 5 seconds.
            refreshTimer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { _ in
                Task { @MainActor in
                    if let ts = viewModel.dashboard?.updatedAt {
                        withAnimation { updatedAgo = Self.relativeTime(ts) }
                    }
                }
            }
        }
        .onDisappear {
            refreshTimer?.invalidate()
            refreshTimer = nil
            healthMonitor.onPollRefresh = nil
            if let broadcaster {
                viewModel.stopListening(broadcaster: broadcaster)
            }
        }
        .refreshable {
            await refreshDashboard()
            HapticManager.light()
        }
        .task {
            healthMonitor.onPollRefresh = { [weak viewModel, weak healthMonitor] in
                guard let viewModel, let healthMonitor else { return }
                await Self.refreshDashboard(viewModel: viewModel, healthMonitor: healthMonitor)
            }

            // Bind the inbox to this connection's client. The AlertsViewModel
            // is created by ContentView before authentication, so it can't be
            // constructed with a client.
            alertsViewModel.configure(apiClient: apiClient)

            // A deep link may have requested the inbox before this view
            // existed; consume the pending flag on first appear.
            consumePendingAlertInbox()

            await refreshDashboard()

            if let broadcaster {
                viewModel.startListening(broadcaster: broadcaster)
            }

            // Seed the inbox from the server store so the unread count and the
            // critical "DO NEXT" card reflect history, not just this session's
            // SSE traffic.
            await alertsViewModel.load()
        }
        .onChange(of: navigationCoordinator?.pendingAlertInbox ?? false) { _, requested in
            guard requested else { return }
            consumePendingAlertInbox()
        }
        .sheet(isPresented: $showingAlertInbox) {
            NavigationStack {
                AlertsListView(
                    viewModel: alertsViewModel,
                    focusedAlertID: focusedAlertID
                ) { action, alert in
                    showingAlertInbox = false
                    routeFromAlert(action, alert)
                }
            }
            .preferredColorScheme(.dark)
        }
    }

    // MARK: - Alert inbox

    private func openAlertInbox(focus alertID: String? = nil) {
        focusedAlertID = alertID
        showingAlertInbox = true
    }

    private func consumePendingAlertInbox() {
        guard let coordinator = navigationCoordinator, coordinator.pendingAlertInbox else { return }
        openAlertInbox(focus: coordinator.pendingAlertID)
        coordinator.clearPendingAlertInbox()
    }

    /// Follow an alert row's quick-action out of the inbox.
    private func routeFromAlert(_ action: AlertAction, _ alert: AlertItem) {
        switch action {
        case .viewSession:
            if let id = alert.relatedSessionId, !id.isEmpty {
                navigationCoordinator?.navigateToSession(id: id)
            }
        case .viewWorkflow:
            if let id = alert.relatedWorkflowId, !id.isEmpty {
                navigationCoordinator?.navigateToWorkflow(id: id)
            }
        case .viewDashboard, .acknowledge:
            break
        }
    }

    @MainActor
    private static func refreshDashboard(
        viewModel: DashboardViewModel,
        healthMonitor: ConnectionHealthMonitor
    ) async {
        await viewModel.load()
        if let error = viewModel.error {
            healthMonitor.handleAPIError(error)
        } else {
            healthMonitor.handleSuccess()
        }
    }

    // MARK: - Triage-First Decomposition (HUD A2 alignment)

    /// Count of distinct pressure points the operator should consider. Mirrors
    /// the HUD inbox count: each attention lane + each unread critical alert.
    /// Health degradations are already projected into lanes by the backend, so
    /// we don't double-count them here.
    private var pressureCount: Int {
        guard let dashboard = viewModel.dashboard else { return 0 }
        let lanes = dashboard.coordination.attentionLanes.count
        let unreadCritical = alertsViewModel.criticalAlerts.filter { !$0.isRead }.count
        return lanes + unreadCritical
    }

    /// Worst severity present across alerts + lanes. Drives the count-pill tint.
    private var topSeverity: DashboardInboxHeader.Severity {
        guard let dashboard = viewModel.dashboard else { return .nominal }
        let unreadCritical = alertsViewModel.criticalAlerts.filter { !$0.isRead }
        if !unreadCritical.isEmpty { return .critical }
        if dashboard.coordination.attentionLanes.contains(where: { $0.severity == "critical" }) {
            return .critical
        }
        if dashboard.coordination.attentionLanes.contains(where: { $0.severity == "warning" }) {
            return .warning
        }
        if !dashboard.coordination.attentionLanes.isEmpty { return .info }
        return .nominal
    }

    private var isClear: Bool {
        pressureCount == 0
    }

    // MARK: - Clear state (hide-when-clear)

    /// When nothing needs attention, the dashboard becomes a single calm anchor
    /// + the compact fleet chip. Everything else recedes. This is the operator
    /// "the world is fine" surface — the opposite of glance-overload.
    @ViewBuilder
    private func clearState(dashboard: DashboardData) -> some View {
        LoomEmptyState(
            tone: .nominal,
            title: "System nominal",
            detail: clearDetail(dashboard: dashboard)
        )
        .loomCard(priority: .standard)
        .cardAppear(index: 0)

        Button {
            HapticManager.selection()
            onNavigate?(.people)
        } label: {
            FleetSummaryCard(dashboard: dashboard)
        }
        .buttonStyle(.plain)
        .cardAppear(index: 1)
    }

    /// Short mono-styled detail line under "System nominal", composed from the
    /// fleet numbers the operator would otherwise check by scrolling.
    private func clearDetail(dashboard: DashboardData) -> String {
        var parts: [String] = []
        parts.append("\(dashboard.activeAgents) agent\(dashboard.activeAgents == 1 ? "" : "s") active")
        parts.append("\(dashboard.activeSessions) session\(dashboard.activeSessions == 1 ? "" : "s")")
        if dashboard.offlineAgents > 0 {
            parts.append("\(dashboard.offlineAgents) offline")
        }
        return parts.joined(separator: " · ")
    }

    // MARK: - Pressure state (triage queue + collapsed context)

    /// When there's work to do, surface the inbox stack — hero, attention
    /// queue, active work — and demote steady-state context (fleet, health,
    /// timeline) below a subtle "Context" divider so they don't compete.
    @ViewBuilder
    private func pressureState(dashboard: DashboardData) -> some View {
        NextActionCard(
            lanes: dashboard.coordination.attentionLanes,
            health: dashboard.health,
            criticalAlerts: alertsViewModel.criticalAlerts,
            onNavigate: navigate,
            onLaneNavigate: routeFromLane
        )
        .cardAppear(index: 0)

        if dashboard.coordination.attentionLanes.count > 1 {
            AttentionLanesCard(
                lanes: dashboard.coordination.attentionLanes,
                skipFirst: true
            ) { lane in
                HapticManager.selection()
                routeFromLane(lane)
            }
            .cardAppear(index: 1)
        }

        if let counts = viewModel.taskCounts,
           counts.pending + counts.inProgress + counts.blocked > 0 {
            ActiveWorkCard(counts: counts) {
                onNavigate?(.work)
            }
            .cardAppear(index: 2)
        }

        // Steady-state context — recedes below the triage queue.
        contextDivider

        Button {
            HapticManager.selection()
            onNavigate?(.people)
        } label: {
            FleetSummaryCard(dashboard: dashboard)
        }
        .buttonStyle(.plain)
        .cardAppear(index: 3)

        Button {
            HapticManager.selection()
            onNavigate?(.connection)
        } label: {
            HealthStatusCard(health: dashboard.health)
        }
        .buttonStyle(.plain)
        .cardAppear(index: 4)

        TimelineListView(entries: dashboard.recentTimeline)
            .cardAppear(index: 5)
    }

    /// Subtle "below the fold" divider that signals what follows is context,
    /// not action. Uses the kindLabel motif for visual continuity with the
    /// inbox header.
    private var contextDivider: some View {
        HStack(spacing: LoomSpacing.sm) {
            Rectangle()
                .fill(LoomColors.border)
                .frame(height: 1)
            Text("CONTEXT")
                .font(LoomTypography.kindLabel)
                .tracking(LoomTypography.kindLabelTracking)
                .foregroundStyle(LoomColors.fgMuted)
            Rectangle()
                .fill(LoomColors.border)
                .frame(height: 1)
        }
        .padding(.top, LoomSpacing.xs)
    }

    /// Optional so an unparseable timestamp hides the header chip rather than
    /// showing a placeholder. Formatting itself is delegated to ``LoomFormat``.
    private static func relativeTime(_ iso: String) -> String? {
        guard let date = LoomFormat.date(fromISO: iso) else { return nil }
        return LoomFormat.relative(from: date)
    }

    /// Nav sink for child cards. `.alerts` is handled here (the inbox is this
    /// view's own sheet) rather than being bounced up to ContentView, which
    /// used to swallow it by re-selecting the Dashboard tab.
    private func navigate(_ action: DashboardNavAction) {
        if case .alerts = action {
            openAlertInbox()
            return
        }
        onNavigate?(action)
    }

    private func navigationAction(for lane: DashboardAttentionLane) -> DashboardNavAction {
        if lane.isTaskLane {
            return .work
        }

        switch lane.route {
        case "people":
            return .people
        case "connection":
            return .connection
        default:
            return .work
        }
    }

    private func routeFromLane(_ lane: DashboardAttentionLane) {
        if let url = URL(string: lane.deepLink),
           let link = DeepLink.from(url) {
            route(link)
            return
        }

        if lane.isTaskLane {
            navigationCoordinator?.filterTasks(
                status: lane.taskStatusHint,
                agentId: lane.filter?.agentId,
                sessionId: lane.filter?.sessionId
            )
            onNavigate?(.work)
            return
        }

        switch lane.targetKind {
        case "session":
            if !lane.targetId.isEmpty {
                navigationCoordinator?.navigateToSession(id: lane.targetId)
                return
            }
        case "agent":
            if !lane.targetId.isEmpty {
                navigationCoordinator?.navigateToAgent(id: lane.targetId)
                return
            }
        case "task_filter":
            navigationCoordinator?.filterTasks(
                status: lane.filter?.status,
                agentId: lane.filter?.agentId,
                sessionId: lane.filter?.sessionId
            )
            onNavigate?(.work)
            return
        case "workflow":
            if !lane.targetId.isEmpty {
                navigationCoordinator?.navigateToWorkflow(id: lane.targetId)
                return
            }
        case "spawn":
            if !lane.targetId.isEmpty {
                navigationCoordinator?.navigateToSpawn(id: lane.targetId)
                return
            }
        case "handoff":
            navigationCoordinator?.openHandoffInbox()
            onNavigate?(.work)
            return
        case "connection":
            onNavigate?(.connection)
            return
        case "alert":
            // The lane's target id is the HUD alert id — open the inbox
            // focused on it.
            openAlertInbox(focus: lane.targetId.isEmpty ? nil : lane.targetId)
            return
        case "merge_request":
            // mrwatch lanes carry the GitLab MR web_url as their deep link — an
            // https:// URL that DeepLink.from deliberately rejects (loom:// only),
            // which used to strand the tap on the generic Work tab. Hand it to
            // the system browser instead.
            if let url = URL(string: lane.deepLink),
               url.scheme == "https" || url.scheme == "http" {
                openURL(url)
                return
            }
        default:
            break
        }
        navigate(navigationAction(for: lane))
    }

    private func route(_ link: DeepLink) {
        switch link {
        case .dashboard:
            break
        case .people:
            onNavigate?(.people)
        case .work:
            onNavigate?(.work)
        case .mills, .pipelineEscalate:
            onNavigate?(.mills)
        case .alerts:
            openAlertInbox()
        case .connection, .configure:
            onNavigate?(.connection)
        case .session(let id):
            navigationCoordinator?.navigateToSession(id: id)
        case .agent(let id):
            navigationCoordinator?.navigateToAgent(id: id)
        case .sessions(let status, let agentId):
            navigationCoordinator?.filterSessions(status: status, agentId: agentId)
            onNavigate?(.people)
        case .agents(let status, let type):
            navigationCoordinator?.filterAgents(status: status, type: type)
            onNavigate?(.people)
        case .tasks(let status, let agentId, let sessionId):
            navigationCoordinator?.filterTasks(status: status, agentId: agentId, sessionId: sessionId)
            onNavigate?(.work)
        case .workflow(let id, _):
            navigationCoordinator?.navigateToWorkflow(id: id)
        case .spawn(let id):
            navigationCoordinator?.navigateToSpawn(id: id)
        case .handoff:
            navigationCoordinator?.openHandoffInbox()
            onNavigate?(.work)
        case .alert(let id):
            openAlertInbox(focus: id)
        }
    }

    @MainActor
    private func refreshDashboard() async {
        await viewModel.load()
        if let error = viewModel.error {
            healthMonitor.handleAPIError(error)
        } else {
            healthMonitor.handleSuccess()
        }
    }

}

private struct NoOpAPIClient: LoomAPIClientProtocol {
    func request<T: Decodable>(_ endpoint: Endpoint) async throws -> T {
        throw LoomAPIError.noToken
    }
}
