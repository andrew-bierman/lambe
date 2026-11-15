package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/mcp"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/surface/rex"
)

func TestControlAdapterSpawnRegistersRun(t *testing.T) {
	t.Parallel()
	tr := newFakeTracker()
	reg := NewRunRegistry(nil, nil)
	ctl := &ControlAdapter{Registry: reg, Tracker: tr}

	h, err := ctl.SpawnRun(context.Background(), mcp.RunSpec{
		IssueID: "feat-1", Prompt: "do the thing\nmore context", Depth: 1,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if h.String() != "feat-1" {
		t.Errorf("handle = %q, want issue id", h.String())
	}
	// Fast registration: the run exists as pending, no progress waited on.
	run, ok := reg.Lookup("feat-1")
	if !ok || run.State != mcp.StatePending {
		t.Fatalf("run after spawn = %+v, ok=%v", run, ok)
	}
	if _, ok := tr.issues["feat-1"]; !ok {
		t.Error("spawn must ensure the tracker issue exists")
	}

	st, err := ctl.RunStatus(context.Background(), h)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != mcp.StatePending || st.Attempt != 1 {
		t.Errorf("status = %+v, want pending attempt 1", st)
	}
}

func TestControlAdapterUnknownRun(t *testing.T) {
	t.Parallel()
	ctl := &ControlAdapter{Registry: NewRunRegistry(nil, nil), Tracker: newFakeTracker()}
	h := mcp.NewRunHandle("nope")
	if _, err := ctl.RunStatus(context.Background(), h); err == nil {
		t.Error("RunStatus on unknown run must error")
	}
	if _, err := ctl.RunOutput(context.Background(), h); err == nil {
		t.Error("RunOutput on unknown run must error")
	}
	if _, err := ctl.CancelRun(context.Background(), h); err == nil {
		t.Error("CancelRun on unknown run must error")
	}
}

func TestControlAdapterCancelKillsLiveGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real children")
	}
	tr := newFakeTracker()
	reg := NewRunRegistry(nil, nil)
	ctl := &ControlAdapter{Registry: reg, Tracker: tr}
	h, err := ctl.SpawnRun(context.Background(), mcp.RunSpec{IssueID: "run-1", Prompt: "p"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	child := spawnTestChild(t, "run-1")
	reg.SetPGID("run-1", child.Pgid())
	reg.MarkRunning("run-1")

	res, err := ctl.CancelRun(context.Background(), h)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if res.State != mcp.StateCanceled {
		t.Errorf("cancel state = %s, want canceled", res.State)
	}
	if proc.GroupAlive(child.Pgid()) {
		t.Fatal("cancel must kill the run's live process group")
	}
	_ = child.Wait()

	// Canceling a finished run reports the terminal state untouched.
	res, err = ctl.CancelRun(context.Background(), h)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if res.State != mcp.StateCanceled {
		t.Errorf("second cancel state = %s, want canceled (already terminal)", res.State)
	}
}

func TestControlAdapterListAndTail(t *testing.T) {
	t.Parallel()
	reg := NewRunRegistry(nil, nil)
	ctl := &ControlAdapter{Registry: reg, Tracker: newFakeTracker()}
	if _, err := ctl.SpawnRun(context.Background(), mcp.RunSpec{IssueID: "a", Prompt: "pa"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.SpawnRun(context.Background(), mcp.RunSpec{IssueID: "b", Prompt: "pb"}); err != nil {
		t.Fatal(err)
	}
	reg.MarkRunning("a")
	reg.AppendTail("a", "first line\n")
	reg.AppendTail("a", strings.Repeat("x", maxRunTail+10)+"tail")

	runs, err := ctl.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 2 || runs[0].IssueID != "a" || runs[0].Status.State != mcp.StateRunning {
		t.Fatalf("runs = %+v", runs)
	}

	out, err := ctl.RunOutput(context.Background(), mcp.NewRunHandle("a"))
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if len(out.Tail) != maxRunTail || !strings.HasSuffix(out.Tail, "tail") {
		t.Errorf("tail len = %d, want bounded %d ending in tail", len(out.Tail), maxRunTail)
	}
}

// fakeRexClient records session creations and captures nothing.
type fakeRexClient struct {
	mu       sync.Mutex
	created  []string
	failNext bool
}

func (f *fakeRexClient) CreateSession(_ context.Context, req rex.SessionRequest) (rex.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return rex.Session{}, errors.New("rex down")
	}
	f.created = append(f.created, req.Name)
	return rex.Session{ID: "s-" + req.Name, Name: req.Name}, nil
}

func (f *fakeRexClient) ListSessions(context.Context) ([]rex.Session, error) { return nil, nil }
func (f *fakeRexClient) Capture(_ context.Context, _ string) (string, error) { return "", nil }

// createdNames returns a copy of the created session names.
func (f *fakeRexClient) createdNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}

func TestRunRegistryFeedsRexSurface(t *testing.T) {
	t.Parallel()
	client := &fakeRexClient{}
	var out syncBuffer
	adapter := rex.NewAdapter(rex.Availability{Available: true}, client, &out)
	reg := NewRunRegistry(adapter, nil)

	reg.Register("issue-9", "issues", "fix it", "/tmp/w", 1)
	reg.MarkRunning("issue-9")

	deadline := time.Now().Add(2 * time.Second)
	for len(client.createdNames()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := client.createdNames(); len(got) != 1 || got[0] != "issues" {
		t.Errorf("rex sessions = %v, want one for agent issues", got)
	}
	deadline = time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "\x1b]7501;") && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "\x1b]7501;") {
		t.Errorf("no OSC 7501 badge emitted; output = %q", out.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
