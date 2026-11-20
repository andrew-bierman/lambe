//
//  SampleData.swift
//  LambeDash
//
//  Bundled sample payloads for the toolbar's Demo mode: enough to exercise
//  every view state — ready and blocked queue rows, a needs-human item
//  with a recorded reason and one with only the gate fallback, gate
//  rejections with known and unknown kind/time (R10 truthfulness even in
//  the sample), and a history mixing settled, failed, and interrupted
//  endings. Small on purpose; the demo is a view of the UI, not a
//  simulator.
//

import Foundation

/// Static demo payloads, all built through the wire DTO initializers so
/// they can never drift from the real decoder's shape.
nonisolated enum SampleData {
    /// The demo snapshot. Built by decoding a canonical §13.7 body — one
    /// construction path, shared with the contract tests. The daemon
    /// identity inside is labeled `version: "demo"` so the footer can
    /// never pass demo data off as an attached daemon.
    static let snapshot: WireSnapshot = decode(
        """
        {
          "schema_version": 1,
          "daemon": {"pid": 4242, "identity": "demo:incarnation:1", "machine": "workbench",
                     "version": "demo", "config_path": "~/work/lambe.yaml"},
          "runs": [
            {"issue": "demo-hk4", "session": "sess-demo-a", "attempt": 2, "state": "running",
             "last_event_age": "12s", "last_event_at": "2026-11-19T09:30:12Z", "phase": "gating"},
            {"issue": "demo-pm7", "attempt": 1, "state": "deferred",
             "last_event_age": "4m", "last_event_at": "2026-11-19T09:26:00Z", "phase": "retry"}
          ],
          "deferred_until": "2026-11-19T09:32:00Z",
          "wake_at": "2026-11-19T09:32:00Z",
          "needs_human": [
            {"issue": "demo-kd8", "reason": "gate rejected: pr checks red after two retries",
             "detail": "The verified-completion gate rejected the attempt twice.",
             "since": "2026-11-19T09:02:00Z"},
            {"issue": "demo-tw1", "reason": "agent exited without a verdict"}
          ],
          "gate_rejections": [
            {"issue": "demo-kd8", "gate": "gh:pr", "reason": "2 checks failing",
             "at": "2026-11-19T09:02:00Z"},
            {"issue": "demo-tw1", "reason": "gate output did not name a check"}
          ],
          "runs_as_of": "2026-11-19T09:30:15Z",
          "needs_human_as_of": "2026-11-19T09:30:02Z",
          "gate_rejections_as_of": "2026-11-19T09:30:02Z"
        }
        """
    )

    /// The demo queue: ready and blocked rows across labels, with
    /// blockers (R2).
    static let queue: WireQueue = decode(
        """
        {
          "schema_version": 1,
          "daemon": {"pid": 4242, "identity": "demo:incarnation:1", "machine": "workbench",
                     "version": "demo"},
          "as_of": "2026-11-19T09:30:10Z",
          "rows": [
            {"id": "demo-hk4", "title": "Wire the retry backoff", "reason": "ready: no open blockers"},
            {"id": "demo-fs2", "title": "Document the smoke suite", "reason": "ready: no open blockers"},
            {"id": "demo-jq9", "title": "Extract the gate parser", "reason": "blocked by 2 issues",
             "blocked": true,
             "blockers": [{"id": "demo-hk4", "title": "Wire the retry backoff"},
                          {"id": "demo-pm7", "title": "Stabilize the depth spawn"}]},
            {"id": "demo-pm7", "title": "Stabilize the depth spawn", "reason": "blocked by 1 issue",
             "blocked": true,
             "blockers": [{"id": "demo-kd8", "title": "Needs-human: gate red"}]}
          ]
        }
        """
    )

    /// The demo history: settled, failed, and restart-interrupted (R4).
    static let history: WireHistory = decode(
        """
        {
          "schema_version": 1,
          "daemon": {"pid": 4242, "identity": "demo:incarnation:1", "machine": "workbench",
                     "version": "demo"},
          "as_of": "2026-11-19T09:30:11Z",
          "runs": [
            {"issue": "demo-rv3", "title": "Trim the config surface", "phase": "stopped: gate green",
             "state": "settled", "attempt": 1, "outcome": "settled",
             "updated_at": "2026-11-19T08:54:00Z"},
            {"issue": "demo-bn6", "title": "Fix the launchd flake", "phase": "stopped: gate red",
             "state": "failed", "attempt": 3, "outcome": "failed",
             "updated_at": "2026-11-19T08:21:30Z"},
            {"issue": "demo-ce1", "title": "Archive old attempts", "phase": "interrupted",
             "state": "interrupted", "attempt": 2,
             "updated_at": "2026-11-19T07:40:00Z"}
          ]
        }
        """
    )

    /// Decodes one canonical body; the demo payloads are written once and
    /// pinned by the decode tests.
    private static func decode<T: Decodable>(_ body: String) -> T {
        // The payloads above are static literals; a decode failure here is
        // a programming error and should be loud, not surfaced as demo
        // emptiness. `try!` is confined to this enum's own constants.
        try! JSONDecoder().decode(T.self, from: Data(body.utf8))
    }
}
