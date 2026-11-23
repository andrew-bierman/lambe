package symphony

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// fixture assembles a Loop against fakes. Retry delays are zeroed so every
// scheduled retry is due on the next Tick.
type fixture struct {
	dir    string
	wfPath string
	tr     *fakeTracker
	runner *fakeRunner
	gate   *fakeGate
	loader *Loader
	spaces *WorkspaceManager
	loop   *waitingLoop
}

func newFixture(t *testing.T, body string, steps ...func(TurnRequest) TurnResult) *fixture {
	t.Helper()
	dir := t.TempDir()
	wfPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(wfPath, []byte("---\n---\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	loader, err := NewLoader(wfPath, fixedEnv(nil), log)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		dir:    dir,
		wfPath: wfPath,
		tr:     newFakeTracker(tracker.Issue{ID: "i-1", Title: "fix the build", Status: "ready"}),
		runner: newFakeRunner(steps...),
		gate:   &fakeGate{},
		loader: loader,
	}
	f.spaces = NewWorkspaceManager(filepath.Join(dir, "workspaces"), log)
	f.loop = &waitingLoop{NewLoop(f.tr, loader, f.runner, f.gate, f.spaces, log)}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := f.loop.Close(ctx); err != nil {
			t.Errorf("close fixture workers: %v", err)
		}
	})
	f.loop.FailureDelay = func(int) time.Duration { return 0 }
	f.loop.ContinuationDelay = 0
	return f
}

// settle is a scripted turn that succeeds and declares completion.
func settle(req TurnRequest) TurnResult {
	_ = req
	return TurnResult{Exit: TurnSuccess, Settled: true, ThreadID: "th-" + req.SessionID}
}

// continueTurn is a scripted turn that succeeds without settling.
func continueTurn(req TurnRequest) TurnResult {
	_ = req
	return TurnResult{Exit: TurnSuccess, Settled: false, ThreadID: "th-" + req.SessionID}
}

// failTurn is a scripted failing turn.
func failTurn(req TurnRequest) TurnResult {
	_ = req
	return TurnResult{Exit: TurnFailure}
}

// workspaceDir returns the expected workspace directory for an issue id.
func (f *fixture) workspaceDir(id string) string {
	return filepath.Join(f.spaces.Root, SanitizeKey(id))
}

func TestTickClaimRunGateGreenTerminalCleanup(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "work {{ issue }} on attempt {{ attempt }}", settle)
	f.gate.outcomes = []GateOutcome{{Verdict: GateGreen, Output: "tests pass"}}

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.recorded()
	if len(calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(calls))
	}
	req := calls[0]
	if !strings.Contains(req.Prompt, "fix the build") || !strings.Contains(req.Prompt, "attempt 1") {
		t.Errorf("prompt = %q, want issue title and attempt count", req.Prompt)
	}
	if req.Workspace != f.workspaceDir("i-1") {
		t.Errorf("workspace = %q, want %q", req.Workspace, f.workspaceDir("i-1"))
	}
	if req.Revision != f.loader.Current().Revision {
		t.Errorf("turn revision %q != claimed revision %q", req.Revision, f.loader.Current().Revision)
	}
	if _, err := os.Stat(req.Workspace); !os.IsNotExist(err) {
		t.Error("workspace not cleaned on terminal transition")
	}
	issue, _ := f.tr.get("i-1")
	if issue.Assignee != "" {
		t.Errorf("claim not released: %+v", issue)
	}
	if len(f.loop.sessions) != 0 {
		t.Errorf("session not torn down: %d remain", len(f.loop.sessions))
	}
}

func TestInvalidWorkflowMidRunLoopKeepsServing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "stable prompt body", settle)
	f.gate.outcomes = []GateOutcome{{Verdict: GateGreen}}

	// A broken WORKFLOW.md lands mid-run. The loader logs and keeps
	// serving the last-good revision; the loop never crashes.
	if err := os.WriteFile(f.wfPath, []byte("---\nnot: [closed\n---\nbroken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.loader.Reload(); err == nil {
		t.Fatal("invalid reload should report an error")
	}
	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatalf("tick after invalid reload: %v", err)
	}
	calls := f.runner.recorded()
	if len(calls) != 1 || !strings.Contains(calls[0].Prompt, "stable prompt body") {
		t.Errorf("runner calls = %+v, want last-good body served", calls)
	}
}

func TestRevisionPinWorkflowEditedBetweenAttempts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "body-one-marker", failTurn, settle)
	pinned := f.loader.Current().Revision

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1", f.runner.count())
	}

	// The workflow is validly edited and reloaded between the failed
	// attempt and its retry.
	if err := os.WriteFile(f.wfPath, []byte("---\n---\nbody-two-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.loader.Reload(); err != nil {
		t.Fatal(err)
	}

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.recorded()
	if len(calls) != 2 {
		t.Fatalf("runner calls = %d, want 2", len(calls))
	}
	if calls[1].Attempt != 2 {
		t.Errorf("second call attempt = %d, want 2", calls[1].Attempt)
	}
	if !strings.Contains(calls[1].Prompt, "body-one-marker") {
		t.Errorf("retry prompt %q rendered the edited body, want the pinned revision", calls[1].Prompt)
	}
	if calls[1].Revision != pinned {
		t.Errorf("retry revision = %q, want pinned %q", calls[1].Revision, pinned)
	}
}

type unavailableRefreshTracker struct {
	tracker.Tracker
	all       bool
	heartbeat bool
}

func (t unavailableRefreshTracker) Dispatchable(ctx context.Context, q tracker.DispatchQuery) ([]tracker.Issue, error) {
	if t.all || len(q.IDs) > 0 {
		return nil, context.DeadlineExceeded
	}
	return t.Tracker.Dispatchable(ctx, q)
}

func (t unavailableRefreshTracker) Heartbeat(ctx context.Context, id string) error {
	if t.heartbeat {
		return context.DeadlineExceeded
	}
	return t.Tracker.Heartbeat(ctx, id)
}

func TestRetrySurvivesTemporaryTrackerRefreshFailure(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		all       bool
		heartbeat bool
	}{{"refresh", false, false}, {"all reads", true, false}, {"heartbeat", false, true}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, "body", failTurn, settle)
			if err := f.loop.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.loop.Tracker = unavailableRefreshTracker{Tracker: f.tr, all: tt.all, heartbeat: tt.heartbeat}
			if err := f.loop.Tick(t.Context()); (err != nil) != tt.all {
				t.Fatalf("outage tick error = %v, all reads unavailable = %t", err, tt.all)
			}
			if got := f.runner.count(); got != 1 {
				t.Fatalf("tracker unavailable: got %d turns, want 1", got)
			}
			f.loop.Tracker = f.tr
			if err := f.loop.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			calls := f.runner.recorded()
			if len(calls) != 2 || calls[1].Attempt != 2 {
				t.Fatalf("retry did not resume after tracker recovery: %+v", calls)
			}
		})
	}
}

type lostRetryClaimTracker struct{ tracker.Tracker }

func (t lostRetryClaimTracker) Heartbeat(context.Context, string) error {
	return tracker.ErrNotOwner
}

func TestRetryStandsDownAfterClaimLost(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "body", failTurn, settle)
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	workspace := f.runner.recorded()[0].Workspace
	f.tr.mu.Lock()
	issue := f.tr.issues["i-1"]
	issue.Assignee = "another-supervisor"
	f.tr.issues["i-1"] = issue
	f.tr.mu.Unlock()
	f.loop.Tracker = lostRetryClaimTracker{Tracker: f.tr}
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.loop.Tracker = f.tr
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.runner.count(); got != 1 {
		t.Fatalf("lost claim must not run another turn: got %d", got)
	}
	current, _ := f.tr.get("i-1")
	if current.Assignee != "another-supervisor" || current.Status != "in_progress" {
		t.Fatalf("foreign claim changed: %+v", current)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("workspace must be retained after losing ownership: %v", err)
	}
}

func TestAfterCreateFailureRunnerNeverInvokedRetriedLater(t *testing.T) {
	t.Parallel()
	flag := filepath.Join(t.TempDir(), "fail-create")
	if err := os.WriteFile(flag, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, "body", settle)
	if err := os.WriteFile(f.wfPath, []byte("---\nhooks:\n  after_create: \"test ! -e "+flag+"\"\n---\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.loader.Reload(); err != nil {
		t.Fatal(err)
	}

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.runner.count(); got != 0 {
		t.Fatalf("runner invoked %d times after after_create failure, want 0", got)
	}
	if _, err := os.Stat(f.workspaceDir("i-1")); !os.IsNotExist(err) {
		t.Error("failed workspace directory still exists")
	}
	if f.loop.retries.Len() != 1 {
		t.Fatalf("scheduled retries = %d, want 1", f.loop.retries.Len())
	}

	// The condition is fixed; the next tick retries creation and the
	// runner finally runs.
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.runner.count(); got != 1 {
		t.Errorf("runner calls after retry = %d, want 1", got)
	}
}

func TestIssueClosedExternallyMidRunReconcileStops(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "body", continueTurn)

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1", f.runner.count())
	}
	if _, err := os.Stat(f.workspaceDir("i-1")); err != nil {
		t.Fatalf("workspace missing after first turn: %v", err)
	}

	// Someone closes the issue externally between turns.
	f.tr.set("i-1", "closed")

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.runner.count(); got != 1 {
		t.Errorf("runner called again after terminal state: %d calls", got)
	}
	if _, err := os.Stat(f.workspaceDir("i-1")); !os.IsNotExist(err) {
		t.Error("workspace not cleaned on reconcile stop")
	}
	if len(f.loop.sessions) != 0 {
		t.Errorf("session not torn down: %d remain", len(f.loop.sessions))
	}
}

func TestContinuationGuidanceOnSameThread(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "body", continueTurn, settle)
	f.gate.outcomes = []GateOutcome{{Verdict: GateGreen}}

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.recorded()
	if len(calls) != 2 {
		t.Fatalf("runner calls = %d, want 2", len(calls))
	}
	if !strings.Contains(calls[1].Prompt, "Continue or hand off") {
		t.Errorf("continuation prompt = %q", calls[1].Prompt)
	}
	if calls[1].ThreadID != "th-i-1" || calls[0].ThreadID != "" {
		t.Errorf("thread ids wrong: first %q (want empty), continuation %q (want th-i-1)", calls[0].ThreadID, calls[1].ThreadID)
	}
	if calls[1].Attempt != 1 || calls[1].Turn != 2 {
		t.Errorf("second call attempt=%d turn=%d, want 1/2", calls[1].Attempt, calls[1].Turn)
	}
}

func TestGateRedRetriesWithGateOutputInPrompt(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "body", settle, settle)
	f.gate.outcomes = []GateOutcome{
		{Verdict: GateRed, Output: "tests failed: build error"},
		{Verdict: GateGreen},
	}
	var rejected []string
	f.loop.OnGateRed = func(issue string, attempt int, output string) {
		rejected = append(rejected, fmt.Sprintf("%s/%d/%s", issue, attempt, output))
	}

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rejected) != 1 || !strings.HasSuffix(rejected[0], "/1/tests failed: build error") {
		t.Errorf("OnGateRed calls = %v, want one for attempt 1 with the gate output", rejected)
	}
	calls := f.runner.recorded()
	if len(calls) != 2 {
		t.Fatalf("runner calls = %d, want 2", len(calls))
	}
	if calls[1].Attempt != 2 {
		t.Errorf("red gate did not consume the attempt: attempt = %d", calls[1].Attempt)
	}
	if !strings.Contains(calls[1].Prompt, "tests failed: build error") {
		t.Errorf("retry prompt lacks gate output: %q", calls[1].Prompt)
	}
}

func TestGateErrorReusesAttempt(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "body", settle, settle)
	f.gate.outcomes = []GateOutcome{{Verdict: GateGreen}}
	f.gate.errs = []error{context.DeadlineExceeded}

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.recorded()
	if len(calls) != 1 {
		t.Fatalf("runner calls = %d, want 1: a gate error retries the GATE on the settled output, never the agent", len(calls))
	}
	if calls[0].Attempt != 1 {
		t.Errorf("attempt = %d, want 1", calls[0].Attempt)
	}
	if got := len(f.gate.calls); got < 2 {
		t.Errorf("gate evaluations = %d, want >= 2 (gate-only retry after the error)", got)
	}
}

func TestFormulaTriggerCookPourDispatch(t *testing.T) {
	t.Parallel()
	formulaDir := filepath.Join(t.TempDir(), ".beads", "formulas")
	if err := os.MkdirAll(formulaDir, 0o750); err != nil {
		t.Fatal(err)
	}
	formulaPath := filepath.Join(formulaDir, "keep-tests-green.formula.toml")
	if err := os.WriteFile(formulaPath, []byte("[trigger]\ncommand = \"exit 3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFixture(t, "body", settle)
	delete(f.tr.issues, "i-1") // the materialized issue is the only dispatchable work
	f.gate.outcomes = []GateOutcome{{Verdict: GateGreen}}
	materializer := &fakeMaterializer{tr: f.tr}
	materializer.materialize = func(Formula) {
		if _, err := f.tr.EnsureIssue(context.Background(), "mat-1", tracker.IssueSpec{Title: "keep tests green"}); err != nil {
			t.Error(err)
		}
	}
	f.loop.Materializer = materializer

	formulas, err := LoadFormulas(formulaDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(formulas) != 1 || formulas[0].Name != "keep-tests-green" {
		t.Fatalf("formulas = %+v", formulas)
	}
	f.loop.Formulas = formulas

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := materializer.materialized(); len(got) != 1 || got[0] != "keep-tests-green" {
		t.Fatalf("materialized = %v, want [keep-tests-green]", got)
	}
	calls := f.runner.recorded()
	if len(calls) != 1 || calls[0].Issue.ID != "mat-1" {
		t.Errorf("materialized issue not dispatched: %+v", calls)
	}
}

func TestFormulaNotFiredSkipsMaterialization(t *testing.T) {
	t.Parallel()
	formulaDir := filepath.Join(t.TempDir(), ".beads", "formulas")
	if err := os.MkdirAll(formulaDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "quiet.formula.toml"), []byte("[trigger]\ncommand = \"exit 0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, "body")
	delete(f.tr.issues, "i-1") // no dispatchable work of its own
	materializer := &fakeMaterializer{tr: f.tr}
	f.loop.Materializer = materializer
	formulas, err := LoadFormulas(formulaDir)
	if err != nil {
		t.Fatal(err)
	}
	f.loop.Formulas = formulas

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := materializer.materialized(); len(got) != 0 {
		t.Errorf("materialized = %v, want none", got)
	}
	if got := f.runner.count(); got != 0 {
		t.Errorf("runner calls = %d, want 0", got)
	}
}

func TestLoaderWatchAppliesValidEdit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte("---\n---\nfirst"), 0o600); err != nil {
		t.Fatal(err)
	}
	loader, err := NewLoader(path, fixedEnv(nil), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = loader.Watch(ctx)
	}()

	// Give the watcher a moment to arm, then rewrite until it reacts.
	time.Sleep(100 * time.Millisecond)
	deadline := time.After(3 * time.Second)
	for loader.Current().PromptBody != "second" {
		edit := filepath.Join(dir, "WORKFLOW.next")
		if err := os.WriteFile(edit, []byte("---\n---\nsecond"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(edit, path); err != nil {
			t.Fatal(err)
		}
		select {
		case <-deadline:
			t.Fatal("watcher did not apply the valid edit")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// closingRunner wraps a fakeRunner with a recording SessionCloser.
type closingRunner struct {
	*fakeRunner
	mu     sync.Mutex
	closed []string
}

// CloseSession implements SessionCloser, recording the call.
func (c *closingRunner) CloseSession(_ context.Context, sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = append(c.closed, sessionID)
}

func (c *closingRunner) closeLog() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.closed...)
}

// claimErrTracker poisons Claim for chosen issue ids while delegating
// everything else.
type claimErrTracker struct {
	*fakeTracker
	poison map[string]error
}

func (c *claimErrTracker) Claim(ctx context.Context, id string) (tracker.Issue, error) {
	if err := c.poison[id]; err != nil {
		return tracker.Issue{}, err
	}
	return c.fakeTracker.Claim(ctx, id)
}

// TestTerminalClosesRunnerSession pins the teardown seam: a terminal
// transition releases the runner's per-session resources.
func TestTerminalClosesRunnerSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "body", settle)
	f.gate.outcomes = []GateOutcome{{Verdict: GateGreen}}
	closer := &closingRunner{fakeRunner: f.runner}
	f.loop.Runner = closer

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := closer.closeLog(); len(got) != 1 || got[0] != "i-1" {
		t.Errorf("CloseSession calls = %v, want [i-1]", got)
	}
}

// TestTurnDeferredClosesRunnerSession pins the defer path: parking the
// issue until the wake time releases the agent process instead of leaking
// it across the defer horizon.
func TestTurnDeferredClosesRunnerSession(t *testing.T) {
	t.Parallel()
	deferTurn := func(TurnRequest) TurnResult {
		return TurnResult{Exit: TurnDeferred, WakeAt: time.Now().Add(time.Hour)}
	}
	f := newFixture(t, "body", deferTurn)
	closer := &closingRunner{fakeRunner: f.runner}
	f.loop.Runner = closer

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := closer.closeLog(); len(got) != 1 || got[0] != "i-1" {
		t.Errorf("CloseSession calls = %v, want [i-1]", got)
	}
}

// TestGateErrorClosesRunnerSession pins the gate-error path: a gate that
// could not judge parks the issue without consuming the attempt, and the
// agent process does not linger idle until the gate retry.
func TestGateErrorClosesRunnerSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "body", settle, settle)
	f.gate.outcomes = []GateOutcome{{Verdict: GateGreen}}
	f.gate.errs = []error{context.DeadlineExceeded}
	closer := &closingRunner{fakeRunner: f.runner}
	f.loop.Runner = closer

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := closer.closeLog(); len(got) != 1 || got[0] != "i-1" {
		t.Errorf("CloseSession calls = %v, want [i-1]", got)
	}
}

// TestClaimErrorDoesNotStarveRemainingIssues pins dispatch resilience: a
// claim failure on one issue is logged and skipped; only the dispatch read
// failure aborts the tick.
func TestClaimErrorDoesNotStarveRemainingIssues(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "body", settle)
	delete(f.tr.issues, "i-1")
	f.tr.issues["bad-1"] = tracker.Issue{ID: "bad-1", Title: "poisoned", Status: "ready"}
	f.tr.issues["good-1"] = tracker.Issue{ID: "good-1", Title: "healthy", Status: "ready"}
	f.loop.Tracker = &claimErrTracker{
		fakeTracker: f.tr,
		poison:      map[string]error{"bad-1": fmt.Errorf("tracker exploded")},
	}
	f.gate.outcomes = []GateOutcome{{Verdict: GateGreen}}

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatalf("tick aborted on one bad claim: %v", err)
	}
	calls := f.runner.recorded()
	if len(calls) != 1 || calls[0].Issue.ID != "good-1" {
		t.Fatalf("runner calls = %+v, want only good-1 dispatched", calls)
	}
}

// TestTriggerFiredCommandUnavailable pins the infra-failure mapping: exit
// codes 126 and 127 mean the trigger command could not run at all, so the
// trigger reports unavailable instead of firing work into the tracker.
func TestTriggerFiredCommandUnavailable(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"126", "127"} {
		f := Formula{Name: "broken", Path: "/tmp/opencode/broken.formula.toml", Command: "exit " + code}
		fired, err := TriggerFired(context.Background(), f)
		if fired {
			t.Errorf("exit %s: fired = true, want false", code)
		}
		if err == nil {
			t.Errorf("exit %s: err = nil, want unavailable error", code)
		}
	}
}

// TestFormulaTriggerUnavailableDoesNotMaterialize pins the loop-level
// behavior: an unavailable trigger logs and materializes nothing, without
// failing the tick.
func TestFormulaTriggerUnavailableDoesNotMaterialize(t *testing.T) {
	t.Parallel()
	formulaDir := filepath.Join(t.TempDir(), ".beads", "formulas")
	if err := os.MkdirAll(formulaDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "gone.formula.toml"), []byte("[trigger]\ncommand = \"definitely-not-a-command-lambe --flag\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, "body")
	delete(f.tr.issues, "i-1")
	materializer := &fakeMaterializer{tr: f.tr}
	f.loop.Materializer = materializer
	formulas, err := LoadFormulas(formulaDir)
	if err != nil {
		t.Fatal(err)
	}
	f.loop.Formulas = formulas

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := materializer.materialized(); len(got) != 0 {
		t.Errorf("materialized = %v, want none", got)
	}
}

func TestTriggerTimeoutDoesNotFireResponsibility(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	fired, err := TriggerFired(ctx, Formula{Name: "timeout", Path: filepath.Join(t.TempDir(), "timeout.formula.toml"), Command: "exec sleep 10"})
	if fired || err == nil {
		t.Fatalf("timed-out trigger = %v, %v; want unavailable, not fired", fired, err)
	}
}

type contextMaterializer func(context.Context, Formula) error

func (f contextMaterializer) Materialize(ctx context.Context, formula Formula) error {
	return f(ctx, formula)
}

func TestFormulaTimeoutStillDispatchesReadyWork(t *testing.T) {
	for _, stage := range []string{"trigger", "materializer"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, "work {{ issue }}", settle)
			// Leave time for the real trigger shell to start on a loaded race-test runner.
			f.loop.ControlTimeout = time.Second
			command := "exit 1"
			if stage == "trigger" {
				command = "exec sleep 10"
			}
			f.loop.Formulas = []Formula{{Name: "slow", Path: filepath.Join(f.dir, "slow.formula.toml"), Command: command}}
			calls := 0
			f.loop.Materializer = contextMaterializer(func(ctx context.Context, _ Formula) error {
				calls++
				<-ctx.Done()
				return ctx.Err()
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := f.loop.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() != nil {
				t.Fatalf("control operation blocked dispatch: %v", ctx.Err())
			}
			want := 1
			if stage == "trigger" {
				want = 0
			}
			if calls != want {
				t.Errorf("materializer calls = %d, want %d", calls, want)
			}
			if len(f.runner.recorded()) != 1 {
				t.Fatal("ready issue was not run after control timeout")
			}
		})
	}
}

// Trigger evaluation continues after materialization; the materializer owns durable dedup.
func TestFormulaChecksEveryCycleThroughDurableMaterializer(t *testing.T) {
	t.Parallel()
	formulaDir := filepath.Join(t.TempDir(), ".beads", "formulas")
	if err := os.MkdirAll(formulaDir, 0o750); err != nil {
		t.Fatal(err)
	}
	formulaPath := filepath.Join(formulaDir, "keep-tests-green.formula.toml")
	if err := os.WriteFile(formulaPath, []byte("[trigger]\ncommand = \"exit 3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFixture(t, "body", settle)
	f.loop.Formulas = []Formula{{Name: "keep-tests-green", Path: formulaPath, Command: "exit 3"}}
	materializer := &fakeMaterializer{tr: f.tr}
	f.loop.Materializer = materializer

	for i := 0; i < 3; i++ {
		if err := f.loop.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := materializer.materialized(); len(got) != 3 {
		t.Fatalf("materializer checks = %v, want one per cycle so closed work can re-arm", got)
	}
}

// TestDispatchSkipsFormulaTemplateIDs pins the dispatch filter (issue #7):
// `bd cook --persist` template beads (ids "<formula>.<step>") are read-only
// and must never reach Claim, while mol-poured work items dispatch normally.
func TestDispatchSkipsFormulaTemplateIDs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "body", settle)
	delete(f.tr.issues, "i-1")
	f.tr.issues["keep-tests-green.activate"] = tracker.Issue{ID: "keep-tests-green.activate", Title: "template", Status: "ready"}
	f.tr.issues["tracker-mol-1"] = tracker.Issue{ID: "tracker-mol-1", Title: "poured step", Status: "ready"}
	f.gate.outcomes = []GateOutcome{{Verdict: GateGreen}}
	f.loop.Formulas = []Formula{{Name: "keep-tests-green"}}

	if err := f.loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.recorded()
	if len(calls) != 1 || calls[0].Issue.ID != "tracker-mol-1" {
		t.Fatalf("runner calls = %+v, want exactly one run for tracker-mol-1 (template skipped before claim)", calls)
	}
}

// TestBeadsMaterializerLedgerSkipsOpenRoot pins the durable dedup (issue
// #6): a second pour is skipped while the recorded root mol is open, and
// allowed once it closes.
func TestBeadsMaterializerLedgerSkipsOpenRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads", "formulas"), 0o750); err != nil {
		t.Fatal(err)
	}
	formulaJSON := filepath.Join(dir, ".beads", "formulas", "latus-1436.formula.json")
	body := `{"formula":"latus-1436","title":"t","steps":[{"id":"s","title":"s","run":"true"}]}`
	if err := os.WriteFile(formulaJSON, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(dir, "fake-bd-state")
	poursFile := filepath.Join(dir, "fake-bd-pours")
	script := filepath.Join(dir, "fake-bd")
	scriptBody := fmt.Sprintf(`#!/bin/bash
case "$1" in
  mol) echo "Root issue: test-mol-9"; echo "poured" >> %q ;;
  show)
    if [ -f %q ]; then echo '[{"status":"closed"}]'; else echo '[{"status":"open"}]'; fi ;;
esac`, poursFile, stateFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o700); err != nil { // #nosec G306 -- executable fixture in a private test directory
		t.Fatal(err)
	}

	m := &BeadsMaterializer{Bin: script, Dir: dir, Log: slog.New(slog.NewTextHandler(&strings.Builder{}, nil))}
	fixture := Formula{Name: "latus-1436"}

	if err := m.Materialize(context.Background(), fixture); err != nil {
		onDisk, _ := os.ReadFile(script) // #nosec G304 -- test-owned executable fixture
		t.Fatalf("first materialize: %v | script on disk:\n%s", err, onDisk)
	}
	if _, err := os.Stat(poursFile); err != nil {
		ledger, _ := m.loadLedger()
		onDisk, _ := os.ReadFile(script) // #nosec G304 -- test-owned executable fixture
		out, runErr := m.run(context.Background(), "mol", "pour", "latus-1436")
		t.Fatalf("pours missing after first pour: stat=%v ledger=%+v manual-run-out=%q manual-err=%v | script on disk:\n%s", err, ledger, out, runErr, onDisk)
	}
	if err := m.Materialize(context.Background(), fixture); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, poursFile)) != "poured\n" {
		t.Fatalf("pours = %q, want exactly one while the root mol is open", mustRead(t, poursFile))
	}
	if err := os.WriteFile(stateFile, []byte("closed"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate the last pour past the refire interval so the closed root
	// re-arms immediately; the interval itself is pinned separately.
	backdateLedger(t, m, "latus-1436")
	if err := m.Materialize(context.Background(), fixture); err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(strings.TrimRight(mustRead(t, poursFile), "\n"), "\n")); n != 2 {
		t.Fatalf("pours after close = %d, want 2 (ledger re-arms once the root closes)", n)
	}
}

// TestBeadsMaterializerRefireInterval pins the re-fire rate limit: once a
// formula's root mol closes, an immediate re-pour is blocked by the
// refire interval and allowed once the interval elapses, so a close-race
// limbo upstream cannot rapid-fire fresh issue sets on every tick.
func TestBeadsMaterializerRefireInterval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o750); err != nil {
		t.Fatal(err)
	}
	poursFile := filepath.Join(dir, "fake-bd-pours")
	script := filepath.Join(dir, "fake-bd")
	scriptBody := fmt.Sprintf(`#!/bin/bash
case "$1" in
  mol) echo "Root issue: test-mol-4"; echo "poured" >> %q ;;
  show) echo '{"status":"closed"}' ;;
esac`, poursFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o700); err != nil { // #nosec G306 -- test script must be executable
		t.Fatal(err)
	}

	m := &BeadsMaterializer{Bin: script, Dir: dir, Log: slog.New(slog.NewTextHandler(&strings.Builder{}, nil))}
	fixture := Formula{Name: "latus-1436"}

	if err := m.Materialize(context.Background(), fixture); err != nil {
		t.Fatal(err)
	}
	// Root already closed: an immediate second pour is inside the
	// default refire window and must be blocked.
	if err := m.Materialize(context.Background(), fixture); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, poursFile); got != "poured\n" {
		t.Fatalf("pours inside the refire interval = %q, want exactly one", got)
	}

	// Once the recorded pour ages past the interval, the pour is allowed.
	backdateLedger(t, m, "latus-1436")
	if err := m.Materialize(context.Background(), fixture); err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(strings.TrimRight(mustRead(t, poursFile), "\n"), "\n")); n != 2 {
		t.Fatalf("pours after the interval = %d, want 2", n)
	}
}

// backdateLedger moves a formula's recorded pour time one refire window
// into the past.
func backdateLedger(t *testing.T, m *BeadsMaterializer, name string) {
	t.Helper()
	entries, err := m.loadLedger()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := entries[name]
	if !ok || entry.RootID == "" {
		t.Fatalf("no ledger entry for %s to backdate", name)
	}
	entry.PouredAt = time.Now().Add(-2 * DefaultRefireInterval).UTC().Format(time.RFC3339)
	entries[name] = entry
	if err := m.saveLedger(entries); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- test-owned fixture path
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// waitingLoop keeps turn/gate protocol tests synchronous; scheduler tests use Loop directly.
type waitingLoop struct{ *Loop }

func (l *waitingLoop) Tick(ctx context.Context) error {
	if err := l.Loop.Tick(ctx); err != nil {
		return err
	}
	return l.Wait(ctx)
}

func TestRecurringFormulaRearmsThroughTheRunningLoop(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "closed")
	pours := filepath.Join(dir, "pours")
	triggers := filepath.Join(dir, "triggers")
	script := filepath.Join(dir, "bd")
	body := fmt.Sprintf(`#!/bin/sh
case "$1" in
 mol) echo poured >> %q; echo 'Root issue: repeat-root';;
 show) if test -f %q; then echo '[{"status":"closed"}]'; else echo '[{"status":"open"}]'; fi;;
esac
`, pours, state)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { // #nosec G306 -- private executable test fixture
		t.Fatal(err)
	}
	f := newFixture(t, "body", settle)
	m := &BeadsMaterializer{Bin: script, Dir: dir, Log: f.loop.Log}
	f.loop.Materializer = m
	f.loop.Formulas = []Formula{{Name: "repeat", Path: filepath.Join(dir, "repeat.formula.toml"), Command: fmt.Sprintf("echo checked >> %q; exit 1", triggers)}}
	for range 2 {
		if err := f.loop.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(mustRead(t, pours)); got != "poured\n" {
		t.Fatalf("duplicate live responsibility: %q", got)
	}
	if err := os.WriteFile(state, []byte("closed"), 0o600); err != nil {
		t.Fatal(err)
	}
	backdateLedger(t, m, "repeat")
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, pours)); got != "poured\npoured\n" {
		t.Fatalf("completed responsibility never rearmed: %q", got)
	}
	if got := string(mustRead(t, triggers)); got != "checked\nchecked\nchecked\n" {
		t.Fatalf("trigger evaluated twice: %q", got)
	}
}

func TestAmbiguousOrCorruptMaterializationNeverPoursAgain(t *testing.T) {
	for _, mode := range []string{"failed-command", "missing-root", "corrupt-ledger"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			pours := filepath.Join(dir, "pours")
			script := filepath.Join(dir, "bd")
			result := "exit 1"
			if mode == "missing-root" {
				result = "echo 'unexpected output'; exit 0"
			}
			body := fmt.Sprintf("#!/bin/sh\necho poured >> %q\n%s\n", pours, result)
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil { // #nosec G306 -- private executable test fixture
				t.Fatal(err)
			}
			m := &BeadsMaterializer{Bin: script, Dir: dir, Log: slog.Default()}
			if mode == "corrupt-ledger" {
				if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(m.ledgerPath(), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := m.Materialize(t.Context(), Formula{Name: "repeat"}); err == nil {
					t.Fatal("uncertain materialization reported success")
				}
			}
			if mode == "corrupt-ledger" {
				if _, err := os.Stat(pours); !os.IsNotExist(err) {
					t.Fatal("corrupt ledger caused another pour")
				}
			} else if got := string(mustRead(t, pours)); got != "poured\n" {
				t.Fatalf("ambiguous side effect repeated: %q", got)
			}
		})
	}
}

type gateCloseRaceTracker struct {
	tracker.Tracker
	tr         *fakeTracker
	closed     bool
	omitClosed bool
}

func (tr gateCloseRaceTracker) Heartbeat(_ context.Context, id string) error {
	if tr.closed {
		tr.tr.set(id, tracker.StatusClosed)
	}
	return tracker.ErrNotOwner
}

func (tr gateCloseRaceTracker) Dispatchable(ctx context.Context, q tracker.DispatchQuery) ([]tracker.Issue, error) {
	rows, err := tr.Tracker.Dispatchable(ctx, q)
	if tr.omitClosed && len(q.Statuses) == 0 && !q.IncludeClosed {
		var filtered []tracker.Issue
		for _, row := range rows {
			if row.Status != tracker.StatusClosed {
				filtered = append(filtered, row)
			}
		}
		return filtered, err
	}
	return rows, err
}

func TestHeartbeatCloseRaceDoesNotCancelGateFinalization(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		settling, closed, omitClosed bool
	}{{"ordinary takeover", false, false, false}, {"own gate close", true, true, false}, {"closed omitted from default list", true, true, true}, {"takeover during gate", true, false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "body")
			f.tr.set("i-1", tracker.StatusInProgress)
			f.loop.Tracker = gateCloseRaceTracker{Tracker: f.tr, tr: f.tr, closed: tc.closed, omitClosed: tc.omitClosed}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			w := &worker{cancel: cancel, stop: workerRunning}
			w.settling.Store(tc.settling)
			f.loop.active["i-1"] = w
			defer delete(f.loop.active, "i-1")
			f.loop.reconcileWorkers(t.Context())
			if tc.closed {
				if ctx.Err() != nil || w.stop != workerTerminal {
					t.Fatal("own gate closure interrupted final bookkeeping")
				}
			} else if ctx.Err() == nil || w.stop != workerClaimLost {
				t.Fatal("foreign ownership loss did not stop work")
			}
		})
	}
}

func TestUnstartedPourCanRetryWithoutOperatorRepair(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "bd")
	m := &BeadsMaterializer{Bin: script, Dir: dir, Log: slog.Default()}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.Materialize(ctx, Formula{Name: "repeat"}); err == nil {
		t.Fatal("canceled pour succeeded")
	}
	if _, err := os.Stat(m.ledgerPath()); !os.IsNotExist(err) {
		t.Fatal("canceled context wrote intent")
	}
	if err := m.Materialize(t.Context(), Formula{Name: "repeat"}); !errors.Is(err, proc.ErrCommandNotStarted) {
		t.Fatalf("missing binary classification: %v", err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'Root issue: created-root'\n"), 0o700); err != nil { // #nosec G306 -- private executable test fixture
		t.Fatal(err)
	}
	if err := m.Materialize(t.Context(), Formula{Name: "repeat"}); err != nil {
		t.Fatalf("unstarted command wedged responsibility: %v", err)
	}
}

func TestInvalidLedgerEntryDoesNotDisableOtherResponsibilities(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "bd")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'Root issue: created-root'\n"), 0o700); err != nil { // #nosec G306 -- private executable test fixture
		t.Fatal(err)
	}
	m := &BeadsMaterializer{Bin: script, Dir: dir, Log: slog.Default()}
	entries := map[string]materializedEntry{"bad": {PouredAt: time.Now().UTC().Format(time.RFC3339)}}
	if err := m.saveLedger(entries); err != nil {
		t.Fatal(err)
	}
	if err := m.Materialize(t.Context(), Formula{Name: "bad"}); err == nil {
		t.Fatal("invalid entry accepted")
	}
	if err := m.Materialize(t.Context(), Formula{Name: "good"}); err != nil {
		t.Fatalf("one bad entry disabled another formula: %v", err)
	}
	entries, err := m.loadLedger()
	if err != nil {
		t.Fatal(err)
	}
	delete(entries, "bad")
	if err := m.saveLedger(entries); err != nil {
		t.Fatal(err)
	}
	if err := m.Materialize(t.Context(), Formula{Name: "bad"}); err != nil {
		t.Fatalf("operator repair failed: %v", err)
	}
}

func TestConfirmedMissingMaterializedRootCanRearm(t *testing.T) {
	for _, reply := range []string{"echo '[]'", "echo 'Issue old-root not found' >&2; echo '{\"schema_version\":1,\"error\":\"no issues found matching the provided IDs\"}'; exit 1"} {
		t.Run(reply, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "bd")
			body := "#!/bin/sh\ncase \"$1\" in\n show) " + reply + ";;\n mol) echo 'Root issue: next-root';;\nesac\n"
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil { // #nosec G306 -- private executable test fixture
				t.Fatal(err)
			}
			m := &BeadsMaterializer{Bin: script, Dir: dir, Log: slog.Default()}
			entries := map[string]materializedEntry{"repeat": {RootID: "old-root", PouredAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}}
			if err := m.saveLedger(entries); err != nil {
				t.Fatal(err)
			}
			if err := m.Materialize(t.Context(), Formula{Name: "repeat"}); err != nil {
				t.Fatalf("deleted root stalled formula: %v", err)
			}
			entries, err := m.loadLedger()
			if err != nil || entries["repeat"].RootID != "next-root" {
				t.Fatalf("no replacement root recorded: %+v %v", entries, err)
			}
		})
	}
}

func TestMaterializationLockGuardsTheWholeLedger(t *testing.T) {
	m := &BeadsMaterializer{Dir: t.TempDir(), Bin: "unused", Log: slog.Default()}
	lock, err := m.materializationLock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	err = m.Materialize(t.Context(), Formula{Name: "repeat"})
	if err == nil {
		t.Fatal("concurrent ledger writer allowed")
	}
	// The lock acquisition waits materializationLockWait before giving up
	// with a descriptive error; it no longer surfaces EWOULDBLOCK.
	if !strings.Contains(err.Error(), "another process is materializing responsibilities") {
		t.Fatalf("lock contention error = %v, want the materialization-lock message", err)
	}
	if _, err := os.Stat(m.ledgerPath()); !os.IsNotExist(err) {
		t.Fatal("contender wrote a pour intent")
	}
}

func TestTrackerWarningsDoNotPreventClosedRootRearming(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "bd")
	body := "#!/bin/sh\ncase \"$1\" in\n show) echo 'Notice: local import checked' >&2; echo '[{\"status\":\"closed\"}]';;\n mol) echo 'Root issue: new-root';;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	} // #nosec G306 -- private executable test fixture
	m := &BeadsMaterializer{Bin: script, Dir: dir, Log: slog.Default()}
	if err := m.saveLedger(map[string]materializedEntry{"repeat": {RootID: "old-root", PouredAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Materialize(t.Context(), Formula{Name: "repeat"}); err != nil {
		t.Fatalf("stderr warning stalled responsibility: %v", err)
	}
}

func TestSlowRootLookupDoesNotStartAStarvedPour(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "bd")
	poured := filepath.Join(dir, "poured")
	body := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n show) sleep 0.65; echo '[{\"status\":\"closed\"}]';;\n mol) echo yes > %q; echo 'Root issue: new-root';;\nesac\n", poured)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	} // #nosec G306 -- private executable test fixture
	m := &BeadsMaterializer{Bin: script, Dir: dir, Log: slog.Default()}
	if err := m.saveLedger(map[string]materializedEntry{"repeat": {RootID: "old-root", PouredAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := m.Materialize(ctx, Formula{Name: "repeat"}); err == nil {
		t.Fatal("started a pour after its lookup consumed the budget")
	}
	entries, err := m.loadLedger()
	if err != nil || entries["repeat"].Pending {
		t.Fatalf("read-only delay left ambiguous intent: %+v %v", entries, err)
	}
	if _, err := os.Stat(poured); !os.IsNotExist(err) {
		t.Fatal("starved pour was executed")
	}
}

// TestGateErrorsEscalateToTerminal pins issue #18: a permanent gate error
// parks the session after the escalation threshold instead of retrying the
// gate forever without consuming an attempt.
func TestGateErrorsEscalateToTerminal(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "body", settle)
	f.loop.ContinuationDelay = 0
	outcomes := make([]GateOutcome, gateEscalationThreshold)
	for i := range outcomes {
		outcomes[i] = GateOutcome{Verdict: GateErr, Output: "no gate formulas configured"}
	}
	f.gate.outcomes = outcomes

	for i := 0; i < gateEscalationThreshold+3; i++ {
		if err := f.loop.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(f.gate.calls); got != gateEscalationThreshold {
		t.Fatalf("gate evaluations = %d, want %d: terminal after the threshold, no further retries", got, gateEscalationThreshold)
	}
	if calls := f.runner.recorded(); len(calls) != 1 {
		t.Fatalf("runner calls = %d, want 1: a gate error never re-prompts the agent", len(calls))
	}
}

func TestNamedWorkflowDispatchSkipsUnroutedAndAmbiguousIssues(t *testing.T) {
	work := func(TurnRequest) TurnResult { return TurnResult{Exit: TurnSuccess} }
	f := newFixture(t, "General task: {{ issue.title }}", work, work)
	fleetPath := filepath.Join(f.dir, "FLEET.md")
	if err := os.WriteFile(fleetPath, []byte("---\n---\nFleet task: {{ issue.title }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	fleet, err := NewLoader(fleetPath, fixedEnv(nil), f.loop.Log)
	if err != nil {
		t.Fatal(err)
	}
	f.loop.WorkflowRoutes = map[string]*Loader{"lambe:general": f.loader, "lambe:fleet": fleet}
	f.tr.issues["i-1"] = tracker.Issue{ID: "i-1", Title: "Fix tests", Status: "ready", Labels: []string{"lambe:general"}}
	f.tr.issues["i-2"] = tracker.Issue{ID: "i-2", Title: "Check PRs", Status: "ready", Labels: []string{"lambe:fleet"}}
	f.tr.issues["i-3"] = tracker.Issue{ID: "i-3", Title: "Ambiguous", Status: "ready", Labels: []string{"lambe:general", "lambe:fleet"}}
	f.tr.issues["i-4"] = tracker.Issue{ID: "i-4", Title: "Unrouted", Status: "ready"}
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, request := range f.runner.recorded() {
		got[request.Issue.ID] = request.Prompt
	}
	if len(got) != 2 || !strings.Contains(got["i-1"], "General task: Fix tests") || !strings.Contains(got["i-2"], "Fleet task: Check PRs") {
		t.Fatalf("unexpected dispatches: %+v", got)
	}
}
