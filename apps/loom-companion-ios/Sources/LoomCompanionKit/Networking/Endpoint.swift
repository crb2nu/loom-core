import Foundation

/// All mobile v1 API routes.
public enum Endpoint: Sendable {
    case ping
    case dashboard
    case controlPlane
    case alertsPolicy
    case sessions(status: String? = nil)
    case sessionsTree(status: String? = nil)
    case sessionDetail(id: String)
    case sessionEvents(id: String, limit: Int? = nil)
    case sessionActivity(id: String)
    case tasks(status: MobileTaskStatus? = nil, agentId: String? = nil, sessionId: String? = nil, limit: Int? = nil, search: String? = nil)
    case workflows(status: MobileWorkflowStatus? = nil, agentId: String? = nil, limit: Int? = nil)
    case workflowDetail(id: String)
    case presence(status: MobilePresenceStatus? = nil, agentId: String? = nil, limit: Int? = nil)
    case memoryStats
    case memoryItems(tier: MobileMemoryTier = .working, query: String? = nil, limit: Int? = nil)
    case stream(types: [String]? = nil, agentId: String? = nil, sessionId: String? = nil, limit: Int? = nil)
    case topology
    case graphStats
    case graphEntities(type: String? = nil, query: String? = nil, limit: Int? = nil)
    case graphPath(sourceId: String, targetId: String, maxDepth: Int? = nil)
    case reasoningChains(status: MobileReasoningStatus? = nil, limit: Int? = nil)
    case reasoningChainDetail(id: String)
    case createSession(agentId: String, namespace: String? = nil, description: String? = nil, autoRecall: Bool? = nil)
    case endSession(id: String, summarize: Bool? = nil)
    case pushRegister(token: String, platform: PushPlatform)
    case pushUnregister(token: String)
    case eventsStream
    case audit(source: String? = nil, limit: Int? = nil)
    case sandbox
    case sandboxStart(project: String, agentId: String? = nil)
    case sandboxStop(project: String)
    case spawnAgent(request: MobileSpawnRequest)
    case spawnList
    case spawnConfig
    case spawnDetail(id: String)
    case spawnStop(id: String)
    case spawnTelemetry(id: String)
    case spawnTelemetryTools(id: String, offset: Int? = nil, limit: Int? = nil)
    case spawnTelemetryFiles(id: String, offset: Int? = nil, limit: Int? = nil)
    case spawnTelemetryErrors(id: String, offset: Int? = nil, limit: Int? = nil)
    case spawnSendMessage(id: String, text: String)
    case spawnInterrupt(id: String)
    case agents(status: MobilePresenceStatus? = nil, type: String? = nil, limit: Int? = nil)
    case pipelines
    case workflowApprove(id: String, stepId: String)
    case workflowReject(id: String, stepId: String, reason: String? = nil)
    case handoffs(limit: Int? = nil)
    // Handoff inbox mutations. The backend resolves an accept from either an
    // explicit `session_id` or a `target_agent_id` (whose active session it
    // looks up), so the app sends the handoff's own target agent and lets the
    // HUD resolve it. Reject carries an optional free-text reason that is
    // forwarded to the source agent.
    // See internal/hud/domain/mobile/handler_ops.go (handleMobileHandoffAccept
    // / handleMobileHandoffReject).
    case handoffAccept(id: String, sessionId: String? = nil, targetAgentId: String? = nil, importEntries: Bool = false)
    case handoffReject(id: String, reason: String? = nil)
    case namespaces

    // Server-side alert store + auto-fix engine. These five routes are
    // registered by the *alerting* domain
    // (`internal/hud/domain/alerting/alerting.go`), not the `mobile` domain,
    // so they respond with BARE JSON rather than the mobile `APIEnvelope` —
    // callers must use `requestRaw`. `/api/mobile/v1/` is on the mobile
    // token's allowlist, so the pairing bearer reaches all five.
    //
    // `alerts` is the durable-ish inbox history (an in-memory ring buffer of
    // the last 200 alerts, reset on HUD restart); SSE only carries alerts
    // fired while the app is connected.
    case alerts(limit: Int? = nil, severity: String? = nil)
    /// Stamps `acked_at`/`acked_by` on the stored alert. It does NOT remove the
    /// alert — ack is the server-side "an operator has seen this" marker.
    case alertAck(id: String, ackedBy: String? = nil)
    case autofixProposals
    /// Admin-gated (`RequireAdminToken`), unlike every other mobile route —
    /// approving executes the proposal (spawning an agent for `agent_fix`).
    case autofixApprove(id: String)
    /// NOT admin-gated server-side; records a `rejected` execution.
    case autofixReject(id: String)

    // MBL-5 slice 2 — recovery-SLO telemetry uploader. POSTs a device's rolling
    // disconnect-to-recovered window to the slice-1 ingest endpoint. Scope-gated
    // server-side by `mobile:telemetry` (off by default); the uploader degrades
    // gracefully when that scope is not granted. Keyed by the X-Device-ID header
    // that `APIClient` already attaches to every request.
    case recoveryTelemetryUpload(samples: [Double], sloTargetSeconds: Double)

    // Phase 7 slice 7.5 — Mills screen reads. These hit the HUD's
    // /api/mills/* proxy directly (different prefix from /api/mobile/v1/*).
    // Both are read-only and tolerate the operator-not-configured 503 the
    // proxy returns when LOOM_MILLS_OPERATOR_URL is unset.
    // `state`/`limit` were added for the shift report's archive read
    // (?state=terminal&limit=500); the plain `.millsPipelineRuns()` call
    // keeps the original unfiltered semantics.
    case millsPipelineRuns(state: String? = nil, limit: Int? = nil)
    case millsKPIs(window: String)

    // Shift report reads (port of the web Factory panel's overlay): one
    // run's detail (failing gate names for sparks), the backlog list
    // (run → backlog → PlanID pattern attribution), and the Pattern Loom
    // catalog. The first two ride the mills proxy; /api/patterns is
    // HUD-served but equally bare JSON.
    case millsPipelineRunDetail(id: String)
    case millsBacklog
    case patternsCatalog(status: String? = nil)

    // Spinning Room (Live Beam slice 3 + async spins, plan .loom/166). Reads
    // are open; the async spin POST is double-gated behind the HUD admin
    // token — the paired bearer must BE that token or the HUD replies 401.
    case millsSpinningRoomFrames
    case millsSpinRuns(limit: Int? = nil)
    case millsSpinRun(id: String)
    case millsSpinAsync(request: MillsSpinRequest)

    // Plan Store board (HUD plans domain, /api/plans). Bare JSON like the
    // mills proxy; the daemon-predates-plan-store case arrives as
    // `{"available": false}`, not an HTTP error.
    case plans
    case planDetail(id: String)
    case planAdvance(id: String, toPhase: String, note: String? = nil)

    // Pipeline oversight — escalate a running pipeline out of the autonomous
    // loop and hold it for human review. Admin-gated behind the HUD admin
    // token like the other Mills mutations. NOTE: the operator's pause/resume
    // are still 501 stubs (the 4.x runner), so escalate is the only real
    // per-run intervention the widget/app can perform today.
    case millsPipelineEscalate(id: String, reason: String? = nil)

    // Phase 7 / weaver-qwen3 S7b — Weaver screen reads. Same proxy
    // pattern as Mills: HUD's /api/weaver/* (status/history/metrics)
    // and /api/aimodels/roles. Read-only; the daemon-without-weaver
    // case returns `{"enabled": false}`, not 404 or 503.
    case weaverStatus
    case weaverHistory
    case weaverMetrics
    case aimodelsRoles

    // Vendor session bridge (!1251) — list/search the claude + codex desktop
    // CLI transcripts on the HUD's workstation, read through the agent-context
    // bridge. HUD-served BARE JSON (internal/hud/domain/vendorsessions), not
    // the mobile envelope, so callers use `requestRaw`. Both routes are on the
    // mobile pairing-token allowlist (exact `/api/vendor-sessions` + prefix
    // `/api/vendor-sessions/` in internal/hud/api_mobile.go). A healthy HUD
    // whose bridge is down still answers 200 with `degraded:true` — that means
    // "bridge offline", never "no sessions".
    case vendorSessions(cwdContains: String? = nil, limit: Int? = nil)
    case vendorSessionSearch(query: String, cwdContains: String? = nil, maxResults: Int? = nil)

    var method: String {
        switch self {
        case .ping, .dashboard, .controlPlane, .alertsPolicy, .sessions, .sessionsTree, .sessionDetail, .sessionEvents, .sessionActivity,
             .tasks, .workflows, .workflowDetail, .presence, .memoryStats,
             .memoryItems, .stream, .topology, .graphStats, .graphEntities,
             .graphPath, .reasoningChains, .reasoningChainDetail,
             .eventsStream, .audit, .sandbox, .spawnList, .spawnConfig, .spawnDetail, .agents,
             .pipelines, .handoffs, .namespaces,
             .spawnTelemetry, .spawnTelemetryTools, .spawnTelemetryFiles, .spawnTelemetryErrors,
             .millsPipelineRuns, .millsKPIs,
             .millsPipelineRunDetail, .millsBacklog, .patternsCatalog,
             .millsSpinningRoomFrames, .millsSpinRuns, .millsSpinRun,
             .plans, .planDetail,
             .alerts, .autofixProposals,
             .weaverStatus, .weaverHistory, .weaverMetrics, .aimodelsRoles,
             .vendorSessions, .vendorSessionSearch:
            return "GET"
        case .createSession, .endSession, .pushRegister, .pushUnregister,
             .sandboxStart, .sandboxStop, .spawnAgent, .spawnStop,
             .workflowApprove, .workflowReject,
             .handoffAccept, .handoffReject,
             .spawnSendMessage, .spawnInterrupt,
             .millsSpinAsync, .planAdvance, .millsPipelineEscalate,
             .alertAck, .autofixApprove, .autofixReject,
             .recoveryTelemetryUpload:
            return "POST"
        }
    }

    var path: String {
        switch self {
        case .ping:
            return "/api/mobile/v1/ping"
        case .dashboard:
            return "/api/mobile/v1/dashboard"
        case .controlPlane:
            return "/api/mobile/v1/control-plane"
        case .alertsPolicy:
            return "/api/mobile/v1/alerts/policy"
        case .sessions:
            return "/api/mobile/v1/sessions"
        case .sessionsTree:
            return "/api/mobile/v1/sessions/tree"
        case let .sessionDetail(id):
            return "/api/mobile/v1/sessions/\(id)"
        case let .sessionEvents(id, _):
            return "/api/mobile/v1/sessions/\(id)/events"
        case let .sessionActivity(id):
            return "/api/mobile/v1/sessions/\(id)/activity"
        case .tasks:
            return "/api/mobile/v1/tasks"
        case .workflows:
            return "/api/mobile/v1/workflows"
        case let .workflowDetail(id):
            return "/api/mobile/v1/workflows/\(id)"
        case .presence:
            return "/api/mobile/v1/presence"
        case .memoryStats:
            return "/api/mobile/v1/memory/stats"
        case .memoryItems:
            return "/api/mobile/v1/memory/items"
        case .stream:
            return "/api/mobile/v1/stream"
        case .topology:
            return "/api/mobile/v1/topology"
        case .graphStats:
            return "/api/mobile/v1/graph/stats"
        case .graphEntities:
            return "/api/mobile/v1/graph/entities"
        case .graphPath:
            return "/api/mobile/v1/graph/path"
        case .reasoningChains:
            return "/api/mobile/v1/reasoning/chains"
        case let .reasoningChainDetail(id):
            return "/api/mobile/v1/reasoning/chains/\(id)"
        case .createSession:
            return "/api/mobile/v1/sessions"
        case let .endSession(id, _):
            return "/api/mobile/v1/sessions/\(id)/end"
        case .pushRegister:
            return "/api/mobile/v1/push/register"
        case .pushUnregister:
            return "/api/mobile/v1/push/unregister"
        case .eventsStream:
            return "/api/mobile/v1/events/stream"
        case .audit:
            return "/api/mobile/v1/audit"
        case .sandbox:
            return "/api/mobile/v1/sandbox"
        case .sandboxStart:
            return "/api/mobile/v1/sandbox/start"
        case .sandboxStop:
            return "/api/mobile/v1/sandbox/stop"
        case .spawnAgent:
            return "/api/mobile/v1/agent/spawn"
        case .spawnList:
            return "/api/mobile/v1/agent/spawns"
        case .spawnConfig:
            return "/api/mobile/v1/agent/spawn/config"
        case let .spawnDetail(id):
            return "/api/mobile/v1/agent/spawn/\(id)"
        case let .spawnStop(id):
            return "/api/mobile/v1/agent/spawn/\(id)/stop"
        case let .spawnTelemetry(id):
            return "/api/mobile/v1/agent/spawn/\(id)/telemetry"
        case let .spawnTelemetryTools(id, _, _):
            return "/api/mobile/v1/agent/spawn/\(id)/telemetry/tools"
        case let .spawnTelemetryFiles(id, _, _):
            return "/api/mobile/v1/agent/spawn/\(id)/telemetry/files"
        case let .spawnTelemetryErrors(id, _, _):
            return "/api/mobile/v1/agent/spawn/\(id)/telemetry/errors"
        case let .spawnSendMessage(id, _):
            return "/api/mobile/v1/agent/spawn/\(id)/message"
        case let .spawnInterrupt(id):
            return "/api/mobile/v1/agent/spawn/\(id)/interrupt"
        case .agents:
            return "/api/mobile/v1/agents"
        case .pipelines:
            return "/api/mobile/v1/pipelines"
        case let .workflowApprove(id, _):
            return "/api/mobile/v1/workflows/\(id)/approve"
        case let .workflowReject(id, _, _):
            return "/api/mobile/v1/workflows/\(id)/reject"
        case .handoffs:
            return "/api/mobile/v1/handoffs"
        case let .handoffAccept(id, _, _, _):
            return "/api/mobile/v1/handoffs/\(id)/accept"
        case let .handoffReject(id, _):
            return "/api/mobile/v1/handoffs/\(id)/reject"
        case .namespaces:
            return "/api/mobile/v1/namespaces"
        case .alerts:
            return "/api/mobile/v1/alerts"
        case let .alertAck(id, _):
            return "/api/mobile/v1/alerts/\(id)/ack"
        case .autofixProposals:
            return "/api/mobile/v1/autofix/proposals"
        case let .autofixApprove(id):
            return "/api/mobile/v1/autofix/proposals/\(id)/approve"
        case let .autofixReject(id):
            return "/api/mobile/v1/autofix/proposals/\(id)/reject"
        case .recoveryTelemetryUpload:
            return "/api/mobile/v1/telemetry/recovery"
        case .millsPipelineRuns:
            return "/api/mills/pipeline/runs"
        case .millsKPIs:
            return "/api/mills/kpis"
        case let .millsPipelineRunDetail(id):
            return "/api/mills/pipeline/runs/\(id)"
        case .millsBacklog:
            return "/api/mills/backlog"
        case .patternsCatalog:
            return "/api/patterns"
        case .millsSpinningRoomFrames:
            return "/api/mills/spinning-room/frames"
        case .millsSpinRuns:
            return "/api/mills/spin/runs"
        case let .millsSpinRun(id):
            return "/api/mills/spin/runs/\(id)"
        case .millsSpinAsync:
            return "/api/mills/spin/async"
        case .plans:
            return "/api/plans"
        case let .planDetail(id):
            return "/api/plans/\(id)"
        case let .planAdvance(id, _, _):
            return "/api/plans/\(id)/advance"
        case let .millsPipelineEscalate(id, _):
            return "/api/mills/pipeline/runs/\(id)/escalate"
        case .weaverStatus:
            return "/api/weaver/status"
        case .weaverHistory:
            return "/api/weaver/history"
        case .weaverMetrics:
            return "/api/weaver/metrics"
        case .aimodelsRoles:
            return "/api/aimodels/roles"
        case .vendorSessions:
            return "/api/vendor-sessions"
        case .vendorSessionSearch:
            return "/api/vendor-sessions/search"
        }
    }

    var isMutation: Bool {
        switch self {
        case .workflowApprove, .workflowReject, .createSession, .endSession,
             .handoffAccept, .handoffReject,
             .pushRegister, .pushUnregister, .sandboxStart, .sandboxStop,
             .spawnAgent, .spawnStop,
             .spawnSendMessage, .spawnInterrupt,
             .millsSpinAsync, .planAdvance, .millsPipelineEscalate,
             .alertAck, .autofixApprove, .autofixReject,
             .recoveryTelemetryUpload:
            return true
        default:
            return false
        }
    }

    /// Whether this route is gated by the HUD admin token (HUD_ADMIN_TOKEN),
    /// distinct from the mobile pairing bearer. When true, `APIClient` attaches
    /// the stored admin token as `X-Admin-Token` so the request clears the HUD's
    /// `requireAdminToken` gate (`internal/hud/domain/mills/mills.go` →
    /// `handleProxyAdminPost`). Gated: the two Mills mutations the app performs
    /// (the async spin and the pipeline escalate) plus the auto-fix *approve*.
    /// Plan advance/priority go through the plans domain's *un*-gated handlers
    /// (matching the HUD web frontend, which advances with a bare fetch), so
    /// they are NOT listed here.
    ///
    /// `autofixApprove` is the one `/api/mobile/v1` path that carries the admin
    /// token: `handleApproveProposal` calls `RequireAdminToken` before executing
    /// the proposal (`internal/hud/domain/alerting/handlers.go`), so the pairing
    /// bearer alone gets a 401. Its siblings `autofixReject` and `alertAck` are
    /// NOT gated and deliberately stay off this list.
    var requiresAdminToken: Bool {
        switch self {
        case .millsSpinAsync, .millsPipelineEscalate, .autofixApprove:
            return true
        default:
            return false
        }
    }

    func urlRequest(baseURL: URL) throws -> URLRequest {
        guard var components = URLComponents(url: baseURL.appendingPathComponent(path), resolvingAgainstBaseURL: false) else {
            throw LoomAPIError.invalidURL(url: baseURL.absoluteString + path)
        }

        // Query parameters
        switch self {
        case let .sessions(status), let .sessionsTree(status):
            if let status {
                components.queryItems = [URLQueryItem(name: "status", value: status)]
            }
        case let .sessionEvents(_, limit):
            if let limit {
                components.queryItems = [URLQueryItem(name: "limit", value: String(limit))]
            }
        case let .tasks(status, agentId, sessionId, limit, search):
            var items: [URLQueryItem] = []
            if let status {
                items.append(URLQueryItem(name: "status", value: status.rawValue))
            }
            if let agentId {
                items.append(URLQueryItem(name: "agent_id", value: agentId))
            }
            if let sessionId {
                items.append(URLQueryItem(name: "session_id", value: sessionId))
            }
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            if let search {
                items.append(URLQueryItem(name: "search", value: search))
            }
            if !items.isEmpty { components.queryItems = items }
        case let .workflows(status, agentId, limit):
            var items: [URLQueryItem] = []
            if let status {
                items.append(URLQueryItem(name: "status", value: status.rawValue))
            }
            if let agentId {
                items.append(URLQueryItem(name: "agent_id", value: agentId))
            }
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            if !items.isEmpty { components.queryItems = items }
        case let .presence(status, agentId, limit):
            var items: [URLQueryItem] = []
            if let status {
                items.append(URLQueryItem(name: "status", value: status.rawValue))
            }
            if let agentId {
                items.append(URLQueryItem(name: "agent_id", value: agentId))
            }
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            if !items.isEmpty { components.queryItems = items }
        case let .memoryItems(tier, query, limit):
            var items: [URLQueryItem] = [URLQueryItem(name: "tier", value: tier.rawValue)]
            if let query {
                items.append(URLQueryItem(name: "query", value: query))
            }
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            components.queryItems = items
        case let .stream(types, agentId, sessionId, limit):
            var items: [URLQueryItem] = []
            if let types, !types.isEmpty {
                items.append(URLQueryItem(name: "types", value: types.joined(separator: ",")))
            }
            if let agentId {
                items.append(URLQueryItem(name: "agent_id", value: agentId))
            }
            if let sessionId {
                items.append(URLQueryItem(name: "session_id", value: sessionId))
            }
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            if !items.isEmpty { components.queryItems = items }
        case let .graphEntities(type, query, limit):
            var items: [URLQueryItem] = []
            if let type {
                items.append(URLQueryItem(name: "type", value: type))
            }
            if let query {
                items.append(URLQueryItem(name: "q", value: query))
            }
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            if !items.isEmpty { components.queryItems = items }
        case let .graphPath(sourceId, targetId, maxDepth):
            var items: [URLQueryItem] = [
                URLQueryItem(name: "source_id", value: sourceId),
                URLQueryItem(name: "target_id", value: targetId),
            ]
            if let maxDepth {
                items.append(URLQueryItem(name: "max_depth", value: String(maxDepth)))
            }
            components.queryItems = items
        case let .reasoningChains(status, limit):
            var items: [URLQueryItem] = []
            if let status {
                items.append(URLQueryItem(name: "status", value: status.rawValue))
            }
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            if !items.isEmpty { components.queryItems = items }
        case let .audit(source, limit):
            var items: [URLQueryItem] = []
            if let source { items.append(URLQueryItem(name: "source", value: source)) }
            if let limit { items.append(URLQueryItem(name: "limit", value: String(limit))) }
            if !items.isEmpty { components.queryItems = items }
        case let .agents(status, type, limit):
            var items: [URLQueryItem] = []
            if let status {
                items.append(URLQueryItem(name: "status", value: status.rawValue))
            }
            if let type {
                items.append(URLQueryItem(name: "type", value: type))
            }
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            if !items.isEmpty { components.queryItems = items }
        case let .handoffs(limit):
            if let limit {
                components.queryItems = [URLQueryItem(name: "limit", value: String(limit))]
            }
        case let .spawnTelemetryTools(_, offset, limit),
             let .spawnTelemetryFiles(_, offset, limit),
             let .spawnTelemetryErrors(_, offset, limit):
            var items: [URLQueryItem] = []
            if let offset {
                items.append(URLQueryItem(name: "offset", value: String(offset)))
            }
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            if !items.isEmpty { components.queryItems = items }
        case let .millsPipelineRuns(state, limit):
            var items: [URLQueryItem] = []
            if let state {
                items.append(URLQueryItem(name: "state", value: state))
            }
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            if !items.isEmpty { components.queryItems = items }
        case let .millsKPIs(window):
            // The operator's /api/mills/kpis handler requires ?window=, so
            // pass it through verbatim. The HUD's proxy preserves query.
            components.queryItems = [URLQueryItem(name: "window", value: window)]
        case let .patternsCatalog(status):
            if let status {
                components.queryItems = [URLQueryItem(name: "status", value: status)]
            }
        case let .millsSpinRuns(limit):
            if let limit {
                components.queryItems = [URLQueryItem(name: "limit", value: String(limit))]
            }
        case let .alerts(limit, severity):
            // handleListAlerts defaults limit to 50 and treats an empty
            // `severity` as "no filter".
            var items: [URLQueryItem] = []
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            if let severity, !severity.isEmpty {
                items.append(URLQueryItem(name: "severity", value: severity))
            }
            if !items.isEmpty { components.queryItems = items }
        case let .vendorSessions(cwdContains, limit):
            // Server defaults apply when unset (listParamsFromQuery treats
            // absent/malformed values as zero).
            var items: [URLQueryItem] = []
            if let cwdContains, !cwdContains.isEmpty {
                items.append(URLQueryItem(name: "cwd_contains", value: cwdContains))
            }
            if let limit {
                items.append(URLQueryItem(name: "limit", value: String(limit)))
            }
            if !items.isEmpty { components.queryItems = items }
        case let .vendorSessionSearch(query, cwdContains, maxResults):
            // `query` is required server-side (400 when empty).
            var items: [URLQueryItem] = [URLQueryItem(name: "query", value: query)]
            if let cwdContains, !cwdContains.isEmpty {
                items.append(URLQueryItem(name: "cwd_contains", value: cwdContains))
            }
            if let maxResults {
                items.append(URLQueryItem(name: "max_results", value: String(maxResults)))
            }
            components.queryItems = items
        default:
            break
        }

        guard let url = components.url else {
            throw LoomAPIError.invalidURL(url: components.string ?? path)
        }

        var request = URLRequest(url: url)
        request.httpMethod = method

        // Request body
        switch self {
        case let .createSession(agentId, namespace, description, autoRecall):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            var body: [String: Any] = ["agent_id": agentId]
            if let namespace { body["namespace"] = namespace }
            if let description { body["description"] = description }
            if let autoRecall { body["auto_recall"] = autoRecall }
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case let .endSession(_, summarize):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            var body: [String: Any] = [:]
            if let summarize { body["summarize"] = summarize }
            request.httpBody = try JSONSerialization.data(withJSONObject: body)
        case let .pushRegister(token, platform):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            let body: [String: Any] = [
                "token": token,
                "platform": platform.rawValue,
            ]
            request.httpBody = try JSONSerialization.data(withJSONObject: body)
        case let .pushUnregister(token):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            let body: [String: Any] = ["token": token]
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case let .sandboxStart(project, agentId):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            var body: [String: Any] = ["project": project]
            if let agentId { body["agent_id"] = agentId }
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case let .sandboxStop(project):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            let body: [String: Any] = ["project": project]
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case let .spawnAgent(spawnRequest):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONEncoder().encode(spawnRequest)

        case .spawnStop:
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: [:] as [String: Any])

        case let .spawnSendMessage(_, text):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            let body: [String: Any] = ["text": text]
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case .spawnInterrupt:
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: [:] as [String: Any])

        case let .workflowApprove(_, stepId):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            let body: [String: Any] = ["step_id": stepId]
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case let .workflowReject(_, stepId, reason):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            var body: [String: Any] = ["step_id": stepId]
            if let reason { body["reason"] = reason }
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case let .handoffAccept(_, sessionId, targetAgentId, importEntries):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            var body: [String: Any] = ["import_entries": importEntries]
            if let sessionId, !sessionId.isEmpty { body["session_id"] = sessionId }
            if let targetAgentId, !targetAgentId.isEmpty { body["target_agent_id"] = targetAgentId }
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case let .handoffReject(_, reason):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            var body: [String: Any] = [:]
            let trimmed = (reason ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
            if !trimmed.isEmpty { body["reason"] = trimmed }
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case let .millsSpinAsync(spinRequest):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: spinRequest.bodyJSON())

        case let .planAdvance(_, toPhase, note):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            var body: [String: Any] = ["to_phase": toPhase, "agent_id": "mobile-user"]
            if let note, !note.isEmpty { body["note"] = note }
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case let .millsPipelineEscalate(_, reason):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            // The operator defaults an empty reason to "manual escalation";
            // send a source-tagged reason so the audit trail shows it came
            // from the phone.
            let r = (reason ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
            let body: [String: Any] = ["reason": r.isEmpty ? "escalated from iOS companion" : r]
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case let .alertAck(_, ackedBy):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            // handleAckAlert decodes `{"acked_by": "..."}` and falls back to
            // "hud-user" on a decode failure or an empty value. Tag the ack so
            // the stored `acked_by` shows the phone did it.
            let who = (ackedBy ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
            let body: [String: Any] = ["acked_by": who.isEmpty ? "ios-companion" : who]
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        case .autofixApprove, .autofixReject:
            // Both handlers ignore the body; send an empty object so the POST
            // is well-formed (matching .spawnStop / .spawnInterrupt).
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: [:] as [String: Any])

        case let .recoveryTelemetryUpload(samples, sloTargetSeconds):
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            let body: [String: Any] = [
                "samples": samples,
                "slo_target_seconds": sloTargetSeconds,
            ]
            request.httpBody = try JSONSerialization.data(withJSONObject: body)

        default:
            break
        }

        return request
    }
}
