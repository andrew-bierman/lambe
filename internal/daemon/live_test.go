package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/andrew-bierman/lambe/internal/harness"
	"github.com/andrew-bierman/lambe/internal/mcp"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

func TestLiveProjectionSharesRealProgressAndOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live", "state.json")
	v := newLiveView(path, func(err error) { t.Errorf("write projection: %v", err) })
	v.registry = NewRunRegistry(nil, nil)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() {
			id := fmt.Sprintf("issue-%d", i)
			v.progress(symphony.Progress{IssueID: id, Title: "task", Phase: "running", Attempt: 2, Turn: 3, Thread: "native"})
			v.event(id, harness.ToolExecutionStartedEvent{ToolName: "read"})
			v.event(id, harness.MessageCompletedEvent{Role: "assistant", Text: strings.Repeat("output", 1000)})
		})
	}
	wg.Wait()
	v.progress(symphony.Progress{IssueID: "issue-0", Phase: "stopped: gate green", Attempt: 2, Turn: 3})
	if err := v.save(); err != nil {
		t.Fatal(err)
	}
	snapshot, alive, err := ReadLive(path)
	if err != nil || !alive || len(snapshot.Runs) != 10 {
		t.Fatalf("invalid live snapshot: %+v alive=%t err=%v", snapshot, alive, err)
	}
	for _, r := range snapshot.Runs {
		if len(r.Tail) > maxRunTail || !strings.Contains(r.Tail, "output") {
			t.Fatalf("missing or unbounded output: %+v", r)
		}
	}
	run, ok := v.registry.Lookup("issue-0")
	if !ok || run.State != mcp.StateSettled || run.Attempt != 2 {
		t.Fatalf("API and terminal disagree: %+v", run)
	}
	v.snapshot.Identity = "different-boot"
	if err := v.save(); err != nil {
		t.Fatal(err)
	}
	if _, alive, err := ReadLive(path); err != nil || alive {
		t.Fatalf("stale daemon reported alive: %t %v", alive, err)
	}
}

func TestLiveHistoryPreservesOwnedWorkAndBoundsRegistry(t *testing.T) {
	v := newLiveView(filepath.Join(t.TempDir(), "state.json"), nil)
	v.registry = NewRunRegistry(nil, nil)
	v.registry.Register("long-running", "issues", "task", "", 0)
	v.progress(symphony.Progress{IssueID: "long-running", Phase: "running"})
	for i := range 110 {
		v.progress(symphony.Progress{IssueID: fmt.Sprintf("done-%d", i), Phase: "stopped: gate green"})
	}
	if len(v.snapshot.Runs) != 100 || len(v.registry.List()) != 100 {
		t.Fatal("terminal history grew without bound")
	}
	if r, ok := v.registry.Lookup("long-running"); !ok || r.State != mcp.StateRunning {
		t.Fatal("evicted owned long-running work")
	}
	ctl := &ControlAdapter{Registry: v.registry}
	handle, err := mcp.ParseRunHandle("long-running")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.CancelRun(t.Context(), handle); err == nil {
		t.Fatal("reported cancellation without stopping the supervisor")
	}
	if r, _ := v.registry.Lookup("long-running"); r.State != mcp.StateRunning {
		t.Fatal("unsupported cancel mutated state")
	}
}

func TestLiveDriverEventsNeverWriteTheSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	v := newLiveView(path, nil)
	v.event("issue", harness.MessageCompletedEvent{Role: "assistant", Text: "finished"})
	before, _, err := ReadLive(path)
	if err != nil || len(before.Runs) != 0 {
		t.Fatalf("driver drain performed disk writes: %+v %v", before, err)
	}
	v.start()
	v.close()
	after, _, err := ReadLive(path)
	if err != nil || len(after.Runs) != 1 || after.Runs[0].Tail != "finished\n" {
		t.Fatalf("writer lost final progress: %+v %v", after, err)
	}
}

func TestLiveHistoryKeepsAttemptsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.json")
	view := newLiveView(path, nil)
	view.progress(symphony.Progress{IssueID: "run", Phase: "retry", Attempt: 1})
	view.event("run", harness.MessageCompletedEvent{Role: "assistant", Text: "first attempt"})
	view.progress(symphony.Progress{IssueID: "run", Phase: "running", Attempt: 2})
	view.event("run", harness.MessageCompletedEvent{Role: "assistant", Text: "second attempt"})
	view.progress(symphony.Progress{IssueID: "run", Phase: "stopped: gate green", Attempt: 2})
	if err := view.save(); err != nil {
		t.Fatal(err)
	}

	newLiveView(path, nil)
	snapshot, alive, err := ReadLive(path)
	if err != nil || !alive || len(snapshot.Runs) != 0 || len(snapshot.History) != 2 {
		t.Fatalf("expected two historical attempts and no active runs: %+v, alive=%t, err=%v", snapshot, alive, err)
	}
	for i, want := range []string{"second attempt\n", "first attempt\n"} {
		if attempt := snapshot.History[i]; attempt.Attempt != 2-i || attempt.Tail != want {
			t.Fatalf("unexpected historical attempt: %+v", attempt)
		}
	}
}

// TestLiveSnapshotExposesLoopTickAndAgentActivity reproduces the "status lies
// when wedged" gap: the projection only changed on run text, so a wedged loop
// or a run whose agent emits only non-text events looked identical to a
// healthy one. TickAt is the loop heartbeat; LastActivityAt is per-run agent
// activity (any event, not just assistant text).
func TestLiveSnapshotExposesLoopTickAndAgentActivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	v := newLiveView(path, nil)
	v.progress(symphony.Progress{IssueID: "a", Phase: "running", Attempt: 1})
	v.event("a", harness.UnknownEvent{Type: "heartbeat"})
	v.tick()
	if err := v.save(); err != nil {
		t.Fatal(err)
	}
	snap, _, err := ReadLive(path)
	if err != nil {
		t.Fatal(err)
	}
	if snap.TickAt.IsZero() {
		t.Error("snapshot has no loop heartbeat (TickAt)")
	}
	if len(snap.Runs) != 1 || snap.Runs[0].LastActivityAt.IsZero() {
		t.Errorf("run has no LastActivityAt after a non-text event: %+v", snap.Runs)
	}
}

func TestNeedsAttention(t *testing.T) {
	for phase, want := range map[string]string{
		"retry": "retrying", "deferred": "waiting to retry", "interrupted": "interrupted",
		"stopped: gate escalation": "gate escalation", "stopped: gate red": "gate failed",
		"running": "", "gating": "", "queued": "", "stopped: gate green": "", "": "",
	} {
		reason, ok := NeedsAttention(LiveRun{Progress: symphony.Progress{Phase: phase}})
		if reason != want || ok != (want != "") {
			t.Errorf("%q: got %q, %v want %q", phase, reason, ok, want)
		}
	}
}
