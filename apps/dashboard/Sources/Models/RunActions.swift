//
//  RunActions.swift
//  LambeDash
//
//  What an operator can do with a run from the app, decided in one pure
//  place. The state API is read-only and the daemon's retry/cancel control
//  surface is MCP-only, so the app offers navigation, not mutation: jump
//  to the run in Rex (first choice), or fall back to Terminal.app, a
//  copied command, or the workspace folder. An action is listed only when
//  it can work — no dead buttons.
//

import Foundation

nonisolated enum RunActionKind: String, Equatable, Sendable {
    case openInRex, openInTerminal, copyLogsCommand, copyTrackerCommand, revealWorkspace
}

nonisolated struct RunAction: Identifiable, Equatable, Sendable {
    let kind: RunActionKind
    let title: String
    let systemImage: String
    var id: String { kind.rawValue }
}

/// Facts needed to build commands and the action list.
nonisolated struct RunActionContext: Equatable, Sendable {
    let issue: String
    /// Config the daemon loaded; passed as `-config` so the command reads
    /// the same daemon state the app is looking at.
    var configPath: String?
    /// Run working directory, only when it exists on this Mac.
    var localWorkspace: String?
    var rexPath: String?
    /// The machine that runs the agent when it is not this Mac. Rex opens
    /// the run there (`rex -S <host>`); local-only actions are not offered.
    var remoteHost: String?

    /// `lambe logs -issue ID -follow [-config X]`.
    var logsArgv: [String] {
        var argv = ["lambe", "logs", "-issue", issue, "-follow"]
        // A config path on this Mac means nothing on another machine.
        if remoteHost == nil, let configPath, !configPath.isEmpty { argv += ["-config", configPath] }
        return argv
    }

    /// `rex -S <host>` arguments, empty for a run on this Mac.
    var rexHostArgs: [String] {
        guard let remoteHost, !remoteHost.isEmpty else { return [] }
        return ["-S", remoteHost]
    }

    var logsCommand: String { Self.shellLine(logsArgv) }
    var trackerCommand: String { Self.shellLine(["bd", "show", issue]) }
    /// Rex window label; reused to focus an existing window.
    var rexLabel: String { "Lambe · \(issue)" }

    /// POSIX single-quote escaping for every word that needs it.
    static func shellLine(_ argv: [String]) -> String {
        argv.map(shellQuote).joined(separator: " ")
    }

    static func shellQuote(_ word: String) -> String {
        let safe = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:=@%+,")
        if !word.isEmpty, word.unicodeScalars.allSatisfy({ safe.contains($0) }) { return word }
        return "'" + word.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    /// Rex first (or Terminal first when the operator prefers it), then the
    /// fallbacks that can work for this run.
    func actions(preferring handoff: Prefs.Handoff = .rex) -> [RunAction] {
        let rex = RunAction(kind: .openInRex, title: "Open in Rex", systemImage: "rectangle.on.rectangle")
        let terminal = RunAction(kind: .openInTerminal, title: "Open in Terminal", systemImage: "terminal")
        var list: [RunAction] = []
        var openers: [RunAction] = []
        if rexPath != nil { openers.append(rex) }
        if remoteHost == nil { openers.append(terminal) }
        if handoff == .terminal { openers.reverse() }
        list += openers
        list.append(RunAction(kind: .copyLogsCommand, title: "Copy logs command", systemImage: "doc.on.doc"))
        list.append(RunAction(kind: .copyTrackerCommand, title: "Copy bd show", systemImage: "doc.on.doc"))
        if localWorkspace != nil, remoteHost == nil {
            list.append(RunAction(kind: .revealWorkspace, title: "Reveal workspace", systemImage: "folder"))
        }
        return list
    }

    var actions: [RunAction] { actions() }
}
