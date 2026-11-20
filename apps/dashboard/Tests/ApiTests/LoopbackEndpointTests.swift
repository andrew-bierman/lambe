//
//  LoopbackEndpointTests.swift
//  LambeDashTests
//
//  The literal-127.0.0.1 + valid-port gate (R15) is the security boundary
//  the bearer token crosses, so both halves are table-driven over the
//  bypass attempts: hostnames, IPv6, alternate IPv4 spellings (decimal,
//  octal-style, hex), padding, whitespace, signed ports, out-of-range
//  ports, and malformed splits.
//

import Testing
import Foundation

@testable import LambeDash

@Suite struct LoopbackEndpointTests {
    @Test("canonical loopback addresses with valid ports parse")
    func parsesCanonicalLoopbackAddresses() throws {
        let endpoint = try #require(LoopbackEndpoint(addr: "127.0.0.1:53412"))
        #expect(endpoint.host == "127.0.0.1")
        #expect(endpoint.port == 53412)
        #expect(endpoint.baseURL.absoluteString == "http://127.0.0.1:53412")

        let boundaryLow = try #require(LoopbackEndpoint(addr: "127.0.0.1:1"))
        #expect(boundaryLow.port == 1)
        let boundaryHigh = try #require(LoopbackEndpoint(addr: "127.0.0.1:65535"))
        #expect(boundaryHigh.port == 65535)

        // Leading-zero ports are numeric under Go's strconv.Atoi semantics
        // on the writer side.
        let zeroPadded = try #require(LoopbackEndpoint(addr: "127.0.0.1:08080"))
        #expect(zeroPadded.port == 8080)
    }

    /// The table covers every bypass family: hostnames, IPv6, alternate
    /// IPv4 encodings, missing/multiple colons, whitespace, and signed,
    /// non-digit, or out-of-range ports.
    @Test("addresses that are not literally 127.0.0.1 with a valid port are refused", arguments: LoopbackEndpointTests.refusedAddresses)
    func refusesNonLiteralLoopbackAddresses(addr: String) {
        #expect(LoopbackEndpoint(addr: addr) == nil)
    }

    private nonisolated static let refusedAddresses: [String] = [
        // Hostnames resolve to the same machine — still refused: the check
        // is literal, and the token must only ever go to 127.0.0.1.
        "localhost:8080",
        "LOCALHOST:8080",
        "127.0.0.1.evil.example:8080",
        // IPv6 forms.
        "[::1]:8080",
        "::1",
        "[::1]:8080]",
        // Alternate IPv4 spellings that all mean 127.0.0.1 to inet_aton.
        "2130706433:8080",
        "0x7f000001:8080",
        "0177.0.0.1:8080",
        "127.1:8080",
        "127.0.0.01:8080",
        // Structural breakage.
        "",
        "127.0.0.1",
        ":8080",
        "127.0.0.1:",
        "127.0.0.1:8080:9",
        // Whitespace anywhere.
        " 127.0.0.1:8080",
        "127.0.0.1:8080 ",
        "127.0.0.1: 8080",
        "127.0.0.1:8080\n",
        // Ports that are not plain ASCII digits in 1...65535.
        "127.0.0.1:0",
        "127.0.0.1:65536",
        "127.0.0.1:99999999999999999999",
        "127.0.0.1:-1",
        "127.0.0.1:+8080",
        "127.0.0.1:8080x",
        "127.0.0.1:0x50",
        "127.0.0.1:٨٠٨٠",
    ]

    @Test("route URLs are versioned GET targets with no query injection")
    func routeURLsAreVersionedAndInjectionSafe() throws {
        let endpoint = try #require(LoopbackEndpoint(addr: "127.0.0.1:47391"))

        let state = try #require(endpoint.routeURL("/api/v1/state"))
        #expect(state.absoluteString == "http://127.0.0.1:47391/api/v1/state")
        #expect(state.scheme == "http")
        #expect(state.host == "127.0.0.1")
        #expect(state.port == 47391)
        #expect(state.query == nil)

        // A crafted path cannot smuggle a query or leave the route table.
        let smuggled = try #require(endpoint.routeURL("/api/v1/state?next=http://10.0.0.9"))
        #expect(smuggled.query == nil)
        #expect(endpoint.routeURL("/api/v1") == nil)
        #expect(endpoint.routeURL("/api/v1/") == nil)
        #expect(endpoint.routeURL("api/v1/state") == nil)
        #expect(endpoint.routeURL("/etc/passwd") == nil)
        #expect(endpoint.routeURL("") == nil)
    }
}
