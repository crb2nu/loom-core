import SwiftUI
import WidgetKit

/// Shared "no snapshot yet" presentation for home- and lock-screen widgets.
///
/// Widgets must never render `SharedDataStore.placeholder*` from
/// `getTimeline` — that is sample data for the widget gallery, and showing it
/// on a real home screen makes an unpaired (or never-synced) install look like
/// a live fleet. `placeholder(in:)` and `getSnapshot(in:)` are the gallery
/// surfaces and may keep using sample data; `getTimeline` must fall back to an
/// explicit empty state, which is what this view renders.
///
/// `SpawnBudgetWidget` and `AttentionLaneWidget` already followed this rule;
/// the fleet/tasks/sessions/lock-screen/Mills widgets were aligned to it.
struct WidgetUnpairedView: View {
    /// Short label for the widget's own subject ("Fleet", "Tasks", …).
    let title: String
    /// SF Symbol matching the widget's normal iconography.
    let systemImage: String

    @Environment(\.widgetFamily) private var family

    var body: some View {
        switch family {
        case .accessoryInline:
            Text("Loom · not paired")
        case .accessoryCircular:
            Image(systemName: "link.badge.plus")
                .font(.title3)
        case .accessoryRectangular:
            VStack(alignment: .leading, spacing: 1) {
                Text("Loom")
                    .font(.headline)
                Text("Not paired")
                    .font(.caption2)
            }
        default:
            VStack(alignment: .leading, spacing: 6) {
                HStack(spacing: 6) {
                    Image(systemName: systemImage)
                        .foregroundStyle(.secondary)
                    Text(title)
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(.secondary)
                    Spacer()
                }

                Spacer(minLength: 0)

                Image(systemName: "link.badge.plus")
                    .font(.title2)
                    .foregroundStyle(.secondary)
                Text("Not paired")
                    .font(.headline)
                Text("Open Loom Companion and pair with your HUD to see live data here.")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                    .lineLimit(3)
                    .minimumScaleFactor(0.85)

                Spacer(minLength: 0)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}
