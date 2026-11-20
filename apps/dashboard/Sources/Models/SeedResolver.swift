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
    /// Queue rows: whether the row is blocked from dispatch.
    let blocked: Bool
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
        if let run = snapshot?.runs.first(where: { $0.issue == issue }) {
            return RunSeed(
                issue: run.issue,
                title: nil,
                phase: run.phase,
                blocked: false
            )
        }
        if let row = queue?.rows.first(where: { $0.id == issue }) {
            return RunSeed(
                issue: row.id,
                title: row.title,
                phase: nil,
                blocked: row.blocked
            )
        }
        if let run = history?.runs.first(where: { $0.issue == issue }) {
            return RunSeed(
                issue: run.issue,
                title: run.title,
                phase: run.phase,
                blocked: false
            )
        }
        // The ID the operator selected is still honored even when no
        // current payload mentions it — the detail fetch decides whether
        // the daemon knows it.
        return RunSeed(issue: issue, title: nil, phase: nil, blocked: false)
    }
}
