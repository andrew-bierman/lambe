//
//  StubURLProtocol.swift
//  LambeDashTests
//
//  Isolated fake transport: a URLProtocol serving canned responses, so the
//  real URLSession pipeline (header attach, redirect following, timeouts,
//  status handling) runs with zero network access. The transport suite is
//  serialized and every static access is lock-guarded, so parallel Swift
//  Testing suites that do not use this stub cannot race it.
//

import Foundation

/// Canned response the stub serves for one request.
nonisolated struct StubResponse {
    let status: Int
    let headers: [String: String]
    let body: Data?
    /// Blocks the loader queue this long before responding (timeout tests).
    let delay: TimeInterval

    init(status: Int, headers: [String: String] = [:], body: Data? = nil, delay: TimeInterval = 0) {
        self.status = status
        self.headers = headers
        self.body = body
        self.delay = delay
    }

    /// A 200 JSON response.
    static func json(_ body: String, status: Int = 200) -> StubResponse {
        StubResponse(
            status: status,
            headers: ["Content-Type": "application/json"],
            body: Data(body.utf8)
        )
    }

    /// A 3xx redirect to `location`.
    static func redirect(to location: String, status: Int = 302) -> StubResponse {
        StubResponse(status: status, headers: ["Location": location])
    }
}

/// The fake transport behind every HTTPDaemonClient test.
nonisolated final class StubURLProtocol: URLProtocol {
    private static let gate = NSLock()
    nonisolated(unsafe) private static var handlerStorage: ((URLRequest) -> StubResponse)?
    nonisolated(unsafe) private static var capturedStorage: [URLRequest] = []

    /// Installs the handler and clears captured requests.
    static func reset(handler: ((URLRequest) -> StubResponse)?) {
        gate.withLock {
            handlerStorage = handler
            capturedStorage = []
        }
    }

    /// Requests seen by the stub, oldest first.
    static var capturedRequests: [URLRequest] {
        gate.withLock { capturedStorage }
    }

    /// How many requests the stub has seen.
    static var requestCount: Int {
        gate.withLock { capturedStorage.count }
    }

    /// The most recent request, if any.
    static var lastRequest: URLRequest? {
        gate.withLock { capturedStorage.last }
    }

    override class func canInit(with request: URLRequest) -> Bool { true }

    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        guard let stub = Self.currentHandler()?(request) else {
            client?.urlProtocol(self, didFailWithError: URLError(.badServerResponse))
            return
        }
        if stub.delay > 0 {
            Thread.sleep(forTimeInterval: stub.delay)
        }
        guard let url = request.url,
              let response = HTTPURLResponse(
                  url: url,
                  statusCode: stub.status,
                  httpVersion: "HTTP/1.1",
                  headerFields: stub.headers
              )
        else {
            client?.urlProtocol(self, didFailWithError: URLError(.badServerResponse))
            return
        }
        Self.capture(request)
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        if let body = stub.body {
            client?.urlProtocol(self, didLoad: body)
        }
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}

    private static func currentHandler() -> ((URLRequest) -> StubResponse)? {
        gate.withLock { handlerStorage }
    }

    private static func capture(_ request: URLRequest) {
        gate.withLock { capturedStorage.append(request) }
    }
}

/// Convenience: an ephemeral configuration routed through the stub.
nonisolated func stubConfiguration() -> URLSessionConfiguration {
    let configuration = URLSessionConfiguration.ephemeral
    configuration.protocolClasses = [StubURLProtocol.self]
    return configuration
}
