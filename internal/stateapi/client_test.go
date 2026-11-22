package stateapi

// The client tests decode the contract, not the daemon: fixture
// descriptors and canned route payloads stand in for the serving daemon,
// while the parity pin between the daemon's published descriptor document
// and this package's decode target lives in
// TestDescriptorDecodesDaemonDocument. The assembled end-to-end proof —
// a real binary serving real seeded facts through this client — is the
// dashboard smoke in test/dashsmoke.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/httpstate"
	"github.com/andrew-bierman/lambe/internal/proc"
)

// token is the fake bearer the tests assert on: every typed error and
// every failure path is checked against it, so a leak cannot hide.
const token = "smoke-bearer-secret"

// writeDescriptorFile writes one descriptor document into dir's live
// subdirectory and returns its path.
func writeDescriptorFile(t *testing.T, dir, name string, d Descriptor) string {
	t.Helper()
	live := filepath.Join(dir, "live")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(d) //nolint:gosec // G117: test fixture for the decode target, in a temp dir
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(live, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// endpointFor builds the Endpoint a fixture descriptor would resolve to.
func endpointFor(addr string) Endpoint {
	return Endpoint{
		Instance: "inst",
		Addr:     addr,
		Token:    token,
		PID:      os.Getpid(),
		Identity: "",
		Version:  "test",
	}
}

// TestResolveSelectsLiveInstanceAndSkipsStale covers the enumeration
// rules: the live descriptor is selected by instance, and every stale or
// invalid candidate — identity mismatch, dead pid, non-loopback address,
// corrupt document — is skipped rather than served.
func TestResolveSelectsLiveInstanceAndSkipsStale(t *testing.T) {
	dir := t.TempDir()
	// Stale: the identity recorded for a live pid no longer matches —
	// the file belongs to a dead incarnation whose pid was recycled.
	writeDescriptorFile(t, dir, "stateapi-stale-identity.json", Descriptor{
		SchemaVersion: 1, Instance: "stale-identity", Addr: "127.0.0.1:41000",
		BearerToken: "old", PID: os.Getpid(), Identity: "not-this-process",
	})
	// Stale: the pid is long dead, so the identity check fails.
	writeDescriptorFile(t, dir, "stateapi-stale-pid.json", Descriptor{
		SchemaVersion: 1, Instance: "stale-pid", Addr: "127.0.0.1:41001",
		BearerToken: "old", PID: 1 << 28, Identity: "whatever",
	})
	// Invalid: not the literal 127.0.0.1 — never resolved, and the token
	// must not even be kept around.
	writeDescriptorFile(t, dir, "stateapi-wider-loop.json", Descriptor{
		SchemaVersion: 1, Instance: "wider", Addr: "127.1.2.3:41002", BearerToken: "wider-token",
	})
	writeDescriptorFile(t, dir, "stateapi-named-host.json", Descriptor{
		SchemaVersion: 1, Instance: "named", Addr: "localhost:41003", BearerToken: "named-token",
	})
	// Corrupt: undecodable documents are skipped, not fatal.
	live := filepath.Join(dir, "live")
	if err := os.WriteFile(filepath.Join(live, "stateapi-corrupt.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The one live instance.
	writeDescriptorFile(t, dir, "stateapi-live.json", Descriptor{
		SchemaVersion: 1, Instance: "live", Addr: "127.0.0.1:41999",
		BearerToken: token, PID: os.Getpid(), Identity: "", Version: "v1",
		ConfigPath: "/cfg/lambe.yaml", StartedAt: "2026-11-19T00:00:00Z",
	})

	ep, err := Resolve(context.Background(), dir, Selector{Instance: "live"})
	if err != nil {
		t.Fatalf("resolve live instance: %v", err)
	}
	if ep.Addr != "127.0.0.1:41999" || ep.Token != token || ep.Instance != "live" ||
		ep.PID != os.Getpid() || ep.Version != "v1" || ep.ConfigPath != "/cfg/lambe.yaml" {
		t.Fatalf("resolved endpoint = %+v", ep)
	}

	// A selector matching nothing is ErrNotFound, not a serve of stale
	// entries.
	if _, err := Resolve(context.Background(), dir, Selector{Instance: "absent"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent instance error = %v, want ErrNotFound", err)
	}
	// Multiple live endpoints without a selector are ambiguous.
	writeDescriptorFile(t, dir, "stateapi-live2.json", Descriptor{
		SchemaVersion: 1, Instance: "live2", Addr: "127.0.0.1:42000", BearerToken: "other-token", PID: os.Getpid(),
	})
	_, err = Resolve(context.Background(), dir, Selector{})
	var amb *AmbiguityError
	if !errors.As(err, &amb) || !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("unselected resolve error = %v, want ErrAmbiguous", err)
	}
	for _, want := range []string{"live", "live2"} {
		if !strings.Contains(amb.Error(), want) {
			t.Errorf("ambiguity error %q does not name instance %q", amb.Error(), want)
		}
	}
	if strings.Contains(amb.Error(), token) || strings.Contains(amb.Error(), "other-token") {
		t.Errorf("ambiguity error carries a token: %q", amb.Error())
	}
	if _, err := Resolve(context.Background(), dir, Selector{Instance: "live2"}); err != nil {
		t.Fatalf("selector narrowed the ambiguity: %v", err)
	}
	// An empty state dir is ErrNotFound.
	if _, err := Resolve(context.Background(), t.TempDir(), Selector{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty dir error = %v, want ErrNotFound", err)
	}
}

// TestDescriptorDecodesDaemonDocument pins parity between the document
// the daemon publishes (daemon.StateAPIDescriptor) and the client's
// decode target: marshaling the daemon's type and resolving it through
// this package must round-trip every fact.
func TestDescriptorDecodesDaemonDocument(t *testing.T) {
	dir := t.TempDir()
	doc := daemon.StateAPIDescriptor{
		SchemaVersion: daemon.StateAPIDescriptorSchemaVersion,
		Instance:      "parity",
		Addr:          "127.0.0.1:42500",
		BearerToken:   token,
		PID:           os.Getpid(),
		Identity:      "pid-identity-must-match",
		Version:       "smoke",
		ConfigPath:    "/cfg/parity.yaml",
		StartedAt:     "2026-11-19T01:02:03Z",
	}
	// The identity is liveness-checked on read, so a non-empty value must
	// be this process's real identity; without a platform identity the
	// check is skipped and the document still resolves.
	if identity, err := proc.ProcessIdentity(os.Getpid()); err == nil {
		doc.Identity = identity
	} else {
		doc.Identity = ""
	}
	live := filepath.Join(dir, "live")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(doc) //nolint:gosec // G117: parity fixture in a temp dir
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "stateapi-parity.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ep, err := Resolve(context.Background(), dir, Selector{})
	if err != nil {
		t.Fatalf("resolve daemon document: %v", err)
	}
	if ep.Instance != doc.Instance || ep.Addr != doc.Addr || ep.Token != doc.BearerToken ||
		ep.PID != doc.PID || ep.Version != doc.Version || ep.ConfigPath != doc.ConfigPath ||
		ep.StartedAt != doc.StartedAt {
		t.Fatalf("daemon document decoded as %+v, want the published facts %+v", ep, doc)
	}
}

// TestNewClientRejectsBadEndpoints pins the gate order: an endpoint that
// is not the literal loopback, or carries no token, is refused before any
// request could attach the token.
func TestNewClientRejectsBadEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		ep   Endpoint
	}{
		{"routable addr", Endpoint{Addr: "10.0.0.5:8080", Token: token}},
		{"ip6 loopback", Endpoint{Addr: "[::1]:8080", Token: token}},
		{"named host", Endpoint{Addr: "localhost:8080", Token: token}},
		{"no port", Endpoint{Addr: "127.0.0.1", Token: token}},
		{"empty token", Endpoint{Addr: "127.0.0.1:8080"}},
	} {
		_, err := NewClient(tc.ep, WithCacheTTL(0))
		if !errors.Is(err, ErrBadEndpoint) {
			t.Errorf("%s: error = %v, want ErrBadEndpoint", tc.name, err)
		}
		if err != nil && strings.Contains(err.Error(), token) {
			t.Errorf("%s: error carries the token: %q", tc.name, err.Error())
		}
	}
}

// fixtureServer is an httptest backend standing in for the daemon: it
// records the Authorization header of every request and answers each
// route from a canned map. It binds 127.0.0.1, so the resolved endpoint
// satisfies the client's literal-loopback validation.
type fixtureServer struct {
	*httptest.Server
	mu      sync.Mutex
	tokens  map[string][]string
	hits    map[string]int
	payload map[string]func(w http.ResponseWriter, r *http.Request)
}

func newFixtureServer(t *testing.T) *fixtureServer {
	t.Helper()
	f := &fixtureServer{
		tokens:  map[string][]string{},
		hits:    map[string]int{},
		payload: map[string]func(w http.ResponseWriter, r *http.Request){},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokens[r.URL.Path] = append(f.tokens[r.URL.Path], r.Header.Get("Authorization"))
		f.hits[r.URL.Path]++
		handler := f.payload[r.URL.Path]
		f.mu.Unlock()
		if handler == nil {
			http.Error(w, "unexpected route", http.StatusNotFound)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fixtureServer) authFor(path string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens[path]...)
}

func (f *fixtureServer) hitCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fixtureServer) setPayload(path string, h func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payload[path] = h
}

// writeJSON answers one canned DTO as the contract's JSON.
func writeJSON(t *testing.T, v any) func(http.ResponseWriter, *http.Request) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

func newTestClient(t *testing.T, f *fixtureServer) *Client {
	t.Helper()
	ep := endpointFor(strings.TrimPrefix(f.URL, "http://"))
	c, err := NewClient(ep, WithCacheTTL(0))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// TestTypedCallsSendBearerAndDecode pins the four reads: each sends the
// bearer header and decodes the contract DTO it claims to return, and an
// unknown issue surfaces the server's typed 404 envelope.
func TestTypedCallsSendBearerAndDecode(t *testing.T) {
	f := newFixtureServer(t)
	wantSnap := httpstate.SnapshotResponse{
		SchemaVersion: httpstate.SchemaVersion,
		Daemon:        httpstate.DaemonDTO{PID: 4242, Identity: "i", Machine: "m", Version: "v"},
		Runs:          []httpstate.RunDTO{{Issue: "smoke-1", State: "running", Attempt: 1}},
		NeedsHuman:    []httpstate.NeedsHumanDTO{{Issue: "smoke-2", Reason: "gate red"}},
	}
	wantQueue := httpstate.QueueResponse{
		SchemaVersion: httpstate.SchemaVersion,
		Rows: []httpstate.QueueRowDTO{
			{ID: "smoke-3", Title: "ready row"},
			{ID: "smoke-4", Blocked: true, Reason: "ambiguous workflow labels", Blockers: []httpstate.QueueBlockerDTO{{ID: "smoke-5"}}},
		},
	}
	wantHistory := httpstate.HistoryResponse{
		SchemaVersion: httpstate.SchemaVersion,
		Runs:          []httpstate.HistoryRunDTO{{Issue: "smoke-1", Phase: "stopped: gate green", Attempt: 1}},
	}
	wantIssue := httpstate.IssueResponse{
		SchemaVersion: httpstate.SchemaVersion,
		Issue:         "smoke-1",
		State:         "settled",
		Phase:         "stopped: gate green",
		Attempt:       1,
		Workspace:     "/tmp/ws",
		Workflow:      "smoke-a",
	}
	f.setPayload("/api/v1/state", writeJSON(t, wantSnap))
	f.setPayload("/api/v1/queue", writeJSON(t, wantQueue))
	f.setPayload("/api/v1/history", writeJSON(t, wantHistory))
	f.setPayload("/api/v1/smoke-1", writeJSON(t, wantIssue))
	f.setPayload("/api/v1/unknown-issue", func(w http.ResponseWriter, _ *http.Request) {
		data, err := json.Marshal(httpstate.ErrorResponse{
			SchemaVersion: httpstate.SchemaVersion, Error: "not_found", Message: "unknown issue",
		})
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(data)
	})

	ctx := context.Background()
	c := newTestClient(t, f)

	gotSnap, err := c.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if gotSnap.SchemaVersion != wantSnap.SchemaVersion || len(gotSnap.Runs) != 1 || gotSnap.Runs[0].Issue != "smoke-1" ||
		len(gotSnap.NeedsHuman) != 1 || gotSnap.NeedsHuman[0].Reason != "gate red" {
		t.Fatalf("State decoded %+v", gotSnap)
	}
	gotQueue, err := c.Queue(ctx)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(gotQueue.Rows) != 2 || gotQueue.Rows[1].Blockers[0].ID != "smoke-5" || !gotQueue.Rows[1].Blocked {
		t.Fatalf("Queue decoded %+v", gotQueue)
	}
	gotHistory, err := c.History(ctx)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(gotHistory.Runs) != 1 || gotHistory.Runs[0].Phase != "stopped: gate green" {
		t.Fatalf("History decoded %+v", gotHistory)
	}
	gotIssue, err := c.Issue(ctx, "smoke-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if gotIssue.Issue != "smoke-1" || gotIssue.Workflow != "smoke-a" || gotIssue.Workspace != "/tmp/ws" {
		t.Fatalf("Issue decoded %+v", gotIssue)
	}

	for path := range map[string]bool{
		"/api/v1/state": true, "/api/v1/queue": true, "/api/v1/history": true, "/api/v1/smoke-1": true,
	} {
		auth := f.authFor(path)
		if len(auth) != 1 || auth[0] != "Bearer "+token {
			t.Errorf("%s: auth headers %q, want exactly one bearer", path, auth)
		}
	}

	_, err = c.Issue(ctx, "unknown-issue")
	var status *StatusError
	if !errors.As(err, &status) || status.Code != http.StatusNotFound || status.Envelope.Error != "not_found" {
		t.Fatalf("unknown issue error = %v, want a typed 404 not_found envelope", err)
	}
	if strings.Contains(status.Error(), token) {
		t.Errorf("status error carries the token: %q", status.Error())
	}
	_, err = c.Issue(ctx, "")
	if err == nil {
		t.Error("empty issue id must be a caller error")
	}
}

// TestRedirectRefused pins the transport hardening: a 302 answer is an
// error and the client never requests the redirect target.
// TestSuccessBodyBoundAllowsBusyHistory pins the two body bounds: a 2xx
// history page over the error envelope's size decodes whole — success
// never rides the error cap — while a body over its own bound is an
// explicit typed overflow, never silent truncation.
func TestSuccessBodyBoundAllowsBusyHistory(t *testing.T) {
	f := newFixtureServer(t)
	c := newTestClient(t, f)

	// A busy history: over maxErrorBodyBytes, under maxBodyBytes. The
	// daemon bounds history at 100 entries, and a page of them can
	// legitimately exceed the error envelope's cap.
	big := strings.Repeat("x", maxErrorBodyBytes+1024)
	want := httpstate.HistoryResponse{
		SchemaVersion: httpstate.SchemaVersion,
		Runs:          []httpstate.HistoryRunDTO{{Issue: big, Phase: "stopped: gate green", Attempt: 1}},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	f.setPayload("/api/v1/history", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	got, err := c.History(context.Background())
	if err != nil {
		t.Fatalf("history over the error bound: %v", err)
	}
	if len(got.Runs) != 1 || got.Runs[0].Issue != big {
		t.Errorf("history carried %d runs, want the one full untruncated row", len(got.Runs))
	}

	// A success body over the success bound is an explicit overflow.
	f.setPayload("/api/v1/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Repeat("a", maxBodyBytes+1)))
	})
	if _, err := c.State(context.Background()); !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("state over the success bound: err %v, want ErrBodyTooLarge", err)
	}

	// A non-2xx body over the error envelope's cap is an explicit
	// overflow too: no silent truncation on the error path either.
	f.setPayload("/api/v1/queue", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(strings.Repeat("e", maxErrorBodyBytes+1)))
	})
	if _, err := c.Queue(context.Background()); !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("error body over the envelope bound: err %v, want ErrBodyTooLarge", err)
	}
}

func TestRedirectRefused(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	f := newFixtureServer(t)
	f.setPayload("/api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusFound)
	})
	c := newTestClient(t, f)
	_, err := c.State(context.Background())
	if !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("redirect error = %v, want ErrRedirectRefused", err)
	}
	var transport *TransportError
	if !errors.As(err, &transport) {
		t.Fatalf("redirect error = %T, want *TransportError", err)
	}
	if targetHits.Load() != 0 {
		t.Errorf("redirect target was requested %d times", targetHits.Load())
	}
}

// TestBoundedTimeoutAndRetryBudget pins the bounded call shape: a hanging
// server fails within the per-attempt timeout and burns exactly the
// configured small retry budget — then returns the transport error.
func TestBoundedTimeoutAndRetryBudget(t *testing.T) {
	f := newFixtureServer(t)
	f.setPayload("/api/v1/state", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	ep := endpointFor(strings.TrimPrefix(f.URL, "http://"))
	c, err := NewClient(ep, WithCacheTTL(0), WithTimeout(50*time.Millisecond), WithAttempts(2))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	start := time.Now()
	_, err = c.State(context.Background())
	elapsed := time.Since(start)
	var transport *TransportError
	if !errors.As(err, &transport) {
		t.Fatalf("slow server error = %v, want *TransportError", err)
	}
	if hits := f.hitCount("/api/v1/state"); hits != 2 {
		t.Errorf("attempts = %d, want the configured budget of 2", hits)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("bounded call took %s, want failure within the timeout budget", elapsed)
	}
}

// TestTypedErrorsAreDistinguishable covers the error taxonomy: transport,
// status, and decode failures come back as different types, every one of
// them token-free.
func TestTypedErrorsAreDistinguishable(t *testing.T) {
	f := newFixtureServer(t)
	ep := endpointFor(strings.TrimPrefix(f.URL, "http://"))
	c, err := NewClient(ep, WithCacheTTL(0), WithAttempts(1))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	f.setPayload("/api/v1/queue", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
	_, qErr := c.Queue(ctx)
	if _, ok := qErr.(*StatusError); !ok {
		t.Errorf("401 error = %T, want *StatusError", qErr)
	}

	f.setPayload("/api/v1/queue", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	})
	_, dErr := c.Queue(ctx)
	var dec *DecodeError
	if !errors.As(dErr, &dec) {
		t.Errorf("garbage 200 error = %T, want *DecodeError", dErr)
	}

	f.Close()
	_, tErr := c.History(ctx)
	var transport *TransportError
	if !errors.As(tErr, &transport) {
		t.Errorf("dead transport error = %T, want *TransportError", tErr)
	}

	for name, e := range map[string]error{"status": qErr, "decode": dErr, "transport": tErr} {
		if e != nil && strings.Contains(e.Error(), token) {
			t.Errorf("%s error carries the token: %q", name, e.Error())
		}
	}
}

// TestCacheServesWithinTTL pins the cache contract: a second call inside
// the TTL is one transport hit, per route, and ages out by the client's
// own clock.
func TestCacheServesWithinTTL(t *testing.T) {
	f := newFixtureServer(t)
	f.setPayload("/api/v1/state", writeJSON(t, httpstate.SnapshotResponse{SchemaVersion: 1}))
	f.setPayload("/api/v1/queue", writeJSON(t, httpstate.QueueResponse{SchemaVersion: 1}))

	now := time.Unix(1_000_000, 0)
	clock := now
	ep := endpointFor(strings.TrimPrefix(f.URL, "http://"))
	c, err := NewClient(ep, WithCacheTTL(time.Minute), WithNow(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.State(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.State(ctx); err != nil {
		t.Fatal(err)
	}
	if hits := f.hitCount("/api/v1/state"); hits != 1 {
		t.Fatalf("state transport hits = %d within the TTL, want 1", hits)
	}
	// Another route is a separate cache entry.
	if _, err := c.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if hits := f.hitCount("/api/v1/queue"); hits != 1 {
		t.Fatalf("queue transport hits = %d, want its own single hit", hits)
	}
	// Past the TTL the next call goes back to the transport.
	clock = now.Add(2 * time.Minute)
	if _, err := c.State(ctx); err != nil {
		t.Fatal(err)
	}
	if hits := f.hitCount("/api/v1/state"); hits != 2 {
		t.Fatalf("state transport hits after aging out = %d, want 2", hits)
	}
	// TTL 0 disables caching outright.
	uncached, err := NewClient(ep, WithCacheTTL(0))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := uncached.Queue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if hits := f.hitCount("/api/v1/queue"); hits != 3 {
		t.Fatalf("queue transport hits with caching disabled = %d, want 3", hits)
	}
}

// TestListReportsLiveDescriptorsWithoutTokens pins the discovery listing:
// the same acceptance rules as Resolve (stale and invalid entries dropped),
// files listed in a stable order with their path, and no bearer token
// anywhere in what the listing can carry.
func TestListReportsLiveDescriptorsWithoutTokens(t *testing.T) {
	dir := t.TempDir()
	writeDescriptorFile(t, dir, "stateapi-b.json", Descriptor{
		SchemaVersion: 1, Instance: "b", Addr: "127.0.0.1:42000", BearerToken: "secret-b", PID: os.Getpid(),
	})
	pathA := writeDescriptorFile(t, dir, "stateapi-a.json", Descriptor{
		SchemaVersion: 1, Instance: "a", Addr: "127.0.0.1:41999", BearerToken: "secret-a", PID: os.Getpid(),
		Version: "v1", ConfigPath: "/cfg/lambe.yaml", StartedAt: "2026-11-19T00:00:00Z",
	})
	writeDescriptorFile(t, dir, "stateapi-stale.json", Descriptor{
		SchemaVersion: 1, Instance: "stale", Addr: "127.0.0.1:41001", BearerToken: "secret-stale", PID: 1 << 28, Identity: "gone",
	})
	writeDescriptorFile(t, dir, "stateapi-wide.json", Descriptor{
		SchemaVersion: 1, Instance: "wide", Addr: "127.1.2.3:41002", BearerToken: "secret-wide",
	})

	got, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].Instance != "a" || got[1].Instance != "b" {
		t.Fatalf("List = %+v, want instances a,b in order", got)
	}
	a := got[0]
	if a.Path != pathA || a.Addr != "127.0.0.1:41999" || a.PID != os.Getpid() ||
		a.Version != "v1" || a.ConfigPath != "/cfg/lambe.yaml" || a.StartedAt != "2026-11-19T00:00:00Z" {
		t.Fatalf("entry a = %+v", a)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "secret") || strings.Contains(string(wire), "bearer") {
		t.Fatalf("listing leaks the token: %s", wire)
	}

	// No state dir, no live dir, no descriptors: an empty answer, not an error.
	for _, missing := range []string{filepath.Join(dir, "nope"), t.TempDir()} {
		entries, err := List(missing)
		if err != nil || len(entries) != 0 {
			t.Fatalf("List(%s) = %v, %v; want empty and no error", missing, entries, err)
		}
	}
}
