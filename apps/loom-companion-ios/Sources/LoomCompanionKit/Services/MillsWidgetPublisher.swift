import Foundation
#if os(iOS)
import WidgetKit
#endif

/// Sole writer of the MillsFactoryWidget's App-Group snapshot.
///
/// Two call paths keep the widget honest without a background daemon:
/// - `publish(using:)` — full fetch (KPI + pipelines + spins + plans) from
///   the dashboard sync, the app's reliable foreground cadence. Every fetch
///   degrades independently, so a flaky operator still publishes what it can.
/// - `publish(kpi:runs:spins:plans:)` — zero-cost re-publish from surfaces
///   that already hold fresh data (Mills screen / Plans board reloads).
///
/// iOS-only side effects; the derivation itself is pure
/// (`MillsWidgetData.from`) and unit-tested.
public enum MillsWidgetPublisher {
    /// Fetch everything the widget shows and persist a fresh snapshot.
    /// Never throws: the widget prefers a partial snapshot (or an
    /// operator-offline one) over going stale silently.
    public static func publish(using client: LoomAPIClientProtocol) async {
        let mills = MillsAPI(client: client)
        let control = MillsControlAPI(client: client)

        async let kpiTask: MillsKPISnapshot? = try? await mills.latestKPI(window: "1d")
        async let runsTask: [MillsPipelineRun]? = try? await mills.pipelineRuns()
        async let spinsTask: [MillsSpinRun]? = try? await control.spinRuns(limit: 10)
        async let plansTask: MillsPlanList? = try? await control.plans()

        let kpi = await kpiTask
        let runs = await runsTask
        let spins = await spinsTask
        let plans = await plansTask

        // All four nil = the HUD itself answered nothing Mills-shaped —
        // treat as operator-unreachable but keep the last counts visible
        // via the reachability flag rather than zeroing history.
        let reachable = kpi != nil || runs != nil || spins != nil || plans != nil
        publish(
            kpi: kpi,
            runs: runs ?? [],
            spins: spins ?? [],
            plans: plans?.plans ?? [],
            operatorReachable: reachable
        )
    }

    /// Persist a snapshot from already-fetched data and nudge WidgetKit.
    public static func publish(
        kpi: MillsKPISnapshot?,
        runs: [MillsPipelineRun],
        spins: [MillsSpinRun],
        plans: [MillsPlan],
        operatorReachable: Bool = true,
        now: Date = .now
    ) {
        let snapshot = MillsWidgetData.from(
            kpi: kpi,
            runs: runs,
            spins: spins,
            plans: plans,
            operatorReachable: operatorReachable,
            now: now
        )
        SharedDataStore.saveMills(snapshot)
        #if os(iOS)
        WidgetCenter.shared.reloadTimelines(ofKind: MillsFactoryWidgetKind)
        #endif
    }
}

// MARK: - Full home-screen sync

extension MillsWidgetPublisher {
    /// Publish EVERY App-Group snapshot (fleet/tasks/sessions + Mills) from
    /// one fetch pass. Surfaces that are not the classic dashboard call this:
    /// the Operator deck replaced `DashboardView` as the home tab
    /// (2026-07-14), and until the deck published, home-screen widgets froze
    /// at whatever the last classic-dashboard visit wrote — the live
    /// "widgets busted" report of 2026-08-22 was a month-old snapshot.
    ///
    /// Degradation mirrors `publish(using:)`: each fetch fails independently
    /// and a failed dashboard read keeps the previous fleet snapshot rather
    /// than zeroing it.
    public static func publishAll(using client: LoomAPIClientProtocol) async {
        if let dashboard: DashboardData = try? await client.request(.dashboard) {
            var counts: MobileTaskCounts?
            if let response: MobileTasksResponse = try? await client.request(.tasks(limit: 1)) {
                counts = response.counts
            }
            SharedDataStore.save(WidgetSnapshotBuilder.snapshot(dashboard: dashboard, counts: counts))
            #if os(iOS)
            WidgetCenter.shared.reloadAllTimelines()
            #endif
        }
        await publish(using: client)
    }
}
