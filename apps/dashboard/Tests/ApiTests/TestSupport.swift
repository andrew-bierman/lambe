//
//  TestSupport.swift
//  LambeDashTests
//
//  Inline fixtures for the U7 transport/discovery tests. Descriptor
//  documents mirror the committed U1 writer byte-for-byte
//  (`internal/daemon/state.go` + `state_test.go`); the snapshot body
//  mirrors the committed §13.7 `SnapshotResponse` (`internal/httpstate/
//  dto.go`). Go-generated contract fixtures arrive with U3 and will
//  replace the inline strings; the field names here are already pinned by
//  the Go tests.
//

import Foundation

/// Descriptor documents covering the canonical shape and the malicious /
/// malformed variants the store must refuse.
nonisolated enum DescriptorDocuments {
    /// Exactly the bytes the U1 writer emits for a full descriptor (all
    /// fields present, omitempty fields populated).
    static let canonical = """
    {"schema_version":1,"instance":"default","addr":"127.0.0.1:53412","bearer_token":"tok","pid":4242,"identity":"darwin:boot:1:2","version":"test","config_path":"/cfg/lambe.yaml","started_at":"2026-11-19T09:30:00Z"}
    """

    /// The omitempty fields absent, as a platform-limited daemon writes.
    static let canonicalWithoutOptionals = """
    {"schema_version":1,"instance":"solo","addr":"127.0.0.1:53413","bearer_token":"tok2","pid":43,"version":"dev","started_at":"2026-11-19T09:31:00Z"}
    """

    /// Builds a descriptor document with surgical overrides for the
    /// admission and malicious-input tests. `raw` fragments are spliced in
    /// verbatim, so wrong-typed fields (`"pid":"4242"`) are expressible.
    static func json(
        schemaVersion: Int = 1,
        instance: String = "default",
        addr: String = "127.0.0.1:53412",
        bearerToken: String = "tok",
        pid: String = "4242",
        identity: String? = "darwin:boot:1:2",
        version: String = "test",
        configPath: String? = "/cfg/lambe.yaml",
        startedAt: String = "2026-11-19T09:30:00Z",
        extraKey: String = ""
    ) -> String {
        var fields = [
            #""schema_version":\#(schemaVersion)"#,
            #""instance":"\#(instance)""#,
            #""addr":"\#(addr)""#,
            #""bearer_token":"\#(bearerToken)""#,
            #""pid":\#(pid)"#,
        ]
        if let identity { fields.append(#""identity":"\#(identity)""#) }
        fields.append(#""version":"\#(version)""#)
        if let configPath { fields.append(#""config_path":"\#(configPath)""#) }
        fields.append(#""started_at":"\#(startedAt)""#)
        if !extraKey.isEmpty { fields.append(extraKey) }
        return "{" + fields.joined(separator: ",") + "}"
    }
}

/// A minimal probe of the committed §13.7 state snapshot: only the wire
/// schema version, which this transport subset decodes. The typed
/// Queue/Attention/History payload DTOs belong to the U2-shaped data layer
/// and are deliberately not defined here.
nonisolated struct StateProbe: Decodable, Equatable, Sendable {
    let schemaVersion: Int

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
    }
}

/// Canonical GET /api/v1/state bodies shaped like the committed DTO.
nonisolated enum SnapshotDocuments {
    /// §13.7 SnapshotResponse with one running run and one needs-human
    /// entry (detail-only fields omitted where the writer would omit them).
    static let withRuns = """
    {"schema_version":1,"runs":[{"issue":"lambe-xq2","session":"sess-9","attempt":2,"state":"running","last_event_age":"5s"}],"wake_at":"2026-11-19T09:31:00Z","needs_human":[{"issue":"lambe-kd8","reason":"gate_rejected","detail":"gate failed"}],"gate_rejections":[]}
    """

    /// An empty daemon snapshot: sections present but empty, never null
    /// arrays in this fixture (the decoder accepts either).
    static let empty = """
    {"schema_version":1,"runs":[],"needs_human":[],"gate_rejections":[]}
    """
}
