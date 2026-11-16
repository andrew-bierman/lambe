package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/harness"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// fakeDriver is a scriptable harness.Driver for wire tests. Events mirrors
// the real drivers' 1024-buffered stream, so a run that emits more events
// than the buffer holds deadlocks unless something drains the stream.
type fakeDriver struct {
	events  chan harness.Event
	startFn func(*fakeDriver) error
	runFn   func(*fakeDriver, string) harness.Outcome

	mu     sync.Mutex
	closes int
	closed bool
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{events: make(chan harness.Event, 1024)}
}

func (d *fakeDriver) Capabilities() harness.Capabilities { return harness.Capabilities{} }

func (d *fakeDriver) Start(_ context.Context) error {
	if d.startFn != nil {
		return d.startFn(d)
	}
	return nil
}

func (d *fakeDriver) Events() <-chan harness.Event { return d.events }

func (d *fakeDriver) Prompt(context.Context, string) error { return nil }
func (d *fakeDriver) Steer(context.Context, string) error  { return nil }
func (d *fakeDriver) Abort(context.Context) error          { return nil }
func (d *fakeDriver) Resume(context.Context, string) error { return nil }
func (d *fakeDriver) SessionStats(context.Context) (harness.SessionStats, error) {
	return harness.SessionStats{}, nil
}

func (d *fakeDriver) Run(_ context.Context, prompt string) (harness.Outcome, error) {
	out := d.runFn(d, prompt)
	return out, out.RunError()
}

func (d *fakeDriver) Close(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	d.closes++
	close(d.events)
	return nil
}

func (d *fakeDriver) closeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closes
}

// emit delivers one event on the stream, honoring Close.
func (d *fakeDriver) emit(ev harness.Event) {
	harness.Emit(d.events, ev, nil)
}

// newTestRunner returns a runner whose drivers come from a counting
// factory, so tests can assert how many driver instances were built.
func newTestRunner(runFn func(*fakeDriver, string) harness.Outcome) (*harnessRunner, *int32) {
	var constructed int32
	r := newHarnessRunner("pi", nil)
	r.factory = func(symphony.TurnRequest) (harness.Driver, error) {
		atomic.AddInt32(&constructed, 1)
		d := newFakeDriver()
		d.runFn = runFn
		return d, nil
	}
	return r, &constructed
}

func turnReq(session string) symphony.TurnRequest {
	return symphony.TurnRequest{SessionID: session, Workspace: tWorkspace}
}

var tWorkspace = "/tmp/opencode/lambe-test-ws"

// TestRunTurnDrainsEventStream pins the drain goroutine: a run that emits
// far more events than the 1024-slot buffer must complete instead of
// blocking on a full stream, and the drain must exit once the driver
// closes.
func TestRunTurnDrainsEventStream(t *testing.T) {
	t.Parallel()
	r, constructed := newTestRunner(func(d *fakeDriver, _ string) harness.Outcome {
		for i := 0; i < 3000; i++ {
			d.emit(harness.ToolExecutionStartedEvent{ToolName: "bash"})
		}
		return harness.Outcome{Kind: harness.OutcomeSettled}
	})

	done := make(chan struct{})
	var res symphony.TurnResult
	var err error
	go func() {
		defer close(done)
		res, err = r.RunTurn(context.Background(), turnReq("drain-1"))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunTurn blocked on an undrained event stream")
	}
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.Exit != symphony.TurnSuccess || !res.Settled {
		t.Fatalf("result = %+v, want settled success", res)
	}
	if n := atomic.LoadInt32(constructed); n != 1 {
		t.Fatalf("drivers constructed = %d, want 1", n)
	}

	r.mu.Lock()
	sd := r.drivers["drain-1"]
	r.mu.Unlock()
	if sd == nil {
		t.Fatal("supervised driver missing after turn")
	}
	r.CloseSession(context.Background(), "drain-1")
	select {
	case <-sd.done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain goroutine did not exit after Close")
	}
}

// TestRunTurnSettledOutputCarriesLastAssistantMessage pins the settled
// output: the gate judges the agent's own final message, not empty
// diagnostics.
func TestRunTurnSettledOutputCarriesLastAssistantMessage(t *testing.T) {
	t.Parallel()
	r, _ := newTestRunner(func(d *fakeDriver, _ string) harness.Outcome {
		d.emit(harness.MessageCompletedEvent{Role: "user", Text: "please fix"})
		d.emit(harness.MessageCompletedEvent{Role: "assistant", Text: "earlier status"})
		d.emit(harness.MessageCompletedEvent{Role: "assistant", Text: "done: tests pass"})
		return harness.Outcome{Kind: harness.OutcomeSettled}
	})

	res, err := r.RunTurn(context.Background(), turnReq("out-1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "done: tests pass" {
		t.Fatalf("settled output = %q, want the last assistant message", res.Output)
	}
}

// TestDeadDriverRetiredAndReconstructed pins the dead-driver fix: a driver
// whose run ended in a supervision outcome (failed, watchdog-killed, limit,
// exited) is closed and forgotten, so the next turn gets a fresh instance
// instead of a Start no-op on a corpse.
func TestDeadDriverRetiredAndReconstructed(t *testing.T) {
	t.Parallel()
	var first *fakeDriver
	r, constructed := newTestRunner(func(d *fakeDriver, _ string) harness.Outcome {
		if first == nil {
			first = d
			return harness.Outcome{Kind: harness.OutcomeFailed, Err: errors.New("agent exploded")}
		}
		return harness.Outcome{Kind: harness.OutcomeSettled}
	})

	res, err := r.RunTurn(context.Background(), turnReq("dead-1"))
	if res.Exit != symphony.TurnFailure || err == nil {
		t.Fatalf("first turn = %+v, %v; want failure", res, err)
	}
	if n := atomic.LoadInt32(constructed); n != 1 {
		t.Fatalf("drivers constructed = %d after first turn, want 1", n)
	}
	deadline := time.After(5 * time.Second)
	for first.closeCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("dead driver was never closed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	res, err = r.RunTurn(context.Background(), turnReq("dead-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Settled || res.Exit != symphony.TurnSuccess {
		t.Fatalf("second turn = %+v, want settled success", res)
	}
	if n := atomic.LoadInt32(constructed); n != 2 {
		t.Fatalf("drivers constructed = %d after second turn, want 2 (fresh driver)", n)
	}
}

// TestCloseSessionClosesLiveDriver pins the teardown seam: closing a
// session with a live (settled) driver shuts it down and forgets it, so a
// later turn constructs a new driver.
func TestCloseSessionClosesLiveDriver(t *testing.T) {
	t.Parallel()
	var live *fakeDriver
	r, constructed := newTestRunner(func(d *fakeDriver, _ string) harness.Outcome {
		live = d
		return harness.Outcome{Kind: harness.OutcomeSettled}
	})

	if _, err := r.RunTurn(context.Background(), turnReq("close-1")); err != nil {
		t.Fatal(err)
	}
	r.CloseSession(context.Background(), "close-1")
	if live.closeCount() != 1 {
		t.Fatalf("driver closes = %d, want 1", live.closeCount())
	}

	// Closing an unknown session is a no-op, not a panic.
	r.CloseSession(context.Background(), "missing")

	if _, err := r.RunTurn(context.Background(), turnReq("close-1")); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(constructed); n != 2 {
		t.Fatalf("drivers constructed = %d, want 2 after close + rerun", n)
	}
}

// pgidRecorder is a tracker.Tracker double with real upsert semantics for
// PutAttempt, so the persist seam's re-put is observable.
type pgidRecorder struct {
	mu         sync.Mutex
	attempts   map[string][]tracker.Attempt
	puts       []tracker.Attempt
	attemptErr error
}

func newPgidRecorder() *pgidRecorder {
	return &pgidRecorder{attempts: map[string][]tracker.Attempt{}}
}

func (p *pgidRecorder) Capabilities(context.Context) (tracker.Capabilities, error) {
	return tracker.Capabilities{}, nil
}

func (p *pgidRecorder) Dispatchable(context.Context, tracker.DispatchQuery) ([]tracker.Issue, error) {
	return nil, nil
}

func (p *pgidRecorder) Claim(_ context.Context, id string) (tracker.Issue, error) {
	return tracker.Issue{ID: id}, nil
}
func (p *pgidRecorder) Release(context.Context, string) error   { return nil }
func (p *pgidRecorder) Heartbeat(context.Context, string) error { return nil }
func (p *pgidRecorder) Reclaim(context.Context, time.Duration, []string) error {
	return nil
}
func (p *pgidRecorder) Defer(context.Context, string, time.Time, string) error { return nil }
func (p *pgidRecorder) Events(context.Context, int64) ([]tracker.Event, error) {
	return nil, tracker.ErrJournalDisabled
}

func (p *pgidRecorder) EnsureIssue(_ context.Context, key string, _ tracker.IssueSpec) (tracker.Issue, error) {
	return tracker.Issue{ID: key}, nil
}

func (p *pgidRecorder) PutAttempt(_ context.Context, a tracker.Attempt) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attemptErr != nil {
		return p.attemptErr
	}
	p.puts = append(p.puts, a)
	for i, existing := range p.attempts[a.SessionID] {
		if existing.Number == a.Number {
			p.attempts[a.SessionID][i] = a
			return nil
		}
	}
	p.attempts[a.SessionID] = append(p.attempts[a.SessionID], a)
	return nil
}

func (p *pgidRecorder) Attempts(_ context.Context, sessionID string) ([]tracker.Attempt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tracker.Attempt(nil), p.attempts[sessionID]...), nil
}

func (p *pgidRecorder) recorded() []tracker.Attempt {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tracker.Attempt(nil), p.puts...)
}

// TestAttemptPersisterRecordsPGIDOnLatestAttempt pins the persist seam's
// contract: the pgid lands on the session's highest-numbered attempt bead
// (the one the loop recorded just before the turn), via an idempotent
// re-put.
func TestAttemptPersisterRecordsPGIDOnLatestAttempt(t *testing.T) {
	t.Parallel()
	rec := newPgidRecorder()
	if err := rec.PutAttempt(context.Background(), tracker.Attempt{SessionID: "ses-1", Number: 1}); err != nil {
		t.Fatal(err)
	}
	if err := rec.PutAttempt(context.Background(), tracker.Attempt{SessionID: "ses-1", Number: 2}); err != nil {
		t.Fatal(err)
	}

	persist := newAttemptPersisterFrom(func() (tracker.Tracker, error) { return rec, nil }, slog.New(slog.DiscardHandler))
	persist("ses-1", 4242)

	puts := rec.recorded()
	if len(puts) != 3 {
		t.Fatalf("puts = %+v, want the pgid re-put", puts)
	}
	got := puts[len(puts)-1]
	if got.SessionID != "ses-1" || got.Number != 2 || got.PGID != 4242 {
		t.Fatalf("pgid put = %+v, want ses-1/2/4242", got)
	}
}

// TestAttemptPersisterCreatesFirstAttempt pins the no-attempt fallback:
// a runner invoked outside the loop still records something recoverable.
func TestAttemptPersisterCreatesFirstAttempt(t *testing.T) {
	t.Parallel()
	rec := newPgidRecorder()
	persist := newAttemptPersisterFrom(func() (tracker.Tracker, error) { return rec, nil }, slog.New(slog.DiscardHandler))
	persist("fresh-1", 7)

	attempts, err := rec.Attempts(context.Background(), "fresh-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Number != 1 || attempts[0].PGID != 7 {
		t.Fatalf("attempts = %+v, want one attempt number 1 pgid 7", attempts)
	}
}

// TestPersistForBindsIssueID pins the per-driver closure: the drivers'
// (pgid, session) seam is bound to the lambe issue id, whatever backend
// session label the driver passes along.
func TestPersistForBindsIssueID(t *testing.T) {
	t.Parallel()
	r := newHarnessRunner("pi", nil)
	if hook := r.persistFor("i-1"); hook != nil {
		t.Fatal("persistFor with no persist configured must be nil")
	}
	var mu sync.Mutex
	var got []tracker.Attempt
	r.persist = func(sessionID string, pgid int) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, tracker.Attempt{SessionID: sessionID, PGID: pgid})
	}
	r.persistFor("i-9")(555, "lambe-i-9")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].SessionID != "i-9" || got[0].PGID != 555 {
		t.Fatalf("persist calls = %+v, want i-9/555", got)
	}
}
