import Testing
import Foundation
@testable import LoomCompanionKit

@Suite("AttentionLaneKind")
struct AttentionLaneKindTests {

    // MARK: - Helpers

    /// Build a lane mirroring what `buildMobileAttentionLanes` emits for each type.
    private func lane(
        type: String,
        targetKind: String = "",
        deepLink: String = "",
        summary: String = "",
        route: String = "work"
    ) -> DashboardAttentionLane {
        DashboardAttentionLane(
            type: type,
            id: "id-\(type)",
            label: "\(type) lane",
            route: route,
            scope: "scope",
            summary: summary,
            severity: "info",
            targetKind: targetKind,
            deepLink: deepLink
        )
    }

    // MARK: - Live mobile backend contract

    @Test("Classifies the four live backend lane types")
    func classifiesBackendTypes() {
        #expect(lane(type: "agent", targetKind: "session", deepLink: "loom://agent/a", route: "people").kind == .agent)
        #expect(lane(type: "namespace", targetKind: "task_filter", deepLink: "loom://tasks?status=blocked").kind == .work)
        #expect(lane(type: "conflict", targetKind: "connection", deepLink: "loom://work").kind == .conflict)
    }

    @Test("Merge-ready lane stays .merge despite task_filter target")
    func mergeLaneNotCollapsedToWork() {
        // The backend tags the merge lane with target_kind="task_filter", so
        // isTaskLane is true — explicit type must win over the task fallback.
        let merge = lane(
            type: "merge",
            targetKind: "task_filter",
            deepLink: "loom://tasks?status=in_progress",
            summary: "2 branches ready to merge"
        )
        #expect(merge.isTaskLane)
        #expect(merge.kind == .merge)
    }

    @Test("Untyped task-shaped lane falls back to .work")
    func untypedTaskLaneFallsBackToWork() {
        let untyped = lane(type: "", targetKind: "task_filter", deepLink: "loom://tasks?status=blocked")
        #expect(untyped.kind == .work)
    }

    @Test("Unknown non-task lane is .other")
    func unknownLaneIsOther() {
        #expect(lane(type: "mystery").kind == .other)
        #expect(lane(type: "").kind == .other)
    }

    // MARK: - Legacy HUD vocabulary (forward-compat)

    @Test("Retains legacy HUD lane types")
    func classifiesLegacyTypes() {
        #expect(AttentionLaneKind(typeKey: "approval") == .approval)
        #expect(AttentionLaneKind(typeKey: "workflow_approval") == .approval)
        #expect(AttentionLaneKind(typeKey: "degraded_server") == .server)
        #expect(AttentionLaneKind(typeKey: "idle_agent") == .idleAgent)
        #expect(AttentionLaneKind(typeKey: "merge_conflict") == .conflict)
        #expect(AttentionLaneKind(typeKey: "handoff") == .handoff)
        #expect(AttentionLaneKind(typeKey: "blocked_task") == .work)
    }

    // MARK: - Presentation

    @Test("Live backend types no longer fall back to the generic flag icon")
    func backendTypesHaveDistinctIcons() {
        // The regression this slice fixes: agent/namespace/merge previously
        // rendered the generic flag on both the hero and the queue cards.
        for kind in [AttentionLaneKind.agent, .work, .merge] {
            #expect(kind.heroIcon != "flag.fill")
            #expect(kind.rowIcon != "flag.fill")
        }
        #expect(AttentionLaneKind.agent.heroIcon == "person.fill.questionmark")
        #expect(AttentionLaneKind.merge.heroIcon == "arrow.triangle.pull")
    }

    @Test("Only .other uses the generic flag fallback")
    func onlyOtherUsesFlag() {
        for kind in AttentionLaneKind.allCases where kind != .other {
            #expect(kind.heroIcon != "flag.fill")
        }
        #expect(AttentionLaneKind.other.heroIcon == "flag.fill")
    }

    @Test("Every kind supplies a non-empty singular title")
    func singularTitlesPresent() {
        for kind in AttentionLaneKind.allCases {
            #expect(!kind.singularTitle.isEmpty)
        }
    }

    @Test("Aggregate titles are known for live types, nil for other")
    func aggregateTitles() {
        #expect(AttentionLaneKind.agent.aggregateTitleIfKnown == "Agents need attention")
        #expect(AttentionLaneKind.merge.aggregateTitleIfKnown == "Merge-ready branches")
        #expect(AttentionLaneKind.work.aggregateTitleIfKnown == "Work lanes")
        #expect(AttentionLaneKind.other.aggregateTitleIfKnown == nil)
    }
}
