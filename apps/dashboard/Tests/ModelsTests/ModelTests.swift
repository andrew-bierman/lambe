//
//  ModelTests.swift
//  LambeDashTests
//
//  U7/U8 model tests against stubbed fetch outcomes (KTD7 — the models
//  depend on the `DaemonClient` protocol, so no network and no daemon):
//  the Equatable skip at the feed boundary, backoff escalation and reset,
//  the R11 lifecycle over a failing-then-recovering client, staleness →
//  degraded, the U8 phase-to-content mapping table, and selection-by-ID
//  surviving reordered polls.
//

import Foundation
import Testing

@testable import LambeDash

// MARK: - Stub client

/// A `DaemonClient` with canned outcomes per route — the protocol seam
/// doing its job.
nonisolated struct StubClient: DaemonClient, Sendable {
    var snapshot: Result<WireSnapshot, APIError>
    var queue: Result<WireQueue, APIError>
    var history: Result<WireHistory, APIError>
    var issue: Result<WireIssue, APIError>

    /// Rewrites the as_of staleness inputs in a body to "seconds ago", so
    /// a decoded payload is fresh against the current clock. Only the
    /// four section-level timestamps move; event/escalation times stay.
    private static func fresh(_ body: String) -> String {
        let now = ISO8601DateFormatter().string(
            from: Date(timeIntervalSinceNow: -5)
        )
        return body
            .replacingOccurrences(of: "2026-11-19T09:30:15Z", with: now)
            .replacingOccurrences(of: "2026-11-19T09:30:02Z", with: now)
            .replacingOccurrences(of: "2026-11-19T09:30:10Z", with: now)
            .replacingOccurrences(of: "2026-11-19T09:30:11Z", with: now)
    }

    static func ok() -> StubClient {
        let snapshot = try! decode(fresh(ContractBodies.snapshot), as: WireSnapshot.self)
        let queue = try! decode(fresh(ContractBodies.queue), as: WireQueue.self)
        let history = try! decode(fresh(ContractBodies.history), as: WireHistory.self)
        let issue = try! decode(ContractBodies.issue, as: WireIssue.self)
        return StubClient(
            snapshot: .success(snapshot),
            queue: .success(queue),
            history: .success(history),
            issue: .success(issue)
        )
    }

    static func failing(_ error: APIError) -> StubClient {
        StubClient(
            snapshot: .failure(error),
            queue: .failure(error),
            history: .failure(error),
            issue: .failure(error)
        )
    }

    func state() async throws -> WireSnapshot { try snapshot.get() }
    func queue() async throws -> WireQueue { try queue.get() }
    func history() async throws -> WireHistory { try history.get() }
    func issue(_ id: String) async throws -> WireIssue { try issue.get() }
}

/// A deterministic single-slot gate for one async value: `hold()`
/// suspends the fetch until the test calls `release(_:)`, and
/// `waitUntilHeld()` resolves exactly when a holder is suspended inside
/// the gate. MainActor-isolated on purpose — the poll loop hops here to
/// park, so interleavings are continuation-ordered, never polled or
/// slept.
@MainActor
final class FetchGate<Value: Sendable> {
    private var held: CheckedContinuation<Value, Never>?
    private var entered: CheckedContinuation<Void, Never>?

    init() {}

    /// Resolves when a caller is suspended in `hold()`.
    func waitUntilHeld() async {
        await withCheckedContinuation { (entered: CheckedContinuation<Void, Never>) in
            self.entered = entered
        }
    }

    /// Suspends the caller until `release` supplies a value.
    func hold() async -> Value {
        await withCheckedContinuation { (held: CheckedContinuation<Value, Never>) in
            self.held = held
            entered?.resume()
            entered = nil
        }
    }

    /// Releases the suspended caller with `value`; a no-op if nobody holds.
    func release(_ value: Value) {
        held?.resume(returning: value)
        held = nil
    }
}

/// A stub whose `state()` fetch parks in a `FetchGate` until the test
/// releases it; the remaining routes behave like `StubClient.ok()`.
nonisolated struct GatedSnapshotClient: DaemonClient, Sendable {
    let gate: FetchGate<WireSnapshot>
    let rest: StubClient

    func state() async throws -> WireSnapshot { await gate.hold() }
    func queue() async throws -> WireQueue { try await rest.queue() }
    func history() async throws -> WireHistory { try await rest.history() }
    func issue(_ id: String) async throws -> WireIssue { try await rest.issue(id) }
}

@MainActor
@Suite("Connection lifecycle")
struct ConnectionModelTests {
    private func attachedModel() -> ConnectionModel {
        let model = ConnectionModel()
        model.client = StubClient.ok()
        return model
    }

    @Test("Successful tick attaches and fills all feeds")
    func attach() async {
        let model = attachedModel()
        await model.tick()
        #expect(model.state == .attached)
        #expect(model.snapshotFeed.phase == .loaded)
        #expect(model.queueFeed.phase == .loaded)
        #expect(model.historyFeed.phase == .loaded)
        #expect(model.daemon?.pid == 4242)
        #expect(model.lastSync != nil)
    }

    @Test("Repeated transport failures disconnect; recovery reattaches and keeps last good data")
    func disconnectAndRecover() async {
        let model = attachedModel()
        await model.tick()
        let goodSnapshot = model.snapshotFeed.payload
        #expect(goodSnapshot != nil)

        model.client = StubClient.failing(.transport(.cannotConnectToHost))
        await model.tick()
        model.client = StubClient.failing(.transport(.cannotConnectToHost))
        await model.tick()
        #expect(model.state == .disconnected(reason: "Daemon not reachable"))
        // Last good data retained (R11), failure recorded — not swallowed.
        #expect(model.snapshotFeed.payload == goodSnapshot)
        #expect(model.snapshotFeed.phase == .failed)
        #expect(model.snapshotFeed.lastError == "Daemon not reachable")

        model.client = StubClient.ok()
        await model.tick()
        #expect(model.state == .attached)
        #expect(model.snapshotFeed.phase == .loaded)
    }

    @Test("Backoff escalates across failures and resets on success")
    func backoffThroughLifecycle() async {
        let model = attachedModel()
        model.client = StubClient.failing(.transport(.timedOut))
        let before = model.backoff.current
        await model.tick()
        let afterOne = model.backoff.current
        #expect(afterOne > before)
        await model.tick()
        #expect(model.backoff.current > afterOne)
        model.client = StubClient.ok()
        await model.tick()
        #expect(model.backoff.current == ConnectionModel.pollSeconds)
    }

    @Test("Daemon identity change forces re-resolution for reattach")
    func identityChangeReattaches() async {
        let model = attachedModel()
        await model.tick()
        #expect(model.client != nil)

        let restarted = try! decode(
            ContractBodies.snapshot.replacingOccurrences(
                of: "\"pid\": 4242", with: "\"pid\": 9999"
            ),
            as: WireSnapshot.self
        )
        model.client = StubClient(
            snapshot: .success(restarted),
            queue: .success(try! decode(ContractBodies.queue, as: WireQueue.self)),
            history: .success(try! decode(ContractBodies.history, as: WireHistory.self)),
            issue: .failure(.status(code: 404))
        )
        await model.tick()
        // The new identity dropped the old client; the next tick resolves
        // from fresh descriptors (new addr and token after a launchd
        // restart).
        #expect(model.client == nil)
        #expect(model.daemon?.pid == 9999)
    }

    @Test("Stale tracker sections degrade; fresh sections stay attached")
    func staleness() throws {
        let fresh = "2026-11-19T09:30:10Z"
        let stale = "2001-01-01T00:00:00Z"
        // "Now" sits seconds after the fresh fixture timestamps, so the
        // test is deterministic regardless of wall clock.
        let now = try #require(RFC3339.date("2026-11-19T09:31:00Z"))
        let snap = try decode(ContractBodies.snapshot, as: WireSnapshot.self)
        let queue = try decode(ContractBodies.queue, as: WireQueue.self)
        let history = try decode(ContractBodies.history, as: WireHistory.self)

        let allFresh = ConnectionModel.degradedState(
            snapshot: snap, queue: queue, history: history, now: now
        )
        #expect(allFresh == .attached)

        let queueStale = try decode(
            ContractBodies.queue.replacingOccurrences(of: fresh, with: stale),
            as: WireQueue.self
        )
        if case .degraded(let sections) = ConnectionModel.degradedState(
            snapshot: snap, queue: queueStale, history: history, now: now
        ) {
            #expect(sections == ["Queue"])
        } else {
            Issue.record("expected degraded, got \(allFresh)")
        }

        // Missing as_of (before the daemon's first read) is not degraded.
        let unread = try decode(
            ContractBodies.queue.replacingOccurrences(
                of: "\"as_of\":\"\(fresh)\",", with: ""
            ),
            as: WireQueue.self
        )
        #expect(
            ConnectionModel.degradedState(
                snapshot: snap, queue: unread, history: history, now: now
            ) == .attached
        )
    }

    @Test("Demo mode applies sample data without a client and stays honest")
    func demoMode() async {
        let model = ConnectionModel()
        model.setMode(.demo)
        await model.tick()
        #expect(model.mode == .demo)
        #expect(model.client == nil)
        #expect(model.state == .attached)
        #expect(model.snapshotFeed.payload?.daemon.version == "demo")
        #expect(model.queueFeed.hasPayload)
        // Demo has no live detail — the seed facts carry the column.
        #expect(model.detailFetch(id: "demo-hk4") == nil)
    }

    @Test("Demo content is connection-active — no disconnected overlay over sample data")
    func demoIsActive() async {
        let model = ConnectionModel()
        model.setMode(.demo)
        await model.tick()
        #expect(model.isConnectionActive)
        let facts = SectionFacts(
            connectionActive: model.isConnectionActive,
            connectionLoading: model.isConnectionLoading,
            feedPhase: model.snapshotFeed.phase,
            hasPayload: model.snapshotFeed.hasPayload
        )
        #expect(PresentationResolver.resolve(facts) == .content)
    }

    @Test("A tick suspended mid-flight never writes into a newly chosen target; the new target polls at release")
    func staleTickAbandonedOnTargetSwitch() async throws {
        // A controlled store: the record's live directory is empty, so
        // the follow-up tick deterministically resolves to no descriptor
        // on every machine.
        let defaults = try #require(UserDefaults(suiteName: UUID().uuidString))
        let store = InstanceStore(defaults: defaults)
        let model = ConnectionModel(instanceStore: store)
        store.add(InstanceRecord(name: "smoke", stateDirectory: "/nonexistent-lambe-smoke"))

        let gate = FetchGate<WireSnapshot>()
        model.client = GatedSnapshotClient(gate: gate, rest: StubClient.ok())
        let tickTask = Task { await model.tick() }
        // Deterministic: the tick is parked inside its first fetch.
        await gate.waitUntilHeld()

        // The operator switches target while the tick is suspended.
        model.connectionTargetChanged()
        #expect(model.snapshotFeed.payload == nil)

        gate.release(try decode(ContractBodies.snapshot, as: WireSnapshot.self))
        await tickTask.value

        // The stale payloads were dropped whole — nothing from the old
        // target landed anywhere.
        #expect(model.snapshotFeed.payload == nil)
        #expect(model.queueFeed.payload == nil)
        #expect(model.historyFeed.payload == nil)
        #expect(model.daemon == nil)
        // The new target was scheduled promptly at release, not left for
        // the next cadence beat: it already resolved (to "no descriptor"
        // — the controlled directory is empty).
        #expect(model.state == .neverConnected(reason: .noDescriptor))
        #expect(model.instanceName == "smoke")
    }

    @Test("A tick suspended mid-flight cannot overwrite demo sample data")
    func staleTickAbandonedOnDemoSwitch() async throws {
        let model = ConnectionModel()
        let gate = FetchGate<WireSnapshot>()
        model.client = GatedSnapshotClient(gate: gate, rest: StubClient.ok())
        let tickTask = Task { await model.tick() }
        await gate.waitUntilHeld()

        // Demo lands while the live tick is suspended.
        model.setMode(.demo)
        #expect(model.snapshotFeed.payload == SampleData.snapshot)

        gate.release(try decode(ContractBodies.snapshot, as: WireSnapshot.self))
        await tickTask.value

        // The sample data is intact; no stale live payload, identity, or
        // live follow-up leaked through.
        #expect(model.mode == .demo)
        #expect(model.snapshotFeed.payload == SampleData.snapshot)
        #expect(model.queueFeed.payload == SampleData.queue)
        #expect(model.historyFeed.payload == SampleData.history)
        #expect(model.state == .attached)
        #expect(model.daemon?.version == "demo")
        #expect(model.client == nil)
    }
}

// MARK: - Feed skip

@Suite("Feed model")
struct FeedModelTests {
    private struct Payload: Equatable { var items: [String] }

    @Test("Identical consecutive payloads skip invalidation")
    func identicalSkip() {
        let feed = FeedModel<Payload>()
        let first = feed.apply(Payload(items: ["a"]), isEmpty: { $0.items.isEmpty })
        #expect(first)
        let second = feed.apply(Payload(items: ["a"]), isEmpty: { $0.items.isEmpty })
        #expect(!second)
        #expect(feed.phase == .loaded)
    }

    @Test("Failure keeps the last good payload and records the reason")
    func failureRetains() {
        let feed = FeedModel<Payload>()
        _ = feed.apply(Payload(items: ["a"]), isEmpty: { _ in false })
        feed.fail("Timed out")
        #expect(feed.phase == .failed)
        #expect(feed.payload?.items == ["a"])
        #expect(feed.lastError == "Timed out")
        // Recovery clears the error.
        _ = feed.apply(Payload(items: ["a", "b"]), isEmpty: { _ in false })
        #expect(feed.lastError == nil)
        #expect(feed.phase == .loaded)
    }

    @Test("Empty payload reports the empty phase")
    func emptyPhase() {
        let feed = FeedModel<Payload>()
        _ = feed.apply(Payload(items: []), isEmpty: { $0.items.isEmpty })
        #expect(feed.phase == .empty)
        #expect(feed.hasPayload)
    }
}

// MARK: - Backoff

@Suite("Backoff policy")
struct BackoffPolicyTests {
    @Test("Escalates exponentially and caps near 15 s")
    func escalation() {
        var backoff = BackoffPolicy(base: 2, cap: 15)
        #expect(backoff.current == 2)
        backoff.escalate()
        #expect(backoff.current == 4)
        backoff.escalate()
        #expect(backoff.current == 8)
        backoff.escalate()
        #expect(backoff.current == 15)
        backoff.escalate()
        #expect(backoff.current == 15, "cap holds")
    }

    @Test("Reset returns to the attached cadence")
    func reset() {
        var backoff = BackoffPolicy(base: 2, cap: 15)
        backoff.escalate()
        backoff.escalate()
        backoff.reset()
        #expect(backoff.current == 2)
    }
}

// MARK: - Presentation table

@Suite("Phase-to-content mapping")
struct PresentationResolverTests {
    private func resolve(
        _ active: Bool, _ loading: Bool, _ phase: FeedPhase, _ hasPayload: Bool
    ) -> SectionPresentation {
        PresentationResolver.resolve(
            SectionFacts(
                connectionActive: active,
                connectionLoading: loading,
                feedPhase: phase,
                hasPayload: hasPayload
            )
        )
    }

    @Test("Mapping table: attached, degraded, disconnected, empty, loading")
    func table() {
        // Connecting with nothing yet → loading placeholder.
        #expect(resolve(false, true, .idle, false) == .loading)
        // Connecting but older data already on screen → retained, banner.
        #expect(resolve(false, true, .loaded, true) == .disconnected(hasPayload: true))
        // Disconnected keeps last good data visible (dimmed).
        #expect(resolve(false, false, .loaded, true) == .disconnected(hasPayload: true))
        // Disconnected with nothing ever loaded → placeholder.
        #expect(resolve(false, false, .idle, false) == .disconnected(hasPayload: false))
        // Attached: content, emptiness, loading, and refreshing-with-data.
        #expect(resolve(true, false, .loaded, false) == .content)
        #expect(resolve(true, false, .empty, true) == .empty)
        #expect(resolve(true, false, .loading, false) == .loading)
        #expect(resolve(true, false, .idle, true) == .content)
        // Per-section fetch failure while otherwise attached.
        #expect(resolve(true, false, .failed, true) == .sectionUnavailable)
        #expect(resolve(true, false, .failed, false) == .sectionUnavailable)
    }

    @Test("Attention empty copy carries the read-only posture (R13)")
    func postureCopy() {
        let copy = PresentationResolver.emptyCopy(for: .attention)
        #expect(copy.detail.contains("Read-only"))
        #expect(copy.detail.contains("bd"))
    }
}

// MARK: - Selection

@Suite("Selection by issue ID")
struct SeedResolverTests {
    @Test("Seed resolves across payload kinds and survives reordering")
    func survivesReorder() throws {
        let snapshot = try decode(ContractBodies.snapshot, as: WireSnapshot.self)
        var queue = try decode(ContractBodies.queue, as: WireQueue.self)
        let history = try decode(ContractBodies.history, as: WireHistory.self)

        // Snapshot hit first.
        let fromRuns = SeedResolver.resolve(
            issue: "lambe-xq2", snapshot: snapshot, queue: queue, history: history
        )
        #expect(fromRuns?.phase == "gating")

        // Queue hit, then reorder the rows — same ID still resolves the
        // same row (selection is identity, not index).
        let before = SeedResolver.resolve(
            issue: "lambe-jq9", snapshot: nil, queue: queue, history: nil
        )
        let reordered = try decode(ContractBodies.queueReordered, as: WireQueue.self)
        let after = SeedResolver.resolve(
            issue: "lambe-jq9", snapshot: nil, queue: reordered, history: nil
        )
        #expect(before == after)
        #expect(after?.blocked == true)
        #expect(after?.title == "Extract the gate parser")

        // History fallback for an issue the daemon no longer supervises.
        let archived = SeedResolver.resolve(
            issue: "lambe-ce1", snapshot: nil, queue: nil, history: history
        )
        #expect(archived?.phase == "interrupted")
    }

    @Test("An unknown ID still seeds its own detail fetch")
    func unknownID() {
        let seed = SeedResolver.resolve(
            issue: "lambe-zz", snapshot: nil, queue: nil, history: nil
        )
        #expect(seed?.issue == "lambe-zz")
        #expect(seed?.title == nil)
    }

    @Test("Attention reason and newest gate rejection attach to the seed (R3)")
    func attentionAndGateFacts() throws {
        let snapshot = try decode(ContractBodies.snapshot, as: WireSnapshot.self)
        // lambe-kd8 appears only in the snapshot's needs-human and gate
        // rejection sections — no live run record — so the seed is the
        // ID plus the attached facts, and the detail column can render
        // them before any run exists.
        let seed = SeedResolver.resolve(
            issue: "lambe-kd8", snapshot: snapshot, queue: nil, history: nil
        )
        #expect(seed?.attentionReason == "gate rejected: checks red")
        #expect(seed?.attentionDetail == "gate failed twice")
        #expect(seed?.attentionSince == "2026-11-19T09:02:00Z")
        #expect(seed?.gate == "gh:pr")
        #expect(seed?.gateReason == "2 checks failing")
        #expect(seed?.gateAt == "2026-11-19T09:02:00Z")
    }

    @Test("A queue row's seed carries its blocked reason")
    func queueRowReason() throws {
        let queue = try decode(ContractBodies.queue, as: WireQueue.self)
        let seed = SeedResolver.resolve(
            issue: "lambe-jq9", snapshot: nil, queue: queue, history: nil
        )
        #expect(seed?.blocked == true)
        #expect(seed?.queueReason == "blocked by 2 issues")
        #expect(seed?.attentionReason == nil)
    }
}

// MARK: - Queue composition

/// The live-smoke regressions for what the Queue renders and what the
/// sidebar badge counts: both read `QueueComposition`, so the assertions
/// here pin the exact rows the section list shows.
@Suite("Queue composition")
struct QueueCompositionTests {
    /// The daemon's actual state vocabulary (`internal/mcp` RunState
    /// names): pending, running, deferred stay; settled, failed, canceled
    /// are terminal; one unknown state for additive evolution.
    private static let snapshotFixture = """
    {"schema_version":1,"daemon":{"pid":4242},"runs":[
      {"issue":"lambe-run","attempt":1,"state":"running","last_event_age":"2s"},
      {"issue":"lambe-pend","attempt":1,"state":"pending","last_event_age":"n/a"},
      {"issue":"lambe-wait","attempt":1,"state":"deferred","last_event_age":"4m"},
      {"issue":"lambe-done","attempt":1,"state":"settled","last_event_age":"9s"},
      {"issue":"lambe-fail","attempt":3,"state":"failed","last_event_age":"1m"},
      {"issue":"lambe-cancel","attempt":1,"state":"canceled","last_event_age":"2m"},
      {"issue":"lambe-add","attempt":1,"state":"brand-new-state","last_event_age":"1s"}
    ],"needs_human":[],"gate_rejections":[]}
    """

    /// Queue rows for: the running issue (would duplicate the active
    /// run), the settled issue (re-queued after archiving), the unknown
    /// state's issue, and one never-dispatched issue.
    private static let queueFixture = """
    {"schema_version":1,"daemon":{"pid":4242},"as_of":"2026-11-19T09:30:10Z","rows":[
      {"id":"lambe-run","title":"Running now","reason":"ready: no open blockers"},
      {"id":"lambe-done","title":"Re-queued after settling","reason":"ready: no open blockers"},
      {"id":"lambe-add","title":"Fresh issue","reason":"ready: no open blockers"},
      {"id":"lambe-only","title":"Never dispatched","reason":"blocked by 1 issue","blocked":true}
    ]}
    """

    @Test("Terminal snapshot rows stay out of Active runs; the server's states decide")
    func terminalRowsExcluded() throws {
        let snapshot = try decode(Self.snapshotFixture, as: WireSnapshot.self)
        let active = QueueComposition.activeRuns(in: snapshot).map(\.issue)
        #expect(active == ["lambe-run", "lambe-pend", "lambe-wait", "lambe-add"])
    }

    @Test("Readiness dedupes against active runs only — a settled run's re-queued issue still renders")
    func readinessDedupesActiveOnly() throws {
        let snapshot = try decode(Self.snapshotFixture, as: WireSnapshot.self)
        let queue = try decode(Self.queueFixture, as: WireQueue.self)
        let rows = QueueComposition.readinessRows(queue: queue, snapshot: snapshot)
            .map(\.id)
        // lambe-run and lambe-add are suppressed (active runs render
        // them — the unknown state stays active); lambe-done renders —
        // its only snapshot row is settled, History's.
        #expect(rows == ["lambe-done", "lambe-only"])
    }

    @Test("Sidebar count equals the rendered rows: deduped, terminal-free")
    func renderedCount() throws {
        let snapshot = try decode(Self.snapshotFixture, as: WireSnapshot.self)
        let queue = try decode(Self.queueFixture, as: WireQueue.self)
        let count = QueueComposition.renderedRowCount(snapshot: snapshot, queue: queue)
        // 4 active runs + 2 readiness rows — not the raw payload sum of 11.
        #expect(count == 6)
        #expect(count == QueueComposition.activeRuns(in: snapshot).count
            + QueueComposition.readinessRows(queue: queue, snapshot: snapshot).count)
    }

    @Test("Demo payloads: issues in both payloads count once, badge matches the list")
    func demoBadge() {
        // Sample snapshot runs demo-hk4 + demo-pm7; the queue also lists
        // both plus demo-fs2 and demo-jq9. Rendered: 2 runs + 2 readiness.
        let count = QueueComposition.renderedRowCount(
            snapshot: SampleData.snapshot, queue: SampleData.queue
        )
        #expect(count == 4)
        #expect(count != SampleData.queue.rows.count + SampleData.snapshot.runs.count)
    }

    @Test("No payloads render zero rows")
    func emptyCount() {
        #expect(QueueComposition.renderedRowCount(snapshot: nil, queue: nil) == 0)
    }
}

// MARK: - Detail

@MainActor
@Suite("Detail model")
struct DetailModelTests {
    @Test("Demo (no fetch) degrades to seed facts honestly")
    func seedOnly() async {
        let detail = DetailModel()
        await detail.load(id: "demo-hk4", fetch: nil)
        #expect(detail.phase == .seedOnly)
        #expect(detail.issue == nil)
    }

    @Test("A 404 detail fetch renders seed facts as normal — no failure posture")
    func notFoundIsSeedOnly() async {
        let detail = DetailModel()
        await detail.load(id: "lambe-kd8") {
            throw APIError.status(code: 404)
        }
        // An attention item before (or without) a run record: the seed
        // facts carry the column, the same as an archived history row.
        #expect(detail.phase == .seedOnly)
        #expect(detail.issue == nil)
    }

    @Test("Every other error keeps the failure posture")
    func otherErrorsStayFailed() async {
        let detail = DetailModel()
        await detail.load(id: "lambe-xq2") {
            throw APIError.status(code: 503)
        }
        #expect(detail.phase == .failed("Daemon error (HTTP 503)"))
        await detail.load(id: "lambe-xq2") {
            throw APIError.transport(.timedOut)
        }
        #expect(detail.phase == .failed("Timed out"))
        #expect(detail.issue == nil)
    }

    @Test("A successful fetch loads the typed detail")
    func loadedFetch() async throws {
        let detail = DetailModel()
        let issue = try decode(ContractBodies.issue, as: WireIssue.self)
        await detail.load(id: "lambe-xq2") { issue }
        #expect(detail.phase == .loaded)
        #expect(detail.issue?.turn == 12)
        #expect(detail.loadedID == "lambe-xq2")
    }

    @Test("A re-selected id supersedes a suspended fetch of the same id")
    func refetchSameIDSupersedes() async throws {
        let detail = DetailModel()
        let fresh = try decode(ContractBodies.issue, as: WireIssue.self)
        let stale = try decode(
            ContractBodies.issue.replacingOccurrences(of: "\"turn\": 12", with: "\"turn\": 99"),
            as: WireIssue.self
        )
        let gate = FetchGate<WireIssue>()
        let first = Task {
            await detail.load(id: "lambe-xq2") { try await gate.hold() }
        }
        // Deterministic: the first load is parked inside its fetch.
        await gate.waitUntilHeld()
        // Same id, newer load — as when the operator re-selects a row
        // after a target or mode switch reset the detail mid-fetch.
        let second = Task {
            await detail.load(id: "lambe-xq2") { fresh }
        }
        await second.value
        #expect(detail.issue?.turn == 12)

        gate.release(stale)
        await first.value
        // The suspended fetch lost to the newer load; the stale body —
        // from whichever target it was fetched — never surfaced.
        #expect(detail.phase == .loaded)
        #expect(detail.issue?.turn == 12)
        #expect(detail.loadedID == "lambe-xq2")
    }
}
