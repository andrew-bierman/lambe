//
//  StallPolicy.swift
//  LambeDash
//
//  Silent stalling is the core operator pain, so liveness is decided in
//  one pure place: a non-terminal run whose last observed event is old is
//  "quiet" and then "stalled". The thresholds are client-side
//  presentation policy — the daemon reports only the event time.
//

import Foundation

/// How alive a run looks right now.
nonisolated enum RunLiveness: Equatable, Sendable {
    /// Events are arriving.
    case flowing
    /// No event for `StallPolicy.quietAfter`; worth a look.
    case quiet
    /// No event for `StallPolicy.stalledAfter`; probably stuck.
    case stalled
    /// Deferred on purpose (backoff / wake_at); silence is expected.
    case waiting
    /// A live run with no observed event time; cannot judge.
    case unobserved
}

nonisolated enum StallPolicy {
    static let quietAfter: TimeInterval = 120
    static let stalledAfter: TimeInterval = 300

    /// Seconds since the last event, nil when unobserved or unparseable.
    static func silence(lastEventAt: String?, now: Date) -> TimeInterval? {
        guard let date = RFC3339.date(lastEventAt) else { return nil }
        return max(0, now.timeIntervalSince(date))
    }

    static func classify(state: String, lastEventAt: String?, now: Date) -> RunLiveness {
        if state == "deferred" { return .waiting }
        guard let silent = silence(lastEventAt: lastEventAt, now: now) else { return .unobserved }
        if silent >= stalledAfter { return .stalled }
        if silent >= quietAfter { return .quiet }
        return .flowing
    }

    /// The runs among `runs` that classify as stalled at `now`.
    static func stalledRuns(_ runs: [WireRun], now: Date) -> [WireRun] {
        runs.filter { classify(state: $0.state, lastEventAt: $0.lastEventAt, now: now) == .stalled }
    }

    /// "7m", "1h 5m", "40s": compact silence text.
    static func format(_ seconds: TimeInterval) -> String {
        let s = Int(seconds)
        if s < 60 { return "\(s)s" }
        if s < 3600 { return "\(s / 60)m" }
        return "\(s / 3600)h \((s % 3600) / 60)m"
    }
}
