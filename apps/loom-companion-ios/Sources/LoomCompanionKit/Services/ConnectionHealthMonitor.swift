import Foundation

/// Connection health states.
public enum ConnectionHealth: Sendable, Equatable {
    case unknown
    case healthy
    case degradedStream
    case authFailure(message: String)
    case permissionDenied(message: String)
    case gatewayRouteMissing(message: String)
    case unreachable
    case rateLimited
}

/// Rolling telemetry for transient connection outages, toward the
/// disconnect-to-recovered SLO (MBL-5). A sample is the wall-clock duration
/// from when an established (healthy) connection first degraded transiently
/// (SSE drop, network unreachable, or rate-limit) to when it returned to
/// healthy. Auth/permission/gateway config errors are excluded — they do not
/// self-recover, so they would skew the transient-recovery distribution.
public struct SSERecoveryStats: Sendable, Equatable {
    public let count: Int
    public let lastSeconds: TimeInterval?
    public let meanSeconds: TimeInterval?
    public let p95Seconds: TimeInterval?

    public init(
        count: Int,
        lastSeconds: TimeInterval?,
        meanSeconds: TimeInterval?,
        p95Seconds: TimeInterval?
    ) {
        self.count = count
        self.lastSeconds = lastSeconds
        self.meanSeconds = meanSeconds
        self.p95Seconds = p95Seconds
    }

    /// Empty stats (no outages observed yet).
    public static let empty = SSERecoveryStats(count: 0, lastSeconds: nil, meanSeconds: nil, p95Seconds: nil)
}

/// Monitors connection health by observing API responses and SSE state.
/// Manages polling fallback when SSE is degraded and records recovery telemetry.
@Observable
public final class ConnectionHealthMonitor {
    public private(set) var health: ConnectionHealth = .unknown
    public var isPollingFallback: Bool = false
    public var lastPingTime: Date?

    /// Start of the in-progress transient outage, if any. `nil` when healthy or
    /// when the current unhealthy state is a non-transient config error.
    public private(set) var degradedSince: Date?
    /// Most recent recovery duration, in seconds.
    public private(set) var lastRecoveryDuration: TimeInterval?
    /// Rolling window of recovery durations (seconds), newest last.
    public private(set) var recoverySampleSeconds: [TimeInterval] = []

    private var pollTask: Task<Void, Never>?

    @ObservationIgnored
    private let now: () -> Date

    /// Polling interval matches HUD web frontend (30s).
    public static let pollInterval: TimeInterval = 30.0

    /// Cap on retained recovery samples (keeps the rolling p95 bounded).
    public static let maxRecoverySamples = 50

    /// Disconnect-to-recovered p95 target. Aligned with the 30s poll fallback:
    /// even a fully SSE-dead connection should recover within one poll cycle.
    public static let recoveryP95TargetSeconds: TimeInterval = 30.0

    /// Callback for polling fallback refresh. Set by the ViewModel layer.
    @ObservationIgnored
    public var onPollRefresh: (() async -> Void)?

    public init(now: @escaping () -> Date = { Date() }) {
        self.now = now
    }

    /// Handle a successful API response.
    public func handleSuccess() {
        switch health {
        case .unknown, .unreachable, .rateLimited, .gatewayRouteMissing:
            apply(.healthy)
        case .authFailure, .permissionDenied:
            apply(.healthy)
        case .healthy, .degradedStream:
            break
        }
    }

    /// Handle an API error.
    public func handleAPIError(_ error: LoomAPIError) {
        switch error {
        case let .apiError(code, message, _):
            switch code {
            case .unauthorized, .tokenRevoked:
                apply(.authFailure(message: message))
                stopPolling()
            case .forbidden:
                apply(.permissionDenied(message: message))
                stopPolling()
            case .notFound:
                let detail = message.isEmpty || message == "Not found"
                    ? "The gateway did not route /api/mobile/v1 to the mobile API backend."
                    : message
                apply(.gatewayRouteMissing(message: detail))
                stopPolling()
            case .rateLimited:
                apply(.rateLimited)
            default:
                break
            }
        case .networkError:
            apply(.unreachable)
            stopPolling()
        case .noToken:
            apply(.authFailure(message: "No token configured"))
            stopPolling()
        default:
            break
        }
    }

    /// Handle SSE connection state changes.
    public func handleSSEStateChange(_ state: SSEConnectionState) {
        switch state {
        case .connected:
            stopPolling()
            if health == .degradedStream || health == .unknown {
                apply(.healthy)
            }
        case .reconnecting:
            if health == .healthy {
                apply(.degradedStream)
            }
            startPolling()
        case .disconnected:
            if health == .healthy {
                apply(.degradedStream)
            }
            startPolling()
        case .connecting:
            break
        }
    }

    /// Record a successful ping.
    public func recordPing() {
        lastPingTime = now()
        handleSuccess()
    }

    // MARK: - Recovery Telemetry

    /// Whether a transient outage is currently being timed.
    public var isDegraded: Bool { degradedSince != nil }

    /// Elapsed seconds of the in-progress outage, if any.
    public func currentOutageSeconds() -> TimeInterval? {
        guard let started = degradedSince else { return nil }
        return max(0, now().timeIntervalSince(started))
    }

    /// Aggregate recovery telemetry over the rolling sample window.
    public var recoveryStats: SSERecoveryStats {
        let samples = recoverySampleSeconds
        guard !samples.isEmpty else { return .empty }
        let mean = samples.reduce(0, +) / Double(samples.count)
        let sorted = samples.sorted()
        // Nearest-rank p95.
        let rank = Int((0.95 * Double(sorted.count)).rounded(.up))
        let index = min(max(rank - 1, 0), sorted.count - 1)
        return SSERecoveryStats(
            count: samples.count,
            lastSeconds: lastRecoveryDuration,
            meanSeconds: mean,
            p95Seconds: sorted[index]
        )
    }

    /// Whether the rolling p95 meets the recovery SLO target. `nil` when no
    /// samples have been recorded yet.
    public var meetsRecoverySLO: Bool? {
        guard let p95 = recoveryStats.p95Seconds else { return nil }
        return p95 <= Self.recoveryP95TargetSeconds
    }

    /// Apply a health transition, recording transient-outage recovery timing.
    private func apply(_ newHealth: ConnectionHealth) {
        let previous = health
        health = newHealth

        if newHealth == .healthy {
            if let started = degradedSince {
                recordRecovery(max(0, now().timeIntervalSince(started)))
                degradedSince = nil
            }
        } else if degradedSince == nil, previous == .healthy, Self.isTransientOutage(newHealth) {
            // An established connection just degraded transiently — start timing.
            degradedSince = now()
        }
    }

    private static func isTransientOutage(_ health: ConnectionHealth) -> Bool {
        switch health {
        case .degradedStream, .unreachable, .rateLimited:
            return true
        case .unknown, .healthy, .authFailure, .permissionDenied, .gatewayRouteMissing:
            return false
        }
    }

    private func recordRecovery(_ seconds: TimeInterval) {
        lastRecoveryDuration = seconds
        recoverySampleSeconds.append(seconds)
        if recoverySampleSeconds.count > Self.maxRecoverySamples {
            recoverySampleSeconds.removeFirst(recoverySampleSeconds.count - Self.maxRecoverySamples)
        }
    }

    // MARK: - Polling Fallback

    private func startPolling() {
        guard !isPollingFallback else { return }
        isPollingFallback = true
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                do {
                    try await Task.sleep(for: .seconds(Self.pollInterval))
                } catch {
                    return
                }
                await self?.onPollRefresh?()
            }
        }
    }

    private func stopPolling() {
        pollTask?.cancel()
        pollTask = nil
        isPollingFallback = false
    }
}
