//
//  HTTPDaemonClientTests.swift
//  LambeDashTests
//
//  The transport over an isolated fake transport (StubURLProtocol): proves
//  GET /api/v1/state is reachable and decodes the committed §13.7 shape,
//  the bearer header is attached only to literal-loopback targets, every
//  3xx answer is refused pre-send (the redirect target is never
//  contacted), timeouts are bounded, and no error payload ever carries
//  the token or reflects untrusted text. The suite is serialized because
//  the stub protocol holds one shared handler.
//

import Testing
import Foundation

@testable import LambeDash

@Suite(.serialized)
struct HTTPDaemonClientTests {
    private let token = "test-bearer-token-0123456789abcdef"
    private let addr = "127.0.0.1:53412"

    private func makeClient(
        configuration: URLSessionConfiguration = stubConfiguration(),
        requestTimeout: TimeInterval = 2
    ) throws -> HTTPDaemonClient {
        try HTTPDaemonClient(
            endpoint: LoopbackEndpoint(addr: addr)!,
            bearerToken: token,
            requestTimeout: requestTimeout,
            configuration: configuration
        )
    }

    @Test("GET /api/v1/state is reachable over the fake transport")
    func stateRouteIsReachable() async throws {
        StubURLProtocol.reset(handler: { request in
            guard request.url?.path == "/api/v1/state" else {
                return .json(#"{"error":"not_found"}"#, status: 404)
            }
            return .json(SnapshotDocuments.withRuns)
        })

        let client = try makeClient()
        let probe = try await client.get("/api/v1/state", as: StateProbe.self)
        #expect(probe.schemaVersion == 1)

        let request = try #require(StubURLProtocol.lastRequest)
        #expect(request.httpMethod == "GET")
        #expect(request.url?.absoluteString == "http://127.0.0.1:53412/api/v1/state")
        #expect(request.url?.host == "127.0.0.1")
        #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer \(token)")
        #expect(request.value(forHTTPHeaderField: "Cookie") == nil)

        // An empty daemon snapshot decodes the same way: sections empty,
        // never a decode failure.
        StubURLProtocol.reset(handler: { _ in .json(SnapshotDocuments.empty) })
        let empty = try await client.get("/api/v1/state", as: StateProbe.self)
        #expect(empty.schemaVersion == 1)
    }

    @Test("a descriptor-built client reaches the state route end to end")
    func descriptorBuiltClientReachesState() async throws {
        StubURLProtocol.reset(handler: { _ in .json(SnapshotDocuments.withRuns) })
        let descriptor = try StateAPIDescriptor.decode(
            #require(DescriptorDocuments.canonical.data(using: .utf8))
        )
        let client = try HTTPDaemonClient(
            descriptor: descriptor,
            configuration: stubConfiguration()
        )
        #expect(client.endpoint.port == 53412)
        let probe = try await client.get("/api/v1/state", as: StateProbe.self)
        #expect(probe.schemaVersion == 1)
    }

    /// Every redirect class a loopback peer could answer with. The
    /// boundary: the session delegate refuses the follow before a second
    /// request exists, and the client refuses the 3xx answer itself — so
    /// the target is never contacted and the Authorization header is
    /// never replayed to it.
    @Test(
        "every 3xx answer is refused pre-send and its target is never contacted",
        arguments: [300, 301, 302, 303, 307, 308]
    )
    func redirectsAreRefusedPreSend(status: Int) async throws {
        StubURLProtocol.reset(handler: { request in
            if request.url?.path == "/api/v1/state" {
                return .redirect(to: "http://127.0.0.1:53412/api/v1/elsewhere", status: status)
            }
            // If the redirect were ever followed, this answers — and the
            // session would have replayed the Authorization header to it.
            return .json(#"{"schema_version":1}"#)
        })

        let client = try makeClient()
        await #expect(throws: APIError.redirectRefused) {
            try await client.get("/api/v1/state", as: StateProbe.self)
        }
        // One request seen — the 3xx itself. Nothing reached the redirect
        // target, so the token never left the loopback route.
        #expect(StubURLProtocol.requestCount == 1)
        #expect(
            StubURLProtocol.capturedRequests.allSatisfy { $0.url?.path == "/api/v1/state" }
        )
    }

    @Test("non-2xx statuses become typed errors that exclude the body")
    func statusErrorsExcludeTheBody() async throws {
        // A hostile loopback server reflects the Authorization header back
        // in its 401 body; the error must not carry it.
        StubURLProtocol.reset(handler: { _ in
            .json("denied; echoed \(self.token)", status: 401)
        })
        let client = try makeClient()
        do {
            _ = try await client.get("/api/v1/state", as: StateProbe.self)
            Issue.record("expected a status error")
        } catch let error as APIError {
            #expect(error == .status(code: 401))
            #expect(String(describing: error).contains(token) == false)
            #expect(String(reflecting: error).contains(token) == false)
        }
    }

    @Test("timeouts are bounded and their errors carry no token and no request text")
    func timeoutsAreBounded() async throws {
        StubURLProtocol.reset(handler: { _ in
            StubResponse(status: 200, body: Data("{}".utf8), delay: 0.6)
        })
        let client = try makeClient(requestTimeout: 0.15)
        let startedAt = Date()
        do {
            _ = try await client.get("/api/v1/state", as: StateProbe.self)
            Issue.record("expected a timeout")
        } catch let error as APIError {
            guard case .transport(let code) = error else {
                Issue.record("expected a transport error, got \(error)")
                return
            }
            #expect(code == .timedOut)
            // The payload is the machine code only: it names neither the
            // token nor the request target.
            #expect(String(describing: error).contains(token) == false)
            #expect(String(describing: error).contains("127.0.0.1:53412") == false)
        }
        #expect(Date().timeIntervalSince(startedAt) < 2)
    }

    @Test("decode failures become typed errors")
    func decodeFailuresAreTyped() async throws {
        StubURLProtocol.reset(handler: { _ in .json("<html>not json</html>") })
        let client = try makeClient()
        do {
            _ = try await client.get("/api/v1/state", as: StateProbe.self)
            Issue.record("expected a decoding error")
        } catch let error as APIError {
            #expect(error == .decoding)
            #expect(String(describing: error).contains(token) == false)
        }
    }

    @Test("paths outside the versioned route table are refused before any request")
    func pathsOutsideTheRouteTableAreRefused() async throws {
        StubURLProtocol.reset(handler: { _ in .json(#"{"schema_version":1}"#) })
        let client = try makeClient()
        // The rejected path is caller-supplied text: the typed error does
        // not reflect it.
        await #expect(throws: APIError.invalidPath) {
            try await client.get("/etc/passwd", as: StateProbe.self)
        }
        await #expect(throws: APIError.invalidPath) {
            try await client.get("/", as: StateProbe.self)
        }
        #expect(StubURLProtocol.requestCount == 0)
    }

    @Test("an off-host descriptor is refused before any request exists")
    func offHostDescriptorsAreRefused() async throws {
        StubURLProtocol.reset(handler: { _ in .json(#"{"schema_version":1}"#) })
        let descriptor = try StateAPIDescriptor.decode(
            #require(DescriptorDocuments.json(addr: "10.0.0.9:8080").data(using: .utf8))
        )
        #expect(throws: APIError.invalidEndpoint) {
            try HTTPDaemonClient(descriptor: descriptor, configuration: stubConfiguration())
        }
        #expect(StubURLProtocol.requestCount == 0)
    }

    @Test("the hardened configuration is applied to injected configurations")
    func hardeningIsAppliedToInjectedConfigurations() throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [StubURLProtocol.self]
        _ = try HTTPDaemonClient(
            endpoint: LoopbackEndpoint(addr: addr)!,
            bearerToken: token,
            requestTimeout: 0.25,
            resourceTimeout: 1.5,
            configuration: configuration
        )
        #expect(configuration.timeoutIntervalForRequest == 0.25)
        #expect(configuration.timeoutIntervalForResource == 1.5)
        #expect(configuration.httpShouldSetCookies == false)
        #expect(configuration.httpCookieAcceptPolicy == .never)
        #expect(configuration.connectionProxyDictionary?.isEmpty == true)
        #expect(configuration.requestCachePolicy == .reloadIgnoringLocalCacheData)
        #expect(configuration.urlCache == nil)
        #expect(configuration.waitsForConnectivity == false)
    }

    @Test("an empty token is refused before anything is built")
    func emptyTokensAreRefused() throws {
        StubURLProtocol.reset(handler: { _ in .json(#"{"schema_version":1}"#) })
        #expect(throws: APIError.descriptorInvalid(reason: "bearer token missing")) {
            try HTTPDaemonClient(
                endpoint: LoopbackEndpoint(addr: addr)!,
                bearerToken: "",
                configuration: stubConfiguration()
            )
        }
        #expect(StubURLProtocol.requestCount == 0)
    }
}
