//
//  StatusViews.swift
//  LambeDash
//
//  The explicit connection states (R11) rendered once and reused: the
//  banner row (newer daemon, degraded, disconnected, ambiguous), the
//  disconnected overlays, and the window footer carrying the attached
//  daemon's identity and version (R9, R14) plus the read-only posture
//  (R13). Error text comes only from `APIError.displayText` and the
//  connection state — no token, no reflected strings.
//

import SwiftUI

/// The banner row shown above the content column. Banners state facts,
/// never hide content: degraded and disconnected keep the last good data
/// visible below.
struct ConnectionBanners: View {
    @Environment(ConnectionModel.self) private var connection

    var body: some View {
        VStack(spacing: 0) {
            if connection.newerDaemon {
                banner(
                    icon: "arrow.up.circle",
                    color: .yellow,
                    text: "This daemon reports a newer schema (v\(newerSchemaVersion)) than the app supports. Some information may be missing."
                )
            }
            if case .degraded(let sections) = connection.state {
                banner(
                    icon: "clock.badge.exclamationmark",
                    color: .orange,
                    text: "Tracker data as of \(staleAsOfText) — \(sections.joined(separator: ", ")) awaiting the daemon's next read."
                )
            }
            if case .ambiguous(let names) = connection.state {
                banner(
                    icon: "square.stack.3d.up",
                    color: .orange,
                    text: "That state directory resolves to \(names.count) running daemons (\(names.joined(separator: ", "))). Pick an exact instance in Settings."
                )
            }
        }
    }

    private var newerSchemaVersion: Int {
        connection.snapshotFeed.payload?.schemaVersion ?? WireContract.schemaVersion + 1
    }

    /// The stalest known section timestamp — the fact behind the degraded
    /// banner (R11: stale `as_of` shown, never silently mixed).
    private var staleAsOfText: String {
        let candidates = [
            connection.snapshotFeed.payload?.needsHumanAsOf,
            connection.queueFeed.payload?.asOf,
            connection.historyFeed.payload?.asOf,
        ]
        let dates = candidates.compactMap { RFC3339.date($0) }
        guard let stalest = dates.min() else { return "an unknown time" }
        return stalest.formatted(date: .abbreviated, time: .shortened)
    }

    private func banner(icon: String, color: Color, text: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Image(systemName: icon)
                .foregroundStyle(color)
                .accessibilityHidden(true)
            Text(text)
                .font(.footnote)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(color.opacity(0.12))
        .accessibilityElement(children: .combine)
    }
}

/// Full-area disconnected state: nothing was ever loaded.
struct DisconnectedPlaceholder: View {
    @Environment(ConnectionModel.self) private var connection

    var body: some View {
        VStack(spacing: 14) {
            if case .neverConnected(let reason) = connection.state {
                ContentUnavailableView(
                    "Not connected",
                    systemImage: "antenna.slash",
                    description: Text(reason.detail)
                )
            } else {
                ContentUnavailableView(
                    "Disconnected",
                    systemImage: "antenna.slash",
                    description: Text(disconnectReason)
                )
            }
            HStack(spacing: 12) {
                Button("Retry now") {
                    connection.refreshNow()
                }
                SettingsLink {
                    Text("Choose instance…")
                }
            }
            .buttonStyle(.bordered)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .accessibilityElement(children: .contain)
    }

    private var disconnectReason: String {
        if case .disconnected(let reason) = connection.state {
            return reason
        }
        return "The daemon is not answering. Last good data is kept; the app retries with backoff."
    }
}

/// The floating retry card over dimmed retained data.
struct DisconnectedOverlay: View {
    var body: some View {
        Text("Disconnected — showing the last good data. The app retries automatically.")
            .font(.footnote)
            .padding(.horizontal, 14)
            .padding(.vertical, 8)
            .background(.regularMaterial, in: Capsule())
            .accessibilityElement(children: .combine)
    }
}

// MARK: - Badges

/// Coarse state badge for active runs.
struct StateBadge: View {
    let state: String
    let phase: String?

    var body: some View {
        Text(display)
            .font(.caption2.weight(.medium))
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(tone.color.opacity(0.15), in: Capsule())
            .foregroundStyle(tone.color)
            .accessibilityLabel("state: \(display)")
    }

    private var display: String { phase ?? state }
    private var tone: OutcomeTone { OutcomeTone.tone(state: state, phase: phase, outcome: nil) }
}

/// Ending badge for history rows.
struct OutcomeBadge: View {
    let phase: String?
    let state: String?
    let outcome: String?

    var body: some View {
        Text(display)
            .font(.caption2.weight(.medium))
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(tone.color.opacity(0.15), in: Capsule())
            .foregroundStyle(tone.color)
            .accessibilityLabel("outcome: \(display)")
    }

    private var display: String { outcome ?? phase ?? state ?? "—" }
    private var tone: OutcomeTone { OutcomeTone.tone(state: state, phase: phase, outcome: outcome) }
}

/// How an ending should read, pure and testable. Unknown endings are
/// neutral — never styled as success or failure.
nonisolated enum OutcomeTone: Equatable, Sendable {
    case good, warning, bad, neutral

    var color: Color {
        switch self {
        case .good: .green
        case .warning: .orange
        case .bad: .red
        case .neutral: .secondary
        }
    }

    static func tone(state: String?, phase: String?, outcome: String?) -> OutcomeTone {
        let text = (outcome ?? phase ?? state ?? "").lowercased()
        if text.contains("green") || text.contains("settled") { return .good }
        if text.contains("red") || text.contains("failed") { return .bad }
        if text.contains("interrupted") || text.contains("retry") || text.contains("deferred")
            || text.contains("canceled") || text.contains("cancelled")
        {
            return .warning
        }
        if text.contains("running") || text.contains("gating") { return .neutral }
        return .neutral
    }
}

// MARK: - Time

/// A machine-readable RFC 3339 timestamp rendered as a live relative age
/// (R9: machine-readable accompanies every human age). Renders "unknown"
/// when the field is absent — never a fabricated time.
struct RelativeTimeText: View {
    var label: String = ""
    let iso: String?

    var body: some View {
        Group {
            if let date = RFC3339.date(iso) {
                Text("\(labelPrefix)\(date, style: .relative) ago")
            } else {
                Text("time unknown")
            }
        }
        .font(.caption.monospacedDigit())
        .foregroundStyle(.secondary)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(accessibilityText)
    }

    /// The label with its separating space, empty when unlabeled.
    private var labelPrefix: String {
        label.isEmpty ? "" : label + " "
    }

    private var accessibilityText: String {
        if let date = RFC3339.date(iso) {
            let absolute = date.formatted(date: .abbreviated, time: .standard)
            let prefix = label.isEmpty ? "" : label + " "
            return prefix + "at \(absolute)"
        }
        return "time unknown"
    }
}

// MARK: - Footer

/// The window footer: connection state, attached daemon identity (R14),
/// last sync, and the read-only posture (R13).
struct StatusFooter: View {
    @Environment(ConnectionModel.self) private var connection

    var body: some View {
        HStack(spacing: 10) {
            Circle()
                .fill(stateTone.color)
                .frame(width: 8, height: 8)
                .accessibilityHidden(true)
            Text(stateText)
                .font(.caption)
            if connection.mode == .demo {
                Text("Demo data — not connected to any daemon")
                    .font(.caption)
                    .foregroundStyle(.orange)
            } else if let daemon = connection.daemon {
                Text(identityText(daemon))
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
            Spacer()
            if let lastSync = connection.lastSync, connection.mode == .live {
                Text("updated")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Text(lastSync, style: .relative)
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
            Text("Read-only — resolve in bd / the tracker")
                .font(.caption2)
                .foregroundStyle(.tertiary)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
        .background(.bar)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(accessibilityText)
    }

    private var stateText: String {
        if connection.mode == .demo {
            return "Demo"
        }
        switch connection.state {
        case .neverConnected: return "Not connected"
        case .loading: return "Connecting…"
        case .attached: return "Attached"
        case .degraded: return "Degraded"
        case .disconnected: return "Disconnected"
        case .ambiguous: return "Ambiguous instance"
        }
    }

    private var stateTone: OutcomeTone {
        if connection.mode == .demo {
            return .neutral
        }
        switch connection.state {
        case .attached: return .good
        case .degraded, .ambiguous: return .warning
        case .disconnected: return .bad
        case .loading: return .neutral
        case .neverConnected: return .neutral
        }
    }

    /// Identity facts only — the descriptor's token never leaves the
    /// client, so the footer physically cannot show it (R15).
    private func identityText(_ daemon: WireDaemonIdentity) -> String {
        var parts: [String] = []
        if let machine = daemon.machine, !machine.isEmpty {
            parts.append(machine)
        }
        parts.append(daemon.versionPidText)
        return parts.joined(separator: " · ")
    }

    private var accessibilityText: String {
        var parts = ["Connection: \(stateText)"]
        if connection.mode == .demo {
            parts.append("Demo sample data, not connected to any daemon")
        } else if let daemon = connection.daemon {
            parts.append("Attached to \(identityText(daemon))")
        }
        parts.append("The app is read-only; resolve items in bd or the tracker")
        return parts.joined(separator: ". ")
    }
}
