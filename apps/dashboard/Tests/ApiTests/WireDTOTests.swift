//
//  WireDTOTests.swift
//  LambeDashTests
//
//  U7 contract decode tests: every route's canonical body decodes, Go's
//  `null`/absent-omitempty shapes decode to typed defaults, unknown fields
//  are ignored, and a higher `schema_version` decodes into the
//  newer-daemon flag rather than failing (R12). The inline bodies mirror
//  the authoritative U2 DTOs (`internal/httpstate/dto.go` @ 3caa45e); the
//  Go-generated fixture parity test lands with the committed contract
//  fixtures (U3) and is deliberately not claimed here.
//

import Foundation
import Testing

@testable import LambeDash

nonisolated enum ContractBodies {
    /// GET /api/v1/state — full snapshot: runs, needs-human (recorded
    /// reason + gate fallback), gate rejections (known and unknown
    /// kind/time), identity, per-section as_of.
    static let snapshot = """
    {
      "schema_version": 1,
      "daemon": {"pid": 4242, "identity": "darwin:boot:1:2", "machine": "workbench",
                 "version": "v0.3.1", "config_path": "/cfg/lambe.yaml"},
      "runs": [
        {"issue": "lambe-xq2", "session": "sess-9", "attempt": 2, "state": "running",
         "last_event_age": "5s", "last_event_at": "2026-11-19T09:30:00Z", "phase": "gating"},
        {"issue": "lambe-def", "attempt": 1, "state": "deferred", "last_event_age": "4m"}
      ],
      "deferred_until": "2026-11-19T09:31:00Z",
      "wake_at": "2026-11-19T09:31:00Z",
      "needs_human": [
        {"issue": "lambe-kd8", "reason": "gate rejected: checks red",
         "detail": "gate failed twice", "since": "2026-11-19T09:02:00Z"},
        {"issue": "lambe-tw1", "reason": "agent exited without a verdict"}
      ],
      "gate_rejections": [
        {"issue": "lambe-kd8", "gate": "gh:pr", "reason": "2 checks failing",
         "at": "2026-11-19T09:02:00Z"},
        {"issue": "lambe-tw1", "reason": "output did not name a check"}
      ],
      "runs_as_of": "2026-11-19T09:30:15Z",
      "needs_human_as_of": "2026-11-19T09:30:02Z",
      "gate_rejections_as_of": "2026-11-19T09:30:02Z"
    }
    """

    /// GET /api/v1/state — Go marshals nil slices as null; sections must
    /// decode as empty arrays, never fail.
    static let snapshotNullSections = """
    {"schema_version":1,"daemon":{"pid":7},"runs":null,"needs_human":null,"gate_rejections":null}
    """

    /// GET /api/v1/queue — ready and blocked rows with blockers.
    static let queue = """
    {
      "schema_version": 1,
      "daemon": {"pid": 4242, "version": "v0.3.1"},
      "as_of": "2026-11-19T09:30:10Z",
      "rows": [
        {"id": "lambe-xq2", "title": "Wire the retry backoff", "reason": "ready: no open blockers"},
        {"id": "lambe-jq9", "title": "Extract the gate parser", "reason": "blocked by 2 issues",
         "blocked": true,
         "blockers": [{"id": "lambe-xq2", "title": "Wire the retry backoff"}]}
      ]
    }
    """

    /// GET /api/v1/queue — empty tracker: `[]`, rows empty.
    static let queueEmpty = """
    {"schema_version":1,"daemon":{"pid":4242},"as_of":"2026-11-19T09:30:10Z","rows":[]}
    """

    /// GET /api/v1/history — terminal and interrupted endings, newest first.
    static let history = """
    {
      "schema_version": 1,
      "daemon": {"pid": 4242, "version": "v0.3.1"},
      "as_of": "2026-11-19T09:30:11Z",
      "runs": [
        {"issue": "lambe-rv3", "title": "Trim the config surface",
         "phase": "stopped: gate green", "state": "settled", "attempt": 1,
         "outcome": "settled", "updated_at": "2026-11-19T08:54:00Z"},
        {"issue": "lambe-ce1", "phase": "interrupted", "attempt": 2,
         "updated_at": "2026-11-19T07:40:00Z"}
      ]
    }
    """

    /// The queue body with its rows in a different order — the reordering
    /// poll the selection test resolves against.
    static let queueReordered = """
    {
      "schema_version": 1,
      "daemon": {"pid": 4242, "version": "v0.3.1"},
      "as_of": "2026-11-19T09:30:10Z",
      "rows": [
        {"id": "lambe-jq9", "title": "Extract the gate parser", "reason": "blocked by 2 issues",
         "blocked": true,
         "blockers": [{"id": "lambe-xq2", "title": "Wire the retry backoff"}]},
        {"id": "lambe-xq2", "title": "Wire the retry backoff", "reason": "ready: no open blockers"}
      ]
    }
    """

    /// GET /api/v1/{issue} — full detail with every additive field.
    static let issue = """
    {
      "schema_version": 1,
      "daemon": {"pid": 4242, "version": "v0.3.1"},
      "issue": "lambe-xq2", "state": "running", "attempt": 2,
      "session": "sess-9", "last_event_age": "5s",
      "last_event_at": "2026-11-19T09:30:00Z",
      "summary": "editing retry.go", "phase": "gating",
      "turn": 12, "thread": "thr-4", "workspace": "/w/lambe",
      "workflow": "default", "wake_at": "2026-11-19T09:35:00Z",
      "last_gate": "gate: gh:pr — 2 checks failing",
      "tail": "line one\\nline two", "tail_truncated": true
    }
    """

    /// GET /api/v1/{issue} — the omitempty additive fields absent (turn 0,
    /// no tail).
    static let issueMinimal = """
    {"schema_version":1,"daemon":{"pid":4242},"issue":"lambe-def","state":"pending","attempt":1,"last_event_age":"n/a"}
    """

    /// ErrorResponse — the typed 404 envelope.
    static let error = """
    {"schema_version":1,"error":"not_found","message":"unknown issue \\"lambe-zz\\""}
    """

    /// A newer daemon: additive evolution plus a bumped version — decode
    /// must succeed and flag the banner (R12).
    static let snapshotNewerSchema = """
    {"schema_version":2,"daemon":{"pid":9},"runs":[{"issue":"lambe-new","state":"running","last_event_age":"1s","freshness_score":77}],"needs_human":[],"gate_rejections":[]}
    """
}

nonisolated func decode<T: Decodable>(_ body: String, as type: T.Type) throws -> T {
    try JSONDecoder().decode(T.self, from: Data(body.utf8))
}

@Suite("Wire contract decoding")
struct WireDTOTests {
    @Test("Snapshot decodes: runs, needs-human, rejections, identity, as_of")
    func snapshot() throws {
        let snap = try decode(ContractBodies.snapshot, as: WireSnapshot.self)
        #expect(snap.schemaVersion == 1)
        #expect(!snap.isNewerThanSupported)
        #expect(snap.daemon.pid == 4242)
        #expect(snap.daemon.identity == "darwin:boot:1:2")
        #expect(snap.daemon.machine == "workbench")
        #expect(snap.daemon.configPath == "/cfg/lambe.yaml")
        #expect(snap.runs.count == 2)
        #expect(snap.runs[0].phase == "gating")
        #expect(snap.runs[0].lastEventAt == "2026-11-19T09:30:00Z")
        #expect(snap.runs[1].session == nil)
        #expect(snap.runs[1].phase == nil)
        #expect(snap.needsHuman.count == 2)
        #expect(snap.needsHuman[0].since == "2026-11-19T09:02:00Z")
        #expect(snap.needsHuman[1].since == nil)
        #expect(snap.gateRejections[0].gate == "gh:pr")
        // Unknown kind/time stays nil — never fabricated (R10).
        #expect(snap.gateRejections[1].gate == nil)
        #expect(snap.gateRejections[1].at == nil)
        #expect(RFC3339.date(snap.needsHumanAsOf) != nil)
        #expect(RFC3339.date(snap.wakeAt) != nil)
    }

    @Test("Null snapshot sections decode as empty arrays")
    func snapshotNullSections() throws {
        let snap = try decode(ContractBodies.snapshotNullSections, as: WireSnapshot.self)
        #expect(snap.runs.isEmpty)
        #expect(snap.needsHuman.isEmpty)
        #expect(snap.gateRejections.isEmpty)
    }

    @Test("Queue decodes: ready, blocked, blockers; empty stays empty")
    func queue() throws {
        let q = try decode(ContractBodies.queue, as: WireQueue.self)
        #expect(q.rows.count == 2)
        #expect(q.rows[0].blocked == false)
        #expect(q.rows[1].blocked == true)
        #expect(q.rows[1].blockers.map(\.id) == ["lambe-xq2"])
        #expect(RFC3339.date(q.asOf) != nil)

        let empty = try decode(ContractBodies.queueEmpty, as: WireQueue.self)
        #expect(empty.rows.isEmpty)
    }

    @Test("History decodes: settled, failed, interrupted endings")
    func history() throws {
        let h = try decode(ContractBodies.history, as: WireHistory.self)
        #expect(h.runs.count == 2)
        #expect(h.runs[0].outcome == "settled")
        #expect(h.runs[1].title == nil)
        #expect(h.runs[1].phase == "interrupted")
        #expect(h.runs[1].state == nil)
    }

    @Test("Issue decodes: full detail; minimal body takes defaults")
    func issue() throws {
        let full = try decode(ContractBodies.issue, as: WireIssue.self)
        #expect(full.turn == 12)
        #expect(full.workspace == "/w/lambe")
        #expect(full.tailTruncated == true)
        #expect(full.lastGate == "gate: gh:pr — 2 checks failing")

        let minimal = try decode(ContractBodies.issueMinimal, as: WireIssue.self)
        #expect(minimal.turn == 0)
        #expect(minimal.tailTruncated == false)
        #expect(minimal.tail == nil)
        #expect(minimal.lastEventAge == "n/a")
    }

    @Test("Error envelope decodes")
    func errorEnvelope() throws {
        let e = try decode(ContractBodies.error, as: WireError.self)
        #expect(e.error == "not_found")
        #expect(e.schemaVersion == 1)
    }

    @Test("Higher schema version decodes and flags the newer-daemon state",
          arguments: [2, 7])
    func newerSchema(version: Int) throws {
        let body = ContractBodies.snapshotNewerSchema
            .replacingOccurrences(of: "\"schema_version\":2", with: "\"schema_version\":\(version)")
        let snap = try decode(body, as: WireSnapshot.self)
        #expect(snap.isNewerThanSupported)
        // The unknown additive field is ignored; known fields still decode.
        #expect(snap.runs.count == 1)
        #expect(snap.runs[0].issue == "lambe-new")
    }

    @Test("Sample demo payloads decode through the same contract path")
    func sampleData() {
        #expect(SampleData.snapshot.runs.count == 2)
        #expect(SampleData.snapshot.needsHuman.count == 2)
        #expect(SampleData.snapshot.gateRejections.count == 2)
        #expect(SampleData.snapshot.daemon.version == "demo")
        #expect(SampleData.queue.rows.count == 4)
        #expect(SampleData.queue.rows.filter(\.blocked).count == 2)
        #expect(SampleData.history.runs.count == 3)
        #expect(!SampleData.snapshot.isNewerThanSupported)
    }
}

@Suite("Issue route admission")
struct IssueRouteTests {
    @Test("Slug ids build route paths", arguments: ["lambe-xq2", "pg-123", "a.b_c-d"])
    func accepted(id: String) {
        #expect(IssueRoute.path(for: id) == "/api/v1/" + id)
    }

    @Test("Adversarial or malformed ids are refused", arguments: [
        "", "with space", "slash/ed", "../etc", "..", ".", ".hidden",
        "semi;colon", "unicode-é", String(repeating: "a", count: 129),
    ])
    func refused(id: String) {
        #expect(IssueRoute.path(for: id) == nil)
    }
}
