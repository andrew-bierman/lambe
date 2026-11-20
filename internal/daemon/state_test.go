package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/mcp"
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
	cache.Store(Report{
		NeedsHuman: []NeedsHumanItem{{Issue: "stuck", LastGateOutput: "tests failed"}},
		GateRejections: []GateRejection{
			{Issue: "run-1", Attempt: 2, Output: "ci red"},
		},
	})

	src := NewStateSource(reg, cache, nil)
	snap := src.Snapshot()

	if len(snap.Runs) != 2 {
		t.Fatalf("runs = %+v, want two", snap.Runs)
	}
	if snap.Runs[0].Issue != "run-1" || snap.Runs[0].State != "running" || snap.Runs[0].Attempt != 1 {
		t.Errorf("first run = %+v", snap.Runs[0])
	}
	if snap.Runs[1].State != "deferred" {
		t.Errorf("second run state = %q, want deferred", snap.Runs[1].State)
	}
	if !snap.WakeAt.Equal(wake) {
		t.Errorf("wake = %s, want %s", snap.WakeAt, wake)
	}
	if len(snap.NeedsHuman) != 1 || snap.NeedsHuman[0].Issue != "stuck" {
		t.Errorf("needs human = %+v", snap.NeedsHuman)
	}
	if len(snap.GateRejections) != 1 || snap.GateRejections[0].Reason != "ci red" {
		t.Errorf("gate rejections = %+v", snap.GateRejections)
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
