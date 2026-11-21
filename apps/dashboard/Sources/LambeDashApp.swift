//
//  LambeDashApp.swift
//  lambe operator dashboard
//
//  lambe operator dashboard entry point (U8). One main window hosting the
//  three-column shell, a Dashboard menu with the refresh command (⌘R, GET
//  only — the app never POSTs, R13), and a Settings scene hosting the
//  instance picker (R14). The connection model is created once here and
//  injected into both scenes; the app owns no daemon process and no
//  tracker access — presentation only.
//
//  The project compiles with `SWIFT_DEFAULT_ACTOR_ISOLATION = MainActor`,
//  so everything here is MainActor-isolated by default.
//

import SwiftUI

@main
struct LambeDashApp: App {
    /// The app-level connection: lifecycle, single poll loop, feeds.
    /// Created once and injected; the shell's `.task` drives its loop.
    @State private var connection: ConnectionModel
    /// `-LambeScenario <name> [-LambeSection history] [-LambeIssue id]`
    /// renders a network-free fixture; used by the screenshot script.
    private let scenario = LaunchScenario.fromArguments()

    init() {
        if let scenario = LaunchScenario.fromArguments() {
            _connection = State(initialValue: ConnectionModel.fixture(scenario.fixture))
        } else {
            _connection = State(initialValue: ConnectionModel())
        }
    }

    var body: some Scene {
        WindowGroup {
            ShellView(
                initialSection: scenario?.section ?? .queue,
                initialIssue: scenario?.issue
            )
            .environment(connection)
        }
        .defaultSize(width: 1080, height: 700)
        .commands {
            CommandMenu("Dashboard") {
                Button("Refresh State") {
                    connection.refreshNow()
                }
                .keyboardShortcut("r", modifiers: .command)
                .disabled(connection.mode == .demo)
                Button("Show Demo Data") {
                    connection.setMode(.demo)
                }
                Button("Connect to Daemon") {
                    connection.setMode(.live)
                }
            }
        }

        Settings {
            SettingsView()
                .environment(connection)
        }
    }
}

/// Launch-argument fixture selection (`-LambeScenario stalled`).
struct LaunchScenario {
    let fixture: FixtureScenario
    let section: SidebarSection
    let issue: String?

    static func fromArguments(_ args: [String] = ProcessInfo.processInfo.arguments) -> LaunchScenario? {
        func value(_ key: String) -> String? {
            args.firstIndex(of: key).flatMap { args.indices.contains($0 + 1) ? args[$0 + 1] : nil }
        }
        guard let name = value("-LambeScenario"), let fixture = FixtureScenario(rawValue: name) else { return nil }
        return LaunchScenario(
            fixture: fixture,
            section: value("-LambeSection").flatMap(SidebarSection.init(rawValue:)) ?? .queue,
            issue: value("-LambeIssue")
        )
    }
}
