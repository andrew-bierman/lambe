//
//  DescriptorStore.swift
//  LambeDash
//
//  Read-only enumeration of the per-instance endpoint descriptors in the
//  state dir's live directory (KTD3, R7). The client never writes or
//  deletes: only the owning daemon manages its descriptor file. Entries
//  that are unreadable, oversized, malformed, or fail structural admission
//  are skipped, not surfaced, so a crafted file cannot crash or misdirect
//  resolution.
//
//  Staleness policy — pid liveness and identity match against the attached
//  daemon — is the connection layer's job (it observes identity in every
//  response); this layer only admits structurally plausible documents and
//  exposes the facts the policy needs.
//

import Foundation

/// One admitted endpoint discovered on disk: a structurally valid
/// descriptor plus its pre-validated loopback endpoint.
nonisolated struct ResolvedEndpoint: Equatable, Sendable {
    /// The full decoded descriptor (instance, pid, identity, version,
    /// started-at — the facts the connection layer's staleness policy
    /// needs).
    let descriptor: StateAPIDescriptor
    /// The validated `127.0.0.1:<port>` endpoint to connect to.
    let endpoint: LoopbackEndpoint

    /// Convenience passthroughs: the token-free facts the connection
    /// layer's staleness policy compares, plus the token the client needs.
    var instance: String { descriptor.instance }
    var pid: Int { descriptor.pid }
    var identity: String? { descriptor.identity }
    var bearerToken: String { descriptor.bearerToken }
}

/// Enumerates `stateapi-*.json` descriptors under one live directory.
/// Reads are bounded (regular files only, symlinks refused, size capped
/// before and after reading) and the result is deterministic (sorted by
/// instance name, then address).
nonisolated struct DescriptorStore: Sendable {
    /// A descriptor is a few hundred bytes; anything larger is not one and
    /// is skipped before being read into memory.
    static let maxDescriptorBytes = 16 * 1024

    /// The `<stateDir>/live` directory holding `stateapi-*.json`.
    let liveDirectory: URL

    /// Enumerates and admits descriptors. An empty or missing directory
    /// yields an empty list — never an error.
    func resolvedEndpoints() -> [ResolvedEndpoint] {
        let entries = (try? FileManager.default.contentsOfDirectory(
            at: liveDirectory,
            includingPropertiesForKeys: [.isRegularFileKey, .fileSizeKey],
            options: [.skipsHiddenFiles]
        )) ?? []
        return entries
            .filter { url in
                let name = url.lastPathComponent
                return name.hasPrefix("stateapi-") && name.hasSuffix(".json")
            }
            .compactMap { Self.admit(url: $0) }
            .sorted {
                ($0.descriptor.instance, $0.descriptor.addr)
                    < ($1.descriptor.instance, $1.descriptor.addr)
            }
    }

    /// Reads and admits one descriptor file. Returns nil for anything that
    /// is not a small regular file carrying a descriptor that passes
    /// structural admission.
    static func admit(url: URL) -> ResolvedEndpoint? {
        let values = try? url.resourceValues(forKeys: [.isRegularFileKey, .fileSizeKey])
        // Symlinks are not regular files: a link planted in the live
        // directory cannot redirect the read elsewhere.
        guard values?.isRegularFile == true else { return nil }
        guard let size = values?.fileSize, (0...maxDescriptorBytes).contains(size) else {
            return nil
        }
        guard let data = try? Data(contentsOf: url),
              data.count <= maxDescriptorBytes
        else { return nil }
        guard let descriptor = try? StateAPIDescriptor.decode(data),
              let endpoint = try? descriptor.validatedEndpoint()
        else { return nil }
        return ResolvedEndpoint(descriptor: descriptor, endpoint: endpoint)
    }
}
