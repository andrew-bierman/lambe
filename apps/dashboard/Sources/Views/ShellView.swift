//
//  ShellView.swift
//  LambeDash
//
//  The window shell (U8): a three-column `NavigationSplitView` — sidebar
//  (Queue / Attention / History), the selected section's list, and the run
//  detail column — with the toolbar's refresh and Demo/Live controls and
//  the connection footer. Selection is tracked by issue ID, never index,
//  so polls that reorder or extend the payloads never move the operator's
//  selection (R11). The single poll loop is bound to this view's `.task`
//  lifetime: leaving the shell cancels it.
//

import SwiftUI

struct ShellView: View {
    @Environment(ConnectionModel.self) private var connection
    @Environment(\.scenePhase) private var scenePhase

    /// Section, selected issue and run actions, owned by the app so the
    /// menu bar drives the same state. Payload updates never write to it,
    /// so selection survives reordering (the U8 selection rule), and a
    /// change of connection context clears it.
    @Environment(ShellModel.self) private var shell

    /// No daemon to show and none attached: the setup guide replaces the
    /// three columns. Demo data and attached daemons never need it unless
    /// the operator asks.
    private var showsSetup: Bool {
        if shell.showSetup { return true }
        guard connection.mode == .live else { return false }
        if case .neverConnected = connection.state { return true }
        return false
    }

    var body: some View {
        Group {
            if showsSetup {
                OnboardingView()
            } else {
                splitView
            }
        }
        .background(WindowSizeApplier())
        .toolbar { toolbar }
        .navigationTitle("LambeDash")
        .navigationSubtitle(showsSetup ? "Setup" : subtitle)
        .task { await connection.pollLoop() }
        .onChange(of: scenePhase) { _, phase in
            if phase == .active {
                connection.refreshIfStale()
            }
        }
        .onChange(of: connection.contextID) { shell.contextChanged() }
        .animation(.default, value: connection.state)
    }

    private var splitView: some View {
        @Bindable var shell = shell
        return NavigationSplitView {
            SidebarView(section: $shell.section)
                .navigationSplitViewColumnWidth(min: 190, ideal: 220)
        } content: {
            contentColumn
                .navigationSplitViewColumnWidth(min: 340, ideal: 400)
        } detail: {
            RunDetailView(
                selectedIssue: shell.selectedIssue,
                seed: SeedResolver.resolve(
                    issue: shell.selectedIssue,
                    snapshot: connection.snapshotFeed.payload,
                    queue: connection.queueFeed.payload,
                    history: connection.historyFeed.payload
                )
            )
            .navigationSplitViewColumnWidth(min: 320, ideal: 440)
        }
        .frame(minWidth: 960, idealWidth: 1200, minHeight: 600, idealHeight: 780)
        .safeAreaInset(edge: .bottom) {
            StatusFooter()
        }
    }

    // MARK: - Content column

    @ViewBuilder
    private var contentColumn: some View {
        VStack(spacing: 0) {
            ConnectionHeader()
            ConnectionBanners()
            @Bindable var shell = shell
            switch shell.section {
            case .queue: QueueView(selectedIssue: $shell.selectedIssue)
            case .attention: AttentionView(selectedIssue: $shell.selectedIssue)
            case .history: HistoryView(selectedIssue: $shell.selectedIssue)
            }
        }
    }

    // MARK: - Toolbar

    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        ToolbarItem(placement: .primaryAction) {
            Picker("Data source", selection: Binding(
                get: { connection.mode },
                set: { connection.setMode($0) }
            )) {
                ForEach(ConnectionModel.Mode.allCases) { mode in
                    Text(mode.rawValue).tag(mode)
                }
            }
            .pickerStyle(.segmented)
            .accessibilityLabel("Data source: live daemon or demo sample data")
        }
        ToolbarItem(placement: .primaryAction) {
            Button {
                connection.refreshNow()
            } label: {
                Label("Refresh", systemImage: "arrow.clockwise")
            }
            .disabled(connection.mode == .demo)
            .help("Refresh (⌘R)")
            .accessibilityLabel("Refresh state from the daemon")
        }
    }

    /// The window subtitle: which daemon the operator is looking at (R14).
    private var subtitle: String {
        if connection.mode == .demo {
            return "Demo data"
        }
        var parts: [String] = []
        if let name = connection.instanceName {
            parts.append(name)
        }
        if let daemon = connection.daemon {
            parts.append(daemon.versionPidText)
        }
        let stalled = StallPolicy.stalledRuns(
            QueueComposition.activeRuns(in: connection.snapshotFeed.payload), now: Date()
        ).count
        if stalled > 0 {
            parts.append("\(stalled) STALLED")
        }
        return parts.joined(separator: " · ")
    }
}

/// The sidebar: the three operator sections with live counts.
struct SidebarView: View {
    @Environment(ConnectionModel.self) private var connection
    @Binding var section: SidebarSection

    var body: some View {
        List(selection: $section) {
            Section("Views") {
                ForEach(SidebarSection.allCases) { item in
                    if let count = count(for: item) {
                        Label(item.title, systemImage: item.systemImage)
                            .badge(count)
                            .tag(item)
                            .accessibilityLabel("\(item.title), \(count) items")
                    } else {
                        Label(item.title, systemImage: item.systemImage)
                            .tag(item)
                            .accessibilityLabel("\(item.title), no items")
                    }
                }
            }
            InstancesSection()
        }
        .listStyle(.sidebar)
        .navigationTitle("Views")
    }

    /// Row count per section — exactly the rows each section renders.
    /// The queue badge shares `QueueComposition` with the list, so a
    /// settled row in History is never counted as a queued run and an
    /// issue with both payloads is never counted twice.
    private func count(for item: SidebarSection) -> Int? {
        switch item {
        case .queue:
            let total = QueueComposition.renderedRowCount(
                snapshot: connection.snapshotFeed.payload,
                queue: connection.queueFeed.payload
            )
            return total > 0 ? total : nil
        case .attention:
            let items = connection.snapshotFeed.payload?.needsHuman.count ?? 0
            return items > 0 ? items : nil
        case .history:
            let runs = connection.historyFeed.payload?.runs.count ?? 0
            return runs > 0 ? runs : nil
        }
    }
}
