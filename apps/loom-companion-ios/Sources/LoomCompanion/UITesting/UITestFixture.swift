import Foundation
import LoomCompanionKit

enum UITestFixture {
    static let launchArgument = "--uitesting-work-fixture"

    static var isEnabled: Bool {
        ProcessInfo.processInfo.arguments.contains(launchArgument)
    }

    static func makeAPIClient() -> APIClient {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [UITestURLProtocol.self]
        return APIClient(
            baseURL: URL(string: "https://loom-ui-test.invalid")!,
            token: "ui-test-token",
            session: URLSession(configuration: configuration)
        )
    }

    fileprivate static func responseDelay(for path: String) -> TimeInterval {
        guard path == "/api/mobile/v1/tasks" else { return 0.05 }
        let prefix = "--uitesting-response-delay-ms="
        guard let argument = ProcessInfo.processInfo.arguments.first(where: { $0.hasPrefix(prefix) }),
              let milliseconds = Double(argument.dropFirst(prefix.count))
        else {
            return 0.8
        }
        return max(0, milliseconds / 1_000)
    }

    fileprivate static func responseBody(for path: String) -> Data {
        let data: Any
        switch path {
        case "/api/mobile/v1/dashboard":
            data = [
                "daemon_running": true,
                "server_count": 7,
                "active_sessions": 3,
                "active_agents": 5,
                "idle_agents": 1,
                "offline_agents": 0,
                "updated_at": "2026-07-14T15:00:00Z",
                "health": [
                    "total_servers": 7,
                    "healthy_servers": 7,
                    "degraded_servers": 0,
                    "down_servers": 0,
                    "idle_servers": 0,
                ],
                "coordination": [
                    "summary": [:],
                    "attention_lanes": [],
                ],
                "recent_timeline": [],
            ]
        case "/api/mobile/v1/tasks":
            data = [
                "tasks": workTasks,
                "counts": [
                    "pending": 3,
                    "in_progress": 2,
                    "blocked": 4,
                    "completed": 12,
                ],
            ]
        case "/api/mobile/v1/workflows":
            data = [
                "workflows": [],
                "pending_approvals": 0,
                "deprecated": false,
            ]
        case "/api/mobile/v1/agent/spawn/config":
            data = [
                "agent_types": [["id": "codex", "name": "Codex", "available": true]],
                "projects": [["name": "loom-core", "path": "/workspace/services/loom-core"]],
                "defaults": [
                    "agent_type": "codex",
                    "base_branch": "main",
                    "memory_mb": 4096,
                    "cpus": 2.0,
                    "timeout_minutes": 30,
                ],
            ]
        default:
            data = [:]
        }

        let envelope: [String: Any] = [
            "ok": true,
            "data": data,
            "meta": [
                "request_id": "ui-test",
                "timestamp": "2026-07-14T15:00:00Z",
            ],
        ]
        return (try? JSONSerialization.data(withJSONObject: envelope)) ?? Data()
    }

    private static let workTasks: [[String: Any]] = [
        task(id: "blocked-1", title: "Stabilize first Work launch", agent: "codex-ios", priority: "critical", status: "blocked", projected: false),
        task(id: "blocked-2", title: "Resolve dashboard refresh ownership", agent: "codex-ios", priority: "high", status: "blocked", projected: true),
        task(id: "blocked-3", title: "Verify simulator loading states", agent: "qa-agent", priority: "high", status: "blocked", projected: false),
        task(id: "blocked-4", title: "Audit Dynamic Type layout", agent: "accessibility-agent", priority: "high", status: "blocked", projected: false),
        task(id: "active-1", title: "Build deterministic UI fixture", agent: "codex-ios", priority: "high", status: "in_progress", projected: false),
        task(id: "pending-1", title: "Polish operator queue hierarchy", agent: "design-agent", priority: "medium", status: "pending", projected: false),
    ]

    private static func task(
        id: String,
        title: String,
        agent: String,
        priority: String,
        status: String,
        projected: Bool
    ) -> [String: Any] {
        [
            "id": id,
            "session_id": "session-\(id)",
            "agent_id": agent,
            "namespace": "services/loom-core/ios-ux",
            "title": title,
            "context": "Deterministic UI test fixture",
            "priority": priority,
            "status": status,
            "tags": ["ios", "ux"],
            "blocked_by": status == "blocked" ? ["fixture-dependency"] : [],
            "is_projected": projected,
            "created_at": "2026-07-14T14:00:00Z",
            "updated_at": "2026-07-14T15:00:00Z",
        ]
    }
}

private final class UITestURLProtocol: URLProtocol {
    private var responseWorkItem: DispatchWorkItem?

    override class func canInit(with request: URLRequest) -> Bool {
        request.url?.host == "loom-ui-test.invalid"
    }

    override class func canonicalRequest(for request: URLRequest) -> URLRequest {
        request
    }

    override func startLoading() {
        guard let url = request.url else { return }
        let item = DispatchWorkItem { [weak self] in
            guard let self else { return }
            let response = HTTPURLResponse(
                url: url,
                statusCode: 200,
                httpVersion: "HTTP/1.1",
                headerFields: ["Content-Type": "application/json"]
            )!
            self.client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            self.client?.urlProtocol(self, didLoad: UITestFixture.responseBody(for: url.path))
            self.client?.urlProtocolDidFinishLoading(self)
        }
        responseWorkItem = item
        DispatchQueue.global(qos: .userInitiated).asyncAfter(
            deadline: .now() + UITestFixture.responseDelay(for: url.path),
            execute: item
        )
    }

    override func stopLoading() {
        responseWorkItem?.cancel()
        responseWorkItem = nil
    }
}
