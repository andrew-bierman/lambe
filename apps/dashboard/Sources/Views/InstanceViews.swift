//
//  InstanceViews.swift
//  LambeDash
//
//  Where am I looking, and is it healthy? The sidebar lists every
//  configured instance (one per machine/workflow config) with its own
//  connection status; the content column starts with a header naming the
//  attached daemon. Only the selected instance is polled, so the other
//  rows report what their live descriptor says (offline, live, ambiguous)
//  — read from disk, never the network.
//

import SwiftUI

/// Presentation facts for one instance row. Pure value, so previews and
/// tests need no daemon.
nonisolated struct InstanceStatus: Identifiable, Equatable, Sendable {
    let id: UUID
    let name: String
    let detail: String
    let tone: OutcomeTone
    let isSelected: Bool
    var stalled: Int = 0

    /// Status for a non-polled instance, from its live descriptors.
    static func fromDescriptors(_ record: InstanceRecord, isSelected: Bool = false) -> InstanceStatus {
        let endpoints = record.resolvedEndpoints()
        let detail: String
        let tone: OutcomeTone
        switch endpoints.count {
        case 0:
            (detail, tone) = ("offline: no live daemon descriptor", .bad)
        case 1:
            let d = endpoints[0].descriptor
            (detail, tone) = (["daemon up", "pid \(d.pid)", d.version.isEmpty ? nil : "v\(d.version)"].compactMap { $0 }.joined(separator: " · "), .good)
        default:
            (detail, tone) = ("\(endpoints.count) daemons: " + endpoints.map(\.instance).joined(separator: ", "), .warning)
        }
        return InstanceStatus(id: record.id, name: record.name, detail: detail, tone: tone, isSelected: isSelected)
    }
}

/// One instance row in the sidebar.
struct InstanceStatusRow: View {
    let status: InstanceStatus

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Circle()
                .fill(status.tone.color)
                .frame(width: 8, height: 8)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 2) {
                Text(status.name)
                    .font(.body.weight(status.isSelected ? .semibold : .regular))
                    .lineLimit(1)
                    .truncationMode(.tail)
                Text(status.detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
            }
            Spacer(minLength: 0)
            if status.stalled > 0 {
                Text("\(status.stalled) stalled")
                    .font(.caption2.weight(.bold))
                    .padding(.horizontal, 6)
                    .padding(.vertical, 2)
                    .background(Color.red, in: Capsule())
                    .foregroundStyle(.white)
            }
        }
        .padding(.vertical, 2)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(status.name), \(status.detail)\(status.stalled > 0 ? ", \(status.stalled) stalled" : "")\(status.isSelected ? ", selected" : "")")
        .accessibilityAddTraits(status.isSelected ? .isSelected : [])
    }
}

/// The sidebar's Instances section. Hidden in demo mode, where there is
/// no daemon to compare.
struct InstancesSection: View {
    @Environment(ConnectionModel.self) private var connection

    var body: some View {
        if connection.mode == .live {
            Section("Instances") {
                ForEach(connection.instanceStore.records) { record in
                    InstanceSidebarRow(record: record)
                }
            }
        }
    }
}

/// Resolves one record's status (selected: the live connection; others:
/// descriptors) and switches to it on click.
struct InstanceSidebarRow: View {
    @Environment(ConnectionModel.self) private var connection
    let record: InstanceRecord
    @State private var descriptorStatus: InstanceStatus?

    var body: some View {
        let selected = record.id == connection.instanceStore.selectedID
        Button {
            guard !selected else { return }
            connection.instanceStore.select(record.id)
            connection.connectionTargetChanged()
        } label: {
            InstanceStatusRow(status: selected ? connectionStatus : (descriptorStatus ?? placeholder))
        }
        .buttonStyle(.plain)
        .task(id: record) { descriptorStatus = InstanceStatus.fromDescriptors(record) }
        .task(id: connection.lastSync) {
            // Re-read the unselected rows' descriptors on every poll tick.
            if !selected { descriptorStatus = InstanceStatus.fromDescriptors(record) }
        }
    }

    private var placeholder: InstanceStatus {
        InstanceStatus(id: record.id, name: record.name, detail: "checking…", tone: .neutral, isSelected: false)
    }

    private var connectionStatus: InstanceStatus {
        let (text, tone) = connection.statusSummary
        var parts = [text]
        if let machine = connection.daemon?.machine, !machine.isEmpty { parts.insert(machine, at: 0) }
        let stalled = StallPolicy.stalledRuns(
            QueueComposition.activeRuns(in: connection.snapshotFeed.payload), now: Date()
        ).count
        return InstanceStatus(
            id: record.id, name: record.name, detail: parts.joined(separator: " · "),
            tone: tone, isSelected: true, stalled: stalled
        )
    }
}

/// The content column's header: which daemon this list belongs to.
struct ConnectionHeader: View {
    @Environment(ConnectionModel.self) private var connection

    var body: some View {
        if let daemon = connection.daemon {
            HStack(spacing: 6) {
                Image(systemName: "server.rack")
                    .foregroundStyle(.secondary)
                    .accessibilityHidden(true)
                Text(connection.mode == .demo ? "Demo" : (connection.instanceName ?? "Instance"))
                    .font(.footnote.weight(.semibold))
                if let machine = daemon.machine, !machine.isEmpty {
                    Text("on \(machine)")
                        .font(.footnote)
                }
                if let config = daemon.configPath, !config.isEmpty {
                    Text(URL(fileURLWithPath: config).lastPathComponent)
                        .font(.caption.monospaced())
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                Spacer(minLength: 0)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 6)
            .background(.bar)
            .accessibilityElement(children: .combine)
        }
    }
}
