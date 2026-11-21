//
//  Previews.swift
//  LambeDash
//
//  Every major view in every state that matters, driven by
//  `FixtureScenario` models: no daemon, no network, no disk descriptors
//  (fixtures never poll). Open this file in Xcode's canvas, or run
//  `apps/dashboard/scripts/screenshots.sh` for the same scenarios as PNGs.
//

import SwiftUI

// MARK: - Whole window

#Preview("Live", traits: .fixedLayout(width: 1080, height: 700)) {
    ShellView().environment(ConnectionModel.fixture(.live))
}

#Preview("Stalled", traits: .fixedLayout(width: 1080, height: 700)) {
    ShellView(initialIssue: "lam-103").environment(ConnectionModel.fixture(.stalled))
}

#Preview("Stalled · dark", traits: .fixedLayout(width: 1080, height: 700)) {
    ShellView(initialIssue: "lam-103").environment(ConnectionModel.fixture(.stalled))
        .preferredColorScheme(.dark)
}

#Preview("Archive", traits: .fixedLayout(width: 1080, height: 700)) {
    ShellView(initialSection: .history, initialIssue: "lam-090")
        .environment(ConnectionModel.fixture(.archive))
}

#Preview("Empty", traits: .fixedLayout(width: 1080, height: 700)) {
    ShellView().environment(ConnectionModel.fixture(.empty))
}

#Preview("Disconnected", traits: .fixedLayout(width: 1080, height: 700)) {
    ShellView().environment(ConnectionModel.fixture(.disconnected))
}

#Preview("Never connected", traits: .fixedLayout(width: 1080, height: 700)) {
    ShellView().environment(ConnectionModel.fixture(.neverConnected))
}

#Preview("Long text", traits: .fixedLayout(width: 1080, height: 700)) {
    ShellView(initialIssue: "lambe-with-an-extraordinarily-long-issue-identifier-0042")
        .environment(ConnectionModel.fixture(.longText))
}

#Preview("Multiple machines", traits: .fixedLayout(width: 1080, height: 700)) {
    ShellView(initialIssue: "lam-103").environment(ConnectionModel.fixture(.multiMachine))
}

// MARK: - Columns and rows

#Preview("Queue · narrow (320pt)", traits: .fixedLayout(width: 320, height: 520)) {
    QueueView(selectedIssue: .constant(nil))
        .environment(ConnectionModel.fixture(.stalled))
}

#Preview("Queue · narrow long text", traits: .fixedLayout(width: 320, height: 420)) {
    QueueView(selectedIssue: .constant(nil))
        .environment(ConnectionModel.fixture(.longText))
}

#Preview("Run rows by liveness") {
    let now = Date()
    let rows = [
        FixtureScenario.run("lam-1", "running", "running", 3),
        FixtureScenario.run("lam-2", "running", "running", 150),
        FixtureScenario.run("lam-3", "running", "gating", 700),
        FixtureScenario.run("lam-4", "deferred", "retry", 900),
    ]
    VStack(spacing: 8) {
        ForEach(Array(rows.enumerated()), id: \.offset) { _, json in
            ActiveRunRow(run: try! JSONDecoder().decode(WireRun.self, from: Data(json.utf8)), fixedNow: now)
        }
    }
    .padding()
    .frame(width: 360)
}

#Preview("Stalled banner") {
    let run = try! JSONDecoder().decode(
        WireRun.self, from: Data(FixtureScenario.run("lam-103", "running", "running", 700).utf8)
    )
    StalledBanner(runs: [run, run]).frame(width: 340).padding()
}

#Preview("Badges") {
    VStack(alignment: .leading, spacing: 10) {
        LivenessBadge(liveness: .stalled, silence: 540)
        LivenessBadge(liveness: .quiet, silence: 150)
        LivenessBadge(liveness: .waiting, silence: nil)
        LivenessBadge(liveness: .unobserved, silence: nil)
        HStack {
            StandingBadge(standing: .live)
            StandingBadge(standing: .queued)
            StandingBadge(standing: .archived)
            StandingBadge(standing: .unknown)
        }
    }
    .padding()
}

#Preview("Instances · machines") {
    let id = { UUID() }
    VStack(alignment: .leading, spacing: 8) {
        InstanceStatusRow(status: InstanceStatus(id: id(), name: "Workbench · app-repo", detail: "workbench · attached", tone: .good, isSelected: true, stalled: 2))
        InstanceStatusRow(status: InstanceStatus(id: id(), name: "Mac mini · infra", detail: "daemon up · pid 811 · v1.0.0", tone: .good, isSelected: false))
        InstanceStatusRow(status: InstanceStatus(id: id(), name: "omarchy · docs-site", detail: "offline: no live daemon descriptor", tone: .bad, isSelected: false))
        InstanceStatusRow(status: InstanceStatus(id: id(), name: "Staging", detail: "2 daemons: a, b", tone: .warning, isSelected: false))
    }
    .padding()
    .frame(width: 240)
}

// MARK: - Detail column

#Preview("Detail · stalled live run", traits: .fixedLayout(width: 440, height: 700)) {
    RunDetailView(selectedIssue: "lam-103", seed: previewSeed("lam-103", .stalled))
        .environment(ConnectionModel.fixture(.stalled))
}

#Preview("Detail · archived", traits: .fixedLayout(width: 440, height: 520)) {
    RunDetailView(selectedIssue: "lam-090", seed: previewSeed("lam-090", .archive))
        .environment(ConnectionModel.fixture(.archive))
}

#Preview("Detail · nothing selected", traits: .fixedLayout(width: 440, height: 300)) {
    RunDetailView(selectedIssue: nil, seed: nil)
        .environment(ConnectionModel.fixture(.empty))
}

#Preview("Actions · Rex installed") {
    RunActionsSection(issue: "lam-103", configPath: "/Users/me/work/lambe.yaml", workspace: "/tmp",
                      rexPath: "/usr/local/bin/rex")
        .padding().frame(width: 420)
}

#Preview("Actions · no Rex (fallback)") {
    RunActionsSection(issue: "lam-103", configPath: nil, workspace: nil, rexPath: nil)
        .padding().frame(width: 420)
}

@MainActor
private func previewSeed(_ issue: String, _ scenario: FixtureScenario) -> RunSeed? {
    let model = ConnectionModel.fixture(scenario)
    return SeedResolver.resolve(
        issue: issue, snapshot: model.snapshotFeed.payload,
        queue: model.queueFeed.payload, history: model.historyFeed.payload
    )
}
