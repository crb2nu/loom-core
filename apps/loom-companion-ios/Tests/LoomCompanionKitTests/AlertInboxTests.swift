import Foundation
import Testing
@testable import LoomCompanionKit

/// Coverage for the server-backed alert inbox and the auto-fix decision queue.
///
/// Before this slice the app built its inbox from SSE alone — every alert was
/// lost on relaunch — and the five `/api/mobile/v1` alerting routes
/// (`internal/hud/domain/alerting/alerting.go`) had zero callers.
@Suite("Alert inbox")
struct AlertInboxTests {

    private static let baseURL = URL(string: "http://localhost:3333")!

    // MARK: - Endpoint contract

    @Test("Alerting endpoints use the documented paths and methods")
    func endpointPaths() throws {
        #expect(Endpoint.alerts().path == "/api/mobile/v1/alerts")
        #expect(Endpoint.alertAck(id: "alert-9").path == "/api/mobile/v1/alerts/alert-9/ack")
        #expect(Endpoint.autofixProposals.path == "/api/mobile/v1/autofix/proposals")
        #expect(Endpoint.autofixApprove(id: "p-1").path
                == "/api/mobile/v1/autofix/proposals/p-1/approve")
        #expect(Endpoint.autofixReject(id: "p-1").path
                == "/api/mobile/v1/autofix/proposals/p-1/reject")

        #expect(Endpoint.alerts().method == "GET")
        #expect(Endpoint.autofixProposals.method == "GET")
        #expect(Endpoint.alertAck(id: "a").method == "POST")
        #expect(Endpoint.autofixApprove(id: "p").method == "POST")
        #expect(Endpoint.autofixReject(id: "p").method == "POST")

        #expect(Endpoint.alertAck(id: "a").isMutation)
        #expect(Endpoint.autofixApprove(id: "p").isMutation)
        #expect(Endpoint.autofixReject(id: "p").isMutation)

        // The alert store read must not collide with the policy matrix route.
        #expect(Endpoint.alertsPolicy.path == "/api/mobile/v1/alerts/policy")
    }

    @Test("Only autofix approve is admin-gated")
    func adminGating() throws {
        // handleApproveProposal calls RequireAdminToken; ack and reject do not.
        #expect(Endpoint.autofixApprove(id: "p-1").requiresAdminToken)
        #expect(Endpoint.autofixReject(id: "p-1").requiresAdminToken == false)
        #expect(Endpoint.alertAck(id: "a-1").requiresAdminToken == false)
        #expect(Endpoint.alerts().requiresAdminToken == false)
    }

    @Test("Alert list forwards limit and severity as query items")
    func alertsQuery() throws {
        let plain = try Endpoint.alerts().urlRequest(baseURL: Self.baseURL)
        #expect(plain.url?.query == nil)

        let filtered = try Endpoint.alerts(limit: 25, severity: "critical")
            .urlRequest(baseURL: Self.baseURL)
        let query = try #require(filtered.url?.query)
        #expect(query.contains("limit=25"))
        #expect(query.contains("severity=critical"))
    }

    @Test("Ack body tags the acknowledgement as coming from the phone")
    func ackBody() throws {
        let request = try Endpoint.alertAck(id: "alert-1").urlRequest(baseURL: Self.baseURL)
        let body = try #require(request.httpBody)
        let json = try #require(try JSONSerialization.jsonObject(with: body) as? [String: Any])
        // handleAckAlert defaults a blank acked_by to "hud-user"; send our own
        // so the store shows which surface acked.
        #expect(json["acked_by"] as? String == "ios-companion")

        let explicit = try Endpoint.alertAck(id: "alert-1", ackedBy: " cody ")
            .urlRequest(baseURL: Self.baseURL)
        let explicitBody = try #require(explicit.httpBody)
        let explicitJSON = try #require(
            try JSONSerialization.jsonObject(with: explicitBody) as? [String: Any])
        #expect(explicitJSON["acked_by"] as? String == "cody")
    }

    // MARK: - Decoding (real HUD bytes)

    @Test("Alert list decodes the alerting domain's bare JSON")
    func decodesAlertList() throws {
        // Shape produced by handleListAlerts → writeJSON(map{"alerts": []Alert}).
        // NOTE: bare JSON, NOT the mobile APIEnvelope.
        let json = """
        {"alerts":[
          {"id":"alert-pipeline_failed-1753440000","rule_id":"pipeline_failed",
           "rule_name":"Pipeline failed","severity":"critical",
           "title":"Pipeline failed","message":"loom-core #4211 failed on main",
           "pipeline":{"id":4211,"project":"loom-core","ref":"main","status":"failed",
                       "url":"https://gitlab/loom-core/-/pipelines/4211"},
           "fired_at":"2026-07-25T10:00:00Z"},
          {"id":"alert-stuck-1753439000","rule_id":"stuck","rule_name":"Stuck",
           "severity":"warning","title":"Pipeline stuck","message":"running 90m",
           "pipeline":{"id":4210,"project":"gitops","ref":"main","status":"running"},
           "fired_at":"2026-07-25T09:40:00Z",
           "acked_at":"2026-07-25T09:55:00Z","acked_by":"hud-user"}
        ]}
        """
        let decoded = try Self.rawDecoder.decode(ServerAlertsResponse.self, from: Data(json.utf8))
        #expect(decoded.alerts.count == 2)
        #expect(decoded.alerts[0].severity == "critical")
        #expect(decoded.alerts[0].pipeline.id == 4211)
        #expect(decoded.alerts[0].pipeline.label == "loom-core #4211")
        #expect(decoded.alerts[0].isAcked == false)
        #expect(decoded.alerts[1].isAcked)
        #expect(decoded.alerts[1].ackedBy == "hud-user")
    }

    @Test("Empty alert store (no engine configured) decodes to an empty list")
    func decodesEmptyAlertList() throws {
        let decoded = try Self.rawDecoder.decode(
            ServerAlertsResponse.self, from: Data(#"{"alerts":[]}"#.utf8))
        #expect(decoded.alerts.isEmpty)
    }

    @Test("Proposal list tolerates Go's null estimated_files")
    func decodesProposals() throws {
        // AutoFixProposal.Files has no omitempty, so a nil slice marshals as
        // `null` — decoding it as a non-optional [String] would throw.
        let json = """
        {"proposals":[
          {"id":"proposal-1753440000","diagnosis_id":"loom-core:4211",
           "description":"Bump the flaky test timeout","strategy":"agent_fix",
           "estimated_files":null,"confidence":0.86,"requires_approval":true,
           "created_at":"2026-07-25T10:01:00Z"},
          {"id":"proposal-1753439000","diagnosis_id":"gitops:4210",
           "description":"Runner flake","strategy":"retry",
           "estimated_files":["ci/.gitlab-ci.yml"],"confidence":0.4,
           "requires_approval":false,"created_at":"2026-07-25T09:41:00Z"}
        ]}
        """
        let decoded = try Self.rawDecoder.decode(
            AutofixProposalsResponse.self, from: Data(json.utf8))
        #expect(decoded.proposals.count == 2)
        #expect(decoded.proposals[0].estimatedFiles.isEmpty)
        #expect(decoded.proposals[0].kind == .agentFix)
        #expect(decoded.proposals[0].kind.isNoOp == false)
        #expect(decoded.proposals[1].kind == .retry)
        // The HUD's retry strategy never re-runs anything — the UI must say so.
        #expect(decoded.proposals[1].kind.isNoOp)
        #expect(decoded.proposals[1].kind.approveEffect.contains("placeholder"))
    }

    @Test("Approve response decodes the 202 execution envelope")
    func decodesApproveResponse() throws {
        let json = """
        {"execution":{"id":"exec-1753440100","proposal_id":"proposal-1753440000",
         "status":"running","spawn_id":"sp-9","agent_id":"spawn-sp-9",
         "started_at":"2026-07-25T10:02:00Z"}}
        """
        let decoded = try Self.rawDecoder.decode(
            AutofixApproveResponse.self, from: Data(json.utf8))
        #expect(decoded.execution.status == "running")
        #expect(decoded.execution.spawnId == "sp-9")
        #expect(decoded.execution.completedAt == nil)
    }

    @Test("Reject response decodes the ack shape")
    func decodesRejectResponse() throws {
        let decoded = try Self.rawDecoder.decode(
            AutofixRejectResponse.self,
            from: Data(#"{"rejected":true,"proposal_id":"proposal-1"}"#.utf8))
        #expect(decoded.rejected)
        #expect(decoded.proposalId == "proposal-1")
    }

    // MARK: - ViewModel: load + merge

    @Test("load() seeds the inbox from the server store")
    @MainActor
    func loadsFromServer() async throws {
        let mock = MockAPIClient()
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [
            Self.serverAlert(id: "a-1", severity: "critical"),
            Self.serverAlert(id: "a-2", severity: "info", ackedAt: Date()),
        ])
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [])
        let vm = AlertsViewModel(apiClient: mock)

        await vm.load()

        #expect(vm.alerts.count == 2)
        #expect(vm.hasLoaded)
        #expect(vm.loadError == nil)
        // Read state is derived from the server ack — one read, not two.
        #expect(vm.unreadCount == 1)
        let allServerBacked = vm.alerts.allSatisfy(\.isServerBacked)
        #expect(allServerBacked)
        #expect(mock.lastAlertsQuery?.limit == AlertsViewModel.serverAlertLimit)
    }

    @Test("load() surfaces the error instead of a silently empty inbox")
    @MainActor
    func loadFailureSurfacesError() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/mobile/v1/alerts"] =
            .apiError(code: .forbidden, message: "denied", requestId: "r1")
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [])
        let vm = AlertsViewModel(apiClient: mock)

        await vm.load()

        #expect(vm.alerts.isEmpty)
        #expect(vm.hasLoaded)
        #expect(vm.loadError != nil)
    }

    @Test("A pipeline.alert SSE event merges onto the stored alert, not beside it")
    @MainActor
    func sseDedupesAgainstStore() async throws {
        let mock = MockAPIClient()
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [
            Self.serverAlert(id: "alert-7", severity: "warning"),
        ])
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [])
        let vm = AlertsViewModel(apiClient: mock)
        await vm.load()
        #expect(vm.alerts.count == 1)

        // The dispatcher marshals the alerting.Alert record verbatim.
        vm.handleSSEEvent(SSEEvent(type: "pipeline.alert", data: """
        {"id":"alert-7","rule_id":"stuck","rule_name":"Stuck","severity":"critical",
         "title":"Pipeline stuck","message":"escalated","pipeline":{"id":1,"project":"p"},
         "fired_at":"2026-07-25T10:05:00Z"}
        """))

        #expect(vm.alerts.count == 1)
        #expect(vm.alerts[0].severity == .critical)
        #expect(vm.alerts[0].isServerBacked)
    }

    @Test("Stream-only alerts survive a server reload")
    @MainActor
    func streamAlertsSurviveReload() async throws {
        let mock = MockAPIClient()
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [
            Self.serverAlert(id: "a-1", severity: "info"),
        ])
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [])
        let vm = AlertsViewModel(apiClient: mock)

        vm.handleSSEEvent(SSEEvent(type: "agent.session.reaped", data: """
        {"session_id":"s1"}
        """))
        #expect(vm.alerts.count == 1)

        await vm.load()

        #expect(vm.alerts.count == 2)
        #expect(vm.alerts.contains { !$0.isServerBacked })
    }

    @Test("A server ack made elsewhere wins on reload")
    @MainActor
    func serverAckWinsOnReload() async throws {
        let mock = MockAPIClient()
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [
            Self.serverAlert(id: "a-1", severity: "critical"),
        ])
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [])
        let vm = AlertsViewModel(apiClient: mock)
        await vm.load()
        #expect(vm.unreadCount == 1)

        // Someone acks it in the web HUD.
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [
            Self.serverAlert(id: "a-1", severity: "critical",
                             ackedAt: Date(), ackedBy: "hud-user"),
        ])
        await vm.loadAlerts()

        #expect(vm.unreadCount == 0)
        #expect(vm.alerts[0].ackedBy == "hud-user")
    }

    // MARK: - ViewModel: ack

    @Test("Acking a server alert POSTs and keeps the row read")
    @MainActor
    func acksServerAlert() async throws {
        let mock = MockAPIClient()
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [
            Self.serverAlert(id: "a-1", severity: "critical"),
        ])
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [])
        mock.alertAckResponse = AlertAckResponse(acked: true, id: "a-1")
        let vm = AlertsViewModel(apiClient: mock)
        await vm.load()

        await vm.ack("a-1")

        #expect(mock.lastAlertAck == "a-1")
        #expect(vm.alerts[0].isRead)
        #expect(vm.alerts[0].ackedAt != nil)
        #expect(vm.actionError == nil)
        #expect(vm.ackingAlertIDs.isEmpty)
    }

    @Test("A failed ack rolls the row back to unread")
    @MainActor
    func ackRollsBackOnFailure() async throws {
        let mock = MockAPIClient()
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [
            Self.serverAlert(id: "a-1", severity: "critical"),
        ])
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [])
        let vm = AlertsViewModel(apiClient: mock)
        await vm.load()

        mock.endpointFailures["/api/mobile/v1/alerts/a-1/ack"] =
            .apiError(code: .notFound, message: "alert not found", requestId: "r2")
        await vm.ack("a-1")

        #expect(vm.alerts[0].isRead == false)
        #expect(vm.alerts[0].ackedAt == nil)
        #expect(vm.actionError?.contains("alert not found") == true)
        #expect(vm.unreadCount == 1)
    }

    @Test("Marking a stream alert read stays local (no ack POST)")
    @MainActor
    func streamAlertReadIsLocal() async throws {
        let mock = MockAPIClient()
        let vm = AlertsViewModel(apiClient: mock)
        vm.handleSSEEvent(SSEEvent(type: "agent.session.start", data: """
        {"session_id":"s1","agent_id":"a"}
        """))
        let id = vm.alerts[0].id

        vm.markRead(id)

        #expect(vm.alerts[0].isRead)
        #expect(mock.lastAlertAck == nil)
        #expect(mock.requestCount(for: .alertAck(id: id)) == 0)
    }

    @Test("markAllRead acks every unread server alert")
    @MainActor
    func marksAllRead() async throws {
        let mock = MockAPIClient()
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [
            Self.serverAlert(id: "a-1", severity: "critical"),
            Self.serverAlert(id: "a-2", severity: "warning"),
        ])
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [])
        mock.alertAckResponse = AlertAckResponse(acked: true, id: "a-1")
        let vm = AlertsViewModel(apiClient: mock)
        await vm.load()
        vm.handleSSEEvent(SSEEvent(type: "agent.session.start", data: """
        {"session_id":"s1","agent_id":"a"}
        """))

        await vm.markAllRead()

        #expect(vm.unreadCount == 0)
        // Both server alerts were acked (the stream one needs no round-trip).
        #expect(mock.requestCount(for: .alertAck(id: "a-1")) == 1)
        #expect(mock.requestCount(for: .alertAck(id: "a-2")) == 1)
    }

    // MARK: - ViewModel: auto-fix decisions

    @Test("Approving a proposal reports the execution the HUD started")
    @MainActor
    func approvesProposal() async throws {
        let mock = MockAPIClient()
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [])
        let proposal = AutofixProposal(id: "p-1", strategy: "agent_fix", confidence: 0.9)
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [proposal])
        mock.autofixApproveResponse = AutofixApproveResponse(
            execution: AutofixExecution(
                id: "exec-1", proposalId: "p-1", status: "running", spawnId: "sp-1"))
        let vm = AlertsViewModel(apiClient: mock)
        await vm.load()
        #expect(vm.pendingProposals.count == 1)

        await vm.approveProposal(proposal)

        #expect(mock.lastAutofixDecision?.id == "p-1")
        #expect(mock.lastAutofixDecision?.verb == "approved")
        #expect(vm.actionError == nil)
        #expect(vm.actionMessage?.contains("exec-1") == true)
        // The HUD's proposal list is append-only, so the app tracks the
        // decision locally to stop re-offering it.
        #expect(vm.decidedProposalIDs["p-1"] == "approved")
        #expect(vm.pendingProposals.isEmpty)
        #expect(vm.decidingProposalID == nil)
    }

    @Test("A failed approve leaves the proposal actionable")
    @MainActor
    func approveFailureKeepsProposalPending() async throws {
        let mock = MockAPIClient()
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [])
        let proposal = AutofixProposal(id: "p-1", strategy: "agent_fix")
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [proposal])
        // Approve is the one admin-gated mobile route — a pairing-only device
        // gets a 401 here.
        mock.endpointFailures["/api/mobile/v1/autofix/proposals/p-1/approve"] =
            .apiError(code: .unauthorized, message: "invalid admin token", requestId: "r3")
        let vm = AlertsViewModel(apiClient: mock)
        await vm.load()

        await vm.approveProposal(proposal)

        #expect(vm.actionError?.contains("invalid admin token") == true)
        #expect(vm.decidedProposalIDs["p-1"] == nil)
        #expect(vm.pendingProposals.count == 1)
    }

    @Test("Rejecting a proposal records the decision")
    @MainActor
    func rejectsProposal() async throws {
        let mock = MockAPIClient()
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [])
        let proposal = AutofixProposal(id: "p-2", strategy: "retry")
        mock.autofixProposalsResponse = AutofixProposalsResponse(proposals: [proposal])
        mock.autofixRejectResponse = AutofixRejectResponse(rejected: true, proposalId: "p-2")
        let vm = AlertsViewModel(apiClient: mock)
        await vm.load()

        await vm.rejectProposal(proposal)

        #expect(mock.lastAutofixDecision?.verb == "rejected")
        #expect(vm.decidedProposalIDs["p-2"] == "rejected")
        #expect(vm.actionError == nil)
    }

    @Test("Proposals load independently of alerts")
    @MainActor
    func proposalErrorDoesNotBlankAlerts() async throws {
        let mock = MockAPIClient()
        mock.serverAlertsResponse = ServerAlertsResponse(alerts: [
            Self.serverAlert(id: "a-1", severity: "info"),
        ])
        mock.endpointFailures["/api/mobile/v1/autofix/proposals"] =
            .apiError(code: .upstreamError, message: "engine down", requestId: "r4")
        let vm = AlertsViewModel(apiClient: mock)

        await vm.load()

        #expect(vm.alerts.count == 1)
        #expect(vm.loadError == nil)
        #expect(vm.proposalsError != nil)
    }

    @Test("An unconfigured client marks the inbox loaded without erroring")
    @MainActor
    func noClientDegradesQuietly() async throws {
        let vm = AlertsViewModel()
        await vm.load()
        #expect(vm.hasLoaded)
        #expect(vm.loadError == nil)
        #expect(vm.alerts.isEmpty)
    }

    // MARK: - Helpers

    private static func serverAlert(
        id: String,
        severity: String,
        ackedAt: Date? = nil,
        ackedBy: String? = nil
    ) -> ServerAlert {
        ServerAlert(
            id: id,
            ruleId: "rule",
            ruleName: "Rule",
            severity: severity,
            title: "Title \(id)",
            message: "Message \(id)",
            pipeline: ServerAlertPipeline(id: 1, project: "loom-core", ref: "main", status: "failed"),
            firedAt: Date(timeIntervalSince1970: 1_753_440_000),
            ackedAt: ackedAt,
            ackedBy: ackedBy
        )
    }

    /// Mirrors `APIClient.rawJSONDecoder` (Go RFC3339 dates, bare bodies).
    private static let rawDecoder: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { d in
            let container = try d.singleValueContainer()
            let raw = try container.decode(String.self)
            if let date = LoomFormat.date(fromISO: raw) { return date }
            throw DecodingError.dataCorruptedError(
                in: container, debugDescription: "Expected ISO-8601 date, got \(raw)")
        }
        return decoder
    }()
}

/// The `loom://alert/<id>` deep link, restored now that a single-alert
/// destination exists again (the alert inbox, presented from the Dashboard).
@Suite("Alert deep link")
struct AlertDeepLinkTests {

    @Test("loom://alert/<id> round-trips through parse and build")
    func roundTrips() throws {
        let url = try #require(URL(string: "loom://alert/alert-pipeline_failed-1753440000"))
        let link = try #require(DeepLink.from(url))
        #expect(link == .alert(id: "alert-pipeline_failed-1753440000"))
        #expect(link.urlString == "loom://alert/alert-pipeline_failed-1753440000")
        #expect(link.destinationGroup == .alerts)
        #expect(DeepLink.from(try #require(link.url)) == link)
    }

    @Test("loom://alert with no id is rejected")
    func rejectsMissingID() throws {
        #expect(DeepLink.from(try #require(URL(string: "loom://alert"))) == nil)
        #expect(DeepLink.from(try #require(URL(string: "loom://alert/"))) == nil)
    }
}
