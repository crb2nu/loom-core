import LoomCompanionKit
import SwiftUI
import WidgetKit

/// MillsFactoryWidget — oversee the autonomous factory from the home/lock
/// screen: north-star merges (24h), floor state (running/queued/escalated),
/// efficiency KPIs, the in-flight pipeline tree's top rows, live
/// Spinning-Room spins, and plan-board review pressure. Taps deep-link to
/// the Mills tab (`loom://mills`).
///
/// Data comes from the App-Group snapshot written by `MillsWidgetPublisher`
/// (dashboard sync cadence); the timeline just re-renders it. The refresh
/// hint tightens while a spin is live so a landing draft shows sooner.
struct MillsFactoryWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: MillsFactoryWidgetKind, provider: MillsFactoryProvider()) { entry in
            MillsFactoryWidgetView(entry: entry)
                .containerBackground(.fill.tertiary, for: .widget)
                .widgetURL(URL(string: "loom://mills"))
        }
        .configurationDisplayName("Mills Factory")
        .description("Autonomous merges, in-flight pipelines, spins, and plan review pressure.")
        .supportedFamilies([
            .systemSmall, .systemMedium, .systemLarge,
            .accessoryCircular, .accessoryRectangular, .accessoryInline,
        ])
    }
}

struct MillsFactoryEntry: WidgetKit.TimelineEntry {
    let date: Date
    let data: MillsWidgetData
    /// False when no App-Group Mills snapshot exists (never paired / never
    /// synced). Distinct from `data.operatorReachable`, which means "paired,
    /// but the operator was down at the last publish".
    var isPaired: Bool = true
}

struct MillsFactoryProvider: TimelineProvider {
    func placeholder(in context: Context) -> MillsFactoryEntry {
        MillsFactoryEntry(date: .now, data: SharedDataStore.placeholderMills)
    }

    func getSnapshot(in context: Context, completion: @escaping (MillsFactoryEntry) -> Void) {
        let data = SharedDataStore.loadMills() ?? SharedDataStore.placeholderMills
        completion(MillsFactoryEntry(date: .now, data: data))
    }

    func getTimeline(in context: Context, completion: @escaping (Timeline<MillsFactoryEntry>) -> Void) {
        // Real home/lock screen — no sample data. See WidgetUnpairedView.
        let stored = SharedDataStore.loadMills()
        let data = stored ?? MillsWidgetData(operatorReachable: false)
        let entry = MillsFactoryEntry(date: .now, data: data, isPaired: stored != nil)
        // A live spin means a draft can land within minutes; otherwise the
        // dashboard-sync publisher is the real refresher and 15m is plenty.
        let minutes = data.hasLiveSpin ? 5 : 15
        let next = Calendar.current.date(byAdding: .minute, value: minutes, to: .now) ?? .now
        completion(Timeline(entries: [entry], policy: .after(next)))
    }
}

// MARK: - View

struct MillsFactoryWidgetView: View {
    let entry: MillsFactoryEntry
    @Environment(\.widgetFamily) var family

    private var data: MillsWidgetData { entry.data }

    var body: some View {
        if !entry.isPaired {
            WidgetUnpairedView(title: "Mills", systemImage: "hexagon")
        } else {
            switch family {
            case .systemSmall: smallView
            case .systemMedium: mediumView
            case .systemLarge: largeView
            case .accessoryCircular: circularView
            case .accessoryRectangular: rectangularView
            case .accessoryInline: inlineView
            default: smallView
            }
        }
    }

    // MARK: Shared semantics

    private var statusColor: Color {
        if !data.operatorReachable { return .gray }
        if data.escalatedRuns > 0 { return .red }
        if data.activeRuns > 0 { return .green }
        if data.queueDepth > 0 { return .orange }
        return .secondary
    }

    private var statusIcon: String {
        if !data.operatorReachable { return "bolt.slash.fill" }
        if data.escalatedRuns > 0 { return "exclamationmark.triangle.fill" }
        if data.activeRuns > 0 { return "gearshape.2.fill" }
        return "gearshape.2"
    }

    private func categoryColor(_ category: MillsRunCategory) -> Color {
        switch category {
        case .queued: return .gray
        case .running: return .green
        case .review: return .purple
        case .merging: return .cyan
        case .escalated: return .orange
        case .failed: return .red
        case .done: return .mint
        case .unknown: return .secondary
        }
    }

    private func age(_ date: Date?) -> String {
        guard let date else { return "" }
        return LoomFormat.relativeCompact(seconds: Int(entry.date.timeIntervalSince(date)))
    }

    // MARK: systemSmall

    private var smallView: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 5) {
                Image(systemName: statusIcon)
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(statusColor)
                Text("MILLS")
                    .font(.caption2.weight(.bold))
                    .tracking(1.2)
                    .foregroundStyle(.secondary)
                Spacer()
                if data.draftPlans > 0 {
                    Text("\(data.draftPlans)⧉")
                        .font(.caption2.weight(.semibold))
                        .foregroundStyle(.purple)
                        .accessibilityLabel("\(data.draftPlans) draft plans")
                }
            }

            Spacer(minLength: 0)

            VStack(alignment: .leading, spacing: 1) {
                Text("\(data.mergedRuns24h)")
                    .font(.system(size: 34, weight: .bold, design: .rounded))
                    .monospacedDigit()
                    .contentTransition(.numericText())
                Text(data.mergedRuns24h == 1 ? "merge · 24h" : "merges · 24h")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }

            Spacer(minLength: 0)

            HStack(spacing: 7) {
                statChip("play.fill", data.activeRuns, .green)
                statChip("tray.full.fill", data.queueDepth, .orange)
                statChip("exclamationmark.triangle.fill", data.escalatedRuns, .red)
                Spacer(minLength: 0)
            }

            Text(data.floorLine)
                .font(.system(size: 9, weight: .medium))
                .foregroundStyle(data.operatorReachable ? .secondary : Color.red)
                .lineLimit(1)
        }
    }

    private func statChip(_ icon: String, _ value: Int, _ color: Color) -> some View {
        HStack(spacing: 2) {
            Image(systemName: icon)
                .font(.system(size: 8, weight: .bold))
            Text("\(value)")
                .font(.caption2.weight(.semibold))
                .monospacedDigit()
        }
        .foregroundStyle(value > 0 ? color : .secondary.opacity(0.55))
    }

    // MARK: systemMedium

    private var mediumView: some View {
        HStack(spacing: 12) {
            VStack(alignment: .leading, spacing: 6) {
                HStack(spacing: 6) {
                    Image(systemName: statusIcon)
                        .font(.callout.weight(.semibold))
                        .foregroundStyle(statusColor)
                    Text("Mills")
                        .font(.subheadline.weight(.semibold))
                    Spacer(minLength: 0)
                }

                HStack(alignment: .firstTextBaseline, spacing: 4) {
                    Text("\(data.mergedRuns24h)")
                        .font(.system(size: 30, weight: .bold, design: .rounded))
                        .monospacedDigit()
                        .contentTransition(.numericText())
                    Text("merges 24h")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }

                Text(data.floorLine)
                    .font(.caption2.weight(.medium))
                    .foregroundStyle(data.escalatedRuns > 0 ? Color.red : .secondary)
                    .lineLimit(1)

                Spacer(minLength: 0)

                kpiStrip
            }
            .frame(maxWidth: .infinity, alignment: .leading)

            Divider().foregroundStyle(.secondary.opacity(0.3))

            VStack(alignment: .leading, spacing: 5) {
                if data.pipelines.isEmpty {
                    Spacer()
                    Text(data.operatorReachable ? "No pipelines in flight" : "Operator offline")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity, alignment: .center)
                    Spacer()
                } else {
                    ForEach(data.pipelines.prefix(3)) { row in
                        pipelineRow(row)
                    }
                    Spacer(minLength: 0)
                }
                if let spin = data.spins.first(where: { !$0.statusKind.isTerminal }) {
                    spinLine(spin)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private var kpiStrip: some View {
        HStack(spacing: 8) {
            if let rate = data.autoMergeRate {
                kpiMini("\(Int((rate * 100).rounded()))%", "auto")
            }
            if let cost = data.costPerMergeUSD {
                kpiMini(String(format: "$%.0f", cost), "/merge")
            }
            if data.draftPlans > 0 {
                kpiMini("\(data.draftPlans)", "drafts", color: .purple)
            }
            Spacer(minLength: 0)
        }
    }

    private func kpiMini(_ value: String, _ label: String, color: Color = .primary) -> some View {
        HStack(spacing: 3) {
            Text(value)
                .font(.caption2.weight(.bold))
                .monospacedDigit()
                .foregroundStyle(color)
            Text(label)
                .font(.system(size: 9))
                .foregroundStyle(.secondary)
        }
    }

    /// Deep link that escalates this run. The tap opens the app (the widget
    /// extension can't read the operator token), which shows a confirm sheet
    /// and performs the admin-gated escalate. Reuses the canonical DeepLink
    /// builder so escaping stays consistent.
    private func escalateURL(_ id: String) -> URL {
        DeepLink.pipelineEscalate(id: id).url ?? URL(string: "loom://mills")!
    }

    /// One in-flight pipeline row, wrapped in a Link so tapping escalates it
    /// (via the app + confirm). A small triangle telegraphs the action; the
    /// confirm sheet is the real guard against a mis-tap. Only rendered in
    /// medium/large, where WidgetKit supports per-row Links.
    private func pipelineRow(_ row: MillsWidgetPipelineRow) -> some View {
        let color = categoryColor(row.category)
        return Link(destination: escalateURL(row.id)) {
            HStack(spacing: 5) {
                Circle()
                    .fill(color)
                    .frame(width: 6, height: 6)
                    .padding(.leading, CGFloat(min(row.depth, 2)) * 7)
                Text(row.id)
                    .font(.system(size: 10, weight: .medium, design: .monospaced))
                    .lineLimit(1)
                    .truncationMode(.middle)
                Spacer(minLength: 3)
                Text(row.state)
                    .font(.system(size: 9, weight: .medium))
                    .foregroundStyle(color)
                    .lineLimit(1)
                if !age(row.startedAt).isEmpty {
                    Text(age(row.startedAt))
                        .font(.system(size: 9, design: .monospaced))
                        .foregroundStyle(.secondary)
                }
                Image(systemName: "exclamationmark.triangle")
                    .font(.system(size: 8, weight: .semibold))
                    .foregroundStyle(.orange.opacity(0.85))
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("Escalate \(row.id), currently \(row.state)")
        .accessibilityHint("Opens the app to confirm")
    }

    private func spinLine(_ spin: MillsWidgetSpinRow) -> some View {
        HStack(spacing: 4) {
            Image(systemName: "arrow.triangle.2.circlepath")
                .font(.system(size: 8, weight: .bold))
                .foregroundStyle(.cyan)
            Text("\(spin.frames.joined(separator: "+")) spinning…")
                .font(.system(size: 9, weight: .medium))
                .foregroundStyle(.cyan)
                .lineLimit(1)
            Spacer(minLength: 0)
            if !age(spin.startedAt).isEmpty {
                Text(age(spin.startedAt))
                    .font(.system(size: 9, design: .monospaced))
                    .foregroundStyle(.secondary)
            }
        }
    }

    // MARK: systemLarge

    private var largeView: some View {
        VStack(alignment: .leading, spacing: 10) {
            // Header + north star
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Image(systemName: statusIcon)
                    .font(.title3.weight(.semibold))
                    .foregroundStyle(statusColor)
                Text("Mills Factory")
                    .font(.headline)
                Spacer()
                Text("\(data.mergedRuns24h)")
                    .font(.system(size: 30, weight: .bold, design: .rounded))
                    .monospacedDigit()
                    .contentTransition(.numericText())
                Text("merges 24h")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }

            // KPI tiles
            HStack(spacing: 8) {
                kpiTile("play.fill", "Running", "\(data.activeRuns)", data.activeRuns > 0 ? .green : .secondary)
                kpiTile("tray.full.fill", "Queued", "\(data.queueDepth)", data.queueDepth > 0 ? .orange : .secondary)
                kpiTile("exclamationmark.triangle.fill", "Escalated", "\(data.escalatedRuns)", data.escalatedRuns > 0 ? .red : .secondary)
                if let rate = data.autoMergeRate {
                    kpiTile("checkmark.seal.fill", "Auto-merge", "\(Int((rate * 100).rounded()))%", .mint)
                } else if let cost = data.costPerMergeUSD {
                    kpiTile("dollarsign.circle.fill", "Per merge", String(format: "$%.2f", cost), .mint)
                }
            }

            // Pipelines
            sectionLabel(data.pipelines.isEmpty ? "No pipelines in flight" : "In-flight pipelines")
            if !data.pipelines.isEmpty {
                VStack(spacing: 4) {
                    ForEach(data.pipelines.prefix(5)) { row in
                        pipelineRow(row)
                    }
                }
            }

            // Spinning Room
            if !data.spins.isEmpty {
                sectionLabel("Spinning Room")
                VStack(spacing: 3) {
                    ForEach(data.spins.prefix(2)) { spin in
                        spinRowLarge(spin)
                    }
                }
            }

            Spacer(minLength: 0)

            // Plan pressure footer
            HStack(spacing: 4) {
                Image(systemName: "square.stack.3d.up")
                    .font(.system(size: 9, weight: .semibold))
                    .foregroundStyle(.purple)
                Text(planFooter)
                    .font(.system(size: 10, weight: .medium))
                    .foregroundStyle(.secondary)
                Spacer(minLength: 0)
                Text("as of \(age(data.lastUpdated)) ago")
                    .font(.system(size: 9, design: .monospaced))
                    .foregroundStyle(.tertiary)
            }
        }
    }

    private var planFooter: String {
        var parts: [String] = []
        if data.draftPlans > 0 {
            parts.append("\(data.draftPlans) draft\(data.draftPlans == 1 ? "" : "s") awaiting review")
        }
        parts.append("\(data.activePlans) active plan\(data.activePlans == 1 ? "" : "s")")
        return parts.joined(separator: " · ")
    }

    private func sectionLabel(_ text: String) -> some View {
        Text(text.uppercased())
            .font(.system(size: 9, weight: .bold))
            .tracking(0.8)
            .foregroundStyle(.secondary)
    }

    private func kpiTile(_ icon: String, _ label: String, _ value: String, _ color: Color) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Image(systemName: icon)
                .font(.system(size: 10, weight: .semibold))
                .foregroundStyle(color)
            Text(value)
                .font(.system(size: 15, weight: .semibold, design: .rounded))
                .monospacedDigit()
            Text(label)
                .font(.system(size: 9))
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func spinRowLarge(_ spin: MillsWidgetSpinRow) -> some View {
        let live = !spin.statusKind.isTerminal
        let color: Color = live ? .cyan : (spin.statusKind == .succeeded ? .mint : .red)
        return HStack(spacing: 5) {
            Image(systemName: live ? "arrow.triangle.2.circlepath" : (spin.statusKind == .succeeded ? "checkmark.circle.fill" : "xmark.circle.fill"))
                .font(.system(size: 9, weight: .bold))
                .foregroundStyle(color)
            Text(spin.frames.joined(separator: " + "))
                .font(.system(size: 10, weight: .medium, design: .monospaced))
                .lineLimit(1)
                .truncationMode(.middle)
            Spacer(minLength: 3)
            Text(live ? "spinning…" : (spin.statusKind == .succeeded ? "\(spin.planCount) draft\(spin.planCount == 1 ? "" : "s")" : spin.status))
                .font(.system(size: 9, weight: .medium))
                .foregroundStyle(color)
            if !age(spin.startedAt).isEmpty {
                Text(age(spin.startedAt))
                    .font(.system(size: 9, design: .monospaced))
                    .foregroundStyle(.secondary)
            }
        }
        .accessibilityElement(children: .combine)
    }

    // MARK: Lock screen accessories

    private var circularView: some View {
        VStack(spacing: 0) {
            Text("\(data.mergedRuns24h)")
                .font(.system(size: 20, weight: .bold, design: .rounded))
                .monospacedDigit()
            Text("MILLS")
                .font(.system(size: 7, weight: .bold))
                .tracking(0.5)
            if data.escalatedRuns > 0 {
                Image(systemName: "exclamationmark.triangle.fill")
                    .font(.system(size: 7))
            } else {
                Text("\(data.activeRuns)▶")
                    .font(.system(size: 8, weight: .semibold))
                    .monospacedDigit()
            }
        }
        .accessibilityLabel("Mills: \(data.mergedRuns24h) merges, \(data.floorLine)")
    }

    private var rectangularView: some View {
        VStack(alignment: .leading, spacing: 1) {
            HStack(spacing: 3) {
                Image(systemName: "gearshape.2.fill")
                    .font(.system(size: 9, weight: .semibold))
                Text("Mills · \(data.mergedRuns24h) merged 24h")
                    .font(.system(size: 12, weight: .semibold))
                    .lineLimit(1)
            }
            Text(data.floorLine)
                .font(.system(size: 11))
                .lineLimit(1)
            Text(rectangularDetail)
                .font(.system(size: 10))
                .foregroundStyle(.secondary)
                .lineLimit(1)
        }
        .accessibilityElement(children: .combine)
    }

    private var rectangularDetail: String {
        if data.hasLiveSpin { return "spin in flight" }
        if data.draftPlans > 0 {
            return "\(data.draftPlans) draft\(data.draftPlans == 1 ? "" : "s") to review"
        }
        if let rate = data.autoMergeRate {
            return "auto-merge \(Int((rate * 100).rounded()))%"
        }
        return "\(data.activePlans) active plans"
    }

    private var inlineView: some View {
        Text(inlineText)
    }

    private var inlineText: String {
        guard data.operatorReachable else { return "Mills offline" }
        var s = "Mills \(data.mergedRuns24h)✓ \(data.activeRuns)▶"
        if data.escalatedRuns > 0 { s += " \(data.escalatedRuns)⚠" }
        return s
    }
}

// MARK: - Previews

#Preview("Mills · small", as: .systemSmall) {
    MillsFactoryWidget()
} timeline: {
    MillsFactoryEntry(date: .now, data: SharedDataStore.placeholderMills)
}

#Preview("Mills · medium", as: .systemMedium) {
    MillsFactoryWidget()
} timeline: {
    MillsFactoryEntry(date: .now, data: SharedDataStore.placeholderMills)
}

#Preview("Mills · large", as: .systemLarge) {
    MillsFactoryWidget()
} timeline: {
    MillsFactoryEntry(date: .now, data: SharedDataStore.placeholderMills)
}
