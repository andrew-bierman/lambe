package httpstate

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
)

// BearerAuth returns middleware guarding every state-surface route. It
// rejects requests whose Host does not name the bound loopback host, then
// requires the bearer token, comparing it in constant time (crypto/subtle).
// Missing and wrong tokens get the same 401 ErrorResponse envelope on every
// route — including the refresh POST — so there is no unauthenticated
// bypass and no route-specific exception; missing-token and wrong-token
// responses are indistinguishable. No CORS headers are ever emitted; the
// surface serves same-origin loopback clients only. The daemon wraps its
// server's handler with this middleware once at startup, leaving the route
// handlers and DTOs untouched behind it.
func BearerAuth(token, host string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !hostMatches(r.Host, host) {
				writeAuthError(w, http.StatusForbidden, "bad_host",
					"request host does not match the bound loopback address")
				return
			}
			// One comparison path for every request shape: an empty
			// presented token compares just like a wrong one, and a never
			// configured token authorizes nothing.
			presented := bearerToken(r.Header.Get("Authorization"))
			if token == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="lambe-stateapi"`)
				writeAuthError(w, http.StatusUnauthorized, "unauthorized",
					"missing or invalid bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the credentials from a Bearer Authorization header.
// Anything else — a missing header, another scheme, empty credentials —
// yields the empty string, so every malformed shape is rejected through
// the same comparison as a wrong token.
func bearerToken(header string) string {
	scheme, credentials, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(credentials)
}

// hostMatches reports whether a request's Host header names the bound
// host, ignoring case and the port the client used. A missing Host header
// never matches.
func hostMatches(headerHost, boundHost string) bool {
	if headerHost == "" {
		return false
	}
	host := headerHost
	if h, _, err := net.SplitHostPort(headerHost); err == nil {
		host = h
	}
	return strings.EqualFold(host, boundHost)
}

// writeAuthError answers a rejected request with the package's
// ErrorResponse DTO — the same envelope every route uses — carrying a
// static message that never quotes the presented credentials.
func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{
		SchemaVersion: SchemaVersion,
		Error:         code,
		Message:       message,
	})
}
