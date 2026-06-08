import Testing
import Foundation
@testable import LoomCompanionKit

/// Deterministic clock for recovery-duration measurement.
private final class MutableClock {
    private var date: Date
    init(_ start: Date = Date(timeIntervalSince1970: 1_000_000)) { self.date = start }
    func now() -> Date { date }
    func advance(_ seconds: TimeInterval) { date = date.addingTimeInterval(seconds) }
}

@Suite("ConnectionHealthMonitor Recovery Telemetry")
struct ConnectionRecoveryTelemetryTests {

    @Test("No recovery recorded on cold-start failure (never was healthy)")
    func coldStartNotCounted() {
        let clock = MutableClock()
        let monitor = ConnectionHealthMonitor(now: clock.now)

        // unknown -> unreachable -> healthy (never established before failing)
        monitor.handleAPIError(.networkError(underlying: "refused"))
        clock.advance(5)
        monitor.handleSuccess()

        #expect(monitor.recoverySampleSeconds.isEmpty)
        #expect(monitor.recoveryStats.count == 0)
        #expect(monitor.lastRecoveryDuration == nil)
    }

    @Test("Measures SSE disconnect-to-recovered duration")
    func measuresSSEDisconnectRecovery() {
        let clock = MutableClock()
        let monitor = ConnectionHealthMonitor(now: clock.now)

        monitor.handleSuccess() // established/healthy
        monitor.handleSSEStateChange(.disconnected)
        #expect(monitor.isDegraded)
        #expect(monitor.currentOutageSeconds() == 0)

        clock.advance(12)
        #expect(monitor.currentOutageSeconds() == 12)

        monitor.handleSSEStateChange(.connected)
        #expect(monitor.health == .healthy)
        #expect(monitor.isDegraded == false)
        #expect(monitor.lastRecoveryDuration == 12)
        #expect(monitor.recoveryStats.count == 1)
    }

    @Test("Measures network-unreachable recovery via successful poll")
    func measuresUnreachableRecovery() {
        let clock = MutableClock()
        let monitor = ConnectionHealthMonitor(now: clock.now)

        monitor.handleSuccess()
        monitor.handleAPIError(.networkError(underlying: "down"))
        clock.advance(8)
        monitor.handleSuccess() // poll fallback recovers

        #expect(monitor.lastRecoveryDuration == 8)
        #expect(monitor.recoveryStats.count == 1)
    }

    @Test("Multi-step outage is timed once, end to end")
    func multiStepOutageTimedOnce() {
        let clock = MutableClock()
        let monitor = ConnectionHealthMonitor(now: clock.now)

        monitor.handleSuccess()
        monitor.handleSSEStateChange(.disconnected) // degradedStream, clock starts
        clock.advance(3)
        monitor.handleAPIError(.networkError(underlying: "down")) // -> unreachable, still same outage
        clock.advance(7)
        monitor.handleSuccess() // recovered

        #expect(monitor.recoveryStats.count == 1)
        #expect(monitor.lastRecoveryDuration == 10) // 3 + 7, not reset mid-outage
    }

    @Test("Auth failure is not counted as a transient outage")
    func authFailureExcluded() {
        let clock = MutableClock()
        let monitor = ConnectionHealthMonitor(now: clock.now)

        monitor.handleSuccess()
        monitor.handleAPIError(.apiError(code: .unauthorized, message: "bad", requestId: "r"))
        #expect(monitor.isDegraded == false) // config error, no timer
        clock.advance(30)
        monitor.handleSuccess()

        #expect(monitor.recoverySampleSeconds.isEmpty)
    }

    @Test("p95 and mean over the sample window")
    func p95AndMean() {
        let clock = MutableClock()
        let monitor = ConnectionHealthMonitor(now: clock.now)
        monitor.handleSuccess()

        for d in 1...20 {
            monitor.handleSSEStateChange(.disconnected)
            clock.advance(TimeInterval(d))
            monitor.handleSSEStateChange(.connected)
        }

        let stats = monitor.recoveryStats
        #expect(stats.count == 20)
        #expect(stats.lastSeconds == 20)
        #expect(stats.meanSeconds == 10.5)
        // nearest-rank p95 of 1...20 -> rank ceil(19) = 19 -> 19s
        #expect(stats.p95Seconds == 19)
    }

    @Test("Recovery SLO verdict reflects the p95 target")
    func recoverySLOVerdict() {
        let clock = MutableClock()
        let monitor = ConnectionHealthMonitor(now: clock.now)
        #expect(monitor.meetsRecoverySLO == nil) // no samples yet

        monitor.handleSuccess()
        // one fast recovery, well under the 30s target
        monitor.handleSSEStateChange(.disconnected)
        clock.advance(5)
        monitor.handleSSEStateChange(.connected)
        #expect(monitor.meetsRecoverySLO == true)

        // one slow recovery pushes p95 over the target
        monitor.handleSSEStateChange(.disconnected)
        clock.advance(120)
        monitor.handleSSEStateChange(.connected)
        #expect(monitor.meetsRecoverySLO == false)
    }

    @Test("onRecovery hook fires with the current window when an outage resolves")
    func onRecoveryHookFires() async {
        let clock = MutableClock()
        let monitor = ConnectionHealthMonitor(now: clock.now)
        monitor.handleSuccess() // established/healthy

        let received: [TimeInterval] = await withCheckedContinuation { continuation in
            monitor.onRecovery = { samples in
                continuation.resume(returning: samples)
            }
            monitor.handleSSEStateChange(.disconnected)
            clock.advance(7)
            monitor.handleSSEStateChange(.connected) // records recovery -> fires onRecovery
        }

        #expect(received == [7])
    }

    @Test("Rolling window is capped")
    func rollingWindowCapped() {
        let clock = MutableClock()
        let monitor = ConnectionHealthMonitor(now: clock.now)
        monitor.handleSuccess()

        for _ in 0..<(ConnectionHealthMonitor.maxRecoverySamples + 12) {
            monitor.handleSSEStateChange(.disconnected)
            clock.advance(1)
            monitor.handleSSEStateChange(.connected)
        }

        #expect(monitor.recoverySampleSeconds.count == ConnectionHealthMonitor.maxRecoverySamples)
    }
}
