//
//  RunDetailView.swift
//  LambeDash
//
//  The detail column (U8): the selected run's facts at parity with the
//  TUI inspector (R5) — phase, state, attempt, turn, thread, workspace,
//  workflow, wake-at, session, last gate preview, and the bounded output
//  tail. Seed facts render immediately from the selected row — the
//  recorded attention reason, the newest gate rejection, and the queue
//  block included, so a selection before its first run is already
//  useful; the fetched `GET /api/v1/{issue}` body upgrades the column
//  when it arrives. Truthful about absence: an issue with no live record
//  (a 404, an archived history row) shows the seed facts and says so; a
//  failed fetch shows the seed facts and the failure — nothing silently
//  swallowed (R11).
//

import AppKit
import SwiftUI

struct RunDetailView: View {
    @Environment(ConnectionModel.self) private var connection
    @Environment(ShellModel.self) private var shell
    let selectedIssue: String?
    let seed: RunSeed?

    var body: some View {
        Group {
            if let seed {
                loaded(seed)
            } else {
                ContentUnavailableView(
                    "No run selected",
                    systemImage: "cursorarrow.motionlines",
                    description: Text("Select a run in Queue, Attention, or History.")
                )
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
    }

    @ViewBuilder
    private func loaded(_ seed: RunSeed) -> some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                header(seed)
                liveness(seed)
                RunActionsSection(context: shell.context(for: seed.issue, connection: connection))
                statusBlock(seed)
                seedFacts(seed)
                if let issue = connection.detail.issue {
                    factsGrid(issue)
                    if let lastGate = issue.lastGate, !lastGate.isEmpty {
                        gatePreview(lastGate)
                    }
                    if let tail = issue.tail, !tail.isEmpty {
                        LiveOutputView(
                            tail: tail,
                            truncated: issue.tailTruncated,
                            following: connection.mode == .live && QueueComposition.isActive(issue.state)
                        )
                    }
                }
            }
            .padding(20)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .task(id: seed.issue) {
            // One fetch per selection; changing selection cancels the
            // previous fetch (single in-flight detail, cancellation-aware).
            await connection.detail.load(
                id: seed.issue,
                fetch: connection.detailFetch(id: seed.issue)
            )
            // Follow a live run: re-read its detail in place every 2 s so
            // the output tail moves without a loading flash.
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(2))
                guard connection.mode == .live, connection.isConnectionActive,
                      let state = connection.detail.issue?.state, QueueComposition.isActive(state)
                else { continue }
                await connection.detail.refresh(
                    id: seed.issue, fetch: connection.detailFetch(id: seed.issue)
                )
            }
        }
    }

    // MARK: - Blocks

    private func header(_ seed: RunSeed) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                Text(seed.issue)
                    .font(.title3.weight(.semibold).monospaced())
                    .lineLimit(1)
                    .truncationMode(.middle)
                StandingBadge(standing: seed.standing)
            }
            if let title = seed.title, !title.isEmpty {
                Text(title)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
            let context = shell.context(for: seed.issue, connection: connection)
            let issue = connection.detail.loadedID == seed.issue ? connection.detail.issue : nil
            let run = connection.snapshotFeed.payload?.runs.first { $0.issue == seed.issue }
            let workflow = issue?.workflow ?? run?.workflow
            let machine = issue?.machine ?? run?.machine
            if workflow != nil || machine != nil {
                HStack(spacing: 6) {
                    if let workflow { MetaChip(text: workflow, systemImage: "arrow.triangle.branch") }
                    if let machine {
                        MetaChip(text: machine, systemImage: "desktopcomputer", emphasized: context.remoteHost != nil)
                    }
                }
            }
        }
    }

    /// The stalled/quiet callout for a live run, from its last event time.
    @ViewBuilder
    private func liveness(_ seed: RunSeed) -> some View {
        if seed.standing == .live {
            TimelineView(.periodic(from: .now, by: 15)) { context in
                let at = connection.detail.issue?.lastEventAt ?? seed.updatedAt
                let state = connection.detail.issue?.state ?? seed.state ?? ""
                let kind = StallPolicy.classify(state: state, lastEventAt: at, now: context.date)
                if kind == .stalled || kind == .quiet {
                    let silence = StallPolicy.silence(lastEventAt: at, now: context.date)
                    HStack(spacing: 8) {
                        LivenessBadge(liveness: kind, silence: silence)
                        Text(kind == .stalled
                            ? "The agent has produced no events. Open the run to see what it is stuck on."
                            : "The agent has gone quiet.")
                            .font(.footnote)
                    }
                    .padding(10)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background((kind == .stalled ? Color.red : Color.orange).opacity(0.12), in: RoundedRectangle(cornerRadius: 8))
                    .accessibilityElement(children: .combine)
                }
            }
        }
    }

    @ViewBuilder
    private func statusBlock(_ seed: RunSeed) -> some View {
        switch connection.detail.phase {
        case .idle:
            EmptyView()
        case .loading:
            HStack(spacing: 8) {
                ProgressView()
                    .controlSize(.small)
                Text("Reading live detail…")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
            .accessibilityElement(children: .combine)
        case .seedOnly:
            if connection.isConnectionActive {
                note("The daemon has no live record for this issue — showing the facts the section list carries.")
            } else {
                note("Not connected — showing the facts the section list carries. The daemon's live detail returns when the connection recovers.")
            }
        case .failed(let reason):
            VStack(alignment: .leading, spacing: 8) {
                note(reason)
                Button("Retry") {
                    Task {
                        await connection.detail.load(
                            id: seed.issue,
                            fetch: connection.detailFetch(id: seed.issue)
                        )
                    }
                }
                .buttonStyle(.bordered)
                .accessibilityLabel("Retry fetching detail for \(seed.issue)")
            }
        case .loaded:
            EmptyView()
        }
    }

    private func note(_ text: String) -> some View {
        Text(text)
            .font(.footnote)
            .foregroundStyle(.secondary)
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 8))
            .accessibilityElement(children: .combine)
    }

    /// The facts the section row already carries (R3): the recorded
    /// needs-human reason, the newest gate rejection, and the queue
    /// block. Rendered from the seed alone, so the column is useful
    /// before a run exists — the fetched live body never carries these.
    @ViewBuilder
    private func seedFacts(_ seed: RunSeed) -> some View {
        if seed.attentionReason != nil || seed.gateReason != nil || seed.blocked {
            VStack(alignment: .leading, spacing: 10) {
                if let reason = seed.attentionReason {
                    seedBlock(title: "Needs human") {
                        Text(reason)
                            .font(.footnote)
                        if let detail = seed.attentionDetail, !detail.isEmpty {
                            Text(detail)
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                        RelativeTimeText(label: "waiting", iso: seed.attentionSince)
                    }
                }
                if let gateReason = seed.gateReason {
                    seedBlock(title: "Latest gate rejection") {
                        Text(gateReason)
                            .font(.footnote)
                        Text(gateLine(seed))
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                if seed.blocked {
                    seedBlock(title: "Blocked from dispatch") {
                        Text(seed.queueReason ?? "blocked")
                            .font(.footnote)
                    }
                }
            }
        }
    }

    /// One seed-fact block, styled like the status note.
    private func seedBlock<V: View>(
        title: String, @ViewBuilder content: () -> V
    ) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title)
                .font(.caption)
                .foregroundStyle(.secondary)
            content()
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 8))
        .accessibilityElement(children: .combine)
    }

    /// The gate kind and time, truthful about absence (R10) — the same
    /// posture the Attention section's rejection rows render.
    private func gateLine(_ seed: RunSeed) -> String {
        let gate = seed.gate ?? "gate kind unknown"
        if RFC3339.date(seed.gateAt) != nil {
            return gate
        }
        return "\(gate), time unknown"
    }

    private func factsGrid(_ issue: WireIssue) -> some View {
        let facts: [(String, String?)] = [
            ("State", issue.state),
            ("Phase", issue.phase),
            ("Attempt", issue.attempt > 0 ? String(issue.attempt) : nil),
            ("Turn", issue.turn > 0 ? String(issue.turn) : nil),
            ("Session", issue.session),
            ("Thread", issue.thread),
            ("Workflow", issue.workflow),
            ("Wake at", formattedWake(issue.wakeAt)),
            ("Last event", issue.lastEventAt != nil ? "\(issue.lastEventAge) (\(issue.lastEventAt ?? ""))" : issue.lastEventAge),
            ("Summary", issue.summary),
        ]
        let visible = facts.filter { ($0.1 ?? "").isEmpty == false }
        return VStack(alignment: .leading, spacing: 0) {
            ForEach(visible, id: \.0) { name, value in
                HStack(alignment: .firstTextBaseline) {
                    Text(name)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .frame(width: 92, alignment: .leading)
                    Text(value ?? "")
                        .font(.footnote.monospaced())
                        .textSelection(.enabled)
                    Spacer(minLength: 0)
                }
                .padding(.vertical, 4)
                Divider()
            }
        }
        .background(
            RoundedRectangle(cornerRadius: 8)
                .fill(.quaternary.opacity(0.3))
        )
        .padding(.vertical, 2)
        .accessibilityElement(children: .combine)
    }

    private func gatePreview(_ gate: String) -> some View {
        boundedBlock(title: "Last gate output", body: gate)
    }

    private func boundedBlock(title: String, body: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title)
                .font(.caption)
                .foregroundStyle(.secondary)
            ScrollView(.horizontal, showsIndicators: false) {
                Text(body)
                    .font(.system(.caption, design: .monospaced))
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .frame(maxHeight: 220)
            .padding(10)
            .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 8))
            .accessibilityElement(children: .combine)
            .accessibilityLabel("\(title): \(body)")
        }
    }

    private func formattedWake(_ iso: String?) -> String? {
        guard let iso, !iso.isEmpty else { return nil }
        guard let date = RFC3339.date(iso) else { return iso }
        return date.formatted(date: .abbreviated, time: .standard)
    }
}


/// A small label for a run's workflow or machine.
struct MetaChip: View {
    let text: String
    let systemImage: String
    var emphasized = false

    var body: some View {
        Label(text, systemImage: systemImage)
            .font(.caption)
            .padding(.horizontal, 7)
            .padding(.vertical, 2)
            .background(emphasized ? Color.accentColor.opacity(0.16) : Color.secondary.opacity(0.12), in: Capsule())
            .foregroundStyle(emphasized ? Color.accentColor : Color.secondary)
            .lineLimit(1)
            .accessibilityElement(children: .combine)
    }
}

/// The run's recent output in a terminal-style panel that follows the end
/// while the run is live. The daemon serves at most 4 KB of tail.
struct LiveOutputView: View {
    let tail: String
    let truncated: Bool
    let following: Bool
    private let endID = "output-end"

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                Text(truncated ? "Output (last 4 KB)" : "Output")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                if following {
                    Label("live", systemImage: "circle.fill")
                        .font(.caption2.weight(.semibold))
                        .foregroundStyle(.green)
                        .symbolEffect(.pulse)
                        .accessibilityLabel("Following live output")
                }
                Spacer()
                Button {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(tail, forType: .string)
                } label: {
                    Image(systemName: "doc.on.doc")
                }
                .buttonStyle(.borderless)
                .help("Copy output")
                .accessibilityLabel("Copy output")
            }
            ScrollViewReader { proxy in
                ScrollView {
                    VStack(alignment: .leading, spacing: 0) {
                        Text(tail)
                            .font(.system(.caption, design: .monospaced))
                            .foregroundStyle(Color(white: 0.88))
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                        Color.clear.frame(height: 1).id(endID)
                    }
                    .padding(10)
                }
                .frame(minHeight: 110, maxHeight: 320)
                .background(Color(white: 0.09), in: RoundedRectangle(cornerRadius: 8))
                .onAppear { proxy.scrollTo(endID, anchor: .bottom) }
                .onChange(of: tail) { proxy.scrollTo(endID, anchor: .bottom) }
            }
            .accessibilityLabel("Run output: \(tail)")
        }
    }
}
