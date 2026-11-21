package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/harness"
	acpdrv "github.com/andrew-bierman/lambe/internal/harness/acp"
	"github.com/andrew-bierman/lambe/internal/logging"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestAgentStderrKeepsStructuredSessionDiagnostics(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	w := agentStderr{log: logging.New(&out, slog.LevelInfo).With("session", "smoke-1")}
	diagnostic := "Node startup failed\nsecond line\n"
	n, err := w.Write([]byte(diagnostic))
	if err != nil || n != len(diagnostic) {
		t.Fatalf("write = %d, %v", n, err)
	}
	var record map[string]string
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatalf("diagnostic must remain JSON: %v", err)
	}
	if record["msg"] != "agent stderr" || record["session"] != "smoke-1" || record["output"] != diagnostic {
		t.Fatalf("diagnostic lost context or output: %v", record)
	}
}

// fakeDriver is a scriptable harness.Driver for wire tests. Events mirrors
// the real drivers' 1024-buffered stream, so a run that emits more events
// than the buffer holds deadlocks unless something drains the stream.
type fakeDriver struct {
	sessionID string
	statsFn   func() (harness.SessionStats, error)
	events    chan harness.Event
	startFn   func(*fakeDriver) error
	runFn     func(*fakeDriver, string) harness.Outcome

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
	if d.statsFn != nil {
		return d.statsFn()
	}
	return harness.SessionStats{SessionID: d.sessionID}, nil
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

// ListByMetadata is unused by the persist seam, which reads one session's
// attempts.
func (p *pgidRecorder) ListByMetadata(context.Context, string, string) ([]tracker.Issue, error) {
	return nil, nil
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

	persist := newAttemptPersisterFrom(func(context.Context) (tracker.Tracker, error) { return rec, nil }, slog.New(slog.DiscardHandler), func(int) (string, error) { return "test:identity", nil })
	if err := persist(t.Context(), "ses-1", 4242); err != nil {
		t.Fatal(err)
	}

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
	persist := newAttemptPersisterFrom(func(context.Context) (tracker.Tracker, error) { return rec, nil }, slog.New(slog.DiscardHandler), func(int) (string, error) { return "test:identity", nil })
	if err := persist(t.Context(), "fresh-1", 7); err != nil {
		t.Fatal(err)
	}

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
	r.persist = func(sessionID string, pgid int) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, tracker.Attempt{SessionID: sessionID, PGID: pgid})
		return nil
	}
	if err := r.persistFor("i-9")(555, "lambe-i-9"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].SessionID != "i-9" || got[0].PGID != 555 {
		t.Fatalf("persist calls = %+v, want i-9/555", got)
	}
}

type closeContextDriver struct {
	*fakeDriver
	seen func(context.Context)
}

func (d closeContextDriver) Close(ctx context.Context) error {
	d.seen(ctx)
	return d.fakeDriver.Close(ctx)
}

func TestCloseSessionPreservesShutdownDeadlineAfterCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	deadline, _ := ctx.Deadline()
	cancel()
	drv := closeContextDriver{fakeDriver: newFakeDriver(), seen: func(cleanup context.Context) {
		got, ok := cleanup.Deadline()
		if !ok || got.After(deadline.Add(time.Millisecond)) {
			t.Errorf("cleanup deadline %v extends shutdown deadline %v", got, deadline)
		}
		if cleanup.Err() != nil {
			t.Error("cleanup inherited cancellation instead of a bounded fresh context")
		}
	}}
	r := newHarnessRunner("pi", nil)
	r.drivers["shutdown"] = newSupervisedDriver(drv)
	r.CloseSession(ctx, "shutdown")
}

func TestAttemptPersisterRetriesOpeningAfterTransientFailure(t *testing.T) {
	rec := newPgidRecorder()
	calls := 0
	persist := newAttemptPersisterFrom(func(context.Context) (tracker.Tracker, error) {
		calls++
		if calls <= 2 {
			return nil, context.DeadlineExceeded
		}
		return rec, nil
	}, slog.New(slog.DiscardHandler), func(int) (string, error) { return "test:identity", nil })
	if err := persist(t.Context(), "recovery", 1234); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first persistence error = %v", err)
	}
	if err := persist(t.Context(), "recovery", 1234); err != nil {
		t.Fatal(err)
	}
	if got := rec.recorded(); len(got) != 1 || got[0].ProcessIdentity != "test:identity" {
		t.Fatalf("binding did not recover: %+v", got)
	}
}

func TestHarnessRunnerRecordsNativeThreadBeforePrompt(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted", true: "checkpoint-failure"}[fail], func(t *testing.T) {
			drv := newFakeDriver()
			drv.sessionID = "backend-native-thread"
			saved := ""
			prompted := false
			drv.runFn = func(_ *fakeDriver, _ string) harness.Outcome {
				if saved != drv.sessionID {
					t.Fatal("prompted before persisting actual backend identity")
				}
				prompted = true
				return harness.Outcome{Kind: harness.OutcomeSettled}
			}
			runner := newHarnessRunner("acp", nil)
			runner.factory = func(symphony.TurnRequest) (harness.Driver, error) { return drv, nil }
			defer runner.CloseSession(t.Context(), "issue")
			res, err := runner.RunTurn(t.Context(), symphony.TurnRequest{SessionID: "issue", Prompt: "work", RecordThread: func(id, _ string) error {
				if fail {
					return errors.New("checkpoint unavailable")
				}
				saved = id
				return nil
			}})
			if fail {
				if err == nil || prompted || drv.closeCount() != 1 {
					t.Fatalf("failed checkpoint started work: prompted=%v closed=%d err=%v", prompted, drv.closeCount(), err)
				}
			} else if err != nil || !prompted || res.ThreadID != drv.sessionID {
				t.Fatalf("backend thread lost: %+v, %v", res, err)
			}
		})
	}
}

func TestHarnessCommandSelection(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{"pi", config.Config{Harness: "pi"}, []string{"pi"}},
		{"codex", config.Config{Harness: "codex"}, []string{"codex-acp"}},
		{"gemini", config.Config{Harness: "acp", ACP: config.ACP{Agent: "gemini"}}, []string{"gemini", "--acp"}},
		{"custom", config.Config{Harness: "acp", ACP: config.ACP{Command: []string{"my-agent", "argument with spaces"}}}, []string{"my-agent", "argument with spaces"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HarnessCommand(&tt.cfg)
			if err != nil || !slices.Equal(got, tt.want) {
				t.Fatalf("launch argv=%v, %v", got, err)
			}
		})
	}
	if _, err := HarnessCommand(&config.Config{Harness: "typo"}); err == nil {
		t.Fatal("unknown harness silently selected Pi")
	}
	t.Setenv("PATH", t.TempDir())
	if err := CheckHarness(&config.Config{Harness: "codex"}); err == nil || !strings.Contains(err.Error(), "codex-acp") {
		t.Fatalf("missing executable lacks startup guidance: %v", err)
	}
}

func TestMissingFirstTurnSessionRecreatesOnlyWithFullPrompt(t *testing.T) {
	for _, turn := range []int{1, 2} {
		t.Run(fmt.Sprintf("turn-%d", turn), func(t *testing.T) {
			runner := newHarnessRunner("acp", nil)
			starts := 0
			prompted := false
			runner.factory = func(req symphony.TurnRequest) (harness.Driver, error) {
				starts++
				d := newFakeDriver()
				if req.ThreadID != "" {
					d.startFn = func(*fakeDriver) error { return acpdrv.ErrSessionNotFound }
				} else {
					d.sessionID = "replacement-thread"
					d.runFn = func(_ *fakeDriver, prompt string) harness.Outcome {
						if prompt != "complete original task" {
							t.Fatal("lost full first-turn prompt")
						}
						prompted = true
						return harness.Outcome{Kind: harness.OutcomeSettled}
					}
				}
				return d, nil
			}
			defer runner.CloseSession(t.Context(), "issue")
			result, err := runner.RunTurn(t.Context(), symphony.TurnRequest{SessionID: "issue", ThreadID: "empty-thread", Turn: turn, Prompt: "complete original task"})
			if turn == 1 {
				if err != nil || !prompted || starts != 2 || result.ThreadID != "replacement-thread" {
					t.Fatalf("first-turn recovery = %+v, %v, starts=%d", result, err, starts)
				}
			} else if err == nil || prompted || starts != 1 {
				t.Fatalf("recreated later conversation without its history: %v, starts=%d", err, starts)
			}
		})
	}
}

func TestPiThreadDoesNotDependOnUsageStats(t *testing.T) {
	d := newFakeDriver()
	d.statsFn = func() (harness.SessionStats, error) {
		t.Fatal("Pi stats must not gate a prompt")
		return harness.SessionStats{}, errors.New("stats unsupported")
	}
	d.runFn = func(*fakeDriver, string) harness.Outcome { return harness.Outcome{Kind: harness.OutcomeSettled} }
	runner := newHarnessRunner("pi", nil)
	runner.factory = func(symphony.TurnRequest) (harness.Driver, error) { return d, nil }
	defer runner.CloseSession(t.Context(), "issue")
	res, err := runner.RunTurn(t.Context(), symphony.TurnRequest{SessionID: "issue", ThreadID: "preserved-pi-session", Turn: 2})
	if err != nil || res.ThreadID != "preserved-pi-session" {
		t.Fatalf("Pi resumed a different thread: %+v, %v", res, err)
	}
}

func TestUnsupportedResumeRestartsAtFullTaskBoundary(t *testing.T) {
	runner := newHarnessRunner("acp", nil)
	creates := 0
	runner.factory = func(req symphony.TurnRequest) (harness.Driver, error) {
		creates++
		d := newFakeDriver()
		d.sessionID = "replacement"
		if req.ThreadID != "" {
			d.startFn = func(*fakeDriver) error { return acpdrv.ErrResumeNotSupported }
		} else {
			d.runFn = func(_ *fakeDriver, prompt string) harness.Outcome {
				if prompt != "full original task" {
					t.Fatal("fresh conversation received continuation fragment")
				}
				return harness.Outcome{Kind: harness.OutcomeSettled}
			}
		}
		return d, nil
	}
	defer runner.CloseSession(t.Context(), "issue")
	res, err := runner.RunTurn(t.Context(), symphony.TurnRequest{SessionID: "issue", ThreadID: "old-session", Turn: 1, Prompt: "full original task"})
	if err != nil || res.ThreadID != "replacement" || creates != 2 {
		t.Fatalf("unsupported backend stuck across attempts: %+v, %v, creates=%d", res, err, creates)
	}
}

func TestForeignOrLegacyThreadNeverLoadsIntoAnotherHarness(t *testing.T) {
	for _, origin := range []string{"pi", "acp:other", ""} {
		for _, turn := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s-turn-%d", origin, turn), func(t *testing.T) {
				runner := newHarnessRunner("acp", nil)
				creates := 0
				runner.factory = func(req symphony.TurnRequest) (harness.Driver, error) {
					creates++
					if req.ThreadID != "" {
						t.Fatal("sent incompatible session to backend")
					}
					d := newFakeDriver()
					d.sessionID = "new-native-id"
					d.runFn = func(*fakeDriver, string) harness.Outcome { return harness.Outcome{Kind: harness.OutcomeSettled} }
					return d, nil
				}
				defer runner.CloseSession(t.Context(), "issue")
				thread := sessionIDFor("issue")
				if origin != "" {
					thread = "foreign-native-thread"
				}
				res, err := runner.RunTurn(t.Context(), symphony.TurnRequest{SessionID: "issue", ThreadID: thread, ThreadHarness: origin, Turn: turn})
				if turn == 1 {
					if err != nil || creates != 1 || res.ThreadID != "new-native-id" || res.ThreadHarness != "acp" {
						t.Fatalf("migration failed: %+v, %v", res, err)
					}
				} else if err == nil || creates != 0 {
					t.Fatalf("fresh backend received continuation instead of full task: %v, creates=%d", err, creates)
				}
			})
		}
	}
}

func TestTransientStartupFailureNeverDiscardsSavedThread(t *testing.T) {
	for _, attempt := range []int{1, 2} {
		t.Run(fmt.Sprintf("attempt-%d", attempt), func(t *testing.T) {
			runner := newHarnessRunner("acp", nil)
			creates := 0
			prompted := false
			runner.factory = func(req symphony.TurnRequest) (harness.Driver, error) {
				creates++
				d := newFakeDriver()
				d.sessionID = "reconstructed-session"
				if req.ThreadID != "" {
					d.startFn = func(*fakeDriver) error { return errors.New("restore transport failed") }
				} else {
					d.runFn = func(_ *fakeDriver, prompt string) harness.Outcome {
						if prompt != "full task" {
							t.Fatal("lost full task prompt")
						}
						prompted = true
						return harness.Outcome{Kind: harness.OutcomeSettled}
					}
				}
				return d, nil
			}
			defer runner.CloseSession(t.Context(), "issue")
			res, err := runner.RunTurn(t.Context(), symphony.TurnRequest{SessionID: "issue", ThreadID: "old-session", Turn: 1, Attempt: attempt, Prompt: "full task"})
			if err == nil || prompted || creates != 1 || res.ThreadID != "" {
				t.Fatalf("discarded history on transient failure: err=%v, creates=%d", err, creates)
			}
		})
	}
}

func TestCancellationBeforePromptRetiresStartedAgent(t *testing.T) {
	for _, phase := range []string{"startup", "checkpoint"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			d := newFakeDriver()
			d.sessionID = "native-session"
			if phase == "startup" {
				d.startFn = func(*fakeDriver) error { cancel(); return nil }
			}
			d.runFn = func(*fakeDriver, string) harness.Outcome {
				t.Fatal("prompted after cancellation")
				return harness.Outcome{}
			}
			runner := newHarnessRunner("acp", nil)
			runner.factory = func(symphony.TurnRequest) (harness.Driver, error) { return d, nil }
			_, err := runner.RunTurn(ctx, symphony.TurnRequest{SessionID: "issue", RecordThread: func(string, string) error {
				if phase == "checkpoint" {
					cancel()
				}
				return nil
			}})
			if !errors.Is(err, context.Canceled) || d.closeCount() != 1 {
				t.Fatalf("canceled startup retained agent: %v, closes=%d", err, d.closeCount())
			}
		})
	}
}

func TestSwitchingBackToPiStartsUniqueConversation(t *testing.T) {
	runner := newHarnessRunner("pi", nil)
	runner.factory = func(req symphony.TurnRequest) (harness.Driver, error) {
		if req.ThreadID == "" || req.ThreadID == sessionIDFor("issue") {
			t.Fatal("reopened stale Pi conversation")
		}
		d := newFakeDriver()
		d.runFn = func(*fakeDriver, string) harness.Outcome { return harness.Outcome{Kind: harness.OutcomeSettled} }
		return d, nil
	}
	defer runner.CloseSession(t.Context(), "issue")
	res, err := runner.RunTurn(t.Context(), symphony.TurnRequest{SessionID: "issue", ThreadID: "codex-thread", ThreadHarness: "codex", Turn: 1})
	if err != nil || res.ThreadID == sessionIDFor("issue") {
		t.Fatalf("stale conversation: %+v %v", res, err)
	}
}

type timeoutBindingTracker struct {
	*pgidRecorder
	reads, writes int
}

func (t *timeoutBindingTracker) Attempts(ctx context.Context, sessionID string) ([]tracker.Attempt, error) {
	t.reads++
	if t.reads == 1 {
		return nil, context.DeadlineExceeded
	}
	return t.pgidRecorder.Attempts(ctx, sessionID)
}

func (t *timeoutBindingTracker) PutAttempt(ctx context.Context, a tracker.Attempt) error {
	t.writes++
	if err := t.pgidRecorder.PutAttempt(ctx, a); err != nil {
		return err
	}
	if t.writes == 1 {
		return context.DeadlineExceeded // The first write may have succeeded despite the timeout.
	}
	return nil
}

func TestAttemptPersisterRetriesTimedOutBinding(t *testing.T) {
	t.Parallel()
	tr := &timeoutBindingTracker{pgidRecorder: newPgidRecorder()}
	persist := newAttemptPersisterFrom(func(context.Context) (tracker.Tracker, error) { return tr, nil }, slog.New(slog.DiscardHandler), func(int) (string, error) { return "test:identity", nil })
	if err := persist(t.Context(), "session", 42); err != nil {
		t.Fatal(err)
	}
	attempts, err := tr.pgidRecorder.Attempts(t.Context(), "session")
	if err != nil || len(attempts) != 1 || attempts[0].PGID != 42 || tr.reads != 2 || tr.writes != 2 {
		t.Fatalf("retry lost or duplicated binding: attempts=%+v reads=%d writes=%d err=%v", attempts, tr.reads, tr.writes, err)
	}
}

func TestRetryPersistDoesNotRetryPermanentErrors(t *testing.T) {
	t.Parallel()
	permanent := errors.New("permission denied")
	calls := 0
	err := retryPersistTimeout(t.Context(), func(context.Context) error { calls++; return permanent })
	if !errors.Is(err, permanent) || calls != 1 {
		t.Fatalf("permanent error: calls=%d err=%v", calls, err)
	}
}

func TestAttemptPersisterObservesDeadlineWhileWaitingForOpen(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	persist := newAttemptPersisterFrom(func(ctx context.Context) (tracker.Tracker, error) {
		close(started)
		select {
		case <-release:
			return newPgidRecorder(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, slog.New(slog.DiscardHandler), func(int) (string, error) { return "test:identity", nil })
	first := make(chan error, 1)
	go func() { first <- persist(t.Context(), "first", 41) }()
	<-started

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	second := make(chan error, 1)
	go func() { second <- persist(ctx, "second", 42) }()
	select {
	case err := <-second:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("persist waiting for open: got %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("persist did not stop waiting at the deadline")
	}
	select {
	case err := <-first:
		t.Fatalf("first opener should still hold the lock, got %v", err)
	default:
	}
}

func TestRetryPersistUsesSharedDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	want, _ := ctx.Deadline()
	calls := 0
	err := retryPersistTimeout(ctx, func(attempt context.Context) error {
		calls++
		if got, ok := attempt.Deadline(); !ok || !got.Equal(want) {
			t.Fatalf("attempt deadline = %v, want transaction deadline %v", got, want)
		}
		if calls == 1 {
			return context.DeadlineExceeded
		}
		<-attempt.Done()
		return attempt.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 2 {
		t.Fatalf("shared deadline: calls=%d err=%v", calls, err)
	}
	err = retryPersistTimeout(ctx, func(context.Context) error {
		t.Fatal("expired transaction must not start another operation")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired transaction: %v", err)
	}
}

// TestWorkflowHarnessSelectsDriverPerIssue pins per-workflow harness
// selection: an issue routed to a workflow that names a harness gets that
// harness's driver; everything else keeps the daemon default.
func TestWorkflowHarnessSelectsDriverPerIssue(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Harness:    "pi",
		ACP:        config.ACP{Command: []string{self}},
		Workflows:  []config.NamedWorkflow{{Name: "Repo", Label: "lambe:repo"}, {Name: "Fleet", Label: "lambe:fleet", Harness: "acp"}},
		Supervisor: config.Supervisor{SilenceTimeout: time.Minute, TurnTimeout: time.Hour},
	}
	r := newHarnessRunner("pi", cfg)
	pi, err := r.newDriver(symphony.TurnRequest{SessionID: "a", Issue: tracker.Issue{Labels: []string{"lambe:repo"}}})
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := r.newDriver(symphony.TurnRequest{SessionID: "b", Issue: tracker.Issue{Labels: []string{"lambe:fleet"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeDriver(pi); closeDriver(fleet) })
	if _, ok := fleet.(*acpdrv.Driver); !ok {
		t.Fatalf("fleet workflow driver = %T, want *acp.Driver", fleet)
	}
	if _, ok := pi.(*acpdrv.Driver); ok {
		t.Fatal("repo workflow must keep the default pi harness")
	}
	if a, b := backendKeyFor(r, "lambe:repo"), backendKeyFor(r, "lambe:fleet"); a == b {
		t.Fatalf("saved conversations from different harnesses share backend key %q", a)
	}
}

func backendKeyFor(r *harnessRunner, label string) string {
	return backendKey(r.harnessFor(symphony.TurnRequest{Issue: tracker.Issue{Labels: []string{label}}}))
}
