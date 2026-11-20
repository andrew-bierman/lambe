//
//  StateAPIDescriptorTests.swift
//  LambeDashTests
//
//  Decode fidelity against the committed U1 field names, and strict
//  admission over the malicious-descriptor families: wrong schema, dead
//  pid, missing or header-unsafe token, and every non-literal-loopback
//  address shape. Admission must also be separable from decoding: a
//  future schema version decodes fine and is then refused, with a typed
//  reason, before any client exists.
//

import Testing
import Foundation

@testable import LambeDash

@Suite struct StateAPIDescriptorTests {
    @Test("the canonical U1 descriptor decodes field-for-field")
    func decodesCanonicalDescriptor() throws {
        let data = try #require(DescriptorDocuments.canonical.data(using: .utf8))
        let descriptor = try StateAPIDescriptor.decode(data)
        #expect(descriptor.schemaVersion == 1)
        #expect(descriptor.instance == "default")
        #expect(descriptor.addr == "127.0.0.1:53412")
        #expect(descriptor.bearerToken == "tok")
        #expect(descriptor.pid == 4242)
        #expect(descriptor.identity == "darwin:boot:1:2")
        #expect(descriptor.version == "test")
        #expect(descriptor.configPath == "/cfg/lambe.yaml")
        #expect(descriptor.startedAt == "2026-11-19T09:30:00Z")
        let endpoint = try descriptor.validatedEndpoint()
        #expect(endpoint == LoopbackEndpoint(addr: "127.0.0.1:53412"))
    }

    @Test("omitted omitempty fields decode as absent, not missing")
    func decodesDescriptorWithoutOptionalFields() throws {
        let data = try #require(DescriptorDocuments.canonicalWithoutOptionals.data(using: .utf8))
        let descriptor = try StateAPIDescriptor.decode(data)
        #expect(descriptor.identity == nil)
        #expect(descriptor.configPath == nil)
        #expect(descriptor.instance == "solo")
        #expect(try descriptor.validatedEndpoint().port == 53413)
    }

    @Test("unknown keys are ignored, additively")
    func ignoresUnknownKeys() throws {
        let json = DescriptorDocuments.json(
            extraKey: #""gateway":"10.0.0.1","renamed_since":"yesterday""#
        )
        let data = try #require(json.data(using: .utf8))
        let descriptor = try StateAPIDescriptor.decode(data)
        #expect(descriptor.instance == "default")
        #expect(descriptor.bearerToken == "tok")
    }

    /// Wrong JSON types, missing required keys, and non-JSON payloads all
    /// fail decoding — the store then skips the file silently.
    @Test("wrong JSON types and missing required keys fail decoding", arguments: [
        DescriptorDocuments.json(pid: #""4242""#),
        DescriptorDocuments.json(bearerToken: "tok").replacingOccurrences(
            of: #""bearer_token":"tok","#,
            with: ""
        ),
        DescriptorDocuments.json().replacingOccurrences(
            of: ",\"started_at\":\"2026-11-19T09:30:00Z\"",
            with: ""
        ),
        "{",
        "not json at all",
    ])
    func rejectsMalformedDocuments(document: String) throws {
        let data = try #require(document.data(using: .utf8))
        do {
            _ = try StateAPIDescriptor.decode(data)
            Issue.record("expected decoding to fail for \(document)")
        } catch {
            // Expected: the document is not a decodable descriptor.
        }
    }

    @Test("a future descriptor schema decodes but is refused with a typed reason")
    func refusesUnknownDescriptorSchema() throws {
        let data = try #require(
            DescriptorDocuments.json(schemaVersion: 2).data(using: .utf8)
        )
        let descriptor = try StateAPIDescriptor.decode(data)
        #expect(throws: APIError.unsupportedDescriptorSchema(found: 2)) {
            try descriptor.validatedEndpoint()
        }
    }

    @Test("admission refuses structurally impossible descriptors")
    func refusesStructurallyImpossibleDescriptors() throws {
        // A dead or placeholder pid: the daemon never writes pid <= 0.
        let zeroPid = try StateAPIDescriptor.decode(
            #require(DescriptorDocuments.json(pid: "0").data(using: .utf8))
        )
        #expect(throws: APIError.descriptorInvalid(reason: "pid must be positive")) {
            try zeroPid.validatedEndpoint()
        }

        // An empty token can never authenticate.
        let emptyToken = try StateAPIDescriptor.decode(
            #require(DescriptorDocuments.json(bearerToken: "").data(using: .utf8))
        )
        #expect(throws: APIError.descriptorInvalid(reason: "bearer token missing")) {
            try emptyToken.validatedEndpoint()
        }

        // Whitespace in the token is a header-injection attempt (a CR/LF
        // would forge or split the Authorization header on the wire). The
        // escapes are JSON-level, so the decoded value really carries the
        // control characters while the document stays valid JSON.
        let injectionToken = try StateAPIDescriptor.decode(
            #require(
                DescriptorDocuments.json(bearerToken: "\\r\\nX-Evil: 1").data(using: .utf8)
            )
        )
        #expect(throws: APIError.descriptorInvalid(reason: "bearer token contains whitespace")) {
            try injectionToken.validatedEndpoint()
        }
    }

    /// Address refusal is also enforced at the descriptor layer; the
    /// scheme-prefixed forms are new here, the rest guard against the
    /// endpoint gate ever loosening.
    @Test("admission refuses every address that is not literal loopback", arguments: StateAPIDescriptorTests.hostileAddresses)
    func refusesNonLoopbackAddresses(addr: String) throws {
        let descriptor = try StateAPIDescriptor.decode(
            #require(DescriptorDocuments.json(addr: addr).data(using: .utf8))
        )
        #expect(throws: APIError.invalidEndpoint) {
            try descriptor.validatedEndpoint()
        }
    }

    private nonisolated static let hostileAddresses: [String] = [
        "http://127.0.0.1:53412",
        "https://127.0.0.1:53412",
        "10.0.0.9:8080",
        "0.0.0.0:8080",
        "localhost:8080",
        "[::1]:8080",
        "127.0.0.1.evil.example:8080",
        "127.0.0.1:0",
        "127.0.0.1:notaport",
    ]

    @Test("admission error payloads never carry the token or echo hostile input")
    func admissionErrorsNeverCarryTheToken() throws {
        let secret = "S3CRET-TOKEN-abcdef"
        let documents: [(String, String)] = [
            ("schema", DescriptorDocuments.json(schemaVersion: 7, bearerToken: secret)),
            ("pid", DescriptorDocuments.json(bearerToken: secret, pid: "-3")),
            ("addr", DescriptorDocuments.json(addr: "10.9.9.9:1", bearerToken: secret)),
        ]
        for (label, document) in documents {
            let descriptor = try StateAPIDescriptor.decode(
                #require(document.data(using: .utf8))
            )
            do {
                _ = try descriptor.validatedEndpoint()
                Issue.record("\(label): expected admission to fail")
            } catch {
                #expect(String(describing: error).contains(secret) == false, "\(label)")
                #expect(String(reflecting: error).contains(secret) == false, "\(label)")
                // The descriptor's address is attacker-controlled text:
                // the error must not echo it either.
                if label == "addr" {
                    #expect(String(describing: error).contains("10.9.9.9") == false, "addr")
                    #expect(String(reflecting: error).contains("10.9.9.9") == false, "addr")
                }
            }
        }
    }
}
