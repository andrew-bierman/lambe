//
//  DescriptorStoreTests.swift
//  LambeDashTests
//
//  Read-only enumeration over real temp directories (no network, no
//  daemon): admits canonical U1 documents, skips every malformed,
//  hostile, or oversized entry, stays silent on empty/missing directories,
//  and provably writes and deletes nothing.
//

import Testing
import Foundation

@testable import LambeDash

@Suite struct DescriptorStoreTests {
    /// Isolated live directory per test.
    private func makeLiveDirectory() throws -> URL {
        let directory = FileManager.default.temporaryDirectory
            .appending(component: "lambedash-store-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        return directory
    }

    private func write(_ text: String, name: String, to directory: URL) throws {
        let url = directory.appending(component: name)
        try Data(text.utf8).write(to: url)
    }

    private func listing(_ directory: URL) -> [String] {
        let entries = (try? FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil)) ?? []
        return entries.map(\.lastPathComponent).sorted()
    }

    @Test("enumeration admits canonical descriptors with validated endpoints")
    func admitsCanonicalDescriptors() throws {
        let directory = try makeLiveDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        try write(DescriptorDocuments.canonical, name: "stateapi-default.json", to: directory)
        try write(DescriptorDocuments.canonicalWithoutOptionals, name: "stateapi-solo.json", to: directory)

        let store = DescriptorStore(liveDirectory: directory)
        let resolved = store.resolvedEndpoints()

        #expect(resolved.count == 2)
        #expect(resolved.map(\.instance) == ["default", "solo"]) // sorted, deterministic
        #expect(resolved[0].endpoint == LoopbackEndpoint(addr: "127.0.0.1:53412"))
        #expect(resolved[0].bearerToken == "tok")
        #expect(resolved[0].pid == 4242)
        #expect(resolved[0].identity == "darwin:boot:1:2")
        #expect(resolved[1].endpoint.port == 53413)
        #expect(resolved[1].identity == nil)
    }

    @Test("enumeration skips every non-descriptor or hostile file")
    func skipsHostileEntries() throws {
        let directory = try makeLiveDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }

        // Valid document under the wrong name.
        try write(DescriptorDocuments.canonical, name: "endpoints.json", to: directory)
        try write(DescriptorDocuments.canonical, name: "stateapi-backup.json.bak", to: directory)
        try write(DescriptorDocuments.canonical, name: "stateapi.json", to: directory)
        // Hidden descriptor: skipped together with other hidden files.
        try write(DescriptorDocuments.canonical, name: ".stateapi-hidden.json", to: directory)
        // Name matches, content does not.
        try write("not json at all", name: "stateapi-broken.json", to: directory)
        try write("", name: "stateapi-empty.json", to: directory)
        try write(DescriptorDocuments.json(schemaVersion: 2), name: "stateapi-future.json", to: directory)
        try write(DescriptorDocuments.json(pid: "0"), name: "stateapi-zero-pid.json", to: directory)
        try write(DescriptorDocuments.json(bearerToken: ""), name: "stateapi-no-token.json", to: directory)
        try write(DescriptorDocuments.json(addr: "10.0.0.9:8080"), name: "stateapi-offhost.json", to: directory)
        try write(DescriptorDocuments.json(addr: "localhost:8080"), name: "stateapi-namedhost.json", to: directory)
        // Oversized: bigger than the read cap, filled with valid-looking padding.
        let oversized = String(repeating: "x", count: DescriptorStore.maxDescriptorBytes + 1)
        try write(oversized, name: "stateapi-huge.json", to: directory)
        // Subdirectory and symlink wearing descriptor names.
        try FileManager.default.createDirectory(at: directory.appending(component: "stateapi-dir.json"), withIntermediateDirectories: true)
        try FileManager.default.createSymbolicLink(
            at: directory.appending(component: "stateapi-link.json"),
            withDestinationURL: directory.appending(component: "stateapi-offhost.json")
        )

        let store = DescriptorStore(liveDirectory: directory)
        #expect(store.resolvedEndpoints().isEmpty)
    }

    @Test("empty and missing directories yield an empty list, not an error")
    func toleratesMissingDirectories() throws {
        let directory = try makeLiveDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }

        #expect(DescriptorStore(liveDirectory: directory).resolvedEndpoints() == [])
        #expect(
            DescriptorStore(liveDirectory: directory.appending(component: "absent")).resolvedEndpoints() == []
        )
    }

    @Test("enumeration writes and deletes nothing")
    func isReadOnly() throws {
        let directory = try makeLiveDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        try write(DescriptorDocuments.canonical, name: "stateapi-default.json", to: directory)
        try write("junk", name: "stateapi-junk.json", to: directory)

        let before = listing(directory)
        let store = DescriptorStore(liveDirectory: directory)
        #expect(store.resolvedEndpoints().count == 1)
        #expect(store.resolvedEndpoints().count == 1) // repeated reads stay stable
        #expect(listing(directory) == before)

        // The admitted document is untouched on disk.
        let reRead = try Data(contentsOf: directory.appending(component: "stateapi-default.json"))
        #expect(String(decoding: reRead, as: UTF8.self) == DescriptorDocuments.canonical)
    }
}
