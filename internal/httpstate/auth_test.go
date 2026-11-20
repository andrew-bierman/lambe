package httpstate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newAuthedServer starts a real Server with its mux wrapped in BearerAuth
// for the given token and bound host — the exact composition the daemon
// installs — so middleware tests drive the assembled handler, not a
// re-factored copy of it.
func newAuthedServer(t *testing.T, token, host string) (*fakeSource, string) {
	t.Helper()
	src := &fakeSource{
		snapshot: testSnapshot(),
		issue:    IssueState{State: "running", Attempt: 1},
		issueID:  "T-1",
		issueOK:  true,
	}
	srv := New(src, mustListen(t))
	srv.Wrap(BearerAuth(token, host))
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	go func() { _ = srv.Start() }()
	return src, "http://" + srv.Addr().String()
}

// authRequest performs one request with an optional bearer token and an
// optional Host override; empty values mean "absent".
func authRequest(t *testing.T, method, url, token, host string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if host != "" {
		req.Host = host
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeEnvelope(t *testing.T, resp *http.Response) ErrorResponse {
	t.Helper()
	var got ErrorResponse
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode envelope %q: %v", body, err)
	}
	return got
}

// TestBearerAuthGuardsEveryRoute pins the no-exception contract: without a
// valid token every route — the three §13.7 routes, the refresh POST, the
// not-yet-served queue/history reads, and unknown paths — answers the same
// 401 envelope before any route handler runs.
func TestBearerAuthGuardsEveryRoute(t *testing.T) {
	_, base := newAuthedServer(t, "correct-token", "127.0.0.1")
	routes := []struct{ method, path string }{
		{"GET", "/api/v1/state"},
		{"GET", "/api/v1/T-1"},
		{"POST", "/api/v1/refresh"},
		{"GET", "/api/v1/queue"},
		{"GET", "/api/v1/history"},
		{"GET", "/api/v1/not-a-route"},
		{"PUT", "/api/v1/state"},
	}
	for _, rt := range routes {
		for name, token := range map[string]string{"missing": "", "wrong": "not-the-token"} {
			resp := authRequest(t, rt.method, base+rt.path, token, "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s token: status %d, want 401", rt.method, rt.path, name, resp.StatusCode)
			}
			got := decodeEnvelope(t, resp)
			if got.SchemaVersion != SchemaVersion || got.Error != "unauthorized" || got.Message == "" {
				t.Errorf("%s %s with %s token: unexpected envelope %+v", rt.method, rt.path, name, got)
			}
		}
	}
}

// TestBearerAuthMissingAndWrongTokensAreIndistinguishable pins the uniform
// rejection shape: byte-identical bodies and matching headers for a missing
// and a wrong token, the code-reviewed substitute for a timing test.
func TestBearerAuthMissingAndWrongTokensAreIndistinguishable(t *testing.T) {
	_, base := newAuthedServer(t, "correct-token", "127.0.0.1")
	missing := authRequest(t, "GET", base+"/api/v1/state", "", "")
	wrong := authRequest(t, "GET", base+"/api/v1/state", "not-the-token", "")

	if missing.StatusCode != wrong.StatusCode {
		t.Fatalf("status mismatch: missing %d, wrong %d", missing.StatusCode, wrong.StatusCode)
	}
	missingBody, err := io.ReadAll(missing.Body)
	if err != nil {
		t.Fatal(err)
	}
	wrongBody, err := io.ReadAll(wrong.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(missingBody, wrongBody) {
		t.Fatalf("bodies differ: missing %q, wrong %q", missingBody, wrongBody)
	}
	for _, key := range []string{"Content-Type", "Www-Authenticate"} {
		if missing.Header.Get(key) != wrong.Header.Get(key) {
			t.Errorf("header %q differs: missing %q, wrong %q", key, missing.Header.Get(key), wrong.Header.Get(key))
		}
	}
}

// TestBearerAuthAdmitsOnlyThePerStartToken pins the positive path: exactly
// the configured token passes, and each route then behaves as unguarded —
// proving the middleware wrapped the real mux rather than replacing it.
func TestBearerAuthAdmitsOnlyThePerStartToken(t *testing.T) {
	src, base := newAuthedServer(t, "correct-token", "127.0.0.1")

	resp := authRequest(t, "GET", base+"/api/v1/state", "correct-token", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state status = %d, want 200", resp.StatusCode)
	}
	var snap SnapshotResponse
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.SchemaVersion != 1 || len(snap.Runs) != 2 {
		t.Errorf("snapshot behind middleware: schema %d runs %d", snap.SchemaVersion, len(snap.Runs))
	}

	resp = authRequest(t, "GET", base+"/api/v1/T-1", "correct-token", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("issue status = %d, want 200", resp.StatusCode)
	}
	resp = authRequest(t, "GET", base+"/api/v1/not-a-route", "correct-token", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown route with token: status %d, want 404", resp.StatusCode)
	}
	if got := decodeEnvelope(t, resp); got.Error != "not_found" {
		t.Errorf("unknown route envelope: %+v", got)
	}

	resp = authRequest(t, "POST", base+"/api/v1/refresh", "correct-token", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("refresh with token: status %d, want 202", resp.StatusCode)
	}
	if n := src.refreshes.Load(); n != 1 {
		t.Errorf("refresh called %d times, want 1", n)
	}
}

// TestBearerAuthRejectsForeignHostBeforeHandlers pins the strict Host
// check: a Host header other than the bound loopback host is rejected
// before any route handler runs, even with the correct token — the refresh
// POST included.
func TestBearerAuthRejectsForeignHostBeforeHandlers(t *testing.T) {
	src, base := newAuthedServer(t, "correct-token", "127.0.0.1")
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/v1/state"},
		{"POST", "/api/v1/refresh"},
	} {
		resp := authRequest(t, rt.method, base+rt.path, "correct-token", "evil.example.com:80")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with foreign host: status %d, want 403", rt.method, rt.path, resp.StatusCode)
		}
		if got := decodeEnvelope(t, resp); got.SchemaVersion != SchemaVersion || got.Error == "" {
			t.Errorf("%s %s foreign host envelope: %+v", rt.method, rt.path, got)
		}
	}
	if n := src.refreshes.Load(); n != 0 {
		t.Errorf("foreign-host requests reached the handler: %d refreshes", n)
	}
}

// TestBearerAuthNeverEmitsCORSHeaders pins the no-CORS posture: no
// Access-Control-* header may appear on any response — allowed, 401, 403,
// 404, or OPTIONS preflight alike.
func TestBearerAuthNeverEmitsCORSHeaders(t *testing.T) {
	_, base := newAuthedServer(t, "correct-token", "127.0.0.1")
	cases := []struct {
		name              string
		method, url, host string
		token             string
	}{
		{"allowed", "GET", "/api/v1/state", "", "correct-token"},
		{"missing token", "GET", "/api/v1/state", "", ""},
		{"wrong token", "GET", "/api/v1/state", "", "wrong"},
		{"foreign host", "GET", "/api/v1/state", "evil.example.com", "correct-token"},
		{"unknown route", "GET", "/api/v1/nope", "", "correct-token"},
		{"preflight unauthenticated", "OPTIONS", "/api/v1/state", "", ""},
		{"preflight authenticated", "OPTIONS", "/api/v1/state", "", "correct-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := authRequest(t, tc.method, base+tc.url, tc.token, tc.host)
			for key := range resp.Header {
				if strings.HasPrefix(strings.ToLower(key), "access-control-") {
					t.Errorf("%s %s: response carries CORS header %q", tc.method, tc.url, key)
				}
			}
		})
	}
}

// TestBearerTokenParsing covers the Authorization header shapes: only a
// case-insensitive Bearer scheme yields credentials; everything else is a
// missing token.
func TestBearerTokenParsing(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"canonical", "Bearer abc", "abc"},
		{"lowercase scheme", "bearer abc", "abc"},
		{"extra spaces", "  Bearer   abc  ", "abc"},
		{"empty credentials", "Bearer ", ""},
		{"wrong scheme", "Basic abc", ""},
		{"no space", "Bearerabc", ""},
		{"empty header", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bearerToken(tt.header); got != tt.want {
				t.Errorf("bearerToken(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

// TestBearerAuthHostMatching covers the Host comparison: same host with any
// port matches, a different host or a missing header does not, and bracketed
// IPv6 hosts compare by their address against the bound address.
func TestBearerAuthHostMatching(t *testing.T) {
	tests := []struct {
		header string
		bound  string
		want   bool
	}{
		{"127.0.0.1:9000", "127.0.0.1", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"[::1]:9000", "::1", true},
		{"[::1]:9000", "127.0.0.1", false},
		{"localhost:9000", "127.0.0.1", false},
		{"127.0.0.1.evil.com:9000", "127.0.0.1", false},
		{"", "127.0.0.1", false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("host=%q", tt.header), func(t *testing.T) {
			if got := hostMatches(tt.header, tt.bound); got != tt.want {
				t.Errorf("hostMatches(%q, %q) = %v, want %v", tt.header, tt.bound, got, tt.want)
			}
		})
	}
}
