package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/testchild"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestMain(m *testing.M) {
	testchild.Main(m)
}

// fakeTracker is an in-memory tracker.Tracker fixture. All methods are
// guarded: tests read tracker state from the test goroutine while loops and
// adapters run on their own.
type fakeTracker struct {
	mu         sync.Mutex
	issues     map[string]tracker.Issue
	attempts   map[string][]tracker.Attempt
	heartbeats []string
	// heartbeatErr overrides Heartbeat for matching ids.
	heartbeatErr map[string]error
	// attemptsErr overrides Attempts for matching sessions.
	attemptsErr map[string]error
	// dispatchErr, when set, fails every Dispatchable read.
	dispatchErr error
}

func newFakeTracker() *fakeTracker {
	return &fakeTracker{
		issues:       map[string]tracker.Issue{},
		attempts:     map[string][]tracker.Attempt{},
		heartbeatErr: map[string]error{},
		attemptsErr:  map[string]error{},
	}
}

func (f *fakeTracker) Capabilities(context.Context) (tracker.Capabilities, error) {
	return tracker.Capabilities{}, nil
}

func (f *fakeTracker) Dispatchable(_ context.Context, q tracker.DispatchQuery) ([]tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dispatchErr != nil {
		return nil, f.dispatchErr
	}
	if q.Empty() {
		return nil, nil
	}
	var out []tracker.Issue
	for _, issue := range f.issues {
		if !matchesQuery(issue, q) {
			continue
		}
		out = append(out, issue)
	}
	return out, nil
}

func matchesQuery(issue tracker.Issue, q tracker.DispatchQuery) bool {
	if len(q.IDs) > 0 {
		found := false
		for _, id := range q.IDs {
			if id == issue.ID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(q.Statuses) > 0 {
		found := false
		for _, s := range q.Statuses {
			if s == issue.Status {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for k, v := range q.Metadata {
		if issue.Metadata[k] != v {
			return false
		}
	}
	return true
}

func (f *fakeTracker) Claim(_ context.Context, id string) (tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue := f.issues[id]
	issue.Assignee = "lambe@test"
	issue.Status = "in_progress"
	f.issues[id] = issue
	return issue, nil
}

func (f *fakeTracker) Release(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue := f.issues[id]
	issue.Assignee = ""
	issue.Status = "open"
	f.issues[id] = issue
	return nil
}

func (f *fakeTracker) Heartbeat(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats = append(f.heartbeats, id)
	if err := f.heartbeatErr[id]; err != nil {
		return err
	}
	return nil
}

func (f *fakeTracker) Reclaim(context.Context, time.Duration, []string) error { return nil }

func (f *fakeTracker) Defer(_ context.Context, id string, until time.Time, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[id] = append(f.attempts[id], tracker.Attempt{SessionID: id, Number: len(f.attempts[id]) + 1, WakeAt: until})
	return nil
}

func (f *fakeTracker) Events(context.Context, int64) ([]tracker.Event, error) {
	return nil, tracker.ErrJournalDisabled
}

func (f *fakeTracker) EnsureIssue(_ context.Context, key string, _ tracker.IssueSpec) (tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if issue, ok := f.issues[key]; ok {
		return issue, nil
	}
	issue := tracker.Issue{ID: key, Status: "open"}
	f.issues[key] = issue
	return issue, nil
}

func (f *fakeTracker) PutAttempt(_ context.Context, a tracker.Attempt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.attempts[a.SessionID] {
		if existing.Number == a.Number {
			return nil
		}
	}
	f.attempts[a.SessionID] = append(f.attempts[a.SessionID], a)
	return nil
}

func (f *fakeTracker) Attempts(_ context.Context, sessionID string) ([]tracker.Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.attemptsErr[sessionID]; err != nil {
		return nil, err
	}
	return f.attempts[sessionID], nil
}

// heartbeatLog returns a copy of the recorded heartbeat calls.
func (f *fakeTracker) heartbeatLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.heartbeats...)
}

// staticOrphans serves a fixed orphan list.
type staticOrphans []Orphan

func (s staticOrphans) Orphans(context.Context) ([]Orphan, error) { return s, nil }

// spawnTestChild starts a hanging fake agent and returns its Child.
func spawnTestChild(t *testing.T, session string) *proc.Child {
	t.Helper()
	child, err := proc.Start(proc.Options{
		Path:    os.Args[0],
		Args:    []string{"-test.run=^$"},
		Env:     append(os.Environ(), testchild.EnvVar+"="+testchild.BehaviorHang),
		Session: session,
	})
	if err != nil {
		t.Fatalf("spawn test child: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = proc.TerminateGroup(ctx, child.Pgid(), 2*time.Second)
		_ = child.Wait()
	})
	return child
}

func TestAcquireStateLockContention(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lambe.lock")

	first, err := AcquireStateLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = first.Release() }()

	second, err := AcquireStateLock(path)
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second acquire: want ErrLockHeld, got %v", err)
	}
	if second != nil {
		t.Fatal("second acquire must not return a lock")
	}
	// The first holder is unaffected: its pid hint still reads this pid.
	if got := first.PidHint(); got != os.Getpid() {
		t.Errorf("holder pid hint = %d, want %d", got, os.Getpid())
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	third, err := AcquireStateLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	_ = third.Release()
}

func TestAcquireStateLockForeignPidHintDoesNotBlock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lambe.lock")
	// A stale hint naming some other live pid must not arbitrate: the
	// flock alone decides.
	if err := os.WriteFile(path+".pid", []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireStateLock(path)
	if err != nil {
		t.Fatalf("acquire over foreign pid hint: %v", err)
	}
	if got := lock.PidHint(); got != os.Getpid() {
		t.Errorf("pid hint = %d, want rewritten to %d", got, os.Getpid())
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestRecover(t *testing.T) {
	const actor = "lambe@test"
	newCase := func(t *testing.T) (*fakeTracker, *proc.Child) {
		tr := newFakeTracker()
		child := spawnTestChild(t, "issue-1")
		return tr, child
	}

	t.Run("adopted when lease still holds", func(t *testing.T) {
		tr, child := newCase(t)
		tr.issues["issue-1"] = tracker.Issue{ID: "issue-1", Status: "in_progress", Assignee: actor}
		adopted, err := Recover(context.Background(), staticOrphans{{PGID: child.Pgid(), SessionID: "issue-1"}},
			tr, actor, 5*time.Second, nil)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(adopted) != 1 || adopted[0].SessionID != "issue-1" {
			t.Fatalf("adopted = %+v, want one adoption for issue-1", adopted)
		}
		if !proc.GroupAlive(child.Pgid()) {
			t.Fatal("adopted child must stay alive")
		}
		if len(tr.heartbeatLog()) != 1 {
			t.Errorf("heartbeat calls = %v, want exactly one lease re-verification", tr.heartbeatLog())
		}
	})

	t.Run("stands down without killing when assignee changed", func(t *testing.T) {
		tr, child := newCase(t)
		tr.issues["issue-1"] = tracker.Issue{ID: "issue-1", Status: "in_progress", Assignee: "someone-else"}
		adopted, err := Recover(context.Background(), staticOrphans{{PGID: child.Pgid(), SessionID: "issue-1"}},
			tr, actor, 5*time.Second, nil)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(adopted) != 0 {
			t.Fatalf("adopted = %+v, want none", adopted)
		}
		if !proc.GroupAlive(child.Pgid()) {
			t.Fatal("stand-down must not kill the orphan")
		}
		// No heartbeat attempt against someone else's claim.
		if len(tr.heartbeatLog()) != 0 {
			t.Errorf("heartbeat calls = %v, want none", tr.heartbeatLog())
		}
	})

	t.Run("stands down without killing when claim lost", func(t *testing.T) {
		tr, child := newCase(t)
		tr.issues["issue-1"] = tracker.Issue{ID: "issue-1", Status: "in_progress", Assignee: actor}
		tr.heartbeatErr["issue-1"] = fmt.Errorf("wrap: %w", tracker.ErrNotOwner)
		adopted, err := Recover(context.Background(), staticOrphans{{PGID: child.Pgid(), SessionID: "issue-1"}},
			tr, actor, 5*time.Second, nil)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(adopted) != 0 {
			t.Fatalf("adopted = %+v, want none", adopted)
		}
		if !proc.GroupAlive(child.Pgid()) {
			t.Fatal("claim loss must not kill the orphan")
		}
	})

	t.Run("kills orphan when issue terminal", func(t *testing.T) {
		tr, child := newCase(t)
		tr.issues["issue-1"] = tracker.Issue{ID: "issue-1", Status: "closed", Assignee: actor}
		st, ok := procStartTime(child.Pgid())
		if !ok {
			t.Skip("starttime unreadable on this platform")
		}
		adopted, err := Recover(context.Background(), staticOrphans{{PGID: child.Pgid(), SessionID: "issue-1", StartTime: st}},
			tr, actor, 5*time.Second, nil)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(adopted) != 0 {
			t.Fatalf("adopted = %+v, want none", adopted)
		}
		if proc.GroupAlive(child.Pgid()) {
			t.Fatal("terminal issue orphan must be group-killed")
		}
		_ = child.Wait()
	})

	t.Run("stands down when no starttime was recorded", func(t *testing.T) {
		tr, child := newCase(t)
		tr.issues["issue-1"] = tracker.Issue{ID: "issue-1", Status: "closed", Assignee: actor}
		adopted, err := Recover(context.Background(), staticOrphans{{PGID: child.Pgid(), SessionID: "issue-1"}},
			tr, actor, 5*time.Second, nil)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(adopted) != 0 {
			t.Fatalf("adopted = %+v, want none", adopted)
		}
		if !proc.GroupAlive(child.Pgid()) {
			t.Fatal("unverifiable pgid must never be killed")
		}
	})

	t.Run("stands down when starttime mismatches", func(t *testing.T) {
		tr, child := newCase(t)
		tr.issues["issue-1"] = tracker.Issue{ID: "issue-1", Status: "closed", Assignee: actor}
		st, ok := procStartTime(child.Pgid())
		if !ok {
			t.Skip("starttime unreadable on this platform")
		}
		adopted, err := Recover(context.Background(), staticOrphans{{PGID: child.Pgid(), SessionID: "issue-1", StartTime: st + 1}},
			tr, actor, 5*time.Second, nil)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(adopted) != 0 {
			t.Fatalf("adopted = %+v, want none", adopted)
		}
		if !proc.GroupAlive(child.Pgid()) {
			t.Fatal("a recycled-pid mismatch must never be killed")
		}
	})

	t.Run("never kills when ownership unverified", func(t *testing.T) {
		tr, child := newCase(t)
		tr.dispatchErr = errors.New("bd unreachable")
		adopted, err := Recover(context.Background(), staticOrphans{{PGID: child.Pgid(), SessionID: "issue-1"}},
			tr, actor, 5*time.Second, nil)
		if err != nil {
			t.Fatalf("recover must degrade, got %v", err)
		}
		if len(adopted) != 0 {
			t.Fatalf("adopted = %+v, want none", adopted)
		}
		if !proc.GroupAlive(child.Pgid()) {
			t.Fatal("unverified ownership must never kill")
		}
	})
}

// TestTrackerOrphansSkipsFailingAttemptsRead encodes the recovery
// contract: one session's failing Attempts read is skipped, not fatal —
// the other sessions' orphans are still recovered.
func TestTrackerOrphansSkipsFailingAttemptsRead(t *testing.T) {
	t.Parallel()
	tr := newFakeTracker()
	tr.issues["broken"] = tracker.Issue{ID: "broken", Status: "in_progress", Assignee: "lambe@test"}
	tr.issues["healthy"] = tracker.Issue{ID: "healthy", Status: "in_progress", Assignee: "lambe@test"}
	tr.attemptsErr["broken"] = errors.New("journal unreadable")
	tr.attempts["healthy"] = []tracker.Attempt{{SessionID: "healthy", Number: 1, PGID: 4242}}

	orphans, err := TrackerOrphans{Tracker: tr}.Orphans(context.Background())
	if err != nil {
		t.Fatalf("one failing attempts read must not abort the scan: %v", err)
	}
	if len(orphans) != 1 || orphans[0].SessionID != "healthy" || orphans[0].PGID != 4242 {
		t.Fatalf("orphans = %+v, want only the healthy session's pgid", orphans)
	}
}

// slowSyncer records the peak number of concurrent Pull calls while each
// call blocks briefly.
type slowSyncer struct {
	mu       sync.Mutex
	inFlight int
	peak     int
}

func (s *slowSyncer) Pull(context.Context) error {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	s.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	return nil
}

func (s *slowSyncer) Push(context.Context) error { return nil }

func (s *slowSyncer) peakInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// TestDaemonCycleSerializesTickAndPoke proves the mutual exclusion the
// Symphony loop requires: concurrent cycle invocations (a tick beside a
// poke) never interleave — their pulls run strictly one at a time.
func TestDaemonCycleSerializesTickAndPoke(t *testing.T) {
	t.Parallel()
	syncer := &slowSyncer{}
	d, err := New(Options{
		Config: &config.Config{Supervisor: config.Supervisor{
			TickSeconds:    10,
			SilenceTimeout: time.Minute,
		}},
		StateDir: t.TempDir(),
		Machine:  "testbox",
		Actor:    "lambe@test",
		Wire: func(a *Assembly) error {
			a.Sync = syncer
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(poke bool) {
			defer wg.Done()
			if poke {
				d.cycle(context.Background(), "poke", true)
			} else {
				d.cycle(context.Background(), "tick", false)
			}
		}(i%2 == 0)
	}
	wg.Wait()

	if peak := syncer.peakInFlight(); peak > 1 {
		t.Fatalf("peak concurrent cycle pulls = %d, want 1 (cycles must serialize)", peak)
	}
}

func TestSkipsMachine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		labels  []string
		machine string
		skip    bool
	}{
		{"no labels", nil, "boxy", false},
		{"unrelated label", []string{"needs-review"}, "boxy", false},
		{"own machine", []string{"lambe/machine:boxy"}, "boxy", false},
		{"other machine", []string{"lambe/machine:elsewhere"}, "boxy", true},
		{"other machine among many", []string{"a", "lambe/machine:elsewhere"}, "boxy", true},
		{"prefix substring is not a match", []string{"lambe/machine:boxy2"}, "boxy", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SkipsMachine(tt.labels, tt.machine); got != tt.skip {
				t.Errorf("SkipsMachine(%v, %q) = %v, want %v", tt.labels, tt.machine, got, tt.skip)
			}
		})
	}
}

func TestFilteredTrackerSkipsForeignMachines(t *testing.T) {
	t.Parallel()
	tr := newFakeTracker()
	tr.issues["local-1"] = tracker.Issue{ID: "local-1", Status: "ready"}
	tr.issues["foreign"] = tracker.Issue{ID: "foreign", Status: "ready", Metadata: map[string]string{"labels": "lambe/machine:elsewhere"}}
	tr.issues["pinned-here"] = tracker.Issue{ID: "pinned-here", Status: "ready", Metadata: map[string]string{"labels": "lambe/machine:boxy"}}
	tr.issues["running-elsewhere"] = tracker.Issue{ID: "running-elsewhere", Status: "ready", Metadata: map[string]string{"labels": "bug,lambe/machine:other"}}

	filtered := &FilteredTracker{
		Tracker: tr,
		Machine: "boxy",
		Labels:  MetadataLabelSource{Tracker: tr},
	}
	got, err := filtered.Dispatchable(context.Background(), tracker.DispatchQuery{Statuses: []string{"ready"}})
	if err != nil {
		t.Fatalf("dispatchable: %v", err)
	}
	ids := map[string]bool{}
	for _, issue := range got {
		ids[issue.ID] = true
	}
	if ids["foreign"] || ids["running-elsewhere"] {
		t.Errorf("foreign-machine issues must never be dispatched locally, got %v", ids)
	}
	if !ids["local-1"] || !ids["pinned-here"] {
		t.Errorf("local issues must stay dispatchable, got %v", ids)
	}
}

func TestDaemonShutdownLadderAndLockRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real children")
	}
	stateDir := t.TempDir()
	tr := newFakeTracker()
	tr.issues["issue-1"] = tracker.Issue{ID: "issue-1", Status: "in_progress", Assignee: "lambe@test"}

	d, err := New(Options{
		Config: &config.Config{Supervisor: config.Supervisor{
			TickSeconds:       10,
			AttemptCap:        8,
			SilenceTimeout:    time.Minute,
			TurnTimeout:       2 * time.Minute,
			HeartbeatInterval: time.Hour,
		}},
		StateDir: stateDir,
		Machine:  "testbox",
		Actor:    "lambe@test",
		Wire: func(a *Assembly) error {
			a.Trackers = []tracker.Tracker{tr}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}

	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(context.Background()) }()

	// Wait until the lock is held, then spawn a child through the daemon's
	// registry and terminate the daemon with SIGTERM.
	lockPath := filepath.Join(stateDir, "lambe.lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, err := AcquireStateLock(lockPath)
		if errors.Is(err, ErrLockHeld) {
			break
		}
		// A successful probe must release immediately: a leaked probe
		// lock would block the daemon's own acquire.
		if probe != nil {
			_ = probe.Release()
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon never took the state lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The holder must be the daemon, not a leftover: it is still running.
	select {
	case err := <-runDone:
		t.Fatalf("daemon exited during startup: %v", err)
	default:
	}

	child := spawnTestChild(t, "issue-1")
	d.asm.Children.Add(child)
	time.Sleep(200 * time.Millisecond) // let Run settle into its select loop

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("daemon run: %v", err)
		}
	case <-time.After(ShutdownBound + 5*time.Second):
		t.Fatal("daemon did not exit within the shutdown bound")
	}

	if proc.GroupAlive(child.Pgid()) {
		t.Fatal("SIGTERM must run the kill ladder against every child group")
	}
	_ = child.Wait()

	// The kernel/flock release means a new instance can start immediately.
	relock, err := AcquireStateLock(lockPath)
	if err != nil {
		t.Fatalf("lock must be released after daemon exit: %v", err)
	}
	_ = relock.Release()
}

func TestPushAfterWritesRetriesNonFastForward(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		fake    fakeSyncSequence
		wantOps []string
		wantErr bool
	}{
		{
			name:    "clean push",
			fake:    fakeSyncSequence{pushFails: 0},
			wantOps: []string{"push"},
		},
		{
			name:    "non-fast-forward retries pull then push",
			fake:    fakeSyncSequence{pushFails: 1, nonFF: true},
			wantOps: []string{"push", "pull", "push"},
		},
		{
			name:    "persistent non-fast-forward gives up without forcing",
			fake:    fakeSyncSequence{pushFails: 99, nonFF: true},
			wantOps: []string{"push", "pull", "push"},
			wantErr: true,
		},
		{
			name:    "plain push failure is returned as-is",
			fake:    fakeSyncSequence{pushFails: 1, nonFF: false},
			wantOps: []string{"push"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := PushAfterWrites(context.Background(), &tt.fake)
			if (got != nil) != tt.wantErr {
				t.Errorf("PushAfterWrites error = %v, wantErr %v", got, tt.wantErr)
			}
			if strings.Join(tt.fake.ops, ",") != strings.Join(tt.wantOps, ",") {
				t.Errorf("ops = %v, want %v", tt.fake.ops, tt.wantOps)
			}
		})
	}
}

// fakeSyncSequence records sync operations and fails the first pushFails
// pushes, as non-fast-forward when nonFF is set.
type fakeSyncSequence struct {
	ops       []string
	pushFails int
	nonFF     bool
}

func (f *fakeSyncSequence) Pull(context.Context) error {
	f.ops = append(f.ops, "pull")
	return nil
}

func (f *fakeSyncSequence) Push(context.Context) error {
	f.ops = append(f.ops, "push")
	if f.pushFails > 0 {
		f.pushFails--
		if f.nonFF {
			return fmt.Errorf("bd dolt push: %w", ErrNonFastForward)
		}
		return errors.New("remote unreachable")
	}
	return nil
}

func TestBuildAndRenderReport(t *testing.T) {
	t.Parallel()
	tr := newFakeTracker()
	wake := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	tr.issues["run-1"] = tracker.Issue{ID: "run-1", Status: "in_progress", Assignee: "lambe@test"}
	tr.attempts["run-1"] = []tracker.Attempt{{SessionID: "run-1", Number: 1}, {SessionID: "run-1", Number: 2, PGID: 4242}}
	tr.issues["later-1"] = tracker.Issue{ID: "later-1", Status: "open"}
	tr.attempts["later-1"] = []tracker.Attempt{{SessionID: "later-1", Number: 3, WakeAt: wake}}
	tr.issues["stuck-1"] = tracker.Issue{
		ID:       "stuck-1",
		Status:   "blocked",
		Metadata: map[string]string{"lambe_last_gate_output": "tests failed\nfull log follows"},
	}

	rep, err := BuildReport(context.Background(), tr)
	if err != nil {
		t.Fatalf("build report: %v", err)
	}
	if len(rep.Active) != 1 || rep.Active[0].Attempt != 2 {
		t.Errorf("active runs = %+v, want one run at attempt 2", rep.Active)
	}
	if len(rep.Deferred) != 1 || !rep.Deferred[0].WakeAt.Equal(wake) {
		t.Errorf("deferred = %+v, want one item waking at %s", rep.Deferred, wake)
	}
	if len(rep.NeedsHuman) != 1 || rep.NeedsHuman[0].LastGateOutput != "tests failed" {
		t.Errorf("needs human = %+v, want gate summary 'tests failed'", rep.NeedsHuman)
	}

	var b strings.Builder
	if err := RenderReport(&b, rep); err != nil {
		t.Fatalf("render report: %v", err)
	}
	out := b.String()
	for _, want := range []string{"run-1", "2", wake.Format(time.RFC3339), "stuck-1", "tests failed", "active runs:", "deferred:", "needs human:", "last-night gate rejections:"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q; got:\n%s", want, out)
		}
	}
}

// TestDaemonRunKeepsStartingWhenRecoveryReadFails encodes the degradation
// contract: a recovery read failure is logged and the daemon continues its
// life instead of aborting startup.
func TestDaemonRunKeepsStartingWhenRecoveryReadFails(t *testing.T) {
	stateDir := t.TempDir()
	tr := newFakeTracker()
	tr.dispatchErr = errors.New("bd unreachable")

	d, err := New(Options{
		Config: &config.Config{Supervisor: config.Supervisor{
			TickSeconds:       10,
			SilenceTimeout:    time.Minute,
			TurnTimeout:       2 * time.Minute,
			HeartbeatInterval: time.Hour,
		}},
		StateDir: stateDir,
		Machine:  "testbox",
		Actor:    "lambe@test",
		Wire: func(a *Assembly) error {
			a.Trackers = []tracker.Tracker{tr}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()

	// The lock being held proves the daemon got past the failing recovery
	// read and settled into its run loop.
	lockPath := filepath.Join(stateDir, "lambe.lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, perr := AcquireStateLock(lockPath)
		if errors.Is(perr, ErrLockHeld) {
			break
		}
		if probe != nil {
			_ = probe.Release()
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon never took the state lock after recovery read failure")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("daemon run after recovery failure: %v", err)
		}
	case <-time.After(ShutdownBound + 5*time.Second):
		t.Fatal("daemon did not exit within the shutdown bound")
	}
}

func TestHeartbeatLoopBeatsHeldClaims(t *testing.T) {
	t.Parallel()
	tr := newFakeTracker()
	tr.issues["mine"] = tracker.Issue{ID: "mine", Status: "in_progress", Assignee: "lambe@test"}
	tr.issues["theirs"] = tracker.Issue{ID: "theirs", Status: "in_progress", Assignee: "other"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		HeartbeatLoop(ctx, tr, "lambe@test", func() time.Duration { return 5 * time.Millisecond }, nil)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for len(tr.heartbeatLog()) < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	for _, id := range tr.heartbeatLog() {
		if id != "mine" {
			t.Errorf("heartbeats touched foreign claim %q", id)
		}
	}
	if len(tr.heartbeatLog()) < 3 {
		t.Errorf("heartbeats = %v, want at least three beats on the held claim", tr.heartbeatLog())
	}
}

// slowHeartbeatTracker records the peak number of concurrent Heartbeat
// calls while each call blocks briefly.
type slowHeartbeatTracker struct {
	*fakeTracker
	mu       sync.Mutex
	inFlight int
	peak     int
}

func (s *slowHeartbeatTracker) Heartbeat(ctx context.Context, id string) error {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	s.mu.Unlock()
	time.Sleep(25 * time.Millisecond)
	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	return s.fakeTracker.Heartbeat(ctx, id)
}

func (s *slowHeartbeatTracker) peakInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

func TestHeartbeatLoopBoundsBeatConcurrency(t *testing.T) {
	t.Parallel()
	tr := &slowHeartbeatTracker{fakeTracker: newFakeTracker()}
	const claims = heartbeatConcurrency * 2
	for i := range claims {
		id := fmt.Sprintf("mine-%d", i)
		tr.issues[id] = tracker.Issue{ID: id, Status: "in_progress", Assignee: "lambe@test"}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		HeartbeatLoop(ctx, tr, "lambe@test", func() time.Duration { return 5 * time.Millisecond }, nil)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for len(tr.heartbeatLog()) < claims && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	if got := len(tr.heartbeatLog()); got < claims {
		t.Fatalf("heartbeats = %d, want every one of %d claims beaten", got, claims)
	}
	if peak := tr.peakInFlight(); peak > heartbeatConcurrency {
		t.Errorf("peak concurrent beats = %d, want at most %d", peak, heartbeatConcurrency)
	}
}
