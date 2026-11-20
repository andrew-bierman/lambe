//
//  SeedResolver.swift
//  LambeDash
//
//  Selection support (U8): the facts a section row already carries about
//  an issue, resolved by ID from the current payloads. The detail column
//  renders these immediately and upgrades to the fetched
//  `GET /api/v1/{issue}` body when it arrives — so selection survives
//  reordering polls (identity, not index) and history rows whose live
//  record is gone still show what the archive knows.
//

import Foundation

/// The row-level facts known about one issue before (or without) a live
/// detail fetch.
nonisolated struct RunSeed: Equatable, Sendable {
    let issue: String
    let title: String?
    /// Operator-facing phase, when the row carried one.
    let phase: String?
    /// Coarse state name, when the row carried one.
    let state: String?
    let attempt: Int
    /// History rows: when the attempt was last observed (RFC 3339).
    let updatedAt: String?
    /// Queue rows: whether the row is blocked from dispatch.
    let blocked: Bool
    /// Queue rows: the readiness reason the row carried
    /// ("blocked by 2 issues"), if any.
    var queueReason: String?
    /// Attention rows: the recorded needs-human reason and detail (R3),
    /// attached from the snapshot whichever section resolved the row —
    /// the facts that make a pre-run selection useful.
    var attentionReason: String?
    var attentionDetail: String?
    /// Attention rows: when the item was raised (RFC 3339), if recorded.
    var attentionSince: String?
    /// The newest gate rejection for this issue (R3): kind, reason, and
    /// when it fired — each nil when the record does not carry it.
    var gate: String?
    var gateReason: String?
    var gateAt: String?
}

/// Resolves the seed for an issue ID across payloads. Lookup is by ID —
/// poll order changes cannot move or drop the operator's selection.
nonisolated enum SeedResolver {
    static func resolve(
        issue: String?,
        snapshot: WireSnapshot?,
        queue: WireQueue?,
        history: WireHistory?
    ) -> RunSeed? {
        guard let issue else { return nil }
        var seed = primarySeed(
            issue: issue, snapshot: snapshot, queue: queue, history: history
        )
        // The snapshot's attention sections carry facts about this issue
        // no matter which section resolved the row (R3): the recorded
        // needs-human reason and the newest gate rejection — the gate
        // history is oldest-first, so the last match is the newest.
        let attention = snapshot?.needsHuman.first(where: { $0.issue == issue })
        seed.attentionReason = attention?.reason
        seed.attentionDetail = attention?.detail
        seed.attentionSince = attention?.since
        let rejection = snapshot?.gateRejections.last(where: { $0.issue == issue })
        seed.gate = rejection?.gate
        seed.gateReason = rejection?.reason
        seed.gateAt = rejection?.at
        return seed
    }

    /// The facts of the section row that renders this issue: a snapshot
    /// run, a queue row, a history row, or — when no current payload
    /// mentions the ID — the ID itself, so the detail fetch decides
    /// whether the daemon knows it.
    private static func primarySeed(
        issue: String,
        snapshot: WireSnapshot?,
        queue: WireQueue?,
        history: WireHistory?
    ) -> RunSeed {
        if let run = snapshot?.runs.first(where: { $0.issue == issue }) {
            return RunSeed(
                issue: run.issue,
                title: nil,
                phase: run.phase,
                state: run.state,
                attempt: run.attempt,
                updatedAt: run.lastEventAt,
                blocked: false
            )
        }
        if let row = queue?.rows.first(where: { $0.id == issue }) {
            return RunSeed(
                issue: row.id,
                title: row.title,
                phase: nil,
                state: nil,
                attempt: 0,
                updatedAt: nil,
                blocked: row.blocked,
                queueReason: row.reason
            )
        }
        if let run = history?.runs.first(where: { $0.issue == issue }) {
            return RunSeed(
                issue: run.issue,
                title: run.title,
                phase: run.phase,
                state: run.state,
                attempt: run.attempt,
                updatedAt: run.updatedAt,
                blocked: false
            )
        }
        return RunSeed(
            issue: issue, title: nil, phase: nil, state: nil,
            attempt: 0, updatedAt: nil, blocked: false
        )
    }
}
