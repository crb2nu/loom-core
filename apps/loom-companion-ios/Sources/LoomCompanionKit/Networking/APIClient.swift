import Foundation

/// Protocol for testability — ViewModels depend on this, not the concrete client.
public protocol LoomAPIClientProtocol: Sendable {
    func request<T: Decodable>(_ endpoint: Endpoint) async throws -> T

    /// Decode a *bare* (non-enveloped) JSON body. The mobile v1 API wraps
    /// responses in `APIEnvelope`, but the `/api/mills/*` and `/api/weaver/*`
    /// surfaces are reverse-proxied straight from their operators and return
    /// raw JSON (a bare array or object). Those callers must use this path —
    /// routing them through `request(_:)` fails envelope decoding on every
    /// response. Dates are parsed as ISO-8601 (Go RFC3339), not the default
    /// `JSONDecoder` epoch-number strategy.
    func requestRaw<T: Decodable>(_ endpoint: Endpoint) async throws -> T
}

public extension LoomAPIClientProtocol {
    /// Default keeps existing conformances (and test fakes) source-compatible
    /// while they adopt the raw path; the concrete `APIClient` overrides it.
    func requestRaw<T: Decodable>(_ endpoint: Endpoint) async throws -> T {
        try await request(endpoint)
    }
}

/// URLSession-based REST client for the Loom mobile v1 API.
public final class APIClient: LoomAPIClientProtocol, Sendable {
    private let session: URLSession
    private let baseURL: URL
    private let token: String
    /// HUD admin token (HUD_ADMIN_TOKEN), separate from the pairing bearer.
    /// Attached as `X-Admin-Token` ONLY on admin-gated endpoints
    /// (`Endpoint.requiresAdminToken`) so Mills mutations clear the HUD gate.
    /// Empty when the operator hasn't set one — mutations then 401 with the
    /// existing actionable copy (`millsMutationFailureMessage`).
    private let adminToken: String
    private let deviceId: String
    private let cloudflareAccessClientID: String?
    private let cloudflareAccessClientSecret: String?

    /// Delegate that accepts self-signed TLS certificates for LAN mode.
    /// Must be retained for the lifetime of the URLSession.
    private let sessionDelegate: InsecureTLSDelegate?

    public init(
        baseURL: URL,
        token: String,
        adminToken: String = "",
        deviceId: String = "",
        cloudflareAccessClientID: String? = nil,
        cloudflareAccessClientSecret: String? = nil,
        allowsInsecureTLS: Bool = false,
        session: URLSession? = nil
    ) {
        self.baseURL = baseURL
        self.token = token
        self.adminToken = adminToken.trimmingCharacters(in: .whitespacesAndNewlines)
        self.deviceId = deviceId
        self.cloudflareAccessClientID = cloudflareAccessClientID?.trimmingCharacters(in: .whitespacesAndNewlines)
        self.cloudflareAccessClientSecret = cloudflareAccessClientSecret?.trimmingCharacters(in: .whitespacesAndNewlines)

        if let session {
            self.session = session
            self.sessionDelegate = nil
        } else {
            let config = URLSessionConfiguration.default
            config.timeoutIntervalForRequest = 15
            config.timeoutIntervalForResource = 30
            if allowsInsecureTLS {
                let delegate = InsecureTLSDelegate()
                self.sessionDelegate = delegate
                self.session = URLSession(configuration: config, delegate: delegate, delegateQueue: nil)
            } else {
                self.sessionDelegate = nil
                self.session = URLSession(configuration: config)
            }
        }
    }

    public func request<T: Decodable>(_ endpoint: Endpoint) async throws -> T {
        let (data, status) = try await performRequest(endpoint)
        return try decodeResponse(data, statusCode: status)
    }

    public func requestRaw<T: Decodable>(_ endpoint: Endpoint) async throws -> T {
        let (data, status) = try await performRequest(endpoint)
        return try decodeRaw(data, statusCode: status)
    }

    /// Decode a bare (non-enveloped) JSON body. 2xx decodes `T` directly; any
    /// other status maps through `mapHTTPError` so operator error envelopes
    /// (`not_configured`, `not_found`) reach callers with the right code.
    /// Separated from `requestRaw` so it can be unit-tested against real bytes,
    /// mirroring `decodeResponse`.
    func decodeRaw<T: Decodable>(_ data: Data, statusCode: Int) throws -> T {
        guard (200 ..< 300).contains(statusCode) else {
            throw mapHTTPError(status: statusCode, data: data)
        }
        do {
            return try Self.rawJSONDecoder.decode(T.self, from: data)
        } catch {
            throw LoomAPIError.decodingError(
                underlying: "Invalid raw response (HTTP \(statusCode)): \(Self.decodingErrorDetail(error))"
            )
        }
    }

    /// Issue the request and return the body + HTTP status, mapping transport
    /// failures to `LoomAPIError.networkError`. Shared by the enveloped and raw
    /// decode paths.
    /// Build the fully-decorated URLRequest for an endpoint: base request +
    /// auth headers + (for admin-gated endpoints) the `X-Admin-Token`. Extracted
    /// from `performRequest` so header attachment is unit-testable without a
    /// network round-trip (`@testable`).
    func authorizedRequest(for endpoint: Endpoint) throws -> URLRequest {
        var urlRequest = try endpoint.urlRequest(baseURL: baseURL)
        applyAuthHeaders(to: &urlRequest)
        // Admin-gated Mills mutations carry the HUD admin token separately from
        // the pairing bearer. Only attach it where the gate reads it, so the
        // admin secret never rides on the /api/mobile/v1 surface.
        if endpoint.requiresAdminToken, !adminToken.isEmpty {
            urlRequest.setValue(adminToken, forHTTPHeaderField: "X-Admin-Token")
        }
        return urlRequest
    }

    private func performRequest(_ endpoint: Endpoint) async throws -> (Data, Int) {
        let urlRequest = try authorizedRequest(for: endpoint)

        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.data(for: urlRequest)
        } catch {
            throw LoomAPIError.networkError(underlying: error.localizedDescription)
        }

        guard let httpResponse = response as? HTTPURLResponse else {
            throw LoomAPIError.networkError(underlying: "Non-HTTP response")
        }

        return (data, httpResponse.statusCode)
    }

    /// Decoder for bare operator JSON. Go's `encoding/json` emits `time.Time`
    /// as RFC3339 strings, so we parse dates with the shared `LoomFormat`
    /// ISO parser (handles fractional + plain seconds) rather than the default
    /// epoch-number strategy, which would throw on every timestamped record.
    private static let rawJSONDecoder: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { d in
            let container = try d.singleValueContainer()
            let raw = try container.decode(String.self)
            if let date = LoomFormat.date(fromISO: raw) { return date }
            throw DecodingError.dataCorruptedError(
                in: container,
                debugDescription: "Expected ISO-8601 date, got \(raw)"
            )
        }
        return decoder
    }()

    // MARK: - Spawn Telemetry Convenience Methods
    //
    // These wrap `request(_:)` with typed response shapes for the spawn
    // telemetry endpoints introduced in Wave 2. They keep SpawnDetailView
    // and SpawnViewModel free of boilerplate envelope decoding while still
    // respecting the shared `LoomAPIClientProtocol.request` contract.

    /// Fetch accumulated SDK telemetry for a spawn. Returns `nil` when the
    /// spawn exists but has not produced telemetry yet (e.g. gemini).
    public func spawnTelemetry(id: String) async throws -> SpawnTelemetry? {
        let response: SpawnTelemetryResponse = try await request(.spawnTelemetry(id: id))
        return response.telemetry
    }

    /// Fetch a page of tool calls captured for a spawn.
    public func spawnTelemetryTools(
        id: String,
        offset: Int? = nil,
        limit: Int? = nil
    ) async throws -> SpawnTelemetryToolsPage {
        try await request(.spawnTelemetryTools(id: id, offset: offset, limit: limit))
    }

    /// Fetch a page of file changes captured for a spawn.
    public func spawnTelemetryFiles(
        id: String,
        offset: Int? = nil,
        limit: Int? = nil
    ) async throws -> SpawnTelemetryFilesPage {
        try await request(.spawnTelemetryFiles(id: id, offset: offset, limit: limit))
    }

    /// Fetch a page of agent errors captured for a spawn.
    public func spawnTelemetryErrors(
        id: String,
        offset: Int? = nil,
        limit: Int? = nil
    ) async throws -> SpawnTelemetryErrorsPage {
        try await request(.spawnTelemetryErrors(id: id, offset: offset, limit: limit))
    }

    /// Forward a follow-up user message to a running multi-turn spawn.
    /// The HUD responds with a 202 envelope containing the ack shape.
    @discardableResult
    public func sendSpawnMessage(spawnID: String, text: String) async throws -> SpawnControlResponse {
        try await request(.spawnSendMessage(id: spawnID, text: text))
    }

    /// Abort the in-flight turn of a running multi-turn spawn. The HUD
    /// responds with a 202 envelope containing the ack shape.
    @discardableResult
    public func interruptSpawn(spawnID: String) async throws -> SpawnControlResponse {
        try await request(.spawnInterrupt(id: spawnID))
    }

    /// Decode and validate the standard mobile API envelope contract.
    ///
    /// Contract expectations:
    /// - Body is an `APIEnvelope<T>`
    /// - `ok == false` carries structured `error`
    /// - `ok == true` includes non-nil `data`
    /// - 2xx with invalid/missing envelope data is a decoding contract error
    func decodeResponse<T: Decodable>(_ data: Data, statusCode: Int) throws -> T {
        let envelope: APIEnvelope<T>
        do {
            envelope = try JSONDecoder().decode(APIEnvelope<T>.self, from: data)
        } catch {
            // If we can't decode a successful payload, surface a contract error
            // with the actual decode error for diagnostics.
            if (200 ..< 300).contains(statusCode) {
                throw LoomAPIError.decodingError(underlying: "Invalid API response contract (HTTP \(statusCode)): \(Self.decodingErrorDetail(error))")
            }
            // For non-2xx responses, map HTTP status to structured API errors.
            throw mapHTTPError(status: statusCode, data: data)
        }

        if !envelope.ok {
            guard let envelopeError = envelope.error else {
                if (200 ..< 300).contains(statusCode) {
                    throw LoomAPIError.decodingError(underlying: "Invalid API response contract (HTTP \(statusCode)): missing error payload")
                }
                throw mapHTTPError(status: statusCode, data: data)
            }
            let errorCode = APIErrorCode(rawValue: envelopeError.code) ?? .unknown
            throw LoomAPIError.apiError(
                code: errorCode,
                message: envelopeError.message,
                requestId: envelope.meta.requestId
            )
        }

        guard let result = envelope.data else {
            throw LoomAPIError.decodingError(underlying: "Missing data payload in successful API response (HTTP \(statusCode))")
        }

        return result
    }

    /// Build an SSE request (used by SSEClient).
    public func sseRequest() throws -> URLRequest {
        var request = try Endpoint.eventsStream.urlRequest(baseURL: baseURL)
        applyAuthHeaders(to: &request)
        request.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        request.timeoutInterval = 0 // No timeout for SSE
        return request
    }

    /// Build a URLSession suitable for SSE streaming (no request timeout).
    /// Inherits TLS trust settings from this client.
    public func sseSession() -> URLSession {
        let config = URLSessionConfiguration.default
        config.timeoutIntervalForRequest = 0
        config.timeoutIntervalForResource = 0
        if let delegate = sessionDelegate {
            return URLSession(configuration: config, delegate: delegate, delegateQueue: nil)
        }
        return URLSession(configuration: config)
    }

    private func applyAuthHeaders(to request: inout URLRequest) {
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if !deviceId.isEmpty {
            request.setValue(deviceId, forHTTPHeaderField: "X-Device-ID")
        }
        if let id = cloudflareAccessClientID,
           let secret = cloudflareAccessClientSecret,
           !id.isEmpty,
           !secret.isEmpty
        {
            request.setValue(id, forHTTPHeaderField: "CF-Access-Client-Id")
            request.setValue(secret, forHTTPHeaderField: "CF-Access-Client-Secret")
        }
    }

    /// Extract a human-readable detail string from a DecodingError including the key path.
    static func decodingErrorDetail(_ error: Error) -> String {
        guard let decodingError = error as? DecodingError else {
            return error.localizedDescription
        }
        switch decodingError {
        case let .keyNotFound(key, context):
            let path = context.codingPath.map(\.stringValue).joined(separator: ".")
            return "missing key '\(key.stringValue)' at path '\(path)'"
        case let .typeMismatch(type, context):
            let path = context.codingPath.map(\.stringValue).joined(separator: ".")
            return "type mismatch for \(type) at path '\(path)': \(context.debugDescription)"
        case let .valueNotFound(type, context):
            let path = context.codingPath.map(\.stringValue).joined(separator: ".")
            return "null value for \(type) at path '\(path)'"
        case let .dataCorrupted(context):
            let path = context.codingPath.map(\.stringValue).joined(separator: ".")
            return "corrupted data at path '\(path)': \(context.debugDescription)"
        @unknown default:
            return error.localizedDescription
        }
    }

    private func mapHTTPError(status: Int, data: Data) -> LoomAPIError {
        // Try to parse just the error body
        if let envelope = try? JSONDecoder().decode(APIEnvelope<EmptyData>.self, from: data),
           let error = envelope.error
        {
            let code = APIErrorCode(rawValue: error.code) ?? .unknown
            return .apiError(code: code, message: error.message, requestId: envelope.meta.requestId)
        }

        // Fallback to HTTP status
        switch status {
        case 400: return .apiError(code: .badRequest, message: "Bad request", requestId: "")
        case 401: return .apiError(code: .unauthorized, message: "Unauthorized", requestId: "")
        case 403: return .apiError(code: .forbidden, message: "Forbidden", requestId: "")
        case 404: return .apiError(code: .notFound, message: "Not found", requestId: "")
        case 429: return .apiError(code: .rateLimited, message: "Rate limited", requestId: "")
        case 500, 502, 503, 504:
            return .apiError(code: .upstreamError, message: "Upstream service error (HTTP \(status))", requestId: "")
        default: return .networkError(underlying: "HTTP \(status)")
        }
    }
}

/// Empty decodable for error-only envelopes.
private struct EmptyData: Decodable {}

/// URLSession delegate that accepts self-signed TLS certificates.
/// Used in LAN mode where the HUD serves HTTPS with a self-signed cert.
final class InsecureTLSDelegate: NSObject, URLSessionDelegate, Sendable {
    func urlSession(
        _ session: URLSession,
        didReceive challenge: URLAuthenticationChallenge,
        completionHandler: @escaping (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
    ) {
        guard challenge.protectionSpace.authenticationMethod == NSURLAuthenticationMethodServerTrust,
              let serverTrust = challenge.protectionSpace.serverTrust
        else {
            completionHandler(.performDefaultHandling, nil)
            return
        }
        completionHandler(.useCredential, URLCredential(trust: serverTrust))
    }
}
