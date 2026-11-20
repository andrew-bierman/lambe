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
    @State private var connection = ConnectionModel()

    var body: some Scene {
        WindowGroup {
            ShellView()
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
