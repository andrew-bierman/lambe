//
//  Setup.swift
//  LambeDash
//
//  First-run support: which daemons are already running on this Mac, which
//  setup steps are done, and the exact `lambe` commands for the ones that
//  are not. Pure and testable. Discovery only reads live descriptors; it
//  never starts, stops, or touches a daemon.
//

import Foundation

/// A running daemon found by scanning state directories.
nonisolated struct DiscoveredDaemon: Identifiable, Equatable, Sendable {
    let stateDirectory: String
    let instance: String
    let pid: Int
    let version: String
    let configPath: String?
    let startedAt: String

    var id: String { stateDirectory + "|" + instance }

    /// A readable default label: the config file's folder, else the instance.
    var suggestedName: String {
        if let configPath, !configPath.isEmpty {
            let url = URL(fileURLWithPath: configPath)
            let folder = url.deletingLastPathComponent().lastPathComponent
            return folder.isEmpty || folder == "/" ? url.lastPathComponent : "\(folder) · \(url.lastPathComponent)"
        }
        return instance
    }
}

nonisolated enum DaemonDiscovery {
    /// State directories worth scanning: the daemon default, the ones the
    /// operator already configured, and any extras, without duplicates.
    static func candidateDirectories(configured: [String], extra: [String] = []) -> [String] {
        var seen = Set<String>()
        return ([DefaultInstance.stateDirectory()] + configured + extra).filter {
            !$0.isEmpty && seen.insert($0).inserted
        }
    }

    /// Live daemons (descriptor present, process alive) under `directories`.
    static func scan(
        _ directories: [String],
        isAlive: (Int) -> Bool = pidIsAlive
    ) -> [DiscoveredDaemon] {
        directories.flatMap { dir -> [DiscoveredDaemon] in
            let live = URL(fileURLWithPath: dir, isDirectory: true).appending(path: "live")
            return DescriptorStore(liveDirectory: live).resolvedEndpoints().compactMap { endpoint in
                let d = endpoint.descriptor
                guard isAlive(d.pid) else { return nil }
                return DiscoveredDaemon(
                    stateDirectory: dir, instance: d.instance, pid: d.pid,
                    version: d.version, configPath: d.configPath, startedAt: d.startedAt
                )
            }
        }
    }

    static func pidIsAlive(_ pid: Int) -> Bool {
        guard pid > 0 else { return false }
        return kill(pid_t(pid), 0) == 0 || errno == EPERM
    }
}

/// One onboarding step: what to do, whether it is already done, and the
/// exact command that does it.
nonisolated struct SetupStep: Identifiable, Equatable, Sendable {
    let id: String
    let title: String
    let detail: String
    let command: String?
    let done: Bool
}

nonisolated struct SetupGuide: Equatable, Sendable {
    var lambePath: String?
    var configPath: String
    var configExists: Bool
    var configEnablesStateAPI: Bool
    var daemons: [DiscoveredDaemon]
    /// The state directory the app is looking at.
    var stateDirectory: String = DefaultInstance.stateDirectory()

    static let installCommand =
        "curl -fsSL https://raw.githubusercontent.com/andrew-bierman/lambe/master/scripts/install.sh | sh"
    static let stateAPISnippet = "stateapi:\n  enable: true"

    /// `lambe init`'s default config: `<user config dir>/lambe/config.yaml`.
    static var defaultConfigPath: String {
        URL.homeDirectory.appending(path: "Library/Application Support/lambe/config.yaml").path
    }

    /// True when `text` turns the state API on under a top-level `stateapi:` key.
    static func configEnablesStateAPI(_ text: String) -> Bool {
        var inBlock = false
        for raw in text.split(separator: "\n", omittingEmptySubsequences: false) {
            let line = String(raw)
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.isEmpty || trimmed.hasPrefix("#") { continue }
            if !line.hasPrefix(" ") && !line.hasPrefix("\t") {
                inBlock = trimmed.hasPrefix("stateapi:")
                continue
            }
            if inBlock, trimmed.hasPrefix("enable:") {
                let value = trimmed.dropFirst("enable:".count)
                    .split(separator: "#").first.map { $0.trimmingCharacters(in: .whitespaces) } ?? ""
                return ["true", "yes", "on"].contains(value.lowercased())
            }
        }
        return false
    }

    /// Reads the guide's disk facts. `read` is the seam tests replace.
    static func inspect(
        lambePath: String?,
        configPath: String = defaultConfigPath,
        stateDirectory: String = DefaultInstance.stateDirectory(),
        daemons: [DiscoveredDaemon],
        read: (String) -> String? = { try? String(contentsOfFile: $0, encoding: .utf8) }
    ) -> SetupGuide {
        let text = read(configPath)
        return SetupGuide(
            lambePath: lambePath,
            configPath: configPath,
            configExists: text != nil,
            configEnablesStateAPI: text.map(configEnablesStateAPI) ?? false,
            daemons: daemons,
            stateDirectory: stateDirectory
        )
    }

    /// `-config <path>` only when the path is not lambe's own default.
    var configFlag: String {
        configPath == Self.defaultConfigPath ? "" : " -config " + RunActionContext.shellQuote(configPath)
    }

    /// The command that starts the daemon with the state API on, without
    /// editing the config.
    var oneOffRunCommand: String { runCommand(lambe: "lambe", enableStateAPI: true) }

    /// `LAMBE_STATE_DIR=...` only when the state directory is not lambe's default.
    private var stateDirPrefix: String {
        stateDirectory == URL.homeDirectory.appending(path: ".local/state/lambe").path
            ? "" : "LAMBE_STATE_DIR=\(RunActionContext.shellQuote(stateDirectory)) "
    }

    /// The command line that starts the daemon, optionally forcing the state API on.
    func runCommand(lambe: String, enableStateAPI: Bool) -> String {
        stateDirPrefix + (enableStateAPI ? "LAMBE_STATEAPI_ENABLE=true " : "")
            + RunActionContext.shellQuote(lambe) + " run" + configFlag
    }

    var steps: [SetupStep] {
        [
            SetupStep(
                id: "install",
                title: "Install lambe",
                detail: lambePath.map { "Found at \($0)." } ?? "The `lambe` command is not on this Mac's PATH.",
                command: lambePath == nil ? Self.installCommand : nil,
                done: lambePath != nil
            ),
            SetupStep(
                id: "init",
                title: "Create a configuration",
                detail: configExists ? "Found \(configPath)." : "Writes the config, a WORKFLOW.md, and a starter formula.",
                command: configExists ? nil : "lambe init",
                done: configExists
            ),
            SetupStep(
                id: "stateapi",
                title: "Turn on the local state API",
                detail: configEnablesStateAPI
                    ? "stateapi.enable is on in the config."
                    : "Off by default. Add this to the config, or start once with the environment variable below. It binds to 127.0.0.1 only and every request needs a per-start token.",
                command: configEnablesStateAPI ? nil : Self.stateAPISnippet,
                done: configEnablesStateAPI || !daemons.isEmpty
            ),
            SetupStep(
                id: "run",
                title: "Start the daemon",
                detail: daemons.isEmpty
                    ? "Run it in a terminal, or keep it running with `lambe service install`."
                    : "\(daemons.count) running.",
                command: daemons.isEmpty ? runCommand(lambe: "lambe", enableStateAPI: !configEnablesStateAPI) : nil,
                done: !daemons.isEmpty
            ),
        ]
    }
}
