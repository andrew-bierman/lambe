//
//  InstanceStore.swift
//  LambeDash
//
//  The ordered list of daemon instances the operator maintains, persisted
//  locally (R14). A record names a state directory (plus an optional
//  instance-name filter), never a daemon process: the app resolves the
//  record to exactly one running daemon by enumerating that directory's
//  live descriptors. A dev-smoke instance is therefore never confused with
//  production — selection is explicit, and the app owns no daemon.
//

import Foundation

/// One configured daemon instance.
nonisolated struct InstanceRecord: Codable, Equatable, Identifiable, Sendable {
    /// Stable id for selection persistence.
    var id: UUID
    /// The operator's display label ("Production", "smoke-1").
    var name: String
    /// The daemon state directory whose `live/` holds the descriptors.
    var stateDirectory: String
    /// Optional exact instance-name filter. nil means the directory must
    /// resolve to exactly one live descriptor — ambiguity is surfaced,
    /// never silently resolved.
    var instanceName: String?

    init(
        id: UUID = UUID(),
        name: String,
        stateDirectory: String,
        instanceName: String? = nil
    ) {
        self.id = id
        self.name = name
        self.stateDirectory = stateDirectory
        self.instanceName = instanceName
    }
}

/// The daemon's default state directory — the same path Go resolves
/// (`DefaultStateDir`): `LAMBE_STATE_DIR` when set, else
/// `~/.local/state/lambe`. Read once per launch; a running daemon's env is
/// its own business.
nonisolated enum DefaultInstance {
    static func stateDirectory() -> String {
        if let dir = ProcessInfo.processInfo.environment["LAMBE_STATE_DIR"],
           !dir.isEmpty
        {
            return dir
        }
        return URL.homeDirectory.appending(path: ".local/state/lambe").path
    }

    static func record() -> InstanceRecord {
        InstanceRecord(name: "Default", stateDirectory: stateDirectory())
    }
}

/// Persists and exposes the instance records plus the selected one.
/// Backed by `UserDefaults` under one key; the default seed is the
/// daemon's default state directory so first launch resolves a local
/// production daemon without any setup.
@MainActor
@Observable
final class InstanceStore {
    private static let recordsKey = "lambe.dash.instanceRecords"
    private static let selectedKey = "lambe.dash.selectedInstance"

    private(set) var records: [InstanceRecord]
    /// The selected record id, or nil when the operator has not chosen.
    private(set) var selectedID: UUID?

    private let defaults: UserDefaults

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        if let data = defaults.data(forKey: Self.recordsKey),
           let decoded = try? JSONDecoder().decode([InstanceRecord].self, from: data),
           !decoded.isEmpty
        {
            records = decoded
        } else {
            records = [DefaultInstance.record()]
        }
        // Stored as a uuidString; re-read through the failable initializer
        // so a corrupted value simply falls back to the first record.
        if let stored = defaults.string(forKey: Self.selectedKey) {
            selectedID = UUID(uuidString: stored)
        } else {
            selectedID = nil
        }
        // A stored selection that no longer exists resolves to nothing;
        // fall back to the first record so the window is never dead.
        if selectedID == nil || !records.contains(where: { $0.id == selectedID }) {
            selectedID = records.first?.id
        }
    }

    /// The currently selected record, if any.
    var selected: InstanceRecord? {
        records.first { $0.id == selectedID }
    }

    /// Selects a record by id.
    func select(_ id: UUID) {
        guard records.contains(where: { $0.id == id }) else { return }
        selectedID = id
        defaults.set(id.uuidString, forKey: Self.selectedKey)
    }

    /// Adds a record and selects it.
    func add(_ record: InstanceRecord) {
        records.append(record)
        persist()
        select(record.id)
    }

    /// Removes a record; selection falls back to the first remaining.
    func remove(_ record: InstanceRecord) {
        records.removeAll { $0.id == record.id }
        persist()
        if selectedID == record.id {
            selectedID = records.first?.id
            if let id = selectedID {
                defaults.set(id.uuidString, forKey: Self.selectedKey)
            } else {
                defaults.removeObject(forKey: Self.selectedKey)
            }
        }
    }

    private func persist() {
        if let data = try? JSONEncoder().encode(records) {
            defaults.set(data, forKey: Self.recordsKey)
        }
    }
}
