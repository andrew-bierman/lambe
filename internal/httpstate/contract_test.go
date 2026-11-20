package httpstate

// The versioned wire contract, pinned: docs/api/state-api.md documents the
// local state API for clients that never read Go source, and this file keeps
// the repo honest about it. The fixture tests marshal canonical DTO values
// into testdata/contract/v1 behind the -update flag, assert the committed
// bytes match the current DTOs exactly (any drift fails CI), and decode the
// committed fixtures back into the current types. The error-shape tests pin
// every non-2xx envelope as JSON on the wire, and the route-precedence test
// pins that reserved literal paths shadow the {issue} wildcard.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// updateFixtures rewrites the committed contract fixtures. Usage:
//
//	go test ./internal/httpstate -run TestContract -update
var updateFixtures = flag.Bool("update", false, "rewrite the committed contract fixtures in testdata/contract/v1")

// contractFixturesDir is the committed fixture home relative to this
// package: the repo-root testdata tree Swift tests mirror.
var contractFixturesDir = filepath.Join("..", "..", "testdata", "contract", "v1")

// contractFixture pairs one canonical DTO value with a fresh decode target
// of the same type.
type contractFixture struct {
	name    string
	value   any
	decoded any
}

// canonicalDaemon is the daemon identity block every fixture carries: one
// fixed, obviously synthetic identity so fixtures stay byte-stable.
func canonicalDaemon() DaemonDTO {
	return DaemonDTO{
		PID:        4242,
		Identity:   "darwin:boot:1:2",
		Machine:    "operators-mac",
		Version:    "v0.9.0",
		ConfigPath: "/Users/operator/lambe/lambe.yaml",
	}
}

// canonicalSnapshot demonstrates the snapshot shape, including the omitted
// optional fields: the second run carries no session and no last event, and
// the gate rejection carries no gate kind — absent because unknown, never a
// placeholder.
func canonicalSnapshot() SnapshotResponse {
	return SnapshotResponse{
		SchemaVersion: SchemaVersion,
		Daemon:        canonicalDaemon(),
		Runs: []RunDTO{
			{Issue: "T-1", Session: "ses-t1", Attempt: 2, State: "running", LastEventAge: "30s", LastEventAt: "2026-11-19T11:59:30Z", Phase: "gating"},
			{Issue: "T-2", Attempt: 1, State: "deferred", LastEventAge: "n/a", Phase: "deferred"},
		},
		DeferredUntil: "2026-11-19T12:10:00Z",
		WakeAt:        "2026-11-19T12:05:00Z",
		NeedsHuman: []NeedsHumanDTO{
			{Issue: "T-2", Reason: "gate_failed", Detail: "gh:pr rejected: merge conflict", Since: "2026-11-19T11:00:00Z"},
		},
		GateRejections: []GateRejectionDTO{
			{Issue: "T-2", Reason: "gh:pr rejected: merge conflict", At: "2026-11-19T11:00:00Z"},
		},
		RunsAsOf:           "2026-11-19T12:00:00Z",
		NeedsHumanAsOf:     "2026-11-19T12:00:00Z",
		GateRejectionsAsOf: "2026-11-19T12:00:00Z",
	}
}

// canonicalIssue demonstrates the enriched issue detail with every optional
// field present; tail_truncated stays omitted because the tail is not cut.
func canonicalIssue() IssueResponse {
	return IssueResponse{
		SchemaVersion: SchemaVersion,
		Daemon:        canonicalDaemon(),
		Issue:         "T-1",
		State:         "running",
		Attempt:       2,
		Session:       "ses-t1",
		LastEventAge:  "30s",
		LastEventAt:   "2026-11-19T11:59:30Z",
		Summary:       "attempt 2 in progress",
		Phase:         "gating",
		Turn:          4,
		Thread:        "thread-t1",
		Workspace:     "/workspaces/T-1",
		Workflow:      "general",
		WakeAt:        "2026-11-19T13:00:00Z",
		LastGate:      "pr checks pending",
		Tail:          "10:59:30 assistant: running the completion gate",
	}
}

// canonicalQueue demonstrates ready and blocked rows: blocked has no
// omitempty (always served), blockers are omitted when empty.
func canonicalQueue() QueueResponse {
	return QueueResponse{
		SchemaVersion: SchemaVersion,
		Daemon:        canonicalDaemon(),
		AsOf:          "2026-11-19T12:00:00Z",
		Rows: []QueueRowDTO{
			{ID: "Q-1", Title: "ready task", Reason: "all blockers closed"},
			{
				ID: "Q-2", Title: "blocked task", Reason: "waiting on api-9", Blocked: true,
				Blockers: []QueueBlockerDTO{{ID: "api-9", Title: "api migration"}},
			},
		},
	}
}

// canonicalHistory demonstrates the newest-first merge: a gate-stopped
// terminal row, a restart-archived interrupted attempt, and a registry
// terminal row that carries only the coarse state and outcome.
func canonicalHistory() HistoryResponse {
	return HistoryResponse{
		SchemaVersion: SchemaVersion,
		Daemon:        canonicalDaemon(),
		AsOf:          "2026-11-19T12:00:00Z",
		Runs: []HistoryRunDTO{
			{Issue: "H-2", Title: "gate red task", Phase: "stopped: gate red", Attempt: 3, UpdatedAt: "2026-11-19T12:00:00Z"},
			{Issue: "H-1", Title: "restart interrupted task", Phase: "interrupted", Attempt: 1, UpdatedAt: "2026-11-19T11:00:00Z"},
			{Issue: "H-3", State: "settled", Attempt: 1, Outcome: "completed", UpdatedAt: "2026-11-19T10:00:00Z"},
		},
	}
}

// canonicalError is the shared error envelope, shown for the not-found code.
func canonicalError() ErrorResponse {
	return ErrorResponse{SchemaVersion: SchemaVersion, Error: "not_found", Message: `unknown issue "T-404"`}
}

// contractFixtures lists every committed fixture and its decode target.
func contractFixtures() []contractFixture {
	return []contractFixture{
		{name: "snapshot.json", value: canonicalSnapshot(), decoded: &SnapshotResponse{}},
		{name: "issue.json", value: canonicalIssue(), decoded: &IssueResponse{}},
		{name: "queue.json", value: canonicalQueue(), decoded: &QueueResponse{}},
		{name: "history.json", value: canonicalHistory(), decoded: &HistoryResponse{}},
		{name: "error.json", value: canonicalError(), decoded: &ErrorResponse{}},
	}
}

// marshalFixture renders a fixture the contract pins it as: indented for
// review, HTML characters unescaped, trailing newline.
func marshalFixture(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TestContractFixtures generates (with -update) and verifies the committed
// fixtures: the committed bytes must equal the canonical marshal of the
// current DTOs, and must decode back into exactly those values. Generation
// is idempotent — rewriting produces the same bytes — so an unchanged DTO
// never churns the tree.
func TestContractFixtures(t *testing.T) {
	for _, fx := range contractFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			canonical, err := marshalFixture(fx.value)
			if err != nil {
				t.Fatalf("marshal %s: %v", fx.name, err)
			}
			path := filepath.Join(contractFixturesDir, fx.name)
			if *updateFixtures {
				if err := os.MkdirAll(contractFixturesDir, 0o750); err != nil {
					t.Fatalf("create fixture dir: %v", err)
				}
				if err := os.WriteFile(path, canonical, 0o600); err != nil { // #nosec G304 -- committed fixture path under the repo testdata dir
					t.Fatalf("write %s: %v", fx.name, err)
				}
			}
			committed, err := os.ReadFile(path) // #nosec G304 -- committed fixture path under the repo testdata dir
			if err != nil {
				t.Fatalf("read committed fixture (generate with `go test ./internal/httpstate -run TestContract -update`): %v", err)
			}
			if !bytes.Equal(committed, canonical) {
				t.Errorf("fixture %s drifted from the current DTOs; regenerate with `go test ./internal/httpstate -run TestContract -update`\ncommitted:\n%s\ncanonical:\n%s", fx.name, committed, canonical)
			}
			if err := json.Unmarshal(committed, fx.decoded); err != nil {
				t.Fatalf("decode %s into %T: %v", fx.name, fx.decoded, err)
			}
			got := reflect.Indirect(reflect.ValueOf(fx.decoded)).Interface()
			if !reflect.DeepEqual(got, fx.value) {
				t.Errorf("decoded fixture %s = %+v, want the canonical %+v", fx.name, got, fx.value)
			}
		})
	}
}

// descriptorFixtureName and descriptorFixtureJSON pin the committed
// discovery-document example. It is a literal rather than a marshal of the
// daemon's descriptor type: that type lives in internal/daemon, and importing
// it here would make this package's tests depend on the daemon package.
// The daemon's own tests pin the Go type's round trip; this test pins the
// committed fixture's shape — exact key set, value types, loopback addr,
// and the known placeholder token — so docs and clients cannot drift from
// the committed example and no real secret can ever be committed.
const (
	descriptorFixtureName = "descriptor.json"
	// descriptorFixtureToken is the placeholder committed in the fixture —
	// never a real credential.
	descriptorFixtureToken = "descriptor-fixture-token-not-a-secret" //nolint:gosec // G101: fixture placeholder, not a credential
	descriptorFixtureJSON  = `{
  "schema_version": 1,
  "instance": "lambe-1a2b3c4d5e6f",
  "addr": "127.0.0.1:53412",
  "bearer_token": "descriptor-fixture-token-not-a-secret",
  "pid": 4242,
  "identity": "darwin:boot:1:2",
  "version": "v0.9.0",
  "config_path": "/Users/operator/lambe/lambe.yaml",
  "started_at": "2026-11-19T11:30:00Z"
}
`
)

// TestContractDescriptorFixture verifies the committed descriptor fixture
// against the documented discovery schema: exact key set, correct types, a
// literal loopback addr, an RFC 3339 start time, and the placeholder token.
func TestContractDescriptorFixture(t *testing.T) {
	path := filepath.Join(contractFixturesDir, descriptorFixtureName)
	if *updateFixtures {
		if err := os.MkdirAll(contractFixturesDir, 0o750); err != nil {
			t.Fatalf("create fixture dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(descriptorFixtureJSON), 0o600); err != nil { // #nosec G304 -- committed fixture path under the repo testdata dir
			t.Fatalf("write %s: %v", descriptorFixtureName, err)
		}
	}
	data, err := os.ReadFile(path) // #nosec G304 -- committed fixture path under the repo testdata dir
	if err != nil {
		t.Fatalf("read committed fixture (generate with `go test ./internal/httpstate -run TestContract -update`): %v", err)
	}
	if !bytes.Equal(data, []byte(descriptorFixtureJSON)) {
		t.Errorf("descriptor fixture drifted from the pinned example; regenerate with `go test ./internal/httpstate -run TestContract -update`")
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode descriptor fixture: %v", err)
	}
	for _, key := range []string{
		"schema_version", "instance", "addr", "bearer_token",
		"pid", "identity", "version", "config_path", "started_at",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("descriptor fixture missing key %q", key)
		}
	}
	if len(raw) != 9 {
		t.Errorf("descriptor fixture carries %d keys, want exactly the 9 documented ones: %v", len(raw), raw)
	}
	if v, ok := raw["schema_version"].(float64); !ok || v != 1 {
		t.Errorf("schema_version = %v, want the number 1", raw["schema_version"])
	}
	addr, _ := raw["addr"].(string)
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("addr = %q, want a literal 127.0.0.1 host", addr)
	}
	if token, _ := raw["bearer_token"].(string); token != descriptorFixtureToken {
		t.Errorf("bearer_token = %q, want the documented placeholder (never a real secret)", token)
	}
	if v, ok := raw["pid"].(float64); !ok || v != 4242 {
		t.Errorf("pid = %v, want the number 4242", raw["pid"])
	}
	startedAt, _ := raw["started_at"].(string)
	if _, err := time.Parse(time.RFC3339, startedAt); err != nil {
		t.Errorf("started_at = %q, want RFC 3339: %v", startedAt, err)
	}
	for _, key := range []string{"instance", "identity", "version", "config_path"} {
		if s, _ := raw[key].(string); s == "" {
			t.Errorf("%s = %v, want a non-empty string", key, raw[key])
		}
	}
}

// newContractServer starts a real Server wrapped in BearerAuth exactly as
// the daemon assembles it, so the pinned envelopes are the wire bytes a
// client actually receives.
func newContractServer(t *testing.T, token string) (*fakeSource, string) {
	t.Helper()
	src := &fakeSource{
		snapshot: testSnapshot(),
		issue:    IssueState{State: "running", Attempt: 1},
		issueID:  "T-1",
		issueOK:  true,
	}
	srv := New(src, mustListen(t))
	srv.Wrap(BearerAuth(token, "127.0.0.1"))
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	go func() { _ = srv.Start() }()
	return src, "http://" + srv.Addr().String()
}

// contractRequest performs one request with an optional bearer token and
// Host override; empty values mean absent.
func contractRequest(t *testing.T, method, url, token, host string) *http.Response {
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
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// mustEnvelope renders the exact bytes writeJSON emits for an error
// envelope: compact, HTML-unescaped, trailing newline.
func mustEnvelope(t *testing.T, code, message string) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ErrorResponse{SchemaVersion: SchemaVersion, Error: code, Message: message}); err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return buf.Bytes()
}

// TestContractErrorEnvelopes pins every non-2xx shape as exact JSON on the
// wire — never plaintext — through the assembled bearer-guarded server: the
// 401 uniform rejection, the 403 foreign-host rejection, the 404 wildcard
// miss and the 404 catch-all (an empty identifier never reaches the
// wildcard), and both wrong-method 405s. The defensive 400 branch is pinned
// by driving the issue handler directly, since routing currently delivers
// empty identifiers to the catch-all instead.
func TestContractErrorEnvelopes(t *testing.T) {
	_, base := newContractServer(t, "contract-token")
	const token = "contract-token"
	cases := []struct {
		name                string
		method, url         string
		token, host         string
		wantStatus          int
		wantCode, wantMsg   string
		wantWWWAuthenticate bool
	}{
		{
			name:   "missing token is a 401 envelope on every route",
			method: http.MethodGet, url: base + "/api/v1/state",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized", wantMsg: "missing or invalid bearer token",
			wantWWWAuthenticate: true,
		},
		{
			name:   "foreign host is a 403 envelope before any handler",
			method: http.MethodGet, url: base + "/api/v1/state", token: token, host: "evil.example.com:80",
			wantStatus: http.StatusForbidden,
			wantCode:   "bad_host", wantMsg: "request host does not match the bound loopback address",
		},
		{
			name:   "unknown issue identifier is a 404 envelope from the wildcard",
			method: http.MethodGet, url: base + "/api/v1/definitely-not-a-route", token: token,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found", wantMsg: `unknown issue "definitely-not-a-route"`,
		},
		{
			name:   "empty identifier is the 404 catch-all, not the wildcard",
			method: http.MethodGet, url: base + "/api/v1/", token: token,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found", wantMsg: "unknown route",
		},
		{
			name:   "a multi-segment path is the 404 catch-all",
			method: http.MethodGet, url: base + "/api/v1/a/b", token: token,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found", wantMsg: "unknown route",
		},
		{
			name:   "GET refresh is a 405 envelope",
			method: http.MethodGet, url: base + "/api/v1/refresh", token: token,
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed", wantMsg: "wrong method for this endpoint",
		},
		{
			name:   "POST state is a 405 envelope",
			method: http.MethodPost, url: base + "/api/v1/state", token: token,
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed", wantMsg: "wrong method for this endpoint",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := contractRequest(t, tc.method, tc.url, tc.token, tc.host)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if auth := resp.Header.Get("Www-Authenticate"); tc.wantWWWAuthenticate && auth != `Bearer realm="lambe-stateapi"` {
				t.Errorf("WWW-Authenticate = %q, want the bearer realm", auth)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			want := mustEnvelope(t, tc.wantCode, tc.wantMsg)
			if !bytes.Equal(body, want) {
				t.Errorf("body = %s, want the pinned envelope %s", body, want)
			}
			var decoded ErrorResponse
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode body as ErrorResponse: %v", err)
			}
		})
	}

	// The defensive 400 branch: an empty identifier reaching the issue
	// handler directly. The mux never routes one there today; the pin keeps
	// the envelope a DTO if routing ever changes.
	srv := New(&fakeSource{issueID: "T-1", issueOK: true}, nopListener{})
	rec := httptest.NewRecorder()
	srv.handleIssue(rec, httptest.NewRequest(http.MethodGet, "/api/v1/", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad-request branch status = %d, want 400", rec.Code)
	}
	want := mustEnvelope(t, "bad_request", "missing issue identifier")
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("bad-request body = %s, want the pinned envelope %s", rec.Body.Bytes(), want)
	}
}

// TestContractRoutePrecedence pins the reserved-name rule: the literal
// routes state, queue, history, and refresh win over the /api/v1/{issue}
// wildcard, so an issue whose identifier collides with a reserved name is
// shadowed, while ordinary identifiers still reach the wildcard.
func TestContractRoutePrecedence(t *testing.T) {
	// The shadowed source knows an issue literally named "state"; if the
	// wildcard ever won, /api/v1/state would return that issue's detail.
	shadow := &fakeSource{
		snapshot: testSnapshot(),
		queue: Queue{
			AsOf: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
			Rows: []QueueRow{{ID: "Q-1", Title: "ready task"}},
		},
		history: History{
			AsOf: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
			Runs: []HistoryRun{{Issue: "H-9", Phase: "stopped: gate green", Attempt: 2, UpdatedAt: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}},
		},
		issueID: "state",
		issue:   IssueState{State: "running", Attempt: 1},
		issueOK: true,
	}
	srv := New(shadow, mustListen(t))
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	go func() { _ = srv.Start() }()
	base := "http://" + srv.Addr().String()

	t.Run("state serves the snapshot, not the shadowed issue", func(t *testing.T) {
		var snap SnapshotResponse
		if err := getJSON(t, base+"/api/v1/state", &snap); err != nil {
			t.Fatalf("GET state: %v", err)
		}
		if snap.SchemaVersion != 1 || len(snap.Runs) != 2 {
			t.Errorf("snapshot = schema %d with %d runs, want the served snapshot", snap.SchemaVersion, len(snap.Runs))
		}
		var issue IssueResponse
		if err := getJSON(t, base+"/api/v1/state", &issue); err != nil {
			t.Fatalf("GET state: %v", err)
		}
		if issue.Issue != "" {
			t.Errorf("state route served issue detail for %q; the wildcard shadowed a reserved path", issue.Issue)
		}
	})

	t.Run("queue serves the queue, not the shadowed issue", func(t *testing.T) {
		var queue QueueResponse
		if err := getJSON(t, base+"/api/v1/queue", &queue); err != nil {
			t.Fatalf("GET queue: %v", err)
		}
		if len(queue.Rows) != 1 || queue.Rows[0].ID != "Q-1" {
			t.Errorf("queue rows = %+v, want the served readiness row", queue.Rows)
		}
		var issue IssueResponse
		if err := getJSON(t, base+"/api/v1/queue", &issue); err != nil {
			t.Fatalf("GET queue: %v", err)
		}
		if issue.Issue != "" {
			t.Errorf("queue route served issue detail for %q; the wildcard shadowed a reserved path", issue.Issue)
		}
	})

	t.Run("history serves the history, not the shadowed issue", func(t *testing.T) {
		var history HistoryResponse
		if err := getJSON(t, base+"/api/v1/history", &history); err != nil {
			t.Fatalf("GET history: %v", err)
		}
		if len(history.Runs) != 1 || history.Runs[0].Issue != "H-9" {
			t.Errorf("history runs = %+v, want the served archived run", history.Runs)
		}
		var queue QueueResponse
		if err := getJSON(t, base+"/api/v1/history", &queue); err != nil {
			t.Fatalf("GET history: %v", err)
		}
		if len(queue.Rows) != 0 {
			t.Errorf("history route decoded as %d queue rows; the wildcard shadowed a reserved path", len(queue.Rows))
		}
	})

	t.Run("refresh POST reaches the refresh handler, GET stays 405", func(t *testing.T) {
		resp := contractRequest(t, http.MethodPost, base+"/api/v1/refresh", "", "")
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST refresh status = %d, want 202", resp.StatusCode)
		}
		if n := shadow.refreshes.Load(); n != 1 {
			t.Errorf("refresh called %d times, want 1", n)
		}
		resp = contractRequest(t, http.MethodGet, base+"/api/v1/refresh", "", "")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET refresh status = %d, want 405", resp.StatusCode)
		}
		if n := shadow.refreshes.Load(); n != 1 {
			t.Errorf("GET refresh reached a handler: %d refreshes", n)
		}
	})

	t.Run("wildcard still serves ordinary identifiers", func(t *testing.T) {
		normal := &fakeSource{issue: IssueState{State: "running", Attempt: 1}, issueID: "T-1", issueOK: true}
		other := New(normal, mustListen(t))
		t.Cleanup(func() { _ = other.Shutdown(context.Background()) })
		go func() { _ = other.Start() }()
		var issue IssueResponse
		if err := getJSON(t, "http://"+other.Addr().String()+"/api/v1/T-1", &issue); err != nil {
			t.Fatalf("GET issue: %v", err)
		}
		if issue.Issue != "T-1" || issue.State != "running" {
			t.Errorf("wildcard issue = %+v, want T-1", issue)
		}
		resp := contractRequest(t, http.MethodGet, "http://"+other.Addr().String()+"/api/v1/T-404", "", "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown issue status = %d, want 404", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, mustEnvelope(t, "not_found", `unknown issue "T-404"`)) {
			t.Errorf("unknown-issue body = %s, want the pinned envelope", body)
		}
	})
}
