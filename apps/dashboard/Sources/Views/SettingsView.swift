//
//  SettingsView.swift
//  LambeDash
//
//  The Settings scene's instance picker (R14): the ordered list of daemon
//  instances the operator maintains, the selected one resolved to exactly
//  one running daemon. Shows resolution facts per record — instance name,
//  pid, version — and never the bearer token, which lives only inside the
//  client. Selecting a record retargets the connection immediately; a
//  dev-smoke instance can therefore never be confused with production.
//

import SwiftUI

struct SettingsView: View {
    @Environment(ConnectionModel.self) private var connection
    @State private var addingSheet = false

    var body: some View {
        VStack(spacing: 0) {
            list
            Divider()
            HStack(spacing: 8) {
                Text("Instances resolve to exactly one running daemon via its live descriptor. The token is never shown.")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                Spacer()
                Button("Add…") {
                    addingSheet = true
                }
                Button("Remove") {
                    if let selected = connection.instanceStore.selected {
                        connection.instanceStore.remove(selected)
                        connection.connectionTargetChanged()
                    }
                }
                .disabled(connection.instanceStore.selected == nil)
            }
            .padding(12)
        }
        .frame(width: 520, height: 320)
        .sheet(isPresented: $addingSheet) {
            AddInstanceSheet()
        }
    }

    private var list: some View {
        List(selection: Binding(
            get: { connection.instanceStore.selectedID },
            set: { newID in
                guard let newID, newID != connection.instanceStore.selectedID else { return }
                connection.instanceStore.select(newID)
                connection.connectionTargetChanged()
            }
        )) {
            ForEach(connection.instanceStore.records) { record in
                InstanceRow(record: record, isSelected: record.id == connection.instanceStore.selectedID)
                    .tag(record.id)
            }
        }
        .listStyle(.bordered)
    }
}

/// One instance row: label, state directory, and live resolution facts.
struct InstanceRow: View {
    let record: InstanceRecord
    let isSelected: Bool
    @State private var health: String = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 8) {
                Text(record.name)
                    .font(.body.weight(.medium))
                if isSelected {
                    Text("selected")
                        .font(.caption2.weight(.medium))
                        .padding(.horizontal, 6)
                        .padding(.vertical, 2)
                        .background(.tint.opacity(0.15), in: Capsule())
                        .foregroundStyle(.tint)
                }
            }
            Text(record.stateDirectory)
                .font(.caption.monospaced())
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .truncationMode(.middle)
            Text(healthText)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .padding(.vertical, 2)
        .task(id: record) {
            health = InstanceHealth.describe(record)
        }
        .accessibilityElement(children: .combine)
    }

    private var healthText: String {
        if let filter = record.exactInstanceFilter {
            return "\(health) — instance filter: \(filter)"
        }
        return health
    }
}

/// Read-only resolution facts for one record: enumerates its live dir the
/// same way the connection does. Facts only — no token, ever.
nonisolated enum InstanceHealth {
    static func describe(_ record: InstanceRecord) -> String {
        let endpoints = record.resolvedEndpoints()
        switch endpoints.count {
        case 0:
            return "no live descriptor"
        case 1:
            let d = endpoints[0].descriptor
            var parts = ["live", "pid \(d.pid)"]
            if !d.version.isEmpty {
                parts.append("v\(d.version)")
            }
            return parts.joined(separator: " · ")
        default:
            return "\(endpoints.count) live daemons: " + endpoints.map(\.instance).joined(separator: ", ")
        }
    }
}

/// The add-instance sheet: a display name, the daemon's state directory,
/// and an optional exact instance filter.
struct AddInstanceSheet: View {
    @Environment(ConnectionModel.self) private var connection
    @Environment(\.dismiss) private var dismiss

    @State private var name = ""
    @State private var directory = ""
    @State private var instanceFilter = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Add daemon instance")
                .font(.headline)
            Form {
                TextField("Name", text: $name, prompt: Text("smoke-1"))
                TextField(
                    "State directory",
                    text: $directory,
                    prompt: Text(DefaultInstance.stateDirectory())
                )
                TextField(
                    "Instance filter (optional)",
                    text: $instanceFilter,
                    prompt: Text("exact instance name, or empty for a single-daemon directory")
                )
            }
            Text("The directory's live/ folder holds the daemon's endpoint descriptors. Never point an instance at a directory you do not control: its descriptors carry the daemon's bearer token.")
                .font(.caption)
                .foregroundStyle(.secondary)
            HStack {
                Spacer()
                Button("Cancel") {
                    dismiss()
                }
                .keyboardShortcut(.cancelAction)
                Button("Add") {
                    connection.instanceStore.add(
                        InstanceRecord(
                            name: name.isEmpty ? "Instance" : name,
                            stateDirectory: directory.isEmpty
                                ? DefaultInstance.stateDirectory()
                                : directory,
                            instanceName: instanceFilter.isEmpty ? nil : instanceFilter
                        )
                    )
                    connection.connectionTargetChanged()
                    dismiss()
                }
                .keyboardShortcut(.defaultAction)
                .disabled(directory.trimmingCharacters(in: .whitespaces).isEmpty && name.isEmpty)
            }
        }
        .padding(16)
        .frame(width: 440)
    }
}
