import Foundation

/// ViewModel for the in-app alert inbox and the auto-fix decision queue.
///
/// ## Where alerts come from
///
/// Two sources, merged into one list keyed by `AlertItem.id`:
///
/// 1. **The HUD alert store** (`GET /api/mobile/v1/alerts`) — the source of
///    truth for *history*. It survives app launches (the app used to lose every
///    alert on relaunch because it built the inbox from SSE alone). It is an
///    in-memory ring buffer of the last 200 alerts on the HUD side, so it does
///    reset when the HUD restarts.
/// 2. **The SSE stream** — the source of truth for *liveness*. A
///    `pipeline.alert` event carries the store's own record verbatim, so it
///    merges onto the same id; the other notification-worthy event types
///    (session lifecycle, nudges, handoffs, health) exist only in the stream.
///
/// On a conflict the freshly-loaded server record wins for the fields the
/// server owns (`acked_at`, `acked_by`), because the same alert may have been
/// acked from the web HUD or another device.
///
/// ## Read state vs server ack
///
/// There is exactly ONE read state. `AlertItem.isRead` means "an operator has
/// seen this", and for server-backed alerts that is *defined* as the server's
/// `acked_at` being set:
///
/// - Marking a `.server` alert read POSTs `/alerts/{id}/ack` and stamps
///   `ackedAt` optimistically; a failure rolls the row back to unread so the UI
///   never claims an ack the HUD did not record.
/// - Marking a `.stream` alert read is local-only — those events have no store
///   entry to ack, so there is nothing to round-trip.
/// - A reload re-derives `isRead` for server alerts from `acked_at`, which is
///   why an ack made on the web HUD shows up here too.
///
/// Server-side ack does NOT remove the alert (`AckAlert` stamps the record in
/// place), so "dismiss" stays a local-only list operation and is offered only
/// for stream alerts — dismissing a server alert would just resurrect it on the
/// next load.
@MainActor
@Observable
public final class AlertsViewModel {
    public private(set) var alerts: [AlertItem] = []

    /// Pending auto-fix proposals from `GET /autofix/proposals`.
    public private(set) var proposals: [AutofixProposal] = []

    /// Maximum number of alerts retained; oldest are evicted beyond this limit.
    public static let maxAlerts = 100

    /// Server page size for the store read. The HUD caps its ring buffer at
    /// 200; we ask for our own retention limit.
    public static let serverAlertLimit = 100

    // MARK: - Load / action state

    public private(set) var isLoading = false
    /// True once a load attempt has completed, so views can tell "still
    /// loading" from "genuinely empty".
    public private(set) var hasLoaded = false
    public private(set) var loadError: LoomAPIError?
    public private(set) var proposalsError: LoomAPIError?

    /// Alert ids with an ack request in flight (drives per-row spinners).
    public private(set) var ackingAlertIDs: Set<String> = []
    /// Proposal id with an approve/reject in flight — one decision at a time.
    public private(set) var decidingProposalID: String?
    /// Proposals decided in this session, id → verb. The HUD's proposal list is
    /// append-only (neither approve nor reject removes the record), so without
    /// this the operator would be offered the same decision forever.
    public private(set) var decidedProposalIDs: [String: String] = [:]

    public var actionMessage: String?
    public var actionError: String?

    @ObservationIgnored
    private var apiClient: (any LoomAPIClientProtocol)?

    public init(apiClient: (any LoomAPIClientProtocol)? = nil) {
        self.apiClient = apiClient
    }

    /// Bind (or rebind) the API client. The inbox is created before the app is
    /// authenticated, so the client arrives later.
    public func configure(apiClient: any LoomAPIClientProtocol) {
        self.apiClient = apiClient
    }

    public var unreadCount: Int {
        alerts.filter { !$0.isRead }.count
    }

    public var criticalAlerts: [AlertItem] {
        alerts.filter { $0.severity == .critical && !$0.isRead }
    }

    /// Proposals still awaiting a decision from this device.
    public var pendingProposals: [AutofixProposal] {
        proposals.filter { decidedProposalIDs[$0.id] == nil }
    }

    // MARK: - Server load

    /// Load the alert store and the auto-fix proposal queue.
    ///
    /// Both reads are best-effort and independent: a HUD without an alert
    /// engine returns `{"alerts": []}` (HTTP 200), and a HUD without an
    /// auto-fix engine returns `{"proposals": []}` — neither is an error state.
    public func load() async {
        guard apiClient != nil else {
            hasLoaded = true
            return
        }
        isLoading = true
        // A refresh clears the last action banner — otherwise a stale "ack
        // failed" would outlive the reload that fixed it.
        actionError = nil
        actionMessage = nil
        defer {
            isLoading = false
            hasLoaded = true
        }
        await loadAlerts()
        await loadProposals()
    }

    /// Read the server alert store and merge it into the list.
    public func loadAlerts() async {
        guard let apiClient else { return }
        do {
            // Bare JSON, not the mobile envelope — the alerting domain writes
            // with the HUD's plain writeJSON. See ServerAlert.swift.
            let response: ServerAlertsResponse = try await apiClient.requestRaw(
                .alerts(limit: Self.serverAlertLimit)
            )
            mergeServerAlerts(response.alerts)
            loadError = nil
        } catch {
            loadError = error as? LoomAPIError
                ?? .networkError(underlying: error.localizedDescription)
        }
    }

    /// Read the pending auto-fix proposals.
    public func loadProposals() async {
        guard let apiClient else { return }
        do {
            let response: AutofixProposalsResponse = try await apiClient.requestRaw(
                .autofixProposals
            )
            proposals = response.proposals
            proposalsError = nil
        } catch {
            proposalsError = error as? LoomAPIError
                ?? .networkError(underlying: error.localizedDescription)
        }
    }

    /// Merge a freshly-read page of the store into the list.
    ///
    /// The server record wins for the fields the server owns (`acked_at`,
    /// `acked_by`, and therefore read state) so an ack performed on the web HUD
    /// or another phone lands here. Stream-only alerts are untouched.
    func mergeServerAlerts(_ serverAlerts: [ServerAlert]) {
        var byID: [String: AlertItem] = [:]
        var order: [String] = []

        for alert in serverAlerts {
            let item = AlertItem(serverAlert: alert)
            if byID[item.id] == nil { order.append(item.id) }
            byID[item.id] = item
        }
        for existing in alerts {
            if byID[existing.id] != nil {
                // A server record for this id already replaced it above.
                continue
            }
            byID[existing.id] = existing
            order.append(existing.id)
        }

        alerts = order.compactMap { byID[$0] }
            .sorted { $0.timestamp > $1.timestamp }
        trim()
    }

    // MARK: - SSE

    /// Classify an SSE event via NotificationPolicy and merge it into the list.
    ///
    /// A `pipeline.alert` event decodes to the store's own record, so it
    /// replaces (rather than duplicates) an entry already loaded from
    /// `GET /alerts`.
    public func handleSSEEvent(_ event: SSEEvent) {
        guard let alert = NotificationPolicy.classify(event: event) else { return }
        upsert(alert)
    }

    /// Insert or replace an alert, keeping the list newest-first.
    private func upsert(_ alert: AlertItem) {
        if let index = alerts.firstIndex(where: { $0.id == alert.id }) {
            // Preserve a read/ack state the operator already established
            // locally; the live event does not know about it.
            var merged = alert
            if alerts[index].isRead, !merged.isRead {
                merged.isRead = true
                merged.ackedAt = merged.ackedAt ?? alerts[index].ackedAt
                merged.ackedBy = merged.ackedBy ?? alerts[index].ackedBy
            }
            alerts[index] = merged
            return
        }
        alerts.insert(alert, at: 0)
        trim()
    }

    private func trim() {
        if alerts.count > Self.maxAlerts {
            alerts = Array(alerts.prefix(Self.maxAlerts))
        }
    }

    // MARK: - Read / ack

    /// Mark a single alert as read.
    ///
    /// For a server-backed alert this is the ack: the row flips optimistically
    /// and rolls back if `POST /alerts/{id}/ack` fails.
    public func markRead(_ id: String) {
        guard let index = alerts.firstIndex(where: { $0.id == id }) else { return }
        guard !alerts[index].isRead else { return }
        if alerts[index].isServerBacked {
            Task { await ack(id) }
            return
        }
        alerts[index].isRead = true
    }

    /// Acknowledge a server-backed alert. No-op for stream alerts (there is no
    /// store record to stamp) and for an alert already being acked.
    public func ack(_ id: String) async {
        guard let index = alerts.firstIndex(where: { $0.id == id }) else { return }
        let alert = alerts[index]
        guard alert.isServerBacked else {
            alerts[index].isRead = true
            return
        }
        guard !ackingAlertIDs.contains(id) else { return }
        guard let apiClient else { return }

        // Optimistic flip.
        let previousRead = alert.isRead
        let previousAckedAt = alert.ackedAt
        let previousAckedBy = alert.ackedBy
        alerts[index].isRead = true
        alerts[index].ackedAt = Date()
        alerts[index].ackedBy = "ios-companion"
        ackingAlertIDs.insert(id)
        defer { ackingAlertIDs.remove(id) }

        do {
            let response: AlertAckResponse = try await apiClient.requestRaw(.alertAck(id: id))
            if !response.acked {
                throw LoomAPIError.apiError(
                    code: .unknown, message: "HUD did not confirm the ack", requestId: "")
            }
            actionError = nil
        } catch {
            // Roll back — never leave the row claiming an ack the HUD refused.
            if let current = alerts.firstIndex(where: { $0.id == id }) {
                alerts[current].isRead = previousRead
                alerts[current].ackedAt = previousAckedAt
                alerts[current].ackedBy = previousAckedBy
            }
            let loomError = error as? LoomAPIError
                ?? .networkError(underlying: error.localizedDescription)
            actionError = "Acknowledge failed: \(loomError.description)"
        }
    }

    /// Mark all alerts as read, acking every server-backed one.
    public func markAllRead() async {
        let serverIDs = alerts.filter { $0.isServerBacked && !$0.isRead }.map(\.id)
        for i in alerts.indices where !alerts[i].isServerBacked {
            alerts[i].isRead = true
        }
        for id in serverIDs {
            await ack(id)
        }
    }

    /// Remove a single alert from the local list.
    ///
    /// Only meaningful for stream alerts: the HUD has no delete route, so a
    /// dismissed server alert reappears on the next load. Views gate this to
    /// `!isServerBacked`.
    public func removeAlert(_ id: String) {
        alerts.removeAll { $0.id == id }
    }

    /// Clear the local list. Server-backed alerts return on the next load.
    public func clearAll() {
        alerts.removeAll()
    }

    // MARK: - Auto-fix decisions

    /// Approve a proposal — the HUD executes it immediately
    /// (`ExecuteAutoFix`), so this is not a "queue it" action.
    ///
    /// Admin-gated server-side: `Endpoint.autofixApprove` carries the HUD admin
    /// token, and a pairing-only device gets a 401.
    public func approveProposal(_ proposal: AutofixProposal) async {
        await decide(
            proposal,
            endpoint: .autofixApprove(id: proposal.id),
            verb: "approved",
            actionLabel: "Approve"
        ) { (client: any LoomAPIClientProtocol, endpoint: Endpoint) in
            let response: AutofixApproveResponse = try await client.requestRaw(endpoint)
            let exec = response.execution
            var detail = "execution \(exec.id) · \(exec.status)"
            if let result = exec.result, !result.isEmpty {
                detail += " — \(result)"
            }
            return detail
        }
    }

    /// Reject a proposal. Records a `rejected` execution server-side; the
    /// proposal record itself is not deleted.
    public func rejectProposal(_ proposal: AutofixProposal) async {
        await decide(
            proposal,
            endpoint: .autofixReject(id: proposal.id),
            verb: "rejected",
            actionLabel: "Reject"
        ) { (client: any LoomAPIClientProtocol, endpoint: Endpoint) in
            let response: AutofixRejectResponse = try await client.requestRaw(endpoint)
            return response.rejected ? "recorded" : "not recorded by the HUD"
        }
    }

    private func decide(
        _ proposal: AutofixProposal,
        endpoint: Endpoint,
        verb: String,
        actionLabel: String,
        perform: (any LoomAPIClientProtocol, Endpoint) async throws -> String
    ) async {
        guard decidingProposalID == nil else { return }
        guard let apiClient else { return }
        decidingProposalID = proposal.id
        actionError = nil
        actionMessage = nil
        defer { decidingProposalID = nil }

        do {
            let detail = try await perform(apiClient, endpoint)
            // Only mark decided on success — a failed approve must stay
            // actionable.
            decidedProposalIDs[proposal.id] = verb
            actionMessage = "Proposal \(verb) — \(detail)."
        } catch {
            let loomError = error as? LoomAPIError
                ?? .networkError(underlying: error.localizedDescription)
            actionError = "\(actionLabel) failed: \(loomError.description)"
        }
    }
}
