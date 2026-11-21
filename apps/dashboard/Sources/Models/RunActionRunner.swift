//
//  RunActionRunner.swift
//  LambeDash
//
//  Executes a `RunAction`. Rex is driven through its CLI only
//  (`rex window focus` / `rex window new`); the app never touches Rex
//  sessions it did not ask for, and never mutates a run. Every action
//  returns a user-visible outcome — failures are stated, not swallowed.
//

import AppKit
import Foundation

nonisolated struct ActionOutcome: Equatable, Sendable {
    let ok: Bool
    let message: String
}

/// Seam for process execution so tests need no `rex` binary.
nonisolated protocol CommandRunning: Sendable {
    /// Runs `executable args…`; returns the exit status, nil if it could not launch.
    func run(_ executable: String, _ args: [String]) async -> Int32?
}

nonisolated struct ProcessRunner: CommandRunning {
    func run(_ executable: String, _ args: [String]) async -> Int32? {
        await withCheckedContinuation { continuation in
            let process = Process()
            process.executableURL = URL(fileURLWithPath: executable)
            process.arguments = args
            process.standardOutput = FileHandle.nullDevice
            process.standardError = FileHandle.nullDevice
            process.terminationHandler = { continuation.resume(returning: $0.terminationStatus) }
            do { try process.run() } catch { continuation.resume(returning: nil) }
        }
    }
}

nonisolated enum RexLocator {
    /// A GUI app's PATH is minimal, so search the usual install homes too.
    static func locate(
        env: [String: String] = ProcessInfo.processInfo.environment,
        isExecutable: (String) -> Bool = { FileManager.default.isExecutableFile(atPath: $0) }
    ) -> String? {
        let home = NSHomeDirectory()
        let dirs = (env["PATH"] ?? "").split(separator: ":").map(String.init)
            + ["\(home)/.local/bin", "/opt/homebrew/bin", "/usr/local/bin"]
        for dir in dirs where isExecutable("\(dir)/rex") { return "\(dir)/rex" }
        return nil
    }
}

nonisolated enum RunActionRunner {
    /// Focuses the run's existing Rex window, or opens a new one that
    /// follows the run's logs.
    static func openInRex(
        _ ctx: RunActionContext, runner: any CommandRunning = ProcessRunner()
    ) async -> ActionOutcome {
        guard let rex = ctx.rexPath else {
            return ActionOutcome(ok: false, message: "Rex was not found on this Mac.")
        }
        if await runner.run(rex, ["window", "focus", ctx.rexLabel]) == 0 {
            return ActionOutcome(ok: true, message: "Focused \(ctx.rexLabel) in Rex.")
        }
        let status = await runner.run(
            rex, ["window", "new", ctx.rexLabel, "--"] + ctx.logsArgv
        )
        return status == 0
            ? ActionOutcome(ok: true, message: "Opened \(ctx.rexLabel) in Rex.")
            : ActionOutcome(ok: false, message: "Rex could not open a window (is the Rex server running?). Use Open in Terminal instead.")
    }

    /// Opens Terminal.app on a throwaway `.command` file that runs `command`.
    @MainActor
    static func openInTerminal(command: String) -> ActionOutcome {
        let dir = FileManager.default.temporaryDirectory
        let file = dir.appending(path: "lambe-\(UUID().uuidString).command")
        let script = "#!/bin/zsh\n\(command)\n"
        do {
            try script.write(to: file, atomically: true, encoding: .utf8)
            try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: file.path)
        } catch {
            return ActionOutcome(ok: false, message: "Could not prepare a Terminal script: \(error.localizedDescription)")
        }
        guard let terminal = NSWorkspace.shared.urlForApplication(withBundleIdentifier: "com.apple.Terminal") else {
            return ActionOutcome(ok: false, message: "Terminal.app was not found. Use Copy logs command instead.")
        }
        NSWorkspace.shared.open([file], withApplicationAt: terminal, configuration: NSWorkspace.OpenConfiguration())
        return ActionOutcome(ok: true, message: "Opened Terminal.")
    }

    @MainActor
    static func copy(_ text: String) -> ActionOutcome {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
        return ActionOutcome(ok: true, message: "Copied: \(text)")
    }

    @MainActor
    static func reveal(path: String) -> ActionOutcome {
        NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
        return ActionOutcome(ok: true, message: "Revealed workspace in Finder.")
    }
}
