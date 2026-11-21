//
//  LambeDashApp.swift
//  lambe operator dashboard
//
//  lambe operator dashboard entry point. One main window hosting the
//  three-column shell (or the setup guide when no daemon is attached), a
//  menu bar with the navigation, refresh, and run commands, and a Settings
//  scene. The connection and shell models are created once here and
//  injected into every scene; the app owns no daemon process and no
//  tracker access — presentation only.
//
//  The project compiles with `SWIFT_DEFAULT_ACTOR_ISOLATION = MainActor`,
//  so everything here is MainActor-isolated by default.
//

import SwiftUI

@main
struct LambeDashApp: App {
    /// The app-level connection: lifecycle, single poll loop, feeds.
    @State private var connection: ConnectionModel
    @State private var shell: ShellModel

    init() {
        let options = LaunchOptions.fromArguments()
        if let suite = options.defaultsSuite, let defaults = UserDefaults(suiteName: suite) {
            Prefs.defaults = defaults
        }
        DefaultInstance.launchOverride = options.stateDirectory
        let model: ConnectionModel
        if let scenario = options.scenario {
            model = ConnectionModel.fixture(scenario)
        } else {
            model = ConnectionModel(instanceStore: InstanceStore(defaults: Prefs.defaults))
            if options.demo || Prefs.defaults.bool(forKey: Prefs.startInDemo) {
                model.setMode(.demo)
            }
        }
        if let appearance = options.appearance {
            NSApplication.shared.appearance = NSAppearance(named: appearance == "dark" ? .darkAqua : .aqua)
        }
        LaunchWindowSizer.size = options.windowSize
        LaunchWindowSizer.headless = options.headless
        // Hosted unit tests must never grab focus either.
        if options.headless || ProcessInfo.processInfo.environment["XCTestConfigurationFilePath"] != nil {
            NSApplication.shared.setActivationPolicy(options.headless ? .accessory : .prohibited)
        }
        let shellModel = ShellModel(section: options.section ?? .queue, issue: options.issue)
        _connection = State(initialValue: model)
        _shell = State(initialValue: shellModel)
        if options.headless {
            // Automation-safe window: created by hand, sent behind every other
            // window, never activated, so a capture never touches focus.
            let size = options.windowSize ?? CGSize(width: 1200, height: 780)
            DispatchQueue.main.async {
                HeadlessWindow.present(connection: model, shell: shellModel, size: size)
            }
        }
    }

    var body: some Scene {
        WindowGroup {
            ShellView()
                .environment(connection)
                .environment(shell)
        }
        .defaultSize(width: 1200, height: 780)
        .windowResizability(.contentMinSize)
        .commands { DashboardCommands(connection: connection, shell: shell) }

        Settings {
            SettingsView()
                .environment(connection)
                .environment(shell)
        }
    }
}

/// Launch arguments for demos, screenshots, and isolated runs:
///
/// - `--demo` (or `-LambeDemo`): start with the bundled sample data.
/// - `-LambeScenario <name>`: render a network-free fixture scenario.
/// - `-LambeSection queue|attention|history`, `-LambeIssue <id>`: initial view.
/// - `-LambeStateDir <path>`: the default state directory, instead of
///   `LAMBE_STATE_DIR` / `~/.local/state/lambe`.
/// - `-LambeHeadless`: never activate or raise (automation-safe).
/// - `-LambeWindowSize WxH`, `-LambeAppearance light|dark`: for screenshots.
/// - `-LambeDefaultsSuite <name>`: keep preferences and instances in that
///   defaults domain instead of the app's own.
struct LaunchOptions: Equatable {
    var demo = false
    var scenario: FixtureScenario?
    var section: SidebarSection?
    var issue: String?
    var stateDirectory: String?
    var defaultsSuite: String?
    /// `-LambeWindowSize 1200x780`: content size applied once at launch.
    var windowSize: CGSize?
    /// `-LambeAppearance light|dark`.
    var appearance: String?
    /// `-LambeHeadless`: never take focus or come to the front (accessory
    /// activation policy, window sent behind others). For automated runs.
    var headless = false

    static func fromArguments(_ args: [String] = ProcessInfo.processInfo.arguments) -> LaunchOptions {
        func value(_ key: String) -> String? {
            args.firstIndex(of: key).flatMap { args.indices.contains($0 + 1) ? args[$0 + 1] : nil }
        }
        var options = LaunchOptions()
        options.demo = args.contains("--demo") || args.contains("-LambeDemo")
        options.scenario = value("-LambeScenario").flatMap(FixtureScenario.init(rawValue:))
        options.section = value("-LambeSection").flatMap(SidebarSection.init(rawValue:))
        options.issue = value("-LambeIssue")
        options.stateDirectory = value("-LambeStateDir")
        options.defaultsSuite = value("-LambeDefaultsSuite")
        options.appearance = value("-LambeAppearance")
        options.headless = args.contains("-LambeHeadless")
        if let size = value("-LambeWindowSize")?.split(separator: "x").compactMap({ Double($0) }), size.count == 2 {
            options.windowSize = CGSize(width: size[0], height: size[1])
        }
        return options
    }
}

/// Applies `-LambeWindowSize` to the first window that appears, once.
enum LaunchWindowSizer {
    nonisolated(unsafe) static var size: CGSize?
    nonisolated(unsafe) static var headless = false
}

struct WindowSizeApplier: NSViewRepresentable {
    final class Probe: NSView {
        override func viewDidMoveToWindow() {
            guard let window, LaunchWindowSizer.size != nil || LaunchWindowSizer.headless else { return }
            let size = LaunchWindowSizer.size
            let headless = LaunchWindowSizer.headless
            // SwiftUI sizes the window after it is attached; apply ours last.
            for delay in [0.1, 0.4, 0.9, 1.6] {
                DispatchQueue.main.asyncAfter(deadline: .now() + delay) {
                    if let size { window.setContentSize(size) }
                    if headless { window.orderBack(nil) }
                }
            }
        }
    }

    func makeNSView(context: Context) -> NSView { Probe() }
    func updateNSView(_ nsView: NSView, context: Context) {}
}

/// An off-screen, never-key window hosting the real shell, for `-LambeHeadless`.
@MainActor
enum HeadlessWindow {
    private final class Window: NSWindow {
        override func constrainFrameRect(_ frameRect: NSRect, to screen: NSScreen?) -> NSRect { frameRect }
        override var canBecomeKey: Bool { false }
        override var canBecomeMain: Bool { false }
        // Drawn as the active window (colored controls) without ever being made key.
        override var isKeyWindow: Bool { true }
        override var isMainWindow: Bool { true }
    }

    private static var window: NSWindow?

    static func present(connection: ConnectionModel, shell: ShellModel, size: CGSize) {
        let root = ShellView().environment(connection).environment(shell)
        let host = NSHostingController(rootView: root)
        let window = Window(
            contentRect: NSRect(origin: .zero, size: size),
            styleMask: [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView],
            backing: .buffered, defer: false
        )
        window.contentViewController = host
        window.setContentSize(size)
        window.setFrameOrigin(NSPoint(x: 40, y: 40))
        window.collectionBehavior = [.stationary, .ignoresCycle]
        window.isReleasedWhenClosed = false
        window.title = "LambeDash"
        window.orderBack(nil)
        self.window = window
    }
}
