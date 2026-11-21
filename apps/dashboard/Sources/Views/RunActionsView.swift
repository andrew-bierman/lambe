//
//  RunActionsView.swift
//  LambeDash
//
//  The detail column's action row: Rex first when it is installed, then
//  Terminal / copy / Finder fallbacks. Which buttons exist is decided by
//  `RunActionContext.actions` (no dead buttons); every press reports an
//  outcome line. A run on another machine opens in Rex on that machine.
//  The app is a viewer: no retry/cancel/nudge, because the daemon exposes
//  no such HTTP route.
//

import SwiftUI

struct RunActionsSection: View {
    @Environment(ShellModel.self) private var shell
    @Environment(ConnectionModel.self) private var connection
    let context: RunActionContext

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 6) {
                Text("Open")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                if let host = context.remoteHost {
                    Label("runs on \(host)", systemImage: "desktopcomputer")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 190), alignment: .leading)], alignment: .leading, spacing: 8) {
                ForEach(context.actions(preferring: Prefs.preferredHandoff)) { action in
                    Button {
                        Task { await shell.perform(action.kind, context, demo: connection.mode == .demo) }
                    } label: {
                        Label(action.title, systemImage: action.systemImage)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .buttonStyle(.bordered)
                    .tint(action.kind == .openInRex ? .accentColor : nil)
                    .disabled(shell.busy)
                    .help(action.title)
                    .accessibilityLabel("\(action.title) for \(context.issue)")
                }
            }
            if context.rexPath == nil {
                Text(context.remoteHost == nil
                    ? "Rex was not found, so runs open in Terminal. Install Rex to jump straight to a run's tab."
                    : "Rex was not found. Install it, or run the copied command on \(context.remoteHost ?? "that machine").")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            if let outcome = shell.outcome {
                Text(outcome.message)
                    .font(.caption)
                    .foregroundStyle(outcome.ok ? Color.secondary : Color.red)
                    .lineLimit(3)
                    .textSelection(.enabled)
            }
        }
        .onChange(of: context.issue) { shell.outcome = nil }
    }
}
