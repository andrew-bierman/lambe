package daemon

import (
	"sync/atomic"
	"testing"
	"time"

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
