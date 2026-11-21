//
//  ShellModel.swift
//  LambeDash
//
//  The window's navigation state (section, selected run, setup guide) and
//  the run actions, in one observable so the toolbar, the detail column and
//  the menu bar drive the same thing. Selection is by issue ID and is
//  dropped whenever the connection context changes, so a previous daemon's
//  IDs never linger over another daemon's data.
//

import AppKit
import Foundation
import SwiftUI

@MainActor
@Observable
final class ShellModel {
    var section: SidebarSection
    var selectedIssue: String?
    /// The setup guide is open even though a daemon is attached.
    var showSetup = false
    /// The last action's result, shown under the Open buttons.
    var outcome: ActionOutcome?
    private(set) var busy = false

    init(section: SidebarSection = .queue, issue: String? = nil) {
        self.section = section
        self.selectedIssue = issue
    }

    /// Drops everything that belonged to the previous connection context.
    func contextChanged() {
        selectedIssue = nil
        outcome = nil
    }

    /// The facts the actions need for `issue`: this Mac's paths when the
    /// daemon runs here, or the owning machine when the run lives elsewhere.
    func context(for issue: String, connection: ConnectionModel) -> RunActionContext {
        let live = connection.detail.loadedID == issue ? connection.detail.issue : nil
        let run = connection.snapshotFeed.payload?.runs.first { $0.issue == issue }
        let machine = live?.machine ?? run?.machine
        let here = connection.daemon?.machine
        let remote = (machine?.isEmpty == false && machine != here) ? machine : nil
        let workspace = live?.workspace.flatMap { FileManager.default.fileExists(atPath: $0) ? $0 : nil }
        return RunActionContext(
            issue: issue,
            configPath: connection.daemon?.configPath,
            localWorkspace: remote == nil ? workspace : nil,
            rexPath: Prefs.locateRex(),
            remoteHost: remote
        )
    }

    /// Runs one action and records its outcome. Demo data has nothing real
    /// to open, so only the copy actions act there.
    func perform(
        _ kind: RunActionKind, _ ctx: RunActionContext, demo: Bool,
        runner: any CommandRunning = ProcessRunner()
    ) async {
        if demo, kind != .copyLogsCommand, kind != .copyTrackerCommand {
            outcome = ActionOutcome(ok: false, message: "Demo data: there is no real run to open.")
            return
        }
        switch kind {
        case .openInRex:
            busy = true
            outcome = await RunActionRunner.openInRex(ctx, runner: runner)
            busy = false
        case .openInTerminal:
            outcome = RunActionRunner.openInTerminal(command: ctx.logsCommand)
        case .copyLogsCommand:
            outcome = RunActionRunner.copy(ctx.logsCommand)
        case .copyTrackerCommand:
            outcome = RunActionRunner.copy(ctx.trackerCommand)
        case .revealWorkspace:
            if let path = ctx.localWorkspace { outcome = RunActionRunner.reveal(path: path) }
        }
    }
}
