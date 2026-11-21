package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/httpstate"
	"github.com/andrew-bierman/lambe/internal/mcp"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestStateSourceSnapshot(t *testing.T) {
	t.Parallel()
	reg := NewRunRegistry(nil, nil)
	reg.Register("run-1", "issues", "p", "/w", 1)
	reg.MarkRunning("run-1")
	wake := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	reg.Register("later-2", "issues", "p", "/w", 1)
	reg.MarkDeferred("later-2", wake)

	cache := &ReportCache{}
	since := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	cache.Store(Report{
		NeedsHuman: []NeedsHumanItem{{Issue: "stuck", Reason: "completion check failed", LastGateOutput: "tests failed", Since: since}},
	})

	src := NewStateSource(reg, cache, nil)
	src.Identify("testbox", "test-version", "/cfg/lambe.yaml")
	redAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	live := LiveSnapshot{
		PID: os.Getpid(),
		Runs: []LiveRun{
			{Progress: symphony.Progress{IssueID: "run-1", Phase: "gating", Turn: 4, Thread: "lambe-run-1"}, UpdatedAt: time.Now()},
			{Progress: symphony.Progress{IssueID: "gate-red", Phase: "stopped: gate red", LastGate: "pr checks failed", Attempt: 2}, UpdatedAt: redAt},
		},
	}
	src.WatchLive(writeProjectionFile(t, live))

	snap := src.Snapshot()

	if len(snap.Runs) != 2 {
		t.Fatalf("runs = %+v, want two", snap.Runs)
	}
	if snap.Runs[0].Issue != "run-1" || snap.Runs[0].State != "running" || snap.Runs[0].Attempt != 1 {
		t.Errorf("first run = %+v", snap.Runs[0])
	}
	// The operator phase rides alongside the coarse state, from the
	// projection's own record.
	if snap.Runs[0].Phase != "gating" {
		t.Errorf("first run phase = %q, want gating", snap.Runs[0].Phase)
	}
	if snap.Runs[1].State != "deferred" {
		t.Errorf("second run state = %q, want deferred", snap.Runs[1].State)
	}
	if !snap.WakeAt.Equal(wake) {
		t.Errorf("wake = %s, want %s", snap.WakeAt, wake)
	}
	if len(snap.NeedsHuman) != 1 {
		t.Fatalf("needs human = %+v, want one", snap.NeedsHuman)
	}
	// The real recorded reason and its record time are served, never a
	// hardcoded placeholder.
	if snap.NeedsHuman[0].Issue != "stuck" || snap.NeedsHuman[0].Reason != "completion check failed" {
		t.Errorf("needs human = %+v", snap.NeedsHuman[0])
	}
	if !snap.NeedsHuman[0].Since.Equal(since) {
		t.Errorf("needs human since = %s, want %s", snap.NeedsHuman[0].Since, since)
	}
	// Gate rejections come from the projection's own records: the reason is
	// the last gate output, the time is the recorded stop moment, and no
	// gate kind is invented for a record that does not carry one.
	if len(snap.GateRejections) != 1 {
		t.Fatalf("gate rejections = %+v, want one from the projection", snap.GateRejections)
	}
	gr := snap.GateRejections[0]
	if gr.Issue != "gate-red" || gr.Reason != "pr checks failed" || gr.Gate != "" {
		t.Errorf("gate rejection = %+v, want reason from the record and no fabricated kind", gr)
	}
	if !gr.At.Equal(redAt) {
		t.Errorf("gate rejection at = %s, want the recorded stop time %s", gr.At, redAt)
	}
	if snap.NeedsHumanAsOf.IsZero() || snap.RunsAsOf.IsZero() {
		t.Errorf("section as_of stamps missing: %+v", snap)
	}
	id := src.Identity()
	if id.PID != os.Getpid() || id.Machine != "testbox" || id.Version != "test-version" || id.ConfigPath != "/cfg/lambe.yaml" {
		t.Errorf("identity = %+v", id)
	}
}

// TestStateSourceGateRejectionsOnlyFromRecords pins the truthfulness rule:
// a report cache that claims rejections the projection does not confirm is
// ignored, phases that are not gate rejections contribute nothing, and the
// oldest-first order holds.
func TestStateSourceGateRejectionsOnlyFromRecords(t *testing.T) {
	t.Parallel()
	cache := &ReportCache{}
	cache.Store(Report{GateRejections: []GateRejection{{Issue: "ghost", Output: "never served"}}})
	old := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	newer := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	live := LiveSnapshot{
		PID: os.Getpid(),
		Runs: []LiveRun{
			{Progress: symphony.Progress{IssueID: "running", Phase: "running"}, UpdatedAt: newer},
			{Progress: symphony.Progress{IssueID: "new-red", Phase: "stopped: gate escalation", LastGate: "infra down", Attempt: 8}, UpdatedAt: newer},
		},
		History: []LiveRun{
			{Progress: symphony.Progress{IssueID: "old-red", Phase: "stopped: gate red", LastGate: "lint red", Attempt: 1}, UpdatedAt: old},
			{Progress: symphony.Progress{IssueID: "archived-green", Phase: "stopped: gate green", Attempt: 1}, UpdatedAt: old},
		},
	}
	src := NewStateSource(NewRunRegistry(nil, nil), cache, nil)
	src.WatchLive(writeProjectionFile(t, live))

	snap := src.Snapshot()
	if len(snap.GateRejections) != 2 {
		t.Fatalf("gate rejections = %+v, want the two gate-stopped records", snap.GateRejections)
	}
	if snap.GateRejections[0].Issue != "old-red" || snap.GateRejections[1].Issue != "new-red" {
		t.Errorf("gate rejection order = %+v, want oldest first", snap.GateRejections)
	}
	for _, gr := range snap.GateRejections {
		if gr.Gate != "" {
			t.Errorf("gate rejection %s fabricates kind %q", gr.Issue, gr.Gate)
		}
	}
	if snap.GateRejectionsAsOf.IsZero() {
		t.Errorf("gate rejection as_of missing")
	}
}

func TestStateSourceIssue(t *testing.T) {
	t.Parallel()
	reg := NewRunRegistry(nil, nil)
	reg.Register("live-1", "issues", "p", "/w", 1)
	reg.MarkRunning("live-1")
	reg.SetSession("live-1", "sess-7")

	cache := &ReportCache{}
	cache.Store(Report{Active: []ActiveRun{{Issue: "cached-1", Session: "sess-1", Attempt: 3, LastFrameAge: time.Minute}}})

	src := NewStateSource(reg, cache, nil)

	st, ok := src.Issue("live-1")
	if !ok || st.State != "running" || st.Session != "sess-7" {
		t.Errorf("live issue = %+v ok=%v", st, ok)
	}

	st, ok = src.Issue("cached-1")
	if !ok || st.State != "running" || st.Attempt != 3 {
		t.Errorf("cached issue = %+v ok=%v", st, ok)
	}

	if _, ok := src.Issue("unknown"); ok {
		t.Error("unknown issue must report not-found")
	}
}

// TestStateSourceQueueServesCanonicalReadiness pins the queue seam: rows
// come from the cached canonical tracker read, blockers included, and the
// as_of is that read's stamp. Repeated calls must not add work.
func TestStateSourceQueueServesCanonicalReadiness(t *testing.T) {
	t.Parallel()
	cache := &ReportCache{}
	cache.Store(Report{Queue: []tracker.ReadinessRow{
		{ID: "ready-1", Title: "first ready", Reason: "no blockers"},
		{
			ID: "blocked-1", Title: "blocked", Reason: "waiting on api", Blocked: true,
			Blockers: []struct {
				ID, Title string
			}{{ID: "api-9", Title: "api migration"}},
		},
	}})

	src := NewStateSource(NewRunRegistry(nil, nil), cache, nil)
	for range 2 {
		queue := src.Queue()
		if len(queue.Rows) != 2 {
			t.Fatalf("queue rows = %+v, want two", queue.Rows)
		}
		if queue.Rows[0].ID != "ready-1" || queue.Rows[0].Blocked {
			t.Errorf("ready row = %+v", queue.Rows[0])
		}
		blocked := queue.Rows[1]
		if !blocked.Blocked || blocked.Reason != "waiting on api" {
			t.Errorf("blocked row = %+v", blocked)
		}
		if len(blocked.Blockers) != 1 || blocked.Blockers[0].ID != "api-9" || blocked.Blockers[0].Title != "api migration" {
			t.Errorf("blockers = %+v", blocked.Blockers)
		}
		_, at := cache.Load()
		if !queue.AsOf.Equal(at) {
			t.Errorf("queue as_of = %s, want the cache read stamp %s", queue.AsOf, at)
		}
	}
}

// TestStateSourceQueueEmptyBeforeFirstTick pins the empty shape: no cached
// read yet means no rows and no as_of, not an error.
func TestStateSourceQueueEmptyBeforeFirstTick(t *testing.T) {
	t.Parallel()
	src := NewStateSource(NewRunRegistry(nil, nil), &ReportCache{}, nil)
	queue := src.Queue()
	if len(queue.Rows) != 0 || !queue.AsOf.IsZero() {
		t.Errorf("queue = %+v, want empty rows and zero as_of", queue)
	}
}

// TestStateSourceHistoryMergesTerminalAndArchived pins the merge: terminal
// live rows and restart-archived attempts (interrupted) combine newest
// first, deduped by issue and attempt.
func TestStateSourceHistoryMergesTerminalAndArchived(t *testing.T) {
	t.Parallel()
	reg := NewRunRegistry(nil, nil)
	reg.Register("mcp-done", "issues", "p", "/w", 1)
	reg.MarkFailed("mcp-done", "attempt cap")
	reg.Register("still-running", "issues", "p", "/w", 1)
	reg.MarkRunning("still-running")

	old := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	recent := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	live := LiveSnapshot{
		PID: os.Getpid(),
		Runs: []LiveRun{
			{Progress: symphony.Progress{IssueID: "just-stopped", Phase: "stopped: gate red", Attempt: 3}, UpdatedAt: recent},
			{Progress: symphony.Progress{IssueID: "still-running", Phase: "running", Attempt: 2}, UpdatedAt: recent},
		},
		History: []LiveRun{
			{Progress: symphony.Progress{IssueID: "old-green", Phase: "stopped: gate green", Attempt: 1}, UpdatedAt: old},
		},
	}
	src := NewStateSource(reg, &ReportCache{}, nil)
	src.WatchLive(writeProjectionFile(t, live))

	history := src.History()
	if len(history.Runs) != 3 {
		t.Fatalf("history = %+v, want the three terminal rows", history.Runs)
	}
	// Newest first: the registry failure (just marked), then the
	// just-stopped projection row, then the archived green attempt.
	wantOrder := []string{"mcp-done", "just-stopped", "old-green"}
	for i, want := range wantOrder {
		if history.Runs[i].Issue != want {
			t.Errorf("history[%d] = %s, want %s", i, history.Runs[i].Issue, want)
		}
	}
	if history.Runs[1].Phase != "stopped: gate red" || history.Runs[1].Attempt != 3 {
		t.Errorf("stopped row = %+v", history.Runs[1])
	}
	if history.Runs[0].State != "failed" || history.Runs[0].Outcome != "attempt cap" {
		t.Errorf("registry row = %+v, want state failed and its outcome", history.Runs[0])
	}
	if history.Runs[2].Phase != "stopped: gate green" {
		t.Errorf("archived row = %+v", history.Runs[2])
	}
	// Live rows that are not terminal never enter history.
	for _, run := range history.Runs {
		if run.Issue == "still-running" {
			t.Errorf("live running row leaked into history")
		}
	}
	if history.AsOf.IsZero() {
		t.Errorf("history as_of missing")
	}
}

// TestStateSourceHistoryCarriesRestartInterruptions proves the restart path
// end to end: a projection written by a previous daemon life, reopened by a
// new one, archives its in-flight runs as interrupted and serves them.
func TestStateSourceHistoryCarriesRestartInterruptions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "live.json")
	previous := newLiveView(path, nil)
	previous.progress(symphony.Progress{IssueID: "cut-off", Phase: "running", Attempt: 2, Turn: 5})
	previous.progress(symphony.Progress{IssueID: "finished-before", Phase: "stopped: gate green", Attempt: 1})
	if err := previous.save(); err != nil {
		t.Fatal(err)
	}

	// The next daemon life archives the in-flight row as interrupted.
	replacement := newLiveView(path, nil)
	if err := replacement.save(); err != nil {
		t.Fatal(err)
	}

	src := NewStateSource(NewRunRegistry(nil, nil), &ReportCache{}, nil)
	src.WatchLive(path)
	history := src.History()
	if len(history.Runs) != 2 {
		t.Fatalf("history = %+v, want the two archived attempts", history.Runs)
	}
	var interrupted bool
	for _, run := range history.Runs {
		if run.Issue == "cut-off" {
			interrupted = run.Phase == "interrupted" && run.Attempt == 2
		}
	}
	if !interrupted {
		t.Errorf("restart-interrupted attempt missing or mislabeled: %+v", history.Runs)
	}
}

// TestStateSourceHistoryBounds pins the 100-entry bound: rows beyond it are
// dropped oldest first.
func TestStateSourceHistoryBounds(t *testing.T) {
	t.Parallel()
	runs := make([]LiveRun, 0, maxHistoryEntries+10)
	base := time.Now().Add(-time.Hour)
	for i := range maxHistoryEntries + 10 {
		runs = append(runs, LiveRun{
			Progress:  symphony.Progress{IssueID: fmt.Sprintf("h%03d", i), Phase: "stopped: gate green", Attempt: 1},
			UpdatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	src := NewStateSource(NewRunRegistry(nil, nil), &ReportCache{}, nil)
	src.WatchLive(writeProjectionFile(t, LiveSnapshot{PID: os.Getpid(), Runs: runs}))

	history := src.History()
	if len(history.Runs) != maxHistoryEntries {
		t.Fatalf("history length = %d, want %d", len(history.Runs), maxHistoryEntries)
	}
	if history.Runs[0].Issue != fmt.Sprintf("h%03d", maxHistoryEntries+9) {
		t.Errorf("newest row = %s, want the last archived one", history.Runs[0].Issue)
	}
	if history.Runs[maxHistoryEntries-1].Issue != fmt.Sprintf("h%03d", 10) {
		t.Errorf("oldest row = %s, want h%03d (the ten oldest dropped)", history.Runs[maxHistoryEntries-1].Issue, 10)
	}
}

// TestStateSourceIssueDetail pins the enriched run detail: phase, turn,
// thread, workspace, workflow, wake-at, last gate preview, and the bounded
// tail come from the registry and the projection; unknown issues stay
// not-found.
func TestStateSourceIssueDetail(t *testing.T) {
	t.Parallel()
	reg := NewRunRegistry(nil, nil)
	reg.Register("detail-1", "issues", "fix it", "/workspace/dir", 1)
	reg.MarkRunning("detail-1")
	reg.SetSession("detail-1", "thread-7")
	reg.AppendTail("detail-1", strings.Repeat("out", 100))
	wake := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	reg.Register("deferred-1", "issues", "p", "/w", 1)
	reg.MarkDeferred("deferred-1", wake)

	live := LiveSnapshot{
		PID: os.Getpid(),
		Runs: []LiveRun{
			{Progress: symphony.Progress{
				IssueID: "detail-1", Phase: "gating", Attempt: 1, Turn: 4, Thread: "thread-7",
				Workspace: "/workspace/dir", Workflow: "general", LastGate: "pr checks pending",
			}, UpdatedAt: time.Now()},
			{Progress: symphony.Progress{IssueID: "deferred-1", Phase: "deferred", WakeAt: wake}, UpdatedAt: time.Now()},
		},
	}
	src := NewStateSource(reg, &ReportCache{}, nil)
	src.WatchLive(writeProjectionFile(t, live))

	st, ok := src.Issue("detail-1")
	if !ok {
		t.Fatal("known issue reported not-found")
	}
	if st.State != "running" || st.Attempt != 1 || st.Session != "thread-7" {
		t.Errorf("core fields = %+v", st)
	}
	if st.Phase != "gating" || st.Turn != 4 || st.Thread != "thread-7" {
		t.Errorf("projection fields = %+v", st)
	}
	if st.Workspace != "/workspace/dir" || st.Workflow != "general" || st.LastGate != "pr checks pending" {
		t.Errorf("workspace/workflow/gate = %+v", st)
	}
	if st.Tail == "" || len(st.Tail) > maxRunTail {
		t.Errorf("tail = %d bytes, want present and within the bound", len(st.Tail))
	}
	st, ok = src.Issue("deferred-1")
	if !ok || !st.WakeAt.Equal(wake) {
		t.Errorf("deferred detail = %+v ok=%t, want wake at %s", st, ok, wake)
	}
	if _, ok := src.Issue("unknown"); ok {
		t.Error("unknown issue must report not-found")
	}
}

// writeProjectionFile writes a live projection file the way the daemon's
// flusher does, stamped with this process's identity so ReadLive sees it
// as current, and returns its path.
func writeProjectionFile(t *testing.T, snap LiveSnapshot) string {
	t.Helper()
	if snap.PID == 0 {
		snap.PID = os.Getpid()
	}
	if snap.Identity == "" {
		snap.Identity, _ = proc.ProcessIdentity(os.Getpid())
	}
	if snap.UpdatedAt.IsZero() {
		snap.UpdatedAt = time.Now()
	}
	path := filepath.Join(t.TempDir(), "live.json")
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStateSourceRefreshPokesWithoutBlocking(t *testing.T) {
	t.Parallel()
	var pokes atomic.Int32
	release := make(chan struct{})
	src := NewStateSource(NewRunRegistry(nil, nil), &ReportCache{}, func() {
		pokes.Add(1)
		<-release
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		src.Refresh()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Refresh must not block on the poke completing")
	}
	deadline := time.Now().Add(time.Second)
	for pokes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// An overlapping refresh collapses to nothing while a poke is running.
	src.Refresh()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := pokes.Load(); got == 1 {
			break
		} else if got > 1 {
			t.Fatalf("pokes = %d, want 1", got)
		}
		time.Sleep(time.Millisecond)
	}
	if got := pokes.Load(); got != 1 {
		t.Errorf("pokes = %d, want 1", got)
	}
	close(release)
}

func TestReportCacheRoundTrip(t *testing.T) {
	t.Parallel()
	var c ReportCache
	if rep, at := c.Load(); len(rep.Active) != 0 || !at.IsZero() {
		t.Errorf("empty cache = %+v at %s", rep, at)
	}
	want := Report{Active: []ActiveRun{{Issue: "x"}}}
	c.Store(want)
	rep, at := c.Load()
	if len(rep.Active) != 1 || rep.Active[0].Issue != "x" || at.IsZero() {
		t.Errorf("loaded = %+v at %s", rep, at)
	}
}

func TestRunRegistryTransitions(t *testing.T) {
	t.Parallel()
	reg := NewRunRegistry(nil, nil)
	reg.Register("r", "issues", "p", "/w", 1)
	reg.SetAttempt("r", 4)
	reg.MarkDeferred("r", time.Now().Add(time.Minute))
	if run, _ := reg.Lookup("r"); run.Attempt != 4 || run.State != mcp.StateDeferred {
		t.Errorf("deferred run = %+v", run)
	}
	reg.MarkFailed("r", "attempt cap")
	if run, _ := reg.Lookup("r"); run.State != mcp.StateFailed || run.Outcome != "attempt cap" || !run.WakeAt.IsZero() {
		t.Errorf("failed run = %+v, want cleared wake", run)
	}
	// Unknown handles are ignored, not panics.
	reg.MarkRunning("missing")
	reg.Touch("missing")
	reg.AppendTail("missing", "x")
	if _, ok := reg.Lookup("missing"); ok {
		t.Error("missing run must not exist")
	}
}

// TestWriteStateAPIDescriptor pins the discovery document: the write is
// atomic (temp + rename), lands at 0600, round-trips every field, and the
// per-instance path derives from the instance lock name so two
// configurations never share one file.
func TestWriteStateAPIDescriptor(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Trackers: []config.Tracker{{Name: "local", Type: "repo", Path: t.TempDir()}}}
	path := StateAPIPath(t.TempDir(), cfg)
	instance := strings.TrimSuffix(InstanceLockName(cfg), ".lock")
	if filepath.Base(path) != "stateapi-"+instance+".json" {
		t.Fatalf("descriptor path = %q, want per-instance stateapi-%s.json", path, instance)
	}

	desc := StateAPIDescriptor{
		SchemaVersion: StateAPIDescriptorSchemaVersion,
		Instance:      instance,
		Addr:          "127.0.0.1:53412",
		BearerToken:   "tok",
		PID:           4242,
		Identity:      "darwin:boot:1:2",
		Version:       "test",
		ConfigPath:    "/cfg/lambe.yaml",
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := WriteStateAPIDescriptor(path, desc); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("descriptor mode = %o, want 600 (the file carries the token)", got)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test-owned temporary path
	if err != nil {
		t.Fatal(err)
	}
	var got StateAPIDescriptor
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != desc {
		t.Errorf("round trip mismatch: %+v vs %+v", got, desc)
	}
	// A republish replaces the file in place, never appends.
	desc.Addr = "127.0.0.1:53413"
	if err := WriteStateAPIDescriptor(path, desc); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	data, err = os.ReadFile(path) // #nosec G304 -- test-owned temporary path
	if err != nil {
		t.Fatal(err)
	}
	var got2 StateAPIDescriptor
	if err := json.Unmarshal(data, &got2); err != nil || got2.Addr != desc.Addr {
		t.Fatalf("republished descriptor = %s, %v", data, err)
	}
	// No temp-file litter survives.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".stateapi-") {
			t.Errorf("temp descriptor %q left behind", e.Name())
		}
	}
}

// TestNewStateAPIToken pins the per-start credential: 32 random bytes,
// base64url without padding, and two draws never collide.
func TestNewStateAPIToken(t *testing.T) {
	t.Parallel()
	a, err := newStateAPIToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	b, err := newStateAPIToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if a == "" || b == "" || a == b {
		t.Fatalf("tokens = %q, %q; want distinct non-empty secrets", a, b)
	}
	if strings.ContainsAny(a, "+/=") {
		t.Errorf("token %q is not URL-safe base64", a)
	}
	if len(a) < 32 {
		t.Errorf("token %q shorter than its 32-byte source", a)
	}
}

func TestStateSourceStalledAndActivityFields(t *testing.T) {
	t.Parallel()
	now := time.Now()
	reg := NewRunRegistry(nil, nil)
	for _, id := range []string{"quiet", "chatty", "waiting", "fresh"} {
		reg.Register(id, "issues", "", "", 1)
	}
	reg.MarkRunning("quiet")
	reg.MarkRunning("chatty")
	reg.MarkRunning("fresh")
	reg.MarkDeferred("waiting", now.Add(time.Hour))
	// Age the registry's own stamp: only the projection's activity time can
	// keep a run from being stalled.
	reg.mu.Lock()
	for _, id := range []string{"quiet", "chatty", "waiting"} {
		reg.runs[id].LastEventAt = now.Add(-30 * time.Minute)
	}
	reg.mu.Unlock()

	old := now.Add(-20 * time.Minute).UTC().Truncate(time.Second)
	recent := now.Add(-10 * time.Second).UTC().Truncate(time.Second)
	tick := now.Add(-3 * time.Second).UTC().Truncate(time.Second)
	live := LiveSnapshot{
		TickAt: tick,
		Runs: []LiveRun{
			{Progress: symphony.Progress{IssueID: "quiet", Phase: "running", Workflow: "review"}, LastActivityAt: old},
			{Progress: symphony.Progress{IssueID: "chatty", Phase: "running", Workflow: "general"}, LastActivityAt: recent},
			{Progress: symphony.Progress{IssueID: "waiting", Phase: "deferred"}, LastActivityAt: old},
		},
	}
	src := NewStateSource(reg, &ReportCache{}, nil)
	src.Identify("studio", "v", "")
	src.SetActor("lambe@studio")
	src.SetStallAfter(func() time.Duration { return 5 * time.Minute })
	src.WatchLive(writeProjectionFile(t, live))

	snap := src.Snapshot()
	if !snap.TickAt.Equal(tick) {
		t.Errorf("tick_at = %s, want %s", snap.TickAt, tick)
	}
	if snap.StallAfter != 5*time.Minute {
		t.Errorf("stall_after = %s, want 5m", snap.StallAfter)
	}
	byIssue := map[string]httpstate.Run{}
	for _, r := range snap.Runs {
		byIssue[r.Issue] = r
	}
	want := map[string]bool{"quiet": true, "chatty": false, "waiting": false, "fresh": false}
	for id, stalled := range want {
		if byIssue[id].Stalled != stalled {
			t.Errorf("%s stalled = %v, want %v", id, byIssue[id].Stalled, stalled)
		}
	}
	if q := byIssue["quiet"]; q.Workflow != "review" || q.Machine != "studio" || q.Actor != "lambe@studio" || !q.LastActivity.Equal(old) {
		t.Errorf("quiet run fields = %+v", q)
	}

	st, ok := src.Issue("quiet")
	if !ok || !st.Stalled || st.StallAfter != 5*time.Minute || st.Actor != "lambe@studio" || st.Machine != "studio" || !st.LastActivity.Equal(old) {
		t.Errorf("issue detail = %+v ok=%v, want stalled with identity and activity", st, ok)
	}
	if st, _ := src.Issue("chatty"); st.Stalled {
		t.Errorf("chatty issue reported stalled: %+v", st)
	}
}
