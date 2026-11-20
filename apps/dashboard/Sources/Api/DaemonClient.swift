//
//  DaemonClient.swift
//  LambeDash
//
//  The typed seam between the connection layer and the loopback transport
//  (KTD7): one protocol, one real implementation over `HTTPDaemonClient`.
//  Models depend on the protocol, so tests can stub fetch outcomes without
//  a network. GET-only — the app never calls the mutating refresh route
//  (R13).
//

import Foundation

/// Typed reads over the daemon state API. Implementations must be safe to
/// call concurrently from the single poll loop and the detail fetch.
nonisolated protocol DaemonClient: Sendable {
    /// GET /api/v1/state.
    func state() async throws -> WireSnapshot
    /// GET /api/v1/queue.
    func queue() async throws -> WireQueue
    /// GET /api/v1/history.
    func history() async throws -> WireHistory
    /// GET /api/v1/{issue}.
    func issue(_ id: String) async throws -> WireIssue
}

extension HTTPDaemonClient: DaemonClient {
    func state() async throws -> WireSnapshot {
        try await get("/api/v1/state", as: WireSnapshot.self)
    }

    func queue() async throws -> WireQueue {
        try await get("/api/v1/queue", as: WireQueue.self)
    }

    func history() async throws -> WireHistory {
        try await get("/api/v1/history", as: WireHistory.self)
    }

    func issue(_ id: String) async throws -> WireIssue {
        guard let path = IssueRoute.path(for: id) else {
            throw APIError.invalidPath
        }
        return try await get(path, as: WireIssue.self)
    }
}

/// Builds the `/api/v1/{issue}` route path for an issue identifier. Issue
/// ids come from the daemon, but they flow into a URL path, so admission
/// is conservative: plain ASCII slug characters only, no leading dot, no
/// "." / "..", bounded length. Anything else is refused before a request
/// exists — `LoopbackEndpoint.routeURL` and the percent-encoding path
/// setter never see an adversarial segment.
nonisolated enum IssueRoute {
    /// The set of characters an issue id may contain.
    private static let allowed = Set(
        "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_."
    )
    /// Issue ids are short slugs; anything longer is not one.
    private static let maxLength = 128

    /// The route path, or nil when the id fails admission.
    static func path(for id: String) -> String? {
        guard !id.isEmpty, id.count <= maxLength else { return nil }
        guard id.unicodeScalars.allSatisfy({ allowed.contains(Character($0)) }) else {
            return nil
        }
        guard id != "." && id != "..", !id.hasPrefix(".") else { return nil }
        return "/api/v1/" + id
    }
}

extension APIError {
    /// A short, operator-facing description. By `APIError`'s payload
    /// discipline every case renders from fixed text or bare numbers —
    /// the bearer token can never reach the UI through this path, and no
    /// untrusted string is ever reflected (R15).
    var displayText: String {
        switch self {
        case .invalidEndpoint, .invalidPath:
            "Invalid endpoint"
        case .descriptorInvalid:
            "Endpoint descriptor rejected"
        case .unsupportedDescriptorSchema:
            "Descriptor schema not supported by this app"
        case .redirectRefused:
            "Daemon issued a redirect — refused"
        case .transport(.cancelled):
            "Cancelled"
        case .transport(.cannotConnectToHost), .transport(.cannotFindHost):
            "Daemon not reachable"
        case .transport(.timedOut):
            "Timed out"
        case .transport:
            "Connection failed"
        case .unexpectedResponse:
            "Unexpected response"
        case .status(401):
            "Unauthorized — the daemon token changed"
        case .status(404):
            "Not found"
        case .status(let code):
            "Daemon error (HTTP \(code))"
        case .decoding:
            "Unexpected data format"
        }
    }
}
