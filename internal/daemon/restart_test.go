package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestDaemonRestartResumesPersistedContinuation(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	workflow := filepath.Join(root, "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte("---\nagent:\n  max_turns: 20\n---\nWork {{ issue }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := newFakeTracker()
	tr.issues["restart"] = tracker.Issue{ID: "restart", Title: "resume this work", Status: "ready"}
	base := Options{Config: &config.Config{Supervisor: config.Supervisor{TickSeconds: 1, HeartbeatInterval: time.Hour}}, StateDir: state, Machine: "test", WorkflowPath: workflow}
	start := func(runner symphony.AgentRunner) (context.CancelFunc, <-chan error) {
		opts := base
		opts.Wire = func(a *Assembly) error { a.Trackers = []tracker.Tracker{tr}; a.Runner = runner; return nil }
		d, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- d.Run(ctx) }()
		return cancel, done
	}
	stop := func(cancel context.CancelFunc, done <-chan error) {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("daemon did not stop")
		}
	}
	cancelFirst, firstDone := start(daemonTurnFunc(func(context.Context, symphony.TurnRequest) (symphony.TurnResult, error) {
		return symphony.TurnResult{Exit: symphony.TurnSuccess, ThreadID: "preserved-thread"}, nil
	}))
	firstStopped := false
	defer func() {
		if !firstStopped {
			stop(cancelFirst, firstDone)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		files, err := filepath.Glob(filepath.Join(state, "sessions", "lambe", "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		persisted := false
		for _, file := range files {
			data, err := os.ReadFile(file) // #nosec G304 -- private checkpoint in test-owned state directory
			if err != nil {
				continue
			}
			var snapshot struct {
				Phase string `json:"phase"`
			}
			if json.Unmarshal(data, &snapshot) == nil && snapshot.Phase == "retry" {
				persisted = true
			}
		}
		if persisted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon never persisted its pending continuation")
		}
		time.Sleep(time.Millisecond)
	}
	stop(cancelFirst, firstDone)
	firstStopped = true
	resumed := make(chan symphony.TurnRequest, 1)
	cancelSecond, secondDone := start(daemonTurnFunc(func(_ context.Context, req symphony.TurnRequest) (symphony.TurnResult, error) {
		resumed <- req
		return symphony.TurnResult{Exit: symphony.TurnFailure}, nil
	}))
	defer stop(cancelSecond, secondDone)
	select {
	case req := <-resumed:
		if req.Attempt != 1 || req.Turn < 2 || req.ThreadID != "preserved-thread" {
			t.Fatalf("restart lost continuation: %+v", req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fresh daemon abandoned its own in-progress responsibility")
	}
}

func TestDaemonSIGKILLRestoresRealTrackerAndStopsSurvivingAgent(t *testing.T) {
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for real tracker")
	}
	bin, err := exec.LookPath("bd")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	tr, err := tracker.NewLocal(bin, root, "lambe@crash-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	issue, err := tr.EnsureIssue(t.Context(), "crash-work", tracker.IssueSpec{Title: "resume the interrupted work", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "WORKFLOW.md"), []byte("---\n---\nWork {{ issue }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := func(life string) *exec.Cmd {
		t.Helper()
		cmd := exec.Command(executable, "-test.run=^TestDaemonCrashHelper$") // #nosec G204 -- same test executable
		cmd.Env = append(os.Environ(), "LAMBE_CRASH_TEST_ROOT="+root, "LAMBE_CRASH_TEST_LIFE="+life)
		output, err := os.Create(filepath.Join(root, life+".log")) // #nosec G304 -- parent test selects this log in its own temporary directory
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = output.Close() })
		cmd.Stdout = output
		cmd.Stderr = output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	waitRequest := func(life string) symphony.TurnRequest {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(filepath.Join(root, life+".json")) // #nosec G304 -- test-owned path
			var req symphony.TurnRequest
			if err == nil && json.Unmarshal(data, &req) == nil {
				return req
			}
			time.Sleep(20 * time.Millisecond)
		}
		data, _ := os.ReadFile(filepath.Join(root, life+".log")) // #nosec G304 -- test-owned log
		t.Fatalf("daemon did not reach %s request: %s", life, data)
		return symphony.TurnRequest{}
	}
	defer func() {
		data, err := os.ReadFile(filepath.Join(root, "agent.pid")) // #nosec G304 -- helper's test-owned child record
		if err == nil {
			if pid, err := strconv.Atoi(string(data)); err == nil {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	}()
	first := start("first")
	firstWaited := false
	defer func() {
		if !firstWaited {
			_ = first.Process.Kill()
			_ = first.Wait()
		}
	}()
	req := waitRequest("first")
	if req.Turn != 2 || req.Attempt != 1 {
		t.Fatalf("pre-crash request: %+v", req)
	}
	pidData, err := os.ReadFile(filepath.Join(root, "agent.pid")) // #nosec G304 -- helper child record in the parent test temporary directory
	if err != nil {
		t.Fatal(err)
	} // #nosec G304 -- test-owned process record
	pid, err := strconv.Atoi(string(pidData))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-pid, syscall.SIGKILL) }()
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	firstWaited = true
	if !proc.GroupAlive(pid) {
		t.Fatal("agent did not survive daemon SIGKILL")
	}
	second := start("second")
	defer func() { _ = second.Process.Signal(syscall.SIGTERM); _ = second.Wait() }()
	resumed := waitRequest("second")
	if resumed.SessionID != issue.ID || resumed.Attempt != 1 || resumed.Turn != 2 || resumed.ThreadID != "crash-thread" {
		t.Fatalf("restart lost durable state: %+v", resumed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for proc.GroupAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if proc.GroupAlive(pid) {
		t.Fatal("recovery prompted while old agent survived")
	}
	attempts, err := tr.Attempts(t.Context(), issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Number != 1 {
		t.Fatalf("restart consumed attempt: %+v", attempts)
	}
}

func TestDaemonCrashHelper(t *testing.T) {
	root := os.Getenv("LAMBE_CRASH_TEST_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	life := os.Getenv("LAMBE_CRASH_TEST_LIFE")
	bin, err := exec.LookPath("bd")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := tracker.NewLocal(bin, root, "lambe@crash-test")
	if err != nil {
		t.Fatal(err)
	}
	var child *exec.Cmd
	runner := daemonTurnFunc(func(ctx context.Context, req symphony.TurnRequest) (symphony.TurnResult, error) {
		if life == "first" && req.Turn == 1 {
			child = exec.Command("sleep", "120")
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := child.Start(); err != nil {
				return symphony.TurnResult{}, err
			}
			persist := newAttemptPersisterFrom(func() (tracker.Tracker, error) { return tr, nil }, slog.Default(), proc.ProcessIdentity)
			if err := persist(req.SessionID, child.Process.Pid); err != nil {
				proc.AbortStart(child)
				return symphony.TurnResult{}, fmt.Errorf("%w: %w", proc.ErrBindingUnavailable, err)
			}
			// #nosec G703 -- subprocess root is the parent test's temporary directory
			if err := os.WriteFile(filepath.Join(root, "agent.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
				return symphony.TurnResult{}, err
			}
			return symphony.TurnResult{Exit: symphony.TurnSuccess, ThreadID: "crash-thread"}, nil
		}
		data, err := json.Marshal(req)
		if err != nil {
			return symphony.TurnResult{}, err
		}
		// #nosec G703 -- subprocess root and life are selected by the parent test
		if err := os.WriteFile(filepath.Join(root, life+".json"), data, 0o600); err != nil {
			return symphony.TurnResult{}, err
		}
		<-ctx.Done()
		return symphony.TurnResult{Exit: symphony.TurnCanceled}, ctx.Err()
	})
	d, err := New(Options{Config: &config.Config{Supervisor: config.Supervisor{TickSeconds: 1, HeartbeatInterval: time.Hour}}, StateDir: filepath.Join(root, "state"), Machine: "crash-test", Actor: "lambe@crash-test", WorkflowPath: filepath.Join(root, "WORKFLOW.md"), Wire: func(a *Assembly) error { a.Trackers = []tracker.Tracker{tr}; a.Runner = runner; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(t.Context(), syscall.SIGTERM, os.Interrupt)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatal(fmt.Errorf("daemon helper: %w", err))
	}
}
