package faultsuite_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

// TestFault_DaemonKillMidReconcile proves the crash-recovery contract at
// package level: the symphony loop tick runs against a real local tracker
// with an agent mid-turn; the supervisor is abruptly discarded (the
// kill -9 of the assembled daemon — no cleanup calls of any kind); a fresh
// loop constructed purely from tracker state must observe zero
// responsibility loss and no double dispatch, and pinned-id idempotency
// must keep the attempt journal single-recorded across the restart.
//
// The true process-level kill -9 is the env-gated integration hook
// (LAMBE_FAULT_DAEMON_BIN); see TestFault_DaemonKill9_AssembledDaemon and
// README.md.
func TestFault_DaemonKillMidReconcile(t *testing.T) {
	const actor = "lambe@faults-daemonkill"
	ctx := context.Background()
	env := newTracker(t, actor)
	work := seedWork(t, env, "seed-mid-reconcile", "keep the release green")
	loader := newLoader(t, t.TempDir(), "work on {{ issue }} attempt {{ attempt }}")

	// The doomed supervisor: a real loop whose agent hangs mid-turn.
	victim := newBlockingTurn()
	loop := newLoop(env, loader, victim, commandGate{command: "exit 0"}, t.TempDir())

	tickCtx, cancel := context.WithCancel(ctx)
	tickDone := make(chan error, 1)
	go func() { tickDone <- loop.Tick(tickCtx) }()

	// Mid-turn: the runner is inside the turn, which means the claim and
	// the attempt record already happened (the loop records the attempt
	// before prompting the agent).
	select {
	case <-victim.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("victim loop never reached the turn")
	}
	waitFor(t, "attempt recorded before the kill", 30*time.Second, func() (bool, error) {
		attempts, err := env.Attempts(ctx, work)
		if err != nil {
			return false, err
		}
		return len(attempts) == 1, nil
	})

	// kill -9: the process dies between turns. Everything the loop held
	// in memory — the session map, the retry queue, the thread — is gone
	// without a single cleanup call. Cancelling the context only releases
	// the blocked fake agent goroutine; the loop itself treats it as a
	// failed turn and schedules a retry into its (now dead) in-memory
	// queue. No terminal transition, no release, no workspace cleanup.
	cancel()
	if tickErr := <-tickDone; tickErr != nil {
		t.Logf("victim tick ended with %v (post-mortem only; the loop is discarded)", tickErr)
	}

	// Zero responsibility loss: the issue and its claim survive in the
	// tracker, readable by anyone.
	issues, err := env.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{work}})
	if err != nil {
		t.Fatalf("post-kill id read: %v", err)
	}
	if len(issues) != 1 || issues[0].Status != "in_progress" || issues[0].Assignee != actor {
		t.Fatalf("post-kill state: got %+v, want one in_progress issue claimed by %s (no cleanup ran)", issues, actor)
	}
	assertContextParity(ctx, t, env, work)

	// The fresh supervisor, constructed only from tracker state.
	fresh := newTurnRecorder(settleTurn)
	freshLoop := newLoop(env, loader, fresh, commandGate{command: "exit 0"}, t.TempDir())
	// Keep lambe's own attempt bead off the dispatch front (see
	// quarantineRecords).
	quarantineRecords(ctx, t, env.Local, work)

	// No double dispatch: the dead supervisor's claim hides the issue
	// from the ready front, so the fresh loop stands down.
	if err := freshLoop.Tick(ctx); err != nil {
		t.Fatalf("fresh tick: %v", err)
	}
	if got := fresh.count(); got != 0 {
		t.Fatalf("fresh loop dispatched claimed issue: %d runner calls, want 0", got)
	}

	// Recovery: the lease reaper (here an operator using the same
	// identity) gives the stale claim back; the fresh loop re-dispatches
	// exactly once, and the re-recorded attempt 1 collapses onto the same
	// pinned id instead of duplicating.
	reaper := env.openIdentity(t, actor)
	if err := reaper.Release(ctx, work); err != nil {
		t.Fatalf("release stale claim: %v", err)
	}
	if err := freshLoop.Tick(ctx); err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	if got := fresh.count(); got != 1 {
		t.Fatalf("recovery dispatch: %d runner calls, want exactly 1", got)
	}
	calls := fresh.recorded()
	if !strings.Contains(calls[0].Prompt, "keep the release green") {
		t.Errorf("recovery prompt lost the issue title: %q", calls[0].Prompt)
	}
	attempts, err := env.Attempts(ctx, work)
	if err != nil {
		t.Fatalf("attempts after recovery: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Number != 1 {
		t.Fatalf("pinned-id idempotency broken: %+v, want exactly attempt 1", attempts)
	}

	// Terminal transition released the claim; the responsibility is done,
	// not lost.
	issues, err = env.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{work}})
	if err != nil {
		t.Fatalf("final id read: %v", err)
	}
	if len(issues) != 1 || issues[0].Assignee != "" {
		t.Fatalf("final state: %+v, want claim released after terminal transition", issues)
	}
}

// TestFault_DaemonKill9_AssembledDaemon is the integration hook for the
// fault the package-level scenarios can only simulate: a true kill -9 of
// the assembled daemon process. It runs only when the daemon binary is
// provided via LAMBE_FAULT_DAEMON_BIN (with LAMBE_FAULT_DAEMON_DIR
// optionally pointing at the tracker directory to parity-check); otherwise
// it skips. See README.md for the contract.
func TestFault_DaemonKill9_AssembledDaemon(t *testing.T) {
	bin := daemonBinHook(t)
	ctx := context.Background()

	cmd := daemonCommand(t, bin)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	pgid := cmd.Process.Pid // the daemon must lead its own process group
	// Give the daemon a moment to take on responsibility, then kill -9
	// its whole group exactly as an OOM or `kill -9` would.
	time.Sleep(2 * time.Second)
	if err := killGroup(pgid, sigKill); err != nil {
		t.Fatalf("kill -9 daemon group %d: %v", pgid, err)
	}
	waitErr := cmd.Wait()
	if !exitBySignal(waitErr, sigKill) {
		t.Fatalf("daemon did not die from SIGKILL: %v", waitErr)
	}

	if dir := daemonDirHook(); dir != "" {
		tr, err := tracker.NewLocal(bdBin(), dir, "lambe@faults-hook")
		if err != nil {
			t.Fatalf("open hook tracker: %v", err)
		}
		issues, err := tr.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{"ready"}})
		if err != nil {
			t.Fatalf("hook front read: %v", err)
		}
		for _, issue := range issues {
			assertContextParity(ctx, t, tr, issue.ID)
		}
	}
}
