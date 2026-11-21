//
//  RunContextMenu.swift
//  LambeDash
//
//  The right-click menu on a run row: the same actions as the detail
//  column's Open row, so a run can be handed to Rex without selecting it.
//

import SwiftUI

private struct RunContextMenu: ViewModifier {
    @Environment(ShellModel.self) private var shell
    @Environment(ConnectionModel.self) private var connection
    let issue: String

    func body(content: Content) -> some View {
        content.contextMenu {
            let context = shell.context(for: issue, connection: connection)
            ForEach(context.actions(preferring: Prefs.preferredHandoff)) { action in
                Button(action.title, systemImage: action.systemImage) {
                    shell.selectedIssue = issue
                    Task { await shell.perform(action.kind, context, demo: connection.mode == .demo) }
                }
            }
        }
    }
}

extension View {
    /// Adds the run actions as a context menu for `issue`.
    func runContextMenu(_ issue: String) -> some View {
        modifier(RunContextMenu(issue: issue))
    }
}
