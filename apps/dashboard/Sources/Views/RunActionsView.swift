//
//  RunActionsView.swift
//  LambeDash
//
//  The detail column's action row: Rex first when it is installed, then
//  Terminal / copy / Finder fallbacks. Which buttons exist is decided by
//  `RunActionContext.actions` (no dead buttons); every press reports an
//  outcome line. The app is a viewer: no retry/cancel/nudge, because the
//  daemon exposes no such HTTP route.
//

import SwiftUI

struct RunActionsSection: View {
    let context: RunActionContext
    @State private var outcome: ActionOutcome?
    @State private var busy = false

    init(issue: String, configPath: String?, workspace: String?, rexPath: String? = RexLocator.locate()) {
        let local = workspace.flatMap { FileManager.default.fileExists(atPath: $0) ? $0 : nil }
        context = RunActionContext(issue: issue, configPath: configPath, localWorkspace: local, rexPath: rexPath)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Open")
                .font(.caption)
                .foregroundStyle(.secondary)
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 150), alignment: .leading)], alignment: .leading, spacing: 8) {
                ForEach(context.actions) { action in
                    Button {
                        perform(action)
                    } label: {
                        Label(action.title, systemImage: action.systemImage)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .buttonStyle(.bordered)
                    .tint(action.kind == .openInRex ? .accentColor : nil)
                    .disabled(busy)
                    .accessibilityLabel("\(action.title) for \(context.issue)")
                }
            }
            if context.rexPath == nil {
                Text("Rex was not found, so runs open in Terminal. Install Rex to jump straight to a run's tab.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            if let outcome {
                Text(outcome.message)
                    .font(.caption)
                    .foregroundStyle(outcome.ok ? Color.secondary : Color.red)
                    .lineLimit(3)
                    .textSelection(.enabled)
            }
        }
        .onChange(of: context.issue) { outcome = nil }
    }

    private func perform(_ action: RunAction) {
        switch action.kind {
        case .openInRex:
            busy = true
            Task {
                outcome = await RunActionRunner.openInRex(context)
                busy = false
            }
        case .openInTerminal:
            outcome = RunActionRunner.openInTerminal(command: context.logsCommand)
        case .copyLogsCommand:
            outcome = RunActionRunner.copy(context.logsCommand)
        case .copyTrackerCommand:
            outcome = RunActionRunner.copy(context.trackerCommand)
        case .revealWorkspace:
            if let path = context.localWorkspace { outcome = RunActionRunner.reveal(path: path) }
        }
    }
}
