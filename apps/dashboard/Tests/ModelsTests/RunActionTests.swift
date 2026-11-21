//
//  RunActionTests.swift
//  LambeDashTests
//
//  Stall classification, run-action planning and execution (fake process
//  runner, no `rex` needed), live/archived standing, and the fixture
//  scenarios previews and screenshots depend on.
//

import Foundation
import Testing

@testable import LambeDash

struct StallPolicyTests {
    private let now = Date(timeIntervalSince1970: 1_800_000_000)

    private func at(_ secondsAgo: TimeInterval) -> String {
        ISO8601DateFormatter().string(from: now.addingTimeInterval(-secondsAgo))
    }

    @Test("Thresholds: flowing, quiet, stalled", arguments: [
        (10.0, RunLiveness.flowing), (119, .flowing), (120, .quiet), (299, .quiet), (300, .stalled), (7200, .stalled),
    ])
    func thresholds(age: Double, expected: RunLiveness) {
        #expect(StallPolicy.classify(state: "running", lastEventAt: at(age), now: now) == expected)
    }

    @Test("Deferred silence is expected; missing time is unobserved, never stalled")
    func specialCases() {
        #expect(StallPolicy.classify(state: "deferred", lastEventAt: at(9000), now: now) == .waiting)
        #expect(StallPolicy.classify(state: "running", lastEventAt: nil, now: now) == .unobserved)
        #expect(StallPolicy.classify(state: "running", lastEventAt: "garbage", now: now) == .unobserved)
    }

    @Test("Silence formatting")
    func formatting() {
        #expect(StallPolicy.format(40) == "40s")
        #expect(StallPolicy.format(540) == "9m")
        #expect(StallPolicy.format(3900) == "1h 5m")
    }
}

struct RunActionPlanTests {
    @Test("Shell quoting survives spaces and quotes")
    func quoting() {
        #expect(RunActionContext.shellQuote("abc-1") == "abc-1")
        #expect(RunActionContext.shellQuote("/Users/me/my cfg.yaml") == "'/Users/me/my cfg.yaml'")
        #expect(RunActionContext.shellQuote("it's") == "'it'\\''s'")
        #expect(RunActionContext.shellQuote("") == "''")
    }

    @Test("Logs command pins the daemon's config")
    func logsCommand() {
        let ctx = RunActionContext(issue: "lam-1", configPath: "/a b/lambe.yaml")
        #expect(ctx.logsCommand == "lambe logs -issue lam-1 -follow -config '/a b/lambe.yaml'")
        #expect(RunActionContext(issue: "lam-1").logsCommand == "lambe logs -issue lam-1 -follow")
    }

    @Test("Rex action exists only when Rex is installed; fallbacks always do")
    func rexGating() {
        let withRex = RunActionContext(issue: "x", rexPath: "/bin/rex").actions.map(\.kind)
        let without = RunActionContext(issue: "x").actions.map(\.kind)
        #expect(withRex.first == .openInRex)
        #expect(!without.contains(.openInRex))
        for kinds in [withRex, without] {
            #expect(kinds.contains(.openInTerminal))
            #expect(kinds.contains(.copyLogsCommand))
            #expect(!kinds.contains(.revealWorkspace))
        }
        #expect(RunActionContext(issue: "x", localWorkspace: "/tmp").actions.map(\.kind).contains(.revealWorkspace))
    }
}

actor FakeRunner: CommandRunning {
    private(set) var calls: [[String]] = []
    private var results: [Int32?]
    init(_ results: [Int32?]) { self.results = results }
    func run(_ executable: String, _ args: [String]) async -> Int32? {
        calls.append([executable] + args)
        return results.isEmpty ? nil : results.removeFirst()
    }
}

struct RunActionRunnerTests {
    private let ctx = RunActionContext(issue: "lam-1", configPath: "/c.yaml", rexPath: "/bin/rex")

    @Test("An existing Rex window is focused, not duplicated")
    func focusesExisting() async {
        let runner = FakeRunner([0])
        let outcome = await RunActionRunner.openInRex(ctx, runner: runner)
        #expect(outcome.ok)
        #expect(await runner.calls == [["/bin/rex", "window", "focus", "Lambe · lam-1"]])
    }

    @Test("A missing window is created following the run's logs")
    func createsWhenAbsent() async {
        let runner = FakeRunner([1, 0])
        let outcome = await RunActionRunner.openInRex(ctx, runner: runner)
        #expect(outcome.ok)
        #expect(await runner.calls.last == [
            "/bin/rex", "window", "new", "Lambe · lam-1", "--",
            "lambe", "logs", "-issue", "lam-1", "-follow", "-config", "/c.yaml",
        ])
    }

    @Test("A failing Rex reports the failure instead of staying silent")
    func reportsFailure() async {
        let outcome = await RunActionRunner.openInRex(ctx, runner: FakeRunner([1, 1]))
        #expect(!outcome.ok)
        #expect(outcome.message.contains("Terminal"))
        let noRex = await RunActionRunner.openInRex(RunActionContext(issue: "x"), runner: FakeRunner([]))
        #expect(!noRex.ok)
    }

    @Test("Rex lookup searches PATH then the usual install homes")
    func locator() {
        let found = RexLocator.locate(env: ["PATH": "/usr/bin"], isExecutable: { $0 == "/opt/homebrew/bin/rex" })
        #expect(found == "/opt/homebrew/bin/rex")
        #expect(RexLocator.locate(env: ["PATH": "/usr/bin"], isExecutable: { _ in false }) == nil)
    }
}

@MainActor
struct FixtureScenarioTests {
    @Test("Standing: snapshot run is live, history is archived, terminal snapshot row is archived")
    func standing() throws {
        let stalled = ConnectionModel.fixture(.stalled)
        let live = SeedResolver.resolve(
            issue: "lam-103", snapshot: stalled.snapshotFeed.payload,
            queue: stalled.queueFeed.payload, history: stalled.historyFeed.payload
        )
        #expect(live?.standing == .live)
        let archived = SeedResolver.resolve(
            issue: "lam-090", snapshot: stalled.snapshotFeed.payload,
            queue: stalled.queueFeed.payload, history: stalled.historyFeed.payload
        )
        #expect(archived?.standing == .archived)
        let queued = SeedResolver.resolve(
            issue: "lam-110", snapshot: stalled.snapshotFeed.payload,
            queue: stalled.queueFeed.payload, history: stalled.historyFeed.payload
        )
        #expect(queued?.standing == .queued)
        #expect(RunSeed(issue: "x", title: nil, phase: nil, state: "settled", attempt: 1,
                        updatedAt: nil, blocked: false, source: .live).standing == .archived)
    }

    @Test("The stalled scenario has two stalled runs; live has none")
    func stallCounts() {
        func stalled(_ s: FixtureScenario) -> Int {
            let m = ConnectionModel.fixture(s)
            return StallPolicy.stalledRuns(QueueComposition.activeRuns(in: m.snapshotFeed.payload), now: Date()).count
        }
        #expect(stalled(.stalled) == 2)
        #expect(stalled(.live) == 0)
    }

    @Test("Every scenario builds without polling or a client")
    func allScenarios() async {
        for scenario in FixtureScenario.allCases {
            let model = ConnectionModel.fixture(scenario)
            #expect(model.isFixture)
            await model.tick()
            #expect(model.client == nil)
        }
    }

    @Test("Fixture detail answers only its own issue; others 404 into seed facts")
    func fixtureDetail() async throws {
        let model = ConnectionModel.fixture(.stalled)
        let fetch = try #require(model.detailFetch(id: "lam-103"))
        #expect(try await fetch().issue == "lam-103")
        let other = try #require(model.detailFetch(id: "lam-090"))
        await #expect(throws: APIError.self) { _ = try await other() }
    }

    @Test("Launch arguments select a scenario, section, and issue")
    func launchArguments() {
        let s = LaunchScenario.fromArguments(["app", "-LambeScenario", "stalled", "-LambeSection", "history", "-LambeIssue", "lam-1"])
        #expect(s?.fixture == .stalled && s?.section == .history && s?.issue == "lam-1")
        #expect(LaunchScenario.fromArguments(["app"]) == nil)
        #expect(LaunchScenario.fromArguments(["app", "-LambeScenario", "nope"]) == nil)
    }

    @Test("A directory with no descriptors reads as offline")
    func offlineInstance() {
        let record = InstanceRecord(name: "ghost", stateDirectory: "/tmp/lambe-no-such-\(UUID().uuidString)")
        let status = InstanceStatus.fromDescriptors(record)
        #expect(status.tone == .bad)
        #expect(status.detail.hasPrefix("offline"))
    }
}
