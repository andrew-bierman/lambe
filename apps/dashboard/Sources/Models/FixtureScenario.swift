//
//  FixtureScenario.swift
//  LambeDash
//
//  Named, network-free scenarios shared by SwiftUI previews and the
//  screenshot script (`-LambeScenario <name>`). Timestamps are relative to
//  "now" so ages and stall classification read the same whenever the
//  scenario is rendered.
//

import Foundation

nonisolated enum FixtureScenario: String, CaseIterable, Sendable {
    case live, stalled, archive, empty, disconnected, neverConnected, longText, multiMachine

    struct Payloads {
        var snapshot: WireSnapshot
        var queue: WireQueue
        var history: WireHistory
        var issue: WireIssue?
        var state: ConnectionModel.ConnectionState
    }

    /// Instance records the sidebar lists for this scenario.
    @MainActor
    static func store(for scenario: FixtureScenario) -> InstanceStore {
        let defaults = UserDefaults(suiteName: "lambe.fixture.\(UUID().uuidString)")!
        let store = InstanceStore(defaults: defaults)
        let base = "/tmp/lambe-fixture"
        switch scenario {
        case .multiMachine:
            store.remove(store.records[0])
            for (i, name) in ["Workbench · app-repo", "Mac mini · infra", "omarchy · docs-site"].enumerated() {
                store.add(InstanceRecord(name: name, stateDirectory: "\(base)/m\(i)"))
            }
            store.select(store.records[0].id)
        case .longText:
            store.remove(store.records[0])
            store.add(InstanceRecord(
                name: "Production supervisor for the monorepo with a very long operator-chosen label",
                stateDirectory: "\(base)/long"
            ))
        default:
            store.remove(store.records[0])
            store.add(InstanceRecord(name: "Workbench", stateDirectory: "\(base)/workbench"))
        }
        return store
    }

    var data: Payloads {
        let n = Self.now
        switch self {
        case .live:
            return Self.build(runs: [
                Self.run("lam-101", "running", "running", 3),
                Self.run("lam-102", "running", "gating", 20),
            ], queue: Self.defaultQueue, history: Self.defaultHistory, issue: Self.issue("lam-101", "running", 3, n))
        case .stalled:
            return Self.build(runs: [
                Self.run("lam-101", "running", "running", 4),
                Self.run("lam-102", "running", "running", 170),
                Self.run("lam-103", "running", "running", 540),
                Self.run("lam-104", "running", "gating", 4100),
            ], queue: Self.defaultQueue, history: Self.defaultHistory, issue: Self.issue("lam-103", "running", 540, n))
        case .archive:
            return Self.build(runs: [], queue: "[]", history: Self.defaultHistory, issue: nil)
        case .empty:
            return Self.build(runs: [], queue: "[]", history: "[]", issue: nil)
        case .disconnected:
            var d = Self.build(runs: [Self.run("lam-101", "running", "running", 3)],
                               queue: Self.defaultQueue, history: Self.defaultHistory, issue: nil)
            d.state = .disconnected(reason: "connection refused — the daemon stopped answering 40s ago")
            return d
        case .neverConnected:
            var d = Self.build(runs: [], queue: "[]", history: "[]", issue: nil)
            d.state = .neverConnected(reason: .noDescriptor)
            return d
        case .longText:
            let id = "lambe-with-an-extraordinarily-long-issue-identifier-0042"
            return Self.build(runs: [
                Self.run(id, "running", "waiting on the verified-completion gate for the pull request checks to settle after rebase", 600),
            ], queue: """
            [{"id": "lam-9", "title": "Rewrite the supervisor loop so that retry backoff, stall detection, and the needs-human escalation share one clock and one state machine", "reason": "blocked by 3 issues", "blocked": true,
              "blockers": [{"id": "lam-1"}, {"id": "lam-2"}, {"id": "lam-3"}]}]
            """, history: Self.defaultHistory, issue: Self.issue(id, "running", 600, n))
        case .multiMachine:
            return Self.build(runs: [
                Self.run("lam-101", "running", "running", 5),
                Self.run("lam-103", "running", "running", 800),
            ], queue: Self.defaultQueue, history: Self.defaultHistory, issue: Self.issue("lam-103", "running", 800, n),
                machine: "workbench")
        }
    }

    // MARK: Builders

    private static var now: Date { Date() }

    static func iso(_ secondsAgo: TimeInterval) -> String {
        ISO8601DateFormatter().string(from: now.addingTimeInterval(-secondsAgo))
    }

    static func run(_ id: String, _ state: String, _ phase: String, _ ago: TimeInterval) -> String {
        """
        {"issue": "\(id)", "session": "sess-\(id)", "attempt": 1, "state": "\(state)", "phase": "\(phase)",
         "last_event_age": "\(Int(ago))s", "last_event_at": "\(iso(ago))"}
        """
    }

    private static let defaultQueue = """
    [{"id": "lam-110", "title": "Document the smoke suite", "reason": "ready: no open blockers"},
     {"id": "lam-111", "title": "Extract the gate parser", "reason": "blocked by 1 issue", "blocked": true,
      "blockers": [{"id": "lam-101", "title": "Wire the retry backoff"}]}]
    """

    private static var defaultHistory: String {
        """
        [{"issue": "lam-090", "title": "Trim the config surface", "phase": "stopped: gate green", "state": "settled",
          "attempt": 1, "outcome": "settled", "updated_at": "\(iso(1800))"},
         {"issue": "lam-087", "title": "Fix the launchd flake", "phase": "stopped: gate red", "state": "failed",
          "attempt": 3, "outcome": "failed", "updated_at": "\(iso(5400))"},
         {"issue": "lam-080", "title": "Archive old attempts", "phase": "interrupted", "state": "interrupted",
          "attempt": 2, "updated_at": "\(iso(9000))"}]
        """
    }

    static func issue(_ id: String, _ state: String, _ ago: TimeInterval, _ now: Date) -> WireIssue {
        decode("""
        {"schema_version": 1, "daemon": {"pid": 4242, "machine": "workbench", "version": "1.0.0"},
         "issue": "\(id)", "state": "\(state)", "attempt": 1, "session": "sess-\(id)",
         "last_event_age": "\(Int(ago))s", "last_event_at": "\(iso(ago))",
         "phase": "running", "turn": 7, "thread": "thr-9f2", "workspace": "/tmp",
         "workflow": "ship-pr", "summary": "Editing the retry policy",
         "tail": "$ go test ./internal/daemon\\nok  \\tinternal/daemon\\t2.114s\\nrunning gate: gh:pr checks\\n"}
        """)
    }

    private static func build(
        runs: [String], queue: String, history: String, issue: WireIssue?,
        machine: String = "workbench"
    ) -> Payloads {
        let daemon = #"{"pid": 4242, "identity": "fx:1", "machine": "\#(machine)", "version": "1.0.0", "config_path": "/Users/me/work/lambe.yaml"}"#
        let snapshot: WireSnapshot = decode("""
        {"schema_version": 1, "daemon": \(daemon), "runs": [\(runs.joined(separator: ","))],
         "needs_human": [], "gate_rejections": [],
         "runs_as_of": "\(iso(2))", "needs_human_as_of": "\(iso(2))", "gate_rejections_as_of": "\(iso(2))"}
        """)
        let q: WireQueue = decode(#"{"schema_version": 1, "daemon": \#(daemon), "as_of": "\#(iso(2))", "rows": \#(queue)}"#)
        let h: WireHistory = decode(#"{"schema_version": 1, "daemon": \#(daemon), "as_of": "\#(iso(2))", "runs": \#(history)}"#)
        return Payloads(snapshot: snapshot, queue: q, history: h, issue: issue, state: .attached)
    }

    private static func decode<T: Decodable>(_ body: String) -> T {
        // Static literals above: a failure is a programming error, loudly.
        try! JSONDecoder().decode(T.self, from: Data(body.utf8))
    }
}
