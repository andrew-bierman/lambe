// Package faultsuite is lambe's fault-injection suite: the executable
// definition of done for surviving crashes, hung turns, limit lockouts,
// false completion claims, and racing supervisors.
//
// The suite runs at package level against the committed seams —
// internal/tracker (real local `bd` trackers in throwaway directories),
// internal/symphony (the real loop), internal/harness + internal/harness/pi
// (the real driver over a re-exec fake agent), and internal/testchild (the
// re-exec fake child). Where the assembled daemon binary is genuinely
// required (a true kill -9 of the supervisor process itself), the suite
// exposes an env-gated integration hook (LAMBE_FAULT_DAEMON_BIN) instead of
// blocking; see README.md.
package faultsuite_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/harness"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/testchild"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// piFakeEnv selects the fake Pi agent behavior on re-exec, mirroring the
// re-exec pattern of internal/testchild and the pi driver's own tests.
const piFakeEnv = "LAMBE_FAULTSUITE_PI"

// TestMain re-execs as the fake Pi agent when piFakeEnv is set, re-execs as
// a testchild fake agent when internal/testchild's env var is set, and
// otherwise runs the fault suite.
func TestMain(m *testing.M) {
	if script := os.Getenv(piFakeEnv); script != "" {
		runPiFake(script)
		os.Exit(0)
	}
	testchild.Main(m)
}

// --- bd fixture helpers -------------------------------------------------

// bdBin returns the bd binary path, or "" when bd is unavailable. It honors
// LAMBE_TEST_BD both as a path and as the CI gate value "1", matching the
// skip pattern of internal/tracker's tests: bd-backed scenarios are skipped
// (never failed) when bd is absent or -short is set.
func bdBin() string {
	if v := os.Getenv("LAMBE_TEST_BD"); v != "" && v != "1" {
		return v
	}
	if v, err := exec.LookPath("bd"); err == nil {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", "bd")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// needBD skips the test when a real bd is unavailable and returns its path.
func needBD(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping bd fault scenario in -short mode")
	}
	bin := bdBin()
	if bin == "" {
		t.Skip("bd binary not found; set LAMBE_TEST_BD=1 with bd on PATH to run fault scenarios")
	}
	return bin
}

// trackerEnv couples a tracker with the binary and directory it runs
// against, so a second identity can be opened over the same storage.
type trackerEnv struct {
	*tracker.Local
	bin string
	dir string
}

// openIdentity opens another actor identity over the same tracker storage.
func (e trackerEnv) openIdentity(t *testing.T, actor string) *tracker.Local {
	t.Helper()
	l, err := tracker.NewLocal(e.bin, e.dir, actor)
	if err != nil {
		t.Fatalf("NewLocal(%s): %v", actor, err)
	}
	return l
}

// newTracker spins up a throwaway tracker in a temp directory under a
// disposable identity, per the scratch-repo discipline of
// internal/tracker's tests.
func newTracker(t *testing.T, actor string) trackerEnv {
	t.Helper()
	bin := needBD(t)
	dir := t.TempDir()
	l, err := tracker.NewLocal(bin, dir, actor)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if err := l.Init(t.Context()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return trackerEnv{Local: l, bin: bin, dir: dir}
}

// seedWork creates one open work issue and returns its id.
func seedWork(t *testing.T, tr tracker.Tracker, key, title string) string {
	t.Helper()
	issue, err := tr.EnsureIssue(t.Context(), key, tracker.IssueSpec{Title: title, Priority: 2})
	if err != nil {
		t.Fatalf("seed work %q: %v", title, err)
	}
	return issue.ID
}

// --- loop fixture helpers -----------------------------------------------

// quietLog discards loop and loader chatter; scenario logs are assertions,
// not output.
func quietLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newLoader writes a minimal WORKFLOW.md with the given prompt body into
// dir and returns its loader.
func newLoader(t *testing.T, dir, body string) *symphony.Loader {
	t.Helper()
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte("---\n---\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	loader, err := symphony.NewLoader(path, symphony.OSEnv, quietLog())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	return loader
}

// newLoop assembles a real symphony loop over tr with zero retry delays so
// every scheduled retry is due on the next tick.
func newLoop(tr tracker.Tracker, loader *symphony.Loader, runner symphony.AgentRunner, gates symphony.GateEvaluator, dir string) *waitingLoop {
	loop := symphony.NewLoop(tr, loader, runner, gates, symphony.NewWorkspaceManager(filepath.Join(dir, "workspaces"), quietLog()), quietLog())
	loop.FailureDelay = func(int) time.Duration { return 0 }
	loop.ContinuationDelay = 0
	return &waitingLoop{loop}
}

// turnRecorder is an AgentRunner that records every turn request and
// answers from a scriptable factory.
type turnRecorder struct {
	mu    sync.Mutex
	calls []symphony.TurnRequest
	next  func(symphony.TurnRequest) symphony.TurnResult
}

func newTurnRecorder(next func(symphony.TurnRequest) symphony.TurnResult) *turnRecorder {
	return &turnRecorder{next: next}
}

// RunTurn implements symphony.AgentRunner.
func (r *turnRecorder) RunTurn(_ context.Context, req symphony.TurnRequest) (symphony.TurnResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	next := r.next
	r.mu.Unlock()
	return next(req), nil
}

func (r *turnRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *turnRecorder) recorded() []symphony.TurnRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]symphony.TurnRequest(nil), r.calls...)
}

// settleTurn is a scripted turn that succeeds and declares completion.
func settleTurn(req symphony.TurnRequest) symphony.TurnResult {
	return symphony.TurnResult{Exit: symphony.TurnSuccess, Settled: true, ThreadID: "th-" + req.SessionID}
}

// blockingTurn is a scripted runner that never settles until released: it
// reports entry and then stands in for an agent mid-turn. Releasing the
// turn lets it settle (the gate decides); cancelling the run context
// stands in for the supervisor process dying under the agent.
type blockingTurn struct {
	mu      sync.Mutex
	calls   []symphony.TurnRequest
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingTurn() *blockingTurn {
	return &blockingTurn{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// RunTurn implements symphony.AgentRunner.
func (b *blockingTurn) RunTurn(ctx context.Context, req symphony.TurnRequest) (symphony.TurnResult, error) {
	b.mu.Lock()
	b.calls = append(b.calls, req)
	b.mu.Unlock()
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		// Released turns settle as completion claims; the gate decides.
		return settleTurn(req), nil
	case <-ctx.Done():
		// The supervisor died under the agent: the turn ends abruptly
		// with nothing recorded and no cleanup.
		return symphony.TurnResult{Exit: symphony.TurnCanceled}, nil
	}
}

func (b *blockingTurn) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// commandGate is a GateEvaluator backed by a real shell command in the
// workspace: exit 0 is green, non-zero is red with the combined output,
// anything else is a gate error. It stands in for the failing gate-check.
type commandGate struct{ command string }

// Evaluate implements symphony.GateEvaluator.
func (g commandGate) Evaluate(ctx context.Context, req symphony.GateRequest) (symphony.GateOutcome, error) {
	cmd := exec.CommandContext(ctx, "sh", "-lc", g.command) //nolint:gosec // fixed suite-authored command
	cmd.Dir = req.Workspace
	out, err := cmd.CombinedOutput()
	if err == nil {
		return symphony.GateOutcome{Verdict: symphony.GateGreen, Output: string(out)}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return symphony.GateOutcome{Verdict: symphony.GateRed, Output: string(out)}, nil
	}
	return symphony.GateOutcome{}, fmt.Errorf("run gate command: %w", err)
}

// --- tracker state helpers ----------------------------------------------

// readyFront returns the ids currently on the ready dispatch front.
func readyFront(ctx context.Context, tr tracker.Tracker) ([]string, error) {
	issues, err := tr.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{"ready"}})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids, nil
}

// onFront reports whether id sits on the ready dispatch front.
func onFront(ctx context.Context, tr tracker.Tracker, id string) (bool, error) {
	ids, err := readyFront(ctx, tr)
	if err != nil {
		return false, err
	}
	for _, front := range ids {
		if front == id {
			return true, nil
		}
	}
	return false, nil
}

// quarantineRecords moves every bead on the ready front except the given
// work ids to the "blocked" status, taking it off the dispatch front while
// keeping it readable by metadata queries.
//
// This is the suite's stand-in for the assembled daemon's front filter.
// Against a real bd tracker, lambe's own operational records (attempt
// beads, pinned rejections) are ordinary open beads and would otherwise be
// dispatched as work by the next tick; blocking them keeps scenario
// dispatch reads deterministic while every record stays hub-visible.
func quarantineRecords(ctx context.Context, t *testing.T, tr *tracker.Local, workIDs ...string) {
	t.Helper()
	keep := map[string]bool{}
	for _, id := range workIDs {
		keep[id] = true
	}
	ids, err := readyFront(ctx, tr)
	if err != nil {
		t.Fatalf("read front for quarantine: %v", err)
	}
	blocked := false
	for _, id := range ids {
		if keep[id] {
			continue
		}
		if err := tr.QueueUpdate(id, tracker.BatchFields{Status: "blocked"}); err != nil {
			t.Fatalf("queue quarantine %s: %v", id, err)
		}
		blocked = true
	}
	if blocked {
		if err := tr.FlushWrites(ctx); err != nil {
			t.Fatalf("flush quarantine: %v", err)
		}
	}
}

// waitFor polls cond until it holds or the deadline expires.
func waitFor(t *testing.T, what string, deadline time.Duration, cond func() (bool, error)) {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		ok, err := cond()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if ok {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%s: condition not met within %s", what, deadline)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// assertContextParity is the per-fault context-parity check: after a fault,
// everything an agent or a human observer needs must be readable through
// the tracker alone — no daemon-only channel. The issue must answer an
// id-read with real state, the attempt journal must be readable, and the
// mutation journal must be consumable.
func assertContextParity(ctx context.Context, t *testing.T, tr tracker.Tracker, issueID string) {
	t.Helper()
	issues, err := tr.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{issueID}})
	if err != nil {
		t.Fatalf("context parity: id read %s: %v", issueID, err)
	}
	var found bool
	for _, issue := range issues {
		if issue.ID != issueID {
			continue
		}
		found = true
		if issue.Status == "" || issue.Title == "" {
			t.Errorf("context parity: issue %s read back without status/title: %+v", issueID, issue)
		}
	}
	if !found {
		t.Errorf("context parity: issue %s invisible to the tracker id read", issueID)
	}
	if _, err := tr.Attempts(ctx, issueID); err != nil {
		t.Errorf("context parity: attempts for %s unreadable: %v", issueID, err)
	}
	if _, err := tr.Events(ctx, 0); err != nil && !errors.Is(err, tracker.ErrJournalTruncated) {
		t.Errorf("context parity: events journal unreadable: %v", err)
	}
}

// --- policy models ------------------------------------------------------
//
// The attempt cap and the consecutive-deferral escalation are supervisor
// policy. Their enforcement lives in the assembled daemon's gate layer,
// which is outside this suite's committed seams, so the suite carries a
// minimal local model of the same contract (attempt cap 8, escalation after
// five consecutive deferrals, a completed attempt resets the streak) and
// asserts the committed seams can carry every state the policy needs. The
// daemon's own tests must supersede this model once wired.

const (
	// attemptCap is the red-attempt count at which work escalates to
	// needs-human.
	attemptCap = 8
	// deferEscalationThreshold is the consecutive no-progress deferral
	// count at which work escalates to needs-human.
	deferEscalationThreshold = 5
)

// deferEscalator counts consecutive heuristic deferrals with no completed
// attempt. It is the local model of the daemon's persisted escalation
// counter.
type deferEscalator struct{ consecutive int }

// Defer records one more deferral and reports whether the escalation
// threshold was reached.
func (e *deferEscalator) Defer() bool {
	e.consecutive++
	return e.consecutive >= deferEscalationThreshold
}

// Completed records a completed attempt, resetting the streak: deferral is
// only an infinite silent loop when nothing ever finishes.
func (e *deferEscalator) Completed() { e.consecutive = 0 }

// applyLimitOutcome models the daemon wiring between the harness outcome
// and the tracker: a LimitSuspected turn is parked with a defer whose until
// is the outcome's wake-at suggestion. The attempt budget is untouched —
// the caller re-puts the same attempt number, never the next.
func applyLimitOutcome(ctx context.Context, tr tracker.Tracker, issueID string, out harness.Outcome) error {
	if out.Kind != harness.OutcomeLimitSuspected {
		return fmt.Errorf("applyLimitOutcome: kind %s is not limit-suspected", out.Kind)
	}
	reason := fmt.Sprintf("provider limit suspected (confidence %.2f): retryable, attempt budget untouched", out.Confidence)
	return tr.Defer(ctx, issueID, out.WakeAt, reason)
}

// --- fake Pi agent (re-exec) ---------------------------------------------

// runPiFake speaks just enough of the Pi RPC protocol for the limit
// scenario: state and stats queries, prompt/steer/abort commands, and one
// scripted event sequence per behavior. It is the fault-suite sibling of
// the pi driver's own fake agent, re-exec'd from this test binary.
func runPiFake(script string) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var cmd struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			continue
		}
		switch cmd.Type {
		case "get_state":
			respond(cmd.ID, true, "", map[string]any{
				"isStreaming":         false,
				"sessionId":           "ses-fault",
				"sessionFile":         "/sessions/ses-fault.jsonl",
				"pendingMessageCount": 0,
			})
		case "get_session_stats":
			respond(cmd.ID, true, "", map[string]any{
				"sessionId":   "ses-fault",
				"sessionFile": "/sessions/ses-fault.jsonl",
				"tokens":      map[string]any{"input": 100, "output": 20, "cacheRead": 0, "cacheWrite": 0, "total": 120},
				"cost":        0.005,
			})
		case "prompt", "steer", "follow_up":
			respond(cmd.ID, true, "", map[string]any{"disposition": "started"})
			scriptEvents(script)
		case "abort":
			respond(cmd.ID, true, "", nil)
		default:
			respond(cmd.ID, false, "unknown command "+cmd.Type, nil)
		}
	}
}

// scriptEvents emits the scripted event sequence for one accepted prompt.
func scriptEvents(script string) {
	switch script {
	case "limit":
		// A provider-limit-shaped error stream: auto retries reporting
		// rate limiting, then the agent hangs (the provider is locked
		// out, nothing more will arrive).
		for range 3 {
			event("auto_retry_start", map[string]any{
				"attempt": 1, "maxAttempts": 3, "delayMs": 5000,
				"errorMessage": "rate limit exceeded (HTTP 429): usage limit reached",
			})
		}
		blockForever()
	}
}

// blockForever sleeps forever; a plain select{} would trip the runtime
// deadlock detector in a process with no other runnable goroutines.
func blockForever() {
	for {
		time.Sleep(time.Hour)
	}
}

// respond writes one correlated command response.
func respond(id string, success bool, errMsg string, data any) {
	rec := map[string]any{"id": id, "type": "response", "success": success}
	if data != nil {
		rec["data"] = data
	}
	if errMsg != "" {
		rec["error"] = errMsg
	}
	writeRecord(rec)
}

// event writes one session event record.
func event(typ string, fields map[string]any) {
	rec := map[string]any{"type": typ}
	for k, v := range fields {
		rec[k] = v
	}
	writeRecord(rec)
}

// writeRecord marshals and writes one LF-terminated JSON record.
func writeRecord(rec map[string]any) {
	b, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "faultsuite fake pi: marshal: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("%s\n", b)
}

// killGroup sends sig to the process group identified by pgid.
func killGroup(pgid int, sig syscall.Signal) error {
	return syscall.Kill(-pgid, sig)
}

// --- assembled-daemon integration hook ----------------------------------
//
// A true kill -9 of the assembled supervisor process cannot be simulated at
// package level, so the suite exposes an env-gated hook. The contract:
//
//	LAMBE_FAULT_DAEMON_BIN   path to the assembled lambe binary
//	LAMBE_FAULT_DAEMON_DIR   optional tracker directory to parity-check
//	                         after the kill (defaults: none)
//
// The hook starts the daemon in its own process group with `run`, waits for
// it to take on responsibility, kills the group with SIGKILL, and asserts
// the process died from the signal and that every responsibility it held is
// still readable through the tracker. When the env var is unset the hook
// skips; the package-level scenarios remain the always-on definition of
// done.

const (
	daemonBinEnv = "LAMBE_FAULT_DAEMON_BIN"
	daemonDirEnv = "LAMBE_FAULT_DAEMON_DIR"
)

// sigKill is the abrupt-death signal; it cannot be caught or ignored.
const sigKill = syscall.SIGKILL

// daemonBinHook returns the assembled daemon binary path or skips.
func daemonBinHook(t *testing.T) string {
	t.Helper()
	bin := os.Getenv(daemonBinEnv)
	if bin == "" {
		t.Skipf("%s not set; assembled-daemon kill -9 hook not armed", daemonBinEnv)
	}
	return bin
}

// daemonDirHook returns the optional tracker directory for the hook.
func daemonDirHook() string { return os.Getenv(daemonDirEnv) }

// daemonCommand builds the daemon start command in its own process group.
func daemonCommand(t *testing.T, bin string) *exec.Cmd {
	t.Helper()
	dir := daemonDirHook()
	if dir == "" {
		dir = t.TempDir()
	}
	cmd := exec.Command(bin, "run") //nolint:gosec // path comes from the env-gated hook contract
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// exitBySignal reports whether a cmd.Wait error is death by the given
// signal.
func exitBySignal(waitErr error, sig syscall.Signal) bool {
	if waitErr == nil {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == sig
}

// waitingLoop joins dispatched workers for turn/gate protocol assertions.
type waitingLoop struct{ *symphony.Loop }

func (l *waitingLoop) Tick(ctx context.Context) error {
	if err := l.Loop.Tick(ctx); err != nil {
		return err
	}
	return l.Wait(ctx)
}
