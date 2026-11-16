package daemon

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// flushCountingTracker counts FlushWrites calls alongside the fake tracker
// and reports a settable queued-write count.
type flushCountingTracker struct {
	*fakeTracker
	mu       sync.Mutex
	flushes  int
	buffered int
}

func (f *flushCountingTracker) FlushWrites(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	return nil
}

// flushCount returns the number of flushes so far.
func (f *flushCountingTracker) flushCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushes
}

// setBuffered sets the reported queued-write count.
func (f *flushCountingTracker) setBuffered(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buffered = n
}

// BufferedWrites implements WriteCounter.
func (f *flushCountingTracker) BufferedWrites() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buffered
}

// newCycleDaemon builds a Daemon wired for direct cycle tests: fake
// tracker, recording syncer, and a report cache.
func newCycleDaemon(tr *flushCountingTracker, sync *orderedSyncer) *Daemon {
	d := &Daemon{
		asm:     Assembly{Sync: sync, Reports: &ReportCache{}},
		log:     slog.Default(),
		primary: tr,
	}
	d.flusher = tr
	return d
}

func TestRunTicksRunsCyclesUntilCancel(t *testing.T) {
	t.Parallel()
	var cycles atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunTicks(ctx, time.Millisecond, func(context.Context) { cycles.Add(1) })
	}()

	deadline := time.Now().Add(2 * time.Second)
	for cycles.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if cycles.Load() < 2 {
		t.Fatalf("cycles = %d, want at least two", cycles.Load())
	}
}

func TestDaemonCycleFlushesThenPushes(t *testing.T) {
	t.Parallel()
	tr := &flushCountingTracker{fakeTracker: newFakeTracker()}
	sync := &orderedSyncer{}
	d := newCycleDaemon(tr, sync)
	tr.setBuffered(2)

	d.cycle(context.Background(), "tick", false)

	if tr.flushCount() != 1 {
		t.Errorf("flushes = %d, want one", tr.flushCount())
	}
	if len(sync.ops) != 2 || sync.ops[0] != "pull" || sync.ops[1] != "push" {
		t.Errorf("ops = %v, want pull then push", sync.ops)
	}
}

func TestDaemonCycleSkipsIdlePush(t *testing.T) {
	t.Parallel()
	tr := &flushCountingTracker{fakeTracker: newFakeTracker()}
	sync := &orderedSyncer{}
	d := newCycleDaemon(tr, sync)
	tr.setBuffered(0)

	d.cycle(context.Background(), "tick", false)

	if tr.flushCount() != 0 {
		t.Errorf("flushes = %d, want none for a write-free tick", tr.flushCount())
	}
	if len(sync.ops) != 1 || sync.ops[0] != "pull" {
		t.Errorf("ops = %v, want only the pull", sync.ops)
	}
}

func TestDaemonCyclePokePushesUnconditionally(t *testing.T) {
	t.Parallel()
	tr := &flushCountingTracker{fakeTracker: newFakeTracker()}
	sync := &orderedSyncer{}
	d := newCycleDaemon(tr, sync)
	tr.setBuffered(0)

	d.cycle(context.Background(), "poke", true)

	if len(sync.ops) != 2 || sync.ops[0] != "pull" || sync.ops[1] != "push" {
		t.Errorf("ops = %v, want pull then push on poke", sync.ops)
	}
}

// countingDispatch counts dispatch reads so the report refresh cadence is
// observable: each refresh issues three reads.
type countingDispatch struct {
	*fakeTracker
	calls atomic.Int64
}

func (c *countingDispatch) Dispatchable(ctx context.Context, q tracker.DispatchQuery) ([]tracker.Issue, error) {
	c.calls.Add(1)
	return c.fakeTracker.Dispatchable(ctx, q)
}

func TestDaemonCycleReportRefreshCadence(t *testing.T) {
	t.Parallel()
	tr := &countingDispatch{fakeTracker: newFakeTracker()}
	sync := &orderedSyncer{}
	d := &Daemon{
		asm:     Assembly{Sync: sync, Reports: &ReportCache{}},
		log:     slog.Default(),
		primary: tr,
	}

	for range reportRefreshTicks {
		d.cycle(context.Background(), "tick", false)
	}
	if got := tr.calls.Load(); got != 3 {
		t.Fatalf("dispatch reads after first %d ticks = %d, want one refresh (3 reads)", reportRefreshTicks, got)
	}
	d.cycle(context.Background(), "tick", false)
	if got := tr.calls.Load(); got != 6 {
		t.Fatalf("dispatch reads after tick %d = %d, want two refreshes (6 reads)", reportRefreshTicks+1, got)
	}

	// The poke path always refreshes regardless of cadence.
	before := tr.calls.Load()
	d.cycle(context.Background(), "poke", true)
	if got := tr.calls.Load(); got != before+3 {
		t.Fatalf("dispatch reads after poke = %d, want %d (immediate refresh)", got, before+3)
	}
}

// orderedSyncer records pull/push order.
type orderedSyncer struct {
	mu     sync.Mutex
	ops    []string
	pulls  int
	pushes int
}

func (o *orderedSyncer) Pull(context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pulls++
	o.ops = append(o.ops, "pull")
	return nil
}

func (o *orderedSyncer) Push(context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pushes++
	o.ops = append(o.ops, "push")
	return nil
}

// bdBin returns the bd binary path, or "" when unavailable. It honors
// LAMBE_TEST_BD both as a path and as the CI gate value "1" (bd on PATH),
// matching test/faultsuite semantics.
func bdBin() string {
	if v := os.Getenv("LAMBE_TEST_BD"); v != "" && v != "1" {
		return v
	}
	if v := os.Getenv("LAMBE_TEST_BD"); v == "1" {
		if p, err := exec.LookPath("bd"); err == nil {
			return p
		}
	}
	return ""
}

func TestStatusAgainstBdTracker(t *testing.T) {
	bin := bdBin()
	if bin == "" {
		t.Skip("bd binary not found; set LAMBE_TEST_BD=1 with bd on PATH to run")
	}

	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	local, err := tracker.NewLocal(bin, dir, "lambe@testbox")
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	if err := local.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}

	runIssue, err := local.EnsureIssue(ctx, "bd-run-1", tracker.IssueSpec{Title: "run", Priority: 2})
	if err != nil {
		t.Fatalf("ensure run issue: %v", err)
	}
	if err := local.PutAttempt(ctx, tracker.Attempt{SessionID: runIssue.ID, Number: 1}); err != nil {
		t.Fatalf("put attempt: %v", err)
	}
	if err := local.PutAttempt(ctx, tracker.Attempt{SessionID: runIssue.ID, Number: 2, PGID: 1234567}); err != nil {
		t.Fatalf("put attempt: %v", err)
	}
	blocked, err := local.EnsureIssue(ctx, "bd-stuck-1", tracker.IssueSpec{
		Title: "stuck", Priority: 2,
		Metadata: map[string]string{"lambe_last_gate_output": "tests failed\nlog follows"},
	})
	if err != nil {
		t.Fatalf("ensure blocked issue: %v", err)
	}
	_ = blocked

	// The run issue must be in_progress with our assignee to count active.
	if _, err := local.Claim(ctx, runIssue.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Deferred: an open issue whose newest attempt carries a future wake
	// time — the representation the recovery and defer paths persist.
	deferred, err := local.EnsureIssue(ctx, "bd-later-1", tracker.IssueSpec{Title: "later", Priority: 2})
	if err != nil {
		t.Fatalf("ensure deferred issue: %v", err)
	}
	if err := local.PutAttempt(ctx, tracker.Attempt{
		SessionID: deferred.ID, Number: 1,
		WakeAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("put deferred attempt: %v", err)
	}

	rep, err := BuildReport(ctx, local)
	if err != nil {
		t.Fatalf("build report: %v", err)
	}
	if len(rep.Active) != 1 || rep.Active[0].Issue != runIssue.ID {
		t.Errorf("active = %+v, want one run on %s", rep.Active, runIssue.ID)
	}
	if rep.Active[0].Attempt != 2 {
		t.Errorf("active attempt = %d, want 2 (newest attempt record)", rep.Active[0].Attempt)
	}
	if len(rep.Deferred) != 1 || rep.Deferred[0].Issue != deferred.ID {
		t.Errorf("deferred = %+v, want one item on %s", rep.Deferred, deferred.ID)
	}
	// Internal attempt beads are blocked by design and must not surface as
	// needs-human even though the blocked scan sees them.
	for _, nh := range rep.NeedsHuman {
		if strings.Contains(nh.Issue, "-att-") {
			t.Errorf("internal attempt record %q leaked into needs-human", nh.Issue)
		}
	}
	var b strings.Builder
	if err := RenderReport(&b, rep); err != nil {
		t.Fatalf("render report: %v", err)
	}
	out := b.String()
	for _, want := range []string{runIssue.ID, deferred.ID} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q; got:\n%s", want, out)
		}
	}
}

type contextTicker func(context.Context) error

func (f contextTicker) Tick(ctx context.Context) error { return f(ctx) }

func TestDaemonCycleDoesNotImposePollingDeadlineOnAgent(t *testing.T) {
	t.Parallel()
	tr := &flushCountingTracker{fakeTracker: newFakeTracker()}
	d := newCycleDaemon(tr, &orderedSyncer{})
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.loop = contextTicker(func(ctx context.Context) error {
		if deadline, ok := ctx.Deadline(); ok {
			t.Errorf("agent inherited polling deadline %s", deadline)
		}
		cancel()
		if ctx.Err() != context.Canceled {
			t.Error("agent must still observe daemon cancellation")
		}
		return nil
	})
	d.cycle(parent, "tick", false)
}

func TestPokeDoesNotImposeRefreshDeadlineOnAgent(t *testing.T) {
	t.Parallel()
	d, err := New(Options{Config: &config.Config{}, StateDir: t.TempDir(), Machine: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.cancel)
	deadline := make(chan bool, 1)
	d.loop = contextTicker(func(ctx context.Context) error { _, ok := ctx.Deadline(); deadline <- ok; return nil })
	d.poke()
	select {
	case limited := <-deadline:
		if limited {
			t.Fatal("refresh request imposed a deadline on agent execution")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not run")
	}
}
