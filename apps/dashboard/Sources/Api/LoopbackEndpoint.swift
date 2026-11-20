//
//  LoopbackEndpoint.swift
//  LambeDash
//
//  The validated `(host, port)` pair every request URL is built from
//  (KTD5, R15). Validation is literal: the host must equal the string
//  `127.0.0.1` — no hostnames, no IPv6, no alternate IPv4 spellings — and
//  the port must be ASCII digits in the valid TCP range. Because the type
//  is the only way to obtain a request URL, the bearer token can never be
//  attached to a request bound anywhere but the daemon's loopback address.
//

import Foundation

/// A loopback endpoint parsed from a descriptor's `addr` field, or nil
/// when the address fails the literal `127.0.0.1` + valid-port check.
nonisolated struct LoopbackEndpoint: Hashable, Sendable {
    /// The only host this client ever sends the bearer token to.
    static let loopbackHost = "127.0.0.1"

    /// Valid TCP port range for a client target. Port 0 is excluded: it is
    /// what a daemon binds to *select* an ephemeral port, never what it
    /// advertises after binding.
    static let portRange = 1...65535

    /// Always `127.0.0.1`.
    let host: String
    /// The validated port, 1...65535.
    let port: Int
    /// `http://127.0.0.1:<port>` — the base every route URL derives from.
    let baseURL: URL

    /// Parses `"host:port"` literally. Anything that is not exactly
    /// `127.0.0.1` followed by `:` and ASCII digits in the valid range is
    /// refused, including decimal/octal/hex IPv4 spellings, hostnames,
    /// IPv6 forms, whitespace, and signed or padded ports. A leading-zero
    /// port (`08080`) is accepted as 8080, matching Go's `strconv.Atoi`
    /// semantics on the writer side.
    init?(addr: String) {
        guard let colon = addr.lastIndex(of: ":") else { return nil }
        let host = String(addr[..<colon])
        let portText = String(addr[addr.index(after: colon)...])
        guard host == Self.loopbackHost else { return nil }
        guard !portText.isEmpty,
              portText.allSatisfy({ ("0"..."9").contains($0) }),
              let port = Int(portText),
              Self.portRange.contains(port)
        else { return nil }
        var components = URLComponents()
        components.scheme = "http"
        components.host = Self.loopbackHost
        components.port = port
        guard let baseURL = components.url else { return nil }
        self.host = Self.loopbackHost
        self.port = port
        self.baseURL = baseURL
    }

    /// The request URL for a versioned route path such as
    /// `/api/v1/state`, or nil for anything outside `/api/v1/` — the
    /// client only ever talks to the documented route table. Query strings
    /// cannot be smuggled in: the path component percent-encodes `?`.
    func routeURL(_ path: String) -> URL? {
        guard path.hasPrefix("/api/v1/"), path.count > "/api/v1/".count else { return nil }
        var components = URLComponents(url: baseURL, resolvingAgainstBaseURL: false)
        components?.path = path
        return components?.url
    }
}
