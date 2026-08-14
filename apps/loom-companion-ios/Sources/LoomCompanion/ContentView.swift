import SwiftUI
import LoomCompanionKit

struct ContentView: View {
    @Bindable var connectionVM: ConnectionViewModel
    @Binding var pendingDeepLink: DeepLink?
    @State private var healthMonitor = ConnectionHealthMonitor()
    @State private var alertsViewModel = AlertsViewModel()
    @State private var selectedTab: AppTab = .dashboard
    @State private var sseClient: SSEClient?
    @State private var sseBroadcaster = SSEEventBroadcaster()
    @State private var recoveryUploader: RecoveryTelemetryUploader?
    @State private var pendingSessionDeepLinkID: String?
    @State private var pendingWorkflowDeepLinkID: String?
    @State private var pendingEndSessionPrefillID: String?
    @State private var selectedPeopleSection: PeopleSection = .agents
    @State private var navigationCoordinator = NavigationCoordinator()
    @State private var pendingAgentDeepLinkID: String?
    @State private var pendingEscalatePipelineID: String?
    @State private var showingSettings = false
    /// The classic DashboardView, pushed behind the Operator deck (the first
    /// tab's new root). Toolbar-driven, and auto-set by alert deep links so
    /// the dashboard-owned alert inbox sheet still mounts and consumes the
    /// pending flag.
    @State private var showClassicDashboard = false

    enum AppTab {
        case dashboard
        case people
        case spawn
        case work
        case mills
    }

    enum PeopleSection: String, CaseIterable, Identifiable {
        case agents
        case sessions
        case live

        var id: String { rawValue }
    }

    var body: some View {
        Group {
            if connectionVM.isAuthenticated {
                authenticatedContent
            } else {
                LoginView(viewModel: connectionVM)
            }
        }
        .environment(\.navigationCoordinator, navigationCoordinator)
        .task {
            // Launch-argument deep links are seeded in App.init and land here
            // as the INITIAL value of `pendingDeepLink`. `.onChange` doesn't
            // fire for initial values, so consume it explicitly on first
            // appear before any other connection-setup work runs.
            if let link = pendingDeepLink {
                handleDeepLink(link)
                pendingDeepLink = nil
            }
            if connectionVM.isAuthenticated {
                setupSSE()
            }
        }
        .onChange(of: connectionVM.isAuthenticated) { _, isAuth in
            if isAuth {
                setupSSE()
            } else {
                teardownSSE()
            }
        }
        .tint(LoomColors.info)
        .preferredColorScheme(.dark)
        .sheet(isPresented: $showingSettings) {
            NavigationStack {
                ConnectionDiagnosticsView(
                    connectionVM: connectionVM,
                    healthMonitor: healthMonitor
                )
                .toolbar {
                    ToolbarItem(placement: .topBarTrailing) {
                        Button("Done") { showingSettings = false }
                            .tint(LoomColors.info)
                    }
                }
            }
            .preferredColorScheme(.dark)
        }
        .onChange(of: selectedTab) { _, _ in
            HapticManager.selection()
        }
        .onChange(of: selectedPeopleSection) { _, _ in
            HapticManager.selection()
        }
        .onChange(of: pendingDeepLink) { _, link in
            guard let link else { return }
            handleDeepLink(link)
            pendingDeepLink = nil
        }
        .onChange(of: navigationCoordinator.pendingSessionID) { _, sessionId in
            guard let sessionId else { return }
            pendingSessionDeepLinkID = sessionId
            selectedPeopleSection = .sessions
            selectedTab = .people
            navigationCoordinator.clearPendingSession()
        }
        .onChange(of: navigationCoordinator.pendingAgentID) { _, agentId in
            guard let agentId else { return }
            pendingAgentDeepLinkID = agentId
            selectedPeopleSection = .agents
            selectedTab = .people
            navigationCoordinator.clearPendingAgent()
        }
        .onChange(of: navigationCoordinator.pendingSpawnID) { _, spawnId in
            guard spawnId != nil else { return }
            selectedTab = .spawn
            navigationCoordinator.clearPendingSpawn()
        }
        .onChange(of: navigationCoordinator.pendingWorkflowID) { _, workflowId in
            guard let workflowId else { return }
            pendingWorkflowDeepLinkID = workflowId
            selectedTab = .work
            navigationCoordinator.clearPendingWorkflow()
        }
        .onChange(of: navigationCoordinator.pendingTasksFilter) { _, filter in
            guard filter != nil else { return }
            selectedTab = .work
        }
        .onChange(of: navigationCoordinator.pendingHandoffInbox) { _, open in
            guard open else { return }
            selectedTab = .work
        }
        .onChange(of: navigationCoordinator.pendingAlertInbox) { _, open in
            // The alert inbox lives on the classic Dashboard, which is now
            // pushed BEHIND the Operator deck on the first tab — select the
            // tab AND push the dashboard so its sheet mounts and consumes the
            // flag (and the focused id).
            guard open else { return }
            selectedTab = .dashboard
            showClassicDashboard = true
        }
    }

    private func handleDeepLink(_ link: DeepLink) {
        // Dispatch based on the specific link; destinationGroup handles the tab.
        switch link {
        case .dashboard:
            selectedTab = .dashboard

        case .people:
            selectedTab = .people

        case .work:
            selectedTab = .work

        case .mills:
            selectedTab = .mills

        // Mills · pipeline escalate — from the widget's per-pipeline button.
        // Route to the Mills tab and hand the run id to MillsScreen, which
        // shows a confirm sheet and performs the admin-gated escalate.
        case .pipelineEscalate(let id):
            pendingEscalatePipelineID = id
            selectedTab = .mills

        case .alerts:
            // There is no Alerts tab (Spawn took that slot) — the inbox is a
            // sheet DashboardView presents. Switch to Dashboard and ask the
            // coordinator to raise it.
            navigationCoordinator.openAlertInbox()
            selectedTab = .dashboard

        case .connection:
            // Connection is no longer a tab — it lives behind the Dashboard's
            // Settings gear. Surface it as the modal settings sheet.
            showingSettings = true

        // People · single session / agent
        case .session(let id):
            pendingSessionDeepLinkID = id
            selectedPeopleSection = .sessions
            selectedTab = .people

        case .agent(let id):
            pendingAgentDeepLinkID = id
            selectedPeopleSection = .agents
            selectedTab = .people

        // People · filtered lists (preset filter + navigate)
        case .sessions(let status, let agentId):
            navigationCoordinator.filterSessions(status: status, agentId: agentId)
            selectedPeopleSection = .sessions
            selectedTab = .people

        case .agents(let status, let type):
            navigationCoordinator.filterAgents(status: status, type: type)
            selectedPeopleSection = .agents
            selectedTab = .people

        // Work · tasks filter
        case .tasks(let status, let agentId, let sessionId):
            navigationCoordinator.filterTasks(status: status, agentId: agentId, sessionId: sessionId)
            selectedTab = .work

        // Work · workflow (with optional approve intent)
        case .workflow(let id, let approve):
            if approve {
                Task { await approveWorkflowFromDeepLink(workflowId: id) }
            }
            pendingWorkflowDeepLinkID = id
            selectedTab = .work

        // Spawn · active remote execution
        case .spawn(let id):
            navigationCoordinator.navigateToSpawn(id: id)
            selectedTab = .spawn

        // Work · handoff inbox
        case .handoff:
            navigationCoordinator.openHandoffInbox()
            selectedTab = .work

        // Alerts · single alert — open the inbox scrolled to this alert. The
        // id is the HUD alert store's own id (`GET /api/mobile/v1/alerts`),
        // which is also what `POST /alerts/{id}/ack` addresses.
        case .alert(let id):
            navigationCoordinator.openAlertInbox(focus: id)
            selectedTab = .dashboard

        // One-shot bootstrap from `make mobile-app-run-device` over USB.
        case .configure(let spec):
            Task { await connectionVM.applyConfigureSpec(spec) }
            showingSettings = true
        }
    }

    private func approveWorkflowFromDeepLink(workflowId: String) async {
        guard let apiClient = connectionVM.buildAPIClient() else { return }
        // Fetch workflow detail to find the pending step.
        do {
            // The detail route returns `{workflow, events}` — decoding it as a
            // bare MobileWorkflowDetail never succeeded, so the deep-link
            // approve path always bailed before it reached the approve call.
            let response: MobileWorkflowDetailResponse = try await apiClient.request(.workflowDetail(id: workflowId))
            guard let pendingStep = response.workflow.steps?.first(where: { $0.status == .waitingApproval }) else { return }
            // Approve returns a mutation acknowledgement
            // (`{workflow_id, step_id, action}`), NOT the workflow record —
            // decoding it as MobileWorkflowDetail always failed and silently
            // swallowed the (successful) approval's confirmation.
            let _: MobileWorkflowMutationResponse = try await apiClient.request(
                .workflowApprove(id: workflowId, stepId: pendingStep.id)
            )
        } catch {
            // Best-effort - user can manually approve in the work view.
            print("[DeepLink] Workflow approval failed: \(error)")
        }
    }

    @ViewBuilder
    private var authenticatedContent: some View {
        #if os(iOS)
        if UIDevice.current.userInterfaceIdiom == .pad {
            iPadLayout
        } else {
            iPhoneLayout
        }
        #else
        iPhoneLayout
        #endif
    }

    /// The classic dashboard, factored once so the Operator deck can push it
    /// on both layouts without duplicating its routing closure.
    private var classicDashboard: some View {
        DashboardView(apiClient: connectionVM.buildAPIClient(), healthMonitor: healthMonitor, alertsViewModel: alertsViewModel, broadcaster: sseBroadcaster) { action in
            switch action {
            case .people:
                selectedPeopleSection = .agents
                selectedTab = .people
            case .work:
                selectedTab = .work
            case .mills:
                selectedTab = .mills
            case .connection:
                showingSettings = true
            case .liveActivities:
                selectedPeopleSection = .sessions
                selectedTab = .people
            case .alerts:
                // DashboardView handles `.alerts` itself (the inbox is
                // its own sheet); this only fires if some other surface
                // ever routes here.
                navigationCoordinator.openAlertInbox()
                selectedTab = .dashboard
            }
        }
    }

    /// Root of the first tab: the unified Operator deck, with the classic
    /// dashboard pushed behind it (toolbar gauge button; alert deep links
    /// auto-push via showClassicDashboard).
    private var operatorDeckRoot: some View {
        OperatorScreen(apiClient: connectionVM.buildAPIClient()) { action in
            switch action {
            case .people:
                selectedPeopleSection = .agents
                selectedTab = .people
            case .liveActivities:
                selectedPeopleSection = .sessions
                selectedTab = .people
            case .work:
                selectedTab = .work
            case .mills:
                selectedTab = .mills
            }
        }
        .navigationDestination(isPresented: $showClassicDashboard) {
            classicDashboard
        }
        .toolbar {
            ToolbarItem(placement: .topBarLeading) {
                Button {
                    showClassicDashboard = true
                } label: {
                    Image(systemName: "gauge.open.with.lines.needle.33percent")
                }
                .accessibilityLabel("Open classic dashboard")
            }
            ToolbarItem(placement: .topBarTrailing) { settingsButton }
        }
    }

    private var iPhoneLayout: some View {
        TabView(selection: $selectedTab) {
            NavigationStack {
                operatorDeckRoot
            }
            .tabItem { Label("Operator", systemImage: "dial.medium") }
            .tag(AppTab.dashboard)

            peopleTab
                .tabItem { Label("Agents", systemImage: "person.2.wave.2") }
                .tag(AppTab.people)

            NavigationStack {
                SpawnTabView(
                    apiClient: connectionVM.buildAPIClient(),
                    broadcaster: sseBroadcaster
                )
            }
            .tabItem { Label("Spawn", systemImage: "sparkles") }
            .tag(AppTab.spawn)

            NavigationStack {
                OpsView(
                    apiClient: connectionVM.buildAPIClient(),
                    broadcaster: sseBroadcaster,
                    deepLinkWorkflowID: $pendingWorkflowDeepLinkID,
                    taskFilter: Binding(
                        get: { navigationCoordinator.pendingTasksFilter },
                        set: { navigationCoordinator.pendingTasksFilter = $0 }
                    ),
                    prefillEndSessionID: $pendingEndSessionPrefillID
                )
            }
            .tabItem { Label("Work", systemImage: "square.grid.2x2") }
            .tag(AppTab.work)

            NavigationStack {
                // The escalate binding must be passed on BOTH layouts — the
                // Mills widget's per-pipeline escalate deep link is a no-op
                // without it, and iPhone is where the widget actually lives.
                MillsScreen(
                    apiClient: connectionVM.buildAPIClient(),
                    pendingEscalatePipelineID: $pendingEscalatePipelineID
                )
            }
            .tabItem { Label("Mills", systemImage: "hexagon") }
            .tag(AppTab.mills)
        }
    }

    // MARK: - Settings entry (relocated Connection)

    /// Toolbar gear that opens the Connection/diagnostics surface as a modal
    /// sheet. Connection was demoted from a tab to make room for Mills as a
    /// first-class destination; the gear keeps it one tap away and tints to
    /// flag connection trouble now that there's no always-visible tab badge.
    private var settingsButton: some View {
        Button {
            showingSettings = true
        } label: {
            Image(systemName: settingsUnhealthy ? "gearshape.fill" : "gearshape")
                .symbolRenderingMode(.hierarchical)
                .foregroundStyle(settingsTint)
        }
        .accessibilityLabel("Settings and connection")
        .accessibilityValue(settingsUnhealthy ? "Connection needs attention" : "Connected")
    }

    private var settingsUnhealthy: Bool {
        switch healthMonitor.health {
        case .healthy, .unknown: return false
        default: return true
        }
    }

    private var settingsTint: Color {
        switch healthMonitor.health {
        case .healthy, .unknown:
            return LoomColors.info
        case .degradedStream, .rateLimited:
            return LoomColors.statusDegraded
        case .authFailure, .permissionDenied, .gatewayRouteMissing, .unreachable:
            return LoomColors.statusCritical
        }
    }

    private var iPadLayout: some View {
        NavigationSplitView {
            List {
                Label("Operator", systemImage: "dial.medium")
                    .contentShape(Rectangle())
                    .onTapGesture { selectedTab = .dashboard }
                Label("Agents", systemImage: "person.2.wave.2")
                    .contentShape(Rectangle())
                    .onTapGesture { selectedTab = .people }
                Label("Spawn", systemImage: "sparkles")
                    .contentShape(Rectangle())
                    .onTapGesture { selectedTab = .spawn }
                Label("Work", systemImage: "square.grid.2x2")
                    .contentShape(Rectangle())
                    .onTapGesture { selectedTab = .work }
                Label("Mills", systemImage: "hexagon")
                    .contentShape(Rectangle())
                    .onTapGesture { selectedTab = .mills }

                Section {
                    Label("Settings", systemImage: settingsUnhealthy ? "gearshape.fill" : "gearshape")
                        .foregroundStyle(settingsTint)
                        .contentShape(Rectangle())
                        .onTapGesture { showingSettings = true }
                }
            }
            .navigationTitle("Loom")
        } detail: {
            switch selectedTab {
            case .dashboard:
                // NavigationStack so the deck's pushed classic dashboard
                // (navigationDestination) works inside the split view detail.
                NavigationStack {
                    operatorDeckRoot
                }
            case .people:
                peopleTab
            case .spawn:
                SpawnTabView(
                    apiClient: connectionVM.buildAPIClient(),
                    broadcaster: sseBroadcaster
                )
            case .work:
                OpsView(
                    apiClient: connectionVM.buildAPIClient(),
                    broadcaster: sseBroadcaster,
                    deepLinkWorkflowID: $pendingWorkflowDeepLinkID,
                    taskFilter: Binding(
                        get: { navigationCoordinator.pendingTasksFilter },
                        set: { navigationCoordinator.pendingTasksFilter = $0 }
                    ),
                    prefillEndSessionID: $pendingEndSessionPrefillID
                )
            case .mills:
                MillsScreen(
                    apiClient: connectionVM.buildAPIClient(),
                    pendingEscalatePipelineID: $pendingEscalatePipelineID
                )
            }
        }
    }

    @ViewBuilder
    private var peopleTab: some View {
        VStack(spacing: 0) {
            peopleHeader

            Group {
                switch selectedPeopleSection {
                case .agents:
                    AgentsListView(
                        apiClient: connectionVM.buildAPIClient(),
                        broadcaster: sseBroadcaster,
                        deepLinkSessionID: $pendingSessionDeepLinkID,
                        deepLinkAgentID: $pendingAgentDeepLinkID,
                        embeddedInPeopleTab: true,
                        onPrefillEndSession: { sessionID in
                            pendingEndSessionPrefillID = sessionID
                            selectedTab = .work
                        }
                    )
                case .sessions:
                    SessionsListView(
                        apiClient: connectionVM.buildAPIClient(),
                        deepLinkSessionID: $pendingSessionDeepLinkID,
                        embeddedInPeopleTab: true,
                        onPrefillEndSession: { sessionID in
                            pendingEndSessionPrefillID = sessionID
                            selectedTab = .work
                        }
                    )
                case .live:
                    LiveSessionsView(
                        apiClient: connectionVM.buildAPIClient(),
                        broadcaster: sseBroadcaster
                    ) { sessionID in
                        pendingSessionDeepLinkID = sessionID
                        selectedPeopleSection = .sessions
                    }
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
    }

    private var peopleHeader: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(alignment: .center, spacing: LoomSpacing.sm) {
                Text("Agents")
                    .font(.system(size: 30, weight: .bold, design: .rounded))
                    .foregroundStyle(LoomColors.textPrimary)
                    .lineLimit(1)
                    .minimumScaleFactor(0.72)
                    .allowsTightening(true)
                    .fixedSize(horizontal: true, vertical: false)
                    .accessibilityAddTraits(.isHeader)

                Button {
                    selectedTab = .spawn
                } label: {
                    Image(systemName: "sparkles")
                        .font(.title3.weight(.semibold))
                        .foregroundStyle(LoomColors.accent)
                        .frame(width: 34, height: 34)
                        .contentShape(Circle())
                }
                .accessibilityLabel("Spawn new agent")

                Spacer(minLength: 0)
            }

            Picker("Agents Section", selection: $selectedPeopleSection) {
                Text("Roster").tag(PeopleSection.agents)
                Text("Sessions").tag(PeopleSection.sessions)
                Text("Live").tag(PeopleSection.live)
            }
            .pickerStyle(.segmented)
            .frame(maxWidth: .infinity)

            Text(peopleSectionSubtitle)
                .font(.caption)
                .foregroundStyle(LoomColors.textSecondary)
                .lineLimit(2)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(.horizontal, LoomSpacing.xl)
        .padding(.top, 10)
        .padding(.bottom, LoomSpacing.sm)
    }

    private var peopleSectionSubtitle: String {
        switch selectedPeopleSection {
        case .agents:
            return "Every agent on your fleet — live, idle, offline."
        case .sessions:
            return "Agent sessions across every namespace."
        case .live:
            return "Live tool calls flowing through every active session — public-tier redacted."
        }
    }

    // MARK: - SSE Lifecycle

    private func setupSSE() {
        guard !UITestFixture.isEnabled else { return }
        guard sseClient == nil else { return }
        guard let apiClient = connectionVM.buildAPIClient(),
              let request = try? apiClient.sseRequest()
        else { return }
        let client = SSEClient(request: request, session: apiClient.sseSession())
        client.onStateChange = { [weak healthMonitor] state in
            healthMonitor?.handleSSEStateChange(state)
        }
        sseClient = client

        // Publish recovery-SLO telemetry to the backend (MBL-5 slice 2). The
        // uploader is bound to this connection's authenticated client; on each
        // transient-outage recovery it resends the rolling window (idempotent —
        // the backend replaces the device snapshot). Degrades gracefully when
        // the `mobile:telemetry` scope is not granted.
        let uploader = RecoveryTelemetryUploader(client: apiClient)
        recoveryUploader = uploader
        healthMonitor.onRecovery = { samples in
            await uploader.upload(samples: samples)
        }

        client.connect()
        sseBroadcaster.start(sseClient: client)
    }

    private func teardownSSE() {
        sseBroadcaster.stop()
        sseClient?.disconnect()
        sseClient = nil
        healthMonitor.onRecovery = nil
        recoveryUploader = nil
    }
}
