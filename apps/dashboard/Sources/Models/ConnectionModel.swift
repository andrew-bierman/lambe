//
//  ConnectionModel.swift
//  LambeDash
//
//  The app-level daemon connection (KTD7): one observable model owning the
//  R11 connection lifecycle, the single cancellation-aware poll loop, and
//  the R15 recovery policy. The app is a viewer: it owns no daemon process
//  and reads only what the descriptors and the state API publish.
//
//  Loop policy — exactly one poll loop exists, driven by the shell view's
//  `.task` so SwiftUI cancellation stops it immediately (R15):
//  - Attached cadence is ~2 s; failures escalate exponentially, capped
//    near 15 s, and reset on success.
//  - Every request fails fast (short timeouts live in the transport); a
//    failed tick forces endpoint re-resolution before the next attempt, so
//    a launchd restart — new pid, new port, new per-start token — is
//    picked up from fresh descriptors, addr and token together.
//  - A daemon identity change between successful polls also forces
//    re-resolution; last good data stays visible throughout (R11).
//  - Failures are recorded, never swallowed: per-feed reasons surface in
//    the views, and the connection state turns disconnected after two
//    consecutive dead ticks.
//
//  Demo mode renders bundled sample data instead of polling — for
//  exploring the UI and for screenshots without a daemon. It never issues
//  a network request.
//

import Foundation

/// Exponential backoff over the poll interval. Pure and testable.
nonisolated struct BackoffPolicy: Equatable, Sendable {
    /// The attached cadence, seconds.
    let base: TimeInterval
    /// The disconnected ceiling, seconds (R15: capped near 15 s).
    let cap: TimeInterval
    /// The delay the next retry should wait.
    private(set) var current: TimeInterval

    init(base: TimeInterval, cap: TimeInterval) {
        self.base = base
        self.cap = cap
        self.current = base
    }

    /// Doubles the delay, capped at `cap`.
    mutating func escalate() {
        current = min(current * 2, cap)
    }

    /// Returns to the attached cadence after a success.
    mutating func reset() {
        current = base
    }
}

/// The app-level connection: lifecycle, polling, and the three feature
/// feeds. Injected into the view tree via `.environment`.
@MainActor
@Observable
final class ConnectionModel {
    /// The R11 connection lifecycle shared by all views.
    enum ConnectionState: Equatable {
        /// No instance selected, or the selected instance has no live
        /// descriptor, or its descriptor failed admission.
        case neverConnected(reason: NeverConnectedReason)
        /// Connecting: resolving and first fetch in flight.
        case loading
        /// Polling successfully.
        case attached
        /// Attached, but one or more tracker-backed sections are stale
        /// (their `as_of` is older than `staleAfter`) — never silently
        /// mixed: the views show the stale banner with the `as_of` (R11).
        case degraded(staleSections: [String])
        /// Transport failed repeatedly; last good data is retained.
        case disconnected(reason: String)
        /// The selected state directory resolves to more than one running
        /// daemon; surfaced, never silently resolved (R14).
        case ambiguous(instanceNames: [String])
    }

    enum NeverConnectedReason: Equatable, Sendable {
        /// No instance record is selected.
        case noInstance
        /// No live descriptor was found for the selected instance.
        case noDescriptor
        /// A descriptor exists but failed structural admission.
        case descriptorInvalid

        var detail: String {
            switch self {
            case .noInstance:
                "Select a daemon instance in Settings."
            case .noDescriptor:
                "No live endpoint descriptor — start the daemon with stateapi enabled."
            case .descriptorInvalid:
                "The endpoint descriptor is malformed or unsupported."
            }
        }
    }

    /// The toolbar's demo/live control.
    enum Mode: String, CaseIterable, Identifiable, Sendable {
        case live = "Live"
        case demo = "Demo"
        var id: String { rawValue }
    }

    static let pollSeconds: TimeInterval = 2
    static let maxPollSeconds: TimeInterval = 15
    /// A tracker-backed section older than this is degraded (R11). The
    /// daemon's canonical read refreshes well under this in normal
    /// operation.
    static let staleAfter: TimeInterval = 120
    /// Dead ticks before declaring the daemon disconnected.
    static let disconnectAfterFailures = 2

    /// Live daemon polling or bundled sample data.
    var mode: Mode = .live

    private(set) var state: ConnectionState = .loading
    /// The attached daemon's identity facts — rendered in the window
    /// footer so the operator always knows which daemon they see (R9, R14).
    private(set) var daemon: WireDaemonIdentity?
    /// R12: the daemon speaks a newer schema than this app supports.
    private(set) var newerDaemon = false
    /// When the last fully successful tick completed.
    private(set) var lastSync: Date?

    /// The selected instance record's display name, when any.
    var instanceName: String? { instanceStore.selected?.name }

    let instanceStore: InstanceStore
    let snapshotFeed = FeedModel<WireSnapshot>()
    let queueFeed = FeedModel<WireQueue>()
    let historyFeed = FeedModel<WireHistory>()
    let detail = DetailModel()

    private var consecutiveFailures = 0
    private var hasEverAttached = false
    private var attachedIdentity: WireDaemonIdentity?
    /// Guards against a manual refresh overlapping the loop's tick — the
    /// single-polling rule means at most one tick runs at a time.
    private var tickInFlight = false
    /// Bumped on every connection target or mode change. A tick captures
    /// it at entry and drops its results when it has changed by the time
    /// the fetches resume: a tick suspended mid-flight must never write
    /// the old target's payloads into the newly chosen one.
    private var connectionRevision = 0
    /// A refresh requested while a tick was in flight (a target switch or
    /// ⌘R landing mid-tick). Released as one immediate follow-up tick
    /// when the in-flight tick lets go, so the new target is polled
    /// promptly instead of waiting out the cadence sleep.
    private var tickQueued = false

    /// The active typed client, rebuilt only by `resolveClient`. Internal
    /// (not private) as the test seam: model tests inject a stub to drive
    /// the lifecycle without a daemon or a disk.
    var client: (any DaemonClient)?

    /// The live backoff state; readable so tests assert escalation and
    /// reset on the real loop, not a parallel copy.
    private(set) var backoff = BackoffPolicy(
        base: ConnectionModel.pollSeconds,
        cap: ConnectionModel.maxPollSeconds
    )

    init(instanceStore: InstanceStore = InstanceStore()) {
        self.instanceStore = instanceStore
    }

    /// True when live polling is wired to a daemon right now.
    var isLive: Bool { mode == .live && client != nil }

    /// True when the connection lifecycle is attached or degraded — the
    /// states where feed content is authoritative. Demo sample data fills
    /// the same feeds and is identified by the footer and subtitle, not
    /// by a disconnected warning over live-looking content.
    var isConnectionActive: Bool {
        switch state {
        case .attached, .degraded: true
        default: false
        }
    }

    /// True while a connection attempt is in flight toward the first
    /// attach. A failed resolution is not "loading" — it surfaces the
    /// explainer instead of an endless spinner.
    var isConnectionLoading: Bool {
        state == .loading && !hasEverAttached
    }

    // MARK: - Loop

    /// The single poll loop. Runs until its task is cancelled — sleeping
    /// through `Task.sleep` means cancellation interrupts the wait and the
    /// loop exits immediately.
    func pollLoop() async {
        while !Task.isCancelled {
            await tick()
            do {
                try await Task.sleep(for: .seconds(backoff.current))
            } catch {
                return
            }
        }
    }

    /// One poll cycle: a tick, then the release of any refresh that was
    /// requested while it was in flight — notably a target switch that
    /// landed while the tick's fetches were suspended.
    func tick() async {
        await runTick()
        guard tickQueued, mode == .live, !tickInFlight, !Task.isCancelled else {
            return
        }
        tickQueued = false
        await runTick()
    }

    /// One tick: resolve if needed, fetch the three routes, apply outcomes.
    /// Cancellation-aware: an interrupted fetch leaves state consistent
    /// for the next tick.
    private func runTick() async {
        guard !tickInFlight else { return }
        if mode == .demo {
            applyDemo()
            return
        }
        tickInFlight = true
        defer { tickInFlight = false }
        let startRevision = connectionRevision
        for feed in [snapshotFeed, queueFeed, historyFeed] as [AnyFeedLoading] {
            feed.beginLoading()
        }
        if client == nil {
            resolveClient()
            guard client != nil else {
                backoff.escalate()
                return
            }
        }
        // Unwrap once into a local so the fetches below read one snapshot
        // of the seam; assignments to the property later in this tick
        // (failure re-resolution) still hit the property, not this local.
        guard let active = client else { return }

        let snapshotResult = await result { try await active.state() }
        let queueResult = await result { try await active.queue() }
        let historyResult = await result { try await active.history() }

        // Identity guard: the fetches above suspended this tick, and the
        // operator may have switched target or mode meanwhile (demo
        // sample data may have landed). Results of a superseded
        // generation are dropped whole — none of them may reach the
        // target the operator chose while this tick was in flight.
        guard startRevision == connectionRevision else { return }

        var successes = 0
        var firstFailure: String?
        var snap: WireSnapshot?
        var queuePayload: WireQueue?
        var historyPayload: WireHistory?

        switch snapshotResult {
        case .success(let loaded):
            successes += 1
            snap = loaded
            adoptIdentityIfNeeded(loaded.daemon)
            newerDaemon = loaded.isNewerThanSupported
            snapshotFeed.apply(loaded) {
                $0.runs.isEmpty && $0.needsHuman.isEmpty && $0.gateRejections.isEmpty
            }
        case .failure(let error):
            firstFailure = error.displayText
            snapshotFeed.fail(error.displayText)
        }

        switch queueResult {
        case .success(let loaded):
            successes += 1
            queuePayload = loaded
            queueFeed.apply(loaded) { $0.rows.isEmpty }
        case .failure(let error):
            firstFailure = firstFailure ?? error.displayText
            queueFeed.fail(error.displayText)
        }

        switch historyResult {
        case .success(let loaded):
            successes += 1
            historyPayload = loaded
            historyFeed.apply(loaded) { $0.runs.isEmpty }
        case .failure(let error):
            firstFailure = firstFailure ?? error.displayText
            historyFeed.fail(error.displayText)
        }

        if successes > 0 {
            hasEverAttached = true
            consecutiveFailures = 0
            backoff.reset()
            lastSync = Date()
            if successes == 3 {
                state = Self.degradedState(
                    snapshot: snap,
                    queue: queuePayload,
                    history: historyPayload
                )
            } else {
                // Partial success: attached, with per-feed failure notes
                // visible in the affected sections — not silent, not a
                // disconnect.
                state = .attached
            }
        } else {
            consecutiveFailures += 1
            backoff.escalate()
            // Force re-resolution before the next attempt: after a launchd
            // restart the descriptor carries a new addr and token, and the
            // old client can never recover on its own.
            client = nil
            if consecutiveFailures >= Self.disconnectAfterFailures {
                state = hasEverAttached
                    ? .disconnected(reason: firstFailure ?? "Connection failed")
                    : .neverConnected(reason: .noDescriptor)
            } else if !hasEverAttached {
                state = .loading
            }
        }
    }

    /// Manual refresh (⌘R, toolbar, window activation): one immediate
    /// tick on the existing paths. No POST is ever issued (R13). While a
    /// tick is already in flight the request is queued, not dropped — it
    /// runs the moment the in-flight tick releases.
    func refreshNow() {
        guard !tickInFlight else {
            tickQueued = true
            return
        }
        Task { await tick() }
    }

    /// Window-activation refresh, throttled to the poll cadence.
    func refreshIfStale() {
        guard mode == .live else { return }
        if let last = lastSync, Date().timeIntervalSince(last) < Self.pollSeconds {
            return
        }
        refreshNow()
    }

    /// The connection target changed (Settings selection): drop everything
    /// connection-scoped, keep nothing that could pass for the other
    /// daemon's data, and connect immediately.
    func connectionTargetChanged() {
        connectionRevision += 1
        client = nil
        attachedIdentity = nil
        hasEverAttached = false
        consecutiveFailures = 0
        backoff.reset()
        snapshotFeed.reset()
        queueFeed.reset()
        historyFeed.reset()
        detail.reset()
        newerDaemon = false
        daemon = nil
        lastSync = nil
        if mode == .demo {
            applyDemo()
        } else {
            state = .loading
            refreshNow()
        }
    }

    /// The toolbar demo/live control.
    func setMode(_ newMode: Mode) {
        guard mode != newMode else { return }
        mode = newMode
        connectionRevision += 1
        if newMode == .demo {
            applyDemo()
        } else {
            snapshotFeed.reset()
            queueFeed.reset()
            historyFeed.reset()
            detail.reset()
            state = .loading
            refreshNow()
        }
    }

    /// Builds a detail fetch closure for the current mode, or nil in demo
    /// mode (demo has no live detail — the seed facts carry the column).
    /// The closure captures the existing client; it issues one GET and
    /// nothing else.
    func detailFetch(id: String) -> (() async throws -> WireIssue)? {
        guard mode == .live, let client else { return nil }
        return { try await client.issue(id) }
    }

    // MARK: - Demo

    private func applyDemo() {
        client = nil
        snapshotFeed.apply(SampleData.snapshot) {
            $0.runs.isEmpty && $0.needsHuman.isEmpty && $0.gateRejections.isEmpty
        }
        queueFeed.apply(SampleData.queue) { $0.rows.isEmpty }
        historyFeed.apply(SampleData.history) { $0.runs.isEmpty }
        daemon = SampleData.snapshot.daemon
        newerDaemon = false
        lastSync = Date()
        hasEverAttached = true
        consecutiveFailures = 0
        backoff.reset()
        state = .attached
    }

    // MARK: - Resolution

    /// Resolves the selected instance record to exactly one running daemon
    /// by enumerating its state directory's live descriptors (KTD3, R14).
    /// The client is rebuilt only here — never cached across a target
    /// change — and the bearer token lives inside it, never in UI state.
    private func resolveClient() {
        guard let record = instanceStore.selected else {
            state = .neverConnected(reason: .noInstance)
            return
        }
        let liveDir = URL(fileURLWithPath: record.stateDirectory, isDirectory: true)
            .appending(path: "live")
        var endpoints = DescriptorStore(liveDirectory: liveDir).resolvedEndpoints()
        if let filter = record.instanceName, !filter.isEmpty {
            endpoints = endpoints.filter { $0.instance == filter }
        }
        switch endpoints.count {
        case 0:
            state = hasEverAttached
                ? .disconnected(reason: NeverConnectedReason.noDescriptor.detail)
                : .neverConnected(reason: .noDescriptor)
        case 1:
            do {
                client = try HTTPDaemonClient(descriptor: endpoints[0].descriptor)
            } catch {
                client = nil
                state = .neverConnected(reason: .descriptorInvalid)
            }
        default:
            state = .ambiguous(instanceNames: endpoints.map(\.instance))
        }
    }

    /// Detects a daemon identity change (pid / incarnation) between
    /// polls — the launchd-restart case — and forces re-resolution so the
    /// app reattaches to the new process with its new token (R11).
    private func adoptIdentityIfNeeded(_ newIdentity: WireDaemonIdentity) {
        daemon = newIdentity
        if let previous = attachedIdentity, previous != newIdentity {
            attachedIdentity = newIdentity
            client = nil
        } else if attachedIdentity == nil {
            attachedIdentity = newIdentity
        }
    }

    /// Computes the degraded state from per-section `as_of` freshness.
    /// Sections that have not been read yet are not degraded — the views
    /// show their own waiting copy. Static and pure; `now` is injected so
    /// tests are deterministic.
    static func degradedState(
        snapshot: WireSnapshot?,
        queue: WireQueue?,
        history: WireHistory?,
        now: Date = Date()
    ) -> ConnectionState {
        var stale: [String] = []
        if let asOf = RFC3339.date(snapshot?.needsHumanAsOf),
           now.timeIntervalSince(asOf) > Self.staleAfter
        {
            stale.append("Attention")
        }
        if let asOf = RFC3339.date(queue?.asOf),
           now.timeIntervalSince(asOf) > Self.staleAfter
        {
            stale.append("Queue")
        }
        if let asOf = RFC3339.date(history?.asOf),
           now.timeIntervalSince(asOf) > Self.staleAfter
        {
            stale.append("History")
        }
        return stale.isEmpty ? .attached : .degraded(staleSections: stale)
    }
}

// MARK: - Small async helpers

private extension ConnectionModel {
    /// Captures one async throwing call as a Result, so a failure in one
    /// route never discards the others' outcomes.
    func result<T: Sendable>(_ operation: () async throws -> T) async -> Result<T, APIError> {
        do {
            return .success(try await operation())
        } catch let error as APIError {
            return .failure(error)
        } catch {
            return .failure(.unexpectedResponse)
        }
    }
}

/// Type-erased loading marker so the tick can nudge all feeds without
/// caring about their payload types.
private protocol AnyFeedLoading {
    func beginLoading()
}

extension FeedModel: AnyFeedLoading {}

// MARK: - Detail

/// The selected run's detail state. Loading is driven by the shell's
/// `.task(id:)`, so changing selection cancels the in-flight fetch
/// (cancellation-aware, single fetch per selection).
@MainActor
@Observable
final class DetailModel {
    enum Phase: Equatable {
        /// Nothing selected.
        case idle
        /// Fetching live detail.
        case loading
        /// Live detail decoded.
        case loaded
        /// The daemon has no live record for this issue (e.g. a history
        /// row from an archived attempt) — the seed facts carry the view.
        case seedOnly
        /// The fetch failed; the seed facts still render.
        case failed(String)
    }

    private(set) var phase: Phase = .idle
    private(set) var issue: WireIssue?
    /// The id the current state describes, for stale-completion guards.
    private(set) var loadedID: String?
    /// Bumped by every `load`: a fetch suspended mid-flight is abandoned
    /// when a newer load supersedes it — including a re-load of the same
    /// id, e.g. after a target or mode switch reset the model while the
    /// fetch was suspended.
    private var revision = 0

    /// Loads live detail for `id`. `fetch` is the typed client call; in
    /// demo mode the caller passes nil and the model degrades to seed
    /// facts honestly.
    func load(
        id: String,
        fetch: (() async throws -> WireIssue)?
    ) async {
        revision += 1
        let startRevision = revision
        loadedID = id
        issue = nil
        guard let fetch else {
            phase = .seedOnly
            return
        }
        phase = .loading
        do {
            let detail = try await fetch()
            guard !Task.isCancelled, loadedID == id, revision == startRevision else { return }
            issue = detail
            phase = .loaded
        } catch let error as APIError {
            guard !Task.isCancelled, loadedID == id, revision == startRevision else { return }
            phase = .failed(error.displayText)
        } catch {
            guard !Task.isCancelled, loadedID == id, revision == startRevision else { return }
            phase = .failed(APIError.unexpectedResponse.displayText)
        }
    }

    /// Drops state when the connection target or mode changes.
    func reset() {
        phase = .idle
        issue = nil
        loadedID = nil
    }
}
