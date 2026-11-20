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
	queue     Queue
	history   History
	identity  Daemon
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

func (f *fakeSource) Queue() Queue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queue
}

func (f *fakeSource) History() History {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.history
}

func (f *fakeSource) Identity() Daemon {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.identity
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
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST state status = %d, want 405", resp.StatusCode)
		}
		var got ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if got.Error != "method_not_allowed" {
			t.Errorf("POST state error DTO: %+v", got)
		}
	})

	t.Run("GET refresh is method not allowed with DTO", func(t *testing.T) {
		resp, err := http.Get(base + "/api/v1/refresh")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET refresh status = %d, want 405", resp.StatusCode)
		}
		var got ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if got.Error != "method_not_allowed" || got.Message == "" {
			t.Errorf("GET refresh error DTO: %+v", got)
		}
	})

	t.Run("unknown path is 404 with DTO", func(t *testing.T) {
		resp, err := http.Get(base + "/api/v1/nope")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown path status = %d, want 404", resp.StatusCode)
		}
		var got ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if got.Error != "not_found" || got.Message == "" {
			t.Errorf("unknown path error DTO: %+v", got)
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

// TestSnapshotFieldNamesExtendAdditively pins the additive keys: the daemon
// identity block, the per-section as_of stamps, the machine timestamps
// beside every human age, and the operator phase.
func TestSnapshotFieldNamesExtendAdditively(t *testing.T) {
	snap := testSnapshot()
	snap.Runs[0].Phase = "gating"
	snap.RunsAsOf = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	snap.NeedsHumanAsOf = snap.RunsAsOf
	snap.GateRejectionsAsOf = snap.RunsAsOf
	src := &fakeSource{
		snapshot: snap,
		identity: Daemon{PID: 4242, Identity: "darwin:boot:1", Machine: "testbox", Version: "test", ConfigPath: "/cfg/lambe.yaml"},
	}
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
	for _, key := range []string{"daemon", "runs_as_of", "needs_human_as_of", "gate_rejections_as_of"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("snapshot JSON missing additive key %q", key)
		}
	}
	var daemon map[string]json.RawMessage
	if err := json.Unmarshal(raw["daemon"], &daemon); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pid", "identity", "machine", "version", "config_path"} {
		if _, ok := daemon[key]; !ok {
			t.Errorf("daemon JSON missing key %q", key)
		}
	}
	var first map[string]json.RawMessage
	var runsRaw []map[string]json.RawMessage
	if err := json.Unmarshal(raw["runs"], &runsRaw); err != nil {
		t.Fatal(err)
	}
	first = runsRaw[0]
	for _, key := range []string{"phase", "last_event_at"} {
		if _, ok := first[key]; !ok {
			t.Errorf("run JSON missing additive key %q", key)
		}
	}
	var got SnapshotResponse
	rawBytes, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawBytes, &got); err != nil {
		t.Fatal(err)
	}
	if got.Daemon.PID != 4242 || got.Daemon.Machine != "testbox" {
		t.Errorf("daemon block = %+v", got.Daemon)
	}
	if got.Runs[0].Phase != "gating" {
		t.Errorf("phase = %q", got.Runs[0].Phase)
	}
	if _, err := time.Parse(time.RFC3339, got.Runs[0].LastEventAt); err != nil {
		t.Errorf("last_event_at = %q, want RFC3339", got.Runs[0].LastEventAt)
	}
}

// TestQueueRoute pins the queue read: rows carry id, title, reason,
// blocked, and blockers; the as_of is the source's read stamp; the daemon
// block rides along; an empty source yields an empty list, not an error;
// and POST is refused with the typed envelope.
func TestQueueRoute(t *testing.T) {
	src := &fakeSource{queue: Queue{
		AsOf: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
		Rows: []QueueRow{
			{ID: "Q-1", Title: "ready task", Reason: "no blockers"},
			{
				ID: "Q-2", Title: "blocked task", Reason: "waiting on api", Blocked: true,
				Blockers: []QueueBlocker{{ID: "api-9", Title: "api migration"}},
			},
		},
	}}
	srv := New(src, mustListen(t))
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	go func() { _ = srv.Start() }()
	base := "http://" + srv.Addr().String()

	var got QueueResponse
	if err := getJSON(t, base+"/api/v1/queue", &got); err != nil {
		t.Fatalf("GET queue: %v", err)
	}
	if got.SchemaVersion != 1 || got.AsOf != "2026-11-15T12:00:00Z" {
		t.Errorf("queue envelope = %+v", got)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %+v", got.Rows)
	}
	if got.Rows[0].ID != "Q-1" || got.Rows[0].Title != "ready task" || got.Rows[0].Blocked {
		t.Errorf("ready row = %+v", got.Rows[0])
	}
	blocked := got.Rows[1]
	if !blocked.Blocked || blocked.Reason != "waiting on api" || len(blocked.Blockers) != 1 || blocked.Blockers[0].ID != "api-9" || blocked.Blockers[0].Title != "api migration" {
		t.Errorf("blocked row = %+v", blocked)
	}

	resp, err := http.Post(base+"/api/v1/queue", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST queue status = %d, want 405", resp.StatusCode)
	}
	var env ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error != "method_not_allowed" {
		t.Errorf("POST queue envelope = %+v", env)
	}

	empty := New(&fakeSource{}, mustListen(t))
	t.Cleanup(func() { _ = empty.Shutdown(context.Background()) })
	go func() { _ = empty.Start() }()
	var emptyResp QueueResponse
	if err := getJSON(t, "http://"+empty.Addr().String()+"/api/v1/queue", &emptyResp); err != nil {
		t.Fatalf("GET empty queue: %v", err)
	}
	if emptyResp.Rows == nil || len(emptyResp.Rows) != 0 {
		t.Errorf("empty queue rows = %+v, want an empty list", emptyResp.Rows)
	}
}

// TestHistoryRoute pins the history read: newest-first entries carrying the
// operator phase, attempt, and updated_at; the as_of stamp and daemon
// block; and POST refused with the typed envelope.
func TestHistoryRoute(t *testing.T) {
	newest := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{history: History{
		AsOf: newest,
		Runs: []HistoryRun{
			{Issue: "H-2", Phase: "stopped: gate red", Attempt: 3, UpdatedAt: newest},
			{Issue: "H-1", Phase: "interrupted", Attempt: 1, UpdatedAt: newest.Add(-time.Hour)},
		},
	}}
	srv := New(src, mustListen(t))
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	go func() { _ = srv.Start() }()

	var got HistoryResponse
	if err := getJSON(t, "http://"+srv.Addr().String()+"/api/v1/history", &got); err != nil {
		t.Fatalf("GET history: %v", err)
	}
	if got.SchemaVersion != 1 || got.AsOf != "2026-11-15T12:00:00Z" {
		t.Errorf("history envelope = %+v", got)
	}
	if len(got.Runs) != 2 || got.Runs[0].Issue != "H-2" || got.Runs[1].Issue != "H-1" {
		t.Fatalf("runs = %+v, want newest first", got.Runs)
	}
	if got.Runs[1].Phase != "interrupted" || got.Runs[1].UpdatedAt != "2026-11-15T11:00:00Z" {
		t.Errorf("archived row = %+v", got.Runs[1])
	}
	resp, err := http.Post("http://"+srv.Addr().String()+"/api/v1/history", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST history status = %d, want 405", resp.StatusCode)
	}
}

// TestIssueDetailFields pins the enriched run detail: phase, turn, thread,
// workspace, workflow, wake-at, gate preview, and the bounded tail decode
// additively beside the §13.7 core.
func TestIssueDetailFields(t *testing.T) {
	wake := time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC)
	src := &fakeSource{
		snapshot: testSnapshot(),
		issueID:  "T-1",
		issueOK:  true,
		issue: IssueState{
			State: "running", Attempt: 2, Session: "ses-abc",
			LastEvent: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
			Phase:     "gating", Turn: 4, Thread: "thread-7",
			Workspace: "/workspaces/T-1", Workflow: "general",
			WakeAt: wake, LastGate: "pr checks pending",
			Tail: "recent output",
		},
	}
	ts := httptest.NewServer(handlerFor(src))
	defer ts.Close()

	var got IssueResponse
	if err := getJSON(t, ts.URL+"/api/v1/T-1", &got); err != nil {
		t.Fatalf("GET issue: %v", err)
	}
	if got.Phase != "gating" || got.Turn != 4 || got.Thread != "thread-7" {
		t.Errorf("projection fields = %+v", got)
	}
	if got.Workspace != "/workspaces/T-1" || got.Workflow != "general" || got.WakeAt != "2026-11-15T13:00:00Z" {
		t.Errorf("workspace/workflow/wake = %+v", got)
	}
	if got.LastGate != "pr checks pending" || got.Tail != "recent output" || got.TailTruncated {
		t.Errorf("gate/tail = %+v", got)
	}
	if _, err := time.Parse(time.RFC3339, got.LastEventAt); err != nil {
		t.Errorf("last_event_at = %q, want RFC3339", got.LastEventAt)
	}

	// The bounded tail: more than 4KB is cut to the bound, on a rune
	// boundary, with the flag set.
	src.issue.Tail = strings.Repeat("出", 3000)
	resp, err := http.Get(ts.URL + "/api/v1/T-1") // #nosec G107 -- test-local server URL
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var bounded IssueResponse
	if err := json.NewDecoder(resp.Body).Decode(&bounded); err != nil {
		t.Fatal(err)
	}
	if len(bounded.Tail) > maxTailBytes {
		t.Errorf("tail = %d bytes, want at most %d", len(bounded.Tail), maxTailBytes)
	}
	if !bounded.TailTruncated {
		t.Errorf("tail_truncated not set for an over-bound tail")
	}
	if !strings.HasPrefix(bounded.Tail, "出") {
		t.Errorf("tail cut mid-rune: %q", bounded.Tail[:8])
	}
}

// TestResponsesCarryDaemonIdentity pins R9: every data response names the
// daemon serving it.
func TestResponsesCarryDaemonIdentity(t *testing.T) {
	ident := Daemon{PID: 7, Machine: "testbox", Version: "v1"}
	src := &fakeSource{
		snapshot: testSnapshot(), identity: ident,
		issue: IssueState{State: "running", Attempt: 1}, issueID: "T-1", issueOK: true,
	}
	srv := New(src, mustListen(t))
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	go func() { _ = srv.Start() }()
	base := "http://" + srv.Addr().String()

	var snap SnapshotResponse
	if err := getJSON(t, base+"/api/v1/state", &snap); err != nil {
		t.Fatalf("GET state: %v", err)
	}
	if snap.Daemon.PID != 7 || snap.Daemon.Machine != "testbox" || snap.Daemon.Version != "v1" {
		t.Errorf("state daemon block = %+v", snap.Daemon)
	}
	var issue IssueResponse
	if err := getJSON(t, base+"/api/v1/T-1", &issue); err != nil {
		t.Fatalf("GET issue: %v", err)
	}
	if issue.Daemon != snap.Daemon {
		t.Errorf("issue daemon block = %+v", issue.Daemon)
	}
	var refresh RefreshResponse
	if err := postJSON(t, base+"/api/v1/refresh", &refresh); err != nil {
		t.Fatalf("POST refresh: %v", err)
	}
	if !refresh.Accepted || refresh.Daemon != snap.Daemon {
		t.Errorf("refresh daemon block = %+v", refresh.Daemon)
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

// TestListenBindsConfiguredAddr pins the explicit-address binding the
// daemon uses: a host:port string binds exactly that address, an ephemeral
// port included, and a malformed address fails loudly.
func TestListenBindsConfiguredAddr(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen ephemeral: %v", err)
	}
	ta, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !ta.IP.IsLoopback() || ta.Port == 0 {
		_ = ln.Close()
		t.Fatalf("bound addr = %v, want loopback with an ephemeral port", ln.Addr())
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	fixed, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixed.Close() })
	port := fixed.Addr().(*net.TCPAddr).Port
	ln2, err := Listen(fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil {
		_ = ln2.Close()
		t.Fatalf("Listen rebind of an occupied port succeeded")
	}
	if _, err := Listen("bogus"); err == nil {
		t.Fatal("Listen accepted an address without a port")
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

// postJSON issues a POST expecting 202 and decodes the body.
func postJSON(t *testing.T, url string, into any) error {
	t.Helper()
	resp, err := http.Post(url, "application/json", nil) // #nosec G107 -- test-local server URL
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
