package symphony

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

type turnFunc func(context.Context, TurnRequest) (TurnResult, error)

func (f turnFunc) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	return f(ctx, req)
}

func TestTickDispatchesConcurrentWorkersWithoutWaitingForTurns(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "work {{ issue }}")
	if err := os.WriteFile(f.wfPath, []byte("---\nagent:\n  max_concurrent_agents: 2\n---\nwork {{ issue }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.loader.Reload(); err != nil {
		t.Fatal(err)
	}
	f.tr.issues["i-2"] = tracker.Issue{ID: "i-2", Title: "second", Status: "ready"}
	f.tr.issues["i-3"] = tracker.Issue{ID: "i-3", Title: "third", Status: "ready"}
	entered := make(chan string, 3)
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	f.loop.Runner = turnFunc(func(ctx context.Context, req TurnRequest) (TurnResult, error) {
		entered <- req.Issue.ID
		select {
		case <-release:
			return settle(req), nil
		case <-ctx.Done():
			return TurnResult{Exit: TurnCanceled}, ctx.Err()
		}
	})
	done := make(chan error, 1)
	go func() { done <- f.loop.Loop.Tick(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		cancel()
		if err := <-done; err != nil {
			t.Logf("canceled tick: %v", err)
		}
		t.Fatal("Tick waited for a blocked agent turn")
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("workers did not start concurrently")
		}
	}
	select {
	case id := <-entered:
		t.Fatalf("worker %s exceeded configured capacity", id)
	default:
	}
	if issue, _ := f.tr.get("i-3"); issue.Assignee != "" {
		t.Fatal("over-capacity work was claimed")
	}
	close(release)
	if err := f.loop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.Loop.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-entered:
		if id != "i-3" {
			t.Fatalf("next worker = %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("completed workers did not free capacity")
	}
	if err := f.loop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowRejectsInvalidConcurrency(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0", "-1", "1.5", "nope"} {
		t.Run(value, func(t *testing.T) {
			_, err := ParseWorkflow([]byte("---\nagent:\n  max_concurrent_agents: "+value+"\n---\nwork"), fixedEnv(nil))
			if err == nil {
				t.Fatal("invalid agent concurrency was silently ignored")
			}
		})
	}
}

func TestWorkflowReadsStandardAgentSettings(t *testing.T) {
	t.Parallel()
	wf, err := ParseWorkflow([]byte("---\nagent:\n  max_concurrent_agents: 3\n  max_turns: 7\n---\nwork"), fixedEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if wf.MaxConcurrentAgents != 3 || wf.MaxTurns != 7 {
		t.Fatalf("agent settings = %+v", wf)
	}
}

func TestActiveWorkerStopsAfterClaimLostWithoutReleasingForeignClaim(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "work {{ issue }}")
	f.loop.CheckpointDir = filepath.Join(f.dir, "sessions")
	entered := make(chan TurnRequest, 1)
	stopped := make(chan struct{})
	f.loop.Runner = turnFunc(func(ctx context.Context, req TurnRequest) (TurnResult, error) {
		entered <- req
		<-ctx.Done()
		close(stopped)
		return TurnResult{Exit: TurnSuccess, Settled: true}, nil
	})
	if err := f.loop.Loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var req TurnRequest
	select {
	case req = <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	f.tr.mu.Lock()
	issue := f.tr.issues["i-1"]
	issue.Assignee = "foreign"
	f.tr.issues["i-1"] = issue
	f.tr.mu.Unlock()
	f.loop.Tracker = lostRetryClaimTracker{Tracker: f.tr}
	if err := f.loop.Loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("lost-claim worker was not canceled")
	}
	if err := f.loop.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	issue, _ = f.tr.get("i-1")
	if issue.Assignee != "foreign" || issue.Status != "in_progress" {
		t.Fatalf("foreign claim changed: %+v", issue)
	}
	if _, err := os.Stat(req.Workspace); err != nil {
		t.Fatalf("lost-claim workspace removed: %v", err)
	}
	if len(f.gate.calls) != 0 {
		t.Fatal("canceled worker evaluated a completion gate")
	}
	if _, err := os.Stat(f.loop.checkpointPath("i-1")); !os.IsNotExist(err) {
		t.Fatalf("lost claim retained stale continuation: %v", err)
	}
}

func TestCloseCancelsWorkersAndKeepsWorkForRecovery(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "work {{ issue }}")
	started := make(chan struct{})
	f.loop.Runner = turnFunc(func(ctx context.Context, _ TurnRequest) (TurnResult, error) {
		close(started)
		<-ctx.Done()
		return TurnResult{Exit: TurnCanceled}, ctx.Err()
	})
	if err := f.loop.Loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.loop.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.loop.active) != 0 {
		t.Fatal("shutdown left active workers")
	}
	issue, _ := f.tr.get("i-1")
	if issue.Assignee != "lambe" || issue.Status != "in_progress" {
		t.Fatalf("shutdown destroyed recovery claim: %+v", issue)
	}
	if _, err := os.Stat(f.workspaceDir("i-1")); err != nil {
		t.Fatalf("shutdown removed work: %v", err)
	}
}

type gateFunc func(context.Context, GateRequest) (GateOutcome, error)

func (f gateFunc) Evaluate(ctx context.Context, req GateRequest) (GateOutcome, error) {
	return f(ctx, req)
}

func TestReconcileDoesNotInterruptSuccessfulGateWriteback(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "work {{ issue }}", settle)
	closed, finish := make(chan struct{}), make(chan struct{})
	committed := false
	f.loop.Gates = gateFunc(func(ctx context.Context, _ GateRequest) (GateOutcome, error) {
		f.tr.set("i-1", "closed")
		close(closed)
		<-finish
		if err := ctx.Err(); err != nil {
			return GateOutcome{Verdict: GateErr}, err
		}
		committed = true
		return GateOutcome{Verdict: GateGreen}, nil
	})
	if err := f.loop.Loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("gate never closed issue")
	}
	if err := f.loop.Loop.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(finish)
	if err := f.loop.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("active reconciliation canceled successful completion writeback")
	}
}
