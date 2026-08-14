import SwiftUI

/// Coordinates cross-tab navigation requests from child views and deep links.
///
/// Injected into the SwiftUI environment by ContentView so any descendant
/// can trigger a tab switch or list filter without tightly coupling to the
/// tab state. ContentView observes changes to the pending-* properties via
/// `onChange` and performs the actual tab/section switching.
@Observable
final class NavigationCoordinator {
    // MARK: - Single-object navigation

    /// Pending request to navigate to a session by ID.
    var pendingSessionID: String?

    /// Pending request to navigate to an agent by ID.
    var pendingAgentID: String?

    /// Pending request to navigate to a spawn (remote execution) by ID.
    var pendingSpawnID: String?

    /// Pending request to navigate to a workflow by ID.
    var pendingWorkflowID: String?

    /// Pending request to focus a single alert by id inside the alert inbox.
    /// Only meaningful together with `pendingAlertInbox`; set by the
    /// `loom://alert/<id>` deep link and the Dashboard's alert attention lane.
    ///
    /// The alert inbox is presented by `DashboardView` (there is no Alerts tab
    /// — Spawn took that slot), so ContentView switches to the Dashboard and
    /// `DashboardView` consumes these two properties to raise the sheet.
    var pendingAlertID: String?

    // MARK: - Filtered-list navigation

    /// Pending agents-list filter (status + type). Consumers should navigate
    /// to People → Agents and preset the filter.
    var pendingAgentsFilter: AgentsFilter?

    /// Pending sessions-list filter (status + agent). Consumers should
    /// navigate to People → Sessions and preset the filter.
    var pendingSessionsFilter: SessionsFilter?

    /// Pending tasks-list filter (status + agent + session). Consumers should
    /// navigate to Work → Queue and preset the filter.
    var pendingTasksFilter: TasksFilter?

    /// Pending handoff-inbox navigation — just surface the inbox in Work.
    var pendingHandoffInbox: Bool = false

    /// Pending alert-inbox navigation — surface the inbox on the Dashboard.
    var pendingAlertInbox: Bool = false

    // MARK: - Filter payloads

    struct AgentsFilter: Equatable {
        var status: String?
        var type: String?
    }

    struct SessionsFilter: Equatable {
        var status: String?
        var agentId: String?
    }

    struct TasksFilter: Equatable {
        var status: String?
        var agentId: String?
        var sessionId: String?
    }

    // MARK: - Request helpers (imperative)

    func navigateToSession(id: String) { pendingSessionID = id }
    func navigateToAgent(id: String) { pendingAgentID = id }
    func navigateToSpawn(id: String) { pendingSpawnID = id }
    func navigateToWorkflow(id: String) { pendingWorkflowID = id }

    func filterAgents(status: String?, type: String?) {
        pendingAgentsFilter = AgentsFilter(status: status, type: type)
    }

    func filterSessions(status: String?, agentId: String?) {
        pendingSessionsFilter = SessionsFilter(status: status, agentId: agentId)
    }

    func filterTasks(status: String?, agentId: String?, sessionId: String?) {
        pendingTasksFilter = TasksFilter(status: status, agentId: agentId, sessionId: sessionId)
    }

    func openHandoffInbox() { pendingHandoffInbox = true }

    /// Open the alert inbox, optionally scrolled to (and focused on) one alert.
    func openAlertInbox(focus alertID: String? = nil) {
        pendingAlertID = alertID
        pendingAlertInbox = true
    }

    // MARK: - Clear helpers

    func clearPendingSession() { pendingSessionID = nil }
    func clearPendingAgent() { pendingAgentID = nil }
    func clearPendingSpawn() { pendingSpawnID = nil }
    func clearPendingWorkflow() { pendingWorkflowID = nil }
    func clearPendingAgentsFilter() { pendingAgentsFilter = nil }
    func clearPendingSessionsFilter() { pendingSessionsFilter = nil }
    func clearPendingTasksFilter() { pendingTasksFilter = nil }
    func clearPendingHandoffInbox() { pendingHandoffInbox = false }

    func clearPendingAlertInbox() {
        pendingAlertInbox = false
        pendingAlertID = nil
    }
}

// MARK: - Environment Key

private struct NavigationCoordinatorKey: EnvironmentKey {
    static let defaultValue: NavigationCoordinator? = nil
}

extension EnvironmentValues {
    var navigationCoordinator: NavigationCoordinator? {
        get { self[NavigationCoordinatorKey.self] }
        set { self[NavigationCoordinatorKey.self] = newValue }
    }
}
