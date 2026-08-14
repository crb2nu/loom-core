import Foundation
@testable import LoomCompanionKit

/// Mock API client for ViewModel tests.
final class MockAPIClient: LoomAPIClientProtocol, @unchecked Sendable {
    var shouldFail = false
    var failError: LoomAPIError = .apiError(code: .unauthorized, message: "mock error", requestId: "mock")
    var endpointFailures: [String: LoomAPIError] = [:]
    var requestDelayNanoseconds: UInt64 = 0

    private let requestCountLock = NSLock()
    private var requestCounts: [String: Int] = [:]

    var dashboardResponse: DashboardData?
    var sessionsResponse: SessionsResponse?
    var sessionsTreeResponse: SessionTreeResponse?
    var sessionDetailResponse: SessionDetailResponse?
    var sessionEventsResponse: SessionEventsResponse?
    var sessionActivityResponse: SessionActivityResponse?
    var createSessionResponse: SessionCreateResponse?
    var endSessionResponse: SessionEndResponse?
    var tasksResponse: MobileTasksResponse?
    var workflowsResponse: MobileWorkflowsResponse?
    var workflowDetailResponse: MobileWorkflowDetailResponse?
    var presenceResponse: MobilePresenceResponse?
    var pipelinesResponse: MobilePipelinesResponse?
    var memoryStatsResponse: MobileMemoryStatsResponse?
    var memoryItemsResponse: MobileMemoryItemsResponse?
    var streamResponse: MobileStreamResponse?
    var topologyResponse: MobileTopologyResponse?
    var graphStatsResponse: MobileGraphStatsResponse?
    var graphEntitiesResponse: MobileGraphEntitiesResponse?
    var graphPathResponse: MobileGraphPathResponse?
    var reasoningChainsResponse: MobileReasoningChainsResponse?
    var reasoningChainDetailResponse: MobileReasoningChainDetailResponse?
    var controlPlaneResponse: MobileControlPlaneResponse?
    var alertPolicyResponse: MobileAlertPolicyResponse?
    var pushRegistrationResponse: PushRegistrationResponse?
    var pushUnregisterResponse: PushUnregisterResponse?
    var sandboxResponse: MobileSandboxSummary?
    var sandboxStartResponse: MobileSandboxStartResponse?
    var sandboxStopResponse: MobileSandboxStopResponse?
    var spawnTelemetryResponse: SpawnTelemetryResponse?
    var spawnTelemetryToolsResponse: SpawnTelemetryToolsPage?
    var spawnTelemetryFilesResponse: SpawnTelemetryFilesPage?
    var spawnTelemetryErrorsResponse: SpawnTelemetryErrorsPage?
    var spawnControlAckResponse: SpawnControlAck?
    var spawnStopAckResponse: SpawnStopAck?
    var spawnConfigResponse: SpawnConfig?
    var agentsResponse: UnifiedAgentsResponse?
    /// (spawn id, text) captured from the last spawn message request.
    var lastSpawnMessage: (id: String, text: String)?
    /// Spawn id captured from the last interrupt request.
    var lastSpawnInterrupt: String?
    /// Spawn id captured from the last stop request.
    var lastSpawnStop: String?
    /// (session id, summarize) captured from the last end-session request.
    var lastEndSession: (id: String, summarize: Bool?)?
    /// (workflow id, step id) captured from the last workflow approve/reject.
    var lastWorkflowDecision: (id: String, stepId: String, verb: String)?
    var millsPipelineRunsResponse: [MillsPipelineRun]?
    var millsKPIResponse: MillsKPISnapshot?
    var millsPipelineRunDetailResponse: MillsPipelineRunDetail?
    var millsBacklogResponse: [MillsBacklogItem]?
    var patternsCatalogResponse: MillsPatternCatalog?
    var millsSpinningRoomResponse: MillsSpinningRoom?
    var millsSpinRunsResponse: [MillsSpinRun]?
    var millsSpinRunResponse: MillsSpinRun?
    var millsSpinQueuedResponse: MillsSpinQueued?
    var plansResponse: MillsPlanList?
    var planDetailResponse: MillsPlanDetail?
    var planAdvanceResponse: MillsPlanAdvanceAck?
    var millsPipelineEscalateResponse: MillsPipelineEscalateAck?
    /// Body captured from the last millsSpinAsync request, for assertions.
    var lastSpinRequest: MillsSpinRequest?
    /// Status query captured from the last sessions/sessionsTree request.
    var lastSessionsStatus: String?
    var lastSessionsTreeStatus: String?
    /// The (id, reason) captured from the last escalate request.
    var lastEscalate: (id: String, reason: String?)?
    var weaverStatusResponse: WeaverStatus?
    var weaverHistoryResponse: WeaverHistoryResponse?
    var weaverMetricsResponse: WeaverMetrics?
    var aimodelsRolesResponse: AIModelRolesResponse?
    var recoveryAckResponse: RecoveryTelemetryAck?
    var handoffsResponse: MobileHandoffsResponse?
    var handoffActionResponse: MobileHandoffActionResponse?
    var workflowMutationResponse: MobileWorkflowMutationResponse?
    /// (handoff id, target agent id) captured from the last accept request.
    var lastHandoffAccept: (id: String, targetAgentId: String?)?
    /// (handoff id, reason) captured from the last reject request.
    var lastHandoffReject: (id: String, reason: String?)?
    var serverAlertsResponse: ServerAlertsResponse?
    var alertAckResponse: AlertAckResponse?
    var autofixProposalsResponse: AutofixProposalsResponse?
    var autofixApproveResponse: AutofixApproveResponse?
    var autofixRejectResponse: AutofixRejectResponse?
    /// (limit, severity) captured from the last alert-store read.
    var lastAlertsQuery: (limit: Int?, severity: String?)?
    /// Alert id captured from the last ack request.
    var lastAlertAck: String?
    /// (proposal id, verb) captured from the last auto-fix decision.
    var lastAutofixDecision: (id: String, verb: String)?
    var vendorSessionsResponse: VendorSessionListResponse?
    var vendorSessionSearchResponse: VendorSessionSearchResponse?
    /// (cwdContains, limit) captured from the last transcript list read.
    var lastVendorSessionsQuery: (cwdContains: String?, limit: Int?)?
    /// (query, cwdContains, maxResults) captured from the last transcript search.
    var lastVendorSearch: (query: String, cwdContains: String?, maxResults: Int?)?

    func requestCount(for endpoint: Endpoint) -> Int {
        requestCountLock.withLock { requestCounts[endpoint.path, default: 0] }
    }

    func request<T: Decodable>(_ endpoint: Endpoint) async throws -> T {
        requestCountLock.withLock {
            requestCounts[endpoint.path, default: 0] += 1
        }
        if requestDelayNanoseconds > 0 {
            try await Task.sleep(nanoseconds: requestDelayNanoseconds)
        }

        if let specificError = endpointFailures[endpoint.path] {
            throw specificError
        }
        if shouldFail {
            throw failError
        }

        switch endpoint {
        case .dashboard:
            if let r = dashboardResponse as? T { return r }
        case let .sessions(status):
            lastSessionsStatus = status
            if let r = sessionsResponse as? T { return r }
        case let .sessionsTree(status):
            lastSessionsTreeStatus = status
            if let r = sessionsTreeResponse as? T { return r }
        case .controlPlane:
            if let r = controlPlaneResponse as? T { return r }
        case .alertsPolicy:
            if let r = alertPolicyResponse as? T { return r }
        case .sessionDetail:
            if let r = sessionDetailResponse as? T { return r }
        case .sessionEvents:
            if let r = sessionEventsResponse as? T { return r }
        case .sessionActivity:
            if let r = sessionActivityResponse as? T { return r }
        case .tasks:
            if let r = tasksResponse as? T { return r }
        case .workflows:
            if let r = workflowsResponse as? T { return r }
        case .workflowDetail:
            if let r = workflowDetailResponse as? T { return r }
        case .presence:
            if let r = presenceResponse as? T { return r }
        case .pipelines:
            if let r = pipelinesResponse as? T { return r }
        case .memoryStats:
            if let r = memoryStatsResponse as? T { return r }
        case .memoryItems:
            if let r = memoryItemsResponse as? T { return r }
        case .stream:
            if let r = streamResponse as? T { return r }
        case .topology:
            if let r = topologyResponse as? T { return r }
        case .graphStats:
            if let r = graphStatsResponse as? T { return r }
        case .graphEntities:
            if let r = graphEntitiesResponse as? T { return r }
        case .graphPath:
            if let r = graphPathResponse as? T { return r }
        case .reasoningChains:
            if let r = reasoningChainsResponse as? T { return r }
        case .reasoningChainDetail:
            if let r = reasoningChainDetailResponse as? T { return r }
        case .createSession:
            if let r = createSessionResponse as? T { return r }
        case let .endSession(id, summarize):
            lastEndSession = (id, summarize)
            if let r = endSessionResponse as? T { return r }
        case .pushRegister:
            if let r = pushRegistrationResponse as? T { return r }
        case .pushUnregister:
            if let r = pushUnregisterResponse as? T { return r }
        case .sandbox:
            if let r = sandboxResponse as? T { return r }
        case .sandboxStart:
            if let r = sandboxStartResponse as? T { return r }
        case .sandboxStop:
            if let r = sandboxStopResponse as? T { return r }
        case .spawnTelemetry:
            if let r = spawnTelemetryResponse as? T { return r }
        case .spawnTelemetryTools:
            if let r = spawnTelemetryToolsResponse as? T { return r }
        case .spawnTelemetryFiles:
            if let r = spawnTelemetryFilesResponse as? T { return r }
        case .spawnTelemetryErrors:
            if let r = spawnTelemetryErrorsResponse as? T { return r }
        case let .spawnSendMessage(id, text):
            lastSpawnMessage = (id, text)
            if let r = spawnControlAckResponse as? T { return r }
        case let .spawnInterrupt(id):
            lastSpawnInterrupt = id
            if let r = spawnControlAckResponse as? T { return r }
        case let .spawnStop(id):
            lastSpawnStop = id
            if let r = spawnStopAckResponse as? T { return r }
        case .spawnConfig:
            if let r = spawnConfigResponse as? T { return r }
        case .agents:
            if let r = agentsResponse as? T { return r }
        case .spawnAgent, .spawnList, .spawnDetail, .namespaces:
            break
        case let .workflowApprove(id, stepId):
            lastWorkflowDecision = (id, stepId, "approved")
            if let r = workflowMutationResponse as? T { return r }
        case let .workflowReject(id, stepId, _):
            lastWorkflowDecision = (id, stepId, "rejected")
            if let r = workflowMutationResponse as? T { return r }
        case .handoffs:
            if let r = handoffsResponse as? T { return r }
        case let .handoffAccept(id, _, targetAgentId, _):
            lastHandoffAccept = (id, targetAgentId)
            if let r = handoffActionResponse as? T { return r }
        case let .handoffReject(id, reason):
            lastHandoffReject = (id, reason)
            if let r = handoffActionResponse as? T { return r }
        case .audit, .ping, .eventsStream:
            break
        case .millsPipelineRuns:
            if let r = millsPipelineRunsResponse as? T { return r }
        case .millsKPIs:
            if let r = millsKPIResponse as? T { return r }
        case .millsPipelineRunDetail:
            if let r = millsPipelineRunDetailResponse as? T { return r }
        case .millsBacklog:
            if let r = millsBacklogResponse as? T { return r }
        case .patternsCatalog:
            if let r = patternsCatalogResponse as? T { return r }
        case .millsSpinningRoomFrames:
            if let r = millsSpinningRoomResponse as? T { return r }
        case .millsSpinRuns:
            if let r = millsSpinRunsResponse as? T { return r }
        case .millsSpinRun:
            if let r = millsSpinRunResponse as? T { return r }
        case let .millsSpinAsync(request):
            lastSpinRequest = request
            if let r = millsSpinQueuedResponse as? T { return r }
        case .plans:
            if let r = plansResponse as? T { return r }
        case .planDetail:
            if let r = planDetailResponse as? T { return r }
        case .planAdvance:
            if let r = planAdvanceResponse as? T { return r }
        case let .millsPipelineEscalate(id, reason):
            lastEscalate = (id, reason)
            if let r = millsPipelineEscalateResponse as? T { return r }
        case .weaverStatus:
            if let r = weaverStatusResponse as? T { return r }
        case .weaverHistory:
            if let r = weaverHistoryResponse as? T { return r }
        case .weaverMetrics:
            if let r = weaverMetricsResponse as? T { return r }
        case .aimodelsRoles:
            if let r = aimodelsRolesResponse as? T { return r }
        case .recoveryTelemetryUpload:
            if let r = recoveryAckResponse as? T { return r }
        case let .alerts(limit, severity):
            lastAlertsQuery = (limit, severity)
            if let r = serverAlertsResponse as? T { return r }
        case let .alertAck(id, _):
            lastAlertAck = id
            if let r = alertAckResponse as? T { return r }
        case .autofixProposals:
            if let r = autofixProposalsResponse as? T { return r }
        case let .autofixApprove(id):
            lastAutofixDecision = (id, "approved")
            if let r = autofixApproveResponse as? T { return r }
        case let .autofixReject(id):
            lastAutofixDecision = (id, "rejected")
            if let r = autofixRejectResponse as? T { return r }
        case let .vendorSessions(cwdContains, limit):
            lastVendorSessionsQuery = (cwdContains, limit)
            if let r = vendorSessionsResponse as? T { return r }
        case let .vendorSessionSearch(query, cwdContains, maxResults):
            lastVendorSearch = (query, cwdContains, maxResults)
            if let r = vendorSessionSearchResponse as? T { return r }
        }

        throw LoomAPIError.noToken
    }
}
