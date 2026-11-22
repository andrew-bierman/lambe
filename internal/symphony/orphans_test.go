package symphony

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// slowLookupTracker delays per-issue reads (the recovery inspection) the way
// a saturated embedded-Dolt hub does, and records their concurrency.
type slowLookupTracker struct {
	*fakeTracker
	delay time.Duration

	mu           sync.Mutex
	inflight     int
	peak         int
	releaseCalls int
}

func (s *slowLookupTracker) Dispatchable(ctx context.Context, q tracker.DispatchQuery) ([]tracker.Issue, error) {
	if len(q.IDs) == 0 {
		return s.fakeTracker.Dispatchable(ctx, q)
	}
	s.mu.Lock()
	s.inflight++
	s.peak = max(s.peak, s.inflight)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.inflight--; s.mu.Unlock() }()
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.fakeTracker.Dispatchable(ctx, q)
}

func (s *slowLookupTracker) Release(ctx context.Context, id string) error {
	s.mu.Lock()
	s.releaseCalls++
	s.mu.Unlock()
	return s.fakeTracker.Release(ctx, id)
}

const orphanCount = 50

func orphanFixture(t *testing.T, delay time.Duration) (*fixture, *slowLookupTracker) {
	t.Helper()
	f := newFixture(t, "work {{ issue }}", continueTurn)
	st := &slowLookupTracker{fakeTracker: f.tr, delay: delay}
	f.loop.Tracker = st
	f.loop.CheckpointDir = filepath.Join(f.dir, "sessions")
	for i := range orphanCount {
		id := fmt.Sprintf("old-%02d", i)
		f.tr.issues[id] = tracker.Issue{ID: id, Status: tracker.StatusInProgress, Assignee: "lambe"}
	}
	return f, st
}

func TestSlowOrphanInspectionDoesNotBlockDispatch(t *testing.T) {
	f, st := orphanFixture(t, 400*time.Millisecond)
	f.loop.RecoveryBudget = time.Second
	f.loop.RecoveryConcurrency = 4

	start := time.Now()
	if err := f.loop.Restore(t.Context(), "lambe"); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Serial inspection would take 50*400ms = 20s; the pass is time-boxed.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("restore+tick took %s, want it bounded by the recovery budget", elapsed)
	}
	if calls := f.runner.recorded(); len(calls) == 0 || calls[0].Issue.ID != "i-1" {
		t.Fatalf("fresh issue was not dispatched while old claims are inspected: %+v", calls)
	}
	if st.peak > 4 {
		t.Fatalf("peak concurrent inspections = %d, want at most 4", st.peak)
	}
	for id, issue := range f.tr.issues {
		if id != "i-1" && (issue.Assignee != "lambe" || issue.Status != tracker.StatusInProgress) {
			t.Fatalf("claim %s was changed by recovery: %+v", id, issue)
		}
	}
	if st.releaseCalls != 0 {
		t.Fatalf("recovery released %d claims", st.releaseCalls)
	}
}

func TestOrphanClaimsLapseAfterTTLButAreNeverReleased(t *testing.T) {
	child := exec.Command("sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL); _ = child.Wait() }()
	identity, err := proc.ProcessIdentity(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		ttl       time.Duration
		attempts  []tracker.Attempt
		wantLapse bool
	}{
		{"ttl disabled keeps heartbeating", 0, nil, false},
		{"ttl not reached", time.Hour, nil, false},
		{"ttl reached, no recorded agent", time.Nanosecond, nil, true},
		{"ttl reached, recorded agent alive", time.Nanosecond, []tracker.Attempt{{Number: 1, PGID: child.Process.Pid, ProcessIdentity: identity}}, false},
		{"ttl reached, recorded agent recycled pid", time.Nanosecond, []tracker.Attempt{{Number: 1, PGID: child.Process.Pid, ProcessIdentity: "other-boot"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, st := orphanFixture(t, 0)
			f.tr.attempts = tt.attempts
			f.loop.OrphanTTL = tt.ttl
			if err := f.loop.Restore(t.Context(), "lambe"); err != nil {
				t.Fatal(err)
			}
			claims := f.loop.Orphans.List()
			if len(claims) != orphanCount {
				t.Fatalf("registered %d orphans, want %d", len(claims), orphanCount)
			}
			for _, c := range claims {
				if c.Lapsed != tt.wantLapse {
					t.Fatalf("%s lapsed=%v, want %v", c.Issue, c.Lapsed, tt.wantLapse)
				}
				if c.WorkspacePresent || c.Owner != "lambe" || c.Reason != ErrNoContinuation.Error() || c.Since.IsZero() {
					t.Fatalf("incomplete orphan record: %+v", c)
				}
			}
			for id, issue := range f.tr.issues {
				if id != "i-1" && issue.Assignee != "lambe" {
					t.Fatalf("claim %s was released", id)
				}
			}
			if st.releaseCalls != 0 {
				t.Fatalf("daemon released %d claims on its own", st.releaseCalls)
			}
		})
	}
}

func TestOrphanClockSurvivesRestart(t *testing.T) {
	f, _ := orphanFixture(t, 0)
	if err := f.loop.Restore(t.Context(), "lambe"); err != nil {
		t.Fatal(err)
	}
	first := f.loop.Orphans.List()[0].Since
	time.Sleep(5 * time.Millisecond)

	g := newFixture(t, "work {{ issue }}")
	g.tr = f.tr
	g.loop.Tracker = f.tr
	g.loop.CheckpointDir = f.loop.CheckpointDir
	if err := g.loop.Restore(t.Context(), "lambe"); err != nil {
		t.Fatal(err)
	}
	got, ok := g.loop.Orphans.Get("old-00")
	if !ok || !got.Since.Equal(first) {
		t.Fatalf("orphan clock reset by restart: first %s, now %+v", first, got)
	}
	scanned, err := ScanOrphanClaims(f.loop.CheckpointDir)
	if err != nil || len(scanned) != orphanCount {
		t.Fatalf("scan: %d claims, err %v", len(scanned), err)
	}
}

func TestClosedIssueDropsForeignCheckpoint(t *testing.T) {
	f := newFixture(t, "work {{ issue }}")
	l := f.loop.Loop
	l.CheckpointDir = filepath.Join(f.dir, "sessions")
	issue := tracker.Issue{ID: "c-1", Status: tracker.StatusClosed}
	f.tr.issues[issue.ID] = issue
	if err := l.writeCheckpoint(checkpoint{Version: 1, Phase: checkpointLegacy, Issue: issue, Workflow: f.loader.Current(), Key: issue.ID}); err != nil {
		t.Fatal(err)
	}
	if err := l.Restore(t.Context(), "lambe"); err != nil {
		t.Fatal(err)
	}
	scanned, _ := ScanOrphanClaims(l.CheckpointDir)
	if len(scanned) != 0 {
		t.Fatalf("closed, unassigned issue kept its checkpoint: %+v", scanned)
	}
}
