//
//  GoFixtureTests.swift
//  LambeDashTests
//
//  U3 contract parity: the Go-generated fixtures checked in at repo root
//  `testdata/contract/v1/` decode into the Swift wire types. Where
//  WireDTOTests pins decode behavior with inline bodies, this file pins
//  the *shared* artifacts the daemon actually writes: each test decodes
//  the committed bytes and asserts a focused slice chosen from that
//  fixture — the fields whose decode behavior the type contract cares
//  about (absent omitempty fields decoding as absent, absent collection
//  keys defaulting empty, the descriptor surviving loopback admission).
//
//  The fixture directory is resolved from `#filePath` (source layout
//  `apps/dashboard/Tests/ApiTests` → repo root), so the tests never
//  depend on the working directory of the process that runs them and
//  hold under any DerivedData or xcodebuild invocation. A missing fixture
//  file fails the test — fixture absence is a contract break, never a
//  skip.
//

import Foundation
import Testing

@testable import LambeDash

/// Loads committed contract fixtures. Kept private and file-contained:
/// this file must not couple to other test helpers, so the combined
/// branch can land the workers' files independently.
private enum ContractFixtures {
    /// Reads `<repo>/testdata/contract/v1/<name>.json`, located by walking
    /// up from this source file: `ApiTests` → `Tests` → `dashboard` →
    /// `apps` → repo root. Throws when the fixture is missing — tests
    /// below propagate that as a failure, deliberately.
    static func data(_ name: String) throws -> Data {
        let directory = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()   // Tests/ApiTests
            .deletingLastPathComponent()   // Tests
            .deletingLastPathComponent()   // dashboard
            .deletingLastPathComponent()   // apps
            .deletingLastPathComponent()   // repo root
            .appendingPathComponent("testdata", isDirectory: true)
            .appendingPathComponent("contract", isDirectory: true)
            .appendingPathComponent("v1", isDirectory: true)
        return try Data(contentsOf: directory.appendingPathComponent("\(name).json"))
    }

    /// Decodes one fixture into the expected wire type.
    static func decode<T: Decodable>(_ name: String, as type: T.Type) throws -> T {
        try JSONDecoder().decode(T.self, from: try data(name))
    }
}

@Suite("Go fixture contract parity")
struct GoFixtureTests {
    @Test("snapshot.json decodes: full and sparse runs, needs-human, gate rejections")
    func snapshot() throws {
        let snap = try ContractFixtures.decode("snapshot", as: WireSnapshot.self)
        #expect(snap.schemaVersion == 1)
        #expect(snap.daemon.pid == 4242)
        #expect(snap.daemon.identity == "darwin:boot:1:2")
        #expect(snap.runs.count == 2)
        // The fully populated run decodes field-for-field.
        #expect(snap.runs[0].issue == "T-1")
        #expect(snap.runs[0].session == "ses-t1")
        #expect(snap.runs[0].attempt == 2)
        #expect(snap.runs[0].state == "running")
        #expect(snap.runs[0].phase == "gating")
        #expect(snap.runs[0].lastEventAt == "2026-11-19T11:59:30Z")
        // The sparse run's omitted fields decode as absent, not defaults.
        #expect(snap.runs[1].session == nil)
        #expect(snap.runs[1].lastEventAt == nil)
        #expect(snap.needsHuman.map(\.issue) == ["T-2"])
        #expect(snap.needsHuman[0].reason == "gate_failed")
        // The rejection fixture carries no `gate` key: absent stays nil.
        #expect(snap.gateRejections.count == 1)
        #expect(snap.gateRejections[0].reason == "gh:pr rejected: merge conflict")
        #expect(snap.gateRejections[0].gate == nil)
        // Machine timestamps ride alongside the human ages (R9).
        #expect(RFC3339.date(snap.deferredUntil) != nil)
        #expect(RFC3339.date(snap.runsAsOf) != nil)
    }

    @Test("queue.json decodes: ready and blocked rows with blockers")
    func queue() throws {
        let q = try ContractFixtures.decode("queue", as: WireQueue.self)
        #expect(q.schemaVersion == 1)
        #expect(q.rows.count == 2)
        // The ready row omits `blockers` entirely: absent key → empty.
        #expect(q.rows[0].id == "Q-1")
        #expect(q.rows[0].blocked == false)
        #expect(q.rows[0].blockers.isEmpty)
        // The blocked row carries its blocker through.
        #expect(q.rows[1].blocked == true)
        #expect(q.rows[1].blockers.map(\.id) == ["api-9"])
        #expect(RFC3339.date(q.asOf) != nil)
    }

    @Test("history.json decodes: three ending shapes across the rows")
    func history() throws {
        let h = try ContractFixtures.decode("history", as: WireHistory.self)
        #expect(h.runs.count == 3)
        // Titled gate-red ending.
        #expect(h.runs[0].issue == "H-2")
        #expect(h.runs[0].phase == "stopped: gate red")
        #expect(h.runs[0].attempt == 3)
        // Interrupted ending carries no state.
        #expect(h.runs[1].phase == "interrupted")
        #expect(h.runs[1].state == nil)
        // Settled ending carries state + outcome but no title or phase.
        #expect(h.runs[2].state == "settled")
        #expect(h.runs[2].outcome == "completed")
        #expect(h.runs[2].title == nil)
        #expect(h.runs[2].phase == nil)
        #expect(RFC3339.date(h.runs[2].updatedAt) != nil)
    }

    @Test("issue.json decodes: additive detail fields, tail flag defaults false")
    func issue() throws {
        let detail = try ContractFixtures.decode("issue", as: WireIssue.self)
        #expect(detail.issue == "T-1")
        #expect(detail.state == "running")
        #expect(detail.attempt == 2)
        #expect(detail.phase == "gating")
        // Additive detail fields at parity with the TUI inspector (R5).
        #expect(detail.turn == 4)
        #expect(detail.workspace == "/workspaces/T-1")
        #expect(detail.workflow == "general")
        // The fixture omits `tail_truncated`: absent → false, never guessed.
        #expect(detail.tailTruncated == false)
        #expect(detail.tail != nil)
        #expect(detail.lastGate == "pr checks pending")
    }

    @Test("descriptor.json decodes and passes loopback admission")
    func descriptor() throws {
        let descriptor = try StateAPIDescriptor.decode(try ContractFixtures.data("descriptor"))
        #expect(descriptor.schemaVersion == 1)
        #expect(descriptor.instance == "lambe-1a2b3c4d5e6f")
        #expect(descriptor.pid == 4242)
        #expect(descriptor.identity == "darwin:boot:1:2")
        #expect(descriptor.startedAt == "2026-11-19T11:30:00Z")
        // The committed address must survive structural admission: it is
        // the literal loopback the client is allowed to attach to.
        let endpoint = try descriptor.validatedEndpoint()
        #expect(endpoint.port == 53412)
    }

    @Test("error.json decodes: typed envelope with escaped-quote message")
    func error() throws {
        let e = try ContractFixtures.decode("error", as: WireError.self)
        #expect(e.schemaVersion == 1)
        #expect(e.error == "not_found")
        // The JSON-escaped quotes survive decode intact.
        #expect(e.message == "unknown issue \"T-404\"")
    }
}
