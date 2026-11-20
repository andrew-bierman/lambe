//
//  HTTPDaemonClient.swift
//  LambeDash
//
//  Typed loopback HTTP transport for the daemon state API (KTD1, KTD5,
//  R15). GET-only against `/api/v1/` routes. There is exactly one way to
//  build a client, and the hardening lives inside it, applied before any
//  request can exist:
//
//  - The bearer token is attached only to URLs built from a
//    `LoopbackEndpoint`, so it is structurally impossible to send it to a
//    host other than literal `127.0.0.1`.
//  - The session is built here, around a delegate that refuses every
//    redirection before a second request is issued — the Authorization
//    header can never be replayed to a redirect target. No initializer
//    accepts a caller-built session, so no client can exist without the
//    delegate: a redirect can never be followed, not merely detected
//    after the fact.
//  - Any 3xx-class answer is refused unconditionally on the response:
//    followed or not, the only safe redirect is none.
//  - Cookies, proxies, and caching are disabled; timeouts are short and
//    fail fast (`waitsForConnectivity` stays off).
//  - No `APIError` payload ever carries the token or reflects untrusted
//    text (see `APIError`).
//
//  The `configuration:` parameter exists so tests can install a stub URL
//  protocol; the hardening above is applied to it either way.
//
//  The typed Queue/Attention/History calls of the U7 data layer wrap
//  `get(_:as:)` above the U2 wire shapes; this subset stays generic so it
//  lands without them.
//

import Foundation

/// Typed generic GET client over the daemon's loopback state API.
nonisolated final class HTTPDaemonClient: Sendable {
    /// Fail-fast per-request timeout: a wedged or dead daemon must not
    /// stall the poll loop (R15).
    static let defaultRequestTimeout: TimeInterval = 2
    /// Overall budget for one call.
    static let defaultResourceTimeout: TimeInterval = 5

    /// The validated loopback endpoint every request targets.
    let endpoint: LoopbackEndpoint

    private let bearerToken: String
    private let session: URLSession

    /// The only initializer. Applies the loopback transport hardening to
    /// `configuration` (short timeouts, no cookies, no proxy, no cache,
    /// fail-fast connectivity) and builds the session around the
    /// redirect-refusing delegate.
    ///
    /// - Throws: `APIError.descriptorInvalid` when the token is empty —
    ///   before any session or request exists.
    init(
        endpoint: LoopbackEndpoint,
        bearerToken: String,
        requestTimeout: TimeInterval = HTTPDaemonClient.defaultRequestTimeout,
        resourceTimeout: TimeInterval = HTTPDaemonClient.defaultResourceTimeout,
        configuration: URLSessionConfiguration = .ephemeral
    ) throws {
        guard !bearerToken.isEmpty else {
            throw APIError.descriptorInvalid(reason: "bearer token missing")
        }
        configuration.timeoutIntervalForRequest = requestTimeout
        configuration.timeoutIntervalForResource = resourceTimeout
        configuration.httpShouldSetCookies = false
        configuration.httpCookieAcceptPolicy = .never
        configuration.connectionProxyDictionary = [:]
        configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
        configuration.urlCache = nil
        configuration.waitsForConnectivity = false
        self.endpoint = endpoint
        self.bearerToken = bearerToken
        self.session = URLSession(
            configuration: configuration,
            delegate: RedirectRefusingDelegate.shared,
            delegateQueue: nil
        )
    }

    /// Builds a client from a decoded descriptor. Admission runs first
    /// (schema version, pid, header-safe token, literal loopback addr), so
    /// a malicious descriptor can never move the token or the request.
    ///
    /// - Throws: `APIError` from `StateAPIDescriptor.validatedEndpoint()`
    ///   or from an empty token.
    convenience init(
        descriptor: StateAPIDescriptor,
        requestTimeout: TimeInterval = HTTPDaemonClient.defaultRequestTimeout,
        resourceTimeout: TimeInterval = HTTPDaemonClient.defaultResourceTimeout,
        configuration: URLSessionConfiguration = .ephemeral
    ) throws {
        let endpoint = try descriptor.validatedEndpoint()
        try self.init(
            endpoint: endpoint,
            bearerToken: descriptor.bearerToken,
            requestTimeout: requestTimeout,
            resourceTimeout: resourceTimeout,
            configuration: configuration
        )
    }

    deinit {
        // URLSession retains its delegate until invalidated; ending the
        // session here keeps it from outliving the client.
        session.finishTasksAndInvalidate()
    }

    /// Performs a GET against a versioned route and decodes the 2xx body.
    /// Throws `APIError` — never a raw URL-loading error, and never a
    /// payload containing the token.
    func get<Response: Decodable>(_ path: String, as type: Response.Type) async throws -> Response {
        guard let url = endpoint.routeURL(path) else {
            throw APIError.invalidPath
        }
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        request.cachePolicy = .reloadIgnoringLocalCacheData
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        // The only place the token exists on the wire: this header, on a
        // URL LoopbackEndpoint already pinned to 127.0.0.1.
        request.setValue("Bearer \(bearerToken)", forHTTPHeaderField: "Authorization")

        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.data(for: request)
        } catch {
            // Code-only failure fact: the machine code, never the loading
            // system's user-facing message, which can quote the failed
            // request.
            throw APIError.transport((error as? URLError)?.code ?? .unknown)
        }
        guard let http = response as? HTTPURLResponse else {
            throw APIError.unexpectedResponse
        }
        // A redirect-class answer is refused whether or not the session
        // tried to follow it (R15): the only safe redirect is none, and a
        // followed one would replay the Authorization header to its
        // target. The delegate has already refused the follow itself, so
        // in practice this sees the un-followed 3xx.
        if (300..<400).contains(http.statusCode) {
            throw APIError.redirectRefused
        }
        guard (200..<300).contains(http.statusCode) else {
            throw APIError.status(code: http.statusCode)
        }
        do {
            return try JSONDecoder().decode(Response.self, from: data)
        } catch {
            throw APIError.decoding
        }
    }
}

/// Session delegate that refuses every redirection before the second
/// request is issued, so the Authorization header can never be replayed
/// to a redirect target (R15). Stateless: handing back `nil` makes the
/// session return the 3xx response itself, which the client refuses by
/// status — no refusal record is needed.
nonisolated final class RedirectRefusingDelegate: NSObject, URLSessionTaskDelegate, Sendable {
    /// One stateless instance serves every client's session.
    static let shared = RedirectRefusingDelegate()

    private override init() {
        super.init()
    }

    func urlSession(
        _ session: URLSession,
        task: URLSessionTask,
        willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest,
        completionHandler: @escaping (URLRequest?) -> Void
    ) {
        completionHandler(nil)
    }
}
