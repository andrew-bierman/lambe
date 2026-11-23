package symphony

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestRestorePreservesRetryAndInterruptedTurn(t *testing.T) {
	for _, tt := range []struct {
		phase checkpointPhase
		kind  RetryKind
		turn  int
	}{
		{checkpointRetry, RetryContinuation, 3},
		{checkpointRetry, RetryFailure, 3},
		{checkpointGate, RetryGate, 3},
		{checkpointRunning, RetryContinuation, 2},
	} {
		t.Run(fmt.Sprintf("%s-%d", tt.phase, tt.kind), func(t *testing.T) {
			f := newFixture(t, "work {{ issue }}")
			l := f.loop.Loop
			l.CheckpointDir = filepath.Join(f.dir, "sessions")
			issue := tracker.Issue{ID: "i-1", Status: tracker.StatusInProgress, Assignee: "lambe"}
			f.tr.issues[issue.ID] = issue
			at := time.Now().Add(time.Hour).Round(0)
			s := &session{issue: issue, wf: f.loader.Current(), key: issue.ID, attempt: 4, turn: 3, thread: "native-thread", gateOutput: "settled evidence"}
			if err := l.saveCheckpoint(s, tt.phase, &Retry{IssueID: issue.ID, Kind: tt.kind, At: at}); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(l.checkpointPath(issue.ID))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("checkpoint permissions = %o", info.Mode().Perm())
			}
			if err := l.Restore(t.Context(), "lambe"); err != nil {
				t.Fatal(err)
			}
			got := l.sessions[issue.ID]
			if got == nil || got.attempt != 4 || got.turn != tt.turn || got.thread != s.thread || got.gateOutput != s.gateOutput {
				t.Fatalf("lost continuation: %+v", got)
			}
			due := l.retries.Due(time.Now().Add(2 * time.Hour))
			if len(due) != 1 || due[0].Kind != tt.kind {
				t.Fatalf("retry = %+v", due)
			}
			if tt.phase == checkpointRetry && !due[0].At.Equal(at) {
				t.Fatalf("retry time changed: %+v", due)
			}
		})
	}
}

func TestRestoreStandsDownForForeignAndLegacyClaims(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "foreign"}[foreign], func(t *testing.T) {
			f := newFixture(t, "work {{ issue }}")
			l := f.loop.Loop
			l.CheckpointDir = filepath.Join(f.dir, "sessions")
			issue := tracker.Issue{ID: "i-1", Status: tracker.StatusInProgress, Assignee: "lambe"}
			if foreign {
				issue.Assignee = "another-agent"
			}
			f.tr.issues[issue.ID] = issue
			if err := l.Restore(t.Context(), "lambe"); err != nil {
				t.Fatal(err)
			}
			if err := f.loop.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(f.runner.recorded()) != 0 {
				t.Fatal("launched work without recoverable ownership")
			}
			if !foreign && l.recovery[issue.ID].Error == "" {
				t.Fatal("legacy claim was silently abandoned")
			}
		})
	}
}

func TestRecoveryVerifiesNativeProcessIdentity(t *testing.T) {
	f := newFixture(t, "work {{ issue }}")
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
	f.tr.attempts = []tracker.Attempt{{SessionID: "i-1", Number: 1, PGID: child.Process.Pid, ProcessIdentity: "recycled"}}
	if err := f.loop.stopRecordedAgent(t.Context(), "i-1"); err != nil {
		t.Fatalf("a proven recycled process must not block recovery: %v", err)
	}
	if !proc.GroupAlive(child.Process.Pid) {
		t.Fatal("signaled unrelated process")
	}
	f.tr.attempts = append(f.tr.attempts, tracker.Attempt{SessionID: "i-1", Number: 2})
	if err := f.loop.stopRecordedAgent(t.Context(), "i-1"); err != nil {
		t.Fatal(err)
	}
	if !proc.GroupAlive(child.Process.Pid) {
		t.Fatal("reused an older attempt's group")
	}
	f.tr.attempts = []tracker.Attempt{{SessionID: "i-1", Number: 2, PGID: child.Process.Pid, ProcessIdentity: identity}}
	if err := f.loop.stopRecordedAgent(t.Context(), "i-1"); err != nil {
		t.Fatal(err)
	}
}

type recoveryFinalizer struct {
	*fakeGate
	err   error
	calls int
}

func (g *recoveryFinalizer) FinalizeCompletion(context.Context, string) error {
	g.calls++
	return g.err
}

func TestClosedGateRecoveryRetriesBookkeepingWithoutPrompt(t *testing.T) {
	f := newFixture(t, "work {{ issue }}")
	l := f.loop.Loop
	l.CheckpointDir = filepath.Join(f.dir, "sessions")
	issue := tracker.Issue{ID: "i-1", Status: tracker.StatusClosed, Assignee: "lambe"}
	f.tr.issues[issue.ID] = issue
	ws, err := f.spaces.Ensure(t.Context(), issue.ID, f.loader.Current().Hooks)
	if err != nil {
		t.Fatal(err)
	}
	s := &session{issue: issue, wf: f.loader.Current(), key: issue.ID, workspace: ws.Dir, attempt: 2, turn: 1, gateOutput: "green evidence"}
	if err := l.saveCheckpoint(s, checkpointGate, nil); err != nil {
		t.Fatal(err)
	}
	gate := &recoveryFinalizer{fakeGate: f.gate, err: errors.New("tracker temporarily unavailable")}
	l.Gates = gate
	if err := l.Restore(t.Context(), "lambe"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatal("removed workspace before bookkeeping finished")
	}
	if l.recovery[issue.ID].Error == "" {
		t.Fatal("did not retain failed finalization")
	}
	gate.err = nil
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.runner.recorded()) != 0 || gate.calls != 2 {
		t.Fatalf("gate recovery prompted agent or failed retry: calls=%d", gate.calls)
	}
	if _, err := os.Stat(l.checkpointPath(issue.ID)); !os.IsNotExist(err) {
		t.Fatalf("completed checkpoint remains: %v", err)
	}
	if _, err := os.Stat(ws.Dir); !os.IsNotExist(err) {
		t.Fatalf("completed workspace remains: %v", err)
	}
}

func TestCheckpointFailurePreventsClaimAndPrompt(t *testing.T) {
	f := newFixture(t, "work {{ issue }}", settle)
	f.loop.CheckpointDir = filepath.Join(f.dir, "not-a-directory")
	if err := os.WriteFile(f.loop.CheckpointDir, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	issue, _ := f.tr.get("i-1")
	if issue.Assignee != "" || len(f.runner.recorded()) != 0 {
		t.Fatalf("started uncheckpointed work: %+v", issue)
	}
}

func TestDispatchNeverClaimsOpenOperationalRecords(t *testing.T) {
	f := newFixture(t, "work {{ issue }}", continueTurn)
	f.tr.issues["attempt-in-creation"] = tracker.Issue{ID: "attempt-in-creation", Status: tracker.StatusReady, Metadata: map[string]string{tracker.MetaKind: "attempt"}}
	f.tr.issues["run-in-creation"] = tracker.Issue{ID: "run-in-creation", Status: tracker.StatusReady, Metadata: map[string]string{tracker.MetaKind: "run"}}
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.recorded()
	if len(calls) != 1 || calls[0].Issue.ID != "i-1" {
		t.Fatalf("dispatched operational records: %+v", calls)
	}
	for _, id := range []string{"attempt-in-creation", "run-in-creation"} {
		issue, _ := f.tr.get(id)
		if issue.Assignee != "" {
			t.Fatalf("claimed operational record: %+v", issue)
		}
	}
}

func TestReleasedLegacyClaimStartsPositiveAttempt(t *testing.T) {
	f := newFixture(t, "work {{ issue }}", continueTurn)
	f.loop.CheckpointDir = filepath.Join(f.dir, "sessions")
	f.loop.RecoveryRecheck = time.Nanosecond
	f.tr.issues["i-1"] = tracker.Issue{ID: "i-1", Status: tracker.StatusInProgress, Assignee: "lambe"}
	if err := f.loop.Restore(t.Context(), "lambe"); err != nil {
		t.Fatal(err)
	}
	if err := f.tr.Release(t.Context(), "i-1"); err != nil {
		t.Fatal(err)
	}
	f.tr.set("i-1", tracker.StatusReady)
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.recorded()
	if len(calls) != 1 || calls[0].Attempt != 1 || calls[0].Turn != 1 {
		t.Fatalf("legacy release did not start a valid attempt: %+v", calls)
	}
}

func TestFailureRetryRetiresPreviousAgentBeforeNewAttempt(t *testing.T) {
	f := newFixture(t, "work {{ issue }}", settle, continueTurn)
	closer := &closingRunner{fakeRunner: f.runner}
	f.loop.Runner = closer
	f.gate.outcomes = []GateOutcome{{Verdict: GateRed}}
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.recorded()
	if len(calls) != 2 || calls[1].Attempt != 2 || len(closer.closeLog()) != 1 {
		t.Fatalf("next attempt reused old process binding: calls=%+v closes=%v", calls, closer.closeLog())
	}
}

func TestUnpromptedBindingFailurePreservesAttemptAndTurn(t *testing.T) {
	f := newFixture(t, "work {{ issue }}")
	calls := []TurnRequest{}
	f.loop.Runner = turnFunc(func(_ context.Context, req TurnRequest) (TurnResult, error) {
		calls = append(calls, req)
		if len(calls) == 1 {
			return TurnResult{Exit: TurnFailure}, fmt.Errorf("%w: tracker unavailable", proc.ErrBindingUnavailable)
		}
		return TurnResult{Exit: TurnSuccess}, nil
	})
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Attempt != calls[1].Attempt || calls[0].Turn != calls[1].Turn {
		t.Fatalf("unprompted failure consumed attempt/turn: %+v", calls)
	}
}

func TestShutdownDuringPreparationPreservesAttempt(t *testing.T) {
	f := newFixture(t, "work {{ issue }}")
	l := f.loop.Loop
	l.CheckpointDir = filepath.Join(f.dir, "sessions")
	started := filepath.Join(f.dir, "hook-started")
	f.loader.Current().Hooks[HookBeforeRun] = fmt.Sprintf("touch %q; sleep 30", started)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := l.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("preparation hook did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := l.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	saved, err := l.readCheckpoint(l.checkpointPath("i-1"))
	if err != nil {
		t.Fatal(err)
	}
	if saved.Attempt != 1 || saved.Turn != 0 || saved.Phase != checkpointRetry || saved.Retry == nil || saved.Retry.Kind != RetryContinuation {
		t.Fatalf("shutdown consumed preparation attempt: %+v", saved)
	}
	if len(f.runner.recorded()) != 0 {
		t.Fatal("prompted during preparation")
	}
}

func TestNativeThreadIsDurableBeforePrompt(t *testing.T) {
	f := newFixture(t, "work {{ issue }}")
	f.loop.CheckpointDir = filepath.Join(f.dir, "sessions")
	f.loop.Runner = turnFunc(func(_ context.Context, req TurnRequest) (TurnResult, error) {
		if req.RecordThread == nil {
			return TurnResult{}, errors.New("no durable thread recorder")
		}
		if err := req.RecordThread("actual-backend-thread", "codex"); err != nil {
			return TurnResult{}, err
		}
		saved, err := f.loop.readCheckpoint(f.loop.checkpointPath(req.SessionID))
		if err != nil || saved.Thread != "actual-backend-thread" || saved.ThreadHarness != "codex" || saved.Phase != checkpointRunning {
			return TurnResult{}, fmt.Errorf("thread not durable before prompt: %+v, %v", saved, err)
		}
		return TurnResult{Exit: TurnSuccess}, nil
	})
	if err := f.loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	saved, err := f.loop.readCheckpoint(f.loop.checkpointPath("i-1"))
	if err != nil || saved.Thread != "actual-backend-thread" || saved.ThreadHarness != "codex" || saved.Phase != checkpointRetry {
		t.Fatalf("thread not retained across continuation: %+v, %v", saved, err)
	}
}

func TestUnavailableConversationRestartsFullPromptWithinAttempt(t *testing.T) {
	f := newFixture(t, "full task {{ issue }}")
	var calls []TurnRequest
	f.loop.Runner = turnFunc(func(_ context.Context, req TurnRequest) (TurnResult, error) {
		calls = append(calls, req)
		if len(calls) == 1 {
			return TurnResult{Exit: TurnSuccess, ThreadID: "old", ThreadHarness: "acp"}, nil
		}
		if len(calls) == 2 {
			return TurnResult{Exit: TurnFailure}, ErrRestartFromFullPrompt
		}
		return TurnResult{Exit: TurnSuccess}, nil
	})
	for range 3 {
		if err := f.loop.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 3 || calls[1].Turn != 2 || calls[2].Turn != 1 || calls[2].Attempt != calls[0].Attempt || calls[2].ThreadID != "old" || calls[2].Prompt != calls[0].Prompt {
		t.Fatalf("lost full-prompt recovery: %+v", calls)
	}
}

func TestChangedWorkspaceRootRefusesRecoveryAndPreservesSavedWork(t *testing.T) {
	f := newFixture(t, "body")
	l := f.loop.Loop
	l.CheckpointDir = filepath.Join(f.dir, "sessions")
	workspace, err := l.Spaces.Ensure(t.Context(), "i-1", l.Loader.Current().Hooks)
	if err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(workspace.Dir, "unfinished.txt")
	if err := os.WriteFile(precious, []byte("uncommitted work"), 0o600); err != nil {
		t.Fatal(err)
	}
	issue, _ := f.tr.get("i-1")
	s := &session{issue: issue, wf: l.Loader.Current(), key: "i-1", workspace: workspace.Dir, attempt: 1, turn: 1, thread: "saved-native-thread"}
	if err := l.saveCheckpoint(s, checkpointRunning, nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(l.checkpointPath("i-1"))
	if err != nil {
		t.Fatal(err)
	}
	newRoot := t.TempDir()
	l.Spaces = NewWorkspaceManager(newRoot, l.Log)
	err = l.Restore(t.Context(), "lambe")
	if !errors.Is(err, ErrWorkspaceRootChanged) || !strings.Contains(err.Error(), workspace.Dir) || !strings.Contains(err.Error(), newRoot) {
		t.Fatalf("root change did not fail clearly: %v", err)
	}
	after, err := os.ReadFile(l.checkpointPath("i-1"))
	if err != nil || string(after) != string(before) {
		t.Fatal("root change destroyed continuation state")
	}
	if data, err := os.ReadFile(precious); err != nil || string(data) != "uncommitted work" { // #nosec G304 -- path is inside this test's temporary workspace.
		t.Fatal("root change destroyed workspace")
	}
	if len(f.runner.recorded()) != 0 {
		t.Fatal("root mismatch launched an agent")
	}
}

func TestClosedIssueRecoveryStopsAsClosedExternally(t *testing.T) {
	f := newFixture(t, "work {{ issue }}")
	l := f.loop.Loop
	l.CheckpointDir = filepath.Join(f.dir, "sessions")
	var phases []string
	l.Observe = func(p Progress) { phases = append(phases, p.Phase) }
	issue := tracker.Issue{ID: "i-2", Status: tracker.StatusClosed, Assignee: "lambe"}
	f.tr.issues[issue.ID] = issue
	ws, err := f.spaces.Ensure(t.Context(), issue.ID, f.loader.Current().Hooks)
	if err != nil {
		t.Fatal(err)
	}
	s := &session{issue: issue, wf: f.loader.Current(), key: issue.ID, workspace: ws.Dir, attempt: 1, turn: 1}
	if err := l.saveCheckpoint(s, checkpointRetry, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Restore(t.Context(), "lambe"); err != nil {
		t.Fatal(err)
	}
	if len(phases) == 0 || phases[len(phases)-1] != "stopped: closed externally" {
		t.Fatalf("phases = %v, want the closed-externally stop", phases)
	}
}
