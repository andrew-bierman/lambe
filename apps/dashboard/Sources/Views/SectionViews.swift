//
//  SectionViews.swift
//  LambeDash
//
//  The three operator sections (U8): Queue (active runs plus readiness
//  rows, R2), Attention (needs-human items with their real reason and
//  waiting time, plus recent gate rejections, R3), History (terminal and
//  restart-archived runs, R4). Every section maps its connection state and
//  feed phase through `PresentationResolver` — the same explicit states
//  everywhere, never silently mixed data (R11). Read-only: selecting a row
//  opens detail; there are no mutation controls anywhere (R13).
//

import SwiftUI

/// Shared scaffolding for one section: maps the presentation and hosts the
/// content list under the section banners.
struct SectionContainer<Content: View>: View {
    @Environment(ConnectionModel.self) private var connection
    let section: SidebarSection
    @ViewBuilder let content: (SectionPresentation) -> Content

    var body: some View {
        let facts = SectionFacts(
            connectionActive: connection.isConnectionActive,
            connectionLoading: connection.isConnectionLoading,
            feedPhase: phase(for: section),
            hasPayload: hasPayload(for: section)
        )
        let presentation = PresentationResolver.resolve(facts)
        Group {
            switch presentation {
            case .content:
                content(presentation)
            case .sectionUnavailable:
                // This section's route failed while the daemon is otherwise
                // attached: keep retained data visible and state the
                // failure — never silently swallowed (R11).
                VStack(spacing: 0) {
                    if let error = lastError {
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            Image(systemName: "wifi.exclamationmark")
                                .foregroundStyle(.orange)
                                .accessibilityHidden(true)
                            Text("\(section.title) refresh failed: \(error) — retrying automatically.")
                                .font(.footnote)
                        }
                        .padding(.horizontal, 12)
                        .padding(.vertical, 6)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .background(Color.orange.opacity(0.12))
                        .accessibilityElement(children: .combine)
                    }
                    content(presentation)
                }
            case .empty:
                let copy = PresentationResolver.emptyCopy(for: section)
                ContentUnavailableView(copy.title, systemImage: section.systemImage, description: Text(copy.detail))
            case .loading:
                VStack(spacing: 14) {
                    ProgressView()
                        .controlSize(.large)
                    Text("Reading daemon state…")
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            case .disconnected(let hasPayload):
                if hasPayload {
                    // Keep the last good data visible, dimmed under the
                    // banner (R11) — never silently mixed: the banner
                    // states exactly what is stale.
                    content(presentation)
                        .opacity(0.45)
                        .overlay {
                            DisconnectedOverlay()
                                .padding(.vertical, 24)
                        }
                } else {
                    DisconnectedPlaceholder()
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
    }

    private func phase(for section: SidebarSection) -> FeedPhase {
        switch section {
        case .queue: connection.queueFeed.phase
        case .attention: connection.snapshotFeed.phase
        case .history: connection.historyFeed.phase
        }
    }

    private func hasPayload(for section: SidebarSection) -> Bool {
        switch section {
        case .queue: connection.queueFeed.hasPayload
        case .attention: connection.snapshotFeed.hasPayload
        case .history: connection.historyFeed.hasPayload
        }
    }

    private var lastError: String? {
        switch section {
        case .queue: connection.queueFeed.lastError
        case .attention: connection.snapshotFeed.lastError
        case .history: connection.historyFeed.lastError
        }
    }
}

// MARK: - Queue

/// Active supervised runs on top, readiness rows for every configured
/// label below (R2). What renders — and therefore what the sidebar badge
/// counts — is `QueueComposition`'s decision: terminal snapshot rows
/// belong to History (R4) and never duplicate a readiness row.
struct QueueView: View {
    @Environment(ConnectionModel.self) private var connection
    @Binding var selectedIssue: String?

    var body: some View {
        SectionContainer(section: .queue) { _ in
            VStack(spacing: 0) {
                StalledBanner(runs: QueueComposition.activeRuns(in: connection.snapshotFeed.payload))
                List(selection: $selectedIssue) {
                    let active = QueueComposition.activeRuns(in: connection.snapshotFeed.payload)
                    let titles = QueueComposition.titles(
                        queue: connection.queueFeed.payload, history: connection.historyFeed.payload
                    )
                    if !active.isEmpty {
                        Section("Active runs") {
                            ForEach(active) { run in
                                ActiveRunRow(run: run, title: titles[run.issue])
                                    .tag(run.issue)
                                    .runContextMenu(run.issue)
                            }
                        }
                    }
                    let readiness = QueueComposition.readinessRows(
                        queue: connection.queueFeed.payload,
                        snapshot: connection.snapshotFeed.payload
                    )
                    if !readiness.isEmpty {
                        Section("Readiness") {
                            ForEach(readiness) { row in
                                QueueRowView(row: row)
                                    .tag(row.id)
                                    .runContextMenu(row.id)
                            }
                        }
                    }
                }
                .listStyle(.inset)
            }
        }
    }
}

/// One active run row: issue, phase badge, attempt, last event age. A
/// stalled run is unmistakable: red bar, red tint, and a STALLED badge
/// carrying how long it has been silent. Liveness re-evaluates every 15 s
/// even when the poll payload is unchanged.
struct ActiveRunRow: View {
    let run: WireRun
    /// The issue's title, when a payload carries one.
    var title: String?
    /// Test/preview seam; live rows use the wall clock.
    var fixedNow: Date?

    var body: some View {
        TimelineView(.periodic(from: .now, by: 15)) { context in
            row(now: fixedNow ?? context.date)
        }
    }

    private func row(now: Date) -> some View {
        let liveness = StallPolicy.classify(state: run.state, lastEventAt: run.lastEventAt, now: now)
        let silence = StallPolicy.silence(lastEventAt: run.lastEventAt, now: now)
        return HStack(alignment: .top, spacing: 8) {
            if liveness == .stalled {
                RoundedRectangle(cornerRadius: 2)
                    .fill(Color.red)
                    .frame(width: 4)
                    .accessibilityHidden(true)
            }
            VStack(alignment: .leading, spacing: 4) {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text(title ?? run.issue)
                        .font(.body.weight(.medium))
                        .lineLimit(2)
                        .truncationMode(.middle)
                    Spacer(minLength: 4)
                    LivenessBadge(liveness: liveness, silence: silence)
                }
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    if title != nil {
                        Text(run.issue)
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .truncationMode(.middle)
                    }
                    StateBadge(state: run.state, phase: run.phase)
                    Text("attempt \(run.attempt)")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Spacer(minLength: 4)
                    Text("last event \(run.lastEventAge)")
                        .font(.caption.monospacedDigit())
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
                if run.workflow != nil || run.machine != nil {
                    HStack(spacing: 6) {
                        if let workflow = run.workflow { MetaChip(text: workflow, systemImage: "arrow.triangle.branch") }
                        if let machine = run.machine { MetaChip(text: machine, systemImage: "desktopcomputer") }
                    }
                }
            }
        }
        .padding(.vertical, 3)
        .padding(.horizontal, liveness == .stalled ? 4 : 0)
        .background(liveness == .stalled ? Color.red.opacity(0.10) : .clear, in: RoundedRectangle(cornerRadius: 6))
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(
            "\(liveness == .stalled ? "Stalled run" : "Run") \(title.map { "\($0), " } ?? "")\(run.issue), \(run.phase ?? run.state), attempt \(run.attempt), last event \(run.lastEventAge)\(run.machine.map { ", on \($0)" } ?? "")"
        )
    }
}

/// One readiness row: dispatchable, or blocked with reason and blockers.
struct QueueRowView: View {
    let row: WireQueueRow

    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            Image(systemName: row.blocked ? "lock.fill" : "checkmark.circle")
                .foregroundStyle(row.blocked ? Color.orange : Color.green)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 3) {
                Text(row.title ?? row.id)
                    .font(.body.weight(.medium))
                Text(reason)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                if !row.blockers.isEmpty {
                    Text("blocked by " + row.blockers.map(\.id).joined(separator: ", "))
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
            }
            Spacer()
            Text(row.id)
                .font(.caption.monospaced())
                .foregroundStyle(.tertiary)
        }
        .padding(.vertical, 2)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(accessibilityText)
    }

    private var reason: String {
        if let reason = row.reason, !reason.isEmpty {
            return reason
        }
        return row.blocked ? "blocked" : "ready"
    }

    private var accessibilityText: String {
        let base = row.blocked
            ? "Blocked task \(row.title ?? row.id)"
            : "Ready task \(row.title ?? row.id)"
        let blockers = row.blockers.isEmpty
            ? ""
            : ", blocked by \(row.blockers.map(\.id).joined(separator: ", "))"
        return base + ". " + reason + "." + blockers
    }
}

// MARK: - Attention

/// Needs-human items first, recent gate rejections below (R3).
struct AttentionView: View {
    @Environment(ConnectionModel.self) private var connection
    @Binding var selectedIssue: String?

    var body: some View {
        SectionContainer(section: .attention) { presentation in
            List(selection: $selectedIssue) {
                if let snapshot = connection.snapshotFeed.payload {
                    if !snapshot.needsHuman.isEmpty {
                        Section("Needs human") {
                            ForEach(snapshot.needsHuman) { item in
                                NeedsHumanRow(item: item)
                                    .tag(item.issue)
                                    .runContextMenu(item.issue)
                            }
                        }
                    }
                    if !snapshot.gateRejections.isEmpty {
                        Section("Recent gate rejections") {
                            ForEach(Array(snapshot.gateRejections.enumerated()), id: \.offset) { _, rejection in
                                GateRejectionRow(rejection: rejection)
                                    .tag(rejection.issue)
                                    .runContextMenu(rejection.issue)
                            }
                        }
                    }
                }
            }
            .listStyle(.inset)
        }
    }
}

/// One needs-human item: the real reason, the wait, the detail (R3, R10).
struct NeedsHumanRow: View {
    let item: WireNeedsHuman

    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(.orange)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 3) {
                Text(item.issue)
                    .font(.body.weight(.medium))
                    .monospacedDigit()
                Text(item.reason)
                    .font(.footnote)
                if let detail = item.detail, !detail.isEmpty {
                    Text(detail)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            Spacer()
            VStack(alignment: .trailing, spacing: 3) {
                if RFC3339.date(item.since) != nil {
                    RelativeTimeText(label: "waiting", iso: item.since)
                } else {
                    Text("waiting — time unknown")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Text(item.issue)
                    .font(.caption.monospaced())
                    .foregroundStyle(.tertiary)
            }
        }
        .padding(.vertical, 2)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(accessibilityText)
    }

    private var accessibilityText: String {
        let wait = RFC3339.date(item.since) != nil
            ? "waiting since \(item.since ?? "unknown")"
            : "waiting, time unknown"
        return "Needs human: \(item.issue). \(item.reason). \(wait)"
    }
}

/// One gate rejection. Kind and time render only when the record actually
/// carries them — explicitly "unknown" otherwise, never fabricated (R10).
struct GateRejectionRow: View {
    let rejection: WireGateRejection

    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            VStack(alignment: .leading, spacing: 3) {
                Text(rejection.reason)
                    .font(.footnote)
                Text(gateLine)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Text(rejection.issue)
                .font(.caption.monospaced())
                .foregroundStyle(.tertiary)
        }
        .padding(.vertical, 2)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(
            "Gate rejection on \(rejection.issue): \(rejection.reason). \(gateLine)"
        )
    }

    private var gateLine: String {
        let gate = rejection.gate ?? "gate kind unknown"
        if RFC3339.date(rejection.at) != nil {
            return "\(gate)"
        }
        return "\(gate), time unknown"
    }
}

// MARK: - History

/// Terminal and restart-archived runs, newest first (R4).
struct HistoryView: View {
    @Environment(ConnectionModel.self) private var connection
    @Binding var selectedIssue: String?

    var body: some View {
        SectionContainer(section: .history) { _ in
            List(selection: $selectedIssue) {
                if let history = connection.historyFeed.payload {
                    Section("Archived attempts (not running)") {
                        ForEach(history.runs) { run in
                            HistoryRow(run: run)
                                .tag(run.issue)
                                .runContextMenu(run.issue)
                        }
                    }
                }
            }
            .listStyle(.inset)
        }
    }
}

/// One history row: title and ending on the first line, attempt and age
/// below. Two lines so a narrow column never wraps a badge mid-word.
struct HistoryRow: View {
    let run: WireHistoryRun

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(run.title ?? run.issue)
                    .font(.body.weight(.medium))
                    .lineLimit(2)
                Spacer(minLength: 4)
                OutcomeBadge(phase: run.phase, state: run.state, outcome: run.outcome)
            }
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(run.phase ?? run.state ?? "")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                Spacer(minLength: 4)
                Text("attempt \(run.attempt)")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                if RFC3339.date(run.updatedAt) != nil {
                    RelativeTimeText(label: "", iso: run.updatedAt)
                        .lineLimit(1)
                } else {
                    Text("time unknown")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .padding(.vertical, 2)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(accessibilityText)
    }

    private var accessibilityText: String {
        let ending = run.phase ?? run.state ?? "unknown ending"
        let when = RFC3339.date(run.updatedAt) != nil
            ? ", last observed \(run.updatedAt ?? "")"
            : ", time unknown"
        return "Archived run \(run.title ?? run.issue): \(ending), attempt \(run.attempt)\(when)"
    }
}
