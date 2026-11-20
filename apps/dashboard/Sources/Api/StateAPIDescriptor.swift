//
//  StateAPIDescriptor.swift
//  LambeDash
//
//  The per-instance discovery document an enabled daemon publishes as
//  `stateapi-<instance>.json` under the state dir's live directory (U1,
//  KTD3). Fields mirror the committed Go writer one-for-one
//  (`internal/daemon/state.go`, `StateAPIDescriptor`): decode is tolerant
//  of unknown keys and absent omitempty fields, while structural admission
//  (`validatedEndpoint`) is strict so a crafted descriptor can never point
//  the client anywhere but the daemon's literal loopback address.
//

import Foundation

/// Decodes the U1 endpoint descriptor. The document carries the bearer
/// token, which is why admission is strict before any client is built.
nonisolated struct StateAPIDescriptor: Decodable, Equatable, Sendable {
    /// The descriptor document's own schema version this client
    /// understands (`StateAPIDescriptorSchemaVersion` on the Go side).
    /// The HTTP wire schema version is a separate, unrelated number.
    static let supportedSchemaVersion = 1

    /// The descriptor document's schema version.
    let schemaVersion: Int
    /// The daemon instance name (its lock name without the `.lock` suffix).
    let instance: String
    /// The bound `host:port` the state API answers on.
    let addr: String
    /// The per-start bearer token. Lives here and nowhere else: never in
    /// logs or error strings.
    let bearerToken: String
    /// The daemon's process id; 0 is never valid.
    let pid: Int
    /// The process incarnation, when the platform provides one (absent
    /// otherwise — the field is omitempty on the writer).
    let identity: String?
    /// The daemon build version.
    let version: String
    /// The configuration file the daemon loaded; absent when it ran
    /// without one (omitempty).
    let configPath: String?
    /// When the daemon started, RFC 3339 in UTC.
    let startedAt: String

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case instance
        case addr
        case bearerToken = "bearer_token"
        case pid
        case identity
        case version
        case configPath = "config_path"
        case startedAt = "started_at"
    }

    /// Decodes descriptor bytes. Unknown keys are ignored; absent omitempty
    /// fields decode as nil. Wrong JSON types throw — callers skip the
    /// document.
    static func decode(_ data: Data) throws -> StateAPIDescriptor {
        try JSONDecoder().decode(StateAPIDescriptor.self, from: data)
    }

    /// Structural admission: the schema version is supported, the pid is
    /// plausible, the token is present and header-safe, and the address
    /// parses as a literal loopback endpoint. Throws `APIError` with
    /// static or code-only payloads on every failure path — the
    /// descriptor's own strings are never reflected.
    ///
    /// The whitespace check on the token is a header-injection defense:
    /// the token is the only descriptor string that rides in a request
    /// header, and a CR/LF/space would let a crafted file forge or split
    /// headers.
    func validatedEndpoint() throws -> LoopbackEndpoint {
        guard schemaVersion == Self.supportedSchemaVersion else {
            throw APIError.unsupportedDescriptorSchema(found: schemaVersion)
        }
        guard pid > 0 else {
            throw APIError.descriptorInvalid(reason: "pid must be positive")
        }
        guard !bearerToken.isEmpty else {
            throw APIError.descriptorInvalid(reason: "bearer token missing")
        }
        guard !bearerToken.contains(where: { "\r\n\t ".contains($0) }) else {
            throw APIError.descriptorInvalid(reason: "bearer token contains whitespace")
        }
        guard let endpoint = LoopbackEndpoint(addr: addr) else {
            // The address is attacker-controlled descriptor text; the
            // error names the failure, not the text.
            throw APIError.invalidEndpoint
        }
        return endpoint
    }
}
