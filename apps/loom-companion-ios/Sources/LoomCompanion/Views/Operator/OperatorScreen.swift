// OperatorScreen — the unified operator surface, on mobile.
//
// The iOS twin of the web HUD's Operator Deck (#operator/deck): one screen
// that leads with what's in flight (Mills pipelines, live agent sessions)
// beside the plan queue, with jump-offs into the full Mills / Agents / Work
// surfaces for anything deeper. It replaces DashboardView as the root of the
// first tab; the classic dashboard stays reachable from the toolbar.
//
// Presentation only, per the Mills/Weaver convention: all decoding and
// degradation semantics live in LoomCompanionKit (MillsAPI's calm-empty
// contract for 502/503/404, the enveloped /api/mobile/v1/agents read). The
// screen keeps its last good snapshot across transient failures.

import LoomCompanionKit
import SwiftUI

public struct OperatorScreen: View {
    /// Cross-tab jumps the deck offers; ContentView routes them, mirroring
    /// DashboardView's action-closure pattern.
    public enum QuickAction {
        case people
        case liveActivities
        case work
        case mills
    }

    private let api: MillsAPIProtocol?
    private let controlAPI: MillsControlAPIProtocol?
    private let client: (any LoomAPIClientProtocol)?
    private let vendorAPI: VendorSessionsAPIProtocol?
    private let onNavigate: (QuickAction) -> Void

    @Environment(\.navigationCoordinator) private var navigationCoordinator

    @State private var pipelineRuns: [MillsPipelineRun] = []
    @State private var kpi: MillsKPISnapshot?
    @State private var agents: [UnifiedAgent] = []
    @State private var agentsSummary: UnifiedAgentsSummary?
    @State private var plans: [MillsPlan] = []
    @State private var loading = true
    @State private var loadError: String?
    /// Roster row the command sheet is open for. Tapping an agent commands it
    /// in place — the deck is the one place to view AND control the fleet.
    @State private var commandAgent: UnifiedAgent?
    /// Everything waiting on the operator (approvals, handoffs, auto-fixes).
    @State private var inboxVM: OperatorInboxViewModel?

    // Vendor transcripts drill-down (web VendorTranscripts twin). Fetch on
    // open / on search, NOT in the poll loop — transcripts change slowly and
    // the deck already polls several stores.
    @State private var transcriptsOpen = false
    @State private var transcriptQuery = ""
    /// Last executed search; "" renders the recent-sessions list instead.
    @State private var transcriptSubmitted = ""
    @State private var transcriptSessions: [VendorSession] = []
    @State private var transcriptMatches: [VendorSessionMatch] = []
    @State private var transcriptDegraded = false
    @State private var transcriptUnavailable = false
    @State private var transcriptLoading = false
    @State private var transcriptError: String?
    /// Monotonic sequence guards against a slow response landing after a
    /// newer request (open toggle, new search) already refreshed the state.
    @State private var transcriptSeq = 0

    /// Production initializer. Unpaired (nil client) renders the standard
    /// "pair to view" empty state.
    public init(
        apiClient: APIClient?,
        onNavigate: @escaping (QuickAction) -> Void = { _ in }
    ) {
        self.api = apiClient.map(MillsAPI.init(client:))
        self.controlAPI = apiClient.map(MillsControlAPI.init(client:))
        self.client = apiClient
        self.vendorAPI = apiClient.map(VendorSessionsAPI.init(client:))
        self.onNavigate = onNavigate
        _inboxVM = State(initialValue: apiClient.map(OperatorInboxViewModel.init(apiClient:)))
    }

    /// Test-only initializer accepting fakes.
    public init(
        api: MillsAPIProtocol?,
        controlAPI: MillsControlAPIProtocol? = nil,
        client: (any LoomAPIClientProtocol)? = nil,
        vendorAPI: VendorSessionsAPIProtocol? = nil,
        onNavigate: @escaping (QuickAction) -> Void = { _ in }
    ) {
        self.api = api
        self.controlAPI = controlAPI
        self.client = client
        self.vendorAPI = vendorAPI
        self.onNavigate = onNavigate
        _inboxVM = State(initialValue: client.map(OperatorInboxViewModel.init(apiClient:)))
    }

    // MARK: - Derived

    private var inFlight: [MillsPipelineRun] {
        pipelineRuns.filter { !$0.category.isTerminal }
    }

    /// Plans still waiting on a decision — the mobile queue mirrors the web
    /// deck's "Ready" bucket (draft + planned).
    private var readyPlans: [MillsPlan] {
        plans.filter { $0.phase == "draft" || $0.phase == "planned" }
    }

    private var liveAgents: [UnifiedAgent] {
        agents.filter { $0.status == .active || $0.status == .idle }
    }

    /// Roster rows the deck shows: live agents plus anything flagged for
    /// attention or orphaned — an offline agent that needs you still belongs
    /// on the one screen you check.
    ///
    /// Ordering is deliberately anti-jump (the web deck's hard-won rule):
    /// bucket by urgency, then a stable conversation/agent-id sort — never by
    /// heartbeat recency, which reshuffles rows on every poll.
    private var deckAgents: [UnifiedAgent] {
        let visible = agents.filter {
            $0.status == .active || $0.status == .idle || $0.needsAttention || $0.isOrphan
        }
        func bucket(_ agent: UnifiedAgent) -> Int {
            if agent.needsAttention || agent.isOrphan { return 0 }
            return agent.status == .active ? 1 : 2
        }
        return visible.sorted { lhs, rhs in
            let lb = bucket(lhs), rb = bucket(rhs)
            if lb != rb { return lb < rb }
            if lhs.conversationId != rhs.conversationId {
                return lhs.conversationId < rhs.conversationId
            }
            return lhs.agentId < rhs.agentId
        }
    }

    /// One row per conversation: the sorted roster collapses its twins
    /// (Claude cross-repo hops, Codex workspace pairs) so the deck reads as
    /// "who is working", not "how many presence rows exist". Order-preserving,
    /// so the urgency bucketing above survives grouping — the representative
    /// is the highest-ranked member.
    private var deckGroups: [UnifiedConversationGroup] {
        UnifiedConversationGroup.group(deckAgents)
    }

    private var isFirstLoad: Bool {
        loading && kpi == nil && pipelineRuns.isEmpty && agents.isEmpty && plans.isEmpty && loadError == nil
    }

    // MARK: - Body

    public var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: LoomSpacing.lg) {
                if client == nil && api == nil {
                    disconnectedState
                } else if isFirstLoad {
                    VStack(spacing: LoomSpacing.sm) {
                        ForEach(0 ..< 6, id: \.self) { _ in
                            SkeletonCompactRow()
                        }
                    }
                } else {
                    if let loadError {
                        Text(loadError)
                            .font(LoomTypography.caption)
                            .foregroundStyle(LoomColors.statusDegraded)
                    }
                    signalRow
                    if let inboxVM {
                        needsYouSection(inboxVM)
                    }
                    inFlightSection
                    agentsSection
                    queueSection
                    if vendorAPI != nil {
                        transcriptsSection
                    }
                }
            }
            .padding(.horizontal, LoomSpacing.lg)
            .padding(.vertical, LoomSpacing.lg)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(LoomColors.bgPrimary.ignoresSafeArea())
        .navigationTitle("Operator")
        .refreshable { await reload() }
        .task { await pollLoop() }
        .sheet(item: $commandAgent) { agent in
            if let client {
                AgentCommandSheet(
                    agent: agent,
                    apiClient: client,
                    onNavigate: { target in
                        switch target {
                        case let .session(id):
                            navigationCoordinator?.navigateToSession(id: id)
                        case let .agentDetail(id):
                            navigationCoordinator?.navigateToAgent(id: id)
                        }
                    },
                    onMutated: { Task { await reload() } }
                )
            }
        }
    }

    // MARK: - Sections

    private var disconnectedState: some View {
        LoomEmptyState(
            tone: .idle,
            title: "Pair to operate",
            detail: "Connect to your Loom HUD to control agents and the Mills factory from here."
        )
    }

    private var signalRow: some View {
        HStack(spacing: LoomSpacing.sm) {
            signalChip(
                label: "Mills",
                value: "\(inFlight.count)",
                detail: "in flight",
                color: inFlight.isEmpty ? LoomColors.fgMuted : LoomColors.statusActive
            ) { onNavigate(.mills) }
            signalChip(
                label: "Agents",
                value: "\(agentsSummary?.activeAgents ?? liveAgents.count)",
                detail: "active",
                color: (agentsSummary?.activeAgents ?? 0) > 0 ? LoomColors.statusActive : LoomColors.fgMuted
            ) { onNavigate(.people) }
            signalChip(
                label: "Queue",
                value: "\(readyPlans.count)",
                detail: "ready",
                color: readyPlans.isEmpty ? LoomColors.fgMuted : LoomColors.accent
            ) { onNavigate(.work) }
        }
    }

    private func signalChip(
        label: String,
        value: String,
        detail: String,
        color: Color,
        action: @escaping () -> Void
    ) -> some View {
        Button(action: action) {
            VStack(alignment: .leading, spacing: LoomSpacing.xxs) {
                Text(label.uppercased())
                    .font(LoomTypography.sectionTitle)
                    .foregroundStyle(LoomColors.fgMuted)
                Text(value)
                    .font(LoomTypography.counterMedium)
                    .foregroundStyle(color)
                Text(detail)
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.fgSecondary)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(LoomSpacing.md)
            .background(LoomColors.bgSecondary)
            .clipShape(RoundedRectangle(cornerRadius: 12))
            .overlay(
                RoundedRectangle(cornerRadius: 12)
                    .strokeBorder(LoomColors.borderSubtle, lineWidth: 1)
            )
        }
        .buttonStyle(.plain)
    }

    // MARK: - Needs you

    /// Everything blocked on an operator decision, actionable inline:
    /// workflow approvals, agent handoffs, and auto-fix proposals. This is
    /// the control half of "one place for the whole fleet" — approving from
    /// here replaces a trip through the Work tab's sub-surfaces.
    private func needsYouSection(_ inbox: OperatorInboxViewModel) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            HStack {
                Text("Needs you".uppercased())
                    .font(LoomTypography.sectionTitle)
                    .tracking(LoomTypography.kindLabelTracking)
                    .foregroundStyle(inbox.totalCount > 0 ? LoomColors.statusDegraded : LoomColors.fgMuted)
                Text("\(inbox.totalCount)")
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(inbox.totalCount > 0 ? LoomColors.statusDegraded : LoomColors.fgDim)
                Spacer()
            }

            if !inbox.loaded {
                calmRow("Checking for pending decisions…")
            } else if inbox.totalCount == 0 {
                calmRow("Nothing waiting on you.")
            } else {
                ForEach(inbox.approvals) { workflow in
                    inboxRow(
                        icon: "checkmark.seal",
                        title: workflow.name ?? workflow.id,
                        detail: "Workflow step waiting for approval",
                        isBusy: inbox.busyIDs.contains(workflow.id),
                        approveLabel: "Approve",
                        onApprove: { Task { await inbox.approveWorkflow(workflow) } },
                        onReject: { Task { await inbox.rejectWorkflow(workflow) } }
                    )
                }
                ForEach(inbox.handoffs) { handoff in
                    inboxRow(
                        icon: "arrow.left.arrow.right",
                        title: "\(handoff.fromAgent) → \(handoff.toAgent)",
                        detail: handoff.summary.isEmpty ? "Handoff pending" : handoff.summary,
                        isBusy: inbox.busyIDs.contains(handoff.id),
                        approveLabel: "Accept",
                        onApprove: { Task { await inbox.acceptHandoff(handoff) } },
                        onReject: { Task { await inbox.rejectHandoff(handoff) } }
                    )
                }
                ForEach(inbox.proposals) { proposal in
                    inboxRow(
                        icon: "bandage",
                        title: proposal.description.isEmpty ? "Auto-fix proposal" : proposal.description,
                        detail: autofixDetail(proposal),
                        isBusy: inbox.busyIDs.contains(proposal.id),
                        approveLabel: "Approve",
                        onApprove: { Task { await inbox.approveProposal(proposal) } },
                        onReject: { Task { await inbox.rejectProposal(proposal) } }
                    )
                }
            }

            if let message = inbox.actionMessage {
                Label(message, systemImage: "checkmark.circle")
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.statusHealthy)
            }
            if let error = inbox.actionError {
                Label(error, systemImage: "exclamationmark.triangle")
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.statusCritical)
            }
        }
    }

    private func autofixDetail(_ proposal: AutofixProposal) -> String {
        var parts = ["Auto-fix · \(proposal.strategy)"]
        if proposal.confidence > 0 {
            parts.append("\(Int(proposal.confidence * 100))% confident")
        }
        return parts.joined(separator: " · ")
    }

    private func inboxRow(
        icon: String,
        title: String,
        detail: String,
        isBusy: Bool,
        approveLabel: String,
        onApprove: @escaping () -> Void,
        onReject: @escaping () -> Void
    ) -> some View {
        LoomCard(priority: .compact, accent: .severity(LoomColors.statusDegraded, pulse: false)) {
            HStack(spacing: LoomSpacing.sm) {
                Image(systemName: icon)
                    .font(.system(size: 13))
                    .foregroundStyle(LoomColors.statusDegraded)
                    .frame(width: 18)
                VStack(alignment: .leading, spacing: 2) {
                    Text(title)
                        .font(LoomTypography.bodyMedium)
                        .foregroundStyle(LoomColors.fgPrimary)
                        .lineLimit(1)
                    Text(detail)
                        .font(LoomTypography.caption)
                        .foregroundStyle(LoomColors.fgSecondary)
                        .lineLimit(2)
                }
                Spacer(minLength: LoomSpacing.sm)
                if isBusy {
                    ProgressView().controlSize(.small)
                } else {
                    inboxActionButton(approveLabel, color: LoomColors.statusHealthy, action: onApprove)
                    inboxActionButton("Reject", color: LoomColors.fgMuted, action: onReject)
                }
            }
        }
    }

    private func inboxActionButton(
        _ label: String,
        color: Color,
        action: @escaping () -> Void
    ) -> some View {
        Button {
            HapticManager.selection()
            action()
        } label: {
            Text(label)
                .font(LoomTypography.labelSmall)
                .foregroundStyle(color)
                .padding(.horizontal, LoomSpacing.sm)
                .padding(.vertical, LoomSpacing.xxs)
                .background(LoomColors.bgSecondary)
                .clipShape(Capsule())
                .overlay(Capsule().strokeBorder(LoomColors.borderSubtle, lineWidth: 1))
        }
        .buttonStyle(.plain)
    }

    private var inFlightSection: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            sectionHeader("In flight", count: inFlight.count) { onNavigate(.mills) }
            if inFlight.isEmpty {
                calmRow("No Mills pipelines running.")
            } else {
                ForEach(inFlight.prefix(8)) { run in
                    Button { onNavigate(.mills) } label: { runRow(run) }
                        .buttonStyle(.plain)
                }
            }
        }
    }

    private func runRow(_ run: MillsPipelineRun) -> some View {
        LoomCard(priority: .compact, accent: .severity(runColor(run), pulse: run.state == "running")) {
            HStack(spacing: LoomSpacing.sm) {
                VStack(alignment: .leading, spacing: 2) {
                    Text(run.backlogID)
                        .font(LoomTypography.bodyMedium)
                        .foregroundStyle(LoomColors.fgPrimary)
                        .lineLimit(1)
                    Text(run.template + (run.attempts > 1 ? " · attempt \(run.attempts)" : ""))
                        .font(LoomTypography.caption)
                        .foregroundStyle(LoomColors.fgSecondary)
                        .lineLimit(1)
                }
                Spacer(minLength: LoomSpacing.sm)
                VStack(alignment: .trailing, spacing: 2) {
                    LoomPill(run.state, color: runColor(run))
                    if let started = run.startedAt {
                        Text(LoomFormat.relativeCompact(seconds: Int(Date().timeIntervalSince(started))))
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.fgMuted)
                    }
                }
            }
        }
    }

    private func runColor(_ run: MillsPipelineRun) -> Color {
        switch run.state {
        case "escalated": return LoomColors.statusCritical
        case "paused": return LoomColors.statusDegraded
        case "running": return LoomColors.statusActive
        default: return LoomColors.statusIdle
        }
    }

    private var agentsSection: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            sectionHeader("Agents", count: deckGroups.count) { onNavigate(.people) }
            if deckGroups.isEmpty {
                calmRow("No live agents — cluster and workstations are quiet.")
            } else {
                // Tap = command in place (the sheet, aimed at the group's
                // highest-ranked member); the full roster stays a long-press
                // (context menu, one entry per member) or the header away.
                ForEach(deckGroups.prefix(8)) { group in
                    Button { commandAgent = group.representative } label: { agentRow(group) }
                        .buttonStyle(.plain)
                        .contextMenu {
                            ForEach(group.members) { member in
                                Button {
                                    navigationCoordinator?.navigateToAgent(id: member.agentId)
                                } label: {
                                    Label(member.agentId, systemImage: "person.2.wave.2")
                                }
                            }
                        }
                }
            }
        }
    }

    private func agentRow(_ group: UnifiedConversationGroup) -> some View {
        let agent = group.representative
        let anyAttention = group.members.contains { $0.needsAttention }
        let anyOrphan = group.members.contains { $0.isOrphan }
        return LoomCard(priority: .compact, accent: anyAttention || anyOrphan
            ? .severity(LoomColors.statusDegraded, pulse: false)
            : .none
        ) {
            HStack(spacing: LoomSpacing.sm) {
                Circle()
                    .fill(agent.status == .active ? LoomColors.statusActive : LoomColors.statusIdle)
                    .frame(width: 7, height: 7)
                VStack(alignment: .leading, spacing: 2) {
                    Text(agent.displayTitle)
                        .font(LoomTypography.bodyMedium)
                        .foregroundStyle(LoomColors.fgPrimary)
                        .lineLimit(1)
                    Text(agentRowSubtitle(agent))
                        .font(LoomTypography.caption)
                        .foregroundStyle(LoomColors.fgSecondary)
                        .lineLimit(1)
                }
                Spacer(minLength: LoomSpacing.sm)
                if group.memberCount > 1 {
                    LoomPill("×\(group.memberCount)", color: LoomColors.fgMuted, style: .outlined, weight: .micro)
                }
                if agent.isSpawned {
                    LoomPill("k8s", icon: "cloud", color: LoomColors.tierShortTerm, weight: .micro)
                }
                if anyOrphan {
                    LoomPill("orphan", color: LoomColors.statusDegraded, style: .outlined, weight: .micro)
                } else if anyAttention {
                    LoomPill("attention", icon: "exclamationmark.triangle.fill", color: LoomColors.statusDegraded, weight: .micro)
                }
                Text(agent.status.rawValue.uppercased())
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(agent.status == .active ? LoomColors.statusActive : LoomColors.fgMuted)
            }
        }
    }

    /// Scope-first subtitle: repo (or branch) grounds the row, then what the
    /// agent is doing. Falls back to the agent type when nothing else exists.
    private func agentRowSubtitle(_ agent: UnifiedAgent) -> String {
        var parts: [String] = []
        if let project = agent.project, !project.isEmpty {
            parts.append(LoomFormat.lastPathComponent(project))
        } else if !agent.branch.isEmpty {
            parts.append(agent.branch)
        }
        let activity = agent.cleanedActivityLine
        if !activity.isEmpty { parts.append(activity) }
        if parts.isEmpty { parts.append(agent.harnessDisplayName) }
        return parts.joined(separator: " · ")
    }

    private var queueSection: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            sectionHeader("Plan queue", count: readyPlans.count, actionLabel: "Board") { onNavigate(.mills) }
            if readyPlans.isEmpty {
                calmRow("No plans waiting — spin one from the Mills tab.")
            } else {
                ForEach(readyPlans.prefix(6)) { plan in
                    planRow(plan)
                }
                if let controlAPI {
                    NavigationLink {
                        PlansScreen(api: controlAPI)
                    } label: {
                        Text("Open plans board →")
                            .font(LoomTypography.labelLarge)
                            .foregroundStyle(LoomColors.info)
                    }
                    .padding(.top, LoomSpacing.xxs)
                }
            }
        }
    }

    private func planRow(_ plan: MillsPlan) -> some View {
        LoomCard(priority: .compact) {
            HStack(spacing: LoomSpacing.sm) {
                VStack(alignment: .leading, spacing: 2) {
                    Text(plan.title)
                        .font(LoomTypography.bodyMedium)
                        .foregroundStyle(LoomColors.fgPrimary)
                        .lineLimit(1)
                    if let project = plan.project, !project.isEmpty {
                        Text(project)
                            .font(LoomTypography.monoCaption)
                            .foregroundStyle(LoomColors.fgMuted)
                            .lineLimit(1)
                    }
                }
                Spacer(minLength: LoomSpacing.sm)
                if let priority = plan.priority, !priority.isEmpty {
                    LoomPill(priority, color: priority == "P0" ? LoomColors.statusCritical : LoomColors.info)
                }
                LoomPill(plan.phase, color: LoomColors.fgSecondary, style: .outlined)
            }
        }
    }

    // MARK: - Vendor transcripts

    /// Collapsible "Vendor transcripts" affordance — recent claude/codex
    /// desktop CLI transcripts on the HUD's workstation, plus a substring
    /// search, mirroring the web deck's VendorTranscripts section.
    private var transcriptsSection: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            Button {
                transcriptsOpen.toggle()
                if transcriptsOpen {
                    Task { await loadTranscripts() }
                }
            } label: {
                HStack(spacing: LoomSpacing.xs) {
                    Image(systemName: "chevron.right")
                        .font(.system(size: 9, weight: .bold))
                        .rotationEffect(.degrees(transcriptsOpen ? 90 : 0))
                    Text("Vendor transcripts".uppercased())
                        .font(LoomTypography.sectionTitle)
                        .tracking(LoomTypography.kindLabelTracking)
                }
                .foregroundStyle(LoomColors.fgMuted)
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Vendor transcripts")

            if transcriptsOpen {
                transcriptsBody
            }
        }
    }

    @ViewBuilder
    private var transcriptsBody: some View {
        HStack(spacing: LoomSpacing.xs) {
            TextField("Search claude + codex transcripts…", text: $transcriptQuery)
                .font(LoomTypography.caption)
                .textFieldStyle(.plain)
                .autocorrectionDisabled()
                .textInputAutocapitalization(.never)
                .onSubmit { submitTranscriptSearch() }
                .padding(.horizontal, LoomSpacing.sm)
                .padding(.vertical, LoomSpacing.xs)
                .background(LoomColors.bgSecondary)
                .clipShape(RoundedRectangle(cornerRadius: 8))
                .overlay(
                    RoundedRectangle(cornerRadius: 8)
                        .strokeBorder(LoomColors.borderSubtle, lineWidth: 1)
                )
            if transcriptSubmitted.isEmpty {
                Button("Search") { submitTranscriptSearch() }
                    .font(LoomTypography.labelSmall)
                    .foregroundStyle(LoomColors.info)
                    .buttonStyle(.plain)
            } else {
                Button("Clear") { clearTranscriptSearch() }
                    .font(LoomTypography.labelSmall)
                    .foregroundStyle(LoomColors.info)
                    .buttonStyle(.plain)
            }
        }

        if transcriptDegraded {
            calmRow("Vendor transcripts unavailable — agent bridge offline on the HUD's workstation.")
        } else if transcriptUnavailable {
            calmRow("Vendor transcripts aren't served by this HUD yet — update the Loom daemon.")
        } else if let transcriptError {
            Text(transcriptError)
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.statusDegraded)
        } else if transcriptLoading {
            calmRow("Loading…")
        } else if !transcriptSubmitted.isEmpty {
            if transcriptMatches.isEmpty {
                calmRow("No matches for “\(transcriptSubmitted)”.")
            } else {
                ForEach(transcriptMatches, id: \.uid) { match in
                    transcriptMatchRow(match)
                }
            }
        } else if transcriptSessions.isEmpty {
            calmRow("No vendor CLI sessions found on this host.")
        } else {
            ForEach(transcriptSessions, id: \.uid) { session in
                transcriptSessionRow(session)
            }
        }
    }

    private func transcriptSessionRow(_ session: VendorSession) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: LoomSpacing.sm) {
            vendorTag(session.vendor)
            if let host = session.host, !host.isEmpty {
                hostTag(host)
            }
            Text(cwdTail(session.cwd) ?? String(session.id.prefix(12)))
                .font(LoomTypography.monoCaption)
                .foregroundStyle(LoomColors.fgSecondary)
                .lineLimit(1)
            Spacer(minLength: LoomSpacing.sm)
            Text(LoomFormat.relativeCompact(fromISO: session.modifiedAt ?? ""))
                .font(LoomTypography.monoCaption)
                .foregroundStyle(LoomColors.fgMuted)
        }
        .padding(.vertical, 2)
    }

    private func transcriptMatchRow(_ match: VendorSessionMatch) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(alignment: .firstTextBaseline, spacing: LoomSpacing.sm) {
                vendorTag(match.vendor)
                if let host = match.host, !host.isEmpty {
                    hostTag(host)
                }
                if let tail = cwdTail(match.cwd) {
                    Text(tail)
                        .font(LoomTypography.monoCaption)
                        .foregroundStyle(LoomColors.fgSecondary)
                        .lineLimit(1)
                }
                Spacer(minLength: LoomSpacing.sm)
                Text(transcriptMatchMeta(match))
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
            }
            Text(match.snippet)
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.fgSecondary)
                .lineLimit(3)
        }
        .padding(.vertical, LoomSpacing.xxs)
    }

    private func transcriptMatchMeta(_ match: VendorSessionMatch) -> String {
        var parts: [String] = []
        if let ts = match.timestamp, !ts.isEmpty {
            parts.append(LoomFormat.relativeCompact(fromISO: ts))
        }
        // line == 0 means the federating mirror tail-seeked a large
        // transcript and the absolute line number is unknown — hide it.
        if match.line > 0 {
            parts.append("L\(match.line)")
        }
        return parts.joined(separator: " · ")
    }

    /// Source-workstation tag for federated rows (mirror pushes through the
    /// cluster HUD), mirroring the web's .vt-host pill.
    private func hostTag(_ host: String) -> some View {
        Text(host)
            .font(LoomTypography.monoCaption)
            .foregroundStyle(LoomColors.fgMuted)
            .lineLimit(1)
    }

    /// Vendor tag mirroring the web's per-vendor coloring (claude=warning,
    /// codex=info).
    private func vendorTag(_ vendor: String) -> some View {
        Text(vendor.uppercased())
            .font(LoomTypography.monoCaption)
            .foregroundStyle(vendor == "claude" ? LoomColors.statusDegraded : LoomColors.info)
    }

    /// Last two path segments — enough to recognize a repo without the noise.
    private func cwdTail(_ cwd: String?) -> String? {
        guard let cwd, !cwd.isEmpty else { return nil }
        let tail = cwd.split(separator: "/").suffix(2).joined(separator: "/")
        return tail.isEmpty ? nil : tail
    }

    private func submitTranscriptSearch() {
        transcriptSubmitted = transcriptQuery.trimmingCharacters(in: .whitespacesAndNewlines)
        Task { await loadTranscripts() }
    }

    private func clearTranscriptSearch() {
        transcriptQuery = ""
        transcriptSubmitted = ""
        Task { await loadTranscripts() }
    }

    private func loadTranscripts() async {
        guard let vendorAPI else { return }
        transcriptSeq += 1
        let mySeq = transcriptSeq
        transcriptLoading = true
        transcriptError = nil
        do {
            if transcriptSubmitted.isEmpty {
                let res = try await vendorAPI.recentSessions(cwdContains: nil, limit: 8)
                guard mySeq == transcriptSeq else { return }
                transcriptSessions = res.sessions
                transcriptDegraded = res.degraded
                transcriptUnavailable = res.unavailable
            } else {
                let res = try await vendorAPI.search(
                    query: transcriptSubmitted, cwdContains: nil, maxResults: 20)
                guard mySeq == transcriptSeq else { return }
                transcriptMatches = res.matches
                transcriptDegraded = res.degraded
                transcriptUnavailable = res.unavailable
            }
        } catch {
            guard mySeq == transcriptSeq else { return }
            transcriptError = "Couldn't read vendor transcripts."
        }
        if mySeq == transcriptSeq { transcriptLoading = false }
    }

    private func sectionHeader(
        _ title: String,
        count: Int,
        actionLabel: String = "All",
        action: @escaping () -> Void
    ) -> some View {
        HStack {
            Text(title.uppercased())
                .font(LoomTypography.sectionTitle)
                .tracking(LoomTypography.kindLabelTracking)
                .foregroundStyle(LoomColors.fgMuted)
            Text("\(count)")
                .font(LoomTypography.monoCaption)
                .foregroundStyle(LoomColors.fgDim)
            Spacer()
            Button(action: action) {
                Text("\(actionLabel) →")
                    .font(LoomTypography.labelSmall)
                    .foregroundStyle(LoomColors.info)
            }
            .buttonStyle(.plain)
        }
    }

    private func calmRow(_ text: String) -> some View {
        Text(text)
            .font(LoomTypography.caption)
            .foregroundStyle(LoomColors.fgMuted)
            .padding(.vertical, LoomSpacing.xs)
    }

    // MARK: - Data

    /// Sequential best-effort reads; each surface keeps its last good
    /// snapshot on failure (MillsAPI already folds operator-absence into
    /// calm empties). Only a total miss across every read surfaces an error.
    private func reload() async {
        loading = true
        defer { loading = false }
        var anySucceeded = false

        if let api {
            if let runs = try? await api.pipelineRuns() {
                pipelineRuns = runs
                anySucceeded = true
            }
            if let snapshot = try? await api.latestKPI(window: "1d") {
                kpi = snapshot
                anySucceeded = true
            }
        }
        if let client {
            if let resp: UnifiedAgentsResponse = try? await client.request(.agents(limit: 50)) {
                agents = resp.agents
                agentsSummary = resp.summary
                anySucceeded = true
            }
        }
        if let controlAPI {
            if let list = try? await controlAPI.plans() {
                plans = list.available ? list.plans : []
                anySucceeded = true
            }
        }
        if let inboxVM {
            // The inbox VM degrades per-surface internally; its own loadError
            // only fires on a total miss, which the deck-level banner covers.
            await inboxVM.load()
            if inboxVM.loadError == nil { anySucceeded = true }
        }

        loadError = anySucceeded ? nil : "Couldn't reach the HUD — showing the last snapshot."
    }

    private func pollLoop() async {
        await reload()
        while !Task.isCancelled {
            try? await Task.sleep(nanoseconds: 10_000_000_000)
            guard !Task.isCancelled else { return }
            await reload()
        }
    }
}
