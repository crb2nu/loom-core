import Testing
import Foundation
@testable import LoomCompanionKit

/// Endpoint wire-contract tests for the MBL-5 slice-2 recovery-telemetry
/// uploader: path, method, mutation flag, POST body, and ack decoding. These
/// pin the contract against the merged slice-1 backend
/// (`POST /api/mobile/v1/telemetry/recovery`, body `{samples, slo_target_seconds}`).
@Suite("RecoveryTelemetryEndpoint")
struct RecoveryTelemetryEndpointTests {
    private let base = URL(string: "https://localhost:3333")!

    @Test("Recovery telemetry endpoint path")
    func path() {
        #expect(
            Endpoint.recoveryTelemetryUpload(samples: [1.0], sloTargetSeconds: 30).path
                == "/api/mobile/v1/telemetry/recovery"
        )
    }

    @Test("Recovery telemetry upload is a POST mutation")
    func methodAndMutation() {
        let endpoint = Endpoint.recoveryTelemetryUpload(samples: [1.0], sloTargetSeconds: 30)
        #expect(endpoint.method == "POST")
        #expect(endpoint.isMutation == true)
    }

    @Test("Recovery telemetry upload serializes samples + slo_target_seconds body")
    func body() throws {
        let request = try Endpoint.recoveryTelemetryUpload(
            samples: [5.2, 8.1, 22.3],
            sloTargetSeconds: 30
        ).urlRequest(baseURL: base)

        #expect(request.httpMethod == "POST")
        #expect(request.value(forHTTPHeaderField: "Content-Type") == "application/json")
        #expect(request.url?.path == "/api/mobile/v1/telemetry/recovery")

        let bodyData = try #require(request.httpBody)
        let json = try JSONSerialization.jsonObject(with: bodyData) as! [String: Any]
        let samples = try #require(json["samples"] as? [Double])
        #expect(samples == [5.2, 8.1, 22.3])
        #expect(json["slo_target_seconds"] as? Double == 30)
    }

    @Test("Recovery telemetry ack decodes from the standard envelope data")
    func ackDecodesFromEnvelope() throws {
        let client = APIClient(baseURL: base, token: "test-token")
        let payload = """
        {
          "ok": true,
          "data": {
            "accepted": true,
            "device": { "device_id": "dev-1", "sample_count": 3, "p95_seconds": 22.3, "meets_slo": true }
          },
          "meta": { "request_id": "req_ack", "timestamp": "2026-06-08T15:00:00Z" }
        }
        """
        let ack: RecoveryTelemetryAck = try client.decodeResponse(Data(payload.utf8), statusCode: 200)
        #expect(ack.accepted == true)
    }
}

/// Behavior tests for `RecoveryTelemetryUploader`: empty/duplicate suppression,
/// scope-denial short-circuit, and transient-vs-permanent error handling. Drive
/// a spy conforming to `LoomAPIClientProtocol` so no network is touched.
@Suite("RecoveryTelemetryUploader")
struct RecoveryTelemetryUploaderTests {

    /// Serial spy — the uploader (an actor) and these tests await each call, so
    /// plain mutable state is safe.
    private final class SpyClient: LoomAPIClientProtocol, @unchecked Sendable {
        var requestCount = 0
        var lastSamples: [Double]?
        var lastSLOTarget: Double?
        var result: Result<RecoveryTelemetryAck, LoomAPIError> = .success(RecoveryTelemetryAck(accepted: true))

        func request<T: Decodable>(_ endpoint: Endpoint) async throws -> T {
            requestCount += 1
            if case let .recoveryTelemetryUpload(samples, slo) = endpoint {
                lastSamples = samples
                lastSLOTarget = slo
            }
            switch result {
            case let .success(ack):
                if let typed = ack as? T { return typed }
                throw LoomAPIError.decodingError(underlying: "unexpected type")
            case let .failure(error):
                throw error
            }
        }
    }

    @Test("Empty window is skipped without a request")
    func emptyWindowSkipped() async {
        let spy = SpyClient()
        let uploader = RecoveryTelemetryUploader(client: spy)
        let outcome = await uploader.upload(samples: [])
        #expect(outcome == .skippedEmpty)
        #expect(spy.requestCount == 0)
    }

    @Test("Successful upload posts the window and the default SLO target")
    func successPostsWindow() async {
        let spy = SpyClient()
        let uploader = RecoveryTelemetryUploader(client: spy)
        let outcome = await uploader.upload(samples: [5.0, 8.0])
        #expect(outcome == .uploaded(sampleCount: 2))
        #expect(spy.requestCount == 1)
        #expect(spy.lastSamples == [5.0, 8.0])
        #expect(spy.lastSLOTarget == ConnectionHealthMonitor.recoveryP95TargetSeconds)
    }

    @Test("Identical window is deduplicated after a successful upload")
    func duplicateWindowSkipped() async {
        let spy = SpyClient()
        let uploader = RecoveryTelemetryUploader(client: spy)
        _ = await uploader.upload(samples: [5.0, 8.0])
        let outcome = await uploader.upload(samples: [5.0, 8.0])
        #expect(outcome == .skippedDuplicate)
        #expect(spy.requestCount == 1)
    }

    @Test("A changed window uploads again")
    func changedWindowUploadsAgain() async {
        let spy = SpyClient()
        let uploader = RecoveryTelemetryUploader(client: spy)
        _ = await uploader.upload(samples: [5.0])
        let outcome = await uploader.upload(samples: [5.0, 8.0])
        #expect(outcome == .uploaded(sampleCount: 2))
        #expect(spy.requestCount == 2)
    }

    @Test("403 scope denial stops all further uploads")
    func scopeDeniedStopsRetrying() async {
        let spy = SpyClient()
        spy.result = .failure(.apiError(code: .forbidden, message: "scope not granted", requestId: "r"))
        let uploader = RecoveryTelemetryUploader(client: spy)

        let first = await uploader.upload(samples: [5.0])
        #expect(first == .scopeDenied)
        #expect(spy.requestCount == 1)

        // Even if the server would now succeed, the uploader must not retry.
        spy.result = .success(RecoveryTelemetryAck(accepted: true))
        let second = await uploader.upload(samples: [5.0, 8.0])
        #expect(second == .scopeDenied)
        #expect(spy.requestCount == 1)
    }

    @Test("429 rate-limit is transient and retried on the next window")
    func rateLimitedIsTransient() async {
        let spy = SpyClient()
        spy.result = .failure(.apiError(code: .rateLimited, message: "slow down", requestId: "r"))
        let uploader = RecoveryTelemetryUploader(client: spy)

        let first = await uploader.upload(samples: [5.0])
        #expect(first == .rateLimited)
        #expect(spy.requestCount == 1)

        // Not armed as uploaded — the same window retries when the limit clears.
        spy.result = .success(RecoveryTelemetryAck(accepted: true))
        let second = await uploader.upload(samples: [5.0])
        #expect(second == .uploaded(sampleCount: 1))
        #expect(spy.requestCount == 2)
    }

    @Test("Network error is transient and retried")
    func networkErrorIsTransient() async {
        let spy = SpyClient()
        spy.result = .failure(.networkError(underlying: "offline"))
        let uploader = RecoveryTelemetryUploader(client: spy)

        let first = await uploader.upload(samples: [5.0])
        if case .failed = first {} else { Issue.record("expected .failed, got \(first)") }

        spy.result = .success(RecoveryTelemetryAck(accepted: true))
        let second = await uploader.upload(samples: [5.0])
        #expect(second == .uploaded(sampleCount: 1))
        #expect(spy.requestCount == 2)
    }

    @Test("accepted=false is a failure and does not arm dedup")
    func notAcceptedIsFailure() async {
        let spy = SpyClient()
        spy.result = .success(RecoveryTelemetryAck(accepted: false))
        let uploader = RecoveryTelemetryUploader(client: spy)

        let first = await uploader.upload(samples: [5.0])
        if case .failed = first {} else { Issue.record("expected .failed, got \(first)") }

        spy.result = .success(RecoveryTelemetryAck(accepted: true))
        let second = await uploader.upload(samples: [5.0])
        #expect(second == .uploaded(sampleCount: 1))
        #expect(spy.requestCount == 2)
    }

    @Test("A custom SLO target is forwarded in the request")
    func customSLOTargetForwarded() async {
        let spy = SpyClient()
        let uploader = RecoveryTelemetryUploader(client: spy, sloTargetSeconds: 45)
        _ = await uploader.upload(samples: [5.0])
        #expect(spy.lastSLOTarget == 45)
    }
}
