// LiveSessionsViewModel.swift — Phase 5 of the spectator plan
// (`.loom/99-implementation-plan-agent-telemetry-spectator-2026-05-04.md`).
//
// iOS-side mirror of the HUD `liveSessionsStore` (Phase 3). Subscribes to
// the existing `SSEEventBroadcaster` shared with the rest of the
// LoomCompanion app and maintains a session map keyed by session_id.
//
// Decoding is permissive: malformed events are skipped silently rather
// than tearing down the stream, so a future Go-side payload addition does
// not crash the iOS client. Tool-args on the wire are already redacted at
// TierPublic by the producers (Phase 1.3 / 2.x); we display verbatim.

import Foundation

/// Maximum tool calls retained per session. Matches the HUD store cap.
public let liveSessionsRecentCallsLimit = 20

/// Sessions retain visibility this long after `session.end` so the final
/// tail is visible before reaping. Matches the HUD store retention.
public let liveSessionsEndedRetentionSeconds: TimeInterval = 30

@Observable
@MainActor
public final class LiveSessionsViewModel {
    /// Sessions keyed by session_id. Sorted views derive from this dict.
    public private(set) var sessionsByID: [String: LiveSession] = [:]

    /// Monotonic event counter — used by tests and last-update indicators.
    public private(set) var eventCount: Int = 0

    public private(set) var isLoadingSnapshot = false

    public private(set) var snapshotError: LoomAPIError?

    @ObservationIgnored
    private let apiClient: (any LoomAPIClientProtocol)?

    @ObservationIgnored
    private let snapshotRefreshInterval: Duration

    @ObservationIgnored
    private var registrationID: UUID?

    @ObservationIgnored
    private weak var broadcaster: SSEEventBroadcaster?

    @ObservationIgnored
    private var reapTask: Task<Void, Never>?

    @ObservationIgnored
    private var snapshotRefreshTask: Task<Void, Never>?

    @ObservationIgnored
    private var hasLoadedSnapshot = false

    @ObservationIgnored
    private var activityRefreshes: Set<String> = []

    public init(
        apiClient: (any LoomAPIClientProtocol)? = nil,
        snapshotRefreshInterval: Duration = .seconds(30)
    ) {
        self.apiClient = apiClient
        self.snapshotRefreshInterval = snapshotRefreshInterval
    }

    /// Subscribe to the broadcaster's event stream. Idempotent — calling
    /// twice is a no-op so multiple `.task` modifiers in the view tree
    /// don't double-register handlers.
    public func subscribe(to broadcaster: SSEEventBroadcaster) {
        if registrationID != nil { return }
        self.broadcaster = broadcaster
        let id = broadcaster.register { [weak self] event in
            await self?.handle(event)
        }
        registrationID = id
        startReapTask()
        startSnapshotRefreshTask()
        Task { await loadInitialSessions(force: hasLoadedSnapshot) }
    }

    public func unsubscribe() {
        if let id = registrationID, let broadcaster {
            broadcaster.unregister(id)
        }
        registrationID = nil
        broadcaster = nil
        reapTask?.cancel()
        reapTask = nil
        snapshotRefreshTask?.cancel()
        snapshotRefreshTask = nil
    }

    /// Reset state. Used by tests and on auth changes.
    public func reset() {
        sessionsByID = [:]
        eventCount = 0
        hasLoadedSnapshot = false
        snapshotError = nil
    }

    /// Seed Live from the canonical active-session API, then backfill each
    /// session's recent event/context activity. SSE remains the incremental
    /// source after this snapshot, so opening the tab mid-session is reliable.
    public func loadInitialSessions(force: Bool = false) async {
        await loadSessionSnapshot(force: force, refreshActivity: true)
    }

    private func loadSessionSnapshot(force: Bool, refreshActivity: Bool) async {
        guard let apiClient else { return }
        guard !isLoadingSnapshot else { return }
        guard force || !hasLoadedSnapshot else { return }

        isLoadingSnapshot = true
        defer { isLoadingSnapshot = false }
        let reconcileMissing = hasLoadedSnapshot

        do {
            let tree: SessionTreeResponse = try await apiClient.request(.sessionsTree(status: "active"))
            await mergeSnapshot(
                Self.flatten(tree).filter { $0.status == .active },
                reconcileMissing: reconcileMissing,
                refreshActivity: refreshActivity
            )
            snapshotError = nil
            hasLoadedSnapshot = true
        } catch let preferredError as LoomAPIError {
            await loadFlatSnapshot(
                apiClient: apiClient,
                preferredError: preferredError,
                reconcileMissing: reconcileMissing,
                refreshActivity: refreshActivity
            )
        } catch {
            await loadFlatSnapshot(
                apiClient: apiClient,
                preferredError: .networkError(underlying: error.localizedDescription),
                reconcileMissing: reconcileMissing,
                refreshActivity: refreshActivity
            )
        }
    }

    /// Visible sessions, most-recent activity first, with reaped entries
    /// excluded.
    public var visibleSessions: [LiveSession] {
        sessionsByID.values
            .filter { session in
                guard let endedAt = session.endedAt else { return true }
                return Date().timeIntervalSince(endedAt) < liveSessionsEndedRetentionSeconds
            }
            .sorted { $0.lastActivity > $1.lastActivity }
    }

    public var activeSessionCount: Int {
        visibleSessions.filter { $0.endedAt == nil }.count
    }

    public var inFlightCallCount: Int {
        visibleSessions.reduce(0) { acc, s in
            acc + s.recentCalls.filter { $0.inFlight }.count
        }
    }

    /// Decode an SSE event and apply it. Public for tests; the broadcaster
    /// hookup calls this internally in production.
    public func handle(_ event: SSEEvent) {
        eventCount += 1
        guard let envelope = decode(event) else { return }
        switch envelope.canonicalType {
        case .sessionStart:
            applySessionStart(envelope.data)
        case .sessionEnd:
            applySessionEnd(envelope.data)
        case .agentStatusChange:
            applyAgentStatusChange(envelope.data)
        case .toolCallStart:
            applyToolCallStart(envelope.data)
        case .toolCallEnd:
            applyToolCallEnd(envelope.data)
        case .heartbeat:
            applyHeartbeat(envelope.data)
        case .contextAdded, .sessionStatsUpdated:
            applyContextUpdate(envelope.data)
        case .none:
            // Not a spectator event — the broadcaster gives us the full
            // SSE stream including non-spectator types.
            return
        }
    }

    // MARK: - Internal handlers

    private func applySessionStart(_ data: LiveSessionEventData) {
        guard let sid = data.sessionID, !sid.isEmpty else { return }
        var session = sessionsByID[sid]
            ?? LiveSession(id: sid, agentID: data.agentID ?? "")
        if (session.agentID.isEmpty), let aid = data.agentID, !aid.isEmpty {
            session.agentID = aid
        }
        if let namespace = data.namespace, !namespace.isEmpty { session.namespace = namespace }
        if let description = data.description, !description.isEmpty { session.description = description }
        session.lastActivity = Date()
        session.endedAt = nil
        sessionsByID[sid] = session
    }

    private func applySessionEnd(_ data: LiveSessionEventData) {
        guard let sid = data.sessionID, !sid.isEmpty else { return }
        guard var session = sessionsByID[sid] else { return }
        session.endedAt = Date()
        session.lastActivity = Date()
        sessionsByID[sid] = session
    }

    private func applyAgentStatusChange(_ data: LiveSessionEventData) {
        guard let aid = data.agentID, !aid.isEmpty else { return }
        let status = LiveAgentStatus(raw: data.status)
        for (sid, session) in sessionsByID where session.agentID == aid {
            var copy = session
            copy.agentStatus = status
            copy.lastActivity = Date()
            sessionsByID[sid] = copy
        }
    }

    private func applyHeartbeat(_ data: LiveSessionEventData) {
        guard let aid = data.agentID, !aid.isEmpty else { return }
        for (sid, session) in sessionsByID where session.agentID == aid {
            var copy = session
            copy.agentStatus = LiveAgentStatus(raw: data.status ?? "active")
            copy.currentTask = data.currentTask
            copy.branch = data.branch
            copy.activeFileCount = data.activeFiles?.count
            copy.lastActivity = Date()
            sessionsByID[sid] = copy
        }
    }

    private func applyContextUpdate(_ data: LiveSessionEventData) {
        guard let sid = data.sessionID, !sid.isEmpty else { return }
        if sessionsByID[sid] == nil {
            sessionsByID[sid] = LiveSession(id: sid, agentID: data.agentID ?? "")
        }
        sessionsByID[sid]?.lastActivity = Date()
        Task { await refreshSessionActivity(sessionID: sid) }
    }

    private func applyToolCallStart(_ data: LiveSessionEventData) {
        guard let sid = data.sessionID, let cid = data.callID,
              !sid.isEmpty, !cid.isEmpty,
              let toolName = data.toolName, !toolName.isEmpty else {
            return
        }
        var session = sessionsByID[sid]
            ?? LiveSession(id: sid, agentID: data.agentID ?? "")
        if session.agentID.isEmpty, let aid = data.agentID, !aid.isEmpty {
            session.agentID = aid
        }
        let call = LiveToolCall(
            id: cid,
            toolName: toolName,
            serverName: data.serverName?.nilIfEmpty,
            startedAt: data.startedAt,
            inFlight: true,
            source: "tool"
        )
        session.recentCalls.insert(call, at: 0)
        if session.recentCalls.count > liveSessionsRecentCallsLimit {
            session.recentCalls.removeLast(session.recentCalls.count - liveSessionsRecentCallsLimit)
        }
        session.lastActivity = Date()
        sessionsByID[sid] = session
    }

    private func applyToolCallEnd(_ data: LiveSessionEventData) {
        guard let sid = data.sessionID, !sid.isEmpty else { return }
        guard var session = sessionsByID[sid] else { return }

        if let cid = data.callID,
           let idx = session.recentCalls.firstIndex(where: { $0.id == cid }) {
            var call = session.recentCalls[idx]
            call.inFlight = false
            call.durationMs = data.durationMs
            call.exitCode = data.exitCode
            call.resultSummary = data.resultSummary
            call.error = data.error
            call.status = data.status
            call.endedAt = data.endedAt
            session.recentCalls[idx] = call
        } else {
            // No matching start — codex.turn coarse case. Synthesize a
            // closed entry so operators see the activity.
            let synthetic = LiveToolCall(
                id: data.callID ?? "synthetic-\(UUID().uuidString)",
                toolName: data.toolName ?? "unknown",
                durationMs: data.durationMs,
                exitCode: data.exitCode,
                resultSummary: data.resultSummary,
                error: data.error,
                status: data.status,
                endedAt: data.endedAt,
                inFlight: false,
                source: "tool"
            )
            session.recentCalls.insert(synthetic, at: 0)
            if session.recentCalls.count > liveSessionsRecentCallsLimit {
                session.recentCalls.removeLast(session.recentCalls.count - liveSessionsRecentCallsLimit)
            }
        }
        session.lastActivity = Date()
        sessionsByID[sid] = session
    }

    // MARK: - Plumbing

    private func loadFlatSnapshot(
        apiClient: any LoomAPIClientProtocol,
        preferredError: LoomAPIError,
        reconcileMissing: Bool,
        refreshActivity: Bool
    ) async {
        do {
            let response: SessionsResponse = try await apiClient.request(.sessions(status: "active"))
            await mergeSnapshot(
                response.sessions.filter { $0.status == .active },
                reconcileMissing: reconcileMissing,
                refreshActivity: refreshActivity
            )
            snapshotError = nil
            hasLoadedSnapshot = true
        } catch {
            snapshotError = preferredError
        }
    }

    private func mergeSnapshot(
        _ sessions: [SessionInfo],
        reconcileMissing: Bool,
        refreshActivity: Bool
    ) async {
        let activeIDs = Set(sessions.map(\.id))
        if reconcileMissing {
            let now = Date()
            for (sessionID, session) in sessionsByID where !activeIDs.contains(sessionID) && session.endedAt == nil {
                var ended = session
                ended.endedAt = now
                ended.lastActivity = now
                sessionsByID[sessionID] = ended
            }
        }

        for info in sessions {
            let started = LoomFormat.date(fromISO: info.startedAt) ?? Date()
            var live = sessionsByID[info.id] ?? LiveSession(
                id: info.id,
                agentID: info.agentId,
                agentStatus: .active,
                firstSeen: started,
                lastActivity: started
            )
            if live.agentID.isEmpty { live.agentID = info.agentId }
            if live.agentStatus == .unknown { live.agentStatus = .active }
            live.namespace = info.namespace
            live.description = info.description
            live.entryCount = info.entryCount
            live.endedAt = nil
            sessionsByID[info.id] = live
        }

        if refreshActivity {
            for info in sessions {
                await refreshSessionActivity(sessionID: info.id)
            }
        }
    }

    private func refreshSessionActivity(sessionID: String) async {
        guard let apiClient, sessionsByID[sessionID] != nil else { return }
        guard activityRefreshes.insert(sessionID).inserted else { return }
        defer { activityRefreshes.remove(sessionID) }

        do {
            let response: SessionEventsResponse = try await apiClient.request(
                .sessionEvents(id: sessionID, limit: liveSessionsRecentCallsLimit)
            )
            guard var session = sessionsByID[sessionID] else { return }
            session.recentCalls = Self.mergeCalls(
                existing: session.recentCalls,
                incoming: response.events.map { Self.call(from: $0, sessionID: sessionID) }
            )
            if let latest = session.recentCalls.compactMap(Self.callDate).max() {
                session.lastActivity = max(session.lastActivity, latest)
            }
            sessionsByID[sessionID] = session
        } catch {
            // Activity backfill is best-effort; the live event stream remains usable.
        }
    }

    private static func flatten(_ tree: SessionTreeResponse) -> [SessionInfo] {
        func append(_ node: SessionTreeNode, to sessions: inout [SessionInfo]) {
            sessions.append(node.session)
            for child in node.children { append(child, to: &sessions) }
        }

        var sessions: [SessionInfo] = []
        for root in tree.roots { append(root, to: &sessions) }
        for orphan in tree.orphans { append(orphan, to: &sessions) }
        return sessions
    }

    private static func call(from entry: TimelineEntry, sessionID: String) -> LiveToolCall {
        let data = entry.data
        let eventType = data?["entry_type"]?.stringValue ?? entry.eventType
        let summary = data?["title"]?.stringValue
            ?? data?["summary"]?.stringValue
            ?? data?["status"]?.stringValue
            ?? data?["content"]?.stringValue
        return LiveToolCall(
            id: "timeline-\(sessionID)-\(entry.id)",
            toolName: data?["tool_name"]?.stringValue ?? eventType,
            serverName: data?["server_name"]?.stringValue,
            durationMs: data?["duration_ms"]?.intValue,
            exitCode: data?["exit_code"]?.intValue,
            resultSummary: summary.map(Self.truncateSummary),
            error: data?["error"]?.stringValue,
            status: data?["status"]?.stringValue,
            startedAt: entry.timestamp,
            endedAt: entry.timestamp,
            inFlight: false,
            source: Self.isContextEvent(eventType) ? "context" : "event"
        )
    }

    private static func mergeCalls(existing: [LiveToolCall], incoming: [LiveToolCall]) -> [LiveToolCall] {
        var byID: [String: LiveToolCall] = [:]
        for call in existing where byID[call.id] == nil { byID[call.id] = call }
        for call in incoming where byID[call.id] == nil { byID[call.id] = call }
        return byID.values
            .sorted { (callDate($0) ?? .distantPast) > (callDate($1) ?? .distantPast) }
            .prefix(liveSessionsRecentCallsLimit)
            .map { $0 }
    }

    private static func callDate(_ call: LiveToolCall) -> Date? {
        LoomFormat.date(fromISO: call.endedAt ?? call.startedAt ?? "")
    }

    private static func isContextEvent(_ eventType: String) -> Bool {
        ["decision", "finding", "error", "task", "file_read", "note", "code_context", "annotation"]
            .contains(eventType)
    }

    private static func truncateSummary(_ value: String) -> String {
        let oneLine = value.replacingOccurrences(of: "\\s+", with: " ", options: .regularExpression)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        guard oneLine.count > 140 else { return oneLine }
        return String(oneLine.prefix(137)) + "..."
    }

    private func decode(_ event: SSEEvent) -> LiveSessionEventEnvelope? {
        // Reconstruct the envelope from the SSE wire fields.
        // The daemon publishes JSON with `id`, `type`, `timestamp`, `data`
        // — but `SSEClient` parses out the `data:` payload. The HUD's
        // /events stream emits the raw envelope in the SSE `data:` field.
        guard let payload = event.data.data(using: .utf8) else { return nil }
        let decoder = JSONDecoder()
        do {
            return try decoder.decode(LiveSessionEventEnvelope.self, from: payload)
        } catch {
            return nil
        }
    }

    private func startReapTask() {
        reapTask?.cancel()
        reapTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(5))
                await MainActor.run { self?.reapEnded() }
            }
        }
    }

    private func startSnapshotRefreshTask() {
        snapshotRefreshTask?.cancel()
        let interval = snapshotRefreshInterval
        snapshotRefreshTask = Task { [weak self] in
            while !Task.isCancelled {
                do {
                    try await Task.sleep(for: interval)
                } catch {
                    return
                }
                guard let self, !Task.isCancelled else { return }
                await self.loadSessionSnapshot(force: true, refreshActivity: false)
            }
        }
    }

    private func reapEnded() {
        let cutoff = Date().addingTimeInterval(-liveSessionsEndedRetentionSeconds)
        for (sid, s) in sessionsByID {
            if let endedAt = s.endedAt, endedAt < cutoff {
                sessionsByID.removeValue(forKey: sid)
            }
        }
    }
}

private extension String {
    var nilIfEmpty: String? { isEmpty ? nil : self }
}
