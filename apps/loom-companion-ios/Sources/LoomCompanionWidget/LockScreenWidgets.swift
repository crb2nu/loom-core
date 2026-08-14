import SwiftUI
import WidgetKit
import LoomCompanionKit

struct LockScreenWidgets: Widget {
    let kind = "LockScreenWidgets"

    var body: some WidgetConfiguration {
        StaticConfiguration(kind: kind, provider: LockScreenProvider()) { entry in
            LockScreenWidgetView(entry: entry)
        }
        .configurationDisplayName("Loom Status")
        .description("Quick fleet status on your lock screen.")
        .supportedFamilies([
            .accessoryCircular,
            .accessoryRectangular,
            .accessoryInline,
        ])
    }
}

struct LockScreenEntry: WidgetKit.TimelineEntry {
    let date: Date
    let data: WidgetData
    /// False when no App-Group snapshot exists (never paired / never synced).
    var isPaired: Bool = true
}

struct LockScreenProvider: TimelineProvider {
    func placeholder(in context: Context) -> LockScreenEntry {
        LockScreenEntry(date: .now, data: SharedDataStore.placeholder)
    }

    func getSnapshot(in context: Context, completion: @escaping (LockScreenEntry) -> Void) {
        let data = SharedDataStore.load() ?? SharedDataStore.placeholder
        completion(LockScreenEntry(date: .now, data: data))
    }

    func getTimeline(in context: Context, completion: @escaping (Timeline<LockScreenEntry>) -> Void) {
        // Real lock screen — no sample data. See WidgetUnpairedView.
        let stored = SharedDataStore.load()
        let entry = LockScreenEntry(
            date: .now,
            data: stored ?? LockScreenProvider.emptySnapshot,
            isPaired: stored != nil
        )
        let nextUpdate = Calendar.current.date(byAdding: .minute, value: 15, to: .now) ?? .now
        completion(Timeline(entries: [entry], policy: .after(nextUpdate)))
    }

    /// All-zero snapshot used only as filler behind the unpaired state.
    static let emptySnapshot = WidgetData(
        fleet: FleetWidgetData(
            daemonRunning: false, serverCount: 0, sessionCount: 0,
            activeAgents: 0, idleAgents: 0, offlineAgents: 0,
            healthyServers: 0, degradedServers: 0, downServers: 0
        ),
        tasks: TaskWidgetData(pending: 0, inProgress: 0, blocked: 0, completed: 0, recentTitles: []),
        sessions: SessionWidgetData(activeCount: 0, topSessions: [])
    )
}

struct LockScreenWidgetView: View {
    let entry: LockScreenEntry
    @Environment(\.widgetFamily) var family

    var body: some View {
        if !entry.isPaired {
            WidgetUnpairedView(title: "Loom", systemImage: "heart.text.square")
        } else {
            switch family {
            case .accessoryCircular:
                circularView
            case .accessoryRectangular:
                rectangularView
            case .accessoryInline:
                inlineView
            default:
                circularView
            }
        }
    }

    private var circularView: some View {
        let fleet = entry.data.fleet
        let total = fleet.healthyServers + fleet.degradedServers + fleet.downServers
        return Gauge(
            value: Double(fleet.healthyServers),
            in: 0...Double(max(total, 1))
        ) {
            Image(systemName: fleet.daemonRunning ? "heart.fill" : "heart.slash")
        } currentValueLabel: {
            Text("\(fleet.healthyServers)")
                .font(.system(.title3, design: .rounded, weight: .bold))
        }
        .gaugeStyle(.accessoryCircular)
    }

    private var rectangularView: some View {
        let fleet = entry.data.fleet
        let tasks = entry.data.tasks
        return VStack(alignment: .leading, spacing: 2) {
            HStack {
                Image(systemName: "server.rack")
                Text("Loom")
                    .fontWeight(.semibold)
            }
            .font(.caption)
            Text("\(fleet.activeAgents) agents, \(tasks.blocked) blocked")
                .font(.caption2)
            Text("\(fleet.healthyServers) healthy, \(fleet.sessionCount) sessions")
                .font(.caption2)
                .foregroundStyle(.secondary)
        }
    }

    private var inlineView: some View {
        let fleet = entry.data.fleet
        return Text("Loom: \(fleet.activeAgents) agents active")
    }
}
