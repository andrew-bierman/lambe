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

/// The Settings scene: General, Instances, and Tools tabs.
struct SettingsView: View {
    var body: some View {
        TabView {
            Tab("General", systemImage: "gearshape") { GeneralSettings() }
            Tab("Instances", systemImage: "server.rack") { InstanceSettings() }
            Tab("Tools", systemImage: "terminal") { ToolSettings() }
        }
        .frame(width: 560, height: 380)
    }
}

/// Behavior preferences.
struct GeneralSettings: View {
    @Environment(ConnectionModel.self) private var connection
    @Environment(ShellModel.self) private var shell
    @AppStorage(Prefs.startInDemo, store: Prefs.defaults) private var startInDemo = false
    @AppStorage(Prefs.handoff, store: Prefs.defaults) private var handoff = Prefs.Handoff.rex.rawValue

    var body: some View {
        Form {
            Section {
                Picker("Open runs in", selection: $handoff) {
                    ForEach(Prefs.Handoff.allCases) { Text($0.title).tag($0.rawValue) }
                }
                Text("Rex opens a tab on the machine that runs the agent. Terminal is used when Rex is not installed.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Section {
                Toggle("Start with demo data", isOn: $startInDemo)
                Text("Opens the sample runs instead of looking for a daemon. Handy for a tour or screenshots.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Section {
                Button("Show Setup Guide…") { shell.showSetup = true }
                    .help("Find running daemons and see the commands to set one up")
            }
        }
        .formStyle(.grouped)
    }
}

/// Where `lambe` and `rex` are found, with an override for each.
struct ToolSettings: View {
    var body: some View {
        Form {
            ToolPathRow(name: "lambe", key: Prefs.lambeOverride, detail: "Used for the commands the app shows and starts.")
            ToolPathRow(name: "rex", key: Prefs.rexOverride, detail: "Optional. Enables Open in Rex.")
        }
        .formStyle(.grouped)
    }
}

struct ToolPathRow: View {
    let name: String
    let key: String
    let detail: String
    @AppStorage private var override: String
    @State private var found: String?

    init(name: String, key: String, detail: String) {
        self.name = name
        self.key = key
        self.detail = detail
        _override = AppStorage(wrappedValue: "", key, store: Prefs.defaults)
    }

    var body: some View {
        Section(name) {
            LabeledContent("Found") {
                Text(found ?? "not found")
                    .font(.callout.monospaced())
                    .foregroundStyle(found == nil ? Color.red : Color.secondary)
                    .textSelection(.enabled)
            }
            TextField("Override path", text: $override, prompt: Text("/path/to/\(name)"))
                .accessibilityLabel("\(name) path override")
            Text(detail)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .task(id: override) { found = ToolLocator.locate(name, override: override) }
    }
}

/// The instance list: which daemons the app knows, and which is attached.
struct InstanceSettings: View {
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
                parts.append(VersionLabel.text(d.version))
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
