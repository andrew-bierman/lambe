//
//  Tools.swift
//  LambeDash
//
//  Where the `lambe` and `rex` command-line tools live. A GUI app starts
//  with a minimal PATH, so the usual install homes are searched too, and an
//  operator-chosen override (Settings, Tools) wins when it is executable.
//

import Foundation

nonisolated enum ToolLocator {
    /// The first executable `name` among the override, PATH, and the usual
    /// install homes; nil when none is found.
    static func locate(
        _ name: String,
        override: String? = nil,
        env: [String: String] = ProcessInfo.processInfo.environment,
        home: String = NSHomeDirectory(),
        isExecutable: (String) -> Bool = { FileManager.default.isExecutableFile(atPath: $0) }
    ) -> String? {
        if let override, !override.isEmpty, isExecutable(override) { return override }
        let dirs = (env["PATH"] ?? "").split(separator: ":").map(String.init)
            + ["\(home)/.local/bin", "\(home)/go/bin", "/opt/homebrew/bin", "/usr/local/bin"]
        for dir in dirs where isExecutable("\(dir)/\(name)") { return "\(dir)/\(name)" }
        return nil
    }
}

/// Operator preferences, stored in `defaults`. Tests and isolated launches
/// swap `defaults` for a private suite (`-LambeDefaultsSuite`).
enum Prefs {
    /// The store every preference and `@AppStorage` reads. Set once at launch.
    nonisolated(unsafe) static var defaults: UserDefaults = .standard

    static let startInDemo = "lambe.dash.startInDemo"
    static let handoff = "lambe.dash.handoff"
    static let lambeOverride = "lambe.dash.lambePath"
    static let rexOverride = "lambe.dash.rexPath"

    /// Where "Open" prefers to send a run.
    enum Handoff: String, CaseIterable, Identifiable {
        case rex, terminal
        var id: String { rawValue }
        var title: String { self == .rex ? "Rex (falls back to Terminal)" : "Terminal" }
    }

    static var preferredHandoff: Handoff {
        Handoff(rawValue: defaults.string(forKey: handoff) ?? "") ?? .rex
    }

    static func locateLambe() -> String? {
        ToolLocator.locate("lambe", override: defaults.string(forKey: lambeOverride))
    }

    static func locateRex() -> String? {
        ToolLocator.locate("rex", override: defaults.string(forKey: rexOverride))
    }
}
