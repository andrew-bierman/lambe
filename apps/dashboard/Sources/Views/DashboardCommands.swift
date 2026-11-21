//
//  DashboardCommands.swift
//  LambeDash
//
//  The menu bar: view navigation (⌘1-3), refresh (⌘R), the run actions for
//  the selected run (⌥⌘ shortcuts), instance switching, demo data, and the
//  setup guide. Every command is also reachable from the window; the menu
//  adds shortcuts and VoiceOver-friendly discoverability.
//

import SwiftUI

struct DashboardCommands: Commands {
    let connection: ConnectionModel
    let shell: ShellModel

    var body: some Commands {
        CommandGroup(after: .toolbar) {
            Divider()
            ForEach(Array(SidebarSection.allCases.enumerated()), id: \.element) { index, section in
                Button("Show \(section.title)") { shell.section = section }
                    .keyboardShortcut(KeyEquivalent(Character("\(index + 1)")), modifiers: .command)
            }
        }
        CommandMenu("Dashboard") {
            Button("Refresh State") { connection.refreshNow() }
                .keyboardShortcut("r", modifiers: .command)
                .disabled(connection.mode == .demo)
            Divider()
            if connection.mode == .demo {
                Button("Connect to Daemon") { connection.setMode(.live) }
                    .keyboardShortcut("d", modifiers: [.command, .shift])
            } else {
                Button("Show Demo Data") { connection.setMode(.demo) }
                    .keyboardShortcut("d", modifiers: [.command, .shift])
            }
            Button("Setup Guide…") { shell.showSetup = true }
                .keyboardShortcut("g", modifiers: [.command, .shift])
            Menu("Switch Instance") {
                ForEach(connection.instanceStore.records) { record in
                    Button(record.name) {
                        connection.instanceStore.select(record.id)
                        if connection.mode == .demo { connection.setMode(.live) }
                        connection.connectionTargetChanged()
                    }
                }
            }
            .disabled(connection.instanceStore.records.count < 2)
        }
        CommandMenu("Run") {
            let issue = shell.selectedIssue
            let context = issue.map { shell.context(for: $0, connection: connection) }
            runButton("Open in Rex", .openInRex, key: "r", context: context)
            runButton("Open in Terminal", .openInTerminal, key: "t", context: context)
            runButton("Copy Logs Command", .copyLogsCommand, key: "c", context: context)
            runButton("Copy bd show", .copyTrackerCommand, key: "b", context: context)
            runButton("Reveal Workspace in Finder", .revealWorkspace, key: "f", context: context)
            Divider()
            Button("Clear Selection") { shell.selectedIssue = nil }
                .keyboardShortcut(.escape, modifiers: .option)
                .disabled(issue == nil)
        }
    }

    /// One run action: enabled only when it can work for the selected run.
    private func runButton(
        _ title: String, _ kind: RunActionKind, key: Character, context: RunActionContext?
    ) -> some View {
        let available = context?.actions.contains { $0.kind == kind } ?? false
        return Button(title) {
            guard let context else { return }
            Task { await shell.perform(kind, context, demo: connection.mode == .demo) }
        }
        .keyboardShortcut(KeyEquivalent(key), modifiers: [.command, .option])
        .disabled(!available)
    }
}
