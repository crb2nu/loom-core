import Foundation

/// Server acknowledgement for a recovery-telemetry upload. Decoded from the
/// standard envelope's `data` payload; the backend also returns the recomputed
/// `device` stat, which the uploader ignores (it republishes the raw window).
public struct RecoveryTelemetryAck: Decodable, Sendable {
    public let accepted: Bool
}

/// Outcome of a single recovery-telemetry upload attempt. Typed so the trigger
/// layer (and tests) can reason about whether a sample window reached the
/// backend without inspecting transport details.
public enum RecoveryUploadOutcome: Sendable, Equatable {
    /// The window was accepted by the backend.
    case uploaded(sampleCount: Int)
    /// No samples to publish — nothing was sent.
    case skippedEmpty
    /// The window is identical to the last one accepted — nothing was sent.
    case skippedDuplicate
    /// The `mobile:telemetry` scope is not granted (HTTP 403). The scope is off
    /// by default, so the uploader stops trying for the rest of this session to
    /// keep the write ingress disciplined.
    case scopeDenied
    /// The backend rate-limited the upload (HTTP 429). Transient — the window is
    /// not marked uploaded, so a later recovery retries it.
    case rateLimited
    /// Any other failure. Transient — not marked uploaded, retried later.
    case failed(reason: String)
}

/// Publishes a device's rolling disconnect-to-recovered window to the backend
/// recovery-SLO ingest endpoint (`POST /api/mobile/v1/telemetry/recovery`,
/// MBL-5 slice 2).
///
/// The backend `Ingest` **replaces** a device's snapshot, so resending the full
/// window is idempotent — no server-side accumulation. The shared `APIClient`
/// already attaches the keying `X-Device-ID` header to every request.
///
/// Discipline (the slice-1 ingest scope is off by default and rate-limited):
/// - Dedups an unchanged window (only the most-recently *accepted* window is
///   suppressed; a transient failure leaves it eligible for retry).
/// - Stops permanently after a 403 (scope not granted) so it never spams an
///   endpoint the device is not authorized for.
/// - Treats 429 / other errors as transient, leaving the window un-armed.
public actor RecoveryTelemetryUploader {
    private let client: LoomAPIClientProtocol
    private let sloTargetSeconds: TimeInterval

    /// The most-recently *accepted* window, used to suppress redundant resends.
    private var lastUploadedWindow: [TimeInterval]?
    /// Set once the backend reports the telemetry scope is not granted.
    private var scopeDenied = false

    public init(
        client: LoomAPIClientProtocol,
        sloTargetSeconds: TimeInterval = ConnectionHealthMonitor.recoveryP95TargetSeconds
    ) {
        self.client = client
        self.sloTargetSeconds = sloTargetSeconds
    }

    /// Publish the given rolling window. Safe to call on every recovery; it
    /// suppresses empty/duplicate windows and stops after a scope denial.
    @discardableResult
    public func upload(samples: [TimeInterval]) async -> RecoveryUploadOutcome {
        guard !scopeDenied else { return .scopeDenied }
        guard !samples.isEmpty else { return .skippedEmpty }
        if let last = lastUploadedWindow, last == samples { return .skippedDuplicate }

        do {
            let ack: RecoveryTelemetryAck = try await client.request(
                .recoveryTelemetryUpload(samples: samples, sloTargetSeconds: sloTargetSeconds)
            )
            guard ack.accepted else {
                return .failed(reason: "server did not accept telemetry")
            }
            lastUploadedWindow = samples
            return .uploaded(sampleCount: samples.count)
        } catch let LoomAPIError.apiError(code, message, _) {
            switch code {
            case .forbidden:
                scopeDenied = true
                return .scopeDenied
            case .rateLimited:
                return .rateLimited
            default:
                return .failed(reason: message.isEmpty ? code.rawValue : message)
            }
        } catch {
            return .failed(reason: "\(error)")
        }
    }
}
