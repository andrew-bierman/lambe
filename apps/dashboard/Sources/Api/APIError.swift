//
//  APIError.swift
//  LambeDash
//
//  Typed failures for the loopback daemon state transport (U7 transport
//  subset). Invariant (R15): no case ever carries the bearer token, and
//  no case reflects untrusted text — not the descriptor's address field,
//  not a caller-supplied route path, not response bytes, and not a URL
//  loading system message (which can quote the failed request). Every
//  payload is either fixed text written in this file or a bare code (an
//  Int status/schema number or a URLError code), so printing or
//  reflecting an error of this type can never leak the token or echo
//  attacker-chosen input.
//

import Foundation

/// Failure taxonomy for the loopback state transport. Payload discipline:
/// static or code-only by construction (see the header comment).
nonisolated enum APIError: Error, Equatable, Sendable {
    /// Admission refused the descriptor's address: it is not literally
    /// `127.0.0.1` with a valid port. The offending address is not
    /// reflected — it is attacker-controlled descriptor text.
    case invalidEndpoint
    /// The request path is outside the versioned `/api/v1/` route table.
    /// The rejected path is not reflected — it is caller-supplied text.
    case invalidPath
    /// The descriptor decoded but fails structural admission (missing or
    /// whitespace-bearing token, non-positive pid). The reason is one of
    /// this file's fixed strings.
    case descriptorInvalid(reason: String)
    /// The descriptor document follows a different descriptor schema
    /// version than this client understands; carries the found number.
    case unsupportedDescriptorSchema(found: Int)
    /// The daemon answered a GET with a redirect-class (3xx) status.
    /// Redirects are never followed (R15) — the session delegate refuses
    /// the follow before a second request exists, and the un-followed 3xx
    /// answer is refused here — because following one would replay the
    /// loopback authorization header to the redirect target.
    case redirectRefused
    /// The transport failed before an HTTP response existed (connection
    /// refused, timeout, cancellation). Carries only the URL loading
    /// system's machine code (`.timedOut`, `.cannotConnectToHost`,
    /// `.cancelled`, …, `.unknown` when the failure is not a URLError) —
    /// never its user-facing message.
    case transport(URLError.Code)
    /// The response was not an HTTP response.
    case unexpectedResponse
    /// The daemon answered with a non-2xx status. The route and the
    /// response body are deliberately excluded: the path is caller text,
    /// and a hostile loopback server could reflect the Authorization
    /// header back inside its body.
    case status(code: Int)
    /// The 2xx body did not decode into the expected type.
    case decoding
}
