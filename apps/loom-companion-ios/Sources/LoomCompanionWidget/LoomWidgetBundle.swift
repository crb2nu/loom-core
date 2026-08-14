import SwiftUI
import WidgetKit

@main
struct LoomWidgetBundle: WidgetBundle {
    var body: some Widget {
        FleetHealthWidget()
        MillsFactoryWidget()
        TasksWidget()
        ActiveSessionsWidget()
        SessionSummaryWidget()
        AttentionLaneWidget()
        SpawnBudgetWidget()
        LockScreenWidgets()
        WorkflowLiveActivityView()
        SessionLiveActivityView()
        PipelineLiveActivityView()
    }
}
