//
//  FeedModel.swift
//  LambeDash
//
//  The per-feature data holder (KTD7): one `FeedModel` per polled route —
//  snapshot, queue, history — each holding an `Equatable` payload plus a
//  phase derived from fetch outcomes. Identical consecutive payloads are
//  skipped at this boundary, so an unchanged poll costs zero view
//  invalidation. Failures keep the last good payload visible and record a
//  token-free reason — nothing is silently swallowed (R11).
//
//  `PresentationResolver` is the pure phase-to-content mapping the U8 test
//  table exercises: attached / degraded / disconnected / empty per view →
//  expected banner or list state.
//

import Foundation

/// The phase of one feature feed, independent of payload type.
nonisolated enum FeedPhase: Equatable, Sendable {
    /// No fetch has completed yet.
    case idle
    /// A fetch is in flight and no payload exists yet.
    case loading
    /// The last fetch produced a payload.
    case loaded
    /// The last fetch produced an empty payload (per-view emptiness is
    /// orthogonal to the connection state).
    case empty
    /// The last fetch failed; `payload` still holds the last good value
    /// when one existed.
    case failed
}

/// What a section view should render, given the connection and its feed.
nonisolated enum SectionPresentation: Equatable, Sendable {
    /// Render the content list.
    case content
    /// Render a `ContentUnavailableView`; the view supplies the per-section
    /// copy via `PresentationResolver.emptyCopy(for:)`.
    case empty
    /// Render a loading placeholder.
    case loading
    /// Render the disconnected banner; `hasPayload` dims the retained list
    /// beneath it instead of replacing it (R11).
    case disconnected(hasPayload: Bool)
    /// The fetch for this section alone failed while the daemon is
    /// otherwise attached.
    case sectionUnavailable
}

/// Inputs to the mapping, as plain values so the table test needs no
/// observable objects.
nonisolated struct SectionFacts: Equatable, Sendable {
    /// True when the connection state is attached or degraded.
    var connectionActive: Bool
    /// True when a connection attempt is in flight and nothing has
    /// attached yet (never-connected / loading lifecycle states).
    var connectionLoading: Bool
    var feedPhase: FeedPhase
    var hasPayload: Bool
}

/// The phase-to-content mapping, pure and exhaustive (U8 test table).
nonisolated enum PresentationResolver {
    static func resolve(_ facts: SectionFacts) -> SectionPresentation {
        // A dead or dead-in-progress connection dominates its own loading
        // placeholder, but not retained data (which stays visible, dimmed).
        if facts.connectionLoading {
            return facts.hasPayload
                ? .disconnected(hasPayload: true)
                : .loading
        }
        if !facts.connectionActive {
            return .disconnected(hasPayload: facts.hasPayload)
        }
        switch facts.feedPhase {
        case .idle, .loading:
            return facts.hasPayload ? .content : .loading
        case .empty:
            return .empty
        case .loaded:
            return .content
        case .failed:
            return .sectionUnavailable
        }
    }

    /// Per-view empty copy (R13: the Attention copy carries the read-only
    /// posture).
    static func emptyCopy(for section: SidebarSection) -> (title: String, detail: String) {
        switch section {
        case .queue:
            ("Nothing queued", "No readiness rows — nothing is queued or blocked right now.")
        case .attention:
            ("Nothing needs a human", "No items need attention. Read-only — resolve items in bd or the tracker.")
        case .history:
            ("No finished runs yet", "Completed and archived runs will appear here.")
        }
    }
}

/// One sidebar destination.
nonisolated enum SidebarSection: String, CaseIterable, Identifiable, Sendable {
    case queue, attention, history
    var id: String { rawValue }

    var title: String {
        switch self {
        case .queue: "Queue"
        case .attention: "Attention"
        case .history: "History"
        }
    }

    var systemImage: String {
        switch self {
        case .queue: "list.bullet"
        case .attention: "exclamationmark.triangle"
        case .history: "clock.arrow.circlepath"
        }
    }
}

/// Observable holder for one polled route's payload (KTD7).
@MainActor
@Observable
final class FeedModel<Payload: Equatable> {
    /// The last good payload, retained across failures (R11).
    private(set) var payload: Payload?
    private(set) var phase: FeedPhase = .idle
    /// Token-free reason for the last failure, when one failed.
    private(set) var lastError: String?

    /// True when a payload exists.
    var hasPayload: Bool { payload != nil }

    /// Applies a successful fetch. An identical payload is a no-op —
    /// the Equatable short-circuit that keeps unchanged polls from
    /// invalidating views.
    /// - Returns: whether anything changed.
    @discardableResult
    func apply(_ newPayload: Payload, isEmpty: (Payload) -> Bool) -> Bool {
        lastError = nil
        if payload == newPayload {
            phase = isEmpty(newPayload) ? .empty : .loaded
            return false
        }
        payload = newPayload
        phase = isEmpty(newPayload) ? .empty : .loaded
        return true
    }

    /// Records a failed fetch. The last good payload is kept, not dropped
    /// (R11), and the failure is recorded — never swallowed.
    func fail(_ reason: String) {
        lastError = reason
        phase = .failed
    }

    /// Marks a fetch in flight when nothing has loaded yet.
    func beginLoading() {
        if payload == nil, phase == .idle {
            phase = .loading
        }
    }

    /// Drops all state: used when the connection target or mode changes,
    /// so a stale payload from another daemon never lingers.
    func reset() {
        payload = nil
        phase = .idle
        lastError = nil
    }
}
