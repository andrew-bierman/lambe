//
//  WireDTOs.swift
//  LambeDash
//
//  The wire contract of the daemon state API (U2 shapes, `internal/
//  httpstate/dto.go` @ 3caa45e), mirrored additively (KTD2): optional
//  fields decode with defaults, unknown keys are ignored, and every Go
//  `omitempty` field is optional here. Schema version is pinned at 1
//  (WireContract.schemaVersion); a higher daemon version still decodes —
//  the connection layer surfaces the newer-daemon banner instead of
//  failing (R12).
//
//  Two shape rules the Go writer forces, both handled below:
//  - Snapshot collections marshal `null` when empty (nil slices), while
//    queue/history always marshal `[]`; every collection decodes as an
//    empty array for absent *or* null.
//  - Non-pointer scalar fields marked omitempty (`turn`, `tail_truncated`,
//    history `attempt`-adjacent fields) are absent when zero, so they
//    decode through `decodeIfPresent` with typed defaults.
//

import Foundation

/// Contract constants pinned by the versioned local contract (R8, KTD2).
nonisolated enum WireContract {
    /// The wire schema version this app understands.
    static let schemaVersion = 1
}

/// `DaemonDTO` — identity facts every response carries, so the operator
/// can always tell which daemon they are attached to (R9, R14).
nonisolated struct WireDaemonIdentity: Decodable, Equatable, Sendable {
    let pid: Int
    /// Process incarnation; nil when the platform cannot provide one.
    let identity: String?
    /// The daemon's configured dispatch identity.
    let machine: String?
    /// The daemon build version.
    let version: String?
    /// The configuration file the daemon loaded.
    let configPath: String?

    enum CodingKeys: String, CodingKey {
        case pid, identity, machine, version
        case configPath = "config_path"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        pid = try c.decode(Int.self, forKey: .pid)
        identity = try c.decodeIfPresent(String.self, forKey: .identity)
        machine = try c.decodeIfPresent(String.self, forKey: .machine)
        version = try c.decodeIfPresent(String.self, forKey: .version)
        configPath = try c.decodeIfPresent(String.self, forKey: .configPath)
    }
}

/// `SnapshotResponse` — GET /api/v1/state.
nonisolated struct WireSnapshot: Decodable, Equatable, Sendable {
    let schemaVersion: Int
    let daemon: WireDaemonIdentity
    /// Supervised runs, in report order. Empty, never missing.
    let runs: [WireRun]
    /// Items awaiting human attention. Empty, never missing.
    let needsHuman: [WireNeedsHuman]
    /// Recent gate rejections, oldest first. Empty, never missing.
    let gateRejections: [WireGateRejection]
    /// Supervisor deferral / wake times, RFC 3339; nil when unset.
    let deferredUntil: String?
    let wakeAt: String?
    /// Per-section read times, RFC 3339; nil before the daemon's first
    /// read of that section.
    let runsAsOf: String?
    let needsHumanAsOf: String?
    let gateRejectionsAsOf: String?

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case daemon, runs
        case needsHuman = "needs_human"
        case gateRejections = "gate_rejections"
        case deferredUntil = "deferred_until"
        case wakeAt = "wake_at"
        case runsAsOf = "runs_as_of"
        case needsHumanAsOf = "needs_human_as_of"
        case gateRejectionsAsOf = "gate_rejections_as_of"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        schemaVersion = try c.decode(Int.self, forKey: .schemaVersion)
        daemon = try c.decode(WireDaemonIdentity.self, forKey: .daemon)
        runs = try c.decodeIfPresent([WireRun].self, forKey: .runs) ?? []
        needsHuman = try c.decodeIfPresent([WireNeedsHuman].self, forKey: .needsHuman) ?? []
        gateRejections = try c.decodeIfPresent([WireGateRejection].self, forKey: .gateRejections) ?? []
        deferredUntil = try c.decodeIfPresent(String.self, forKey: .deferredUntil)
        wakeAt = try c.decodeIfPresent(String.self, forKey: .wakeAt)
        runsAsOf = try c.decodeIfPresent(String.self, forKey: .runsAsOf)
        needsHumanAsOf = try c.decodeIfPresent(String.self, forKey: .needsHumanAsOf)
        gateRejectionsAsOf = try c.decodeIfPresent(String.self, forKey: .gateRejectionsAsOf)
    }

    /// True when the daemon speaks a newer schema than this app supports
    /// (R12): decode still succeeds; the UI shows the newer-daemon banner.
    var isNewerThanSupported: Bool { schemaVersion > WireContract.schemaVersion }
}

/// `RunDTO` — one supervised run in the snapshot.
nonisolated struct WireRun: Decodable, Equatable, Sendable {
    let issue: String
    let session: String?
    let attempt: Int
    /// Coarse machine state name (§13.7 vocabulary).
    let state: String
    /// Human age of the last event ("5s", "n/a").
    let lastEventAge: String
    /// Machine counterpart of `lastEventAge`, RFC 3339; nil when unobserved.
    let lastEventAt: String?
    /// Operator-facing phase ("running", "gating", "stopped: gate red");
    /// nil when the projection has not observed the run (KTD4).
    let phase: String?
    /// Named workflow the run executes (additive; nil when unknown).
    let workflow: String?
    /// Machine of the daemon that owns the run (additive; nil when unknown).
    let machine: String?

    enum CodingKeys: String, CodingKey {
        case issue, session, attempt, state, phase, workflow, machine
        case lastEventAge = "last_event_age"
        case lastEventAt = "last_event_at"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        issue = try c.decode(String.self, forKey: .issue)
        session = try c.decodeIfPresent(String.self, forKey: .session)
        attempt = try c.decodeIfPresent(Int.self, forKey: .attempt) ?? 0
        state = try c.decode(String.self, forKey: .state)
        lastEventAge = try c.decodeIfPresent(String.self, forKey: .lastEventAge) ?? "n/a"
        lastEventAt = try c.decodeIfPresent(String.self, forKey: .lastEventAt)
        phase = try c.decodeIfPresent(String.self, forKey: .phase)
        workflow = try c.decodeIfPresent(String.self, forKey: .workflow)
        machine = try c.decodeIfPresent(String.self, forKey: .machine)
    }
}

/// `NeedsHumanDTO` — one item awaiting human attention (R3).
nonisolated struct WireNeedsHuman: Decodable, Equatable, Sendable {
    let issue: String
    /// The real recorded reason — never a placeholder (R3).
    let reason: String
    let detail: String?
    /// When the item was raised, RFC 3339; nil when the record carries no
    /// time — never guessed (R10).
    let since: String?

    enum CodingKeys: String, CodingKey {
        case issue, reason, detail, since
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        issue = try c.decode(String.self, forKey: .issue)
        reason = try c.decode(String.self, forKey: .reason)
        detail = try c.decodeIfPresent(String.self, forKey: .detail)
        since = try c.decodeIfPresent(String.self, forKey: .since)
    }
}

/// `GateRejectionDTO` — one verified-completion gate rejection. `gate` and
/// `at` are present only when the source record carries them; absent means
/// unknown, never fabricated (R3, R10).
nonisolated struct WireGateRejection: Decodable, Equatable, Sendable {
    let issue: String
    let gate: String?
    let reason: String
    let at: String?

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        issue = try c.decode(String.self, forKey: .issue)
        gate = try c.decodeIfPresent(String.self, forKey: .gate)
        reason = try c.decode(String.self, forKey: .reason)
        at = try c.decodeIfPresent(String.self, forKey: .at)
    }

    enum CodingKeys: String, CodingKey {
        case issue, gate, reason, at
    }
}

/// `QueueResponse` — GET /api/v1/queue: readiness rows for the configured
/// labels (R2), served from the daemon's cached canonical read.
nonisolated struct WireQueue: Decodable, Equatable, Sendable {
    let schemaVersion: Int
    let daemon: WireDaemonIdentity
    /// When the readiness projection was read from the tracker; nil before
    /// the daemon's first read.
    let asOf: String?
    let rows: [WireQueueRow]

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case daemon
        case asOf = "as_of"
        case rows
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        schemaVersion = try c.decode(Int.self, forKey: .schemaVersion)
        daemon = try c.decode(WireDaemonIdentity.self, forKey: .daemon)
        asOf = try c.decodeIfPresent(String.self, forKey: .asOf)
        rows = try c.decodeIfPresent([WireQueueRow].self, forKey: .rows) ?? []
    }
}

/// `QueueRowDTO` — one readiness row: dispatchable, or blocked with the
/// reason and blockers.
nonisolated struct WireQueueRow: Decodable, Equatable, Sendable {
    let id: String
    let title: String?
    let reason: String?
    let blocked: Bool
    let blockers: [WireQueueBlocker]

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        title = try c.decodeIfPresent(String.self, forKey: .title)
        reason = try c.decodeIfPresent(String.self, forKey: .reason)
        blocked = try c.decodeIfPresent(Bool.self, forKey: .blocked) ?? false
        blockers = try c.decodeIfPresent([WireQueueBlocker].self, forKey: .blockers) ?? []
    }

    enum CodingKeys: String, CodingKey {
        case id, title, reason, blocked, blockers
    }
}

/// `QueueBlockerDTO` — one blocking issue.
nonisolated struct WireQueueBlocker: Decodable, Equatable, Sendable {
    let id: String
    let title: String?
}

/// `HistoryResponse` — GET /api/v1/history: terminal live rows merged with
/// restart-archived attempts, newest first, bounded at 100 (R4).
nonisolated struct WireHistory: Decodable, Equatable, Sendable {
    let schemaVersion: Int
    let daemon: WireDaemonIdentity
    let asOf: String?
    let runs: [WireHistoryRun]

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case daemon
        case asOf = "as_of"
        case runs
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        schemaVersion = try c.decode(Int.self, forKey: .schemaVersion)
        daemon = try c.decode(WireDaemonIdentity.self, forKey: .daemon)
        asOf = try c.decodeIfPresent(String.self, forKey: .asOf)
        runs = try c.decodeIfPresent([WireHistoryRun].self, forKey: .runs) ?? []
    }
}

/// `HistoryRunDTO` — one archived or terminal run.
nonisolated struct WireHistoryRun: Decodable, Equatable, Sendable {
    let issue: String
    let title: String?
    /// The phase the attempt ended in ("stopped: gate red", "interrupted").
    let phase: String?
    let state: String?
    let attempt: Int
    /// How the run ended, when the record carries one.
    let outcome: String?
    /// Last observed, RFC 3339 — the newest-first ordering key.
    let updatedAt: String?

    enum CodingKeys: String, CodingKey {
        case issue, title, phase, state, attempt, outcome
        case updatedAt = "updated_at"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        issue = try c.decode(String.self, forKey: .issue)
        title = try c.decodeIfPresent(String.self, forKey: .title)
        phase = try c.decodeIfPresent(String.self, forKey: .phase)
        state = try c.decodeIfPresent(String.self, forKey: .state)
        attempt = try c.decodeIfPresent(Int.self, forKey: .attempt) ?? 0
        outcome = try c.decodeIfPresent(String.self, forKey: .outcome)
        updatedAt = try c.decodeIfPresent(String.self, forKey: .updatedAt)
    }
}

/// `IssueResponse` — GET /api/v1/{issue}: run detail at parity with the
/// TUI inspector (R5). All fields beyond the §13.7 core are additive and
/// optional.
nonisolated struct WireIssue: Decodable, Equatable, Sendable {
    let schemaVersion: Int
    let daemon: WireDaemonIdentity
    let issue: String
    let state: String
    let attempt: Int
    let session: String?
    let lastEventAge: String
    let lastEventAt: String?
    let summary: String?
    let phase: String?
    let turn: Int
    let thread: String?
    let workspace: String?
    let workflow: String?
    /// Machine of the daemon that owns the run (additive).
    let machine: String?
    let wakeAt: String?
    /// Bounded preview of the last gate output.
    let lastGate: String?
    /// At most 4 KB of recent output; full output lives in artifacts.
    let tail: String?
    let tailTruncated: Bool

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case daemon, issue, state, attempt, session, summary, phase, turn
        case thread, workspace, workflow, machine
        case wakeAt = "wake_at"
        case lastGate = "last_gate"
        case tail
        case tailTruncated = "tail_truncated"
        case lastEventAge = "last_event_age"
        case lastEventAt = "last_event_at"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        schemaVersion = try c.decode(Int.self, forKey: .schemaVersion)
        daemon = try c.decode(WireDaemonIdentity.self, forKey: .daemon)
        issue = try c.decode(String.self, forKey: .issue)
        state = try c.decode(String.self, forKey: .state)
        attempt = try c.decodeIfPresent(Int.self, forKey: .attempt) ?? 0
        session = try c.decodeIfPresent(String.self, forKey: .session)
        lastEventAge = try c.decodeIfPresent(String.self, forKey: .lastEventAge) ?? "n/a"
        lastEventAt = try c.decodeIfPresent(String.self, forKey: .lastEventAt)
        summary = try c.decodeIfPresent(String.self, forKey: .summary)
        phase = try c.decodeIfPresent(String.self, forKey: .phase)
        turn = try c.decodeIfPresent(Int.self, forKey: .turn) ?? 0
        thread = try c.decodeIfPresent(String.self, forKey: .thread)
        workspace = try c.decodeIfPresent(String.self, forKey: .workspace)
        workflow = try c.decodeIfPresent(String.self, forKey: .workflow)
        machine = try c.decodeIfPresent(String.self, forKey: .machine)
        wakeAt = try c.decodeIfPresent(String.self, forKey: .wakeAt)
        lastGate = try c.decodeIfPresent(String.self, forKey: .lastGate)
        tail = try c.decodeIfPresent(String.self, forKey: .tail)
        tailTruncated = try c.decodeIfPresent(Bool.self, forKey: .tailTruncated) ?? false
    }
}

/// `ErrorResponse` — every non-2xx body on the daemon side. The transport
/// deliberately does not reflect response text in `APIError` (see
/// `APIError.status`); this type exists for the typed decode tests and any
/// future trusted-envelope use.
nonisolated struct WireError: Decodable, Equatable, Sendable {
    let schemaVersion: Int
    /// Machine-stable error code ("not_found", "unauthorized", …).
    let error: String
    let message: String

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case error, message
    }
}

/// RFC 3339 timestamps the contract carries everywhere machine-readable
/// ages accompany human strings (R9). One parser, Sendable-safe (the
/// format style has no shared mutable state); a value that does not parse
/// is unknown, never an error surfaced to the operator.
nonisolated enum RFC3339 {
    /// Parses an RFC 3339 timestamp, or nil when absent/unparseable.
    static func date(_ value: String?) -> Date? {
        guard let value, !value.isEmpty else { return nil }
        return try? Date(value, strategy: .iso8601)
    }
}

// MARK: - View identity

extension WireDaemonIdentity {
    /// Token-free version and pid facts shared by the window subtitle and
    /// the connection footer.
    var versionPidText: String {
        var parts: [String] = []
        if let version, !version.isEmpty { parts.append(VersionLabel.text(version)) }
        parts.append("pid \(pid)")
        return parts.joined(separator: " · ")
    }
}

/// "v0.9.3": daemons report versions both with and without the leading v.
nonisolated enum VersionLabel {
    static func text(_ version: String) -> String {
        let digitAfterV = version.hasPrefix("v") && version.dropFirst().first?.isNumber == true
        return digitAfterV ? version : "v" + version
    }
}

// Stable ForEach identity, derived from the contract's uniqueness rules:
// snapshot runs, queue rows, and needs-human items carry at most one row
// per issue; history rows and gate rejections can repeat an issue (many
// attempts / rejections), so their identity composites the record's
// immutable content. None derive from view order.

extension WireRun: Identifiable {
    var id: String { issue }
}

extension WireQueueRow: Identifiable {}

extension WireNeedsHuman: Identifiable {
    var id: String { issue }
}

extension WireHistoryRun: Identifiable {
    var id: String { "\(issue)#\(attempt)#\(updatedAt ?? "")" }
}

extension WireGateRejection: Identifiable {
    var id: String { "\(issue)#\(reason)#\(at ?? "")" }
}
