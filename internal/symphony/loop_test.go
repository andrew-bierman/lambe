package symphony

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	loop   *Loop
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
	f.loop = NewLoop(f.tr, loader, f.runner, f.gate, f.spaces, log)
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
	if len(calls) != 2 {
		t.Fatalf("runner calls = %d, want 2", len(calls))
	}
	if calls[1].Attempt != 1 {
		t.Errorf("gate error consumed the attempt: attempt = %d, want 1", calls[1].Attempt)
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
		if err := os.WriteFile(path, []byte("---\n---\nsecond"), 0o600); err != nil {
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
