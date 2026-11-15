package httpstate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource is a concurrency-safe in-memory StateSource for tests.
type fakeSource struct {
	mu        sync.Mutex
	snapshot  Snapshot
	issueID   string
	issue     IssueState
	issueOK   bool
	refreshes atomic.Int64
}

func (f *fakeSource) Snapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}

func (f *fakeSource) Issue(id string) (IssueState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issue, f.issueOK && id == f.issueID
}

func (f *fakeSource) Refresh() { f.refreshes.Add(1) }

func testSnapshot() Snapshot {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	return Snapshot{
		Runs: []Run{
			{Issue: "T-1", Session: "ses-abc", Attempt: 2, State: "running", LastEvent: now.Add(-30 * time.Second)},
			{Issue: "T-2", State: "stopped"},
		},
		DeferredUntil: now.Add(10 * time.Minute),
		WakeAt:        now.Add(5 * time.Minute),
		NeedsHuman: []NeedsHuman{
			{Issue: "T-2", Reason: "gate_failed", Detail: "gh:pr rejected", Since: now.Add(-time.Hour)},
		},
	}
}

func TestRoutes(t *testing.T) {
	now := time.Now
	snap := testSnapshot()
	snap.GateRejections = []GateRejection{
		{Issue: "T-2", Gate: "gh:pr", Reason: "merge conflict", At: time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC)},
	}
	src := &fakeSource{snapshot: snap, issue: IssueState{
		State: "running", Attempt: 2, Session: "ses-abc",
		LastEvent: now().Add(-30 * time.Second), Summary: "working",
	}, issueOK: true, issueID: "T-1"}

	srv := New(src, mustListen(t))
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	go func() { _ = srv.Start() }()
	base := "http://" + srv.Addr().String()

	t.Run("state returns typed snapshot", func(t *testing.T) {
		var got SnapshotResponse
		if err := getJSON(t, base+"/api/v1/state", &got); err != nil {
			t.Fatalf("GET state: %v", err)
		}
		if got.SchemaVersion != 1 {
			t.Errorf("schema_version = %d, want 1", got.SchemaVersion)
		}
		if len(got.Runs) != 2 {
			t.Fatalf("runs = %d, want 2", len(got.Runs))
		}
		first := got.Runs[0]
		if first.Issue != "T-1" || first.Session != "ses-abc" || first.Attempt != 2 || first.State != "running" {
			t.Errorf("unexpected first run DTO: %+v", first)
		}
		if !strings.Contains(first.LastEventAge, "s") {
			t.Errorf("last_event_age %q should describe seconds", first.LastEventAge)
		}
		if got.Runs[1].LastEventAge != "n/a" {
			t.Errorf("zero-event run last_event_age = %q, want n/a", got.Runs[1].LastEventAge)
		}
		if got.DeferredUntil != "2026-11-15T12:10:00Z" {
			t.Errorf("deferred_until = %q", got.DeferredUntil)
		}
		if got.WakeAt != "2026-11-15T12:05:00Z" {
			t.Errorf("wake_at = %q", got.WakeAt)
		}
		if len(got.NeedsHuman) != 1 || got.NeedsHuman[0].Reason != "gate_failed" {
			t.Errorf("needs_human = %+v", got.NeedsHuman)
		}
		if len(got.GateRejections) != 1 || got.GateRejections[0].Gate != "gh:pr" {
			t.Errorf("gate_rejections = %+v", got.GateRejections)
		}
	})

	t.Run("issue returns typed detail", func(t *testing.T) {
		var got IssueResponse
		if err := getJSON(t, base+"/api/v1/T-1", &got); err != nil {
			t.Fatalf("GET issue: %v", err)
		}
		if got.SchemaVersion != 1 || got.Issue != "T-1" || got.State != "running" || got.Attempt != 2 || got.Session != "ses-abc" {
			t.Errorf("unexpected issue DTO: %+v", got)
		}
	})

	t.Run("unknown issue returns 404 typed error", func(t *testing.T) {
		resp, err := http.Get(base + "/api/v1/T-404")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		var got ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if got.Error != "not_found" || got.Message == "" {
			t.Errorf("unexpected error DTO: %+v", got)
		}
	})

	t.Run("refresh returns accepted and calls source", func(t *testing.T) {
		resp, err := http.Post(base+"/api/v1/refresh", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}
		var got RefreshResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if !got.Accepted || got.SchemaVersion != 1 {
			t.Errorf("unexpected refresh DTO: %+v", got)
		}
		if n := src.refreshes.Load(); n != 1 {
			t.Errorf("refresh called %d times, want 1", n)
		}
	})

	t.Run("method mismatch is rejected", func(t *testing.T) {
		resp, err := http.Post(base+"/api/v1/state", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST state status = %d, want 405", resp.StatusCode)
		}
	})
}

func TestSnapshotFieldNames(t *testing.T) {
	// Golden-ish field-name stability: the exact JSON keys the spec fixes.
	src := &fakeSource{snapshot: testSnapshot(), issue: IssueState{State: "running", Attempt: 1}, issueID: "T-1", issueOK: true}
	ts := httptest.NewServer(handlerFor(src))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/state")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "runs", "needs_human", "gate_rejections"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("snapshot JSON missing key %q", key)
		}
	}
	var runsRaw []map[string]json.RawMessage
	if err := json.Unmarshal(raw["runs"], &runsRaw); err != nil {
		t.Fatal(err)
	}
	if len(runsRaw) != 2 {
		t.Fatalf("runs length = %d, want 2", len(runsRaw))
	}
	for _, key := range []string{"issue", "session", "attempt", "state", "last_event_age"} {
		if _, ok := runsRaw[0][key]; !ok {
			t.Errorf("run JSON missing key %q", key)
		}
	}
}

func TestConcurrentReads(t *testing.T) {
	src := &fakeSource{snapshot: testSnapshot(), issue: IssueState{State: "running", Attempt: 1}, issueID: "T-1", issueOK: true}
	srv := New(src, mustListen(t))
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	go func() { _ = srv.Start() }()
	base := "http://" + srv.Addr().String()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			url := base + "/api/v1/state"
			if i%2 == 0 {
				url = base + "/api/v1/T-1"
			}
			resp, err := http.Get(url) // #nosec G107 -- test-local server URL
			if err != nil {
				t.Errorf("GET %s: %v", url, err)
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s status = %d", url, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
	if n := src.refreshes.Load(); n != 0 {
		t.Errorf("unexpected refreshes: %d", n)
	}
}

func TestGracefulShutdown(t *testing.T) {
	ln, err := ListenLoopback()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(&fakeSource{}, ln)
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned %v, want nil after shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero", time.Time{}, "n/a"},
		{"future clamped", now.Add(time.Minute), "0s"},
		{"seconds", now.Add(-90 * time.Second), "1m30s"},
		{"minutes", now.Add(-10*time.Minute - 20*time.Second), "10m20s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatAge(tt.t, now); got != tt.want {
				t.Errorf("formatAge = %q, want %q", got, tt.want)
			}
		})
	}
}

// handlerFor exposes the same routing New installs, for httptest servers.
func handlerFor(src StateSource) http.Handler {
	s := New(src, nopListener{})
	return s.http.Handler
}

type nopListener struct{}

func (nopListener) Accept() (net.Conn, error) { return nil, fmt.Errorf("nop listener") }
func (nopListener) Close() error              { return nil }
func (nopListener) Addr() net.Addr            { return nopAddr{} }

type nopAddr struct{}

func (nopAddr) Network() string { return "nop" }
func (nopAddr) String() string  { return "nop" }

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := ListenLoopback()
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func getJSON(t *testing.T, url string, into any) error {
	t.Helper()
	resp, err := http.Get(url) // #nosec G107 -- test-local server URL
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return err
	}
	return json.Unmarshal(buf.Bytes(), into)
}
