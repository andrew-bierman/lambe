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

    /// The sidebar destination driving the content column.
    @State private var section: SidebarSection = .queue
    /// The selected issue ID across all sections. Authoritative state lives
    /// here — payload updates never write to it, so selection survives
    /// reordering (the U8 selection rule).
    @State private var selectedIssue: String?

    var body: some View {
        NavigationSplitView {
            SidebarView(section: $section)
                .navigationSplitViewColumnWidth(min: 170, ideal: 200)
        } content: {
            contentColumn
                .navigationSplitViewColumnWidth(min: 320, ideal: 380)
        } detail: {
            RunDetailView(
                selectedIssue: selectedIssue,
                seed: SeedResolver.resolve(
                    issue: selectedIssue,
                    snapshot: connection.snapshotFeed.payload,
                    queue: connection.queueFeed.payload,
                    history: connection.historyFeed.payload
                )
            )
            .navigationSplitViewColumnWidth(min: 300, ideal: 420)
        }
        .frame(minWidth: 900, minHeight: 560)
        .safeAreaInset(edge: .bottom) {
            StatusFooter()
        }
        .toolbar { toolbar }
        .navigationTitle("LambeDash")
        .navigationSubtitle(subtitle)
        .task { await connection.pollLoop() }
        .onChange(of: scenePhase) { _, phase in
            if phase == .active {
                connection.refreshIfStale()
            }
        }
        .animation(.default, value: connection.state)
    }

    // MARK: - Content column

    @ViewBuilder
    private var contentColumn: some View {
        VStack(spacing: 0) {
            ConnectionBanners()
            switch section {
            case .queue: QueueView(selectedIssue: $selectedIssue)
            case .attention: AttentionView(selectedIssue: $selectedIssue)
            case .history: HistoryView(selectedIssue: $selectedIssue)
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
            .keyboardShortcut("r", modifiers: .command)
            .disabled(connection.mode == .demo)
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
        return parts.joined(separator: " · ")
    }
}

/// The sidebar: the three operator sections with live counts.
struct SidebarView: View {
    @Environment(ConnectionModel.self) private var connection
    @Binding var section: SidebarSection

    var body: some View {
        List(selection: $section) {
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
