//
//  AppExperienceTests.swift
//  LambeDashTests
//
//  First-run setup, daemon discovery, remote-host run actions, selection
//  isolation between daemons, live-detail refresh, and the packaged icon.
//

import Foundation
import Testing

@testable import LambeDash

struct SetupGuideTests {
    private func guide(
        lambe: String? = "/bin/lambe", config: String? = nil, daemons: [DiscoveredDaemon] = [],
        stateDir: String = URL.homeDirectory.appending(path: ".local/state/lambe").path
    ) -> SetupGuide {
        SetupGuide.inspect(
            lambePath: lambe, configPath: SetupGuide.defaultConfigPath, stateDirectory: stateDir,
            daemons: daemons, read: { _ in config }
        )
    }

    private let daemon = DiscoveredDaemon(
        stateDirectory: "/s", instance: "lambe-1", pid: 1, version: "0.9.3",
        configPath: "/Users/me/work/lambe.yaml", startedAt: "2026-11-20T00:00:00Z"
    )

    @Test("Nothing installed: every step is open with its exact command")
    func nothingDone() {
        let steps = guide(lambe: nil).steps
        #expect(steps.map(\.done) == [false, false, false, false])
        #expect(steps[0].command == SetupGuide.installCommand)
        #expect(steps[1].command == "lambe init")
        #expect(steps[2].command == "stateapi:\n  enable: true")
        #expect(steps[3].command == "LAMBE_STATEAPI_ENABLE=true lambe run")
    }

    @Test("A config that enables the state API runs plain `lambe run`")
    func enabledConfig() {
        let g = guide(config: "workflow_path: x\nstateapi:\n  enable: true\n")
        #expect(g.steps.map(\.done) == [true, true, true, false])
        #expect(g.steps[3].command == "lambe run")
    }

    @Test("A running daemon completes the guide")
    func running() {
        let g = guide(config: "stateapi:\n  enable: true", daemons: [daemon])
        #expect(g.steps.allSatisfy { $0.done })
        #expect(g.steps.allSatisfy { $0.command == nil })
    }

    @Test("State API detection reads only the top-level stateapi block", arguments: [
        ("stateapi:\n  enable: true", true),
        ("stateapi:\n  enable: true # on", true),
        ("stateapi:\n  addr: 127.0.0.1:0\n  enable: yes", true),
        ("stateapi:\n  enable: false", false),
        ("other:\n  enable: true\nstateapi:\n  addr: x", false),
        ("# stateapi:\n#   enable: true", false),
        ("", false),
    ])
    func detection(text: String, expected: Bool) {
        #expect(SetupGuide.configEnablesStateAPI(text) == expected)
    }

    @Test("A non-default config path and state dir are pinned in the run command")
    func pinned() {
        var g = guide(config: "x")
        g.configPath = "/a b/lambe.yaml"
        g.stateDirectory = "/tmp/iso"
        #expect(g.runCommand(lambe: "/opt/bin/lambe", enableStateAPI: true)
            == "LAMBE_STATE_DIR=/tmp/iso LAMBE_STATEAPI_ENABLE=true /opt/bin/lambe run -config '/a b/lambe.yaml'")
    }

    @Test("Discovery returns live daemons only, without duplicates")
    func discovery() throws {
        let root = FileManager.default.temporaryDirectory.appending(path: "lambe-disc-\(UUID().uuidString)")
        let live = root.appending(path: "live")
        try FileManager.default.createDirectory(at: live, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        try DescriptorDocuments.json(instance: "alive", pid: "111").write(to: live.appending(path: "stateapi-alive.json"), atomically: true, encoding: .utf8)
        try DescriptorDocuments.json(instance: "dead", pid: "222").write(to: live.appending(path: "stateapi-dead.json"), atomically: true, encoding: .utf8)
        let found = DaemonDiscovery.scan([root.path], isAlive: { $0 == 111 })
        #expect(found.map(\.instance) == ["alive"])
        #expect(found[0].stateDirectory == root.path)
        #expect(DaemonDiscovery.candidateDirectories(configured: ["/a", "/a", ""], extra: ["/b"]).filter { $0 == "/a" }.count == 1)
        #expect(!DaemonDiscovery.pidIsAlive(0))
        #expect(DaemonDiscovery.pidIsAlive(Int(getpid())))
    }

    @Test("Suggested names come from the config's folder")
    func names() {
        #expect(daemon.suggestedName == "work · lambe.yaml")
        #expect(DiscoveredDaemon(stateDirectory: "/s", instance: "lambe-9", pid: 1, version: "", configPath: nil, startedAt: "").suggestedName == "lambe-9")
    }
}

struct RemoteRunActionTests {
    private let remote = RunActionContext(issue: "lam-1", configPath: "/local.yaml", localWorkspace: "/tmp", rexPath: "/bin/rex", remoteHost: "mac-mini")

    @Test("A run on another machine opens through `rex -S host`, without this Mac's config")
    func remoteRex() async {
        let runner = FakeRunner([1, 0])
        let outcome = await RunActionRunner.openInRex(remote, runner: runner)
        #expect(outcome.ok && outcome.message.contains("mac-mini"))
        #expect(await runner.calls == [
            ["/bin/rex", "-S", "mac-mini", "window", "focus", "Lambe · lam-1"],
            ["/bin/rex", "-S", "mac-mini", "window", "new", "Lambe · lam-1", "--", "lambe", "logs", "-issue", "lam-1", "-follow"],
        ])
    }

    @Test("Remote runs offer no Terminal or Finder action")
    func remoteActions() {
        let kinds = remote.actions.map(\.kind)
        #expect(kinds == [.openInRex, .copyLogsCommand, .copyTrackerCommand])
        #expect(RunActionContext(issue: "x", remoteHost: "h").actions.map(\.kind) == [.copyLogsCommand, .copyTrackerCommand])
    }

    @Test("Terminal preference reorders the openers; Rex is still offered")
    func preference() {
        let ctx = RunActionContext(issue: "x", rexPath: "/bin/rex")
        #expect(ctx.actions(preferring: .terminal).map(\.kind).prefix(2) == [.openInTerminal, .openInRex])
        #expect(ctx.actions(preferring: .rex).map(\.kind).prefix(2) == [.openInRex, .openInTerminal])
    }

    @Test("A failed remote Rex says which host it could not reach")
    func remoteFailure() async {
        let outcome = await RunActionRunner.openInRex(remote, runner: FakeRunner([1, 1]))
        #expect(!outcome.ok && outcome.message.contains("mac-mini"))
    }
}

@MainActor
struct ShellAndDetailTests {
    @Test("Changing the connection context clears the selection and outcome")
    func contextClears() {
        let shell = ShellModel(section: .history, issue: "ac-123")
        shell.outcome = ActionOutcome(ok: true, message: "x")
        shell.contextChanged()
        #expect(shell.selectedIssue == nil && shell.outcome == nil)
        #expect(shell.section == .history)
    }

    @Test("Target and mode switches bump the context; a same-daemon poll does not")
    func contextID() {
        let model = ConnectionModel.fixture(.live)
        let start = model.contextID
        model.connectionTargetChanged()
        #expect(model.contextID == start + 1)
        model.setMode(.demo)
        #expect(model.contextID == start + 2)
    }

    @Test("Demo mode serves demo run detail, with output")
    func demoDetail() async throws {
        let model = ConnectionModel()
        model.setMode(.demo)
        let fetch = try #require(model.detailFetch(id: "demo-hk4"))
        let issue = try await fetch()
        #expect(issue.tail?.contains("gh pr checks") == true)
        #expect(model.detailFetch(id: "demo-kd8") == nil)
    }

    @Test("A refresh updates the tail in place and ignores a stale id")
    func refresh() async throws {
        let model = ConnectionModel.fixture(.live)
        let first = try #require(model.detailFetch(id: "lam-101"))
        await model.detail.load(id: "lam-101", fetch: first)
        #expect(model.detail.phase == .loaded)
        let before = model.detail.issue?.tail
        await model.detail.refresh(id: "lam-999", fetch: first)
        #expect(model.detail.issue?.tail == before)
        await model.detail.refresh(id: "lam-101", fetch: { throw APIError.status(code: 500) })
        #expect(model.detail.phase == .loaded && model.detail.issue != nil)
    }

    @Test("Run actions resolve a remote host from the run's machine")
    func remoteContext() {
        let model = ConnectionModel()
        model.setMode(.demo)
        let shell = ShellModel()
        #expect(shell.context(for: "demo-zq2", connection: model).remoteHost == "mac-mini")
        #expect(shell.context(for: "demo-hk4", connection: model).remoteHost == nil)
    }

    @Test("Demo mode never launches anything but copies")
    func demoActions() async {
        let shell = ShellModel()
        let runner = FakeRunner([0])
        await shell.perform(.openInRex, RunActionContext(issue: "demo-hk4", rexPath: "/bin/rex"), demo: true, runner: runner)
        #expect(shell.outcome?.ok == false)
        #expect(await runner.calls.isEmpty)
    }

    @Test("Titles come from the readiness and history payloads")
    func titles() {
        let model = ConnectionModel()
        model.setMode(.demo)
        let titles = QueueComposition.titles(queue: model.queueFeed.payload, history: model.historyFeed.payload)
        #expect(titles["demo-hk4"] == "Wire the retry backoff")
        #expect(titles["demo-rv3"] == "Trim the config surface")
    }

    @Test("Tool lookup prefers an executable override")
    func overrides() {
        #expect(ToolLocator.locate("rex", override: "/custom/rex", env: ["PATH": "/usr/bin"], isExecutable: { $0 == "/custom/rex" }) == "/custom/rex")
        #expect(ToolLocator.locate("rex", override: "/gone/rex", env: ["PATH": "/usr/bin"], home: "/h", isExecutable: { $0 == "/h/.local/bin/rex" }) == "/h/.local/bin/rex")
    }
}

struct PackagedIconTests {
    @Test("The built app declares AppIcon and ships the compiled icon")
    func icon() {
        let app = Bundle.main
        #expect(app.object(forInfoDictionaryKey: "CFBundleIconName") as? String == "AppIcon")
        #expect(app.url(forResource: "AppIcon", withExtension: "icns") != nil)
        #expect(app.url(forResource: "Assets", withExtension: "car") != nil)
    }
}

struct VersionLabelTests {
    @Test("A leading v is never doubled", arguments: [("0.9.3", "v0.9.3"), ("v0.9.3-dev+abc", "v0.9.3-dev+abc"), ("demo", "vdemo")])
    func label(raw: String, shown: String) {
        #expect(VersionLabel.text(raw) == shown)
    }
}
