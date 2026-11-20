//
//  QueueComposition.swift
//  LambeDash
//
//  The queue's rendered rows, decided in one pure place (R2, R4): the
//  snapshot runs still active, plus the readiness rows whose issues are
//  not already rendered as runs. Terminal snapshot rows — settled,
//  failed, canceled on the §13.7 state vocabulary — are archived in
//  History, so they never render as active runs and never count toward
//  the queue. The section list and the sidebar badge both read this one
//  source: the badge can only ever count rows the list renders.
//

import Foundation

/// Which snapshot runs and queue rows the Queue section renders.
nonisolated enum QueueComposition {
    /// Terminal run states on the daemon's wire vocabulary
    /// (`internal/mcp` RunState names). Terminal rows live in History
    /// (R4); unknown state names stay — a newer daemon's additive state
    /// keeps rendering rather than vanishing (KTD2).
    private static let terminalStates: Set<String> = ["settled", "failed", "canceled"]

    /// True when a run's server-reported state still counts as active.
    static func isActive(_ state: String) -> Bool {
        !terminalStates.contains(state)
    }

    /// The snapshot runs that render under "Active runs": terminal rows
    /// are History's, not the queue's.
    static func activeRuns(in snapshot: WireSnapshot?) -> [WireRun] {
        guard let snapshot else { return [] }
        return snapshot.runs.filter { isActive($0.state) }
    }

    /// The readiness rows that render under "Readiness": issues already
    /// rendered as active runs are not duplicated. An issue whose only
    /// snapshot row is terminal renders — the settled run is suppressed,
    /// its readiness row is not.
    static func readinessRows(
        queue: WireQueue?, snapshot: WireSnapshot?
    ) -> [WireQueueRow] {
        guard let queue else { return [] }
        let activeIDs = Set(activeRuns(in: snapshot).map(\.issue))
        return queue.rows.filter { !activeIDs.contains($0.id) }
    }

    /// The exact row count the Queue section renders — the sidebar
    /// badge's only source, deduped and free of terminal rows.
    static func renderedRowCount(snapshot: WireSnapshot?, queue: WireQueue?) -> Int {
        activeRuns(in: snapshot).count + readinessRows(queue: queue, snapshot: snapshot).count
    }
}
