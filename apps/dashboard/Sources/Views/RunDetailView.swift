//
//  RunDetailView.swift
//  LambeDash
//
//  The detail column (U8): the selected run's facts at parity with the
//  TUI inspector (R5) — phase, state, attempt, turn, thread, workspace,
//  workflow, wake-at, session, last gate preview, and the bounded output
//  tail. Seed facts render immediately from the selected row; the fetched
//  `GET /api/v1/{issue}` body upgrades the column when it arrives.
//  Truthful about absence: a history row the daemon no longer knows shows
//  the archive facts and says so; a failed fetch shows the seed facts and
//  the failure — nothing silently swallowed (R11).
//

import SwiftUI

struct RunDetailView: View {
    @Environment(ConnectionModel.self) private var connection
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
                statusBlock(seed)
                if let issue = connection.detail.issue {
                    factsGrid(issue)
                    if let lastGate = issue.lastGate, !lastGate.isEmpty {
                        gatePreview(lastGate)
                    }
                    if let tail = issue.tail, !tail.isEmpty {
                        outputTail(tail, truncated: issue.tailTruncated)
                    }
                }
            }
            .padding(18)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .task(id: seed.issue) {
            // One fetch per selection; changing selection cancels the
            // previous fetch (single in-flight detail, cancellation-aware).
            await connection.detail.load(
                id: seed.issue,
                fetch: connection.detailFetch(id: seed.issue)
            )
        }
    }

    // MARK: - Blocks

    private func header(_ seed: RunSeed) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(seed.issue)
                .font(.title3.weight(.semibold).monospaced())
            if let title = seed.title, !title.isEmpty {
                Text(title)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
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
            .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 6))
            .accessibilityElement(children: .combine)
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
                        .frame(width: 86, alignment: .leading)
                    Text(value ?? "")
                        .font(.footnote.monospaced())
                        .textSelection(.enabled)
                    Spacer(minLength: 0)
                }
                .padding(.vertical, 3)
                Divider()
            }
        }
        .background(
            RoundedRectangle(cornerRadius: 6)
                .fill(.quaternary.opacity(0.3))
        )
        .padding(.vertical, 2)
        .accessibilityElement(children: .combine)
    }

    private func gatePreview(_ gate: String) -> some View {
        boundedBlock(title: "Last gate output", body: gate)
    }

    private func outputTail(_ tail: String, truncated: Bool) -> some View {
        boundedBlock(
            title: truncated ? "Output tail (truncated at 4 KB)" : "Output tail",
            body: tail
        )
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
            .padding(8)
            .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 6))
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
