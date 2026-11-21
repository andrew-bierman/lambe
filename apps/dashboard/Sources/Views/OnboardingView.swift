//
//  OnboardingView.swift
//  LambeDash
//
//  First run, and any time no daemon is attached: find the daemons already
//  running on this Mac and connect to one, or walk the setup (install,
//  init, enable the state API, start) with the exact `lambe` commands to
//  copy. The app never edits config and never starts anything silently:
//  "Start in Terminal" opens a Terminal window running the command it
//  shows. Demo data is one click away so the window is never a dead end.
//

import AppKit
import SwiftUI

struct OnboardingView: View {
    @Environment(ConnectionModel.self) private var connection
    @Environment(ShellModel.self) private var shell

    @State private var guide: SetupGuide?
    @State private var rexPath: String?
    @State private var note: String?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                header
                if let guide {
                    if !guide.daemons.isEmpty { daemonList(guide) }
                    setupSteps(guide)
                    actions(guide)
                    toolStatus(guide)
                } else {
                    ProgressView("Looking for daemons…")
                }
            }
            .frame(maxWidth: 620, alignment: .leading)
            .padding(32)
            .frame(maxWidth: .infinity)
        }
        .frame(minWidth: 700, idealWidth: 1200, minHeight: 560, idealHeight: 780)
        .background(Color(nsColor: .windowBackgroundColor))
        .task { await scanLoop() }
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Set up LambeDash")
    }

    // MARK: - Pieces

    private var header: some View {
        HStack(alignment: .center, spacing: 18) {
            Image(nsImage: NSApp.applicationIconImage)
                .resizable()
                .frame(width: 84, height: 84)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 6) {
                Text(shell.showSetup && connection.isConnectionActive ? "Set up another daemon" : "Welcome to LambeDash")
                    .font(.largeTitle.weight(.semibold))
                Text("A window onto your lambe daemon: the queue, what needs you, history, and live output. It only reads state. Runs open in Rex or Terminal.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    private func daemonList(_ guide: SetupGuide) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Running on this Mac")
                .font(.headline)
            ForEach(guide.daemons) { daemon in
                HStack(alignment: .center, spacing: 12) {
                    Image(systemName: "circle.fill")
                        .foregroundStyle(.green)
                        .font(.caption)
                        .accessibilityHidden(true)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(daemon.suggestedName)
                            .font(.body.weight(.medium))
                        Text("pid \(daemon.pid) · \(VersionLabel.text(daemon.version)) · \(daemon.configPath ?? daemon.stateDirectory)")
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .truncationMode(.middle)
                    }
                    Spacer()
                    Button("Connect") { connect(daemon) }
                        .buttonStyle(.borderedProminent)
                        .accessibilityLabel("Connect to \(daemon.suggestedName)")
                }
                .padding(12)
                .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 10))
                .accessibilityElement(children: .contain)
            }
        }
    }

    private func setupSteps(_ guide: SetupGuide) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(guide.daemons.isEmpty ? "No daemon with the state API on" : "Set up another daemon")
                .font(.headline)
            ForEach(Array(guide.steps.enumerated()), id: \.element.id) { index, step in
                StepRow(number: index + 1, step: step) { copy($0) }
            }
        }
    }

    private func actions(_ guide: SetupGuide) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 10) {
                if let lambe = guide.lambePath, guide.configExists, guide.daemons.isEmpty {
                    Button {
                        let command = guide.runCommand(lambe: lambe, enableStateAPI: !guide.configEnablesStateAPI)
                        note = RunActionRunner.openInTerminal(command: command).message
                    } label: {
                        Label("Start in Terminal", systemImage: "play.fill")
                    }
                    .buttonStyle(.borderedProminent)
                    .help("Opens Terminal running: \(guide.runCommand(lambe: lambe, enableStateAPI: !guide.configEnablesStateAPI))")
                }
                Button {
                    Task { await rescan() }
                } label: {
                    Label("Check Again", systemImage: "arrow.clockwise")
                }
                Button {
                    chooseStateDirectory()
                } label: {
                    Label("Choose Folder…", systemImage: "folder")
                }
                Spacer()
                Button {
                    connection.setMode(.demo)
                    shell.showSetup = false
                } label: {
                    Label("Explore Demo Data", systemImage: "play.rectangle")
                }
                if shell.showSetup, connection.isConnectionActive {
                    Button("Done") { shell.showSetup = false }
                        .keyboardShortcut(.defaultAction)
                }
            }
            .buttonStyle(.bordered)
            if let note {
                Text(note)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
            Text("Checking \(guide.stateDirectory) for descriptors. The daemon listens on 127.0.0.1 only, and every request carries a per-start token.")
                .font(.caption)
                .foregroundStyle(.tertiary)
        }
    }

    private func toolStatus(_ guide: SetupGuide) -> some View {
        HStack(spacing: 18) {
            ToolBadge(name: "lambe", path: guide.lambePath, required: true)
            ToolBadge(name: "rex", path: rexPath, required: false)
            Spacer()
        }
        .font(.caption)
        .accessibilityElement(children: .contain)
    }

    // MARK: - Behavior

    private func scanLoop() async {
        while !Task.isCancelled {
            await rescan()
            try? await Task.sleep(for: .seconds(2))
        }
    }

    private func rescan() async {
        let dirs = DaemonDiscovery.candidateDirectories(
            configured: connection.instanceStore.records.map(\.stateDirectory)
        )
        let lambe = Prefs.locateLambe()
        let daemons = await Task.detached { DaemonDiscovery.scan(dirs) }.value
        guide = SetupGuide.inspect(lambePath: lambe, daemons: daemons)
        rexPath = Prefs.locateRex()
    }

    private func connect(_ daemon: DiscoveredDaemon) {
        let store = connection.instanceStore
        let existing = store.records.first {
            $0.stateDirectory == daemon.stateDirectory
                && ($0.exactInstanceFilter == nil || $0.exactInstanceFilter == daemon.instance)
        }
        if let existing {
            store.select(existing.id)
        } else {
            store.add(InstanceRecord(
                name: daemon.suggestedName, stateDirectory: daemon.stateDirectory, instanceName: daemon.instance
            ))
        }
        if connection.mode == .demo { connection.setMode(.live) }
        connection.connectionTargetChanged()
        shell.showSetup = false
    }

    private func chooseStateDirectory() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.directoryURL = URL(fileURLWithPath: DefaultInstance.stateDirectory()).deletingLastPathComponent()
        panel.message = "Choose a lambe state directory (it contains a live/ folder)."
        panel.prompt = "Use Folder"
        guard panel.runModal() == .OK, let url = panel.url else { return }
        let store = connection.instanceStore
        let record = InstanceRecord(name: url.lastPathComponent, stateDirectory: url.path)
        store.add(record)
        connection.connectionTargetChanged()
        shell.showSetup = false
    }

    private func copy(_ text: String) {
        note = RunActionRunner.copy(text).message
    }
}

/// One numbered setup step with its command, if any.
struct StepRow: View {
    let number: Int
    let step: SetupStep
    let onCopy: (String) -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            Image(systemName: step.done ? "checkmark.circle.fill" : "\(number).circle")
                .font(.title3)
                .foregroundStyle(step.done ? Color.green : Color.secondary)
                .frame(width: 24)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 6) {
                Text(step.title)
                    .font(.body.weight(.medium))
                Text(step.detail)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                if let command = step.command {
                    HStack(alignment: .top, spacing: 8) {
                        Text(command)
                            .font(.system(.callout, design: .monospaced))
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                        Button {
                            onCopy(command)
                        } label: {
                            Image(systemName: "doc.on.doc")
                        }
                        .buttonStyle(.borderless)
                        .help("Copy command")
                        .accessibilityLabel("Copy command: \(command)")
                    }
                    .padding(10)
                    .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 8))
                }
            }
        }
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Step \(number): \(step.title), \(step.done ? "done" : "to do")")
    }
}

/// Whether a command-line tool was found, and where.
struct ToolBadge: View {
    let name: String
    let path: String?
    let required: Bool

    var body: some View {
        HStack(spacing: 5) {
            Image(systemName: path != nil ? "checkmark.circle.fill" : (required ? "xmark.circle.fill" : "minus.circle"))
                .foregroundStyle(path != nil ? Color.green : (required ? Color.red : Color.secondary))
            Text(name).font(.caption.monospaced())
            Text(path ?? (required ? "not found" : "not found (optional)"))
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .truncationMode(.middle)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel(path.map { "\(name) found at \($0)" } ?? "\(name) not found\(required ? "" : ", optional")")
    }
}
