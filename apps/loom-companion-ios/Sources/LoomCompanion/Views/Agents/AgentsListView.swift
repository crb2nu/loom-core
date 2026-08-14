import SwiftUI
import LoomCompanionKit

/// Destinations reachable from the agent roster.
///
/// Agents with a live session push straight to the session detail (the
/// richer surface); session-less agents push `AgentDetailView`, which still
/// has real content — presence, branch, attention reasons, pipelines for the
/// agent's branch. Before this existed, session-less rows were inert and the
/// `loom://agent/<id>` deep link had nowhere to land.
enum AgentsRoute: Hashable {
    case session(String)
    case agent(String)
}

struct AgentsListView: View {
    @State private var viewModel: AgentsViewModel
    @State private var showingCreateSheet = false
    @State private var navigationPath: [AgentsRoute] = []
    @State private var pendingDeepLinkSessionID: String?
    @State private var pendingDeepLinkAgentID: String?
    @State private var toastMessage: String?
    @State private var showToast = false
    @Binding private var deepLinkSessionID: String?
    @Binding private var deepLinkAgentID: String?
    private let onPrefillEndSession: ((String) -> Void)?
    private let apiClient: any LoomAPIClientProtocol
    private let broadcaster: SSEEventBroadcaster?
    private let embeddedInPeopleTab: Bool

    init(
        apiClient: APIClient?,
        broadcaster: SSEEventBroadcaster? = nil,
        deepLinkSessionID: Binding<String?> = .constant(nil),
        deepLinkAgentID: Binding<String?> = .constant(nil),
        embeddedInPeopleTab: Bool = false,
        onPrefillEndSession: ((String) -> Void)? = nil
    ) {
        let client: any LoomAPIClientProtocol = apiClient ?? NoOpAgentsClient()
        self.apiClient = client
        self.broadcaster = broadcaster
        _deepLinkSessionID = deepLinkSessionID
        _deepLinkAgentID = deepLinkAgentID
        self.embeddedInPeopleTab = embeddedInPeopleTab
        self.onPrefillEndSession = onPrefillEndSession
        _viewModel = State(initialValue: AgentsViewModel(apiClient: client))
    }

    var body: some View {
        NavigationStack(path: $navigationPath) {
            List {
                if embeddedInPeopleTab {
                    EmbeddedSearchField(text: $viewModel.searchText, prompt: "Search agents")
                }

                AgentFilterView(
                    statusFilter: $viewModel.statusFilter,
                    attentionOnly: $viewModel.attentionOnly,
                    summary: viewModel.summary,
                    pipelineAgentCount: viewModel.agents.filter { $0.pipelineCount > 0 }.count,
                    attentionCount: viewModel.attentionCount
                )

                ForEach(viewModel.groupedAgents) { group in
                    // Scope/infra sections name the repo in their header, so the
                    // row omits it. Cross-repo fold sections keep it (the rows
                    // span multiple repos).
                    let showsScope = !(group.id.hasPrefix("scope:") || group.id.hasPrefix("codex-infra:"))
                    Section {
                        ForEach(group.agents) { agent in
                            if agent.hasSession, let sessionId = agent.sessionId {
                                NavigationLink(value: AgentsRoute.session(sessionId)) {
                                    AgentRowView(agent: agent, showsScope: showsScope)
                                }
                                .swipeActions(edge: .trailing) {
                                    if agent.sessionStatus == "active" {
                                        Button(role: .destructive) {
                                            HapticManager.medium()
                                            onPrefillEndSession?(sessionId)
                                        } label: {
                                            Label("End Session", systemImage: "stop.circle")
                                        }
                                        .tint(LoomColors.statusCritical)
                                    }
                                }
                            } else {
                                // Session-less agents are still navigable —
                                // AgentDetailView shows presence, branch,
                                // attention reasons, and branch pipelines.
                                NavigationLink(value: AgentsRoute.agent(agent.agentId)) {
                                    AgentRowView(agent: agent, showsScope: showsScope)
                                }
                            }
                        }
                    } header: {
                        agentGroupHeader(group)
                    }
                }
            }
            .listStyle(.plain)
            .modifier(EmbeddedListChrome(isEmbedded: embeddedInPeopleTab))
            .navigationTitle(embeddedInPeopleTab ? "" : "Agents")
            .navigationBarTitleDisplayMode(.inline)
            .modifier(EmbeddedNavigationChrome(isHidden: embeddedInPeopleTab))
            .navigationDestination(for: AgentsRoute.self) { route in
                switch route {
                case let .session(sessionId):
                    SessionDetailView(sessionId: sessionId, apiClient: apiClient)
                case let .agent(agentId):
                    if let agent = viewModel.agents.first(where: { $0.agentId == agentId }) {
                        AgentDetailView(agent: agent, apiClient: apiClient)
                    } else {
                        ContentUnavailableView {
                            Label("Agent not found", systemImage: "person.slash")
                        } description: {
                            Text("\(agentId) is no longer in the roster.")
                        }
                    }
                }
            }
            .modifier(SearchableWhenStandalone(
                isEnabled: !embeddedInPeopleTab,
                text: $viewModel.searchText,
                prompt: "Search agents"
            ))
            .safeAreaInset(edge: .bottom) {
                Color.clear
                    .frame(height: 96)
                    .allowsHitTesting(false)
            }
            .refreshable {
                await viewModel.load()
                HapticManager.light()
            }
            .toolbar {
                if !embeddedInPeopleTab {
                    ToolbarItem(placement: .primaryAction) {
                        Button {
                            showingCreateSheet = true
                        } label: {
                            Label("New Session", systemImage: "plus")
                        }
                    }
                }
            }
            .sheet(isPresented: $showingCreateSheet) {
                Task { await viewModel.load() }
            } content: {
                CreateSessionView(viewModel: SessionsViewModel(apiClient: apiClient))
            }
            .overlay {
                if viewModel.isLoading && viewModel.agents.isEmpty {
                    VStack(spacing: LoomSpacing.sm) {
                        ForEach(0..<5, id: \.self) { i in
                            SkeletonAgentRow()
                                .cardAppear(index: i)
                        }
                    }
                    .padding()
                } else if let error = viewModel.error, viewModel.agents.isEmpty {
                    ContentUnavailableView {
                        Label("Connection Error", systemImage: "wifi.exclamationmark")
                    } description: {
                        Text(error.description)
                    } actions: {
                        Button("Retry") {
                            Task { await viewModel.load() }
                        }
                        .buttonStyle(.borderedProminent)
                    }
                } else if viewModel.agents.isEmpty && !viewModel.isLoading {
                    ScrollView {
                        LoomEmptyState(
                            tone: .idle,
                            title: "No agents yet",
                            detail: "Agents appear here when coding agents connect via presence or sessions.\nStart one from your terminal or spawn from the Work tab."
                        ) {
                            if !embeddedInPeopleTab {
                                Button {
                                    showingCreateSheet = true
                                } label: {
                                    Label("Create session", systemImage: "plus.circle")
                                        .font(LoomTypography.labelLarge)
                                }
                                .buttonStyle(.borderedProminent)
                                .tint(LoomColors.accent)
                            }
                        }
                    }
                } else if viewModel.filteredAgents.isEmpty && !viewModel.agents.isEmpty {
                    ScrollView {
                        LoomEmptyState(
                            tone: .attention,
                            title: "No matching agents",
                            detail: "Filter excluded \(viewModel.agents.count) agent\(viewModel.agents.count == 1 ? "" : "s"). Adjust filters or clear search."
                        )
                    }
                }
            }
            .task {
                await viewModel.load()
                if let broadcaster {
                    viewModel.startListening(broadcaster: broadcaster)
                }
                pendingDeepLinkSessionID = deepLinkSessionID
                pendingDeepLinkAgentID = deepLinkAgentID
                resolveSessionDeepLink()
                resolveAgentDeepLink()
            }
            .onChange(of: deepLinkSessionID) { _, newValue in
                pendingDeepLinkSessionID = newValue
                resolveSessionDeepLink()
            }
            .onChange(of: deepLinkAgentID) { _, newValue in
                pendingDeepLinkAgentID = newValue
                resolveAgentDeepLink()
            }
            .overlay(alignment: .top) {
                if showToast, let toastMessage {
                    Text(toastMessage)
                        .font(.caption)
                        .foregroundStyle(.white)
                        .padding(.horizontal, 12)
                        .padding(.vertical, 8)
                        .background(Color.black.opacity(0.85))
                        .clipShape(Capsule())
                        .padding(.top, 8)
                        .transition(.opacity)
                }
            }
        }
    }

    private func resolveSessionDeepLink() {
        guard let requested = pendingDeepLinkSessionID?.trimmingCharacters(in: .whitespacesAndNewlines),
              !requested.isEmpty
        else { return }

        if viewModel.agents.contains(where: { $0.sessionId == requested }) {
            navigationPath.append(.session(requested))
            pendingDeepLinkSessionID = nil
            deepLinkSessionID = nil
            return
        }

        guard !viewModel.isLoading else { return }

        pendingDeepLinkSessionID = nil
        deepLinkSessionID = nil
        showToastMessage("Session \(requested) is not in the current list")
    }

    /// Consume `loom://agent/<id>` (and the SessionSummaryWidget tap, which
    /// issues the same link) by pushing that agent's detail. Falls back to a
    /// toast when the roster doesn't contain the agent.
    private func resolveAgentDeepLink() {
        guard let requested = pendingDeepLinkAgentID?.trimmingCharacters(in: .whitespacesAndNewlines),
              !requested.isEmpty
        else { return }

        if let agent = viewModel.agents.first(where: { $0.agentId == requested }) {
            // Prefer the session surface when the agent has one; otherwise
            // the agent detail. Same rule as the row taps.
            if agent.hasSession, let sessionId = agent.sessionId, !sessionId.isEmpty {
                navigationPath.append(.session(sessionId))
            } else {
                navigationPath.append(.agent(agent.agentId))
            }
            pendingDeepLinkAgentID = nil
            deepLinkAgentID = nil
            return
        }

        guard !viewModel.isLoading else { return }

        pendingDeepLinkAgentID = nil
        deepLinkAgentID = nil
        showToastMessage("Agent \(requested) is not in the current list")
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

    @ViewBuilder
    private func agentGroupHeader(_ group: UnifiedAgentGroup) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text(group.title)
                    .font(LoomTypography.bodyMedium)
                    .foregroundStyle(LoomColors.textPrimary)
                Text("\(group.agents.count)")
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.textTertiary)
            }
            if let subtitle = group.subtitle, !subtitle.isEmpty {
                Text(subtitle)
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.textTertiary)
                    .lineLimit(1)
            }
        }
        .textCase(nil)
    }
}

private struct NoOpAgentsClient: LoomAPIClientProtocol {
    func request<T: Decodable>(_ endpoint: Endpoint) async throws -> T {
        throw LoomAPIError.noToken
    }
}

private struct EmbeddedSearchField: View {
    @Binding var text: String
    let prompt: String

    var body: some View {
        HStack(spacing: LoomSpacing.sm) {
            Image(systemName: "magnifyingglass")
                .font(.body)
                .foregroundStyle(LoomColors.textSecondary)
            TextField(prompt, text: $text)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
        }
        .padding(.horizontal, LoomSpacing.md)
        .padding(.vertical, 10)
        .background(LoomColors.bgElevated, in: Capsule())
        .listRowInsets(EdgeInsets(top: 0, leading: 20, bottom: 6, trailing: 20))
        .listRowBackground(Color.clear)
    }
}

private struct EmbeddedListChrome: ViewModifier {
    let isEmbedded: Bool

    @ViewBuilder
    func body(content: Content) -> some View {
        if isEmbedded {
            content
                .contentMargins(.top, 0, for: .scrollContent)
                .scrollContentBackground(.hidden)
        } else {
            content
        }
    }
}

private struct EmbeddedNavigationChrome: ViewModifier {
    let isHidden: Bool

    @ViewBuilder
    func body(content: Content) -> some View {
        if isHidden {
            content.toolbar(.hidden, for: .navigationBar)
        } else {
            content
        }
    }
}

private struct SearchableWhenStandalone: ViewModifier {
    let isEnabled: Bool
    @Binding var text: String
    let prompt: String

    @ViewBuilder
    func body(content: Content) -> some View {
        if isEnabled {
            content.searchable(text: $text, prompt: Text(prompt))
        } else {
            content
        }
    }
}
